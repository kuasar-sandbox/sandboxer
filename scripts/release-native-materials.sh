#!/usr/bin/env bash
# Copyright/source material for system archives and startup objects in the actual link.
# Sourced by release.sh; uses its existing material layout and fail().
release_native_copy_file() {
  local source="$1" label="$2" name="$3"
  release_materials_safe_relative "$label/$name" || fail "unsafe native material name"
  [ -s "$source" ] || fail "native license material is missing: $source"
  local destination="$RELEASE_MATERIALS_STAGE/share/licenses/$RELEASE_MATERIALS_UNIT/$label/$name"
  mkdir -p "$(dirname "$destination")"
  install -m 0644 "$source" "$destination"
}

release_native_verify_package_file() {
  local input="$1" format="$2" owner="$3" records expected actual
  # Ownership alone also matches locally replaced files. Compare only this
  # linked input with the trusted build host's installed package metadata.
  [[ "$input" != *[[:space:]]* ]] || fail "unsupported native package path: $input"
  case "$format" in
    deb)
      records="$(dpkg-query --control-show "$owner" md5sums)" \
        || fail "native Debian file digests are unavailable: $owner"
      expected="$(awk -v path="${input#/}" '$2 == path {print $1}' <<< "$records")"
      [[ "$expected" =~ ^[0-9a-f]{32}$ ]] || fail "missing or ambiguous native Debian file digest: $input"
      actual="$(md5sum "$input" | awk '{print $1}')"
      ;;
    rpm)
      records="$(rpm -qf --dump "$input")" || fail "native RPM file digests are unavailable: $input"
      expected="$(awk -v path="$input" '$1 == path {print $4}' <<< "$records")"
      # RPM records the digest of each installed payload file. Querying these
      # records does not execute package verification scriptlets or inspect
      # unrelated mutable configuration files from the same package.
      case "$expected" in
        *[!0-9a-f]*|'') fail "missing or ambiguous native RPM file digest: $input" ;;
      esac
      case "${#expected}" in
        32) actual="$(md5sum "$input" | awk '{print $1}')" ;;
        40) actual="$(sha1sum "$input" | awk '{print $1}')" ;;
        64) actual="$(sha256sum "$input" | awk '{print $1}')" ;;
        96) actual="$(sha384sum "$input" | awk '{print $1}')" ;;
        128) actual="$(sha512sum "$input" | awk '{print $1}')" ;;
        *) fail "unsupported native RPM file digest: $input" ;;
      esac
      ;;
    *) fail "unsupported native package format: $format" ;;
  esac
  [ "$actual" = "$expected" ] || fail "native package file content differs from installed metadata: $input"
}

release_native_verify_license_file() {
  local file="$1" format="$2" expected_source="${3:-}" query owner source_name source_version actual_source
  local line owner_list count=0
  local -a owners
  file="$(realpath -e "$file")" || fail "native license input is missing"
  [ -f "$file" ] || fail "native license input is not a regular file"
  case "$format" in
    deb)
      query="$(dpkg-query -S "$file" 2>/dev/null)" || fail "native license has no Debian package owner"
      # Multi-Arch packages can co-own one copyright file. Every owner must
      # independently agree on its installed bytes and the required source.
      while IFS= read -r line; do
        [[ "$line" == *": $file" ]] || fail "ambiguous native license package owner"
        owner_list="${line%": $file"}"
        IFS=', ' read -r -a owners <<< "$owner_list"
        for owner in "${owners[@]}"; do
          [[ "$owner" =~ ^[a-z0-9][a-z0-9+.-]*(:[a-z0-9]+)?$ ]] \
            || fail "invalid native license package owner"
          release_native_verify_package_file "$file" deb "$owner"
          if [ -n "$expected_source" ]; then
            query="$(dpkg-query -W -f '${source:Package}\t${source:Version}\n' "$owner")" \
              || fail "native license source identity is unavailable"
            IFS=$'\t' read -r source_name source_version <<< "$query"
            actual_source="deb-source:$source_name@$source_version"
            [ "$actual_source" = "$expected_source" ] \
              || fail "native license belongs to a different source package"
          fi
          count=$((count + 1))
        done
      done <<< "$query"
      [ "$count" -gt 0 ] || fail "native license has no Debian package owner"
      return 0
      ;;
    rpm)
      release_native_verify_package_file "$file" rpm ""
      if [ -n "$expected_source" ]; then
        query="$(rpm -qf --qf '%{SOURCERPM}\n' "$file")" || fail "native license source identity is unavailable"
        actual_source="rpm-source:$query"
      fi
      ;;
    *) fail "unsupported native license package format" ;;
  esac
  if [ -n "$expected_source" ] && [ "$actual_source" != "$expected_source" ]; then
    fail "native license belongs to a different source package"
  fi
}

