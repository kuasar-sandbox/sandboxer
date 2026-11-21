#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","userfaultfd"]
# Exercise real ENOSPC, then bound each retired holder's output file size so
# failed payload retries cannot also starve new capture metadata. Both faults
# are external process/filesystem controls; no product test hook is used.
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
    if [ "$status" -ne 0 ]; then
        printf 'checkpoint-memory failed (%s -> %s), exit=%s\n' "$FIRST_FORMAT" "$SECOND_FORMAT" "$status" >&2
        df -PB1 "$CP_DIR" "$WORK" >&2
        # Native CI retains the case log, so include redirected command errors
        # before its private OUT directory is collected and removed.
        local diagnostic
        for diagnostic in "$OUT"/*.stderr "$OUT"/*.log "$OUT"/*.timing.json; do
            [ -s "$diagnostic" ] || continue
            printf 'checkpoint-memory diagnostic: %s\n' "${diagnostic##*/}" >&2
            tail -80 "$diagnostic" >&2
        done
    fi
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
    if mountpoint -q "$CP_DIR/diffs"; then umount "$CP_DIR/diffs"; fi
    if mountpoint -q "$CP_DIR"; then umount "$CP_DIR"; fi
    exit "$status"
}
trap cleanup EXIT
# Limit only publication storage. Restoring an older disk version must be
# able to materialize its ordinary lower artifacts beside this mount.
mkdir -p "$CP_DIR"
mount -t tmpfs -o size=64m,mode=700 fastpause-e2e "$CP_DIR"
# Live writable DIFFs also need independent capacity while saves fail.
mkdir -p "$WORK/diffs" "$CP_DIR/diffs"
mount --bind "$WORK/diffs" "$CP_DIR/diffs"
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
    # Dirty every page while preserving the expected bytes. Every generation
    # must have a payload larger than the deliberately small filesystem.
    for offset in range(0,len(memory),4096):
        memory[offset] ^= 1
        memory[offset] ^= 1
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
# The deliberate whole-filesystem ENOSPC can also transiently block the small
# owner publication required by restore. Only this first pending-RAM restore may
# retry that exact failure, after the failed attempt has completely torn down.
owner_enospc() {
    python3 - "$CP_DIR" "$1" <<'PY_OWNER_ENOSPC'
import pathlib,re,sys
lines=pathlib.Path(sys.argv[2]).read_text().splitlines()
assert len(lines)>=2 and lines[-2]=='restore exited before readiness', lines
assert re.fullmatch(r'write '+re.escape(sys.argv[1])+r'/\.owner-[0-9]+: no space left on device',lines[-1]), lines
for line in lines[:-2]:
    assert re.match(r'^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d+ \[sandbox-ctl run --restore\] ',line), line
    assert not re.search(r'(?i)\b(error|failed|failure|fatal|panic)\b|no space left on device',line), line
PY_OWNER_ENOSPC
}
check_pending_memory() {
    python3 - "$CP_DIR" "$OUT/first.json" "$OUT/first-record.json" "$RUN_ROOT/$PATH_ID" "$SID" "$1" <<'PY_PENDING_MEMORY'
import hashlib,json,os,pathlib,stat,sys
root=pathlib.Path(sys.argv[1])
if sys.argv[6]=='initial':
    loc=json.loads((root/'current-memory.json').read_text())
else:
    loc=json.load(open(sys.argv[3]))
record=root/'memory'/hashlib.sha256(loc['checkpoint_id'].encode()).hexdigest()/(loc['memory_id']+'.json')
r=json.loads(record.read_text())
assert (r['checkpoint_id'],r['memory_id'],r['epoch'])==(loc['checkpoint_id'],loc['memory_id'],loc['epoch']), (r,loc)
assert r['snapshot_ref']==json.load(open(sys.argv[2]))['snapshotRef'] and r['producer']['sandbox_id']==sys.argv[5], r
assert r['committed'] and r['runtime_retired'] and not r['file_ready'] and not r['ram_released'], r
assert not r.get('aborted') and not r.get('discarded'), r
baseline=pathlib.Path(sys.argv[3])
if sys.argv[6]!='initial':
    original=json.loads(baseline.read_text())
    for key in ('version','producer','checkpoint_id','host_operation_id','memory_id','epoch','snapshot_ref','sandbox_ref','holder'):
        assert r.get(key)==original.get(key), (key,r,original)
run=pathlib.Path(sys.argv[4])
if sys.argv[6]!='ready':
    assert not os.path.lexists(run), ('ordinary RunDir survived failed restore or pause',run)
holder=r['holder']; service=pathlib.Path(holder['directory']['path'])
assert service.parent.resolve()==run.parent.resolve() and service!=run, (service,run)
st=service.lstat()
assert stat.S_ISDIR(st.st_mode) and (st.st_dev,st.st_ino)==(holder['directory']['device'],holder['directory']['inode']), holder
assert pathlib.Path(holder['socket_path'])==service/'m.sock' and pathlib.Path(holder['socket_path']).is_socket(), holder
assert json.loads((service/'identity.json').read_text())['holder']==holder, holder
process=pathlib.Path(f"/proc/{holder['pid']}/stat").read_text().rsplit(')',1)[1].split()
assert process[0]!='Z' and int(process[19])==holder['start_time'], holder
pin=pathlib.Path(str(record)+'.ram.lock'); st=pin.lstat(); identity=holder['ram_lock']
assert stat.S_ISREG(st.st_mode) and (st.st_dev,st.st_ino)==(identity['device'],identity['inode']), holder
assert len(pin.read_bytes())==32 and pin.read_bytes().hex()==identity['nonce'], holder
if sys.argv[6]=='initial':
    baseline.write_text(json.dumps(r))
PY_PENDING_MEMORY
}
start() {
    local label=$1 path=$2
    shift 2
    CURRENT_PATH=$path
    local attempt attempts=1 deadline=$((SECONDS + 30)) log_label status
    if [[ "$label" = resumed || "$label" = branched ]]; then attempts=8; fi
    for attempt in $(seq 1 "$attempts"); do
        log_label=$label
        if [[ "$label" = resumed || "$label" = branched ]]; then
            [ "$SECONDS" -lt "$deadline" ] || e2e_fail "first RAM restore exceeded its injected-ENOSPC deadline"
            check_pending_memory retry
            log_label="$label-attempt-$attempt"
        fi
        timeout -k 10s 240 "$BIN/sandbox-ctl" run --sandbox-id "$SID" --path-id "$path" \
            --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" \
            --manifest-config "$WORK/publish.yaml" \
            --ref-location "fixture=file://$FIXTURE_DIRECTORY" \
            "$@" >"$OUT/$log_label.log" 2>&1 &
        RUN_PID=$!
        RUN_PIDS+=("$RUN_PID")
        for _ in $(seq 1 90); do
            if timeout -k 2s 5 "$BIN/sandbox-ctl" exec --path-id "$path" --run-root "$RUN_ROOT" \
                -- /bin/cat /memory-status >"$OUT/$log_label.status.json" 2>/dev/null; then
                if [[ "$label" = resumed || "$label" = branched ]]; then
                    [ "$SECONDS" -lt "$deadline" ] || e2e_fail "first RAM restore exceeded its injected-ENOSPC deadline"
                    check_pending_memory ready
                    ln "$OUT/$log_label.log" "$OUT/$label.log"
                    cp "$OUT/$log_label.status.json" "$OUT/$label.status.json"
                fi
                return
            fi
            if ! kill -0 "$RUN_PID" 2>/dev/null; then
                if wait "$RUN_PID"; then status=0; else status=$?; fi
                unset 'RUN_PIDS[-1]'
                printf '%s\n' "$status" >"$OUT/$log_label.exit"
                tail -100 "$OUT/$log_label.log" >&2
                if [[ "$label" = resumed || "$label" = branched ]] && [ "$status" -eq 1 ] && \
                    owner_enospc "$OUT/$log_label.log"; then
                    check_pending_memory retry
                    if [ "$attempt" -lt "$attempts" ] && [ "$SECONDS" -lt "$deadline" ]; then
                        echo "retrying first RAM restore after injected owner ENOSPC (attempt $attempt)" >&2
                        sleep .2
                        continue 2
                    fi
                fi
                e2e_fail "$label exited before readiness (attempt $attempt, status $status)"
            fi
            if [[ "$label" = resumed || "$label" = branched ]] && [ "$SECONDS" -ge "$deadline" ]; then
                e2e_fail "first RAM restore exceeded its injected-ENOSPC deadline"
            fi
            sleep .2
        done
        e2e_fail "$label did not become executable"
    done
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
    result = subprocess.run(sys.argv[4:], stdout=out, stderr=err, timeout=30)
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


# A whole-filesystem fault also affects the next snapshot's metadata. Once a
# holder has actually encountered ENOSPC, retain a payload-only failure using
# its soft RLIMIT_FSIZE. Small CaptureRecord/identity writes remain possible.
# Limit only the exact retired holder; never its live successor or hard limit.
hold_payload_failure() {
    local id=$1 log=$2
    python3 - "$CP_DIR" "$OUT/$id-record.json" "$log" "$OUT/$id-file-limit.json" <<'PY_PAYLOAD_LIMIT'
import json,pathlib,resource,sys,time
root,record,log,proof=map(pathlib.Path,sys.argv[1:])
r=json.loads(record.read_text());holder=r['holder']
assert r['committed'] and r['runtime_retired'] and not r['file_ready'] and not r['ram_released'],r
def identity():
    parts=pathlib.Path(f"/proc/{holder['pid']}/stat").read_text().rsplit(')',1)[1].split()
    assert parts[0]!='Z' and int(parts[19])==holder['start_time'],holder
deadline=time.monotonic()+30
while not any('checkpoint memory save retained for retry:' in line and 'no space left on device' in line for line in log.read_text().splitlines()):
    identity()
    assert time.monotonic()<deadline,'holder did not encounter the injected ENOSPC'
    time.sleep(.02)
identity()
old=resource.prlimit(holder['pid'],resource.RLIMIT_FSIZE)
limit=1024*1024
assert old[0]==resource.RLIM_INFINITY or old[0]>limit,old
resource.prlimit(holder['pid'],resource.RLIMIT_FSIZE,(limit,old[1]))
proof.write_text(json.dumps({'holder':holder,'previous':old,'soft_limit':limit}))
# An already open large partial file is discarded after the failed write.
# Observe reclaimed capacity before the next producer can publish metadata.
import os
while os.statvfs(root).f_bavail*os.statvfs(root).f_frsize<32*1024*1024:
    identity()
    assert time.monotonic()<deadline,'holder did not free the failed partial payload'
    time.sleep(.02)
print('retired holder payload bounded after real ENOSPC:',r['checkpoint_id'],flush=True)
PY_PAYLOAD_LIMIT
}
release_payload_failure() {
    python3 - "$OUT/$1-file-limit.json" <<'PY_PAYLOAD_RELEASE'
import json,pathlib,resource,sys
p=json.load(open(sys.argv[1]));h=p['holder']
parts=pathlib.Path(f"/proc/{h['pid']}/stat").read_text().rsplit(')',1)[1].split()
assert parts[0]!='Z' and int(parts[19])==h['start_time'],h
assert resource.prlimit(h['pid'],resource.RLIMIT_FSIZE)==(p['soft_limit'],p['previous'][1]),p
resource.prlimit(h['pid'],resource.RLIMIT_FSIZE,tuple(p['previous']))
PY_PAYLOAD_RELEASE
}

start cold "$PATH_ID" --config "$WORK/cold.yaml" --enable-checkpoint
FIRST_RUN=$RUN_PID
capture first "$FIRST_FORMAT"
S_FIRST="$(snapshot_path first)"
# Success must precede this round's payload publication, but follow retirement.
"$BIN/sandbox-ctl" info --json --manifest-config "$WORK/publish.yaml" \
    --ref-location "fixture=file://$FIXTURE_DIRECTORY" "$S_FIRST" >"$OUT/first-info.json"
check_pending_memory initial
hold_payload_failure first "$OUT/cold.log"

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
# The second pause creates a second holder while the first still cannot save.
capture second "$SECOND_FORMAT" --no-wait-for-previous-memory-save
S_SECOND="$(snapshot_path second)"
python3 - "$CP_DIR" "$OUT/first-record.json" "$OUT/second-record.json" "$OUT/second.json" "$OUT/keep-second.json" "$FIXTURE_DIRECTORY" <<'PY'
import hashlib,json,pathlib,sys
root=pathlib.Path(sys.argv[1]); loc=json.loads((root/'current-memory.json').read_text())
r=json.loads((root/'memory'/hashlib.sha256(loc['checkpoint_id'].encode()).hexdigest()/(loc['memory_id']+'.json')).read_text())
first=json.load(open(sys.argv[2]));assert r['checkpoint_id']=='second' and r['memory_id']!=first['memory_id'],r
for record in (r,json.loads((root/'memory'/hashlib.sha256(first['checkpoint_id'].encode()).hexdigest()/(first['memory_id']+'.json')).read_text())):
    assert record['committed'] and record['runtime_retired'] and not record['file_ready'] and not record['ram_released'],record
json.dump(r,open(sys.argv[3],'w'))
result=json.load(open(sys.argv[4]))
json.dump({'retain':[{'snapshot_ref':result['snapshotRef'],'sandbox_ref':result['sandboxRef']}], 'ref_locations':{'fixture':sys.argv[6]}},open(sys.argv[5],'w'))
PY
hold_payload_failure second "$OUT/resumed.log"
FIRST_MEMORY="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["memory_id"])' "$OUT/first-record.json")"
SECOND_MEMORY="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["memory_id"])' "$OUT/second-record.json")"
# Removing a parent backend is forbidden while the child is a durable root.
if "$BIN/sandbox-ctl" checkpoint delete --checkpoint-dir "$CP_DIR" --producer-id "$SID" \
    --checkpoint first --memory-id "$FIRST_MEMORY" --manifest-config "$WORK/publish.yaml" \
    --sources "$(cat "$OUT/keep-second.json")" >"$OUT/referenced-delete.json" 2>"$OUT/referenced-delete.stderr"; then
    e2e_fail "referenced parent was revoked"
fi
assert_contains "$OUT/referenced-delete.stderr" "durable source"

# Restore an older branch while both obligations exist. The current locator
# names second, but the selected source is first; neither selects the RAM ledger.
start branched "$PATH_ID" --config "$WORK/restore.yaml" --restore "$S_FIRST" --enable-checkpoint
THIRD_RUN=$RUN_PID
check_memory branched
python3 - "$RUN_ROOT/$PATH_ID" "$SID" "$OUT/branched-run-identity.json" <<'PY'
import json,pathlib,sys
run=pathlib.Path(sys.argv[1]);pid=run/(sys.argv[2]+'.pid');a,b=run.lstat(),pid.lstat()
json.dump({'run':[a.st_dev,a.st_ino],'pid_file':[b.st_dev,b.st_ino],'pid':pid.read_text()},open(sys.argv[3],'w'))
PY
"$BIN/sandbox-ctl" checkpoint list --path-id "$PATH_ID" --run-root "$RUN_ROOT" --json >"$OUT/versions-before-delete.json"
python3 - "$OUT/versions-before-delete.json" <<'PY'
import json,sys
assert {r['id'] for r in json.load(open(sys.argv[1])) if r.get('memory_id')}=={'first','second'}
PY
# Second has no remaining root on this branch. Its cancellation does not need
# directory EX, so first's holder and the running third instance cannot block it.
"$BIN/sandbox-ctl" checkpoint delete --path-id "$PATH_ID" --run-root "$RUN_ROOT" \
    --checkpoint second --memory-id "$SECOND_MEMORY" --sources '{"retain":[]}' --json >"$OUT/deleted-second.json"
wait "$SECOND_RUN"
check_pending_memory ready
python3 - "$CP_DIR" "$OUT/second-record.json" "$RUN_ROOT/$PATH_ID" "$SID" "$OUT/branched-run-identity.json" <<'PY'
import hashlib,json,os,pathlib,sys
root=pathlib.Path(sys.argv[1]);old=json.load(open(sys.argv[2]))
r=json.loads((root/'memory'/hashlib.sha256(old['checkpoint_id'].encode()).hexdigest()/(old['memory_id']+'.json')).read_text())
assert r['deleted'] and r['ram_released'] and r['discarded'] and not r['file_ready'],r
assert not os.path.lexists(r['holder']['directory']['path']),r
run=pathlib.Path(sys.argv[3]);pid=run/(sys.argv[4]+'.pid');expected=json.load(open(sys.argv[5]));a,b=run.lstat(),pid.lstat()
assert [a.st_dev,a.st_ino]==expected['run'] and [b.st_dev,b.st_ino]==expected['pid_file'] and pid.read_text()==expected['pid'],expected
assert (run/'ctl.sock').is_socket()
PY
check_memory branched
# The two pending holders and independent cancellation were observed above.
# Release the fault before capturing the materialized branch: preserving its
# disk lower artifacts is unrelated to the frozen-memory obligation test.
mount -o remount,size=512m "$CP_DIR"
release_payload_failure first
wait "$FIRST_RUN"
capture third "$SECOND_FORMAT"
S_THIRD="$(snapshot_path third)"
wait "$THIRD_RUN"
python3 - "$CP_DIR" <<'PY'
import json,pathlib,sys
records={r['checkpoint_id']:r for r in (json.loads(p.read_text()) for p in (pathlib.Path(sys.argv[1])/'memory').glob('*/*.json'))}
assert set(records)=={'first','second','third'},records
assert len({r['memory_id'] for r in records.values()})==3,records
assert records['second']['deleted'] and records['second']['ram_released'],records['second']
PY
python3 - "$CP_DIR" "$FIRST_FORMAT" "$SECOND_FORMAT" <<'PY'
import json,pathlib,sys,tarfile,zipfile
root=pathlib.Path(sys.argv[1]);records={r['checkpoint_id']:r for r in (json.loads(p.read_text()) for p in (root/'memory').glob('*/*.json'))}
for checkpoint,mode in zip(('first','third'),sys.argv[2:4]):
    r=records[checkpoint]
    assert r['runtime_retired'] and r['file_ready'] and r['ram_released'] and not r.get('deleted'),r
    assert not pathlib.Path(r['holder']['socket_path']).exists(),r
    ref=r['payload_ref'];assert ref.startswith('file://'),ref
    name=ref[7:].split('@',1)[0];assert pathlib.Path(name).name==name,ref
    if mode=='bundle':assert '@manifest:' in ref and zipfile.is_zipfile(root/name),ref
    else:assert '@digest:' in ref and tarfile.is_tarfile(root/name),ref
PY
start next-resume "$PATH_ID" --config "$WORK/restore.yaml" --restore "$S_THIRD" --enable-checkpoint
check_memory next-resume
kill -TERM "$RUN_PID"
wait "$RUN_PID" || true
PORTABLE_REF="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["snapshotRef"])' "$OUT/published.json")"
# Removing the dedicated publication mount makes every local capture
# unavailable, so the published snapshot must restore independently.
umount "$CP_DIR/diffs"
umount "$CP_DIR"
start portable "portable-$PATH_ID" --config "$WORK/restore.yaml" --restore "$PORTABLE_REF" \
    --manifest-config "$WORK/publish.yaml" --ref-location "portable=file://$WORK/published"
check_memory portable
kill -TERM "$RUN_PID"
wait "$RUN_PID" || true
echo "PASS snapshot.checkpoint-memory.sh ($FIRST_FORMAT -> $SECOND_FORMAT)"
