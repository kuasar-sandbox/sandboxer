#!/usr/bin/env bash
# Mandatory read recovery through real CH/KVM, remote memory and COW layers.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BIN:?BIN must contain the exact assembled source set}"
for b in sandbox-ctl cloud-hypervisor sandbox-runtime.bundle vmlinux flatten-ctl manifest-ctl store-ctl cache-ctl; do
    [ -f "$BIN/$b" ] || { echo "missing exact input: $b" >&2; exit 1; }
done
[ -r /dev/kvm ] && [ -w /dev/kvm ] || { echo "KVM is required" >&2; exit 1; }
if [ "$(id -u)" -ne 0 ]; then exec sudo -nE bash "$0" "$@"; fi
source "$SCRIPT_DIR/lib/readiness_helpers.sh"
WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/e2e-read-recovery-XXXXXX")"
PIDS=()
SESSIONS=()
cleanup() {
    local result=$?
    for sid in "${SESSIONS[@]}"; do readiness_kill_session TERM "$sid"; done
    for pid in $(jobs -pr); do kill -TERM "$pid" 2>/dev/null || true; done
    sleep 1
    for sid in "${SESSIONS[@]}"; do readiness_kill_session KILL "$sid"; done
    for pid in $(jobs -pr); do kill -KILL "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; done
    echo "read-recovery evidence: $WORK (exit=$result)"
    if [ "$result" -eq 0 ] && [ -z "${E2E_KEEP:-}" ]; then rm -rf "$WORK"; fi
}
trap cleanup EXIT
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'; }
wait_file() {
    local path=$1 pattern=$2 pid=$3
    for _ in $(seq 1 1200); do
        if grep -qE "$pattern" "$path" 2>/dev/null; then return; fi
        kill -0 "$pid" 2>/dev/null || { cat "$path" >&2; return 1; }
        sleep .05
    done
    echo "timed out: $pattern in $path" >&2; cat "$path" >&2; return 1
}
wait_new_fault() {
    local before=$1 pid=$2
    for _ in $(seq 1 200); do
        [ "$(wc -l < "$WORK/faults.jsonl")" -gt "$before" ] && return
        kill -0 "$pid" || return 1
        sleep .05
    done
    echo "no new source failure after $before records" >&2; return 1
}
mode() { printf '%s\n' "$1" > "$WORK/mode.next"; mv "$WORK/mode.next" "$WORK/mode"; }
STORE_PORT=$(free_port); CACHE_PORT=$(free_port); HEALTH_PORT=$(free_port)
cat > "$WORK/store.yaml" <<YAML
listen: 127.0.0.1:$STORE_PORT
backend: fs
fs: {root: $WORK/store, verify_content_key: true}
YAML
"$BIN/store-ctl" init --config "$WORK/store.yaml" --generation G1
"$BIN/store-ctl" serve --config "$WORK/store.yaml" > "$WORK/store.log" 2>&1 &
PIDS+=($!)
cat > "$WORK/cache.yaml" <<YAML
mode: tiered
listen: 127.0.0.1:$CACHE_PORT
health_listen: 127.0.0.1:$HEALTH_PORT
rpc_timeout: 2s
freq: {counters: 1M, reset_after: 100K}
tiers:
  - type: embedded
    rocks: {path: $WORK/cache, disk_bytes: 1GiB, mem_ratio: 0.1, direct_reads: false}
origin:
  type: store
  store: {endpoint: 127.0.0.1:$STORE_PORT, pool: 2, timeout: 2s}
YAML
"$BIN/cache-ctl" serve --config "$WORK/cache.yaml" > "$WORK/cache.log" 2>&1 &
CACHE_PID=$!; PIDS+=($CACHE_PID)
for _ in $(seq 1 100); do
    if "$BIN/cache-ctl" ping --endpoint "127.0.0.1:$HEALTH_PORT" > "$WORK/cache-health" 2>&1 && grep -q SERVING "$WORK/cache-health"; then break; fi
    sleep .1
