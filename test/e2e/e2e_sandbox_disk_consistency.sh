#!/usr/bin/env bash
#
# e2e_sandbox_disk_consistency.sh — pause/resume stability via repeated
# snapshot → restore cycles with filesystem consistency checks, plus one
# AI-agent style session that exercises guest-visible BlockCOW behavior.
#
# Simulates pause/resume (snapshot destroys the VM; restore resumes it) in a
# loop and verifies that root plus both data disks (single scratch + overlay
# dataset) remain consistent across every cycle:
#   1. cold boot with multi-disk layout (same graph as e2e_sandbox_disks)
#   2. seed deterministic markers and record whole-disk content checksums
#   3. one AI-agent style session (workspace/state/scratch/dataset file ops, then
#      host-side manifest → snapshot/restore → reopen the same explicit diffs)
#   4. snapshot (pause)
#   5. loop PAUSE_RESUME_CYCLES times:
#        restore (resume) → verify markers + whole-disk checksums
#        → every WRITE_EVERY cycles: WRITE_ITERS overwrite R/W passes of a
#          fixed WRITE_MIB file on scratch + dataset, then refresh checksums
#        → snapshot for the next iteration; reclaim the previous snapshot tree
#          and this cycle's host restore diffs
#   6. restore the final snapshot once more and verify consistency
#
# Consistency is checked inside the guest: markers confirm mounts are correctly
# wired, and whole-filesystem tar checksums catch silent content corruption
# across pause/resume.  Periodic write cycles stress scratch + dataset with
# repeated overwrite of a fixed ~750MiB urandom file (write + full read),
# staying under the 1GiB data-disk capacity.
#
# The AI-agent style session exercises guest-visible BlockCOW reads/writes,
# flush, SnapshotView, restore with a new writable diff, and reopening an
# existing explicit diff.
#
# Restore and fresh writable diffs (see fresh_restore_diffs):
#   Snapshot freezes the live disk graph into immutable Sandbox E plus .overlay
#   tarstream artifacts.  On restore the host supplies brand-new empty ext4
#   diff files; those become the new writable uppers while the snapshot layers
#   are read-only bases (from_refs).  Writable state captured at snapshot time
#   lives in the merged .overlay artifacts, not in the old host diff paths.
#
# Prerequisites (checked; missing → skip, exit 0; REQUIRE_KVM=1 to fail hard):
#   /dev/kvm rw · bin/{cloud-hypervisor,sandbox-ctl,sandbox-init,
#   sandbox-runtime.bundle,flatten-ctl,mkfs.erofs} · $VMLINUX · docker (or
#   BLK0_IMAGE=) · mkfs.ext4 · python3 · tar · root (tap/cgroup/vsock).
#   Guest: /bin/sh, python3, sha256sum, sync, tar, dd, stat.
#
# Command line:
#   sudo BIN=/mnt/disk/kuasar/bins PAUSE_RESUME_CYCLES=6 WRITE_EVERY=5 \
#     test/e2e/e2e_sandbox_disk_consistency.sh
#   sudo -E bash test/e2e/e2e_sandbox_disk_consistency.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"

BIN="${BIN:-$REPO_ROOT/bin}"
IMAGE="${IMAGE:-python:3.12-slim}"
TAP_NAME="${TAP_NAME:-sb-tap0}"

# ---- configuration ----
# Data disks (scratch + dataset) are 1GiB.  Each write cycle overwrites a single
# WRITE_MIB urandom file WRITE_ITERS times (write then full read) on both mounts.
# 750MiB leaves headroom under 1GiB for ext4 overhead + persist markers.
if [ -n "${RELEASE_VERSION:-}" ]; then
    PAUSE_RESUME_CYCLES="${PAUSE_RESUME_CYCLES:-15}"
    WRITE_EVERY="${WRITE_EVERY:-5}"
else
    PAUSE_RESUME_CYCLES="${PAUSE_RESUME_CYCLES:-3}"
    WRITE_EVERY="${WRITE_EVERY:-1}"
fi
DATA_DISK_SIZE="${DATA_DISK_SIZE:-1G}"
DATA_DISK_YAML_SIZE="${DATA_DISK_YAML_SIZE:-1GiB}"
WRITE_MIB="${WRITE_MIB:-750}"
WRITE_ITERS="${WRITE_ITERS:-3}"

require_positive_int() { # $1=name $2=value
    case "$2" in
        ''|*[!0-9]*|0) echo "$0: $1 must be a positive integer" >&2; exit 1 ;;
    esac
}

require_positive_int PAUSE_RESUME_CYCLES "$PAUSE_RESUME_CYCLES"
require_positive_int WRITE_EVERY "$WRITE_EVERY"
require_positive_int WRITE_MIB "$WRITE_MIB"
require_positive_int WRITE_ITERS "$WRITE_ITERS"
if [ "$WRITE_MIB" -ge 1024 ]; then
    echo "$0: WRITE_MIB ($WRITE_MIB) must be < 1024 to fit under ${DATA_DISK_YAML_SIZE}" >&2
    exit 1
fi

HR="------------------------------------------------------------------------------------------------"
OK="[✓]"

pass() { echo "$OK $*"; }
section() {
    echo
    echo "$*"
    echo "$HR"
}

skip() {
    echo
    echo "SKIPPED: e2e_sandbox_disk_consistency: skipping ($*)"
    [ "${REQUIRE_KVM:-0}" = "1" ] && { echo "REQUIRE_KVM=1 set; failing" >&2; exit 1; }
    exit 0
}

[ -e /dev/kvm ] || skip "/dev/kvm not present"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || skip "/dev/kvm not accessible"
for b in cloud-hypervisor sandbox-ctl sandbox-init sandbox-runtime.bundle flatten-ctl mkfs.erofs; do
    [ -e "$BIN/$b" ] || skip "missing $BIN/$b"
