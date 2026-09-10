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

export FIXTURE_GO_DISTRIBUTION_CACHE
FIXTURE_GO_DISTRIBUTION_CACHE="$(go env GOMODCACHE)"
bash "$ROOT/scripts/test-release-materials.sh"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-go-environment.py"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-validator-environment.py"
bash "$ROOT/scripts/test-release-license-traversal.sh"
GOWORK=off go test -race "$ROOT/scripts/release-go-toolchain.go" "$ROOT/scripts/release-go-toolchain_test.go"
bash "$ROOT/native-deps/deps/test-common.sh"
PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/scripts/test-release-rust-materials.py"
bash "$ROOT/scripts/test-release-native-materials.sh"
bash "$ROOT/scripts/test-release-rpm-enumeration.sh"

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
bounded_workflow=release.yml
awk '
  $0 == "  publish:" { inside=1; next }
  inside && /^  [A-Za-z0-9_-]+:/ { exit }
  inside && /^    steps:/ { exit }
  inside { print }
' "$ROOT/.github/workflows/$bounded_workflow" | grep -Fx '    timeout-minutes: 30' >/dev/null \
  || fail "$bounded_workflow does not bound privileged publication work"

WORKFLOW="$ROOT/.github/workflows/release.yml"
grep -Fqx 'run-name: Release ${{ inputs.version }} @${{ inputs.source_sha }} [accelerator=${{ inputs.accelerator_version }},connector=${{ inputs.connector_version }}]' \
  "$WORKFLOW" || fail "release run identity does not pin source and dependencies"
grep -Fq 'RELEASE_DEPENDENCIES: accelerator=${{ needs.preflight.outputs.accelerator_version }},connector=${{ needs.preflight.outputs.connector_version }}' \
  "$WORKFLOW" || fail "Preview publisher does not receive dependency binding"
workflow="$ROOT/.github/workflows/release.yml"
for job in build publish; do
  for routing in 'GOPROXY: https://goproxy.cn' 'GOSUMDB: sum.golang.google.cn' 'GOTOOLCHAIN: local'; do
    awk -v job="$job" '
      $0 == "  " job ":" { inside=1; next }
      inside && /^  [A-Za-z0-9_-]+:/ { exit }
      inside && /^    steps:/ { exit }
      inside { print }
    ' "$workflow" | grep -Fx "      $routing" >/dev/null \
      || fail "$workflow $job is missing the verified Go routing policy: $routing"
  done
done
[ "$(grep -Fc 'archive_sha256: ${{ steps.release-archive-digest.outputs.archive_sha256 }}' \
  "$workflow")" -eq 1 ] \
  || fail "$workflow does not expose exactly one independent build archive digest"
