#!/usr/bin/env bash
# Local same-owner snapshot chains merge to depth one and can be promoted remotely.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker ip mkfs.ext4 openssl python3 timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor manifest-ctl store-ctl cache-ctl; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -r "$SANDBOXER_LIB/tarstream.sh" ] || e2e_fail "missing prepared tarstream helpers"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/runtime" "$WORK/base"
TAP_NAME="lm$((BASHPID % 1000000))"
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
    python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1]); s.close()'
}
KEY="$(openssl rand -hex 32)"
STORE_PORT="$(free_port)"
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

CACHE_PORT="$(free_port)"
CACHE_HEALTH_PORT="$(free_port)"
cat >"$WORK/cache.yaml" <<EOF
mode: tiered
listen: 127.0.0.1:$CACHE_PORT
health_listen: 127.0.0.1:$CACHE_HEALTH_PORT
rpc_timeout: 5s
freq: { counters: 1M, reset_after: 100K }
tiers:
  - type: embedded
    rocks: { path: $WORK/cache-rocks, disk_bytes: 2GiB, mem_ratio: 0.1, direct_reads: false, bloom_bits: 10 }
origin:
  type: store
  store: { endpoint: 127.0.0.1:$STORE_PORT, pool: 2, timeout: 5s }
  max_inflight: 16
EOF
"$BIN/cache-ctl" serve --config "$WORK/cache.yaml" >"$OUT/cache.log" 2>&1 &
CACHE_PID=$!
PIDS+=("$CACHE_PID")
for _ in $(seq 1 100); do
    if "$BIN/cache-ctl" ping --endpoint "127.0.0.1:$CACHE_HEALTH_PORT" 2>/dev/null | grep -qx SERVING; then
        break
    fi
    sleep 0.05
done
"$BIN/cache-ctl" ping --endpoint "127.0.0.1:$CACHE_HEALTH_PORT" 2>/dev/null | grep -qx SERVING || e2e_fail "cache did not become ready"

cat >"$WORK/manifest.yaml" <<EOF
manifest: { key: "$KEY" }
store: { endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 30s }
cache: { endpoint: 127.0.0.1:$CACHE_PORT, pool: 4, timeout: 10s }
chunker: { mode: cdc }
crypto: { chunk: aes, manifest: aes }
EOF

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"
DIFF="$WORK/root.diff"
truncate -s 1G "$DIFF"
mkfs.ext4 -q -F -O ^has_journal "$DIFF"

PYCODE='import os,time
print("PYBOOT-OK", flush=True)
fd=os.open("/ticks.dat", os.O_RDWR|os.O_CREAT, 0o644)
i=0
while True:
    os.pwrite(fd, ("TICK%08d" % i).encode().ljust(4096, b"."), i*4096)
    os.fsync(fd)
    blk0=os.pread(fd,12,0).decode()
    print("TICK %d DISK blk0=%s" % (i, blk0), flush=True)
    i+=1
    time.sleep(0.25)'
PYCODE_JSON="$(printf '%s' "$PYCODE" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')"
cat >"$WORK/cold.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: local-merge }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$DIFF }
launch:
  args: ["-c", $PYCODE_JSON]
  restart: never
EOF

write_restore_config() {
    local path=$1 diff=$2
    truncate -s 1G "$diff"
    mkfs.ext4 -q -F -O ^has_journal "$diff"
    cat >"$path" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: local-merge }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: { diff: file://$diff }
EOF
}

wait_tick() {
    local log=$1 pid=$2 tick=$3
    for _ in $(seq 1 600); do
        grep -qE "^TICK $tick DISK blk0=TICK00000000$" "$log" 2>/dev/null && return 0
        kill -0 "$pid" 2>/dev/null || {
            tail -60 "$log" >&2 || true
            return 1
        }
        sleep 0.05
    done
    return 1
}

