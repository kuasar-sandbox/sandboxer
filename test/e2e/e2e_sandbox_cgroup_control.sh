#!/usr/bin/env bash
#
# e2e_sandbox_cgroup_control.sh — exercise both launch.cgroup_control
# topologies with shared and private PID namespaces. Each managed process runs
# an immediate-fork stress probe so a Start→cgroup.procs placement window would
# leave observable children outside the expected scoped path.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
. "$SCRIPT_DIR/lib/tarstream.sh"
BIN="${BIN:-$REPO_ROOT/bin}"
IMAGE="${IMAGE:-python:3.12-slim}"

skip() {
    echo
    echo "==> e2e_sandbox_cgroup_control: skipping ($*)"
    if [ "${REQUIRE_KVM:-0}" = "1" ]; then
        exit 1
    fi
    exit 0
}

[ -e /dev/kvm ] || skip "/dev/kvm not present"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || skip "/dev/kvm not accessible"
for b in cloud-hypervisor sandbox-ctl sandbox-init sandbox-runtime.bundle flatten-ctl; do
    [ -e "$BIN/$b" ] || skip "missing $BIN/$b — assemble the platform binaries"
done
VMLINUX="${VMLINUX:-$BIN/vmlinux}"
[ -f "$VMLINUX" ] || skip "no vmlinux at $VMLINUX"
command -v mkfs.ext4 >/dev/null 2>&1 || skip "mkfs.ext4 not on PATH"
command -v cc >/dev/null 2>&1 || skip "C compiler not on PATH"
command -v base64 >/dev/null 2>&1 || skip "base64 not on PATH"

if [ "$(id -u)" -ne 0 ]; then
    exec sudo -nE "$0" "$@"
fi

WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/e2e-cgroup-control-XXXXXX")"
PIDS=()
cleanup() {
    set +e
    for pid in "${PIDS[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            kill -TERM "$pid" 2>/dev/null
            for _ in $(seq 1 50); do
                kill -0 "$pid" 2>/dev/null || break
                sleep 0.1
            done
            kill -KILL "$pid" 2>/dev/null || true
        fi
        wait "$pid" 2>/dev/null || true
    done
    if [ -n "${E2E_KEEP:-}" ]; then
        echo "kept work dir: $WORK"
    else
        rm -rf "$WORK"
    fi
}
trap cleanup EXIT

BLK0_IMAGE="${BLK0_IMAGE:-}"
if [ -z "$BLK0_IMAGE" ]; then
    command -v docker >/dev/null 2>&1 || skip "docker not available; provide BLK0_IMAGE"
    if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
        docker pull "$IMAGE" >/dev/null
    fi
    BLK0_IMAGE="$WORK/blk0.img"
    docker save "$IMAGE" | "$BIN/flatten-ctl" export --output "$BLK0_IMAGE" --no-progress
fi
BLK0_REF="$(plaintext_tarstream_ref "$BLK0_IMAGE")"

# The libc-free helper forks at its ELF entry point, then execs probe.sh. This
# makes the escape-window check exercise the first possible application code,
# not a shell or language runtime that starts after the old placement race.
cc -nostdlib -static -no-pie -fno-pie -fno-stack-protector -fno-builtin \
    -Wall -Wextra -Werror -Os -s -o "$WORK/cgroup-fork-probe" \
    "$SCRIPT_DIR/lib/cgroup_fork_probe.c"
PROBE_B64="$(base64 < "$WORK/cgroup-fork-probe" | tr -d '\n')"

wait_marker() {
    local marker=$1 log=$2 pid=$3
    for _ in $(seq 1 1200); do
        grep -qF "$marker" "$log" 2>/dev/null && return 0
        if ! kill -0 "$pid" 2>/dev/null; then
            echo "sandbox exited before marker: $marker" >&2
            tail -100 "$log" >&2
            return 1
        fi
        sleep 0.05
    done
    echo "timed out waiting for marker: $marker" >&2
    tail -100 "$log" >&2
    return 1
}

