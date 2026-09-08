[English](sandbox.md) | [简体中文](sandbox_zh.md)

<a id="sandbox--沙箱控制与制品生命周期"></a>

# sandbox — Sandbox control and artifact lifecycle

`sandbox-ctl` is kuasar-sandbox's host control plane for one sandbox. It handles explicit cold starts, cold starts from Sandbox artifacts, memory restore, live export, image-to-Sandbox-E assembly without a VM, memory snapshots, artifact publication, and runtime `exec`/forwarding. The guest protocol is documented in [sandbox-init.md](sandbox-init.md).

This document describes the current format and behavior. The current reader rejects the old `snapshot.cfg` disk-graph schema; it provides no dual reader, automatic migration, or cross-version compatibility guarantee. This format boundary does not mean that the project has never published releases.

<a id="1-概述"></a>

## 1. Overview

<a id="artifact-model"></a>
### 1.1 Artifact model

The runtime consumes images, Sandbox E and Snapshot S. E describes portable configuration and the disk graph; S binds E to captured VMM/memory state. Logical objects are independent of file/named-location/Manifest/Bundle carriers. See [Sandbox artifacts](sandbox-artifacts.md) for full roles, fields and validation.


<a id="13-责任边界"></a>

<a id="13-responsibility-boundaries"></a>
### 1.2 Responsibility boundaries

`sandboxer` provides the complete E/S capabilities but does not define orchestrator's durable API. The recommended upper-layer mapping is:

```text
memory=true  -> kind=snapshot, ref=<Snapshot S>
memory=false -> kind=sandbox,  ref=<Sandbox E>
```

The ordinary data plane must not treat Sandbox E as memory state that can wake automatically. Starting from E requires explicit cold `Connect` semantics.

<a id="2-命令行接口"></a>

## 2. Command-line interface

<a id="21-子命令总览"></a>

### 2.1 Subcommands

```text
sandbox-ctl run
sandbox-ctl export
sandbox-ctl snapshot
sandbox-ctl exec
sandbox-ctl config
sandbox-ctl info
sandbox-ctl publish
sandbox-ctl upload-snapshot
```

`upload-snapshot` is a thin compatibility alias for `publish`; both call the same publisher and graph traversal.

### 2.2 `sandbox-ctl run`

The three input modes are strictly separate:

```bash
# Explicit cold start.
sandbox-ctl run --config sandbox.yaml --sandbox-id s1

# Cold start from Sandbox E.
sandbox-ctl run --from ./s1.sandbox --config host.yaml --sandbox-id s2

# Cold derivation from Sandbox E defaults with a completely new boot graph.
sandbox-ctl run --from ./s1.sandbox --replace-boot \
  --config replacement.yaml --sandbox-id s2-rebased

# Resume memory execution state from Snapshot S.
sandbox-ctl run --restore ./s1.snapshot --config restore-host.yaml --sandbox-id s3
```

RunRoot/BaseRoot are invocation-level roots; RunDir/BaseDir are the actual directories for this Sandbox. `SandboxID` always denotes logical identity. `PathID` is only the directory leaf shared under the two roots:

```text
pathID  = explicitly supplied PathID, otherwise SandboxID
RunDir  = RunRoot/pathID
BaseDir = BaseRoot/pathID
```

For example, a Build phase can retain a globally unique SandboxID while using a fixed private directory:

```bash
sandbox-ctl run \
  --config phase-a.yaml \
  --sandbox-id build-01-phase-a \
  --path-id a \
  --run-root /run/sandbox/builds/build-01 \
  --base-root /var/lib/sandbox/builds/build-01
```

PathID must be a nonempty, safe, single path component: neither `.` nor `..`, and containing no `/`, backslash, or NUL. It is not compared with SandboxID, and there is no PathID-to-SandboxID mapping. Default writable diffs for root/data disks reside in the PathID-derived BaseDir, but their filenames still use logical SandboxID, for example `BaseRoot/a/build-01-phase-a.overlay.diff` and `BaseRoot/a/build-01-phase-a.disk0.diff`. Cold, `--from`, and `--restore` modes use the same rule. Normal Sandbox exit cleans up only the current PathID's RunDir and owned diffs; it does not recursively remove the supplied RunRoot/BaseRoot or sibling PathIDs.

`--from` and `--restore` are mutually exclusive. Ownership of `--config` differs by mode:

- Ordinary run: a complete explicit cold configuration.
- `--from`: a host/instance overlay; E supplies the portable workload.
- `--from --replace-boot`: E supplies only non-boot portable defaults; the host configuration must supply the complete boot definition.
- `--restore`: host-only restore bindings/policy; S and its referenced E supply execution state and the portable workload.

`--config a.yaml:b.yaml` still merges files in order. `SANDBOX_CONFIG` can replace `--config`. `LoadConfigBytesWithPresence` supplies the same presence-aware rules for in-memory inputs such as config-socket.

`--from` and `--restore` support:

- Raw absolute or relative local paths.
- Scheme-qualified local tarstreams.
- `manifest://<key>`.
- `file://<bundle>@manifest:<key>`.
- Inference of a Bundle's default root key.
- Named `--ref-location name=file:///absolute/path` bindings.

Run's stdio, TTY, console, `--ready-fd`, connect forwarding, and resource parameters retain the same host control semantics in all three modes. Memory restore does not resend the cold launch specification.

### 2.3 `sandbox-ctl snapshot`

The following is command syntax; choose one output alternative and omit the square-bracket notation when executing:

```text
sandbox-ctl snapshot \
  --path-id a \
  --run-root /run/sandbox/builds/build-01 \
  (--output /artifacts | --upload) \
  [--mode local|bundle] \
  [--resume] \
  [--drop-caches=false] \
  [--merge-ref=true]
```

A Snapshot always contains memory execution state. One operation produces E and S at the same freeze point, committing S last. On success, the default is to destroy the VM. `--resume` resumes the original VM without making the new E/S its live baseline.

