#!/usr/bin/env bash
#
# e2e_sandbox_restore_concurrent.sh — proves the restore host config's network
# source becomes the actual host backend of the restored virtio-net device
# (sandboxer#161), so one snapshot can fan out to concurrent restores:
#
#   Stage 1  cold boot on TAP_A (name mode), snapshot S (VM destroyed)
#   Stage 2  restore S twice CONCURRENTLY onto distinct taps B and C —
#            both reach ready, rebind their own tap in the per-run
#            snap-state/config.json, keep counting, and answer ping on
#            their own tap (previously the second restore EBUSY'd on TAP_A)
#   Stage 3  cross-mode tap → tapfd: restore S via the tapfd handoff
#            (fresh provider tap + fd re-bound via --restore net_fds)
#   Stage 4  cross-mode tapfd → tap: cold boot via tapfd, snapshot S_fd,
#            then restore S_fd onto named TAP_D (CH opens the tap by name,
#            no net_fds)
#   Stage 5  two restores intentionally selecting the SAME tap still fail
#            with a real resource collision (EBUSY) — exactly one wins
#
# Skips (exit 0) on missing prerequisites; REQUIRE_KVM=1 to fail hard.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
BIN="${BIN:-$REPO_ROOT/bin}"
source "$SCRIPT_DIR/lib/readiness_helpers.sh"

skip() {
    echo
    echo "==> e2e_sandbox_restore_concurrent: skipping ($*)"
    if [ "${REQUIRE_KVM:-0}" = "1" ]; then exit 1; fi
    exit 0
}

[ -e /dev/kvm ] || skip "/dev/kvm not present"
command -v python3 >/dev/null 2>&1 || skip "python3 not on PATH (needed to probe ctl.sock)"
for b in cloud-hypervisor sandbox-ctl sandbox-init sandbox-runtime.bundle flatten-ctl connector-ctl; do
    [ -e "$BIN/$b" ] || skip "missing $BIN/$b"
done
VMLINUX="${VMLINUX:-$BIN/vmlinux}"
[ -f "$VMLINUX" ] || skip "no vmlinux at $VMLINUX"
command -v mkfs.ext4 >/dev/null 2>&1 || skip "mkfs.ext4 not on PATH"
command -v ip >/dev/null 2>&1 || skip "ip not on PATH"
if [ "$(id -u)" -ne 0 ]; then
    exec sudo -nE "$0" "$@"
fi

BLK0_IMAGE="${BLK0_IMAGE:-}"
[ -z "$BLK0_IMAGE" ] && [ -f "$REPO_ROOT/build/python-312.erofs" ] && BLK0_IMAGE="$REPO_ROOT/build/python-312.erofs"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker missing"
    if ! docker image inspect python:3.12-slim >/dev/null 2>&1; then
        docker pull python:3.12-slim >/dev/null
    fi
fi

# ---------------------------------------------------------------------------
# Taps and per-run /31 identities. Four pre-created host taps (name mode):
# A = snapshot source, B/C = concurrent restore targets, D = fd→tap restore.
# X/Y are connector-ctl-managed (non-persistent) taps for the tapfd stages.
TAP_A="${TAP_A:-scr-tap-a}"
TAP_B="${TAP_B:-scr-tap-b}"
TAP_C="${TAP_C:-scr-tap-c}"
TAP_D="${TAP_D:-scr-tap-d}"
TAPFD_X="$(printf 'scrx%x' "$$")"
TAPFD_Y="$(printf 'scry%x' "$$")"
MAC_A="02:00:00:00:80:a1" # snapshot S device MAC (stable across fanout)
MAC_B="02:00:00:00:80:b1" # S_fd device MAC