done
grep -q SERVING "$WORK/cache-health"
mode healthy
python3 "$SCRIPT_DIR/lib/read_fault_proxy.py" "$WORK/proxy.sock" "$CACHE_PORT" "$WORK/mode" "$WORK/faults.jsonl" > "$WORK/proxy.log" 2>&1 &
PROXY_PID=$!; PIDS+=($PROXY_PID)
for _ in $(seq 1 100); do [ -S "$WORK/proxy.sock" ] && break; sleep .05; done
[ -S "$WORK/proxy.sock" ]
KEY=$(openssl rand -hex 32)
cat > "$WORK/manifest.yaml" <<YAML
manifest: {key: "$KEY"}
store: {endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 10s}
cache: {endpoint: $WORK/proxy.sock, pool: 4, timeout: 1s}
chunker: {mode: fixed, fixed: {size: 64KiB}}
crypto: {chunk: aes, manifest: aes}
YAML
IMAGE="${IMAGE:-python:3.12-slim}"
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then docker pull "$IMAGE" > "$WORK/image-pull.log" 2>&1; fi
docker image inspect "$IMAGE" --format 'Guest image: {{.Id}} {{json .RepoDigests}}'
docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$WORK/root.img" --no-progress
ROOT=$("$BIN/manifest-ctl" store --manifest-config "$WORK/manifest.yaml" --no-progress "$WORK/root.img")
[ "${#ROOT}" -eq 64 ]
# A separate data disk isolates the COW and ordinary disk read faults. Its
# payload's middle blocks are untouched by startup and the warm-up read.
python3 - "$WORK" <<'PY'
import json,pathlib,sys
w=pathlib.Path(sys.argv[1])
(w/'chunks.before.json').write_text(json.dumps(sorted(p.name for p in (w/'store/chunk').rglob('*') if p.is_file())))
(w/'cow-data').mkdir()
with (w/'cow-data/payload').open('wb') as f:
 for i in range(512): f.write(bytes([i % 251]) * 4096)
PY
truncate -s 64M "$WORK/cow.raw"
mkfs.ext4 -q -F -d "$WORK/cow-data" "$WORK/cow.raw"
"$BIN/flatten-ctl" tar stream -f "$WORK/cow.img" "$WORK/cow.raw"
COW_ROOT=$("$BIN/manifest-ctl" store --manifest-config "$WORK/manifest.yaml" --no-progress "$WORK/cow.img")
python3 - "$WORK" <<'PY'
import json,pathlib,sys
w=pathlib.Path(sys.argv[1]);before=set(json.loads((w/'chunks.before.json').read_text()))
keys=sorted({p.name for p in (w/'store/chunk').rglob('*') if p.is_file()}-before)
assert keys and all(len(k)==64 for k in keys)
(w/'cow.keys').write_text(','.join(keys))
PY
# The Guest touches captured anonymous pages and partially updates captured
# disk blocks. Direct I/O forces post-restore COW materialization from source.
cat > "$WORK/workload.py" <<'PY'
import mmap, os, time
ram = bytearray(b'R' * (32 << 20))
fd = os.open('/recovery.data', os.O_CREAT | os.O_RDWR, 0o600)
for i in range(256): os.pwrite(fd, bytes([i % 251]) * 4096, i * 4096)
os.fsync(fd)
os.close(fd)
fd = os.open('/recovery.data', os.O_RDWR | os.O_DIRECT)
buf = mmap.mmap(-1, 4096)
i = 0
print('RECOVERY-READY', flush=True)
while True:
    page = i % 256
    assert ram[(i * 4096) % len(ram)] == ord('R')
    buf[:512] = b'W' * 512
    os.pwritev(fd, [memoryview(buf)[:512]], page * 4096)
    os.preadv(fd, [buf], page * 4096)
    assert buf[:512] == b'W' * 512
    assert buf[512:] == bytes([page % 251]) * (4096 - 512)
    os.fsync(fd)
    print('RECOVERY-TICK', i, flush=True)
    i += 1
    time.sleep(.02)