[ "$(grep -Fc 'RELEASE_ARCHIVE_SHA256: ${{ needs.build.outputs.archive_sha256 }}' \
  "$workflow")" -eq 1 ] \
  || fail "$workflow does not pass the independent build digest to publication"
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
  if "$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$bundle" >/dev/null 2>&1; then
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
mkdir -p "$fixture_root/scripts" "$fixture_root/native-deps" "$fixture_root/LICENSES"
install -m 0644 "$ROOT/LICENSE" "$fixture_root/LICENSE"
printf 'fixture nested project notice\n' > "$fixture_root/LICENSES/NOTICE.txt"
printf 'fixture project attribution\n' > "$fixture_root/NOTICE"
printf '/bin/\n/build/\n' > "$fixture_root/.gitignore"
install -m 0755 "$ROOT/scripts/release.sh" "$fixture_root/scripts/release.sh"
install -m 0755 "$ROOT/scripts/publish-release.sh" "$fixture_root/scripts/publish-release.sh"
install -m 0755 "$ROOT/scripts/release-materials.sh" "$fixture_root/scripts/release-materials.sh"
install -m 0644 "$ROOT/scripts/release-go-toolchain.go" "$fixture_root/scripts/release-go-toolchain.go"
cat >> "$fixture_root/scripts/release-materials.sh" <<'EOF'
release_materials_download_go_toolchain() {
  # Seed only public distribution cache files, never HOME/netrc/VCS/auth state.
  # The real filtered downloader still checks sumdb; the ZIP verifier checks h1.
  local cached="${FIXTURE_GO_DISTRIBUTION_CACHE:?}/cache/download/golang.org/toolchain/@v"
  local destination="${WORK:-$RELEASE_MATERIALS_WORK}/toolchain-download/module-cache/cache/download/golang.org/toolchain/@v"
  local suffix identity="v0.0.1-$1.linux-amd64"
  mkdir -p "$destination"
  for suffix in zip ziphash info mod; do
    [ ! -f "$cached/$identity.$suffix" ] || cp --reflink=auto "$cached/$identity.$suffix" "$destination/"
  done
  _release_materials_download_go_toolchain "$@"
}
# This synthetic crate graph has its own independently selected fixture lock;
# the production binding is verified separately against the pinned upstream tar.
release_materials_cloud_hypervisor_lock_sha() {
  sha256sum "$(dirname "$ROOT")/cloud-hypervisor/Cargo.lock" | awk '{print $1}'
}
EOF
install -m 0644 "$ROOT/scripts/release-archive-validator.go" "$fixture_root/scripts/release-archive-validator.go"
install -m 0644 "$ROOT/scripts/release-rust-materials.py" "$fixture_root/scripts/release-rust-materials.py"
install -m 0644 "$ROOT/scripts/test-rust-distribution-fixture.py" "$fixture_root/scripts/test-rust-distribution-fixture.py"
# Only this synthetic packaging checkout supplies fixture TLS response bytes.
# The actual manifest/commit/hash/notices checks still execute without a network
# bypass option in production; independent helper tests mutate each binding.
python3 - "$fixture_root/scripts/release-rust-materials.py" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
source = path.read_text()
marker = 'if __name__ == "__main__":\n'
assert source.count(marker) == 1
source = source.replace(marker, marker +
    '    exec(compile(Path(__file__).with_name("test-rust-distribution-fixture.py").read_text(), "fixture-transport", "exec"), globals())\n'
    '    urlopen = fixture_urlopen\n')
path.write_text(source)
PY
install -m 0644 "$ROOT/scripts/release-native-materials.sh" "$fixture_root/scripts/release-native-materials.sh"
install -m 0644 "$ROOT/native-deps/Makefile" "$fixture_root/native-deps/Makefile"
printf 'module release-fixture.invalid\n\ngo 1.24\n' > "$fixture_root/go.mod"
printf 'package main\nfunc main() {}\n' > "$fixture_root/main.go"
fixture_project_sha="$(init_fixture_repo "$fixture_root" LICENSE LICENSES NOTICE .gitignore scripts native-deps/Makefile go.mod main.go)"
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
git -C "$TMP/accelerator" tag v0.1.3 "$accelerator_sha"
git -C "$TMP/connector" tag v0.1.2 "$connector_sha"

mkdir -p "$TMP/release-build-bin" "$TMP/crate/fixture-1.0.0" "$TMP/rust/share/doc/rust/licenses"
printf 'fixture Rust crate license\n' > "$TMP/crate/fixture-1.0.0/LICENSE"
tar -czf "$TMP/fixture-1.0.0.crate" -C "$TMP/crate" fixture-1.0.0
printf 'version = 3\n[[package]]\nname = "fixture"\nversion = "1.0.0"\nsource = "registry+https://github.com/rust-lang/crates.io-index"\nchecksum = "%s"\n' \
  "$(sha256sum "$TMP/fixture-1.0.0.crate" | awk '{print $1}')" > "$TMP/cloud-hypervisor/Cargo.lock"
