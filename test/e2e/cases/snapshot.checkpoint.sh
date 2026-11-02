#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","userfaultfd"]
# Three writable disks: local versions, resume, concurrent publication, deletion,
# historical/cross-directory materialization, and source-independent restoration.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
source "$E2E_LIB/sandboxer/tarstream.sh"
: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"
require_root
require_kvm
unset MANIFEST_KEY MANIFEST_CONFIG
for command in cmp docker mkfs.ext4 python3 readlink sha256sum timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do require_binary "$binary"; done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded"
mkdir -p "$WORK/run" "$WORK/base" "$OUT"
RUN_ROOT="$WORK/run"
BASE_ROOT="$WORK/base"
SID="checkpoint-$BASHPID"
PATH_ID="slot-$BASHPID"
CP_DIR="$BASE_ROOT/$PATH_ID/checkpoint"
RUN_PID=""
PUBLISH_PIDS=()
cleanup() {
    local status=$?
    set +e
    for pid in "${PUBLISH_PIDS[@]}"; do
        kill -TERM "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    done
    if [ -n "$RUN_PID" ]; then
        kill -TERM "$RUN_PID" 2>/dev/null
        wait "$RUN_PID" 2>/dev/null
    fi
    [ ! -d "$BASE_ROOT/source-hidden" ] || mv "$BASE_ROOT/source-hidden" "$BASE_ROOT/$PATH_ID"
    [ ! -f "$WORK/root.img.hidden" ] || mv "$WORK/root.img.hidden" "$WORK/root.img"
    exit "$status"
}
trap cleanup EXIT

docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$WORK/root.img" --no-progress
ROOT_REF="$(plaintext_tarstream_ref "$WORK/root.img")"
for disk in root scratch dataset; do
    truncate -s 256M "$WORK/$disk.ext4"
    mkfs.ext4 -q -F -O ^has_journal "$WORK/$disk.ext4"
