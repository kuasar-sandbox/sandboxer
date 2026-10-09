#!/usr/bin/env bash
# Source checks are a separate required CI stage, never artifact E2E.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
mode="${1:-}"
if [ "$#" -gt 1 ] || { [ -n "$mode" ] && [ "$mode" != --ordinary ] && [ "$mode" != --privileged ]; }; then
    echo 'usage: ci-source-checks.sh [--ordinary|--privileged]' >&2
    exit 2
fi
if [ "$mode" != --privileged ]; then
    make test-e2e-scripts
    CGO_ENABLED=0 go test -count=1 ./...
    bash scripts/test-vhost-tmpfs-runner.sh
fi
if [ "$mode" != --ordinary ]; then
    # Workbench system mode keeps the exact compiler, libraries and paths for
    # the existing mount-namespace test; build mode runs the ordinary checks.
    ./scripts/test-vhost-tmpfs-enospc.sh
fi
if [ "$mode" != --privileged ]; then
    CGO_ENABLED=1 go test -race -count=1 ./...
    CGO_ENABLED=0 go vet ./...
fi
