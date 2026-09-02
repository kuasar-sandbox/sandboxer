#!/usr/bin/env bash
#
# e2e_sandbox_snapshot.sh — start a long-running sandbox and verify that one
# freeze point produces runnable Sandbox E plus memory Snapshot S.
#
# Approach:
#   1. Run python progress counter (writes /tmp/progress every 100ms)
#      under sandbox-ctl run (background)
#   2. Wait until guest is past phase 2 (sees a marker line in log)
#   3. Run sandbox-ctl snapshot --sandbox-id <sid> --output <out>
#   4. Verify the <sid>.snapshot commit alias and its content-addressed E
#   5. Verify S references E and each strict config is inspectable
#   6. Tear down sandbox

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
BIN="${BIN:-$REPO_ROOT/bin}"
VCPU_COUNT="${VCPU_COUNT:-1}"
VCPU_LIFECYCLE_CYCLES="${VCPU_LIFECYCLE_CYCLES:-100}"
SNAPSHOT_CYCLES="${SNAPSHOT_CYCLES:-1}"
VMM_CGROUP_PATH="${VMM_CGROUP_PATH:-}"
VMM_CPU_MAX=""
VMM_CONTROL_YAML=""

case "$VCPU_COUNT" in
    ''|*[!0-9]*|0) echo "$0: VCPU_COUNT must be a positive integer" >&2; exit 1 ;;
esac
case "$VCPU_LIFECYCLE_CYCLES" in
    ''|*[!0-9]*) echo "$0: VCPU_LIFECYCLE_CYCLES must be a non-negative integer" >&2; exit 1 ;;
esac
case "$SNAPSHOT_CYCLES" in
    ''|*[!0-9]*|0) echo "$0: SNAPSHOT_CYCLES must be a positive integer" >&2; exit 1 ;;
