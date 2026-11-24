#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","userfaultfd"]
# Real systemd capture: S preserves process identity; E cold starts exactly once.
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
for command in date docker mkfs.ext4 pkill python3 sed sha256sum tar timeout truncate; do require_command "$command"; done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do require_binary "$binary"; done
for file in sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"
mkdir -p "$WORK/fixture" "$WORK/run" "$WORK/base" "$WORK/capture" "$OUT"
RUNPID=""
READINESS_WATCHDOG_PID=""
SID=""
RUN_ID=""
cleanup() {
    local status=$?
    set +e
    readiness_stop_watchdog
    if [ -n "$RUNPID" ]; then
        # Capture can leave a frozen source. Failure cleanup is physical Host
        # retirement, never Guest Shutdown, so it cannot thaw that source.
        kill -TERM "$RUNPID" 2>/dev/null
        for _ in $(seq 1 100); do
            kill -0 "$RUNPID" 2>/dev/null || break
            sleep 0.05
        done
        readiness_kill_session KILL "$RUNPID"
        wait "$RUNPID" 2>/dev/null
    fi
    systemd_cleanup_root_fixture
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

sha256sum "$BIN/sandbox-ctl" "$BIN/sandbox-runtime.bundle" "$BIN/vmlinux" "$BIN/cloud-hypervisor" > "$OUT/inputs.sha256"
systemd_prepare_root_fixture
cat > "$WORK/fixture/long-lived.sh" <<'GUEST'
#!/bin/sh
set -eu
kind=$1
dir=/var/lib/systemd-probe
mkdir -p "$dir"
n=0
test ! -f "$dir/$kind.count" || n=$(cat "$dir/$kind.count")
echo $((n+1)) > "$dir/$kind.count"
echo $$ > "/run/$kind.pid"
trap 'exit 0' TERM INT
while :; do sleep 60 & wait $!; done
GUEST
cat > "$WORK/fixture/init.sh" <<'GUEST'
#!/bin/sh
set -eu
test "$(cat /proc/1/comm)" = systemd
command -v systemctl
systemctl is-active sandbox-init.service
mkdir -p /var/lib/systemd-probe
n=0
test ! -f /var/lib/systemd-probe/init.count || n=$(cat /var/lib/systemd-probe/init.count)
echo $((n+1)) > /var/lib/systemd-probe/init.count
cat /proc/sys/kernel/random/uuid > /run/systemd-probe-init.token
systemctl start systemd-probe.service
echo SYSTEMD_ES_INIT_PASS
GUEST
cat > "$WORK/fixture/state.sh" <<'GUEST'
#!/bin/sh
set -eu
test "$(cat /proc/1/comm)" = systemd
systemctl is-active --quiet sandbox-init.service systemd-probe.service
test -L /run/sandbox-init/cgroup
test -z "$(cat /run/sandbox-init/cgroup/cgroup.procs)"
for control in cpu io memory pids; do
    tr ' ' '\n' < /run/sandbox-init/cgroup/cgroup.controllers | grep -qx "$control"
    tr ' ' '\n' < /run/sandbox-init/cgroup/cgroup.subtree_control | grep -qx "$control"
done
plugin_pid=$(cat /run/plugin.pid)
test -e "/proc/$plugin_pid/stat"
echo MANAGER_COMM=$(cat /proc/1/comm)
echo MANAGER_EXE=$(readlink /proc/1/exe)
echo MANAGER_START=$(awk '{print $22}' /proc/1/stat)
echo BOOT_ID=$(cat /proc/sys/kernel/random/boot_id)
echo MANAGER_VERSION=$(systemctl --version | head -1)
echo MANAGER_SOCKET_INODE=$(stat -c %i /run/systemd/private)
echo INIT_COUNT=$(cat /var/lib/systemd-probe/init.count)
echo INIT_TOKEN=$(cat /run/systemd-probe-init.token)
echo PLUGIN_COUNT=$(cat /var/lib/systemd-probe/plugin.count)
echo PLUGIN_PID=$plugin_pid
echo PLUGIN_START=$(awk '{print $22}' "/proc/$plugin_pid/stat")
echo UNIT_COUNT=$(cat /var/lib/systemd-probe/unit.count)
systemctl show sandbox-init.service -p MainPID -p InvocationID -p ControlGroup | sed 's/^/BRIDGE_/'
systemctl show systemd-probe.service -p MainPID -p InvocationID -p ControlGroup | sed 's/^/UNIT_/'
echo EXEC_CGROUP=$(cat /proc/self/cgroup)
echo CGROUP_CONTROLLERS=$(cat /run/sandbox-init/cgroup/cgroup.controllers)
echo CGROUP_SUBTREE=$(cat /run/sandbox-init/cgroup/cgroup.subtree_control)
echo NATIVE_EXEC_PASS
GUEST
cat > "$WORK/fixture/systemd-probe.service" <<'UNIT'
[Unit]
Description=Systemd E/S validation sentinel
[Service]
Type=simple
ExecStart=/bin/sh /opt/systemd-probe/long-lived.sh unit
KillMode=control-group
TimeoutStopSec=1s
UNIT

# JSON is YAML. Keep exact shell content in files and no Launch/Init/files in
# the S host document. The captured E owns its disk graph and cold workload.
python3 - "$WORK" "$BIN" "$SYSTEMD_BASE_REF" "$SYSTEMD_DIFF_TEMPLATE" <<'PY'
import json, pathlib, sys
root, binary = map(pathlib.Path, sys.argv[1:3])
resources = {'capacity': {'cpu': 1, 'memory': '512MiB'}, 'allocatable': {'cpu': 1, 'memory': '512MiB'}}
files = [{'path': '/opt/systemd-probe/' + name, 'mode': '0755', 'content': (root/'fixture'/name).read_text()}
         for name in ['init.sh', 'state.sh', 'long-lived.sh']]
files.append({'path': '/etc/systemd/system/systemd-probe.service', 'mode': '0644',
              'content': (root/'fixture/systemd-probe.service').read_text()})
source = {'resources': resources, 'boot': {'kernel': 'file://'+str(binary/'vmlinux'),
    'runtime': 'file://'+str(binary/'sandbox-runtime.bundle'), 'cmdline': 'console=hvc0 printk.time=1',
    'root': {'base': sys.argv[3], 'overlay': {'diff': 'file://'+str(root/'source.diff'),
                                            'diff_template': 'file://'+sys.argv[4]}}},
    'files': files, 'init': [{'exec': '/bin/sh', 'args': ['/opt/systemd-probe/init.sh']}],
    'launch': {'systemd': 'runtime', 'placeholder': True, 'cgroup_control': False,
               'pid_namespace': 'private', 'stop_grace_period': '2s',
               'plugin': [{'exec': '/bin/sh', 'args': ['/opt/systemd-probe/long-lived.sh', 'plugin'], 'restart': 'never'}]}}
(root/'source.yaml').write_text(json.dumps(source, indent=2)+'\n')
for phase in ['restored', 'cold']:
    # A new template here would replace captured dirty blocks. Bind only a
    # fresh writable path while retaining the artifact's immutable graph.
    host = {'resources': resources, 'boot': {'kernel': 'file://'+str(binary/'vmlinux'),
            'runtime': 'file://'+str(binary/'sandbox-runtime.bundle'),
            'root': {'overlay': {'diff': 'file://'+str(root/(phase+'.diff'))}}}}
    (root/(phase+'-host.yaml')).write_text(json.dumps(host, indent=2)+'\n')
PY

start_run() {
    local phase=$1 reader_pid ready_file log
    shift
    SID="systemd-$phase-$BASHPID"
    RUN_ID="$SID-run"
    ready_file="$WORK/$phase.ready"
    log="$OUT/$phase.run.log"
    readiness_begin_capture "$ready_file"
    reader_pid=$READY_READER_PID
    readiness_exec_in_new_session "$BIN/sandbox-ctl" run "$@" \
        --ready-fd="$READY_WRITE_FD" --ch-binary "$BIN/cloud-hypervisor" \
        --run-root "$WORK/run" --base-root "$WORK/base" --sandbox-id "$SID" --run-id "$RUN_ID" > "$log" 2>&1 &
    RUNPID=$!
    readiness_close_parent_writer
    readiness_start_watchdog "$RUNPID" 240 10
    readiness_wait_event "$ready_file" 1 control_ready "$RUNPID" || { tail -80 "$log" >&2; e2e_fail "$phase control readiness"; }
    readiness_connect_ctl "$WORK/run/$SID/ctl.sock" || e2e_fail "$phase control socket"
    readiness_wait_event "$ready_file" 2 ready "$RUNPID" || { tail -80 "$log" >&2; e2e_fail "$phase execution readiness"; }
    readiness_assert_wire "$ready_file" "$reader_pid" $'control_ready\nready\n' || e2e_fail "$phase readiness wire"
}
capture_state() {
    local phase=$1
    timeout -k 2 25 "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/run" -- /bin/sh -c \
        'i=0; while test ! -s /run/plugin.pid || test ! -s /run/unit.pid; do i=$((i+1)); test "$i" -lt 100; sleep 0.05; done; exec /bin/sh /opt/systemd-probe/state.sh' \
        > "$OUT/$phase.txt" 2>&1 || { cat "$OUT/$phase.txt" >&2; e2e_fail "$phase native exec"; }
    grep -qx NATIVE_EXEC_PASS "$OUT/$phase.txt" || e2e_fail "$phase state incomplete"
}
normal_stop() {
    local phase=$1 started
    started=$(date +%s%N)
    timeout -k 2 20 "$BIN/sandbox-ctl" shutdown --sandbox-id "$SID" --run-id "$RUN_ID" --run-root "$WORK/run" \
        > "$OUT/$phase.shutdown.log" 2>&1 || { cat "$OUT/$phase.shutdown.log" >&2; e2e_fail "$phase normal shutdown"; }
    wait "$RUNPID" || e2e_fail "$phase owner did not exit successfully"
    RUNPID=""
    readiness_stop_watchdog
    python3 - "$OUT/$phase.shutdown.json" "$started" "$(date +%s%N)" <<'PY'
import json, pathlib, sys
seconds = (int(sys.argv[3])-int(sys.argv[2]))/1e9
# This harness bound includes CLI process/connection setup. The Host owns the
# monotonic G+2F deadline; the case must not reconstruct it from wall time.
assert seconds < 20, seconds
pathlib.Path(sys.argv[1]).write_text(json.dumps({'seconds': seconds, 'owner_exit': 0})+'\n')
PY
}

start_run source --config "$WORK/source.yaml"
capture_state before
timeout -k 3 180 "$BIN/sandbox-ctl" snapshot --sandbox-id "$SID" --run-root "$WORK/run" \
    --output "$WORK/capture" --json --timeout 160 > "$OUT/capture.json" 2> "$OUT/capture.log"
wait "$RUNPID" || e2e_fail "captured source physical retirement failed"
RUNPID=""
readiness_stop_watchdog
python3 - "$WORK" "$OUT/capture.json" <<'PY'
import json, pathlib, sys
root=pathlib.Path(sys.argv[1]); report=json.loads(pathlib.Path(sys.argv[2]).read_text())
assert report['removedRefs']==[], report
for key,name in [('snapshotRef','snapshot.ref'),('sandboxRef','sandbox.ref')]:
    ref=report[key]
    assert ref.startswith('file://'), ref
    basename=ref[len('file://'):]
    assert basename and not basename.startswith('/') and '/' not in basename, ref
    (root/name).write_text('file://'+str(root/'capture')+'/'+basename+'\n')
PY
start_run restored --config "$WORK/restored-host.yaml" --restore "$(cat "$WORK/snapshot.ref")"
capture_state restored
python3 - "$OUT" <<'PY'
from pathlib import Path
import sys
root=Path(sys.argv[1])
def read(name):
    return dict(line.split('=',1) for line in (root/name).read_text().splitlines() if '=' in line)
before,after=map(read,['before.txt','restored.txt'])
keys=['MANAGER_COMM','MANAGER_EXE','MANAGER_START','BOOT_ID','MANAGER_VERSION','MANAGER_SOCKET_INODE',
      'INIT_COUNT','INIT_TOKEN','PLUGIN_COUNT','PLUGIN_PID','PLUGIN_START','UNIT_COUNT',
      'BRIDGE_MainPID','BRIDGE_InvocationID','BRIDGE_ControlGroup',
      'UNIT_MainPID','UNIT_InvocationID','UNIT_ControlGroup','CGROUP_CONTROLLERS','CGROUP_SUBTREE']
for key in keys:
    assert key in before and before[key]==after[key], (key,before.get(key),after.get(key))
assert before['INIT_COUNT']==before['PLUGIN_COUNT']==before['UNIT_COUNT']=='1', before
assert before['BRIDGE_InvocationID'] and before['UNIT_InvocationID'], before
PY
normal_stop restored
start_run cold --config "$WORK/cold-host.yaml" --from "$(cat "$WORK/sandbox.ref")"
capture_state cold
python3 - "$OUT" <<'PY'
from pathlib import Path
import sys
root=Path(sys.argv[1])
def read(name):
    return dict(line.split('=',1) for line in (root/name).read_text().splitlines() if '=' in line)
before,cold=map(read,['before.txt','cold.txt'])
assert cold['MANAGER_COMM']=='systemd' and cold['MANAGER_VERSION']==before['MANAGER_VERSION'], cold
assert cold['INIT_COUNT']==cold['PLUGIN_COUNT']==cold['UNIT_COUNT']=='2', cold
for key in ['BOOT_ID','INIT_TOKEN','BRIDGE_InvocationID','UNIT_InvocationID']:
    assert cold[key] and cold[key]!=before[key], (key,before[key],cold[key])
PY
normal_stop cold
echo "PASS snapshot.systemd.sh (S identities retained; E cold Init/plugin/unit once; normal Shutdown)"
