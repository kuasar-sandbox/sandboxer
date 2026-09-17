#!/usr/bin/env bash
#
# e2e_sandbox_artifact.sh — Sandbox E lifecycle:
#   - two live export --resume captures from one immutable C0
#   - run --from performs a cold start from each captured disk point
#   - persistent and ephemeral file/env semantics stay separated
#   - init runs again on cold start while the original active diff stays bound
#   - image-to-Sandbox-E assembly produces a direct EROFS Sandbox E without a VM
#   - direct EROFS run --from requires a pre-formatted writable upper before CH

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
BIN="${BIN:-$REPO_ROOT/bin}"
IMAGE="${IMAGE:-python:3.12-slim}"

skip() {
    echo
    echo "==> e2e_sandbox_artifact: skipping ($*)"
    if [ "${REQUIRE_KVM:-0}" = "1" ]; then exit 1; fi
    exit 0
}

[ -e /dev/kvm ] || skip "/dev/kvm not present"
for b in cloud-hypervisor sandbox-ctl sandbox-init sandbox-runtime.bundle flatten-ctl; do
    [ -e "$BIN/$b" ] || skip "missing $BIN/$b — run 'make build'"
done
VMLINUX="${VMLINUX:-$BIN/vmlinux}"
[ -f "$VMLINUX" ] || skip "no vmlinux at $VMLINUX"
command -v mkfs.ext4 >/dev/null 2>&1 || skip "mkfs.ext4 not on PATH"
if [ "$(id -u)" -ne 0 ]; then
    exec sudo -nE "$0" "$@"
fi

TAP_NAME="${TAP_NAME:-sb-tap0}"
TAP_CREATED_BY_TEST=0
if ! ip link show "$TAP_NAME" >/dev/null 2>&1; then
    ip tuntap add dev "$TAP_NAME" mode tap
    ip addr add 169.254.1.0/31 dev "$TAP_NAME"
    ip link set "$TAP_NAME" up
    TAP_CREATED_BY_TEST=1
fi

WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/e2e-artifact-XXXXXX")"
SOURCE_PID=""
trap '
    if [ -n "$SOURCE_PID" ] && kill -0 "$SOURCE_PID" 2>/dev/null; then
        kill -TERM "$SOURCE_PID" 2>/dev/null || true
        wait "$SOURCE_PID" 2>/dev/null || true
    fi
    [ -n "${E2E_KEEP:-}" ] && echo "kept: $WORK" || rm -rf "$WORK"
    [ "$TAP_CREATED_BY_TEST" = "1" ] && ip link del "$TAP_NAME" 2>/dev/null || true
' EXIT

BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker missing; provide BLK0_IMAGE"
    docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >/dev/null
    BLK0_IMAGE="$WORK/blk0.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"

RUN_ROOT="$WORK/run"
BASE_ROOT="$WORK/base"
mkdir -p "$RUN_ROOT" "$BASE_ROOT"

SOURCE_DIFF="$WORK/source.diff"
truncate -s 1G "$SOURCE_DIFF"
mkfs.ext4 -q -F "$SOURCE_DIFF"

cat > "$WORK/source.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: artifact-source
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay:
      diff: file://$SOURCE_DIFF
files:
  - path: /etc/kuasar-persistent
    content: declared
ephemeral_files:
  - path: /etc/kuasar-ephemeral
    content: source-only
init:
  - exec: /bin/sh
    args: ["-c", "n=0; [ ! -f /init-count ] || n=\$(cat /init-count); n=\$((n+1)); printf '%s\\n' \"\$n\" > /init-count"]
launch:
  exec: /bin/sh
  args: ["-c", "trap 'exit 0' TERM INT; while :; do sleep 1; done"]
  env: { PORTABLE: artifact }
  ephemeral_env: { INSTANCE: source-only }
  restart: never
metadata: { e2e: artifact-lifecycle }
EOF

wait_exec() { # sid, sandbox-ctl pid
    local sid="$1" run_pid="$2"
    for _ in $(seq 1 600); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RUN_ROOT" -- /bin/true \
            >/dev/null 2>&1; then
            return 0
        fi
        kill -0 "$run_pid" 2>/dev/null || return 1
        sleep 0.05
    done
    return 1
}

SOURCE_SID="artifact-source-$$"
SOURCE_LOG="$WORK/source.log"
echo "==> starting source sandbox $SOURCE_SID"
"$BIN/sandbox-ctl" run --config "$WORK/source.yaml" --sandbox-id "$SOURCE_SID" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" \
    >"$SOURCE_LOG" 2>&1 &
SOURCE_PID=$!
wait_exec "$SOURCE_SID" "$SOURCE_PID" \
    || { echo "FAIL: source sandbox did not become ready"; tail -80 "$SOURCE_LOG"; exit 1; }

"$BIN/sandbox-ctl" exec --sandbox-id "$SOURCE_SID" --run-root "$RUN_ROOT" -- \
    /bin/sh -c 'printf "%s\n" mutated > /etc/kuasar-persistent; printf "%s\n" E1 > /generation; sync'

