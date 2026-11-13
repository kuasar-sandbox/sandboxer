#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","tun","userfaultfd"]
# Snapshot restore resumes guest state with a renamed runtime and no external kernel.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/readiness_helpers.sh"
source "$SANDBOXER_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"
: "${LAUNCH_RECOVERY_BIN:?prepared initial-state probe required}"

require_root
require_kvm
for command in cp docker ip mkfs.ext4 mv python3 rm sha256sum timeout truncate; do
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

# A source cutover can supply the already admitted release as a separately
# sealed fixture. Keep the same-version scenario and replay it with a real
# pre-migration capture. The candidate Host always owns the restore side.
CAPTURE_BIN="$BIN"
released="$E2E_WORKSPACE/fixtures/released-snapshot-capture"
case "${1:-}" in
    "")
        if [ -d "$released" ]; then
            script="$(realpath "${BASH_SOURCE[0]}")"
            WORK="$WORK/n" OUT="$OUT/current" bash "$script" current
            WORK="$WORK/o" OUT="$OUT/from-release" bash "$script" from-release
            exit 0
        fi
        ;;
    current) ;;
    from-release)
        CAPTURE_BIN="$released"
        (cd "$released" && sha256sum --check --strict SHA256SUMS)
        printf 'Capturing with published aggregate %s, restoring with selected Host:\n' "$(cat "$released/aggregate")"
        sha256sum "$CAPTURE_BIN/sandbox-ctl" "$BIN/sandbox-ctl"
        ;;
    *) e2e_fail "unknown snapshot restore scenario: $1" ;;
esac

mkdir -p "$WORK" "$OUT" "$WORK/runtime" "$WORK/capture-runtime" "$WORK/deploy-runtime" "$WORK/snapshot"
TAP_NAME="rs$((BASHPID % 1000000))"
TAP_CREATED=0
SBPID1=""
SBPID2=""
READINESS_WATCHDOG_PID=""
cleanup() {
    local status=$?
    set +e
    readiness_stop_watchdog
    if [ "$status" -ne 0 ] && [ -n "${LOG2:-}" ]; then
        restore_failure_diagnostics || true
    fi
    for pid in "$SBPID1" "$SBPID2"; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
    done
    sleep 0.2
    for pid in "$SBPID1" "$SBPID2"; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
        [ -n "$pid" ] && wait "$pid" 2>/dev/null
    done
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    exit "$status"
}
trap cleanup EXIT

# Read-only diagnostics run only after a failed assertion. Keep the original
# failure and budgets intact; a stalled guest must not stall cleanup. Never dump
# arbitrary application/kernel text into CI logs.
restore_failure_diagnostics() {
    local returncode=running probe_status=not_attempted
    if [ -n "$SBPID2" ] && kill -0 "$SBPID2" 2>/dev/null; then
        timeout --kill-after=1 5 "$BIN/sandbox-ctl" exec \
            --sandbox-id "$SID2" --run-root "$WORK/runtime" -- python3 -c '
import json, os
with open("/tmp/dio-state.json") as source:
    state = json.load(source)
print(json.dumps({"trigger_present": os.path.exists("/tmp/dio-trigger"), "state": state}))
' >"$OUT/dio-failure.json" 2>"$OUT/dio-failure-exec.log"
        probe_status=$?
    elif [ -n "$SBPID2" ]; then
        if wait "$SBPID2"; then returncode=0; else returncode=$?; fi
    fi
    python3 -B "$SANDBOXER_LIB/restore_dio_diagnostics.py" \
        "$LOG2" "$OUT/dio-failure.json" "$returncode" "$probe_status" >&2
}

