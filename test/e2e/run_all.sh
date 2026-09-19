#!/usr/bin/env bash

set -euo pipefail
# Disk-specific DIO/residency tests need a disk fixture; runtime also supports tmpfs.
export TMPDIR="${TMPDIR:-/var/tmp}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BIN:?BIN must point to the assembled platform binary directory}"
export BIN REQUIRE_KVM=1

# Source integration CI has an exact multi-repository Go workspace. Exercise
# the controller's deterministic fault/partial-progress cases before real KVM
# tests. Published-asset suites have no source workspace and still run every
# binary E2E below; a missing source checkout in source CI is an error.
if [ -n "${CANDIDATE_REPOSITORY:-}" ]; then
    source_root="$(go list -m -f '{{.Dir}}' github.com/kuasar-sandbox/sandboxer)"
    [ -f "$source_root/go.mod" ] || {
        echo "sandboxer source integration checkout is missing" >&2
        exit 1
    }
    (
        cd "$source_root"
        echo "==> sandboxer source unit and memory-controller race regressions"
        CGO_ENABLED=0 go test -count=1 ./...
        bash scripts/test-vhost-tmpfs-runner.sh
        ./scripts/test-vhost-tmpfs-enospc.sh
        CGO_ENABLED=1 go test -race -count=1 ./pkg/resctl ./pkg/ctl ./pkg/usagereader ./pkg/sandbox
        CGO_ENABLED=0 go vet ./...
    )
fi

shopt -s nullglob
cases=("$SCRIPT_DIR"/e2e_*.sh)
[ "${#cases[@]}" -gt 0 ] || {
    echo "sandboxer e2e suite contains no cases" >&2
    exit 1
}

for script in "${cases[@]}"; do
    case "${SANDBOXER_E2E_GROUP:-all}:$(basename "$script")" in
        defaults:e2e_usage.sh)
            export SANDBOXER_USAGE_CASES=defaults
            ;;
        defaults:*)
            continue
            ;;
        main:e2e_usage.sh)
            export SANDBOXER_USAGE_CASES=off,overlay,single,balloon,balloon-no-oom,oom,multidisk,restore
            ;;
        *)
            unset SANDBOXER_USAGE_CASES || true
            ;;
    esac
    echo
    echo "========================================="
    echo "  sandboxer/$(basename "$script")"
    echo "========================================="
    bash "$script"
done
