#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() {
  echo "test-release: $*" >&2
  exit 1
}

# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"

bash "$ROOT/scripts/test-release-materials.sh"

init_fixture_repo() {
  local directory="$1"
  shift
  git -C "$directory" init -q
  git -C "$directory" config --local user.name "Chen Xiaohui"
  git -C "$directory" config --local user.email "graych@gmail.com"
  git -C "$directory" add -- "$@"
  git -C "$directory" commit -q -m "test: create release source fixture"
  git -C "$directory" rev-parse HEAD
}

mkdir -p "$TMP/git-source" \
  "$TMP/material-hash/share/licenses/hash-test/LICENSES" \
  "$TMP/material-hash/share/sources/hash-test"
printf 'fixture license\n' > "$TMP/git-source/LICENSE"
fixture_git_sha="$(init_fixture_repo "$TMP/git-source" LICENSE)"
[ "$(release_materials_git_version "$TMP/git-source" v1.2.3 "$fixture_git_sha")" = "git:$fixture_git_sha" ] \
  || fail "untagged source was recorded as a component release"
git -C "$TMP/git-source" tag v1.2.3 "$fixture_git_sha"
[ "$(release_materials_git_version "$TMP/git-source" v1.2.3 "$fixture_git_sha")" = v1.2.3 ] \
  || fail "matching source tag was not retained"
if (release_materials_git_version "$TMP/git-source" v1.2.3 \
  0000000000000000000000000000000000000000 >/dev/null 2>&1); then
  fail "source version resolver accepted a tag for another commit"
fi
[ "$(release_materials_resolve_git_source "$TMP/git-source" "$fixture_git_sha" fixture)" = "$fixture_git_sha" ] \
  || fail "clean source worktree did not resolve to its selected commit"
if (release_materials_resolve_git_source "$TMP/git-source" \
  0000000000000000000000000000000000000000 fixture >/dev/null 2>&1); then
  fail "source resolver accepted a commit that differs from the selected commit"
fi
printf 'untracked source\n' > "$TMP/git-source/untracked.go"
if (release_materials_resolve_git_source "$TMP/git-source" "" fixture >/dev/null 2>&1); then
  fail "source resolver accepted a dirty source worktree"
fi
printf 'LICENSE.generated\n' > "$TMP/git-source/.git/info/exclude"
printf 'ignored material\n' > "$TMP/git-source/LICENSE.generated"
if (
  release_materials_init "$TMP/ignored-material/stage" "$TMP/ignored-material/work" fixture
  release_materials_copy_licenses "$TMP/git-source" project >/dev/null 2>&1
); then
  fail "license collection accepted material absent from the selected commit"
fi
printf 'nested license manifest\n' \
  > "$TMP/material-hash/share/licenses/hash-test/LICENSES/MATERIALS.sha256"
printf 'generated inventory\n' \
  > "$TMP/material-hash/share/sources/hash-test/MATERIALS.sha256"
release_materials_hash_tree "$TMP/material-hash" hash-test "$TMP/material-hash-actual"
grep -Fq 'share/licenses/hash-test/LICENSES/MATERIALS.sha256' "$TMP/material-hash-actual" \
  || fail "license file named MATERIALS.sha256 was omitted from the material inventory"
if grep -Fq 'share/sources/hash-test/MATERIALS.sha256' "$TMP/material-hash-actual"; then
  fail "generated material inventory included itself"
fi

material_root="$TMP/material-validation"
material_unit=validation
mkdir -p "$material_root/bin"
printf 'payload\n' > "$material_root/bin/tool"
mkdir -p "$material_root/share/licenses/$material_unit/project" \
  "$material_root/share/sources/$material_unit" "$TMP/material-validation-work"