startup_failure() {
    local log=$1 pid=$2 phase=$3 started=$4 returncode=running
    # A live process may still be draining; never block or change the watchdog.
    if ! kill -0 "$pid" 2>/dev/null; then
        if wait "$pid"; then returncode=0; else returncode=$?; fi
    fi
    python3 -B - "$SANDBOXER_LIB" "$log" "$returncode" "$phase" "$((SECONDS - started))" <<'PY_STARTUP' || true
import json, sys
sys.path.insert(0, sys.argv[1])
from usage import startup_failure_summary
code = None if sys.argv[3] == 'running' else int(sys.argv[3])
summary = startup_failure_summary(sys.argv[2], code, float(sys.argv[5]))
summary.pop('deadline_seconds', None)  # The usage harness's 45s deadline is unrelated.
summary['phase'] = sys.argv[4]
print('snapshot startup: ' + json.dumps(summary, sort_keys=True), file=sys.stderr)
PY_STARTUP
    e2e_fail "$phase run missed readiness"
}

ip tuntap add dev "$TAP_NAME" mode tap
TAP_CREATED=1
ip addr add 169.254.1.0/31 dev "$TAP_NAME"
ip link set "$TAP_NAME" up

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"
DIFF="$WORK/root.diff"
truncate -s 1G "$DIFF.template"
mkfs.ext4 -q -F -O ^has_journal "$DIFF.template"

# Capture with WORK-owned boot files. Restore uses the same runtime content
# under a different basename after both captured paths have been removed.
CAPTURE_KERNEL="$WORK/capture-runtime/vmlinux-v1"
CAPTURE_RUNTIME="$WORK/capture-runtime/runtime-v1.bundle"
DEFAULT_RUNTIME="$WORK/deploy-runtime/runtime-renamed.bundle"
cp --reflink=auto --sparse=always "$CAPTURE_BIN/vmlinux" "$CAPTURE_KERNEL"
cp --reflink=auto --sparse=always "$CAPTURE_BIN/sandbox-runtime.bundle" "$CAPTURE_RUNTIME"

# Retain the primary's mappings and open O_DIRECT descriptor across snapshot.
# JSON is valid YAML and preserves the workload's exact newlines/quoting.
DIO_LAUNCH_ARGS="$(python3 - "$SANDBOXER_LIB/restore_dio_workload.py" <<'PY_DIO'
import json, pathlib, sys
print(json.dumps(["-c", pathlib.Path(sys.argv[1]).read_text()]))
PY_DIO
)"

cat >"$WORK/cold.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-restore }
boot:
  kernel: file://$CAPTURE_KERNEL
  runtime: file://$CAPTURE_RUNTIME
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$DIFF, diff_template: file://$DIFF.template }
launch:
  args: $DIO_LAUNCH_ARGS
  restart: never
  cgroup_control: true
EOF

SID1="restore-cold-$BASHPID"
LOG1="$OUT/cold.log"
readiness_begin_capture "$WORK/cold.ready"
COLD_READER_PID=$READY_READER_PID
STARTUP_STARTED=$SECONDS
readiness_exec_in_new_session "$CAPTURE_BIN/sandbox-ctl" run \
    --ready-fd="$READY_WRITE_FD" --config "$WORK/cold.yaml" \
    --ch-binary "$CAPTURE_BIN/cloud-hypervisor" --run-root "$WORK/runtime" --sandbox-id "$SID1" \
    >"$LOG1" 2>&1 &
SBPID1=$!
readiness_close_parent_writer
readiness_start_watchdog "$SBPID1" 120 10
readiness_wait_event "$WORK/cold.ready" 1 control_ready "$SBPID1" || startup_failure "$LOG1" "$SBPID1" cold-control "$STARTUP_STARTED"
readiness_connect_ctl "$WORK/runtime/$SID1/ctl.sock" || e2e_fail "cold ctl.sock unavailable at control_ready"
readiness_wait_event "$WORK/cold.ready" 2 ready "$SBPID1" || startup_failure "$LOG1" "$SBPID1" cold-ready "$STARTUP_STARTED"
CG_COLD="$("$CAPTURE_BIN/sandbox-ctl" exec --sandbox-id "$SID1" --run-root "$WORK/runtime" -- cat /proc/self/cgroup)"
grep -qE '^0::/init[[:space:]]*$' <<<"$CG_COLD" || e2e_fail "cold exec is not pinned to /init"
readiness_assert_wire "$WORK/cold.ready" "$COLD_READER_PID" $'control_ready\nready\n' || e2e_fail "cold readiness wire is not exact"

