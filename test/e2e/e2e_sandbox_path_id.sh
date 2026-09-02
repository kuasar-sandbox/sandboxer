#!/usr/bin/env bash
#
# e2e_sandbox_path_id.sh — verify that logical SandboxID remains independent
# from the RunRoot/BaseRoot PathID used by Build-like a/b/c phase directories.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
BIN="${BIN:-$REPO_ROOT/bin}"

skip() {
    echo
    echo "==> e2e_sandbox_path_id: skipping ($*)"
    if [ "${REQUIRE_KVM:-0}" = "1" ]; then
        echo "REQUIRE_KVM=1 set; failing instead of skipping" >&2
        exit 1
    fi
    exit 0
}

[ -e /dev/kvm ] || skip "/dev/kvm not present"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || skip "/dev/kvm not accessible"
for binary in cloud-hypervisor sandbox-ctl sandbox-init sandbox-runtime.bundle flatten-ctl; do
    [ -e "$BIN/$binary" ] || skip "missing $BIN/$binary"
done
VMLINUX="${VMLINUX:-$BIN/vmlinux}"
[ -f "$VMLINUX" ] || skip "no vmlinux at $VMLINUX"
command -v mkfs.ext4 >/dev/null 2>&1 || skip "mkfs.ext4 not found"
command -v python3 >/dev/null 2>&1 || skip "python3 not found"

if [ "$(id -u)" -ne 0 ]; then
    exec sudo -nE "$0" "$@"
fi

WORK="$(mktemp -d /tmp/e2e-path-id-XXXXXX)"
RUN_PID=""
cleanup() {
    set +e
    if [ -n "$RUN_PID" ] && kill -0 "$RUN_PID" 2>/dev/null; then
        kill -TERM "$RUN_PID" 2>/dev/null
        wait "$RUN_PID" 2>/dev/null
    fi
    [ -n "${E2E_KEEP:-}" ] && echo "kept: $WORK" || rm -rf "$WORK"
}
trap cleanup EXIT

IMAGE="${IMAGE:-python:3.12-slim}"
BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker unavailable; provide BLK0_IMAGE"
    docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null
    BLK0_IMAGE="$WORK/root.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"

truncate -s 512M "$WORK/root-template.ext4"
mkfs.ext4 -q -F "$WORK/root-template.ext4"
truncate -s 256M "$WORK/data-template.ext4"
mkfs.ext4 -q -F "$WORK/data-template.ext4"

RUN_ROOT="$WORK/run/build-01"
BASE_ROOT="$WORK/base/build-01"
mkdir -p "$RUN_ROOT/b" "$BASE_ROOT/b" "$BASE_ROOT/checkpoint"
: >"$RUN_ROOT/b/sibling.keep"
: >"$BASE_ROOT/b/sibling.keep"

cat >"$WORK/cold.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    base: $BLK0_REF
    overlay:
      diff_template: file://$WORK/root-template.ext4
      size: 512MiB
  disks:
    - name: scratch
      diff_template: file://$WORK/data-template.ext4
      diff_size: 256MiB
mounts:
  - { target: /scratch, type: disk, source: scratch }
launch: { exec: /bin/sleep, args: ["3600"], restart: never }
EOF

