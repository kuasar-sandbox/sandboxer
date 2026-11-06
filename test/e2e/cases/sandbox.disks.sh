#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","tun"]
# Live disk semantics and default discard on all four writable ext4 mount paths.
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
for command in docker ip mkfs.ext4 tar timeout truncate; do
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

mkdir -p "$WORK" "$OUT" "$WORK/runtime" "$WORK/base"
TAP_NAME="dk$((BASHPID % 1000000))"
TAP_CREATED=0
RUNPID=""
ROOT_CONTAINER=""
cleanup() {
    local status=$?
    set +e
    [ -n "$RUNPID" ] && kill -0 "$RUNPID" 2>/dev/null && kill -TERM "$RUNPID" 2>/dev/null
    [ -n "$RUNPID" ] && wait "$RUNPID" 2>/dev/null
    [ -n "$ROOT_CONTAINER" ] && docker rm -f "$ROOT_CONTAINER" >/dev/null 2>&1
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
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
truncate -s 512M "$WORK/root-up.ext4.template"
mkfs.ext4 -q -F -O ^has_journal "$WORK/root-up.ext4.template"
truncate -s 256M "$WORK/scratch.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/scratch.ext4"
mkdir -p "$WORK/dataset-source"
printf 'DATASET-OK\n' >"$WORK/dataset-source/DATASET-OK"
"$BIN/flatten-ctl" export --no-progress \
    --tmpdir "$WORK/tmp" --output "$WORK/dataset.img" "$WORK/dataset-source"
[ -s "$WORK/dataset.img" ] || e2e_fail "dataset flatten produced an empty artifact"
truncate -s 256M "$WORK/dataset-up.ext4"
mkfs.ext4 -q -F -O ^has_journal "$WORK/dataset-up.ext4"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"
DATASET_REF="$(plaintext_tarstream_ref "$WORK/dataset.img")"

cat >"$WORK/sandbox.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: e2e-disks }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$WORK/root-up.ext4, diff_template: file://$WORK/root-up.ext4.template }
  disks:
    - { name: scratch, diff_template: file://$WORK/scratch.ext4 }
    - { name: dataset, base: $DATASET_REF, overlay: { diff_template: file://$WORK/dataset-up.ext4 } }
mounts:
  - { target: /scratch, type: disk, source: scratch }
  - { target: /data, type: disk, source: dataset }
launch: { exec: /bin/sleep, args: ["3600"], restart: never }
EOF

start_vm() {
    local config=$1
    shift
    timeout -k 10s 120 "$BIN/sandbox-ctl" run \
        --config "$config" --sandbox-id "$SID" "$@" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --base-root "$WORK/base" \
        >"$LOG" 2>&1 &
    RUNPID=$!
    for _ in $(seq 1 90); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" \
            -- /bin/true >/dev/null 2>&1; then
            return
        fi
        kill -0 "$RUNPID" 2>/dev/null || {
            tail -60 "$LOG" >&2 || true
            e2e_fail "$SID exited before becoming executable"
        }
        sleep 1
    done
    tail -60 "$LOG" >&2 || true
    e2e_fail "$SID did not become executable"
}

check_discard_mounts() {
    local topology=$1 evidence="$OUT/$1-discard.log"
    if ! timeout -k 2s 30 "$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/sh -c '
        set -eu
        cat /proc/mounts
        if [ "$1" = multi ]; then
            devices="vdb vdc vde"
            targets="/ /scratch /data"
            grep -q " / overlay " /proc/mounts
            grep -q " /scratch ext4 " /proc/mounts
            grep -q " /data overlay " /proc/mounts
        else
            devices=vda
            targets=/
            grep -q " / ext4 " /proc/mounts
        fi
        for device in $devices; do
            options="/proc/fs/ext4/$device/options"
            echo "Checking default discard on $device"
            cat "$options"
            grep -qx discard "$options"
            if grep -qx nodiscard "$options"; then
                echo "Unexpected nodiscard on $device" >&2
                exit 1
            fi
            test "$(cat /sys/block/$device/queue/discard_max_bytes)" -gt 0
        done
        expected=$(mktemp)
        dd if=/dev/urandom of="$expected" bs=4096 count=16 2>/dev/null
        test "$(wc -c < "$expected")" -eq 65536
        for target in $targets; do
            probe=$(mktemp "${target%/}/.discard-probe.XXXXXX")
            dd if="$expected" of="$probe" bs=4096 count=16 conv=fsync 2>/dev/null
            cmp "$expected" "$probe"
            rm "$probe"
            test ! -e "$probe"
            echo "Verified 64 KiB write/read/delete on $target"
        done
        rm "$expected"
        if [ "$1" = multi ]; then
            grep -qx DATASET-OK /data/DATASET-OK
            grep -qx S-OK /scratch/persist
            grep -qx D-OK /data/persist
        fi
    ' discard-check "$topology" >"$evidence" 2>&1; then
        cat "$evidence" >&2
        tail -60 "$LOG" >&2 || true
        e2e_fail "$topology default discard or file I/O check failed"
    fi
}

SID="disks-$BASHPID"
LOG="$OUT/disks.log"
start_vm "$WORK/sandbox.yaml"

"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- \
    /bin/sh -c 'ls -1 /dev/vd*' >"$WORK/devices.out"
for device in vda vdb vdc vdd vde; do
    grep -qx "/dev/$device" "$WORK/devices.out" || e2e_fail "missing /dev/$device"
done

"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/sh -c '
    set -e
    grep -qx DATASET-OK /data/DATASET-OK
    echo S-OK > /scratch/persist
    grep -qx S-OK /scratch/persist
    echo D-OK > /data/persist
    grep -qx D-OK /data/persist
'
check_discard_mounts multi

kill -TERM "$RUNPID"
wait "$RUNPID" || true
RUNPID=""

# A populated single ext4 root exercises the remaining sandbox-init mount path.
# Export only the prepared image; remove only the container created here.
mkdir -p "$WORK/single-rootfs"
ROOT_CONTAINER=$(docker create --pull=never --network none "$E2E_IMAGE" /bin/true)
docker export "$ROOT_CONTAINER" | tar --numeric-owner -xpf - -C "$WORK/single-rootfs"
docker rm "$ROOT_CONTAINER" >/dev/null
ROOT_CONTAINER=""
truncate -s 1G "$WORK/single-root.ext4"
mkfs.ext4 -q -F -O ^has_journal -b 4096 -d "$WORK/single-rootfs" "$WORK/single-root.ext4"
cat >"$WORK/single-root.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root: { diff_template: file://$WORK/single-root.ext4 }
launch: { exec: /bin/sleep, args: ["3600"], restart: never }
EOF
SID="disks-single-$BASHPID"
LOG="$OUT/disks-single.log"
start_vm "$WORK/single-root.yaml" --enable-checkpoint
check_discard_mounts single

kill -TERM "$RUNPID"
wait "$RUNPID" || true
RUNPID=""

echo "PASS sandbox.disks.sh"
