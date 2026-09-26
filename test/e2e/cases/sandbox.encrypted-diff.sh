#!/usr/bin/env bash
# Active encrypted root/data diffs remain sparse and guest-transparent.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker ip mkfs.ext4 od openssl python3 sha256sum stat tar timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor manifest-ctl store-ctl mkfs.erofs; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/run"
TAP_NAME="xd$((BASHPID % 1000000))"
TAP_CREATED=0
PIDS=()
cleanup() {
    local status=$?
    set +e
    for pid in "${PIDS[@]}"; do
        kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    done
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    exit "$status"
}
trap cleanup EXIT

ip tuntap add dev "$TAP_NAME" mode tap
TAP_CREATED=1
ip addr add 169.254.1.0/31 dev "$TAP_NAME"
ip link set "$TAP_NAME" up

free_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1]); s.close()'
}
KEY="$(openssl rand -hex 32)"
STORE_PORT="$(free_port)"
cat >"$WORK/store.yaml" <<EOF
listen: 127.0.0.1:$STORE_PORT
backend: fs
fs: { root: $WORK/store-data, verify_content_key: true }
EOF
"$BIN/store-ctl" init --config "$WORK/store.yaml" --generation G1
"$BIN/store-ctl" serve --config "$WORK/store.yaml" >"$OUT/store.log" 2>&1 &
STORE_PID=$!
PIDS+=("$STORE_PID")
for _ in $(seq 1 100); do
    (echo >/dev/tcp/127.0.0.1/"$STORE_PORT") 2>/dev/null && break
    sleep 0.05
done
(echo >/dev/tcp/127.0.0.1/"$STORE_PORT") 2>/dev/null || e2e_fail "store did not become ready"

write_manifest_config() {
    local path=$1 policy=$2
    cat >"$path" <<EOF
manifest: { key: "$KEY" }
store: { endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 30s }
cache: { endpoint: "" }
chunker: { mode: cdc }
crypto: { chunk: aes, manifest: aes, local: $policy }
EOF
}
AUTO_CONFIG="$WORK/manifest-auto.yaml"
REQUIRED_CONFIG="$WORK/manifest-required.yaml"
write_manifest_config "$AUTO_CONFIG" auto
write_manifest_config "$REQUIRED_CONFIG" required

ROOT_PLAIN="$WORK/root.plain"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_PLAIN" --no-progress
mkdir -p "$WORK/dataset-source"
printf 'DATASET-BASE-OK\n' >"$WORK/dataset-source/DATASET-BASE-OK"
MKFS_EROFS_PATH="$BIN/mkfs.erofs" "$BIN/flatten-ctl" export --no-progress \
    --tmpdir "$WORK/flatten-tmp" --output "$WORK/dataset.plain" "$WORK/dataset-source"

key_bound_ref() {
    local plain=$1 encrypted=$2 manifest_key plain_digest digest
    manifest_key=$("$BIN/manifest-ctl" store --manifest-config "$AUTO_CONFIG" --no-progress "$plain")
    "$BIN/manifest-ctl" load --manifest-config "$REQUIRED_CONFIG" --no-progress \
        --output "$encrypted" "$manifest_key"
    plain_digest=$(tar -tf "$plain" | sed -n 's/^\.kuasar\.digest\.\([0-9a-f]\{64\}\)$/\1/p')
    [ "${#plain_digest}" -eq 64 ] || e2e_fail "plaintext fixture has no canonical digest marker"
    digest=$(python3 - "$KEY" "$plain_digest" <<'PY'
import hashlib, hmac, sys
print(hmac.new(bytes.fromhex(sys.argv[1]), bytes.fromhex(sys.argv[2]), hashlib.sha256).hexdigest())
PY
)
    printf 'file://%s@hmac:%s\n' "$encrypted" "$digest"
}
ROOT_REF="$(key_bound_ref "$ROOT_PLAIN" "$WORK/root.encrypted")"
DATASET_REF="$(key_bound_ref "$WORK/dataset.plain" "$WORK/dataset.encrypted")"

make_template() {
    truncate -s "$2" "$1"
    mkfs.ext4 -q -F -O ^has_journal "$1"
}
make_template "$WORK/root-template.ext4" 512M
make_template "$WORK/scratch-template.ext4" 256M
make_template "$WORK/dataset-template.ext4" 256M
ROOT_TEMPLATE_SHA="$(sha256sum "$WORK/root-template.ext4" | awk '{print $1}')"
SCRATCH_TEMPLATE_SHA="$(sha256sum "$WORK/scratch-template.ext4" | awk '{print $1}')"
DATASET_TEMPLATE_SHA="$(sha256sum "$WORK/dataset-template.ext4" | awk '{print $1}')"