done
VMLINUX="${VMLINUX:-$BIN/vmlinux}"
[ -f "$VMLINUX" ] || skip "no vmlinux at $VMLINUX"
command -v mkfs.ext4 >/dev/null 2>&1 || skip "mkfs.ext4 not on PATH (apt install e2fsprogs)"
command -v python3 >/dev/null 2>&1 || skip "python3 not on PATH"
command -v tar >/dev/null 2>&1 || skip "tar not on PATH"

if [ "$(id -u)" -ne 0 ]; then exec sudo -nE "$0" "$@"; fi

WORK=$(mktemp -d /tmp/e2e-disk-consistency-XXXXXX)
RR=$WORK/runtime
mkdir -p "$RR"
MANIFEST="$WORK/manifest.tsv"
AGENT_MANIFEST="$WORK/agent-manifest.json"
P=0
TAP_CREATED=0
CURRENT_SNAP=""
LAST_DUMP_MS=""
LAST_MEM_MIB=""
ROOT_DIFF_PATH=""
SCRATCH_DIFF_PATH=""
DATASET_DIFF_PATH=""

cleanup() {
    set +e
    stop_sandbox
    pkill -KILL -f "cloud-hypervisor.*dk-consistency-" 2>/dev/null
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    if [ -n "${E2E_KEEP:-}" ]; then
        echo "kept: $WORK"
    else
        rm -rf "$WORK"
    fi
}
trap cleanup EXIT

if ! ip link show "$TAP_NAME" >/dev/null 2>&1; then
    ip tuntap add dev "$TAP_NAME" mode tap
    ip addr add 169.254.1.0/31 dev "$TAP_NAME"
    ip link set "$TAP_NAME" up
    TAP_CREATED=1
fi

# Update the manifest with a new disk content checksum.
manifest_upsert() { # $1=disk-name $2=sha256
    python3 - "$MANIFEST" "$1" "$2" <<'PY'
import sys

manifest, disk, digest = sys.argv[1:4]
lines = []
found = False
try:
    with open(manifest, encoding="utf-8") as source:
        for line in source:
            entry_disk, _, entry_hash = line.rstrip("\n").partition("\t")
            if entry_disk == disk:
                lines.append(f"{disk}\t{digest}\n")
                found = True
            else:
                lines.append(line if line.endswith("\n") else line + "\n")
except FileNotFoundError:
    pass
if not found:
    lines.append(f"{disk}\t{digest}\n")
with open(manifest, "w", encoding="utf-8") as sink:
    sink.writelines(lines)
PY
}

# Root is trickier than /scratch or /data for whole-filesystem checksums:
#   - / is the full OS tree (large) rather than a small dedicated test mount.
#   - Root is an overlay merge (erofs base + ext4 upper); tar must capture the
#     merged view, not just the host diff file or /dev/vdb alone.
#   - Unrelated runtime writes (logs, temp files) can land on / between cycles.
# We tar only the root filesystem (--one-file-system skips /scratch and /data
# because they are separate mounts), save owners as numbers (--numeric-owner),
# exclude volatile pseudo-fs paths
# (--sort=name --mtime=@0 --clamp-mtime) for reproducible hashes.
ROOT_IMAGE_TAR='sync; tar -C / --one-file-system --numeric-owner --sort=name --mtime=@0 --clamp-mtime \
    --exclude=./proc --exclude=./sys --exclude=./dev --exclude=./run --exclude=./tmp \
    -cf - . | sha256sum | awk "{print \$1}"'
SCRATCH_IMAGE_TAR='sync; tar -C /scratch --one-file-system --numeric-owner --sort=name --mtime=@0 --clamp-mtime \
    -cf - . | sha256sum | awk "{print \$1}"'
DATASET_IMAGE_TAR='sync; tar -C /data --one-file-system --numeric-owner --sort=name --mtime=@0 --clamp-mtime \
    -cf - . | sha256sum | awk "{print \$1}"'

# Transient guest-exec channel failures (vsock EOF, etc.).
# Real content mismatches are never retried.
is_exec_channel_error() { # $1=output-file
    grep -Eqi 'exec_ack: EOF|connection reset|broken pipe|i/o timeout|temporary failure' "$1" 2>/dev/null
}

# Run a guest checksum tar command with up to 2 retries on channel errors
# (3 attempts total).
guest_exec_disk_hash() { # $1=sid $2=disk $3=tar_cmd $4=out_file
    local sid=$1 disk=$2 tar_cmd=$3 out=$4
    local attempt=1 max_attempts=3 rc actual

    while [ "$attempt" -le "$max_attempts" ]; do
        set +e
        "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" -- /bin/sh -c "$tar_cmd" \
            >"$out" 2>&1
        rc=$?
        set -e

        if [ "$rc" -eq 0 ]; then
            actual=$(awk 'NF {print; exit}' "$out")
            if [[ "$actual" =~ ^[0-9a-f]{64}$ ]]; then
                printf '%s\n' "$actual"
                return 0
            fi
            echo "FAIL: invalid $disk checksum output" >&2
            sed 's/^/    /' "$out" >&2
            return 1
        fi

        if [ "$attempt" -lt "$max_attempts" ] && is_exec_channel_error "$out"; then
            echo "    retry $attempt/$max_attempts: exec channel error while checksumming $disk; retrying..." >&2
            sleep 1
            attempt=$((attempt + 1))
            continue
        fi

        echo "FAIL: guest exec failed while checksumming $disk (rc=$rc, attempt=$attempt/$max_attempts)" >&2
        if is_exec_channel_error "$out"; then
            echo "    cause: exec/channel error (not a confirmed content mismatch)" >&2
        fi
        sed 's/^/    /' "$out" >&2
        return 1
    done
}