printf 'fixture Rust standard library copyright\n' > "$TMP/rust/share/doc/rust/COPYRIGHT-library.html"
printf 'fixture Rust toolchain license\n' > "$TMP/rust/share/doc/rust/licenses/Apache-2.0.txt"
cat > "$TMP/release-build-bin/make" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
fixture_root="$(cd "$(dirname "$0")/.." && pwd)"
RELEASE_TEST_CH_SOURCE="$fixture_root/cloud-hypervisor"
RELEASE_TEST_CRATE="$fixture_root/fixture-1.0.0.crate"
[ -z "${CARGO_REGISTRIES_CRATES_IO_TOKEN:-}" ]
[ -z "${GH_TOKEN:-}" ]
while [ "$#" -gt 0 ] && [ "$1" != -C ]; do shift; done
[ "$1" = -C ]
root="$2"
if [[ "$root" == */go-build/sandboxer ]]; then
  [ "$GOWORK" = off ] && [ "$GOFLAGS" = -mod=readonly ]
  [ "$GOSUMDB" = sum.golang.google.cn ] && [ "$GOTOOLCHAIN" = local ]
  [ ! -e "$root/ignored-release-input.go" ]
  [ ! -e "$root/../accelerator/ignored-release-input.go" ]
  [ ! -e "$root/../connector/ignored-release-input.go" ]
  for component in sandboxer accelerator connector; do
    [ "$(git -C "$root/../$component" rev-parse HEAD)" = \
      "$(git -C "$fixture_root/${component/sandboxer/project}" rev-parse HEAD)" ]
  done
  mkdir -p "$root/bin/x86_64"
  install -m 0755 "$fixture_root/go-fixture" "$root/bin/x86_64/sandbox-ctl"
  install -m 0755 "$fixture_root/go-fixture" "$root/bin/x86_64/sandbox-init"
  exit 0
fi
"$RUSTC" -vV >/dev/null
[[ "$root" == */native-build/native-deps ]]
source="$root/build/src/cloud-hypervisor"
[ ! -e "$source" ]
mkdir -p "$source/LICENSES" "$root/bin/x86_64" "$CARGO_HOME/registry/cache/fixture"
cp "$RELEASE_TEST_CH_SOURCE/CREDITS.md" "$source/"
cp "$RELEASE_TEST_CH_SOURCE/LICENSES/"* "$source/LICENSES/"
cp "$RELEASE_TEST_CRATE" "$CARGO_HOME/registry/cache/fixture/fixture-1.0.0.crate"
printf '[workspace]\n' > "$source/Cargo.toml"
cp "$RELEASE_TEST_CH_SOURCE/Cargo.lock" "$source/Cargo.lock"
printf '#!/bin/sh\n# fresh native fixture\nexit 0\n' > "$root/bin/x86_64/cloud-hypervisor"
chmod 0755 "$root/bin/x86_64/cloud-hypervisor"
printf '%s\n' \
  '{"reason":"compiler-artifact","package_id":"registry+https://github.com/rust-lang/crates.io-index#fixture@1.0.0","target":{"name":"cloud-hypervisor"},"executable":"/fixture/cloud-hypervisor"}' \
  '{"reason":"build-finished","success":true}' > "$CH_BUILD_REPORT"
printf 'LOAD %s\n' "$fixture_root/system/fixture.o" \
  "$fixture_root/rust/lib/rustlib/x86_64-unknown-linux-gnu/lib/libstd-fixture.rlib" > "$CH_LINK_MAP"
EOF
cat > "$TMP/release-build-bin/cargo" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = metadata ]
printf '%s\n' '{"packages":[{"name":"fixture","version":"1.0.0","id":"registry+https://github.com/rust-lang/crates.io-index#fixture@1.0.0","source":"registry+https://github.com/rust-lang/crates.io-index","manifest_path":"/fixture/fixture-1.0.0/Cargo.toml","license_file":null}]}'
EOF
cat > "$TMP/release-build-bin/rustc" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = -vV ]; then
  printf 'rustc 1.0.0\nrelease: 1.0.0\ncommit-hash: 3333333333333333333333333333333333333333\nhost: x86_64-unknown-linux-gnu\n'
elif [ "$1" = --print ] && [ "$2" = sysroot ]; then
  cd "$(dirname "$(readlink -f "$0")")/.." && pwd
