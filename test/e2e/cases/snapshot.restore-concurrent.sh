#!/usr/bin/env bash
# Concurrent same-snapshot restore fanout and virtio-net backend transitions
# (sandboxer#161): one snapshot restored repeatedly onto distinct host backends,
# per-run snap-state/config.json rebinding, and real same-tap collision EBUSY.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/readiness_helpers.sh"
source "$SANDBOXER_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker ip mkfs.ext4 ping python3 truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor connector-ctl; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -r "$SANDBOXER_LIB/readiness_helpers.sh" ] || e2e_fail "missing prepared readiness helpers"
[ -r "$SANDBOXER_LIB/tarstream.sh" ] || e2e_fail "missing prepared tarstream helpers"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/runtime"
RUNTIME_ROOT="$WORK/runtime"

# Per-run /31 identities. Persistent host taps: A = snapshot source,
# B/C = concurrent restore targets, D = fd->tap restore target. X/Y are
# connector-ctl-managed (non-persistent) taps for the tapfd stages.
TAP_A="scr$(printf '%x' $((BASHPID % 65536)))a"
TAP_B="scr$(printf '%x' $((BASHPID % 65536)))b"
TAP_C="scr$(printf '%x' $((BASHPID % 65536)))c"
TAP_D="scr$(printf '%x' $((BASHPID % 65536)))d"
TAPFD_X="$(printf 'scrx%x' "$BASHPID")"
TAPFD_Y="$(printf 'scry%x' "$BASHPID")"
MAC_A="02:00:00:00:80:a1" # snapshot S device MAC (stable across fanout)
MAC_B="02:00:00:00:80:b1" # S_fd device MAC

declare -A HOST_IPV4_ADDRESSES=() HOST_IPV4_ROUTES=()
while read -r address; do
    HOST_IPV4_ADDRESSES["${address%/*}"]=1
