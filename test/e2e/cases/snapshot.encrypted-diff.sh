#!/usr/bin/env bash
# Encrypted local and remote snapshots preserve state and key-bound references.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker find grep ip mkfs.ext4 od openssl python3 readlink sha256sum stat tar timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor manifest-ctl store-ctl mkfs.erofs; do
    require_binary "$binary"
done
for file in sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/run"
RUN_ROOT="$WORK/run"
TAP_NAME="xs$((BASHPID % 1000000))"
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
    local path=$1 policy=$2 key=$3
    cat >"$path" <<EOF
manifest: { key: "$key" }
store: { endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 30s }
cache: { endpoint: "" }
chunker: { mode: cdc }
crypto: { chunk: aes, manifest: aes, local: $policy }
EOF
}
AUTO_CONFIG="$WORK/manifest-auto.yaml"
REQUIRED_CONFIG="$WORK/manifest-required.yaml"
WRONG_CONFIG="$WORK/manifest-wrong.yaml"
MISSING_CONFIG="$WORK/manifest-missing.yaml"
write_manifest_config "$AUTO_CONFIG" auto "$KEY"
write_manifest_config "$REQUIRED_CONFIG" required "$KEY"
write_manifest_config "$WRONG_CONFIG" required "$(openssl rand -hex 32)"
cat >"$MISSING_CONFIG" <<EOF
manifest: {}
store: { endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 30s }
cache: { endpoint: "" }
chunker: { mode: cdc }
crypto: { chunk: aes, manifest: aes, local: required }
EOF

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

