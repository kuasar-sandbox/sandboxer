#!/usr/bin/env bash
#
# e2e_sandbox_vhost_cow_perf.sh — acceptance (a): guest vs host disk I/O
# through the vhost-user-blk COW overlay.
#
# "Internal" = a file written/read inside the guest (guest FS → virtio-blk →
# vhost-user-blk → BlockCOW → overlay file).
# "External" = the same Python workload against a file on the host, on the
# same filesystem that holds the overlay (no VM, no vhost).
#
# Prints a results table (MB/s, IOPS, loss %) and a JSON document. The test
# PASSES when both sides completed and guest writes actually hit the COW
# backend. It does not fail on a particular loss percentage (that is the
# analysis). Set LOSS_MAX_PCT to turn the table into a gate.
#
# Knobs:
#   IO_MIB          sequential size (default 32); overlay upper is max(256, 2*IO_MIB) MiB
#   SEQ_CHUNK_KIB   sequential chunk (default 1024)
#   RAND_OPS        4K random operations (default 4096)
#   PERF_RESULTS_JSON  extra copy of the results JSON
#   LOSS_MAX_PCT    optional: fail if any sequential loss exceeds this
#
# Not part of test/e2e/run_all.sh — run this file yourself when you want the
# measurement. Host scratch (including --run-root sockets) lives under /tmp
# so Unix paths stay below Linux sun_path; a deep CI checkout exceeds that
# limit. The test does not create a TAP, does not docker-pull, and does not
# write /proc/sys/vm/drop_caches. Cold guest reads posix_fadvise the live
# host-side overlay.diff (BlockCOW buffered I/O) between phases instead.
# Missing KVM/binaries skip (exit 0) unless REQUIRE_KVM=1.
#
# Binaries: sandboxer `make build` plus guest-runtime flatten-ctl / bundle /
# vmlinux. Set BIN= to force a single assembled directory.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
IMAGE="${IMAGE:-python:3.12-slim}"
SID="${SID:-cowperf1}"
IO_MIB="${IO_MIB:-32}"
SEQ_CHUNK_KIB="${SEQ_CHUNK_KIB:-1024}"
RAND_OPS="${RAND_OPS:-4096}"
ARCH="$(uname -m)"

skip() {
    echo
    echo "==> e2e_sandbox_vhost_cow_perf: skipping ($*)"
    if [ "${REQUIRE_KVM:-0}" = "1" ]; then
        echo "REQUIRE_KVM=1 set; failing instead of skipping" >&2
        exit 1
    fi
    exit 0
}

# Prefer BIN if set (assembled platform dir). Otherwise stitch sandboxer/bin
# with the sibling guest-runtime tree so `make build` in this repo is enough.
find_artifact() {
    local name="$1" d
    local dirs=()
    [ -n "${BIN:-}" ] && dirs+=("$BIN")
    dirs+=(
        "$REPO_ROOT/bin"
        "$REPO_ROOT/bin/$ARCH"
        "$REPO_ROOT/../guest-runtime/bin"
        "$REPO_ROOT/../guest-runtime/bin/$ARCH"
        "$REPO_ROOT/../guest-runtime/native-deps/bin/$ARCH"
    )
    for d in "${dirs[@]}"; do
        if [ -e "$d/$name" ]; then
            printf '%s\n' "$d/$name"
            return 0
        fi
    done
    return 1
}

require_artifact() {
    # Look up $1 and optionally assign the path to $2. Must be called from the
    # main shell (not $(...)): skip has to exit this process, not a subshell.
    local name="$1" dest="${2:-}" path
    path="$(find_artifact "$name")" \
        || skip "missing $name — make -C sandboxer build, and make -C guest-runtime flatten-ctl sandbox-runtime vmlinux (or set BIN=)"
    if [ -n "$dest" ]; then
        printf -v "$dest" '%s' "$path"
    fi
}

[ -e /dev/kvm ] || skip "/dev/kvm not present"
command -v python3 >/dev/null 2>&1 || skip "python3 not on PATH"
require_artifact sandbox-ctl SANDBOX_CTL
require_artifact sandbox-init
require_artifact cloud-hypervisor CH_BIN
require_artifact flatten-ctl FLATTEN_CTL
require_artifact sandbox-runtime.bundle RUNTIME_BUNDLE
if [ -n "${VMLINUX:-}" ]; then
    [ -f "$VMLINUX" ] || skip "no vmlinux at $VMLINUX"
