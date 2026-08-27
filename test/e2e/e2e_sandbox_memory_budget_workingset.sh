#!/usr/bin/env bash
#
# e2e_sandbox_memory_budget_workingset.sh — real-KVM regression for #114.
#
# The test records the same deterministic rootfs file under four snapshots:
#
#   new-w         C=8GiB, H=256MiB, drop_caches=false, merge_ref=false
#   no-balloon-w  C=8GiB, H=C,      drop_caches=false, merge_ref=false
#   workaround-w  C=8GiB, H=7.5GiB, drop_caches=false, merge_ref=false
#   new-b         same live VM as new-w, drop_caches=true
#
# Every W restore probes mincore before its first content read. The final JSON
# artifact records freeze MemAvailable/Cached, CH target/current, Budget values,
# snapshot resident bytes, rootfs vhost reads/bytes/latency, and UFFD counters.
# No fixed latency threshold is introduced: the read-amplification gate is
# relative to the two controls. Every W case requires full pre-read residency;
# the one-Step shrink deadband offsets memory.high's minimum PressureReserve so
# the configured clean working set is not reclaimed before capture.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=test/e2e/lib/tarstream.sh
. "$SCRIPT_DIR/lib/tarstream.sh"

BIN="${BIN:-$REPO_ROOT/bin}"
IMAGE="${IMAGE:-python:3.12-slim}"
CAPACITY_BYTES=$((8 * 1024 * 1024 * 1024))
HEADROOM_BYTES=$((256 * 1024 * 1024))
MEMORY_STEP_BYTES=$((64 * 1024 * 1024))
# Match #114's failure shape: anonymous demand consumes roughly the old total
# Budget while a separate 256MiB clean file working set must remain available
# to a W snapshot. A cache-only 64MiB smoke can pass the old broken model.
ANON_BYTES="${ANON_WORKING_SET_BYTES:-$((1024 * 1024 * 1024))}"
WARM_BYTES="${WORKING_SET_BYTES:-$((256 * 1024 * 1024))}"
REPORT_SETTLE_SECONDS="${MEM_REPORT_SETTLE_SECONDS:-6}"

skip() {
    echo
    echo "==> e2e_sandbox_memory_budget_workingset: skipping ($*)"
    if [ "${REQUIRE_KVM:-0}" = "1" ]; then
        echo "REQUIRE_KVM=1 set; failing instead of skipping" >&2
        exit 1
    fi
    exit 0
}

[ -e /dev/kvm ] || skip "/dev/kvm not present"
if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
    skip "/dev/kvm not accessible"
fi
for binary in cloud-hypervisor sandbox-ctl sandbox-init sandbox-runtime.bundle flatten-ctl; do
    [ -e "$BIN/$binary" ] || skip "missing $BIN/$binary"
done
VMLINUX="${VMLINUX:-$BIN/vmlinux}"
[ -f "$VMLINUX" ] || skip "no vmlinux at $VMLINUX"
command -v mkfs.ext4 >/dev/null 2>&1 || skip "mkfs.ext4 not on PATH"
command -v python3 >/dev/null 2>&1 || skip "python3 not on PATH"

case "$WARM_BYTES" in
    ''|*[!0-9]*|0) echo "$0: WORKING_SET_BYTES must be a positive integer" >&2; exit 1 ;;
esac
case "$ANON_BYTES" in
    ''|*[!0-9]*|0) echo "$0: ANON_WORKING_SET_BYTES must be a positive integer" >&2; exit 1 ;;
esac
case "$REPORT_SETTLE_SECONDS" in
    ''|*[!0-9]*|0) echo "$0: MEM_REPORT_SETTLE_SECONDS must be a positive integer" >&2; exit 1 ;;
esac
if [ "$(id -u)" -ne 0 ]; then
    exec sudo -nE "$0" "$@"
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/e2e-memory-budget-ws-XXXXXX")"
RUN_ROOT="$WORK/run"
BASE_ROOT="$WORK/base"
RESULT_ROOT="$WORK/results"
CGROUP_ROOT="/sys/fs/cgroup/kuasar-e2e-memory-budget-$$"
mkdir -p "$RUN_ROOT" "$BASE_ROOT" "$RESULT_ROOT"

declare -a SANDBOX_PIDS=()
declare -a CGROUP_LEAVES=()
declare -A SNAPSHOTS=()
declare -A HEADROOMS=()
declare -A CHECKSUMS=()
declare -A RECORDS=()
declare -A RESTORES=()