for _ in $(seq 1 600); do
    grep -qE '^TICK 10[[:space:]]*$' "$LOG1" 2>/dev/null && break
    kill -0 "$SBPID1" 2>/dev/null || e2e_fail "cold run exited before TICK 10"
    sleep 0.05
done
grep -qE '^TICK 10[[:space:]]*$' "$LOG1" || e2e_fail "cold run never reached TICK 10"
"$CAPTURE_BIN/sandbox-ctl" exec --sandbox-id "$SID1" --run-root "$WORK/runtime" -- cat /tmp/dio-state.json >"$OUT/dio-before.json"
python3 - "$OUT/dio-before.json" <<'PY_DIO'
import json, sys
r = json.load(open(sys.argv[1]))
assert r['phase'] == 'armed' and r['cold_control_mismatches'] == 0 and r['regions'] == 32, r
PY_DIO

"$LAUNCH_RECOVERY_BIN" pong "$WORK/runtime/$SID1/vsock.sock" >"$OUT/initial-before.json"
"$CAPTURE_BIN/sandbox-ctl" snapshot --sandbox-id "$SID1" --output "$WORK/snapshot" --run-root "$WORK/runtime" \
    >"$OUT/snapshot.log" 2>&1
wait "$SBPID1" || true
SBPID1=""
readiness_stop_watchdog
# Capture preparation can advance the guest counter. Sample only after the
# source runner exits and its output is complete; restore must progress from
# that captured state, not from an earlier pre-capture observation.
PRE_SNAP_TICK="$(awk '/^TICK([[:space:]]|$)/ { last=$0 } END { print last }' "$LOG1")"
[[ "$PRE_SNAP_TICK" =~ ^TICK\ ([0-9]+)[[:space:]]*$ ]] || e2e_fail "missing or invalid final cold TICK: $PRE_SNAP_TICK"
PRE_SNAP_TICK="${BASH_REMATCH[1]}"
printf 'Captured cold counter: %s\n' "$PRE_SNAP_TICK"
SNAPSHOT="$WORK/snapshot/$SID1.snapshot"
[ -f "$SNAPSHOT" ] || e2e_fail "snapshot is missing"

mv "$CAPTURE_RUNTIME" "$DEFAULT_RUNTIME"
rm -- "$CAPTURE_KERNEL"
[ ! -e "$CAPTURE_RUNTIME" ] || e2e_fail "captured runtime path still exists"
[ ! -e "$CAPTURE_KERNEL" ] || e2e_fail "captured kernel path still exists"
[ ! -e "$WORK/deploy-runtime/runtime-v1.bundle" ] || e2e_fail "runtime fallback unexpectedly exists"
find "$WORK/snapshot" -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum >"$WORK/source.sha256"