Human-readable local output lists both `Snapshot S` and `Sandbox E`. Upload stdout still contains only the S Manifest key for the existing orchestrator parser. `snapshot_done` returns both `snapshot_ref` and `sandbox_ref`; existing `memory_size`, `memory_resident`, pause/dump timing, and compatibility `overlay_*` response fields remain. `overlay_*` currently mirrors E's identity; only E contains the actual disk graph.

`--drop-caches` belongs only to memory snapshot and defaults to false. `--merge-ref` controls only local memory-parent merging and does not change disk provenance.

A local `snapshot` needs either `--sandbox-id` or `--path-id`. When both are supplied, PathID only selects `RunRoot/PathID/ctl.sock`; no identity consistency check is performed. `--output` may reside on the BaseRoot filesystem, for example `BuildBaseDir/checkpoint`. The running process cleans its own RunDir/owned diffs and does not treat output as a runtime directory to delete.

### 2.4 `sandbox-ctl export`

Live export syntax:

```text
sandbox-ctl export \
  --path-id a \
  --run-root /run/sandbox/builds/build-01 \
  (--output /artifacts | --upload) \
  [--mode local|bundle] \
  [--resume]
```

Image-to-Sandbox-E assembly syntax, without starting a VM:

```text
sandbox-ctl export \
  --from ./flattened.img \
  --config sandbox.yaml \
  --sandbox-id base \
  (--output /artifacts | --upload) \
  [--mode local|bundle]
```

Live mode rejects `--config` and reuses the running process's validated Manifest/ref-location/crypto bindings. Explicit `--manifest-config` and `--ref-location` therefore belong only to assembly mode. Assembly requires `--config` or `SANDBOX_CONFIG` and rejects `--resume`. Both modes support `--timeout`: live mode bounds the client's ctl-socket wait, while assembly sets a context deadline for its own operation; zero disables the respective CLI limit. Neither setting adds a server-side deadline to a live-export request. Export accepts neither `drop_caches` nor memory-merge parameters, does not call CH `/vm.snapshot`, does not read the memfd, and produces no memory refs.

Like local snapshot, live mode may use PathID alone. When SandboxID and PathID are both supplied, PathID only locates the ctl socket. Assembly rejects PathID; its existing `--sandbox-id` remains only an output-artifact alias.

### 2.5 `sandbox-ctl exec`

```bash
sandbox-ctl exec --path-id a --run-root /run/sandbox/builds/build-01 -- /bin/sh -c 'id'
```

Local exec may use only `--path-id`. Supplying both `--sandbox-id build-01-phase-a --path-id a` still dials only `RunRoot/a/ctl.sock`; the request gains neither a SandboxID nor a consistency check. Without PathID it continues to use `RunRoot/SandboxID/ctl.sock`.

Exec creates a sibling process through the current ctl/MUX. The export/snapshot quiesce gate atomically prevents new exec/forward sessions from entering the unstable window, and closes and joins already admitted sessions. In-flight execs are terminated and do not automatically rerun after `--resume`. Restore/attach reopens exec, forwarding, plugins, and app restart only after establishing the new MUX and thawing the application cgroup. Requests racing between ACK and thaw are rejected by the gate rather than forking into the frozen cgroup. Guest `attach` is an idempotent recovery operation. The host immediately retries once when the request/ACK boundary is ambiguous; lifecycle-context cancellation controls the entire dial/ACK sequence.

Remote authorized exec uses `pkg/ctl.ServeExecTunnel(ctx, options)` in this fixed order:

```text
Authorize -> AcceptDownstream -> ReadExecRequestFrame -> AuthorizeRequest
          -> DialBackend -> write frame.Raw once -> duplex relay
```

Before request admission succeeds, the caller must not dial the backend or trigger Sandbox lifecycle changes. The first frame's JSON payload is limited to 64 KiB, plus its four-byte length prefix. It must contain one `exec_request` with strict field sets, no duplicate fields, and a nonempty `exec.argv` array. Reading waits at most 10 seconds; callers may only shorten that limit. After admission, the original frame is written exactly once without decode/re-encode. Recognized request/backend rejections expose only the sanitized `exec request rejected`; `ProxyExec` remains available only for callers with an already connected backend.

### 2.6 `sandbox-ctl config`

```bash
sandbox-ctl config --template
sandbox-ctl config --config host.yaml:instance.yaml --check strict
sandbox-ctl config --config restore.yaml --mode restore --check strict
```

The template identifies portable, host-only, and ephemeral fields. Restore mode emits a host-only document, removing cold-only workload fields and immutable disk refs. It does not introduce a second restore schema.

### 2.7 `sandbox-ctl info`

```bash
sandbox-ctl info [--json] [--manifest-config manifest.yaml] <artifact>
```

- For E, output is `sandbox.runtime.cfg`.
- For S, default output is the original minimal `snapshot.cfg`, containing at least `sandbox_ref`.
- For S, `--json` preserves the resolved view needed by existing machine callers: `Version`, `SandboxRef`, and memory `FromRefs` come from S; `Resources`, `Boot`, `Launch`, and `Metadata` derive only from the E referenced by S. This view is not written back to S and is not a second snapshot provenance schema.
- Malformed, ambiguous, or logical roots that are neither E nor S fail closed.

Local crypto, Manifest, Bundle, selectors, and named ref-locations use the same opener as run/publish.

### 2.8 `sandbox-ctl publish`

```bash
# Publish to Manifest store.
sandbox-ctl publish --manifest-config manifest.yaml ./s1.snapshot

# Publish to a trusted named file location.
sandbox-ctl publish \
  --manifest-config manifest.yaml \
  --to-ref-location release=file:///srv/sandbox-artifacts \
  --ref-location source=file:///srv/source \
  ./s1.sandbox
```

