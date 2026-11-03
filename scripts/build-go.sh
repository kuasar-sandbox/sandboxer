#!/usr/bin/env bash
# Embed Git's description; Go itself supplies the authoritative full revision.
set -euo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
describe="$(git -C "$root" describe --tags --long --always --abbrev=12 --dirty --match 'v[0-9]*' 2>/dev/null || true)"
# Linker argument parsing must never interpret tag names as quoting syntax.
if [[ -n "$describe" && ! "$describe" =~ ^[a-zA-Z0-9._+-]+$ ]]; then
  echo 'unsupported characters in Git version description' >&2
  exit 1
fi
exec "${GO:-go}" build -buildvcs=true \
  -ldflags "${GO_LDFLAGS:-} -X github.com/kuasar-sandbox/sandboxer/internal/buildversion.describe=$describe" "$@"