SID="local-merge-$BASHPID"
SNAPDIR="$WORK/base/$SID/checkpoint"
mkdir -p "$SNAPDIR"
LOG1="$OUT/cold.log"
"$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --manifest-config "$WORK/manifest.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --base-root "$WORK/base" --sandbox-id "$SID" >"$LOG1" 2>&1 &
PID1=$!
PIDS+=("$PID1")
wait_tick "$LOG1" "$PID1" 10 || e2e_fail "cold sandbox did not reach merge checkpoint"
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID" --output "$SNAPDIR" --run-root "$WORK/runtime" >"$OUT/snapshot1.log" 2>&1
wait "$PID1" || true
PIDS=("$STORE_PID" "$CACHE_PID")
S1="$SNAPDIR/$SID.snapshot"
[ -e "$S1" ] || e2e_fail "first local snapshot is missing"
FROM1="$("$BIN/sandbox-ctl" info --json "$S1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["FromRefs"] or [])')"
[ "$FROM1" = "[]" ] || e2e_fail "first local snapshot unexpectedly has parents: $FROM1"
TICK1="$(grep -oE '^TICK [0-9]+' "$LOG1" | tail -1 | awk '{print $2}')"

write_restore_config "$WORK/restore2.yaml" "$WORK/restore2.diff"
LOG2="$OUT/restore2.log"
"$BIN/sandbox-ctl" run --restore "$S1" --config "$WORK/restore2.yaml" --manifest-config "$WORK/manifest.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --base-root "$WORK/base" --sandbox-id "$SID" >"$LOG2" 2>&1 &
PID2=$!
PIDS+=("$PID2")
wait_tick "$LOG2" "$PID2" "$((TICK1 + 3))" || e2e_fail "first restore lost memory/disk continuity"
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID" --output "$SNAPDIR" --run-root "$WORK/runtime" >"$OUT/snapshot2.log" 2>&1
wait "$PID2" || true
PIDS=("$STORE_PID" "$CACHE_PID")
S2="$SNAPDIR/$SID.snapshot"
FROM2="$("$BIN/sandbox-ctl" info --json "$S2" | python3 -c 'import json,sys; print(json.load(sys.stdin)["FromRefs"] or [])')"
[ "$FROM2" = "[]" ] || e2e_fail "same-owner local parent stacked instead of merging: $FROM2"
TICK2="$(grep -oE '^TICK [0-9]+' "$LOG2" | tail -1 | awk '{print $2}')"

write_restore_config "$WORK/restore3.yaml" "$WORK/restore3.diff"
LOG3="$OUT/restore3.log"
"$BIN/sandbox-ctl" run --restore "$S2" --config "$WORK/restore3.yaml" --manifest-config "$WORK/manifest.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --base-root "$WORK/base" --sandbox-id "$SID" >"$LOG3" 2>&1 &
PID3=$!
PIDS+=("$PID3")
wait_tick "$LOG3" "$PID3" "$((TICK2 + 3))" || e2e_fail "merged snapshot lost guest state"
grep -qE 'snapshot source: 1 memory layer' "$LOG3" || e2e_fail "merged local snapshot did not restore as one memory layer"
kill -TERM "$PID3"
wait "$PID3" || true
PIDS=("$STORE_PID" "$CACHE_PID")

REMOTE_REF="$("$BIN/sandbox-ctl" upload-snapshot --manifest-config "$WORK/manifest.yaml" --quiet "$S2")"
[[ "$REMOTE_REF" =~ ^manifest://[0-9a-f]{64}$ ]] || e2e_fail "offline upload returned invalid manifest ref: $REMOTE_REF"
write_restore_config "$WORK/restore4.yaml" "$WORK/restore4.diff"
LOG4="$OUT/restore4.log"
SID4="local-merge-remote-$BASHPID"
"$BIN/sandbox-ctl" run --restore "$REMOTE_REF" --config "$WORK/restore4.yaml" --manifest-config "$WORK/manifest.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --base-root "$WORK/base" --sandbox-id "$SID4" >"$LOG4" 2>&1 &
PID4=$!
PIDS+=("$PID4")
wait_tick "$LOG4" "$PID4" "$((TICK2 + 3))" || e2e_fail "remote promotion did not preserve merged state"
kill -TERM "$PID4"
wait "$PID4" || true
PIDS=("$STORE_PID" "$CACHE_PID")

echo "PASS snapshot.local-merge.sh"