cleanup() {
    set +e
    for pid in "${SANDBOX_PIDS[@]}"; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
    done
    sleep 1
    for pid in "${SANDBOX_PIDS[@]}"; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
    done
    for leaf in "${CGROUP_LEAVES[@]}"; do
        [ -n "$leaf" ] && rmdir "$leaf" 2>/dev/null
    done
    rmdir "$CGROUP_ROOT" 2>/dev/null
    if [ -n "${E2E_KEEP:-}" ]; then
        echo "kept work dir: $WORK"
    else
        rm -rf "$WORK"
    fi
}
trap cleanup EXIT

for controller in cpu memory; do
    grep -qw "$controller" /sys/fs/cgroup/cgroup.controllers \
        || skip "cgroup v2 $controller controller is unavailable"
done
echo '+cpu +memory' > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
mkdir "$CGROUP_ROOT" 2>/dev/null || skip "cannot create $CGROUP_ROOT"
echo '+cpu +memory' > "$CGROUP_ROOT/cgroup.subtree_control" 2>/dev/null \
    || skip "cannot enable cpu and memory below $CGROUP_ROOT"
for controller in cpu memory; do
    grep -qw "$controller" "$CGROUP_ROOT/cgroup.subtree_control" \
        || skip "$controller is not enabled below $CGROUP_ROOT"
done

new_cgroup() { # $1 = stable leaf label
    local label="$1" leaf="$CGROUP_ROOT/$1"
    mkdir "$leaf"
    if [ ! -f "$leaf/memory.current" ] || [ ! -f "$leaf/memory.high" ]; then
        echo "FAIL: memory controller files missing in $leaf" >&2
        exit 1
    fi
    CGROUP_LEAVES+=("$leaf")
    NEW_CGROUP="$leaf"
}

make_diff() { # $1 = path
    truncate -s 1G "$1"
    mkfs.ext4 -q -F "$1"
}

BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker unavailable; provide BLK0_IMAGE"
    docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null
    BLK0_IMAGE="$WORK/root.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"

write_config() { # $1=path $2=cgroup $3=headroom $4=startup $5=diff
    local path="$1" cgroup="$2" headroom="$3" startup="$4" diff="$5"
    cat > "$path" <<EOF
resources:
  capacity: { cpu: 1, memory: 8GiB }
  allocatable: { cpu: 1, memory: $headroom, deflate_on_oom: true }
  startup: { memory: $startup }
  overhead: { memory: 32MiB }
  watermark_high: { ratio: 0.875 }
  control: { cgroup_path: $cgroup }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay: { diff: file://$diff, size: 1GiB }
launch:
  exec: /usr/local/bin/python3
  args:
    - -c
    - |
        import os
        import sys
        import time

        allocation = bytearray(int(sys.argv[1]))
        for offset in range(0, len(allocation), 4096):
            allocation[offset] = 1
        with open("/run/kuasar-anon-ready", "w", encoding="utf-8") as ready:
            ready.write(str(len(allocation)))
        while True:
            time.sleep(3600)
    - "$ANON_BYTES"
  restart: never
EOF
}

write_restore_config() { # $1=path $2=cgroup $3=headroom $4=diff
    local path="$1" cgroup="$2" headroom="$3" diff="$4"
    cat > "$path" <<EOF
resources:
  capacity: { cpu: 1, memory: 8GiB }
  allocatable: { cpu: 1, memory: $headroom, deflate_on_oom: true }
  overhead: { memory: 32MiB }
  watermark_high: { ratio: 0.875 }
  control: { cgroup_path: $cgroup }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: { diff: file://$diff, size: 1GiB }
EOF
}

ready() { # $1=sid $2=pid $3=log
    local sid="$1" pid="$2" log="$3"
    for _ in $(seq 1 180); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec \
            --sandbox-id "$sid" --run-root "$RUN_ROOT" -- /bin/sh -c 'echo READY' \
            >"$WORK/$sid.ready" 2>/dev/null \
            && grep -qx READY "$WORK/$sid.ready"; then
            return 0
        fi
        if ! kill -0 "$pid" 2>/dev/null; then
            echo "FAIL: $sid exited before ready" >&2
            sed -n '1,240p' "$log" >&2
            return 1
        fi
        sleep 0.5
    done
    echo "FAIL: $sid did not become ready" >&2
    sed -n '1,240p' "$log" >&2
    return 1
}

wait_anon_ready() { # $1=sid $2=pid $3=log
    local sid="$1" pid="$2" log="$3"
    for _ in $(seq 1 180); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec \
            --sandbox-id "$sid" --run-root "$RUN_ROOT" -- \
            /bin/cat /run/kuasar-anon-ready \
            >"$WORK/$sid.anon-ready" 2>/dev/null \
            && grep -qx "$ANON_BYTES" "$WORK/$sid.anon-ready"; then
            return 0
        fi
        if ! kill -0 "$pid" 2>/dev/null; then
            echo "FAIL: $sid exited before anonymous demand was resident" >&2
            sed -n '1,280p' "$log" >&2
            return 1
        fi
        sleep 0.5
    done
    echo "FAIL: $sid did not establish $ANON_BYTES bytes of anonymous demand" >&2
    sed -n '1,280p' "$log" >&2
    return 1
}

read_ch_state() { # $1=sid; prints target currentBudget
    python3 - "$RUN_ROOT/$1/ch.sock" <<'PY'
import http.client
import json
import socket
import sys

class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=2)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)

connection = UnixHTTPConnection(sys.argv[1])
connection.request("GET", "/api/v1/vm.info")
response = connection.getresponse()
body = response.read()
if response.status != 200:
    raise SystemExit(f"vm.info HTTP {response.status}: {body.decode(errors='replace')}")
info = json.loads(body)
balloon = (info.get("config") or {}).get("balloon") or {}
target = balloon.get("size", 0)
current = info.get("memory_actual_size")
if not isinstance(target, int) or not isinstance(current, int):
    raise SystemExit(f"invalid vm.info balloon state: target={target!r} current={current!r}")
print(target, current)
PY
}

