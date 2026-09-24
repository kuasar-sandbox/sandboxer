#!/usr/bin/env bash
# Independent journald targets across cold, exec, snapshot/restore and startup failure.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker ip journalctl mkfs.ext4 python3 truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -r "$SANDBOXER_LIB/tarstream.sh" ] || e2e_fail "missing prepared tarstream helpers"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"
[ -S /run/systemd/journal/socket ] || e2e_fail "native journal socket is unavailable"
journalctl --sync || e2e_fail "journal is not accessible"

mkdir -p "$WORK" "$OUT"
CASE="journal-$BASHPID-${WORK##*/}"
APP="e2e-app-$BASHPID"
CONSOLE="e2e-console-$BASHPID"
CTL="e2e-ctl-$BASHPID"
BARE="e2e-bare-$CASE"
TAP_NAME="jl$((BASHPID % 1000000))"
TAP_CREATED=0
PID=""
cleanup() {
    local status=$?
    set +e
    if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
        kill -TERM "$PID" 2>/dev/null
        for _ in $(seq 1 100); do
            kill -0 "$PID" 2>/dev/null || break
            sleep 0.1
        done
        kill -0 "$PID" 2>/dev/null && kill -KILL "$PID" 2>/dev/null
        wait "$PID" 2>/dev/null
    fi
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    if [ "$status" != 0 ]; then
        journalctl "TEST_ID=$CASE" --no-pager -n 80 >&2 || true
    fi
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

RUN_ROOT="$WORK/run"
BASE_ROOT="$WORK/base"
mkdir -p "$RUN_ROOT" "$BASE_ROOT" "$WORK/snapshot"
truncate -s 512M "$WORK/template.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/template.ext4"
cat >"$WORK/cold.yaml" <<EOF
resources:
  capacity: {cpu: 1, memory: 512MiB}
  allocatable: {cpu: 1, memory: 512MiB}
network: {tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: journal-e2e}
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: 'console=hvc0 printk.time=1'
  root:
    base: $ROOT_REF
    overlay: {diff_template: file://$WORK/template.ext4}
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
          next=\$(cat /journal-command)
          if [ "\$next" != "\$last" ]; then
            printf 'APP-OUT-%s\n' "\$next"
            printf 'APP-ERR-%s\n' "\$next" >&2
            last=\$next
          fi
          [ "\$next" != stop ] || exit 0
        fi
        sleep 0.1
      done
  restart: never
EOF

start_run() {
    local phase=$1 sid=$2 config=$3
    shift 3
    "$BIN/sandbox-ctl" run --config "$config" --sandbox-id "$sid" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" \
        --ready-fd 3 \
        --stdout-to "journald=$APP,TEST_ID=$CASE,PHASE=$phase,CHANNEL=stdout,NOTE=a%2Cb%252C+c%3Dd" \
        --stderr-to "journald=$APP,TEST_ID=$CASE,PHASE=$phase,CHANNEL=stderr" \
        --console "journald=$CONSOLE,TEST_ID=$CASE,PHASE=$phase,CHANNEL=console" \
        --log-to "journald=$CTL,TEST_ID=$CASE,PHASE=$phase,CHANNEL=component" \
        "$@" 3>"$WORK/$phase.ready" >"$OUT/$phase.out" 2>"$OUT/$phase.err" &
    PID=$!
    for _ in $(seq 1 400); do
        grep -qx ready "$WORK/$phase.ready" && return 0
        kill -0 "$PID" 2>/dev/null || e2e_fail "$phase run exited before ready"
        sleep 0.15
    done
    e2e_fail "$phase run did not become ready"
}

wait_run_exit() {
    for _ in $(seq 1 400); do
        if ! kill -0 "$PID" 2>/dev/null; then
            local status=0
            wait "$PID" || status=$?
            PID=""
            [ "$status" = 0 ] || e2e_fail "run exited with status $status"
            return 0
        fi
        sleep 0.15
    done
    e2e_fail "run did not exit"
}

SID1="journal-cold-$BASHPID"
SID2="journal-restore-$BASHPID"
start_run cold "$SID1" "$WORK/cold.yaml"

# Naked exec stderr target must not inherit fields from the running VM target.
"$BIN/sandbox-ctl" exec --sandbox-id "$SID1" --run-root "$RUN_ROOT" \
    --stdout-to "journald=$APP,TEST_ID=$CASE,PHASE=exec,CHANNEL=stdout" \
    --stderr-to "journald=$BARE" -- /bin/sh -c \
    "printf EXEC-OUT; printf EXEC-ERR >&2" >"$OUT/exec.out" 2>"$OUT/exec.err"

"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID1" --run-root "$RUN_ROOT" \
    --output "$WORK/snapshot" --timeout 60 --drop-caches=false \
    >"$OUT/snapshot.out" 2>"$OUT/snapshot.err"
wait_run_exit
SNAPSHOT="$WORK/snapshot/$SID1.snapshot"
[ -f "$SNAPSHOT" ] || e2e_fail "journal snapshot is missing"

truncate -s 512M "$WORK/restored.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/restored.ext4"
cat >"$WORK/restore.yaml" <<EOF
resources:
  capacity: {cpu: 1, memory: 512MiB}
  allocatable: {cpu: 1, memory: 512MiB}
network: {tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: journal-e2e}
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: {diff: file://$WORK/restored.ext4}
EOF
start_run restore "$SID2" "$WORK/restore.yaml" --restore "$SNAPSHOT"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$RUN_ROOT" -- \
    /bin/sh -c 'echo restored > /journal-command' >"$OUT/restore-command.out" 2>"$OUT/restore-command.err"

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
[ "$found" = 1 ] || e2e_fail "restored application did not emit its marker"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$RUN_ROOT" -- \
    /bin/sh -c 'echo stop > /journal-command' >"$OUT/stop.out" 2>"$OUT/stop.err"
wait_run_exit

# Target-bound startup failures are native entries and must not duplicate to stderr.
status=0
"$BIN/sandbox-ctl" run --config "$WORK/missing.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --log-to "journald=$CTL,TEST_ID=$CASE,PHASE=failure,CHANNEL=component" \
    >"$OUT/failure.out" 2>"$OUT/failure.err" || status=$?
[ "$status" = 1 ] || e2e_fail "startup failure returned status $status"
[ ! -s "$OUT/failure.err" ] || e2e_fail "native component failure duplicated to stderr"

journalctl --sync
journalctl "TEST_ID=$CASE" --no-pager -o json >"$WORK/entries.jsonl"
journalctl "SYSLOG_IDENTIFIER=$BARE" --no-pager -o json >"$WORK/bare.jsonl"
python3 - "$WORK" "$APP" "$CONSOLE" "$CTL" <<'PY'
import json
import pathlib
import sys

work = pathlib.Path(sys.argv[1])
app, console, ctl = sys.argv[2:]
rows = [json.loads(line) for line in (work / "entries.jsonl").read_text().splitlines()]

def selected(phase, channel):
    return [r for r in rows if r.get("PHASE") == phase and r.get("CHANNEL") == channel]

def one(phase, channel, message):
    matches = [r for r in selected(phase, channel) if r.get("MESSAGE") == message]
    assert len(matches) == 1, (phase, channel, message, matches)

for phase, marker in [("cold", "cold"), ("restore", "restored")]:
    one(phase, "stdout", "APP-OUT-" + marker)
    one(phase, "stderr", "APP-ERR-" + marker)
    assert selected(phase, "component"), ("missing component diagnostics", phase)
assert selected("cold", "console"), "missing cold console output"
one("exec", "stdout", "EXEC-OUT")
for row in rows:
    channel = row.get("CHANNEL")
    assert channel in ("stdout", "stderr", "console", "component"), row
    expected_tag = app if channel in ("stdout", "stderr") else console if channel == "console" else ctl
    assert row.get("SYSLOG_IDENTIFIER") == expected_tag, row
    if channel == "stdout" and row.get("PHASE") in ("cold", "restore"):
        assert row.get("NOTE") == "a,b%2C+c=d", row
    else:
        assert "NOTE" not in row, ("cross-output field leak", row)
failure = selected("failure", "component")
assert len(failure) == 1 and "missing.yaml" in failure[0].get("MESSAGE", ""), failure
bare = [json.loads(line) for line in (work / "bare.jsonl").read_text().splitlines()]
assert len(bare) == 1 and bare[0].get("MESSAGE") == "EXEC-ERR", bare
assert not any(key in bare[0] for key in ("TEST_ID", "PHASE", "CHANNEL", "NOTE")), bare
PY

echo "PASS sandbox.journal.sh"
