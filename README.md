[English](README.md) | [简体中文](README_zh.md)

# sandboxer

`sandboxer` is the **MicroVM lifecycle engine** for [Kuasar Sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox). It creates sandboxes, captures and restores state, controls the guest, serves block devices, loads memory and disk data on demand, and coordinates each sandbox's cgroup, balloon, and VMM lifecycle.

The repository is both a component of the complete Kuasar Sandbox platform and an independently usable runtime engine. It exposes narrow Go packages and host-local protocols instead of importing the orchestration, storage-server, or eBPF implementations into its runtime closure.

## What it provides

- cold creation from a flattened image;
- creation of independent instances from a snapshot template;
- pause, snapshot, export, publish, restore, and resume of one logical sandbox;
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
| `sandbox-ctl` | Host control plane: run, snapshot, export, publish, restore, inspect, execute, and render configuration |
| `sandbox-init` | Guest PID 1: staged initialization, application supervision, vsock control, stdio multiplexing, and guest lifecycle responses |

`sandbox-init` is built here and packaged into `sandbox-runtime.bundle` by [`guest-runtime`](https://github.com/kuasar-sandbox/guest-runtime).

## Public integration packages

| Package | Purpose |
| --- | --- |
| `pkg/resource` | Node resource-control wire contract and client, consumed by `orchestrator` |
| `pkg/ctl` | Host-local control-socket protocol and policy callbacks for `ServeExecTunnel`; `ProxyExec` relays an already connected backend |
| `pkg/usage` | Host-only cumulative resource usage, snapshots, records and offline readers |
| `pkg/usagereader` | Shared lossless live/saved/history reads for the CLI and local conductor adapters |
| `pkg/sandbox`, `pkg/restore`, `pkg/snapshot` | Lifecycle, restore, and snapshot orchestration |
| `pkg/artifact` | Typed publication to a Manifest store or a named-location Bundle |
| `pkg/uffd`, `pkg/memory` | On-demand memory loading and memfd ownership |
| `pkg/vhost` | vhost-user-blk backend and copy-on-write data path |
| `pkg/guestlink`, `pkg/mux`, `pkg/proto`, `pkg/fwd`, `pkg/stdio` | Host/guest control, multiplexing, and forwarding |
| `pkg/config`, `pkg/resctl`, `pkg/chapi`, `pkg/tapfd` | Runtime configuration, resource execution, Cloud Hypervisor API, and TAP handoff |

## Data paths

The lifecycle layer can consume local files and Manifest-backed content through common sparse-data interfaces. A deployment can therefore place snapshots on local storage, a shared filesystem such as NAS/NFS, or object storage with caches. The orchestration API remains independent of the selected backend.

Memory is faulted in by page; disk data is read by block. Data that is never touched does not have to be fully loaded before a restored sandbox can continue.

## Build and test

Builds use environment-provided Go and inherit its `GOROOT` and `GOTOOLCHAIN` selection. Release automation requires a working `gh` with `api --slurp` support on `PATH`; the project does not install, replace, or authenticate these environment tools against fixed binary digests. Rust builds use the environment's `cargo`, `rustc`, and target linker, without requiring rustup management.

```bash
make build                      # sandbox-ctl and sandbox-init
make sandbox-ctl sandbox-init   # explicit binary targets
make build TARGET_ARCH=aarch64  # cross-compile; amd64/arm64 aliases are accepted
make vet test                   # static checks and unit tests
make test-e2e                   # component owner suite; requires the assembled project BIN
```

Requirements:

- Go 1.26.1 or newer for source builds;
- Linux and root privileges for real runtime operations;
- KVM and the required kernel interfaces for MicroVM E2E;
- `sandbox-runtime.bundle` and VMLinux from `guest-runtime`;
- the patched Cloud Hypervisor built by `sandboxer/native-deps`.

Unit and static checks do not prove that privileged KVM, TAP, cgroup, vhost, or restore paths have been exercised. A skipped privileged suite must not be presented as a completed integration validation.

## Cloud Hypervisor boundary

`sandboxer` carries the project patch set and build support for Cloud Hypervisor. The patch set enables the external-memory, userfaultfd, balloon, and lifecycle contracts required by the runtime. Upstream source, the exact patch target, build inputs, and license boundary are documented in [`docs/cloud-hypervisor.md`](docs/cloud-hypervisor.md) and [`LICENSE_SCOPE.md`](LICENSE_SCOPE.md).

Cloud Hypervisor remains third-party software under its upstream license. Project-original Go code is not used to relicense upstream code or patches that retain upstream copyright and licensing.

## Cross-repository dependencies

The Go import surface is intentionally narrow:

| Repository | Imported surface | Purpose |
| --- | --- | --- |
| `accelerator` | Manifest, cache/store clients, sparse/image helpers | Snapshot ingest/fetch, block reads, and flattened-image configuration |
| `connector` | `pkg/tapfd` | Receive TAP and network-namespace file descriptors |

Go-only source builds need this repository plus sibling `accelerator` and
`connector` checkouts. The tracked local `replace` directives remain active with
`GOWORK=off`. Internal `require` versions describe each component's target formal
release (Daily Preview uses that target without the preview suffix); the target
tag need not exist yet because builds use the sibling source. Record the actual
source SHAs for validation rather than treating that version label as the compiled
revision. Use the [project workspace](https://github.com/kuasar-sandbox/kuasar-sandbox)
for the complete system. Patched Cloud Hypervisor, Guest Runtime and Kernel are
separate native/runtime prerequisites, not dependencies of every Go-only check.

## Release model

Package source records keep an internal dependency's release version only when
its local Git tag matches the selected source commit. Untagged source builds
record `git:<commit>`; this does not require creating target release tags.

`sandboxer` publishes independent component versions named `vX.Y.Z`. The x86_64 archive contains `sandbox-ctl`, `sandbox-init`, and the patched `cloud-hypervisor` binary. Component documentation and E2E sources are collected from the selected tag into the project platform archive rather than duplicated in the component archive.

The project repository publishes an independently numbered aggregate `release-vX.Y.Z`, selecting an exact `sandboxer` tag together with exact versions of the other release units and validating the combined system.

See the [project release documentation](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release.md) and the [latest Stable aggregate release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases/latest).

## Documentation

- [sandbox.md](docs/sandbox.md) — host control and the entire sandbox lifecycle: configuration, output targets, resource usage, COW, artifact formats/publication, capture, restore, read recovery and cleanup.
- [sandbox-init.md](docs/sandbox-init.md) — Guest PID 1 ABI, initialization, vsock, raw usage observations, stdio and application lifecycle.
- [cloud-hypervisor.md](docs/cloud-hypervisor.md) — VMM integration, patches, build and runtime boundaries.

These three owner pairs provide complete English and Chinese contracts. Native-build and license-scope guides also have both editions. See the [documentation contribution policy](CONTRIBUTING.md#documentation) for section ownership and new-document requirements.

## Project boundaries

- node and cluster orchestration belongs to [`orchestrator`](https://github.com/kuasar-sandbox/orchestrator);
- data access, storage, encryption, and caching belong to [`accelerator`](https://github.com/kuasar-sandbox/accelerator);
- MicroVM networking belongs to [`connector`](https://github.com/kuasar-sandbox/connector);
- guest runtime image and kernel artifacts belong to [`guest-runtime`](https://github.com/kuasar-sandbox/guest-runtime);
- system-level design, shared integration tests, and aggregate releases belong to [`kuasar-sandbox/kuasar-sandbox`](https://github.com/kuasar-sandbox/kuasar-sandbox).

## Contributing and security

Read the [organization contribution guide](https://github.com/kuasar-sandbox/.github/blob/main/CONTRIBUTING.md). Changes to exported packages or cross-repository contracts must link companion pull requests in both directions and pass exact-source project validation.

Do not report vulnerabilities in a public issue. Use the [Kuasar Sandbox Security Policy](https://github.com/kuasar-sandbox/kuasar-sandbox/security/policy) and GitHub private vulnerability reporting.

## License

Original project content is licensed under the [Apache License 2.0](LICENSE). The Cloud Hypervisor and other third-party boundaries are documented in [`LICENSE_SCOPE.md`](LICENSE_SCOPE.md). Preserve upstream copyright, attribution, NOTICE, and SPDX declarations.
