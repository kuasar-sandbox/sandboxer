#!/usr/bin/env bash
# Remote read recovery through real CH/KVM, memory/UFFD, COW, and snapshot capture.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/readiness_helpers.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker grep mkfs.ext4 mv openssl pgrep python3 truncate wc; do
    require_command "$command"
done
for binary in sandbox-ctl cloud-hypervisor flatten-ctl manifest-ctl store-ctl cache-ctl; do
    require_binary "$binary"
done
for file in sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -f "$SANDBOXER_LIB/read_fault_proxy.py" ] || e2e_fail "missing prepared read_fault_proxy.py"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/run" "$WORK/base"
SESSIONS=()
cleanup() {
    local status=$?
    set +e
    for sid in "${SESSIONS[@]}"; do readiness_kill_session TERM "$sid"; done
    for pid in $(jobs -pr); do kill -TERM "$pid" 2>/dev/null || true; done
    sleep 1
    for sid in "${SESSIONS[@]}"; do readiness_kill_session KILL "$sid"; done
    for pid in $(jobs -pr); do kill -KILL "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; done
    exit "$status"
}
trap cleanup EXIT

free_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}
wait_file() {
    local path=$1 pattern=$2 pid=$3
    for _ in $(seq 1 1200); do
        grep -qE "$pattern" "$path" 2>/dev/null && return
        kill -0 "$pid" 2>/dev/null || { cat "$path" >&2; return 1; }
        sleep .05
    done
    echo "timed out: $pattern in $path" >&2
    cat "$path" >&2
    return 1
}
fault_count() {
    if [ -f "$WORK/faults.jsonl" ]; then wc -l <"$WORK/faults.jsonl"; else printf '0\n'; fi
}
wait_fault_count() {
    local before=$1 count=$2 pid=$3 target\n    target=$((before + count))
    for _ in $(seq 1 400); do
        [ "$(fault_count)" -ge "$target" ] && return
        kill -0 "$pid" 2>/dev/null || return 1
        sleep .05
    done
    echo "source failures did not reach $target records (started at $before)" >&2
    return 1
}
wait_new_fault() { wait_fault_count "$1" 1 "$2"; }
wait_file_count() {
    local path=$1 pattern=$2 before=$3 pid=$4 count
    for _ in $(seq 1 200); do
        count=$(grep -cE "$pattern" "$path" 2>/dev/null || true)
        [ "$count" -gt "$before" ] && return
        kill -0 "$pid" 2>/dev/null || { cat "$path" >&2; return 1; }
        sleep .05
    done
    echo "timed out waiting for a new $pattern in $path" >&2
    cat "$path" >&2
    return 1
}
wait_cache_serving() {
    local pid=$1
    for _ in $(seq 1 1200); do
        if "$BIN/cache-ctl" ping --endpoint "127.0.0.1:$HEALTH_PORT" >"$WORK/cache-health" 2>&1 \
            && grep -q SERVING "$WORK/cache-health"; then
            return
        fi
        kill -0 "$pid" 2>/dev/null || {
            cat "$WORK/cache.log" >&2
            return 1
        }
        sleep .05
    done
    cat "$WORK/cache-health" "$WORK/cache.log" >&2
    return 1
}
connect_failure_count() { grep -c '"event": "connect-failure"' "$WORK/faults.jsonl" 2>/dev/null || true; }
wait_connect_failures() {
    local before=$1 count=$2 pid=$3 target\n    target=$((before + count))
    for _ in $(seq 1 400); do
        [ "$(connect_failure_count)" -ge "$target" ] && return
        kill -0 "$pid" 2>/dev/null || return 1
        sleep .05
    done
    echo "cache connect failures did not reach $target records (started at $before)" >&2
    return 1
}
mode() {
    printf '%s\n' "$1" >"$WORK/mode.next"
    mv "$WORK/mode.next" "$WORK/mode"
}

STORE_PORT=$(free_port)
CACHE_PORT=$(free_port)
HEALTH_PORT=$(free_port)
cat >"$WORK/store.yaml" <<YAML
listen: 127.0.0.1:$STORE_PORT
backend: fs
fs: {root: $WORK/store, verify_content_key: true}
YAML
"$BIN/store-ctl" init --config "$WORK/store.yaml" --generation G1
"$BIN/store-ctl" serve --config "$WORK/store.yaml" >"$WORK/store.log" 2>&1 &
STORE_PID=$!