PY
# Keep the workload in the existing launch specification captured by Sandbox E.
truncate -s 1G "$WORK/seed.diff"
mkfs.ext4 -q -F "$WORK/seed.diff"
python3 - "$WORK" "$ROOT" "$BIN" <<'PY'
import json,pathlib,sys
w,root,binpath=sys.argv[1:]
script=pathlib.Path(w,'workload.py').read_text()
pathlib.Path(w,'sandbox.yaml').write_text('''resources:
  capacity: {cpu: 1, memory: 512MiB}
  allocatable: {cpu: 1, memory: 512MiB}
boot:
  kernel: file://%s/vmlinux
  runtime: file://%s/sandbox-runtime.bundle
  root:
    base: manifest://%s
    overlay: {diff: file://%s/seed.diff}
launch:
  exec: /usr/local/bin/python3
  args: %s
  restart: never
''' % (binpath,binpath,root,w,json.dumps(['-c',script])))
PY
mkdir -p "$WORK/run" "$WORK/base"
launch() {
    local sid=$1
    shift
    local ready="$WORK/$sid.ready"
    readiness_begin_capture "$ready"
    local ready_reader=$READY_READER_PID
    readiness_exec_in_new_session "$BIN/sandbox-ctl" run --ready-fd="$READY_WRITE_FD" "$@" \
        --manifest-config "$WORK/manifest.yaml" --ch-binary "$BIN/cloud-hypervisor" \
        --sandbox-id "$sid" --run-root "$WORK/run" --base-root "$WORK/base" \
        --stats-json "$WORK/$sid.stats.json" > "$WORK/$sid.log" 2>&1 &
    RUN_PID=$!; SESSIONS+=($RUN_PID)
    readiness_close_parent_writer
    # Restored stdout can contain buffered pre-snapshot TICKs. Only the
    # one-shot readiness descriptor establishes completion of resume/quiesce.
    readiness_wait_event "$ready" 1 control_ready "$RUN_PID" || return 1
    readiness_wait_event "$ready" 2 ready "$RUN_PID" || return 1
    readiness_assert_wire "$ready" "$ready_reader" $'control_ready\nready\n'
}
# Only the dedicated data disk's immutable chunks fail in this phase. Root
# reads and UFFD remain available, so neither can delay the Guest before the
# first partial write reaches the COW base materialization boundary.
python3 - "$WORK" "$ROOT" "$COW_ROOT" "$BIN" <<'PY'
import json,pathlib,sys
w,root,cow,binpath=sys.argv[1:]
script="""import mmap, os, time
fd=os.open('/data/payload', os.O_RDWR | os.O_DIRECT)
buf=mmap.mmap(-1,4096)
assert os.preadv(fd,[buf],0)==4096 and buf[:]==bytes(4096)
buf[:512]=b'W'*512
print('COW-ARMED',flush=True)
time.sleep(7)
print('COW-BEGIN',flush=True)
assert os.pwritev(fd,[memoryview(buf)[:512]],512<<10)==512
assert os.preadv(fd,[buf],512<<10)==4096
assert buf[:512]==b'W'*512 and buf[512:]==bytes([128])*3584
os.fsync(fd)
print('COW-RECOVERED',flush=True)
print('DISK-READ-ARMED',flush=True)
time.sleep(7)
print('DISK-READ-BEGIN',flush=True)
assert os.preadv(fd,[buf],1<<20)==4096 and buf[:]==bytes([5])*4096
print('DISK-READ-RECOVERED',flush=True)
while True: time.sleep(1)
"""
pathlib.Path(w,'cow.yaml').write_text('''resources:
  capacity: {cpu: 1, memory: 512MiB}
  allocatable: {cpu: 1, memory: 512MiB}
boot:
  kernel: file://%s/vmlinux
  runtime: file://%s/sandbox-runtime.bundle
  root:
    base: manifest://%s
    overlay: {diff_template: file://%s/seed.diff}
  disks:
    - {name: cow, base: manifest://%s}
mounts:
  - {target: /data, type: disk, source: cow}
launch:
  exec: /usr/local/bin/python3
  args: %s
  restart: never
''' % (binpath,binpath,root,w,cow,json.dumps(['-c',script])))
PY
launch cow --config "$WORK/cow.yaml"
wait_file "$WORK/cow.log" '^COW-ARMED$' "$RUN_PID"
"$BIN/sandbox-ctl" exec --sandbox-id cow --run-root "$WORK/run" -- /bin/true
mode "cow:$(cat "$WORK/cow.keys")"
wait_file "$WORK/cow.log" '^COW-BEGIN$' "$RUN_PID"
wait_file "$WORK/faults.jsonl" '"mode": "cow"' "$RUN_PID"
sleep 3
kill -0 "$RUN_PID"
if grep -q '^COW-RECOVERED$' "$WORK/cow.log"; then
    echo "COW write completed while its base source was unavailable" >&2; exit 1
