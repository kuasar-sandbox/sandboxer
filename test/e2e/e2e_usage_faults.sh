#!/usr/bin/env bash
set -euo pipefail
# Active diff bodies require disk-backed storage; /tmp may be tmpfs.
export TMPDIR="${TMPDIR:-/var/tmp}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BIN:?BIN must contain the assembled binaries and rebuilt runtime bundle}"
test -r /dev/kvm && test -w /dev/kvm || { echo "KVM is required" >&2; exit 1; }
# The shared usage module resolves and executes the selected Go distribution.
for command in strace mount umount mkfs.ext4 python3; do
    command -v "$command" >/dev/null || { echo "missing $command" >&2; exit 1; }
done
if [ "$(id -u)" -ne 0 ]; then
    exec sudo -nE bash "$0" "$@"
fi
export PYTHONPYCACHEPREFIX="$(mktemp -d /tmp/usage-faults-pycache-XXXXXX)"
python3 "$SCRIPT_DIR/usage_faults.py" "$@"