printf 'fixture license\n' > "$material_root/share/licenses/$material_unit/project/LICENSE"
{
  printf 'payload\tname\tversion\tsource\tintegrity\tlicense_directory\n'
  printf 'bin/tool\tfixture\tv1.0.0\thttps://example.invalid/source.tar.gz\tsha256:fixture\tshare/licenses/%s/project\n' \
    "$material_unit"
} > "$material_root/share/sources/$material_unit/SOURCES.tsv"
printf 'payload\trecord\tname\tversion_or_value\tchecksum\n' \
  > "$material_root/share/sources/$material_unit/GO-BUILD-INFO.tsv"
printf 'module\tversion\tchecksum\n' \
  > "$material_root/share/sources/$material_unit/GO-MODULES.tsv"
release_materials_hash_tree "$material_root" "$material_unit" \
  "$material_root/share/sources/$material_unit/MATERIALS.sha256"
find "$material_root/share" -type d -exec chmod 0755 {} +
find "$material_root/share" -type f -exec chmod 0644 {} +
(
  WORK="$TMP/material-validation-work"
  release_materials_validate "$material_root" "$material_unit"
)

cp -a "$material_root" "$TMP/material-unsafe-license-path"
{
  printf 'payload\tname\tversion\tsource\tintegrity\tlicense_directory\n'
  printf 'bin/tool\tfixture\tv1.0.0\thttps://example.invalid/source.tar.gz\tsha256:fixture\t../../../etc\n'
} > "$TMP/material-unsafe-license-path/share/sources/$material_unit/SOURCES.tsv"
release_materials_hash_tree "$TMP/material-unsafe-license-path" "$material_unit" \
  "$TMP/material-unsafe-license-path/share/sources/$material_unit/MATERIALS.sha256"
chmod 0644 "$TMP/material-unsafe-license-path/share/sources/$material_unit/MATERIALS.sha256"
if (
  WORK="$TMP/material-validation-work"
  release_materials_validate "$TMP/material-unsafe-license-path" "$material_unit" \
    >/dev/null 2>&1
); then
  fail "release material validator accepted a license directory outside its unit"
fi

cp -a "$material_root" "$TMP/material-invalid-record"
{
  printf 'payload\tname\tversion\tsource\tintegrity\tlicense_directory\n'
  printf 'bin/tool\tfixture\tv1.0.0\thttps://example.invalid/source.tar.gz\tsha256:fixture\n'
} > "$TMP/material-invalid-record/share/sources/$material_unit/SOURCES.tsv"
release_materials_hash_tree "$TMP/material-invalid-record" "$material_unit" \
  "$TMP/material-invalid-record/share/sources/$material_unit/MATERIALS.sha256"
chmod 0644 "$TMP/material-invalid-record/share/sources/$material_unit/MATERIALS.sha256"
if (
  WORK="$TMP/material-validation-work"
  release_materials_validate "$TMP/material-invalid-record" "$material_unit" \
    >/dev/null 2>&1
); then
  fail "release material validator accepted a SOURCES.tsv row with fewer than six fields"
fi

cp -a "$material_root" "$TMP/material-unsafe-parent-mode"
chmod 0777 "$TMP/material-unsafe-parent-mode/share"
if (
  WORK="$TMP/material-validation-work"
  release_materials_validate "$TMP/material-unsafe-parent-mode" "$material_unit" \
    >/dev/null 2>&1
); then
  fail "release material validator accepted an unsafe parent directory mode"
fi

cp -a "$material_root" "$TMP/material-empty-license"
rm "$TMP/material-empty-license/share/licenses/$material_unit/project/LICENSE"
release_materials_hash_tree "$TMP/material-empty-license" "$material_unit" \
  "$TMP/material-empty-license/share/sources/$material_unit/MATERIALS.sha256"
if (
  WORK="$TMP/material-validation-work"
  release_materials_validate "$TMP/material-empty-license" "$material_unit" >/dev/null 2>&1
); then
  fail "release material validator accepted an empty license directory"
fi
if (
  release_materials_require_source "$material_root" "$material_unit" bin/tool fixture v2.0.0 \
    >/dev/null 2>&1
); then
  fail "release material validator accepted a different release version"
