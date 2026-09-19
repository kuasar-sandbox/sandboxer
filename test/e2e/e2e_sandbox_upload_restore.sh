#!/usr/bin/env bash
#
# e2e_sandbox_upload_restore.sh — full snapshot upload + restore manifest://
# round-trip:
#
#   1. Spin up store-ctl + cache-ctl tiered (rocksdb L1 + store origin)
#   2. Cold-start sandbox running a python TICK counter that also writes a
#      fresh 4 KiB block per tick to /ticks.dat (blk1 overlay) and reads back
#      block 0 → "TICK <i> DISK blk0=<marker>"
#   3. Wait for TICK 10 (with blk0=cold marker) in stdout, snapshot --upload.
#   4. run --restore manifest://<hex>; verify TICK > snapshot tick (vCPU
#      resumed) AND blk0 still reads the cold marker (disk fall-through).
#   5. Snapshot the manifest-backed sandbox locally and publish it to a named
#      location; verify portable dependencies do not create a redundant overlay.
#   6. Snapshot the restored sandbox again → snap#2 (from_refs=[snap#1]),
#      restore it: the incremental 2-layer chain (mem + disk overlay)
#   7. Snapshot THAT chained-restored sandbox → snap#3 (3-layer chain),
#      restore it: validates the recurring quiesce teardown + deep-chain
#      mem/disk fall-through (asserts restore builds 3 memory layers)
#   8. Print perf metrics: dedup ratio, lazy-load ratio, restore wallclock

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
. "$SCRIPT_DIR/lib/uffd_performance_gate.sh"
BIN="${BIN:-$REPO_ROOT/bin}"
IMAGE="${IMAGE:-python:3.12-slim}"

skip() {
    echo
    echo "==> e2e_sandbox_upload_restore: skipping ($*)"
    if [ "${REQUIRE_KVM:-0}" = "1" ]; then exit 1; fi
    exit 0
}

[ -e /dev/kvm ] || skip "/dev/kvm not present"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || skip "/dev/kvm not accessible"

for b in cloud-hypervisor sandbox-ctl sandbox-init sandbox-runtime.bundle flatten-ctl manifest-ctl store-ctl cache-ctl; do
    [ -e "$BIN/$b" ] || skip "missing $BIN/$b — run 'make build'"
done
VMLINUX="${VMLINUX:-$BIN/vmlinux}"
[ -f "$VMLINUX" ] || skip "no vmlinux at $VMLINUX"
# Self-elevate: tap creation, cgroup writes, vsock all need root. Done
# here (after prereq checks) so /dev/kvm-missing and missing-binary cases
# still fast-fail without prompting for sudo.
if [ "$(id -u)" -ne 0 ]; then
    exec sudo -nE "$0" "$@"
fi

TAP_NAME="${TAP_NAME:-sb-tap0}"
TAP_CREATED_BY_TEST=0
if ! ip link show "$TAP_NAME" >/dev/null 2>&1; then
    [ "$(id -u)" -eq 0 ] || { echo "$0: must run as root to create $TAP_NAME" >&2; exit 1; }
    ip tuntap add dev "$TAP_NAME" mode tap
    ip addr add 169.254.1.0/31 dev "$TAP_NAME"
    ip link set "$TAP_NAME" up
    TAP_CREATED_BY_TEST=1
fi
[ "$(id -u)" -eq 0 ] || skip "must run as root (cgroup + uffd)"

WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/e2e-snap-upload-XXXXXX")"
PIDS=()
cleanup() {
    for pid in "${PIDS[@]}"; do
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    if [ -n "${E2E_KEEP:-}" ]; then echo "kept: $WORK"; else rm -rf "$WORK"; fi
    [ "$TAP_CREATED_BY_TEST" = "1" ] && ip link del "$TAP_NAME" 2>/dev/null || true
}
trap cleanup EXIT

free_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1]); s.close()'
}
KEY=$(openssl rand -hex 32)

# ---- daemons -------------------------------------------------------------

echo "==> spin up store-ctl"
STORE_PORT=$(free_port)
cat > "$WORK/store-ctl.yaml" <<EOF
listen: 127.0.0.1:$STORE_PORT
backend: fs
fs:
  root: $WORK/store-data
  verify_content_key: true