ROOT_DIFF="$WORK/root-active.diff"
SCRATCH_DIFF="$WORK/scratch-active.diff"
DATASET_DIFF="$WORK/dataset-active.diff"
cat >"$WORK/sandbox.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: encrypted-diff }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$ROOT_DIFF, diff_template: file://$WORK/root-template.ext4 }
  disks:
    - { name: scratch, diff: file://$SCRATCH_DIFF, diff_template: file://$WORK/scratch-template.ext4 }
    - { name: dataset, base: $DATASET_REF, overlay: { diff: file://$DATASET_DIFF, diff_template: file://$WORK/dataset-template.ext4 } }
mounts:
  - { target: /scratch, type: disk, source: scratch }
  - { target: /data, type: disk, source: dataset }
launch: { exec: /bin/sleep, args: ["3600"], restart: never }
EOF

SID="encrypted-active-$BASHPID"
LOG="$OUT/sandbox.log"
"$BIN/sandbox-ctl" run --config "$WORK/sandbox.yaml" --manifest-config "$REQUIRED_CONFIG" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/run" --sandbox-id "$SID" >"$LOG" 2>&1 &
RUN_PID=$!
PIDS+=("$RUN_PID")
for _ in $(seq 1 120); do
    if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/run" -- /bin/true >/dev/null 2>&1; then
        break
    fi
    kill -0 "$RUN_PID" 2>/dev/null || {
        tail -80 "$LOG" >&2 || true
        e2e_fail "encrypted sandbox exited before becoming executable"
    }
    sleep 1
done
timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/run" -- /bin/true >/dev/null 2>&1 || \
    e2e_fail "encrypted sandbox did not become executable"

"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/run" -- /bin/sh -c \
    'set -e; grep -qx DATASET-BASE-OK /data/DATASET-BASE-OK; echo ROOT-ACTIVE-OK > /root-active; echo SCRATCH-ACTIVE-OK > /scratch/persist; echo DATA-ACTIVE-OK > /data/persist; sync'

assert_diff() {
    local path=$1 logical=$2 magic physical blocks block_size allocated
    [ -f "$path" ] || e2e_fail "missing active diff: $path"
    magic=$(od -An -tx1 -N8 "$path" | tr -d ' \n')
    [ "$magic" = 894b44585453310a ] || e2e_fail "$path does not have the KDXTS1 header"
    physical=$(stat -c %s "$path")
    [ "$physical" -eq $((logical + 4096)) ] || e2e_fail "$path is not length-preserving plus its 4KiB header"
    read -r blocks block_size < <(stat -c '%b %B' "$path")
    allocated=$((blocks * block_size))
    [ "$allocated" -lt "$physical" ] || e2e_fail "$path lost sparse-body semantics"
}
assert_diff "$ROOT_DIFF" $((512 * 1024 * 1024))
assert_diff "$SCRATCH_DIFF" $((256 * 1024 * 1024))
assert_diff "$DATASET_DIFF" $((256 * 1024 * 1024))
[ "$(sha256sum "$WORK/root-template.ext4" | awk '{print $1}')" = "$ROOT_TEMPLATE_SHA" ] || e2e_fail "root template was modified"
[ "$(sha256sum "$WORK/scratch-template.ext4" | awk '{print $1}')" = "$SCRATCH_TEMPLATE_SHA" ] || e2e_fail "scratch template was modified"
[ "$(sha256sum "$WORK/dataset-template.ext4" | awk '{print $1}')" = "$DATASET_TEMPLATE_SHA" ] || e2e_fail "dataset template was modified"

HEADERS=$(for path in "$ROOT_DIFF" "$SCRATCH_DIFF" "$DATASET_DIFF"; do
    dd if="$path" bs=4096 count=1 status=none | sha256sum | awk '{print $1}'
done | sort -u | wc -l)
[ "$HEADERS" -eq 3 ] || e2e_fail "active diffs reused a wrapped header/XTS key"
for marker in ROOT-ACTIVE-OK SCRATCH-ACTIVE-OK DATA-ACTIVE-OK; do
    if grep -aFq "$marker" "$ROOT_DIFF" "$SCRATCH_DIFF" "$DATASET_DIFF"; then
        e2e_fail "guest plaintext marker appears in an encrypted active diff"
    fi
done

kill -TERM "$RUN_PID"
wait "$RUN_PID" || true
PIDS=()

echo "PASS sandbox.encrypted-diff.sh"