C0="$RUN_ROOT/$SOURCE_SID/sandbox.runtime.cfg"
[ -f "$C0" ] || { echo "FAIL: immutable C0 was not written at $C0"; exit 1; }
C0_SHA="$(sha256sum "$C0" | awk '{print $1}')"
DIFF_BINDING="$(stat -Lc '%d:%i' "$SOURCE_DIFF")"

assert_live_baseline() {
    local phase="$1"
    [ "$(sha256sum "$C0" | awk '{print $1}')" = "$C0_SHA" ] \
        || { echo "FAIL: C0 changed after $phase"; exit 1; }
    [ "$(stat -Lc '%d:%i' "$SOURCE_DIFF")" = "$DIFF_BINDING" ] \
        || { echo "FAIL: active diff binding changed after $phase"; exit 1; }
    kill -0 "$SOURCE_PID" 2>/dev/null \
        || { echo "FAIL: source sandbox stopped after $phase"; tail -80 "$SOURCE_LOG"; exit 1; }
    "$BIN/sandbox-ctl" exec --sandbox-id "$SOURCE_SID" --run-root "$RUN_ROOT" -- /bin/true
}

E1_OUT="$WORK/e1"
mkdir -p "$E1_OUT"
echo "==> live export E1 with --resume"
"$BIN/sandbox-ctl" export --sandbox-id "$SOURCE_SID" --run-root "$RUN_ROOT" \
    --output "$E1_OUT" --resume | tee "$WORK/export-e1.log"
E1="$E1_OUT/$SOURCE_SID.sandbox"
[ -L "$E1" ] || { echo "FAIL: E1 alias was not committed"; ls -la "$E1_OUT"; exit 1; }
E1_TARGET="$(readlink "$E1")"
assert_live_baseline "E1 export"
E1_INFO="$WORK/e1.yaml"
"$BIN/sandbox-ctl" info "$E1" >"$E1_INFO"
grep -q '^version: 1$' "$E1_INFO" || { echo "FAIL: E1 has no portable V1 config"; exit 1; }
grep -q 'kuasar-persistent' "$E1_INFO" || { echo "FAIL: persistent file missing from E1"; exit 1; }
grep -q 'PORTABLE: artifact' "$E1_INFO" || { echo "FAIL: persistent env missing from E1"; exit 1; }
if grep -qE 'ephemeral|source-only|mutated' "$E1_INFO"; then
    echo "FAIL: E1 leaked ephemeral input or injected-file runtime mutation"
    cat "$E1_INFO"
    exit 1
fi

"$BIN/sandbox-ctl" exec --sandbox-id "$SOURCE_SID" --run-root "$RUN_ROOT" -- \
    /bin/sh -c 'printf "%s\n" E2 > /generation; sync'
E2_OUT="$WORK/e2"
mkdir -p "$E2_OUT"
echo "==> live export E2 from the same C0 with --resume"
"$BIN/sandbox-ctl" export --sandbox-id "$SOURCE_SID" --run-root "$RUN_ROOT" \
    --output "$E2_OUT" --resume | tee "$WORK/export-e2.log"
E2="$E2_OUT/$SOURCE_SID.sandbox"
[ -L "$E2" ] || { echo "FAIL: E2 alias was not committed"; ls -la "$E2_OUT"; exit 1; }
E2_TARGET="$(readlink "$E2")"
[ "$E1_TARGET" != "$E2_TARGET" ] \
    || { echo "FAIL: distinct disk points produced the same Sandbox E: $E1_TARGET"; exit 1; }
assert_live_baseline "E2 export"

kill -TERM "$SOURCE_PID"
wait "$SOURCE_PID" 2>/dev/null || true
SOURCE_PID=""

run_from_export() { # artifact, expected generation, suffix
    local artifact="$1" expected="$2" suffix="$3"
    local diff="$WORK/from-$suffix.diff"
    local cfg="$WORK/from-$suffix.yaml"
    local log="$WORK/from-$suffix.log"
    local sid="artifact-from-$suffix-$$"
    truncate -s 1G "$diff"
    cat > "$cfg" <<EOF
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: artifact-from-$suffix
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff: file://$diff
ephemeral_files:
  - path: /etc/kuasar-ephemeral
    content: clone-only
launch:
  exec: /bin/sh
  args: ["-c", "printf 'GEN=%s PERSIST=%s EPH=%s ENV=%s/%s INIT=%s\\n' \"\$(cat /generation)\" \"\$(cat /etc/kuasar-persistent)\" \"\$(cat /etc/kuasar-ephemeral)\" \"\$PORTABLE\" \"\$INSTANCE\" \"\$(cat /init-count)\""]
  ephemeral_env: { INSTANCE: clone-only }
EOF
    echo "==> run --from $suffix (expect disk point $expected)"
    set +e
    timeout -k 10s 90 "$BIN/sandbox-ctl" run --from "$artifact" --config "$cfg" \
        --sandbox-id "$sid" --ch-binary "$BIN/cloud-hypervisor" \
        --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" >"$log" 2>&1
    local rc=$?
    set -e
    [ "$rc" -eq 0 ] \
        || { echo "FAIL: run --from $suffix exited $rc"; tail -100 "$log"; exit 1; }
    grep -Fq "GEN=$expected PERSIST=declared EPH=clone-only ENV=artifact/clone-only INIT=2" "$log" \
        || { echo "FAIL: run --from $suffix did not apply cold-start semantics"; tail -100 "$log"; exit 1; }
}

