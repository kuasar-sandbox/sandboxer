[English](README.md) | [简体中文](README_zh.md)

# sandboxer/native-deps

This directory builds native artifacts consumed directly by `sandboxer` at
runtime but not included in its Go module. It currently contains patched
`cloud-hypervisor`. `vmlinux`, `mkfs.erofs`, `fsck.erofs`, and `envd` belong to
`guest-runtime/native-deps`; `librocksdb` belongs to `accelerator`.

<a id="1-产物"></a>
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

<a id="2-构建"></a>
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
4. Build `cloud-hypervisor` with Cargo when the target output is absent.
5. Copy the artifact to this directory's `bin/<arch>/cloud-hypervisor`.

An existing target is skipped. Delete that output, or run `make clean`, before
rerunning to force a rebuild. In particular, formatting a changed patch does not
by itself invalidate the existing binary. `make clean` removes the native build
output and `bin/`, but preserves the source patch workspace and tarball cache.

The release workflow records the completed archive's SHA-256 as a build-job
output before uploading it. The publisher receives that independent value as
`RELEASE_ARCHIVE_SHA256` and checks it before any Tag or Release write; a value
recalculated from the downloaded bundle is not a substitute. This binds every
payload and material file to that completed build, even if the bundle's own
checksums are regenerated. Local packaging and standalone validation do not
require this publication input. The receipt does not attest compiler provenance
or isolate untrusted candidate code.

Go dependency and toolchain downloads use fresh private module/VCS state, an
enabled checksum database and `GOAUTH=off`. They clear persisted Go settings, private-module
bypasses, Git configuration and caller credentials while retaining validated,
credential-free routing. Uploaded Go record keys must match the exact official
payload names before any source or toolchain download; path aliases are rejected.
These release checks do not change ordinary development module authentication.
Source inventories reject duplicate or excessive records before per-row work;
each metadata table is capped at 16 MiB and the source inventory at 16,384 rows.

The trusted publisher generates the standard release text and source/Preview
markers from its validated request. Downloaded `release-notes.md` is a local
bundle aid, not an authority for the public release body or reconciliation.

Release packaging does not reuse that development binary or patch workspace.
It also rebuilds `sandbox-ctl` and `sandbox-init` in fresh checkouts of the
selected sandboxer, accelerator and connector commits with `GOWORK=off` and
read-only module resolution. Ignored development inputs and old sibling
binaries are not copied; `RELEASE_BIN_DIR` overrides are rejected. The resulting
Go VCS information is checked against the selected project commit before staging.
The Go build uses the same credential-filtered environment policy, with private
build/module caches; credential-free HTTPS `GOPROXY` routing may be retained.
The configured `GOSUMDB` identity and optional credential-free HTTPS mirror are
preserved, as is `GOTOOLCHAIN` selection. Their defaults are `sum.golang.org`
and `local`, respectively; local-only CI does not silently enable automatic
compiler selection. Malformed or authenticated routing values fail before building.

Release packaging records the Go compiler selected in the fresh build context,
then compares its distribution inputs before and after building with the matching
`golang.org/toolchain` archive authenticated by the configured checksum database.
This covers the compiler, standard-library sources and other files in that
distribution; extra non-build `api`, `doc`, `misc` and `test` files in a full Go
installation are not authenticated or used as release license sources. The
standard `go.mod`/`_go.mod` installation transformation is accounted for.
Go license/notice bytes, including nested compiler and standard-library dependency
materials, come from the verified archive with their relative paths retained.
Standalone validation
rechecks their bytes, source URL and module h1. A version string or recomputed
bundle checksum cannot substitute for that source check. Verification requires
an enabled checksum database and its matching archive/cache; it may fetch
verification material with `GOTOOLCHAIN=local` but does not switch the build
compiler or silently enable automatic toolchain selection. These checks assume
the trusted build host and do not attest a compromised host.

The archive name remains the requested release target. The project source record
uses that version only when its local tag matches the selected commit, otherwise
`git:<commit>`. Validation binds both Go binaries and the project source URL/digest
to that commit; publication supplies the expected commit and rejects a mismatch
before any Tag or Release write.
Standalone validation compares the complete project license/notice set, including
nested `LICENSES`, with the selected commit's Git blobs. It rejects changed,
missing and extra files, even when bundle checksums have been regenerated.
Fetch that exact commit before validation; the trusted publisher fetches source
history for inspection without executing candidate source or helper files.
Validation also requires the pinned Cloud Hypervisor source URL/digest, the
selected project's patch-set URL/commit, and the pinned upstream `Cargo.lock`
URL and bytes. The expected lock digest is checked against the fresh source at
build time as well; changing the pin or lock requires updating that binding.
If `RELEASE_DEPENDENCIES` is supplied, validation requires exactly the requested
accelerator and connector release versions; missing, duplicate, unexpected or
conflicting bindings fail before publication. Ordinary local-replacement source
builds still do not require remote target tags.
License collection refuses unreadable subtrees and incomplete traversals rather
than publishing only the readable notices. Third-party local Go replacements
without authenticated module checksums are not supported in official component
packages; use versioned module replacements. Existing Kuasar sibling replacements
and ordinary source development are unchanged.
It extracts the selected sandboxer commit into a temporary directory, verifies
the pinned Cloud Hypervisor tarball, applies that commit's patches, and performs
a locked build with a fresh private Cargo home. Python 3.11 or newer collects
source material from the actual Cargo build report: registry crate archives must
match `Cargo.lock` checksums, and Git dependencies must match its full commits.
Only observed build inputs are listed, including build-time/procedural-macro
dependencies; this is not a claim that every listed crate's code is shipped.
Git crate materials include the manifest's explicit `license-file`, even for
nonstandard names or a workspace-root file above the crate. The path must stay
inside the selected repository and identify a tracked regular file; all collected
bytes come from the locked Git commit, not editable checkout notices.
Editable extracted cache files are not the authority for registry licenses.
Only credential-free HTTPS registry routing from the caller's Cargo source
configuration is carried into the private home. Tokens, credential providers,
build wrappers and directory/git source overrides are not copied.
Native build commands receive an explicit environment allowlist and a private
home: Cargo tokens, cloud/release credentials and SSH-agent settings are not
inherited. Compiler/wrapper overrides are rejected for release packaging; the
fresh release build also explicitly rejects nonempty `GOEXPERIMENT`, `RUSTFLAGS`
and `CARGO_ENCODED_RUSTFLAGS` instead of silently discarding requested settings.
Ordinary development builds continue to accept their existing build flags. The
selected toolchain's exact `rustc` executable is used both for Cargo and the
material record, including its digest. This is credential hygiene for trusted
release inputs, not a substitute for isolating untrusted CI candidates.
The standard `CARGO_NET_GIT_FETCH_WITH_CLI` boolean is preserved; use `false`
to select Cargo's built-in Git transport when the Git CLI transport is unavailable.
The published `vhost` crate omits its workspace-root licenses. Its supplemental
files come from the exact Git commit in its checksum-verified Cargo VCS record,
after comparing the upstream package manifest with `Cargo.toml.orig` from that
crate. No current branch or separately maintained version list selects them.

