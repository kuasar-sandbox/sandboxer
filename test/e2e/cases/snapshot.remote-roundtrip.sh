#!/usr/bin/env bash
# Remote snapshot upload/restore preserves vCPU and disk state and portable refs.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
source "$E2E_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker find grep ip mkfs.ext4 openssl python3 timeout truncate; do
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
TAP_NAME="xr$((BASHPID % 1000000))"
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

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
ROOT_REF=$(plaintext_tarstream_ref "$ROOT_IMAGE")
ROOT_DIFF="$WORK/root.diff"
truncate -s 1G "$ROOT_DIFF"
mkfs.ext4 -q -F -O ^has_journal "$ROOT_DIFF"

cat >"$WORK/cold.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: remote-roundtrip }
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
    local log=$1 want=$2 pid=$3 marker=${4:-TICK00000000}
    for _ in $(seq 1 1200); do
        grep -qE "^TICK $want DISK blk0=$marker$" "$log" 2>/dev/null && return
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

SID1="remote-source-$BASHPID"
LOG1="$OUT/source.log"
"$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --manifest-config "$WORK/manifest.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --sandbox-id "$SID1" >"$LOG1" 2>&1 &
PID1=$!
PIDS+=("$PID1")
wait_tick "$LOG1" 10 "$PID1"
SNAP1=$("$BIN/sandbox-ctl" snapshot --sandbox-id "$SID1" --upload --run-root "$RUN_ROOT" 2>"$OUT/upload.log")
wait "$PID1" 2>/dev/null || true
PIDS=("$STORE_PID")
SNAP1="${SNAP1#manifest://}"
[ "${#SNAP1}" -eq 64 ] || e2e_fail "snapshot upload did not return a manifest key"
SNAP1_TICK=$(last_tick "$LOG1")
"$BIN/manifest-ctl" verify --manifest-config "$WORK/manifest.yaml" "$SNAP1" >/dev/null

write_host_config "$WORK/restore.yaml" "$WORK/restore.diff" remote-restored
SID2="remote-restored-$BASHPID"
LOG2="$OUT/restore.log"
"$BIN/sandbox-ctl" run --restore "manifest://$SNAP1" --config "$WORK/restore.yaml" --manifest-config "$WORK/manifest.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --sandbox-id "$SID2" >"$LOG2" 2>&1 &
PID2=$!
PIDS+=("$PID2")
WANT=$((SNAP1_TICK + 3))
wait_tick "$LOG2" "$WANT" "$PID2"

# A local snapshot of a manifest-backed restore must keep existing remote
# dependencies as refs rather than copying the immutable base into an overlay.
PORTABLE_OUT="$OUT/portable"
PORTABLE_LOCATION="$OUT/location"
LOCATION_NAME="remote-parent"
mkdir -p "$PORTABLE_OUT" "$PORTABLE_LOCATION"
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID2" --output "$PORTABLE_OUT" --resume --run-root "$RUN_ROOT" \
    >"$OUT/portable-snapshot.out" 2>"$OUT/portable-snapshot.log"
PORTABLE_SNAPSHOT="$PORTABLE_OUT/$SID2.snapshot"
[ -e "$PORTABLE_SNAPSHOT" ] || e2e_fail "manifest-backed local snapshot is missing"
if find "$PORTABLE_OUT" -maxdepth 1 -type f -name '*.overlay' -print -quit | grep -q .; then
    e2e_fail "manifest-backed local snapshot copied a portable dependency into .overlay"
fi
LOCATED_REF=$("$BIN/sandbox-ctl" publish --quiet --manifest-config "$WORK/manifest.yaml" \
    --to-ref-location "$LOCATION_NAME=file://$PORTABLE_LOCATION" "$PORTABLE_SNAPSHOT")
case "$LOCATED_REF" in
    file://*.snapshot@digest:*@location:"$LOCATION_NAME") ;;
    *) e2e_fail "non-canonical located snapshot ref: $LOCATED_REF" ;;
esac
if find "$PORTABLE_LOCATION" -maxdepth 1 -type f -name '*.overlay' -print -quit | grep -q .; then
    e2e_fail "named publication copied a portable dependency into .overlay"
fi
[ "$(find "$PORTABLE_LOCATION" -maxdepth 1 -type f -name '*.sandbox' | wc -l)" -eq 1 ] || e2e_fail "named publication must contain exactly one new Sandbox E"
[ "$(find "$PORTABLE_LOCATION" -maxdepth 1 -type f -name '*.snapshot' | wc -l)" -eq 1 ] || e2e_fail "named publication must contain exactly one new Snapshot S"

kill -TERM "$PID2" 2>/dev/null || true
wait "$PID2" 2>/dev/null || true
PIDS=("$STORE_PID")

echo "PASS snapshot.remote-roundtrip.sh"
