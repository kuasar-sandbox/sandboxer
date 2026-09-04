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
#        restore (resume) → verify markers + whole-disk checksums
#        → every WRITE_EVERY cycles: WRITE_ITERS overwrite R/W passes of a
#          fixed WRITE_MIB file on scratch + dataset, then refresh checksums
#        → snapshot for the next iteration
#   5. restore the final snapshot once more and verify consistency
#
# Consistency is checked inside the guest: markers confirm mounts are correctly
# wired, and whole-filesystem tar checksums catch silent content corruption
# across pause/resume.  Periodic write cycles stress scratch + dataset with
# repeated overwrite of a fixed ~750MiB urandom file (write + full read),
# staying under the 1GiB data-disk capacity.
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
#   Guest: /bin/sh, sha256sum, sync, tar, dd, stat.
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
PAUSE_RESUME_CYCLES="${PAUSE_RESUME_CYCLES:-15}"
WRITE_EVERY="${WRITE_EVERY:-5}"
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
P=0
TAP_CREATED=0
CURRENT_SNAP=""
LAST_DUMP_MS=""
LAST_MEM_MIB=""

cleanup() {
    set +e
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
    overlay: { diff: file://$3, size: 512MiB }
  disks:
    - { name: scratch }
    - { name: dataset, overlay: { diff: file://$4, size: $DATA_DISK_YAML_SIZE } }
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
mkdir -p "$WORK/ds"; echo "DATASET-OK" > "$WORK/ds/DATASET-OK"
MKFS_EROFS_PATH="$BIN/mkfs.erofs" "$BIN/flatten-ctl" export --no-progress \
    --tmpdir "$WORK/tmp" --output "$WORK/dataset.img" "$WORK/ds"
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"
DATASET_REF="$(plaintext_tarstream_ref "$WORK/dataset.img")"
truncate -s "$DATA_DISK_SIZE" "$WORK/dataset-up.ext4"; mkfs.ext4 -q -F "$WORK/dataset-up.ext4"
pass "Building disk artifacts (data disks ${DATA_DISK_YAML_SIZE})"

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
    - { name: scratch, diff: file://$WORK/scratch.ext4, diff_size: $DATA_DISK_YAML_SIZE }
    - { name: dataset, base: $DATASET_REF, overlay: { diff: file://$WORK/dataset-up.ext4, diff_size: $DATA_DISK_YAML_SIZE } }
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
pass "Cold boot multi-disk sandbox"

"$BIN/sandbox-ctl" exec --sandbox-id "$SID0" --run-root "$RR" -- /bin/sh -c \
    'command -v sha256sum >/dev/null && command -v sync >/dev/null && command -v tar >/dev/null \
     && command -v dd >/dev/null && command -v stat >/dev/null' \
    || { echo "FAIL: guest image must provide sha256sum, sync, tar, dd, and stat"; exit 1; }

"$BIN/sandbox-ctl" exec --sandbox-id "$SID0" --run-root "$RR" -- /bin/sh -c \
    'mkdir -p /kuasar-e2e; echo ROOT-OK > /kuasar-e2e/persist; echo S-OK > /scratch/persist; echo D-OK > /data/persist; sync' \
    >"$WORK/seed.out" 2>&1 \
    || { echo "FAIL: could not seed markers"; cat "$WORK/seed.out"; exit 1; }
: > "$MANIFEST"
manifest_record_disk_images "$SID0"
pass "Seeding initial markers and manifests"

verify_consistency "$SID0" "cold-seed"
pass "Initial verification (root, dataset, scratch)"

snapshot_sandbox "$SID0" "$WORK/snaps/initial"
pass "Initial snapshot committed (Dump: ${LAST_DUMP_MS}ms | Memory: ${LAST_MEM_MIB}MB)"

# =============================================================================
section "2.  Running Pause/Resume Cycles [markers + checksums | bulk ${WRITE_ITERS}x${WRITE_MIB}MiB file overwrites]"

for cycle in $(seq 1 "$PAUSE_RESUME_CYCLES"); do
    sid="dk-consistency-${cycle}"
    line="[$cycle/$PAUSE_RESUME_CYCLES]"

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
    echo "$line"
done

# Verify consistency of the final snapshot
restore_sandbox "$CURRENT_SNAP" "dk-consistency-final" "final"
verify_consistency "dk-consistency-final" "final-restore"
pass "Final snapshot restore + consistency check"

echo
echo "$HR"
echo "SUCCESS: e2e_sandbox_disk_consistency: OK (${PAUSE_RESUME_CYCLES} cycles; disk images stable)"
