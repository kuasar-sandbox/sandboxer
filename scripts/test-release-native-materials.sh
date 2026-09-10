#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT
fail() { echo "test-native-materials: $*" >&2; exit 1; }
# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"
# shellcheck source=scripts/release-native-materials.sh
source "$ROOT/scripts/release-native-materials.sh"

# Keep fixture package ownership/digests separate from mutable input bytes.
package_input="$test_root/package-input.a"
printf 'original package payload\n' > "$package_input"
deb_digest="$(md5sum "$package_input" | awk '{print $1}')"
rpm_digest="$(sha256sum "$package_input" | awk '{print $1}')"
package_mutation=none
dpkg-query() {
  case "$1" in
    -S) printf 'fixture:amd64: %s\n' "$package_input"; return ;;
    --control-show) [ "$2" = fixture:amd64 ] && [ "$3" = md5sums ] || return 1 ;;
    *) return 1 ;;
  esac
  case "$package_mutation" in
    unavailable) return 1 ;;
    missing) printf '%s  other-file\n' "$deb_digest" ;;
    duplicate) printf '%s  %s\n' "$deb_digest" "${package_input#/}" "$deb_digest" "${package_input#/}" ;;
    *) printf '%s  %s\n' "$deb_digest" "${package_input#/}" ;;
  esac
}
rpm() {
  [ "$1" = -qf ] && [ "$2" = --dump ] && [ "$3" = "$package_input" ] || return 1
  case "$package_mutation" in
    unavailable) return 1 ;;
    missing) printf '/other-file 1 0 %s 0100644 root root 0 0 0 X\n' "$rpm_digest" ;;
    duplicate) printf '%s 1 0 %s 0100644 root root 0 0 0 X\n' "$package_input" "$rpm_digest" "$package_input" "$rpm_digest" ;;
    *) printf '%s 1 0 %s 0100644 root root 0 0 0 X\n' "$package_input" "$rpm_digest" ;;
  esac
}
for package_format in deb rpm; do
  release_native_verify_package_file "$package_input" "$package_format" fixture:amd64
  for package_mutation in missing duplicate unavailable; do
    if (release_native_verify_package_file "$package_input" "$package_format" fixture:amd64 \
        > "$test_root/package-rejection.log" 2>&1); then
      fail "accepted $package_format $package_mutation package metadata"
    fi
    grep -Eq 'file digest|file digests' "$test_root/package-rejection.log" \
      || fail "package metadata was rejected for an unrelated reason"
  done
  package_mutation=none
done
printf 'locally replaced package payload\n' > "$package_input"
for package_format in deb rpm; do
  if (release_native_verify_package_file "$package_input" "$package_format" fixture:amd64 \
      > "$test_root/package-rejection.log" 2>&1); then
    fail "accepted altered $package_format package payload"
  fi
  grep -Fq 'content differs from installed metadata' "$test_root/package-rejection.log" \
    || fail "altered package payload was rejected for an unrelated reason"
done
# The production collector must verify bytes before relying on source/license
# ownership. This still-owned Debian input has been replaced since installation.
if (release_native_system_input "$package_input" bin/fixture \
    > "$test_root/package-rejection.log" 2>&1); then
  fail "native material collection accepted a replaced Debian input"
fi
grep -Fq 'content differs from installed metadata' "$test_root/package-rejection.log" \
  || fail "material collection did not verify the installed input bytes"
unset -f dpkg-query rpm
printf 'test-native-materials: installed package byte verification PASS\n'

