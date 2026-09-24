#!/usr/bin/env bash
# Remote A -> B -> C snapshot chains preserve process/disk state and reduction.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
source "$E2E_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker grep ip mkfs.ext4 openssl python3 truncate; do
    require_command "$command"
done
for binary in sandbox-ctl cloud-hypervisor flatten-ctl manifest-ctl store-ctl; do
    require_binary "$binary"
done
for file in sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/run"
RUN_ROOT="$WORK/run"
TAP_NAME="xc$((BASHPID % 1000000))"
TAP_CREATED=0
PIDS=()
cleanup() {
    local status=$?
    set +e
    for pid in "${PIDS[@]}"; do
        kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    done
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    exit "$status"
}
trap cleanup EXIT

ip tuntap add dev "$TAP_NAME" mode tap
TAP_CREATED=1
ip addr add 169.254.1.0/31 dev "$TAP_NAME"
ip link set "$TAP_NAME" up

free_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}
STORE_PORT=$(free_port)
cat >"$WORK/store.yaml" <<EOF
listen: 127.0.0.1:$STORE_PORT
backend: fs
fs: { root: $WORK/store-data, verify_content_key: true }
EOF
"$BIN/store-ctl" init --config "$WORK/store.yaml" --generation G1
"$BIN/store-ctl" serve --config "$WORK/store.yaml" >"$OUT/store.log" 2>&1 &
STORE_PID=$!
PIDS+=("$STORE_PID")
for _ in $(seq 1 100); do
    (echo >/dev/tcp/127.0.0.1/"$STORE_PORT") 2>/dev/null && break
    sleep 0.05
done
(echo >/dev/tcp/127.0.0.1/"$STORE_PORT") 2>/dev/null || e2e_fail "store did not become ready"

KEY=$(openssl rand -hex 32)
cat >"$WORK/manifest.yaml" <<EOF
manifest: { key: "$KEY", verify_content: true, write_generation: G1 }
store: { endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 30s }
cache: { endpoint: "" }
chunker: { mode: cdc }
crypto: { chunk: aes, manifest: aes }
EOF
cat >"$WORK/reader.yaml" <<EOF
manifest: { key: "$KEY", verify_content: true }
store: { endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 30s }
cache: { endpoint: "" }
crypto: { chunk: aes, manifest: aes }
EOF

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
ROOT_REF=$(plaintext_tarstream_ref "$ROOT_IMAGE")
ROOT_DIFF="$WORK/root.diff"
truncate -s 1G "$ROOT_DIFF"
mkfs.ext4 -q -F -O ^has_journal "$ROOT_DIFF"

cat >"$WORK/cold.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: remote-chain }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$ROOT_DIFF }
launch:
  exec: /usr/local/bin/python3
  args: ["-c", "import os,time\nfd=os.open('/ticks.dat',os.O_RDWR|os.O_CREAT,0o644)\ni=0\nwhile True:\n os.pwrite(fd,('TICK%08d'%i).encode().ljust(4096,b'.'),i*4096); os.fsync(fd); blk0=os.pread(fd,12,0).decode(); print('TICK %d DISK blk0=%s'%(i,blk0),flush=True); i+=1; time.sleep(.10)"]
  restart: never
EOF

