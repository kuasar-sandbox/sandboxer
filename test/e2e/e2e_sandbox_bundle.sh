#!/usr/bin/env bash
#
# e2e_sandbox_bundle.sh — self-contained E/S Manifest Bundle lifecycle:
#   A cold Bundle -> published location A -> B -> published location B -> C,
#   exact Store upload, and remote restore. Each Bundle contains S, referenced
#   E, and all explicit disk/memory dependencies under one admission.

set -euo pipefail
shopt -s nullglob

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
BIN="${BIN:-$REPO_ROOT/bin}"
IMAGE="${IMAGE:-python:3.12-slim}"
TAP_NAME="${TAP_NAME:-sb-tapb0}"

skip() {
    echo
    echo "==> e2e_sandbox_bundle: skipping ($*)"
    [ "${REQUIRE_KVM:-0}" = "1" ] && exit 1
    exit 0
}

[ -e /dev/kvm ] || skip "/dev/kvm not present"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || skip "/dev/kvm not accessible"
for binary in cloud-hypervisor sandbox-ctl sandbox-init sandbox-runtime.bundle flatten-ctl manifest-ctl store-ctl; do
    [ -e "$BIN/$binary" ] || skip "missing $BIN/$binary"
done
VMLINUX="${VMLINUX:-$BIN/vmlinux}"
[ -f "$VMLINUX" ] || skip "no vmlinux at $VMLINUX"
command -v mkfs.ext4 >/dev/null 2>&1 || skip "mkfs.ext4 not on PATH"
command -v python3 >/dev/null 2>&1 || skip "python3 not on PATH"
command -v openssl >/dev/null 2>&1 || skip "openssl not on PATH"
if [ "$(id -u)" -ne 0 ]; then exec sudo -nE "$0" "$@"; fi

WORK="$(mktemp -d /tmp/e2e-manifest-bundle-XXXXXX)"
RR="$WORK/runtime"
mkdir -p "$RR"
PIDS=()
TAP_CREATED=0
cleanup() {
    set +e
    for pid in "${PIDS[@]}"; do
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    pkill -f "cloud-hypervisor.*bundle-" 2>/dev/null || true
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null || true
    [ -n "${E2E_KEEP:-}" ] && echo "kept: $WORK" || rm -rf "$WORK"
}
trap cleanup EXIT

if ! ip link show "$TAP_NAME" >/dev/null 2>&1; then
    ip tuntap add dev "$TAP_NAME" mode tap
    ip addr add 169.254.1.0/31 dev "$TAP_NAME"
    ip link set "$TAP_NAME" up
    TAP_CREATED=1
fi

free_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("", 0)); print(s.getsockname()[1]); s.close()'
}

STORE_PORT="$(free_port)"
cat > "$WORK/store.yaml" <<EOF
listen: 127.0.0.1:$STORE_PORT
backend: fs
fs: { root: $WORK/store-data, verify_content_key: true }
EOF
"$BIN/store-ctl" init --config "$WORK/store.yaml" --generation G1
"$BIN/store-ctl" serve --config "$WORK/store.yaml" >"$WORK/store.log" 2>&1 &
PIDS+=("$!")
for _ in $(seq 1 100); do
    (echo >/dev/tcp/127.0.0.1/$STORE_PORT) >/dev/null 2>&1 && break
    sleep 0.05
done

KEY="$(openssl rand -hex 32)"
cat > "$WORK/manifest.yaml" <<EOF
manifest:
  key: "$KEY"
  verify_content: true
  write_generation: G1
store: { endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 30s }
chunker: { mode: cdc }
crypto: { chunk: aes, manifest: aes }
EOF

BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker unavailable; set BLK0_IMAGE"
    docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null
    BLK0_IMAGE="$WORK/root.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"
truncate -s 512M "$WORK/root-upper.ext4"
mkfs.ext4 -q -F "$WORK/root-upper.ext4"
truncate -s 256M "$WORK/scratch-template.ext4"
mkfs.ext4 -q -F "$WORK/scratch-template.ext4"