else
    require_artifact vmlinux VMLINUX
fi
command -v mkfs.ext4 >/dev/null 2>&1 || skip "mkfs.ext4 not on PATH (apt install e2fsprogs)"

if [ "$(id -u)" -ne 0 ]; then
    if sudo -n true 2>/dev/null; then
        exec sudo -nE "$0" "$@"
    fi
    echo "==> sudo -n unavailable; continuing as $(id -un) (need rw /dev/kvm)"
fi
[ -r /dev/kvm ] && [ -w /dev/kvm ] || skip "/dev/kvm not accessible to current user"

case "$IO_MIB" in
    ''|*[!0-9]*|0) echo "$0: IO_MIB must be a positive integer" >&2; exit 1 ;;
esac
case "$SEQ_CHUNK_KIB" in
    ''|*[!0-9]*|0) echo "$0: SEQ_CHUNK_KIB must be a positive integer" >&2; exit 1 ;;
esac
case "$RAND_OPS" in
    ''|*[!0-9]*|0) echo "$0: RAND_OPS must be a positive integer" >&2; exit 1 ;;
esac
# Bench file + ext4 journal/reserved blocks + overlayfs boot copy-up. The
# default IO_MIB=32 keeps the historical 256MiB upper.
OVERLAY_MIB=$((IO_MIB * 2))
if [ "$OVERLAY_MIB" -lt 256 ]; then
    OVERLAY_MIB=256
fi

# Scratch under /tmp (same as the other e2e cases). --run-root sockets such as
# uffd.sock must fit in sockaddr_un.sun_path (108 bytes including NUL).
WORK="$(mktemp -d /tmp/e2e-cow-perf-XXXXXX)"
mkdir -p "$WORK/tmp"
export TMPDIR="$WORK/tmp"
RR="$WORK/run"; mkdir -p "$RR"
BASE_ROOT="$WORK/base"; mkdir -p "$BASE_ROOT"
SBPID=""
cleanup() {
    set +e
    if [ -n "$SBPID" ] && kill -0 "$SBPID" 2>/dev/null; then
        kill -TERM "$SBPID" 2>/dev/null
        wait "$SBPID" 2>/dev/null
    fi
    pkill -f "cloud-hypervisor.*$SID" 2>/dev/null
    [ -n "${E2E_KEEP:-}" ] && echo "kept: $WORK" || rm -rf "$WORK"
}
trap cleanup EXIT