else exit 1; fi
EOF
mkdir -p "$TMP/rust/bin" "$TMP/system"
mkdir -p "$TMP/rust/lib/rustlib/x86_64-unknown-linux-gnu/lib"
printf 'fixture linked Rust standard library\n' > "$TMP/rust/lib/rustlib/x86_64-unknown-linux-gnu/lib/libstd-fixture.rlib"
install -m 0755 "$TMP/release-build-bin/rustc" "$TMP/rust/bin/rustc"
printf 'fixture static object\n' > "$TMP/system/fixture.o"
printf 'fixture system license\n' > "$TMP/system/LICENSE"
cat > "$TMP/release-build-bin/dpkg-query" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
cat > "$TMP/release-build-bin/rpm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  -qf)
    if [ "$2" = --dump ]; then
      printf '%s 1 0 %s 0100644 root root 0 0 0 X\n' "$3" "$(sha256sum "$3" | awk '{print $1}')"
    elif [ "$3" = '%{SOURCERPM}\n' ]; then
      printf 'fixture-native-1.0-1.src.rpm\n'
    else
      printf 'fixture-native\t1.0-1\tfixture-native-1.0-1.src.rpm\n'
    fi
    ;;
  -qa) printf 'fixture-native.x86_64\tfixture-native-1.0-1.src.rpm\n' ;;
  -ql) printf '%s/system/LICENSE\n' "$(cd "$(dirname "$0")/.." && pwd)" ;;
  *) exit 1 ;;
esac
EOF
rm "$TMP/release-build-bin/rustc"
ln -s ../rust/bin/rustc "$TMP/release-build-bin/rustc"
chmod 0755 "$TMP/release-build-bin/make" "$TMP/release-build-bin/cargo" "$TMP/release-build-bin/rustc"
chmod 0755 "$TMP/release-build-bin/dpkg-query" "$TMP/release-build-bin/rpm"
native_fixture_env=(
  PATH="$TMP/release-build-bin:$PATH"
  GOSUMDB=sum.golang.google.cn
  GOTOOLCHAIN=local
  CARGO_REGISTRIES_CRATES_IO_TOKEN=fixture-must-not-reach-build
  GH_TOKEN=fixture-must-not-reach-build
)
for source in "$fixture_root" "$TMP/accelerator" "$TMP/connector"; do
  printf 'ignored-release-input.go\n' >> "$source/.git/info/exclude"
  printf 'this ignored file must not enter a release build\n' > "$source/ignored-release-input.go"
done
if RELEASE_BIN_DIR="$TMP/bin" "$fixture_root/scripts/release.sh" package v1.2.3 x86_64 \
  "$TMP/prebuilt-override" > "$TMP/prebuilt-override.log" 2>&1; then
  fail "packager accepted a prebuilt Go payload override"
fi
grep -Fq 'RELEASE_BIN_DIR is not supported' "$TMP/prebuilt-override.log" \
  || fail "prebuilt Go payload override failed for an unrelated reason"
[ ! -e "$TMP/prebuilt-override" ] || fail "rejected prebuilt override created an output bundle"
env "${native_fixture_env[@]}" SOURCE_DATE_EPOCH=1700000000 \
  RELEASE_ACCELERATOR_SOURCE_DIR="$TMP/accelerator" \
  RELEASE_ACCELERATOR_SOURCE_SHA="$accelerator_sha" \
  RELEASE_ACCELERATOR_VERSION=v0.1.3 \
  RELEASE_CONNECTOR_SOURCE_DIR="$TMP/connector" \
  RELEASE_CONNECTOR_SOURCE_SHA="$connector_sha" \
  RELEASE_CONNECTOR_VERSION=v0.1.2 \
  "$fixture_root/scripts/release.sh" package v1.2.3 x86_64 "$TMP/bundle"
"$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$TMP/bundle"
RELEASE_DEPENDENCIES=accelerator=v0.1.3,connector=v0.1.2 \
  "$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$TMP/bundle"
