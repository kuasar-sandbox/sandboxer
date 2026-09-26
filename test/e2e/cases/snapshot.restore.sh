#!/usr/bin/env bash
# Snapshot restore resumes guest state and resolves the captured runtime by sibling fallback.
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
for command in cp docker ip mkfs.ext4 python3 sha256sum truncate; do
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

ip tuntap add dev "$TAP_NAME" mode tap
TAP_CREATED=1
ip addr add 169.254.1.0/31 dev "$TAP_NAME"
ip link set "$TAP_NAME" up

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"
DIFF="$WORK/root.diff"
truncate -s 1G "$DIFF"
mkfs.ext4 -q -F -O ^has_journal "$DIFF"

# Capture from v1. On restore, the recorded path is deliberately removed; the
# supplied v2 path must fail digest/basename validation and the sibling v1 must win.
CAPTURE_RUNTIME="$WORK/capture-runtime/runtime-v1.bundle"
SELECTED_RUNTIME="$WORK/deploy-runtime/runtime-v1.bundle"
DEFAULT_RUNTIME="$WORK/deploy-runtime/runtime-v2.bundle"
cp --reflink=auto --sparse=always "$BIN/sandbox-runtime.bundle" "$CAPTURE_RUNTIME"

cat >"$WORK/cold.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-restore }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$CAPTURE_RUNTIME
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$DIFF }
launch:
  args: ["-c", "import time\nprint('PYBOOT-OK', flush=True)\ni=0\nwhile True:\n print('TICK', i, flush=True); i+=1; time.sleep(0.25)"]
  restart: never
  cgroup_control: true
EOF

SID1="restore-cold-$BASHPID"
LOG1="$OUT/cold.log"
readiness_begin_capture "$WORK/cold.ready"
COLD_READER_PID=$READY_READER_PID
readiness_exec_in_new_session "$BIN/sandbox-ctl" run \
    --ready-fd="$READY_WRITE_FD" --config "$WORK/cold.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --sandbox-id "$SID1" \
    >"$LOG1" 2>&1 &
SBPID1=$!
readiness_close_parent_writer
readiness_start_watchdog "$SBPID1" 120 10
readiness_wait_event "$WORK/cold.ready" 1 control_ready "$SBPID1" || e2e_fail "cold run missed control_ready"
readiness_connect_ctl "$WORK/runtime/$SID1/ctl.sock" || e2e_fail "cold ctl.sock unavailable at control_ready"
readiness_wait_event "$WORK/cold.ready" 2 ready "$SBPID1" || e2e_fail "cold run missed ready"
CG_COLD="$("$BIN/sandbox-ctl" exec --sandbox-id "$SID1" --run-root "$WORK/runtime" -- cat /proc/self/cgroup)"
grep -qE '^0::/init[[:space:]]*$' <<<"$CG_COLD" || e2e_fail "cold exec is not pinned to /init"
readiness_assert_wire "$WORK/cold.ready" "$COLD_READER_PID" $'control_ready\nready\n' || e2e_fail "cold readiness wire is not exact"

for _ in $(seq 1 600); do
    grep -qE '^TICK 10[[:space:]]*$' "$LOG1" 2>/dev/null && break
    kill -0 "$SBPID1" 2>/dev/null || e2e_fail "cold run exited before TICK 10"
    sleep 0.05
done
grep -qE '^TICK 10[[:space:]]*$' "$LOG1" || e2e_fail "cold run never reached TICK 10"
PRE_SNAP_TICK="$(grep -oE '^TICK [0-9]+' "$LOG1" | tail -1 | awk '{print $2}')"

"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID1" --output "$WORK/snapshot" --run-root "$WORK/runtime" \
    >"$OUT/snapshot.log" 2>&1
wait "$SBPID1" || true
SBPID1=""
readiness_stop_watchdog
SNAPSHOT="$WORK/snapshot/$SID1.snapshot"
[ -f "$SNAPSHOT" ] || e2e_fail "snapshot is missing"

mv "$CAPTURE_RUNTIME" "$SELECTED_RUNTIME"
cp --reflink=auto --sparse=always "$SELECTED_RUNTIME" "$DEFAULT_RUNTIME"
[ ! -e "$CAPTURE_RUNTIME" ] || e2e_fail "captured runtime path still exists"
find "$WORK/snapshot" -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum >"$WORK/source.sha256"

RESTORE_DIFF="$WORK/restore.diff"
truncate -s 1G "$RESTORE_DIFF"
mkfs.ext4 -q -F -O ^has_journal "$RESTORE_DIFF"
cat >"$WORK/restore.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-restore }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$DEFAULT_RUNTIME
  root:
    overlay: { diff: file://$RESTORE_DIFF }
EOF
sha256sum "$WORK/restore.yaml" "$DEFAULT_RUNTIME" >"$WORK/host-inputs.sha256"

SID2="restore-live-$BASHPID"
LOG2="$OUT/restore.log"
readiness_begin_capture "$WORK/restore.ready"
RESTORE_READER_PID=$READY_READER_PID
readiness_exec_in_new_session "$BIN/sandbox-ctl" run \
    --ready-fd="$READY_WRITE_FD" --restore "$SNAPSHOT" --config "$WORK/restore.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --sandbox-id "$SID2" \
    >"$LOG2" 2>&1 &
SBPID2=$!
readiness_close_parent_writer
readiness_start_watchdog "$SBPID2" 120 10
readiness_wait_event "$WORK/restore.ready" 1 control_ready "$SBPID2" || e2e_fail "restore missed control_ready"
readiness_connect_ctl "$WORK/runtime/$SID2/ctl.sock" || e2e_fail "restore ctl.sock unavailable at control_ready"
readiness_wait_event "$WORK/restore.ready" 2 ready "$SBPID2" || e2e_fail "restore missed ready"
CG_RESTORE="$("$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$WORK/runtime" -- cat /proc/self/cgroup)"
grep -qE '^0::/init[[:space:]]*$' <<<"$CG_RESTORE" || e2e_fail "restored exec is not pinned to /init"
readiness_assert_wire "$WORK/restore.ready" "$RESTORE_READER_PID" $'control_ready\nready\n' || e2e_fail "restore readiness wire is not exact"

python3 - "$WORK/runtime/$SID2/snap-state/config.json" "$SELECTED_RUNTIME" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    config = json.load(source)
assert len(config["pmem"]) == 1, config["pmem"]
assert config["pmem"][0]["file"] == sys.argv[2], config["pmem"]
PY
sha256sum --check --status "$WORK/source.sha256" || e2e_fail "restore modified source snapshot artifacts"
sha256sum --check --status "$WORK/host-inputs.sha256" || e2e_fail "restore modified host inputs"

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
