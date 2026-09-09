#!/usr/bin/env bash

set -euo pipefail
umask 022

NAME=sandboxer
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"
# shellcheck source=scripts/release-native-materials.sh
source "$ROOT/scripts/release-native-materials.sh"

fail() {
  echo "release: $*" >&2
  exit 1
}

validate_version() {
  [[ "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-preview\.[0-9]{8})?$ ]] \
    || fail "version must match vX.Y.Z or vX.Y.Z-preview.YYYYMMDD"
}

normalize_arch() {
  case "$1" in
    amd64|x86_64) printf 'x86_64\n' ;;
    *) fail "unsupported release architecture: $1; current release target is x86_64" ;;
  esac
}

archive_name() {
  local version="$1" arch
  validate_version "$version"
  arch="$(normalize_arch "$2")"
  printf '%s-%s-linux-%s.tar.gz\n' "$NAME" "$version" "$arch"
}

copy_file() {
  local source="$1" destination="$2"
  [ -f "$ROOT/$source" ] || fail "missing release input: $ROOT/$source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0644 "$ROOT/$source" "$STAGE/$destination"
}

copy_executable() {
  local source="$1" destination="$2"
  [ -x "$source" ] || fail "missing executable release input: $source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0755 "$source" "$STAGE/$destination"
}

copy_root_executable() {
  local source="$1" destination="$2"
  [ -x "$ROOT/$source" ] || fail "missing executable release input: $ROOT/$source"
  mkdir -p "$(dirname "$STAGE/$destination")"
  install -m 0755 "$ROOT/$source" "$STAGE/$destination"
}

check_go_binary() {
  local file="$1"
  go version -m "$file" >/dev/null 2>&1 \
    || fail "Go build info is missing from $file"
}

validate_archive_contract() {
  local archive="$1"
  go run "$ROOT/scripts/release-archive-validator.go" "$archive" \
    || fail "$archive violates the exact entry contract"
}

stage_go_source() {
  local source="$1" sha="$2" destination="$3"
  # A new checkout contains only committed inputs, including when the caller's
  # development tree has ignored .go/embed files. A real .git directory keeps
  # Go VCS stamping available; no commit or tag is created.
  git clone --quiet --no-hardlinks --no-checkout --single-branch --no-tags "$source" "$destination"
  git -C "$destination" -c advice.detachedHead=false checkout --quiet --detach "$sha"
}

prepare_cloud_hypervisor() {
  local project_sha="$1" arch="$2" native_root="$WORK/native-build/native-deps" variable
  for variable in RELEASE_CLOUD_HYPERVISOR_SOURCE_DIR CLOUD_HYPERVISOR_TARBALL \
    CLOUD_HYPERVISOR_TARBALL_SHA256; do
    [ -z "${!variable:-}" ] || fail "$variable override is not supported by release source materials"
  done
  [ "$(sed -n 's/^CLOUD_HYPERVISOR_TARBALL  *?= //p' "$ROOT/native-deps/Makefile")" = \
    'https://codeload.github.com/cloud-hypervisor/cloud-hypervisor/tar.gz/refs/tags/v51.1\#cloud-hypervisor-51.1.tar.gz' ] \
    || fail "Cloud Hypervisor source pin differs from its release source record"
  [ "$(sed -n 's/^CLOUD_HYPERVISOR_TARBALL_SHA256 *?= //p' "$ROOT/native-deps/Makefile")" = \
    a2393046c0230f6360792ed2ef1b60968aa4e04d12b6be419c86306774e2e4ef ] \
    || fail "Cloud Hypervisor source checksum differs from its release source record"
  mkdir -p "$WORK/native-build" "$WORK/cargo-home" "$WORK/rust-tmp"
  chmod 0700 "$WORK/cargo-home"
  python3 "$ROOT/scripts/release-rust-materials.py" configure-cargo-home \
    "${CARGO_HOME:-$HOME/.cargo}" "$WORK/cargo-home"
  git -C "$ROOT" archive "$project_sha" native-deps | tar -x -C "$WORK/native-build"
  CH_RELEASE_RUSTC="$(rustc --print sysroot)/bin/rustc"
  [ -x "$CH_RELEASE_RUSTC" ] || fail "selected Rust compiler is missing"
  local native_env=(
    python3 "$ROOT/scripts/release-rust-materials.py" run-native
    "$WORK/native-home" "$WORK/cargo-home" "$CH_RELEASE_RUSTC" env
    CH_BUILD_REPORT="$WORK/ch-build.jsonl" CH_LINK_MAP="$WORK/ch-link.map"
    TMPDIR="$WORK/rust-tmp"
  )
  "${native_env[@]}" make --no-print-directory -C "$native_root" \
    TARGET_ARCH="$arch" TARBALL_DIR="$ROOT/native-deps/build/tarball" ch-patches-apply ch-build
  CH_RELEASE_SOURCE="$native_root/build/src/cloud-hypervisor"
  copy_executable "$native_root/bin/$arch/cloud-hypervisor" bin/cloud-hypervisor
  "${native_env[@]}" cargo metadata --locked --format-version=1 \
    --filter-platform "$arch-unknown-linux-gnu" \
    --manifest-path "$CH_RELEASE_SOURCE/Cargo.toml" > "$WORK/ch-metadata.json"
}

