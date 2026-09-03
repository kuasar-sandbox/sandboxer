#!/usr/bin/env bash
#
# e2e_sandbox_disk_consistency.sh — pause/resume stability via repeated
# snapshot → restore cycles with filesystem consistency checks.
#
# Simulates pause/resume (snapshot destroys the VM; restore resumes it) in a
# loop and verifies that root plus both data disks (single scratch + overlay
# dataset) remain consistent across every cycle:
#   1. cold boot with multi-disk layout (same graph as e2e_sandbox_disks)
#   2. seed deterministic markers and record whole-disk content checksums
#   3. snapshot (pause)
#   4. loop PAUSE_RESUME_CYCLES times:
#        restore (resume) → verify → host e2fsck on all writable diffs
#        → every WRITE_EVERY cycles write new data and refresh disk checksums
#        → snapshot for the next iteration
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
#   BLK0_IMAGE=) · mkfs.ext4 · e2fsck · python3 · tar · root (tap/cgroup/vsock).
#   Guest: /bin/sh, sha256sum, sync, tar.
#
# Command line: 
# 1. sudo BIN=/mnt/disk/kuasar/bins PAUSE_RESUME_CYCLES=6 WRITE_EVERY=3 test/e2e/e2e_sandbox_disk_consistency.sh
# 2. sudo -E bash test/e2e/e2e_sandbox_disk_consistency.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
BIN="${BIN:-$REPO_ROOT/bin}"
IMAGE="${IMAGE:-python:3.12-slim}"
TAP_NAME="${TAP_NAME:-sb-tap0}"

# ---- configuration ----
PAUSE_RESUME_CYCLES="${PAUSE_RESUME_CYCLES:-20}"
WRITE_EVERY="${WRITE_EVERY:-5}"

case "$PAUSE_RESUME_CYCLES" in
    ''|*[!0-9]*|0) echo "$0: PAUSE_RESUME_CYCLES must be a positive integer" >&2; exit 1 ;;
esac
case "$WRITE_EVERY" in
    ''|*[!0-9]*|0) echo "$0: WRITE_EVERY must be a positive integer" >&2; exit 1 ;;
esac

HR="------------------------------------------------------------------------------------"
OK="[✓]"
FAIL_MARK="[✗]"

pass() { echo "$OK $*"; }
fail() { echo "$FAIL_MARK $*"; exit 1; }
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
command -v e2fsck >/dev/null 2>&1 || skip "e2fsck not on PATH (apt install e2fsprogs)"
command -v python3 >/dev/null 2>&1 || skip "python3 not on PATH"
command -v tar >/dev/null 2>&1 || skip "tar not on PATH"

if [ "$(id -u)" -ne 0 ]; then exec sudo -nE "$0" "$@"; fi

WORK=$(mktemp -d /tmp/e2e-disk-consistency-XXXXXX)
RR=$WORK/runtime; mkdir -p "$RR"
# Manifest tracks whole-disk image checksums for scratch + dataset.
MANIFEST="$WORK/manifest.tsv"
P=""; TAP_CREATED=0
CURRENT_SNAP=""; CURRENT_SID=""
LAST_DUMP_MS=""; LAST_MEM_MIB=""

cleanup() {
    set +e
    [ -n "$P" ] && kill -0 "$P" 2>/dev/null && kill -KILL "$P" 2>/dev/null
    pkill -f "cloud-hypervisor.*dk-consistency-" 2>/dev/null
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    [ -n "${E2E_KEEP:-}" ] && echo "kept: $WORK" || rm -rf "$WORK"
}
trap cleanup EXIT

if ! ip link show "$TAP_NAME" >/dev/null 2>&1; then
    ip tuntap add dev "$TAP_NAME" mode tap
    ip addr add 169.254.1.0/31 dev "$TAP_NAME"
    ip link set "$TAP_NAME" up
    TAP_CREATED=1
fi

# Update the manifest with a new disk image checksum.
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

# Root is trickier than /scratch or /data for whole-image checksums:
#   - / is the full OS tree (large) rather than a small dedicated test mount.
#   - Root is an overlay merge (erofs base + ext4 upper); tar must capture the
#     merged view, not just the host diff file or /dev/vdb alone.
#   - Unrelated runtime writes (logs, temp files) can land on / between cycles.
# We tar only the root filesystem (--one-file-system skips /scratch and /data
# because they are separate mounts), (--numeric-owner saves user and group IDs
# as numbers), exclude volatile pseudo-fs paths, and pin tar metadata
# (--sort=name --mtime=@0 --clamp-mtime) for reproducible hashes.
ROOT_IMAGE_TAR='sync; tar -C / --one-file-system --numeric-owner --sort=name --mtime=@0 --clamp-mtime \
    --exclude=./proc --exclude=./sys --exclude=./dev --exclude=./run --exclude=./tmp \
    -cf - . | sha256sum | awk "{print \$1}"'
