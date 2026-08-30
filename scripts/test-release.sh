#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() {
  echo "test-release: $*" >&2
  exit 1
}

bash "$ROOT/scripts/test-preview-line.sh"
bash "$ROOT/scripts/test-delete-preview.sh"

mkdir -p "$TMP/source-bin"
cat > "$TMP/source-bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = api ] || exit 2
[ "${2:-}" = "repos/$GITHUB_REPOSITORY/git/ref/heads/${FAKE_SOURCE_REF:?}" ] || exit 2
printf '%s\n' "${FAKE_SOURCE_SHA:?}"
EOF
chmod +x "$TMP/source-bin/gh"
env PATH="$TMP/source-bin:$PATH" GITHUB_REPOSITORY=kuasar-sandbox/sandboxer FAKE_SOURCE_REF=release/v1.2.x FAKE_SOURCE_SHA=1111111111111111111111111111111111111111 bash "$ROOT/scripts/validate-release-source.sh" release/v1.2.x 1111111111111111111111111111111111111111 v1.2.3 sandboxer >/dev/null
if env PATH="$TMP/source-bin:$PATH" GITHUB_REPOSITORY=kuasar-sandbox/sandboxer FAKE_SOURCE_REF=release/v1.2.x FAKE_SOURCE_SHA=1111111111111111111111111111111111111111 bash "$ROOT/scripts/validate-release-source.sh" release/v1.2.x 1111111111111111111111111111111111111111 v1.3.0 sandboxer >/dev/null 2>&1; then
  fail "release source validator accepted a tag from another version line"
fi
bash -n "$ROOT/scripts/delete-preview.sh" "$ROOT/scripts/validate-release-source.sh"

WORKFLOW="$ROOT/.github/workflows/release.yml"
for input in accelerator_version connector_version; do
  grep -Fq "      $input:" "$WORKFLOW" \
    || fail "release workflow is missing required $input input"
  [ "$(grep -Fc "ref: \${{ needs.preflight.outputs.$input }}" "$WORKFLOW")" -eq 2 ] \
    || fail "release workflow does not pin both $input checkouts"
done
grep -Fq "repos/kuasar-sandbox/\$repository/releases/tags/\$version" "$WORKFLOW" \
  || fail "release workflow does not verify dependency releases"

ARCHIVE_NAME=sandboxer-v1.2.3-linux-x86_64.tar.gz

repack_bundle() {
  local bundle="$1" root="$2" archive
  archive="$bundle/assets/$ARCHIVE_NAME"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@1700000000" \
    --pax-option=delete=atime,delete=ctime -czf "$archive" -C "$root" .
  (cd "$bundle/assets" && sha256sum "$ARCHIVE_NAME" > SHA256SUMS)
}

repack_bundle_with_owner_names() {
  local bundle="$1" root="$2" owner="$3" group="$4" archive
  archive="$bundle/assets/$ARCHIVE_NAME"
  tar --sort=name --owner="$owner" --group="$group" --mtime="@1700000000" \
    --pax-option=delete=atime,delete=ctime -czf "$archive" -C "$root" .
  (cd "$bundle/assets" && sha256sum "$ARCHIVE_NAME" > SHA256SUMS)
}

expect_invalid_archive() {
  local bundle="$1" description="$2"
  if "$ROOT/scripts/release.sh" validate v1.2.3 x86_64 "$bundle" >/dev/null 2>&1; then
    fail "validator accepted $description"
  fi
}

for entrypoint in test/e2e/e2e_sandbox_*.sh test/e2e/run_all.sh; do
  [ "$(git -C "$ROOT" ls-files -s -- "$entrypoint" | awk '{print $1}')" = 100755 ] \
    || fail "$entrypoint is not executable in the Git index"
done

mkdir -p "$TMP/bin" "$TMP/src"
printf 'package main\nfunc main() {}\n' > "$TMP/src/main.go"
GO111MODULE=off go build -o "$TMP/go-fixture" "$TMP/src/main.go"
for binary in sandbox-ctl sandbox-init; do
  install -m 0755 "$TMP/go-fixture" "$TMP/bin/$binary"
done
printf '#!/bin/sh\nexit 0\n' > "$TMP/bin/cloud-hypervisor"
chmod +x "$TMP/bin/cloud-hypervisor"

SOURCE_DATE_EPOCH=1700000000 RELEASE_BIN_DIR="$TMP/bin" \
  "$ROOT/scripts/release.sh" package v1.2.3 x86_64 "$TMP/bundle"
"$ROOT/scripts/release.sh" validate v1.2.3 x86_64 "$TMP/bundle"
bash "$ROOT/scripts/test-publisher.sh" "$ROOT/scripts/publish-release.sh" \
  "$TMP/bundle" kuasar-sandbox/sandboxer v1.2.3 \
  1111111111111111111111111111111111111111 main
bash "$ROOT/scripts/test-publisher.sh" "$ROOT/scripts/publish-release.sh" \
  "$TMP/bundle" kuasar-sandbox/sandboxer v1.2.3 \
  1111111111111111111111111111111111111111 release/v1.2.x

