#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","userfaultfd"]
# Exercise the production holder while ENOSPC prevents its payload publication.
# Growing the private filesystem releases the fault without a product test hook.
set -euo pipefail
umask 077
source "${E2E_LIB:?E2E_LIB is required}/common.sh"
source "$E2E_LIB/sandboxer/tarstream.sh"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"
require_root
require_kvm
unset MANIFEST_KEY MANIFEST_CONFIG
for command in docker mount umount mountpoint python3 timeout truncate mkfs.ext4; do require_command "$command"; done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do require_binary "$binary"; done
# Each pair runs in an isolated filesystem and sandbox instance. Flatten the
# immutable guest root once; every case references it outside the ENOSPC mount.
if [ -z "${CHECKPOINT_MEMORY_FIRST_FORMAT:-}" ]; then
    mkdir -p "$WORK/fixture" "$OUT"
    docker image inspect "$E2E_IMAGE" >/dev/null
    docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$WORK/fixture/root.img" --no-progress
    FIXTURE_DIRECTORY="$WORK/fixture"
    for pair in local:local local:bundle bundle:local bundle:bundle; do
        first=${pair%%:*}
        second=${pair##*:}
        WORK="$WORK/$first-$second" OUT="$OUT/$first-$second" \
            CHECKPOINT_MEMORY_FIXTURE_DIRECTORY="$FIXTURE_DIRECTORY" \
            CHECKPOINT_MEMORY_FIRST_FORMAT="$first" CHECKPOINT_MEMORY_SECOND_FORMAT="$second" \
            bash "$0"
    done
    echo "PASS snapshot.checkpoint-memory.sh (four format pairs)"
    exit 0
fi
FIRST_FORMAT=$CHECKPOINT_MEMORY_FIRST_FORMAT
SECOND_FORMAT=${CHECKPOINT_MEMORY_SECOND_FORMAT:?second capture format is required}
case "$FIRST_FORMAT:$SECOND_FORMAT" in local:local|local:bundle|bundle:local|bundle:bundle) ;; *) e2e_fail "invalid capture format pair" ;; esac
FIXTURE_DIRECTORY=${CHECKPOINT_MEMORY_FIXTURE_DIRECTORY:-$WORK}
mkdir -p "$WORK/base" "$WORK/run" "$OUT"
BASE_ROOT="$WORK/base"
RUN_ROOT="$WORK/run"
SID="memory-checkpoint-$BASHPID"
PATH_ID="memory-$BASHPID"
CP_DIR="$BASE_ROOT/$PATH_ID/checkpoint"
RUN_PIDS=()
REQUEST_PIDS=()
cleanup() {
    local status=$?
    set +e
    for pid in "${REQUEST_PIDS[@]}" "${RUN_PIDS[@]}"; do kill -TERM "$pid" 2>/dev/null; done
    # A failed assertion must not leave an intentionally ENOSPC-blocked holder.
    # Fence only records in this case's private directory and exact generation.
    python3 - "$BASE_ROOT" "$SID" <<'PY'
import json, os, pathlib, signal, sys
for path in pathlib.Path(sys.argv[1]).glob('*/checkpoint/memory/*/*.json'):
    try:
        r = json.loads(path.read_text())
        if r['producer']['sandbox_id'] != sys.argv[2]:
            continue
        h = r['holder']
        stat = pathlib.Path(f"/proc/{h['pid']}/stat").read_text().rsplit(')', 1)[1].split()
        if int(stat[19]) == h['start_time']:
            os.kill(h['pid'], signal.SIGKILL)
    except (OSError, ValueError, KeyError):
        pass
PY
    for pid in "${REQUEST_PIDS[@]}" "${RUN_PIDS[@]}"; do wait "$pid" 2>/dev/null; done
    if mountpoint -q "$BASE_ROOT"; then umount "$BASE_ROOT"; fi
    exit "$status"
}
trap cleanup EXIT
mount -t tmpfs -o size=64m,mode=700 fastpause-e2e "$BASE_ROOT"
if [ ! -f "$FIXTURE_DIRECTORY/root.img" ]; then
    docker image inspect "$E2E_IMAGE" >/dev/null
    docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$FIXTURE_DIRECTORY/root.img" --no-progress
