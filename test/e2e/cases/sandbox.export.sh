#!/usr/bin/env bash
# Live export creates immutable Sandbox E disk points with cold-start semantics.
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
for command in docker grep ip mkfs.ext4 sha256sum stat timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl cloud-hypervisor flatten-ctl; do
    require_binary "$binary"
done
for file in sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/run" "$WORK/base"
RUN_ROOT="$WORK/run"
BASE_ROOT="$WORK/base"
TAP_NAME="xe$((BASHPID % 1000000))"
TAP_CREATED=0
SOURCE_PID=""
cleanup() {
    local status=$?
    set +e
    if [ -n "$SOURCE_PID" ] && kill -0 "$SOURCE_PID" 2>/dev/null; then
        kill -TERM "$SOURCE_PID" 2>/dev/null || true
        wait "$SOURCE_PID" 2>/dev/null || true
    fi
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    exit "$status"
}
trap cleanup EXIT

ip tuntap add dev "$TAP_NAME" mode tap
TAP_CREATED=1
ip addr add 169.254.1.0/31 dev "$TAP_NAME"
ip link set "$TAP_NAME" up

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
ROOT_REF=$(plaintext_tarstream_ref "$ROOT_IMAGE")
SOURCE_DIFF="$WORK/source.diff"
truncate -s 1G "$SOURCE_DIFF"
mkfs.ext4 -q -F -O ^has_journal "$SOURCE_DIFF"

cat >"$WORK/source.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: export-source }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$SOURCE_DIFF }
files:
  - { path: /etc/kuasar-persistent, content: declared }
ephemeral_files:
  - { path: /etc/kuasar-ephemeral, content: source-only }
init:
  - exec: /bin/sh
    args: ["-c", "n=0; [ ! -f /init-count ] || n=\$(cat /init-count); n=\$((n+1)); printf '%s\\n' \"\$n\" > /init-count"]
launch:
  exec: /bin/sh
  args: ["-c", "trap 'exit 0' TERM INT; while :; do sleep 1; done"]
  env: { PORTABLE: artifact }
  ephemeral_env: { INSTANCE: source-only }
  restart: never
EOF

wait_exec() {
    local sid=$1 pid=$2 log=$3
    for _ in $(seq 1 600); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RUN_ROOT" -- /bin/true >/dev/null 2>&1; then
            return
        fi
        kill -0 "$pid" 2>/dev/null || { tail -80 "$log" >&2 || true; e2e_fail "source sandbox exited before exec readiness"; }
        sleep 0.05
    done
    e2e_fail "source sandbox did not become executable"
}

SID="export-source-$BASHPID"
SOURCE_LOG="$OUT/source.log"
"$BIN/sandbox-ctl" run --config "$WORK/source.yaml" --sandbox-id "$SID" --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" >"$SOURCE_LOG" 2>&1 &
SOURCE_PID=$!
wait_exec "$SID" "$SOURCE_PID" "$SOURCE_LOG"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$RUN_ROOT" -- /bin/sh -c \
    'printf "%s\n" mutated > /etc/kuasar-persistent; printf "%s\n" E1 > /generation; sync'

C0="$RUN_ROOT/$SID/sandbox.runtime.cfg"
[ -f "$C0" ] || e2e_fail "immutable C0 was not written"
C0_SHA=$(sha256sum "$C0" | awk '{print $1}')
DIFF_BINDING=$(stat -Lc '%d:%i' "$SOURCE_DIFF")
assert_live_baseline() {
    local phase=$1
    [ "$(sha256sum "$C0" | awk '{print $1}')" = "$C0_SHA" ] || e2e_fail "C0 changed after $phase"
    [ "$(stat -Lc '%d:%i' "$SOURCE_DIFF")" = "$DIFF_BINDING" ] || e2e_fail "active diff binding changed after $phase"
    kill -0 "$SOURCE_PID" 2>/dev/null || e2e_fail "source sandbox stopped after $phase"
    "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$RUN_ROOT" -- /bin/true >/dev/null
}

E1_OUT="$OUT/e1"
mkdir -p "$E1_OUT"
"$BIN/sandbox-ctl" export --sandbox-id "$SID" --run-root "$RUN_ROOT" --output "$E1_OUT" --resume >"$OUT/export-e1.log"
E1="$E1_OUT/$SID.sandbox"
[ -L "$E1" ] || e2e_fail "E1 alias was not committed"
E1_TARGET=$(readlink "$E1")
assert_live_baseline E1
"$BIN/sandbox-ctl" info "$E1" >"$WORK/e1.info"
grep -q '^version: 1$' "$WORK/e1.info" || e2e_fail "E1 is not portable V1 config"
grep -q 'kuasar-persistent' "$WORK/e1.info" || e2e_fail "persistent file missing from E1"
grep -q 'PORTABLE: artifact' "$WORK/e1.info" || e2e_fail "persistent env missing from E1"
if grep -qE 'ephemeral|source-only|mutated' "$WORK/e1.info"; then
    e2e_fail "E1 leaked ephemeral input or injected-file runtime mutation"
fi

"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$RUN_ROOT" -- /bin/sh -c 'printf "%s\n" E2 > /generation; sync'
E2_OUT="$OUT/e2"
mkdir -p "$E2_OUT"
"$BIN/sandbox-ctl" export --sandbox-id "$SID" --run-root "$RUN_ROOT" --output "$E2_OUT" --resume >"$OUT/export-e2.log"
E2="$E2_OUT/$SID.sandbox"
[ -L "$E2" ] || e2e_fail "E2 alias was not committed"
E2_TARGET=$(readlink "$E2")
[ "$E1_TARGET" != "$E2_TARGET" ] || e2e_fail "distinct disk points produced the same Sandbox E"
assert_live_baseline E2

kill -TERM "$SOURCE_PID"
wait "$SOURCE_PID" 2>/dev/null || true
SOURCE_PID=""

run_from_export() {
    local artifact=$1 expected=$2 suffix=$3
    local diff="$WORK/from-$suffix.diff" cfg="$WORK/from-$suffix.yaml" log="$OUT/from-$suffix.log" clone="export-from-$suffix-$BASHPID"
    truncate -s 1G "$diff"
    cat >"$cfg" <<EOF
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: export-from-$suffix }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root: { overlay: { diff: file://$diff } }
ephemeral_files:
  - { path: /etc/kuasar-ephemeral, content: clone-only }
launch:
  exec: /bin/sh
  args: ["-c", "printf 'GEN=%s PERSIST=%s EPH=%s ENV=%s/%s INIT=%s\\n' \"\$(cat /generation)\" \"\$(cat /etc/kuasar-persistent)\" \"\$(cat /etc/kuasar-ephemeral)\" \"\$PORTABLE\" \"\$INSTANCE\" \"\$(cat /init-count)\""]
  ephemeral_env: { INSTANCE: clone-only }
EOF
    if ! timeout -k 10s 90 "$BIN/sandbox-ctl" run --from "$artifact" --config "$cfg" --sandbox-id "$clone" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$RUN_ROOT" --base-root "$BASE_ROOT" >"$log" 2>&1; then
        tail -100 "$log" >&2 || true
        e2e_fail "run --from $suffix failed"
    fi
    grep -Fq "GEN=$expected PERSIST=declared EPH=clone-only ENV=artifact/clone-only INIT=2" "$log" \
        || { tail -100 "$log" >&2 || true; e2e_fail "run --from $suffix lost cold-start persistent/ephemeral semantics"; }
}
run_from_export "$E1" E1 e1
run_from_export "$E2" E2 e2

echo "PASS sandbox.export.sh"