The publisher strictly identifies local E/S carriers automatically. Already portable graph dependencies keep their original refs. A `manifest://` root is not materialized into a named location, and there is no Manifest tail rewrite. The publisher does not read `artifact.json` and has no artifact-kind registry.

<a id="3-配置与制品格式"></a>

## 3. Configuration and artifact formats

### 3.1 `sandbox.yaml`

`sandbox.yaml` is host input and may contain a portable workload, host policy, and instance data. This example illustrates ownership, not prescribed deployment values:

```yaml
resources:
  capacity: { cpu: 2, memory: 2GiB }
  allocatable: { cpu: 1.5, memory: 1536MiB }
  # Host-only policy:
  control: { cgroup_path: /sys/fs/cgroup/sandboxes/s1 }
  overhead: { memory: 128MiB }
  watermark_high: { ratio: 0.875 }
  startup: { memory: 512MiB }

network:
  # Host provider and current identity:
  tap: tap0
  interface: eth0
  ip: 169.254.1.1/31
  mac: "02:00:00:00:00:01"
  hostname: sandbox-1

boot:
  kernel: file:///opt/kuasar/vmlinux
  runtime: file:///opt/kuasar/sandbox-runtime.bundle
  cmdline: "console=hvc0"
  root:
    base: file:///images/root.erofs@digest:<digest>
    overlay:
      base: file:///layers/parent.overlay@digest:<digest>
      base_from_refs: []
      # Host-only active binding:
      diff: file:///var/lib/kuasar/s1.overlay.diff
      diff_template: file:///opt/kuasar/empty.ext4
      diff_size: 1GiB
  disks:
    - name: data
      base: file:///layers/data.overlay@digest:<digest>
      diff: file:///var/lib/kuasar/s1.data.diff

mounts:
  - { target: /data, type: disk, source: data }
  - { target: /tmp, type: tmpfs }
  - { target: /var/cache/app, type: empty }

files:
  - { path: /etc/app/config, content: "portable" }
ephemeral_files:
  - { path: /run/instance-token, content: "current-run-only" }

init:
  - { exec: /bin/sh, args: ["-c", "prepare-data"] }

launch:
  exec: /usr/bin/app
  args: ["serve"]
  env: { MODE: production }
  ephemeral_env: { REQUEST_ID: current-run-only }
  plugin:
    - { exec: /usr/bin/sidecar, restart: always }
  restart: never

metadata: { workload.kind: api }

restore:
  prefetch: off
timeouts:
  ch_api: 30s
```

Ordinary cold run performs full validation. `run --from` and `run --restore` first strictly parse the artifact, then apply their respective field-presence rules. An unconstrained `LoadMerged` must not overwrite the artifact graph.

<a id="portable-artifact-configuration"></a>
### 3.2 Portable artifact configuration

[Sandbox artifacts](sandbox-artifacts.md) owns C0/C1, PortableSandboxConfig, strict decoding, disk binding, E/S encoding and identity checks. The following section continues the runtime YAML file and environment behavior.


<a id="38-filesenv-与-ephemeral"></a>

<a id="38-files-environment-and-ephemeral-data"></a>
### 3.3 Files, environment, and ephemeral data

Cold-launch merging:

```text
files < ephemeral_files       # same path: ephemeral wins
launch.env < ephemeral_env    # same key: ephemeral wins
```

Duplicate paths within either files list are rejected. Persistent `files` and `launch.env` enter C0/C1; ephemeral fields do not enter the Portable schema.

Runtime semantics:

- sandbox-init injects `files` using tmpfs backing and bind mounts.
- Export saves file declarations, not runtime modifications to injected files.
- `run --from` reinjects persistent files and reapplies persistent environment.
- `ephemeral_files` and `ephemeral_env` affect only the current cold invocation.
- Ephemeral does not mean erased from a memory snapshot: RAM can still contain the data. If the guest copies it into an exported disk-backed file, excluding the declaration also does not erase that copy.
- Disk-backed `type: empty` volumes export with their owning root/data disk.
- `type: tmpfs` contents do not accompany export.
- `init` reruns on `run --from`.
- Plugins/apps restart with cold semantics.
- `run --restore` does not rerun launch/files/init/plugin.

If the restore host explicitly supplies `boot.cmdline`, persistent/ephemeral launch fields, mounts, files/ephemeral_files, init, or metadata, validation rejects them before side effects instead of silently ignoring them. `resources.startup` is host-only node policy and may be supplied on restore. It is included in the node reservation contract, but the Snapshot's captured `BudgetAtSnapshot` remains authoritative for the initial restore Budget.

<a id="4-资源模型"></a>

## 4. Resource model

<a id="41-三种部署模式"></a>

### 4.1 Three deployment modes

| Mode | `cgroup_path` | `controller` | Behavior |
|---|---|---|---|
| No cgroup | Empty | Empty | No cgroup writes; allocatable CPU must equal capacity CPU, and allocatable memory must remain within capacity |
| Static cgroup | Set | Empty | Configure local CPU/memory limits and optionally run the local sensor |
| Dynamic | Set | Set | Use resource-protocol admission/leases/Budget and run the local sensor |

`resources.capacity` is guest-visible VM capacity and enters Portable configuration. `resources.allocatable` is the cold-start workload default and also enters Portable configuration. `allocatable.cpu` must be finite and satisfy `0 < allocatable.cpu <= capacity.cpu`. Restore retains capacity and the captured `deflate_on_oom` from E, but can explicitly reapply the destination node's allocatable CPU/memory. This runtime policy does not rewrite E or C0. `control`, `overhead`, `watermark_high`, and `startup` are node policy and do not enter E.