EOF
"$BIN/store-ctl" init --config "$WORK/store-ctl.yaml" --generation G1
"$BIN/store-ctl" serve --config "$WORK/store-ctl.yaml" >"$WORK/store.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 50); do
    if (echo >/dev/tcp/127.0.0.1/$STORE_PORT) 2>/dev/null; then break; fi
    sleep 0.1
done

echo "==> spin up cache-ctl (tiered: rocksdb L1 + store origin)"
CACHE_PORT=$(free_port)
CACHE_HEALTH_PORT=$(free_port)
cat > "$WORK/cache-ctl.yaml" <<EOF
mode: tiered
listen: 127.0.0.1:$CACHE_PORT
health_listen: 127.0.0.1:$CACHE_HEALTH_PORT
rpc_timeout: 5s
freq:
  counters: 1M
  reset_after: 100K
tiers:
  - type: embedded
    rocks:
      path: $WORK/cache-rocks
      disk_bytes: 2GiB
      mem_ratio: 0.1
      direct_reads: false
      bloom_bits: 10
origin:
  type: store
  store:
    endpoint: 127.0.0.1:$STORE_PORT
    pool: 2
    timeout: 5s
  max_inflight: 16
EOF
"$BIN/cache-ctl" serve --config "$WORK/cache-ctl.yaml" >"$WORK/cache.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 50); do
    if "$BIN/cache-ctl" ping --endpoint "127.0.0.1:$CACHE_HEALTH_PORT" 2>/dev/null | grep -q SERVING; then break; fi
    sleep 0.1
done
echo "==> store=127.0.0.1:$STORE_PORT cache=127.0.0.1:$CACHE_PORT"

cat > "$WORK/accelerator.yaml" <<EOF
manifest:
  key: "$KEY"
store:
  endpoint: 127.0.0.1:$STORE_PORT
  pool: 4
  timeout: 30s
cache:
  endpoint: 127.0.0.1:$CACHE_PORT
  pool: 4
  timeout: 10s
chunker:
  mode: cdc
crypto:
  chunk: aes
  manifest: aes
EOF

# ---- prepare blk0 ------------------------------------------------------

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    docker pull "$IMAGE" >/dev/null
fi
BLK0_EROFS="$WORK/blk0.img"
docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_EROFS" --no-progress
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_EROFS")"

mkdir -p "$WORK/runtime"
DIFF_FILE="$WORK/runtime/blk1.diff"
truncate -s 1G "$DIFF_FILE"
mkfs.ext4 -q -F "$DIFF_FILE"

# Long-running TICK counter that also exercises the disk overlay layer:
# each tick writes a fresh 4 KiB block at offset i*4096 of /ticks.dat (on the
# blk1 ext4 upper → CoW diff, captured into each snapshot's overlay) and reads
# back block 0 (written at cold tick 0). Per tick it prints
# "TICK <i> DISK blk0=<marker>". Different runs touch different block ranges,
# so each snapshot's overlay is non-empty and holds distinct blocks; reading
# block 0 after a chained restore must fall through every overlay layer to the
# deepest (snap#1) — blk0=TICK00000000 proves the layered disk read + vCPU
# continuity together. quiesce drop_caches forces post-restore reads off disk.
cat > "$WORK/sandbox.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: e2e-upload
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay:
      diff: file://$DIFF_FILE
launch:
  args: ["-c", "import os,time\nprint('PYBOOT-OK', flush=True)\nfd=os.open('/ticks.dat', os.O_RDWR|os.O_CREAT, 0o644)\ni=0\nwhile True:\n    os.pwrite(fd, ('TICK%08d' % i).encode().ljust(4096, b'.'), i*4096)\n    os.fsync(fd)\n    blk0=os.pread(fd, 12, 0).decode()\n    print('TICK %d DISK blk0=%s' % (i, blk0), flush=True)\n    i+=1\n    time.sleep(0.25)"]
  restart: never
  cgroup_control: true
EOF

assert_cgroup_init() {
    local sid=$1 label=$2 got
    got=$("$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$WORK/runtime" -- cat /proc/self/cgroup)
    grep -qE '^0::/init[[:space:]]*$' <<<"$got" || {
        echo "FAIL: $label native exec cgroup=$got, want 0::/init"
        exit 1
    }
    echo "==> $label native exec joined pinned cgroup namespace at /init"
}