cat >"$WORK/cold.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: encrypted-snapshot }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$WORK/root-active.diff, diff_template: file://$WORK/root-template.ext4 }
  disks:
    - { name: scratch, diff: file://$WORK/scratch-active.diff, diff_template: file://$WORK/scratch-template.ext4 }
    - { name: dataset, base: $DATASET_REF, overlay: { diff: file://$WORK/dataset-active.diff, diff_template: file://$WORK/dataset-template.ext4 } }
mounts:
  - { target: /scratch, type: disk, source: scratch }
  - { target: /data, type: disk, source: dataset }
launch: { exec: /bin/sleep, args: ["3600"], restart: never }
EOF

ready() {
    local sid=$1 pid=$2 log=$3
    for _ in $(seq 1 120); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RUN_ROOT" -- /bin/true >/dev/null 2>&1; then
            return
        fi
        kill -0 "$pid" 2>/dev/null || {
            tail -80 "$log" >&2 || true
            e2e_fail "sandbox $sid exited before becoming executable"
        }
        sleep 1
    done
    e2e_fail "sandbox $sid did not become executable"
}

assert_encrypted_diff() {
    local path=$1 logical=$2 magic physical blocks block_size allocated
    [ -f "$path" ] || e2e_fail "missing encrypted diff: $path"
    magic=$(od -An -tx1 -N8 "$path" | tr -d ' \n')
    [ "$magic" = 894b44585453310a ] || e2e_fail "$path does not have the KDXTS1 header"
    physical=$(stat -c %s "$path")
    [ "$physical" -eq $((logical + 4096)) ] || e2e_fail "$path lost encrypted length geometry"
    read -r blocks block_size < <(stat -c '%b %B' "$path")
    allocated=$((blocks * block_size))
    [ "$allocated" -lt "$physical" ] || e2e_fail "$path lost sparse-body semantics"
}

SID1="encrypted-snapshot-$BASHPID"
LOG1="$OUT/cold.log"
"$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --manifest-config "$REQUIRED_CONFIG" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --sandbox-id "$SID1" >"$LOG1" 2>&1 &
PID1=$!
PIDS+=("$PID1")
ready "$SID1" "$PID1" "$LOG1"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID1" --run-root "$RUN_ROOT" -- /bin/sh -c \
    'set -e; grep -qx DATASET-BASE-OK /data/DATASET-BASE-OK; echo ROOT-SNAPSHOT-OK > /root-snapshot; echo SCRATCH-SNAPSHOT-OK > /scratch/persist; echo DATA-SNAPSHOT-OK > /data/persist; sync'

"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID1" --output "$OUT" --run-root "$RUN_ROOT" >"$OUT/snapshot-local.out" 2>"$OUT/snapshot-local.log"
wait "$PID1" 2>/dev/null || true
PIDS=("$STORE_PID")
SNAP="$OUT/$SID1.snapshot"
[ -e "$SNAP" ] || e2e_fail "local encrypted snapshot missing"
mapfile -t OVERLAYS < <(find "$OUT" -maxdepth 1 -type f -name '*.overlay' -print | sort)
mapfile -t IMAGES < <(find "$OUT" -maxdepth 1 -type f -name '*.image' -print | sort)
"$BIN/sandbox-ctl" info --json --manifest-config "$REQUIRED_CONFIG" "$SNAP" >"$WORK/snapshot.json"
E_BASENAME=$(python3 -c 'import json,os,sys; print(os.path.basename(json.load(open(sys.argv[1]))["SandboxRef"].split("@",1)[0]))' "$WORK/snapshot.json")
SANDBOX_E="$OUT/$E_BASENAME"
[ -e "$SANDBOX_E" ] || e2e_fail "Snapshot S references missing local Sandbox E $E_BASENAME"
[ "${#OVERLAYS[@]}" -eq 2 ] || e2e_fail "expected two encrypted data-disk writable overlays"
[ "${#IMAGES[@]}" -eq 2 ] || e2e_fail "expected two encrypted immutable images"
for artifact in "${IMAGES[@]}" "${OVERLAYS[@]}" "$(readlink -f "$SANDBOX_E")" "$(readlink -f "$SNAP")"; do
    magic=$(od -An -tx1 -N8 "$artifact" | tr -d ' \n')
    [ "$magic" = 894b5453454e430a ] || e2e_fail "local snapshot artifact is not encrypted v1: $artifact"
done
for marker in ROOT-SNAPSHOT-OK SCRATCH-SNAPSHOT-OK DATA-SNAPSHOT-OK; do
    if grep -aFq "$marker" "${IMAGES[@]}" "${OVERLAYS[@]}" "$(readlink -f "$SANDBOX_E")" "$(readlink -f "$SNAP")"; then
        e2e_fail "guest plaintext marker appears in encrypted snapshot artifacts"
    fi
done
"$BIN/sandbox-ctl" info --json --manifest-config "$REQUIRED_CONFIG" "$SANDBOX_E" >"$WORK/sandbox.json"
python3 - "$WORK/sandbox.json" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
refs = []
root = cfg["Boot"]["Root"]
refs += [root.get("Base", "")]
refs += root.get("BaseFromRefs") or []
if root.get("Overlay"):
    refs += [root["Overlay"].get("Base", "")]
    refs += root["Overlay"].get("BaseFromRefs") or []
for disk in cfg["Boot"].get("Disks") or []:
    refs += [disk.get("Base", "")]
    refs += disk.get("BaseFromRefs") or []
    if disk.get("Overlay"):
        refs += [disk["Overlay"].get("Base", "")]
        refs += disk["Overlay"].get("BaseFromRefs") or []
refs = [ref for ref in refs if ref and ref != "self"]
if not refs or any("@hmac:" not in ref for ref in refs):
    raise SystemExit("disk artifact refs are not uniformly @hmac: %r" % refs)
PY
if "$BIN/sandbox-ctl" info --json --manifest-config "$WRONG_CONFIG" "$SNAP" >"$OUT/wrong-key.out" 2>"$OUT/wrong-key.err"; then
    e2e_fail "encrypted local snapshot unexpectedly opened with a wrong customer key"
fi
if "$BIN/sandbox-ctl" info --json --manifest-config "$MISSING_CONFIG" "$SNAP" >"$OUT/missing-key.out" 2>"$OUT/missing-key.err"; then
    e2e_fail "encrypted local snapshot unexpectedly opened without a customer key"
fi

write_restore_config() {
    local path=$1 root_diff=$2 scratch_diff=$3 dataset_diff=$4 hostname=$5
    cat >"$path" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: $hostname }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root: { overlay: { diff: file://$root_diff } }
  disks:
    - { name: scratch, diff: file://$scratch_diff }
    - { name: dataset, overlay: { diff: file://$dataset_diff } }
EOF
}

ROOT_LOCAL="$WORK/root-local.diff"
SCRATCH_LOCAL="$WORK/scratch-local.diff"
DATASET_LOCAL="$WORK/dataset-local.diff"
write_restore_config "$WORK/restore-local.yaml" "$ROOT_LOCAL" "$SCRATCH_LOCAL" "$DATASET_LOCAL" encrypted-local
SID2="encrypted-local-$BASHPID"
LOG2="$OUT/restore-local.log"
"$BIN/sandbox-ctl" run --restore "$SNAP" --config "$WORK/restore-local.yaml" --manifest-config "$REQUIRED_CONFIG" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --sandbox-id "$SID2" >"$LOG2" 2>&1 &
PID2=$!
PIDS+=("$PID2")
ready "$SID2" "$PID2" "$LOG2"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$RUN_ROOT" -- /bin/sh -c \
    'set -e; cat /root-snapshot /scratch/persist /data/persist /data/DATASET-BASE-OK; echo ROOT-RESTORED-OK > /root-restored; echo SCRATCH-RESTORED-OK > /scratch/restored; echo DATA-RESTORED-OK > /data/restored; sync' >"$WORK/local-check.out"
for marker in ROOT-SNAPSHOT-OK SCRATCH-SNAPSHOT-OK DATA-SNAPSHOT-OK DATASET-BASE-OK; do
    grep -qx "$marker" "$WORK/local-check.out" || e2e_fail "local encrypted restore lost $marker"
done
assert_encrypted_diff "$ROOT_LOCAL" $((512 * 1024 * 1024))
assert_encrypted_diff "$SCRATCH_LOCAL" $((256 * 1024 * 1024))
assert_encrypted_diff "$DATASET_LOCAL" $((256 * 1024 * 1024))
for marker in ROOT-RESTORED-OK SCRATCH-RESTORED-OK DATA-RESTORED-OK; do
    if grep -aFq "$marker" "$ROOT_LOCAL" "$SCRATCH_LOCAL" "$DATASET_LOCAL"; then
        e2e_fail "restored guest plaintext marker appears in encrypted diff"
    fi
done

SNAP_KEY=$("$BIN/sandbox-ctl" snapshot --sandbox-id "$SID2" --upload --run-root "$RUN_ROOT" 2>"$OUT/snapshot-upload.log")
wait "$PID2" 2>/dev/null || true
PIDS=("$STORE_PID")
SNAP_KEY="${SNAP_KEY#manifest://}"
[ "${#SNAP_KEY}" -eq 64 ] || e2e_fail "snapshot upload did not return a manifest key"

ROOT_REMOTE="$WORK/root-remote.diff"
SCRATCH_REMOTE="$WORK/scratch-remote.diff"
DATASET_REMOTE="$WORK/dataset-remote.diff"
write_restore_config "$WORK/restore-remote.yaml" "$ROOT_REMOTE" "$SCRATCH_REMOTE" "$DATASET_REMOTE" encrypted-remote
SID3="encrypted-remote-$BASHPID"
LOG3="$OUT/restore-remote.log"
"$BIN/sandbox-ctl" run --restore "manifest://$SNAP_KEY" --config "$WORK/restore-remote.yaml" --manifest-config "$REQUIRED_CONFIG" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --sandbox-id "$SID3" >"$LOG3" 2>&1 &
PID3=$!
PIDS+=("$PID3")
ready "$SID3" "$PID3" "$LOG3"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID3" --run-root "$RUN_ROOT" -- /bin/sh -c \
    'set -e; cat /root-snapshot /scratch/persist /data/persist /root-restored /scratch/restored /data/restored /data/DATASET-BASE-OK' >"$WORK/remote-check.out"
for marker in ROOT-SNAPSHOT-OK SCRATCH-SNAPSHOT-OK DATA-SNAPSHOT-OK ROOT-RESTORED-OK SCRATCH-RESTORED-OK DATA-RESTORED-OK DATASET-BASE-OK; do
    grep -qx "$marker" "$WORK/remote-check.out" || e2e_fail "remote encrypted restore lost $marker"
done
assert_encrypted_diff "$ROOT_REMOTE" $((512 * 1024 * 1024))
assert_encrypted_diff "$SCRATCH_REMOTE" $((256 * 1024 * 1024))
assert_encrypted_diff "$DATASET_REMOTE" $((256 * 1024 * 1024))

kill -TERM "$PID3"
wait "$PID3" || true
PIDS=("$STORE_PID")

echo "PASS snapshot.encrypted-diff.sh"