archive="$TMP/bundle/assets/$ARCHIVE_NAME"
printf '%s\n' ./ ./bin/ ./bin/cloud-hypervisor ./bin/sandbox-ctl ./bin/sandbox-init \
  > "$TMP/expected-archive-entries"
LC_ALL=C tar --quoting-style=escape -tzf "$archive" | LC_ALL=C sort \
  > "$TMP/actual-archive-entries"
cmp -s "$TMP/expected-archive-entries" "$TMP/actual-archive-entries" \
  || fail "packager did not emit the exact archive entry set"

SOURCE_DATE_EPOCH=1700000000 RELEASE_BIN_DIR="$TMP/bin" \
  "$ROOT/scripts/release.sh" package v1.2.3 x86_64 "$TMP/reproducible"
cmp -s "$archive" "$TMP/reproducible/assets/sandboxer-v1.2.3-linux-x86_64.tar.gz" \
  || fail "identical inputs did not produce an identical archive"

cp -a "$TMP/bundle" "$TMP/tampered"
printf 'tampered\n' >> "$TMP/tampered/assets/sandboxer-v1.2.3-linux-x86_64.tar.gz"
if "$ROOT/scripts/release.sh" validate v1.2.3 x86_64 "$TMP/tampered" >/dev/null 2>&1; then
  fail "validator accepted a tampered archive"
fi

cp -a "$TMP/bundle" "$TMP/extra"
touch "$TMP/extra/assets/release.json"
if "$ROOT/scripts/release.sh" validate v1.2.3 x86_64 "$TMP/extra" >/dev/null 2>&1; then
  fail "validator accepted an extra asset"
fi

mkdir -p "$TMP/archive-root"
tar -xzf "$archive" -C "$TMP/archive-root"

cp -a "$TMP/archive-root" "$TMP/extra-file-root"
printf 'unexpected\n' > "$TMP/extra-file-root/bin/unexpected"
cp -a "$TMP/bundle" "$TMP/extra-file-bundle"
repack_bundle "$TMP/extra-file-bundle" "$TMP/extra-file-root"
expect_invalid_archive "$TMP/extra-file-bundle" "an unexpected regular file"

cp -a "$TMP/archive-root" "$TMP/linked-binary-root"
rm "$TMP/linked-binary-root/bin/sandbox-init"
ln -s sandbox-ctl "$TMP/linked-binary-root/bin/sandbox-init"
cp -a "$TMP/bundle" "$TMP/linked-binary-bundle"
repack_bundle "$TMP/linked-binary-bundle" "$TMP/linked-binary-root"
expect_invalid_archive "$TMP/linked-binary-bundle" "a symlink in place of a binary"

cp -a "$TMP/archive-root" "$TMP/unexpected-link-root"
ln -s ./ "$TMP/unexpected-link-root/unexpected-link"
cp -a "$TMP/bundle" "$TMP/unexpected-link-bundle"
repack_bundle "$TMP/unexpected-link-bundle" "$TMP/unexpected-link-root"
expect_invalid_archive "$TMP/unexpected-link-bundle" "an unexpected symlink whose target masks its name"

cp -a "$TMP/archive-root" "$TMP/extra-directory-root"
mkdir "$TMP/extra-directory-root/etc"
cp -a "$TMP/bundle" "$TMP/extra-directory-bundle"
repack_bundle "$TMP/extra-directory-bundle" "$TMP/extra-directory-root"
expect_invalid_archive "$TMP/extra-directory-bundle" "an unexpected directory"

cp -a "$TMP/archive-root" "$TMP/mode-root"
chmod 0777 "$TMP/mode-root/bin"
cp -a "$TMP/bundle" "$TMP/mode-bundle"
repack_bundle "$TMP/mode-bundle" "$TMP/mode-root"
expect_invalid_archive "$TMP/mode-bundle" "unexpected archive permissions"

cp -a "$TMP/bundle" "$TMP/conflicting-owner-bundle"
repack_bundle_with_owner_names \
  "$TMP/conflicting-owner-bundle" "$TMP/archive-root" 'nobody:0' 'nogroup:0'
expect_invalid_archive "$TMP/conflicting-owner-bundle" \
  "root numeric IDs paired with non-root owner names"

cp -a "$TMP/bundle" "$TMP/numeric-owner-name-bundle"
repack_bundle_with_owner_names \
  "$TMP/numeric-owner-name-bundle" "$TMP/archive-root" '0:0' '0:0'
expect_invalid_archive "$TMP/numeric-owner-name-bundle" \
  "literal numeric owner names paired with root numeric IDs"

if RELEASE_BIN_DIR="$TMP/bin" "$ROOT/scripts/release.sh" package 01.2.3 x86_64 \
  "$TMP/invalid-version" >/dev/null 2>&1; then
  fail "packager accepted an invalid version"
fi
if RELEASE_BIN_DIR="$TMP/bin" "$ROOT/scripts/release.sh" package v1.2.3 aarch64 \
  "$TMP/invalid-arch" >/dev/null 2>&1; then
  fail "packager accepted an unvalidated release architecture"
fi

echo "test-release: PASS"
