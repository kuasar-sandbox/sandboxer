#!/usr/bin/env bash
#
# e2e_sandbox_disk_quota_control.sh — guest-filesystem ENOSPC, reclaim, and
# continued writes across an active diff and one snapshot/restore cycle.
#
# Boots a sandbox with a 2 GiB scratch disk mounted at /scratch, seeds:
#   /scratch/protected  — one fixed-content file (never deleted); host records hash
#   /scratch/fill       — capacity fill / reclaim / post-restore writes
#
# Then:
#   1. Fill /scratch/fill with deterministic 16 MiB chunks via Python
#      os.open / os.write / os.fsync until errno=28 (ENOSPC). Record bytes
#      written, failed file, and failure offset.
#   2. Delete one completed file + the failed residue, fsync the directory, and
#      assert statvfs available bytes increase.
#   3. Without restarting, write a new 16 MiB file under /scratch/fill and
#      verify its hash on the active writable diff.
#   4. Snapshot → restore; protected file unchanged; deleted paths stay gone.
#   5. On the restored writable diff, write and verify one more 16 MiB file.
#
# This validates guest-filesystem ENOSPC, space reclamation, continued writes on
# the active diff, and a new writable diff after restore. It does not cover
# host-side diff ENOSPC or a short write injected into BlockCOW.WriteAt.
#
# Prerequisites (missing → skip, exit 0; REQUIRE_KVM=1 to fail hard):
#   /dev/kvm rw · bin/{cloud-hypervisor,sandbox-ctl,sandbox-init,
#   sandbox-runtime.bundle,flatten-ctl,mkfs.erofs} · $VMLINUX · docker (or
#   BLK0_IMAGE=) · mkfs.ext4 · python3 (host) · root (tap/vsock).
#   Guest: python3, sha256sum.
#
# Command line:
#   sudo -E bash test/e2e/e2e_sandbox_disk_quota_control.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
BIN="${BIN:-$REPO_ROOT/bin}"
IMAGE="${IMAGE:-python:3.12-slim}"
TAP_NAME="${TAP_NAME:-sb-tap0}"

SCRATCH_SIZE=2G
SCRATCH_YAML_SIZE=2GiB
CHUNK_BYTES=$((16 * 1024 * 1024))
# Cap each fill file so ENOSPC lands mid-file after at least one completed file.
FILL_FILE_MAX=$((128 * 1024 * 1024))
FILL_TIMEOUT_SECS="${FILL_TIMEOUT_SECS:-300}"
PROTECTED_CONTENT=$'PROTECTED-QUOTA-E2E-V1\n'

# truncate(1)-style size → bytes (K/M/G = KiB/MiB/GiB).
scratch_size_bytes() {
    local s=$1 n
    case "$s" in
        ''|*[!0-9KMGkmg]*)
            echo "$0: SCRATCH_SIZE must look like 2G / 2048M (got '$s')" >&2
            exit 1
            ;;
    esac
    n=${s%%[KMGkmg]}
    case "$s" in
        *G|*g) echo $((n * 1024 * 1024 * 1024)) ;;
        *M|*m) echo $((n * 1024 * 1024)) ;;
        *K|*k) echo $((n * 1024)) ;;
        *)     echo "$n" ;;
    esac
}
SCRATCH_BYTES=$(scratch_size_bytes "$SCRATCH_SIZE")
# Abort past capacity + one fill-file of slack if ENOSPC never arrives.
FILL_MAX_BYTES=$((SCRATCH_BYTES + FILL_FILE_MAX))

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
    echo "SKIPPED: e2e_sandbox_disk_quota_control: skipping ($*)"
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

if [ "$(id -u)" -ne 0 ]; then exec sudo -nE "$0" "$@"; fi

WORK=$(mktemp -d /tmp/e2e-disk-quota-XXXXXX)
RR=$WORK/runtime
mkdir -p "$RR"
TAP_CREATED=0
P=0
CURRENT_SNAP=""
SID=""

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
    pkill -KILL -f "cloud-hypervisor.*disk-quota-" 2>/dev/null
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

