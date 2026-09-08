#!/usr/bin/env bash
# Exercise independent journal targets with real cold/restore/exec guest I/O.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
BIN="${BIN:-$REPO_ROOT/bin}"
VMLINUX="${VMLINUX:-$BIN/vmlinux}"
IMAGE="${IMAGE:-python:3.12-slim}"

skip() {
    echo "==> e2e_sandbox_journald: skipping ($*)"
    if [ "${REQUIRE_KVM:-0}" = 1 ]; then exit 1; fi
    exit 0
}
fail() { echo "==> FAIL: $*" >&2; exit 1; }
[ -e /dev/kvm ] || skip '/dev/kvm not present'
for binary in cloud-hypervisor sandbox-ctl sandbox-init sandbox-runtime.bundle flatten-ctl; do
    [ -e "$BIN/$binary" ] || skip "missing $BIN/$binary"
done
[ -f "$VMLINUX" ] || skip "missing $VMLINUX"
for command in journalctl python3 mkfs.ext4 ip; do
    command -v "$command" >/dev/null || skip "missing $command"
done
if [ "$(id -u)" -ne 0 ]; then exec sudo -nE "$0" "$@"; fi
[ -S /run/systemd/journal/socket ] || skip 'native journal socket unavailable'
journalctl --sync || skip 'journal is not accessible'