fi
ROOT_REF="$(plaintext_tarstream_ref "$FIXTURE_DIRECTORY/root.img")"
# Keep the immutable root in its named external location. Otherwise ordinary
# local dependency preservation could fill the test filesystem before freeze.
ROOT_REF="file://root.img@digest:${ROOT_REF##*@digest:}@location:fixture"
truncate -s 256M "$WORK/root.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/root.ext4"
python3 - "$WORK/cold.yaml" "$WORK/restore.yaml" "$WORK/publish.yaml" "$BIN" "$ROOT_REF" "$WORK/root.ext4" <<'PY'
import json, pathlib, secrets, sys
program = '''import hashlib,json,os,time
memory = bytearray(os.urandom(128 * 1024 * 1024))
expected = hashlib.sha256(memory).hexdigest()
counter = 0
while True:
    actual = hashlib.sha256(memory).hexdigest()
    if actual != expected: raise RuntimeError('frozen memory corrupted')
    counter += 1
    with open('/memory-status.tmp','w') as f:
        json.dump({'hash':actual,'counter':counter},f)
    os.replace('/memory-status.tmp','/memory-status')
    time.sleep(.2)
'''
common = {'resources': {'capacity': {'cpu': 1, 'memory': '512MiB'},
                        'allocatable': {'cpu': 1, 'memory': '512MiB'}},
          'boot': {'kernel': 'file://' + sys.argv[4] + '/vmlinux',
                   'runtime': 'file://' + sys.argv[4] + '/sandbox-runtime.bundle'}}
pathlib.Path(sys.argv[2]).write_text(json.dumps(common))
common['boot']['root'] = {'base': sys.argv[5], 'overlay': {'diff_template': 'file://' + sys.argv[6]}}
common['launch'] = {'exec': '/usr/bin/env', 'args': ['python3', '-u', '-c', program], 'restart': 'never'}
pathlib.Path(sys.argv[1]).write_text(json.dumps(common))
pathlib.Path(sys.argv[3]).write_text(json.dumps({'manifest': {'key': secrets.token_hex(32)},
    'crypto': {'chunk': 'aes', 'manifest': 'aes', 'local': 'off', 'ref': 'auto'}}))
PY
guest() {
    "$BIN/sandbox-ctl" exec --path-id "$CURRENT_PATH" --run-root "$RUN_ROOT" -- "$@"
}
start() {
    local label=$1 path=$2
    shift 2
    CURRENT_PATH=$path
    timeout -k 10s 240 "$BIN/sandbox-ctl" run --sandbox-id "$SID" --path-id "$path" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" \
        --manifest-config "$WORK/publish.yaml" \
        --ref-location "fixture=file://$FIXTURE_DIRECTORY" \
        "$@" >"$OUT/$label.log" 2>&1 &
    RUN_PID=$!
    RUN_PIDS+=("$RUN_PID")
    for _ in $(seq 1 90); do
        if timeout -k 2s 5 "$BIN/sandbox-ctl" exec --path-id "$path" --run-root "$RUN_ROOT" \
            -- /bin/cat /memory-status >"$OUT/$label.status.json" 2>/dev/null; then return; fi
        if ! kill -0 "$RUN_PID" 2>/dev/null; then
            tail -100 "$OUT/$label.log" >&2
            e2e_fail "$label exited before readiness"
        fi
        sleep .2
    done
    e2e_fail "$label did not become executable"
}
capture() {
    local id=$1 mode=$2
    shift 2
    python3 - "$OUT/$id.json" "$OUT/$id.timing.json" "$OUT/$id.stderr" \
        "$BIN/sandbox-ctl" snapshot --path-id "$PATH_ID" --run-root "$RUN_ROOT" \
        --checkpoint "$id" --mode "$mode" --json "$@" <<'PY'
import json, subprocess, sys, time
start = time.monotonic()
with open(sys.argv[1], 'w') as out, open(sys.argv[3], 'w') as err:
    result = subprocess.run(sys.argv[4:], stdout=out, stderr=err)
json.dump({'elapsed_seconds': time.monotonic() - start, 'exit': result.returncode}, open(sys.argv[2], 'w'))
raise SystemExit(result.returncode)
PY
}
snapshot_path() {
    python3 - "$OUT/$1.json" "$CP_DIR" <<'PY'
import json,pathlib,sys
r=json.load(open(sys.argv[1])); raw=r['snapshotRef']
assert raw.startswith('file://'), r
name=raw[7:].split('@',1)[0]
assert name==pathlib.Path(name).name, raw
print(pathlib.Path(sys.argv[2])/name)
PY
}
check_memory() {
    local label=$1
    local initial_counter
    initial_counter="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["counter"])' "$OUT/$label.status.json")"
    for _ in $(seq 1 90); do
        guest /bin/cat /memory-status >"$OUT/$label.status.json"
        if python3 - "$OUT/cold.status.json" "$OUT/$label.status.json" "$initial_counter" <<'PY'
import json,sys
before,after=(json.load(open(p)) for p in sys.argv[1:3])
assert before['hash']==after['hash'], (before,after)
raise SystemExit(0 if after['counter']>int(sys.argv[3]) else 1)
PY
        then return; fi
        sleep .2
    done
    e2e_fail "$label did not resume the memory-checking workload"
}