fi
mode healthy
wait_file "$WORK/cow.log" '^DISK-READ-ARMED$' "$RUN_PID"
mode "disk-read:$(cat "$WORK/cow.keys")"
wait_file "$WORK/cow.log" '^DISK-READ-BEGIN$' "$RUN_PID"
wait_file "$WORK/faults.jsonl" '"mode": "disk-read"' "$RUN_PID"
sleep 3
kill -0 "$RUN_PID"
if grep -q '^DISK-READ-RECOVERED$' "$WORK/cow.log"; then
    echo "disk read completed while its source was unavailable" >&2; exit 1
fi
mode healthy
wait_file "$WORK/cow.log" '^DISK-READ-RECOVERED$' "$RUN_PID"
kill -TERM "$RUN_PID"; wait "$RUN_PID"
SESSIONS=()
python3 - "$WORK/cow.stats.json" "$WORK/faults.jsonl" "$WORK/cow.keys" <<'PY'
import json,pathlib,sys
d=json.loads(pathlib.Path(sys.argv[1]).read_text())
b=next(b for b in d['backends'] if b['name']=='blk2')
assert b['write']['lat_max_ns']>3_000_000_000, 'COW write never waited for its implicit base read'
assert b['read']['lat_max_ns']>3_000_000_000, 'ordinary disk read never waited'
for backend in d['backends']:
 for kind in ('read','write','flush'): assert backend[kind]['err_count']==0,(backend['name'],kind)
