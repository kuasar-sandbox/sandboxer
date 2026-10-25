#!/usr/bin/env bash
# Complete declared test-helper inventory; retain the existing owner recipes.
set -euo pipefail
[ "$#" = 2 ] || { echo 'usage: build-e2e-helpers.sh ARCH OUTPUT' >&2; exit 2; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
case "$1" in x86_64|aarch64) ;; *) exit 2 ;; esac
make -C "$ROOT" "TARGET_ARCH=$1" "E2E_FIXTURE_DIR=$2" e2e-usage-probe e2e-cgroup-fork-probe e2e-artifact-ref