cat >"$WORK/cache.yaml" <<YAML
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
"$BIN/cache-ctl" serve --config "$WORK/cache.yaml" >"$WORK/cache.log" 2>&1 &
CACHE_PID=$!
wait_cache_serving "$CACHE_PID"
mode healthy
python3 "$SANDBOXER_LIB/read_fault_proxy.py" "$WORK/proxy.sock" "$CACHE_PORT" "$WORK/mode" "$WORK/faults.jsonl" >"$WORK/proxy.log" 2>&1 &
PROXY_PID=$!
for _ in $(seq 1 100); do [ -S "$WORK/proxy.sock" ] && break; sleep .05; done
[ -S "$WORK/proxy.sock" ] || e2e_fail "read-fault proxy did not create its socket"

KEY=$(openssl rand -hex 32)
cat >"$WORK/manifest.yaml" <<YAML
manifest: {key: "$KEY"}
store: {endpoint: 127.0.0.1:$STORE_PORT, pool: 4, timeout: 10s}
cache: {endpoint: $WORK/proxy.sock, pool: 4, timeout: 1s}
chunker: {mode: fixed, fixed: {size: 64KiB}}
crypto: {chunk: aes, manifest: aes}
YAML

docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$WORK/root.img" --no-progress
ROOT=$("$BIN/manifest-ctl" store --manifest-config "$WORK/manifest.yaml" --no-progress "$WORK/root.img")
[ "${#ROOT}" -eq 64 ] || e2e_fail "root manifest key is not 64 hex characters"

python3 - "$WORK" <<'PY'
import json, pathlib, sys
w = pathlib.Path(sys.argv[1])
(w/'chunks.before.json').write_text(json.dumps(sorted(p.name for p in (w/'store/chunk').rglob('*') if p.is_file())))
(w/'cow-data').mkdir()
with (w/'cow-data/payload').open('wb') as f:
    for i in range(512):
        f.write(bytes([i % 251]) * 4096)
PY
truncate -s 64M "$WORK/cow.raw"
mkfs.ext4 -q -F -O ^has_journal -b 4096 -d "$WORK/cow-data" "$WORK/cow.raw"
"$BIN/flatten-ctl" tar stream -f "$WORK/cow.img" "$WORK/cow.raw"
COW_ROOT=$("$BIN/manifest-ctl" store --manifest-config "$WORK/manifest.yaml" --no-progress "$WORK/cow.img")
python3 - "$WORK" <<'PY'
import json, pathlib, sys
w = pathlib.Path(sys.argv[1])
before = set(json.loads((w/'chunks.before.json').read_text()))
keys = sorted({p.name for p in (w/'store/chunk').rglob('*') if p.is_file()} - before)
assert keys and all(len(k) == 64 for k in keys)
(w/'cow.keys').write_text(','.join(keys))
PY

cat >"$WORK/workload.py" <<'PY'
import mmap, os, time
ram = bytearray(b'R' * (32 << 20))
fd = os.open('/recovery.data', os.O_CREAT | os.O_RDWR, 0o600)
for i in range(256):
    os.pwrite(fd, bytes([i % 251]) * 4096, i * 4096)
os.fsync(fd)
os.close(fd)
fd = os.open('/recovery.data', os.O_RDWR | os.O_DIRECT)
buf = mmap.mmap(-1, 4096)
i = 0
print('RECOVERY-READY', flush=True)
while True:
    if os.path.exists('/recovery.idle'):
        print('RECOVERY-IDLE', flush=True)
        while os.path.exists('/recovery.idle'):
            time.sleep(.01)
        print('RECOVERY-RESUMED', flush=True)
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
truncate -s 1G "$WORK/seed.diff"
mkfs.ext4 -q -F -O ^has_journal "$WORK/seed.diff"
python3 - "$WORK" "$ROOT" "$BIN" <<'PY'
import json, pathlib, sys
w, root, binpath = sys.argv[1:]
script = pathlib.Path(w, 'workload.py').read_text()
pathlib.Path(w, 'sandbox.yaml').write_text('''resources:
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
''' % (binpath, binpath, root, w, json.dumps(['-c', script])))
PY