keys=set(pathlib.Path(sys.argv[3]).read_text().split(','))
faults=[json.loads(line) for line in pathlib.Path(sys.argv[2]).read_text().splitlines()]
assert {f['mode'] for f in faults}=={'cow','disk-read'} and all(f['key'] in keys for f in faults)
print('PASS: isolated real COW partial write and ordinary disk read; unchanged suffix verified; no Guest I/O errors',b['write']['lat_max_ns'],b['read']['lat_max_ns'])
PY
launch seed --config "$WORK/sandbox.yaml"
wait_file "$WORK/seed.log" '^RECOVERY-TICK 20$' "$RUN_PID"
SNAP=$("$BIN/sandbox-ctl" snapshot --sandbox-id seed --upload --run-root "$WORK/run" 2> "$WORK/seed.snapshot.log")
[ "${#SNAP}" -eq 64 ]
wait "$RUN_PID"
SESSIONS=()
cat > "$WORK/host.yaml" <<YAML
resources:
  capacity: {cpu: 1, memory: 512MiB}
  allocatable: {cpu: 1, memory: 512MiB}
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
YAML
launch recovered --restore "manifest://$SNAP" --config "$WORK/host.yaml"
wait_file "$WORK/recovered.log" '^RECOVERY-TICK [0-9]+$' "$RUN_PID"
"$BIN/sandbox-ctl" exec --sandbox-id recovered --run-root "$WORK/run" -- /bin/true
"$BIN/sandbox-ctl" exec --sandbox-id recovered --run-root "$WORK/run" -- sh -c 'echo EXEC-ARMED; sleep 7; echo 3 > /proc/sys/vm/drop_caches; cat /recovery.data >/dev/null; echo READ-RECOVERED' > "$WORK/pending-exec.log" 2>&1 &
EXEC_PID=$!; PIDS+=($EXEC_PID)
wait_file "$WORK/pending-exec.log" '^EXEC-ARMED$' "$RUN_PID"
mode offline
wait_file "$WORK/faults.jsonl" '"mode": "offline"' "$RUN_PID"
# Stop only our cache, retaining its data and original endpoint. The outage
# exceeds the old five-attempt refill backoff window (15.5 seconds).
kill -TERM "$CACHE_PID"; wait "$CACHE_PID"
python3 - "$RUN_PID" "$PROXY_PID" "$WORK/resources.before.json" <<'PY'
import json,pathlib,sys
out={}
for pid in sys.argv[1:3]:
 p=pathlib.Path('/proc',pid)
 out[pid]={'fds':len(list((p/'fd').iterdir())), 'threads':len(list((p/'task').iterdir())), 'rss':next(x for x in (p/'status').read_text().splitlines() if x.startswith('VmRSS:'))}
pathlib.Path(sys.argv[3]).write_text(json.dumps(out))
PY
sleep 18
kill -0 "$RUN_PID"
if grep -q '^READ-RECOVERED$' "$WORK/pending-exec.log"; then
    echo "pending read completed before source recovery" >&2; exit 1
fi
if grep -qE 'Traceback|Input/output error|mandatory source read' "$WORK/recovered.log"; then
    echo "transient source outage caused a Guest error or fatal exit" >&2; exit 1
fi
python3 - "$WORK/resources.before.json" "$WORK/resources.after.json" <<'PY'
import json,pathlib,sys
before=json.loads(pathlib.Path(sys.argv[1]).read_text());after={}
for pid,old in before.items():
 p=pathlib.Path('/proc',pid);new={'fds':len(list((p/'fd').iterdir())), 'threads':len(list((p/'task').iterdir())), 'rss':next(x for x in (p/'status').read_text().splitlines() if x.startswith('VmRSS:'))};after[pid]=new
 assert new['fds']<=old['fds']+8 and new['threads']<=old['threads']+8,(old,new)
 assert int(new['rss'].split()[1])<=int(old['rss'].split()[1])+32768,(old,new)
pathlib.Path(sys.argv[2]).write_text(json.dumps(after))
PY
"$BIN/cache-ctl" serve --config "$WORK/cache.yaml" >> "$WORK/cache.log" 2>&1 &
CACHE_PID=$!; PIDS+=($CACHE_PID)
for _ in $(seq 1 100); do
    if "$BIN/cache-ctl" ping --endpoint "127.0.0.1:$HEALTH_PORT" > "$WORK/cache-health" 2>&1 && grep -q SERVING "$WORK/cache-health"; then break; fi
    sleep .1