fi
if (
  release_materials_require_go "$material_root" "$material_unit" bin/tool >/dev/null 2>&1
); then
  fail "release material validator accepted missing Go build records"
fi
cp -a "$material_root" "$TMP/material-missing-payload"
rm "$TMP/material-missing-payload/bin/tool"
if (
  WORK="$TMP/material-validation-work"
  release_materials_validate "$TMP/material-missing-payload" "$material_unit" >/dev/null 2>&1
); then
  fail "release material validator accepted a record for an unshipped payload"
fi

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
grep -Fqx 'run-name: Release ${{ inputs.version }} @${{ inputs.source_sha }} [accelerator=${{ inputs.accelerator_version }},connector=${{ inputs.connector_version }}]' \
  "$WORKFLOW" || fail "release run identity does not pin source and dependencies"
grep -Fq 'RELEASE_DEPENDENCIES: accelerator=${{ needs.preflight.outputs.accelerator_version }},connector=${{ needs.preflight.outputs.connector_version }}' \
  "$WORKFLOW" || fail "Preview publisher does not receive dependency binding"
grep -Fq 'kuasar-preview-binding' "$ROOT/scripts/publish-release.sh" \
  || fail "Preview publisher does not record its build binding"
for workflow in release.yml delete-preview.yml; do
  [ "$(grep -Fc 'group: component-mutation-${{ github.repository }}-${{ inputs.version }}' \
    "$ROOT/.github/workflows/$workflow")" -eq 1 ] \
    || fail "$workflow does not hold exactly one full-workflow mutation lock"
done
grep -Fq 'kuasar-release-source' "$ROOT/scripts/publish-release.sh" \
  || fail "publisher does not record Stable source provenance"
grep -Fq 'reconcile_main_latest' "$ROOT/scripts/publish-release.sh" \
  || fail "publisher does not reconcile component main Latest by source commit"
RECONCILE_WORKFLOW="$ROOT/.github/workflows/reconcile-latest.yml"
grep -Fq 'group: component-latest-reconciliation-${{ github.repository }}' \
  "$RECONCILE_WORKFLOW" \
  || fail "Latest reconciliation is not serialized across component versions"
grep -Fq 'workflow_run:' "$RECONCILE_WORKFLOW" \
  || fail "Latest reconciliation is not triggered after release completion"
grep -Fq 'schedule:' "$RECONCILE_WORKFLOW" \
  || fail "Latest reconciliation has no automatic recovery schedule"
grep -Fq 'publish-release.sh reconcile' "$RECONCILE_WORKFLOW" \
  || fail "Latest reconciliation does not use the idempotent entrypoint"
if grep -R -Fq 'queue: max' "$ROOT/.github/workflows"; then
  fail "workflows use the unsupported concurrency queue key"
fi
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

mkdir -p "$TMP/bin" "$TMP/src" "$TMP/cloud-hypervisor/LICENSES" \
  "$TMP/accelerator" "$TMP/connector"
fixture_root="$TMP/project"
mkdir -p "$fixture_root/scripts"
install -m 0644 "$ROOT/LICENSE" "$fixture_root/LICENSE"
printf '/bin/\n/build/\n' > "$fixture_root/.gitignore"
install -m 0755 "$ROOT/scripts/release.sh" "$fixture_root/scripts/release.sh"
install -m 0755 "$ROOT/scripts/release-materials.sh" "$fixture_root/scripts/release-materials.sh"
install -m 0644 "$ROOT/scripts/release-archive-validator.go" "$fixture_root/scripts/release-archive-validator.go"
printf 'module release-fixture.invalid\n\ngo 1.24\n' > "$fixture_root/go.mod"
printf 'package main\nfunc main() {}\n' > "$fixture_root/main.go"
fixture_project_sha="$(init_fixture_repo "$fixture_root" LICENSE .gitignore scripts go.mod main.go)"
(cd "$fixture_root" && GOWORK=off go build -buildvcs=true -o "$TMP/go-fixture" .)
release_materials_require_go_revision "$TMP/go-fixture" "$fixture_project_sha"
printf '// dirty fixture\n' >> "$fixture_root/main.go"
(cd "$fixture_root" && GOWORK=off go build -buildvcs=true -o "$TMP/dirty-go-fixture" .)
if (release_materials_require_go_revision "$TMP/dirty-go-fixture" "$fixture_project_sha" >/dev/null 2>&1); then
  fail "release accepted a binary built from dirty source"