SCRATCH_IMAGE_TAR='sync; tar -C /scratch --numeric-owner --one-file-system -cf - . | sha256sum | awk "{print \$1}"'
DATASET_IMAGE_TAR='sync; tar -C /data --numeric-owner --one-file-system -cf - . | sha256sum | awk "{print \$1}"'

# Transient guest-exec channel failures (vsock EOF, etc.).  Real content
# mismatches are never retried.
is_exec_channel_error() { # $1=output-file
    grep -Eqi 'exec_ack: EOF|connection reset|broken pipe|i/o timeout|temporary failure' "$1" 2>/dev/null
}

# Run a guest checksum tar command with up to 2 retries on channel errors
# (3 attempts total).  Prints the sha256 digest on stdout.  Non-channel
# failures fail immediately.
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

# Record a deterministic whole-filesystem checksum.  Catches silent data
# corruption / wrong bytes across pause/resume even when the guest still
# boots and marker reads succeed.
guest_disk_image_hash() { # $1=sid $2=disk-name (root|scratch|dataset)
    local sid=$1 disk=$2 tar_cmd
    local out="$WORK/disk-hash-${disk}.out"
    case "$disk" in
        root) tar_cmd=$ROOT_IMAGE_TAR ;;
        scratch) tar_cmd=$SCRATCH_IMAGE_TAR ;;
        dataset) tar_cmd=$DATASET_IMAGE_TAR ;;
        *) echo "FAIL: unknown disk $disk" >&2; exit 1 ;;
    esac
    guest_exec_disk_hash "$sid" "$disk" "$tar_cmd" "$out" \
        || { echo "FAIL: could not checksum $disk disk image"; exit 1; }
}

manifest_record_disk_images() { # $1=sid [$2=root|all]
    local sid=$1 scope=${2:-all} disk hash
    for disk in scratch dataset; do
        hash=$(guest_disk_image_hash "$sid" "$disk")
        [[ "$hash" =~ ^[0-9a-f]{64}$ ]] || { echo "FAIL: invalid $disk disk image checksum"; exit 1; }
        manifest_upsert "$disk" "$hash"
    done
    if [ "$scope" = all ]; then
        hash=$(guest_disk_image_hash "$sid" root)
        [[ "$hash" =~ ^[0-9a-f]{64}$ ]] || { echo "FAIL: invalid root disk image checksum"; exit 1; }
        manifest_upsert root "$hash"
    fi
}

# Recompute recorded whole-disk image checksums inside the guest and verify against the
# manifest. Channel errors are retried twice; a real hash mismatch fails immediately.
verify_disk_image_manifest() { # $1=sid $2=label [$3=all|data-only]
    local sid=$1 label=$2 scope=${3:-all}
    local disk expected tar_cmd out actual
    [ -f "$MANIFEST" ] || { echo "FAIL: missing manifest"; exit 1; }
    while IFS=$'\t' read -r disk expected; do
        [ -n "$disk" ] || continue
        if [ "$scope" = data-only ] && [ "$disk" = root ]; then
            continue
        fi
        case "$disk" in
            root) tar_cmd=$ROOT_IMAGE_TAR ;;
            scratch) tar_cmd=$SCRATCH_IMAGE_TAR ;;
            dataset) tar_cmd=$DATASET_IMAGE_TAR ;;
            *) echo "FAIL: unknown manifest disk $disk" >&2; exit 1 ;;
        esac
        out="$WORK/verify-${label}-${disk}.out"
        actual=$(guest_exec_disk_hash "$sid" "$disk" "$tar_cmd" "$out") \
            || { echo "FAIL: could not verify $disk after $label"; exit 1; }
        if [ "$actual" != "$expected" ]; then
            echo "FAIL: disk image checksum mismatch for $disk after $label"
            echo "    want: $expected"
            echo "    got:  $actual"
            exit 1
        fi
    done < "$MANIFEST"
}

