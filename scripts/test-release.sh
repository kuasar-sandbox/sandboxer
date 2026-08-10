#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() {
  echo "test-release: $*" >&2
  exit 1
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
  1111111111111111111111111111111111111111

archive="$TMP/bundle/assets/sandboxer-v1.2.3-linux-x86_64.tar.gz"
for path in ./bin/sandbox-ctl ./bin/sandbox-init ./bin/cloud-hypervisor; do
  tar -tzf "$archive" | grep -Fx "$path" >/dev/null || fail "archive is missing $path"
done
if tar -tzf "$archive" | grep -E '^\./(docs|test/e2e)(/|$)' >/dev/null; then
  fail "component archive contains documentation or E2E sources"
fi
if tar -tzf "$archive" | grep -E '(^|/)release\.json$|(^|/)release/[^/]+\.json$' >/dev/null; then
  fail "archive contains release metadata JSON"
fi

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

cp -a "$TMP/bundle" "$TMP/extra-archive-file"
extra_archive="$TMP/extra-archive-file/assets/sandboxer-v1.2.3-linux-x86_64.tar.gz"
mkdir -p "$TMP/extra-archive-root"
tar -xzf "$extra_archive" -C "$TMP/extra-archive-root"
printf 'unexpected\n' > "$TMP/extra-archive-root/bin/unexpected"
tar -czf "$extra_archive" -C "$TMP/extra-archive-root" .
(cd "$(dirname "$extra_archive")" && sha256sum "$(basename "$extra_archive")" > SHA256SUMS)
if "$ROOT/scripts/release.sh" validate v1.2.3 x86_64 "$TMP/extra-archive-file" >/dev/null 2>&1; then
  fail "validator accepted an unexpected file inside the archive"
fi

cp -a "$TMP/extra-archive-root" "$TMP/symlink-archive-root"
rm "$TMP/symlink-archive-root/bin/unexpected" "$TMP/symlink-archive-root/bin/sandbox-init"
ln -s sandbox-ctl "$TMP/symlink-archive-root/bin/sandbox-init"
cp -a "$TMP/bundle" "$TMP/symlink-archive-file"
symlink_archive="$TMP/symlink-archive-file/assets/sandboxer-v1.2.3-linux-x86_64.tar.gz"
tar -czf "$symlink_archive" -C "$TMP/symlink-archive-root" .
(cd "$(dirname "$symlink_archive")" && sha256sum "$(basename "$symlink_archive")" > SHA256SUMS)
if "$ROOT/scripts/release.sh" validate v1.2.3 x86_64 "$TMP/symlink-archive-file" >/dev/null 2>&1; then
  fail "validator accepted a symlink in place of a release binary"
fi

if RELEASE_BIN_DIR="$TMP/bin" "$ROOT/scripts/release.sh" package 01.2.3 x86_64 \
  "$TMP/invalid-version" >/dev/null 2>&1; then
  fail "packager accepted an invalid version"
fi
if RELEASE_BIN_DIR="$TMP/bin" "$ROOT/scripts/release.sh" package v1.2.3 aarch64 \
  "$TMP/invalid-arch" >/dev/null 2>&1; then
  fail "packager accepted an unvalidated release architecture"
fi

echo "test-release: PASS"