run_from_export "$E1" E1 e1
run_from_export "$E2" E2 e2

ASSEMBLY_TEMPLATE="$WORK/assembly-template.ext4"
truncate -s 512M "$ASSEMBLY_TEMPLATE"
mkfs.ext4 -q -F "$ASSEMBLY_TEMPLATE"
cat > "$WORK/assembly.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay:
      diff_template: file://$ASSEMBLY_TEMPLATE
launch:
  exec: /bin/true
  restart: never
EOF

ASSEMBLY_OUT="$WORK/assembly-out"
mkdir -p "$ASSEMBLY_OUT"
echo "==> image-to-Sandbox-E assembly"
"$BIN/sandbox-ctl" export --from "$BLK0_IMAGE" --config "$WORK/assembly.yaml" \
    --sandbox-id assembly --output "$ASSEMBLY_OUT" | tee "$WORK/export-assembly.log"
ASSEMBLY_E="$ASSEMBLY_OUT/assembly.sandbox"
[ -L "$ASSEMBLY_E" ] || { echo "FAIL: assembled Sandbox E alias was not committed"; ls -la "$ASSEMBLY_OUT"; exit 1; }
"$BIN/sandbox-ctl" info --json "$ASSEMBLY_E" >"$WORK/assembly-info.json"
python3 - "$WORK/assembly-info.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    cfg = json.load(source)
root = cfg["Boot"]["Root"]
overlay = root.get("Overlay") or {}
if (root.get("Base") != "self" or root.get("BaseFromRefs")
        or overlay.get("Base") or overlay.get("BaseFromRefs")):
    raise SystemExit(f"assembled E is not direct EROFS self layout: {root!r}")
PY

# The wrapper is an observable CH side effect. Missing direct-EROFS upper must
# fail during config/disk preflight and therefore never execute it.
CH_MARKER="$WORK/ch-started"
CH_WRAPPER="$WORK/cloud-hypervisor-marker"
cat > "$CH_WRAPPER" <<EOF
#!/bin/sh
touch "$CH_MARKER"
exec "$BIN/cloud-hypervisor" "\$@"
EOF
chmod 0755 "$CH_WRAPPER"
cat > "$WORK/assembly-missing-upper.yaml" <<EOF
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay: {}
EOF
set +e
"$BIN/sandbox-ctl" run --from "$ASSEMBLY_E" --config "$WORK/assembly-missing-upper.yaml" \
    --sandbox-id assembly-reject --ch-binary "$CH_WRAPPER" --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" \
    >"$WORK/assembly-reject.log" 2>&1
ASSEMBLY_REJECT_RC=$?
set -e
[ "$ASSEMBLY_REJECT_RC" -ne 0 ] || { echo "FAIL: direct EROFS run accepted a missing upper"; exit 1; }
[ ! -e "$CH_MARKER" ] \
    || { echo "FAIL: missing direct-EROFS upper failed only after Cloud Hypervisor started"; exit 1; }
grep -qE 'diff_template|mountable|formatted|upper' "$WORK/assembly-reject.log" \
    || { echo "FAIL: missing-upper error lacked field context"; cat "$WORK/assembly-reject.log"; exit 1; }

cat > "$WORK/assembly-host.yaml" <<EOF
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff_template: file://$ASSEMBLY_TEMPLATE
launch:
  exec: /bin/sh
  args: ["-c", "echo ASSEMBLY-RUN-OK"]
EOF
set +e
timeout -k 10s 90 "$BIN/sandbox-ctl" run --from "$ASSEMBLY_E" --config "$WORK/assembly-host.yaml" \
    --sandbox-id assembly-run --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" >"$WORK/assembly-run.log" 2>&1
ASSEMBLY_RUN_RC=$?
set -e
[ "$ASSEMBLY_RUN_RC" -eq 0 ] \
    || { echo "FAIL: direct EROFS run --from exited $ASSEMBLY_RUN_RC"; tail -100 "$WORK/assembly-run.log"; exit 1; }
grep -q '^ASSEMBLY-RUN-OK$' "$WORK/assembly-run.log" \
    || { echo "FAIL: direct EROFS run --from did not start the cold app"; tail -100 "$WORK/assembly-run.log"; exit 1; }

echo "==> PASS: live E1/E2, immutable C0, cold semantics, and direct EROFS assembly"
echo "==> e2e_sandbox_artifact: OK"