for binding in accelerator=v9.0.0,connector=v0.1.2 accelerator=v0.1.3,connector=v9.0.0 \
  accelerator=v0.1.3 accelerator=v0.1.3,accelerator=v0.1.3 \
  accelerator=v0.1.3,unexpected=v0.1.2 'accelerator=v0.1.3,connector=v0.1.2,'; do
  if RELEASE_DEPENDENCIES="$binding" "$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 \
    "$TMP/bundle" > "$TMP/dependency-binding.log" 2>&1; then
    fail "validator accepted a conflicting or malformed dependency release request"
  fi
  grep -Eq 'source record|release binding|dependency|dependencies' "$TMP/dependency-binding.log" \
    || fail "dependency binding was rejected for an unrelated reason"
done
bash "$ROOT/scripts/test-publisher.sh" "$fixture_root/scripts/publish-release.sh" \
  "$TMP/bundle" kuasar-sandbox/sandboxer v1.2.3 \
  "$fixture_project_sha" main
bash "$ROOT/scripts/test-publisher.sh" "$fixture_root/scripts/publish-release.sh" \
  "$TMP/bundle" kuasar-sandbox/sandboxer v1.2.3 \
  "$fixture_project_sha" release/v1.2.x

archive="$TMP/bundle/assets/$ARCHIVE_NAME"
for source_name in cloud-hypervisor cloud-hypervisor-patches cloud-hypervisor-cargo-lock; do
  for column in 4 5; do
    candidate="$TMP/ch-source-$source_name-$column"
    cp -a "$TMP/bundle" "$candidate"
    mkdir "$candidate/root"
    tar -xzf "$archive" -C "$candidate/root"
    source_table="$candidate/root/share/sources/sandboxer/SOURCES.tsv"
    awk -F '\t' -v OFS='\t' -v name="$source_name" -v column="$column" '
      NR > 1 && $2 == name { $column="not-the-selected-native-source" }
      { print }
    ' "$source_table" > "$candidate/sources.changed"
    install -m 0644 "$candidate/sources.changed" "$source_table"
    release_materials_hash_tree "$candidate/root" sandboxer \
      "$candidate/root/share/sources/sandboxer/MATERIALS.sha256"
    repack_bundle "$candidate" "$candidate/root"
    if "$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
      fail "validator accepted altered $source_name source column $column"
    fi
    grep -Fq "missing or inconsistent source record for $source_name" "$candidate/result.log" \
      || fail "native source mutation failed for an unrelated reason"
  done
done
candidate="$TMP/ch-lock-bytes"
cp -a "$TMP/bundle" "$candidate"
mkdir "$candidate/root"
tar -xzf "$archive" -C "$candidate/root"
printf 'altered lock bytes\n' >> "$candidate/root/share/sources/sandboxer/CLOUD-HYPERVISOR-Cargo.lock"
release_materials_hash_tree "$candidate/root" sandboxer \
  "$candidate/root/share/sources/sandboxer/MATERIALS.sha256"
repack_bundle "$candidate" "$candidate/root"
if "$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
  fail "validator accepted changed Cargo.lock bytes with regenerated checksums"
fi
grep -Fq 'Cloud Hypervisor Cargo.lock differs from the pinned source' "$candidate/result.log" \
  || fail "Cargo.lock byte mutation failed for an unrelated reason"
for mutation in top-level nested missing extra; do
  candidate="$TMP/project-license-$mutation"
  cp -a "$TMP/bundle" "$candidate"
  mkdir "$candidate/root"
  tar -xzf "$archive" -C "$candidate/root"
  license_root="$candidate/root/share/licenses/sandboxer/project"
  case "$mutation" in
    top-level) printf 'altered project license\n' > "$license_root/LICENSE" ;;
    nested) printf 'altered nested license\n' > "$license_root/LICENSES/NOTICE.txt" ;;
    missing) rm "$license_root/NOTICE" ;;
    extra) printf 'extra unauthenticated notice\n' > "$license_root/NOTICE.extra" ;;
  esac
  release_materials_hash_tree "$candidate/root" sandboxer \
    "$candidate/root/share/sources/sandboxer/MATERIALS.sha256"
  repack_bundle "$candidate" "$candidate/root"
  if "$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$candidate" > "$candidate/result.log" 2>&1; then
    fail "validator accepted $mutation project-license mutation with regenerated checksums"
  fi
  grep -Fq 'license bytes differ from selected Git source: project' "$candidate/result.log" \
    || fail "project-license mutation failed for an unrelated reason"