fi
printf 'package main\nfunc main() {}\n' > "$fixture_root/main.go"
if (release_materials_require_go_revision "$TMP/go-fixture" \
  0000000000000000000000000000000000000000 >/dev/null 2>&1); then
  fail "release accepted a binary built from another commit"
fi
GO111MODULE=off go build -o "$TMP/unstamped-go-fixture" "$fixture_root/main.go"
if (release_materials_require_go_revision "$TMP/unstamped-go-fixture" "$fixture_project_sha" >/dev/null 2>&1); then
  fail "release accepted a binary without source stamping"
fi
for binary in sandbox-ctl sandbox-init; do
  install -m 0755 "$TMP/go-fixture" "$TMP/bin/$binary"
done
printf '#!/bin/sh\nexit 0\n' > "$TMP/bin/cloud-hypervisor"
chmod +x "$TMP/bin/cloud-hypervisor"
printf 'fixture credits\n' > "$TMP/cloud-hypervisor/CREDITS.md"
printf 'fixture Apache license\n' > "$TMP/cloud-hypervisor/LICENSES/Apache-2.0.txt"
printf 'fixture BSD license\n' > "$TMP/cloud-hypervisor/LICENSES/BSD-3-Clause.txt"
printf 'version = 3\n' > "$TMP/cloud-hypervisor/Cargo.lock"
printf 'fixture accelerator license\n' > "$TMP/accelerator/LICENSE"
printf 'fixture connector license\n' > "$TMP/connector/LICENSE"
accelerator_sha="$(init_fixture_repo "$TMP/accelerator" LICENSE)"
connector_sha="$(init_fixture_repo "$TMP/connector" LICENSE)"

SOURCE_DATE_EPOCH=1700000000 RELEASE_BIN_DIR="$TMP/bin" \
  RELEASE_CLOUD_HYPERVISOR_SOURCE_DIR="$TMP/cloud-hypervisor" \
  RELEASE_ACCELERATOR_SOURCE_DIR="$TMP/accelerator" \
  RELEASE_ACCELERATOR_SOURCE_SHA="$accelerator_sha" \
  RELEASE_ACCELERATOR_VERSION=v0.1.3 \
  RELEASE_CONNECTOR_SOURCE_DIR="$TMP/connector" \
  RELEASE_CONNECTOR_SOURCE_SHA="$connector_sha" \
  RELEASE_CONNECTOR_VERSION=v0.1.2 \
  "$fixture_root/scripts/release.sh" package v1.2.3 x86_64 "$TMP/bundle"
"$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$TMP/bundle"
bash "$ROOT/scripts/test-publisher.sh" "$ROOT/scripts/publish-release.sh" \
  "$TMP/bundle" kuasar-sandbox/sandboxer v1.2.3 \
  1111111111111111111111111111111111111111 main
bash "$ROOT/scripts/test-publisher.sh" "$ROOT/scripts/publish-release.sh" \
  "$TMP/bundle" kuasar-sandbox/sandboxer v1.2.3 \
  1111111111111111111111111111111111111111 release/v1.2.x

