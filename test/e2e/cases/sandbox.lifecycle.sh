#!/usr/bin/env bash
# Cold sandbox lifecycle contract using only prepared products and the prepared image.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/readiness_helpers.sh"
source "$SANDBOXER_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker ip mkfs.ext4 python3 tar timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -r "$SANDBOXER_LIB/readiness_helpers.sh" ] || e2e_fail "missing prepared readiness helpers"
[ -r "$SANDBOXER_LIB/tarstream.sh" ] || e2e_fail "missing prepared tarstream helpers"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/runtime"
SID="life-$((BASHPID % 1000000))"
TAP_NAME="sb$((BASHPID % 1000000))"
BLK0_IMAGE="$WORK/blk0.img"
DIFF_FILE="$WORK/blk1.diff"
LOG="$OUT/lifecycle.log"
STATS_JSON="$OUT/lifecycle-stats.json"
SBPID=""
POST_PID=""
TAP_CREATED=0
READINESS_WATCHDOG_PID=""

cleanup() {
    local status=$?
    set +e
    readiness_stop_watchdog
    for pid in "$SBPID" "$POST_PID"; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
    done
    [ -n "$SBPID" ] && readiness_kill_session KILL "$SBPID"
    [ -n "$POST_PID" ] && kill -0 "$POST_PID" 2>/dev/null && kill -KILL "$POST_PID" 2>/dev/null
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ip tuntap add dev "$TAP_NAME" mode tap
TAP_CREATED=1
ip addr add 169.254.1.0/31 dev "$TAP_NAME"
ip link set "$TAP_NAME" up

echo "==> flatten prepared image"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
[ -s "$BLK0_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"

truncate -s 1G "$DIFF_FILE"
mkfs.ext4 -q -F -O ^has_journal "$DIFF_FILE"

cat >"$WORK/sandbox.yaml" <<EOF
resources:
  capacity:
    cpu: 1
    memory: 512MiB
  allocatable:
    cpu: 1
    memory: 512MiB
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: e2e-lifecycle
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay:
      diff: file://$DIFF_FILE
launch:
  args:
    - "-c"
    - |
      import os, sys, time
      print('LIFECYCLE-OK', sys.version_info.major*100+sys.version_info.minor, flush=True)
      while not os.path.exists('/tmp/e2e-lifecycle-exit'):
          time.sleep(0.01)
  restart: never
EOF

# Failure before control_ready must close the descriptor without emitting data.
readiness_begin_capture "$WORK/pre-control.ready"
PRE_READER_PID=$READY_READER_PID
set +e
"$BIN/sandbox-ctl" run --ready-fd="$READY_WRITE_FD" \
    --config "$WORK/missing.yaml" --sandbox-id lifecycle-pre-fail \
    --run-root "$WORK/runtime" >"$OUT/pre-control.log" 2>&1
PRE_RC=$?
set -e
readiness_close_parent_writer
[ "$PRE_RC" -ne 0 ] || e2e_fail "invalid config unexpectedly started"
readiness_assert_wire "$WORK/pre-control.ready" "$PRE_READER_PID" "" || \
    e2e_fail "pre-control failure emitted readiness data"

# Failure after control_ready must emit control_ready and then EOF, never ready.
readiness_begin_capture "$WORK/post-control.ready"
POST_READER_PID=$READY_READER_PID
"$BIN/sandbox-ctl" run --ready-fd="$READY_WRITE_FD" \
    --config "$WORK/sandbox.yaml" --sandbox-id lifecycle-post-fail \
    --ch-binary "$WORK/missing-cloud-hypervisor" --run-root "$WORK/runtime" \
    >"$OUT/post-control.log" 2>&1 &
POST_PID=$!
readiness_close_parent_writer
readiness_wait_event "$WORK/post-control.ready" 1 control_ready "$POST_PID" || \
    e2e_fail "post-control failure never emitted control_ready"
set +e
wait "$POST_PID"
POST_RC=$?
set -e
POST_PID=""
[ "$POST_RC" -ne 0 ] || e2e_fail "missing cloud-hypervisor unexpectedly succeeded"
readiness_assert_wire "$WORK/post-control.ready" "$POST_READER_PID" $'control_ready\n' || \
    e2e_fail "post-control failure emitted an invalid readiness wire"

# Real prepared-product cold lifecycle.
readiness_begin_capture "$WORK/lifecycle.ready"
LIFECYCLE_READER_PID=$READY_READER_PID
readiness_exec_in_new_session "$BIN/sandbox-ctl" run \
    --ready-fd="$READY_WRITE_FD" \
    --config "$WORK/sandbox.yaml" \
    --sandbox-id "$SID" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    --stats-json "$STATS_JSON" \
    >"$LOG" 2>&1 &
SBPID=$!
readiness_close_parent_writer
readiness_start_watchdog "$SBPID" 60 10

readiness_wait_event "$WORK/lifecycle.ready" 1 control_ready "$SBPID" || {
    tail -60 "$LOG" >&2 || true
    e2e_fail "control_ready was not observed"
}
readiness_connect_ctl "$WORK/runtime/$SID/ctl.sock" || e2e_fail "ctl.sock is not connectable at control_ready"
readiness_wait_event "$WORK/lifecycle.ready" 2 ready "$SBPID" || {
    tail -60 "$LOG" >&2 || true
    e2e_fail "ready was not observed"
}
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/true || \
    e2e_fail "immediate exec after ready failed"
readiness_assert_wire "$WORK/lifecycle.ready" "$LIFECYCLE_READER_PID" $'control_ready\nready\n' || \
    e2e_fail "cold readiness wire was not exact"

for _ in $(seq 1 1200); do
    grep -qE '^LIFECYCLE-OK [0-9]+$' "$LOG" 2>/dev/null && break
    kill -0 "$SBPID" 2>/dev/null || break
    sleep 0.05
done
grep -qE '^LIFECYCLE-OK [0-9]+$' "$LOG" || {
    tail -60 "$LOG" >&2 || true
    e2e_fail "prepared image application did not execute"
}
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" \
    -- /bin/sh -c 'touch /tmp/e2e-lifecycle-exit' || e2e_fail "could not release lifecycle application"
wait "$SBPID"
SBPID=""
readiness_stop_watchdog

assert_contains "$LOG" 'image config: cmd=[python3]'
[ -s "$STATS_JSON" ] || e2e_fail "stats-json output is missing"
python3 - "$STATS_JSON" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as stream:
    report = json.load(stream)
backends = {backend["name"]: backend for backend in report["backends"]}
assert {"blk0", "blk1"} <= backends.keys(), f"missing backends: {sorted(backends)}"
blk0 = backends["blk0"]
assert blk0["read"]["count"] > 0, "blk0 had no reads"
assert blk0["loaded_blocks"] > 0, "blk0 loaded no blocks"
assert blk0["read"]["p50_ns"] <= blk0["read"]["lat_max_ns"], "blk0 p50 exceeds max"
assert blk0["read"]["p99_ns"] <= blk0["read"]["lat_max_ns"], "blk0 p99 exceeds max"
PY

echo "PASS sandbox.lifecycle.sh"
