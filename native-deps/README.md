[English](README.md) | [简体中文](README_zh.md)

# sandboxer/native-deps

This directory builds native artifacts consumed directly by `sandboxer` at
runtime but not included in its Go module. It currently contains patched
`cloud-hypervisor`. `vmlinux`, `mkfs.erofs`, `fsck.erofs`, and `envd` belong to
`guest-runtime/native-deps`; `librocksdb` belongs to `accelerator`.

## 1. Artifact

| Artifact | Source | Consumer |
| --- | --- | --- |
| `cloud-hypervisor` | Cloud Hypervisor v51.1 plus `deps/ch-patches/` | The VMM subprocess started by `sandbox-ctl` |

Output paths, relative to the sandboxer repository and its native-deps directory,
respectively, are:

```text
native-deps/bin/<arch>/cloud-hypervisor
../bin/<arch>/cloud-hypervisor
```

The native build itself produces `native-deps/bin/<arch>/cloud-hypervisor`.
The top-level sandboxer `make cloud-hypervisor` target copies that output to
`sandboxer/bin/<arch>/`, where release packaging places it beside `sandbox-ctl`
and where the default `sandbox-ctl --ch-binary` lookup expects it.

## 2. Build

Run the following commands from `sandboxer/native-deps`:

```bash
make build
make cloud-hypervisor
make cloud-hypervisor TARGET_ARCH=aarch64
```

A fresh `make cloud-hypervisor` performs these steps:

1. Download and cache the pinned Cloud Hypervisor source tarball.
2. Extract it into `build/src/cloud-hypervisor`.
3. Initialize the Git baseline and apply `deps/ch-patches/*.patch`.
4. Build `cloud-hypervisor` with Cargo when the target or its material records are absent.
5. Copy the artifact to this directory's `bin/<arch>/cloud-hypervisor`.

An existing target with its build report and link map is skipped. Delete that
output, or run `make clean`, before
rerunning to force a rebuild. In particular, formatting a changed patch does not
by itself invalidate the existing binary. `make clean` removes the native build
output and `bin/`, but preserves the source patch workspace and tarball cache.

Build from the selected source with the component Makefile. `release.sh package`
uses the matching binaries in `bin/<arch>`, or an explicit `RELEASE_BIN_DIR`;
it collects materials and creates the bundle without rebuilding those binaries
or resetting source/build caches. Keep the selected source checkouts, dependency
versions and native build records together with the outputs.

Packaging records the actual Go versions and effective module replacements.
Go/module LICENSE and NOTICE files come from the selected compiler installation
and matching module sources, preserving nested paths. Module resolution uses the
normal Go cache and routing; downloaded module checksums must match the binaries.
Only explicitly collected internal sibling dependencies use their own source
materials; an organization namespace alone does not exempt other modules.
Unsupported third-party local replacements need versioned module inputs for
the official package. Existing Kuasar local `replace` directives remain in use.

Materials live under `share/licenses/<component>` and
`share/sources/<component>`. The latter contains `SOURCES.tsv`,
`GO-BUILD-INFO.tsv`, `GO-MODULES.tsv` and `MATERIALS.sha256`.
Collection fails on missing notices, unreadable subtrees or partial traversals.
Independent validation checks the shipped inventory, checksums, required files,
source-record consistency, payload identities and archive paths/types/modes.
It does not fetch source checkouts or Go modules, compare notices with remote
source trees, or download/authenticate compiler distributions. Checksums and
VCS records are consistency checks, not proof of an arbitrary producer's identity.

The archive name identifies the requested release target. Project and internal
dependency records use a release version when its local tag matches the selected
commit, otherwise `git:<commit>`; packaging does not require creating future
target tags. The publisher passes the selected project SHA to validation before
Tag/Release writes, uses the bundle's `release-notes.md` body, and appends the
existing source/Preview markers. Trusted source selection, build/publish permission
separation and the refusal to replace published assets remain required.
Producer-supplied notes may not contain the publisher's reserved source/Preview markers.

The component package contains `sandbox-ctl`, `sandbox-init` and
`cloud-hypervisor`. Select matching Accelerator/Connector sources with the
existing `RELEASE_*_SOURCE_DIR`, `RELEASE_*_SOURCE_SHA` and
`RELEASE_*_VERSION` inputs. Ordinary local replacement builds do not require
remote target tags. Go VCS records must match the selected sandboxer commit.

Cloud Hypervisor packaging uses the selected prebuilt binary and its source
tree, by default `native-deps/build/src/cloud-hypervisor`; the existing
`RELEASE_CLOUD_HYPERVISOR_SOURCE_DIR` or `CLOUD_HYPERVISOR_SRC` can select
another matching location. The normal Cargo build retains
`build-report.jsonl` and `link.map` in
`native-deps/build/<arch>/cloud-hypervisor`.
`CLOUD_HYPERVISOR_BUILD_OUT`, `CH_BUILD_REPORT` and `CH_LINK_MAP` select
the existing output/record locations. If an older binary has no such records,
run `make -C native-deps ch-build` from the sandboxer root with matching sources;
packaging itself does not perform a fresh checkout or rebuild. Build flags and
the normal Cargo cache/routing remain available.
The existing report is kept incomplete until the binary and link map have been
copied successfully, so a failed build cannot satisfy the completed-output reuse
check. No additional completion marker or build receipt is required.

