[English](README.md) | [简体中文](README_zh.md)

# sandboxer

For the first cross-component sandbox or Demo, follow the project
[Quick Start](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart.md)
with the matching aggregate release and
[workbench](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/workbench/README.md).
This component can still be built/deployed independently; workbench is not a
production runtime dependency. Published architectures are specific to the
selected release; source support does not imply all historical assets exist.

`sandboxer` is the **MicroVM lifecycle engine** for [Kuasar Sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox). It creates sandboxes, captures and restores state, controls the guest, serves block devices, loads memory and disk data on demand, and coordinates each sandbox's cgroup, balloon, and VMM lifecycle.

The repository is both a component of the complete Kuasar Sandbox platform and an independently usable runtime engine. It exposes narrow Go packages and host-local protocols instead of importing the orchestration, storage-server, or eBPF implementations into its runtime closure.

## What it provides

- cold creation from a flattened image;
- creation of independent instances from a snapshot template;
- snapshot, live export and publication, with optional resume of the original runtime;
- on-demand guest-memory loading through an external memfd/userfaultfd path;
- a vhost-user block backend for local files or Manifest-backed data, with an instance-specific copy-on-write diff;
- controlled host/guest communication over vsock and a multiplexed stdio/port-forwarding path;
- a host-local control socket used by trusted node services after platform authorization;
- per-sandbox resource execution through cgroups and virtio-balloon coordination;
- TAP file-descriptor handoff from `connector`;
- package APIs for assembling and publishing image-class and snapshot artifacts.

Snapshot reuse is represented by explicit parent and child references. Multiple instances created from the same template can share immutable parent state while keeping independent writable changes. The runtime does not require independently running VMs to produce highly identical memory snapshots.

## Main binaries

| Binary | Purpose |
| --- | --- |
| `sandbox-ctl` | Host CLI: run/restore, exec, snapshot, export, reset, checkpoint administration, usage, info, publish and configuration |
| `cloud-hypervisor` | Patched host VMM for external memory, UFFD, balloon and lifecycle operations |

