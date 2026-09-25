#!/usr/bin/env bash
# Image-to-Sandbox-E assembly produces direct EROFS artifacts without a VM.
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
for command in docker grep ip mkfs.ext4 python3 timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl cloud-hypervisor flatten-ctl; do
    require_binary "$binary"
done
for file in sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/run" "$WORK/base"
RUN_ROOT="$WORK/run"
BASE_ROOT="$WORK/base"
TAP_NAME="xa$((BASHPID % 1000000))"
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
ROOT_REF=$(plaintext_tarstream_ref "$ROOT_IMAGE")
TEMPLATE="$WORK/upper-template.ext4"
truncate -s 512M "$TEMPLATE"
mkfs.ext4 -q -F -O ^has_journal "$TEMPLATE"

cat >"$WORK/assembly.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff_template: file://$TEMPLATE }
launch: { exec: /bin/true, restart: never }
EOF

ASSEMBLY_OUT="$OUT/assembly"
mkdir -p "$ASSEMBLY_OUT"
"$BIN/sandbox-ctl" export --from "$ROOT_IMAGE" --config "$WORK/assembly.yaml" --sandbox-id assembly --output "$ASSEMBLY_OUT" \
    >"$OUT/assembly-export.log"
ASSEMBLY_E="$ASSEMBLY_OUT/assembly.sandbox"
[ -L "$ASSEMBLY_E" ] || e2e_fail "assembled Sandbox E alias was not committed"
"$BIN/sandbox-ctl" info --json "$ASSEMBLY_E" >"$WORK/assembly.json"
python3 - "$WORK/assembly.json" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1], encoding='utf-8'))
root = cfg['Boot']['Root']
overlay = root.get('Overlay') or {}
if root.get('Base') != 'self' or root.get('BaseFromRefs'):
    raise SystemExit(f'assembled E does not use direct EROFS self base: {root!r}')
if overlay.get('Base') or overlay.get('BaseFromRefs'):
    raise SystemExit(f'assembled E unexpectedly retained lower overlay refs: {overlay!r}')
PY

# Missing a formatted writable upper is a config/disk preflight error. Record
# whether the VMM wrapper was ever entered; failure after CH start is too late.
CH_MARKER="$WORK/ch-started"
CH_WRAPPER="$WORK/cloud-hypervisor-marker"
cat >"$CH_WRAPPER" <<EOF
#!/bin/sh
touch "$CH_MARKER"
exec "$BIN/cloud-hypervisor" "\$@"
EOF
chmod 0755 "$CH_WRAPPER"
cat >"$WORK/missing-upper.yaml" <<EOF
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root: { overlay: {} }
EOF
if "$BIN/sandbox-ctl" run --from "$ASSEMBLY_E" --config "$WORK/missing-upper.yaml" --sandbox-id assembly-reject \
    --ch-binary "$CH_WRAPPER" --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" >"$OUT/missing-upper.log" 2>&1; then
    e2e_fail "direct EROFS run accepted a missing writable upper"
fi
[ ! -e "$CH_MARKER" ] || e2e_fail "missing writable upper failed only after Cloud Hypervisor started"
grep -qE 'diff_template|mountable|formatted|upper' "$OUT/missing-upper.log" \
    || { cat "$OUT/missing-upper.log" >&2; e2e_fail "missing-upper error lacked field context"; }

cat >"$WORK/host.yaml" <<EOF
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root: { overlay: { diff_template: file://$TEMPLATE } }
launch:
  exec: /bin/sh
  args: ["-c", "echo ASSEMBLY-RUN-OK"]
  restart: never
EOF
if ! timeout -k 10s 90 "$BIN/sandbox-ctl" run --from "$ASSEMBLY_E" --config "$WORK/host.yaml" --sandbox-id assembly-run \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" >"$OUT/assembly-run.log" 2>&1; then
    tail -100 "$OUT/assembly-run.log" >&2 || true
    e2e_fail "direct EROFS run --from failed"
fi
grep -q '^ASSEMBLY-RUN-OK$' "$OUT/assembly-run.log" \
    || { tail -100 "$OUT/assembly-run.log" >&2 || true; e2e_fail "assembled Sandbox E did not start its cold app"; }

echo "PASS image.sandbox-assembly.sh"