done < <(ip -o -4 addr show | awk '{print $4}')
while read -r destination _; do
    [[ "$destination" == */* ]] && HOST_IPV4_ROUTES["$destination"]=1
done < <(ip -4 route show table all type unicast)

network_in_use() { # <cidr> <host_ip> <guest_ip>
    [ -n "${HOST_IPV4_ROUTES[$1]:-}" ] || \
        [ -n "${HOST_IPV4_ADDRESSES[$2]:-}" ] || \
        [ -n "${HOST_IPV4_ADDRESSES[$3]:-}" ]
}

# Allocate six disjoint free /31s: A B C D X Y (host ip = even, guest = odd).
# Slots are picked independently (first-fit) so a partially used address space
# does not defeat the allocation the way a consecutive-window scan would.
CIDRS=(); HOSTIPS=(); GUESTIPS=()
start=$(( BASHPID % 8192 ))
allocate_slot() {
    for ((offset = 0; offset < 8192; offset++)); do
        slot=$(( (start + offset) % 8192 ))
        block=$(( slot / 128 ))
        host_byte=$(( (slot % 128) * 2 ))
        host_ip="169.254.$((64 + block)).$host_byte"
        guest_ip="169.254.$((64 + block)).$((host_byte + 1))"
        if ! network_in_use "$host_ip/31" "$host_ip" "$guest_ip"; then
            CIDRS+=("$host_ip/31"); HOSTIPS+=("$host_ip"); GUESTIPS+=("$guest_ip")
            HOST_IPV4_ADDRESSES["$host_ip"]=1
            HOST_IPV4_ADDRESSES["$guest_ip"]=1
            return 0
        fi
    done
    e2e_fail "no free /31 for the restore fanout"
}
for _ in 0 1 2 3 4 5; do
    allocate_slot
done

TAPS=("$TAP_A" "$TAP_B" "$TAP_C" "$TAP_D")
TAP_CREATED=()
RUN_PIDS=()
cleanup() {
    local status=$?
    set +e
    readiness_stop_watchdog
    for pid in "${RUN_PIDS[@]:-}"; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
    done
    sleep 0.2
    for pid in "${RUN_PIDS[@]:-}"; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
        [ -n "$pid" ] && wait "$pid" 2>/dev/null
    done
    for tap in "${TAP_CREATED[@]:-}"; do
        [ -n "$tap" ] && ip link del "$tap" 2>/dev/null
    done
    exit "$status"
}
trap cleanup EXIT

for i in 0 1 2 3; do
    ip tuntap add dev "${TAPS[$i]}" mode tap
    ip addr add "${HOSTIPS[$i]}/31" dev "${TAPS[$i]}"
    ip link set "${TAPS[$i]}" up
    TAP_CREATED+=("${TAPS[$i]}")
done

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"

mkdiff() {
    truncate -s 1G "$1"
    mkfs.ext4 -q -F -O ^has_journal "$1"
}

wait_marker() { # <regex> <log> <pid> [attempts]
    local attempts="${4:-600}"
    for _ in $(seq 1 "$attempts"); do
        grep -qE "$1" "$2" 2>/dev/null && return 0
        kill -0 "$3" 2>/dev/null || return 1
        sleep 0.05
    done
    return 1
}

ping_guest() { # <tap> <guest_ip>
    for _ in $(seq 1 24); do
        ping -I "$1" -c1 -W1 "$2" >/dev/null 2>&1 && return 0
        sleep 0.25
    done
    return 1
}

# captured_net_id <sid> — the id CH captured for net[0], preserved by the
# rewrite; read from the per-restore snap-state/config.json.
captured_net_id() {
    python3 -c 'import json,sys; n=(json.load(open(sys.argv[1])).get("net") or [{}])[0]; print(n.get("id") or "")' \
        "$RUNTIME_ROOT/$1/snap-state/config.json"
}

# assert_net_config <sid> <want_tap|-> <want_fds|absent|placeholder> [want_mac] [want_id]
assert_net_config() {
    python3 - "$RUNTIME_ROOT/$1/snap-state/config.json" "$2" "$3" "${4:-}" "${5:-}" <<'PY'
import json, sys
path, want_tap, want_fds, want_mac, want_id = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
cfg = json.load(open(path))
net = cfg.get("net") or []
assert len(net) == 1, f"expected exactly one net device, got {net!r}"
n = net[0]
if want_tap == "-":
    assert not n.get("tap"), f"tap unexpectedly present: {n.get('tap')!r}"
elif want_tap != "null":
    assert n.get("tap") == want_tap, f"tap={n.get('tap')!r}, want {want_tap!r}"
if want_fds == "absent":
    assert "fds" not in n, f"fds unexpectedly present: {n.get('fds')!r}"
elif want_fds == "placeholder":
    assert n.get("fds") == [-1], f"fds={n.get('fds')!r}, want [-1]"
if want_mac:
    assert n.get("mac") == want_mac, f"mac={n.get('mac')!r}, want {want_mac!r}"
if want_id:
    assert n.get("id") == want_id, f"id={n.get('id')!r}, want {want_id!r}"
PY
}

# launch_run <snapshot|-> <cfg> <sid> <readyfile> <log> — background a cold run
# ("-") or a restore as a session leader with a readiness wire; sets
# RUN_PID and RUN_READER_PID.
launch_run() {
    local snapshot=$1
    readiness_begin_capture "$4"
    local wfd="$READY_WRITE_FD" rpid="$READY_READER_PID"
    if [ "$snapshot" = "-" ]; then
        readiness_exec_in_new_session "$BIN/sandbox-ctl" run \
            --ready-fd="$wfd" \
            --config "$2" \
            --ch-binary "$BIN/cloud-hypervisor" \
            --run-root "$RUNTIME_ROOT" \
            --sandbox-id "$3" \
            > "$5" 2>&1 &
    else
        readiness_exec_in_new_session "$BIN/sandbox-ctl" run \
            --ready-fd="$wfd" \
            --restore "$snapshot" \
            --config "$2" \
            --ch-binary "$BIN/cloud-hypervisor" \
            --run-root "$RUNTIME_ROOT" \
            --sandbox-id "$3" \
            > "$5" 2>&1 &
    fi
    RUN_PID=$!
    readiness_close_parent_writer
    RUN_READER_PID=$rpid
}

# await_ready <sid> <readyfile> <reader_pid> <pid> <log> — control_ready, ctl
# socket, ready, exact wire, and the pinned /init exec cgroup.
await_ready() {
    readiness_start_watchdog "$4" 120 10
    readiness_wait_event "$2" 1 control_ready "$4" \
        || { tail -60 "$5" >&2 || true; readiness_stop_watchdog; e2e_fail "$1 missed control_ready"; }
    readiness_stop_watchdog
    readiness_connect_ctl "$RUNTIME_ROOT/$1/ctl.sock" \
        || e2e_fail "$1 ctl.sock unavailable at control_ready"
    readiness_start_watchdog "$4" 120 10
    readiness_wait_event "$2" 2 ready "$4" \
        || { tail -60 "$5" >&2 || true; readiness_stop_watchdog; e2e_fail "$1 missed ready"; }
    readiness_stop_watchdog
    readiness_assert_wire "$2" "$3" $'control_ready\nready\n' \
        || e2e_fail "$1 readiness wire is not exact"
    "$BIN/sandbox-ctl" exec --sandbox-id "$1" --run-root "$RUNTIME_ROOT" -- cat /proc/self/cgroup \
        | grep -qE '^0::/init[[:space:]]*$' \
        || e2e_fail "$1 exec is not pinned to /init"
}

mk_restore_yaml() { # <out> <tap> <guest_ip> <diff> [mac]
    cat > "$1" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $2
  interface: eth0
  ip: $3/31
  mac: ${5:-$MAC_A}
  hostname: scr-restore
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff: file://$4
      size: 1GiB
EOF
}

# ===================== Stage 1: cold boot on TAP_A + snapshot ==============
mkdiff "$WORK/blk1-a.diff"
cat > "$WORK/cold-a.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $TAP_A
  interface: eth0
  ip: ${GUESTIPS[0]}/31
  mac: $MAC_A
  hostname: scr-cold
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay:
      diff: file://$WORK/blk1-a.diff
      size: 1GiB
launch:
  args: ["-c", "import sys,time\nprint('PYBOOT-OK', flush=True)\ni=0\nwhile True:\n    print('TICK', i, flush=True)\n    i+=1\n    time.sleep(0.25)"]
  restart: never
  cgroup_control: true
EOF

SID_A="scr-a-$BASHPID"
LOG_A="$OUT/cold-a.log"
launch_run "-" "$WORK/cold-a.yaml" "$SID_A" "$WORK/a.ready" "$LOG_A"
RUN_PIDS+=("$RUN_PID")
await_ready "$SID_A" "$WORK/a.ready" "$RUN_READER_PID" "$RUN_PID" "$LOG_A"
wait_marker '^TICK 10[[:space:]]*$' "$LOG_A" "$RUN_PID" \
    || { tail -40 "$LOG_A" >&2 || true; e2e_fail "cold boot did not reach TICK 10"; }

SNAP_DIR="$WORK/snapshot"
mkdir -p "$SNAP_DIR"
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID_A" --output "$SNAP_DIR" --run-root "$RUNTIME_ROOT" \
    >"$OUT/snapshot.log" 2>&1
wait "$RUN_PID" 2>/dev/null || true
RUN_PIDS=()
SNAP_FILE="$SNAP_DIR/$SID_A.snapshot"
[ -f "$SNAP_FILE" ] || e2e_fail "snapshot is missing"
PRE_SNAP_TICK="$(grep -oE '^TICK [0-9]+' "$LOG_A" | tail -1 | awk '{print $2}')"
WANT_TICK=$((PRE_SNAP_TICK + 5))

# ============ Stage 2: concurrent restore of S onto TAP_B and TAP_C ========
mkdiff "$WORK/blk1-b.diff"
mkdiff "$WORK/blk1-c.diff"
mk_restore_yaml "$WORK/restore-b.yaml" "$TAP_B" "${GUESTIPS[1]}" "$WORK/blk1-b.diff"
mk_restore_yaml "$WORK/restore-c.yaml" "$TAP_C" "${GUESTIPS[2]}" "$WORK/blk1-c.diff"

SID_B="scr-b-$BASHPID"
SID_C="scr-c-$BASHPID"
mkdir -p "$RUNTIME_ROOT/$SID_B" "$RUNTIME_ROOT/$SID_C"
LOG_B="$OUT/restore-b.log"
LOG_C="$OUT/restore-c.log"
launch_run "$SNAP_FILE" "$WORK/restore-b.yaml" "$SID_B" "$WORK/b.ready" "$LOG_B"
PID_B="$RUN_PID"; READER_B="$RUN_READER_PID"
RUN_PIDS+=("$PID_B")
launch_run "$SNAP_FILE" "$WORK/restore-c.yaml" "$SID_C" "$WORK/c.ready" "$LOG_C"
PID_C="$RUN_PID"; READER_C="$RUN_READER_PID"
RUN_PIDS+=("$PID_C")

readiness_start_watchdog "$PID_B" 120 10
readiness_wait_event "$WORK/b.ready" 1 control_ready "$PID_B" \
    || { tail -60 "$LOG_B" >&2 || true; readiness_stop_watchdog; e2e_fail "restore B missed control_ready"; }
readiness_stop_watchdog
readiness_start_watchdog "$PID_C" 120 10
readiness_wait_event "$WORK/c.ready" 1 control_ready "$PID_C" \
    || { tail -60 "$LOG_C" >&2 || true; readiness_stop_watchdog; e2e_fail "restore C missed control_ready"; }
readiness_stop_watchdog
readiness_connect_ctl "$RUNTIME_ROOT/$SID_B/ctl.sock" \
    || e2e_fail "restore B ctl.sock unavailable at control_ready"
readiness_connect_ctl "$RUNTIME_ROOT/$SID_C/ctl.sock" \
    || e2e_fail "restore C ctl.sock unavailable at control_ready"
readiness_start_watchdog "$PID_B" 120 10
readiness_wait_event "$WORK/b.ready" 2 ready "$PID_B" \
    || { tail -60 "$LOG_B" >&2 || true; readiness_stop_watchdog; e2e_fail "restore B missed ready"; }
readiness_stop_watchdog
readiness_start_watchdog "$PID_C" 120 10
readiness_wait_event "$WORK/c.ready" 2 ready "$PID_C" \
    || { tail -60 "$LOG_C" >&2 || true; readiness_stop_watchdog; e2e_fail "restore C missed ready"; }
readiness_stop_watchdog
readiness_assert_wire "$WORK/b.ready" "$READER_B" $'control_ready\nready\n' \
    || e2e_fail "restore B readiness wire is not exact"
readiness_assert_wire "$WORK/c.ready" "$READER_C" $'control_ready\nready\n' \
    || e2e_fail "restore C readiness wire is not exact"

for sid in "$SID_B" "$SID_C"; do
    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$RUNTIME_ROOT" -- cat /proc/self/cgroup \
        | grep -qE '^0::/init[[:space:]]*$' \
        || e2e_fail "$sid exec is not pinned to /init"
done

wait_marker "^TICK $WANT_TICK[[:space:]]*$" "$LOG_B" "$PID_B" \
    || { tail -40 "$LOG_B" >&2 || true; e2e_fail "restore B did not keep counting to TICK $WANT_TICK"; }
wait_marker "^TICK $WANT_TICK[[:space:]]*$" "$LOG_C" "$PID_C" \
    || { tail -40 "$LOG_C" >&2 || true; e2e_fail "restore C did not keep counting to TICK $WANT_TICK"; }

sleep 2 # let the guest finish network re-apply before pinging
ping_guest "$TAP_B" "${GUESTIPS[1]}" \
    || { tail -30 "$LOG_B" >&2 || true; e2e_fail "guest B did not answer ping through $TAP_B"; }
ping_guest "$TAP_C" "${GUESTIPS[2]}" \
    || { tail -30 "$LOG_C" >&2 || true; e2e_fail "guest C did not answer ping through $TAP_C"; }

if grep -q "net_fds=" "$LOG_B"; then
    e2e_fail "name-mode restore B unexpectedly carried net_fds"
fi
if grep -q "net_fds=" "$LOG_C"; then
    e2e_fail "name-mode restore C unexpectedly carried net_fds"
fi

NET_ID_B="$(captured_net_id "$SID_B")"
NET_ID_C="$(captured_net_id "$SID_C")"
[ -n "$NET_ID_B" ] && [ "$NET_ID_B" = "$NET_ID_C" ] \
    || e2e_fail "net device id capture mismatch: B='$NET_ID_B' C='$NET_ID_C'"
assert_net_config "$SID_B" "$TAP_B" absent "$MAC_A" "$NET_ID_B" \
    || e2e_fail "B snap-state/config.json rebinding wrong"
assert_net_config "$SID_C" "$TAP_C" absent "$MAC_A" "$NET_ID_C" \
    || e2e_fail "C snap-state/config.json rebinding wrong"

kill -TERM "$PID_B" "$PID_C" 2>/dev/null || true
wait "$PID_B" "$PID_C" 2>/dev/null || true
RUN_PIDS=()

# ================= Stage 3: cross-mode tap → tapfd (S via handoff) =========
mkdiff "$WORK/blk1-x.diff"
cat > "$WORK/restore-x.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tapfd:
    exec: ["$BIN/connector-ctl", "tapfd", "get", "--new", "--host-cidr", "${CIDRS[4]}",
           "--mac", "$MAC_A", "--ip", "${GUESTIPS[4]}", "$TAPFD_X"]
  ip: ${GUESTIPS[4]}/31
  hostname: scr-x
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff: file://$WORK/blk1-x.diff
      size: 1GiB
EOF

SID_X="scr-x-$BASHPID"
mkdir -p "$RUNTIME_ROOT/$SID_X"
LOG_X="$OUT/restore-x.log"
launch_run "$SNAP_FILE" "$WORK/restore-x.yaml" "$SID_X" "$WORK/x.ready" "$LOG_X"
RUN_PIDS+=("$RUN_PID")
await_ready "$SID_X" "$WORK/x.ready" "$RUN_READER_PID" "$RUN_PID" "$LOG_X"
PID_X="$RUN_PID"
wait_marker "^TICK $WANT_TICK[[:space:]]*$" "$LOG_X" "$PID_X" \
    || { tail -40 "$LOG_X" >&2 || true; e2e_fail "cross-mode tap->tapfd restore did not keep counting"; }

# The captured id is CH's runtime-assigned device id (not always _net0 for
# name-mode snapshots) — cross-check it survived the mode swap and that the
# restore carried net_fds for exactly that id.
NET_ID_X="$(captured_net_id "$SID_X")"
[ "$NET_ID_X" = "$NET_ID_B" ] && [ -n "$NET_ID_X" ] \
    || e2e_fail "captured net id did not survive the tap->tapfd swap: got '$NET_ID_X', want '$NET_ID_B'"
grep -qF "net_fds=[$NET_ID_X@[" "$LOG_X" \
    || e2e_fail "no net_fds rebind for '$NET_ID_X' in cross-mode tap->tapfd restore"
assert_net_config "$SID_X" "-" placeholder "$MAC_A" "$NET_ID_X" \
    || e2e_fail "X snap-state/config.json rebinding wrong"

ping_guest "$TAPFD_X" "${GUESTIPS[4]}" \
    || { tail -30 "$LOG_X" >&2 || true; e2e_fail "guest X did not answer ping through $TAPFD_X"; }
kill -TERM "$PID_X" 2>/dev/null || true
wait "$PID_X" 2>/dev/null || true
RUN_PIDS=()

# ============ Stage 4: cross-mode tapfd → tap (S_fd onto TAP_D) ============
mkdiff "$WORK/blk1-y.diff"
mkdiff "$WORK/blk1-d.diff"
cat > "$WORK/cold-y.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tapfd:
    exec: ["$BIN/connector-ctl", "tapfd", "get", "--new", "--host-cidr", "${CIDRS[5]}",
           "--mac", "$MAC_B", "--ip", "${GUESTIPS[5]}", "$TAPFD_Y"]
  ip: ${GUESTIPS[5]}/31
  hostname: scr-y
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0"
  root:
    base: $ROOT_REF
    overlay:
      diff: file://$WORK/blk1-y.diff
      size: 1GiB
launch:
  args: ["-c", "import time\nprint('NETUP', flush=True)\ni=0\nwhile True:\n    print('TICK', i, flush=True)\n    i+=1\n    time.sleep(0.25)"]
  restart: never
EOF

SID_Y="scr-y-$BASHPID"
LOG_Y="$OUT/cold-y.log"
launch_run "-" "$WORK/cold-y.yaml" "$SID_Y" "$WORK/y.ready" "$LOG_Y"
RUN_PIDS+=("$RUN_PID")
await_ready "$SID_Y" "$WORK/y.ready" "$RUN_READER_PID" "$RUN_PID" "$LOG_Y"
wait_marker '^TICK 10[[:space:]]*$' "$LOG_Y" "$RUN_PID" \
    || { tail -40 "$LOG_Y" >&2 || true; e2e_fail "tapfd cold boot did not reach TICK 10"; }

SNAP_FD="$WORK/snap-fd"
mkdir -p "$SNAP_FD"
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID_Y" --output "$SNAP_FD" --run-root "$RUNTIME_ROOT" \
    >"$OUT/snapshot-y.log" 2>&1
wait "$RUN_PID" 2>/dev/null || true
RUN_PIDS=()
SNAP_FD_FILE="$SNAP_FD/$SID_Y.snapshot"
[ -f "$SNAP_FD_FILE" ] || e2e_fail "S_fd snapshot is missing"
PRE_SNAP_TICK_FD="$(grep -oE '^TICK [0-9]+' "$LOG_Y" | tail -1 | awk '{print $2}')"
WANT_TICK_FD=$((PRE_SNAP_TICK_FD + 5))

mk_restore_yaml "$WORK/restore-d.yaml" "$TAP_D" "${GUESTIPS[3]}" "$WORK/blk1-d.diff" "$MAC_B"
SID_D="scr-d-$BASHPID"
mkdir -p "$RUNTIME_ROOT/$SID_D"
LOG_D="$OUT/restore-d.log"
launch_run "$SNAP_FD_FILE" "$WORK/restore-d.yaml" "$SID_D" "$WORK/d.ready" "$LOG_D"
RUN_PIDS+=("$RUN_PID")
await_ready "$SID_D" "$WORK/d.ready" "$RUN_READER_PID" "$RUN_PID" "$LOG_D"
PID_D="$RUN_PID"
wait_marker "^TICK $WANT_TICK_FD[[:space:]]*$" "$LOG_D" "$PID_D" \
    || { tail -40 "$LOG_D" >&2 || true; e2e_fail "cross-mode tapfd->tap restore did not keep counting"; }

if grep -q "net_fds=" "$LOG_D"; then
    e2e_fail "cross-mode tapfd->tap restore unexpectedly carried net_fds"
fi
assert_net_config "$SID_D" "$TAP_D" absent "$MAC_B" \
    || e2e_fail "D snap-state/config.json rebinding wrong"

ping_guest "$TAP_D" "${GUESTIPS[3]}" \
    || { tail -30 "$LOG_D" >&2 || true; e2e_fail "guest D did not answer ping through $TAP_D"; }
kill -TERM "$PID_D" 2>/dev/null || true
wait "$PID_D" 2>/dev/null || true
RUN_PIDS=()

# ====== Stage 5: same-target collision still fails with a real EBUSY =======
SID_CB="scr-cb-$BASHPID"
SID_CC="scr-cc-$BASHPID"
mkdir -p "$RUNTIME_ROOT/$SID_CB" "$RUNTIME_ROOT/$SID_CC"
LOG_CB="$OUT/restore-cb.log"
LOG_CC="$OUT/restore-cc.log"
launch_run "$SNAP_FILE" "$WORK/restore-b.yaml" "$SID_CB" "$WORK/cb.ready" "$LOG_CB"
PID_CB="$RUN_PID"; READER_CB="$RUN_READER_PID"
RUN_PIDS+=("$PID_CB")
launch_run "$SNAP_FILE" "$WORK/restore-b.yaml" "$SID_CC" "$WORK/cc.ready" "$LOG_CC"
PID_CC="$RUN_PID"; READER_CC="$RUN_READER_PID"
RUN_PIDS+=("$PID_CC")

winner=""
for _ in $(seq 1 240); do
    cb_alive=0; cc_alive=0
    kill -0 "$PID_CB" 2>/dev/null && cb_alive=1
    kill -0 "$PID_CC" 2>/dev/null && cc_alive=1
    if [ $((cb_alive + cc_alive)) -eq 1 ]; then
        if [ "$cb_alive" = 1 ]; then winner=cb; else winner=cc; fi
        break
    fi
    [ "$cb_alive" = 0 ] && [ "$cc_alive" = 0 ] && break
    sleep 0.25
done

[ -n "$winner" ] || {
    tail -40 "$LOG_CB" >&2 || true
    tail -40 "$LOG_CC" >&2 || true
    e2e_fail "could not determine a single collision winner (both alive or both exited)"
}

if [ "$winner" = cb ]; then
    WIN_SID="$SID_CB"; WIN_READY="$WORK/cb.ready"; WIN_PID="$PID_CB"
    WIN_READER="$READER_CB"; WIN_LOG="$LOG_CB"; LOSE_LOG="$LOG_CC"
else
    WIN_SID="$SID_CC"; WIN_READY="$WORK/cc.ready"; WIN_PID="$PID_CC"
    WIN_READER="$READER_CC"; WIN_LOG="$LOG_CC"; LOSE_LOG="$LOG_CB"
fi

readiness_start_watchdog "$WIN_PID" 120 10
readiness_wait_event "$WIN_READY" 1 control_ready "$WIN_PID" \
    || { tail -60 "$WIN_LOG" >&2 || true; readiness_stop_watchdog; e2e_fail "collision winner missed control_ready"; }
readiness_stop_watchdog
readiness_connect_ctl "$RUNTIME_ROOT/$WIN_SID/ctl.sock" \
    || e2e_fail "collision winner ctl.sock unavailable"
readiness_start_watchdog "$WIN_PID" 120 10
readiness_wait_event "$WIN_READY" 2 ready "$WIN_PID" \
    || { tail -60 "$WIN_LOG" >&2 || true; readiness_stop_watchdog; e2e_fail "collision winner missed ready"; }
readiness_stop_watchdog
readiness_assert_wire "$WIN_READY" "$WIN_READER" $'control_ready\nready\n' \
    || e2e_fail "collision winner readiness wire is not exact"
wait_marker "^TICK $WANT_TICK[[:space:]]*$" "$WIN_LOG" "$WIN_PID" \
    || { tail -40 "$WIN_LOG" >&2 || true; e2e_fail "collision winner did not keep counting to TICK $WANT_TICK"; }

grep -qE "Resource busy|ResourceBusy|EBUSY|ConfigureTap" "$LOSE_LOG" \
    || { tail -40 "$LOSE_LOG" >&2 || true; e2e_fail "collision loser log lacks an EBUSY signature"; }

kill -TERM "$PID_CB" "$PID_CC" 2>/dev/null || true
wait "$PID_CB" "$PID_CC" 2>/dev/null || true
RUN_PIDS=()

echo "PASS snapshot.restore-concurrent.sh"
