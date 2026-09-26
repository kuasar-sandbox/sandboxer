#!/usr/bin/env bash
# Bidirectional launch-protocol contract using only prepared products.
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
for command in docker ip mkfs.ext4 python3 tail timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -r "$SANDBOXER_LIB/tarstream.sh" ] || e2e_fail "missing prepared tarstream helper"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || \
    e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/runtime"
TAP_NAME="sp$((BASHPID % 1000000))"
BLK0_IMAGE="$WORK/blk0.img"
DIFF_FILE="$WORK/runtime/blk1.diff"
LOG="$OUT/protocol.log"
STATS_JSON="$OUT/protocol-stats.json"
TAP_CREATED=0

cleanup() {
    local status=$?
    set +e
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ip tuntap add dev "$TAP_NAME" mode tap
TAP_CREATED=1
ip addr add 169.254.1.0/31 dev "$TAP_NAME"
ip link set "$TAP_NAME" up

docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
[ -s "$BLK0_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"
truncate -s 1G "$DIFF_FILE"
mkfs.ext4 -q -F -O ^has_journal "$DIFF_FILE"

cat >"$WORK/sandbox.yaml" <<EOF
resources:
  capacity:
    cpu: 1
    memory: 512MiB
  allocatable:
    cpu: 1
    memory: 512MiB
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: e2e-protocol
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay:
      diff: file://$DIFF_FILE
launch:
  args:
    - "-c"
    - "import time,sys; print('PROTO-BOOT-OK'); sys.stdout.flush(); time.sleep(4); print('PROTO-DONE')"
  restart: never
EOF

set +e
timeout -k 10s 40s "$BIN/sandbox-ctl" run \
    --config "$WORK/sandbox.yaml" \
    --sandbox-id "proto-$((BASHPID % 1000000))" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    --stats-json "$STATS_JSON" \
    >"$LOG" 2>&1
rc=$?
set -e
if [ "$rc" -ne 0 ]; then
    tail -80 "$LOG" >&2 || true
    e2e_fail "protocol sandbox failed with status $rc"
fi

assert_contains "$LOG" 'PROTO-BOOT-OK'
assert_contains "$LOG" 'PROTO-DONE'
grep -qE 'guest reports user app pid=[0-9]+' "$LOG" || e2e_fail "protocol log missing user app pid"
grep -qE 'guest reports user app exited code=[0-9]+' "$LOG" || e2e_fail "protocol log missing user app exit code"
[ -s "$STATS_JSON" ] || e2e_fail "stats-json output is missing"

python3 - "$STATS_JSON" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as stream:
    report = json.load(stream)
ping = report.get("ping")
assert ping is not None, "stats-json missing ping object"
attempts = ping.get("attempts", 0)
success = ping.get("success", 0)
rtt_avg = ping.get("rtt_avg_ns", 0)
rtt_max = ping.get("rtt_max_ns", 0)
assert attempts >= 2, f"ping attempts={attempts}, expected at least 2"
assert success >= 1, f"ping success={success}, expected at least 1"
assert success >= attempts // 2, f"ping success {success} is less than half of attempts {attempts}"
assert rtt_avg > 0, f"rtt_avg_ns={rtt_avg}"
assert rtt_max >= rtt_avg, f"rtt_max_ns={rtt_max} < rtt_avg_ns={rtt_avg}"
PY

echo "PASS sandbox.protocol.sh"