release_native_system_input() {
  local input="$1" payload="$2" query owner source_name version label copyright common
  local source_id rpm_source sibling file count=0
  input="$(realpath -e "$input")" || fail "native link input is missing"
  label="system/$(basename "$input")"
  if command -v dpkg-query >/dev/null 2>&1 \
    && query="$(dpkg-query -S "$input" 2>/dev/null)"; then
    owner="${query%%: /*}"
    [[ "$owner" != *$'\n'* && "$owner" != *,* ]] || fail "ambiguous native package owner"
    release_native_verify_package_file "$input" deb "$owner"
    query="$(dpkg-query -W -f '${source:Package}\t${source:Version}\n' "$owner")"
    IFS=$'\t' read -r source_name version <<< "$query"
    if [ -z "$source_name" ] || [ -z "$version" ]; then
      fail "native source package identity is missing"
    fi
    source_id="deb-source:$source_name@$version"
    copyright="/usr/share/doc/${owner%%:*}/copyright"
    release_native_verify_license_file "$copyright" deb "$source_id"
    release_native_copy_file "$copyright" "$label" copyright
    # Debian copyright files refer to common license texts outside the package.
    while IFS= read -r common; do
      [ -n "$common" ] || continue
      if [ ! -e "$common" ] && [[ "$common" == *. ]]; then
        common="${common%.}"
      fi
      release_native_verify_license_file "$common" deb
      release_native_copy_file "$common" "$label" "common-licenses/$(basename "$common")"
    done < <(grep -Eo '/usr/share/common-licenses/[A-Za-z0-9.+-]+' "$copyright" | LC_ALL=C sort -u)
  elif command -v rpm >/dev/null 2>&1 \
    && query="$(rpm -qf --qf '%{NAME}\t%{VERSION}-%{RELEASE}\t%{SOURCERPM}\n' "$input" 2>/dev/null)"; then
    IFS=$'\t' read -r owner version rpm_source <<< "$query"
    release_native_verify_package_file "$input" rpm "$owner"
    if [ -z "$rpm_source" ] || [ "$rpm_source" = '(none)' ]; then
      fail "native RPM source identity is missing: $input"
    fi
    source_name="$owner"
    source_id="rpm-source:$rpm_source"
    # Static/devel subpackages may keep notices in a sibling from the SAME SRPM.
    # Capture each status before consuming output; partial listings are not a
    # complete license inventory even if another sibling supplied valid files.
    local packages siblings files
    packages="$(rpm -qa --qf '%{NAME}.%{ARCH}\t%{SOURCERPM}\n')" \
      || fail "cannot enumerate installed RPM packages for license collection"
    siblings="$(awk -F '\t' -v source="$rpm_source" '$2 == source {print $1}' <<< "$packages")" \
      || fail "cannot select same-source RPM license packages"
    while IFS= read -r sibling; do
      [ -n "$sibling" ] || continue
      files="$(rpm -ql "$sibling")" || fail "cannot enumerate RPM license files: $sibling"
      while IFS= read -r file; do
        case "$(basename "$file")" in
          LICENSE*|COPYING*|NOTICE*|COPYRIGHT*|copyright|AUTHORS*|CREDITS*) ;;
          *) [[ "$file" == /usr/share/licenses/* ]] || continue ;;
        esac
        [ ! -d "$file" ] || continue
        release_native_verify_license_file "$file" rpm "$source_id"
        release_native_copy_file "$file" "$label" "${file#/}"
        count=$((count + 1))
      done <<< "$files"
    done <<< "$siblings"
    [ "$count" -gt 0 ] || fail "native RPM license material is missing: $rpm_source"
  else
    fail "native input has no verifiable package/source material: $input"
  fi
  release_materials_record_source "$payload" "system:$(basename "$input")" "$version" \
    "$source_id" "sha256:$(sha256sum "$input" | awk '{print $1}');package:$source_name" "$label"
}

release_native_link_inputs() {
  local map="$1" build_root="$2" temporary_root="$3" payload="$4" input canonical name count=0
  local -A selected_inputs=()
  [ -s "$map" ] || fail "fresh native linker map is missing"
  build_root="$(realpath -e "$build_root")"
  temporary_root="$(realpath -e "$temporary_root")"
  while IFS= read -r input; do
    [ -n "$input" ] || continue
    [[ "$input" == /* ]] || fail "native linker map contains an unresolved relative input"
    canonical="$(realpath -m "$input")"
    case "$canonical" in
      "$build_root"/*|"$temporary_root"/*) continue ;; # Fresh owned build objects, not system inputs.
    esac
    name="$(basename "$canonical")"
    if [ -n "${selected_inputs[$name]:-}" ] && [ "${selected_inputs[$name]}" != "$canonical" ]; then
      fail "distinct native link inputs share a material name: $name"
    fi
    selected_inputs[$name]="$canonical"
    release_native_system_input "$canonical" "$payload"
    count=$((count + 1))
  done < <(awk '$1 == "LOAD" && $2 ~ /\.(a|o)$/ {print $2}' "$map" | LC_ALL=C sort -u)
  [ "$count" -gt 0 ] || fail "native linker map contains no system inputs"
}
