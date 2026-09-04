#!/usr/bin/env bash
#
# e2e_sandbox_disk_quota_control.sh — verify writable disk capacity enforcement.
#
# Boots a sandbox with a 30GiB data disk, fills exactly 28GiB (7×4GiB shreds;
# per-file retry up to 3 times on failure), asserts remaining space is under
# 4GiB, then that a further 4GiB write fails with "No space left on device"
# (guest ENOSPC from the sized vhost-blk COW / ext4, not a host-side sandbox exit).
#
# Prerequisites (missing → skip, exit 0; REQUIRE_KVM=1 to fail hard):
#   /dev/kvm rw · bin/{cloud-hypervisor,sandbox-ctl,sandbox-init,
#   sandbox-runtime.bundle,flatten-ctl,mkfs.erofs} · $VMLINUX · docker (or
#   BLK0_IMAGE=) · mkfs.ext4 · shred (in guest) · root (tap/vsock).
#
# In the 30GB disk, the ext4 reserved blocks are around 24K which is 1% of the disk.
# It is still safe to write 28GiB to the disk and then test the quota enforcement.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
BIN="${BIN:-$REPO_ROOT/bin}"
IMAGE="${IMAGE:-python:3.12-slim}"
TAP_NAME="${TAP_NAME:-sb-tap0}"

info()  { printf '[INFO]  %s\n' "$*"; }
pass()  { printf '[PASS]  %s\n' "$*"; }
fail()  { printf '[FAIL]  %s\n' "$*" >&2; }
run()   { printf '[RUN]   %s\n' "$*"; }
check() { printf '[CHECK] %s\n' "$*"; }
step()  { printf '        %s\n' "$*"; }