[`guest-runtime`](https://github.com/kuasar-sandbox/guest-runtime) owns and builds `sandbox-init`, publishes it in its `runtime-v...` archive, and embeds the same binary as `/sbin/init` in `sandbox-runtime.bundle`. This repository consumes its public guest ABI as a Go module dependency.

## Public integration packages

| Package | Purpose |
| --- | --- |
| `pkg/runtime` | Public Runtime SDK: `StartRuntime`, `Launch`, `Start`, `Restore`, live operations and owner lifetime |
| `pkg/resource` | Node resource-control wire contract and client, consumed by `orchestrator` |
| `pkg/ctl` | Host-local control-socket protocol and policy callbacks for `ServeExecTunnel`; `ProxyExec` relays an already connected backend |
| `pkg/usage` | Host-only cumulative resource usage, snapshots, records and offline readers |
| `pkg/usagereader` | Shared lossless live/saved/history reads for the CLI and local conductor adapters |
| `pkg/sandbox`, `pkg/restore`, `pkg/snapshot` | Lifecycle implementation, source preparation and capture helpers behind the public SDK |
| `pkg/artifact` | Typed publication to a Manifest store or a named-location Bundle |
| `pkg/uffd`, `pkg/memory` | On-demand memory loading and memfd ownership |
| `pkg/vhost` | vhost-user-blk transport, queues and guest memory |
| `pkg/diffstore` | [Writable disks, shared cache and single-layer/checkpoint storage](docs/block-storage.md) |
| `pkg/guestlink`, `pkg/stdio` | Host-side guest control and stdio integration |
| `pkg/mux`, `pkg/proto`, `pkg/fwd` | Source-compatible aliases/forwarders to `guest-runtime/pkg/guestabi/{mux,proto,fwd}` |
| `pkg/config`, `pkg/resctl`, `pkg/chapi`, `pkg/tapfd` | Runtime configuration, resource execution, Cloud Hypervisor API, and TAP handoff |

Upstream Go consumers (including `orchestrator`) retain the existing sandboxer SDK and compatibility import paths. Their source workspace and selected release dependency closure must also include `guest-runtime`; guest code depends only on its own ABI packages, never on the host SDK.

## Data paths

The lifecycle layer can consume local files and Manifest-backed content through common sparse-data interfaces. A deployment can therefore place snapshots on local storage, a shared filesystem such as NAS/NFS, or object storage with caches. The orchestration API remains independent of the selected backend.

Memory is faulted in by page; disk data is read by block. Data that is never touched does not have to be fully loaded before a restored sandbox can continue.

## Build and test

Builds use environment-provided Go and inherit its `GOROOT` and `GOTOOLCHAIN` selection. Release automation requires a working `gh` with `api --slurp` support on `PATH`; the project does not install, replace, or authenticate these environment tools against fixed binary digests. Rust builds use the environment's `cargo`, `rustc`, and target linker, without requiring rustup management.

CI selects a verified immutable Workbench image once for each native architecture.
Release builds, ordinary tests and packaging use build mode with an ordinary UID
on native x86_64 and aarch64 runners. The complete
`bash scripts/ci-source-checks.sh` gate runs in Workbench system mode, retaining
the source tests, race detector, vet and real mount-namespace ENOSPC checks.
The gate requires the actual namespace and mount capabilities used by those tests.
`KUASAR_BUILD_JOBS` bounds build concurrency; source, scratch, Cargo and native
cache directories belong to the invocation. Product E2E still consumes exact
prebuilt artifacts. Publishing credentials stay in the Actions orchestration
steps; the publisher executes the trusted workflow revision's archive parser
compiled in Workbench through `RELEASE_ARCHIVE_VALIDATOR`.

```bash
make build                      # sandbox-ctl and patched cloud-hypervisor
make sandbox-ctl                 # Go host binary only
make build TARGET_ARCH=aarch64  # cross-compile; amd64/arm64 aliases are accepted
make vet test                   # static checks and unit tests
make test-e2e-scripts           # source/helper regressions; not product E2E
```

Product E2E uses a platform workspace prepared from released/prebuilt artifacts.
Set `PREPARED` to that workspace. Manual and CI execution use the same runner,
without sibling source checkouts or Go/Rust compilers during execution.
Run the following command from a root shell with the required runtime tools on
`PATH`. The common runner does not elevate privileges; selected runtime cases
fail when root or KVM is missing.

```bash
python3 "$PREPARED/test/e2e/e2e" run --workdir "$PREPARED" \
  --suite sandbox --suite snapshot --suite telemetry \
  --include basic.sandbox-cli.sh --include image.manifest-boot.sh \
  --include image.sandbox-assembly.sh --include network.tapfd.sh
```

Requirements:

- Go 1.26.1 or newer for source builds;
- Linux and root privileges for real runtime operations;
- KVM and the required kernel interfaces for MicroVM E2E;
- `sandbox-runtime.bundle` and VMLinux from `guest-runtime`;
- the patched Cloud Hypervisor built by `sandboxer/native-deps`.

Unit and static checks do not prove that privileged KVM, TAP, cgroup, vhost, or restore paths have been exercised. A skipped privileged suite must not be presented as a completed integration validation.

## Cloud Hypervisor boundary

`sandboxer` carries the project patch set and build support for Cloud Hypervisor. The patch set enables the external-memory, userfaultfd, balloon, and lifecycle contracts required by the runtime. Upstream source, the exact patch target, build inputs, and license boundary are documented in [`docs/vmm.md`](docs/vmm.md) and [`LICENSE_SCOPE.md`](LICENSE_SCOPE.md).

Cloud Hypervisor remains third-party software under its upstream license. Project-original Go code is not used to relicense upstream code or patches that retain upstream copyright and licensing.

## Cross-repository dependencies

The Go import surface is intentionally narrow:

| Repository | Imported surface | Purpose |
| --- | --- | --- |
| `accelerator` | Manifest, cache/store clients, sparse/image helpers | Snapshot ingest/fetch, block reads, and flattened-image configuration |
| `connector` | `pkg/tapfd` | Receive TAP and network-namespace file descriptors |
| `guest-runtime` | `pkg/guestabi/{proto,mux,fwd}` | Guest control protocol, multiplexing and forwarding; no host lifecycle dependency in the guest |

Standalone Go builds use the obtainable dependency versions committed in `go.mod`.
For a coordinated Preview build, check out the selected `sandboxer`, `accelerator`,
`connector` and `guest-runtime` tags and generate their dependency workspace:

```bash
GOWORK="$(python3 ../kuasar-sandbox/ci/integration/go_workspace.py --root .. --owners sandboxer)" || exit 1
export GOWORK
make build
```

The workspace uses local modules without additional `replace` directives and leaves
`go.mod`/`go.sum` unchanged. Helper builds inherit the same selection. Patched Cloud
Hypervisor, Guest Runtime and Kernel remain separate native/runtime prerequisites.
Use the [project workspace](https://github.com/kuasar-sandbox/kuasar-sandbox) for the complete system.

## Release model

Shared Preview retention, cleanup, asset-completeness checks and Latest reconciliation are defined by the [project release contract](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release.md). The component-specific build inputs, package contents and helper selection follow below.

The selected owner source declares its complete compiled E2E helper inventory in `release/helper-inputs.json`. Its recipe, `scripts/build-e2e-helpers.sh`, accepts the target architecture and a fresh output directory. It runs only during the source-build stage with the admitted dependency workspace; package preparation and case execution consume its verified binaries. Internal build targets and package paths remain owned here.

`make release` verifies `ACCELERATOR_VERSION`, `CONNECTOR_VERSION` and `RUNTIME_VERSION` against clean sibling checkouts, binds the selected dependencies, then builds and packages those outputs. `RUNTIME_VERSION` is the artifact tag `runtime-vX.Y.Z` (or its Preview); the root `guest-runtime` Go module requirement is `vX.Y.Z`. Stable builds use committed `go.mod`/`go.sum` with `GOWORK=off`; Preview builds bind selected upstream Preview sources without downloading an unpublished future core. Set `KUASAR_PLATFORM_DIR` when the project checkout is not at `../kuasar-sandbox`. Missing tags, modules or conflicting source directories stop before compilation. Packages retain licenses and runtime files; they do not carry source or test records.

`sandboxer` publishes independent component versions named `vX.Y.Z`. New native-architecture archives contain only `sandbox-ctl` and the patched `cloud-hypervisor` binary, plus their notices. Historical sandboxer archives that contain `sandbox-init` remain readable and validated; new assembly obtains PID 1 from the selected runtime archive. The aggregate platform archive carries selected user guides and canonical E2E runtime inputs from its explicit documentation/test selections. Internal design documents and source-only tests remain in the source repositories.

The project repository publishes an independently numbered aggregate `release-vX.Y.Z`, selecting an exact `sandboxer` tag together with exact versions of the other release units and validating the combined system.

See the [project release documentation](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release.md) and the [latest Stable aggregate release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases/latest).

## Documentation

Start with the [standalone Runtime workflow](docs/runtime.md#standalone-workflow).
Use the [project Quick Start](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart.md)
for a platform deployment that combines components. The component handbook stays
flat under `docs/`; each topic keeps the operation, stable contract and mechanism
needed to understand one state owner or boundary.

| Topic | Use it for | Owns the exact contract |
| --- | --- | --- |
| [Runtime](docs/runtime.md) | Start, Launch, run, exec, snapshot, live export, restore and close one VM | Runtime state, API/CLI operation semantics, contexts and cross-resource capture |
| [Configuration](docs/configuration.md) | Prepare and inspect sandbox.yaml or host overlays | Fields, defaults, validation, merges and output destinations |
| [Artifacts](docs/artifacts.md) | Assemble E, reset retained state, inspect and publish E/S | Portable schemas, identity, dependency graphs, publication and retention |
| [Memory](docs/memory.md) | Understand lazy restore and checkpoint memory | Memfd ownership, UFFD, layered reads, released pages and dirty history |
| [Block storage](docs/block-storage.md) | Integrate Disk, operate disk checkpoints and diagnose block I/O | Vhost transport, immutable lowers, cache, DIFF v3/v4 and reclamation |
| [Resources](docs/resources.md) | Set resource policy and read usage | Capacity/Budget, cgroups, balloon, resource protocol and cumulative accounting |
| [Guest ABI](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md) | Prepare workloads or change host/Guest integration | sandbox-init phases, processes, filesystem, vsock, MUX and raw observations |
| [VMM](docs/vmm.md) | Build a compatible monitor or maintain patches | Cloud Hypervisor/KVM integration, device model, handoffs and patch contracts |

All eight topics have complete English and Chinese editions. Native-build and
license-scope guides also have both editions. Use the guides from the same source
or selected release as the binaries. The [contribution policy](CONTRIBUTING.md#documentation)
defines ownership and how to preserve maintained contracts when reorganizing.

## Project boundaries

- node and cluster orchestration belongs to [`orchestrator`](https://github.com/kuasar-sandbox/orchestrator);
- data access, storage, encryption, and caching belong to [`accelerator`](https://github.com/kuasar-sandbox/accelerator);
- MicroVM networking belongs to [`connector`](https://github.com/kuasar-sandbox/connector);
- guest PID 1, guest ABI, runtime image and kernel artifacts belong to [`guest-runtime`](https://github.com/kuasar-sandbox/guest-runtime);
- system-level design, shared integration tests, and aggregate releases belong to [`kuasar-sandbox/kuasar-sandbox`](https://github.com/kuasar-sandbox/kuasar-sandbox).

## Contributing and security

Read the [organization contribution guide](https://github.com/kuasar-sandbox/.github/blob/main/CONTRIBUTING.md). Changes to exported packages or cross-repository contracts must link companion pull requests in both directions and pass exact-source project validation.

Do not report vulnerabilities in a public issue. Use the [Kuasar Sandbox Security Policy](https://github.com/kuasar-sandbox/kuasar-sandbox/security/policy) and GitHub private vulnerability reporting.

## License

Original project content is licensed under the [Apache License 2.0](LICENSE). The Cloud Hypervisor and other third-party boundaries are documented in [`LICENSE_SCOPE.md`](LICENSE_SCOPE.md). Preserve upstream copyright, attribution, NOTICE, and SPDX declarations.