# Deterministic 16 MiB payload hash (host-side), keyed by a short label.
hash_16mib() { # $1=label → prints sha256
    python3 - "$1" "$CHUNK_BYTES" <<'PY'
import hashlib, sys
label, chunk = sys.argv[1].encode(), int(sys.argv[2])
if len(label) >= chunk:
    raise SystemExit("label too long for chunk")
payload = label + b"\0" * (chunk - len(label))
print(hashlib.sha256(payload).hexdigest())
PY
}

PROTECTED_HASH=$(printf '%s' "$PROTECTED_CONTENT" | sha256sum | awk '{print $1}')
AFTER_RECLAIM_HASH=$(hash_16mib "AFTER-RECLAIM-16MIB")
AFTER_RESTORE_HASH=$(hash_16mib "AFTER-RESTORE-16MIB")

ready() { # $1=sid
    for _ in $(seq 1 90); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$1" --run-root "$RR" -- /bin/sh -c 'echo R' \
            >"$WORK/r.out" 2>/dev/null && grep -q R "$WORK/r.out"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

ex() { "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$RR" "$@"; }

# =============================================================================
section "1.  Starting Sandbox Disk Quota Control [scratch ${SCRATCH_YAML_SIZE} | 16MiB chunks | fill-file max $((FILL_FILE_MAX / 1024 / 1024))MiB]"

BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker not available; set BLK0_IMAGE="
    docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null
    BLK0_IMAGE="$WORK/root.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
truncate -s 512M "$WORK/root-up.ext4"; mkfs.ext4 -q -F "$WORK/root-up.ext4"
truncate -s "$SCRATCH_SIZE" "$WORK/scratch.ext4"; mkfs.ext4 -q -F "$WORK/scratch.ext4"
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"
ROOT_DIFF_PATH="$WORK/root-up.ext4"
SCRATCH_DIFF_PATH="$WORK/scratch.ext4"
pass "Building disk artifacts (scratch ${SCRATCH_YAML_SIZE})"

cat > "$WORK/sandbox.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-disk-quota }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay: { diff: file://$ROOT_DIFF_PATH, size: 512MiB }
  disks:
    - { name: scratch, diff: file://$SCRATCH_DIFF_PATH, diff_size: $SCRATCH_YAML_SIZE }
mounts:
  - { target: /scratch, type: disk, source: scratch }
launch: { exec: /bin/sleep, args: ["3600"] }
EOF

SID=disk-quota-control
timeout -k 10s 120 "$BIN/sandbox-ctl" run --config "$WORK/sandbox.yaml" --sandbox-id "$SID" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" > "$WORK/run.log" 2>&1 &
P=$!
ready "$SID" || { echo "FAIL: cold boot not ready"; tail -60 "$WORK/run.log"; exit 1; }

ex -- /bin/sh -c 'command -v python3 >/dev/null && command -v sha256sum >/dev/null' \
    || skip "guest image $IMAGE must provide python3 and sha256sum"

SIZE_BYTES=$(ex -- /bin/sh -c 'df -P -B1 /scratch | awk "NR==2 {print \$2}"' | tr -d '[:space:]')
case "$SIZE_BYTES" in
    ''|*[!0-9]*)
        echo "FAIL: could not parse /scratch size (got '${SIZE_BYTES:-empty}')"
        exit 1
        ;;
esac
# Formatted ext4 reports slightly under the truncate size in df Size.
MIN_SIZE=$((SCRATCH_BYTES * 1800 / 2048))
MAX_SIZE=$SCRATCH_BYTES
if [ "$SIZE_BYTES" -lt "$MIN_SIZE" ] || [ "$SIZE_BYTES" -gt "$MAX_SIZE" ]; then
    echo "FAIL: /scratch size $SIZE_BYTES bytes not in [${MIN_SIZE}, ${MAX_SIZE}] for ${SCRATCH_YAML_SIZE}"
    ex -- df -h /scratch || true
    exit 1
fi
pass "Cold boot with ~${SCRATCH_YAML_SIZE} scratch ($SIZE_BYTES bytes reported)"

# ---- seed protected + fill directories --------------------------------------
cat > "$WORK/seed_protected.py" <<'PY'
import os

CONTENT = b"PROTECTED-QUOTA-E2E-V1\n"
os.makedirs("/scratch/protected", exist_ok=True)
os.makedirs("/scratch/fill", exist_ok=True)
path = "/scratch/protected/marker.txt"
fd = os.open(path, os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o644)
try:
    n = os.write(fd, CONTENT)
    if n != len(CONTENT):
        raise SystemExit(f"short write {n}/{len(CONTENT)}")
    os.fsync(fd)
finally:
    os.close(fd)
for d in ("/scratch/protected", "/scratch/fill", "/scratch"):
    dir_fd = os.open(d, os.O_RDONLY)
    try:
        os.fsync(dir_fd)
    finally:
        os.close(dir_fd)
print("SEED-OK", flush=True)
PY
ex --stdin-from "$WORK/seed_protected.py" -- python3 - >"$WORK/seed.out" 2>&1 \
    || { echo "FAIL: could not seed protected/fill dirs"; sed 's/^/    /' "$WORK/seed.out"; exit 1; }
grep -q '^SEED-OK$' "$WORK/seed.out" \
    || { echo "FAIL: missing SEED-OK"; sed 's/^/    /' "$WORK/seed.out"; exit 1; }

GOT_PROTECTED=$(ex -- sha256sum /scratch/protected/marker.txt | awk '{print $1}')
[ "$GOT_PROTECTED" = "$PROTECTED_HASH" ] \
    || { echo "FAIL: protected hash mismatch want=$PROTECTED_HASH got=$GOT_PROTECTED"; exit 1; }
pass "Protected marker seeded (hash $PROTECTED_HASH)"

# =============================================================================
section "2.  Fill /scratch/fill until guest ENOSPC (errno=28)"

cat > "$WORK/fill_enospc.py" <<'PY'
import errno
import os
import sys

CHUNK = int(sys.argv[1])
FILE_MAX = int(sys.argv[2])
MAX_BYTES = int(sys.argv[3])
FILL_DIR = "/scratch/fill"


def chunk_payload(file_idx: int, chunk_idx: int) -> bytes:
    header = f"FILL-{file_idx:04d}-{chunk_idx:08d}\n".encode()
    if len(header) >= CHUNK:
        raise SystemExit("header too long")
    pad = CHUNK - len(header)
    # Deterministic repeating pattern (not random); content itself is not hashed later.
    pattern = bytes(((file_idx + chunk_idx + i) % 256) for i in range(256))
    body = (pattern * ((pad // 256) + 1))[:pad]
    return header + body


def fsync_dir(path: str) -> None:
    dir_fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(dir_fd)
    finally:
        os.close(dir_fd)


total_written = 0
completed = []
failed_file = None
failure_offset = None
file_idx = 0

while failed_file is None:
    if total_written >= MAX_BYTES:
        raise SystemExit(
            f"wrote {total_written} bytes without ENOSPC (limit {MAX_BYTES}); aborting"
        )
    path = os.path.join(FILL_DIR, f"fill_{file_idx:04d}.bin")
    fd = os.open(path, os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o644)
    offset = 0
    chunk_idx = 0
    hit_enospc = False
    try:
        while offset < FILE_MAX:
            if total_written >= MAX_BYTES:
                raise SystemExit(
                    f"wrote {total_written} bytes without ENOSPC (limit {MAX_BYTES}); aborting"
                )
            data = chunk_payload(file_idx, chunk_idx)
            n = 0
            try:
                n = os.write(fd, data)
                if n != len(data):
                    # Regular-file ENOSPC usually raises; treat short write as failure site.
                    failed_file = path
                    failure_offset = offset
                    total_written += n
                    hit_enospc = True
                    print(
                        f"ENOSPC bytes_written={total_written} failed_file={failed_file} "
                        f"failure_offset={failure_offset} errno=28 short_write={n}",
                        flush=True,
                    )
                    break
                os.fsync(fd)
            except OSError as e:
                if e.errno != errno.ENOSPC:
                    raise
                failed_file = path
                failure_offset = offset
                total_written += n
                hit_enospc = True
                print(
                    f"ENOSPC bytes_written={total_written} failed_file={failed_file} "
                    f"failure_offset={failure_offset} errno={e.errno}",
                    flush=True,
                )
                break
            offset += n
            total_written += n
            chunk_idx += 1
    finally:
        os.close(fd)

    if hit_enospc:
        break

    if offset == FILE_MAX:
        completed.append(os.path.basename(path))
        fsync_dir(FILL_DIR)
        file_idx += 1
        continue

    raise SystemExit(f"unexpected stop writing {path} at offset={offset}")

if failed_file is None:
    raise SystemExit("fill completed without ENOSPC")
if not completed:
    raise SystemExit("need at least one completed fill file before ENOSPC")

fsync_dir(FILL_DIR)
print("COMPLETED=" + ",".join(completed), flush=True)
print(f"RESIDUE={os.path.basename(failed_file)}", flush=True)
print("FILL-OK", flush=True)
PY

# Host timeout covers a wedged guest; MAX_BYTES covers a disk that never returns ENOSPC.
timeout -k 10s "$FILL_TIMEOUT_SECS" \
    "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$RR" \
    --stdin-from "$WORK/fill_enospc.py" -- \
    python3 - "$CHUNK_BYTES" "$FILL_FILE_MAX" "$FILL_MAX_BYTES" >"$WORK/fill.out" 2>&1 \
    || { echo "FAIL: fill until ENOSPC failed (timeout=${FILL_TIMEOUT_SECS}s or guest error)"; sed 's/^/    /' "$WORK/fill.out"; exit 1; }

grep -q '^FILL-OK$' "$WORK/fill.out" \
    || { echo "FAIL: missing FILL-OK"; sed 's/^/    /' "$WORK/fill.out"; exit 1; }
grep -q '^ENOSPC ' "$WORK/fill.out" \
    || { echo "FAIL: missing ENOSPC record line"; sed 's/^/    /' "$WORK/fill.out"; exit 1; }

ENOSPC_LINE=$(grep '^ENOSPC ' "$WORK/fill.out" | tail -1)
BYTES_WRITTEN=$(printf '%s\n' "$ENOSPC_LINE" | sed -n 's/.*bytes_written=\([0-9][0-9]*\).*/\1/p')
FAILED_FILE=$(printf '%s\n' "$ENOSPC_LINE" | sed -n 's/.*failed_file=\([^ ]*\).*/\1/p')
FAILURE_OFFSET=$(printf '%s\n' "$ENOSPC_LINE" | sed -n 's/.*failure_offset=\([0-9][0-9]*\).*/\1/p')
COMPLETED_CSV=$(grep '^COMPLETED=' "$WORK/fill.out" | tail -1 | cut -d= -f2-)
RESIDUE_NAME=$(grep '^RESIDUE=' "$WORK/fill.out" | tail -1 | cut -d= -f2-)

case "$BYTES_WRITTEN" in ''|*[!0-9]*) echo "FAIL: bad bytes_written in: $ENOSPC_LINE"; exit 1 ;; esac
case "$FAILURE_OFFSET" in ''|*[!0-9]*) echo "FAIL: bad failure_offset in: $ENOSPC_LINE"; exit 1 ;; esac
[ -n "$FAILED_FILE" ] || { echo "FAIL: missing failed_file in: $ENOSPC_LINE"; exit 1; }
[ -n "$COMPLETED_CSV" ] || { echo "FAIL: missing COMPLETED= list"; exit 1; }
[ -n "$RESIDUE_NAME" ] || { echo "FAIL: missing RESIDUE="; exit 1; }

DELETE_COMPLETED=$(printf '%s\n' "$COMPLETED_CSV" | cut -d, -f1)
[ -n "$DELETE_COMPLETED" ] || { echo "FAIL: empty completed list"; exit 1; }

echo "    ENOSPC bytes_written=$BYTES_WRITTEN failed_file=$FAILED_FILE failure_offset=$FAILURE_OFFSET"
echo "    completed=$COMPLETED_CSV"
echo "    delete completed=$DELETE_COMPLETED residue=$RESIDUE_NAME"
pass "Guest ENOSPC (errno=28) observed"

# =============================================================================
section "3.  Reclaim space + continue writing on the active diff"

cat > "$WORK/reclaim.py" <<'PY'
import os
import sys

fill_dir = "/scratch/fill"
completed = sys.argv[1]
residue = sys.argv[2]


def avail_bytes(path: str) -> int:
    st = os.statvfs(path)
    return st.f_bavail * st.f_frsize


def fsync_dir(path: str) -> None:
    dir_fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(dir_fd)
    finally:
        os.close(dir_fd)


before = avail_bytes("/scratch")
os.unlink(os.path.join(fill_dir, completed))
os.unlink(os.path.join(fill_dir, residue))
fsync_dir(fill_dir)
os.sync()
after = avail_bytes("/scratch")
print(f"STATVFS before={before} after={after}", flush=True)
if after <= before:
    raise SystemExit(f"available bytes did not increase: before={before} after={after}")
print("RECLAIM-OK", flush=True)
PY

ex --stdin-from "$WORK/reclaim.py" -- \
    python3 - "$DELETE_COMPLETED" "$RESIDUE_NAME" >"$WORK/reclaim.out" 2>&1 \
    || { echo "FAIL: reclaim failed"; sed 's/^/    /' "$WORK/reclaim.out"; exit 1; }
grep -q '^RECLAIM-OK$' "$WORK/reclaim.out" \
    || { echo "FAIL: missing RECLAIM-OK"; sed 's/^/    /' "$WORK/reclaim.out"; exit 1; }
STATVFS_LINE=$(grep '^STATVFS ' "$WORK/reclaim.out" | tail -1)
echo "    $STATVFS_LINE"
pass "statvfs available bytes increased after deletion + directory fsync"

cat > "$WORK/write_16mib.py" <<'PY'
import hashlib
import os
import sys

label = sys.argv[1].encode()
path = sys.argv[2]
chunk = int(sys.argv[3])
expect = sys.argv[4]

if len(label) >= chunk:
    raise SystemExit("label too long")
payload = label + b"\0" * (chunk - len(label))
digest = hashlib.sha256(payload).hexdigest()
if digest != expect:
    raise SystemExit(f"host/guest payload hash prep mismatch {digest} != {expect}")

fd = os.open(path, os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o644)
try:
    n = os.write(fd, payload)
    if n != len(payload):
        raise SystemExit(f"short write {n}/{len(payload)}")
    os.fsync(fd)
finally:
    os.close(fd)

dir_fd = os.open(os.path.dirname(path), os.O_RDONLY)
try:
    os.fsync(dir_fd)
finally:
    os.close(dir_fd)

got = hashlib.sha256(open(path, "rb").read()).hexdigest()
if got != expect:
    raise SystemExit(f"content hash mismatch want={expect} got={got}")
print(f"HASH={got}", flush=True)
print("WRITE16-OK", flush=True)
PY

ex --stdin-from "$WORK/write_16mib.py" -- \
    python3 - "AFTER-RECLAIM-16MIB" /scratch/fill/after_reclaim.bin "$CHUNK_BYTES" "$AFTER_RECLAIM_HASH" \
    >"$WORK/after_reclaim.out" 2>&1 \
    || { echo "FAIL: post-reclaim 16 MiB write failed"; sed 's/^/    /' "$WORK/after_reclaim.out"; exit 1; }
grep -q '^WRITE16-OK$' "$WORK/after_reclaim.out" \
    || { echo "FAIL: missing WRITE16-OK after reclaim"; sed 's/^/    /' "$WORK/after_reclaim.out"; exit 1; }
grep -q "^HASH=${AFTER_RECLAIM_HASH}$" "$WORK/after_reclaim.out" \
    || { echo "FAIL: after_reclaim hash mismatch"; sed 's/^/    /' "$WORK/after_reclaim.out"; exit 1; }
pass "Active-diff 16 MiB write verified (hash $AFTER_RECLAIM_HASH)"

# =============================================================================
section "4.  Snapshot → restore → verify protected/deleted paths → write on restored diff"

SNAP_OUT="$WORK/snaps/quota"
mkdir -p "$SNAP_OUT"
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID" --output "$SNAP_OUT" --run-root "$RR" \
    >"$WORK/snap.log" 2>&1 \
    || { echo "FAIL: snapshot failed"; sed 's/^/    /' "$WORK/snap.log"; exit 1; }
wait "$P" 2>/dev/null || true
P=0
CURRENT_SNAP="$SNAP_OUT/$SID.snapshot"
[ -f "$CURRENT_SNAP" ] || { echo "FAIL: no snapshot at $CURRENT_SNAP"; ls -la "$SNAP_OUT"; exit 1; }
pass "Snapshot committed"

# Scratch topology comes from the snapshot graph (same as e2e_sandbox_disks);
# only root gets a fresh empty writable overlay upper here.
truncate -s 512M "$WORK/root-restore.ext4"; mkfs.ext4 -q -F "$WORK/root-restore.ext4"
cat > "$WORK/restore.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-disk-quota-r }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: { diff: file://$WORK/root-restore.ext4, size: 512MiB }
  disks:
    - { name: scratch }
EOF

SID=disk-quota-restore
timeout -k 10s 120 "$BIN/sandbox-ctl" run --restore "$CURRENT_SNAP" --config "$WORK/restore.yaml" \
    --sandbox-id "$SID" --ch-binary "$BIN/cloud-hypervisor" --run-root "$RR" \
    >"$WORK/restore.log" 2>&1 &
P=$!
ready "$SID" || { echo "FAIL: restore not ready"; tail -60 "$WORK/restore.log"; exit 1; }
pass "Restore ready ($SID)"

GOT_PROTECTED=$(ex -- sha256sum /scratch/protected/marker.txt | awk '{print $1}')
[ "$GOT_PROTECTED" = "$PROTECTED_HASH" ] \
    || { echo "FAIL: protected hash after restore want=$PROTECTED_HASH got=$GOT_PROTECTED"; exit 1; }

ex -- /bin/sh -c "
set -e
test -f /scratch/protected/marker.txt
test ! -e /scratch/fill/$DELETE_COMPLETED
test ! -e /scratch/fill/$RESIDUE_NAME
test -f /scratch/fill/after_reclaim.bin
" >"$WORK/post_restore_paths.out" 2>&1 \
    || { echo "FAIL: post-restore path checks failed"; sed 's/^/    /' "$WORK/post_restore_paths.out"; exit 1; }

GOT_RECLAIM=$(ex -- sha256sum /scratch/fill/after_reclaim.bin | awk '{print $1}')
[ "$GOT_RECLAIM" = "$AFTER_RECLAIM_HASH" ] \
    || { echo "FAIL: after_reclaim.bin hash after restore want=$AFTER_RECLAIM_HASH got=$GOT_RECLAIM"; exit 1; }
pass "Protected unchanged; deleted paths absent; after_reclaim.bin hash intact"

ex --stdin-from "$WORK/write_16mib.py" -- \
    python3 - "AFTER-RESTORE-16MIB" /scratch/fill/after_restore.bin "$CHUNK_BYTES" "$AFTER_RESTORE_HASH" \
    >"$WORK/after_restore.out" 2>&1 \
    || { echo "FAIL: post-restore 16 MiB write failed"; sed 's/^/    /' "$WORK/after_restore.out"; exit 1; }
grep -q '^WRITE16-OK$' "$WORK/after_restore.out" \
    || { echo "FAIL: missing WRITE16-OK after restore"; sed 's/^/    /' "$WORK/after_restore.out"; exit 1; }
grep -q "^HASH=${AFTER_RESTORE_HASH}$" "$WORK/after_restore.out" \
    || { echo "FAIL: after_restore hash mismatch"; sed 's/^/    /' "$WORK/after_restore.out"; exit 1; }
pass "Restored-diff 16 MiB write verified (hash $AFTER_RESTORE_HASH)"

echo
echo "$HR"
echo "SUCCESS: e2e_sandbox_disk_quota_control: OK (ENOSPC + reclaim + active/restored writes)"
exit 0
