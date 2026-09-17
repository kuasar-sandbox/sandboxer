#!/usr/bin/env bash
# Component-owned real CH/KVM usage integration. Missing prerequisites fail.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# sudo may replace PATH with secure_path even when -E preserves GOROOT. Pin the
# Go driver selected by the caller so the driver and its compiler stay paired.
if [[ -z "${USAGE_GO:-}" ]]; then
    USAGE_GO="$(command -v go)" || { echo "go is required" >&2; exit 1; }
fi
[[ "$USAGE_GO" = /* && -x "$USAGE_GO" ]] || {
    echo "USAGE_GO must be an absolute executable path: $USAGE_GO" >&2
    exit 1
}
export USAGE_GO
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
python3 "$SCRIPT_DIR/usage.py" "$@"
