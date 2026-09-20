#!/usr/bin/env bash
set -euo pipefail
# Active diff bodies require disk-backed storage; /tmp may be tmpfs.
export TMPDIR="${TMPDIR:-/var/tmp}"
: "${BIN:?set BIN to the assembled runtime binary directory}"
[[ -r /dev/kvm && -w /dev/kvm ]] || { echo 'usage sources requires KVM' >&2; exit 1; }
# The shared usage module resolves and executes the selected Go distribution.
for command in strace ldd mkfs.ext4 mount umount python3; do
    command -v "$command" >/dev/null || { echo "missing $command" >&2; exit 1; }
done
if [[ "$(id -u)" != 0 ]]; then
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
export PYTHONPYCACHEPREFIX="$(mktemp -d /tmp/e2e-usage-sources-pycache.XXXXXX)"
exec python3 "$(dirname "$0")/usage_sources.py" "$@"
