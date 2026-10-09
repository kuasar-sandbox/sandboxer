#!/usr/bin/env bash
# Source checks are a separate required CI stage, never artifact E2E.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
make test-e2e-scripts
CGO_ENABLED=0 go test -count=1 ./...
bash scripts/test-vhost-tmpfs-runner.sh
./scripts/test-vhost-tmpfs-enospc.sh
CGO_ENABLED=1 go test -race -count=1 ./...
CGO_ENABLED=0 go vet ./...