wait_memory_stable() { # $1=sid $2=pid $3=log $4=headroom-bytes
    local sid="$1" pid="$2" log="$3" headroom="$4"
    local target=-1 current=-1 available=-1 demand=0 requested=0 expected_target=-1 target_gap=-1
    local matching_observations=0 meminfo="$WORK/$sid.stability.meminfo"
    local deadline=$((SECONDS + 120))
    while [ "$SECONDS" -lt "$deadline" ]; do
        if grep -q 'memory: initial Stable observation accepted' "$log" 2>/dev/null \
            && read -r target current <<<"$(read_ch_state "$sid" 2>/dev/null)" \
            && guest_exec "$sid" -- /bin/cat /proc/meminfo >"$meminfo" 2>/dev/null; then
            available=$(awk '$1 == "MemAvailable:" { printf "%.0f\n", $2 * 1024; exit }' "$meminfo")
            if [[ "$available" =~ ^[0-9]+$ ]] \
                && [ $((CAPACITY_BYTES - target)) -eq "$current" ]; then
                demand=0
                if [ "$current" -gt "$available" ]; then
                    demand=$((current - available))
                fi
                requested=$((demand + headroom))
                if [ "$requested" -gt "$CAPACITY_BYTES" ]; then
                    requested=$CAPACITY_BYTES
                fi
                expected_target=$((CAPACITY_BYTES - requested))
                expected_target=$((expected_target - expected_target % MEMORY_STEP_BYTES))
                # The controller deliberately keeps up to one Step of extra
                # Budget as its shrink deadband. A target above the formula
                # result would under-provision headroom; a target more than one
                # Step below it would indicate that steady reclaim stalled.
                target_gap=-1
                if [ "$target" -le "$expected_target" ]; then
                    target_gap=$((expected_target - target))
                fi
                if [ "$target_gap" -ge 0 ] \
                    && [ "$target_gap" -le "$MEMORY_STEP_BYTES" ] \
                    && [ $((target % MEMORY_STEP_BYTES)) -eq 0 ]; then
                    matching_observations=$((matching_observations + 1))
                    if [ "$matching_observations" -ge 2 ]; then
                        echo "    steady target=$target CurrentBudget=$current MemAvailable=$available headroom=$headroom desiredTarget=$expected_target deadband=$target_gap"
                        return 0
                    fi
                else
                    matching_observations=0
                fi
            else
                matching_observations=0
            fi
        fi
        kill -0 "$pid" 2>/dev/null || { echo "FAIL: $sid exited before stable" >&2; return 1; }
        sleep 0.5
    done
    echo "FAIL: $sid did not reach formula-derived steady Budget within one-Step deadband (target=$target current=$current MemAvailable=$available expected_target=$expected_target target_gap=$target_gap)" >&2
    sed -n '1,280p' "$log" >&2
    return 1
}