Export/snapshot acquires the MemoryController mutation barrier and, before freezing, lifts and drains any `memory.high` operation that could race with CH pause. The host ping gate lets an admitted probe complete both `pong` and the guest-EOF transport barrier. This drain has its own 8-second quiesce budget; capture cannot wait forever merely because ordinary `timeouts.ping` has no forced timeout. On expiry, the host cancels and joins the probe, and failed capture enters full recovery. Guest quiesce also drains and pauses periodic `mem_report`, preventing S from capturing a reporter that holds the stream lock while waiting on the old host vsock. Restore switches to a new observation epoch before ACK; `--resume` and failed-capture attach recover the original epoch. A live attach only reopens the pause gate and does not corrupt accounting for already admitted reports. Recovery releases the host barrier after VM, MUX, app, and backends recover.

### 4.2 Memory terms

```text
CapacityMemory      = CH boot memory zone maximum
AllocatableMemory   = workload settled guest headroom
DemandMemory        = controller observation
Budget              = admitted/locally enforced working allowance
VMM memory.max      = CapacityMemory + host overhead
```

Memory S capture records CH configuration/state and sparse memfd content. Resource policy is not written to `snapshot.cfg`. Restore obtains capacity identity from E, and any explicitly supplied host capacity must match. Allocatable CPU/memory, `startup`, `overhead`, `watermark_high`, and resource-controller bindings come from the current host. `deflate_on_oom` must remain consistent with captured VMM state. The Snapshot's `BudgetAtSnapshot` remains authoritative for the initial restore Budget.

<a id="43-cpu-与-balloon"></a>

### 4.3 CPU and balloon

Capacity CPU determines vCPU topology. In cgroup mode, allocatable CPU maps to `cpu.weight`; without a cgroup it must equal capacity CPU, so a separate fractional allocation cannot be expressed. CH restore state authoritatively restores the current balloon state; the host does not invent another balloon state from S.

<a id="5-cold-start-与-run---from"></a>

## 5. Cold start and `run --from`

<a id="51-显式-cold-start"></a>

### 5.1 Explicit cold start

The ordinary `run --config` preparation and launch sequence is:

```text
T0 parse/merge/validate config and limits
T1 open/check immutable refs and image defaults; derive kernel/runtime identities
T2 project portable C0 and validate exactly one self
T3 preflight active diff requirements and source binding
T4 create run directory and atomically write sandbox.runtime.cfg once
T5 acquire controller/cgroup resources; materialize active diffs and network binding
T6 construct vhost block devices from payload-only streams
T7 spawn/configure CH
T8 launch sandbox-init spec and app
T9 establish MUX/pinger/forward/resource lifecycle
```

