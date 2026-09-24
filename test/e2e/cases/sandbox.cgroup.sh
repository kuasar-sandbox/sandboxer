#!/usr/bin/env bash
# cgroup_control topology and immediate-fork placement race using a prepared probe.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"
: "${CGROUP_FORK_PROBE_BIN:?CGROUP_FORK_PROBE_BIN is a required prepared fixture}"

require_root
require_kvm
for command in base64 docker mkfs.ext4 timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -x "$CGROUP_FORK_PROBE_BIN" ] || e2e_fail "prepared cgroup fork probe is missing or not executable"
[ -r "$SANDBOXER_LIB/tarstream.sh" ] || e2e_fail "missing prepared tarstream helpers"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT"
PIDS=()
cleanup() {
    local status=$?
    set +e
    for pid in "${PIDS[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            kill -TERM "$pid" 2>/dev/null
            for _ in $(seq 1 50); do
                kill -0 "$pid" 2>/dev/null || break
                sleep 0.1
            done
            kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
        fi
        wait "$pid" 2>/dev/null
    done
    exit "$status"
}
trap cleanup EXIT

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"
# The fixture was compiled during the source-build stage. Product E2E only
# transports its exact prepared bytes into the guest.
PROBE_B64="$(base64 <"$CGROUP_FORK_PROBE_BIN" | tr -d '\n')"

wait_marker() {
    local marker=$1 log=$2 pid=$3
    for _ in $(seq 1 1200); do
        grep -qF "$marker" "$log" 2>/dev/null && return 0
        if ! kill -0 "$pid" 2>/dev/null; then
            tail -100 "$log" >&2 || true
            return 1
        fi
        sleep 0.05
    done
    tail -100 "$log" >&2 || true
    return 1
}

