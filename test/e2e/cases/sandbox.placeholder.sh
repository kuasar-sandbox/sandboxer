#!/usr/bin/env bash
# Placeholder/no-network anchor contract using prepared products only.
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
for command in dd docker mkfs.ext4 python3 sha256sum truncate; do
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
SID="placeholder-$BASHPID"
RUNLOG="$OUT/placeholder.log"
RUNPID=""
READINESS_WATCHDOG_PID=""
cleanup() {
    local status=$?
    set +e
    readiness_stop_watchdog
    if [ -n "$RUNPID" ] && kill -0 "$RUNPID" 2>/dev/null; then
        kill -TERM "$RUNPID" 2>/dev/null
        for _ in $(seq 1 50); do
            kill -0 "$RUNPID" 2>/dev/null || break
            sleep 0.1
        done
    fi
    [ -n "$RUNPID" ] && readiness_kill_session KILL "$RUNPID"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"
DIFF="$WORK/root.diff"
truncate -s 1G "$DIFF"
mkfs.ext4 -q -F -O ^has_journal "$DIFF"

cat >"$WORK/sandbox.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay:
      diff: file://$DIFF
launch:
  placeholder: true
EOF

readiness_begin_capture "$WORK/placeholder.ready"
READER_PID=$READY_READER_PID
readiness_exec_in_new_session "$BIN/sandbox-ctl" run \
    --ready-fd="$READY_WRITE_FD" \
    --config "$WORK/sandbox.yaml" \
    --sandbox-id "$SID" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" >"$RUNLOG" 2>&1 &
RUNPID=$!
readiness_close_parent_writer
readiness_start_watchdog "$RUNPID" 120 10

exec1() {
    "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" "$@"
}
readiness_wait_event "$WORK/placeholder.ready" 1 control_ready "$RUNPID" || {
    tail -60 "$RUNLOG" >&2 || true
    e2e_fail "placeholder never emitted control_ready"
}
readiness_connect_ctl "$WORK/runtime/$SID/ctl.sock" || e2e_fail "ctl.sock is not connectable at control_ready"
readiness_wait_event "$WORK/placeholder.ready" 2 ready "$RUNPID" || {
    tail -60 "$RUNLOG" >&2 || true
    e2e_fail "placeholder never emitted ready"
}
exec1 -- /bin/true || e2e_fail "immediate exec after placeholder ready failed"
readiness_assert_wire "$WORK/placeholder.ready" "$READER_PID" $'control_ready\nready\n' || \
    e2e_fail "placeholder readiness wire was not exact"

CH_ARGS="$(grep -m1 '\[sandbox-ctl\] CH args:' "$RUNLOG" || true)"
[ -n "$CH_ARGS" ] || e2e_fail "Cloud Hypervisor args line is missing"
if grep -Eq '(^|[[:space:]])--net([[:space:]]|$)' <<<"$CH_ARGS"; then
    e2e_fail "no-network placeholder received a --net device"
fi
exec1 -- /bin/sh -c 'for path in /sys/class/net/*; do printf "%s\n" "${path##*/}"; done' \
    >"$WORK/netdevs"
[ "$(tr -d '\r' <"$WORK/netdevs")" = lo ] || e2e_fail "placeholder guest network devices are not exactly lo"

exec1 -- cat /proc/mounts >"$WORK/mounts"
grep -Eq '^/dev/root /opt/sandbox-runtime erofs .*dax=always' "$WORK/mounts" || \
    e2e_fail "runtime bundle is not offset-zero EROFS mounted with dax=always"

exec1 -- /bin/sh -c 'echo EXEC-OK-1; id; uname -sr' >"$WORK/exec-one.out"
assert_contains "$WORK/exec-one.out" 'EXEC-OK-1'

# Every exec must cross the complete MUX_CLOSE barrier before the next session.
dd if=/dev/urandom of="$WORK/exec.stdin" bs=1024 count=256 status=none
HOST_HASH="$(sha256sum "$WORK/exec.stdin" | cut -d' ' -f1)"
for ((i = 1; i <= 256; i++)); do
    exec1 --stdin-from "$WORK/exec.stdin" --stdout=false --stderr=false -- \
        /bin/dd of=/tmp/exec.stdin bs=65536 2>"$WORK/exec-$i.err" || {
        tail -60 "$RUNLOG" >&2 || true
        e2e_fail "consecutive stdin exec $i failed"
    }
    exec1 -- sha256sum /tmp/exec.stdin >"$WORK/exec-$i.hash"
    [ "$(cut -d' ' -f1 <"$WORK/exec-$i.hash")" = "$HOST_HASH" ] || \
        e2e_fail "consecutive stdin exec $i changed payload"
done

exec1 -- cat /proc/1/cmdline >"$WORK/pid1"
mapfile -d '' -t PID1_ARGV <"$WORK/pid1"
if [ "${#PID1_ARGV[@]}" -lt 6 ] || [ "${PID1_ARGV[1]}" != exec-child ] || [ "${PID1_ARGV[5]}" != 1 ]; then
    e2e_fail "PID 1 is not the placeholder anchor: $(tr '\0' ' ' <"$WORK/pid1")"
fi

# Killing PID 1 must restart the anchor in place, not reboot the VM.
exec1 -- /bin/sh -c 'kill -TERM 1' >/dev/null 2>&1 || true
for _ in $(seq 1 60); do
    if exec1 -- /bin/sh -c 'echo EXEC-OK-AFTER-RESTART' >"$WORK/restart.out" 2>/dev/null; then
        break
    fi
    kill -0 "$RUNPID" 2>/dev/null || e2e_fail "placeholder kill terminated the sandbox"
    sleep 0.1
done
assert_contains "$WORK/restart.out" 'EXEC-OK-AFTER-RESTART'
readiness_assert_wire "$WORK/placeholder.ready" "$READER_PID" $'control_ready\nready\n' || \
    e2e_fail "in-place placeholder restart emitted another readiness event"
assert_contains "$RUNLOG" 'restarting in'

kill -TERM "$RUNPID"
wait "$RUNPID"
RUNPID=""
readiness_stop_watchdog

echo "PASS sandbox.placeholder.sh"
