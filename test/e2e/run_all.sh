#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BIN:?BIN must point to the assembled platform binary directory}"
export BIN REQUIRE_KVM=1

shopt -s nullglob
cases=("$SCRIPT_DIR"/e2e_*.sh)
[ "${#cases[@]}" -gt 0 ] || {
    echo "sandboxer e2e suite contains no cases" >&2
    exit 1
}

for script in "${cases[@]}"; do
    # Manual perf case: long, needs a guest-runtime tree, and is not a
    # correctness gate. Run it explicitly:
    #   ./test/e2e/e2e_sandbox_vhost_cow_perf.sh
    case "$(basename "$script")" in
        e2e_sandbox_vhost_cow_perf.sh) continue ;;
    esac
    echo
    echo "========================================="
    echo "  sandboxer/$(basename "$script")"
    echo "========================================="
    bash "$script"
done