wait_tick() {
    local log=$1 want=$2 pid=$3
    for _ in $(seq 1 1200); do
        grep -qE "^TICK $want DISK blk0=TICK00000000$" "$log" 2>/dev/null && return
        kill -0 "$pid" 2>/dev/null || { tail -80 "$log" >&2 || true; e2e_fail "sandbox exited before TICK $want"; }
        sleep 0.05
    done
    tail -80 "$log" >&2 || true
    e2e_fail "timed out waiting for TICK $want"
}
last_tick() {
    awk '/^TICK [0-9]+ DISK blk0=TICK00000000$/ { n=$2 } END { if (n=="") exit 1; print n }' "$1"
}
write_host_config() {
    local path=$1 diff=$2 hostname=$3
    truncate -s 1G "$diff"
    cat >"$path" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: $hostname }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root: { overlay: { diff: file://$diff } }
EOF
}
run_restore() {
    local sid=$1 ref=$2 config=$3 log=$4 manifest_config=${5:-$WORK/manifest.yaml}
    shift 5 || true
    "$BIN/sandbox-ctl" run --restore "$ref" --config "$config" --manifest-config "$manifest_config" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --sandbox-id "$sid" "$@" >"$log" 2>&1 &
    RUN_PID=$!
    PIDS+=("$RUN_PID")
}
snapshot_upload() {
    local sid=$1 log=$2
    local key
    key=$("$BIN/sandbox-ctl" snapshot --sandbox-id "$sid" --upload --run-root "$RUN_ROOT" 2>"$log")
    key="${key#manifest://}"
    [ "${#key}" -eq 64 ] || e2e_fail "snapshot upload did not return a manifest key"
    printf '%s\n' "$key"
}

# A: cold state -> remote snapshot.
SID1="chain-a-$BASHPID"
LOG1="$OUT/a.log"
"$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --manifest-config "$WORK/manifest.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --sandbox-id "$SID1" >"$LOG1" 2>&1 &
PID1=$!
PIDS+=("$PID1")
wait_tick "$LOG1" 10 "$PID1"
SNAP1=$(snapshot_upload "$SID1" "$OUT/a-snapshot.log")
wait "$PID1" 2>/dev/null || true
PIDS=("$STORE_PID")
TICK1=$(last_tick "$LOG1")

# B: restore A, prove process + deepest disk state, then snapshot again.
write_host_config "$WORK/host-b.yaml" "$WORK/b.diff" chain-b
SID2="chain-b-$BASHPID"
LOG2="$OUT/b.log"
run_restore "$SID2" "manifest://$SNAP1" "$WORK/host-b.yaml" "$LOG2" "$WORK/manifest.yaml"
PID2=$RUN_PID
wait_tick "$LOG2" $((TICK1 + 3)) "$PID2"
SNAP2=$(snapshot_upload "$SID2" "$OUT/b-snapshot.log")
wait "$PID2" 2>/dev/null || true
PIDS=("$STORE_PID")
TICK2=$(last_tick "$LOG2")

# C: restore B. The memory chain must be explicit two-layer state, while disk
# block zero still falls through to the oldest overlay.
write_host_config "$WORK/host-c.yaml" "$WORK/c.diff" chain-c
SID3="chain-c-$BASHPID"
LOG3="$OUT/c.log"
run_restore "$SID3" "manifest://$SNAP2" "$WORK/host-c.yaml" "$LOG3" "$WORK/manifest.yaml"
PID3=$RUN_PID
wait_tick "$LOG3" $((TICK2 + 3)) "$PID3"
grep -qE 'snapshot source: 2 memory layer\(s\)' "$LOG3" || e2e_fail "second restore did not build a two-layer memory chain"
SNAP3=$(snapshot_upload "$SID3" "$OUT/c-snapshot.log")
wait "$PID3" 2>/dev/null || true
PIDS=("$STORE_PID")
TICK3=$(last_tick "$LOG3")

# D: restore C. This proves the recurring snapshot/restore path accumulates,
# rather than silently flattening or dropping, all three memory layers.
write_host_config "$WORK/host-d.yaml" "$WORK/d.diff" chain-d
SID4="chain-d-$BASHPID"
LOG4="$OUT/d.log"
run_restore "$SID4" "manifest://$SNAP3" "$WORK/host-d.yaml" "$LOG4" "$WORK/manifest.yaml"
PID4=$RUN_PID
wait_tick "$LOG4" $((TICK3 + 3)) "$PID4"
grep -qE 'snapshot source: 3 memory layer\(s\)' "$LOG4" || e2e_fail "third restore did not build a three-layer memory chain"
kill -TERM "$PID4" 2>/dev/null || true
wait "$PID4" 2>/dev/null || true
PIDS=("$STORE_PID")

# Reduce the complete remote chain into one portable root, then physically
# retire every historical Manifest root referenced by the original graph. A
# successful one-layer restore proves the reduced root is self-contained.
REDUCED_DIR="$OUT/reduced-location"
REDUCED_LOCATION="reduced-chain"
mkdir -p "$REDUCED_DIR"
"$BIN/sandbox-ctl" info --json --manifest-config "$WORK/manifest.yaml" "manifest://$SNAP3" >"$WORK/original.json"
REDUCED_REF=$("$BIN/sandbox-ctl" publish --quiet --reduce-ref=any --manifest-config "$WORK/manifest.yaml" \
    --to-ref-location "$REDUCED_LOCATION=file://$REDUCED_DIR" "manifest://$SNAP3")
"$BIN/sandbox-ctl" info --json --manifest-config "$WORK/manifest.yaml" \
    --ref-location "$REDUCED_LOCATION=file://$REDUCED_DIR" "$REDUCED_REF" >"$WORK/reduced.json"
python3 - "$WORK/original.json" "$WORK/reduced.json" "$WORK/store-data/manifest" "$WORK/retired" "$SNAP3" <<'PY'
import json, pathlib, re, sys
original, reduced = (json.load(open(path)) for path in sys.argv[1:3])
refs = {"manifest://" + sys.argv[5]}
def walk(value, keep):
    if isinstance(value, dict):
        for key, item in value.items():
            field = key.lower().replace("_", "")
            if field in ("fromrefs", "basefromrefs"):
                if keep:
                    refs.update(item or [])
                elif item:
                    raise SystemExit("reduced result retains lower-reference lists")
            if keep and field == "sandboxref" and isinstance(item, str):
                refs.add(item)
            walk(item, keep)
    elif isinstance(value, list):
        for item in value:
            walk(item, keep)
walk(original, True)
walk(reduced, False)
root = pathlib.Path(sys.argv[3])
retired = pathlib.Path(sys.argv[4])
if root.name != "manifest" or not root.is_dir():
    raise SystemExit("test Manifest partition is unavailable")
retired.mkdir()
moved = 0
for ref in sorted(refs):
    if not isinstance(ref, str) or not ref.startswith("manifest://"):
        continue
    key = ref.removeprefix("manifest://")
    if not re.fullmatch(r"[0-9a-f]{64}", key):
        raise SystemExit(f"invalid historical Manifest ref {ref!r}")
    paths = list(root.glob(f"*/{key[:2]}/{key[2:4]}/{key}"))
    if not paths:
        raise SystemExit(f"historical Manifest fixture was not stored: {key}")
    for path in paths:
        if not path.is_file() or path.is_symlink():
            raise SystemExit(f"unexpected Manifest object type: {path}")
        path.rename(retired / f"{key}-{moved}")
        moved += 1
if not moved:
    raise SystemExit("no historical Manifest roots were retired")
PY

write_host_config "$WORK/host-reduced.yaml" "$WORK/reduced.diff" chain-reduced
SID5="chain-reduced-$BASHPID"
LOG5="$OUT/reduced.log"
run_restore "$SID5" "$REDUCED_REF" "$WORK/host-reduced.yaml" "$LOG5" "$WORK/reader.yaml" \
    --ref-location "$REDUCED_LOCATION=file://$REDUCED_DIR"
PID5=$RUN_PID
wait_tick "$LOG5" $((TICK3 + 3)) "$PID5"
grep -qE 'snapshot source: 1 memory layer\(s\)' "$LOG5" || e2e_fail "reduced snapshot did not restore as one memory layer"

# A reduced restore must itself remain snapshot-able and restorable without the
# retired roots. This catches reduced refs that only work for their first load.
ROUNDTRIP_OUT="$OUT/reduced-roundtrip"
mkdir -p "$ROUNDTRIP_OUT"
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID5" --output "$ROUNDTRIP_OUT" --run-root "$RUN_ROOT" \
    >"$OUT/reduced-resnapshot.out" 2>"$OUT/reduced-resnapshot.log"
wait "$PID5" 2>/dev/null || true
PIDS=("$STORE_PID")
ROUNDTRIP_REF="$ROUNDTRIP_OUT/$SID5.snapshot"
[ -e "$ROUNDTRIP_REF" ] || e2e_fail "reduced restore did not emit a follow-up local snapshot"
ROUNDTRIP_TICK=$(last_tick "$LOG5")
write_host_config "$WORK/host-roundtrip.yaml" "$WORK/roundtrip.diff" chain-roundtrip
SID6="chain-roundtrip-$BASHPID"
LOG6="$OUT/roundtrip.log"
run_restore "$SID6" "$ROUNDTRIP_REF" "$WORK/host-roundtrip.yaml" "$LOG6" "$WORK/reader.yaml" \
    --ref-location "$REDUCED_LOCATION=file://$REDUCED_DIR"
PID6=$RUN_PID
wait_tick "$LOG6" $((ROUNDTRIP_TICK + 3)) "$PID6"
kill -TERM "$PID6" 2>/dev/null || true
wait "$PID6" 2>/dev/null || true
PIDS=("$STORE_PID")

echo "PASS snapshot.remote-chain.sh"