The component archive carries those crates' license/notice files and the Rust
toolchain's copyright and license materials in component-specific directories.
Crate directories also include a digest of the complete Cargo source identity,
so same-name/version packages from different registries or Git commits do not
overwrite one another's license files.
If two distinct native link inputs would use the same system-material name,
packaging refuses the collision before the second input can overwrite notices.
Rustup's standard-library notices come from the official `rustc` distribution,
not editable local documentation. Its HTTPS release manifest must match the
selected compiler's complete Git commit, release string and host; the complete
archive must match the manifest's SHA-256 before its generated library copyright
and license texts are collected. Stable selection uses the exact version;
beta/nightly selection uses a fixed installed date and verifies it against the
same full commit. `RUST-NOTICES.tsv` records the manifest/archive URLs and digests.
The toolchain is not replaced or installed by this check. See Rust's
[distribution layout](https://forge.rust-lang.org/infra/channel-layout.html).
Matching installed Debian/RPM source-package notices remain supported; missing
materials fail with an installation hint. Distribution Rust notice bytes must match their installed package digests
and source identity. Package-owned symlinks are copied as regular files only after
verifying the resolved target; all Debian Multi-Arch co-owners must agree. Referenced
common-license texts retain their own package identity. This does not attest the
host or its package database.
The final link map identifies the selected target sysroot. `RUST-STDLIB.tsv`
lists the relative paths and SHA-256 digests of that target's complete `.rlib`
input set, including standard-library bitcode consumed before final linking by
LTO; it does not claim every listed archive is linked into the result. Its digest
is bound into the Rust toolchain record. These are actual toolchain input digests, not an
assertion that a locally modified toolchain is an unmodified upstream release.
The fresh final-link map also selects the system static libraries and startup
objects actually used by Cloud Hypervisor. Their installed source-package
identities, input digests, copyright and referenced license texts are included;
temporary objects from this build remain covered by the CH/Rust source records.
Each installed system input must also match its file digest in the trusted
build host's Debian or RPM database; ownership alone is insufficient. Missing,
ambiguous or changed file records fail packaging. This checks installed file
integrity, not the trustworthiness of a compromised host or package database.
Copyright, license and NOTICE bytes also must match their installed package
digests and the linked input's source package; referenced Debian common-license
texts are verified against their own owners. Multi-Arch co-owners must all agree
on the bytes and required source identity. Missing, changed or conflicting
license records fail collection.
Unknown sources, missing materials, altered archives, and unsuccessful builds
fail packaging. Existing `RELEASE_CLOUD_HYPERVISOR_SOURCE_DIR` and upstream
tarball environment overrides are not accepted by the release packager; source
development overrides remain available through the Makefile. A pin change must
update and validate the build recipe and source records together. These checks
support release review, not a legal certification.

<a id="3-patch-开发循环"></a>
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

<a id="4-与其他-native-deps-的边界"></a>
## 4. Boundaries with other native dependencies

```text
guest-runtime/native-deps ──► vmlinux / mkfs.erofs / fsck.erofs / envd
sandboxer/native-deps     ──► cloud-hypervisor
accelerator               ──► librocksdb
```

`cloud-hypervisor` is a host VMM subprocess, not part of the guest runtime image.
`vmlinux` is not built here either: `guest-runtime` releases it independently and
`sandbox-ctl` selects it through configuration.

<a id="5-验证"></a>
## 5. Validation

For a native build, verify the artifact produced in this directory:

```bash
make cloud-hypervisor
./bin/$(uname -m)/cloud-hypervisor --version
```

For broader validation, run from the `sandboxer/` repository root:

```bash
make test
make test-e2e
```

The component E2E target uses the assembled platform binaries selected by
`E2E_BIN`. The complete platform build-and-test gate is
`make -C ../kuasar-sandbox test-e2e` from the sandboxer repository root; the old
`test-e2e-sandbox-cold` platform target does not exist.

Real E2E requires `/dev/kvm`, the guest runtime, `vmlinux`, and the network/TAP
prerequisites of the selected cases. Some individual scripts can skip missing
prerequisites when invoked directly, but `test/e2e/run_all.sh` exports
`REQUIRE_KVM=1`: the component and release gates fail rather than treating those
missing prerequisites as success. Run the full platform gate in a suitable KVM
environment before release.
