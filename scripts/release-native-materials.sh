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

release_native_material_label() {
  local input="$1" name label digest
  name="$(basename "$input")"
  label="system/$name"
  if release_materials_safe_relative "$label"; then
    printf '%s\n' "$label"
    return
  fi
  digest="$(printf '%s' "$name" | sha256sum | awk '{print $1}')"
  printf 'system/encoded/%s\n' "$digest"
}

release_native_system_input() {
  local input="$1" payload="$2" query owner source_name version label copyright common
  local source_id rpm_source sibling file count=0 line query_owner query_path
  input="$(realpath -e "$input")" || fail "native link input is missing"
  label="$(release_native_material_label "$input")"
  if command -v dpkg-query >/dev/null 2>&1 \
    && query="$(dpkg-query -S "$input" 2>/dev/null)"; then
    owner=""
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      query_path="${line#*: }"
      [ "$query_path" = "$input" ] || continue
      query_owner="${line%%: *}"
      if [ -n "$owner" ] && [ "$owner" != "$query_owner" ]; then
        fail "ambiguous native package owner"
      fi
      owner="$query_owner"
    done <<< "$query"
    [ -n "$owner" ] || fail "native package owner does not exactly match input: $input"
    [[ "$owner" != *$'\n'* && "$owner" != *,* ]] || fail "ambiguous native package owner"
    query="$(dpkg-query -W -f '${source:Package}\t${source:Version}\n' "$owner")"
    IFS=$'\t' read -r source_name version <<< "$query"
    if [ -z "$source_name" ] || [ -z "$version" ]; then
      fail "native source package identity is missing"
    fi
    source_id="deb-source:$source_name@$version"
    copyright="/usr/share/doc/${owner%%:*}/copyright"
    release_native_copy_file "$copyright" "$label" copyright
    # Debian copyright files refer to common license texts outside the package.
    while IFS= read -r common; do
      [ -n "$common" ] || continue
      if [ ! -e "$common" ] && [[ "$common" == *. ]]; then
        common="${common%.}"
      fi
      release_native_copy_file "$common" "$label" "common-licenses/$(basename "$common")"
    done < <(grep -Eo '/usr/share/common-licenses/[A-Za-z0-9.+-]+' "$copyright" | LC_ALL=C sort -u)
  elif command -v rpm >/dev/null 2>&1 \
    && query="$(rpm -qf --qf '%{NAME}\t%{VERSION}-%{RELEASE}\t%{SOURCERPM}\n' "$input" 2>/dev/null)"; then
    IFS=$'\t' read -r owner version rpm_source <<< "$query"
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
        release_native_copy_file "$file" "$label" "${file#/}"
        count=$((count + 1))
      done <<< "$files"
    done <<< "$siblings"
    [ "$count" -gt 0 ] || fail "native RPM license material is missing: $rpm_source"
  else
    fail "native input has no package/source material: $input"
  fi
  release_materials_record_source "$payload" "system:$(basename "$input")" "$version" \
    "$source_id" "sha256:$(sha256sum "$input" | awk '{print $1}');package:$source_name" "$label"
}

