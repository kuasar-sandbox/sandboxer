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
    exec sudo -nE bash "$0" "$@"
fi
cd "$REPO_ROOT"
export PYTHONPYCACHEPREFIX="$(mktemp -d /tmp/usage-e2e-pycache-XXXXXX)"
python3 "$SCRIPT_DIR/usage_harness_test.py"
if [ -n "${SANDBOXER_USAGE_CASES:-}" ]; then
    python3 "$SCRIPT_DIR/usage.py" --cases "$SANDBOXER_USAGE_CASES" "$@"
else
    python3 "$SCRIPT_DIR/usage.py" "$@"
fi
