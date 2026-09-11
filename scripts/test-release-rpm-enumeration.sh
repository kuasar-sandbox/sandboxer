#!/usr/bin/env bash
# Verify command status propagation, not a real RPM database or MicroVM.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'test-rpm-enumeration: %s\n' "$*" >&2; exit 1; }
# shellcheck source=scripts/release-materials.sh
source "$ROOT/scripts/release-materials.sh"
# shellcheck source=scripts/release-native-materials.sh
source "$ROOT/scripts/release-native-materials.sh"
printf 'fixture native input\n' > "$TMP/input"
printf 'fixture first notice\n' > "$TMP/NOTICE-sibling1"
printf 'fixture second notice\n' > "$TMP/NOTICE-sibling2"
dpkg-query() { return 1; }
# Isolate complete/partial RPM enumeration and actual source/notice collection.
rpm() {
  case "$1" in
    -qf) printf 'fixture-owner\t1.0-1\tfixture-source-1.0-1.src.rpm\n' ;;
    -qa)
      printf 'sibling1\tfixture-source-1.0-1.src.rpm\nsibling2\tfixture-source-1.0-1.src.rpm\n'
      [ "$case_name" != enumeration-failure ]
      ;;
    -ql)
      printf '%s/NOTICE-%s\n' "$TMP" "$2"
      [ "$case_name:$2" != files-failure:sibling2 ]
      ;;
    *) return 2 ;;
  esac
}
for case_name in enumeration-failure files-failure complete; do
  if (
    release_materials_init "$TMP/stage-$case_name" "$TMP/work-$case_name" fixture-rpm
    release_native_system_input "$TMP/input" bin/payload
  ) > "$TMP/$case_name.log" 2>&1; then
    [ "$case_name" = complete ] || fail "accepted a partial RPM listing: $case_name"
    [ -s "$TMP/work-$case_name/sources" ] || fail "complete enumeration emitted no source record"
  else
    [ "$case_name" != complete ] || { cat "$TMP/$case_name.log" >&2; fail "complete enumeration failed"; }
    [ ! -s "$TMP/work-$case_name/sources" ] || fail "partial enumeration emitted a complete source record"
    case "$case_name" in
      enumeration-failure) expected='cannot enumerate installed RPM packages' ;;
      files-failure) expected='cannot enumerate RPM license files: sibling2' ;;
    esac
    grep -Fq "$expected" "$TMP/$case_name.log" || fail "enumeration failed for an unrelated reason"
  fi
done
printf 'test-rpm-enumeration: partial package/file listings rejected; complete two-sibling collection PASS\n'