Predictable config/ref/format failures are intended to fail in preflight before controller/network/VM side effects. This does not promise that every later operation is side-effect-free: creating the run directory, writing C0, creating diffs, and acquiring resources are explicit subsequent steps that can fail and require cleanup. Kernel/runtime verification differs for an existing portable C0 as explained in [PortableSandboxConfig](sandbox-artifacts.md#32-portablesandboxconfig).

### 5.2 CH command-line boundary

Cold start constructs CH argv from host-bound kernel/runtime paths, memfd, vhost sockets, active diffs, and an optional network provider. Portable configuration never stores these absolute paths/fds.

Block devices bind only logical Payload:

- Container image or direct EROFS E: EROFS prefix only.
- Parent E as an ext4 layer: its root payload only.
- Current run-from E: `self` resolves only to the E payload section.
- The ZIP tail is never exposed to vhost.

CH stdin is fixed to `/dev/null`; sandbox-ctl bridges console output. The network provider is TAP or TapFD; portable topology records only NIC presence and interface name.

### 5.3 `run --from` rules

`ApplyFromRules` applies ownership using YAML field presence:

| Owner | Fields |
|---|---|
| Artifact strong | Immutable root/data graph, disk count/order/name, topology, self position, cmdline |
| Host strong | Actual kernel/runtime paths, active diff/template, cgroup/controller, network provider, timeouts, Manifest/crypto/ref-location, CH binary |
| Persistent override allowed | Resource workload defaults, launch, mounts, files, init, metadata |
| Instance-only | IP/MAC/hostname, ephemeral files/env, stdio/forward |

Conflicts with protected fields return explicit field context. Hosts cannot silently change disk graphs through omission or YAML merging. Persistent `mounts` may change ordinary tmpfs/empty declarations, but data-disk mount sources, targets, names, and order must retain artifact topology. A mount override cannot move a disk.

Networking must satisfy:

```text
portable network.enabled == host provider presence
portable interface       == explicitly supplied host interface
```

A direct EROFS E contains only a read-only image. The destination host must supply a preformatted `diff_template` or an explicitly bound, already formatted diff. A missing mountable upper fails before controller/network/CH side effects.

The final call is ordinary `sandbox.Run`; `run --from` does not enter `restore.Run`.

Default `run --from` continues to enforce these strong ownership rules. `--replace-boot` selects a separate, explicit, atomic cold derivation:

```text
source E's non-boot portable defaults
  + presence-aware persistent overrides
  + the host configuration's complete boot value
  + host/instance-only bindings
  -> ordinary ValidateCold / artifact preflight / ProjectPortableCold
```

Complete replacement covers `boot.kernel`, `boot.runtime`, `boot.cmdline`, `boot.root`, and the entire `boot.disks[]` together. There is no root-only or leaf-level graph merge. An omitted host `boot.disks` means zero data disks, not inheritance of the source disks. If inherited mounts still reference removed/renamed disks, the caller must also replace mounts. Ordinary cold disk/mount 1:1 validation rejects an inconsistent result before controller, cgroup, run-directory, network, or VM side effects.

Replacement does not reuse the source E's default kernel/runtime bindings, `self`, `RunSourceBinding`, or Bundle reader/fetcher. Host boot refs still undergo ordinary cold canonicalization, local crypto, Manifest/Bundle lookup, and preflight. The new C0 and subsequent E/S disk closure depend only on replacement boot. `--replace-boot` requires both `--from` and explicit `--config` (or `SANDBOX_CONFIG`) and can never be combined with `--restore`.

<a id="6-export-与-snapshot-数据流"></a>

## 6. Export and snapshot data flow

<a id="61-output-graph-与-commit-point"></a>

### 6.1 Output graph and commit point

Live export graph:

```text
data/lower disk artifacts first
              |
              v
          Sandbox E last  <-- operation root / alias commit
```

Memory snapshot graph:

```text
disk dependencies -> Sandbox E -> Snapshot S
                                  ^ operation root / alias commit
```

The Snapshot Bundle's root Manifest is S; the Export Bundle's root is E. E, data/lower Manifests, and S's memory dependencies belong to one planned Bundle graph. Before emitting the metadata prefix, the writer must complete admission, ordered source selection, parent copying, and the ref-replacement plan.

<a id="62-freeze-sequence-与-failure-recovery"></a>

### 6.2 Freeze sequence and failure recovery

Predictable preflight work completes before guest freeze:

```text
T0 output/Manifest/local-crypto/ref/Bundle/merge/config/source preflight
T1 enter MemoryController/Budget mutation barrier
T2 lock and lift/drain memory.high
T3 pause pinger and gate new host exec/forward; guest drains exec/forward and
   mem_report, freezes app, syncs, closes MUX; after quiesced+EOF the host
   joins residual exec/forward handlers
T4 pause CH, quiesce all block backends
T5 capture every data disk exactly once
T6 build C1 from immutable C0 + source binding + captured disk refs
T7 capture root once and emit Sandbox E
```

Export continues:

```text
T8 commit E last
T9 destroy, or --resume backend -> CH -> MUX/pinger/forward -> app -> barrier
```

Snapshot continues within the same T4 freeze:

```text
T8  CH /vm.snapshot -> config.json/state.json
T9  scan/capture memory self
T10 build S(snapshot.cfg.sandbox_ref=E, from_refs=memory parents)
T11 commit S last
T12 destroy, or --resume full recovery
```

Export never drops caches, invokes `/vm.snapshot`, or reads CH snapshot files or memfd. Snapshot reads disks once, rather than capturing them once for E and a second time through an old disk path.

BlockCOW SnapshotView is stable only while the backend is quiesced. V1 keeps the VM paused throughout sink reads; the VM cannot resume while the sink continues reading that view.

Failure semantics:

- Data artifacts/E can remain as content-addressed orphans. Reclamation belongs to the owning storage/retention policy; sandboxer does not promise automatic graph-aware collection of local output or a shared location.
- The root alias is committed only after E or S succeeds.
- `--resume` recovery after success or failure follows backend → CH → MUX → pinger/forward → app → resource barrier.
- With an ambiguous attach ACK, the host retries the idempotent operation once. If it still cannot establish MUX, capture becomes a terminal failure. While retaining memory/`memory.high` guards, the host requests VMM shutdown and uses bounded SIGTERM/SIGKILL fallback. It does not leave a VM that accepts new requests while the guest remains frozen.
- Default destruction also waits for CH exit. Failed or timed-out `/vmm.shutdown` falls back to bounded SIGTERM/SIGKILL before lifecycle guards are released.
- C0, active diffs, and the live lower graph remain unchanged.
- Local capture uses same-directory temporary files and cleans failed partial output. Named-location publication has its separate ownership-checked cleanup rules in [Local tarstream and crypto](sandbox-artifacts.md#112-local-tarstream-and-crypto).

### 6.3 `ctl.sock` protocol

The socket is always `RunRoot/PathID/ctl.sock`; omitted PathID defaults to SandboxID. PathID is a host-side locator and does not enter the wire request.

Snapshot and export use distinct request types:

```text
snapshot_request -> snapshot_done | error
export_request   -> export_done   | error
exec_request     -> exec_ack      | error
```

Export is not `snapshot_request{memory:false}`. Requests execute in the run process, reusing its lifecycle barrier, guest/MUX gate, CH API socket, and live vhost SnapshotView.

Responses do not echo secret values. Remote Manifest uploads may take a long time. For local snapshot/live export, CLI `--timeout=0` leaves the ctl connection without a deadline; a positive value bounds that client's wait. Neither value sets a server-side deadline field in the wire request. Server lifecycle contexts still govern cancellable I/O, and callers must not interpret a timed-out CLI as proof that no artifact was committed. Image-to-Sandbox-E assembly instead applies a positive timeout to its own operation context (§2.4).

### 6.4 Image-to-Sandbox-E assembly

Input must be a flattened `EROFS + ZIP(config.json)`, not an already wrapped `.sandbox`:

```text
open carrier -> validate flattened image -> retain config.json bytes
load explicit config -> apply image defaults -> build direct-EROFS C1
rebuild EROFS + ZIP(config.json,sandbox.runtime.cfg)
emit local/Manifest/Bundle E -> commit E root
```

This creates no VM and needs no freeze, but still validates portable identities, limits, carrier admission, and output atomicity. The CLI is a thin wrapper around package APIs:

```text
OpenFlattenedImage
  -> PrepareSandboxEConfig
  -> AssembleSandboxE (logical sparse.Source)
  -> caller-selected direct publisher
```

`AssembleSandboxE` does not create a complete intermediate `.sandbox`. It borrows the caller-owned `FlattenedImage`, preserves the payload's Hole/Zero/Data map and byte-for-byte `config.json`, and appends canonical `sandbox.runtime.cfg`. Context cancellation can interrupt opening, configuration-ref canonicalization, image validation, and later sink consumption. Malformed image, JSON, or portable configuration fails closed before a root ref is published. Embedded multitask processes must not switch tenants by changing global `MANIFEST_KEY`; they should pass a task-scoped resolver through `NewProcessStorageWithCustomerKey`. `ProcessStorage` evaluates it at most once and fixes the same customer key for its fetch, ingest, and local-codec operations.

<a id="7-memory-restore-数据流"></a>

## 7. Memory restore data flow

`run --restore S` handles only genuine memory Snapshots:

```text
T0 parse host config with presence
T1 open carrier and strict-parse S
T2 parse canonical snapshot.cfg
T3 open S.sandbox_ref and strict-parse E
T4 apply restore host-only rules to E config -> immutable lifecycle C0
T5 preflight kernel binding, compare runtime identity, and check disk sources
T6 atomically write run-dir C0
T7 bind self and reconstruct root/data disks from E
T8 create memfd and layer S.memory over S.from_refs
T9 create UFFD/va_report endpoints and CH restore argv
T10 start CH from config.json/state.json
T11 transfer UFFD descriptor(s) and complete restore handshake
T12 /vm.resume
T13 establish restore MUX and resume original process
T14 settle balloon/resource observation
T15 start pinger, forwarder, sensor, and steady lifecycle
```

Restore does not:

- Read a disk graph from `snapshot.cfg`.
- Cold-start on zero memory.
- Rerun init/files/launch/plugin.
- Treat E's `config.json` as a new process-launch request.
- Update memory parents or C0 as a re-snapshot side effect.

Allowed host-only fields include the network provider/current identity, cgroup/controller, allocatable CPU/memory and resource enforcement, actual kernel/runtime paths, active diff/template, restore prefetch, and timeouts. E owns the immutable disk graph, capacity identity, `deflate_on_oom`, and network topology. The kernel re-hash exception and runtime-footer comparison are described in [PortableSandboxConfig](sandbox-artifacts.md#32-portablesandboxconfig).

After restoring S0:

```text
C0 = E0 portable config
memory parent = S0

next snapshot:
  E1 = Export(C0,current disks)
  S1 = memory self + from_refs=[S0,...] + sandbox_ref=E1
```

After an explicit cold start or cold start from E, the memory-parent list is empty. `snapshot --resume` does not silently make S1 the current parent.

### 7.1 Memory prefetch

`restore.prefetch: memory` is a host-only optimization. It warms the current S self's file page cache or Manifest chunks, excluding parent memory layers and disk streams. The call receives the opened root stream, so its scope may also include S's bounded ZIP metadata tail; it is not a memory-payload-only section. It changes neither sparse truth, fault ordering, C0, S, nor memory parents. The default is `off`. An invalid configured mode fails validation before side effects; lack of prefetch capability or a prefetch I/O failure is best-effort and does not fail restore. It is logged and restore continues on demand. The asynchronous task is canceled and joined before its streams close. See [prefetch.go](../pkg/restore/prefetch.go) and its call in [restore.go](../pkg/restore/restore.go).

## 8. UFFD handler

Memory restore uses one shared memfd and a host handler that adopts CH-side userfaultfd descriptors. CH can report multiple regions of the same memory zone, for example when x86 RAM is split around the PCI hole; those descriptors share the handler's epoll set and address map. S self and `from_refs` are opened as a layered sparse source: resident top-layer data overrides lower layers, a top Hole falls through to its parent, and Zero remains explicit zero.

### 8.1 Single UFFD contract

The single-UFFD design means faults are handled on CH's mapping rather than registering a second UFFD on sandbox-ctl's backend mapping. It does not require exactly one CH descriptor for every layout. CH registers missing-page events for its restored memory regions; the host receives the descriptors through the restore handshake and validates region bounds and nonoverlap against the shared memfd address map. The first report starts the handler; later valid regions use `AddUffd`. Out-of-range faults, duplicate/invalid region registration, and truncated sources fail closed. See [serveandwait.go](../pkg/sandbox/serveandwait.go), [handler.go](../pkg/uffd/handler.go), and [addrmap.go](../pkg/uffd/addrmap.go).

### 8.2 Read path

UFFD uses two-stage population: one fault page followed by a best-effort serial tail. The fault worker first completes the fault page. It may submit the tail only after that page is actually committed as `Loaded`. Each handler retains at most one tail reservation; concurrent faults handle only their fault page when the reservation is busy. The fault page does not wait for a tail worker or tail ioctl. Concurrent source I/O, shared buffers, and guest population remain bounded. To reuse one physical decode result, `ChunkRun` completes the buffered source read described below before the urgent copy.

Population bounds depend on the final visible Run type:

| Run type | Fault stage | Tail stage | Total maximum per fault |
|---|---|---|---:|
| `Hole`, `Zero`, `Released` | One page via `UFFDIO_ZEROPAGE` | Up to 15 pages | 16 pages / 64 KiB |
| Ordinary `Data` | Read and `UFFDIO_COPY` one page | Deferred read/population of up to 15 pages | 16 pages / 64 KiB |
| Manifest `ChunkRun` | Read the final visible chunk window containing the fault once; populate the fault page separately first | Populate the following suffix, then the preceding prefix only after success | One current visible window, at most 1 MiB / 256 pages |
| Reclaimed `Loaded` | Restore only the current page | None | One page |

`ChunkRun` means the final serving leaf is a physical Manifest chunk. A chunk is the unit of decryption/decompression and, when enabled, ordinary content verification. The handler therefore avoids repeatedly reading it through ordinary Data's fixed window. `SnapshotReader` still returns only a forward anchor starting at the fault offset. Through a package-local optional capability, `StreamSnapshotSource` calls `fetch.ResolveChunkWindow` and uses metadata from the final composed Stream to expand that anchor to the largest contiguous visible window containing the fault within the same physical chunk. Resolution reads no payload. Upper Holes are transparent; upper Data/Zero, the memory section's end, and discontinuous visibility of the same lower chunk all truncate the window. Physical chunk bounds must not bypass the overlay.

The tail reservation is acquired before window resolution or payload reads. If busy, only the current 4 KiB fault page is read/populated from the original anchor. With the reservation, the handler reads a window no larger than 1 MiB into its existing buffer once. For an entirely visible chunk within that bound, this is an exact whole-chunk read. If an overlay truncates visibility, the source still performs the necessary chunk-level decryption, decompression, and configured verification internally; the handler receives only the contiguous window that is safe to populate. The fault worker takes the current page from the middle of the buffer for urgent `UFFDIO_COPY`. The tail worker first fills the adjacent suffix with one batch; only if that batch succeeds completely does it fill the adjacent prefix with one batch. Before executing a tail, it rechecks states outward from the fault's adjacent page and truncates at the first mismatch. Any state conflict, partial completion, or ioctl error stops more distant population and later stages without retry. One `ChunkRun` therefore issues at most one urgent copy and two tail copies.

Before workers start, an ordinary Data handler allocates a 64 KiB shared buffer; a source with chunk-window capability uses 1 MiB. A cold `ZeroSource` allocates neither buffer. Fault/tail paths do not grow these buffers or allocate temporary payload buffers; each fault worker holds a fixed 4 KiB urgent buffer. This is a constraint on the handler's additional payload allocation, not on source internals: Run objects, cache leases, and partial-chunk decode allocations remain the `SnapshotReader` implementation's responsibility. The handler calls `SnapshotReader.RunAt` once for a nonzero source; the optional window resolver performs further metadata `Stream.RunAt` calls only within accelerator. Ordinary Data and zero-like runs remain limited by the 64 KiB state boundary. Bidirectional `ChunkRun` candidates contain complete pages only. One scan under the state read lock clips them to contiguous `PageState` on both sides, RAM bounds, and the current CH UFFD region. These guest-population boundaries do not shorten an already admitted whole-chunk source read.

Default CDC chunks have a 1 MiB maximum, but Manifest configuration can admit larger chunks within its decoded-format limit. The UFFD buffer/window cap remains 1 MiB regardless. An unavailable, invalid, or over-limit expanded window retains the original safe forward anchor; if a buffered window cannot fit, the handler makes urgent-page progress without allocating a larger buffer. No oversized-chunk-specific population path is introduced. See [fault.go](../pkg/uffd/fault.go) and accelerator's [chunker configuration](https://github.com/kuasar-sandbox/accelerator/blob/main/pkg/manifest/chunker/chunker.go).

A tail issues a single multipage `UFFDIO_COPY` or `UFFDIO_ZEROPAGE` for its contiguous valid range; `ChunkRun` suffix and prefix each issue at most one. The kernel processes pages individually and may return a completed page-aligned prefix. The handler conditionally commits only that prefix as `Loaded`, abandoning the remaining tail without retry after the first conflict/error. Batching therefore retains the successful kernel-completed prefix while reducing the ordinary 16-page policy's tail to one ioctl and a fully visible 256-page window's 255-page tail to at most two.

`EVENT_REMOVE` makes discarded ranges missing again. Conditional tail-state updates must not overwrite concurrently produced `Released` state. Context cancellation stops workers and closes the owned descriptors/streams.

<a id="9-cgroup-与-balloon"></a>

## 9. Cgroups and balloon

### 9.1 Memory enforcement

Static/dynamic cgroup modes lift a potentially competing `memory.high` before CH pause and wait for existing high events to drain. Snapshot/export recovery restores the previous value. The VMM cgroup contains CH, not sandbox-ctl itself; sandbox-ctl is not charged to that workload Budget.

### 9.2 CPU

`capacity.cpu` determines the vCPU count. `allocatable.cpu` must be finite, positive, and no greater than capacity CPU; cgroup mode maps it to a clamped `cpu.weight`. This weight is relative contention policy, not a hard fractional-core quota; the VMM's `cpu.max` is based on capacity. Controllers may adjust grants during a lifecycle but do not rewrite C0 or the artifact.

### 9.3 Balloon

Balloon mutations serialize with snapshot/export through the same barrier. The freeze window cannot overlap asynchronous balloon resize. Restore takes CH state as the actual current value, then enters a new observation epoch; host YAML does not reconstruct the restored balloon's instantaneous state.

## 10. Resource protocol

### 10.1 Lifecycle states

```text
prepare -> admit -> start -> ready -> steady
                  \-> freeze -> capture -> resume|destroy
restore -> resume -> settle -> steady
```

Controller leases, heartbeats, and recovery inventory manage only host resource ownership and do not enter Portable configuration.

### 10.2 Capture barrier

Before snapshot/export, new Budget mutations and balloon transitions are blocked. Already running mutations must complete before freeze. Recovery releases the barrier only when the guest/app can continue; otherwise a liveness bug would permanently prevent resource updates after resume.

### 10.3 Pressure sensor

Static/dynamic modes can use PSI or `memory.events.local` polling. Host configuration determines the PSI trigger and debounce; they are not written to E. The sensor stops initiating growth during the capture gate and establishes a new observation epoch after restore.

<a id="artifact-provenance-and-publication"></a>
## 11. Artifact provenance and publication

See [Sandbox artifacts](sandbox-artifacts.md) for disk/memory references, tarstream/Manifest/Bundle outputs, exact publication and commit rules. This runtime specification retains capture sequencing.


## 12. vhost-user-blk backend

### 12.1 Payload boundary

The vhost backend receives the `fetch.Stream` Payload section, not FullStream. Including a `.sandbox` ZIP tail in the block device's logical size is a bug covered by format/unit tests.

### 12.2 Layered reads

Read order is active diff → captured top → `base_from_refs` → root image where applicable. Hole falls through; Zero/Data stop traversal. The immutable layers composed within one writable block device must have matching logical sizes. In the EROFS-plus-ext4 topology, the read-only EROFS base is a separate vhost device; it is not the final fall-through layer of the writable ext4 BlockCOW device. See [disks.go](../pkg/sandbox/disks.go) and [serveandwait.go](../pkg/sandbox/serveandwait.go).

### 12.3 SnapshotView

`BlockCOW.SnapshotView` exposes the decrypted upper-only logical view and authoritative hole map. It does not reopen the active-diff path and is stable only while the backend is quiesced.

### 12.4 BlockCOW state

BlockCOW uses a dirty bitmap for 4 KiB active-upper blocks, not a three-state discard map. Dirty blocks read from the diff; clean blocks fall through to the base, or return zeros when no base exists. The low-level `Discard` helper punches only complete blocks and clears their dirty bits, exposing the base again; it does not persist an explicit Zero that masks a lower layer. The current vhost profile advertises neither DISCARD nor WRITE_ZEROES, and its request dispatcher returns unsupported for both instead of calling this helper. Writing zero bytes keeps a block dirty and must not be scanned into Hole. Export/snapshot neither rotates the active diff nor makes the new E a backend base. See [blk_cow.go](../pkg/vhost/blk_cow.go), [server.go](../pkg/vhost/server.go), and [worker.go](../pkg/vhost/worker.go).

### 12.5 Quiesce / Resume

Quiesce waits for in-flight block requests to leave and prevents new ones. All data/root views are read within the same quiesce window. Recovery makes backends serviceable before resuming CH and guest connections, avoiding permanent blocking of block requests after VM resume.

<a id="13-validation错误与安全"></a>

## 13. Validation, errors, and security

### 13.1 Cold validation

Ordinary cold configuration validates at least:

- Capacity/allocatable consistency with resource mode.
- Absolute, unlocated file bindings for kernel/runtime.
- A unique network provider, and a provider for any identity fields.
- Valid root/data topology, disk names/order, and a 1:1 disk-to-mount relationship.
- Active diff/template/size combinations capable of yielding a mountable filesystem.
- Consistent local/Manifest/Bundle refs and crypto policy.
- Launch/files/init/plugin/metadata limits.

<a id="132-run---from-与-restore-validation"></a>

### 13.2 `run --from` and restore validation

Both modes strictly parse the logical artifact and canonical configuration before checking host ownership. `--from` permits persistent-workload overrides; restore rejects all cold-only fields. Kernel binding preflight, runtime identity comparison, network topology/provider, disk count/names/topology, and active-diff binding checks precede external lifecycle side effects. This does not add a kernel-digest re-hash to these two modes; see [PortableSandboxConfig](sandbox-artifacts.md#32-portablesandboxconfig).

### 13.3 CLI mutual exclusion

- `run --from` and `--restore` are mutually exclusive.
- `run --replace-boot` is allowed only with `--from`, requires host configuration, and rejects `--restore`.
- Export/snapshot require exactly one of `--output` and `--upload`.
- Explicit `--mode` and `--upload` are mutually exclusive.
- Live export rejects `--config`; image-to-Sandbox-E assembly requires `--from` plus `--config` or `SANDBOX_CONFIG`, and rejects `--resume`.
- Snapshot has no memory toggle.
- The exec command must follow `--`; local and proxy target rules are mutually exclusive.

### 13.4 Failure contract

- Errors provide field/entry/ref/disk-index context without printing inline file/env values, customer keys, or plaintext digests.
- Local crypto errors retain protected presentation.
- ZIP/path-traversal/symlink/regular-file checks fail closed.
- Remote I/O receives operation contexts; cancellation follows the owning I/O implementation and is not proof of rollback of already committed objects.
- Streams, fetchers, and Bundle readers share explicit close ownership; failure cleanup must not leak fds, mappings, or goroutines.
- Any post-freeze failure must recover app, CH/backends/MUX/pinger/forwarding and release resource locks. If MUX recovery can no longer be established, CH must be terminated while locks remain held and the capture gate becomes terminal.
- Successful operation-root publication is the commit point, followed by the semantic alias for local output. Named locations have no alias. Orphan dependencies do not constitute success.

<a id="14-reliabilityperformance-与兼容边界"></a>

## 14. Reliability, performance, and compatibility boundaries

<a id="142-streaming-与-memory-use"></a>

<a id="142-streaming-and-memory-use"></a>
### 14.1 Streaming and memory use

- Sparse tarstream does not spool the logical stream to disk.
- Manifest ingest reads resident extents rather than materializing holes.
- Large E/S payloads are not staged in the per-run `/run` directory; CH's bounded config/state staging files go there. Caller-selected output directories and named-location temporary Bundle files remain separate and consume their selected filesystem's capacity.
- Bundle dependency planning performs remote I/O and admission before pause.
- V1 `--resume` may keep the VM paused until sink writes finish, preserving SnapshotView stability.

<a id="143-performance-observations"></a>
### 14.2 Performance observations

Key metrics:

- Preflight latency.
- Guest freeze-ACK latency.
- CH pause latency.
- Disk/E emission time.
- CH memory-snapshot and resident-memory emission time.
- Resume/reattach latency.
- Manifest stored/deduplicated chunks.
- UFFD fault latency and restore-ready latency.

Benchmarks separately cover local tarstream/Bundle creation, Bundle reads, sparse merging, and UFFD faults. Performance optimization must not alter Hole/Zero/Data semantics, commit order, identity-verification policy, or freeze safety.

## 15. See Also

- [sandbox-init.md](sandbox-init.md) — Guest PID 1 and launch/quiesce/MUX protocols.
- [cloud-hypervisor.md](cloud-hypervisor.md) — CH build, API, and restore boundaries.
- [Connector TAPFD protocol](https://github.com/kuasar-sandbox/connector/blob/main/docs/tapfd.md) — TAP descriptor handoff and network namespaces, owned by connector.
- [timeouts-production.yaml](../examples/timeouts-production.yaml) — Production host-timeout example.
- [restore-prefetch-memory.yaml](../examples/restore-prefetch-memory.yaml) — Explicit memory-prefetch example.
- [README.md](../README.md) — Build, release, and repository entry points.