WRITER_PROBE=$(cat <<'PY'
import os
import sys

path = sys.argv[1]
size = int(sys.argv[2])
block = (b"KUASAR-SANDBOX-MEMORY-BUDGET-114\n" * 2048)[:65536]
with open(path, "wb", buffering=0) as output:
    remaining = size
    while remaining:
        chunk = block[:min(remaining, len(block))]
        output.write(chunk)
        remaining -= len(chunk)
os.sync()
PY
)

MINCORE_PROBE=$(cat <<'PY'
import ctypes
import mmap
import os
import sys

path = sys.argv[1]
with open(path, "rb", buffering=0) as source:
    size = os.fstat(source.fileno()).st_size
    mapping = mmap.mmap(source.fileno(), size, access=mmap.ACCESS_COPY)
    try:
        view = (ctypes.c_char * size).from_buffer(mapping)
        try:
            page_size = os.sysconf("SC_PAGE_SIZE")
            pages = (size + page_size - 1) // page_size
            residency = (ctypes.c_ubyte * pages)()
            libc = ctypes.CDLL(None, use_errno=True)
            mincore = libc.mincore
            mincore.argtypes = (ctypes.c_void_p, ctypes.c_size_t,
                                ctypes.POINTER(ctypes.c_ubyte))
            mincore.restype = ctypes.c_int
            if mincore(ctypes.addressof(view), size, residency) != 0:
                error = ctypes.get_errno()
                raise OSError(error, os.strerror(error), path)
            resident = sum(value & 1 for value in residency)
            print(f"mincore resident={resident} pages={pages} bytes={size} path={path}")
        finally:
            del view
    finally:
        mapping.close()
PY
)

guest_exec() { # $1=sid, remaining args are sandbox-ctl exec argv
    local sid="$1"
    shift
    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RUN_ROOT" "$@"
}

assert_resident_at_least() { # $1=mincore output $2=floor bytes $3=context
    local output="$1" floor_bytes="$2" context="$3"
    python3 - "$output" "$floor_bytes" "$context" <<'PY'
import re
import sys

text = open(sys.argv[1], encoding="utf-8").read()
floor_bytes = int(sys.argv[2])
match = re.search(r"resident=(\d+) pages=(\d+) bytes=(\d+)", text)
if not match:
    raise SystemExit(f"{sys.argv[3]}: invalid mincore output: {text!r}")
resident, pages, file_bytes = map(int, match.groups())
floor_pages = (floor_bytes * pages + file_bytes - 1) // file_bytes
if resident < floor_pages:
    raise SystemExit(
        f"{sys.argv[3]}: only {resident}/{pages} pages resident, "
        f"want at least {floor_pages} pages ({floor_bytes} bytes)"
    )
PY
}

record_snapshot_metrics() { # $1=key $2=snapshot-output $3=run-log $4=meminfo $5=mincore
    local key="$1" snapshot_output="$2" run_log="$3" meminfo="$4" mincore="$5"
    local record="$RESULT_ROOT/$key-record.json"
    python3 - "$key" "$snapshot_output" "$run_log" "$meminfo" "$mincore" "$record" <<'PY'
import json
import re
import sys

key, snapshot_output, run_log, meminfo_path, mincore_path, output = sys.argv[1:]
snapshot_text = open(snapshot_output, encoding="utf-8").read()
resident_matches = re.findall(r"\bresident=(\d+)", snapshot_text)
if not resident_matches:
    raise SystemExit(f"{key}: snapshot output lacks resident bytes")

lines = [line.strip() for line in open(run_log, encoding="utf-8", errors="replace")
         if "snapshot: freeze memory " in line]
if not lines:
    raise SystemExit(f"{key}: run log lacks freeze memory observation")
freeze = {name: int(value) for name, value in
          re.findall(r"([A-Za-z]+)=([0-9]+)", lines[-1])}
report = re.search(r"report=(\d+)/(\d+)", lines[-1])
if report:
    freeze["ReportEpoch"], freeze["ReportSeq"] = map(int, report.groups())

meminfo = {}
for line in open(meminfo_path, encoding="utf-8"):
    match = re.match(r"([^:]+):\s+(\d+)\s+kB", line)
    if match:
        meminfo[match.group(1)] = int(match.group(2)) * 1024

mincore_text = open(mincore_path, encoding="utf-8").read()
mincore_match = re.search(r"resident=(\d+) pages=(\d+) bytes=(\d+)", mincore_text)
if not mincore_match:
    raise SystemExit(f"{key}: invalid mincore output: {mincore_text!r}")
mincore = dict(zip(("resident_pages", "pages", "bytes"),
                   map(int, mincore_match.groups())))

with open(output, "w", encoding="utf-8") as destination:
    json.dump({
        "key": key,
        "memory_resident_bytes": int(resident_matches[-1]),
        "freeze": freeze,
        "prefreeze_meminfo": meminfo,
        "prefreeze_mincore": mincore,
    }, destination, indent=2, sort_keys=True)
    destination.write("\n")
PY
    RECORDS["$key"]="$record"
}