launch() {
    local sid=$1
    shift
    local ready="$WORK/$sid.ready"
    readiness_begin_capture "$ready"
    local ready_reader=$READY_READER_PID
    readiness_exec_in_new_session "$BIN/sandbox-ctl" run --ready-fd="$READY_WRITE_FD" "$@" \
        --manifest-config "$WORK/manifest.yaml" --ch-binary "$BIN/cloud-hypervisor" \
        --sandbox-id "$sid" --run-root "$WORK/run" --base-root "$WORK/base" \
        --stats-json "$WORK/$sid.stats.json" >"$WORK/$sid.log" 2>&1 &
    RUN_PID=$!
    SESSIONS+=("$RUN_PID")
    readiness_close_parent_writer
    readiness_wait_event "$ready" 1 control_ready "$RUN_PID" || return 1
    readiness_wait_event "$ready" 2 ready "$RUN_PID" || return 1
    readiness_assert_wire "$ready" "$ready_reader" $'control_ready\nready\n'
}

python3 - "$WORK" "$ROOT" "$COW_ROOT" "$BIN" <<'PY'
import json, pathlib, sys
w, root, cow, binpath = sys.argv[1:]
script = """import mmap, os, time
fd=os.open('/data/payload', os.O_RDWR | os.O_DIRECT)
buf=mmap.mmap(-1,4096)
assert os.preadv(fd,[buf],0)==4096 and buf[:]==bytes(4096)
buf[:512]=b'W'*512
print('COW-ARMED',flush=True)
while not os.path.exists('/cow.begin'): time.sleep(.01)
print('COW-BEGIN',flush=True)
assert os.pwritev(fd,[memoryview(buf)[:512]],512<<10)==512
assert os.preadv(fd,[buf],512<<10)==4096
assert buf[:512]==b'W'*512 and buf[512:]==bytes([128])*3584
os.fsync(fd)
print('COW-RECOVERED',flush=True)
print('DISK-READ-ARMED',flush=True)
while not os.path.exists('/disk-read.begin'): time.sleep(.01)
print('DISK-READ-BEGIN',flush=True)
assert os.preadv(fd,[buf],1<<20)==4096 and buf[:]==bytes([5])*4096
print('DISK-READ-RECOVERED',flush=True)
while True: time.sleep(1)
"""
pathlib.Path(w, 'cow.yaml').write_text('''resources:
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
''' % (binpath, binpath, root, w, cow, json.dumps(['-c', script])))
PY

launch cow --config "$WORK/cow.yaml"
wait_file "$WORK/cow.log" '^COW-ARMED$' "$RUN_PID"
"$BIN/sandbox-ctl" exec --sandbox-id cow --run-root "$WORK/run" -- /bin/true
COW_FAULT_BASE=$(fault_count)
mode "cow:$(cat "$WORK/cow.keys")"
"$BIN/sandbox-ctl" exec --sandbox-id cow --run-root "$WORK/run" -- touch /cow.begin
wait_file "$WORK/cow.log" '^COW-BEGIN$' "$RUN_PID"
wait_fault_count "$COW_FAULT_BASE" 4 "$RUN_PID"
kill -0 "$RUN_PID"
if grep -q '^COW-RECOVERED$' "$WORK/cow.log"; then
    e2e_fail "COW write completed while its base source was unavailable"
fi
mode healthy
wait_file "$WORK/cow.log" '^DISK-READ-ARMED$' "$RUN_PID"
DISK_FAULT_BASE=$(fault_count)
mode "disk-read:$(cat "$WORK/cow.keys")"
"$BIN/sandbox-ctl" exec --sandbox-id cow --run-root "$WORK/run" -- touch /disk-read.begin
wait_file "$WORK/cow.log" '^DISK-READ-BEGIN$' "$RUN_PID"
wait_fault_count "$DISK_FAULT_BASE" 4 "$RUN_PID"
kill -0 "$RUN_PID"
if grep -q '^DISK-READ-RECOVERED$' "$WORK/cow.log"; then
    e2e_fail "disk read completed while its source was unavailable"
