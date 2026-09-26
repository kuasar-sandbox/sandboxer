#!/usr/bin/env bash
# Sandbox stdio routing contract using only prepared products and the prepared image.
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
for command in docker ip mkfs.ext4 timeout truncate; do
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

mkdir -p "$WORK" "$OUT" "$WORK/runtime"
TAP_NAME="ss$((BASHPID % 1000000))"
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

write_config() {
    local output=$1
    local diff=$2
    truncate -s 1G "$diff"
    mkfs.ext4 -q -F -O ^has_journal "$diff"
    cat >"$output" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: e2e-stdio
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay:
      diff: file://$diff
launch:
  args: ["-c", "print('STDIO-MARKER-12345', flush=True)"]
  restart: never
EOF
}

write_config "$WORK/file.yaml" "$WORK/file.diff"
FILE_STDOUT="$OUT/file.stdout"
FILE_LOG="$OUT/file.log"
timeout -k 10s 90 "$BIN/sandbox-ctl" run \
    --config "$WORK/file.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    --sandbox-id "stdio-file-$BASHPID" \
    --stdout-to "$FILE_STDOUT" \
    >"$FILE_LOG" 2>&1
assert_contains "$FILE_STDOUT" 'STDIO-MARKER-12345'
if grep -q 'STDIO-MARKER-12345' "$FILE_LOG"; then
    e2e_fail "--stdout-to marker leaked to sandbox-ctl stdout"
fi

write_config "$WORK/drop.yaml" "$WORK/drop.diff"
DROP_LOG="$OUT/drop.log"
timeout -k 10s 90 "$BIN/sandbox-ctl" run \
    --config "$WORK/drop.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    --sandbox-id "stdio-drop-$BASHPID" \
    --stdout=false \
    >"$DROP_LOG" 2>&1
if grep -q 'STDIO-MARKER-12345' "$DROP_LOG"; then
    e2e_fail "--stdout=false leaked guest stdout"
fi

write_config "$WORK/inherit.yaml" "$WORK/inherit.diff"
INHERIT_LOG="$OUT/inherit.log"
timeout -k 10s 90 "$BIN/sandbox-ctl" run \
    --config "$WORK/inherit.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    --sandbox-id "stdio-inherit-$BASHPID" \
    >"$INHERIT_LOG" 2>&1
assert_contains "$INHERIT_LOG" 'STDIO-MARKER-12345'

echo "PASS sandbox.stdio.sh"
