#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","tun"]
# A manifest-backed image cold-boots through the prepared store/cache path and preserves image config.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker grep ip mkfs.ext4 openssl python3 timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl cloud-hypervisor flatten-ctl manifest-ctl store-ctl cache-ctl; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/run" "$WORK/base"
TAP_NAME="xm$((BASHPID % 1000000))"
TAP_CREATED=0
PIDS=()
manifest_ready_diagnostics() {
    local phase=$1 name pid status
    printf 'manifest boot readiness: phase=%s store_port=%s cache_port=%s cache_health_port=%s cache_ping_exit=%s\n' \
        "$phase" "$STORE_PORT" "$CACHE_PORT" "$CACHE_HEALTH_PORT" "${CACHE_PING_RC:-not-run}" >&2
    for name in STORE CACHE; do
        if [ "$name" = STORE ]; then pid=${STORE_PID:-}; else pid=${CACHE_PID:-}; fi
        if [ -z "$pid" ]; then
            printf 'manifest boot readiness: %s not-started\n' "$name" >&2
        elif kill -0 "$pid" 2>/dev/null; then
            printf 'manifest boot readiness: %s pid=%s alive\n' "$name" "$pid" >&2
        else
            if wait "$pid" 2>/dev/null; then status=0; else status=$?; fi
            printf 'manifest boot readiness: %s pid=%s exited status=%s\n' "$name" "$pid" "$status" >&2
        fi
    done
    # Read only known readiness logs, never configuration or the manifest key.
    python3 - "$OUT" <<'PY' >&2
import os
from pathlib import Path
import re
import stat
import sys

for name in ("store.log", "cache.log", "cache-ping.stdout", "cache-ping.stderr"):
    print(f"manifest boot readiness: {name} tail (at most 8192 bytes / 80 lines)")
    try:
        fd = os.open(Path(sys.argv[1]) / name, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
        with os.fdopen(fd, "rb") as source:
            info = os.fstat(source.fileno())
            if not stat.S_ISREG(info.st_mode):
                raise OSError("not a regular log")
            source.seek(max(0, info.st_size - 8192))
            tail = source.read(8192).decode(errors="replace")
        print(re.sub(r"(?i)[0-9a-f]{64}", "<redacted>", "\n".join(tail.splitlines()[-80:])))
    except OSError:
        print("unavailable")
PY
}
cleanup() {
    local status=$?
    set +e
    for pid in "${PIDS[@]}"; do
        kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    done
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    exit "$status"
}
trap cleanup EXIT

ip tuntap add dev "$TAP_NAME" mode tap
TAP_CREATED=1
ip addr add 169.254.1.0/31 dev "$TAP_NAME"
ip link set "$TAP_NAME" up

free_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

KEY=$(openssl rand -hex 32)
STORE_PORT=$(free_port)
CACHE_PORT=$(free_port)
CACHE_HEALTH_PORT=$(free_port)

cat >"$WORK/store.yaml" <<EOF
listen: 127.0.0.1:$STORE_PORT
backend: fs
fs:
  root: $WORK/store-data
  verify_content_key: true
EOF
"$BIN/store-ctl" init --config "$WORK/store.yaml" --generation G1
"$BIN/store-ctl" serve --config "$WORK/store.yaml" >"$OUT/store.log" 2>&1 &
STORE_PID=$!
PIDS+=("$STORE_PID")
for _ in $(seq 1 100); do
    (echo >/dev/tcp/127.0.0.1/"$STORE_PORT") 2>/dev/null && break
    sleep 0.05
done
(echo >/dev/tcp/127.0.0.1/"$STORE_PORT") 2>/dev/null || {
    manifest_ready_diagnostics store || true
    e2e_fail "store did not become ready"
}

cat >"$WORK/cache.yaml" <<EOF
mode: tiered
listen: 127.0.0.1:$CACHE_PORT
health_listen: 127.0.0.1:$CACHE_HEALTH_PORT
rpc_timeout: 5s
freq:
  counters: 1M
  reset_after: 100K
tiers:
  - type: embedded
    rocks:
      path: $WORK/cache-rocks
      disk_bytes: 1GiB
      mem_ratio: 0.1
      direct_reads: false
      bloom_bits: 10
origin:
  type: store
  store:
    endpoint: 127.0.0.1:$STORE_PORT
    pool: 2
    timeout: 5s
  max_inflight: 16
EOF
"$BIN/cache-ctl" serve --config "$WORK/cache.yaml" >"$OUT/cache.log" 2>&1 &
CACHE_PID=$!
PIDS+=("$CACHE_PID")
CACHE_READY=0
for _ in $(seq 1 100); do
    if "$BIN/cache-ctl" ping --endpoint "127.0.0.1:$CACHE_HEALTH_PORT" \
        >"$OUT/cache-ping.stdout" 2>"$OUT/cache-ping.stderr"; then
        CACHE_PING_RC=0
    else
        CACHE_PING_RC=$?
    fi
    if [ "$CACHE_PING_RC" -eq 0 ] && grep -qx 'SERVING' "$OUT/cache-ping.stdout"; then
        CACHE_READY=1
        break
    fi
    sleep 0.05
done
[ "$CACHE_READY" = 1 ] || {
    manifest_ready_diagnostics cache || true
    e2e_fail "cache did not become SERVING"
}

cat >"$WORK/manifest.yaml" <<EOF
manifest:
  key: "$KEY"
store:
  endpoint: 127.0.0.1:$STORE_PORT
  pool: 4
  timeout: 10s
cache:
  endpoint: 127.0.0.1:$CACHE_PORT
  pool: 4
  timeout: 5s
chunker:
  mode: cdc
crypto:
  chunk: aes
  manifest: aes
EOF

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
MKEY=$("$BIN/manifest-ctl" store --manifest-config "$WORK/manifest.yaml" --no-progress "$ROOT_IMAGE")
[[ "$MKEY" =~ ^[0-9a-f]{64}$ ]] || e2e_fail "manifest store returned a non-canonical key: $MKEY"

DIFF_FILE="$WORK/root.ext4"
truncate -s 1G "$DIFF_FILE.template"
mkfs.ext4 -q -F -O ^has_journal "$DIFF_FILE.template"
cat >"$WORK/sandbox.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: manifest-boot
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: manifest://$MKEY
    overlay: { diff: file://$DIFF_FILE, diff_template: file://$DIFF_FILE.template }
launch:
  args: ["-c", "import sys; print('PYBOOT-OK', sys.version_info.major*100+sys.version_info.minor)"]
  restart: never
EOF

SID="manifest-boot-$BASHPID"
LOG="$OUT/run.log"
set +e
timeout -k 10s 90 "$BIN/sandbox-ctl" run \
    --config "$WORK/sandbox.yaml" \
    --manifest-config "$WORK/manifest.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/run" \
    --base-root "$WORK/base" \
    --sandbox-id "$SID" \
    --stats-json "$OUT/stats.json" >"$LOG" 2>&1
RC=$?
set -e
[ "$RC" -eq 0 ] || {
    tail -80 "$LOG" >&2 || true
    e2e_fail "manifest-backed cold boot failed: exit=$RC"
}

grep -q 'manifest fetcher: store=' "$LOG" || e2e_fail "manifest-backed root did not engage the manifest fetcher"
grep -qE '^PYBOOT-OK [0-9]+$' "$LOG" || e2e_fail "image command did not execute from the manifest-backed root"
grep -q 'image config: cmd=\[python3\]' "$LOG" || e2e_fail "image config was not extracted from the manifest-backed image"
[ -s "$OUT/stats.json" ] || e2e_fail "manifest-backed cold boot did not produce stats"

echo "PASS image.manifest-boot.sh"
