#!/usr/bin/env bash
# Source checks are a separate required CI stage, never artifact E2E.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
BIN="${BIN:-$PWD/bin/x86_64}" python3 -B test/e2e/usage_harness_test.py
CGO_ENABLED=0 go test -count=1 ./...
bash scripts/test-vhost-tmpfs-runner.sh
./scripts/test-vhost-tmpfs-enospc.sh
CGO_ENABLED=1 go test -race -count=1 ./...
CGO_ENABLED=0 go vet ./...
