#!/usr/bin/env bash

set -euo pipefail
# Disk-specific DIO/residency tests need a disk fixture; runtime also supports tmpfs.
export TMPDIR="${TMPDIR:-/var/tmp}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BIN:?BIN must point to the assembled platform binary directory}"
export BIN REQUIRE_KVM=1

# Source unit/race/vet checks run separately via scripts/ci-source-checks.sh.

shopt -s nullglob
cases=("$SCRIPT_DIR"/e2e_*.sh)
[ "${#cases[@]}" -gt 0 ] || {
    echo "sandboxer e2e suite contains no cases" >&2
    exit 1
}

for script in "${cases[@]}"; do
    echo
    echo "========================================="
    echo "  sandboxer/$(basename "$script")"
    echo "========================================="
    bash "$script"
done