# Keep the legacy kernel binding in YAML; its WORK-owned file no longer exists.
RESTORE_DIFF="$WORK/restore.diff"
cat >"$WORK/restore.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-restore }
boot:
  kernel: file://$CAPTURE_KERNEL
  runtime: file://$DEFAULT_RUNTIME
  root:
    overlay: { diff: file://$RESTORE_DIFF }
EOF
sha256sum "$WORK/restore.yaml" "$DEFAULT_RUNTIME" >"$WORK/host-inputs.sha256"

SID2="restore-live-$BASHPID"
LOG2="$OUT/restore.log"
readiness_begin_capture "$WORK/restore.ready"
RESTORE_READER_PID=$READY_READER_PID
STARTUP_STARTED=$SECONDS
readiness_exec_in_new_session "$BIN/sandbox-ctl" run \
    --ready-fd="$READY_WRITE_FD" --restore "$SNAPSHOT" --config "$WORK/restore.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --sandbox-id "$SID2" \
    >"$LOG2" 2>&1 &
SBPID2=$!
readiness_close_parent_writer
readiness_start_watchdog "$SBPID2" 120 10
readiness_wait_event "$WORK/restore.ready" 1 control_ready "$SBPID2" || startup_failure "$LOG2" "$SBPID2" restore-control "$STARTUP_STARTED"
readiness_connect_ctl "$WORK/runtime/$SID2/ctl.sock" || e2e_fail "restore ctl.sock unavailable at control_ready"
readiness_wait_event "$WORK/restore.ready" 2 ready "$SBPID2" || startup_failure "$LOG2" "$SBPID2" restore-ready "$STARTUP_STARTED"
CG_RESTORE="$("$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$WORK/runtime" -- cat /proc/self/cgroup)"
grep -qE '^0::/init[[:space:]]*$' <<<"$CG_RESTORE" || e2e_fail "restored exec is not pinned to /init"
readiness_assert_wire "$WORK/restore.ready" "$RESTORE_READER_PID" $'control_ready\nready\n' || e2e_fail "restore readiness wire is not exact"

python3 - "$WORK/runtime/$SID2/snap-state/config.json" "$DEFAULT_RUNTIME" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    config = json.load(source)
assert len(config["pmem"]) == 1, config["pmem"]
assert config["pmem"][0]["file"] == sys.argv[2], config["pmem"]
PY
sha256sum --check --status "$WORK/source.sha256" || e2e_fail "restore modified source snapshot artifacts"
sha256sum --check --status "$WORK/host-inputs.sha256" || e2e_fail "restore modified host inputs"

"$LAUNCH_RECOVERY_BIN" pong "$WORK/runtime/$SID2/vsock.sock" >"$OUT/initial-after.json"
python3 - "$OUT/initial-before.json" "$OUT/initial-after.json" <<'PY_INITIAL'
import json,sys
before,after=[json.load(open(p)) for p in sys.argv[1:]]
assert before and before["token"] and before["state"]=="started", before
assert after==before,(before,after)
PY_INITIAL

# A separate native exec triggers the retained primary only after restore.
"$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$WORK/runtime" -- touch /tmp/dio-trigger
for _ in $(seq 1 600); do
    grep -qE '^DIO-COMPLETE[[:space:]]*$' "$LOG2" 2>/dev/null && break
    kill -0 "$SBPID2" 2>/dev/null || e2e_fail "restored O_DIRECT workload exited"
    sleep 0.05
done
grep -qE '^DIO-COMPLETE[[:space:]]*$' "$LOG2" || e2e_fail "restored O_DIRECT integrity check did not complete"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$WORK/runtime" -- cat /tmp/dio-state.json >"$OUT/dio-after.json"
python3 - "$OUT/dio-after.json" <<'PY_DIO'
import json, sys
r = json.load(open(sys.argv[1]))
assert r['phase'] == 'complete' and r['cold_control_mismatches'] == 0, r
assert r['mismatches'] == [0] * 16 and r['warm_mismatches'] == [0] * 16 and r['corrupt_regions'] == 0, r
PY_DIO

WANT_TICK=$((PRE_SNAP_TICK + 3))
for _ in $(seq 1 600); do
    grep -qE "^TICK $WANT_TICK[[:space:]]*$" "$LOG2" 2>/dev/null && break
    kill -0 "$SBPID2" 2>/dev/null || e2e_fail "restored run exited before counter resumed"
    sleep 0.05
done
grep -qE "^TICK $WANT_TICK[[:space:]]*$" "$LOG2" || e2e_fail "restored counter did not continue past captured state"

kill -TERM "$SBPID2"
wait "$SBPID2" || true
SBPID2=""
readiness_stop_watchdog

echo "PASS snapshot.restore.sh"