run_case() {
    local control=$1 pid_mode=$2
    local expected=/
    [ "$control" = true ] && expected=/init
    local real_check=0
    [ "$pid_mode" = shared ] && real_check=1
    # Plugins always share sandbox-init's PID namespace and can inspect the
    # guest-global cgroup mount through /proc/1 even when primary is private.
    local plugin_real_check=1
    local name="${control}-${pid_mode}"
    local case_dir="$WORK/$name"
    local run_root="$case_dir/runtime"
    local sid="cg-${name}-$BASHPID"
    local log="$OUT/$name.log"
    mkdir -p "$case_dir" "$run_root"

    local diff="$case_dir/root.diff"
    truncate -s 1G "$diff"
    mkfs.ext4 -q -F -O ^has_journal "$diff"

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
    overlay: { diff: file://$diff }
files:
  - path: /cgroup-fork-probe.b64
    mode: "0644"
    content: "$PROBE_B64"
  - path: /probe.sh
    mode: "0755"
    content: |
      #!/bin/sh
      set -eu
      role="\$1"; expected="\$2"; pid_mode="\$3"; control="\$4"; real_check="\$5"
      generation_file="/tmp/cg-\${role}.generation"
      generation=0
      [ ! -f "\$generation_file" ] || generation="\$(cat "\$generation_file")"
      generation=\$((generation + 1))
      echo "\$generation" >"\$generation_file"

      self_path="\$(awk -F: '\$1 == "0" { print \$3 }' /proc/self/cgroup)"
      [ "\$self_path" = "\$expected" ] || { echo "CG-FAIL \$role generation=\$generation self=\$self_path expected=\$expected"; exit 70; }
      [ ! -e /sys/fs/cgroup/app ] || { echo "CG-FAIL \$role inherited-guest-global-cgroupfs"; exit 68; }
      for fd_path in /proc/self/fd/*; do
        fd_target="\$(readlink "\$fd_path" 2>/dev/null || true)"
        case "\$fd_target" in
          cgroup:\[*|mnt:\[*|/sys/fs/cgroup/app*) echo "CG-FAIL \$role leaked-fd=\$fd_path target=\$fd_target"; exit 69 ;;
        esac
      done
      echo "CG-\$role-GEN-\$generation-SELF \$self_path"

      stress="/tmp/cg-\${role}.fast-paths"
      count="\$(wc -l <"\$stress" | tr -d ' ')"
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
          [ "\$\$" != 1 ] && tr '\0' ' ' </proc/1/cmdline | grep -q '/sbin/init' || {
            echo "CG-FAIL primary shared cannot-see-sandbox-init-pid1"; exit 73;
          }
        fi
        echo "CG-\$role-GEN-\$generation-PID-\$pid_mode-OK"
      elif [ "\$role" = exec ] && [ "\$pid_mode" = private ]; then
        tr '\0' ' ' </proc/1/cmdline | grep -q '/probe.sh' || { echo "CG-FAIL exec did-not-join-primary-pidns"; exit 74; }
        echo "CG-exec-PID-private-OK"
      fi

      if [ "\$control" = true ]; then
        for controller in \$(cat /sys/fs/cgroup/cgroup.controllers); do
          tr ' ' '\n' </sys/fs/cgroup/cgroup.subtree_control | grep -qx "\$controller" || {
            echo "CG-FAIL controller-not-enabled=\$controller"; exit 77;
          }
        done
        for file in cpu.weight memory.current io.stat; do
          [ -e "/sys/fs/cgroup/init/\$file" ] || { echo "CG-FAIL missing-controller-file=\$file"; exit 78; }
        done
        echo "CG-\$role-GEN-\$generation-CONTROLLERS-OK"
      fi

      if [ "\$real_check" = 1 ]; then
        real_root=/proc/1/root/sys/fs/cgroup/app
        if [ "\$control" = true ]; then
          [ ! -s "\$real_root/cgroup.procs" ] || { echo "CG-FAIL \$role real-app-root-not-empty"; cat "\$real_root/cgroup.procs"; exit 75; }
          grep -qx "\$\$" "\$real_root/init/cgroup.procs" || { echo "CG-FAIL \$role pid=\$\$ missing-real-app-init"; exit 76; }
        else
          grep -qx "\$\$" "\$real_root/cgroup.procs" || { echo "CG-FAIL \$role pid=\$\$ missing-real-app"; exit 79; }
        fi
        echo "CG-\$role-GEN-\$generation-REAL-OK"
      fi

      touch "/tmp/cg-\${role}.\${generation}.ready"
      case "\$role:\$generation" in
        primary:1) while [ ! -e /tmp/cg-plugin.2.ready ]; do sleep 0.01; done; exit 42 ;;
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

    "$BIN/sandbox-ctl" run --config "$case_dir/sandbox.yaml" --ch-binary "$BIN/cloud-hypervisor" \
        --run-root "$run_root" --sandbox-id "$sid" >"$log" 2>&1 &
    local run_pid=$!
    PIDS+=("$run_pid")

    wait_marker "CG-primary-GEN-2-SELF $expected" "$log" "$run_pid" || e2e_fail "$name primary did not restart in expected cgroup"
    wait_marker "CG-primary-GEN-2-FORK-OK 128" "$log" "$run_pid" || e2e_fail "$name primary immediate-fork placement failed"
    wait_marker "CG-plugin-GEN-2-SELF $expected" "$log" "$run_pid" || e2e_fail "$name plugin did not restart in expected cgroup"
    wait_marker "CG-plugin-GEN-2-FORK-OK 128" "$log" "$run_pid" || e2e_fail "$name plugin immediate-fork placement failed"
    wait_marker "CG-primary-GEN-2-PID-$pid_mode-OK" "$log" "$run_pid" || e2e_fail "$name PID namespace contract failed"
    grep -qF 'CG-TOP-INIT 0::/' "$log" || e2e_fail "$name sandbox-init cgroup baseline missing"

    local exec_log="$OUT/$name-exec.log"
    "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$run_root" -- \
        /cgroup-fork-probe /probe.sh exec "$expected" "$pid_mode" "$control" "$real_check" >"$exec_log" 2>&1
    grep -qF "CG-exec-GEN-1-SELF $expected" "$exec_log" || e2e_fail "$name exec cgroup mismatch"
    grep -qF 'CG-exec-GEN-1-FORK-OK 128' "$exec_log" || e2e_fail "$name exec immediate-fork placement failed"
    [ "$pid_mode" != private ] || grep -qF 'CG-exec-PID-private-OK' "$exec_log" || e2e_fail "$name exec missed private primary PID namespace"
    if [ "$real_check" = 1 ]; then
        grep -qF 'CG-primary-GEN-2-REAL-OK' "$log" || e2e_fail "$name primary real membership missing"
        grep -qF 'CG-exec-GEN-1-REAL-OK' "$exec_log" || e2e_fail "$name exec real membership missing"
    fi
    grep -qF 'CG-plugin-GEN-2-REAL-OK' "$log" || e2e_fail "$name plugin real membership missing"
    if [ "$control" = true ]; then
        grep -qF 'CG-primary-GEN-2-CONTROLLERS-OK' "$log" || e2e_fail "$name primary controllers missing"
        grep -qF 'CG-plugin-GEN-2-CONTROLLERS-OK' "$log" || e2e_fail "$name plugin controllers missing"
        grep -qF 'CG-exec-GEN-1-CONTROLLERS-OK' "$exec_log" || e2e_fail "$name exec controllers missing"
    fi
    if grep -qF 'CG-FAIL' "$log" "$exec_log"; then
        grep -nF 'CG-FAIL' "$log" "$exec_log" >&2 || true
        e2e_fail "$name cgroup probe reported a mismatch"
    fi

    kill -TERM "$run_pid"
    wait "$run_pid" || true
    PIDS=("${PIDS[@]:0:${#PIDS[@]}-1}")
}

run_case false shared
run_case false private
run_case true shared
run_case true private

echo "PASS sandbox.cgroup.sh"
