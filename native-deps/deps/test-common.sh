#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT
# shellcheck source=common.sh
source "$SCRIPT_DIR/common.sh"
export TARBALL_CACHE="$test_root/cache"
mkdir -p "$test_root/bin"
cat > "$test_root/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [ "$#" -gt 0 ] && [ "$1" != -o ]; do shift; done
[ "$1" = -o ]
printf '%s\n' "$TEST_DOWNLOAD_CONTENT" > "$2"
exit "$TEST_DOWNLOAD_EXIT"
EOF
chmod 0755 "$test_root/bin/curl"
export PATH="$test_root/bin:$PATH"
export TEST_DOWNLOAD_CONTENT=partial TEST_DOWNLOAD_EXIT=7
if (result="$(resolve_tarball https://example.invalid/source.tar.gz)" >/dev/null 2>&1); then
    die "failed curl was accepted as a downloaded source"
fi
[ ! -e "$TARBALL_CACHE/source.tar.gz" ] || die "failed download was promoted into the cache"
[ -f "$TARBALL_CACHE/source.tar.gz.tmp" ] || die "failed download evidence was unexpectedly deleted"
export TEST_DOWNLOAD_CONTENT=complete TEST_DOWNLOAD_EXIT=0
expected="$(printf 'complete\n' | sha256sum | awk '{print $1}')"
result="$(resolve_tarball https://example.invalid/source.tar.gz "$expected")"
[ "$result" = "$TARBALL_CACHE/source.tar.gz" ]
[ ! -e "$TARBALL_CACHE/source.tar.gz.tmp" ]
[ "$(cat "$result")" = complete ]
printf 'test-common: failed download refusal and successful retry PASS\n'