# Deterministic whole-filesystem checksum.  Catches silent corruption across
# pause/resume even when the guest still boots and marker reads succeed.
guest_disk_image_hash() { # $1=sid $2=disk-name (root|scratch|dataset)
    local sid=$1 disk=$2 tar_cmd
    local out="$WORK/disk-hash-${disk}.out"

    case "$disk" in
        root)    tar_cmd=$ROOT_IMAGE_TAR ;;
        scratch) tar_cmd=$SCRATCH_IMAGE_TAR ;;
        dataset) tar_cmd=$DATASET_IMAGE_TAR ;;
        *) echo "FAIL: unknown disk $disk" >&2; exit 1 ;;
    esac
    guest_exec_disk_hash "$sid" "$disk" "$tar_cmd" "$out" \
        || { echo "FAIL: could not checksum $disk"; exit 1; }
}

manifest_record_disk_images() { # $1=sid [$2=all|data-only]
    local sid=$1 scope=${2:-all} disk hash

    for disk in scratch dataset; do
        hash=$(guest_disk_image_hash "$sid" "$disk")
        [[ "$hash" =~ ^[0-9a-f]{64}$ ]] || { echo "FAIL: invalid $disk checksum"; exit 1; }
        manifest_upsert "$disk" "$hash"
    done
    if [ "$scope" = all ]; then
        hash=$(guest_disk_image_hash "$sid" root)
        [[ "$hash" =~ ^[0-9a-f]{64}$ ]] || { echo "FAIL: invalid root checksum"; exit 1; }
        manifest_upsert root "$hash"
    fi
}

# Recompute recorded whole-disk checksums inside the guest and verify against
# the manifest.  Channel errors are retried twice; a real hash mismatch fails
# immediately.
verify_disk_image_manifest() { # $1=sid $2=label [$3=all|data-only]
    local sid=$1 label=$2 scope=${3:-all}
    local disk expected actual

    [ -f "$MANIFEST" ] || { echo "FAIL: missing manifest"; exit 1; }
    while IFS=$'\t' read -r disk expected; do
        [ -n "$disk" ] || continue
        if [ "$scope" = data-only ] && [ "$disk" = root ]; then
            continue
        fi
        case "$disk" in
            root|scratch|dataset) ;;
            *) echo "FAIL: unknown manifest disk $disk" >&2; exit 1 ;;
        esac
        actual=$(guest_disk_image_hash "$sid" "$disk") \
            || { echo "FAIL: could not verify $disk after $label"; exit 1; }
        if [ "$actual" != "$expected" ]; then
            echo "FAIL: disk checksum mismatch for $disk after $label"
            echo "    want: $expected"
            echo "    got:  $actual"
            exit 1
        fi
    done < "$MANIFEST"
}

