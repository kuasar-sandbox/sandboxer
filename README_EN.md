# Kuasar Sandboxer

[简体中文](README.md) · [Kuasar Sandbox project](https://github.com/kuasar-sandbox/kuasar-sandbox)

`sandboxer` is the MicroVM lifecycle engine of Kuasar Sandbox. It creates and restores sandboxes, coordinates guest initialization and control, exposes block devices and on-demand memory paths, manages instance-local writable state, and provides the host-side execution boundary used by higher-level node services.

The component is designed for use inside the complete Kuasar Sandbox platform and as an independently integrated MicroVM runtime. System-level documentation, exact cross-component validation, demos, and aggregate releases are maintained in the project repository.

## User-visible lifecycle

The snapshot infrastructure supports two primary user scenarios:

### Template to instances (1:N)

An initialized environment can be saved as a template and used to create multiple isolated instances. Instances share the immutable template parent layer while each instance has its own identity and writable state.

```text
image → initialized template ─┬→ instance A + private changes
                              ├→ instance B + private changes
                              └→ instance C + private changes
```

### Pause and resume one logical instance (1:1)

A running sandbox can preserve guest memory, processes, and filesystem changes, release runtime resources, and later resume as the same logical sandbox. Higher layers can keep a stable sandbox identity even when restore occurs on another node.

These are different user workflows built on the same layered snapshot and restore foundation. A warm pool, when used by a platform, is a higher-level preparation policy rather than a separate snapshot mechanism.

## Main capabilities

- MicroVM create, start, pause, snapshot, restore, stop, and cleanup lifecycle;
- guest initialization, process supervision, and host–guest control channels;
- memory images loaded on demand by page rather than requiring a full eager restore;
- block-device data loaded on demand, with instance changes written to private copy-on-write state;
- local-file and manifest-backed data references consumed through common runtime interfaces;
- snapshot layering and parent-reference reuse;
- resource execution through cgroups, balloon coordination, and VMM lifecycle serialization;
- network attachment through the connector boundary;
- native guest command execution through scoped host-controlled paths;
- failure rollback and cleanup for partially created runtime resources.

Implementation-specific mechanisms such as userfaultfd, memfd, vhost-user block devices, vsock, and VMM APIs are documented in the component design documents. The public entry point focuses first on the behavior these mechanisms provide.

## Data and component boundaries

| Dependency | Boundary |
| --- | --- |
| [`accelerator`](https://github.com/kuasar-sandbox/accelerator) | Supplies common file/manifest data references, sparse artifacts, stores, caches, encryption, and image/snapshot access. Shared NAS-backed files do not have to be converted into content chunks. |
| [`connector`](https://github.com/kuasar-sandbox/connector) | Creates the sandbox network attachment and transfers the interface contract without embedding higher-level network policy in the runtime. |
| [`guest-runtime`](https://github.com/kuasar-sandbox/guest-runtime) | Supplies the guest Runtime and VMLinux artifacts and the guest-side initialization/control implementation. |
| [`orchestrator`](https://github.com/kuasar-sandbox/orchestrator) | Owns node and cluster APIs, stable identities, admission, placement, routing, builds, and higher-level lifecycle coordination. |
| [`kuasar-sandbox`](https://github.com/kuasar-sandbox/kuasar-sandbox) | Selects exact component versions and validates the integrated platform. |

`sandboxer` owns the per-sandbox resource execution loop. Node-level admission, shared resource accounting, grants, and recovery belong to the orchestrator's node Reservation Controller.

## Storage choices

The runtime is not tied to one storage model. Depending on the deployment and resource type, a sandbox can consume:

- **local files** for single-node, local-NVMe, or node-affine execution;
- **shared files** on NAS/NFS or another trusted shared filesystem;
- **manifest/object-storage references** for remote persistence, large-scale distribution, and layered caching.

Snapshot efficiency for template fan-out primarily comes from explicit parent-layer reuse. Content addressing and deduplication are additional data-layer capabilities for suitable stable artifacts; the runtime does not assume independently executed VM memory snapshots will have a universally high duplicate ratio.

## Build and test

This repository combines Go code with native runtime dependencies. For a standalone checkout, keep Go workspace overrides disabled unless you intentionally use the six-repository sibling workspace:

```bash
GOWORK=off go test ./...
GOWORK=off go build ./...
```

Use the repository `Makefile`, current Chinese README, and `docs/` tree as the authoritative source for:

- generated code and binary targets;
- Cloud Hypervisor source version and project patch set;
- native dependency download/build targets;
- required C libraries and toolchain packages;
- privileged component E2E commands.

### Test levels

- Unit and static checks should run without production credentials.
- Native build checks may require compiler toolchains and public upstream sources.
- Runtime E2E requires Linux, systemd, cgroup v2, `/dev/kvm`, root or equivalent privileges, TAP/network capabilities, and compatible guest artifacts.
- Cross-component changes are validated by the project BMS using an exact source set.

A machine without KVM or required privileges must report tests as skipped or unavailable, not as complete runtime validation.

## Cloud Hypervisor and third-party boundaries

The runtime uses Cloud Hypervisor as a VMM and may carry a project patch set against an identified upstream version. The repository documentation and release artifacts must preserve:

- the upstream source and version;
- patch provenance and applicability;
- upstream copyright and license notices;
- the distinction between project-owned Apache-2.0 code and third-party/native material;
- the source and redistribution obligations of bundled helper binaries.

Do not assume every file in a component release is Apache-2.0 solely because the repository's project-owned code is.

## Documentation

- [Project English overview](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/README_EN.md)
- [English Quick Start](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart_en.md)
- [Project architecture](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/kuasar-sandbox.md)
- [English release overview](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/releases_en.md)
- [Sandbox lifecycle documentation](docs/sandbox.md)
- [Guest initialization/control documentation](docs/sandbox-init.md)

Detailed design documents may remain Chinese during the initial source-publication phase. Build prerequisites, public interfaces, security boundaries, and release contracts must retain an accurate English entry point.

## Releases

The component publishes independent `sandboxer` versions. Aggregate Kuasar Sandbox releases select one exact sandboxer version with exact orchestrator, accelerator, connector, Runtime, and VMLinux versions.

- [Component releases](https://github.com/kuasar-sandbox/sandboxer/releases)
- [Aggregate releases](https://github.com/kuasar-sandbox/kuasar-sandbox/releases)

Do not combine component archives merely because their version numbers look similar; use the aggregate release selection.

## Contributing

Read the project [English contribution guide](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/CONTRIBUTING_EN.md). Keep ordinary runtime changes in this repository. Exported APIs, protocols, artifact contracts, or integration changes may require linked companion pull requests and exact-source-set BMS validation.

## Security

Do not report vulnerabilities in public issues. Use the project [English Security Policy](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/SECURITY_EN.md) and GitHub private vulnerability reporting.

Security-sensitive reports can include guest escape, host-facing parser errors, snapshot/data-boundary violations, scoped-exec bypass, unsafe VMM integration, resource-isolation failures, and privileged CI or native-supply-chain issues. Remove credentials, customer data, internal endpoints, and unredacted production logs from all public material.

## License

Project-owned code is licensed under the [Apache License 2.0](LICENSE). Cloud Hypervisor, Linux interfaces, generated code, third-party libraries, native tools, and bundled artifacts retain their own licenses and attribution requirements; consult the repository's license-scope documentation and release notices.