archive="$TMP/bundle/assets/$ARCHIVE_NAME"
go_toolchain="$(go version | awk '{print $3}')"
for path in ./bin/cloud-hypervisor ./bin/sandbox-ctl ./bin/sandbox-init \
  ./share/licenses/sandboxer/project/LICENSE \
  ./share/licenses/sandboxer/cloud-hypervisor/CREDITS.md \
  ./share/licenses/sandboxer/cloud-hypervisor/LICENSES/Apache-2.0.txt \
  ./share/licenses/sandboxer/cloud-hypervisor/LICENSES/BSD-3-Clause.txt \
  ./share/licenses/sandboxer/accelerator/LICENSE \
  ./share/licenses/sandboxer/connector/LICENSE \
  ./share/licenses/sandboxer/go-toolchain/"$go_toolchain"/LICENSE \
  ./share/sources/sandboxer/CLOUD-HYPERVISOR-Cargo.lock \
  ./share/sources/sandboxer/SOURCES.tsv \
  ./share/sources/sandboxer/GO-BUILD-INFO.tsv \
  ./share/sources/sandboxer/GO-MODULES.tsv \
  ./share/sources/sandboxer/MATERIALS.sha256; do
  tar -tzf "$archive" | grep -Fx "$path" >/dev/null \
    || fail "archive is missing $path"
done
tar -xOf "$archive" ./share/sources/sandboxer/SOURCES.tsv \
  | grep -Fq $'\tGo toolchain\t'"$go_toolchain"$'\t' \
  || fail "archive does not associate its Go toolchain with license material"

SOURCE_DATE_EPOCH=1700000000 RELEASE_BIN_DIR="$TMP/bin" \
  RELEASE_CLOUD_HYPERVISOR_SOURCE_DIR="$TMP/cloud-hypervisor" \
  RELEASE_ACCELERATOR_SOURCE_DIR="$TMP/accelerator" \
  RELEASE_ACCELERATOR_SOURCE_SHA="$accelerator_sha" \
  RELEASE_ACCELERATOR_VERSION=v0.1.3 \
  RELEASE_CONNECTOR_SOURCE_DIR="$TMP/connector" \
  RELEASE_CONNECTOR_SOURCE_SHA="$connector_sha" \
  RELEASE_CONNECTOR_VERSION=v0.1.2 \
  "$fixture_root/scripts/release.sh" package v1.2.3 x86_64 "$TMP/reproducible"
cmp -s "$archive" "$TMP/reproducible/assets/sandboxer-v1.2.3-linux-x86_64.tar.gz" \
  || fail "identical inputs did not produce an identical archive"

cp -a "$TMP/bundle" "$TMP/tampered"
printf 'tampered\n' >> "$TMP/tampered/assets/sandboxer-v1.2.3-linux-x86_64.tar.gz"
if "$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$TMP/tampered" >/dev/null 2>&1; then
  fail "validator accepted a tampered archive"
fi

cp -a "$TMP/bundle" "$TMP/extra"
touch "$TMP/extra/assets/release.json"
if "$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$TMP/extra" >/dev/null 2>&1; then
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

cp -a "$TMP/archive-root" "$TMP/foreign-material-root"
mkdir -p "$TMP/foreign-material-root/share/licenses/another-unit"
printf 'foreign material\n' > "$TMP/foreign-material-root/share/licenses/another-unit/LICENSE"
cp -a "$TMP/bundle" "$TMP/foreign-material-bundle"
repack_bundle "$TMP/foreign-material-bundle" "$TMP/foreign-material-root"
expect_invalid_archive "$TMP/foreign-material-bundle" "another release unit's material namespace"

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

if RELEASE_BIN_DIR="$TMP/bin" "$fixture_root/scripts/release.sh" package 01.2.3 x86_64 \
  "$TMP/invalid-version" >/dev/null 2>&1; then
  fail "packager accepted an invalid version"
fi
if RELEASE_BIN_DIR="$TMP/bin" "$fixture_root/scripts/release.sh" package v1.2.3 aarch64 \
  "$TMP/invalid-arch" >/dev/null 2>&1; then
  fail "packager accepted an unvalidated release architecture"
fi

echo "test-release: PASS"
