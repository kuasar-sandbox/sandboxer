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

mkdir -p "$test_root/system-a" "$test_root/system-b"
printf 'first native input\n' > "$test_root/system-a/same.a"
printf 'second native input\n' > "$test_root/system-b/same.a"
printf 'LOAD %s\n' "$test_root/system-a/same.a" "$test_root/system-b/same.a" > "$test_root/collision.map"
: > "$test_root/observed"
if (release_native_link_inputs "$test_root/collision.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor > "$test_root/collision.log" 2>&1); then
  fail "accepted two distinct native inputs with one material directory name"
fi
grep -Fq 'distinct native link inputs share a material name' "$test_root/collision.log" \
  || fail "native input collision was rejected for an unrelated reason"
printf '%s\t%s\n' "$test_root/system-a/same.a" bin/cloud-hypervisor > "$test_root/expected"
cmp "$test_root/expected" "$test_root/observed" \
  || fail "second native input reached the shared material destination"
printf 'test-native-materials: colliding input rejected before overwriting materials PASS\n'
