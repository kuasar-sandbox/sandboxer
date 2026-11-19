#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","tun"]
# All template source types and size-only disks use the real Guest block path.
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
for command in docker dumpe2fs ip mkfs.ext4 timeout truncate python3; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor manifest-ctl store-ctl; do
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
STORE_PID=""
: "${ARTIFACT_REF_BIN:?prepared artifact-ref helper is required}"
cleanup() {
    local status=$?
    set +e
    if [ -n "$STORE_PID" ]; then kill "$STORE_PID" 2>/dev/null; wait "$STORE_PID" 2>/dev/null; fi
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

# Carrier packaging runs through an already-built owner helper.
mkdir -p "$WORK/carriers"
"$ARTIFACT_REF_BIN" template-carriers "$TEMPLATE" "$WORK/carriers" >"$WORK/templates.json"
fixture() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$WORK/templates.json" "$1"; }
KEY="$(fixture customer_key)"
export MANIFEST_KEY="$KEY"
STORE_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
cat >"$WORK/store.yaml" <<EOF
listen: 127.0.0.1:$STORE_PORT
backend: fs
fs: { root: $WORK/store-data, verify_content_key: true }
EOF
"$BIN/store-ctl" init --config "$WORK/store.yaml" --generation G1
"$BIN/store-ctl" serve --config "$WORK/store.yaml" >"$OUT/store.log" 2>&1 &
STORE_PID=$!
for _ in $(seq 1 100); do
    (echo >/dev/tcp/127.0.0.1/"$STORE_PORT") 2>/dev/null && break
    sleep 0.05
done
(echo >/dev/tcp/127.0.0.1/"$STORE_PORT") 2>/dev/null || e2e_fail "store did not become ready"
cat >"$WORK/manifest.yaml" <<EOF
manifest: { key: "$KEY", verify_content: false, write_generation: G1 }
store: { endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 30s }
chunker: { mode: cdc }
crypto: { chunk: aes, manifest: aes, local: auto }
EOF
MANIFEST_KEY_HEX="$("$BIN/manifest-ctl" store --manifest-config "$WORK/manifest.yaml" --no-progress "$(fixture bundle)")"
[[ "$MANIFEST_KEY_HEX" =~ ^[0-9a-f]{64}$ ]] || e2e_fail "invalid stored template identity"

declare -A REFS
for mode in raw plain encrypted bundle; do REFS[$mode]="$(fixture "$mode")"; done
REFS[manifest]="manifest://$MANIFEST_KEY_HEX"
REFS[mkfs]="mkfs://ext4?size=512MiB"
run_source() {
    local mode=$1 phase=$2
    cat >"$WORK/$mode.yaml" <<EOF
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
      diff: file://$WORK/$mode.diff
      diff_template: ${REFS[$mode]}
  disks:
    - name: generated
      disk: { kind: disk, version: 1, size: 67108864 }
mounts:
  - { type: disk, source: generated, target: /generated }
launch:
  args: ["-c", "import os,pathlib; p=pathlib.Path('/template-probe'); prior=p.exists(); assert prior == ('$phase' == 'reopen'); p.write_bytes(b'root-persisted'); q=pathlib.Path('/generated/probe'); q.write_bytes(b'generated-data'); os.sync(); assert q.read_bytes() == b'generated-data'; print('PYBOOT-OK', '$mode', '$phase', flush=True)"]
  restart: never
EOF
    local log="$OUT/$mode-$phase.log" stdout="$OUT/$mode-$phase.stdout"
    : >"$stdout"
    timeout -k 10s 90 "$BIN/sandbox-ctl" run \
        --config "$WORK/$mode.yaml" --manifest-config "$WORK/manifest.yaml" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/run" \
        --base-root "$WORK/base" --stdout-to "$stdout" >"$log" 2>&1
    grep -qx "PYBOOT-OK $mode $phase" "$stdout" || {
        cat "$stdout" >&2 || true
        tail -60 "$log" >&2 || true
        e2e_fail "Guest template source $mode/$phase failed"
    }
}
for mode in raw plain encrypted bundle manifest mkfs; do run_source "$mode" fresh; done
# The retained DIFF must own every template byte before normal shutdown returns.
# Reopening each disk with unavailable original inputs proves independence.
kill "$STORE_PID"
wait "$STORE_PID" || true
STORE_PID=""
rm -rf "$WORK/carriers"
rm -f "$TEMPLATE"
REFS[mkfs]="mkfs://ext4?size=1"
for mode in raw plain encrypted bundle manifest mkfs; do run_source "$mode" reopen; done
echo "PASS sandbox.diff-template.sh"