cat > "$WORK/cold.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: bundle-cold }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay: { diff: file://$WORK/root-upper.ext4, size: 512MiB }
  disks:
    - { name: scratch, diff_template: file://$WORK/scratch-template.ext4, diff_size: 256MiB }
mounts:
  - { target: /scratch, type: disk, source: scratch }
launch: { exec: /bin/sleep, args: ["3600"] }
EOF

ready() {
    local sid="$1" pid="$2" output="$3"
    for _ in $(seq 1 120); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" -- /bin/sh -c 'echo READY' >"$output" 2>/dev/null \
            && grep -q READY "$output"; then
            return 0
        fi
        kill -0 "$pid" 2>/dev/null || return 1
        sleep 1
    done
    return 1
}

validate_bundle() {
    local directory="$1" sid="$2" minimum_manifests="$3" expected_refs="$4" expected_files="$5" info_json="$6"
    local bundles=("$directory"/*.bundle)
    [ "${#bundles[@]}" = "$expected_files" ] || { echo "FAIL: $directory has ${#bundles[@]} Bundle files, want $expected_files"; ls -la "$directory"; exit 1; }
    [ -L "$directory/$sid.snapshot" ] || { echo "FAIL: missing $sid.snapshot symlink"; exit 1; }
    local root_bundle
    root_bundle="$(readlink -f "$directory/$sid.snapshot")"
    [ -f "$root_bundle" ] || { echo "FAIL: sid symlink does not select a Bundle"; exit 1; }
    if find "$directory" -maxdepth 1 -type f \( -name '*.overlay' -o -name '*.snapshot' -o -name '*.partial' \) | grep -q .; then
        echo "FAIL: Bundle mode emitted a tarstream/partial sibling"
        find "$directory" -maxdepth 1 -printf '%f\n'
        exit 1
    fi
    "$BIN/sandbox-ctl" info --json --manifest-config "$WORK/manifest.yaml" "$directory/$sid.snapshot" >"$info_json"
    python3 - "$root_bundle" "$minimum_manifests" "$expected_refs" <<'PY'
import os
import re
import sys
import zipfile

path, minimum_text, expected_refs_text = sys.argv[1:]
root = os.path.basename(path).removesuffix(".bundle")
if not re.fullmatch(r"[0-9a-f]{64}", root):
    raise SystemExit(f"invalid root Bundle filename: {path}")
with zipfile.ZipFile(path) as archive:
    names = [item.filename for item in archive.infolist()]
    if len(names) != len(set(names)):
        raise SystemExit("duplicate ZIP entry")
    refs = [name for name in names if name == "bundle/refs"]
    admissions = [name for name in names if name.startswith("bundle/admission/")]
    indexes = [name for name in names if name == "bundle/index"]
    if len(admissions) != 1:
        raise SystemExit("Bundle does not contain exactly one admission")
    if indexes != ["bundle/index"] or names[-1] != "bundle/index":
        raise SystemExit(f"Bundle index must be the sole final entry: {indexes!r}")
    expected_refs = int(expected_refs_text)
    if len(refs) != (1 if expected_refs else 0):
        raise SystemExit(f"bundle/refs presence mismatch: {refs!r}")
    expected_prefix = ["bundle/refs", admissions[0]] if expected_refs else [admissions[0]]
    if names[:len(expected_prefix)] != expected_prefix:
        raise SystemExit(f"metadata order={names[:len(expected_prefix)]!r}, want {expected_prefix!r}")
    if expected_refs:
        lines = archive.read("bundle/refs").decode("utf-8").splitlines()
        if len(lines) != expected_refs or not archive.read("bundle/refs").endswith(b"\n"):
            raise SystemExit(f"bundle/refs={lines!r}, want {expected_refs} LF-terminated lines")
    manifests = [name for name in names if name.startswith("manifest/")]
    if len(manifests) < int(minimum_text):
        raise SystemExit(f"Manifest count {len(manifests)} < {minimum_text}")
    if f"manifest/{root}" not in names:
        raise SystemExit("filename-selected root Manifest is absent")
    allowed = re.compile(r"(?:bundle/admission/[A-Za-z0-9][A-Za-z0-9._-]{0,127}/[0-9a-f]{64}|bundle/(?:refs|index)|(?:manifest|chunk)/[0-9a-f]{64})$")
    for item in archive.infolist():
        if item.compress_type != zipfile.ZIP_STORED:
            raise SystemExit(f"non-Store ZIP entry: {item.filename}")
        if item.extract_version != 45:
            raise SystemExit(f"non-ZIP64 entry version: {item.filename}={item.extract_version}")
        if not allowed.fullmatch(item.filename):
            raise SystemExit(f"unknown Bundle entry: {item.filename}")
print(f"    Bundle root={root} refs={expected_refs} manifests={len(manifests)} chunks={sum(n.startswith('chunk/') for n in names)}")
PY
}

assert_bundle_refs() {
    local bundle="$1"
    shift
    python3 - "$bundle" "$@" <<'PY'
import sys
import zipfile

path, *expected = sys.argv[1:]
with zipfile.ZipFile(path) as archive:
    got = archive.read("bundle/refs").decode("utf-8").splitlines() if expected else []
if got != expected:
    raise SystemExit(f"bundle/refs={got!r}, want {expected!r}")
PY
}

assert_located_bundle() {
    local ref="$1" location_name="$2" location_dir="$3" root="$4" source="$5"
    local expected="file://$root.bundle@manifest:$root@location:$location_name"
    [ "$ref" = "$expected" ] || { echo "FAIL: located Bundle ref=$ref, want $expected"; exit 1; }
    [ -f "$location_dir/$root.bundle" ] || { echo "FAIL: located Bundle final is absent"; exit 1; }
    cmp "$source" "$location_dir/$root.bundle" \
        || { echo "FAIL: located Bundle is not an exact copy"; exit 1; }
    [ "$(find "$location_dir" -mindepth 1 -maxdepth 1 | wc -l)" -eq 1 ] \
        || { echo "FAIL: named location contains aliases or extra artifacts"; find "$location_dir" -mindepth 1 -maxdepth 1 -ls; exit 1; }
    [ -z "$(find "$location_dir" -mindepth 1 -maxdepth 1 -type l -print -quit)" ] \
        || { echo "FAIL: named location contains a symlink"; exit 1; }
}

assert_manifest_snapshot_refs() {
    local bundle="$1" snapshot_json="$2" want_parents="$3"
    local sandbox_ref sandbox_key sandbox_json
    sandbox_ref=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["SandboxRef"])' "$snapshot_json")
    sandbox_key=${sandbox_ref#manifest://}
    sandbox_json="$snapshot_json.sandbox"
    "$BIN/sandbox-ctl" info --json --manifest-config "$WORK/manifest.yaml" \
        "file://$bundle@manifest:$sandbox_key" >"$sandbox_json"
    python3 - "$snapshot_json" "$sandbox_json" "$want_parents" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    snapshot = json.load(source)
with open(sys.argv[2], encoding="utf-8") as source:
    sandbox = json.load(source)
parents = snapshot.get("FromRefs") or []
if len(parents) != int(sys.argv[3]):
    raise SystemExit(f"from_refs={parents!r}, want {sys.argv[3]} entries")
if any(not ref.startswith("manifest://") for ref in parents):
    raise SystemExit(f"Snapshot S has non-Manifest memory refs: {parents!r}")

refs = []
boot = sandbox["Boot"]
for node in [boot["Root"], *(boot.get("Disks") or [])]:
    refs.append(node.get("Base", ""))
    refs.extend(node.get("BaseFromRefs") or [])
    overlay = node.get("Overlay")
    if overlay:
        refs.append(overlay.get("Base", ""))
        refs.extend(overlay.get("BaseFromRefs") or [])
bad = [ref for ref in refs if ref and ref != "self" and not ref.startswith("manifest://")]
if bad:
    raise SystemExit(f"Sandbox E retained non-Manifest disk refs: {bad!r}")
if any(len(ref) != len("manifest://") + 64 for ref in refs if ref and ref != "self"):
    raise SystemExit(f"invalid Manifest refs: {refs!r}")
PY
}

echo "==> phase 1: cold root + data disk -> one multi-Manifest Bundle"
SID1="bundle-1-$$"
OUT1="$WORK/out1"
mkdir -p "$OUT1"
timeout -k 10s 180 "$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --manifest-config "$WORK/manifest.yaml" \
    --sandbox-id "$SID1" --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" >"$WORK/run1.log" 2>&1 &
P1=$!
PIDS+=("$P1")
ready "$SID1" "$P1" "$WORK/ready1" || { tail -80 "$WORK/run1.log"; exit 1; }
"$BIN/sandbox-ctl" exec --sandbox-id "$SID1" --run-root "$RR" -- /bin/sh -c \
    'echo ROOT-BASE > /bundle-root; echo DATA-BASE > /scratch/bundle-data; sync'
start_ns="$(date +%s%N)"
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID1" --output "$OUT1" --mode bundle --run-root "$RR" >"$WORK/snapshot1.out" 2>"$WORK/snapshot1.log"
echo "    create_ms=$((($(date +%s%N)-start_ns)/1000000))"
wait "$P1" 2>/dev/null || true
validate_bundle "$OUT1" "$SID1" 3 0 1 "$WORK/info1.json"
assert_manifest_snapshot_refs "$(readlink -f "$OUT1/$SID1.snapshot")" "$WORK/info1.json" 0

ROOT1_PATH="$(readlink -f "$OUT1/$SID1.snapshot")"
ROOT1="$(basename "$ROOT1_PATH" .bundle)"
LOCATION_A="$WORK/location-a"
LOCATED_A="$("$BIN/sandbox-ctl" upload-snapshot --manifest-config "$WORK/manifest.yaml" \
    --to-ref-location A=file://$LOCATION_A --quiet "$OUT1/$SID1.snapshot")"
assert_located_bundle "$LOCATED_A" A "$LOCATION_A" "$ROOT1" "$ROOT1_PATH"

write_restore_yaml() {
    local output="$1" hostname="$2" root_diff="$3"
    rm -f "$root_diff"
    truncate -s 512M "$root_diff"
    mkfs.ext4 -q -F "$root_diff"
    cat > "$output" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: $hostname }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: { diff: file://$root_diff, size: 512MiB }
  disks:
    - { name: scratch }
EOF
}

echo "==> phase 2: located A restore -> self-contained Bundle B"
write_restore_yaml "$WORK/restore2.yaml" bundle-child "$WORK/root-r2.ext4"
SID2="bundle-2-$$"
OUT2="$WORK/out2"
mkdir -p "$OUT2"
start_ns="$(date +%s%N)"
timeout -k 10s 180 "$BIN/sandbox-ctl" run --restore "$LOCATED_A" --ref-location A=file://$LOCATION_A \
    --config "$WORK/restore2.yaml" \
    --manifest-config "$WORK/manifest.yaml" --sandbox-id "$SID2" --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$RR" >"$WORK/run2.log" 2>&1 &
P2=$!
PIDS+=("$P2")
ready "$SID2" "$P2" "$WORK/ready2" || { tail -80 "$WORK/run2.log"; exit 1; }
echo "    local_restore_ready_ms=$((($(date +%s%N)-start_ns)/1000000))"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID2" --run-root "$RR" -- /bin/sh -c \
    'grep -qx ROOT-BASE /bundle-root; grep -qx DATA-BASE /scratch/bundle-data; echo ROOT-CHILD > /bundle-root-child; echo DATA-CHILD > /scratch/bundle-data-child; sync'
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID2" --output "$OUT2" --mode bundle --merge-ref=false \
    --drop-caches=false --run-root "$RR" >"$WORK/snapshot2.out" 2>"$WORK/snapshot2.log"
wait "$P2" 2>/dev/null || true
validate_bundle "$OUT2" "$SID2" 3 0 1 "$WORK/info2.json"
assert_manifest_snapshot_refs "$(readlink -f "$OUT2/$SID2.snapshot")" "$WORK/info2.json" 1

ROOT2_PATH="$(readlink -f "$OUT2/$SID2.snapshot")"
ROOT2="$(basename "$ROOT2_PATH" .bundle)"
assert_bundle_refs "$ROOT2_PATH"
LOCATION_B="$WORK/location-b"
LOCATED_B="$("$BIN/sandbox-ctl" upload-snapshot --manifest-config "$WORK/manifest.yaml" \
    --to-ref-location B=file://$LOCATION_B --quiet "$OUT2/$SID2.snapshot")"
assert_located_bundle "$LOCATED_B" B "$LOCATION_B" "$ROOT2" "$ROOT2_PATH"

rm -rf "$OUT1" "$OUT2" "$LOCATION_A"
echo "==> phase 3: located B alone restores -> self-contained Bundle C"
write_restore_yaml "$WORK/restore3.yaml" bundle-grandchild "$WORK/root-r3.ext4"
SID3="bundle-3-$$"
OUT3="$WORK/out3"
mkdir -p "$OUT3"
timeout -k 10s 180 "$BIN/sandbox-ctl" run --restore "$LOCATED_B" \
    --ref-location B=file://$LOCATION_B --config "$WORK/restore3.yaml" \
    --manifest-config "$WORK/manifest.yaml" --sandbox-id "$SID3" --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$RR" >"$WORK/run3.log" 2>&1 &
P3=$!
PIDS+=("$P3")
ready "$SID3" "$P3" "$WORK/ready3" || { tail -80 "$WORK/run3.log"; exit 1; }
"$BIN/sandbox-ctl" exec --sandbox-id "$SID3" --run-root "$RR" -- /bin/sh -c \
    'grep -qx ROOT-BASE /bundle-root; grep -qx ROOT-CHILD /bundle-root-child; grep -qx DATA-BASE /scratch/bundle-data; grep -qx DATA-CHILD /scratch/bundle-data-child; echo ROOT-C > /bundle-root-c; echo DATA-C > /scratch/bundle-data-c; sync'
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID3" --output "$OUT3" --mode bundle --merge-ref=false \
    --drop-caches=false --run-root "$RR" >"$WORK/snapshot3.out" 2>"$WORK/snapshot3.log"
wait "$P3" 2>/dev/null || true
validate_bundle "$OUT3" "$SID3" 3 0 1 "$WORK/info3.json"
assert_manifest_snapshot_refs "$(readlink -f "$OUT3/$SID3.snapshot")" "$WORK/info3.json" 2
ROOT3_PATH="$(readlink -f "$OUT3/$SID3.snapshot")"
ROOT3="$(basename "$ROOT3_PATH" .bundle)"
assert_bundle_refs "$ROOT3_PATH"

echo "==> phase 4: self-contained local C restore (20 samples)"
RESTORE_SAMPLES="$WORK/a-b-c-restore-ms"
: >"$RESTORE_SAMPLES"
for sample in $(seq 1 20); do
    write_restore_yaml "$WORK/restore4-$sample.yaml" "bundle-flat-refs-$sample" "$WORK/root-r4-$sample.ext4"
    SID4="bundle-4-$sample-$$"
    start_ns="$(date +%s%N)"
    timeout -k 10s 180 "$BIN/sandbox-ctl" run --restore "$OUT3/$SID3.snapshot" \
        --config "$WORK/restore4-$sample.yaml" \
        --manifest-config "$WORK/manifest.yaml" --sandbox-id "$SID4" --ch-binary "$BIN/cloud-hypervisor" \
        --run-root "$RR" >"$WORK/run4-$sample.log" 2>&1 &
    P4=$!
    PIDS+=("$P4")
    ready "$SID4" "$P4" "$WORK/ready4-$sample" || { tail -80 "$WORK/run4-$sample.log"; exit 1; }
    echo "$((($(date +%s%N)-start_ns)/1000000))" >>"$RESTORE_SAMPLES"
    if [ "$sample" = 1 ]; then
        "$BIN/sandbox-ctl" exec --sandbox-id "$SID4" --run-root "$RR" -- /bin/sh -c \
            'cat /bundle-root /bundle-root-child /bundle-root-c /scratch/bundle-data /scratch/bundle-data-child /scratch/bundle-data-c' >"$WORK/local-child-data"
        for marker in ROOT-BASE ROOT-CHILD ROOT-C DATA-BASE DATA-CHILD DATA-C; do
            grep -qx "$marker" "$WORK/local-child-data" || { cat "$WORK/local-child-data"; exit 1; }
        done
    fi
    kill "$P4" 2>/dev/null || true
    wait "$P4" 2>/dev/null || true
    unset "PIDS[$((${#PIDS[@]} - 1))]"
done
python3 - "$RESTORE_SAMPLES" <<'PY'
import math
import sys

samples = sorted(int(line) for line in open(sys.argv[1], encoding="ascii") if line.strip())
def percentile(value):
    return samples[math.ceil(value * len(samples)) - 1]

print(f"    A_to_B_to_C_restore_ready_ms n={len(samples)} p50={percentile(0.50)} p95={percentile(0.95)} p99={percentile(0.99)}")
PY

echo "==> phase 5: publish self-contained C to the Manifest store"
start_ns="$(date +%s%N)"
UPLOADED="$("$BIN/sandbox-ctl" upload-snapshot --manifest-config "$WORK/manifest.yaml" \
    --quiet "$OUT3/$SID3.snapshot")"
echo "    exact_upload_ms=$((($(date +%s%N)-start_ns)/1000000))"
[ "$UPLOADED" = "manifest://$ROOT3" ] || { echo "FAIL: exact upload changed root: $UPLOADED"; exit 1; }
"$BIN/manifest-ctl" verify --manifest-config "$WORK/manifest.yaml" "$ROOT3" >/dev/null

rm -rf "$OUT3" "$LOCATION_B"
echo "==> phase 6: Store-only restore of the byte-identical C root"
write_restore_yaml "$WORK/restore5.yaml" bundle-remote "$WORK/root-r5.ext4"
SID5="bundle-5-$$"
start_ns="$(date +%s%N)"
timeout -k 10s 180 "$BIN/sandbox-ctl" run --restore "manifest://$ROOT3" --config "$WORK/restore5.yaml" \
    --manifest-config "$WORK/manifest.yaml" --sandbox-id "$SID5" --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$RR" >"$WORK/run5.log" 2>&1 &
P5=$!
PIDS+=("$P5")
ready "$SID5" "$P5" "$WORK/ready5" || { tail -80 "$WORK/run5.log"; exit 1; }
echo "    remote_restore_ready_ms=$((($(date +%s%N)-start_ns)/1000000))"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID5" --run-root "$RR" -- /bin/sh -c \
    'cat /bundle-root /bundle-root-child /bundle-root-c /scratch/bundle-data /scratch/bundle-data-child /scratch/bundle-data-c' >"$WORK/remote-data"
for marker in ROOT-BASE ROOT-CHILD ROOT-C DATA-BASE DATA-CHILD DATA-C; do
    grep -qx "$marker" "$WORK/remote-data" || { cat "$WORK/remote-data"; exit 1; }
done
kill "$P5" 2>/dev/null || true
wait "$P5" 2>/dev/null || true

echo
echo "==> e2e_sandbox_bundle: OK (self-contained E/S Bundles, located publish/restore, Store publish, multi-disk)"