start cold "$PATH_ID" --config "$WORK/cold.yaml" --enable-checkpoint
FIRST_RUN=$RUN_PID
capture first "$FIRST_FORMAT"
S_FIRST="$(snapshot_path first)"
# Success must precede this round's payload publication, but follow retirement.
"$BIN/sandbox-ctl" info --json --manifest-config "$WORK/publish.yaml" \
    --ref-location "fixture=file://$FIXTURE_DIRECTORY" "$S_FIRST" >"$OUT/first-info.json"
python3 - "$CP_DIR" "$OUT/first.json" "$OUT/first-record.json" "$RUN_ROOT/$PATH_ID" <<'PY'
import hashlib,json,os,pathlib,stat,sys
root=pathlib.Path(sys.argv[1]); loc=json.loads((root/'current-memory.json').read_text())
r=json.loads((root/'memory'/hashlib.sha256(loc['checkpoint_id'].encode()).hexdigest()/(loc['memory_id']+'.json')).read_text())
assert (r['checkpoint_id'],r['memory_id'],r['epoch'])==(loc['checkpoint_id'],loc['memory_id'],loc['epoch']), (r,loc)
assert r['snapshot_ref']==json.load(open(sys.argv[2]))['snapshotRef'], r
assert r['committed'] and r['runtime_retired'] and not r['file_ready'] and not r['ram_released'], r
run=pathlib.Path(sys.argv[4])
assert not os.path.lexists(run), ('ordinary RunDir survived pause', run)
holder=r['holder']; service=pathlib.Path(holder['directory']['path'])
assert service.parent.resolve()==run.parent.resolve() and service!=run, (service,run)
st=service.lstat()
assert stat.S_ISDIR(st.st_mode) and (st.st_dev,st.st_ino)==(holder['directory']['device'],holder['directory']['inode']), holder
assert pathlib.Path(holder['socket_path'])==service/'m.sock' and pathlib.Path(holder['socket_path']).is_socket(), holder
assert json.loads((service/'identity.json').read_text())['holder']==holder, holder
json.dump(r,open(sys.argv[3],'w'))
PY
start resumed "$PATH_ID" --config "$WORK/restore.yaml" --restore "$S_FIRST" --enable-checkpoint
SECOND_RUN=$RUN_PID
check_memory resumed
python3 - "$RUN_ROOT/$PATH_ID" "$SID" "$OUT/resumed-run-identity.json" <<'PY'
import json,pathlib,sys
run=pathlib.Path(sys.argv[1]); pid=run/(sys.argv[2]+'.pid')
a,b=run.lstat(),pid.lstat()
json.dump({'run':[a.st_dev,a.st_ino], 'pid_file':[b.st_dev,b.st_ino], 'pid':pid.read_text()},open(sys.argv[3],'w'))
PY
# A portable publisher must consume the frozen view while the disk is blocked.
"$BIN/sandbox-ctl" publish --json --manifest-config "$WORK/publish.yaml" \
    --ref-location "fixture=file://$FIXTURE_DIRECTORY" \
    --to-ref-location "portable=file://$WORK/published" "$S_FIRST" >"$OUT/published.json" 2>"$OUT/published.stderr"
if capture rejected "$SECOND_FORMAT" --no-wait-for-previous-memory-save; then e2e_fail "no-wait captured a second pending generation"; fi
assert_contains "$OUT/rejected.stderr" "previous checkpoint memory save is pending"
guest /bin/true
# The default request waits while the successor remains executable. Its
# cancellation must finish before unblocking the dump, so the live successor
# cannot be retired by a competing second capture during our directory check.
timeout -k 2s 1 "$BIN/sandbox-ctl" snapshot --path-id "$PATH_ID" --run-root "$RUN_ROOT" \
    --checkpoint cancelled --mode "$SECOND_FORMAT" --json >"$OUT/cancelled.json" 2>"$OUT/cancelled.stderr" &