record_case() { # $1=label $2=headroom-yaml $3=startup-yaml $4=also-b $5=headroom-bytes $6=resident-floor
    local label="$1" headroom="$2" startup="$3" also_b="$4" headroom_bytes="$5"
    local resident_floor_bytes="$6"
    local sid="mb-${label}-$$" log="$WORK/$label-run.log"
    local cgroup diff cfg pid checksum warm_path=/opt/kuasar-working-set-114.bin
    new_cgroup "$sid"
    cgroup="$NEW_CGROUP"
    diff="$WORK/$label-root.diff"
    cfg="$WORK/$label.yaml"
    make_diff "$diff"
    write_config "$cfg" "$cgroup" "$headroom" "$startup" "$diff"

    echo
    echo "==> recording $label (headroom=$headroom startup=$startup)"
    "$BIN/sandbox-ctl" run --config "$cfg" --sandbox-id "$sid" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" \
        --base-root "$BASE_ROOT" --stats-json "$WORK/$label-cold-stats.json" \
        >"$log" 2>&1 &
    pid=$!
    SANDBOX_PIDS+=("$pid")
    ready "$sid" "$pid" "$log"
    wait_anon_ready "$sid" "$pid" "$log"
    wait_memory_stable "$sid" "$pid" "$log" "$headroom_bytes"

    guest_exec "$sid" -- python3 -c "$WRITER_PROBE" "$warm_path" "$WARM_BYTES"
    guest_exec "$sid" -- /bin/sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches'
    guest_exec "$sid" -- sha256sum "$warm_path" >"$WORK/$label-sha.out"
    checksum="$(awk 'NF >= 2 {print $1; exit}' "$WORK/$label-sha.out")"
    [[ "$checksum" =~ ^[0-9a-f]{64}$ ]] \
        || { echo "FAIL: $label invalid working-set checksum" >&2; exit 1; }

    # Wait across one production mem-report period. The retained shrink
    # deadband must keep the complete warmed clean working set resident.
    sleep "$REPORT_SETTLE_SECONDS"
    guest_exec "$sid" -- python3 -c "$MINCORE_PROBE" "$warm_path" \
        >"$WORK/$label-prefreeze.mincore"
    assert_resident_at_least "$WORK/$label-prefreeze.mincore" \
        "$resident_floor_bytes" "$label before W freeze"
    guest_exec "$sid" -- cat /proc/meminfo >"$WORK/$label-prefreeze.meminfo"

    local w_key="$label-w" w_out="$WORK/$label-w" w_snapshot_output="$WORK/$label-w.snapshot.out"
    mkdir -p "$w_out"
    echo "==> snapshot $w_key (drop_caches=false merge_ref=false)"
    if [ "$also_b" = "1" ]; then
        "$BIN/sandbox-ctl" snapshot --sandbox-id "$sid" --run-root "$RUN_ROOT" \
            --output "$w_out" --drop-caches=false --merge-ref=false --resume \
            >"$w_snapshot_output" 2>&1
    else
        "$BIN/sandbox-ctl" snapshot --sandbox-id "$sid" --run-root "$RUN_ROOT" \
            --output "$w_out" --drop-caches=false --merge-ref=false \
            >"$w_snapshot_output" 2>&1
    fi
    sed 's/^/    /' "$w_snapshot_output"
    SNAPSHOTS["$w_key"]="$w_out/$sid.snapshot"
    HEADROOMS["$w_key"]="$headroom"
    CHECKSUMS["$w_key"]="$checksum"
    [ -f "${SNAPSHOTS[$w_key]}" ] || { echo "FAIL: missing ${SNAPSHOTS[$w_key]}"; exit 1; }
    record_snapshot_metrics "$w_key" "$w_snapshot_output" "$log" \
        "$WORK/$label-prefreeze.meminfo" "$WORK/$label-prefreeze.mincore"

    if [ "$also_b" = "1" ]; then
        ready "$sid" "$pid" "$log"
        guest_exec "$sid" -- cat /proc/meminfo >"$WORK/$label-b-prefreeze.meminfo"
        guest_exec "$sid" -- python3 -c "$MINCORE_PROBE" "$warm_path" \
            >"$WORK/$label-b-prefreeze.mincore"
        local b_key="$label-b" b_out="$WORK/$label-b" b_snapshot_output="$WORK/$label-b.snapshot.out"
        mkdir -p "$b_out"
        echo "==> snapshot $b_key (drop_caches=true merge_ref=false)"
        "$BIN/sandbox-ctl" snapshot --sandbox-id "$sid" --run-root "$RUN_ROOT" \
            --output "$b_out" --drop-caches=true --merge-ref=false \
            >"$b_snapshot_output" 2>&1
        sed 's/^/    /' "$b_snapshot_output"
        SNAPSHOTS["$b_key"]="$b_out/$sid.snapshot"
        HEADROOMS["$b_key"]="$headroom"
        CHECKSUMS["$b_key"]="$checksum"
        [ -f "${SNAPSHOTS[$b_key]}" ] || { echo "FAIL: missing ${SNAPSHOTS[$b_key]}"; exit 1; }
        record_snapshot_metrics "$b_key" "$b_snapshot_output" "$log" \
            "$WORK/$label-b-prefreeze.meminfo" "$WORK/$label-b-prefreeze.mincore"
    fi

    wait "$pid" 2>/dev/null || true
}

