#!/usr/bin/env bash
# One freeze point produces an inspectable Snapshot S and runnable Sandbox E graph.
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
for command in docker ip mkfs.ext4 numfmt python3 stat tar truncate; do
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
TAP_NAME="sc$((BASHPID % 1000000))"
TAP_CREATED=0
RUNPID=""
cleanup() {
    local status=$?
    set +e
    [ -n "$RUNPID" ] && kill -0 "$RUNPID" 2>/dev/null && kill -TERM "$RUNPID" 2>/dev/null
    [ -n "$RUNPID" ] && wait "$RUNPID" 2>/dev/null
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

cat >"$WORK/sandbox.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-snapshot }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$DIFF }
launch:
  args: ["-c", "import sys,time\nprint('PYBOOT-OK', sys.version_info.major*100+sys.version_info.minor, flush=True)\ni=0\nwhile True:\n print('TICK', i, flush=True); i+=1; time.sleep(0.25)"]
  restart: never
EOF

SID="capture-$BASHPID"
LOG="$OUT/run.log"
"$BIN/sandbox-ctl" run --config "$WORK/sandbox.yaml" --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" --sandbox-id "$SID" >"$LOG" 2>&1 &
RUNPID=$!
for _ in $(seq 1 600); do
    grep -q '^TICK 5$' "$LOG" 2>/dev/null && break
    kill -0 "$RUNPID" 2>/dev/null || {
        tail -60 "$LOG" >&2 || true
        e2e_fail "sandbox exited before snapshot point"
    }
    sleep 0.05
done
grep -q '^TICK 5$' "$LOG" || e2e_fail "application never reached the snapshot point"

"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID" --output "$WORK/snapshot" \
    --run-root "$WORK/runtime" --resume >"$OUT/snapshot.log" 2>&1
SNAPSHOT="$WORK/snapshot/$SID.snapshot"
[ -f "$SNAPSHOT" ] || e2e_fail "snapshot alias is missing"
INFO_JSON="$("$BIN/sandbox-ctl" info --json "$SNAPSHOT")"
SANDBOX_REF="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("SandboxRef", ""))' <<<"$INFO_JSON")"
[ -n "$SANDBOX_REF" ] || e2e_fail "Snapshot S has no SandboxRef"
SANDBOX_BASENAME="$(python3 -c 'import os,sys; print(os.path.basename(sys.argv[1].split("@",1)[0]))' "$SANDBOX_REF")"
SANDBOX_FILE="$WORK/snapshot/$SANDBOX_BASENAME"
[ -f "$SANDBOX_FILE" ] || e2e_fail "Snapshot S references missing Sandbox E"

mapfile -t IMAGE_FILES < <(find "$WORK/snapshot" -maxdepth 1 -type f -name '*.image' -print | sort)
mapfile -t OVERLAY_FILES < <(find "$WORK/snapshot" -maxdepth 1 -type f -name '*.overlay' -print | sort)
[ "${#IMAGE_FILES[@]}" -eq 1 ] || e2e_fail "snapshot must contain exactly one immutable root image"
[ "${#OVERLAY_FILES[@]}" -eq 0 ] || e2e_fail "current writable root must stay in Sandbox E payload, not duplicate as .overlay"

E_INFO_JSON="$("$BIN/sandbox-ctl" info --json "$SANDBOX_FILE")"
python3 - <<'PY' "$E_INFO_JSON"
import json, sys
cfg = json.loads(sys.argv[1])
assert cfg["Version"] == 1, cfg
root = cfg["Boot"]["Root"]
assert root["Base"].split("@", 1)[0].endswith(".image"), root
assert root["Overlay"]["Base"] == "self", root
PY

SNAP_BYTES="$(stat -L -c%s "$SNAPSHOT")"
RAM_BYTES=$((512 * 1024 * 1024))
[ "$SNAP_BYTES" -gt $((1 << 20)) ] || e2e_fail "snapshot memory payload is unexpectedly empty"
[ "$SNAP_BYTES" -lt "$RAM_BYTES" ] || e2e_fail "snapshot envelope did not preserve sparse resident-memory semantics"

# --resume must return the VM to execution after the same freeze point.
BEFORE="$(grep '^TICK ' "$LOG" | tail -1 | awk '{print $2}')"
for _ in $(seq 1 100); do
    AFTER="$(grep '^TICK ' "$LOG" | tail -1 | awk '{print $2}')"
    [ "$AFTER" -gt "$BEFORE" ] 2>/dev/null && break
    sleep 0.05
done
AFTER="$(grep '^TICK ' "$LOG" | tail -1 | awk '{print $2}')"
[ "$AFTER" -gt "$BEFORE" ] || e2e_fail "sandbox did not resume after snapshot --resume"

kill -TERM "$RUNPID"
wait "$RUNPID" || true
RUNPID=""

echo "PASS snapshot.capture.sh"
