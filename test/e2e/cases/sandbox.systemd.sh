#!/usr/bin/env bash
# e2e-requires: ["docker","kvm"]
# Runtime selection, shared execution and bounded stop with prepared products.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/readiness_helpers.sh"
source "$SANDBOXER_LIB/tarstream.sh"
source "$SANDBOXER_LIB/systemd_helpers.sh"
: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker mkfs.ext4 pkill python3 sed sha256sum tail timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do require_binary "$binary"; done
for file in sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded"
mkdir -p "$WORK" "$OUT"
RUNPID=""
READINESS_WATCHDOG_PID=""
cleanup() {
    local status=$?
    set +e
    readiness_stop_watchdog
    if [ -n "$RUNPID" ]; then
        kill -TERM "$RUNPID" 2>/dev/null
        for _ in $(seq 1 100); do
            kill -0 "$RUNPID" 2>/dev/null || break
            sleep 0.05
        done
        readiness_kill_session KILL "$RUNPID"
        wait "$RUNPID" 2>/dev/null
    fi
    if [ "$status" -ne 0 ] && [ -n "${SCENARIO_OUT:-}" ] && [ -f "$SCENARIO_OUT/run.log" ]; then
        echo "systemd scenario ${scenario:-setup}: sandbox run log" >&2
        tail -n 80 "$SCENARIO_OUT/run.log" >&2
    fi
    systemd_cleanup_root_fixture
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

sha256sum "$BIN/sandbox-ctl" "$BIN/sandbox-runtime.bundle" "$BIN/vmlinux" "$BIN/cloud-hypervisor" >"$OUT/inputs.sha256"
systemd_prepare_root_fixture
cat >"$WORK/probe.sh" <<'GUEST'
#!/bin/sh
set -eu
for path in /usr/lib/systemd/systemd /lib/systemd/systemd /sbin/init; do
    test ! -e "$path"
    test ! -L "$path"
done
test "$(cat /proc/1/comm)" = systemd
test "$(command -v systemctl)" = /run/sandbox-runtime/systemd/bin/systemctl
systemctl is-active --quiet sandbox-init.service
main=$(systemctl show sandbox-init.service -p MainPID --value)
test "$main" -gt 1
group=$(systemctl show sandbox-init.service -p ControlGroup --value)
test -n "$group"
test "$group" != /
test -L /run/sandbox-init/cgroup
test "$(readlink -f /run/sandbox-init/cgroup)" = "/sys/fs/cgroup$group"
test -z "$(cat /run/sandbox-init/cgroup/cgroup.procs)"
test "$(cat /proc/self/cgroup)" = "0::$group/init"
for namespace in pid mnt cgroup; do
    test "$(readlink "/proc/$main/ns/$namespace")" = "$(readlink "/proc/1/ns/$namespace")"
    test "$(readlink "/proc/self/ns/$namespace")" = "$(readlink "/proc/1/ns/$namespace")"
done
for controller in cpu io memory pids; do
    grep -qw "$controller" /run/sandbox-init/cgroup/cgroup.controllers
    grep -qw "$controller" /run/sandbox-init/cgroup/cgroup.subtree_control
done
if [ "$1" = init ]; then
    test ! -e /tmp/systemd-e2e-init.count
    echo 1 >/tmp/systemd-e2e-init.count
fi
test "$(cat /tmp/systemd-e2e-init.count)" = 1
echo MANAGER_START=$(awk '{print $22}' /proc/1/stat)
echo BOOT_ID=$(cat /proc/sys/kernel/random/boot_id)
echo EXECUTOR_PID=$main
echo INVOCATION_ID=$(systemctl show sandbox-init.service -p InvocationID --value)
echo CONTROL_GROUP=$group
echo INIT_COUNT=1
echo SYSTEMD_PROBE_PASS
GUEST

ctl_exec() {
    timeout -k 2 20 "$BIN/sandbox-ctl" exec --sandbox-id "$SID" \
        --run-root "$SCENARIO_WORK/run" -- "$@"
}

scenario_index=0
for scenario in runtime auto guest-poweroff executor-fault; do
    scenario_index=$((scenario_index + 1))
    mode=runtime
    [ "$scenario" != auto ] || mode=auto
    # Launch adds a port suffix to its Unix socket. Keep fixture paths short
    # under the runner's per-case directory; use the full label for reports.
    SCENARIO_WORK="$WORK/s$scenario_index"
    SCENARIO_OUT="$OUT/$scenario"
    mkdir -p "$SCENARIO_WORK/run" "$SCENARIO_WORK/base" "$SCENARIO_OUT"
    SID="sd-$scenario_index-$BASHPID"
    RUN_ID="$SID-run"
    python3 - "$SCENARIO_WORK" "$BIN" "$SYSTEMD_BASE_REF" "$SYSTEMD_DIFF_TEMPLATE" "$mode" "$WORK/probe.sh" <<'PY'
import json, pathlib, sys
root, binary = map(pathlib.Path, sys.argv[1:3])
config = {
    'resources': {'capacity': {'cpu': 1, 'memory': '512MiB'},
                  'allocatable': {'cpu': 1, 'memory': '512MiB'}},
    'boot': {'kernel': 'file://'+str(binary/'vmlinux'),
             'runtime': 'file://'+str(binary/'sandbox-runtime.bundle'),
             'cmdline': 'console=hvc0 printk.time=1',
             'root': {'base': sys.argv[3], 'overlay': {
                 'diff': 'file://'+str(root/'root.diff'), 'diff_template': 'file://'+sys.argv[4]}}},
    'files': [{'path': '/systemd-e2e-probe.sh', 'mode': '0755',
               'content': pathlib.Path(sys.argv[6]).read_text()}],
    'init': [{'exec': '/bin/sh', 'args': ['/systemd-e2e-probe.sh', 'init']}],
    'launch': {'systemd': sys.argv[5], 'placeholder': True,
               'cgroup_control': False, 'pid_namespace': 'private', 'stop_grace_period': '2s'},
}
(root/'source.yaml').write_text(json.dumps(config, indent=2)+'\n')
PY
    readiness_begin_capture "$SCENARIO_OUT/ready"
    reader_pid=$READY_READER_PID
    readiness_exec_in_new_session "$BIN/sandbox-ctl" run --config "$SCENARIO_WORK/source.yaml" \
        --sandbox-id "$SID" --run-id "$RUN_ID" --ch-binary "$BIN/cloud-hypervisor" \
        --run-root "$SCENARIO_WORK/run" --base-root "$SCENARIO_WORK/base" \
        --stdout-to "$SCENARIO_OUT/app.log" --ready-fd="$READY_WRITE_FD" \
        >"$SCENARIO_OUT/run.log" 2>&1 &
    RUNPID=$!
    readiness_close_parent_writer
    readiness_start_watchdog "$RUNPID" 100 10
    readiness_wait_event "$SCENARIO_OUT/ready" 1 control_ready "$RUNPID" || e2e_fail "$scenario control readiness"
    readiness_connect_ctl "$SCENARIO_WORK/run/$SID/ctl.sock" || e2e_fail "$scenario control socket"
    readiness_wait_event "$SCENARIO_OUT/ready" 2 ready "$RUNPID" || e2e_fail "$scenario execution readiness"
    readiness_assert_wire "$SCENARIO_OUT/ready" "$reader_pid" $'control_ready\nready\n' || e2e_fail "$scenario readiness wire"
    ctl_exec /bin/sh /systemd-e2e-probe.sh native >"$SCENARIO_OUT/before.txt" 2>&1 || e2e_fail "$scenario execution contract"
    grep -qx SYSTEMD_PROBE_PASS "$SCENARIO_OUT/before.txt" || e2e_fail "$scenario incomplete probe"

    if [ "$scenario" = runtime ]; then
        ctl_exec systemctl daemon-reexec >"$SCENARIO_OUT/reexec.log" 2>&1 || e2e_fail "daemon-reexec request"
        # Observe past the five-second communication-failure budget. A
        # reexec must retain PID 1, the service and the initial Init fact.
        for tick in $(seq 1 6); do
            sleep 1
            ctl_exec /bin/sh /systemd-e2e-probe.sh native >"$SCENARIO_OUT/reexec-$tick.txt" 2>&1 || \
                e2e_fail "execution did not survive daemon-reexec"
        done
        python3 - "$SCENARIO_OUT" <<'PY'
import pathlib, sys
root = pathlib.Path(sys.argv[1])
def read(name):
    return dict(line.split('=', 1) for line in (root/name).read_text().splitlines() if '=' in line)
before = read('before.txt')
for tick in range(1, 7):
    after = read(f'reexec-{tick}.txt')
    for key in ['MANAGER_START', 'BOOT_ID', 'EXECUTOR_PID', 'INVOCATION_ID', 'CONTROL_GROUP', 'INIT_COUNT']:
        assert before.get(key) and before[key] == after.get(key), (tick, key, before.get(key), after.get(key))
PY
    fi

    start_ns=$(python3 -c 'import time; print(time.monotonic_ns())')
    case "$scenario" in
        guest-poweroff)
            # Namespace teardown may close this triggering exec connection.
            # The independent owner result below decides success.
            ctl_exec systemctl --no-block poweroff >"$SCENARIO_OUT/trigger.log" 2>&1 || true
            ;;
        executor-fault)
            ctl_exec systemctl kill --kill-whom=main --signal=KILL sandbox-init.service \
                >"$SCENARIO_OUT/trigger.log" 2>&1 || true
            ;;
        *)
            timeout -k 2 15 "$BIN/sandbox-ctl" shutdown --sandbox-id "$SID" --run-id "$RUN_ID" \
                --run-root "$SCENARIO_WORK/run" >"$SCENARIO_OUT/shutdown.log" 2>&1 || \
                e2e_fail "$scenario normal shutdown"
            ;;
    esac
    for _ in $(seq 1 240); do
        kill -0 "$RUNPID" 2>/dev/null || break
        sleep 0.05
    done
    kill -0 "$RUNPID" 2>/dev/null && e2e_fail "$scenario owner did not retire within its stop budget"
    set +e
    wait "$RUNPID"
    run_status=$?
    set -e
    RUNPID=""
    readiness_stop_watchdog
    end_ns=$(python3 -c 'import time; print(time.monotonic_ns())')
    python3 - "$SCENARIO_OUT" "$scenario" "$mode" "$run_status" "$start_ns" "$end_ns" <<'PY'
import json, pathlib, re, sys
root = pathlib.Path(sys.argv[1]); scenario, mode = sys.argv[2:4]
status = int(sys.argv[4]); seconds = (int(sys.argv[6])-int(sys.argv[5]))/1e9
fault = scenario == 'executor-fault'
assert (status != 0) == fault, (scenario, status)
assert seconds < 12, (scenario, seconds)  # Fixture G=2 plus Host 2F, not a new default.
log = (root/'run.log').read_text(errors='replace')
state = 'failed' if fault else 'stopped'
assert 'environment exited state='+state in log, (scenario, 'missing actual manager result')
assert not re.search(r'WARNING: CPU:|kernel BUG|dax_insert_entry', log), (scenario, 'kernel diagnostic')
(root/'result.json').write_text(json.dumps({'result': 'PASS', 'mode': mode, 'scenario': scenario,
    'owner_exit': status, 'stop_seconds': seconds, 'manager_state': state,
    'raw_cgroup_control': False, 'raw_pid_namespace': 'private'}, indent=2)+'\n')
PY
done
echo "PASS sandbox.systemd.sh (Runtime/auto, execution, reexec, normal and fault stop)"
