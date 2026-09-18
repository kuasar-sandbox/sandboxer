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
printf 'spaced GNU object\n' > "$test_root/system/gnu space.o"
[ "$(release_native_material_label /usr/lib/Scrt1.o)" = 'system/Scrt1.o' ] \
  || fail "changed the material label for an already-safe basename"
encoded_label="$(release_native_material_label '/usr/lib/space object.o')"
release_materials_safe_relative "$encoded_label" \
  || fail "whitespace-bearing native basename produced an unsafe material label"
[[ "$encoded_label" == system/encoded/* && "$encoded_label" != *' '* ]] \
  || fail "unsafe native basename was not deterministically encoded"
printf 'test-native-materials: safe and encoded native material labels PASS\n'

# dpkg-query -S treats its operand as a pattern. A wildcard-bearing literal
# input must not inherit provenance from a different pathname matched by it.
printf 'literal wildcard object\n' > "$test_root/system/Scrt?.o"
printf 'different object\n' > "$test_root/system/Scrt1.o"
if (
  DPKG_FALSE_MATCH="$test_root/system/Scrt1.o"
  dpkg-query() {
    [ "$1" = -S ] || return 97
    printf 'fixture-dev: %s\n' "$DPKG_FALSE_MATCH"
  }
  release_native_system_input "$test_root/system/Scrt?.o" bin/cloud-hypervisor
) > "$test_root/dpkg-pattern.log" 2>&1; then
  fail "accepted a dpkg pattern match for a different native input"
fi
grep -Fq 'native package owner does not exactly match input' "$test_root/dpkg-pattern.log" \
  || fail "dpkg wildcard mismatch was rejected for an unrelated reason"
printf 'test-native-materials: dpkg ownership requires exact input pathname PASS\n'

# Observe dispatch without requiring the test host's distribution package DB.
release_native_system_input() { printf '%s\t%s\n' "$1" "$2" >> "$test_root/observed"; }
printf 'LOAD %s\n' "$test_root/build/owned.o" "$test_root/temporary/already-removed.o" \
  "$test_root/system/fixture.o" "$test_root/system/gnu space.o" > "$test_root/link.map"
release_native_link_inputs "$test_root/link.map" "$test_root/build" "$test_root/temporary" bin/cloud-hypervisor
printf '%s\t%s\n' \
  "$test_root/system/fixture.o" bin/cloud-hypervisor \
  "$test_root/system/gnu space.o" bin/cloud-hypervisor \
  | LC_ALL=C sort > "$test_root/expected"
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

# rust-lld maps describe native inputs in structurally indented input-section
# rows instead of GNU LOAD rows. Output and symbol rows must not be interpreted
# as inputs, and the complete input remainder may contain whitespace.
mkdir -p "$test_root/lld-system" "$test_root/lld system" "$test_root/lld-system/cache.a(old)"
printf 'archive\n' > "$test_root/lld-system/libfixture.a"
printf 'nested archive\n' > "$test_root/lld-system/cache.a(old)/libnested.a"
printf 'startup object\n' > "$test_root/lld-system/Scrt1.o"
printf 'delimiter object\n' > "$test_root/lld-system/foo.o:(bar).o"
printf 'non-native object\n' > "$test_root/lld-system/libfixture.rlib"
printf 'spaced object\n' > "$test_root/lld system/space object.o"
printf 'owned object\n' > "$test_root/build/owned.o"
printf 'temporary object\n' > "$test_root/temporary/transient.o"
cat > "$test_root/lld.map" <<EOF
             VMA              LMA     Size Align Out     In      Symbol
             238              238       1c     1 .text
             238              238       1c     1         $test_root/lld-system/libfixture.a(member.o):(.text)
             248              248       10     1         $test_root/lld-system/libfixture.a(a(b).o):(.rodata)
             250              250        8     1         $test_root/lld-system/libfixture.a(foo.o:(bar).o):(.data)
             251              251        8     1         $test_root/lld-system/libfixture.a(a(.o):(.data)
             252              252        8     1         $test_root/lld-system/libfixture.a(a).o):(.data)
             253              253        8     1         $test_root/lld-system/cache.a(old)/libnested.a(member.o):(.data)
             254              254       20     4         $test_root/lld-system/Scrt1.o:(.foo)bar)
             258              258        4     4         $test_root/lld-system/Scrt1.o:(.foo.o:(bar))
             260              260        4     4         $test_root/lld-system/foo.o:(bar).o:(.text)
             262              262        4     4         $test_root/lld-system/libfixture.rlib:(.foo.o)
             264              264       10     4         $test_root/lld system/space object.o:(.text)
             264              264        0     1                 foo.o
             274              274       10     4         $test_root/build/owned.o:(.text)
             284              284       10     4         $test_root/temporary/transient.o:(.text)
EOF
: > "$test_root/observed"
release_native_link_inputs "$test_root/lld.map" "$test_root/build" "$test_root/temporary" bin/cloud-hypervisor
printf '%s\t%s\n' \
  "$test_root/lld-system/Scrt1.o" bin/cloud-hypervisor \
  "$test_root/lld-system/libfixture.a" bin/cloud-hypervisor \
  "$test_root/lld-system/cache.a(old)/libnested.a" bin/cloud-hypervisor \
  "$test_root/lld-system/foo.o:(bar).o" bin/cloud-hypervisor \
  "$test_root/lld system/space object.o" bin/cloud-hypervisor \
  | LC_ALL=C sort > "$test_root/expected"
cmp "$test_root/expected" "$test_root/observed"
printf 'test-native-materials: rust-lld structured archive/direct inputs and owned filtering PASS\n'

for mutation in lld-relative lld-malformed lld-archive-no-member lld-archive-empty-member lld-object-bad-suffix lld-owned-only; do
  case "$mutation" in
    lld-relative) printf '0 0 0 1         foreign.o:(.text)\n' > "$test_root/$mutation.map" ;;
    lld-malformed) printf '0 0 0 1         %s(member.o)\n' "$test_root/lld-system/libfixture.a" > "$test_root/$mutation.map" ;;
    lld-archive-no-member) printf '0 0 0 1         %s:(.text)\n' "$test_root/lld-system/libfixture.a" > "$test_root/$mutation.map" ;;
    lld-archive-empty-member) printf '0 0 0 1         %s():(.text)\n' "$test_root/lld-system/libfixture.a" > "$test_root/$mutation.map" ;;
    lld-object-bad-suffix) printf '0 0 0 1         %s):(.text)\n' "$test_root/lld-system/Scrt1.o" > "$test_root/$mutation.map" ;;
    lld-owned-only) printf '0 0 0 1         %s:(.text)\n' "$test_root/build/owned.o" > "$test_root/$mutation.map" ;;
  esac
  if (release_native_link_inputs "$test_root/$mutation.map" "$test_root/build" \
      "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
    fail "accepted $mutation link map"
  fi
done
printf 'test-native-materials: rust-lld malformed/relative/owned-only rejection PASS\n'

for malformed in archive object-suffix; do
  case "$malformed" in
    archive) malformed_row="$test_root/lld-system/libfixture.a(member.o)" ;;
    object-suffix) malformed_row="$test_root/lld-system/Scrt1.o):(.text)" ;;
  esac
  cat > "$test_root/lld-mixed-$malformed.map" <<EOF
0 0 0 1         $test_root/lld-system/Scrt1.o:(.text)
0 0 0 1         $malformed_row
EOF
  : > "$test_root/observed"
  if (release_native_link_inputs "$test_root/lld-mixed-$malformed.map" "$test_root/build" \
      "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
    fail "accepted a mixed valid/malformed rust-lld map: $malformed"
  fi
  [ ! -s "$test_root/observed" ] \
    || fail "processed a valid rust-lld input before rejecting malformed $malformed row"
done
printf 'test-native-materials: mixed valid/malformed rust-lld rejection PASS\n'

# If both an early delimiter prefix and the full delimiter-bearing object exist,
# the textual lld row is genuinely ambiguous. Fail closed rather than choosing
# one provenance source by delimiter position.
printf 'prefix object\n' > "$test_root/lld-system/ambiguous.o"
printf 'full object\n' > "$test_root/lld-system/ambiguous.o:(member).o"
printf '0 0 0 1         %s:(.text)\n' \
  "$test_root/lld-system/ambiguous.o:(member).o" > "$test_root/lld-ambiguous.map"
if (release_native_link_inputs "$test_root/lld-ambiguous.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
  fail "accepted an lld row with two existing owner interpretations"
fi
printf 'test-native-materials: ambiguous existing lld owners fail closed PASS\n'

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

# Run the real normal-build entry with owned command fixtures, not a Rust build.
(
ch_test_tools="$test_root/ch-tools"
mkdir -p "$ch_test_tools"
export CH_TEST_REAL_CP CH_TEST_REAL_CHMOD
CH_TEST_REAL_CP="$(command -v cp)"
CH_TEST_REAL_CHMOD="$(command -v chmod)"
cat > "$ch_test_tools/cargo" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'build\n' >> "$CH_TEST_CALLS"
mkdir -p "$CARGO_TARGET_DIR/release"
printf '#!/bin/sh\nprintf "new fixture binary\\n"\n' > "$CARGO_TARGET_DIR/release/cloud-hypervisor"
map="${!#}"
map="${map#link-arg=-Wl,-Map,}"
printf 'new fixture map\n' > "$map"
printf '%s\n' '{"reason":"build-finished","success":true}'
[ "${CH_TEST_FAIL:-}" != cargo ]
EOF
cat > "$ch_test_tools/rustc" <<'EOF'
#!/bin/sh
printf 'host: x86_64-unknown-linux-gnu\n'
EOF
cat > "$ch_test_tools/cp" <<'EOF'
#!/usr/bin/env bash
[ "${CH_TEST_FAIL:-}" != copy ] || exit 23
exec "$CH_TEST_REAL_CP" "$@"
EOF
cat > "$ch_test_tools/chmod" <<'EOF'
#!/usr/bin/env bash
[ "${CH_TEST_FAIL:-}" != mode ] || exit 24
exec "$CH_TEST_REAL_CHMOD" "$@"
EOF
chmod 0755 "$ch_test_tools/cargo" "$ch_test_tools/rustc" "$ch_test_tools/cp" "$ch_test_tools/chmod"
for failure in copy cargo mode; do
  run="$test_root/ch-$failure"
  mkdir -p "$run/source" "$run/out" "$run/bin"
  printf '[package]\nname="fixture"\n' > "$run/source/Cargo.toml"
  printf '#!/bin/sh\nprintf "old fixture binary\\n"\n' > "$run/bin/cloud-hypervisor"
  chmod 0755 "$run/bin/cloud-hypervisor"
  printf 'old fixture report\n' > "$run/out/build-report.jsonl"
  ch_test_env=(PATH="$ch_test_tools:$PATH" STAGE=build RUST_TARGET=''
    CH_SRC="$run/source" CH_BUILD_OUT="$run/out" BINDIR="$run/bin"
    CH_BUILD_REPORT="$run/out/build-report.jsonl" CH_LINK_MAP="$run/out/link.map"
    CARGO_HOME="$run/cargo-home" CH_TEST_CALLS="$run/calls")
  if env "${ch_test_env[@]}" CH_TEST_FAIL="$failure" bash \
      "$ROOT/native-deps/deps/build-cloud-hypervisor.sh" > "$run/failure.log" 2>&1; then
    fail "normal CH build accepted $failure failure"
  fi
  [ ! -s "$run/out/build-report.jsonl" ] \
    || fail "failed CH $failure left a reusable report beside an incomplete output set"
  env "${ch_test_env[@]}" CH_TEST_FAIL='' bash \
    "$ROOT/native-deps/deps/build-cloud-hypervisor.sh" > "$run/retry.log" 2>&1
  [ "$(wc -l < "$run/calls")" -eq 2 ] || fail "CH retry skipped an incomplete prior build"
  [ "$("$run/bin/cloud-hypervisor" --version)" = 'new fixture binary' ]
  grep -Fq 'new fixture map' "$run/out/link.map"
  grep -Fq '"success":true' "$run/out/build-report.jsonl"
  env "${ch_test_env[@]}" CH_TEST_FAIL='' bash \
    "$ROOT/native-deps/deps/build-cloud-hypervisor.sh" > "$run/reuse.log" 2>&1
  [ "$(wc -l < "$run/calls")" -eq 2 ] || fail "CH did not reuse a completed binary/record set"
done
printf 'test-native-materials: CH failed-build rejection, retry and completed-set reuse PASS\n'
)
