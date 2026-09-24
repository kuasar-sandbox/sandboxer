#!/usr/bin/env bash
# Logical SandboxID vs filesystem PathID contract using prepared products only.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker mkfs.ext4 python3 timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -r "$SANDBOXER_LIB/tarstream.sh" ] || e2e_fail "missing prepared tarstream helpers"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT"
RUN_PID=""
cleanup() {
    local status=$?
    set +e
    if [ -n "$RUN_PID" ] && kill -0 "$RUN_PID" 2>/dev/null; then
        kill -TERM "$RUN_PID" 2>/dev/null
        wait "$RUN_PID" 2>/dev/null
    fi
    exit "$status"
}
trap cleanup EXIT

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"

truncate -s 512M "$WORK/root-template.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/root-template.ext4"
truncate -s 256M "$WORK/data-template.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/data-template.ext4"

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
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    base: $ROOT_REF
    overlay:
      diff_template: file://$WORK/root-template.ext4
  disks:
    - name: scratch
      diff_template: file://$WORK/data-template.ext4
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

SID_A="logical-phase-a-$BASHPID"
timeout -k 10s 150 "$BIN/sandbox-ctl" run \
    --config "$WORK/cold.yaml" --sandbox-id "$SID_A" --path-id a \
    --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" \
    --ch-binary "$BIN/cloud-hypervisor" >"$OUT/cold.log" 2>&1 &
RUN_PID=$!
wait_exec_path a || {
    tail -80 "$OUT/cold.log" >&2 || true
    e2e_fail "cold PathID sandbox did not become executable"
}

[ -S "$RUN_ROOT/a/ctl.sock" ] || e2e_fail "missing PathID ctl.sock"
[ ! -e "$RUN_ROOT/$SID_A" ] || e2e_fail "logical SandboxID incorrectly became RunDir"
[ -f "$BASE_ROOT/a/$SID_A.overlay.diff" ] || e2e_fail "missing logical root diff in PathID BaseDir"
[ -f "$BASE_ROOT/a/$SID_A.disk0.diff" ] || e2e_fail "missing logical data diff in PathID BaseDir"
[ ! -e "$BASE_ROOT/a/a.overlay.diff" ] || e2e_fail "PathID incorrectly became diff identity"

"$BIN/sandbox-ctl" exec --path-id a --run-root "$RUN_ROOT" -- \
    /bin/sh -c 'echo PATH-ID-OK > /scratch/path-id-marker'

EXPORT_OUT="$BASE_ROOT/checkpoint/export"
"$BIN/sandbox-ctl" export --path-id a --run-root "$RUN_ROOT" \
    --output "$EXPORT_OUT" --resume
[ -e "$EXPORT_OUT/$SID_A.sandbox" ] || e2e_fail "live export lost logical SandboxID alias"

EXPORT_BOTH_OUT="$BASE_ROOT/checkpoint/export-both"
"$BIN/sandbox-ctl" export --sandbox-id not-the-directory --path-id a \
    --run-root "$RUN_ROOT" --output "$EXPORT_BOTH_OUT" --resume
[ -e "$EXPORT_BOTH_OUT/$SID_A.sandbox" ] || e2e_fail "PathID precedence export failed"

SNAPSHOT_OUT="$BASE_ROOT/checkpoint/snapshot"
"$BIN/sandbox-ctl" snapshot --path-id a --run-root "$RUN_ROOT" \
    --output "$SNAPSHOT_OUT"
wait "$RUN_PID"
RUN_PID=""
SNAPSHOT="$SNAPSHOT_OUT/$SID_A.snapshot"
[ -e "$SNAPSHOT" ] || e2e_fail "snapshot output missing from BuildBaseDir/checkpoint"
[ ! -e "$RUN_ROOT/a" ] || e2e_fail "PathID RunDir survived normal exit"
[ -f "$RUN_ROOT/b/sibling.keep" ] || e2e_fail "sibling RunDir was removed"
[ -f "$BASE_ROOT/b/sibling.keep" ] || e2e_fail "sibling BaseDir was removed"
[ -d "$RUN_ROOT" ] && [ -d "$BASE_ROOT" ] || e2e_fail "caller roots were removed"

cat >"$WORK/restore.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff_template: file://$WORK/root-template.ext4
  disks:
    - name: scratch
      diff_template: file://$WORK/data-template.ext4
EOF

SID_C="logical-phase-c-$BASHPID"
timeout -k 10s 150 "$BIN/sandbox-ctl" run --restore "$SNAPSHOT" \
    --config "$WORK/restore.yaml" --sandbox-id "$SID_C" --path-id c \
    --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" \
    --ch-binary "$BIN/cloud-hypervisor" >"$OUT/restore.log" 2>&1 &
RUN_PID=$!
wait_exec_path c || {
    tail -80 "$OUT/restore.log" >&2 || true
    e2e_fail "restored PathID sandbox did not become executable"
}

"$BIN/sandbox-ctl" exec --sandbox-id not-the-directory --path-id c \
    --run-root "$RUN_ROOT" -- /bin/sh -c 'grep -q PATH-ID-OK /scratch/path-id-marker'
[ -f "$BASE_ROOT/c/$SID_C.overlay.diff" ] || e2e_fail "restore root diff lost logical SandboxID"
[ -f "$BASE_ROOT/c/$SID_C.disk0.diff" ] || e2e_fail "restore data diff lost logical SandboxID"

RESTORE_SNAPSHOT_OUT="$BASE_ROOT/checkpoint/restore-snapshot"
"$BIN/sandbox-ctl" snapshot --path-id c --run-root "$RUN_ROOT" \
    --output "$RESTORE_SNAPSHOT_OUT"
wait "$RUN_PID"
RUN_PID=""
[ -e "$RESTORE_SNAPSHOT_OUT/$SID_C.snapshot" ] || e2e_fail "restored PathID snapshot missing"
[ ! -e "$RUN_ROOT/c" ] || e2e_fail "restored PathID RunDir survived exit"
[ -f "$RUN_ROOT/b/sibling.keep" ] && [ -f "$BASE_ROOT/b/sibling.keep" ] || \
    e2e_fail "phase cleanup removed a sibling"

echo "PASS sandbox.path-id.sh"
