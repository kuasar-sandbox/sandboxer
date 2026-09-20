#!/usr/bin/env bash
# Component-owned real CH/KVM usage integration. Missing prerequisites fail.
set -euo pipefail
# Active diff bodies require disk-backed storage; /tmp may be tmpfs.
export TMPDIR="${TMPDIR:-/var/tmp}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
: "${BIN:?BIN must contain the assembled binaries and newly rebuilt runtime bundle}"
for name in sandbox-ctl sandbox-init cloud-hypervisor flatten-ctl sandbox-runtime.bundle vmlinux; do
    test -f "$BIN/$name" || { echo "missing $BIN/$name" >&2; exit 1; }
done
test -r /dev/kvm && test -w /dev/kvm || { echo "KVM is required" >&2; exit 1; }
if [ "$(id -u)" -ne 0 ]; then
    selected_go=$(command -v "${KUASAR_E2E_GO:-${GOROOT:+$GOROOT/bin/}go}") || exit 1
    selected_go="$(cd "$(dirname "$selected_go")" && pwd)/${selected_go##*/}"
    # Resolve before sudo drops the PATH entries used by named/+path policies.
    go_source="$(cd "$(dirname "$0")/../.." && pwd)"
    selected_root=$(GOWORK=off "$selected_go" -C "$go_source" env GOROOT) || exit 1
    bundled_root=$(GO111MODULE=off GOWORK=off GOTOOLCHAIN=local "$selected_go" env GOROOT) || exit 1
    if [ "$selected_root" != "$bundled_root" ]; then
        selected_version=$(GOWORK=off "$selected_go" -C "$go_source" env GOVERSION) || exit 1
        selected_go=$(command -v "$selected_version" || printf '%s/bin/go\n' "$selected_root")
        [ -f "$selected_go" ] && [ -x "$selected_go" ] || { echo "selected Go toolchain is unavailable" >&2; exit 1; }
        selected_go="$(cd "$(dirname "$selected_go")" && pwd)/${selected_go##*/}"
    fi
    exec sudo -nE env KUASAR_E2E_GO="$selected_go" GOROOT="$selected_root" /bin/bash "$0" "$@"
fi
cd "$REPO_ROOT"
export PYTHONPYCACHEPREFIX="$(mktemp -d /tmp/usage-e2e-pycache-XXXXXX)"
python3 "$SCRIPT_DIR/usage_harness_test.py"
python3 "$SCRIPT_DIR/usage.py" "$@"
