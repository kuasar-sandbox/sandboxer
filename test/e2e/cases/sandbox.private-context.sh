#!/usr/bin/env bash
# e2e-requires: ["docker","kvm"]
# A private primary owns its mounts; native exec joins it, plugins stay outside.
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
for command in docker mkfs.ext4 pkill python3 sed timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT"
RUNPID=""
READINESS_WATCHDOG_PID=""
cleanup() {
    local status=$?
    set +e
    readiness_stop_watchdog
    if [ -n "$RUNPID" ]; then
        kill -TERM "$RUNPID" 2>/dev/null
        for _ in $(seq 1 50); do
            kill -0 "$RUNPID" 2>/dev/null || break
            sleep 0.1
        done
        readiness_kill_session KILL "$RUNPID"
        wait "$RUNPID" 2>/dev/null
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$WORK/root.img" --no-progress
[ -s "$WORK/root.img" ] || e2e_fail "flatten produced an empty root artifact"
ROOT_REF="$(plaintext_tarstream_ref "$WORK/root.img")"

# Use the prepared image's Python and libc. The real launch primary issues
# mount(2); an exec helper doing the mount would not test the launch boundary.
cat >"$WORK/private-context.py" <<'PY'
import ctypes
import json
import os
from pathlib import Path
import signal
import sys
import time

control = Path("/tmp/private-context")
target = Path("/mnt/private-context")


def wait_for(name):
    deadline = time.monotonic() + 30
    path = control / name
    while not path.exists():
        if time.monotonic() >= deadline:
            raise TimeoutError(f"waiting for {name}")
        time.sleep(0.01)


def publish(name, value):
    temporary = control / (name + ".tmp")
    temporary.write_text(json.dumps(value))
    temporary.replace(control / name)


def context():
    pid = os.getpid()
    proc_pid = int(Path("/proc/self/stat").read_text().split(" ", 1)[0])
    assert proc_pid == pid, ("procfs does not describe this PID namespace", pid, proc_pid)
    namespaces = {kind + "ns": os.readlink(f"/proc/self/ns/{kind}") for kind in ("pid", "mnt")}
    assert namespaces["pidns"] == os.readlink("/proc/1/ns/pid"), "PID 1 belongs to a different namespace"
    return {"pid": pid, **namespaces}


def hold():
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    signal.signal(signal.SIGINT, lambda *_: sys.exit(0))
    while True:
        signal.pause()


role = sys.argv[1]
if role == "init":
    control.mkdir()
    target.mkdir()
    (target / "underlying").write_text("underlying filesystem")
elif role == "plugin":
    before = context()
    assert (target / "underlying").read_text() == "underlying filesystem"
    publish("plugin-ready", before)
    # The completion marker is outside the mount: it cannot be hidden by a
    # broken propagation boundary. This check runs after mount(2) has returned.
    wait_for("primary-mounted")
    assert (target / "underlying").read_text() == "underlying filesystem", "primary mount leaked into plugin"
    assert not (target / "mounted").exists(), "private mount marker visible in plugin"
    assert context() == before, "plugin context changed"
    publish("plugin-checked", before)
    hold()
elif role == "primary":
    own = context()
    assert own["pid"] == 1, ("private primary must be PID 1", own)
    wait_for("plugin-ready")
    plugin = json.loads((control / "plugin-ready").read_text())
    assert own["pid"] != plugin["pid"]
    assert own["mntns"] != plugin["mntns"], "private primary shares plugin mount namespace"
    assert own["pid"] == int(Path("/proc/1/stat").read_text().split(" ", 1)[0])
    assert own["mntns"] == os.readlink("/proc/1/ns/mnt")
    libc = ctypes.CDLL(None, use_errno=True)
    libc.mount.argtypes = (ctypes.c_char_p, ctypes.c_char_p, ctypes.c_char_p, ctypes.c_ulong, ctypes.c_char_p)
    libc.mount.restype = ctypes.c_int
    if libc.mount(b"tmpfs", os.fsencode(target), b"tmpfs", 0, b"size=1m") != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))
    assert not (target / "underlying").exists(), "mount did not cover underlying marker"
    (target / "mounted").write_text("private primary mount")
    publish("primary-mounted", own)
    hold()
