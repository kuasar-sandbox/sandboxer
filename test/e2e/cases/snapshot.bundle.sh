#!/usr/bin/env bash
# Portable Bundle A -> B -> C snapshot chain, exact upload, and remote restore.
set -euo pipefail
shopt -s nullglob

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
source "$E2E_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in cmp docker find grep ip mkfs.ext4 openssl python3 readlink timeout truncate; do
    require_command "$command"
done
for binary in cloud-hypervisor sandbox-ctl flatten-ctl manifest-ctl store-ctl; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/run"
RR="$WORK/run"
TAP_NAME="xb$((BASHPID % 1000000))"
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

KEY="$(openssl rand -hex 32)"
cat >"$WORK/manifest.yaml" <<EOF
manifest:
  key: "$KEY"
  verify_content: true
  write_generation: G1
store: { endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 30s }
chunker: { mode: cdc }
crypto: { chunk: aes, manifest: aes }
EOF

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
BLK0_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"
truncate -s 512M "$WORK/root-upper.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/root-upper.ext4"
truncate -s 256M "$WORK/scratch-template.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/scratch-template.ext4"

cat >"$WORK/cold.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: bundle-cold }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay: { diff: file://$WORK/root-upper.ext4 }
  disks:
    - { name: scratch, diff_template: file://$WORK/scratch-template.ext4 }
mounts:
  - { target: /scratch, type: disk, source: scratch }
launch: { exec: /bin/sleep, args: ["3600"], restart: never }
EOF

ready() {
    local sid=$1 pid=$2 log=$3
    for _ in $(seq 1 120); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" -- /bin/true >/dev/null 2>&1; then
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

validate_bundle() {
    local directory=$1 sid=$2 minimum_manifests=$3 expected_refs=$4 expected_files=$5 info_json=$6
    local bundles=("$directory"/*.bundle)
    [ "${#bundles[@]}" -eq "$expected_files" ] || e2e_fail "$directory has ${#bundles[@]} bundle files, want $expected_files"
    [ -L "$directory/$sid.snapshot" ] || e2e_fail "missing $sid.snapshot symlink"
    local root_bundle
    root_bundle="$(readlink -f "$directory/$sid.snapshot")"
    [ -f "$root_bundle" ] || e2e_fail "snapshot symlink does not select a Bundle"
    if find "$directory" -maxdepth 1 -type f \( -name '*.image' -o -name '*.overlay' -o -name '*.snapshot' -o -name '*.partial' \) | grep -q .; then
        e2e_fail "Bundle mode emitted a tarstream/partial sibling"
    fi
    "$BIN/sandbox-ctl" info --json --manifest-config "$WORK/manifest.yaml" "$directory/$sid.snapshot" >"$info_json"
    python3 - "$root_bundle" "$minimum_manifests" "$expected_refs" <<'PY'
import os, re, sys, zipfile
path, minimum_text, expected_refs_text = sys.argv[1:]
root = os.path.basename(path).removesuffix('.bundle')
if not re.fullmatch(r'[0-9a-f]{64}', root):
    raise SystemExit(f'invalid root Bundle filename: {path}')
with zipfile.ZipFile(path) as archive:
    names = [item.filename for item in archive.infolist()]
    if len(names) != len(set(names)):
        raise SystemExit('duplicate ZIP entry')
    refs = [name for name in names if name == 'bundle/refs']
    admissions = [name for name in names if name.startswith('bundle/admission/')]
    indexes = [name for name in names if name == 'bundle/index']
    if len(admissions) != 1:
        raise SystemExit('Bundle does not contain exactly one admission')
    if indexes != ['bundle/index'] or names[-1] != 'bundle/index':
        raise SystemExit(f'Bundle index must be the sole final entry: {indexes!r}')
    expected_refs = int(expected_refs_text)
    if len(refs) != (1 if expected_refs else 0):
        raise SystemExit(f'bundle/refs presence mismatch: {refs!r}')
    expected_prefix = ['bundle/refs', admissions[0]] if expected_refs else [admissions[0]]
    if names[:len(expected_prefix)] != expected_prefix:
        raise SystemExit(f'metadata order={names[:len(expected_prefix)]!r}, want {expected_prefix!r}')
    if expected_refs:
        lines = archive.read('bundle/refs').decode('utf-8').splitlines()
        if len(lines) != expected_refs or not archive.read('bundle/refs').endswith(b'\n'):
            raise SystemExit(f'bundle/refs={lines!r}, want {expected_refs} LF-terminated lines')
    manifests = [name for name in names if name.startswith('manifest/')]
    if len(manifests) < int(minimum_text):
        raise SystemExit(f'Manifest count {len(manifests)} < {minimum_text}')
    if f'manifest/{root}' not in names:
        raise SystemExit('filename-selected root Manifest is absent')
    allowed = re.compile(r'(?:bundle/admission/[A-Za-z0-9][A-Za-z0-9._-]{0,127}/[0-9a-f]{64}|bundle/(?:refs|index)|(?:manifest|chunk)/[0-9a-f]{64})$')
    for item in archive.infolist():
        if item.compress_type != zipfile.ZIP_STORED:
            raise SystemExit(f'non-Store ZIP entry: {item.filename}')
        if item.extract_version != 45:
            raise SystemExit(f'non-ZIP64 entry version: {item.filename}={item.extract_version}')
        if not allowed.fullmatch(item.filename):
            raise SystemExit(f'unknown Bundle entry: {item.filename}')
PY
}

assert_bundle_refs() {
    local bundle=$1
    shift
    python3 - "$bundle" "$@" <<'PY'
import sys, zipfile
path, *expected = sys.argv[1:]
with zipfile.ZipFile(path) as archive:
    got = archive.read('bundle/refs').decode('utf-8').splitlines() if expected else []
if got != expected:
    raise SystemExit(f'bundle/refs={got!r}, want {expected!r}')
PY
}

assert_located_bundle() {
    local ref=$1 location_name=$2 location_dir=$3 root=$4 source=$5
    local expected="file://$root.bundle@manifest:$root@location:$location_name"
    [ "$ref" = "$expected" ] || e2e_fail "located Bundle ref=$ref, want $expected"
    [ -f "$location_dir/$root.bundle" ] || e2e_fail "located Bundle final is absent"
    cmp "$source" "$location_dir/$root.bundle" || e2e_fail "located Bundle is not an exact copy"
    [ "$(find "$location_dir" -mindepth 1 -maxdepth 1 | wc -l)" -eq 1 ] || e2e_fail "named location contains extra artifacts"
    [ -z "$(find "$location_dir" -mindepth 1 -maxdepth 1 -type l -print -quit)" ] || e2e_fail "named location contains a symlink"
}

assert_manifest_snapshot_refs() {
    local bundle=$1 snapshot_json=$2 want_parents=$3 want_locations=$4
    local sandbox_ref sandbox_key sandbox_json
    sandbox_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["SandboxRef"])' "$snapshot_json")
    sandbox_key=${sandbox_ref#manifest://}
    sandbox_json="$snapshot_json.sandbox"
    "$BIN/sandbox-ctl" info --json --manifest-config "$WORK/manifest.yaml" "file://$bundle@manifest:$sandbox_key" >"$sandbox_json"
    python3 - "$snapshot_json" "$sandbox_json" "$want_parents" "$want_locations" <<'PY'
import json, re, sys
snapshot = json.load(open(sys.argv[1], encoding='utf-8'))
sandbox = json.load(open(sys.argv[2], encoding='utf-8'))
parents = snapshot.get('FromRefs') or []
if len(parents) != int(sys.argv[3]):
    raise SystemExit(f'from_refs={parents!r}, want {sys.argv[3]} entries')
allowed_locations = set(filter(None, sys.argv[4].split(',')))
seen_locations = set()
manifest_ref = re.compile(r'manifest://[0-9a-f]{64}$')
located_ref = re.compile(r'file://[0-9a-f]{64}\.bundle@manifest:[0-9a-f]{64}@location:([A-Za-z0-9][A-Za-z0-9._-]{0,127})$')
def validate(ref):
    if manifest_ref.fullmatch(ref):
        return
    match = located_ref.fullmatch(ref)
    if not match or match.group(1) not in allowed_locations:
        raise SystemExit(f'non-canonical or unexpected portable ref: {ref!r}')
    seen_locations.add(match.group(1))
for ref in parents:
    validate(ref)
refs = []
boot = sandbox['Boot']
for node in [boot['Root'], *(boot.get('Disks') or [])]:
    refs.append(node.get('Base', ''))
    refs.extend(node.get('BaseFromRefs') or [])
    overlay = node.get('Overlay')
    if overlay:
        refs.append(overlay.get('Base', ''))
        refs.extend(overlay.get('BaseFromRefs') or [])
for ref in refs:
    if ref and ref != 'self':
        validate(ref)
if seen_locations != allowed_locations:
    raise SystemExit(f'located dependency set={sorted(seen_locations)!r}, want {sorted(allowed_locations)!r}')
PY
}

write_restore_yaml() {
    local output=$1 hostname=$2 root_diff=$3
    rm -f "$root_diff"
    truncate -s 512M "$root_diff"
    mkfs.ext4 -q -F -O ^has_journal "$root_diff"
    cat >"$output" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: $hostname }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root: { overlay: { diff: file://$root_diff } }
  disks:
    - { name: scratch }
EOF
}

SID1="bundle-a-$BASHPID"
OUT1="$OUT/a"
mkdir -p "$OUT1"
timeout -k 10s 180 "$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --manifest-config "$WORK/manifest.yaml" \
    --sandbox-id "$SID1" --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" >"$OUT/run-a.log" 2>&1 &
P1=$!
PIDS+=("$P1")
ready "$SID1" "$P1" "$OUT/run-a.log"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID1" --run-root "$RR" -- /bin/sh -c \
    'echo ROOT-BASE > /bundle-root; echo DATA-BASE > /scratch/bundle-data; sync'
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID1" --output "$OUT1" --mode bundle --run-root "$RR" >"$OUT/snapshot-a.out" 2>"$OUT/snapshot-a.log"
wait "$P1" 2>/dev/null || true
PIDS=("$STORE_PID")
validate_bundle "$OUT1" "$SID1" 3 0 1 "$WORK/info-a.json"
assert_manifest_snapshot_refs "$(readlink -f "$OUT1/$SID1.snapshot")" "$WORK/info-a.json" 0 ""
ROOT1_PATH="$(readlink -f "$OUT1/$SID1.snapshot")"
ROOT1="$(basename "$ROOT1_PATH" .bundle)"
LOCATION_A="$WORK/location-a"
LOCATED_A="$("$BIN/sandbox-ctl" upload-snapshot --manifest-config "$WORK/manifest.yaml" --to-ref-location A=file://$LOCATION_A --quiet "$OUT1/$SID1.snapshot")"
assert_located_bundle "$LOCATED_A" A "$LOCATION_A" "$ROOT1" "$ROOT1_PATH"

write_restore_yaml "$WORK/restore-b.yaml" bundle-child "$WORK/root-b.ext4"
SID2="bundle-b-$BASHPID"
OUT2="$OUT/b"
mkdir -p "$OUT2"
timeout -k 10s 180 "$BIN/sandbox-ctl" run --restore "$LOCATED_A" --ref-location A=file://$LOCATION_A \
    --config "$WORK/restore-b.yaml" --manifest-config "$WORK/manifest.yaml" --sandbox-id "$SID2" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" >"$OUT/run-b.log" 2>&1 &
P2=$!
PIDS+=("$P2")
ready "$SID2" "$P2" "$OUT/run-b.log"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$RR" -- /bin/sh -c \
    'grep -qx ROOT-BASE /bundle-root; grep -qx DATA-BASE /scratch/bundle-data; echo ROOT-CHILD > /bundle-root-child; echo DATA-CHILD > /scratch/bundle-data-child; sync'
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID2" --output "$OUT2" --mode bundle --merge-ref=false --drop-caches=false --run-root "$RR" >"$OUT/snapshot-b.out" 2>"$OUT/snapshot-b.log"
wait "$P2" 2>/dev/null || true
PIDS=("$STORE_PID")
validate_bundle "$OUT2" "$SID2" 3 0 1 "$WORK/info-b.json"
assert_manifest_snapshot_refs "$(readlink -f "$OUT2/$SID2.snapshot")" "$WORK/info-b.json" 1 "A"
ROOT2_PATH="$(readlink -f "$OUT2/$SID2.snapshot")"
ROOT2="$(basename "$ROOT2_PATH" .bundle)"
assert_bundle_refs "$ROOT2_PATH"
LOCATION_B="$WORK/location-b"
LOCATED_B="$("$BIN/sandbox-ctl" upload-snapshot --manifest-config "$WORK/manifest.yaml" --to-ref-location B=file://$LOCATION_B --quiet "$OUT2/$SID2.snapshot")"
assert_located_bundle "$LOCATED_B" B "$LOCATION_B" "$ROOT2" "$ROOT2_PATH"

rm -rf "$OUT1" "$OUT2"
write_restore_yaml "$WORK/restore-c.yaml" bundle-grandchild "$WORK/root-c.ext4"
SID3="bundle-c-$BASHPID"
OUT3="$OUT/c"
mkdir -p "$OUT3"
timeout -k 10s 180 "$BIN/sandbox-ctl" run --restore "$LOCATED_B" --ref-location A=file://$LOCATION_A --ref-location B=file://$LOCATION_B \
    --config "$WORK/restore-c.yaml" --manifest-config "$WORK/manifest.yaml" --sandbox-id "$SID3" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" >"$OUT/run-c.log" 2>&1 &
P3=$!
PIDS+=("$P3")
ready "$SID3" "$P3" "$OUT/run-c.log"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID3" --run-root "$RR" -- /bin/sh -c \
    'grep -qx ROOT-BASE /bundle-root; grep -qx ROOT-CHILD /bundle-root-child; grep -qx DATA-BASE /scratch/bundle-data; grep -qx DATA-CHILD /scratch/bundle-data-child; echo ROOT-C > /bundle-root-c; echo DATA-C > /scratch/bundle-data-c; sync'
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID3" --output "$OUT3" --mode bundle --merge-ref=false --drop-caches=false --run-root "$RR" >"$OUT/snapshot-c.out" 2>"$OUT/snapshot-c.log"
wait "$P3" 2>/dev/null || true
PIDS=("$STORE_PID")
validate_bundle "$OUT3" "$SID3" 3 0 1 "$WORK/info-c.json"
assert_manifest_snapshot_refs "$(readlink -f "$OUT3/$SID3.snapshot")" "$WORK/info-c.json" 2 "A,B"
ROOT3_PATH="$(readlink -f "$OUT3/$SID3.snapshot")"
ROOT3="$(basename "$ROOT3_PATH" .bundle)"
assert_bundle_refs "$ROOT3_PATH"

write_restore_yaml "$WORK/restore-local-c.yaml" bundle-local-c "$WORK/root-local-c.ext4"
SID4="bundle-local-c-$BASHPID"
timeout -k 10s 180 "$BIN/sandbox-ctl" run --restore "$OUT3/$SID3.snapshot" \
    --ref-location A=file://$LOCATION_A --ref-location B=file://$LOCATION_B --config "$WORK/restore-local-c.yaml" \
    --manifest-config "$WORK/manifest.yaml" --sandbox-id "$SID4" --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" >"$OUT/run-local-c.log" 2>&1 &
P4=$!
PIDS+=("$P4")
ready "$SID4" "$P4" "$OUT/run-local-c.log"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID4" --run-root "$RR" -- /bin/sh -c \
    'cat /bundle-root /bundle-root-child /bundle-root-c /scratch/bundle-data /scratch/bundle-data-child /scratch/bundle-data-c' >"$WORK/local-c-data"
for marker in ROOT-BASE ROOT-CHILD ROOT-C DATA-BASE DATA-CHILD DATA-C; do
    grep -qx "$marker" "$WORK/local-c-data" || e2e_fail "local A->B->C restore lost $marker"
done
kill -TERM "$P4"
wait "$P4" || true
PIDS=("$STORE_PID")

UPLOADED="$("$BIN/sandbox-ctl" upload-snapshot --manifest-config "$WORK/manifest.yaml" \
    --ref-location A=file://$LOCATION_A --ref-location B=file://$LOCATION_B --quiet "$OUT3/$SID3.snapshot")"
[ "$UPLOADED" = "manifest://$ROOT3" ] || e2e_fail "exact upload changed Bundle root: $UPLOADED"
"$BIN/manifest-ctl" verify --manifest-config "$WORK/manifest.yaml" "$ROOT3" >/dev/null

rm -rf "$OUT3"
write_restore_yaml "$WORK/restore-remote.yaml" bundle-remote "$WORK/root-remote.ext4"
SID5="bundle-remote-$BASHPID"
timeout -k 10s 180 "$BIN/sandbox-ctl" run --restore "manifest://$ROOT3" --config "$WORK/restore-remote.yaml" \
    --ref-location A=file://$LOCATION_A --ref-location B=file://$LOCATION_B --manifest-config "$WORK/manifest.yaml" \
    --sandbox-id "$SID5" --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" >"$OUT/run-remote.log" 2>&1 &
P5=$!
PIDS+=("$P5")
ready "$SID5" "$P5" "$OUT/run-remote.log"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID5" --run-root "$RR" -- /bin/sh -c \
    'cat /bundle-root /bundle-root-child /bundle-root-c /scratch/bundle-data /scratch/bundle-data-child /scratch/bundle-data-c' >"$WORK/remote-data"
for marker in ROOT-BASE ROOT-CHILD ROOT-C DATA-BASE DATA-CHILD DATA-C; do
    grep -qx "$marker" "$WORK/remote-data" || e2e_fail "remote A->B->C restore lost $marker"
done
kill -TERM "$P5"
wait "$P5" || true
PIDS=("$STORE_PID")

echo "PASS snapshot.bundle.sh"