validate_bundle() {
  [ "$#" -eq 3 ] || fail "usage: release.sh validate <version> <arch> <bundle-dir>"
  local version="$1" arch archive bundle="$3"
  arch="$(normalize_arch "$2")"
  archive="$(archive_name "$version" "$arch")"
  [ -s "$bundle/release-notes.md" ] || fail "release-notes.md is missing"
  [ -d "$bundle/assets" ] || fail "assets directory is missing"

  local expected="$WORK/expected-assets" actual="$WORK/actual-assets"
  printf '%s\n' "$archive" SHA256SUMS | LC_ALL=C sort > "$expected"
  find "$bundle/assets" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort > "$actual"
  cmp -s "$expected" "$actual" \
    || { diff -u "$expected" "$actual" >&2 || true; fail "bundle contains an unexpected asset set"; }
  [ "$(grep -cve '^[[:space:]]*$' "$bundle/assets/SHA256SUMS")" -eq 1 ] \
    || fail "SHA256SUMS must contain exactly one entry"
  local digest listed extra
  read -r digest listed extra < "$bundle/assets/SHA256SUMS"
  listed="${listed#\*}"
  if ! [[ "$digest" =~ ^[0-9a-f]{64}$ ]] \
    || [ "$listed" != "$archive" ] || [ -n "${extra:-}" ]; then
    fail "SHA256SUMS does not describe the expected archive"
  fi
  (cd "$bundle/assets" && sha256sum --quiet -c SHA256SUMS) \
    || fail "SHA256SUMS validation failed"

  validate_archive_contract "$bundle/assets/$archive"
  local extract="$WORK/extract"
  rm -rf "$extract"
  mkdir -p "$extract"
  tar -xzf "$bundle/assets/$archive" -C "$extract"
  release_materials_validate "$extract" "$NAME"
  release_materials_require_project_source "$extract" "$NAME" 'bin/sandbox-ctl,bin/sandbox-init' "$version" \
    bin/sandbox-ctl bin/sandbox-init
  release_materials_require_source "$extract" "$NAME" 'bin/cloud-hypervisor' 'cloud-hypervisor' "v51.1"
  release_materials_require_source "$extract" "$NAME" 'bin/cloud-hypervisor' 'cloud-hypervisor-patches' "$version"
  release_materials_require_source "$extract" "$NAME" 'bin/cloud-hypervisor' 'cloud-hypervisor-cargo-lock' "v51.1"
  release_materials_require_source "$extract" "$NAME" 'bin/cloud-hypervisor' 'Rust toolchain' ""
  awk -F '\t' '$1 == "bin/cloud-hypervisor" && $2 ~ /^rust-build-input:/ {found=1} END {exit !found}' \
    "$extract/share/sources/$NAME/SOURCES.tsv" || fail "Rust dependency materials are missing"
  release_materials_require_source "$extract" "$NAME" 'bin/sandbox-ctl' 'accelerator' ""
  release_materials_require_source "$extract" "$NAME" 'bin/sandbox-ctl' 'connector' ""
  release_materials_require_go "$extract" "$NAME" 'bin/sandbox-ctl'
  release_materials_require_go "$extract" "$NAME" 'bin/sandbox-init'
  local file
  for file in sandbox-ctl sandbox-init; do
    [ -x "$extract/bin/$file" ] || fail "$archive is missing executable bin/$file"
    check_go_binary "$extract/bin/$file"
  done
  [ -x "$extract/bin/cloud-hypervisor" ] \
    || fail "$archive is missing executable bin/cloud-hypervisor"
}