fi
mode healthy
wait_file "$WORK/cow.log" '^DISK-READ-RECOVERED$' "$RUN_PID"
kill -TERM "$RUN_PID"
wait "$RUN_PID"
SESSIONS=()
python3 - "$WORK/cow.stats.json" "$WORK/faults.jsonl" "$WORK/cow.keys" <<'PY'
import json, pathlib, sys
d = json.loads(pathlib.Path(sys.argv[1]).read_text())
b = next(b for b in d['backends'] if b['name'] == 'blk2')
assert b['write']['count'] > 0, 'COW write path was not exercised'
assert b['read']['count'] > 0, 'ordinary disk read path was not exercised'
for backend in d['backends']:
    for kind in ('read', 'write', 'flush'):
        assert backend[kind]['err_count'] == 0, (backend['name'], kind)
keys = set(pathlib.Path(sys.argv[3]).read_text().split(','))
faults = [json.loads(line) for line in pathlib.Path(sys.argv[2]).read_text().splitlines()]
assert {f['mode'] for f in faults} == {'cow', 'disk-read'}
assert all(f['key'] in keys for f in faults)
PY

launch seed --config "$WORK/sandbox.yaml"
wait_file "$WORK/seed.log" '^RECOVERY-TICK 20$' "$RUN_PID"
SNAP=$("$BIN/sandbox-ctl" snapshot --sandbox-id seed --upload --run-root "$WORK/run" 2>"$WORK/seed.snapshot.log")
SNAP="${SNAP#manifest://}"
[ "${#SNAP}" -eq 64 ] || e2e_fail "seed snapshot did not return a manifest key"
wait "$RUN_PID"
SESSIONS=()
cat >"$WORK/host.yaml" <<YAML
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
"$BIN/sandbox-ctl" exec --sandbox-id recovered --run-root "$WORK/run" -- sh -c 'touch /recovery.idle'
wait_file "$WORK/recovered.log" '^RECOVERY-IDLE$' "$RUN_PID"
CAPTURE_FAULT_BASE=$(fault_count)
mode offline
"$BIN/sandbox-ctl" snapshot --sandbox-id recovered --upload --run-root "$WORK/run" >"$WORK/next.key" 2>"$WORK/recovered.snapshot.log" &
CAPTURE_PID=$!
wait_new_fault "$CAPTURE_FAULT_BASE" "$CAPTURE_PID"
kill -0 "$RUN_PID"
kill -0 "$CAPTURE_PID"
[ ! -s "$WORK/next.key" ] || e2e_fail "capture completed while its source was offline"

kill -TERM "$CACHE_PID"
wait "$CACHE_PID" || true
RETRY_BASE=$(connect_failure_count)
python3 - "$RUN_PID" "$PROXY_PID" "$WORK/resources.before.json" <<'PY'
import json, pathlib, sys
out = {}
for pid in sys.argv[1:3]:
    p = pathlib.Path('/proc', pid)
    out[pid] = {
        'fds': len(list((p/'fd').iterdir())),
        'threads': len(list((p/'task').iterdir())),
        'rss': next(x for x in (p/'status').read_text().splitlines() if x.startswith('VmRSS:')),
    }
pathlib.Path(sys.argv[3]).write_text(json.dumps(out))
PY
wait_connect_failures "$RETRY_BASE" 12 "$CAPTURE_PID"
kill -0 "$RUN_PID"
kill -0 "$CAPTURE_PID"
[ ! -s "$WORK/next.key" ] || e2e_fail "capture completed during prolonged endpoint loss"
python3 - "$WORK/resources.before.json" "$WORK/resources.after.json" <<'PY'
import json, pathlib, sys
before = json.loads(pathlib.Path(sys.argv[1]).read_text())
after = {}
for pid, old in before.items():
    p = pathlib.Path('/proc', pid)
    new = {
        'fds': len(list((p/'fd').iterdir())),
        'threads': len(list((p/'task').iterdir())),
        'rss': next(x for x in (p/'status').read_text().splitlines() if x.startswith('VmRSS:')),
    }
    after[pid] = new
    assert new['fds'] <= old['fds'] + 8 and new['threads'] <= old['threads'] + 8, (old, new)
    assert int(new['rss'].split()[1]) <= int(old['rss'].split()[1]) + 32768, (old, new)