record_restore_metrics() { # $1=key $2=stats $3=mincore $4=record $5=output
    python3 - "$@" <<'PY'
import json
import re
import sys

key, stats_path, mincore_path, record_path, output = sys.argv[1:]
stats = json.load(open(stats_path, encoding="utf-8"))
record = json.load(open(record_path, encoding="utf-8"))
mincore_text = open(mincore_path, encoding="utf-8").read()
match = re.search(r"resident=(\d+) pages=(\d+) bytes=(\d+)", mincore_text)
if not match:
    raise SystemExit(f"{key}: invalid restore mincore output: {mincore_text!r}")
mincore = dict(zip(("resident_pages", "pages", "bytes"), map(int, match.groups())))

rootfs = [backend for backend in stats.get("backends", [])
          if backend.get("name") in ("blk0", "blk1")]
if len(rootfs) != 2:
    raise SystemExit(f"{key}: rootfs backend count={len(rootfs)}, want blk0+blk1")
for backend in rootfs:
    for operation in ("read", "write", "flush", "discard"):
        if backend.get(operation, {}).get("err_count", 0) != 0:
            raise SystemExit(f"{key}: {backend['name']} {operation} errors: {backend[operation]}")
uffd = stats.get("uffd")
if not isinstance(uffd, dict) or uffd.get("errors") != 0:
    raise SystemExit(f"{key}: missing/error UFFD stats: {uffd!r}")

reads = {
    "count": sum(backend["read"]["count"] for backend in rootfs),
    "bytes": sum(backend["read"]["bytes"] for backend in rootfs),
    "lat_sum_ns": sum(backend["read"]["lat_sum_ns"] for backend in rootfs),
    "p50_ns_max": max(backend["read"]["p50_ns"] for backend in rootfs),
    "p99_ns_max": max(backend["read"]["p99_ns"] for backend in rootfs),
}
result = {
    "key": key,
    "record": record,
    "restore_mincore": mincore,
    "rootfs_read": reads,
    "vhost": rootfs,
    "uffd": uffd,
}
with open(output, "w", encoding="utf-8") as destination:
    json.dump(result, destination, indent=2, sort_keys=True)
    destination.write("\n")
PY
}

