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
  local record_name
  record_name="system:${label#system/}"
  release_materials_record_source "$payload" "$record_name" "$version" \
    "$source_id" "sha256:$(sha256sum "$input" | awk '{print $1}');package:$source_name" "$label"
}

release_native_candidate_canonical() {
  local candidate="$1"
  [[ "$candidate" == /* && -f "$candidate" ]] || return 1
  realpath -e "$candidate"
}

release_native_candidate_owned_root() {
  local candidate="$1" build_root="$2" temporary_root="$3" normalized
  [[ "$candidate" == /* ]] || return 1
  normalized="$(realpath -m -- "$candidate")" || return 1
  [[ "$normalized" == "$build_root/"* || "$normalized" == "$temporary_root/"* ]]
}

# Resolve only lld rows whose textual owner boundary is ambiguous. lld does not
# escape `:(` or parentheses in file/member names, so syntax alone cannot
# distinguish those bytes from the owner/section wrapper. In that rare case,
# require one unique file/archive candidate that actually exists. Multiple
# plausible files fail closed rather than allowing provenance to be attributed
# to the wrong path.
release_native_lld_ambiguous_candidate() {
  local input="$1" build_root="$2" temporary_root="$3"
  local rest="$input" prefix='' before owner body archive_rest archive_prefix archive_before member
  local rust_rest rust_prefix rust_before
  local canonical path kind='' found_path='' found_kind='' invalid_native=false
  local missing_owned_candidate=false missing_unowned_native_candidate=false

  [[ "$input" == *')' ]] || return 1
  while [[ "$rest" == *':('* ]]; do
    before="${rest%%":("*}"
    prefix+="$before"
    owner="$prefix"

    if [[ "$owner" == *.o ]]; then
      if canonical="$(release_native_candidate_canonical "$owner" "$build_root" "$temporary_root")"; then
        path="$canonical"
        kind=native
      else
        path=''
        if release_native_candidate_owned_root "$owner" "$build_root" "$temporary_root"; then
          missing_owned_candidate=true
        else
          missing_unowned_native_candidate=true
        fi
      fi
    elif [[ "$owner" == *.a ]]; then
      # A bare archive path is not an lld input-section owner; archives require
      # a non-empty member wrapper. Its existence must not invalidate a later
      # direct-object owner whose filename itself contains `:(`.
      path=''
    else
      # A bare `.rlib` prefix is not an lld input-section owner either. Real
      # Cargo-covered Rust contributions use `.rlib(member)` and are resolved
      # below. Treating a bare archive prefix as non-native could otherwise
      # hide a missing direct-object interpretation whose filename contains
      # `:(`.
      path=''
    fi
    if [ -n "$path" ]; then
      if [ -n "$found_path" ] \
        && { [ "$found_path" != "$path" ] || [ "$found_kind" != "$kind" ]; }; then
        return 1
      fi
      found_path="$path"
      found_kind="$kind"
    fi

    # For archive syntax, the last byte of the owner is the lld wrapper `)`.
    # The member bytes before it are opaque: they may contain balanced or
    # unbalanced parentheses and even `:(`. Check every `.a(` marker against
    # the filesystem instead of trying to balance member-name punctuation.
    if [[ "$owner" == *')' ]]; then
      body="${owner%?}"
      archive_rest="$body"
      archive_prefix=''
      while [[ "$archive_rest" == *'.a('* ]]; do
        archive_before="${archive_rest%%".a("*}"
        archive_prefix+="$archive_before.a"
        member="${archive_rest#*".a("}"
        if [ -n "$member" ]; then
          if canonical="$(release_native_candidate_canonical "$archive_prefix" "$build_root" "$temporary_root")"; then
            if [ -n "$found_path" ] \
              && { [ "$found_path" != "$canonical" ] || [ "$found_kind" != native ]; }; then
              return 1
            fi
            found_path="$canonical"
            found_kind=native
          elif release_native_candidate_owned_root "$archive_prefix" "$build_root" "$temporary_root"; then
            missing_owned_candidate=true
          else
            missing_unowned_native_candidate=true
          fi
        elif canonical="$(release_native_candidate_canonical "$archive_prefix" "$build_root" "$temporary_root")"; then
          # If the candidate archive itself is real, this is an empty member,
          # not punctuation from some unrelated path component.
          invalid_native=true
        fi
        archive_rest="$member"
        archive_prefix+='('
      done

      # Rust `.rlib` member rows are non-native inputs: Cargo/toolchain
      # provenance already covers them. Resolve the actual archive rather than
      # matching the member `.o` text, so a direct `.o` filename containing
      # `.rlib(` is still treated as native unless an rlib archive really
      # participates in the same ambiguous row.
      rust_rest="$body"
      rust_prefix=''
      while [[ "$rust_rest" == *'.rlib('* ]]; do
        rust_before="${rust_rest%%".rlib("*}"
        rust_prefix+="$rust_before.rlib"
        member="${rust_rest#*".rlib("}"
        if [ -n "$member" ]; then
          if canonical="$(release_native_candidate_canonical "$rust_prefix" "$build_root" "$temporary_root")"; then
            if [ -n "$found_path" ] \
              && { [ "$found_path" != "$canonical" ] || [ "$found_kind" != non-native ]; }; then
              return 1
            fi
            found_path="$canonical"
            found_kind=non-native
          elif release_native_candidate_owned_root "$rust_prefix" "$build_root" "$temporary_root"; then
            missing_owned_candidate=true
          fi
        fi
        rust_rest="$member"
        rust_prefix+='('
      done
    fi

    rest="${rest#*":("}"
    prefix+=':('
  done

  # An empty archive-member spelling is malformed only when it is the row's
  # actual owner. The same bytes may legally occur inside a later direct-object
  # filename; a unique real owner found above wins over that impossible prefix.
  if $invalid_native && [ -z "$found_path" ]; then
    return 1
  fi
  if [ -z "$found_path" ]; then
    $missing_unowned_native_candidate && return 1
    $missing_owned_candidate && return 0
    return 1
  fi
  if [ "$found_kind" = native ]; then
    printf '%s\n' "$found_path"
  fi
}

release_native_link_input_candidates() {
  local map="$1" build_root="$2" temporary_root="$3" rows kind value resolved
  if ! rows="$(awk '
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
        if (archive != "" || (owner ~ /\.o$/ && !has_open_archive_member(owner))) {
          return pos
        }
        offset = pos + 2
      }
      return 0
    }
    function delimiter_count(input,    rest, pos, count) {
      rest = input
      while ((pos = index(rest, ":(")) > 0) {
        count++
        rest = substr(rest, pos + 2)
      }
      return count
    }
    function archive_marker_count(input,    rest, pos, count) {
      rest = input
      while ((pos = index(rest, ".a(")) > 0) {
        count++
        rest = substr(rest, pos + 3)
      }
      return count
    }
    function looks_like_native_input(input) {
      return input ~ /\.(a|o)($|[^[:alnum:]_.+-])/
    }
    function looks_like_native_load(input,    base) {
      base = input
      sub(/^.*\//, "", base)
      return base ~ /\.(a|o)($|[^[:alnum:]_.+-])/
    }
    function structured_row() {
      return NF >= 5 && $1 ~ /^[[:xdigit:]]+$/ && $2 ~ /^[[:xdigit:]]+$/ &&
        $3 ~ /^[[:xdigit:]]+$/ && $4 ~ /^[[:digit:]]+$/
    }
    function after_four_columns(line,    work, i) {
      work = line
      sub(/^[ \t]*/, "", work)
      for (i = 1; i <= 4; i++) {
        sub(/^[^ \t]+/, "", work)
        if (i < 4) {
          sub(/^[ \t]+/, "", work)
        }
      }
      return work
    }
    function content_indent(work) {
      if (work !~ /^[ \t]*[^ \t]/) {
        return -1
      }
      return match(work, /[^ \t]/) - 1
    }

    # Once a native owner was already complete before a physical newline, the
    # following bytes belong to the section name, not to another map record.
    # A normal LOAD or normal In-column row here is ambiguous with a truncated
    # row, so fail closed instead of swallowing a real record.
    section_continuation {
      if ($1 == "LOAD") {
        exit 1
      }
      if (structured_row()) {
        continuation_work = after_four_columns($0)
        if (content_indent(continuation_work) == 9) {
          exit 1
        }
      }
      if (substr($0, length($0), 1) == ")") {
        section_continuation = 0
      }
      next
    }

    # A split lld pathname can leave an absolute-looking In-column prefix that
    # is not itself a native owner. Inspect the immediately following physical
    # line before forgetting that ambiguity. This runs before the ordinary
    # numeric-row classification so a continuation such as
    # `0 0 0 1 break.o:(.text)` cannot masquerade as an output row.
    pending_lld_owner {
      if (!structured_row()) {
        if (looks_like_native_input($0) && $0 ~ /:\(/) {
          exit 1
        }
        pending_lld_owner = 0
      } else {
        pending_work = after_four_columns($0)
        pending_indent = content_indent(pending_work)
        pending_value = pending_indent >= 0 ? substr(pending_work, pending_indent + 1) : ""
        if (looks_like_native_input(pending_value) && pending_value ~ /:\(/) {
          exit 1
        }
        pending_lld_owner = 0
      }
    }

    # GNU ld also writes LOAD path bytes verbatim. If an absolute LOAD prefix
    # is physically split, reject a following native-looking continuation.
    # A new LOAD or a normal numeric map row proves the prior LOAD was merely a
    # non-native input and clears the tentative state.
    pending_gnu_load {
      if ($1 == "LOAD") {
        pending_gnu_load = 0
      } else if (structured_row()) {
        pending_work = after_four_columns($0)
        pending_indent = content_indent(pending_work)
        pending_value = pending_indent >= 0 ? substr(pending_work, pending_indent + 1) : ""
        if (pending_indent != 9 && looks_like_native_load(pending_value)) {
          exit 1
        }
        pending_gnu_load = 0
      } else {
        if (looks_like_native_load($0)) {
          exit 1
        }
        pending_gnu_load = 0
      }
    }

    $1 == "LOAD" {
      load = $0
      sub(/^[ \t]*LOAD[ \t]+/, "", load)
      if (load ~ /\.(a|o)$/) {
        print "P\t" load
      } else if (looks_like_native_load(load)) {
        exit 1
      } else if (load ~ /^\//) {
        pending_gnu_load = 1
      }
      next
    }
    {
      if (!structured_row()) {
        # lld writes input path bytes verbatim. A newline in a linked native
        # pathname therefore splits one input-section row across physical map
        # lines. Reject an unmistakable native-looking continuation instead of
        # silently omitting it.
        if (looks_like_native_input($0) && $0 ~ /:\(/) {
          exit 1
        }
        next
      }
      work = after_four_columns($0)
      indent = content_indent(work)
      if (indent != 9) {
        next
      }
      input = substr(work, indent + 1)

      # A complete owner followed by an incomplete section wrapper means the
      # newline is in section text. Record the already-known native owner and
      # suppress physical continuation lines until that wrapper closes. If no
      # native owner is complete yet, remember an absolute prefix for one line
      # so a newline-split pathname cannot disappear as an output-looking row.
      delimiter = valid_owner_delimiter(input)
      if (substr(input, length(input), 1) != ")") {
        if (delimiter != 0 &&
            (delimiter_count(input) > 1 || archive_marker_count(input) > 1 ||
             input ~ /\.rlib\(/ || input ~ /\.rlib:\(/)) {
          # Without the final wrapper byte, an already textually ambiguous
          # owner cannot be resolved safely from syntax alone. Fail closed
          # rather than attributing a prefix while section text is multiline.
          exit 1
        }
        if (delimiter != 0) {
          owner = substr(input, 1, delimiter - 1)
          archive = archive_path(owner)
          if (archive != "") {
            print "P\t" archive
            section_continuation = 1
            next
          }
          if (owner ~ /\.o$/ && !has_open_archive_member(owner)) {
            print "P\t" owner
            section_continuation = 1
            next
          }
        }
        if (input ~ /^\//) {
          pending_lld_owner = 1
          next
        }
        if (looks_like_native_input(input)) {
          print "R\t" input
        }
        next
      }

      # Multiple textual owner delimiters or archive markers are inherently
      # ambiguous because lld leaves file/member/section names unescaped. Resolve
      # those rows against the actual existing link inputs outside awk. A bare
      # `.rlib:(...)` prefix also goes through the resolver: it is not a valid
      # Cargo-covered owner by itself, but those bytes may be part of a later
      # valid direct-object filename such as `x.rlib:(foo).o`.
      if (delimiter_count(input) > 1 || archive_marker_count(input) > 1 ||
          input ~ /\.rlib\(/ || input ~ /\.rlib:\(/) {
        print "R\t" input
        next
      }
      if (delimiter == 0) {
        if (looks_like_native_input(input)) {
          print "R\t" input
        } else if (input ~ /^\//) {
          # This can be an output-section name padded into the In column or the
          # first physical fragment of a newline-bearing owner. Defer only one
          # line; a normal next In row clears the ambiguity, while a native
          # continuation fails closed above.
          pending_lld_owner = 1
        }
        next
      }
      owner = substr(input, 1, delimiter - 1)
      archive = archive_path(owner)
      if (archive != "") {
        print "P\t" archive
      } else if (owner ~ /\.o$/ && !has_open_archive_member(owner)) {
        print "P\t" owner
      } else {
        print "R\t" input
      }
    }
    END {
      if (section_continuation) {
        exit 1
      }
    }
  ' "$map")"; then
    return 1
  fi

  while IFS=$'\t' read -r kind value; do
    [ -n "$kind" ] || continue
    case "$kind" in
      P)
        printf '%s\n' "$value"
        ;;
      R)
        if ! resolved="$(release_native_lld_ambiguous_candidate "$value" "$build_root" "$temporary_root")"; then
          return 1
        fi
        [ -z "$resolved" ] || printf '%s\n' "$resolved"
        ;;
      *)
        return 1
        ;;
    esac
  done <<< "$rows"
}
release_native_link_inputs() {
  local map="$1" build_root="$2" temporary_root="$3" payload="$4" input canonical name count=0
  local -A selected_inputs=()
  local candidates
  [ -s "$map" ] || fail "fresh native linker map is missing"
  build_root="$(realpath -e "$build_root")"
  temporary_root="$(realpath -e "$temporary_root")"
  if ! candidates="$(release_native_link_input_candidates "$map" "$build_root" "$temporary_root")"; then
    fail "native linker map contains a malformed input"
  fi
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