# License bytes and their source package are checked independently of the linked
# library. Both package databases are private fixtures, never host modifications.
mkdir -p "$test_root/license-package"
license_file="$test_root/license-package/LICENSE"
license_library="$test_root/license-package/fixture-native.a"
printf 'original fixture copyright\n' > "$license_file"
printf 'fixture linked library\n' > "$license_library"
license_md5="$(md5sum "$license_file" | awk '{print $1}')"
license_sha="$(sha256sum "$license_file" | awk '{print $1}')"
library_sha="$(sha256sum "$license_library" | awk '{print $1}')"
license_mutation=none
dpkg-query() {
  case "$1" in
    -S)
      [ "$2" = "$license_file" ] || return 1
      case "$license_mutation" in
        coowned*) printf 'fixture:amd64, fixture:i386: %s\n' "$license_file" ;;
        *) printf 'fixture:amd64: %s\n' "$license_file" ;;
      esac ;;
    --control-show)
      [ "$license_mutation" != unavailable ] || return 1
      if [ "$license_mutation" = missing ]; then
        printf '%s  unrelated\n' "$license_md5"
      elif [ "$license_mutation" = coowned-conflict ] && [ "$2" = fixture:i386 ]; then
        printf '%032d  %s\n' 0 "${license_file#/}"
      else
        printf '%s  %s\n' "$license_md5" "${license_file#/}"
      fi ;;
    -W)
      if [ "$license_mutation" = wrong-source ] \
        || { [ "$license_mutation" = coowned-source ] && [ "$4" = fixture:i386 ]; }; then
        printf 'unrelated\t2.0\n'
      else printf 'fixture-source\t1.0\n'; fi ;;
    *) return 1 ;;
  esac
}
rpm() {
  case "$1" in
    -qf)
      if [ "$2" = --dump ]; then
        [ "$license_mutation" != unavailable ] || return 1
        local file="$3" digest="$license_sha"
        [ "$file" != "$license_library" ] || digest="$library_sha"
        [ "$license_mutation" != missing ] || file=/unrelated
        printf '%s 1 0 %s 0100644 root root 0 0 0 X\n' "$file" "$digest"
      elif [ "$3" = '%{SOURCERPM}\n' ]; then
        if [ "$license_mutation" = wrong-source ]; then printf 'unrelated-2.0.src.rpm\n'
        else printf 'fixture-source-1.0.src.rpm\n'; fi
      else
        printf 'fixture-native\t1.0\tfixture-source-1.0.src.rpm\n'
      fi ;;
    -qa) printf 'fixture-native.x86_64\tfixture-source-1.0.src.rpm\n' ;;
    -ql) printf '%s\n' "$license_file" ;;
    *) return 1 ;;
  esac
}
for package_format in deb rpm; do
  expected_source=deb-source:fixture-source@1.0
  [ "$package_format" != rpm ] || expected_source=rpm-source:fixture-source-1.0.src.rpm
  release_native_verify_license_file "$license_file" "$package_format" "$expected_source"
  for license_mutation in missing unavailable wrong-source changed; do
    if [ "$license_mutation" = changed ]; then printf 'altered license\n' > "$license_file"; fi
    if (release_native_verify_license_file "$license_file" "$package_format" "$expected_source" \
        > "$test_root/license-rejection.log" 2>&1); then
      fail "accepted $package_format $license_mutation license material"
    fi
    grep -Eq 'file digest|metadata|different source package' "$test_root/license-rejection.log" \
      || fail "license material was rejected for an unrelated reason"
    printf 'original fixture copyright\n' > "$license_file"
  done
  license_mutation=none
done
license_mutation=coowned
release_native_verify_license_file "$license_file" deb deb-source:fixture-source@1.0
for license_mutation in coowned-conflict coowned-source; do
  if (release_native_verify_license_file "$license_file" deb deb-source:fixture-source@1.0 \
      > "$test_root/license-rejection.log" 2>&1); then
    fail "accepted conflicting Multi-Arch license ownership"
  fi
  grep -Eq 'metadata|different source package' "$test_root/license-rejection.log" \
    || fail "conflicting license co-ownership failed for an unrelated reason"
done
license_mutation=none
release_materials_init "$test_root/license-stage" "$test_root/license-work" fixture
# The production function is sourced above; a dispatcher stub is defined later.
# shellcheck disable=SC2218
release_native_system_input "$license_library" bin/fixture
printf 'altered still-owned license\n' > "$license_file"
if (release_native_system_input "$license_library" bin/fixture > "$test_root/license-rejection.log" 2>&1); then
  fail "collector accepted altered RPM license bytes for an unchanged linked library"
fi
grep -Fq 'content differs from installed metadata' "$test_root/license-rejection.log" \
  || fail "collector license verification failed for an unrelated reason"
unset -f dpkg-query rpm
printf 'test-native-materials: package license bytes and source identity PASS\n'

mkdir -p "$test_root/build" "$test_root/temporary" "$test_root/system"
printf 'object\n' > "$test_root/system/fixture.o"
# Observe dispatch without requiring the test host's distribution package DB.
release_native_system_input() { printf '%s\t%s\n' "$1" "$2" >> "$test_root/observed"; }
printf 'LOAD %s\n' "$test_root/build/owned.o" "$test_root/temporary/already-removed.o" \
  "$test_root/system/fixture.o" > "$test_root/link.map"
release_native_link_inputs "$test_root/link.map" "$test_root/build" "$test_root/temporary" bin/cloud-hypervisor
printf '%s\t%s\n' "$test_root/system/fixture.o" bin/cloud-hypervisor > "$test_root/expected"
cmp "$test_root/expected" "$test_root/observed"
for mutation in missing empty relative; do
  case "$mutation" in
    missing) ;;
    empty) printf 'LOAD %s\n' "$test_root/build/owned.o" > "$test_root/$mutation.map" ;;
    relative) printf 'LOAD foreign.o\n' > "$test_root/$mutation.map" ;;
  esac
  if (release_native_link_inputs "$test_root/$mutation.map" "$test_root/build" \
      "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
    fail "accepted $mutation link map"
  fi
done
printf 'test-native-materials: exact system/owned-input selection PASS\n'
