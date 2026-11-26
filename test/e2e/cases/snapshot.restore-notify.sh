#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","userfaultfd"]
# Restore signals and systemd jobs through real capture/attach/restore boundaries.
set -euo pipefail
source "${E2E_LIB:?}/common.sh"
source "$E2E_LIB/sandboxer/readiness_helpers.sh"
source "$E2E_LIB/sandboxer/tarstream.sh"
source "$E2E_LIB/sandboxer/systemd_helpers.sh"
: "${WORK:?}" "${OUT:?}" "${E2E_WORKSPACE:?}" "${E2E_IMAGE:?}"
require_root
require_kvm
for command in docker mkfs.ext4 pkill python3 sha256sum timeout truncate; do require_command "$command"; done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do require_binary "$binary"; done
mkdir -p "$WORK" "$OUT"
RUNPID=""
READINESS_WATCHDOG_PID=""
cleanup() {
    local status=$?
    set +e
    readiness_stop_watchdog
    if [ -n "$RUNPID" ]; then
        kill -TERM "$RUNPID" 2>/dev/null
        for _ in $(seq 1 100); do kill -0 "$RUNPID" 2>/dev/null || break; sleep 0.05; done
        readiness_kill_session KILL "$RUNPID"
        wait "$RUNPID" 2>/dev/null
    fi
    systemd_cleanup_root_fixture
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
sha256sum "$BIN/sandbox-ctl" "$BIN/sandbox-runtime.bundle" "$BIN/vmlinux" "$BIN/cloud-hypervisor" >"$OUT/inputs.sha256"
systemd_prepare_root_fixture
mkdir -p "$WORK/fixture"
cat >"$WORK/fixture/process.sh" <<'GUEST'
#!/bin/sh
set -eu
kind=$1
count=0
mkdir -p /run/restore-probe
notify() { count=$((count+1)); echo "$count" >"/run/restore-probe/$kind.count"; }
trap notify USR1 USR2
trap 'exit 0' TERM INT
echo 0 >"/run/restore-probe/$kind.count"
echo $$ >"/run/restore-probe/$kind.pid"
if [ "$kind" = main ]; then /bin/sh /opt/restore-probe/process.sh descendant & fi
while :; do sleep 1 & wait $! || :; done
GUEST
cat >"$WORK/fixture/hook.sh" <<'GUEST'
#!/bin/sh
set -eu
kind=$1
path=/run/restore-probe/$kind.count
n=0
test ! -f "$path" || n=$(cat "$path")
echo $((n+1)) >"$path"
mode=$(cat /run/restore-probe/mode)
# A completed ready must observe this marker, including a failed WantedBy job.
sleep 0.3
echo "$((n+1))" >"/run/restore-probe/$kind.completed"
case "$mode:$kind" in
    wanted-fail:wanted|required-fail:required) exit 1 ;;
    unit-timeout:required) sleep 20 ;;
    rpc-timeout:required) sleep 20 ;;
esac
GUEST
cat >"$WORK/fixture/init.sh" <<'GUEST'
#!/bin/sh
set -eu
mkdir -p /run/restore-probe
echo ok >/run/restore-probe/mode
if [ "$1" != off ]; then
    systemctl daemon-reload
    systemctl enable restore-required.service restore-wanted.service
    test "$(systemctl show sandbox-restore.target -p ActiveState --value)" = inactive
fi
GUEST
cat >"$WORK/fixture/check.sh" <<'GUEST'
#!/bin/sh
set -eu
want=$1
hooks=$2
for kind in main plugin; do
    i=0
    while test "$(cat /run/restore-probe/$kind.count 2>/dev/null || echo absent)" != "$want"; do
        i=$((i+1)); test "$i" -lt 100; sleep 0.05
    done
done
for kind in unsubscribed descendant; do
    i=0
    while test ! -s "/run/restore-probe/$kind.count"; do i=$((i+1)); test "$i" -lt 100; sleep 0.05; done
done
test "$(cat /run/restore-probe/unsubscribed.count)" = 0
test "$(cat /run/restore-probe/descendant.count)" = 0
if [ "$hooks" = absent ]; then
    test ! -f /run/restore-probe/required.count
    test ! -f /run/restore-probe/wanted.count
else
    for kind in required wanted; do
        test "$(cat /run/restore-probe/$kind.count)" = "$hooks"
        test "$(cat /run/restore-probe/$kind.completed)" = "$hooks"
    done
    systemctl is-active --quiet sandbox-restore.target
