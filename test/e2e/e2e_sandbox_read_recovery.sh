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
WORK="$(mktemp -d /tmp/e2e-read-recovery-XXXXXX)"
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
    readiness_exec_in_new_session "$BIN/sandbox-ctl" run "$@" \
        --manifest-config "$WORK/manifest.yaml" --ch-binary "$BIN/cloud-hypervisor" \
        --sandbox-id "$sid" --run-root "$WORK/run" --base-root "$WORK/base" \
        --stats-json "$WORK/$sid.stats.json" > "$WORK/$sid.log" 2>&1 &
    RUN_PID=$!; SESSIONS+=($RUN_PID)
}
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
! grep -q '^READ-RECOVERED$' "$WORK/pending-exec.log"
! grep -qE 'Traceback|Input/output error|mandatory source read' "$WORK/recovered.log"
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
mode offline
sleep 6
BEFORE_CAPTURE_FAULTS=$(wc -l < "$WORK/faults.jsonl")
"$BIN/sandbox-ctl" snapshot --sandbox-id recovered --upload --run-root "$WORK/run" > "$WORK/next.key" 2> "$WORK/recovered.snapshot.log" &
CAPTURE_PID=$!
for _ in $(seq 1 100); do
    [ "$(wc -l < "$WORK/faults.jsonl")" -gt "$BEFORE_CAPTURE_FAULTS" ] && break
    kill -0 "$CAPTURE_PID"
    sleep .1
done
[ "$(wc -l < "$WORK/faults.jsonl")" -gt "$BEFORE_CAPTURE_FAULTS" ]
sleep 3
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
assert any(b['write']['lat_max_ns']>1_000_000_000 for b in d['backends']), 'COW write never waited for its implicit base read'
for b in d['backends']:
 for kind in ('read','write','flush'): assert b[kind]['err_count']==0,(b['name'],kind)
print('PASS: real UFFD/Chunk window and COW implicit base read; no Guest I/O errors')
PY
launch verified --restore "manifest://$NEXT" --config "$WORK/host.yaml"
wait_file "$WORK/verified.log" '^RECOVERY-TICK [0-9]+$' "$RUN_PID"
"$BIN/sandbox-ctl" exec --sandbox-id verified --run-root "$WORK/run" -- /bin/true
# A complete corrupt immutable response is a deterministic failure.
mode corrupt
sleep 6
"$BIN/sandbox-ctl" exec --sandbox-id verified --run-root "$WORK/run" -- sh -c 'echo 3 > /proc/sys/vm/drop_caches; cat /recovery.data >/dev/null' > "$WORK/fatal-exec.log" 2>&1 &
PIDS+=($!)
wait_file "$WORK/faults.jsonl" '"mode": "corrupt"' "$RUN_PID"
for _ in $(seq 1 200); do kill -0 "$RUN_PID" 2>/dev/null || break; sleep .05; done
if kill -0 "$RUN_PID" 2>/dev/null; then echo "fatal did not stop sandbox" >&2; exit 1; fi
if wait "$RUN_PID"; then echo "fatal returned success" >&2; exit 1; fi
! pgrep -s "$RUN_PID" -x cloud-hyperviso >/dev/null
# The fatal owner must record the source cause, not only a pinger/timeout exit.
grep 'mandatory source read' "$WORK/verified.log"
! grep -qE 'Traceback|Input/output error' "$WORK/verified.log"
cat "$WORK/faults.jsonl"
echo "PASS: real CH source recovery, recovered snapshot contents, and whole-VM fatal"