WAIT_PID=$!
REQUEST_PIDS+=("$WAIT_PID")
sleep .2
kill -0 "$WAIT_PID" || e2e_fail "default pause did not wait for previous memory"
guest /bin/true
python3 - "$CP_DIR" "$OUT/first-record.json" <<'PY'
import json,pathlib,sys
root=pathlib.Path(sys.argv[1]); first=json.load(open(sys.argv[2]))
loc=json.loads((root/'current-memory.json').read_text())
assert loc['memory_id']==first['memory_id'], (loc,first)
assert len(list((root/'memory').glob('*/*.json')))==1, 'waiting request created another holder'
PY
set +e
wait "$WAIT_PID"
CANCELLED_STATUS=$?
set -e
REQUEST_PIDS=()
[ "$CANCELLED_STATUS" -eq 124 ] || e2e_fail "waiting capture was not cancellable: $CANCELLED_STATUS"
guest /bin/true
# The normal writer retries and publishes; the live restored reader switches
# to that file even without another explicit restore or export request.
mount -o remount,size=512m "$BASE_ROOT"
wait "$FIRST_RUN"
kill -0 "$SECOND_RUN" || e2e_fail "old holder completion stopped the live successor"
python3 - "$RUN_ROOT/$PATH_ID" "$SID" "$OUT/resumed-run-identity.json" "$OUT/first-record.json" "$CP_DIR" <<'PY'
import hashlib,json,os,pathlib,sys
run=pathlib.Path(sys.argv[1]); pid=run/(sys.argv[2]+'.pid'); expected=json.load(open(sys.argv[3]))
a,b=run.lstat(),pid.lstat()
assert [a.st_dev,a.st_ino]==expected['run'] and [b.st_dev,b.st_ino]==expected['pid_file'], expected
assert pid.read_text()==expected['pid'] and (run/'ctl.sock').is_socket(), expected
original=json.load(open(sys.argv[4])); root=pathlib.Path(sys.argv[5])
r=json.loads((root/'memory'/hashlib.sha256(original['checkpoint_id'].encode()).hexdigest()/(original['memory_id']+'.json')).read_text())
assert (r['memory_id'],r['epoch'])==(original['memory_id'],original['epoch']), r
assert r['file_ready'] and r['ram_released'], r
assert not os.path.lexists(r['holder']['directory']['path']), r
PY
guest /bin/true
check_memory resumed
capture second "$SECOND_FORMAT"
wait "$SECOND_RUN"
python3 - "$CP_DIR" "$OUT/first-record.json" "$FIRST_FORMAT" "$SECOND_FORMAT" <<'PY'
import hashlib,json,pathlib,sys,tarfile,zipfile
root=pathlib.Path(sys.argv[1]); original=json.load(open(sys.argv[2]))
records={r['checkpoint_id']:r for r in (json.loads(p.read_text()) for p in (root/'memory').glob('*/*.json'))}
assert set(records)=={'first','second'}, records.keys()
for r in records.values():
    assert r['runtime_retired'] and r['file_ready'] and r['ram_released'], r
    assert not pathlib.Path(r['holder']['socket_path']).exists(), r
assert records['second']['memory_id']!=original['memory_id']
for checkpoint,mode in zip(('first','second'),sys.argv[3:5]):
    r=records[checkpoint]
    ref=r['payload_ref']
    assert ref.startswith('file://'),ref
    name=ref[7:].split('@',1)[0]
    assert pathlib.Path(name).name==name,ref
    carrier=root/name
    if mode=='bundle':
        assert '@manifest:' in ref and zipfile.is_zipfile(carrier), ('Bundle capture fell back to tarstream',ref)
    else:
        assert '@digest:' in ref and tarfile.is_tarfile(carrier), ('tarstream capture used unexpected format',ref)
PY
S_SECOND="$(snapshot_path second)"
start next-resume "$PATH_ID" --config "$WORK/restore.yaml" --restore "$S_SECOND" --enable-checkpoint
check_memory next-resume
kill -TERM "$RUN_PID"
wait "$RUN_PID" || true
PORTABLE_REF="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["snapshotRef"])' "$OUT/published.json")"
mv "$CP_DIR" "$CP_DIR.hidden"
start portable "portable-$PATH_ID" --config "$WORK/restore.yaml" --restore "$PORTABLE_REF" \
    --manifest-config "$WORK/publish.yaml" --ref-location "portable=file://$WORK/published"
check_memory portable
kill -TERM "$RUN_PID"
wait "$RUN_PID" || true
mv "$CP_DIR.hidden" "$CP_DIR"
echo "PASS snapshot.checkpoint-memory.sh ($FIRST_FORMAT -> $SECOND_FORMAT)"