wait_exec_path() {
    local path_id=$1
    for _ in $(seq 1 90); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --path-id "$path_id" \
            --run-root "$RUN_ROOT" -- /bin/true >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

SID_A="logical-phase-a"
echo "==> cold start: SandboxID=$SID_A PathID=a"
timeout -k 10s 150 "$BIN/sandbox-ctl" run \
    --config "$WORK/cold.yaml" --sandbox-id "$SID_A" --path-id a \
    --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" \
    --ch-binary "$BIN/cloud-hypervisor" >"$WORK/cold.log" 2>&1 &
RUN_PID=$!
wait_exec_path a || { tail -80 "$WORK/cold.log"; exit 1; }

[ -S "$RUN_ROOT/a/ctl.sock" ] || { echo "missing PathID ctl.sock" >&2; exit 1; }
[ ! -e "$RUN_ROOT/$SID_A" ] || { echo "logical SandboxID incorrectly became RunDir" >&2; exit 1; }
[ -f "$BASE_ROOT/a/$SID_A.overlay.diff" ] || { echo "missing logical root diff in PathID BaseDir" >&2; exit 1; }
[ -f "$BASE_ROOT/a/$SID_A.disk0.diff" ] || { echo "missing logical data diff in PathID BaseDir" >&2; exit 1; }
[ ! -e "$BASE_ROOT/a/a.overlay.diff" ] || { echo "PathID incorrectly became diff identity" >&2; exit 1; }

"$BIN/sandbox-ctl" exec --path-id a --run-root "$RUN_ROOT" -- \
    /bin/sh -c 'echo PATH-ID-OK > /scratch/path-id-marker'

EXPORT_OUT="$BASE_ROOT/checkpoint/export"
"$BIN/sandbox-ctl" export --path-id a --run-root "$RUN_ROOT" \
    --output "$EXPORT_OUT" --resume
[ -e "$EXPORT_OUT/$SID_A.sandbox" ] || { echo "live export lost logical SandboxID alias" >&2; exit 1; }

EXPORT_BOTH_OUT="$BASE_ROOT/checkpoint/export-both"
"$BIN/sandbox-ctl" export --sandbox-id not-the-directory --path-id a \
    --run-root "$RUN_ROOT" --output "$EXPORT_BOTH_OUT" --resume
[ -e "$EXPORT_BOTH_OUT/$SID_A.sandbox" ] || { echo "PathID precedence export failed" >&2; exit 1; }

SNAPSHOT_OUT="$BASE_ROOT/checkpoint/snapshot"
"$BIN/sandbox-ctl" snapshot --path-id a --run-root "$RUN_ROOT" \
    --output "$SNAPSHOT_OUT"
wait "$RUN_PID"
RUN_PID=""
SNAPSHOT="$SNAPSHOT_OUT/$SID_A.snapshot"
[ -e "$SNAPSHOT" ] || { echo "snapshot output missing from BuildBaseDir/checkpoint" >&2; exit 1; }
[ ! -e "$RUN_ROOT/a" ] || { echo "PathID RunDir survived normal exit" >&2; exit 1; }
[ -f "$RUN_ROOT/b/sibling.keep" ] || { echo "sibling RunDir was removed" >&2; exit 1; }
[ -f "$BASE_ROOT/b/sibling.keep" ] || { echo "sibling BaseDir was removed" >&2; exit 1; }
[ -d "$RUN_ROOT" ] && [ -d "$BASE_ROOT" ] || { echo "caller roots were removed" >&2; exit 1; }

cat >"$WORK/restore.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff_template: file://$WORK/root-template.ext4
      size: 512MiB
  disks:
    - name: scratch
      diff_template: file://$WORK/data-template.ext4
      diff_size: 256MiB
EOF

SID_C="logical-phase-c"
echo "==> restore: SandboxID=$SID_C PathID=c"
timeout -k 10s 150 "$BIN/sandbox-ctl" run --restore "$SNAPSHOT" \
    --config "$WORK/restore.yaml" --sandbox-id "$SID_C" --path-id c \
    --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" \
    --ch-binary "$BIN/cloud-hypervisor" >"$WORK/restore.log" 2>&1 &
RUN_PID=$!
wait_exec_path c || { tail -80 "$WORK/restore.log"; exit 1; }

"$BIN/sandbox-ctl" exec --sandbox-id not-the-directory --path-id c \
    --run-root "$RUN_ROOT" -- /bin/sh -c 'grep -q PATH-ID-OK /scratch/path-id-marker'
[ -f "$BASE_ROOT/c/$SID_C.overlay.diff" ] || { echo "restore root diff lost logical SandboxID" >&2; exit 1; }
[ -f "$BASE_ROOT/c/$SID_C.disk0.diff" ] || { echo "restore data diff lost logical SandboxID" >&2; exit 1; }

RESTORE_SNAPSHOT_OUT="$BASE_ROOT/checkpoint/restore-snapshot"
"$BIN/sandbox-ctl" snapshot --path-id c --run-root "$RUN_ROOT" \
    --output "$RESTORE_SNAPSHOT_OUT"
wait "$RUN_PID"
RUN_PID=""
[ -e "$RESTORE_SNAPSHOT_OUT/$SID_C.snapshot" ] || { echo "restored PathID snapshot missing" >&2; exit 1; }
[ ! -e "$RUN_ROOT/c" ] || { echo "restored PathID RunDir survived exit" >&2; exit 1; }
[ -f "$RUN_ROOT/b/sibling.keep" ] && [ -f "$BASE_ROOT/b/sibling.keep" ] \
    || { echo "phase cleanup removed a sibling" >&2; exit 1; }

echo "==> e2e_sandbox_path_id: OK"