done
cat >"$WORK/cold.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    base: $ROOT_REF
    overlay: { diff_template: file://$WORK/root.ext4 }
  disks:
    - { name: scratch, diff_template: file://$WORK/scratch.ext4 }
    - { name: dataset, diff_template: file://$WORK/dataset.ext4 }
mounts:
  - { target: /scratch, type: disk, source: scratch }
  - { target: /data, type: disk, source: dataset }
launch: { exec: /bin/sleep, args: ["3600"], restart: never }
EOF
cat >"$WORK/restore.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
  diff_cow: { cache_size: 64MiB }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
EOF

guest() {
    "$BIN/sandbox-ctl" exec --path-id "$CURRENT_PATH" --run-root "$RUN_ROOT" -- "$@"
}
start() {
    local label=$1 path=$2
    shift 2
    CURRENT_PATH=$path
    timeout -k 10s 600 "$BIN/sandbox-ctl" run --sandbox-id "$SID" --path-id "$path" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" \
        "$@" >"$OUT/$label.log" 2>&1 &
    RUN_PID=$!
    for _ in $(seq 1 120); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --path-id "$path" --run-root "$RUN_ROOT" -- /bin/true >/dev/null 2>&1; then
            return
        fi
        if ! kill -0 "$RUN_PID" 2>/dev/null; then
            tail -100 "$OUT/$label.log" >&2 || true
            e2e_fail "$label exited before readiness"
        fi
        sleep 1
    done
    e2e_fail "$label did not become executable"
}
stop_run() {
    kill -TERM "$RUN_PID" 2>/dev/null || true
    wait "$RUN_PID" || true
    RUN_PID=""
}
finish_capture() {
    wait "$RUN_PID" || true
    RUN_PID=""
}
capture() {
    local kind=$1 id=$2
    shift 2
    "$BIN/sandbox-ctl" "$kind" --path-id "$CURRENT_PATH" --run-root "$RUN_ROOT" \
        --checkpoint "$id" --json "$@" >"$OUT/$id.json" 2>"$OUT/$id.log"
}
capture_path() {
    local id=$1 role=$2 directory=$3
    python3 - "$OUT/$id.json" "$role" "$directory" <<'PY'
import json, os, sys
r = json.load(open(sys.argv[1]))
assert isinstance(r['removedRefs'], list) and not r['removedRefs'], r
raw = r[sys.argv[2] + 'Ref']
assert raw.startswith('file://'), raw
name = raw.removeprefix('file://').split('@', 1)[0]
assert name == os.path.basename(name) and name not in ('.', '..'), raw
path = os.path.join(sys.argv[3], name)
assert os.path.isfile(path), path
print(path)
PY
}
check_hashes() {
    guest /bin/sh -c 'sha256sum /checkpoint-payload /scratch/checkpoint-payload /data/checkpoint-payload' >"$OUT/$1.hashes"
    cmp "$OUT/original.hashes" "$OUT/$1.hashes"
    guest python3 -c "$(cat "$E2E_LIB/sandboxer/trim_probe.py")" check >"$OUT/$1.trim.json"
}
check_exported_trim() {
    local artifact=$1 label=$2
    "$BIN/sandbox-ctl" info --json "$artifact" >"$OUT/$label.info.json"
    python3 - "$artifact" "$OUT/$label.info.json" "$OUT/trim-seed.json" >"$OUT/$label.payload.json" <<'PY'
import json, pathlib, struct, sys, tarfile, zlib
artifact = pathlib.Path(sys.argv[1])
config = json.load(open(sys.argv[2]))
position = json.load(open(sys.argv[3]))
disk = next(d for d in config['Boot']['Disks'] if d['Name'] == 'scratch')
assert disk.get('Overlay') is None, disk
# A restored active layer can be Hole above an immutable Zero layer. Resolve
# the real chain: its first present layer must provide zero and mask the seed.
# Legacy carriers store explicit zero Data; payload omission is independent.
for layer, ref in enumerate([disk['Base']] + (disk.get('BaseFromRefs') or [])):
    name = ref.removeprefix('file://').split('@', 1)[0]
    assert name == pathlib.Path(name).name, ref
    with tarfile.open(artifact.parent / name) as carrier:
        members = carrier.getmembers()
        bitmaps = [m for m in members if m.name == '.kuasar.presence.v1']
        payloads = [m for m in members if m.isfile() and not m.name.startswith('.kuasar.')]
        assert len(payloads) == 1 and len(bitmaps) <= 1, members
        payload = payloads[0]
        payload.size = int(payload.pax_headers.get('GNU.sparse.realsize', payload.size))
        page = 4096
        offset = position['offset']
        assert offset + page <= payload.size and offset % page == 0
        extents = payload.sparse if payload.sparse is not None else [(0, payload.size)]
        stored = sum(max(0, min(start + length, offset + page) - max(start, offset))
                     for start, length in extents)
        if bitmaps:
            metadata = carrier.extractfile(bitmaps[0]).read()
            assert metadata[:8] == b'KUPRES1\n'
            version, page, logical, bitmap_size = struct.unpack_from('<IIQQ', metadata, 8)
            assert version == 1 and page == 4096 and logical % page == 0
            assert bitmap_size == (logical // page + 7) // 8
            assert len(metadata) == 64 + bitmap_size and offset + page <= logical
            assert zlib.crc32(metadata[:60] + metadata[64:]) == struct.unpack_from('<I', metadata, 60)[0]
            assert payload.pax_headers['KUASAR.presence'] == '1'
            block = offset // page
            if not metadata[64 + block // 8] & (1 << (block % 8)):
                assert stored == 0, 'payload in absent page'
                continue
        else:
            assert 'KUASAR.presence' not in payload.pax_headers, 'missing presence entry'
            if stored == 0:
                continue
            assert stored == page, 'partial lower mask'
        data = carrier.extractfile(payload)
        data.seek(offset)
        assert data.read(page) == bytes(page)
        print(json.dumps({'offset': offset, 'presence': 1, 'payload_bytes': stored,
                          'carrier_bytes': (artifact.parent / name).stat().st_size,
                          'mask_layer': layer}))
        break
else:
    raise AssertionError('trim lost its lower mask through the complete chain')
PY
}
assert_list() {
    local path=$1 expected=$2
    "$BIN/sandbox-ctl" checkpoint list --path-id "$path" --run-root "$RUN_ROOT" --json >"$OUT/list.json"
    python3 - "$OUT/list.json" "$expected" <<'PY'
import collections, json, sys
refs = json.load(open(sys.argv[1]))
expected = {x: 3 for x in sys.argv[2].split(',') if x}
assert dict(collections.Counter(r['id'] for r in refs)) == expected, refs
assert len({(r['diff_id'], r['id']) for r in refs}) == len(refs)
PY
}
assert_checkpoint() {
    python3 - "$1" "$2" <<'PY'
import json, sys, tarfile, zipfile
with tarfile.open(sys.argv[1]) as carrier:
    payloads = [m for m in carrier if m.isfile() and not m.name.startswith('.kuasar.')]
    assert len(payloads) == 1, payloads
    payloads[0].size = int(payloads[0].pax_headers.get('GNU.sparse.realsize', payloads[0].size))
    z = zipfile.ZipFile(carrier.extractfile(payloads[0]))
    names = z.namelist()
    assert len(names) == len(set(names)), names
    assert set(names) == {'sandbox.runtime.cfg', 'checkpoint.json'}, names
    c = json.loads(z.read('checkpoint.json'))
    assert c['version'] == 1 and c['checkpoint_id'] == sys.argv[2], c
    assert [d['name'] for d in c['disks']] == ['scratch', 'dataset'], c
    ids = [c['root']['diff_id']] + [d['diff_id'] for d in c['disks']]
    assert len(set(ids)) == 3, c
    assert all('/' not in x and '\\' not in x for x in ids), c
PY
}
disk_inodes() {
    python3 - "$CP_DIR/diffs" <<'PY'
import pathlib, sys
for p in sorted(pathlib.Path(sys.argv[1]).glob('*.diff')):
    print(p.name, p.stat().st_ino)
PY
}
assert_no_restore_pins() {
    # Query the version-pin range on a new OFD without acquiring any lock.
    # A live directory/writer owner must remain; only temporary source readers
    # should have disappeared. A visible name alone cannot prove this handoff.
    python3 - "$1/diffs" <<'PY'
import fcntl, os, pathlib, struct, sys, time
paths = sorted(pathlib.Path(sys.argv[1]).glob('*.diff.lock'))
assert len(paths) >= 3, paths
for path in paths:
    fd = os.open(path, os.O_RDWR)
    try:
        query = struct.pack('hhqqii', fcntl.F_WRLCK, os.SEEK_SET, 16, 0, 0, 0)
        deadline = time.monotonic() + 5
        while True:
            lock = fcntl.fcntl(fd, fcntl.F_OFD_GETLK, query)
            kind = struct.unpack('hhqqii', lock)[0]
            if kind == fcntl.F_UNLCK:
                break
            assert kind != fcntl.F_RDLCK, \
                f'restore source pin still held after disk handoff: {path.name}'
            # GC briefly probes the same range with a write lock. Wait for
            # an unlocked observation; it must not hide a remaining reader.
            assert kind == fcntl.F_WRLCK and time.monotonic() < deadline, \
                f'version pin range remained locked after handoff: {path.name}'
            time.sleep(0.01)
    finally:
        os.close(fd)
PY
}

# Establish a portable nonzero lower first. A later Guest trim must mask that
# lower through both local Checkpoint and portable export/restore paths.
start trim-seed "seed-$PATH_ID" --config "$WORK/cold.yaml"
guest /bin/sh -c 'set -e; for f in /checkpoint-payload /scratch/checkpoint-payload /data/checkpoint-payload; do dd if=/dev/urandom of="$f" bs=1048576 count=16 2>/dev/null; done; sync'
guest python3 -c "$(cat "$E2E_LIB/sandboxer/trim_probe.py")" seed >"$OUT/trim-seed.json"
"$BIN/sandbox-ctl" export --path-id "$CURRENT_PATH" --run-root "$RUN_ROOT" --resume \
    --output "$WORK/trim-lower" >"$OUT/trim-lower.log" 2>&1
stop_run
# Exercise real Guest fstrim over the same nonzero lower in all writable
# policies. V4 also crosses a local memory Checkpoint; every policy crosses a
# portable export and cold restore. Preparation remains outside this case.
python3 - "$WORK/trim-crypto.yaml" <<'PY'
import secrets, sys
with open(sys.argv[1], 'w') as out:
    out.write('manifest: { key: "' + secrets.token_hex(32) + '" }\n')
    out.write('crypto: { chunk: aes, manifest: aes, local: auto }\n')
PY
for mode in v3 v4; do
    for policy in plain xts; do
        TRIM_LABEL="trim-$mode-$policy"
        TRIM_PATH="$TRIM_LABEL-$BASHPID"
        TRIM_ARGS=()
        [ "$mode" != v4 ] || TRIM_ARGS+=(--enable-checkpoint)
        CRYPTO_ARGS=()
        [ "$policy" != xts ] || CRYPTO_ARGS=(--manifest-config "$WORK/trim-crypto.yaml")
        start "$TRIM_LABEL" "$TRIM_PATH" --config "$WORK/restore.yaml" \
            --from "$WORK/trim-lower/$SID.sandbox" "${TRIM_ARGS[@]}" "${CRYPTO_ARGS[@]}"
        guest python3 -c "$(cat "$E2E_LIB/sandboxer/trim_probe.py")" trim >"$OUT/$TRIM_LABEL.json"
        if [ "$mode" = v4 ]; then
            capture snapshot "$TRIM_LABEL"
            finish_capture
            TRIM_S="$(capture_path "$TRIM_LABEL" snapshot "$BASE_ROOT/$TRIM_PATH/checkpoint")"
            start "$TRIM_LABEL-hot" "$TRIM_PATH" --config "$WORK/restore.yaml" \
                --restore "$TRIM_S" "${TRIM_ARGS[@]}" "${CRYPTO_ARGS[@]}"
            guest python3 -c "$(cat "$E2E_LIB/sandboxer/trim_probe.py")" check >"$OUT/$TRIM_LABEL-hot.json"
        fi
        "$BIN/sandbox-ctl" export --path-id "$TRIM_PATH" --run-root "$RUN_ROOT" --resume \
            --output "$WORK/$TRIM_LABEL-portable" >"$OUT/$TRIM_LABEL-export.log" 2>&1
        stop_run
        start "$TRIM_LABEL-portable" "$TRIM_PATH-portable" --config "$WORK/restore.yaml" \
            --from "$WORK/$TRIM_LABEL-portable/$SID.sandbox" "${TRIM_ARGS[@]}" "${CRYPTO_ARGS[@]}"
        guest python3 -c "$(cat "$E2E_LIB/sandboxer/trim_probe.py")" check >"$OUT/$TRIM_LABEL-portable.json"
        stop_run
    done
done
start cold "$PATH_ID" --config "$WORK/restore.yaml" --from "$WORK/trim-lower/$SID.sandbox" --enable-checkpoint
guest python3 -c "$(cat "$E2E_LIB/sandboxer/trim_probe.py")" trim >"$OUT/trim-completed.json"
# Keep the historical-GC fixture in the active DIFF as well as in its lower.
# In-place overwrite preserves allocated file extents, so it cannot reuse the
# trimmed probe's free block and invalidate the later raw-zero observation.
guest /bin/sh -c 'set -e; for f in /checkpoint-payload /scratch/checkpoint-payload /data/checkpoint-payload; do dd if=/dev/urandom of="$f" bs=1048576 count=16 conv=notrunc 2>/dev/null; done; sync'
guest /bin/sh -c 'sha256sum /checkpoint-payload /scratch/checkpoint-payload /data/checkpoint-payload' >"$OUT/original.hashes"

# Enabling the backend does not turn an ordinary export into a local version.
"$BIN/sandbox-ctl" export --path-id "$PATH_ID" --run-root "$RUN_ROOT" --resume \
    --output "$WORK/ordinary" >"$OUT/ordinary.log" 2>&1
check_exported_trim "$WORK/ordinary/$SID.sandbox" ordinary-trim
python3 - "$WORK/ordinary/$SID.sandbox" <<'PY'
import sys, tarfile, zipfile
with tarfile.open(sys.argv[1]) as carrier:
    payloads = [m for m in carrier if m.isfile() and not m.name.startswith('.kuasar.')]
    assert len(payloads) == 1, payloads
    # Python tarfile may reapply packed PAX size after decoding the GNU sparse map.
    payloads[0].size = int(payloads[0].pax_headers.get('GNU.sparse.realsize', payloads[0].size))
    with zipfile.ZipFile(carrier.extractfile(payloads[0])) as z:
        assert 'checkpoint.json' not in z.namelist()
PY
assert_list "$PATH_ID" ""

# Validation must fail before suspending the guest or publishing a version.
if "$BIN/sandbox-ctl" snapshot --path-id "$PATH_ID" --run-root "$RUN_ROOT" \
    --checkpoint rejected --output "$WORK/wrong-directory" >"$OUT/rejected.log" 2>&1; then
    e2e_fail "checkpoint capture accepted an unrelated output directory"
fi
guest /bin/true
assert_list "$PATH_ID" ""

capture export export-a --resume
E_A="$(capture_path export-a sandbox "$CP_DIR")"
assert_checkpoint "$E_A" export-a
capture snapshot snapshot-b
finish_capture
S_B="$(capture_path snapshot-b snapshot "$CP_DIR")"
E_B="$(capture_path snapshot-b sandbox "$CP_DIR")"
assert_checkpoint "$E_B" snapshot-b
REF_KEY="$(python3 - "$WORK/ref-required.yaml" <<'PY'
import pathlib, secrets, sys
key = secrets.token_hex(32)
path = pathlib.Path(sys.argv[1])
path.write_text(f'manifest: {{key: "{key}"}}\ncrypto: {{chunk: aes, manifest: aes, local: off, ref: required}}\n')
path.chmod(0o600)
print(key)
PY
)"
sha256sum "$E_A" "$E_B" "$S_B" >"$OUT/source-artifacts.hashes"
disk_inodes >"$OUT/before-resume.inodes"
[ "$(wc -l <"$OUT/before-resume.inodes")" -eq 3 ] || e2e_fail "expected exactly three DIFF files"

# A standalone publisher may pin first, then the owner resumes the latest version.
"$BIN/sandbox-ctl" publish --json --manifest-config "$WORK/ref-required.yaml" --to-ref-location "portable=file://$WORK/published-b" "$S_B" >"$OUT/published-b.json" 2>"$OUT/published-b.log" &
PUB_B=$!
PUBLISH_PIDS+=("$PUB_B")
start resumed "$PATH_ID" --config "$WORK/restore.yaml" --restore "$S_B" --enable-checkpoint
disk_inodes >"$OUT/after-resume.inodes"
cmp "$OUT/before-resume.inodes" "$OUT/after-resume.inodes"
check_hashes resumed
assert_list "$PATH_ID" export-a,snapshot-b
cp "$OUT/list.json" "$OUT/initial-version-refs.json"
if "$BIN/sandbox-ctl" checkpoint list --checkpoint-dir "$CP_DIR" --json >"$OUT/offline-busy.log" 2>&1; then
    e2e_fail "offline checkpoint management bypassed a live directory owner"
fi
if "$BIN/sandbox-ctl" checkpoint gc --checkpoint-dir "$CP_DIR" --json >"$OUT/offline-gc-busy.log" 2>&1; then
    e2e_fail "offline GC bypassed a live directory owner"
fi

# Reverse order: owner is live before publication pins; guest overwrites proceed.
# Successful publish routes exact deletion back through the live owner's gate.
"$BIN/sandbox-ctl" publish --json --delete-checkpoint --manifest-config "$WORK/ref-required.yaml" \
    --to-ref-location "portable=file://$WORK/published-a" "$E_A" >"$OUT/published-a.json" 2>"$OUT/published-a.log" &
PUB_A=$!
PUBLISH_PIDS+=("$PUB_A")
guest /bin/sh -c 'set -e; for f in /checkpoint-payload /scratch/checkpoint-payload /data/checkpoint-payload; do dd if=/dev/zero of="$f" bs=1048576 count=16 2>/dev/null; done; sync'
wait "$PUB_B"
wait "$PUB_A"
PUBLISH_PIDS=()
# Reuse the still-live snapshot-b checkpoint for a fresh auto-mode destination.
MANIFEST_KEY="$REF_KEY" "$BIN/sandbox-ctl" publish --json \
    --to-ref-location "portable=file://$WORK/published-auto" "$S_B" >"$OUT/published-auto.json"
for kind in a b auto; do
    python3 - "$OUT/published-$kind.json" "$WORK/published-$kind" <<'PY'
import json, pathlib, re, sys
report = json.load(open(sys.argv[1]))
directory = pathlib.Path(sys.argv[2])
for raw in (report.get('sandboxRef'), report.get('snapshotRef')):
    if raw:
        match = re.fullmatch(r'file://([0-9a-f]{64})\.(sandbox|snapshot)@hmac:([0-9a-f]{64})@location:portable', raw)
        assert match and match[1] == match[3], raw
        assert (directory / (match[1] + '.' + match[2])).is_file()
files = list(directory.iterdir())
assert files, directory
for path in files:
    with path.open('rb') as f:
        assert f.read(8) == b'\x89KTSENC\n', path
print(directory.name, 'encrypted tarstreams:', len(files))
PY
done
AUTO_REF="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["snapshotRef"])' "$OUT/published-auto.json")"
MANIFEST_KEY="$REF_KEY" "$BIN/sandbox-ctl" info --json \
    --ref-location "portable=file://$WORK/published-auto" "$AUTO_REF" >"$OUT/auto.info.json"
assert_no_restore_pins "$CP_DIR"
assert_list "$PATH_ID" snapshot-b
# Retry the original exact set after publication has already tombstoned it.
DELETE_ARGS=()
while IFS= read -r id; do DELETE_ARGS+=(--diff-id "$id"); done < <(python3 - "$OUT/initial-version-refs.json" <<'PY'
import json, sys
for r in json.load(open(sys.argv[1])):
    if r['id'] == 'export-a':
        print(r['diff_id'])
PY
)
[ "${#DELETE_ARGS[@]}" -eq 6 ] || e2e_fail "expected exact references for three deleted disks"
"$BIN/sandbox-ctl" checkpoint delete --path-id "$PATH_ID" --run-root "$RUN_ROOT"     --checkpoint export-a "${DELETE_ARGS[@]}" --json >"$OUT/delete-retry.json"
assert_list "$PATH_ID" snapshot-b
sha256sum -c "$OUT/source-artifacts.hashes"
stop_run

# The active epoch changed, so a historical restore must allocate new identities.
start historical "$PATH_ID" --config "$WORK/restore.yaml" --restore "$S_B" --enable-checkpoint
check_hashes historical
assert_no_restore_pins "$CP_DIR"
capture snapshot historical-c
finish_capture
E_C="$(capture_path historical-c sandbox "$CP_DIR")"
assert_checkpoint "$E_C" historical-c
python3 - "$E_B" "$E_C" <<'PY'
import json, sys, tarfile, zipfile
def ids(p):
    with tarfile.open(p) as carrier:
        payloads = [m for m in carrier if m.isfile() and not m.name.startswith('.kuasar.')]
        assert len(payloads) == 1, payloads
        payloads[0].size = int(payloads[0].pax_headers.get('GNU.sparse.realsize', payloads[0].size))
        with zipfile.ZipFile(carrier.extractfile(payloads[0])) as z:
            c = json.loads(z.read('checkpoint.json'))
    return {c['root']['diff_id'], *(d['diff_id'] for d in c['disks'])}
assert ids(sys.argv[1]).isdisjoint(ids(sys.argv[2]))
PY

# Capturing after materialization must keep its immutable lower dependencies
# after the producing runtime releases all temporary materialization handles.
S_C="$(capture_path historical-c snapshot "$CP_DIR")"
start historical-recapture historical-recapture --config "$WORK/restore.yaml" --restore "$S_C" --enable-checkpoint
check_hashes historical-recapture
stop_run

# A different PathID also materializes versions, with checkpoint mode on or off.
start cross-directory cross-slot --config "$WORK/restore.yaml" --restore "$S_B" --enable-checkpoint
check_hashes cross-directory
"$BIN/sandbox-ctl" export --path-id "$CURRENT_PATH" --run-root "$RUN_ROOT" --resume \
    --output "$WORK/cross-trim" >"$OUT/cross-trim.log" 2>&1
check_exported_trim "$WORK/cross-trim/$SID.sandbox" cross-trim
assert_no_restore_pins "$CP_DIR"
capture snapshot cross-d
finish_capture
E_D="$(capture_path cross-d sandbox "$BASE_ROOT/cross-slot/checkpoint")"
assert_checkpoint "$E_D" cross-d
S_D="$(capture_path cross-d snapshot "$BASE_ROOT/cross-slot/checkpoint")"
start cross-recapture cross-recapture --config "$WORK/restore.yaml" --restore "$S_D" --enable-checkpoint
check_hashes cross-recapture
stop_run
start mode-disabled legacy-slot --config "$WORK/restore.yaml" --restore "$S_B"
check_hashes mode-disabled
"$BIN/sandbox-ctl" export --path-id "$CURRENT_PATH" --run-root "$RUN_ROOT" --resume \
    --output "$WORK/single-trim" >"$OUT/single-trim.log" 2>&1
check_exported_trim "$WORK/single-trim/$SID.sandbox" single-trim
assert_no_restore_pins "$CP_DIR"
set +e
"$BIN/sandbox-ctl" export --path-id "$CURRENT_PATH" --run-root "$RUN_ROOT"     --checkpoint unsupported --resume >"$OUT/unsupported.log" 2>&1
UNSUPPORTED_STATUS=$?
set -e
[ "$UNSUPPORTED_STATUS" -eq 78 ] || e2e_fail "legacy checkpoint exit=$UNSUPPORTED_STATUS, want 78"
guest /bin/true
stop_run

# Published roots contain portable data. Hide the original version directory and
# immutable root source before restoring both the published E and S artifacts.
mv "$BASE_ROOT/$PATH_ID" "$BASE_ROOT/source-hidden"
mv "$WORK/root.img" "$WORK/root.img.hidden"
for kind in a b; do
    read -r REF < <(python3 - "$OUT/published-$kind.json" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
assert isinstance(r['removedRefs'], list)
print(r.get('snapshotRef') or r['sandboxRef'])
PY
)
    if [ "$kind" = a ]; then MODE=--from; else MODE=--restore; fi
    start "portable-$kind" "portable-$kind" --config "$WORK/restore.yaml" "$MODE" "$REF" \
        --enable-checkpoint --manifest-config "$WORK/ref-required.yaml" \
        --ref-location "portable=file://$WORK/published-$kind" --stats-json "$OUT/portable-$kind.stats.json"
    check_hashes "portable-$kind"
    guest /bin/sh -ec 'for file in /cow-marker /scratch/cow-marker /data/cow-marker; do
        echo "$1" >"$file"
    done
    sync
    for file in /cow-marker /scratch/cow-marker /data/cow-marker; do
        test "$(cat "$file")" = "$1"
    done' sh "portable-$kind"
    stop_run
    # Each writable backend reports the same shared sandbox cache.
    python3 - "$OUT/portable-$kind.stats.json" "$MODE" "$BASE_ROOT/portable-$kind/checkpoint/diffs" <<'PY'
import json, pathlib, struct, sys
disks = list(pathlib.Path(sys.argv[3]).glob('*.diff'))
assert len(disks) == 3, disks
for path in disks:
    with path.open('rb') as f:
        magic, version, prefix, flag = struct.unpack('>8sHHI', f.read(16))
    assert (magic, version, prefix, flag) == (b'\x89KDXTS1\n', 4, 16, 0), (path, magic, version, prefix, flag)
    print(sys.argv[2], path.name, 'local DIFF version', version, 'encryption flag', flag)
with open(sys.argv[1]) as stream:
    backends = json.load(stream)['backends']
writable = [b for b in backends if 'diff_cow_cache' in b.get('extra', {})]
assert len(writable) == len({b['name'] for b in writable}) == 3, backends
for b in writable:
    cache = b['extra']['diff_cow_cache']
    assert (cache['Capacity'], cache['MaxDirty']) == (67108864, 33554432), (b['name'], cache)
    assert b['write']['count'] > 0, b
    print(sys.argv[2], b['name'], 'Capacity', cache['Capacity'], 'MaxDirty', cache['MaxDirty'],
          'writes', b['write']['count'])
PY
done
mv "$BASE_ROOT/source-hidden" "$BASE_ROOT/$PATH_ID"
mv "$WORK/root.img.hidden" "$WORK/root.img"

# Reusing the business directory creates new active identities. The earlier
# writers are now historical files; online management must share their bounded
# metadata handles with independent maintenance, and obsolete payload must be
# reclaimed without another delete or foreground write.
CP_DIR="$BASE_ROOT/$PATH_ID/checkpoint"
python3 - "$CP_DIR" "$OUT/history-before-gc.json" <<'PY'
import json, pathlib, sys
files = list((pathlib.Path(sys.argv[1]) / 'diffs').glob('*.diff'))
assert len(files) >= 6, files
json.dump({p.name: {'allocated': p.stat().st_blocks * 512, 'length': p.stat().st_size}
           for p in files}, open(sys.argv[2], 'w'))
PY
start gc-history "$PATH_ID" --config "$WORK/cold.yaml" --enable-checkpoint
for version in snapshot-b historical-c; do
    "$BIN/sandbox-ctl" checkpoint delete --path-id "$PATH_ID" --run-root "$RUN_ROOT" \
        --checkpoint "$version" --json >"$OUT/gc-delete-$version.json"
done
assert_list "$PATH_ID" ""
python3 - "$CP_DIR" "$OUT/history-before-gc.json" "$OUT/history-after-gc.json" <<'PY'
import json, pathlib, sys, time
root = pathlib.Path(sys.argv[1]) / 'diffs'
before = json.load(open(sys.argv[2]))
deadline = time.monotonic() + 60
while True:
    after = {name: {'allocated': (root / name).stat().st_blocks * 512,
                    'length': (root / name).stat().st_size} for name in before}
    assert all(after[n]['length'] == before[n]['length'] for n in before), (before, after)
    reclaimed = sum(v['allocated'] for v in before.values()) - sum(v['allocated'] for v in after.values())
    if reclaimed >= 8 * 1024 * 1024:
        json.dump({'reclaimed_bytes': reclaimed, 'files': after}, open(sys.argv[3], 'w'))
        break
    assert time.monotonic() < deadline, ('historical maintenance did not reclaim backlog', before, after)
    time.sleep(.2)
PY
guest /bin/true
stop_run
"$BIN/sandbox-ctl" checkpoint gc --checkpoint-dir "$CP_DIR" --json >"$OUT/offline-gc.json"
python3 - "$OUT/offline-gc.json" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))
assert len(rows) >= 9 and len({r['diff_id'] for r in rows}) == len(rows), rows
assert all(r['state'] == 'complete' and r['freed_slots'] >= 0 for r in rows), rows
PY

# Explicit handoff/delete/re-capture flow through both hot and CLI cold restore.
# Repeat with local XTS enabled; the runtime still owns the live disks after its
# temporary restore readers disappear. Publication must produce a portable root.
for policy in plain xts; do
    PIN_PATH="pins-$policy"
    CP_DIR="$BASE_ROOT/$PIN_PATH/checkpoint"
    CRYPTO_ARGS=()
    if [ "$policy" = xts ]; then
        python3 - "$WORK/pins-crypto.yaml" <<'PY'
import secrets, sys
with open(sys.argv[1], 'w') as out:
    out.write('manifest: { key: "' + secrets.token_hex(32) + '" }\n')
    out.write('crypto: { chunk: aes, manifest: aes, local: auto }\n')
PY
        CRYPTO_ARGS=(--manifest-config "$WORK/pins-crypto.yaml")
    fi
    start "pins-cold-$policy" "$PIN_PATH" --config "$WORK/cold.yaml" --enable-checkpoint "${CRYPTO_ARGS[@]}"
    guest /bin/sh -c 'for f in /handoff /scratch/handoff /data/handoff; do echo before-delete > "$f"; done; sync'
    capture snapshot "pins-source-$policy"
    finish_capture
    PIN_S="$(capture_path "pins-source-$policy" snapshot "$CP_DIR")"
    start "pins-hot-$policy" "$PIN_PATH" --config "$WORK/restore.yaml" --restore "$PIN_S" --enable-checkpoint "${CRYPTO_ARGS[@]}"
    assert_no_restore_pins "$CP_DIR"
    guest /bin/sh -c 'for f in /handoff /scratch/handoff /data/handoff; do grep -qx before-delete "$f" || exit 1; done'
    capture snapshot "pins-cold-source-$policy"
    finish_capture
    PIN_E="$(capture_path "pins-cold-source-$policy" sandbox "$CP_DIR")"
    start "pins-from-$policy" "$PIN_PATH" --config "$WORK/restore.yaml" --from "$PIN_E" --enable-checkpoint "${CRYPTO_ARGS[@]}"
    assert_no_restore_pins "$CP_DIR"
    for version in "pins-source-$policy" "pins-cold-source-$policy"; do
        "$BIN/sandbox-ctl" checkpoint delete --path-id "$PIN_PATH" --run-root "$RUN_ROOT" \
            --checkpoint "$version" --json >"$OUT/delete-$version.json"
    done
    assert_list "$PIN_PATH" ""
    guest /bin/sh -c 'for f in /handoff /scratch/handoff /data/handoff; do grep -qx before-delete "$f" || exit 1; echo after-delete > "$f"; done; sync'
    capture snapshot "pins-after-delete-$policy"
    finish_capture
    PIN_S="$(capture_path "pins-after-delete-$policy" snapshot "$CP_DIR")"
    PUBLISH_ARGS=("${CRYPTO_ARGS[@]}")
    if [ "$policy" = plain ]; then PUBLISH_ARGS=(--manifest-config "$WORK/ref-required.yaml"); fi
    "$BIN/sandbox-ctl" publish --json "${PUBLISH_ARGS[@]}" \
        --to-ref-location "portable=file://$WORK/pins-published-$policy" "$PIN_S" >"$OUT/pins-published-$policy.json"
    PIN_REF="$(python3 - "$OUT/pins-published-$policy.json" <<'PY'
import json, sys
print(json.load(open(sys.argv[1]))['snapshotRef'])
PY
)"
    start "pins-portable-$policy" "pins-portable-$policy" --config "$WORK/restore.yaml" --restore "$PIN_REF" \
        --ref-location "portable=file://$WORK/pins-published-$policy" "${PUBLISH_ARGS[@]}"
    guest /bin/sh -c 'for f in /handoff /scratch/handoff /data/handoff; do grep -qx after-delete "$f" || exit 1; done'
    stop_run
    # Metadata-only reclamation of encrypted DIFFs needs no key/config.
    "$BIN/sandbox-ctl" checkpoint gc --checkpoint-dir "$CP_DIR" --json >"$OUT/gc-$policy.json"
    python3 - "$OUT/gc-$policy.json" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))
assert rows and all(r['state'] == 'complete' for r in rows), rows
PY
done
echo "PASS snapshot.checkpoint.sh"