done
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
  ./share/sources/sandboxer/RUST-STDLIB.tsv \
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

env "${native_fixture_env[@]}" SOURCE_DATE_EPOCH=1700000000 \
  RELEASE_ACCELERATOR_SOURCE_DIR="$TMP/accelerator" \
  RELEASE_ACCELERATOR_SOURCE_SHA="$accelerator_sha" \
  RELEASE_ACCELERATOR_VERSION=v0.1.3 \
  RELEASE_CONNECTOR_SOURCE_DIR="$TMP/connector" \
  RELEASE_CONNECTOR_SOURCE_SHA="$connector_sha" \
  RELEASE_CONNECTOR_VERSION=v0.1.2 \
  "$fixture_root/scripts/release.sh" package v1.2.3 x86_64 "$TMP/reproducible"
cmp -s "$archive" "$TMP/reproducible/assets/sandboxer-v1.2.3-linux-x86_64.tar.gz" \
  || fail "identical inputs did not produce an identical archive"

# The archive name is the requested release target; an untagged source record
# identifies the actual commit and does not pretend that target tag exists.
tar -xOf "$archive" ./share/sources/sandboxer/SOURCES.tsv | \
  awk -F '\t' -v sha="$fixture_project_sha" \
    '$2 == "sandboxer" && $3 == "git:" sha {found=1} END {exit !found}' \
  || fail "pre-tag project source was recorded as an existing release"
for column in 3 4 5; do
  candidate="$TMP/project-source-$column"
  cp -a "$TMP/bundle" "$candidate"
  mkdir "$candidate/root"
  tar -xzf "$archive" -C "$candidate/root"
  inventory="$candidate/root/share/sources/sandboxer/SOURCES.tsv"
  awk -F '\t' -v OFS='\t' -v column="$column" \
    '$2 == "sandboxer" {$column="not-the-selected-source"} {print}' \
    "$inventory" > "$candidate/changed.tsv"
  mv "$candidate/changed.tsv" "$inventory"
  release_materials_hash_tree "$candidate/root" sandboxer \
    "$candidate/root/share/sources/sandboxer/MATERIALS.sha256"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
    -czf "$candidate/assets/$(basename "$archive")" -C "$candidate/root" .
  (cd "$candidate/assets" && sha256sum "$(basename "$archive")" > SHA256SUMS)
  if "$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$candidate" >/dev/null 2>&1; then
    fail "validator accepted project provenance column $column with regenerated checksums"
  fi
done

candidate="$TMP/changed-rust-stdlib-inventory"
cp -a "$TMP/bundle" "$candidate"
mkdir "$candidate/root"
tar -xzf "$archive" -C "$candidate/root"
printf 'altered inventory\n' >> "$candidate/root/share/sources/sandboxer/RUST-STDLIB.tsv"
release_materials_hash_tree "$candidate/root" sandboxer \
  "$candidate/root/share/sources/sandboxer/MATERIALS.sha256"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1700000000 \
  -czf "$candidate/assets/$(basename "$archive")" -C "$candidate/root" .
(cd "$candidate/assets" && sha256sum "$(basename "$archive")" > SHA256SUMS)
if "$fixture_root/scripts/release.sh" validate v1.2.3 x86_64 "$candidate" > "$candidate/rejection.log" 2>&1; then
  fail "validator accepted changed Rust stdlib inventory with regenerated material checksums"
fi
grep -Fq 'Rust standard-library inventory is not bound' "$candidate/rejection.log" \
  || fail "Rust stdlib inventory was rejected for an unrelated reason"

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

if "$fixture_root/scripts/release.sh" package 01.2.3 x86_64 \
  "$TMP/invalid-version" >/dev/null 2>&1; then
  fail "packager accepted an invalid version"
fi
if "$fixture_root/scripts/release.sh" package v1.2.3 aarch64 \
  "$TMP/invalid-arch" >/dev/null 2>&1; then
  fail "packager accepted an unvalidated release architecture"
fi

echo "test-release: PASS"