done
grep -q SERVING "$WORK/cache-health"
mode healthy
wait_file "$WORK/pending-exec.log" '^READ-RECOVERED$' "$RUN_PID"
wait "$EXEC_PID"
# A capture requiring unavailable pages must wait, then preserve the contents.
BEFORE_OUTAGE_FAULTS=$(wc -l < "$WORK/faults.jsonl")
mode offline
wait_new_fault "$BEFORE_OUTAGE_FAULTS" "$RUN_PID"
BEFORE_CAPTURE_FAULTS=$(wc -l < "$WORK/faults.jsonl")
"$BIN/sandbox-ctl" snapshot --sandbox-id recovered --upload --run-root "$WORK/run" > "$WORK/next.key" 2> "$WORK/recovered.snapshot.log" &
CAPTURE_PID=$!
wait_new_fault "$BEFORE_CAPTURE_FAULTS" "$CAPTURE_PID"
# Stay within the existing eight-second quiesce/drain budget. A source
# retry must delay this operation, not require disabling its health policy.
sleep 1
kill -0 "$RUN_PID"; kill -0 "$CAPTURE_PID"
[ ! -s "$WORK/next.key" ]
mode healthy
wait "$CAPTURE_PID"
NEXT=$(cat "$WORK/next.key")
[ "${#NEXT}" -eq 64 ]; wait "$RUN_PID"
SESSIONS=()
python3 - "$WORK/recovered.stats.json" "$WORK/recovered.log" <<'PY'
import json,pathlib,re,sys
d=json.loads(pathlib.Path(sys.argv[1]).read_text())
assert d['uffd']['source_read_calls']>0 and d['uffd']['tail_buffered_data']>0
assert re.search(r'uffd .*inflight=[1-9]',pathlib.Path(sys.argv[2]).read_text())
for b in d['backends']:
 for kind in ('read','write','flush'): assert b[kind]['err_count']==0,(b['name'],kind)
print('PASS: real UFFD/Chunk window; no Guest I/O errors')
PY
launch verified --restore "manifest://$NEXT" --config "$WORK/host.yaml"
wait_file "$WORK/verified.log" '^RECOVERY-TICK [0-9]+$' "$RUN_PID"
"$BIN/sandbox-ctl" exec --sandbox-id verified --run-root "$WORK/run" -- /bin/true
# A complete corrupt immutable response is a deterministic failure, including
# while a capture is waiting on the runtime's still-required source read.
BEFORE_OUTAGE_FAULTS=$(wc -l < "$WORK/faults.jsonl")
mode offline
wait_new_fault "$BEFORE_OUTAGE_FAULTS" "$RUN_PID"
BEFORE_FATAL_FAULTS=$(wc -l < "$WORK/faults.jsonl")
"$BIN/sandbox-ctl" snapshot --sandbox-id verified --upload --run-root "$WORK/run" > "$WORK/fatal.key" 2> "$WORK/fatal.snapshot.log" &
FATAL_CAPTURE_PID=$!
wait_new_fault "$BEFORE_FATAL_FAULTS" "$FATAL_CAPTURE_PID"
kill -0 "$FATAL_CAPTURE_PID"
[ ! -s "$WORK/fatal.key" ]
mode corrupt
wait_file "$WORK/faults.jsonl" '"mode": "corrupt"' "$RUN_PID"
for _ in $(seq 1 200); do kill -0 "$RUN_PID" 2>/dev/null || break; sleep .05; done
if kill -0 "$RUN_PID" 2>/dev/null; then echo "fatal did not stop sandbox" >&2; exit 1; fi
if wait "$RUN_PID"; then echo "fatal returned success" >&2; exit 1; fi
if wait "$FATAL_CAPTURE_PID"; then echo "fatal capture returned success" >&2; exit 1; fi
[ ! -s "$WORK/fatal.key" ]
if pgrep -s "$RUN_PID" -x cloud-hyperviso >/dev/null; then
    echo "CH survived the sandbox's fatal exit" >&2; exit 1
fi
# The runtime owner must record the injected corruption, not only a timeout.
# Either UFFD or COW can observe the failing immutable source first.
grep -E 'sandbox fatal I/O:.*ciphertext hash mismatch' "$WORK/verified.log"
if grep -qE 'Traceback|Input/output error' "$WORK/verified.log"; then
    echo "fatal source failure reached the Guest as an I/O error" >&2; exit 1
fi
cat "$WORK/faults.jsonl"
echo "PASS: real CH source recovery, recovered snapshot contents, and whole-VM fatal without a successful capture"
