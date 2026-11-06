[English](README.md) | [简体中文](README_zh.md)

# sandboxer/native-deps

This directory builds native artifacts consumed directly by `sandboxer` at
runtime but not included in its Go module. It currently contains patched
`cloud-hypervisor`. `vmlinux` and `envd` belong to
`guest-runtime/native-deps`; `librocksdb` belongs to `accelerator`. Host-installed
`erofs-utils` supplies `mkfs.erofs` and validation tools such as `fsck.erofs`
and `dump.erofs`; they are not project-built guest-runtime native artifacts.

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
the official package. Standalone builds use the published versions in `go.mod`. Coordinated builds
select the admitted sibling modules through a generated Go workspace with `use`
entries, leaving `go.mod` and `go.sum` unchanged; see the
[workspace workflow](../README.md#cross-repository-dependencies).

Licenses and notices live under `share/licenses/<component>`. Collection fails on missing notices, unreadable subtrees or partial traversals. Package validation checks required files, payload identities and archive paths, types and modes. Downloads use native GitHub asset sizes and digests. New packages and Releases contain no source or historical test records, independent `SHA256SUMS`, or source/Preview markers. The publisher uses human release notes, checks the current build outputs, and refuses to replace existing assets with different bytes.

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
claim that every listed crate's code is shipped. `Cargo.lock` and build records remain local collection inputs; packages contain the resulting licenses and notices. Native pin changes require updating the recipe and license collection together.

Git crate materials include the manifest's explicit `license-file`, even for a
nonstandard name or workspace-root file above the crate. Its path must stay
inside the selected repository and identify a tracked regular file; collection
reads the locked Git commit. Registry notices come from the matching crate
archive. The published `vhost` crate omits workspace-root licenses: supplemental
files come from the exact Git commit in its checksum-verified Cargo VCS record,
after comparing the upstream package manifest with that crate's
`Cargo.toml.orig`. A moving branch does not select those files.

Those three vhost HTTP requests share a 120-second budget. Each request has a
30-second deadline and at most three attempts for transient network or HTTP
failures. Request diagnostics identify the exact URL, attempt, elapsed time and
remaining budget. A complete set is cached under
`$CARGO_HOME/release-materials/vhost/<crate-sha256>/<git-commit>.json`.
Offline reuse verifies the repository, commit, crate checksum, exact file URLs,
material checksums and original manifest again. A damaged or mismatched entry
fails packaging without silently downloading a replacement; incomplete downloads
do not publish a cache entry or supplemental licenses. Cargo's own dependency
downloads retain their separate Cargo retry settings.

Crate material directories include a digest of the complete Cargo source
identity, preventing same-name/version packages from different registries or Git
commits from overwriting each other's notices. Rust copyright and license texts
come from the selected installation's `share/doc/rust/COPYRIGHT-library.html`
and `licenses/`, or the corresponding Debian/RPM source-package notices.
The collector uses the actual Rust installation to select its notices.
An explicit bare `RUSTC` command is resolved through the caller's `PATH`.
Missing notices produce an installation hint; collected files are regular files.

The matching Cloud Hypervisor link map selects the system static libraries and
startup objects actually used. Packaging uses their installed source-package identities to collect copyright/NOTICE and referenced license texts. Temporary objects use the matching CH/Rust notices. Distinct inputs cannot overwrite a shared material name. RPM collection
checks the installed-package listing and every same-source sibling file listing;
partial enumeration is an error even if another sibling supplied valid notices.
Installed package ownership is used for source attribution, not byte-for-byte
authentication against dpkg/RPM file digests or co-owner certification.

Standalone validation checks the bundle payloads, licenses, namespaces, paths and permissions; it needs no project/dependency checkout, Cargo download or native build. These materials support release review, not a legal certification or
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
  owner over a Unix socket, preserving logical balloon release before skipping
  redundant inode punching, restore-safe vsock, lifecycle barriers, ARM register
  preservation and applicable upstream clock/seccomp fixes. Each functional patch
  includes its corrections and tests; future maintenance and retirement conditions
  are recorded in the CH owner document.
- Patch files are stored in commit order in `deps/ch-patches/`.
- For an upstream CH upgrade, update the pin, fetch/import the intended source,
  reapply the patches, build, and run the sandboxer and platform E2E suites.
- `build/src/cloud-hypervisor` is the patch workspace. Do not remove it before
  exporting local patch work with `ch-patches-format`.

Patch semantics and the device model are specified in
[vmm.md](../docs/vmm.md).

## 4. Boundaries with other native dependencies

| Owner | Native artifact or tool |
| --- | --- |
| `guest-runtime/native-deps` | `vmlinux`, `envd` |
| Host `erofs-utils` installation | `mkfs.erofs`, `fsck.erofs`, `dump.erofs` |
| `sandboxer/native-deps` | Patched `cloud-hypervisor` |
| `accelerator` | `librocksdb` |

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