esac
if [ -n "$VMM_CGROUP_PATH" ]; then
    case "$VMM_CGROUP_PATH" in
        /*) ;;
        *) echo "$0: VMM_CGROUP_PATH must be absolute" >&2; exit 1 ;;
    esac
    [ -d "$VMM_CGROUP_PATH" ] || { echo "$0: missing VMM_CGROUP_PATH: $VMM_CGROUP_PATH" >&2; exit 1; }
    [ -r "$VMM_CGROUP_PATH/cpu.max" ] || { echo "$0: missing $VMM_CGROUP_PATH/cpu.max" >&2; exit 1; }
    VMM_CONTROL_YAML="  control: { cgroup_path: \"$VMM_CGROUP_PATH\" }"
fi

skip() {
    echo
    echo "==> e2e_sandbox_snapshot: skipping ($*)"
    if [ "${REQUIRE_KVM:-0}" = "1" ]; then exit 1; fi
    exit 0
}

assert_vmm_cpu_max_unchanged() {
    [ -z "$VMM_CGROUP_PATH" ] || [ -z "$VMM_CPU_MAX" ] \
        || [ "$(cat "$VMM_CGROUP_PATH/cpu.max")" = "$VMM_CPU_MAX" ] \
        || { echo "==> VMM cpu.max changed from '$VMM_CPU_MAX'" >&2; return 1; }
}

[ -e /dev/kvm ] || skip "/dev/kvm not present"
for b in cloud-hypervisor sandbox-ctl sandbox-init sandbox-runtime.bundle flatten-ctl; do
    [ -e "$BIN/$b" ] || skip "missing $BIN/$b — run 'make build'"
done
VMLINUX="${VMLINUX:-$BIN/vmlinux}"
[ -f "$VMLINUX" ] || skip "no vmlinux at $VMLINUX"
# Self-elevate: tap creation, cgroup writes, vsock all need root. Done
# here (after prereq checks) so /dev/kvm-missing and missing-binary cases
# still fast-fail without prompting for sudo.
if [ "$(id -u)" -ne 0 ]; then
    exec sudo -nE "$0" "$@"
fi

TAP_NAME="${TAP_NAME:-sb-tap0}"
TAP_CREATED_BY_TEST=0
if ! ip link show "$TAP_NAME" >/dev/null 2>&1; then
    [ "$(id -u)" -eq 0 ] || { echo "$0: must run as root to create $TAP_NAME" >&2; exit 1; }
    ip tuntap add dev "$TAP_NAME" mode tap
    ip addr add 169.254.1.0/31 dev "$TAP_NAME"
    ip link set "$TAP_NAME" up
    TAP_CREATED_BY_TEST=1
fi

# Need root to make /run/<sid>/ctl.sock + uffd usable.
if [ "$(id -u)" -ne 0 ]; then
    skip "must run as root (cgroup + uffd)"
fi

WORK="$(mktemp -d /tmp/e2e-snapshot-XXXXXX)"
trap '[ -n "${E2E_KEEP:-}" ] && echo "kept: $WORK" || rm -rf "$WORK"; [ "$TAP_CREATED_BY_TEST" = "1" ] && ip link del "$TAP_NAME" 2>/dev/null; true' EXIT

IMAGE="${IMAGE:-python:3.12-slim}"
BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker missing; provide BLK0_IMAGE"
    if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
        echo "==> docker pull $IMAGE"
        docker pull "$IMAGE" >/dev/null
    fi
    BLK0_IMAGE="$WORK/blk0.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"

mkdir -p "$WORK/runtime"
DIFF_FILE="$WORK/runtime/blk1.diff"
truncate -s 1G "$DIFF_FILE"
mkfs.ext4 -q -F "$DIFF_FILE"

# Long-running app: print a counter every 250ms; first marker
# "PYBOOT-OK" within ~1s confirms app is up.
cat > "$WORK/sandbox.yaml" <<EOF
resources:
  capacity:    { cpu: $VCPU_COUNT, memory: 512MiB }
  allocatable: { cpu: $VCPU_COUNT, memory: 512MiB }
$VMM_CONTROL_YAML
network:
  tap: $TAP_NAME
  interface: eth0
  ip: 169.254.1.1/31
  hostname: e2e-snap
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay:
      diff: file://$DIFF_FILE
      size: 1GiB
launch:
  args: ["-c", "import sys,time\nprint('PYBOOT-OK', sys.version_info.major*100+sys.version_info.minor, flush=True)\ni=0\nwhile True:\n    print('TICK', i, flush=True)\n    i+=1\n    time.sleep(0.25)"]
  restart: never
EOF

LOG="$WORK/run.log"
SID="snap-$$"
RUNTIME_ROOT="$WORK/runtime"
mkdir -p "$RUNTIME_ROOT/$SID"
"$BIN/sandbox-ctl" run \
    --config "$WORK/sandbox.yaml" \
    --ch-binary "$BIN/cloud-hypervisor" \
    --run-root "$RUNTIME_ROOT" \
    --sandbox-id "$SID" \
    > "$LOG" 2>&1 &
SBPID=$!

# Wait for first PYBOOT-OK + a few TICKs (~3s).
echo "==> waiting for app to start..."
for i in $(seq 1 600); do
    if grep -q "TICK 5" "$LOG" 2>/dev/null; then break; fi
    if ! kill -0 "$SBPID" 2>/dev/null; then
        echo "==> sandbox-ctl exited early"; tail -30 "$LOG"; exit 1
    fi
    sleep 0.05
done
if ! grep -q "TICK 5" "$LOG"; then
    echo "==> timeout waiting for guest app"; tail -40 "$LOG"; kill -TERM "$SBPID" 2>/dev/null; exit 1
fi
echo "==> guest app running"
if [ -n "$VMM_CGROUP_PATH" ]; then
    VMM_CPU_MAX="$(cat "$VMM_CGROUP_PATH/cpu.max")"
    case "$VMM_CPU_MAX" in
        max|"max "*) echo "==> VMM cpu.max is not numeric: $VMM_CPU_MAX" >&2; exit 1 ;;
    esac
    echo "==> VMM cpu.max baseline: $VMM_CPU_MAX"
fi

# Exercise the same CH pause/resume barrier used by snapshot without paying the
# memory-dump cost on every iteration. A persistent Unix HTTP connection keeps
# the 10k-cycle issue #112 stress practical; the real snapshot below still
# validates the complete sandbox-ctl path and ordered shutdown follows cleanup.
if [ "$VCPU_LIFECYCLE_CYCLES" -gt 0 ]; then
    CH_SOCKET="$RUNTIME_ROOT/$SID/ch.sock"
    [ -S "$CH_SOCKET" ] || { echo "==> missing Cloud Hypervisor API socket: $CH_SOCKET"; exit 1; }
    echo "==> exercising $VCPU_LIFECYCLE_CYCLES pause/resume cycles ($VCPU_COUNT vCPU)"
    python3 - "$CH_SOCKET" "$VCPU_LIFECYCLE_CYCLES" <<'PY'
import http.client
import socket
import sys

socket_path, cycles_text = sys.argv[1:]
cycles = int(cycles_text)


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=2)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


connection = UnixHTTPConnection(socket_path)
for iteration in range(1, cycles + 1):
    for endpoint in ("/api/v1/vm.pause", "/api/v1/vm.resume"):
        connection.request("PUT", endpoint, body=b"")
        response = connection.getresponse()
        body = response.read()
        if response.status not in (200, 204):
            raise SystemExit(
                f"iteration {iteration} {endpoint}: HTTP {response.status}: "
                f"{body.decode(errors='replace')}"
            )
    if iteration % 1000 == 0:
        print(f"    completed={iteration}", flush=True)
connection.close()
PY
fi
assert_vmm_cpu_max_unchanged

echo "==> taking snapshot"

for snapshot_cycle in $(seq 1 "$SNAPSHOT_CYCLES"); do
    OUT="$WORK/snap-out-$snapshot_cycle"
    mkdir -p "$OUT"
    if [ "$SNAPSHOT_CYCLES" -gt 1 ]; then
        echo "==> snapshot cycle $snapshot_cycle/$SNAPSHOT_CYCLES"
    fi
    "$BIN/sandbox-ctl" snapshot \
        --sandbox-id "$SID" \
        --output "$OUT" \
        --run-root "$RUNTIME_ROOT" \
        --resume 2>&1 | tee "$WORK/snap-$snapshot_cycle.log"
    assert_vmm_cpu_max_unchanged
    if [ "$snapshot_cycle" -lt "$SNAPSHOT_CYCLES" ]; then
        rm -rf "$OUT"
    fi
done

# Tear down sandbox
kill -TERM "$SBPID" 2>/dev/null || true
wait "$SBPID" 2>/dev/null || true
assert_vmm_cpu_max_unchanged
if [ -n "$VMM_CGROUP_PATH" ]; then
    grep -qx 'populated 0' "$VMM_CGROUP_PATH/cgroup.events" \
        || { echo "==> VMM cgroup remains populated after shutdown" >&2; cat "$VMM_CGROUP_PATH/cgroup.events" >&2; exit 1; }
fi

# Validate outputs
echo "==> validating snapshot bundle"
SNAP_FILE="$OUT/$SID.snapshot"
[ -f "$SNAP_FILE" ] || { echo "FAIL: no $SID.snapshot"; ls -la "$OUT"; exit 1; }
INFO_JSON=$("$BIN/sandbox-ctl" info --json "$SNAP_FILE")
SANDBOX_REF=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("SandboxRef", ""))' <<<"$INFO_JSON")
[ -n "$SANDBOX_REF" ] || { echo "==> FAIL: Snapshot S has no sandbox_ref"; echo "$INFO_JSON"; exit 1; }
SANDBOX_BASENAME=$(python3 -c 'import os,sys; print(os.path.basename(sys.argv[1].split("@",1)[0]))' "$SANDBOX_REF")
SANDBOX_FILE="$OUT/$SANDBOX_BASENAME"
[ -f "$SANDBOX_FILE" ] || { echo "FAIL: Snapshot S references missing Sandbox E $SANDBOX_BASENAME"; ls -la "$OUT"; exit 1; }
mapfile -t OVERLAY_FILES < <(find "$OUT" -maxdepth 1 -type f -name '*.overlay' -print | sort)
mapfile -t IMAGE_FILES < <(find "$OUT" -maxdepth 1 -type f -name '*.image' -print | sort)
[ "${#OVERLAY_FILES[@]}" -eq 0 ] || { echo "FAIL: root image or Sandbox E payload was duplicated as .overlay"; ls -la "$OUT"; exit 1; }
[ "${#IMAGE_FILES[@]}" -eq 1 ] || { echo "FAIL: expected one immutable root image, got ${#IMAGE_FILES[@]}"; ls -la "$OUT"; exit 1; }

# Sizes. Artifacts are tarstream envelopes: the FILE is dense (size ≈
# resident data + envelope), holes ride the envelope map. Sparseness
# shows as file size ≪ the logical entry size (ramSize = 512 MiB).
# <sid>.snapshot is a symlink; Sandbox E is content-addressed.
SNAP_BYTES=$(stat -L -c%s "$SNAP_FILE")
SANDBOX_BYTES=$(stat -L -c%s "$SANDBOX_FILE")
echo "    $SID.snapshot:   artifact=$(numfmt --to=iec $SNAP_BYTES)"
echo "    $SID.sandbox:    artifact=$(numfmt --to=iec $SANDBOX_BYTES)"
RAM_BYTES=$((512 * 1024 * 1024))
if [ "$SNAP_BYTES" -ge "$RAM_BYTES" ]; then
    echo "==> FAIL: snapshot artifact ($SNAP_BYTES) not smaller than ramSize ($RAM_BYTES) — holes not carried by the envelope?"
    exit 1
fi
if [ "$SNAP_BYTES" -lt $((1 << 20)) ]; then
    echo "==> FAIL: snapshot has < 1 MiB content (snapshot probably empty)"; exit 1
fi
echo "==> PASS: snapshot artifact carries only resident data ($(numfmt --to=iec $SNAP_BYTES) of $(numfmt --to=iec $RAM_BYTES) logical)"

# The artifact is a tar envelope; info reads snapshot.cfg through it
# (the same path restore uses).
E_INFO_JSON=$("$BIN/sandbox-ctl" info --json "$SANDBOX_FILE")
python3 -c 'import json,sys; c=json.load(sys.stdin); root=c["Boot"]["Root"]; assert c["Version"] == 1 and root["Base"].split("@",1)[0].endswith(".image") and root["Overlay"]["Base"] == "self"' <<<"$E_INFO_JSON" \
    || { echo "==> FAIL: Sandbox E info is incomplete"; echo "$E_INFO_JSON"; exit 1; }
echo "==> PASS: Snapshot S references Sandbox E; root image is .image and the current writable root is the E payload"

# Content addressing: the basename matches the digest declared by the
# artifact's final marker. The marker records the carrier identity and payload
# commitment metadata; it is not part of the logical payload.
ARTIFACT_FILES=("$(readlink -f "$SANDBOX_FILE")" "$(readlink -f "$SNAP_FILE")" "${IMAGE_FILES[@]}" "${OVERLAY_FILES[@]}")
for f in "${ARTIFACT_FILES[@]}"; do
    base=$(basename "$f"); base=${base%.*}
    markers=$(tar -tf "$f" | grep -E '^\.kuasar\.digest\.[0-9a-f]{64}$' || true)
    marker_count=$(grep -c . <<<"$markers" || true)
    [ "$marker_count" = 1 ] || { echo "==> FAIL: $(basename "$f") has $marker_count digest markers"; exit 1; }
    digest=${markers#.kuasar.digest.}
    if [ "$base" != "$digest" ]; then
        echo "==> FAIL: $(basename "$f") basename != digest marker ($digest)"
        exit 1
    fi
done
echo "==> PASS: artifact basenames match their digest markers"

# Exercise the public named-location CLI with the live S/E graph produced
# above. The location contains content-addressed finals only: no semantic
# aliases or symlinks. A second publication must validate and reuse the same
# carrier-provided identities.
LOCATION_NAME="snapshot-e2e"
LOCATION_DIR="$WORK/named-location"
mkdir -p "$LOCATION_DIR"
LOCATED_REF=$("$BIN/sandbox-ctl" upload-snapshot --quiet \
    --to-ref-location "$LOCATION_NAME=file://$LOCATION_DIR" "$SNAP_FILE")
LOCATED_AGAIN=$("$BIN/sandbox-ctl" upload-snapshot --quiet \
    --to-ref-location "$LOCATION_NAME=file://$LOCATION_DIR" "$SNAP_FILE")
[ "$LOCATED_AGAIN" = "$LOCATED_REF" ] || {
    echo "==> FAIL: repeated named publication changed root ref" >&2
    printf 'first:  %s\nsecond: %s\n' "$LOCATED_REF" "$LOCATED_AGAIN" >&2
    exit 1
}
LOCATED_BASENAME=${LOCATED_REF#file://}
LOCATED_BASENAME=${LOCATED_BASENAME%%@*}
LOCATED_DIGEST=${LOCATED_BASENAME%.snapshot}
[ "$LOCATED_REF" = "file://$LOCATED_BASENAME@digest:$LOCATED_DIGEST@location:$LOCATION_NAME" ] || {
    echo "==> FAIL: non-canonical located snapshot ref: $LOCATED_REF" >&2
    exit 1
}
[ -f "$LOCATION_DIR/$LOCATED_BASENAME" ] || {
    echo "==> FAIL: located snapshot final is missing" >&2
    exit 1
}
if find "$LOCATION_DIR" -maxdepth 1 -type l -print -quit | grep -q .; then
    echo "==> FAIL: named location contains a symlink" >&2
    find "$LOCATION_DIR" -maxdepth 1 -type l -print >&2
    exit 1
fi
[ ! -e "$LOCATION_DIR/$SID.snapshot" ] && [ ! -e "$LOCATION_DIR/$SID.sandbox" ] || {
    echo "==> FAIL: named location contains a semantic alias" >&2
    exit 1
}
[ "$(find "$LOCATION_DIR" -maxdepth 1 -type f -name '*.image' | wc -l)" -eq 1 ] || {
    echo "==> FAIL: named location did not contain exactly one root .image" >&2
    find "$LOCATION_DIR" -maxdepth 1 -type f -print >&2
    exit 1
}
[ "$(find "$LOCATION_DIR" -maxdepth 1 -type f -name '*.overlay' | wc -l)" -eq 0 ] || {
    echo "==> FAIL: named location duplicated the root image or Sandbox E payload as .overlay" >&2
    find "$LOCATION_DIR" -maxdepth 1 -type f -print >&2
    exit 1
}
LOCATED_INFO=$("$BIN/sandbox-ctl" info --json \
    --ref-location "$LOCATION_NAME=file://$LOCATION_DIR" "$LOCATED_REF")
python3 -c 'import json,sys; snapshot=json.load(sys.stdin); assert snapshot["SandboxRef"].endswith("@location:" + sys.argv[1]), snapshot' \
    "$LOCATION_NAME" <<<"$LOCATED_INFO"
echo "==> PASS: named location publication is canonical, reusable, alias-free, role-correct, and readable"

# Both operation roots are valid tarstream carriers. Their logical payload
# boundaries and sparse semantics are covered by sandboxfile/snapshotfile tests;
# a live upper may legitimately have a Hole at the ext4 superblock position.
if command -v file >/dev/null && file -L "$SNAP_FILE" 2>&1 | grep -qiE "tar archive"; then
    echo "==> PASS: snapshot artifact is a tar envelope"
else
    echo "==> WARN: file(1) did not recognize the artifact as tar"
fi
if command -v file >/dev/null && file -L "$SANDBOX_FILE" 2>&1 | grep -qiE "tar archive"; then
    echo "==> PASS: Sandbox E artifact is a tar envelope"
else
    echo "==> WARN: file(1) did not recognize Sandbox E as tar"
fi

echo "==> e2e_sandbox_snapshot: OK"
