#!/usr/bin/env bash
# Launch-spec contract using only prepared products and the prepared image.
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
for command in docker ip mkfs.ext4 sed tail timeout truncate; do
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
TAP_NAME="sl$((BASHPID % 1000000))"
BLK0_IMAGE="$WORK/blk0.img"
DIFF_FILE="$WORK/runtime/blk1.diff"
LOG="$OUT/launch.log"
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

echo "==> flatten prepared image"
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
  hostname: e2e-launchspec
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay:
      diff: file://$DIFF_FILE
mounts:
  - { target: /tmp, type: tmpfs, options: "nosuid,nodev,mode=1777" }
  - { target: /var/log, type: empty }
files:
  - path: /etc/resolv.conf
    mode: "0644"
    content: |
      nameserver 169.254.169.253
  - path: /verify.sh
    mode: "0755"
    content: |
      #!/bin/sh
      echo "LS-WHOAMI \$(id -u):\$(id -g)"
      echo "LS-RESOLV \$(head -1 /etc/resolv.conf)"
      grep -q " /tmp tmpfs " /proc/self/mounts && echo LS-TMPFS-OK
      grep -qE " /var/log (ext4|overlay) " /proc/self/mounts && echo LS-VOLUME-OK
      [ -f /tmp/ls-init-ran ] && echo "LS-INIT-OK \$(cat /tmp/ls-init-ran)"
      echo LS-DONE
init:
  - exec: /bin/sh
    args: ["-c", "echo provisioned > /tmp/ls-init-ran"]
launch:
  exec: /bin/sh
  args: ["/verify.sh"]
  user: "nobody"
  stop_signal: SIGTERM
  stop_grace_period: 5s
  restart: never
EOF

echo "==> launch prepared sandbox"
set +e
timeout -k 10s 70s "$BIN/sandbox-ctl" run \
    --config "$WORK/sandbox.yaml" \
    --sandbox-id "launch-$((BASHPID % 1000000))" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    >"$LOG" 2>&1
rc=$?
set -e
if [ "$rc" -ne 0 ]; then
    tail -80 "$LOG" >&2 || true
    e2e_fail "launch-spec sandbox failed with status $rc"
fi

grep -qE '^LS-DONE$' "$LOG" || e2e_fail "launch log missing LS-DONE"
grep -qE '^LS-WHOAMI 65534:65534$' "$LOG" || e2e_fail "launch log missing LS-WHOAMI"
grep -qE '^LS-RESOLV nameserver 169\.254\.169\.253$' "$LOG" || e2e_fail "launch log missing LS-RESOLV"
grep -qE '^LS-TMPFS-OK$' "$LOG" || e2e_fail "launch log missing LS-TMPFS-OK"
grep -qE '^LS-VOLUME-OK$' "$LOG" || e2e_fail "launch log missing LS-VOLUME-OK"
grep -qE '^LS-INIT-OK provisioned$' "$LOG" || e2e_fail "launch log missing LS-INIT-OK"

echo "PASS sandbox.launch.sh"