declare -A USED_NETS=()
while read -r address; do USED_NETS["${address%/*}"]=1; done < <(ip -o -4 addr show | awk '{print $4}')
while read -r destination _; do
    [[ "$destination" == */* ]] && USED_NETS["$destination"]=1
done < <(ip -4 route show table all type unicast)

net_free() { # <cidr> <host_ip> <guest_ip>
    [ -z "${USED_NETS[$1]:-}" ] && [ -z "${USED_NETS[$2]:-}" ] && [ -z "${USED_NETS[$3]:-}" ]
}

# Allocate six disjoint free /31s: A B C D X Y (host ip = even, guest = odd).
CIDRS=(); HOSTIPS=(); GUESTIPS=()
start=$(( ($$ * 3 + 17) % 8192 ))
for ((offset = 0; offset < 8000; offset++)); do
    base=$(( (start + offset) % 8192 ))
    window_ok=1
    for k in 0 1 2 3 4 5; do
        slot=$(( (base + k) % 8192 ))
        block=$(( slot / 128 )); byte=$(( (slot % 128) * 2 ))
        cidrs[k]="169.254.$((64 + block)).$byte"
        guests[k]="169.254.$((64 + block)).$((byte + 1))"
        net_free "${cidrs[k]}/31" "${cidrs[k]}" "${guests[k]}" || window_ok=0
    done
    if [ "$window_ok" = "1" ]; then
        for k in 0 1 2 3 4 5; do
            CIDRS+=("${cidrs[k]}/31"); HOSTIPS+=("${cidrs[k]}"); GUESTIPS+=("${guests[k]}")
        done
        break
    fi
done
[ "${#CIDRS[@]}" -eq 6 ] || { echo "e2e_sandbox_restore_concurrent: no free /31 window" >&2; exit 1; }
# A=0 B=1 C=2 D=3 X=4 Y=5

TAPS=("$TAP_A" "$TAP_B" "$TAP_C" "$TAP_D")
TAP_CREATED=()
for i in 0 1 2 3; do
    if ! ip link show "${TAPS[$i]}" >/dev/null 2>&1; then
        ip tuntap add dev "${TAPS[$i]}" mode tap
        ip addr add "${HOSTIPS[$i]}/31" dev "${TAPS[$i]}"
        ip link set "${TAPS[$i]}" up
        TAP_CREATED+=("${TAPS[$i]}")
    fi
done

WORK="$(mktemp -d /tmp/e2e-restore-concurrent-XXXXXX)"
declare -A PIDS=()
cleanup() {
    set +e
    for pid in "${PIDS[@]}"; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
    done
    sleep 1
    for pid in "${PIDS[@]}"; do
        [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
    done
    [ -n "${E2E_KEEP:-}" ] && echo "kept: $WORK" || rm -rf "$WORK"
    for tap in "${TAP_CREATED[@]:-}"; do
        [ -n "$tap" ] && ip link del "$tap" 2>/dev/null
    done
}
trap cleanup EXIT

if [ -z "$BLK0_IMAGE" ]; then
    BLK0_IMAGE="$WORK/blk0.img"
    docker save python:3.12-slim | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"

mkdiff() { truncate -s 1G "$1"; mkfs.ext4 -q -F "$1"; }
RUNTIME_ROOT="$WORK/runtime"
mkdir -p "$RUNTIME_ROOT"

PASS=0; FAIL=0
ok()  { echo "  PASS: $1"; PASS=$((PASS+1)); }
bad() { echo "  FAIL: $1"; FAIL=$((FAIL+1)); }

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

# launch_restore <cfg> <sid> <readyfile> <log> — background restore with a
# readiness wire; sets RESTORE_PID and RESTORE_READER_PID.
launch_restore() {
    readiness_begin_capture "$3"
    local wfd="$READY_WRITE_FD" rpid="$READY_READER_PID"
    "$BIN/sandbox-ctl" run \
        --ready-fd="$wfd" \
        --restore "$SNAP_FILE" \
        --config "$1" \
        --ch-binary "$BIN/cloud-hypervisor" \
        --run-root "$RUNTIME_ROOT" \
        --sandbox-id "$2" \
        > "$4" 2>&1 &
    RESTORE_PID=$!
    readiness_close_parent_writer
    RESTORE_READER_PID=$rpid
}

# wait_ready_and_probe <sid> <readyfile> <reader_pid> <pid> <log> <want_tick>
wait_ready_and_probe() {
    readiness_wait_event "$2" 1 control_ready "$4" \
        || { tail -60 "$5"; return 1; }
    readiness_connect_ctl "$RUNTIME_ROOT/$1/ctl.sock" \
        || { echo "ctl.sock not connectable at control_ready for $1"; return 1; }
    readiness_wait_event "$2" 2 ready "$4" \
        || { tail -60 "$5"; return 1; }
    readiness_assert_wire "$2" "$3" $'control_ready\nready\n' \
        || { echo "readiness wire not exact for $1"; return 1; }
    "$BIN/sandbox-ctl" exec --sandbox-id "$1" --run-root "$RUNTIME_ROOT" -- cat /proc/self/cgroup \
        | grep -qE '^0::/init[[:space:]]*$' \
        || { echo "exec cgroup check failed for $1"; return 1; }
    wait_marker "^TICK $6[[:space:]]*$" "$5" "$4" \
        || { echo "did not reach TICK $6 for $1"; tail -40 "$5"; return 1; }
    return 0
}

mkdiff "$WORK/blk1-a.diff"

# ===================== Stage 1: cold boot on TAP_A + snapshot ==============
echo "==> Stage 1: cold boot on $TAP_A (${CIDRS[0]}), snapshot S"
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
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay:
      diff: file://$WORK/blk1-a.diff
      size: 1GiB
launch:
  args: ["-c", "import sys,time\nprint('PYBOOT-OK', flush=True)\ni=0\nwhile True:\n    print('TICK', i, flush=True)\n    i+=1\n    time.sleep(0.25)"]
  restart: never
  cgroup_control: true
EOF

SID_A="scr-a-$$"
"$BIN/sandbox-ctl" run \
    --config "$WORK/cold-a.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$RUNTIME_ROOT" \
    --sandbox-id "$SID_A" \
    > "$WORK/cold-a.log" 2>&1 &
PIDS[a]=$!
wait_marker "^TICK 10[[:space:]]*$" "$WORK/cold-a.log" "${PIDS[a]}" \
    || { echo "--- cold-a.log tail ---"; tail -40 "$WORK/cold-a.log"; echo "==> FAIL: cold boot did not reach TICK 10"; exit 1; }
ok "cold boot on $TAP_A reached TICK 10"

OUT="$WORK/snap-out"
mkdir -p "$OUT"
"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID_A" --output "$OUT" --run-root "$RUNTIME_ROOT" \
    > "$WORK/snap.log" 2>&1 \
    || { echo "--- snap.log ---"; cat "$WORK/snap.log"; echo "==> FAIL: snapshot failed"; exit 1; }
wait "${PIDS[a]}" 2>/dev/null || true
PIDS[a]=""
SNAP_FILE="$OUT/$SID_A.snapshot"
[ -f "$SNAP_FILE" ] || { echo "==> FAIL: no $SNAP_FILE"; exit 1; }
PRE_SNAP_TICK=$(grep -oE "^TICK [0-9]+" "$WORK/cold-a.log" | tail -1 | awk '{print $2}')
WANT_TICK=$((PRE_SNAP_TICK + 5))

mk_restore_yaml() { # <out> <tap> <guest_ip> <diff>
    cat > "$1" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $2
  interface: eth0
  ip: $3/31
  mac: $MAC_A
  hostname: scr-restore
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff: file://$4
      size: 1GiB
EOF
}

# ============ Stage 2: concurrent restore of S onto TAP_B and TAP_C ========
echo "==> Stage 2: concurrent restore of S onto $TAP_B (${CIDRS[1]}) and $TAP_C (${CIDRS[2]})"
mkdiff "$WORK/blk1-b.diff"
mkdiff "$WORK/blk1-c.diff"
mk_restore_yaml "$WORK/restore-b.yaml" "$TAP_B" "${GUESTIPS[1]}" "$WORK/blk1-b.diff"
mk_restore_yaml "$WORK/restore-c.yaml" "$TAP_C" "${GUESTIPS[2]}" "$WORK/blk1-c.diff"

mkdir -p "$RUNTIME_ROOT/scr-b-$$" "$RUNTIME_ROOT/scr-c-$$"
launch_restore "$WORK/restore-b.yaml" "scr-b-$$" "$WORK/b.ready" "$WORK/restore-b.log"
PIDS[b]=$RESTORE_PID
READER_B=$RESTORE_READER_PID
launch_restore "$WORK/restore-c.yaml" "scr-c-$$" "$WORK/c.ready" "$WORK/restore-c.log"
PIDS[c]=$RESTORE_PID
READER_C=$RESTORE_READER_PID

if wait_ready_and_probe "scr-b-$$" "$WORK/b.ready" "$READER_B" "${PIDS[b]}" "$WORK/restore-b.log" "$WANT_TICK"; then
    ok "restore B reached ready and kept counting (TICK $WANT_TICK)"
else
    bad "restore B did not reach ready/keep counting"
fi
if wait_ready_and_probe "scr-c-$$" "$WORK/c.ready" "$READER_C" "${PIDS[c]}" "$WORK/restore-c.log" "$WANT_TICK"; then
    ok "restore C reached ready and kept counting (TICK $WANT_TICK)"
else
    bad "restore C did not reach ready/keep counting"
fi

sleep 2 # let the guest finish network re-apply before pinging
ping_guest "$TAP_B" "${GUESTIPS[1]}" \
    && ok "guest B answered ping on its own backend $TAP_B (${GUESTIPS[1]})" \
    || { echo "--- restore-b.log tail ---"; tail -30 "$WORK/restore-b.log"; bad "ping guest B via $TAP_B failed"; }
ping_guest "$TAP_C" "${GUESTIPS[2]}" \
    && ok "guest C answered ping on its own backend $TAP_C (${GUESTIPS[2]})" \
    || { echo "--- restore-c.log tail ---"; tail -30 "$WORK/restore-c.log"; bad "ping guest C via $TAP_C failed"; }

for s in b c; do
    if grep -q "net_fds=" "$WORK/restore-$s.log"; then
        bad "name-mode restore $s unexpectedly carried net_fds"
    else
        ok "name-mode restore $s used the rebound tap (no net_fds)"
    fi
done
NET_ID_B="$(captured_net_id "scr-b-$$")"
NET_ID_C="$(captured_net_id "scr-c-$$")"
if [ -n "$NET_ID_B" ] && [ "$NET_ID_B" = "$NET_ID_C" ]; then
    ok "B/C captured the same net device id '$NET_ID_B' from snapshot S"
else
    bad "net device id capture mismatch: B='$NET_ID_B' C='$NET_ID_C'"
fi
if assert_net_config "scr-b-$$" "$TAP_B" absent "$MAC_A" "$NET_ID_B"; then
    ok "B snap-state/config.json: tap rebound to $TAP_B, fds removed, id/MAC preserved"
else
    bad "B snap-state/config.json rebinding wrong"
fi
if assert_net_config "scr-c-$$" "$TAP_C" absent "$MAC_A" "$NET_ID_C"; then
    ok "C snap-state/config.json: tap rebound to $TAP_C, fds removed, id/MAC preserved"
else
    bad "C snap-state/config.json rebinding wrong"
fi

kill -TERM "${PIDS[b]}" "${PIDS[c]}" 2>/dev/null || true
wait "${PIDS[b]}" "${PIDS[c]}" 2>/dev/null || true
PIDS[b]=""; PIDS[c]=""

# ================= Stage 3: cross-mode tap → tapfd (S via handoff) =========
echo "==> Stage 3: cross-mode restore S tap → tapfd ($TAPFD_X, ${CIDRS[4]})"
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
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff: file://$WORK/blk1-x.diff
      size: 1GiB
EOF

mkdir -p "$RUNTIME_ROOT/scr-x-$$"
"$BIN/sandbox-ctl" run \
    --restore "$SNAP_FILE" \
    --config "$WORK/restore-x.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$RUNTIME_ROOT" \
    --sandbox-id "scr-x-$$" \
    > "$WORK/restore-x.log" 2>&1 &
PIDS[x]=$!
if wait_marker "^TICK $WANT_TICK[[:space:]]*$" "$WORK/restore-x.log" "${PIDS[x]}"; then
    ok "cross-mode tap→tapfd restore kept counting (TICK $WANT_TICK)"
else
    echo "--- restore-x.log tail ---"; tail -40 "$WORK/restore-x.log"
    bad "cross-mode tap→tapfd restore did not keep counting"
fi
# The captured id is CH's runtime-assigned device id (not always _net0 for
# name-mode snapshots) — cross-check it survived the mode swap and that the
# restore carried net_fds for exactly that id.
NET_ID_X="$(captured_net_id "scr-x-$$")"
if [ "$NET_ID_X" = "$NET_ID_B" ] && [ -n "$NET_ID_X" ] && grep -qF "net_fds=[$NET_ID_X@[" "$WORK/restore-x.log"; then
    ok "cross-mode tap→tapfd re-bound fd via net_fds (id $NET_ID_X)"
else
    bad "no net_fds rebind in cross-mode tap→tapfd restore (captured id: '$NET_ID_X', want '$NET_ID_B')"
fi
if assert_net_config "scr-x-$$" "-" placeholder "$MAC_A" "$NET_ID_X"; then
    ok "X snap-state/config.json: tap removed, fds=[-1] placeholder, MAC preserved"
else
    bad "X snap-state/config.json rebinding wrong"
fi
ping_guest "$TAPFD_X" "${GUESTIPS[4]}" \
    && ok "guest X answered ping on provider tap $TAPFD_X (${GUESTIPS[4]})" \
    || { echo "--- restore-x.log tail ---"; tail -30 "$WORK/restore-x.log"; bad "ping guest X via $TAPFD_X failed"; }
kill -TERM "${PIDS[x]}" 2>/dev/null || true
wait "${PIDS[x]}" 2>/dev/null || true
PIDS[x]=""

# ============ Stage 4: cross-mode tapfd → tap (S_fd onto TAP_D) ============
echo "==> Stage 4: cross-mode tapfd → tap (cold via $TAPFD_Y ${CIDRS[5]}, restore onto $TAP_D ${CIDRS[3]})"
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
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0"
  root:
    base: $BLK0_REF
    overlay:
      diff: file://$WORK/blk1-y.diff
      size: 1GiB
launch:
  args: ["-c", "import time\nprint('NETUP', flush=True)\ni=0\nwhile True:\n    print('TICK', i, flush=True)\n    i+=1\n    time.sleep(0.25)"]
  restart: never
EOF

SID_Y="scr-y-$$"
"$BIN/sandbox-ctl" run \
    --config "$WORK/cold-y.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$RUNTIME_ROOT" \
    --sandbox-id "$SID_Y" \
    > "$WORK/cold-y.log" 2>&1 &
PIDS[y]=$!
if wait_marker "^TICK 10[[:space:]]*$" "$WORK/cold-y.log" "${PIDS[y]}"; then
    ok "cold boot via tapfd ($TAPFD_Y) reached TICK 10"
else
    echo "--- cold-y.log tail ---"; tail -40 "$WORK/cold-y.log"
    bad "tapfd cold boot did not reach TICK 10"
fi

SNAP_FD="$WORK/snap-fd"
mkdir -p "$SNAP_FD"
if "$BIN/sandbox-ctl" snapshot --sandbox-id "$SID_Y" --output "$SNAP_FD" --run-root "$RUNTIME_ROOT" \
    > "$WORK/snap-y.log" 2>&1; then
    ok "snapshot S_fd taken"
else
    echo "--- snap-y.log ---"; cat "$WORK/snap-y.log"
    bad "snapshot S_fd failed"
fi
wait "${PIDS[y]}" 2>/dev/null || true
PIDS[y]=""
SNAP_FD_FILE="$SNAP_FD/$SID_Y.snapshot"
PRE_SNAP_TICK_FD=$(grep -oE "^TICK [0-9]+" "$WORK/cold-y.log" | tail -1 | awk '{print $2}')
WANT_TICK_FD=$((PRE_SNAP_TICK_FD + 5))

cat > "$WORK/restore-d.yaml" <<EOF
resources:
  capacity:    { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
network:
  tap: $TAP_D
  interface: eth0
  ip: ${GUESTIPS[3]}/31
  mac: $MAC_B
  hostname: scr-d
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    overlay:
      diff: file://$WORK/blk1-d.diff
      size: 1GiB
EOF

if [ -f "$SNAP_FD_FILE" ]; then
    mkdir -p "$RUNTIME_ROOT/scr-d-$$"
    "$BIN/sandbox-ctl" run \
        --restore "$SNAP_FD_FILE" \
        --config "$WORK/restore-d.yaml" \
        --ch-binary "$BIN/cloud-hypervisor" \
        --run-root "$RUNTIME_ROOT" \
        --sandbox-id "scr-d-$$" \
        > "$WORK/restore-d.log" 2>&1 &
    PIDS[d]=$!
    if wait_marker "^TICK $WANT_TICK_FD[[:space:]]*$" "$WORK/restore-d.log" "${PIDS[d]}"; then
        ok "cross-mode tapfd→tap restore kept counting (TICK $WANT_TICK_FD)"
    else
        echo "--- restore-d.log tail ---"; tail -40 "$WORK/restore-d.log"
        bad "cross-mode tapfd→tap restore did not keep counting"
    fi
    if grep -q "net_fds=" "$WORK/restore-d.log"; then
        bad "cross-mode tapfd→tap restore unexpectedly carried net_fds"
    else
        ok "cross-mode tapfd→tap used the named tap (no net_fds)"
    fi
    if assert_net_config "scr-d-$$" "$TAP_D" absent "$MAC_B"; then
        ok "D snap-state/config.json: fds removed, tap rebound to $TAP_D, MAC preserved"
    else
        bad "D snap-state/config.json rebinding wrong"
    fi
    ping_guest "$TAP_D" "${GUESTIPS[3]}" \
        && ok "guest D answered ping on $TAP_D (${GUESTIPS[3]})" \
        || { echo "--- restore-d.log tail ---"; tail -30 "$WORK/restore-d.log"; bad "ping guest D via $TAP_D failed"; }
    kill -TERM "${PIDS[d]}" 2>/dev/null || true
    wait "${PIDS[d]}" 2>/dev/null || true
    PIDS[d]=""
else
    bad "no S_fd snapshot at $SNAP_FD_FILE; skipping tapfd→tap restore"
fi

# ====== Stage 5: same-target collision still fails with a real EBUSY =======
echo "==> Stage 5: two concurrent restores onto the SAME $TAP_B — exactly one wins"
mkdir -p "$RUNTIME_ROOT/scr-cb-$$" "$RUNTIME_ROOT/scr-cc-$$"
launch_restore "$WORK/restore-b.yaml" "scr-cb-$$" "$WORK/cb.ready" "$WORK/restore-cb.log"
PIDS[cb]=$RESTORE_PID
READER_CB=$RESTORE_READER_PID
launch_restore "$WORK/restore-b.yaml" "scr-cc-$$" "$WORK/cc.ready" "$WORK/restore-cc.log"
PIDS[cc]=$RESTORE_PID
READER_CC=$RESTORE_READER_PID

winner=""
loser=""
for _ in $(seq 1 240); do
    cb_alive=0; cc_alive=0
    kill -0 "${PIDS[cb]}" 2>/dev/null && cb_alive=1
    kill -0 "${PIDS[cc]}" 2>/dev/null && cc_alive=1
    if [ $((cb_alive + cc_alive)) -eq 1 ]; then
        if [ "$cb_alive" = "1" ]; then winner=cb; loser=cc; else winner=cc; loser=cb; fi
        break
    fi
    if [ "$cb_alive" = "0" ] && [ "$cc_alive" = "0" ]; then break; fi
    sleep 0.25
done

if [ -n "$winner" ]; then
    ok "exactly one restore claimed $TAP_B (loser exited during restore)"
    if wait_ready_and_probe "scr-$winner-$$" "$WORK/$winner.ready" "$( [ "$winner" = cb ] && echo "$READER_CB" || echo "$READER_CC" )" \
        "${PIDS[$winner]}" "$WORK/restore-$winner.log" "$WANT_TICK"; then
        ok "collision winner reached ready and kept counting"
    else
        bad "collision winner did not reach ready"
    fi
    if grep -qE "Resource busy|ResourceBusy|EBUSY|ConfigureTap" "$WORK/restore-$loser.log"; then
        ok "collision loser log shows the tap resource collision (EBUSY)"
    else
        echo "--- restore-$loser.log ---"; tail -40 "$WORK/restore-$loser.log"
        bad "collision loser log lacks an EBUSY signature"
    fi
else
    bad "could not determine a single collision winner (both alive or both exited)"
    echo "--- restore-cb.log tail ---"; tail -40 "$WORK/restore-cb.log" 2>/dev/null || true
    echo "--- restore-cc.log tail ---"; tail -40 "$WORK/restore-cc.log" 2>/dev/null || true
fi

kill -TERM "${PIDS[cb]}" "${PIDS[cc]}" 2>/dev/null || true
wait "${PIDS[cb]}" "${PIDS[cc]}" 2>/dev/null || true
PIDS[cb]=""; PIDS[cc]=""

echo
echo "========================================="
echo "  e2e_sandbox_restore_concurrent: $PASS passed, $FAIL failed"
echo "========================================="
if [ "$FAIL" -eq 0 ]; then
    echo "==> e2e_sandbox_restore_concurrent: OK"
else
    exit 1
fi