ready() { # $1=sid
    local sid=$1
    local out="$WORK/ready-${sid}.out"

    for _ in $(seq 1 90); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" -- /bin/sh -c 'echo R' \
            >"$out" 2>/dev/null && grep -q R "$out"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

wait_sandbox() {
    [ -n "$P" ] && [ "$P" != 0 ] || return 0
    wait "$P" 2>/dev/null || true
    P=0
}

# Restore host yaml supplies fresh writable uppers only.  Snapshot Sandbox E
# owns the immutable disk graph (bases + captured overlay artifacts).
write_restore_yaml() { # $1=out $2=hostname $3=root $4=dataset
    cat > "$1" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: $2 }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: { diff: file://$3 }
  disks:
    - { name: scratch }
    - { name: dataset, overlay: { diff: file://$4 } }
EOF
}

# Each restore binds new empty ext4 files as the writable uppers for root and
# dataset.  Scratch is fully described by the snapshot graph (same as
# e2e_sandbox_disks); binding a fresh scratch diff here breaks restore after
# scratch writes.
fresh_restore_diffs() { # $1=tag
    local tag=$1

    ROOT_DIFF="$WORK/root-${tag}.ext4"
    DATASET_DIFF="$WORK/dataset-${tag}.ext4"
    truncate -s 512M "$ROOT_DIFF"
    mkfs.ext4 -q -F "$ROOT_DIFF"
    truncate -s "$DATA_DISK_SIZE" "$DATASET_DIFF"
    mkfs.ext4 -q -F "$DATASET_DIFF"
}

# Sets CURRENT_SNAP, LAST_DUMP_MS, LAST_MEM_MIB.
snapshot_sandbox() { # $1=sid $2=output-dir
    local sid=$1 outdir=$2
    local log="$WORK/snap-$(basename "$outdir").log"

    mkdir -p "$outdir"
    "$BIN/sandbox-ctl" snapshot --sandbox-id "$sid" --output "$outdir" --run-root "$RR" \
        >"$log" 2>&1 \
        || { echo "FAIL: snapshot $sid"; sed 's/^/    /' "$log"; exit 1; }
    wait_sandbox
    CURRENT_SNAP="$outdir/$sid.snapshot"
    [ -f "$CURRENT_SNAP" ] || { echo "FAIL: no snapshot at $CURRENT_SNAP"; ls -la "$outdir"; exit 1; }

    LAST_DUMP_MS=$(awk '/dump_ms=/ {
        for (i = 1; i <= NF; i++) if ($i ~ /^dump_ms=/) { split($i, a, "="); print a[2]; exit }
    }' "$log")
    LAST_MEM_MIB=$(awk '/memory_size=/ {
        for (i = 1; i <= NF; i++) if ($i ~ /^memory_size=/) {
            split($i, a, "="); print int(a[2] / 1024 / 1024); exit
        }
    }' "$log")
    [ -n "$LAST_DUMP_MS" ] || LAST_DUMP_MS="?"
    [ -n "$LAST_MEM_MIB" ] || LAST_MEM_MIB="?"
}

# Drop a prior snapshot tree once a newer CURRENT_SNAP exists.  Keeps only the
# live restore baseline so cycle overlays / memory dumps do not accumulate.
reclaim_snapshot_dir() { # $1=dir
    local dir=$1
    [ -n "$dir" ] || return 0
    [ -d "$dir" ] || return 0
    case "$CURRENT_SNAP" in
        "$dir"/*) return 0 ;;  # still the live baseline
    esac
    rm -rf "$dir"
}

# Drop host-bound restore diffs for a finished cycle tag.  Safe after snapshot
# (VM destroyed); those empty uppers are not part of the next restore baseline.
reclaim_restore_diffs() { # $1=tag
    local tag=$1
    [ -n "$tag" ] || return 0
    rm -f "$WORK/root-${tag}.ext4" "$WORK/dataset-${tag}.ext4"
}

restore_sandbox() { # $1=snapshot $2=sid $3=tag
    local snap=$1 sid=$2 tag=$3
    local log="$WORK/run-${tag}.log"

    fresh_restore_diffs "$tag"
    write_restore_yaml "$WORK/restore-${tag}.yaml" "e2e-consistency-${tag}" \
        "$ROOT_DIFF" "$DATASET_DIFF"
    timeout -k 10s 120 "$BIN/sandbox-ctl" run --restore "$snap" \
        --config "$WORK/restore-${tag}.yaml" --sandbox-id "$sid" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" >"$log" 2>&1 &
    P=$!
    ready "$sid" || { echo "FAIL: restore not ready ($sid)"; tail -60 "$log"; exit 1; }
}

# Stress scratch + dataset with WRITE_ITERS overwrite passes of a fixed WRITE_MIB
# urandom file on each mount (write → full read → size check).  Same paths every
# time so capacity stays at one ~WRITE_MIB file per disk plus markers.
write_cycle_data() { # $1=sid $2=cycle
    local sid=$1 cycle=$2
    local out="$WORK/write-cycle-${cycle}.out"
    local want_bytes=$((WRITE_MIB * 1024 * 1024))

    echo "    Performing intensive reads/writes: ${WRITE_ITERS}x${WRITE_MIB}MiB on scratch + dataset (this may take a while)..."
    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" -- /bin/sh -c '
        set -e
        cycle=$1
        iters=$2
        mib=$3
        want_bytes=$4
        i=1
        while [ "$i" -le "$iters" ]; do
            for path in /scratch/bulk.dat /data/bulk.dat; do
                # Intensive Write stress
                dd if=/dev/urandom of="$path" bs=1M count="$mib" status=none
                # Intensive Read stress
                dd if="$path" of=/dev/null bs=1M status=none
                sz=$(stat -c %s "$path")
                [ "$sz" = "$want_bytes" ] || {
                    echo "size mismatch on $path after iter $i: got $sz want $want_bytes" >&2
                    exit 1
                }
            done
            i=$((i + 1))
        done
        sync
        echo "OK iters=$iters mib=$mib cycle=$cycle"
    ' sh "$cycle" "$WRITE_ITERS" "$WRITE_MIB" "$want_bytes" >"$out" 2>&1 \
        || { echo "FAIL: bulk R/W stress failed on cycle $cycle"; sed 's/^/    /' "$out"; exit 1; }

    grep -q "^OK iters=${WRITE_ITERS} mib=${WRITE_MIB} cycle=${cycle}$" "$out" \
        || { echo "FAIL: missing bulk R/W completion marker on cycle $cycle"; sed 's/^/    /' "$out"; exit 1; }
    manifest_record_disk_images "$sid" data-only
}

# Quick marker reads: wrong mount, missing disk, or overlay stack not wired.
verify_markers() { # $1=sid $2=label
    local sid=$1 label=$2
    local out="$WORK/markers-${label}.out"

    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" -- /bin/sh -c \
        'cat /kuasar-e2e/persist /scratch/persist /data/persist /data/DATASET-OK' >"$out" 2>&1 \
        || { echo "FAIL: marker read failed ($label)"; cat "$out"; exit 1; }
    for marker in ROOT-OK S-OK D-OK DATASET-OK; do
        grep -q "$marker" "$out" || { echo "FAIL: missing $marker after $label"; cat "$out"; exit 1; }
    done
}

# Post-boot / post-restore check:
#   markers               → mounts wired (incl. /scratch/persist)
#   disk content manifest → whole-filesystem checksums for root + scratch + dataset
verify_consistency() { # $1=sid $2=label
    local sid=$1 label=$2
    verify_markers "$sid" "$label"
    verify_disk_image_manifest "$sid" "$label"
}

stop_sandbox() {
    if [ -n "$P" ] && [ "$P" != 0 ] && kill -0 "$P" 2>/dev/null; then
        kill -TERM "$P" 2>/dev/null
        for _ in $(seq 1 50); do
            kill -0 "$P" 2>/dev/null || break
            sleep 0.1
        done
        if kill -0 "$P" 2>/dev/null; then
            disown "$P" 2>/dev/null
            kill -KILL "$P" 2>/dev/null
        fi
        wait "$P" 2>/dev/null
    fi
    P=0
}

write_cold_yaml() { # $1=out $2=hostname
    cat > "$1" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: $2 }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay: { diff: file://$ROOT_DIFF_PATH }
  disks:
    - { name: scratch, diff: file://$SCRATCH_DIFF_PATH }
    - { name: dataset, base: $DATASET_REF, overlay: { diff: file://$DATASET_DIFF_PATH } }
mounts:
  - { target: /scratch, type: disk, source: scratch }
  - { target: /data,    type: disk, source: dataset }
launch: { exec: /bin/sleep, args: ["3600"] }
EOF
}

# Cold-boot (or reopen) with the explicit root/scratch/dataset diff paths.
# Existing non-empty diffs are reopened via PrepareDiff Existing=true.
boot_explicit_diffs() { # $1=sid $2=hostname $3=log-tag
    local sid=$1 hostname=$2 tag=$3
    local log="$WORK/run-${tag}.log"

    write_cold_yaml "$WORK/cold-${tag}.yaml" "$hostname"
    timeout -k 10s 120 "$BIN/sandbox-ctl" run --config "$WORK/cold-${tag}.yaml" \
        --sandbox-id "$sid" --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" \
        >"$log" 2>&1 &
    P=$!
    ready "$sid" || { echo "FAIL: boot not ready ($sid / $tag)"; tail -60 "$log"; exit 1; }
}

# Guest AI-agent session: workspace layout, scratch zero-read, sparse mutation,
# sqlite/jsonl state, cache atomic replace, and dataset base whiteouts.
run_agent_session() { # $1=sid
    local sid=$1
    local out="$WORK/agent-session.out"
    local script="$WORK/agent-session.py"

    cat > "$script" <<'PY'
import json
import os
import shutil
import sqlite3
import sys

def fail(msg: str) -> None:
    print(f"FAIL: {msg}", file=sys.stderr)
    raise SystemExit(1)

# 1. Workspace layout: source dir, config, empty file, 16 KiB sparse, symlink.
os.makedirs("/workspace/src", exist_ok=True)
with open("/workspace/src/main.py", "w", encoding="utf-8") as source:
    source.write("print('agent')\n")
with open("/workspace/config.toml", "w", encoding="utf-8") as config:
    config.write('mode = "agent"\n')
open("/workspace/empty", "wb").close()
sparse_path = "/workspace/sparse.bin"
fd = os.open(sparse_path, os.O_CREAT | os.O_RDWR | os.O_TRUNC, 0o644)
try:
    os.ftruncate(fd, 16 * 1024)
finally:
    os.close(fd)
if os.path.lexists("/workspace/config.link"):
    os.unlink("/workspace/config.link")
os.symlink("config.toml", "/workspace/config.link")

# 2. Untouched 4 KiB region on /scratch must read as zeros.
scratch_path = "/scratch/untouched.bin"
fd = os.open(scratch_path, os.O_CREAT | os.O_RDWR | os.O_TRUNC, 0o644)
try:
    os.ftruncate(fd, 8 * 1024)
    got = os.pread(fd, 4096, 0)
finally:
    os.close(fd)
if got != b"\x00" * 4096:
    fail(f"scratch untouched region not zeros (len={len(got)})")

# 3. Mutate the sparse file across block boundaries, then truncate/extend.
fd = os.open(sparse_path, os.O_RDWR)
try:
    os.lseek(fd, 0, os.SEEK_END)
    os.write(fd, b"APPEND-MARK")
    os.pwrite(fd, b"X" * 1024, 3584)
    os.pwrite(fd, b"\x00" * 4096, 8192)
    os.ftruncate(fd, 4097)
    os.ftruncate(fd, 16 * 1024)
    os.fsync(fd)
finally:
    os.close(fd)
size = os.stat(sparse_path).st_size
if size != 16 * 1024:
    fail(f"sparse.bin size={size}, want 16384")

# 4. Session state: sqlite sequence 1..100 + fsynced JSONL records.
os.makedirs("/state", exist_ok=True)
db_path = "/state/session.db"
if os.path.exists(db_path):
    os.unlink(db_path)
conn = sqlite3.connect(db_path)
try:
    conn.execute("CREATE TABLE seq (n INTEGER PRIMARY KEY)")
    conn.executemany("INSERT INTO seq(n) VALUES (?)", [(i,) for i in range(1, 101)])
    conn.commit()
finally:
    conn.close()

jsonl_path = "/state/session.jsonl"
with open(jsonl_path, "w", encoding="utf-8") as jsonl:
    for i in range(1, 101):
        jsonl.write(json.dumps({"seq": i, "kind": "session"}) + "\n")
    jsonl.flush()
    os.fsync(jsonl.fileno())

# 5. Cache + executable with fsync-then-rename publish.
os.makedirs("/workspace/cache", exist_ok=True)
os.makedirs("/workspace/bin", exist_ok=True)
with open("/workspace/cache/model.cache", "w", encoding="utf-8") as cache:
    cache.write("cache-v1\n")
tmp_cache = "/workspace/cache/model.cache.tmp"
with open(tmp_cache, "w", encoding="utf-8") as cache:
    cache.write("cache-v2\n")
    cache.flush()
    os.fsync(cache.fileno())
os.replace(tmp_cache, "/workspace/cache/model.cache")
exe_path = "/workspace/bin/tool"
with open(exe_path, "w", encoding="utf-8") as exe:
    exe.write("#!/bin/sh\necho TOOL-OK\n")
os.chmod(exe_path, 0o755)

# 6. Delete pre-seeded dataset base paths; recreate base-file as a directory.
if not os.path.isfile("/data/base-file"):
    fail("missing pre-seeded /data/base-file")
if not os.path.isdir("/data/base-dir"):
    fail("missing pre-seeded /data/base-dir")
os.unlink("/data/base-file")
shutil.rmtree("/data/base-dir")
# Re-using base-file as a directory name.
os.makedirs("/data/base-file", exist_ok=True)
with open("/data/base-file/replacement", "w", encoding="utf-8") as replacement:
    replacement.write("replacement-dir\n")

for path in ("/workspace", "/state", "/scratch", "/data"):
    dir_fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(dir_fd)
    finally:
        os.close(dir_fd)
os.sync()

print("AGENT-SESSION-OK")
PY

    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" \
        --stdin-from "$script" -- python3 - >"$out" 2>&1 \
        || { echo "FAIL: AI-agent style session failed"; sed 's/^/    /' "$out"; exit 1; }

    grep -q '^AGENT-SESSION-OK$' "$out" \
        || { echo "FAIL: missing AGENT-SESSION-OK"; sed 's/^/    /' "$out"; exit 1; }
}

# Capture a host-side manifest of the agent-visible guest state.
capture_agent_manifest() { # $1=sid $2=expected-jsonl-count
    local sid=$1 expect=$2
    local out="$WORK/agent-manifest-capture.out"
    local script="$WORK/agent-manifest-capture.py"

    cat > "$script" <<'PY'
import hashlib
import json
import os
import sqlite3
import stat
import sys

expect = int(sys.argv[1])

def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()

def entry(path: str) -> dict:
    st = os.lstat(path)
    item = {
        "path": path,
        "mode": stat.filemode(st.st_mode),
        "size": st.st_size,
        "is_dir": stat.S_ISDIR(st.st_mode),
        "is_lnk": stat.S_ISLNK(st.st_mode),
        "is_reg": stat.S_ISREG(st.st_mode),
        "link": os.readlink(path) if stat.S_ISLNK(st.st_mode) else None,
        "sha256": None,
    }
    if item["is_reg"] and not item["is_lnk"]:
        item["sha256"] = sha256_file(path)
    return item

paths = [
    "/workspace/src",
    "/workspace/src/main.py",
    "/workspace/config.toml",
    "/workspace/empty",
    "/workspace/sparse.bin",
    "/workspace/config.link",
    "/workspace/cache/model.cache",
    "/workspace/bin/tool",
    "/scratch/untouched.bin",
    "/state/session.db",
    "/state/session.jsonl",
    "/data/base-file",
    "/data/base-file/replacement",
]
manifest = {
    "expect_seq": expect,
    "entries": [entry(path) for path in paths],
    "sqlite_integrity": None,
    "sqlite_max": None,
    "sqlite_count": None,
    "jsonl_seqs": [],
    "absent": ["/data/base-dir", "/data/base-dir/entry"],
}

conn = sqlite3.connect("/state/session.db")
try:
    integrity = conn.execute("PRAGMA integrity_check").fetchone()[0]
    maximum, count = conn.execute("SELECT MAX(n), COUNT(*) FROM seq").fetchone()
finally:
    conn.close()
manifest["sqlite_integrity"] = integrity
manifest["sqlite_max"] = maximum
manifest["sqlite_count"] = count

with open("/state/session.jsonl", encoding="utf-8") as jsonl:
    for line in jsonl:
        line = line.strip()
        if not line:
            continue
        manifest["jsonl_seqs"].append(json.loads(line)["seq"])

if manifest["sqlite_integrity"] != "ok":
    raise SystemExit(f"sqlite integrity_check={manifest['sqlite_integrity']}")
if manifest["sqlite_count"] != expect or manifest["sqlite_max"] != expect:
    raise SystemExit(
        f"sqlite count/max={manifest['sqlite_count']}/{manifest['sqlite_max']} want {expect}"
    )
if manifest["jsonl_seqs"] != list(range(1, expect + 1)):
    raise SystemExit(f"jsonl seqs mismatch: {manifest['jsonl_seqs'][:5]}...")

if not os.path.isdir("/data/base-file"):
    raise SystemExit("/data/base-file should be a directory after agent rewrite")
if os.path.exists("/data/base-dir"):
    raise SystemExit("/data/base-dir should be absent after agent delete")

print(json.dumps(manifest, sort_keys=True))
PY

    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" \
        --stdin-from "$script" -- python3 - "$expect" >"$out" 2>&1 \
        || { echo "FAIL: could not capture agent manifest"; sed 's/^/    /' "$out"; exit 1; }

    python3 -c 'import json,sys; json.load(open(sys.argv[1],encoding="utf-8"))' "$out" \
        || { echo "FAIL: agent manifest is not JSON"; sed 's/^/    /' "$out"; exit 1; }
    cp "$out" "$AGENT_MANIFEST"
}

# Verify agent-visible state against the host-side manifest (+ optional seq bump).
# Content/size/link checks use the snapshotted generation; when expect_seq is
# raised (reopen + append), only structural + sqlite/jsonl sequence checks apply
# to the mutated session files.
verify_agent_session() { # $1=sid $2=label [$3=expect-seq]
    local sid=$1 label=$2
    local expect=${3:-}
    local out="$WORK/agent-verify-${label}.out"
    local script="$WORK/agent-verify-guest.py"

    if [ -z "$expect" ]; then
        expect=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1],encoding="utf-8"))["expect_seq"])' \
            "$AGENT_MANIFEST")
    fi

    python3 - "$AGENT_MANIFEST" "$expect" "$script" <<'PY'
import json
import pathlib
import sys

manifest = json.load(open(sys.argv[1], encoding="utf-8"))
expect = int(sys.argv[2])
out = pathlib.Path(sys.argv[3])
out.write_text(
    "import hashlib\n"
    "import json\n"
    "import os\n"
    "import sqlite3\n"
    "import stat\n"
    f"manifest = json.loads({json.dumps(json.dumps(manifest))})\n"
    f"expect = {expect}\n"
    "baseline = int(manifest['expect_seq'])\n"
    "\n"
    "def sha256_file(path):\n"
    "    h = hashlib.sha256()\n"
    "    with open(path, 'rb') as source:\n"
    "        for chunk in iter(lambda: source.read(1024 * 1024), b''):\n"
    "            h.update(chunk)\n"
    "    return h.hexdigest()\n"
    "\n"
    "for path in manifest.get('absent', []):\n"
    "    if os.path.lexists(path):\n"
    "        raise SystemExit(f'path should be absent: {path}')\n"
    "\n"
    "for item in manifest['entries']:\n"
    "    path = item['path']\n"
    "    if not os.path.lexists(path):\n"
    "        raise SystemExit(f'missing path: {path}')\n"
    "    st = os.lstat(path)\n"
    "    if bool(stat.S_ISDIR(st.st_mode)) != bool(item['is_dir']):\n"
    "        raise SystemExit(f'dir mismatch for {path}')\n"
    "    if bool(stat.S_ISLNK(st.st_mode)) != bool(item['is_lnk']):\n"
    "        raise SystemExit(f'link mismatch for {path}')\n"
    "    if item['is_lnk']:\n"
    "        got = os.readlink(path)\n"
    "        if got != item['link']:\n"
    "            raise SystemExit(f'link target {path}: got {got!r} want {item[\"link\"]!r}')\n"
    "        continue\n"
    "    if item['is_reg']:\n"
    "        check_bytes = expect == baseline or path not in (\n"
    "            '/state/session.db',\n"
    "            '/state/session.jsonl',\n"
    "        )\n"
    "        if check_bytes and st.st_size != item['size']:\n"
    "            raise SystemExit(f'size {path}: got {st.st_size} want {item[\"size\"]}')\n"
    "        if check_bytes and item['sha256'] is not None:\n"
    "            got = sha256_file(path)\n"
    "            if got != item['sha256']:\n"
    "                raise SystemExit(f'hash {path}: got {got} want {item[\"sha256\"]}')\n"
    "        if path == '/workspace/bin/tool' and (st.st_mode & 0o111) == 0:\n"
    "            raise SystemExit('tool is not executable')\n"
    "\n"
    "conn = sqlite3.connect('/state/session.db')\n"
    "try:\n"
    "    integrity = conn.execute('PRAGMA integrity_check').fetchone()[0]\n"
    "    maximum, count = conn.execute('SELECT MAX(n), COUNT(*) FROM seq').fetchone()\n"
    "finally:\n"
    "    conn.close()\n"
    "if integrity != 'ok':\n"
    "    raise SystemExit(f'sqlite integrity_check={integrity}')\n"
    "if count != expect or maximum != expect:\n"
    "    raise SystemExit(f'sqlite count/max={count}/{maximum} want {expect}')\n"
    "\n"
    "seqs = []\n"
    "with open('/state/session.jsonl', encoding='utf-8') as jsonl:\n"
    "    for line in jsonl:\n"
    "        line = line.strip()\n"
    "        if line:\n"
    "            seqs.append(json.loads(line)['seq'])\n"
    "if seqs != list(range(1, expect + 1)):\n"
    "    raise SystemExit(f'jsonl seqs mismatch want 1..{expect} got {seqs[:5]}...')\n"
    "\n"
    "if not os.path.isdir('/data/base-file'):\n"
    "    raise SystemExit('/data/base-file should remain a directory')\n"
    "if os.path.exists('/data/base-dir'):\n"
    "    raise SystemExit('/data/base-dir should remain absent')\n"
    "print('AGENT-VERIFY-OK')\n",
    encoding="utf-8",
)
PY

    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" \
        --stdin-from "$script" -- python3 - >"$out" 2>&1 \
        || { echo "FAIL: agent verify failed after $label"; sed 's/^/    /' "$out"; exit 1; }

    grep -q '^AGENT-VERIFY-OK$' "$out" \
        || { echo "FAIL: missing AGENT-VERIFY-OK after $label"; sed 's/^/    /' "$out"; exit 1; }
}

append_agent_session_record() { # $1=sid $2=seq
    local sid=$1 seq=$2
    local out="$WORK/agent-append-${seq}.out"
    local script="$WORK/agent-append.py"

    cat > "$script" <<'PY'
import json
import os
import sqlite3
import sys

seq = int(sys.argv[1])
conn = sqlite3.connect("/state/session.db")
try:
    conn.execute("INSERT INTO seq(n) VALUES (?)", (seq,))
    conn.commit()
finally:
    conn.close()

with open("/state/session.jsonl", "a", encoding="utf-8") as jsonl:
    jsonl.write(json.dumps({"seq": seq, "kind": "session"}) + "\n")
    jsonl.flush()
    os.fsync(jsonl.fileno())

dir_fd = os.open("/state", os.O_RDONLY)
try:
    os.fsync(dir_fd)
finally:
    os.close(dir_fd)
print("AGENT-APPEND-OK")
PY

    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" \
        --stdin-from "$script" -- python3 - "$seq" >"$out" 2>&1 \
        || { echo "FAIL: could not append session record $seq"; sed 's/^/    /' "$out"; exit 1; }

    grep -q '^AGENT-APPEND-OK$' "$out" \
        || { echo "FAIL: missing AGENT-APPEND-OK for seq $seq"; sed 's/^/    /' "$out"; exit 1; }
}

# =============================================================================
section "1.  Starting Sandbox Disk Consistency Test [Cycles: $PAUSE_RESUME_CYCLES | Write Interval: $WRITE_EVERY | Bulk: ${WRITE_ITERS}x${WRITE_MIB}MiB]"

# ---- artifacts: root base + overlay upper, single data disk, overlay data disk ----
BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker not available; set BLK0_IMAGE="
    docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null
    BLK0_IMAGE="$WORK/root.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
truncate -s 512M "$WORK/root-up.ext4"; mkfs.ext4 -q -F "$WORK/root-up.ext4"
truncate -s "$DATA_DISK_SIZE" "$WORK/scratch.ext4"; mkfs.ext4 -q -F "$WORK/scratch.ext4"
mkdir -p "$WORK/ds/base-dir"
echo "DATASET-OK" > "$WORK/ds/DATASET-OK"
echo "BASE-FILE" > "$WORK/ds/base-file"
echo "BASE-DIR-ENTRY" > "$WORK/ds/base-dir/entry"
MKFS_EROFS_PATH="$BIN/mkfs.erofs" "$BIN/flatten-ctl" export --no-progress \
    --tmpdir "$WORK/tmp" --output "$WORK/dataset.img" "$WORK/ds"
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"
DATASET_REF="$(plaintext_tarstream_ref "$WORK/dataset.img")"
truncate -s "$DATA_DISK_SIZE" "$WORK/dataset-up.ext4"; mkfs.ext4 -q -F "$WORK/dataset-up.ext4"
ROOT_DIFF_PATH="$WORK/root-up.ext4"
SCRATCH_DIFF_PATH="$WORK/scratch.ext4"
DATASET_DIFF_PATH="$WORK/dataset-up.ext4"
pass "Building disk artifacts (data disks ${DATA_DISK_YAML_SIZE})"

write_cold_yaml "$WORK/cold.yaml" "e2e-consistency-cold"

SID0=dk-consistency-0
timeout -k 10s 120 "$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --sandbox-id "$SID0" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" > "$WORK/run-cold.log" 2>&1 &
P=$!
ready "$SID0" || { echo "FAIL: cold boot not ready"; tail -60 "$WORK/run-cold.log"; exit 1; }
pass "Cold boot multi-disk sandbox"

"$BIN/sandbox-ctl" exec --sandbox-id "$SID0" --run-root "$RR" -- /bin/sh -c \
    'command -v python3 >/dev/null && command -v sha256sum >/dev/null && command -v sync >/dev/null \
     && command -v tar >/dev/null && command -v dd >/dev/null && command -v stat >/dev/null' \
    || { echo "FAIL: guest image must provide python3, sha256sum, sync, tar, dd, and stat"; exit 1; }

"$BIN/sandbox-ctl" exec --sandbox-id "$SID0" --run-root "$RR" -- /bin/sh -c \
    'mkdir -p /kuasar-e2e; echo ROOT-OK > /kuasar-e2e/persist; echo S-OK > /scratch/persist; echo D-OK > /data/persist; sync' \
    >"$WORK/seed.out" 2>&1 \
    || { echo "FAIL: could not seed markers"; cat "$WORK/seed.out"; exit 1; }
: > "$MANIFEST"
manifest_record_disk_images "$SID0"
pass "Seeding initial markers and manifests"

verify_consistency "$SID0" "cold-seed"
pass "Initial verification (root, dataset, scratch)"

# =============================================================================
section "2.  AI-agent style session [workspace/state ops → snapshot/restore → reopen explicit diffs]"

run_agent_session "$SID0"
pass "AI-agent style session file ops complete"

capture_agent_manifest "$SID0" 100
pass "Host-side agent manifest saved"

: > "$MANIFEST"
manifest_record_disk_images "$SID0"
verify_consistency "$SID0" "agent-pre-snapshot"
pass "Pre-snapshot consistency after agent session"

snapshot_sandbox "$SID0" "$WORK/snaps/agent"
pass "Agent snapshot committed (Dump: ${LAST_DUMP_MS}ms)"

SID_AGENT_RESTORE=dk-consistency-agent-restore
restore_sandbox "$CURRENT_SNAP" "$SID_AGENT_RESTORE" "agent-restore"
verify_consistency "$SID_AGENT_RESTORE" "agent-restore"
verify_agent_session "$SID_AGENT_RESTORE" "agent-restore" 100
pass "Agent snapshot restore verified (content, links, sqlite, jsonl)"

stop_sandbox
pkill -KILL -f "cloud-hypervisor.*dk-consistency-agent-restore" 2>/dev/null || true
reclaim_restore_diffs "agent-restore"

SID_AGENT_REOPEN=dk-consistency-agent-reopen
boot_explicit_diffs "$SID_AGENT_REOPEN" "e2e-consistency-reopen" "agent-reopen"
verify_agent_session "$SID_AGENT_REOPEN" "agent-reopen-pre" 100
append_agent_session_record "$SID_AGENT_REOPEN" 101
verify_agent_session "$SID_AGENT_REOPEN" "agent-reopen-post" 101
pass "Reopened explicit diffs remain present and writable"

: > "$MANIFEST"
manifest_record_disk_images "$SID_AGENT_REOPEN"
verify_consistency "$SID_AGENT_REOPEN" "agent-reopen"
snapshot_sandbox "$SID_AGENT_REOPEN" "$WORK/snaps/initial"
pass "Initial pause/resume baseline snapshot committed (Dump: ${LAST_DUMP_MS}ms)"
reclaim_snapshot_dir "$WORK/snaps/agent"

# =============================================================================
section "3.  Running Pause/Resume Cycles [markers + checksums | bulk ${WRITE_ITERS}x${WRITE_MIB}MiB file overwrites]"

for cycle in $(seq 1 "$PAUSE_RESUME_CYCLES"); do
    sid="dk-consistency-${cycle}"
    line="[$cycle/$PAUSE_RESUME_CYCLES]"
    prev_snap_dir=$(dirname "$CURRENT_SNAP")

    restore_sandbox "$CURRENT_SNAP" "$sid" "c${cycle}"
    line+=" -> Restore $OK"

    verify_consistency "$sid" "cycle-${cycle}-restore"
    line+=" | Consistency check $OK"

    if [ $((cycle % WRITE_EVERY)) -eq 0 ]; then
        write_cycle_data "$sid" "$cycle"
        verify_consistency "$sid" "cycle-${cycle}-post-write"
        line+=" | Bulk R/W stress on scratch + dataset"
    fi

    snapshot_sandbox "$sid" "$WORK/snaps/cycle-${cycle}"
    line+=" | Snapshot committed (Dump: ${LAST_DUMP_MS}ms)"

    # Keep only the newest snapshot + reclaim this cycle's host restore diffs.
    reclaim_snapshot_dir "$prev_snap_dir"
    reclaim_restore_diffs "c${cycle}"
    echo "$line"
done

# Verify consistency of the final snapshot
restore_sandbox "$CURRENT_SNAP" "dk-consistency-final" "final"
verify_consistency "dk-consistency-final" "final-restore"
pass "Final snapshot restore + consistency check"

echo
echo "$HR"
echo "SUCCESS: e2e_sandbox_disk_consistency: OK (agent session + ${PAUSE_RESUME_CYCLES} cycles; disk images stable)"