WORK="$(mktemp -d /tmp/e2e-journald-XXXXXX)"
CASE="journal-$$-$(date +%s)-${WORK##*-}"
APP="e2e-app-$$"
CONSOLE="e2e-console-$$"
CTL="e2e-ctl-$$"
# This output intentionally has no TEST_ID field. Its identifier must still
# distinguish this invocation from earlier uses of the same PID on the host.
BARE="e2e-bare-$CASE"
TAP_NAME="${TAP_NAME:-jlog$$}"
TAP_CREATED=0
PID=""
cleanup() {
    local status=$?
    trap - EXIT
    if [ -n "$PID" ]; then
        kill -TERM "$PID" 2>/dev/null || true
        for _ in $(seq 1 100); do
            kill -0 "$PID" 2>/dev/null || break
            sleep 0.1
        done
        if kill -0 "$PID" 2>/dev/null; then kill -KILL "$PID" 2>/dev/null || true; fi
        wait "$PID" 2>/dev/null || true
    fi
    if [ "$TAP_CREATED" = 1 ]; then ip link del "$TAP_NAME" 2>/dev/null || true; fi
    if [ "$status" != 0 ]; then
        for log in "$WORK"/*.err "$WORK"/*.out; do
            [ -f "$log" ] && { echo "---- $log" >&2; tail -40 "$log" >&2; }
        done
        journalctl "TEST_ID=$CASE" --no-pager -n 80 >&2 || true
    fi
    if [ -n "${E2E_KEEP:-}" ]; then echo "kept: $WORK"; else rm -rf "$WORK"; fi
    exit "$status"
}
trap cleanup EXIT
if ! ip link show "$TAP_NAME" >/dev/null 2>&1; then
    ip tuntap add dev "$TAP_NAME" mode tap
    TAP_CREATED=1
    ip addr add 169.254.1.0/31 dev "$TAP_NAME"
    ip link set "$TAP_NAME" up
fi

BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null || skip 'docker missing'
    docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null
    BLK0_IMAGE="$WORK/blk0.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"
RR="$WORK/run"
BR="$WORK/base"
mkdir -p "$RR" "$BR" "$WORK/snapshot"
truncate -s 512M "$WORK/template.ext4"
mkfs.ext4 -q -F "$WORK/template.ext4"
cat > "$WORK/cold.yaml" <<YAML
resources:
  capacity: {cpu: 1, memory: 512MiB}
  allocatable: {cpu: 1, memory: 512MiB}
network: {tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: journal-e2e}
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: 'console=hvc0 printk.time=1'
  root:
    base: $BLK0_REF
    overlay: {diff_template: file://$WORK/template.ext4}
YAML
cat >> "$WORK/cold.yaml" <<'YAML'
launch:
  exec: /bin/sh
  args:
    - -c
    - |
      printf 'APP-OUT-cold\n'
      printf 'APP-ERR-cold\n' >&2
      last=cold
      while :; do
        if [ -f /journal-command ]; then
          next=$(cat /journal-command)
          if [ "$next" != "$last" ]; then
            printf 'APP-OUT-%s\n' "$next"
            printf 'APP-ERR-%s\n' "$next" >&2
            last=$next
          fi
          [ "$next" != stop ] || exit 0
        fi
        sleep 0.1
      done
  restart: never
YAML

# No stdout/stderr descriptor is a journal stream here: targets are explicit.
start_run() {
    local phase=$1 sid=$2 config=$3
    shift 3
    "$BIN/sandbox-ctl" run --config "$config" --sandbox-id "$sid" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" --base-root "$BR" \
        --ready-fd 3 \
        --stdout-to "journald=$APP,TEST_ID=$CASE,PHASE=$phase,CHANNEL=stdout,NOTE=a%2Cb%252C+c%3Dd" \
        --stderr-to "journald=$APP,TEST_ID=$CASE,PHASE=$phase,CHANNEL=stderr" \
        --console "journald=$CONSOLE,TEST_ID=$CASE,PHASE=$phase,CHANNEL=console" \
        --log-to "journald=$CTL,TEST_ID=$CASE,PHASE=$phase,CHANNEL=component" \
        "$@" 3>"$WORK/$phase.ready" >"$WORK/$phase.out" 2>"$WORK/$phase.err" &
    PID=$!
    for _ in $(seq 1 400); do
        if grep -qx ready "$WORK/$phase.ready"; then return; fi
        kill -0 "$PID" 2>/dev/null || fail "$phase run exited before ready"
        sleep 0.15
    done
    fail "$phase run did not become ready"
}
wait_run_exit() {
    for _ in $(seq 1 400); do
        if ! kill -0 "$PID" 2>/dev/null; then
            local status=0
            wait "$PID" || status=$?
            PID=""
            [ "$status" = 0 ] || fail "run exit=$status"
            return
        fi
        sleep 0.15
    done
    fail 'run did not exit'
}
SID1="jl-$$-a"
SID2="jl-$$-b"
start_run cold "$SID1" "$WORK/cold.yaml"

# The client's naked stderr target must not inherit the running VM's fields.
# Both outputs end in partial lines to exercise client Close flushing.
"$BIN/sandbox-ctl" exec --sandbox-id "$SID1" --run-root "$RR" \
    --stdout-to "journald=$APP,TEST_ID=$CASE,PHASE=exec,CHANNEL=stdout" \
    --stderr-to "journald=$BARE" -- /bin/sh -c \
    "printf EXEC-OUT; printf EXEC-ERR >&2" >"$WORK/exec.out" 2>"$WORK/exec.err"

"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID1" --run-root "$RR" \
    --output "$WORK/snapshot" --timeout 60 --drop-caches=false >"$WORK/snapshot.out" 2>"$WORK/snapshot.err"
wait_run_exit
SNAP="$WORK/snapshot/$SID1.snapshot"
[ -f "$SNAP" ] || fail 'snapshot root is missing'
truncate -s 512M "$WORK/restored.ext4"
mkfs.ext4 -q -F "$WORK/restored.ext4"
cat > "$WORK/restore.yaml" <<YAML
resources:
  capacity: {cpu: 1, memory: 512MiB}
  allocatable: {cpu: 1, memory: 512MiB}
network: {tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: journal-e2e}
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: {diff: file://$WORK/restored.ext4}
YAML
start_run restore "$SID2" "$WORK/restore.yaml" --restore "$SNAP"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$RR" -- \
    /bin/sh -c 'echo restored > /journal-command' >"$WORK/restore-command.out" 2>"$WORK/restore-command.err"
# Wait for the primary app to consume the post-restore command before stopping.
found=0
for _ in $(seq 1 100); do
    journalctl --sync
    journalctl "TEST_ID=$CASE" PHASE=restore CHANNEL=stdout --no-pager -o cat >"$WORK/restore-markers.out"
    if grep -qx APP-OUT-restored "$WORK/restore-markers.out"; then
        found=1
        break
    fi
    sleep 0.1
done
[ "$found" = 1 ] || fail 'restored application did not emit its marker'
"$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$RR" -- \
    /bin/sh -c 'echo stop > /journal-command' >"$WORK/stop.out" 2>"$WORK/stop.err"
wait_run_exit

# Target-bound startup failures are native entries, not copied to stderr.
status=0
"$BIN/sandbox-ctl" run --config "$WORK/missing.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --log-to "journald=$CTL,TEST_ID=$CASE,PHASE=failure,CHANNEL=component" \
    >"$WORK/failure.out" 2>"$WORK/failure.err" || status=$?
[ "$status" = 1 ] || fail "startup failure exit=$status"
[ ! -s "$WORK/failure.err" ] || fail 'native component failure duplicated to stderr'

journalctl --sync
journalctl "TEST_ID=$CASE" --no-pager -o json >"$WORK/entries.jsonl"
journalctl "SYSLOG_IDENTIFIER=$BARE" --no-pager -o json >"$WORK/bare.jsonl"
python3 - "$WORK" "$APP" "$CONSOLE" "$CTL" <<'PY'
import json
import pathlib
import sys
work = pathlib.Path(sys.argv[1])
app, console, ctl = sys.argv[2:]
rows = [json.loads(line) for line in (work / 'entries.jsonl').read_text().splitlines()]
def selected(phase, channel):
    return [r for r in rows if r.get('PHASE') == phase and r.get('CHANNEL') == channel]
def one(phase, channel, message):
    matches = [r for r in selected(phase, channel) if r.get('MESSAGE') == message]
    assert len(matches) == 1, (phase, channel, message, matches)
for phase, marker in [('cold', 'cold'), ('restore', 'restored')]:
    one(phase, 'stdout', 'APP-OUT-' + marker)
    one(phase, 'stderr', 'APP-ERR-' + marker)
    assert selected(phase, 'component'), ('missing component diagnostics', phase)
assert selected('cold', 'console'), 'missing cold console output'
one('exec', 'stdout', 'EXEC-OUT')
for r in rows:
    channel = r.get('CHANNEL')
    assert channel in ('stdout', 'stderr', 'console', 'component'), r
    expected_tag = app if channel in ('stdout', 'stderr') else console if channel == 'console' else ctl
    assert r.get('SYSLOG_IDENTIFIER') == expected_tag, r
    if channel == 'stdout' and r.get('PHASE') in ('cold', 'restore'):
        assert r.get('NOTE') == 'a,b%2C+c=d', r
    else:
        assert 'NOTE' not in r, ('cross-output field leak', r)
failure = selected('failure', 'component')
assert len(failure) == 1 and 'missing.yaml' in failure[0].get('MESSAGE', ''), failure
bare = [json.loads(line) for line in (work / 'bare.jsonl').read_text().splitlines()]
assert len(bare) == 1 and bare[0].get('MESSAGE') == 'EXEC-ERR', bare
assert not any(key in bare[0] for key in ('TEST_ID', 'PHASE', 'CHANNEL', 'NOTE')), bare
for path in work.glob('*.err'):
    assert 'APP-OUT-' not in path.read_text() and 'APP-ERR-' not in path.read_text(), path
print('PASS: independent journal fields, cold/restore component and app logs, console, exec tail and bare target')
PY
echo '==> e2e_sandbox_journald: OK'
