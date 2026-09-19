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

# The source-record name must use the same safe identity as the material label.
# A tab is legal in a filesystem basename and is preserved by lld, but cannot be
# written as a raw TSV field in the release source manifest.
(
  tab_input="$test_root/system/tab"$'\t'"object.o"
  tab_license="$test_root/system/LICENSE-tab"
  printf 'tab object\n' > "$tab_input"
  printf 'license\n' > "$tab_license"
  export RELEASE_MATERIALS_WORK="$test_root/tab-materials-work"
  export RELEASE_MATERIALS_STAGE="$test_root/tab-materials-stage"
  export RELEASE_MATERIALS_UNIT=sandboxer
  mkdir -p "$RELEASE_MATERIALS_WORK" "$RELEASE_MATERIALS_STAGE"
  dpkg-query() { return 1; }
  rpm() {
    case "$1" in
      -qf) printf 'fixture-devel\t1.0-1\tfixture-1.0-1.src.rpm\n' ;;
      -qa) printf 'fixture-devel.x86_64\tfixture-1.0-1.src.rpm\n' ;;
      -ql) printf '%s\n' "$tab_license" ;;
      *) return 97 ;;
    esac
  }
  release_native_system_input "$tab_input" bin/cloud-hypervisor
  tab_label="$(release_native_material_label "$tab_input")"
  tab_record_name="$(awk -F '\t' 'NR == 1 { print $2 }' "$RELEASE_MATERIALS_WORK/sources")"
  [ "$tab_record_name" = "system:${tab_label#system/}" ] \
    || fail "tab-bearing native basename did not use the encoded source identity"
)
printf 'test-native-materials: encoded native source record identity PASS\n'

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
printf 'bare archive prefix\n' > "$test_root/lld-system/direct-prefix.a"
printf 'direct object after archive prefix\n' > "$test_root/lld-system/direct-prefix.a:(x).o"
printf 'empty member archive prefix\n' > "$test_root/lld-system/empty-prefix.a"
printf 'direct object after empty member prefix\n' > "$test_root/lld-system/empty-prefix.a():(foo).o"
printf 'bare rlib filename prefix\n' > "$test_root/lld-system/direct-prefix.rlib"
printf 'direct object after rlib filename prefix\n' > "$test_root/lld-system/direct-prefix.rlib:(x).o"
printf 'nested-marker archive\n' > "$test_root/lld-system/outer-missing.a(inner.a"
printf 'non-native object\n' > "$test_root/lld-system/libfixture.rlib"
printf 'spaced object\n' > "$test_root/lld system/space object.o"
printf 'owned object\n' > "$test_root/build/owned.o"
printf 'owned archive\n' > "$test_root/build/libowned.a"
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
             261              261        4     4         $test_root/lld-system/outer-missing.a(inner.a(member.o):(.text)
             263              263        4     4         $test_root/lld-system/libfixture.rlib(member.o):(.text)
             263              263        4     4         $test_root/lld-system/libfixture.rlib(member.a(foo.o):(.text)
             264              264       10     4         $test_root/lld system/space object.o:(.text)
             265              265        4     4         $test_root/lld-system/direct-prefix.a:(x).o:(.text)
             266              266        4     4         $test_root/lld-system/empty-prefix.a():(foo).o:(.text)
             267              267        4     4         $test_root/lld-system/direct-prefix.rlib:(x).o:(.text)
             264              264        0     1                 foo.o
             274              274       10     4         $test_root/build/owned.o:(.text)
             275              275       10     4         $test_root/build/libowned.a(foo.o:(bar).o):(.text)
             284              284       10     4         $test_root/temporary/transient.o:(.text)
EOF
: > "$test_root/observed"
release_native_link_inputs "$test_root/lld.map" "$test_root/build" "$test_root/temporary" bin/cloud-hypervisor
printf '%s\t%s\n' \
  "$test_root/lld-system/Scrt1.o" bin/cloud-hypervisor \
  "$test_root/lld-system/libfixture.a" bin/cloud-hypervisor \
  "$test_root/lld-system/cache.a(old)/libnested.a" bin/cloud-hypervisor \
  "$test_root/lld-system/foo.o:(bar).o" bin/cloud-hypervisor \
  "$test_root/lld-system/outer-missing.a(inner.a" bin/cloud-hypervisor \
  "$test_root/lld-system/direct-prefix.a:(x).o" bin/cloud-hypervisor \
  "$test_root/lld-system/empty-prefix.a():(foo).o" bin/cloud-hypervisor \
  "$test_root/lld-system/direct-prefix.rlib:(x).o" bin/cloud-hypervisor \
  "$test_root/lld system/space object.o" bin/cloud-hypervisor \
  | LC_ALL=C sort > "$test_root/expected"
cmp "$test_root/expected" "$test_root/observed"
printf 'test-native-materials: rust-lld structured archive/direct inputs and owned filtering PASS\n'

# Ambiguous owned inputs may legitimately be gone by packaging time. Preserve
# the GNU-LOAD contract: if every plausible missing native candidate belongs to
# the build/temp roots, filter the row rather than rejecting an otherwise valid
# map. Do not create the delimiter-bearing owned object.
cat > "$test_root/lld-removed-owned.map" <<EOF
0 0 0 1         $test_root/lld-system/Scrt1.o:(.text)
0 0 0 1         $test_root/build/removed.o:(bar).o:(.text)
EOF
: > "$test_root/observed"
release_native_link_inputs "$test_root/lld-removed-owned.map" "$test_root/build" \
  "$test_root/temporary" bin/cloud-hypervisor
printf '%s\t%s\n' "$test_root/lld-system/Scrt1.o" bin/cloud-hypervisor > "$test_root/expected"
cmp "$test_root/expected" "$test_root/observed"
printf 'test-native-materials: removed ambiguous owned lld input filtering PASS\n'

# A deleted ambiguous input is filterable only when every missing native
# interpretation remains inside an owned build/temp root. A missing system-side
# interpretation must keep the map fail-closed rather than being hidden by an
# invented owned prefix.
cat > "$test_root/lld-removed-mixed-root.map" <<EOF
0 0 0 1         $test_root/build/prefix.o:(dir)/../../lld-system/removed-system.o:(.text)
0 0 0 1         $test_root/lld-system/Scrt1.o:(.text)
EOF
: > "$test_root/observed"
if (release_native_link_inputs "$test_root/lld-removed-mixed-root.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
  fail "accepted ambiguous deleted lld input with an unowned interpretation"
fi
[ ! -s "$test_root/observed" ] \
  || fail "processed a later valid input after ambiguous deleted unowned input"
printf 'test-native-materials: ambiguous deleted unowned lld input rejection PASS\n'

# A directory cannot be an lld-linked object/archive candidate. Do not let an
# existing directory at a longer ambiguous prefix hide a missing system object.
mkdir -p "$test_root/lld-system/deleted-dir.o:(.foo)"
cat > "$test_root/lld-directory-candidate.map" <<EOF
0 0 0 1         $test_root/lld-system/deleted-dir.o:(.foo):(.bar)
0 0 0 1         $test_root/lld-system/Scrt1.o:(.text)
EOF
: > "$test_root/observed"
if (release_native_link_inputs "$test_root/lld-directory-candidate.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
  fail "accepted an lld directory as an owner candidate"
fi
[ ! -s "$test_root/observed" ] \
  || fail "processed a later valid input after an lld directory candidate"
printf 'test-native-materials: lld directory owner candidate rejection PASS\n'

# An arbitrary regular file at a longer delimiter prefix is not a known lld
# owner kind. It must not hide a missing unowned native interpretation.
printf 'unrelated regular file\n' > "$test_root/lld-system/deleted-file.o:(.foo)"
cat > "$test_root/lld-regular-prefix-candidate.map" <<EOF
0 0 0 1         $test_root/lld-system/deleted-file.o:(.foo):(.bar)
0 0 0 1         $test_root/lld-system/Scrt1.o:(.text)
EOF
: > "$test_root/observed"
if (release_native_link_inputs "$test_root/lld-regular-prefix-candidate.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
  fail "accepted an arbitrary regular file as an lld owner candidate"
fi
[ ! -s "$test_root/observed" ] \
  || fail "processed a later valid input after an arbitrary lld regular-file prefix"
printf 'test-native-materials: lld arbitrary regular-file owner rejection PASS\n'

# A bare `.rlib` archive is not itself an lld input-section owner. If a missing
# direct-object interpretation embeds `:(` after that prefix, the archive must
# not hide the missing unowned native input.
printf 'cargo archive\n' > "$test_root/lld-system/deleted-prefix.rlib"
cat > "$test_root/lld-bare-rlib-prefix.map" <<EOF
0 0 0 1         $test_root/lld-system/deleted-prefix.rlib:(foo).o:(.text)
0 0 0 1         $test_root/lld-system/Scrt1.o:(.text)
EOF
: > "$test_root/observed"
if (release_native_link_inputs "$test_root/lld-bare-rlib-prefix.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
  fail "accepted a bare rlib prefix as a Cargo-owned lld input"
fi
[ ! -s "$test_root/observed" ] \
  || fail "processed a later valid input after a bare rlib prefix"
printf 'test-native-materials: bare rlib delimiter cannot hide missing native owner PASS\n'

# lld preserves newlines in pathnames, which physically splits the map row.
# Reject the native-looking continuation before dispatching an earlier valid
# candidate, rather than silently dropping the split input from provenance.
newline_input="$test_root/lld-system/newline"$'\n'"break.o"
printf 'newline object\n' > "$newline_input"
{
  printf '0 0 0 1         %s:(.text)\n' "$test_root/lld-system/Scrt1.o"
  printf '0 0 0 1         %s:(.text)\n' "$newline_input"
} > "$test_root/lld-newline-input.map"
: > "$test_root/observed"
if (release_native_link_inputs "$test_root/lld-newline-input.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
  fail "accepted a newline-split lld native input"
fi
[ ! -s "$test_root/observed" ] \
  || fail "processed a valid input before rejecting a newline-split lld row"
printf 'test-native-materials: newline-split lld native input rejection PASS\n'

# A newline continuation can itself begin with text that looks exactly like
# lld's four numeric columns. The first input-column fragment is already
# malformed because it lost its `:(section)` wrapper, so reject it before the
# numeric-looking continuation can be mistaken for another map row.
numeric_newline_input="$test_root/lld-system/numeric-newline"$'\n'"0 0 0 1 break.o"
printf 'numeric newline object\n' > "$numeric_newline_input"
{
  printf '0 0 0 1         %s:(.text)\n' "$test_root/lld-system/Scrt1.o"
  printf '0 0 0 1         %s:(.text)\n' "$numeric_newline_input"
} > "$test_root/lld-numeric-newline-input.map"
: > "$test_root/observed"
if (release_native_link_inputs "$test_root/lld-numeric-newline-input.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
  fail "accepted a numeric-looking continuation of a newline-split lld input"
fi
[ ! -s "$test_root/observed" ] \
  || fail "processed a valid input before rejecting a numeric-looking newline continuation"
printf 'test-native-materials: numeric-looking newline continuation rejection PASS\n'

# A standalone bare `.rlib:(section)` row is not the real Cargo-covered
# `.rlib(member):(section)` form. It must fail closed instead of disappearing
# when another valid native input keeps the map non-empty.
cat > "$test_root/lld-standalone-bare-rlib.map" <<EOF
0 0 0 1         $test_root/lld-system/libfixture.rlib:(.text)
0 0 0 1         $test_root/lld-system/Scrt1.o:(.text)
EOF
: > "$test_root/observed"
if (release_native_link_inputs "$test_root/lld-standalone-bare-rlib.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
  fail "accepted a standalone bare rlib lld owner row"
fi
[ ! -s "$test_root/observed" ] \
  || fail "processed a later valid input after a standalone bare rlib row"
printf 'test-native-materials: standalone bare rlib owner rejection PASS\n'

# Native and Cargo-covered interpretations remain ambiguous even when symlink
# names canonicalize to the same file. Provenance kind is part of the identity.
printf 'shared archive\n' > "$test_root/lld-system/shared-kind-archive"
ln -s "$test_root/lld-system/shared-kind-archive" "$test_root/lld-system/kind.rlib"
ln -s "$test_root/lld-system/shared-kind-archive" "$test_root/lld-system/kind.rlib(member.a"
cat > "$test_root/lld-kind-conflict.map" <<EOF
0 0 0 1         $test_root/lld-system/kind.rlib(member.a(foo)):(.text)
0 0 0 1         $test_root/lld-system/Scrt1.o:(.text)
EOF
: > "$test_root/observed"
if (release_native_link_inputs "$test_root/lld-kind-conflict.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
  fail "accepted canonical lld owner candidates with conflicting provenance kinds"
fi
[ ! -s "$test_root/observed" ] \
  || fail "processed a later valid input after conflicting lld provenance kinds"
printf 'test-native-materials: canonical lld provenance-kind conflict rejection PASS\n'

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

# Multiple `.a(` markers can also encode more than one archive interpretation
# even when the row has only one `:(` section delimiter. Require the real
# filesystem-backed archive owner to be unique.
printf 'outer archive\n' > "$test_root/lld-system/outer.a"
printf 'inner archive path\n' > "$test_root/lld-system/outer.a(inner.a"
printf '0 0 0 1         %s(member.o):(.text)\n' \
  "$test_root/lld-system/outer.a(inner.a" > "$test_root/lld-archive-ambiguous.map"
if (release_native_link_inputs "$test_root/lld-archive-ambiguous.map" "$test_root/build" \
    "$test_root/temporary" bin/cloud-hypervisor >/dev/null 2>&1); then
  fail "accepted an lld row with two existing archive interpretations"
fi
printf 'test-native-materials: ambiguous existing lld archive markers fail closed PASS\n'

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
