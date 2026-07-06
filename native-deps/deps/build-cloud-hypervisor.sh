#!/usr/bin/env bash
#
# Multi-stage dispatcher for cloud-hypervisor source-with-patches build.
# Stages are selected via the STAGE env var (default: build):
#
#   STAGE=fetch          extract tarball + git init + tag ch-patches-base
#   STAGE=patches-apply  git am deps/ch-patches/*.patch onto the source tree
#   STAGE=patches-format git format-patch ch-patches-base..HEAD → deps/ch-patches/
#   STAGE=build          cargo build --release --bin cloud-hypervisor → BINDIR
#
# Each stage is idempotent in the safe sense:
#   - fetch skips if the ch-patches-base tag already exists; refuses if a
#     foreign git tree is present without that tag (won't overwrite WIP).
#   - patches-apply requires HEAD == ch-patches-base; otherwise refuses
#     and tells the user to format-extract WIP first, then reset.
#   - patches-format clears stale .patch files before regenerating.
#   - build relies on cargo's incremental compile.
#
# Inputs (env, all optional):
#   CLOUD_HYPERVISOR_TARBALL         URL or local path; supports "url#filename" form.
#                                     Default: cloud-hypervisor v51.1 github archive.
#   CLOUD_HYPERVISOR_TARBALL_SHA256  Optional expected SHA256.
#   BUILD_DIR                         Repo's build/ root.
#   BINDIR                            Repo's bin/.
#   CH_SRC                            Source tree location. Default: $BUILD_DIR/src/cloud-hypervisor.
#   CH_BUILD_OUT                      cargo target dir. Default: $BUILD_DIR/cloud-hypervisor.
#   PATCHES_DIR                       Patch files location. Default: $(pwd)/deps/ch-patches.
#   CH_BASE_TAG                       git tag name for the import baseline. Default: ch-patches-base.
#
# WSL2 users on /mnt/<drive>/ should override CH_SRC + CH_BUILD_OUT to a
# Linux-native filesystem (e.g. ~/ch-build/{src,out}) — DrvFs adds 5-10×
# I/O overhead per small file, and a kernel-grade Rust workspace has
# ~50k files plus a 2 GB cargo target.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
source "$script_dir/common.sh"

: "${STAGE:=build}"
: "${CLOUD_HYPERVISOR_TARBALL:=https://github.com/cloud-hypervisor/cloud-hypervisor/archive/refs/tags/v51.1.tar.gz#cloud-hypervisor-51.1.tar.gz}"
: "${CLOUD_HYPERVISOR_TARBALL_SHA256:=}"
: "${BUILD_DIR:=$(pwd)/build}"
: "${BINDIR:=$(pwd)/bin}"
: "${CH_SRC:=$BUILD_DIR/src/cloud-hypervisor}"
: "${CH_BUILD_OUT:=$BUILD_DIR/cloud-hypervisor}"
: "${PATCHES_DIR:=$(pwd)/deps/ch-patches}"
: "${CH_BASE_TAG:=ch-patches-base}"

do_fetch() {
    if [ -d "$CH_SRC/.git" ]; then
        if git -C "$CH_SRC" rev-parse --verify "$CH_BASE_TAG" >/dev/null 2>&1; then
            log "$CH_BASE_TAG tag exists at $CH_SRC, skipping fetch"
            return 0
        fi
        die "git tree exists at $CH_SRC but no $CH_BASE_TAG tag — refusing to overwrite. Either tag manually (git -C $CH_SRC tag $CH_BASE_TAG <commit>) or rm -rf $CH_SRC to start fresh."
    fi
    require_cmd tar git
    tarball="$(resolve_tarball "$CLOUD_HYPERVISOR_TARBALL" "$CLOUD_HYPERVISOR_TARBALL_SHA256")"
    extract_tarball "$tarball" "$CH_SRC" >/dev/null
    log "git init + tag $CH_BASE_TAG at $CH_SRC"
    git -C "$CH_SRC" init -q
    git -C "$CH_SRC" -c user.name=deps -c user.email=deps@local add -A
    git -C "$CH_SRC" -c user.name=deps -c user.email=deps@local commit -q -m "import $tarball"
    git -C "$CH_SRC" tag "$CH_BASE_TAG"
}

