#!/usr/bin/env bash
# Memory restore rejects cold-only host fields and resumes original process state unchanged.
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
for command in docker ip mkfs.ext4 python3 timeout truncate; do
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

mkdir -p "$WORK" "$OUT" "$WORK/runtime" "$WORK/snapshot"
TAP_NAME="rc$((BASHPID % 1000000))"
TAP_CREATED=0
PID1=""
PID2=""
cleanup() {
    local status=$?
    set +e
    for pid in "$PID1" "$PID2"; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
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
DIFF="$WORK/cold.diff"
truncate -s 1G "$DIFF"
mkfs.ext4 -q -F -O ^has_journal "$DIFF"

PYCODE='import time
print("PYBOOT-OK", flush=True)
i=0
while True:
    try:
        value=open("/etc/instance-id").read().strip()
    except Exception:
        value="none"
    print("TICK", i, "ID="+value, flush=True)
    i+=1
    time.sleep(0.25)'
PYCODE_JSON="$(printf '%s' "$PYCODE" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')"
cat >"$WORK/cold.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-restore-config }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$DIFF }
launch:
  args: ["-c", $PYCODE_JSON]
  restart: never
EOF

SID1="restore-config-cold-$BASHPID"
LOG1="$OUT/cold.log"
"$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" --sandbox-id "$SID1" >"$LOG1" 2>&1 &
PID1=$!
for _ in $(seq 1 600); do
    grep -qE '^TICK 10 ID=none$' "$LOG1" 2>/dev/null && break
    kill -0 "$PID1" 2>/dev/null || e2e_fail "cold sandbox exited before expected state"
    sleep 0.05
done
grep -qE '^TICK 10 ID=none$' "$LOG1" || e2e_fail "cold app did not establish ID=none state"

"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID1" --output "$WORK/snapshot" --run-root "$WORK/runtime" \
    >"$OUT/snapshot.log" 2>&1
wait "$PID1" || true
PID1=""
SNAPSHOT="$WORK/snapshot/$SID1.snapshot"
[ -f "$SNAPSHOT" ] || e2e_fail "snapshot is missing"

RESTORE_DIFF="$WORK/restore.diff"
truncate -s 1G "$RESTORE_DIFF"
mkfs.ext4 -q -F -O ^has_journal "$RESTORE_DIFF"
cat >"$WORK/bad.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-restore-config }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: { diff: file://$RESTORE_DIFF }
files:
  - path: /etc/instance-id
    content: "clone-42"
EOF

set +e
timeout -k 2s 20 "$BIN/sandbox-ctl" run --restore "$SNAPSHOT" --config "$WORK/bad.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --sandbox-id restore-config-reject \
    >"$OUT/reject.log" 2>&1
REJECT_RC=$?
set -e
[ "$REJECT_RC" -ne 0 ] || e2e_fail "restore accepted cold-only files"
grep -q 'files is cold-start-only' "$OUT/reject.log" || {
    cat "$OUT/reject.log" >&2
    e2e_fail "restore rejection did not identify cold-only files"
}
[ ! -e "$WORK/runtime/restore-config-reject/ch.sock" ] || e2e_fail "restore rejection reached Cloud Hypervisor creation"

cat >"$WORK/restore.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-restore-config }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: { diff: file://$RESTORE_DIFF }
EOF

SID2="restore-config-live-$BASHPID"
LOG2="$OUT/restore.log"
"$BIN/sandbox-ctl" run --restore "$SNAPSHOT" --config "$WORK/restore.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --sandbox-id "$SID2" >"$LOG2" 2>&1 &
PID2=$!
for _ in $(seq 1 400); do
    grep -qE '^TICK [0-9]+ ID=none$' "$LOG2" 2>/dev/null && break
    kill -0 "$PID2" 2>/dev/null || e2e_fail "restored sandbox exited before original process resumed"
    sleep 0.05
done
grep -qE '^TICK [0-9]+ ID=none$' "$LOG2" || e2e_fail "restored process state was not preserved"
if grep -qE '^TICK [0-9]+ ID=clone-42$' "$LOG2"; then
    e2e_fail "cold-only file injection appeared in restored process"
fi

kill -TERM "$PID2"
wait "$PID2" || true
PID2=""

echo "PASS snapshot.restore-config.sh"