run_case() {
    local control=$1 pid_mode=$2
    local expected=/
    [ "$control" = true ] && expected=/init
    local real_check=0
    [ "$pid_mode" = shared ] && real_check=1
    # Plugins always share sandbox-init's PID namespace, even when the primary
    # is private, so they can inspect the guest-global mount through /proc/1.
    local plugin_real_check=1
    local name="${control}-${pid_mode}"
    local case_dir="$WORK/$name"
    local run_root="$case_dir/runtime"
    local sid="cg-${name}-$$"
    local log="$case_dir/run.log"
    mkdir -p "$run_root"

    local diff="$case_dir/root.diff"
    truncate -s 1G "$diff"
    mkfs.ext4 -q -F "$diff"

    # probe.sh starts the stress children before doing any long-lived work.
    # With the old post-Start placement, one or more children could remain in
    # the guest-global root after their parent was migrated.
    cat > "$case_dir/sandbox.yaml" <<EOF
resources:
  capacity: { cpu: 1, memory: 512MiB }
  allocatable: { cpu: 1, memory: 512MiB }
boot:
  kernel: file://$VMLINUX
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $BLK0_REF
    overlay:
      diff: file://$diff
files:
  - path: /cgroup-fork-probe.b64
    mode: "0644"
    content: "$PROBE_B64"
  - path: /probe.sh
    mode: "0755"
    content: |
      #!/bin/sh
      set -eu
      role="\$1"
      expected="\$2"
      pid_mode="\$3"
      control="\$4"
      real_check="\$5"
      generation_file="/tmp/cg-\${role}.generation"
      generation=0
      [ ! -f "\$generation_file" ] || generation="\$(cat "\$generation_file")"
      generation=\$((generation + 1))
      echo "\$generation" > "\$generation_file"

      self_path="\$(awk -F: '\$1 == "0" { print \$3 }' /proc/self/cgroup)"
      [ "\$self_path" = "\$expected" ] || {
        echo "CG-FAIL \$role generation=\$generation self=\$self_path expected=\$expected"
        exit 70
      }
      [ ! -e /sys/fs/cgroup/app ] || {
        echo "CG-FAIL \$role inherited-guest-global-cgroupfs"
        exit 68
      }
      for fd_path in /proc/self/fd/*; do
        fd_target="\$(readlink "\$fd_path" 2>/dev/null || true)"
        case "\$fd_target" in
          cgroup:\[*|mnt:\[*|/sys/fs/cgroup/app*)
            echo "CG-FAIL \$role leaked-fd=\$fd_path target=\$fd_target"
            exit 69
            ;;
        esac
      done
      echo "CG-\$role-GEN-\$generation-SELF \$self_path"

      # cgroup-fork-probe ran this check at ELF entry, before this shell. The
      # file is cumulative across restarts, so generation N must contribute
      # exactly N*128 children and every line must show the final cgroup.
      stress="/tmp/cg-\${role}.fast-paths"
      count="\$(wc -l < "\$stress" | tr -d ' ')"
      expected_count=\$((generation * 128))
      [ "\$count" = "\$expected_count" ] && ! grep -Fvx "0::\$expected" "\$stress" >/dev/null || {
        echo "CG-FAIL \$role generation=\$generation immediate-fork=\$(sort -u "\$stress" | tr '\n' ',') count=\$count"
        exit 71
      }
      echo "CG-\$role-GEN-\$generation-FORK-OK 128"

      if [ "\$role" = primary ]; then
        if [ "\$pid_mode" = private ]; then
          [ "\$\$" = 1 ] || { echo "CG-FAIL primary private pid=\$\$"; exit 72; }
        else
          [ "\$\$" != 1 ] && tr '\0' ' ' < /proc/1/cmdline | grep -q '/sbin/init' || {
            echo "CG-FAIL primary shared cannot-see-sandbox-init-pid1"; exit 73;
          }
        fi
        echo "CG-\$role-GEN-\$generation-PID-\$pid_mode-OK"
      elif [ "\$role" = exec ] && [ "\$pid_mode" = private ]; then
        tr '\0' ' ' < /proc/1/cmdline | grep -q '/probe.sh' || {
          echo "CG-FAIL exec did-not-join-primary-pidns"; exit 74;
        }
        echo "CG-exec-PID-private-OK"
      fi

      if [ "\$control" = true ]; then
        for controller in \$(cat /sys/fs/cgroup/cgroup.controllers); do
          tr ' ' '\n' < /sys/fs/cgroup/cgroup.subtree_control | grep -qx "\$controller" || {
            echo "CG-FAIL controller-not-enabled=\$controller"; exit 77;
          }
        done
        for file in cpu.weight memory.current io.stat; do
          [ -e "/sys/fs/cgroup/init/\$file" ] || {
            echo "CG-FAIL missing-controller-file=\$file"; exit 78;
          }
        done
        echo "CG-\$role-GEN-\$generation-CONTROLLERS-OK"
      fi

      if [ "\$real_check" = 1 ]; then
        real_root=/proc/1/root/sys/fs/cgroup/app
        if [ "\$control" = true ]; then
          [ ! -s "\$real_root/cgroup.procs" ] || {
            echo "CG-FAIL \$role real-app-root-not-empty"; cat "\$real_root/cgroup.procs"; exit 75;
          }
          grep -qx "\$\$" "\$real_root/init/cgroup.procs" || {
            echo "CG-FAIL \$role pid=\$\$ missing-real-app-init"; exit 76;
          }
        else
          grep -qx "\$\$" "\$real_root/cgroup.procs" || {
            echo "CG-FAIL \$role pid=\$\$ missing-real-app"; exit 79;
          }
        fi
        echo "CG-\$role-GEN-\$generation-REAL-OK"
      fi

      touch "/tmp/cg-\${role}.\${generation}.ready"
      case "\$role:\$generation" in
        primary:1)
          while [ ! -e /tmp/cg-plugin.2.ready ]; do sleep 0.01; done
          exit 42
          ;;
        plugin:1) exit 43 ;;
        exec:*) exit 0 ;;
      esac
      trap 'exit 0' TERM INT
      while :; do sleep 1; done
init:
  - exec: /bin/sh
    args: ["-c", "base64 -d /cgroup-fork-probe.b64 > /cgroup-fork-probe && chmod 0755 /cgroup-fork-probe"]
  - exec: /bin/sh
    args: ["-c", "printf 'CG-TOP-INIT '; cat /proc/self/cgroup"]
launch:
  exec: /cgroup-fork-probe
  args: [/probe.sh, primary, "$expected", "$pid_mode", "$control", "$real_check"]
  restart: always
  cgroup_control: $control
  pid_namespace: $pid_mode
  plugin:
    - exec: /cgroup-fork-probe
      args: [/probe.sh, plugin, "$expected", "$pid_mode", "$control", "$plugin_real_check"]
      restart: always
EOF

    echo "==> cgroup case $name"
    "$BIN/sandbox-ctl" run \
        --config "$case_dir/sandbox.yaml" \
        --ch-binary "$BIN/cloud-hypervisor" \
        --run-root "$run_root" \
        --sandbox-id "$sid" > "$log" 2>&1 &
    local run_pid=$!
    PIDS+=("$run_pid")

    wait_marker "CG-primary-GEN-2-SELF $expected" "$log" "$run_pid"
    wait_marker "CG-primary-GEN-2-FORK-OK 128" "$log" "$run_pid"
    wait_marker "CG-plugin-GEN-2-SELF $expected" "$log" "$run_pid"
    wait_marker "CG-plugin-GEN-2-FORK-OK 128" "$log" "$run_pid"
    wait_marker "CG-primary-GEN-2-PID-$pid_mode-OK" "$log" "$run_pid"
    grep -qF "CG-TOP-INIT 0::/" "$log"

    local exec_log="$case_dir/exec.log"
    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$run_root" -- \
        /cgroup-fork-probe /probe.sh exec "$expected" "$pid_mode" "$control" "$real_check" > "$exec_log" 2>&1
    grep -qF "CG-exec-GEN-1-SELF $expected" "$exec_log"
    grep -qF "CG-exec-GEN-1-FORK-OK 128" "$exec_log"
    if [ "$pid_mode" = private ]; then
        grep -qF "CG-exec-PID-private-OK" "$exec_log"
    fi
    if [ "$real_check" = 1 ]; then
        grep -qF "CG-primary-GEN-2-REAL-OK" "$log"
        grep -qF "CG-exec-GEN-1-REAL-OK" "$exec_log"
    fi
    grep -qF "CG-plugin-GEN-2-REAL-OK" "$log"
    if [ "$control" = true ]; then
        grep -qF "CG-primary-GEN-2-CONTROLLERS-OK" "$log"
        grep -qF "CG-plugin-GEN-2-CONTROLLERS-OK" "$log"
        grep -qF "CG-exec-GEN-1-CONTROLLERS-OK" "$exec_log"
    fi
    if grep -qF "CG-FAIL" "$log" "$exec_log"; then
        echo "==> FAIL: cgroup probe reported a mismatch ($name)"
        grep -nF "CG-FAIL" "$log" "$exec_log"
        exit 1
    fi

    kill -TERM "$run_pid"
    wait "$run_pid" || true
    PIDS=("${PIDS[@]:0:${#PIDS[@]}-1}")
    echo "==> PASS: $name"
}

run_case false shared
run_case false private
run_case true shared
run_case true private

echo "==> e2e_sandbox_cgroup_control: OK"
