#!/usr/bin/env bash
# Auto-default overlay diff seeded from a prepared ext4 template.
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
for command in docker dumpe2fs ip mkfs.ext4 timeout truncate; do
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

mkdir -p "$WORK" "$OUT" "$WORK/run" "$WORK/base"
TAP_NAME="dt$((BASHPID % 1000000))"
TAP_CREATED=0
cleanup() {
    local status=$?
    set +e
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

TEMPLATE="$WORK/basic.ext4"
truncate -s 512M "$TEMPLATE"
mkfs.ext4 -q -F -O ^has_journal "$TEMPLATE"
FEATURES="$(LC_ALL=C dumpe2fs -h "$TEMPLATE" | sed -n 's/^Filesystem features: *//p')"
[ -n "$FEATURES" ] || e2e_fail "could not read ext4 template features"
[[ " $FEATURES " != *" has_journal "* ]] || e2e_fail "diff template unexpectedly contains an ext4 journal"

cat >"$WORK/sandbox.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-dt }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay:
      diff_template: file://$TEMPLATE
launch:
  args: ["-c", "import sys; print('PYBOOT-OK', sys.version_info.major*100+sys.version_info.minor, flush=True)"]
  restart: never
EOF

LOG="$OUT/diff-template.log"
timeout -k 10s 90 "$BIN/sandbox-ctl" run \
    --config "$WORK/sandbox.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/run" \
    --base-root "$WORK/base" >"$LOG" 2>&1

grep -qE '^PYBOOT-OK [0-9]+$' "$LOG" || {
    tail -60 "$LOG" >&2 || true
    e2e_fail "app did not run on template-seeded auto-default diff"
}
# No explicit diff path was supplied; sandboxer must have materialized one under base-root.
find "$WORK/base" -type f -name '*.overlay.diff' -size +0c | grep -q . || \
    e2e_fail "auto-default overlay diff was not materialized under base-root"

echo "PASS sandbox.diff-template.sh"