pathlib.Path(sys.argv[2]).write_text(json.dumps(after))
PY
"$BIN/cache-ctl" serve --config "$WORK/cache.yaml" >>"$WORK/cache.log" 2>&1 &
CACHE_PID=$!
wait_cache_serving "$CACHE_PID"
mode healthy
wait "$CAPTURE_PID"
NEXT=$(cat "$WORK/next.key")
NEXT="${NEXT#manifest://}"
[ "${#NEXT}" -eq 64 ] || e2e_fail "recovered capture did not return a manifest key"
wait "$RUN_PID"
SESSIONS=()
python3 - "$WORK/recovered.stats.json" <<'PY'
import json, pathlib, sys
d = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert d['uffd']['source_read_calls'] > 0 and d['uffd']['tail_buffered_data'] > 0
assert d['uffd']['fault_inflight_hwm'] > 0
for b in d['backends']:
    for kind in ('read', 'write', 'flush'):
        assert b[kind]['err_count'] == 0, (b['name'], kind)
PY

launch verified --restore "manifest://$NEXT" --config "$WORK/host.yaml"
VERIFIED_READY=0
for _ in $(seq 1 200); do
    if "$BIN/sandbox-ctl" exec --sandbox-id verified --run-root "$WORK/run" -- rm -f /recovery.idle >"$WORK/verified-ready.log" 2>&1; then
        VERIFIED_READY=1
        break
    fi
    kill -0 "$RUN_PID" 2>/dev/null || {
        cat "$WORK/verified.log" "$WORK/verified-ready.log" >&2
        e2e_fail "verified restore exited before exec readiness"
    }
    sleep .05
done
[ "$VERIFIED_READY" = 1 ] || e2e_fail "verified restore never became exec-ready"
wait_file "$WORK/verified.log" '^RECOVERY-RESUMED$' "$RUN_PID"
wait_file "$WORK/verified.log" '^RECOVERY-TICK [0-9]+$' "$RUN_PID"
"$BIN/sandbox-ctl" exec --sandbox-id verified --run-root "$WORK/run" -- /bin/true
VERIFIED_IDLE_BASE=$(grep -cE '^RECOVERY-IDLE$' "$WORK/verified.log" 2>/dev/null || true)
"$BIN/sandbox-ctl" exec --sandbox-id verified --run-root "$WORK/run" -- touch /recovery.idle
wait_file_count "$WORK/verified.log" '^RECOVERY-IDLE$' "$VERIFIED_IDLE_BASE" "$RUN_PID"

FATAL_FAULT_BASE=$(fault_count)
mode offline
"$BIN/sandbox-ctl" snapshot --sandbox-id verified --upload --run-root "$WORK/run" >"$WORK/fatal.key" 2>"$WORK/fatal.snapshot.log" &
FATAL_CAPTURE_PID=$!
wait_new_fault "$FATAL_FAULT_BASE" "$FATAL_CAPTURE_PID"
kill -0 "$FATAL_CAPTURE_PID"
[ ! -s "$WORK/fatal.key" ] || e2e_fail "fatal capture completed before corruption injection"
mode corrupt
for _ in $(seq 1 200); do
    grep -q '"mode": "corrupt"' "$WORK/faults.jsonl" 2>/dev/null && break
    kill -0 "$FATAL_CAPTURE_PID" 2>/dev/null || break
    sleep .05
done
grep -q '"mode": "corrupt"' "$WORK/faults.jsonl" || e2e_fail "corrupt source response was not exercised"
for _ in $(seq 1 200); do
    kill -0 "$RUN_PID" 2>/dev/null || break
    sleep .05
done
if kill -0 "$RUN_PID" 2>/dev/null; then
    e2e_fail "fatal source corruption did not stop sandbox"
fi
if wait "$RUN_PID"; then
    e2e_fail "fatal sandbox returned success"
fi
if wait "$FATAL_CAPTURE_PID"; then
    e2e_fail "fatal capture returned success"
fi
[ ! -s "$WORK/fatal.key" ] || e2e_fail "fatal capture emitted a successful key"
if pgrep -s "$RUN_PID" -x cloud-hyperviso >/dev/null; then
    e2e_fail "Cloud Hypervisor survived the sandbox fatal exit"
fi
grep -E 'sandbox fatal I/O:.*ciphertext hash mismatch' "$WORK/verified.log" >/dev/null || e2e_fail "runtime did not record injected ciphertext corruption"
if grep -qE 'Traceback|Input/output error' "$WORK/verified.log"; then
    e2e_fail "fatal source failure reached the Guest as an I/O error"
fi

echo "PASS snapshot.read-recovery.sh"