fi
echo RESTORE_NOTIFICATION_PASS
GUEST
for kind in required wanted; do
    relation=RequiredBy
    [ "$kind" != wanted ] || relation=WantedBy
    cat >"$WORK/fixture/restore-$kind.service" <<UNIT
[Unit]
PartOf=sandbox-restore.target
Before=sandbox-restore.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh /opt/restore-probe/hook.sh $kind
TimeoutStartSec=2s
[Install]
$relation=sandbox-restore.target
UNIT
done

ctl_exec() {
    timeout -k 2 25 "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$SCENE/run" -- "$@"
}
start_run() {
    local phase=$1 expect=$2 reader_pid
    shift 2
    SID="rn-$BASHPID"
    RUN_ID="$phase-$BASHPID"
    local ready="$SCENE/$phase.ready" log="$REPORT/$phase.run.log"
    readiness_begin_capture "$ready"
    reader_pid=$READY_READER_PID
    readiness_exec_in_new_session "$BIN/sandbox-ctl" run "$@" \
        --ready-fd="$READY_WRITE_FD" --ch-binary "$BIN/cloud-hypervisor" \
        --run-root "$SCENE/run" --base-root "$SCENE/base" --sandbox-id "$SID" --run-id "$RUN_ID" >"$log" 2>&1 &
    RUNPID=$!
    readiness_close_parent_writer
    readiness_start_watchdog "$RUNPID" 240 10
    if [ "$expect" = success ]; then
        readiness_wait_event "$ready" 1 control_ready "$RUNPID" || { tail -80 "$log" >&2; e2e_fail "$phase control ready"; }
        readiness_wait_event "$ready" 2 ready "$RUNPID" || { tail -80 "$log" >&2; e2e_fail "$phase ready"; }
        readiness_assert_wire "$ready" "$reader_pid" $'control_ready\nready\n' || e2e_fail "$phase readiness wire"
    else
        if wait "$RUNPID"; then e2e_fail "$phase unexpectedly restored"; fi
        RUNPID=""
        readiness_stop_watchdog
        wait "$reader_pid"
        python3 - "$ready" "$log" <<'PY'
import json, pathlib, sys
events=pathlib.Path(sys.argv[1]).read_text().splitlines()
assert 'ready' not in events, events
log=pathlib.Path(sys.argv[2]).read_text()
assert 'sandbox-restore.target' in log or 'deadline exceeded' in log, log[-4000:]
PY
    fi
}
stop_run() {
    if [ "$mode" = off ]; then
        # Direct supervision has no normal-Shutdown API. Retire the Host owner
        # after the capture/attach assertions without asking the Guest to stop.
        kill -TERM "$RUNPID"
        wait "$RUNPID" || true
    else
        timeout -k 2 20 "$BIN/sandbox-ctl" shutdown --sandbox-id "$SID" --run-id "$RUN_ID" --run-root "$SCENE/run" >"$REPORT/$RUN_ID.stop.log" 2>&1
        wait "$RUNPID" || e2e_fail "normal shutdown"
    fi
    RUNPID=""
    readiness_stop_watchdog
}
capture() {
    local name=$1
    mkdir -p "$SCENE/$name"
    timeout -k 3 180 "$BIN/sandbox-ctl" snapshot --sandbox-id "$SID" --run-root "$SCENE/run" \
        --output "$SCENE/$name" --resume --json --timeout 160 >"$REPORT/$name.capture.json" 2>"$REPORT/$name.capture.log"
    python3 - "$SCENE" "$REPORT/$name.capture.json" "$name" <<'PY'
import json, pathlib, sys
root=pathlib.Path(sys.argv[1]); report=json.loads(pathlib.Path(sys.argv[2]).read_text()); name=sys.argv[3]
for key,suffix in [('snapshotRef','s'),('sandboxRef','e')]:
    ref=report[key]; assert ref.startswith('file://'), ref
    base=ref[7:]; assert base and '/' not in base, ref
    (root/(name+'.'+suffix)).write_text('file://'+str(root/name/base)+'\n')
PY
}
restore() {
    local phase=$1 from=$2 expect=${3:-success}
    python3 - "$SCENE" "$phase" "$BIN" <<'PY'
import json,pathlib,sys
root=pathlib.Path(sys.argv[1]); phase=sys.argv[2]; binary=pathlib.Path(sys.argv[3])
host={'resources':{'capacity':{'cpu':1,'memory':'512MiB'},'allocatable':{'cpu':1,'memory':'512MiB'}},
      'boot':{'runtime':'file://'+str(binary/'sandbox-runtime.bundle'),
              'root':{'overlay':{'diff':'file://'+str(root/(phase+'.diff'))}}}}
(root/(phase+'.yaml')).write_text(json.dumps(host)+'\n')
PY
    start_run "$phase" "$expect" --config "$SCENE/$phase.yaml" --restore "$(cat "$SCENE/$from.s")"
}
check() {
    local phase=$1 signals=$2 hooks=$3
    ctl_exec /bin/sh /opt/restore-probe/check.sh "$signals" "$hooks" >"$REPORT/$phase.check.log" 2>&1 || {
        cat "$REPORT/$phase.check.log" >&2; e2e_fail "$phase notification counts";
    }
}