package_release() {
  [ "$#" -eq 3 ] || fail "usage: release.sh package <version> <arch> <output-dir>"
  local version="$1" arch output="$3" archive epoch bin_dir ch_source project_sha cargo_sha
  local accelerator_source connector_source accelerator_version connector_version accelerator_sha connector_sha
  arch="$(normalize_arch "$2")"
  archive="$(archive_name "$version" "$arch")"
  if [ -z "$output" ] || [ "$output" = / ] || [ "$output" = . ]; then
    fail "unsafe output directory: $output"
  fi
  [ ! -e "$output" ] || fail "output already exists: $output"
  epoch="${SOURCE_DATE_EPOCH:-0}"
  [[ "$epoch" =~ ^[0-9]+$ ]] || fail "SOURCE_DATE_EPOCH must be an integer"

  STAGE="$WORK/stage"
  rm -rf "$STAGE"
  install -d -m 0755 "$STAGE" "$STAGE/bin"
  [ -z "${RELEASE_BIN_DIR:-}" ] \
    || fail "RELEASE_BIN_DIR is not supported: release Go payloads are rebuilt from selected sources"

  accelerator_source="${RELEASE_ACCELERATOR_SOURCE_DIR:-$ROOT/../accelerator}"
  connector_source="${RELEASE_CONNECTOR_SOURCE_DIR:-$ROOT/../connector}"
  accelerator_version="${RELEASE_ACCELERATOR_VERSION:-${ACCELERATOR_VERSION:-}}"
  connector_version="${RELEASE_CONNECTOR_VERSION:-${CONNECTOR_VERSION:-}}"
  [[ "$accelerator_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-preview\.[0-9]{8})?$ ]] \
    || fail "RELEASE_ACCELERATOR_VERSION must identify the selected accelerator release"
  [[ "$connector_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-preview\.[0-9]{8})?$ ]] \
    || fail "RELEASE_CONNECTOR_VERSION must identify the selected connector release"
  project_sha="$(release_materials_resolve_git_source "$ROOT" "" sandboxer)"
  local project_version
  project_version="$(release_materials_git_version "$ROOT" "$version" "$project_sha")"
  accelerator_sha="$(release_materials_resolve_git_source "$accelerator_source" \
    "${RELEASE_ACCELERATOR_SOURCE_SHA:-}" accelerator)"
  connector_sha="$(release_materials_resolve_git_source "$connector_source" \
    "${RELEASE_CONNECTOR_SOURCE_SHA:-}" connector)"
  mkdir -p "$WORK/go-build"
  stage_go_source "$ROOT" "$project_sha" "$WORK/go-build/sandboxer"
  stage_go_source "$accelerator_source" "$accelerator_sha" "$WORK/go-build/accelerator"
  stage_go_source "$connector_source" "$connector_sha" "$WORK/go-build/connector"
  python3 "$ROOT/scripts/release-rust-materials.py" run-native \
    "$WORK/go-home" "$WORK/cargo-home" "$(rustc --print sysroot)/bin/rustc" env \
    GOWORK=off GOENV=off GOFLAGS=-mod=readonly GOCACHE="$WORK/go-cache" GOMODCACHE="$WORK/go-mod" \
    make --no-print-directory -C "$WORK/go-build/sandboxer" TARGET_ARCH="$arch" sandbox-ctl sandbox-init
  bin_dir="$WORK/go-build/sandboxer/bin/$arch"
  copy_executable "$bin_dir/sandbox-ctl" bin/sandbox-ctl
  copy_executable "$bin_dir/sandbox-init" bin/sandbox-init
  check_go_binary "$STAGE/bin/sandbox-ctl"
  check_go_binary "$STAGE/bin/sandbox-init"
  release_materials_require_go_revision "$STAGE/bin/sandbox-ctl" "$project_sha"
  release_materials_require_go_revision "$STAGE/bin/sandbox-init" "$project_sha"
  prepare_cloud_hypervisor "$project_sha" "$arch"
  ch_source="$CH_RELEASE_SOURCE"
  cargo_sha="$(sha256sum "$ch_source/Cargo.lock" | awk '{print $1}')"
  accelerator_version="$(release_materials_git_version "$accelerator_source" "$accelerator_version" "$accelerator_sha")"
  connector_version="$(release_materials_git_version "$connector_source" "$connector_version" "$connector_sha")"
  release_materials_init "$STAGE" "$WORK/materials" "$NAME"
  release_materials_copy_licenses "$ROOT" project
  release_materials_copy_licenses "$ch_source" cloud-hypervisor
  release_materials_copy_licenses "$accelerator_source" accelerator
  release_materials_copy_licenses "$connector_source" connector
  python3 "$ROOT/scripts/release-rust-materials.py" \
    --metadata "$WORK/ch-metadata.json" --build-report "$WORK/ch-build.jsonl" \
    --lock "$ch_source/Cargo.lock" --cargo-home "$WORK/cargo-home" \
    --source-root "$ch_source" --stage "$STAGE" --rustc "$CH_RELEASE_RUSTC" \
    >> "$RELEASE_MATERIALS_WORK/sources"
  release_native_link_inputs "$WORK/ch-link.map" "$WORK/native-build" "$WORK/rust-tmp" bin/cloud-hypervisor
  install -m 0644 "$ch_source/Cargo.lock" \
    "$STAGE/share/sources/$NAME/CLOUD-HYPERVISOR-Cargo.lock"
  release_materials_record_source 'bin/sandbox-ctl,bin/sandbox-init' sandboxer "$project_version" \
    "https://github.com/kuasar-sandbox/sandboxer/commit/$project_sha" \
    "git:$project_sha" project
  release_materials_record_source bin/cloud-hypervisor cloud-hypervisor v51.1 \
    'https://github.com/cloud-hypervisor/cloud-hypervisor/archive/refs/tags/v51.1.tar.gz' \
    'sha256:a2393046c0230f6360792ed2ef1b60968aa4e04d12b6be419c86306774e2e4ef' cloud-hypervisor
  release_materials_record_source bin/cloud-hypervisor cloud-hypervisor-patches "$version" \
    "https://github.com/kuasar-sandbox/sandboxer/tree/$project_sha/native-deps/deps/ch-patches" \
    "git:$project_sha" project
  release_materials_record_source bin/cloud-hypervisor cloud-hypervisor-cargo-lock v51.1 \
    "https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v51.1/Cargo.lock" \
    "sha256:$cargo_sha" cloud-hypervisor
  release_materials_record_source bin/sandbox-ctl accelerator "$accelerator_version" \
    "https://github.com/kuasar-sandbox/accelerator/commit/$accelerator_sha" \
    "git:$accelerator_sha" accelerator
  release_materials_record_source bin/sandbox-ctl connector "$connector_version" \
    "https://github.com/kuasar-sandbox/connector/commit/$connector_sha" \
    "git:$connector_sha" connector
  release_materials_add_go_binary "$STAGE/bin/sandbox-ctl" bin/sandbox-ctl
  release_materials_add_go_binary "$STAGE/bin/sandbox-init" bin/sandbox-init
  GOMODCACHE="$WORK/go-mod" release_materials_finish

  mkdir -p "$output/assets"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
    --pax-option=delete=atime,delete=ctime -czf "$output/assets/$archive" -C "$STAGE" .
  (cd "$output/assets" && sha256sum "$archive" > SHA256SUMS)
  cat > "$output/release-notes.md" <<EOF
$NAME $version for Linux $arch.

Extract the archive into a Kuasar Sandbox deployment root and verify it with \`SHA256SUMS\`. Documentation and E2E suites from this exact tag are collected by the aggregate platform release.
EOF
  validate_bundle "$version" "$arch" "$output"
  echo "==> prepared $output for $version"
}

command -v go >/dev/null || fail "go is required"
case "${1:-}" in
  archive-name) shift; [ "$#" -eq 2 ] || fail "usage: release.sh archive-name <version> <arch>"; archive_name "$@" ;;
  package) shift; package_release "$@" ;;
  validate) shift; validate_bundle "$@" ;;
  *) fail "usage: release.sh <archive-name|package|validate> ..." ;;
esac