elif role == "exec":
    wait_for("primary-mounted")
    wait_for("plugin-checked")
    own = context()
    primary = json.loads((control / "primary-mounted").read_text())
    plugin = json.loads((control / "plugin-checked").read_text())
    assert own["pid"] != 1, "native exec replaced the primary"
    for kind in ("pidns", "mntns"):
        assert own[kind] == primary[kind], ("exec did not join private primary", kind, own, primary)
        assert own[kind] != plugin[kind], ("private primary did not isolate plugin", kind)
    assert (target / "mounted").read_text() == "private primary mount"
    assert not (target / "underlying").exists(), "exec sees the underlying filesystem"
    print("PRIVATE-CONTEXT-OK", flush=True)
else:
    raise ValueError(role)
PY

run_case() {
    local delegation=$1
    local case_dir="$WORK/control-$delegation"
    local run_root="$case_dir/r"
    local sid="private-context-$delegation-$BASHPID"
    local log="$OUT/control-$delegation.log"
    mkdir -p "$case_dir" "$run_root"
    truncate -s 1G "$case_dir/root.diff.template"
    mkfs.ext4 -q -F -O ^has_journal "$case_dir/root.diff.template"
    cat >"$case_dir/sandbox.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay:
      diff: file://$case_dir/root.diff
      diff_template: file://$case_dir/root.diff.template
files:
  - path: /private-context.py
    mode: "0644"
    content: |
EOF
    sed 's/^/      /' "$WORK/private-context.py" >>"$case_dir/sandbox.yaml"
    cat >>"$case_dir/sandbox.yaml" <<EOF
init:
  - exec: /usr/local/bin/python3
    args: [/private-context.py, init]
launch:
  exec: /usr/local/bin/python3
  args: [/private-context.py, primary]
  pid_namespace: private
  cgroup_control: $delegation
  restart: never
  plugin:
    - exec: /usr/local/bin/python3
      args: [/private-context.py, plugin]
      restart: never
EOF

    readiness_begin_capture "$case_dir/ready"
    local reader_pid=$READY_READER_PID
    readiness_exec_in_new_session "$BIN/sandbox-ctl" run \
        --ready-fd="$READY_WRITE_FD" --config "$case_dir/sandbox.yaml" \
        --sandbox-id "$sid" --path-id p --ch-binary "$BIN/cloud-hypervisor" \
        --run-root "$run_root" >"$log" 2>&1 &
    RUNPID=$!
    readiness_close_parent_writer
    readiness_start_watchdog "$RUNPID" 120 10
    readiness_wait_event "$case_dir/ready" 2 ready "$RUNPID" || {
        tail -80 "$log" >&2 || true
        e2e_fail "cgroup_control=$delegation: private primary did not become ready"
    }
    readiness_assert_wire "$case_dir/ready" "$reader_pid" $'control_ready\nready\n' || \
        e2e_fail "cgroup_control=$delegation: unexpected readiness wire"

    timeout -k 5s 45s "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --path-id p --run-root "$run_root" \
        -- /usr/local/bin/python3 /private-context.py exec >"$OUT/control-$delegation.exec" 2>&1 || {
        tail -80 "$log" "$OUT/control-$delegation.exec" >&2 || true
        e2e_fail "cgroup_control=$delegation: private context or mount propagation failed"
    }
    assert_contains "$OUT/control-$delegation.exec" PRIVATE-CONTEXT-OK

    kill -TERM "$RUNPID"
    wait "$RUNPID"
    RUNPID=""
    readiness_stop_watchdog
    echo "PASS private context cgroup_control=$delegation"
}

run_case false
run_case true
echo "PASS sandbox.private-context.sh"