for mode in off runtime; do
    SCENE="$WORK/$mode" REPORT="$OUT/$mode"
    mkdir -p "$SCENE/run" "$SCENE/base" "$REPORT"
    python3 - "$SCENE" "$WORK/fixture" "$BIN" "$SYSTEMD_BASE_REF" "$SYSTEMD_DIFF_TEMPLATE" "$mode" <<'PY'
import json,pathlib,sys
root,fixture,binary=map(pathlib.Path,sys.argv[1:4]); mode=sys.argv[6]
files=[{'path':'/opt/restore-probe/'+name,'mode':'0755','content':(fixture/name).read_text()}
       for name in ['process.sh','hook.sh','init.sh','check.sh']]
files += [{'path':'/etc/systemd/system/'+name,'content':(fixture/name).read_text()}
          for name in ['restore-required.service','restore-wanted.service']]
config={'resources':{'capacity':{'cpu':1,'memory':'512MiB'},'allocatable':{'cpu':1,'memory':'512MiB'}},
        'boot':{'kernel':'file://'+str(binary/'vmlinux'),'runtime':'file://'+str(binary/'sandbox-runtime.bundle'),
                'cmdline':'console=hvc0 printk.time=1','root':{'base':sys.argv[4],
                    'overlay':{'diff':'file://'+str(root/'source.diff'),'diff_template':'file://'+sys.argv[5]}}},
        'files':files,'init':[{'exec':'/bin/sh','args':['/opt/restore-probe/init.sh',mode]}],
        'launch':{'systemd':mode,'exec':'/bin/sh','args':['/opt/restore-probe/process.sh','main'],
                  'restore_signal':'SIGUSR1','stop_grace_period':'2s',
                  'plugin':[{'exec':'/bin/sh','args':['/opt/restore-probe/process.sh','plugin'],'restore_signal':'SIGUSR2','restart':'never'},
                            {'exec':'/bin/sh','args':['/opt/restore-probe/process.sh','unsubscribed'],'restart':'never'}]}}
(root/'source.yaml').write_text(json.dumps(config)+'\n')
PY
    start_run source success --config "$SCENE/source.yaml"
    check cold 0 absent
    capture first
    check source-attach 0 absent
    stop_run
    hooks=absent
    [ "$mode" = off ] || hooks=1
    restore first-restore first
    check first-restore 1 "$hooks"
    capture second
    check restored-source-attach 1 "$hooks"
    stop_run
    restore second-restore second
    [ "$mode" = off ] || hooks=2
    check second-restore 2 "$hooks"
    if [ "$mode" = runtime ]; then
        for scenario in wanted-fail required-fail unit-timeout rpc-timeout empty; do
            ctl_exec /bin/sh -c "echo '$scenario' >/run/restore-probe/mode"
            if [ "$scenario" = rpc-timeout ]; then
                ctl_exec /bin/sh -c 'mkdir -p /run/systemd/system/restore-required.service.d; printf "[Service]\nTimeoutStartSec=30s\n" >/run/systemd/system/restore-required.service.d/timeout.conf; systemctl daemon-reload'
            elif [ "$scenario" = empty ]; then
                ctl_exec /bin/sh -c 'systemctl disable --now restore-required.service restore-wanted.service; for kind in required wanted; do mkdir -p /run/systemd/system/restore-$kind.service.d; printf "[Unit]\nPartOf=\n" >/run/systemd/system/restore-$kind.service.d/empty.conf; done; systemctl daemon-reload'
            fi
            capture "$scenario"
            check "$scenario-source-attach" 2 2
        done
        stop_run
        restore wanted-failure wanted-fail
        check wanted-failure 3 3
        stop_run
        for scenario in required-fail unit-timeout rpc-timeout; do restore "$scenario-restored" "$scenario" failure; done
        restore empty-target empty
        check empty-target 3 2
    fi
    stop_run
done
echo 'PASS snapshot.restore-notify.sh (signals, attach exclusion, repeated jobs, required/wanted failures, unit/RPC timeout, empty target)'