# Host-side ext4 structural check on every host-bound writable diff (root,
# scratch, dataset upper).  Catches corrupt superblocks / inode tables in the
# host image files that back guest block devices.
host_fsck() { # $1=path $2=label
    local path=$1 label=$2
    local out="$WORK/fsck-${label}.out"
    [ -f "$path" ] || { echo "FAIL: missing fsck target $path"; exit 1; }
    if e2fsck -fn "$path" >"$out" 2>&1; then
        return 0
    fi
    echo "FAIL: e2fsck $label ($path)"
    sed 's/^/    /' "$out"
    exit 1
}

# Host e2fsck targets.  Root and dataset always have a host-bound writable
# upper we create for restore.  Scratch is different: restore yaml lists only
# `{ name: scratch }` (same as e2e_sandbox_disks), so sandbox-ctl auto-creates
# a blank CoW upper under --base-root — not a raw mkfs.ext4 we can e2fsck.
# Scratch content after restore is still verified by verify_markers + the
# scratch entry in verify_disk_image_manifest (guest tar of /scratch).
host_fsck_all() { # $1=label $2=root $3=dataset [$4=scratch host path, optional]
    local label=$1 root=$2 dataset=$3 scratch=${4:-}
    host_fsck "$root" "${label}-root"
    host_fsck "$dataset" "${label}-dataset"
    [ -n "$scratch" ] && host_fsck "$scratch" "${label}-scratch"
    return 0
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
    [ -n "$P" ] || return 0
    wait "$P" 2>/dev/null || true
    P=""
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
    overlay: { diff: file://$3, size: 512MiB }
  disks:
    - { name: scratch }
    - { name: dataset, overlay: { diff: file://$4, size: 256MiB } }
EOF
}

# Each restore binds new empty ext4 files as the writable uppers for root and
# dataset.  Scratch is fully described by the snapshot graph (same as
# e2e_sandbox_disks); binding a fresh scratch diff here breaks restore after
# scratch writes, so host fsck for scratch uses the cold-boot image only.
fresh_restore_diffs() { # $1=tag
    local tag=$1
    ROOT_DIFF="$WORK/root-${tag}.ext4"
    DATASET_DIFF="$WORK/dataset-${tag}.ext4"
    truncate -s 512M "$ROOT_DIFF"; mkfs.ext4 -q -F "$ROOT_DIFF"
    truncate -s 256M "$DATASET_DIFF"; mkfs.ext4 -q -F "$DATASET_DIFF"
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
    CURRENT_SID="$sid"
}

write_cycle_data() { # $1=sid $2=cycle
    local sid=$1 cycle=$2
    local out="$WORK/write-cycle-${cycle}.out"
    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" -- /bin/sh -c \
        'cycle="$1"; scratch="/scratch/cycle-${cycle}.dat"; data="/data/cycle-${cycle}.dat"; payload="CYCLE-${cycle}-KUASAR"; printf "%s\n" "$payload" > "$scratch"; printf "%s\n" "$payload" > "$data"; sync' \
        sh "$cycle" >"$out" 2>&1 \
        || { echo "FAIL: could not write cycle $cycle data"; cat "$out"; exit 1; }
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

# Full post-boot / post-restore check:
#   markers              → mounts wired (incl. /scratch/persist)
#   disk image manifest  → whole-disk content for root + scratch + dataset
#   host e2fsck          → structural check on host-bound root/dataset uppers;
#                          scratch host image only on cold boot (see host_fsck_all)
verify_and_fsck() { # $1=sid $2=label $3=root $4=dataset [$5=scratch host path]
    local sid=$1 label=$2 root=$3 dataset=$4 scratch=${5:-}
    verify_markers "$sid" "$label"
    verify_disk_image_manifest "$sid" "$label"
    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RR" -- sync \
        || { echo "FAIL: guest sync before fsck ($label)"; exit 1; }
    host_fsck_all "$label" "$root" "$dataset" "$scratch"
}

# =============================================================================
section "1.  Starting Sandbox Disk Consistency Test [Cycles: $PAUSE_RESUME_CYCLES | Write Interval: $WRITE_EVERY]"

# ---- artifacts: root base + overlay upper, single data disk, overlay data disk ----
BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker not available; set BLK0_IMAGE="
    docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null
    BLK0_IMAGE="$WORK/root.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
truncate -s 512M "$WORK/root-up.ext4"; mkfs.ext4 -q -F "$WORK/root-up.ext4"
truncate -s 256M "$WORK/scratch.ext4"; mkfs.ext4 -q -F "$WORK/scratch.ext4"
mkdir -p "$WORK/ds"; echo "DATASET-OK" > "$WORK/ds/DATASET-OK"
MKFS_EROFS_PATH="$BIN/mkfs.erofs" "$BIN/flatten-ctl" export --no-progress \
    --tmpdir "$WORK/tmp" --output "$WORK/dataset.img" "$WORK/ds"
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"
DATASET_REF="$(plaintext_tarstream_ref "$WORK/dataset.img")"
truncate -s 256M "$WORK/dataset-up.ext4"; mkfs.ext4 -q -F "$WORK/dataset-up.ext4"
pass "Building disk artifacts"

cat > "$WORK/cold.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-consistency-cold }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay: { diff: file://$WORK/root-up.ext4, size: 512MiB }
  disks:
    - { name: scratch, diff: file://$WORK/scratch.ext4, diff_size: 256MiB }
    - { name: dataset, base: $DATASET_REF, overlay: { diff: file://$WORK/dataset-up.ext4, diff_size: 256MiB } }
mounts:
  - { target: /scratch, type: disk, source: scratch }
  - { target: /data,    type: disk, source: dataset }
launch: { exec: /bin/sleep, args: ["3600"] }
EOF

SID0=dk-consistency-0
timeout -k 10s 120 "$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --sandbox-id "$SID0" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" > "$WORK/run-cold.log" 2>&1 &
P=$!
ready "$SID0" || { echo "FAIL: cold boot not ready"; tail -60 "$WORK/run-cold.log"; exit 1; }
CURRENT_SID="$SID0"
pass "Cold boot multi-disk sandbox"

"$BIN/sandbox-ctl" exec --sandbox-id "$SID0" --run-root "$RR" -- /bin/sh -c \
    'command -v sha256sum >/dev/null && command -v sync >/dev/null && command -v tar >/dev/null' \
    || { echo "FAIL: guest image must provide sha256sum, sync, and tar"; exit 1; }

"$BIN/sandbox-ctl" exec --sandbox-id "$SID0" --run-root "$RR" -- /bin/sh -c \
    'mkdir -p /kuasar-e2e; echo ROOT-OK > /kuasar-e2e/persist; echo S-OK > /scratch/persist; echo D-OK > /data/persist; sync' \
    >"$WORK/seed.out" 2>&1 \
    || { echo "FAIL: could not seed markers"; cat "$WORK/seed.out"; exit 1; }
: > "$MANIFEST"
manifest_record_disk_images "$SID0"
pass "Seeding initial markers and manifests"

verify_and_fsck "$SID0" "cold-seed" "$WORK/root-up.ext4" "$WORK/dataset-up.ext4" "$WORK/scratch.ext4"
pass "Initial verification (root, dataset, scratch) -> fsck OK"

snapshot_sandbox "$SID0" "$WORK/snaps/initial"
pass "Initial snapshot committed (Dump: ${LAST_DUMP_MS}ms | Memory: ${LAST_MEM_MIB}MB)"

# =============================================================================
section "2.  Running Pause/Resume Cycles with Consistency [checksum verify and e2fsck] check"
#   - Checksum verification: verify the whole-disk image checksums inside the guest and verify against the manifest.
#   - Host e2fsck check: perform a structural check on the host-bound root/dataset uppers; scratch host image only on cold boot (see host_fsck_all)

for cycle in $(seq 1 "$PAUSE_RESUME_CYCLES"); do
    sid="dk-consistency-${cycle}"
    line="[$cycle/$PAUSE_RESUME_CYCLES]"

    restore_sandbox "$CURRENT_SNAP" "$sid" "c${cycle}"
    line+=" → Restore $OK"

    # No scratch host path: restore does not bind a scratch diff file.
    # Scratch is still covered by markers + disk-image checksum above.
    verify_and_fsck "$sid" "cycle-${cycle}-restore" "$ROOT_DIFF" "$DATASET_DIFF"
    line+=" | Consistency check $OK"

    if [ $((cycle % WRITE_EVERY)) -eq 0 ]; then
        write_cycle_data "$sid" "$cycle"
        verify_markers "$sid" "cycle-${cycle}-post-write"
        verify_disk_image_manifest "$sid" "cycle-${cycle}-post-write" data-only
        line+=" | Data written to scratch + dataset"
    fi

    snapshot_sandbox "$sid" "$WORK/snaps/cycle-${cycle}"
    line+=" | Snapshot committed (Dump: ${LAST_DUMP_MS}ms)"
    echo "$line"
done

echo
echo "$HR"
echo "SUCCESS: e2e_sandbox_disk_consistency: OK ($PAUSE_RESUME_CYCLES cycles; disk images + fsck stable)"