restore_case() { # $1=key $2=resident-floor-bytes
    local key="$1" resident_floor_bytes="$2"
    local snapshot="${SNAPSHOTS[$key]}"
    local sid="restore-${key}-$$" cgroup diff cfg log pid stats mincore checksum
    new_cgroup "$sid"
    cgroup="$NEW_CGROUP"
    diff="$WORK/$key-restore.diff"
    cfg="$WORK/$key-restore.yaml"
    log="$WORK/$key-restore.log"
    stats="$RESULT_ROOT/$key-stats.json"
    mincore="$RESULT_ROOT/$key-restore.mincore"
    checksum="${CHECKSUMS[$key]}"
    make_diff "$diff"
    # Omit the optional host startup policy here. Restore reserves the
    # Snapshot's captured BudgetAtSnapshot regardless of that policy.
    write_restore_config "$cfg" "$cgroup" "${HEADROOMS[$key]}" "$diff"

    echo
    echo "==> restoring $key"
    "$BIN/sandbox-ctl" run --restore "$snapshot" --config "$cfg" --sandbox-id "$sid" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" \
        --base-root "$BASE_ROOT" --stats-json "$stats" >"$log" 2>&1 &
    pid=$!
    SANDBOX_PIDS+=("$pid")
    ready "$sid" "$pid" "$log"
    guest_exec "$sid" -- python3 -c "$MINCORE_PROBE" /opt/kuasar-working-set-114.bin \
        >"$mincore"
    assert_resident_at_least "$mincore" "$resident_floor_bytes" \
        "$key after restore before first content read"
    guest_exec "$sid" -- sha256sum /opt/kuasar-working-set-114.bin \
        >"$WORK/$key-restore.sha"
    grep -Fq "$checksum  /opt/kuasar-working-set-114.bin" "$WORK/$key-restore.sha" \
        || { echo "FAIL: $key restore checksum mismatch" >&2; exit 1; }

    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    [ -s "$stats" ] || { echo "FAIL: $key missing stats JSON" >&2; sed -n '1,280p' "$log" >&2; exit 1; }
    local result="$RESULT_ROOT/$key-result.json"
    record_restore_metrics "$key" "$stats" "$mincore" "${RECORDS[$key]}" "$result"
    RESTORES["$key"]="$result"
}

# The two controls are independent W captures. The new-model VM is resumed
# after W so B is captured from the same guest and disk state.
record_case no-balloon 8GiB 8GiB 0 "$CAPACITY_BYTES" "$WARM_BYTES"
record_case workaround 7680MiB 7680MiB 0 $((7680 * 1024 * 1024)) "$WARM_BYTES"
record_case new 256MiB 2GiB 1 "$HEADROOM_BYTES" "$WARM_BYTES"

restore_case no-balloon-w "$WARM_BYTES"
restore_case workaround-w "$WARM_BYTES"
restore_case new-w "$WARM_BYTES"
restore_case new-b 0

python3 - "$CAPACITY_BYTES" "$HEADROOM_BYTES" "$MEMORY_STEP_BYTES" "$ANON_BYTES" "$WARM_BYTES" \
    "${RESTORES[no-balloon-w]}" "${RESTORES[workaround-w]}" \
    "${RESTORES[new-w]}" "${RESTORES[new-b]}" "$RESULT_ROOT/summary.json" <<'PY'
import json
import sys

capacity, headroom, step, anon_bytes, warm_bytes = map(int, sys.argv[1:6])
paths = sys.argv[6:10]
output = sys.argv[10]
results = {doc["key"]: doc for doc in (json.load(open(path, encoding="utf-8")) for path in paths)}

