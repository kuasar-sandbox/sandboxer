#!/usr/bin/env bash
# tapfd handoff and restore-network contract using prepared platform products.
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
for command in docker ip mkfs.ext4 ping timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor connector-ctl; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -r "$SANDBOXER_LIB/tarstream.sh" ] || e2e_fail "missing prepared tarstream helpers"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT"
TAP_NAME="etf$(printf '%x' "$((BASHPID % 65536))")"
RUNPID=""
RPID=""
cleanup() {
    local status=$?
    set +e
    for pid in "$RUNPID" "$RPID"; do
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill -TERM "$pid" 2>/dev/null
            wait "$pid" 2>/dev/null
        fi
    done
    exit "$status"
}
trap cleanup EXIT

# Allocate disjoint cold/restore /31s that are unused on the host. Restore must
# necessarily change the guest identity so it exercises network re-apply.
declare -A HOST_IPV4_ADDRESSES=() HOST_IPV4_ROUTES=()
while read -r address; do
    HOST_IPV4_ADDRESSES["${address%/*}"]=1
done < <(ip -o -4 addr show | awk '{print $4}')
while read -r destination _; do
    [[ "$destination" == */* ]] && HOST_IPV4_ROUTES["$destination"]=1
done < <(ip -4 route show table all type unicast)

network_in_use() {
    [ -n "${HOST_IPV4_ROUTES[$1]:-}" ] || \
        [ -n "${HOST_IPV4_ADDRESSES[$2]:-}" ] || \
        [ -n "${HOST_IPV4_ADDRESSES[$3]:-}" ]
}

allocate_networks() {
    local start=$(( BASHPID % 8192 )) offset slot block host_byte
    local cold_host_ip cold_guest_ip cold_cidr restore_host_ip restore_guest_ip restore_cidr
    for ((offset = 0; offset < 8192; offset++)); do
        slot=$(( (start + offset) % 8192 ))
        block=$(( slot / 128 ))
        host_byte=$(( (slot % 128) * 2 ))
        cold_host_ip="169.254.$((64 + block)).$host_byte"
        cold_guest_ip="169.254.$((64 + block)).$((host_byte + 1))"
        cold_cidr="$cold_host_ip/31"
        restore_host_ip="169.254.$((128 + block)).$host_byte"
        restore_guest_ip="169.254.$((128 + block)).$((host_byte + 1))"
        restore_cidr="$restore_host_ip/31"
        if ! network_in_use "$cold_cidr" "$cold_host_ip" "$cold_guest_ip" && \
            ! network_in_use "$restore_cidr" "$restore_host_ip" "$restore_guest_ip"; then
            COLD_HOST_CIDR="$cold_cidr"
            COLD_GUEST_IP="$cold_guest_ip"
            RESTORE_HOST_CIDR="$restore_cidr"
            RESTORE_GUEST_IP="$restore_guest_ip"
            return 0
        fi
    done
    e2e_fail "no unused tapfd /31 pair is available"
}
allocate_networks

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"
GUEST_MAC="02:00:00:00:80:01"

mkdiff() {
    truncate -s 1G "$1"
    mkfs.ext4 -q -F -O ^has_journal "$1"
}

write_config() {
    local output=$1 guest_ip=$2 diff=$3 host_cidr=$4 with_launch=$5
    cat >"$output" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tapfd:
    exec: ["$BIN/connector-ctl", "tapfd", "get", "--new", "--host-cidr", "$host_cidr",
           "--mac", "$GUEST_MAC", "--ip", "$guest_ip", "$TAP_NAME"]
  ip: $guest_ip/31
  mtu: 1400
  hostname: e2e-tapfd
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
EOF
    if [ "$with_launch" = 1 ]; then
        cat >>"$output" <<EOF
  cmdline: "console=hvc0"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$diff }
launch:
  args: ["-c", "import time; print('MTU='+open('/sys/class/net/eth0/mtu').read().strip(), flush=True); print('NETUP', flush=True); time.sleep(60)"]
  restart: never
EOF
    else
        cat >>"$output" <<EOF
  root:
    overlay: { diff: file://$diff }
EOF
    fi
}

wait_marker() {
    local pattern=$1 log=$2 pid=$3
    for _ in $(seq 1 600); do
        grep -qE "$pattern" "$log" 2>/dev/null && return 0
        kill -0 "$pid" 2>/dev/null || return 1
        sleep 0.05
    done
    return 1
}

ping_guest() {
    local guest=$1
    for _ in $(seq 1 24); do
        ping -I "$TAP_NAME" -c1 -W1 "$guest" >/dev/null 2>&1 && return 0
        sleep 0.25
    done
    return 1
}

DIFF0="$WORK/cold.diff"
mkdiff "$DIFF0"
write_config "$WORK/cold.yaml" "$COLD_GUEST_IP" "$DIFF0" "$COLD_HOST_CIDR" 1
SID="tapfd-cold-$BASHPID"
COLD_LOG="$OUT/cold.log"
mkdir -p "$WORK/runtime"
timeout -k 10s 120 "$BIN/sandbox-ctl" run \
    --config "$WORK/cold.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime" \
    --sandbox-id "$SID" >"$COLD_LOG" 2>&1 &
RUNPID=$!
wait_marker '^NETUP$' "$COLD_LOG" "$RUNPID" || {
    tail -60 "$COLD_LOG" >&2 || true
    e2e_fail "guest did not reach NETUP through tapfd"
}
assert_contains "$COLD_LOG" 'tapfd: received tap fd'
grep -qE "net fd=[0-9]+,mac=$GUEST_MAC,id=_net0" "$COLD_LOG" || \
    e2e_fail "Cloud Hypervisor did not receive fd-mode network"
assert_contains "$COLD_LOG" 'MTU=1400'
ping_guest "$COLD_GUEST_IP" || e2e_fail "host could not ping cold guest through handed-off tap fd"

SNAP_DIR="$WORK/snapshot"
mkdir -p "$SNAP_DIR"
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID" --output "$SNAP_DIR" \
    --run-root "$WORK/runtime" >"$OUT/snapshot.log" 2>&1
wait "$RUNPID"
RUNPID=""
SNAPSHOT="$SNAP_DIR/$SID.snapshot"
[ -f "$SNAPSHOT" ] || e2e_fail "snapshot output is missing"

DIFF1="$WORK/restore.diff"
mkdiff "$DIFF1"
write_config "$WORK/restore.yaml" "$RESTORE_GUEST_IP" "$DIFF1" "$RESTORE_HOST_CIDR" 0
SIDR="tapfd-restore-$BASHPID"
RESTORE_LOG="$OUT/restore.log"
mkdir -p "$WORK/runtime-restore"
timeout -k 10s 120 "$BIN/sandbox-ctl" run --restore "$SNAPSHOT" \
    --config "$WORK/restore.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$WORK/runtime-restore" \
    --sandbox-id "$SIDR" >"$RESTORE_LOG" 2>&1 &
RPID=$!
wait_marker 'restore network re-applied|restore notify acked|VM resumed' "$RESTORE_LOG" "$RPID" || {
    tail -60 "$RESTORE_LOG" >&2 || true
    e2e_fail "restore did not complete"
}
assert_contains "$RESTORE_LOG" 'tapfd: received tap fd for restore'
assert_contains "$RESTORE_LOG" 'net_fds=[_net0@['
ping_guest "$RESTORE_GUEST_IP" || e2e_fail "host could not ping restored guest at its new identity"

kill -TERM "$RPID"
wait "$RPID" || true
RPID=""

echo "PASS network.tapfd.sh"