skip() {
    echo
    info "e2e_sandbox_disk_quota_control: skipping ($*)"
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

if [ "$(id -u)" -ne 0 ]; then exec sudo -nE "$0" "$@"; fi

WORK=$(mktemp -d /tmp/e2e-disks-XXXXXX)
mkdir -p "$WORK/runtime"
TAP_CREATED=0
P=0
cleanup() {
    info "Cleaning up environment..."
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
    pkill -KILL -f "cloud-hypervisor.*disk-quota-control" 2>/dev/null
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    if [ -n "${E2E_KEEP:-}" ]; then
        step "Kept work dir: $WORK (includes 30G scratch disk)."
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

# ---- artifacts: root base + overlay upper, single 30G data disk ------------
info "Initializing disk artifacts..."
BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker not available; set BLK0_IMAGE="
    docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null
    BLK0_IMAGE="$WORK/root.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
truncate -s 512M "$WORK/root-up.ext4"; mkfs.ext4 -q -F "$WORK/root-up.ext4"
truncate -s 30G "$WORK/scratch.ext4"; mkfs.ext4 -q -F "$WORK/scratch.ext4"
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"

cat > "$WORK/sandbox.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-disk-quota }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay: { diff: file://$WORK/root-up.ext4, size: 512MiB }
  disks:
    - { name: scratch, diff_template: file://$WORK/scratch.ext4, diff_size: 30G }
mounts:
  - { target: /scratch, type: disk, source: scratch }
launch: { exec: /bin/sleep, args: ["3600"] }
EOF

SID=disk-quota-control
info "Booting sandbox with 30GB disk quota control..."
"$BIN/sandbox-ctl" run --config "$WORK/sandbox.yaml" --sandbox-id "$SID" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" > "$WORK/run.log" 2>&1 &
P=$!

ready() { # $1=sid
    for _ in $(seq 1 90); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$1" --run-root "$WORK/runtime" -- /bin/sh -c 'echo R' >"$WORK/r.out" 2>/dev/null && grep -q R "$WORK/r.out"; then
            return 0
        fi
        sleep 1
    done
    return 1
}
ready "$SID" || { fail "Cold boot not ready"; tail -60 "$WORK/run.log"; exit 1; }

# Verify shred is available in the guest image.
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- \
    /bin/sh -c 'command -v shred >/dev/null' \
    || skip "shred not in guest image $IMAGE (apt install coreutils)"

"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- df -h /scratch > "$WORK/df_check.out"
grep -q "30G" "$WORK/df_check.out" || { fail "30G disk not present"; cat "$WORK/df_check.out"; exit 1; }
pass "Sandbox booted successfully."
echo

ex1() { "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" "$@"; }

FOUR_GIB=$((4 * 1024 * 1024 * 1024))
FILL_FILES=7
FILL_TOTAL_GIB=$((FILL_FILES * 4))
MAX_FILL_ATTEMPTS=3

run "Testing disk quota control..."
step "-> Writing $FILL_FILES files of 4GiB each in parallel (Total: ${FILL_TOTAL_GIB}GiB)"

pending=()
attempts=()
for i in $(seq 1 "$FILL_FILES"); do
    pending+=("$i")
    attempts[$i]=0
done

while [ "${#pending[@]}" -gt 0 ]; do
    BK_PIDS=()
    BK_IDXS=()
    step "-> [•] Writing file(s): ${pending[*]}"
    for i in "${pending[@]}"; do
        attempts[$i]=$((${attempts[$i]} + 1))
        if [ "${attempts[$i]}" -gt 1 ]; then
            step "-> retrying file $i (attempt ${attempts[$i]}/$MAX_FILL_ATTEMPTS)"
            ex1 -- rm -f "/scratch/4gb_file_$i.bin" >/dev/null 2>&1 || true
        fi
        if ! ex1 -- touch "/scratch/4gb_file_$i.bin"; then
            fail "touch /scratch/4gb_file_$i.bin failed"
            for pid in "${BK_PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
            exit 1
        fi
        (
            "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- \
                shred -n 1 -s 4G "/scratch/4gb_file_$i.bin"
        ) &
        BK_PIDS+=($!)
        BK_IDXS+=("$i")
    done

    next_pending=()
    for j in "${!BK_PIDS[@]}"; do
        pid=${BK_PIDS[$j]}
        i=${BK_IDXS[$j]}
        ok=1
        if ! wait "$pid"; then
            ok=0
            step "-> shred failed for file $i (attempt ${attempts[$i]}/$MAX_FILL_ATTEMPTS)"
        elif ! sz=$(ex1 -- /bin/sh -c "stat -c %s /scratch/4gb_file_$i.bin" 2>/dev/null | tr -d '[:space:]') \
                || [ -z "$sz" ] || [ "$sz" != "$FOUR_GIB" ]; then
            ok=0
            step "-> file $i size is '${sz:-missing}', want $FOUR_GIB (attempt ${attempts[$i]}/$MAX_FILL_ATTEMPTS)"
        fi
        if [ "$ok" -eq 1 ]; then
            step "-> [✓] file $i complete (4GiB)"
            continue
        fi
        ex1 -- rm -f "/scratch/4gb_file_$i.bin" >/dev/null 2>&1 || true
        if [ "${attempts[$i]}" -ge "$MAX_FILL_ATTEMPTS" ]; then
            fail "file $i failed after $MAX_FILL_ATTEMPTS attempts"
            exit 1
        fi
        next_pending+=("$i")
    done
    pending=()
    if [ "${#next_pending[@]}" -gt 0 ]; then
        pending=("${next_pending[@]}")
    fi
done

step "-> [✓] All ${FILL_TOTAL_GIB}GiB successfully written to disk ($FILL_FILES × 4GiB)."
echo

check "Verifying remaining disk space..."
ex1 -- df -h /scratch > "$WORK/df.out"
sed 's/^/        /' "$WORK/df.out"
AVAIL_BYTES=$(ex1 -- /bin/sh -c 'df -P -B1 /scratch | awk "NR==2 {print \$4}"' | tr -d '[:space:]')
case "$AVAIL_BYTES" in
    ''|*[!0-9]*)
        fail "could not parse remaining bytes from df (got '${AVAIL_BYTES:-empty}')"
        exit 1
        ;;
esac
if [ "$AVAIL_BYTES" -ge "$FOUR_GIB" ]; then
    fail "remaining $AVAIL_BYTES bytes >= 4GiB; another 4GiB write would not exceed the 30GiB disk"
    exit 1
fi
echo

run "Writing 1 additional 4GB file to test quota enforcement..."
ex1 -- touch /scratch/4gb_file_failure.bin
ex1 -- shred -n 1 -s 4G /scratch/4gb_file_failure.bin > "$WORK/shred.out" 2>&1 || true

if grep -q "No space left on device" "$WORK/shred.out"; then
    pass "Disk quota control applied successfully (Write blocked as expected)."
    echo
else
    fail "Disk quota control was not applied."
    sed 's/^/        /' "$WORK/shred.out"
    exit 1
fi

exit 0