for key in ("no-balloon-w", "workaround-w", "new-w"):
    doc = results[key]
    freeze = doc["record"]["freeze"]
    required = {
        "BalloonTarget", "BalloonCurrent", "TargetBudget", "CurrentBudget",
        "ObservedBudget", "Reservation", "MemAvailable", "Cached",
        "ReportEpoch", "ReportSeq",
    }
    missing = sorted(required - freeze.keys())
    if missing:
        raise SystemExit(f"{key}: freeze metrics missing {missing}")
    if freeze["BalloonTarget"] + freeze["TargetBudget"] != capacity:
        raise SystemExit(f"{key}: target/TargetBudget do not sum to Capacity: {freeze}")
    if freeze["BalloonCurrent"] + freeze["CurrentBudget"] != capacity:
        raise SystemExit(f"{key}: current/CurrentBudget do not sum to Capacity: {freeze}")
    if freeze["ObservedBudget"] != max(freeze["TargetBudget"], freeze["CurrentBudget"]):
        raise SystemExit(f"{key}: ObservedBudget invariant failed: {freeze}")
    if freeze["Reservation"] < freeze["ObservedBudget"]:
        raise SystemExit(f"{key}: reservation under observed Budget: {freeze}")
    if freeze["TargetBudget"] != freeze["CurrentBudget"]:
        raise SystemExit(f"{key}: W captured with unstable target/current: {freeze}")
    if freeze["ReportEpoch"] == 0 or freeze["ReportSeq"] == 0:
        raise SystemExit(f"{key}: W freeze used no trusted report: {freeze}")
    pre = doc["record"]["prefreeze_mincore"]
    restored = doc["restore_mincore"]
    resident_floor = warm_bytes
    for phase, observation in (("prefreeze", pre), ("restore", restored)):
        resident_bytes = observation["resident_pages"] * observation["bytes"] // observation["pages"]
        if resident_bytes < resident_floor:
            raise SystemExit(
                f"{key}: {phase} resident bytes={resident_bytes}, "
                f"want at least {resident_floor}"
            )
    expected_resident = anon_bytes + resident_floor
    if doc["record"]["memory_resident_bytes"] < expected_resident:
        raise SystemExit(
            f"{key}: W memory resident bytes={doc['record']['memory_resident_bytes']} "
            f"smaller than anon+required file working set={expected_resident}"
        )

new_freeze = results["new-w"]["record"]["freeze"]
if new_freeze["MemAvailable"] < headroom:
    raise SystemExit(
        f"new-w: freeze MemAvailable={new_freeze['MemAvailable']} below "
        f"configured headroom={headroom}"
    )
if new_freeze["Cached"] < warm_bytes:
    raise SystemExit(
        f"new-w: freeze Cached={new_freeze['Cached']} below "
        f"working-set size={warm_bytes}"
    )

# The old failure was thousands of extra rootfs reads while both controls were
# at (or near) zero. Allow a factor-of-two environmental spread plus sixteen
# metadata requests; this is a relative I/O-amplification gate, not a latency
# service-level target.
control_count = max(results["no-balloon-w"]["rootfs_read"]["count"],
                    results["workaround-w"]["rootfs_read"]["count"])
control_bytes = max(results["no-balloon-w"]["rootfs_read"]["bytes"],
                    results["workaround-w"]["rootfs_read"]["bytes"])
new_count = results["new-w"]["rootfs_read"]["count"]
new_bytes = results["new-w"]["rootfs_read"]["bytes"]
if new_count > control_count * 2 + 16:
    raise SystemExit(
        f"new-w rootfs reads={new_count} exceed controls={control_count} by more than 2x+16"
    )
if new_bytes > control_bytes * 2 + 16 * 4096:
    raise SystemExit(
        f"new-w rootfs bytes={new_bytes} exceed controls={control_bytes} by more than 2x+64KiB"
    )

b_snapshot = results["new-b"]
if b_snapshot["record"]["memory_resident_bytes"] < anon_bytes:
    raise SystemExit("new-b: anonymous demand was lost from the B snapshot")
if b_snapshot["rootfs_read"]["count"] == 0:
    raise SystemExit("new-b: drop_caches=true control performed no rootfs read")

with open(output, "w", encoding="utf-8") as destination:
    json.dump({
        "capacity_bytes": capacity,
        "headroom_bytes": headroom,
        "memory_step_bytes": step,
        "anonymous_demand_bytes": anon_bytes,
        "working_set_bytes": warm_bytes,
        "required_file_resident_bytes": warm_bytes,
        "rootfs_control_max_count": control_count,
        "rootfs_control_max_bytes": control_bytes,
        "results": results,
    }, destination, indent=2, sort_keys=True)
    destination.write("\n")

print("case              W-resident   freeze-Mavail freeze-Cached rootfs-reads rootfs-bytes p50-max p99-max")
for key in ("no-balloon-w", "workaround-w", "new-w", "new-b"):
    doc = results[key]
    freeze = doc["record"]["freeze"]
    reads = doc["rootfs_read"]
    print(f"{key:17} {doc['record']['memory_resident_bytes']:10d} "
          f"{freeze.get('MemAvailable', 0):13d} {freeze.get('Cached', 0):13d} "
          f"{reads['count']:12d} {reads['bytes']:12d} "
          f"{reads['p50_ns_max']:7d} {reads['p99_ns_max']:7d}")
PY

echo
echo "==> #114 metrics: $RESULT_ROOT/summary.json"
echo "==> e2e_sandbox_memory_budget_workingset: OK"
