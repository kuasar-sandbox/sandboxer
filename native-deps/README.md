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

Release packaging does not reuse that development binary or patch workspace.
It extracts the selected sandboxer commit into a temporary directory, verifies
the pinned Cloud Hypervisor tarball, applies that commit's patches, and performs
a locked build with a fresh private Cargo home. Python 3.11 or newer collects
source material from the actual Cargo build report: registry crate archives must
match `Cargo.lock` checksums, and Git dependencies must match its full commits.
Only observed build inputs are listed, including build-time/procedural-macro
dependencies; this is not a claim that every listed crate's code is shipped.
Editable extracted cache files are not the authority for registry licenses.
Only credential-free HTTPS registry routing from the caller's Cargo source
configuration is carried into the private home. Tokens, credential providers,
build wrappers and directory/git source overrides are not copied.
Native build commands receive an explicit environment allowlist and a private
home: Cargo tokens, cloud/release credentials and SSH-agent settings are not
inherited. Compiler/wrapper overrides are rejected for release packaging; the
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
The fresh final-link map also selects the system static libraries and startup
objects actually used by Cloud Hypervisor. Their installed source-package
identities, input digests, copyright and referenced license texts are included;
temporary objects from this build remain covered by the CH/Rust source records.
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