Python 3.11 or newer collects materials from the actual Cargo build report and
locked dependency graph. Registry crate archives must match `Cargo.lock`
checksums; identical archives in multiple registry cache namespaces are allowed,
and a matching archive is selected by that checksum. Git dependencies use the
lockfile's full commits. Only observed inputs are
listed, including build-time and procedural-macro dependencies; this is not a
claim that every listed crate's code is shipped. The collected `Cargo.lock`
and its source record must agree. Native pin changes require updating the
recipe, source records and materials together.

Git crate materials include the manifest's explicit `license-file`, even for a
nonstandard name or workspace-root file above the crate. Its path must stay
inside the selected repository and identify a tracked regular file; collection
reads the locked Git commit. Registry notices come from the matching crate
archive. The published `vhost` crate omits workspace-root licenses: supplemental
files come from the exact Git commit in its checksum-verified Cargo VCS record,
after comparing the upstream package manifest with that crate's
`Cargo.toml.orig`. A moving branch does not select those files.

Crate material directories include a digest of the complete Cargo source
identity, preventing same-name/version packages from different registries or Git
commits from overwriting each other's notices. Rust copyright and license texts
come from the selected installation's `share/doc/rust/COPYRIGHT-library.html`
and `licenses/`, or the corresponding Debian/RPM source-package notices.
Records include the actual Rust version and compiler-reported source commit.
An explicit bare `RUSTC` command is resolved through the caller's `PATH`.
Missing notices produce an installation hint; collected files are regular files.

The matching Cloud Hypervisor link map selects the system static libraries and
startup objects actually used. Packaging records their file digests and installed
source-package identities, and collects copyright/NOTICE and referenced license
texts. Temporary objects from that build are covered by the CH/Rust source
records. Distinct inputs cannot overwrite a shared material name. RPM collection
checks the installed-package listing and every same-source sibling file listing;
partial enumeration is an error even if another sibling supplied valid notices.
Installed package ownership is used for source attribution, not byte-for-byte
authentication against dpkg/RPM file digests or co-owner certification.

Standalone validation reads the bundle's own records and notices; it needs no
project/dependency checkout, Cargo download or native build. It retains the
component payload/material namespace, source-record, permission and checksum
checks. These materials support release review, not a legal certification or
isolation of untrusted CI candidates.

## 3. Patch development cycle

```bash
make ch-fetch
cd build/src/cloud-hypervisor
# Edit the source and record the changes with git commit.
cd ../../..
make ch-patches-format
make clean
make cloud-hypervisor
```

Conventions:

- Patches cover CH behavior required by the platform: external memfd memory
  zones, skipping user-managed RAM in snapshots, handing uffd to the external
  owner over a Unix socket, skipping `PUNCH_HOLE`/`MADV_DONTNEED` for already-empty
  file-backed balloon ranges, restore-safe vsock, and reliable
  pause/resume/ordered-shutdown barriers. Resident memory still follows the
  ordinary balloon release path.
- Patch files are stored in commit order in `deps/ch-patches/`.
- For an upstream CH upgrade, update the pin, fetch/import the intended source,
  reapply the patches, build, and run the sandboxer and platform E2E suites.
- `build/src/cloud-hypervisor` is the patch workspace. Do not remove it before
  exporting local patch work with `ch-patches-format`.

Patch semantics and the device model are specified in
[cloud-hypervisor.md](../docs/cloud-hypervisor.md).

## 4. Boundaries with other native dependencies

```text
guest-runtime/native-deps ──► vmlinux / mkfs.erofs / fsck.erofs / envd
sandboxer/native-deps     ──► cloud-hypervisor
accelerator               ──► librocksdb
```

`cloud-hypervisor` is a host VMM subprocess, not part of the guest runtime image.
`vmlinux` is not built here either: `guest-runtime` releases it independently and
`sandbox-ctl` selects it through configuration.

## 5. Validation

For a native build, verify the artifact produced in this directory:

```bash
make cloud-hypervisor
./bin/$(uname -m)/cloud-hypervisor --version
```

For broader validation, run from the `sandboxer/` repository root:

```bash
make test
make test-e2e-scripts
```

Product E2E runs separately from source-build checks. From a platform workspace
prepared with these prebuilt binaries, run:

```bash
python3 "$PREPARED/test/e2e/e2e" run --workdir "$PREPARED" --suite sandbox --suite snapshot
```

Real E2E requires `/dev/kvm`, the guest runtime, `vmlinux`, and the network/TAP
prerequisites of the selected cases. Exact candidate cases run from the
platform-prepared workspace and fail closed when selected prerequisites are
missing. Run the full platform gate in a suitable KVM environment before
release.