release_native_link_input_candidates() {
  local map="$1"
  awk '
    function archive_path(owner,    offset, relative, marker, i, char, depth) {
      offset = 1
      while ((relative = index(substr(owner, offset), ".a(")) > 0) {
        marker = offset + relative - 1
        depth = 1
        for (i = marker + 3; i <= length(owner); i++) {
          char = substr(owner, i, 1)
          if (char == "(") {
            depth++
          } else if (char == ")") {
            depth--
            if (depth == 0) {
              # A real archive/member marker closes at the end of the owner.
              # This rejects directory names such as cache.a(old)/libx.a(...)
              # while permitting balanced parentheses inside the member name.
              if (i == length(owner) && i > marker + 3) {
                return substr(owner, 1, marker + 1)
              }
              break
            }
          }
        }
        offset = marker + 3
      }
      return ""
    }
    function has_open_archive_member(owner,    offset, relative, marker, i, char, depth, closed) {
      offset = 1
      while ((relative = index(substr(owner, offset), ".a(")) > 0) {
        marker = offset + relative - 1
        depth = 1
        closed = 0
        for (i = marker + 3; i <= length(owner); i++) {
          char = substr(owner, i, 1)
          if (char == "(") {
            depth++
          } else if (char == ")") {
            depth--
            if (depth == 0) {
              closed = 1
              break
            }
          }
        }
        if (!closed) {
          return 1
        }
        offset = marker + 3
      }
      return 0
    }
    function valid_owner_delimiter(input,    offset, relative, pos, owner, archive) {
      offset = 1
      while ((relative = index(substr(input, offset), ":(")) > 0) {
        pos = offset + relative - 1
        owner = substr(input, 1, pos - 1)
        archive = archive_path(owner)
        # The first syntactically complete owner delimiter starts the section
        # wrapper. A direct-object-looking delimiter inside an open archive
        # member is not an owner boundary (for example foo.o:(bar).o).
        if (archive != "" || (owner ~ /\.o$/ && !has_open_archive_member(owner))) {
          return pos
        }
        offset = pos + 2
      }
      return 0
    }
    function looks_like_native_input(input) {
      # Fail closed for malformed object/archive-shaped input rows, including
      # suffix punctuation such as `.o):(`. Do not confuse ordinary names like
      # `.old` with an object suffix.
      return input ~ /\.(a|o)($|[^[:alnum:]_.+-])/
    }
    $1 == "LOAD" {
      load = $0
      sub(/^[ \t]*LOAD[ \t]+/, "", load)
      if (load ~ /\.(a|o)$/) {
        print load
      } else if (looks_like_native_input(load)) {
        exit 1
      }
      next
    }
    {
      if (NF < 5 || $1 !~ /^[[:xdigit:]]+$/ || $2 !~ /^[[:xdigit:]]+$/ ||
          $3 !~ /^[[:xdigit:]]+$/ || $4 !~ /^[[:digit:]]+$/) {
        next
      }

      # lld map rows have four numeric columns followed by one space for an
      # output row, nine spaces for an input-section row, and deeper
      # indentation for symbol rows. Keep the input remainder byte-for-byte so
      # whitespace in an absolute path is not lost through awk field splitting.
      work = $0
      sub(/^[ \t]*/, "", work)
      for (i = 1; i <= 4; i++) {
        sub(/^[^ \t]+/, "", work)
        if (i < 4) {
          sub(/^[ \t]+/, "", work)
        }
      }
      if (work !~ /^[ \t]*[^ \t]/) {
        next
      }
      indent = match(work, /[^ \t]/) - 1
      if (indent != 9) {
        next
      }
      input = substr(work, indent + 1)

      # Find a section wrapper whose owner is a direct object or a non-empty
      # archive member. Choosing an owner-valid delimiter also permits `)` (and
      # even `:(`) inside the section name without confusing it with the owner.
      delimiter = valid_owner_delimiter(input)
      if (delimiter == 0 || substr(input, length(input), 1) != ")") {
        if (looks_like_native_input(input)) {
          exit 1
        }
        next
      }
      owner = substr(input, 1, delimiter - 1)
      archive = archive_path(owner)
      if (archive != "") {
        print archive
      } else if (owner ~ /\.o$/ && !has_open_archive_member(owner)) {
        print owner
      } else {
        exit 1
      }
    }
  ' "$map"
}

release_native_link_inputs() {
  local map="$1" build_root="$2" temporary_root="$3" payload="$4" input canonical name count=0
  local -A selected_inputs=()
  local candidates
  [ -s "$map" ] || fail "fresh native linker map is missing"
  if ! candidates="$(release_native_link_input_candidates "$map")"; then
    fail "native linker map contains a malformed input"
  fi
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
  done < <(printf '%s\n' "$candidates" | LC_ALL=C sort -u)
  [ "$count" -gt 0 ] || fail "native linker map contains no system inputs"
}