# Read only after the source process has exited and drained its final output.
snapshot_tick() {
    local tick
    if ! tick=$(awk -v marker="$BLK0_OK" '
        /^TICK([[:space:]]|$)/ { last = $0 }
        END {
            n = split(last, fields, " ")
            if (n != 4 || fields[2] !~ /^[0-9]+$/ ||
                last != "TICK " fields[2] " " marker) exit 1
            print fields[2]
        }' "$1"); then
        echo "FAIL: missing or invalid final TICK/cold-disk record in $1" >&2
        return 1
    fi
    printf '%s\n' "$tick"
}

# ---- run sandbox + first snapshot --upload --------------------------------

echo
# block 0 of /ticks.dat (written at cold tick 0) must read back as this through
# every restore — asserting the disk overlay layered read (fall-through to the
# deepest layer) alongside vCPU continuity. Matched on the same "TICK <n>" line.
BLK0_OK="DISK blk0=TICK00000000"
echo "==> phase 1: cold-start sandbox + run TICK counter (writes /ticks.dat)"
LOG1="$WORK/run1.log"
SID1="up1-$$"
mkdir -p "$WORK/runtime/$SID1"
"$BIN/sandbox-ctl" run \
    --config "$WORK/sandbox.yaml" \
    --manifest-config "$WORK/accelerator.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    --sandbox-id "$SID1" \
    > "$LOG1" 2>&1 &
SBPID1=$!
PIDS+=($SBPID1)

for _ in $(seq 1 600); do
    if grep -qE "^TICK 10 $BLK0_OK$" "$LOG1" 2>/dev/null; then break; fi
    if ! kill -0 "$SBPID1" 2>/dev/null; then
        echo "FAIL: sandbox exited early"; tail -40 "$LOG1"; exit 1
    fi
    sleep 0.05
done
PRE_SNAP_TICK=$(grep -oE "^TICK [0-9]+" "$LOG1" | tail -1 | awk '{print $2}')
assert_cgroup_init "$SID1" "cold"
echo "==> guest at TICK $PRE_SNAP_TICK; taking snapshot --upload"

SNAP1_LOG="$WORK/snap1.log"
T_UP1_BEG=$(date +%s%N)
SNAP_MKEY=$("$BIN/sandbox-ctl" snapshot \
    --sandbox-id "$SID1" \
    --upload \
    --run-root "$WORK/runtime" 2>"$SNAP1_LOG")
T_UP1_END=$(date +%s%N)
UP1_MS=$(( (T_UP1_END - T_UP1_BEG) / 1000000 ))

[ ${#SNAP_MKEY} -eq 64 ] || { echo "FAIL: snapshot manifest key length=${#SNAP_MKEY}, want 64"; cat "$SNAP1_LOG"; exit 1; }
echo "==> upload OK in ${UP1_MS} ms; snapshot manifest key=$SNAP_MKEY"
cat "$SNAP1_LOG" | sed 's/^/    /'

# --resume=false (default) shuts CH down; sandbox-ctl run1 returns naturally.
wait "$SBPID1" 2>/dev/null || true
SNAP1_TICK=$(snapshot_tick "$LOG1")

# ---- restore from manifest:// ------------------------------------------------

echo
echo "==> phase 2: restore from manifest://$SNAP_MKEY"
DIFF_RESTORE="$WORK/runtime/blk1-restore.diff"
# Restore diff is an EMPTY CoW upper — the filesystem comes from the snapshot's
# overlay base; do NOT mkfs (a fresh ext4 superblock/UUID would shadow the base
# and diverge from the guest's in-memory ext4 state).
truncate -s 1G "$DIFF_RESTORE"

# Restore host yaml contains only node bindings and fresh instance identity.
# Snapshot S follows sandbox_ref to E; E owns launch and the disk graph.
cat > "$WORK/host.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: e2e-restore
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff: file://$DIFF_RESTORE
EOF

LOG2="$WORK/run2.log"
SID2="up2-$$"
mkdir -p "$WORK/runtime/$SID2"
T_RES_BEG=$(date +%s%N)
"$BIN/sandbox-ctl" run \
    --restore "manifest://$SNAP_MKEY" \
    --config "$WORK/host.yaml" \
    --manifest-config "$WORK/accelerator.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    --sandbox-id "$SID2" \
    --stats-json "$WORK/stats2.json" \
    > "$LOG2" 2>&1 &
SBPID2=$!
PIDS+=($SBPID2)

WANT_TICK=$((SNAP1_TICK + 3))
T_FIRST_TICK_NS=""
for _ in $(seq 1 600); do
    if grep -qE "^TICK $WANT_TICK $BLK0_OK$" "$LOG2" 2>/dev/null; then
        T_FIRST_TICK_NS=$(date +%s%N)
        break
    fi
    if ! kill -0 "$SBPID2" 2>/dev/null; then
        echo "FAIL: restore sandbox exited early"; tail -50 "$LOG2"; exit 1
    fi
    sleep 0.05
done

if [ -z "$T_FIRST_TICK_NS" ]; then
    echo "FAIL: did not see 'TICK $WANT_TICK $BLK0_OK' (vCPU continuity or disk fall-through broken)"; tail -40 "$LOG2"
    kill -TERM "$SBPID2" 2>/dev/null
    exit 1
fi
RESTORE_MS=$(( (T_FIRST_TICK_NS - T_RES_BEG) / 1000000 ))
assert_cgroup_init "$SID2" "restore #1"
echo "==> restore + TICK $WANT_TICK (disk blk0 OK) seen in ${RESTORE_MS} ms"

# A snapshot taken from a manifest-backed restore must retain the remote root
# image and parent layers as portable refs. It therefore emits only the new E/S
# carriers locally, and publishing those carriers into a named location must
# not copy the immutable root image into a redundant .overlay file.
echo
echo "==> phase 2b: manifest-backed local/named snapshot keeps portable dependencies"
PORTABLE_OUT="$WORK/portable-snapshot"
PORTABLE_LOCATION="$WORK/portable-location"
PORTABLE_LOCATION_NAME="manifest-parent-e2e"
mkdir -p "$PORTABLE_OUT" "$PORTABLE_LOCATION"
"$BIN/sandbox-ctl" snapshot \
    --sandbox-id "$SID2" \
    --output "$PORTABLE_OUT" \
    --resume \
    --run-root "$WORK/runtime" >"$WORK/portable-snapshot.log" 2>&1
PORTABLE_SNAPSHOT="$PORTABLE_OUT/$SID2.snapshot"
[ -e "$PORTABLE_SNAPSHOT" ] || {
    echo "FAIL: manifest-backed local snapshot is missing"; ls -la "$PORTABLE_OUT"; exit 1;
}
if find "$PORTABLE_OUT" -maxdepth 1 -type f -name '*.overlay' -print -quit | grep -q .; then
    echo "FAIL: manifest-backed local snapshot copied a portable dependency into .overlay"
    find "$PORTABLE_OUT" -maxdepth 1 -type f -print
    exit 1
fi
PORTABLE_LOCATED_REF=$("$BIN/sandbox-ctl" publish --quiet \
    --manifest-config "$WORK/accelerator.yaml" \
    --to-ref-location "$PORTABLE_LOCATION_NAME=file://$PORTABLE_LOCATION" \
    "$PORTABLE_SNAPSHOT")
case "$PORTABLE_LOCATED_REF" in
    file://*.snapshot@digest:*@location:"$PORTABLE_LOCATION_NAME") ;;
    *) echo "FAIL: non-canonical located snapshot ref: $PORTABLE_LOCATED_REF"; exit 1 ;;
esac
if find "$PORTABLE_LOCATION" -maxdepth 1 -type f -name '*.overlay' -print -quit | grep -q .; then
    echo "FAIL: named publication copied a portable dependency into .overlay"
    find "$PORTABLE_LOCATION" -maxdepth 1 -type f -print
    exit 1
fi
[ "$(find "$PORTABLE_LOCATION" -maxdepth 1 -type f -name '*.sandbox' | wc -l)" -eq 1 ] || {
    echo "FAIL: named publication did not contain exactly one new Sandbox E"; ls -la "$PORTABLE_LOCATION"; exit 1;
}
[ "$(find "$PORTABLE_LOCATION" -maxdepth 1 -type f -name '*.snapshot' | wc -l)" -eq 1 ] || {
    echo "FAIL: named publication did not contain exactly one new Snapshot S"; ls -la "$PORTABLE_LOCATION"; exit 1;
}
echo "==> PASS: manifest/located dependencies remained refs; no redundant .overlay was created"

# ---- second snapshot --upload (dedup pass) ------------------------------

echo
echo "==> phase 3: second snapshot --upload (dedup measurement)"
SNAP2_LOG="$WORK/snap2.log"
T_UP2_BEG=$(date +%s%N)
SNAP2_MKEY=$("$BIN/sandbox-ctl" snapshot \
    --sandbox-id "$SID2" \
    --upload \
    --run-root "$WORK/runtime" 2>"$SNAP2_LOG")
T_UP2_END=$(date +%s%N)
UP2_MS=$(( (T_UP2_END - T_UP2_BEG) / 1000000 ))

[ ${#SNAP2_MKEY} -eq 64 ] || { echo "FAIL: snapshot 2 manifest key length=${#SNAP2_MKEY}"; cat "$SNAP2_LOG"; exit 1; }
echo "==> upload #2 OK in ${UP2_MS} ms; snapshot manifest key=$SNAP2_MKEY"
cat "$SNAP2_LOG" | sed 's/^/    /'

# --resume=false destroys sandbox; wait for the final source output first.
wait "$SBPID2" 2>/dev/null || true
SNAP2_TICK=$(snapshot_tick "$LOG2")
uffd_performance_gate "manifest-restore-1-layer" "$RESTORE_MS" 3000 \
    "$WORK/stats2.json" buffered

# ---- phase 4: restore from the CHAINED snapshot -------------------------
# snap#2 was taken from the restored sandbox. S2.from_refs carries only the
# memory chain, while E2 independently carries the current disk graph.

echo
echo "==> phase 4: restore from chained manifest://$SNAP2_MKEY (snap2 over snap1; snap2 frozen at TICK $SNAP2_TICK)"
DIFF_RESTORE2="$WORK/runtime/blk1-restore2.diff"
# Empty CoW upper (see phase 2): no mkfs — fs comes from the layered overlay base.
truncate -s 1G "$DIFF_RESTORE2"
cat > "$WORK/host2.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: e2e-restore2
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff: file://$DIFF_RESTORE2
EOF

LOG3="$WORK/run3.log"
SID3="up3-$$"
mkdir -p "$WORK/runtime/$SID3"
T_RES2_BEG=$(date +%s%N)
"$BIN/sandbox-ctl" run \
    --restore "manifest://$SNAP2_MKEY" \
    --config "$WORK/host2.yaml" \
    --manifest-config "$WORK/accelerator.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    --sandbox-id "$SID3" \
    --stats-json "$WORK/stats3.json" \
    > "$LOG3" 2>&1 &
SBPID3=$!
PIDS+=($SBPID3)

WANT_TICK2=$((SNAP2_TICK + 3))
T2_NS=""
for _ in $(seq 1 600); do
    if grep -qE "^TICK $WANT_TICK2 $BLK0_OK$" "$LOG3" 2>/dev/null; then
        T2_NS=$(date +%s%N)
        break
    fi
    if ! kill -0 "$SBPID3" 2>/dev/null; then
        echo "FAIL: chained-restore sandbox exited early"; tail -50 "$LOG3"; exit 1
    fi
    sleep 0.05
done
if [ -z "$T2_NS" ]; then
    echo "FAIL: chained restore did not reach 'TICK $WANT_TICK2 $BLK0_OK' (mem or disk fall-through broken?)"; tail -40 "$LOG3"
    kill -TERM "$SBPID3" 2>/dev/null
    exit 1
fi
# Chain depth: snap#2 carries from_refs=[snap#1], so the restore builds a
# 2-layer memory source (asserts the chain is used, not a flattened bundle).
grep -qE "snapshot source: 2 memory layer\(s\)" "$LOG3" || {
    echo "FAIL: phase-4 restore was not 2-layer"; grep -E 'memory layer' "$LOG3" | grep -ivE faulty; kill -TERM "$SBPID3" 2>/dev/null; exit 1; }
RESTORE2_MS=$(( (T2_NS - T_RES2_BEG) / 1000000 ))
assert_cgroup_init "$SID3" "restore #2"
echo "==> PASS: chained restore reached TICK $WANT_TICK2 in ${RESTORE2_MS} ms (2-layer chain; disk blk0 fall-through OK)"
# Leave SID3 running — phase 5 snapshots it into a 3-layer chain.

# ---- phase 5: snapshot the chained-restored sandbox → 3-layer chain ------
# Snapshotting SID3 (itself a chained restore) re-exercises the deterministic
# quiesce teardown (§4.6) on a chained-restored VM, and produces snap#3 with
# S3.from_refs=[S2,S1]. E3 separately records the current disk graph; S never
# duplicates disk provenance. Restoring S3 must build three memory layers and
# reconstruct disks from E3 while remaining byte-correct.
echo
echo "==> phase 5: snapshot chained-restored SID3 → snap#3, then restore the 3-layer chain"
SNAP3_LOG="$WORK/snap3.log"
SNAP3_MKEY=$("$BIN/sandbox-ctl" snapshot \
    --sandbox-id "$SID3" \
    --upload \
    --run-root "$WORK/runtime" 2>"$SNAP3_LOG")
[ ${#SNAP3_MKEY} -eq 64 ] || { echo "FAIL: snapshot 3 manifest key length=${#SNAP3_MKEY}"; cat "$SNAP3_LOG"; exit 1; }
cat "$SNAP3_LOG" | sed 's/^/    /'
wait "$SBPID3" 2>/dev/null || true  # snapshot --resume=false destroyed SID3
SNAP3_TICK=$(snapshot_tick "$LOG3")
echo "==> upload #3 OK; snap#3 key=$SNAP3_MKEY (frozen at TICK $SNAP3_TICK)"
uffd_performance_gate "manifest-restore-2-layer" "$RESTORE2_MS" 3000 \
    "$WORK/stats3.json" buffered

DIFF_RESTORE3="$WORK/runtime/blk1-restore3.diff"
truncate -s 1G "$DIFF_RESTORE3"     # empty CoW upper; fs comes from the 3-layer overlay base
cat > "$WORK/host3.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: e2e-restore3
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff: file://$DIFF_RESTORE3
EOF
LOG4="$WORK/run4.log"
SID4="up4-$$"
mkdir -p "$WORK/runtime/$SID4"
T_RES3_BEG=$(date +%s%N)
"$BIN/sandbox-ctl" run \
    --restore "manifest://$SNAP3_MKEY" \
    --config "$WORK/host3.yaml" \
    --manifest-config "$WORK/accelerator.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    --sandbox-id "$SID4" \
    --stats-json "$WORK/stats4.json" \
    > "$LOG4" 2>&1 &
SBPID4=$!
PIDS+=($SBPID4)
WANT_TICK3=$((SNAP3_TICK + 3))
T3_NS=""
for _ in $(seq 1 600); do
    if grep -qE "^TICK $WANT_TICK3 $BLK0_OK$" "$LOG4" 2>/dev/null; then T3_NS=$(date +%s%N); break; fi
    if ! kill -0 "$SBPID4" 2>/dev/null; then echo "FAIL: 3-layer restore sandbox exited early"; tail -50 "$LOG4"; exit 1; fi
    sleep 0.05
done
if [ -z "$T3_NS" ]; then
    echo "FAIL: 3-layer chain did not reach 'TICK $WANT_TICK3 $BLK0_OK' (deep-chain mem/disk fall-through broken?)"; tail -40 "$LOG4"
    kill -TERM "$SBPID4" 2>/dev/null
    exit 1
fi
# The chain must have ACCUMULATED to depth 3 (snap#3→snap#2→snap#1), not flattened.
grep -qE "snapshot source: 3 memory layer\(s\)" "$LOG4" || {
    echo "FAIL: snap#3 restore was not 3-layer (chain flattened/lost?)"; grep -E 'memory layer' "$LOG4" | grep -ivE faulty; kill -TERM "$SBPID4" 2>/dev/null; exit 1; }
RESTORE3_MS=$(( (T3_NS - T_RES3_BEG) / 1000000 ))
assert_cgroup_init "$SID4" "restore #3"
echo "==> PASS: 3-layer chained restore reached TICK $WANT_TICK3 in ${RESTORE3_MS} ms (snap3→snap2→snap1; disk blk0 fall-through through 3 layers)"
kill -TERM "$SBPID4" 2>/dev/null
wait "$SBPID4" 2>/dev/null || true
uffd_performance_gate "manifest-restore-3-layer" "$RESTORE3_MS" 3000 \
    "$WORK/stats4.json" buffered

# ---- perf summary --------------------------------------------------------

echo
echo "==> perf summary"
echo "    TICK at snapshot:                 $SNAP1_TICK"
echo "    TICK after restore:               $WANT_TICK (delta=+3, vCPU continuity confirmed)"
echo "    snapshot --upload #1 wallclock:  ${UP1_MS} ms"
echo "    restore manifest:// → first TICK: ${RESTORE_MS} ms"
echo "    snapshot --upload #2 wallclock:  ${UP2_MS} ms (same content, expect high dedup)"
echo "    chained restore (snap2→snap1):    TICK $WANT_TICK2 in ${RESTORE2_MS} ms (2-layer; disk blk0 fall-through OK)"
echo "    chained restore (snap3→snap2→snap1): TICK $WANT_TICK3 in ${RESTORE3_MS} ms (3-layer; disk blk0 fall-through OK)"
echo
echo "    snap1 snapshot dedup: $(grep -oE 'snapshot stored=[0-9]+ dedup=[0-9]+' "$SNAP1_LOG" | head -1)"
echo "    snap1 overlay dedup:  $(grep -oE 'overlay stored=[0-9]+ dedup=[0-9]+' "$SNAP1_LOG" | head -1)"
echo "    snap2 snapshot dedup: $(grep -oE 'snapshot stored=[0-9]+ dedup=[0-9]+' "$SNAP2_LOG" | head -1)"
echo "    snap2 overlay dedup:  $(grep -oE 'overlay stored=[0-9]+ dedup=[0-9]+' "$SNAP2_LOG" | head -1)"
echo "    snap3 snapshot dedup: $(grep -oE 'snapshot stored=[0-9]+ dedup=[0-9]+' "$SNAP3_LOG" | head -1)"
echo "    snap3 overlay dedup:  $(grep -oE 'overlay stored=[0-9]+ dedup=[0-9]+' "$SNAP3_LOG" | head -1)"


# ---- reduced-root recovery with the historical manifests retired ----------
echo "==> phase 6: publish the complete memory/disk chain and retire its old roots"
REDUCED_DIR="$WORK/reduced-location"
REDUCED_LOCATION="reduced-e2e"
mkdir -p "$REDUCED_DIR"
"$BIN/sandbox-ctl" info --json --manifest-config "$WORK/accelerator.yaml" \
    "manifest://$SNAP3_MKEY" > "$WORK/reduction-original.json"
REDUCED_REF=$("$BIN/sandbox-ctl" publish --quiet --reduce-ref=any \
    --manifest-config "$WORK/accelerator.yaml" \
    --to-ref-location "$REDUCED_LOCATION=file://$REDUCED_DIR" \
    "manifest://$SNAP3_MKEY")
"$BIN/sandbox-ctl" info --json --manifest-config "$WORK/accelerator.yaml" \
    --ref-location "$REDUCED_LOCATION=file://$REDUCED_DIR" \
    "$REDUCED_REF" > "$WORK/reduction-result.json"
python3 - "$WORK/reduction-original.json" "$WORK/reduction-result.json" \
    "$WORK/store-data/manifest" "$WORK/retired-manifests" "$SNAP3_MKEY" <<'PY'
import json, pathlib, re, sys
original, result = (json.load(open(path)) for path in sys.argv[1:3])
refs = {"manifest://" + sys.argv[5]}
def walk(value, original):
    if isinstance(value, dict):
        for key, item in value.items():
            field = key.lower().replace("_", "")
            if field in ("fromrefs", "basefromrefs"):
                if original:
                    refs.update(item or [])
                elif item:
                    raise SystemExit("reduced result retains a lower-reference list")
            if original and field == "sandboxref" and isinstance(item, str):
                refs.add(item)
            walk(item, original)
    elif isinstance(value, list):
        for item in value:
            walk(item, original)
walk(original, True)
walk(result, False)
root, retired = pathlib.Path(sys.argv[3]), pathlib.Path(sys.argv[4])
if root.name != "manifest" or not root.is_dir():
    raise SystemExit("test Manifest partition is unavailable")
retired.mkdir()
moved = 0
for ref in sorted(refs):
    if not isinstance(ref, str) or not ref.startswith("manifest://"):
        continue
    key = ref.removeprefix("manifest://")
    if not re.fullmatch(r"[0-9a-f]{64}", key):
        raise SystemExit("invalid historical Manifest reference")
    paths = list(root.glob(f"*/{key[:2]}/{key[2:4]}/{key}"))
    if not paths:
        raise SystemExit("historical Manifest fixture was not stored")
    for path in paths:
        if not path.is_file() or path.is_symlink():
            raise SystemExit("unexpected test Manifest object type")
        path.rename(retired / f"{key}-{moved}")
        moved += 1
print(f"==> retired {moved} historical Manifest objects; reduced lists are empty")
PY
# Read the retained immutable base directly from Store. Old roots cannot be
# hidden by the test cache's previously warmed entries.
cat > "$WORK/reduction-reader.yaml" <<EOF
manifest:
  key: "$KEY"
store:
  endpoint: 127.0.0.1:$STORE_PORT
  pool: 4
  timeout: 30s
crypto:
  chunk: aes
  manifest: aes
EOF
REDUCED_TICK=$(grep -oE '^TICK [0-9]+' "$LOG3" | tail -1 | awk '{print $2}')
REDUCED_NEXT_REF="$REDUCED_REF"
for cycle in 1 2; do
    REDUCED_DIFF="$WORK/runtime/reduced-$cycle.diff"
    truncate -s 1G "$REDUCED_DIFF"
    sed "s|$DIFF_RESTORE3|$REDUCED_DIFF|" "$WORK/host3.yaml" > "$WORK/reduced-host-$cycle.yaml"
    REDUCED_ID="reduced-$cycle-$$"
    REDUCED_LOG="$WORK/reduced-run-$cycle.log"
    "$BIN/sandbox-ctl" run --restore "$REDUCED_NEXT_REF" \
        --config "$WORK/reduced-host-$cycle.yaml" \
        --manifest-config "$WORK/reduction-reader.yaml" \
        --ref-location "$REDUCED_LOCATION=file://$REDUCED_DIR" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" \
        --sandbox-id "$REDUCED_ID" > "$REDUCED_LOG" 2>&1 &
    REDUCED_PID=$!
    PIDS+=("$REDUCED_PID")
    REDUCED_WANT=$((REDUCED_TICK + 3))
    for _ in $(seq 1 600); do
        grep -qE "^TICK $REDUCED_WANT $BLK0_OK$" "$REDUCED_LOG" 2>/dev/null && break
        if ! kill -0 "$REDUCED_PID" 2>/dev/null; then
            echo "FAIL: reduced restore $cycle exited"; tail -50 "$REDUCED_LOG"; exit 1
        fi
        sleep 0.05
    done
    grep -qE "^TICK $REDUCED_WANT $BLK0_OK$" "$REDUCED_LOG" || {
        echo "FAIL: reduced restore $cycle lost process/disk continuity"; tail -50 "$REDUCED_LOG"; exit 1;
    }
    if [ "$cycle" = 1 ]; then
        grep -qE 'snapshot source: 1 memory layer\(s\)' "$REDUCED_LOG" || {
            echo "FAIL: reduced Snapshot did not restore as one memory layer"; exit 1;
        }
        mkdir -p "$WORK/reduced-roundtrip"
        "$BIN/sandbox-ctl" snapshot --sandbox-id "$REDUCED_ID" \
            --output "$WORK/reduced-roundtrip" --run-root "$WORK/runtime" \
            > "$WORK/reduced-resnapshot.log" 2>&1
        wait "$REDUCED_PID" 2>/dev/null || true
        REDUCED_TICK=$(grep -oE '^TICK [0-9]+' "$REDUCED_LOG" | tail -1 | awk '{print $2}')
        REDUCED_NEXT_REF="$WORK/reduced-roundtrip/$REDUCED_ID.snapshot"
    else
        kill -TERM "$REDUCED_PID" 2>/dev/null
        wait "$REDUCED_PID" 2>/dev/null || true
    fi
    echo "==> PASS: reduced restore $cycle kept process state and deepest disk data"
done

echo
echo "==> e2e_sandbox_upload_restore: OK"
