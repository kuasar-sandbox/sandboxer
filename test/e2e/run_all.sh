#!/usr/bin/env bash

set -euo pipefail
# Disk-specific DIO/residency tests need a disk fixture; runtime also supports tmpfs.
export TMPDIR="${TMPDIR:-/var/tmp}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${BIN:?BIN must point to the assembled platform binary directory}"
export BIN REQUIRE_KVM=1

# Source-wide checks run once at the owner-suite boundary. CI source mode
# requires the exact module; documented local source runs discover it directly.
source_root=""
if command -v go >/dev/null 2>&1; then
    source_root="$(GOPROXY=off GOSUMDB=off go list -m -f '{{.Dir}}' github.com/kuasar-sandbox/sandboxer 2>/dev/null || true)"
fi
if [ -n "${CANDIDATE_REPOSITORY:-}" ] && { [ -z "$source_root" ] || [ ! -f "$source_root/go.mod" ]; }; then
    echo "sandboxer source integration checkout is missing" >&2
    exit 1
fi
if [ -n "$source_root" ] && [ -f "$source_root/go.mod" ]; then
    (
        cd "$source_root"
        echo "==> sandboxer source unit, race and vet regressions"
        CGO_ENABLED=0 go test -count=1 ./...
        bash scripts/test-vhost-tmpfs-runner.sh
        ./scripts/test-vhost-tmpfs-enospc.sh
        CGO_ENABLED=1 go test -race -count=1 ./...
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
