#!/usr/bin/env bash
set -euo pipefail
: "${BIN:?set BIN to the assembled runtime binary directory}"
[[ -r /dev/kvm && -w /dev/kvm ]] || { echo 'usage sources requires KVM' >&2; exit 1; }
for command in strace ldd mkfs.ext4 mount umount python3 go; do
    command -v "$command" >/dev/null || { echo "missing $command" >&2; exit 1; }
done
if [[ "$(id -u)" != 0 ]]; then
    exec sudo -n -E bash "$0" "$@"
fi
export PYTHONPYCACHEPREFIX="$(mktemp -d /tmp/e2e-usage-sources-pycache.XXXXXX)"
exec python3 "$(dirname "$0")/usage_sources.py" "$@"