do_patches_apply() {
    require_cmd git
    [ -d "$CH_SRC/.git" ] || die "no git tree at $CH_SRC; run 'make ch-fetch' first"
    git -C "$CH_SRC" rev-parse --verify "$CH_BASE_TAG" >/dev/null 2>&1 \
        || die "$CH_BASE_TAG tag missing at $CH_SRC"

    shopt -s nullglob
    local patches=( "$PATCHES_DIR"/*.patch )

    local base head
    base=$(git -C "$CH_SRC" rev-parse "$CH_BASE_TAG")
    head=$(git -C "$CH_SRC" rev-parse HEAD)

    if [ "${#patches[@]}" -eq 0 ]; then
        if [ "$base" != "$head" ]; then
            log "no patches in $PATCHES_DIR but src tree has commits beyond $CH_BASE_TAG — assuming WIP, leaving as-is"
        else
            log "no patches in $PATCHES_DIR — nothing to apply"
        fi
        return 0
    fi

    if [ "$base" = "$head" ]; then
        log "applying ${#patches[@]} patch(es) from $PATCHES_DIR"
        git -C "$CH_SRC" -c user.name=deps -c user.email=deps@local am "${patches[@]}"
        return 0
    fi

    # HEAD differs from base: either patches are already applied (steady
    # state after a prior successful apply — re-running this stage should
    # be a no-op, not a failure) or there's WIP we must not clobber.
    # Distinguish by comparing commit subjects on base..HEAD against the
    # subject lines extracted from each patch file.
    local applied_count
    applied_count=$(git -C "$CH_SRC" rev-list --count "$CH_BASE_TAG..HEAD")
    if [ "$applied_count" -ne "${#patches[@]}" ]; then
        die "HEAD ($head) is $applied_count commit(s) past $CH_BASE_TAG but $PATCHES_DIR has ${#patches[@]} patch(es). Likely WIP not yet formatted.
Save WIP and reset:
  make ch-patches-format
  git -C $CH_SRC reset --hard $CH_BASE_TAG
Then re-run."
    fi

    local applied_subjects patch_subjects
    applied_subjects=$(git -C "$CH_SRC" log --reverse --format=%s "$CH_BASE_TAG..HEAD")
    # git format-patch emits "Subject: [PATCH N/M] <text>" and folds long
    # subjects across lines (RFC 2822 continuation: leading whitespace).
    # Extract Subject, strip the bracketed prefix, and unfold continuations
    # back into one line so the comparison matches `git log --format=%s`.
    patch_subjects=$(for p in "${patches[@]}"; do
        awk '
            /^Subject: / {
                sub(/^Subject: /, "")
                sub(/^\[PATCH[^]]*\] /, "")
                subj = $0
                capturing = 1
                next
            }
            capturing && /^[ \t]/ {
                sub(/^[ \t]+/, " ")
                subj = subj $0
                next
            }
            capturing {
                print subj
                capturing = 0
                exit
            }
            END { if (capturing) print subj }
        ' "$p"
    done)

    if [ "$applied_subjects" = "$patch_subjects" ]; then
        log "all ${#patches[@]} patch(es) already applied at HEAD — skipping git am"
        return 0
    fi

    die "HEAD ($head) is $applied_count commit(s) past $CH_BASE_TAG but commit subjects don't match $PATCHES_DIR.
Probable cause: WIP commits or hand-edits diverged from the tracked patch files.
Save WIP and reset:
  make ch-patches-format
  git -C $CH_SRC reset --hard $CH_BASE_TAG
Then re-run."
}

do_patches_format() {
    require_cmd git
    [ -d "$CH_SRC/.git" ] || die "no git tree at $CH_SRC"
    git -C "$CH_SRC" rev-parse --verify "$CH_BASE_TAG" >/dev/null 2>&1 \
        || die "$CH_BASE_TAG tag missing at $CH_SRC"

    mkdir -p "$PATCHES_DIR"
    rm -f "$PATCHES_DIR"/*.patch

    local base head
    base=$(git -C "$CH_SRC" rev-parse "$CH_BASE_TAG")
    head=$(git -C "$CH_SRC" rev-parse HEAD)
    if [ "$base" = "$head" ]; then
        log "HEAD == $CH_BASE_TAG; no commits to extract (cleared $PATCHES_DIR)"
        return 0
    fi
    git -C "$CH_SRC" format-patch "$CH_BASE_TAG..HEAD" -o "$PATCHES_DIR" >/dev/null
    local n
    n=$(find "$PATCHES_DIR" -maxdepth 1 -name '*.patch' | wc -l)
    log "extracted $n patch(es) to $PATCHES_DIR"
}

do_build() {
    require_cmd cargo rustc
    [ -f "$CH_SRC/Cargo.toml" ] || die "no source at $CH_SRC; run 'make ch-fetch' first"
    mkdir -p "$CH_BUILD_OUT"
    log "kernel source: $CH_SRC"
    log "cargo target:  $CH_BUILD_OUT"

    # Cross-compilation setup: when RUST_TARGET differs from the host
    # default, pass --target to cargo and point its linker env var at
    # the cross-toolchain gcc. Distros' gcc-aarch64-linux-gnu /
    # gcc-x86-64-linux-gnu provide the needed cross linker.
    local cargo_target_args=()
    local cargo_env=()
    local artifact_subdir="release"
    if [ -n "${RUST_TARGET:-}" ]; then
        local host_triple
        host_triple="$(rustc -vV 2>/dev/null | awk '/^host:/{print $2}')"
        if [ "$RUST_TARGET" != "$host_triple" ]; then
            require_cmd "${CROSS_PREFIX}gcc"
            cargo_target_args+=(--target "$RUST_TARGET")
            artifact_subdir="$RUST_TARGET/release"
            # Cargo reads CARGO_TARGET_<UPPER_UNDERSCORED_TRIPLE>_LINKER
            # to pick a linker for that target.
            local linker_var
            linker_var="CARGO_TARGET_$(echo "$RUST_TARGET" | tr 'a-z-' 'A-Z_')_LINKER"
            cargo_env+=("$linker_var=${CROSS_PREFIX}gcc")
            log "cross-compile mode: --target=$RUST_TARGET linker=${CROSS_PREFIX}gcc"

            # rustup target presence check — fail early with a clear message
            # rather than letting cargo emit a wall of ld errors.
            if command -v rustup >/dev/null 2>&1; then
                if ! rustup target list --installed 2>/dev/null | grep -q "^${RUST_TARGET}\$"; then
                    die "rust target $RUST_TARGET not installed. Run: rustup target add $RUST_TARGET"
                fi
            fi
        fi
    fi

    # Strip the builder's absolute paths from the binary's embedded source
    # references (panic messages + DWARF debug info): our source tree becomes
    # relative ("./vmm/src/...") and registry deps map to a stable "/cargo"
    # prefix, instead of baking in $CH_SRC and $CARGO_HOME. Any inherited
    # RUSTFLAGS is preserved. (Changing RUSTFLAGS invalidates the incremental
    # cache, so the next build is a one-off full recompile — required for the
    # remap to reach every crate.)
    local cargo_home="${CARGO_HOME:-$HOME/.cargo}"
    cargo_env+=("RUSTFLAGS=${RUSTFLAGS:+$RUSTFLAGS }--remap-path-prefix=$CH_SRC=. --remap-path-prefix=$cargo_home=/cargo")

    log "cargo build --release --bin cloud-hypervisor (cache hot ≈ seconds; cold ≈ 5-10 min)"
    env "${cargo_env[@]}" CARGO_TARGET_DIR="$CH_BUILD_OUT" cargo build --release --locked \
        "${cargo_target_args[@]}" \
        --manifest-path "$CH_SRC/Cargo.toml" --bin cloud-hypervisor
    mkdir -p "$BINDIR"
    cp "$CH_BUILD_OUT/$artifact_subdir/cloud-hypervisor" "$BINDIR/cloud-hypervisor"
    chmod +x "$BINDIR/cloud-hypervisor"
    log "built $BINDIR/cloud-hypervisor ($(du -h "$BINDIR/cloud-hypervisor" | cut -f1))"
    # --version only runs on host-native binaries; cross-builds show file info instead.
    if [ -z "${RUST_TARGET:-}" ] || [ "$RUST_TARGET" = "$(rustc -vV 2>/dev/null | awk '/^host:/{print $2}')" ]; then
        "$BINDIR/cloud-hypervisor" --version 2>&1 | head -1 || true
    else
        file "$BINDIR/cloud-hypervisor" 2>&1 | head -1 || true
    fi
}

case "$STAGE" in
    fetch)          do_fetch ;;
    patches-apply)  do_patches_apply ;;
    patches-format) do_patches_format ;;
    build)          do_build ;;
    *) die "unknown STAGE='$STAGE' (want fetch|patches-apply|patches-format|build)" ;;
esac