under_repo() {
    local resolved
    resolved="$(realpath -m "$1")"
    case "$resolved" in
        "$REPO_ROOT"|"$REPO_ROOT"/*) return 0 ;;
        *) return 1 ;;
    esac
}

# Identical guest/host microbench. JSON on stdout; progress on stderr.
cat > "$WORK/io_bench.py" <<'PY'
import argparse
import json
import os
import random
import sys
import time


def evict_file_cache(path, sync=False):
    """Drop this file from page cache only. Does not write /proc/sys/vm/drop_caches.

    sync=True fsyncs the inode first so DONTNEED can drop pages dirtied by
    another fd (the live BlockCOW overlay.diff opened with buffered I/O).
    """
    fd = os.open(path, os.O_RDONLY)
    try:
        if sync:
            os.fsync(fd)
        os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
    finally:
        os.close(fd)


def fill_write(fd, size, chunk, payload):
    left = size
    while left:
        n = chunk if left >= chunk else left
        wrote = os.write(fd, payload if n == chunk else payload[:n])
        if wrote <= 0:
            raise OSError("short write")
        left -= wrote


def timed_seq_write(path, size, chunk, overwrite):
    flags = os.O_RDWR | os.O_CREAT
    if not overwrite:
        flags |= os.O_TRUNC
    fd = os.open(path, flags, 0o644)
    try:
        os.lseek(fd, 0, os.SEEK_SET)
        payload = b"K" * chunk
        t0 = time.perf_counter_ns()
        fill_write(fd, size, chunk, payload)
        os.fsync(fd)
        return time.perf_counter_ns() - t0
    finally:
        os.close(fd)


def timed_seq_read(path, chunk):
    fd = os.open(path, os.O_RDONLY)
    try:
        t0 = time.perf_counter_ns()
        while True:
            data = os.read(fd, chunk)
            if not data:
                break
        return time.perf_counter_ns() - t0
    finally:
        os.close(fd)


def timed_rand(path, io_size, ops, write, seed):
    flags = os.O_RDWR if write else os.O_RDONLY
    fd = os.open(path, flags)
    try:
        size = os.fstat(fd).st_size
        slots = max(1, size // io_size)
        payload = b"R" * io_size
        rng = random.Random(seed)
        offsets = [rng.randrange(slots) * io_size for _ in range(ops)]
        t0 = time.perf_counter_ns()
        if write:
            for off in offsets:
                os.lseek(fd, off, os.SEEK_SET)
                if os.write(fd, payload) != io_size:
                    raise OSError("short random write")
            os.fsync(fd)
        else:
            for off in offsets:
                os.lseek(fd, off, os.SEEK_SET)
                if len(os.read(fd, io_size)) != io_size:
                    raise OSError("short random read")
        return time.perf_counter_ns() - t0
    finally:
        os.close(fd)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--evict-only", metavar="PATH")
    p.add_argument("--path")
    p.add_argument("--bytes", type=int)
    p.add_argument("--chunk", type=int)
    p.add_argument("--rand-ops", type=int)
    p.add_argument("--side", default="")
    p.add_argument(
        "--phase",
        choices=("all", "seq-write", "seq-read", "rand-write", "rand-read"),
        default="all",
    )
    args = p.parse_args()
    if args.evict_only:
        evict_file_cache(args.evict_only, sync=True)
        return
    missing = [
        flag
        for flag, value in (
            ("--path", args.path),
            ("--bytes", args.bytes),
            ("--chunk", args.chunk),
            ("--rand-ops", args.rand_ops),
            ("--side", args.side),
        )
        if value in (None, "")
    ]
    if missing:
        p.error("the following arguments are required: " + ", ".join(missing))
    rand_size = 4096
    result = {
        "side": args.side,
        "path": args.path,
        "bytes": args.bytes,
        "chunk": args.chunk,
        "rand_ops": args.rand_ops,
        "rand_io": rand_size,
    }
    run_all = args.phase == "all"
    if run_all or args.phase == "seq-write":
        print(f"{args.side}: seq write first {args.bytes} bytes", file=sys.stderr, flush=True)
        result["seq_write_first_ns"] = timed_seq_write(args.path, args.bytes, args.chunk, False)
        print(f"{args.side}: seq write overwrite", file=sys.stderr, flush=True)
        result["seq_write_overwrite_ns"] = timed_seq_write(args.path, args.bytes, args.chunk, True)
    if run_all or args.phase == "seq-read":
        print(f"{args.side}: seq read cold", file=sys.stderr, flush=True)
        evict_file_cache(args.path)
        result["seq_read_cold_ns"] = timed_seq_read(args.path, args.chunk)
    if run_all or args.phase == "rand-write":
        print(f"{args.side}: rand 4k write ops={args.rand_ops}", file=sys.stderr, flush=True)
        evict_file_cache(args.path)
        result["rand4k_write_ns"] = timed_rand(args.path, rand_size, args.rand_ops, True, 1)
    if run_all or args.phase == "rand-read":
        print(f"{args.side}: rand 4k read ops={args.rand_ops}", file=sys.stderr, flush=True)
        evict_file_cache(args.path)
        result["rand4k_read_ns"] = timed_rand(args.path, rand_size, args.rand_ops, False, 2)
    json.dump(result, sys.stdout)
    sys.stdout.write("\n")
    sys.stdout.flush()


if __name__ == "__main__":
    main()
PY

BYTES=$((IO_MIB * 1024 * 1024))
CHUNK=$((SEQ_CHUNK_KIB * 1024))
if [ "$CHUNK" -gt "$BYTES" ]; then
    echo "$0: SEQ_CHUNK_KIB is larger than IO_MIB" >&2
    exit 1
fi

echo "==> building rootfs + ${OVERLAY_MIB}MiB overlay template"
# flatten-ctl looks next to itself, then $PATH. Prefer the kuasar mkfs.erofs
# (file cap cap_dac_override) over a distro copy that cannot read root-owned
# dirs in the extracted image when we are not root.
if [ -z "${MKFS_EROFS_PATH:-}" ]; then
    MKFS_EROFS_PATH="$(find_artifact mkfs.erofs || true)"
fi
[ -n "${MKFS_EROFS_PATH:-}" ] && export MKFS_EROFS_PATH
BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -n "$BLK0_IMAGE" ]; then
    under_repo "$BLK0_IMAGE" || { echo "==> FAIL: BLK0_IMAGE must be under $REPO_ROOT (got $BLK0_IMAGE)" >&2; exit 1; }
fi
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker not available; provide BLK0_IMAGE=path/to/prebuilt.erofs"
    docker image inspect "$IMAGE" >/dev/null 2>&1 \
        || skip "docker image $IMAGE is not present locally; pull it yourself or set BLK0_IMAGE= (this test does not docker-pull)"
    CACHE_DIR="/tmp/e2e-cow-perf-cache"
    CACHE_IMG="$CACHE_DIR/$(echo "$IMAGE" | tr '/:' '__').erofs"
    if [ -s "$CACHE_IMG" ]; then
        echo "==> reusing cached flatten $CACHE_IMG"
        BLK0_IMAGE="$CACHE_IMG"
    else
        mkdir -p "$CACHE_DIR"
        BLK0_IMAGE="$WORK/blk0.img"
        docker save "$IMAGE" | "$FLATTEN_CTL" export --output "$BLK0_IMAGE" --tmpdir "$WORK/tmp" --no-progress
        cp -f "$BLK0_IMAGE" "$CACHE_IMG"
        BLK0_IMAGE="$CACHE_IMG"
    fi
fi
# flatten-ctl writes .kuasar.sha256.<hex>; the shared e2e helper only
# accepts .kuasar.digest.<hex>. Read whichever marker is on the artifact.
blk0_tarstream_ref() {
    local path="$1" listing marker scheme digest
    if [ ! -f "$path" ]; then
        echo "plaintext tarstream does not exist: $path" >&2
        return 1
    fi
    if ! listing="$(tar -tf "$path")"; then
        echo "failed to inspect plaintext tarstream: $path" >&2
        return 1
    fi
    marker="$(grep -E '^\.kuasar\.(digest|sha256)\.[0-9a-f]{64}$' <<<"$listing" || true)"
    if [ "$(grep -c . <<<"$marker")" != 1 ]; then
        echo "plaintext tarstream must contain exactly one digest marker: $path" >&2
        return 1
    fi
    case "$marker" in
        .kuasar.digest.*) scheme=digest; digest="${marker#.kuasar.digest.}" ;;
        .kuasar.sha256.*) scheme=sha256; digest="${marker#.kuasar.sha256.}" ;;
        *) echo "plaintext tarstream must contain exactly one digest marker: $path" >&2; return 1 ;;
    esac
    printf 'file://%s@%s:%s\n' "$path" "$scheme" "$digest"
}
BLK0_REF="$(blk0_tarstream_ref "$BLK0_IMAGE")"

TEMPLATE="$WORK/overlay.ext4"
truncate -s "${OVERLAY_MIB}M" "$TEMPLATE"
mkfs.ext4 -q -F "$TEMPLATE"

cat > "$WORK/sandbox.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 1GiB }
  allocatable: { cpu: 1, memory: 1GiB }
boot:
  kernel: file://$VMLINUX
  runtime: file://$RUNTIME_BUNDLE
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay:
      diff_template: file://$TEMPLATE
launch:
  exec: /bin/sleep
  args: ["3600"]
  restart: never
EOF

STATS_JSON="${PERF_STATS_JSON:-$WORK/stats.json}"
RESULTS_JSON="${PERF_RESULTS_JSON:-$WORK/perf-results.json}"
if [ -n "${PERF_STATS_JSON:-}" ]; then
    under_repo "$STATS_JSON" || { echo "==> FAIL: PERF_STATS_JSON must be under $REPO_ROOT (got $STATS_JSON)" >&2; exit 1; }
fi
if [ -n "${PERF_RESULTS_JSON:-}" ]; then
    under_repo "$RESULTS_JSON" || { echo "==> FAIL: PERF_RESULTS_JSON must be under $REPO_ROOT (got $RESULTS_JSON)" >&2; exit 1; }
fi
LOG="$WORK/run.log"
echo "==> launching sandbox-ctl run (sid=$SID, IO=${IO_MIB}MiB)"
# Run sandbox-ctl directly (not under GNU timeout) so SIGTERM reaches it and
# --stats-json is written on the ServeAndWait shutdown path.
"$SANDBOX_CTL" run \
    --config "$WORK/sandbox.yaml" \
    --sandbox-id "$SID" \
    --ch-binary "$CH_BIN" \
    --run-root "$RR" \
    --base-root "$BASE_ROOT" \
    --stats-json "$STATS_JSON" \
    > "$LOG" 2>&1 &
SBPID=$!

ready() {
    for _ in $(seq 1 90); do
        if timeout -k 2s 6 "$SANDBOX_CTL" exec --sandbox-id "$SID" --run-root "$RR" -- \
            /bin/sh -c 'echo R' >"$WORK/r.out" 2>/dev/null && grep -q R "$WORK/r.out"; then
            return 0
        fi
        if ! kill -0 "$SBPID" 2>/dev/null; then
            return 1
        fi
        sleep 1
    done
    return 1
}
ready || { echo "==> FAIL: sandbox not ready"; tail -60 "$LOG"; exit 1; }
echo "==> PASS: sandbox ready"

ex() { "$SANDBOX_CTL" exec --sandbox-id "$SID" --run-root "$RR" "$@"; }

ex -- /bin/sh -c 'command -v python3 >/dev/null' \
    || { echo "==> FAIL: guest image must provide python3 (default IMAGE=python:3.12-slim)"; exit 1; }

echo "==> install io_bench.py in guest"
"$SANDBOX_CTL" exec --stdin --sandbox-id "$SID" --run-root "$RR" -- \
    /bin/sh -c 'cat > /cow-io-bench.py' < "$WORK/io_bench.py"

# Auto-default overlay upper: --base-root/<sid>/<sid>.overlay.diff. Guest
# posix_fadvise only drops guest page cache; BlockCOW keeps this file open
# with buffered I/O, so cold reads must also DONTNEED it on the host.
DIFF_PATH="$BASE_ROOT/$SID/$SID.overlay.diff"
[ -f "$DIFF_PATH" ] || {
    echo "==> FAIL: expected overlay diff at $DIFF_PATH"
    find "$BASE_ROOT" -type f 2>/dev/null | sed 's/^/    /' || true
    exit 1
}

evict_host_cow() {
    echo "==> evict host-side COW overlay cache ($DIFF_PATH)"
    python3 "$WORK/io_bench.py" --evict-only "$DIFF_PATH"
}

run_guest_phase() {
    local phase="$1"
    echo "==> guest (internal) I/O through vhost-user-blk COW: $phase"
    set +e
    ex -- python3 /cow-io-bench.py \
        --phase "$phase" \
        --path /cow-io.bin \
        --bytes "$BYTES" \
        --chunk "$CHUNK" \
        --rand-ops "$RAND_OPS" \
        --side guest \
        > "$WORK/guest-${phase}.out" 2>"$WORK/guest-${phase}.err"
    local rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
        echo "==> FAIL: guest bench phase $phase exited $rc"
        sed 's/^/    /' "$WORK/guest-${phase}.err" "$WORK/guest-${phase}.out"
        tail -40 "$LOG"
        exit 1
    fi
    sed 's/^/    /' "$WORK/guest-${phase}.err" || true
    grep -E '^\{' "$WORK/guest-${phase}.out" | tail -1 > "$WORK/guest-${phase}.json"
    python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$WORK/guest-${phase}.json" \
        || { echo "==> FAIL: guest bench phase $phase stdout is not JSON"; sed 's/^/    /' "$WORK/guest-${phase}.out"; exit 1; }
}

run_guest_phase seq-write
evict_host_cow
run_guest_phase seq-read
evict_host_cow
run_guest_phase rand-write
evict_host_cow
run_guest_phase rand-read
python3 - \
    "$WORK/guest-seq-write.json" \
    "$WORK/guest-seq-read.json" \
    "$WORK/guest-rand-write.json" \
    "$WORK/guest-rand-read.json" \
    "$WORK/guest.json" <<'PY'
import json
import sys

*inputs, dest = sys.argv[1:]
merged = {}
for path in inputs:
    with open(path, encoding="utf-8") as fh:
        merged.update(json.load(fh))
required = (
    "seq_write_first_ns",
    "seq_write_overwrite_ns",
    "seq_read_cold_ns",
    "rand4k_write_ns",
    "rand4k_read_ns",
)
missing = [key for key in required if key not in merged]
if missing:
    raise SystemExit(f"FAIL: merged guest JSON missing {missing}")
with open(dest, "w", encoding="utf-8") as fh:
    json.dump(merged, fh)
    fh.write("\n")
PY

echo "==> host (external) I/O on the overlay's filesystem"
set +e
python3 "$WORK/io_bench.py" \
    --path "$WORK/host-io.bin" \
    --bytes "$BYTES" \
    --chunk "$CHUNK" \
    --rand-ops "$RAND_OPS" \
    --side host \
    > "$WORK/host.json" 2>"$WORK/host.err"
HOST_RC=$?
set -e
if [ "$HOST_RC" -ne 0 ]; then
    echo "==> FAIL: host bench exited $HOST_RC"
    sed 's/^/    /' "$WORK/host.err" "$WORK/host.json"
    exit 1
fi
sed 's/^/    /' "$WORK/host.err" || true

echo "==> stopping sandbox to harvest --stats-json"
kill -TERM "$SBPID" 2>/dev/null || true
set +e
wait "$SBPID"
SB_RC=$?
set -e
SBPID=""
if [ "$SB_RC" -ne 0 ]; then
    echo "==> FAIL: sandbox-ctl exited $SB_RC after SIGTERM (want 0)"
    tail -60 "$LOG"
    exit 1
fi
sleep 0.2

RESULTS_JSON="${PERF_RESULTS_JSON:-$WORK/perf-results.json}"
LOSS_MAX_PCT="${LOSS_MAX_PCT:-}"
python3 - "$WORK/guest.json" "$WORK/host.json" "$STATS_JSON" "$RESULTS_JSON" "$LOSS_MAX_PCT" <<'PY'
import json
import os
import sys

guest_path, host_path, stats_path, out_path, loss_max_raw = sys.argv[1:6]

with open(guest_path, encoding="utf-8") as fh:
    guest = json.load(fh)
with open(host_path, encoding="utf-8") as fh:
    host = json.load(fh)

workloads = [
    ("seq write (first / COW materialize)", "seq_write_first_ns", "seq", None),
    ("seq write (overwrite / dirty)", "seq_write_overwrite_ns", "seq", None),
    ("seq read (cold cache)", "seq_read_cold_ns", "seq", None),
    ("rand 4K write", "rand4k_write_ns", "rand_write", "rand_ops"),
    ("rand 4K read", "rand4k_read_ns", "rand_read", "rand_ops"),
]


def mib_s(nbytes, ns):
    if ns <= 0:
        return 0.0
    return (nbytes / (1024 * 1024)) / (ns / 1e9)


def iops(ops, ns):
    if ns <= 0:
        return 0.0
    return ops / (ns / 1e9)


def loss_pct(guest_rate, host_rate):
    if host_rate <= 0:
        return None
    return (1.0 - guest_rate / host_rate) * 100.0


rows = []
for label, key, kind, ops_key in workloads:
    g_ns = int(guest[key])
    h_ns = int(host[key])
    if kind == "seq":
        nbytes = int(guest["bytes"])
        g_rate = mib_s(nbytes, g_ns)
        h_rate = mib_s(nbytes, h_ns)
        unit = "MiB/s"
    else:
        ops = int(guest[ops_key])
        g_rate = iops(ops, g_ns)
        h_rate = iops(ops, h_ns)
        unit = "IOPS"
    loss = loss_pct(g_rate, h_rate)
    rows.append({
        "workload": label,
        "key": key,
        "unit": unit,
        "guest_ns": g_ns,
        "host_ns": h_ns,
        "guest_rate": g_rate,
        "host_rate": h_rate,
        "loss_pct": loss,
    })

vhost = None
if os.path.isfile(stats_path) and os.path.getsize(stats_path) > 0:
    with open(stats_path, encoding="utf-8") as fh:
        stats = json.load(fh)
    backends = {b["name"]: b for b in stats.get("backends") or []}
    # Overlay mode: blk1 is the writable COW (blk0 is the read-only erofs).
    vhost = backends.get("blk1") or next(
        (b for b in backends.values() if (b.get("extra") or {}).get("diff_dirty_blocks") is not None),
        None,
    )
else:
    stats = None

print()
print("==> Performance comparison: guest VM (vhost-user-blk COW) vs host")
print(f"    size={guest['bytes']} bytes  seq_chunk={guest['chunk']}  rand_ops={guest['rand_ops']} x 4KiB")
print()
print(f"{'workload':<42} {'guest':>12} {'host':>12} {'loss':>10}  unit")
print("-" * 86)
for row in rows:
    loss = row["loss_pct"]
    loss_s = "n/a" if loss is None else f"{loss:7.1f}%"
    print(f"{row['workload']:<42} {row['guest_rate']:12.2f} {row['host_rate']:12.2f} {loss_s:>10}  {row['unit']}")

print()
print("    loss % = (1 - guest_rate / host_rate) * 100")
print("    Positive loss = guest is slower (expected). Negative = guest faster.")
print("    seq write (first) includes COW 4K materialize; overwrite is dirty-page.")
print("    Cold reads: guest posix_fadvise on the bench file, plus host posix_fadvise")
print("    on the live BlockCOW overlay.diff (buffered I/O) between guest phases.")

if vhost is None:
    print()
    if stats is None:
        raise SystemExit(
            f"FAIL: --stats-json missing or empty at {stats_path}; "
            "cannot verify guest writes reached the vhost-user-blk COW backend"
        )
    raise SystemExit(
        "FAIL: --stats-json has no blk1/COW backend; "
        "cannot verify guest writes reached BlockCOW"
    )

wr, rd = vhost["write"], vhost["read"]
extra = vhost.get("extra") or {}
print()
print(f"==> vhost {vhost['name']} (COW overlay) from --stats-json")
print(f"    write: count={wr['count']} bytes={wr['bytes']} err={wr['err_count']} "
      f"p50={wr['p50_ns']/1000:.1f}us p99={wr['p99_ns']/1000:.1f}us max={wr['lat_max_ns']/1000:.1f}us")
print(f"    read:  count={rd['count']} bytes={rd['bytes']} err={rd['err_count']} "
      f"p50={rd['p50_ns']/1000:.1f}us p99={rd['p99_ns']/1000:.1f}us max={rd['lat_max_ns']/1000:.1f}us")
if extra:
    dirty = extra.get("diff_dirty_blocks")
    total = extra.get("diff_total_blocks")
    pct = extra.get("diff_dirty_percent")
    print(f"    dirty: {dirty}/{total} blocks ({pct}%)")

if wr["count"] <= 0 or wr["bytes"] < guest["bytes"]:
    raise SystemExit(
        f"FAIL: COW backend wrote count={wr['count']} bytes={wr['bytes']}; "
        f"expected guest sequential writes of {guest['bytes']} bytes"
    )
print("==> PASS: guest writes reached the vhost-user-blk COW backend")

doc = {
    "guest": guest,
    "host": host,
    "comparison": rows,
    "vhost_cow": vhost,
}
os.makedirs(os.path.dirname(out_path) or ".", exist_ok=True)
with open(out_path, "w", encoding="utf-8") as fh:
    json.dump(doc, fh, indent=2)
    fh.write("\n")
print(f"==> results JSON: {out_path}")

if loss_max_raw:
    limit = float(loss_max_raw)
    offenders = [
        row for row in rows
        if row["unit"] == "MiB/s" and row["loss_pct"] is not None and row["loss_pct"] > limit
    ]
    if offenders:
        names = ", ".join(f"{row['workload']} {row['loss_pct']:.1f}%" for row in offenders)
        raise SystemExit(f"FAIL: sequential loss exceeded LOSS_MAX_PCT={limit}: {names}")
    print(f"==> PASS: sequential loss within LOSS_MAX_PCT={limit}")
PY

echo "==> e2e_sandbox_vhost_cow_perf: OK"