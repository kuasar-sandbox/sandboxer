[English](configuration.md) | [简体中文](configuration_zh.md)

# Sandbox configuration reference

This is the reference for host `sandbox.yaml`, ordered overlays, effective
defaults and output targets. Begin with the ownership example, then render the
effective configuration before running it. Configuration selects policies;
[Runtime](runtime.md) defines the operation that consumes them,
[resources](resources.md) defines Budget and accounting, and
[block storage](block-storage.md) defines the resulting storage lifetime.

Portable E/S schemas belong to [artifacts](artifacts.md#portable-config).
Guest messages and process semantics belong to [Guest ABI](guest-abi.md).
Manifest/store/crypto/cache configuration remains in accelerator; sharing a YAML
input does not make it part of the sandbox schema.

<a id="sandbox-yaml"></a>

## `sandbox.yaml`

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
  diff_cow: { cache_size: 32MiB, max_dirty_size: 16MiB, discard: auto }

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
usage:
  enabled: false
  sample_interval: 1s
  flush_interval: 5m
```

Ordinary cold run performs full validation. `run --from` and `run --restore` first strictly parse the artifact, then apply their respective field-presence rules. An unconstrained `LoadMerged` must not overwrite the artifact graph.

<a id="render-config"></a>

## `sandbox-ctl config`

```bash
sandbox-ctl config --template
sandbox-ctl config --config host.yaml:instance.yaml --check strict
sandbox-ctl config --config restore.yaml --mode restore --check strict
```

The template identifies portable, host-only, and ephemeral fields. Restore mode emits a host-only document, removing cold-only workload fields, `resources.diff_cow.discard` and immutable disk refs while retaining the cache budgets. It does not introduce a second restore schema.

<a id="loading-and-overrides"></a>

## Loading, overlays and validation

`sandbox-ctl run --config a.yaml:b.yaml` and `config --config a.yaml:b.yaml`
load files in order; `SANDBOX_CONFIG` supplies the path list when `--config` is
absent. The ordinary [`LoadMerged`](../pkg/config/config.go) loader sequentially
decodes into one value: later scalars and lists replace earlier values; maps and
structs merge recursively; omitted keys keep the earlier value. Defaults apply
once after all files. Rendering normalizes YAML and does not preserve comments.
`config --template` emits the commented authoring template instead.

Use `config --check strict` to validate values for the selected `--mode
default|restore`. This flag is not a promise that the historical top-level
ordinary YAML loader rejects every unknown field. Owned strict sub-schemas,
including `usage`, `resources.diff_cow` and `resources.watermark_high`, have their
own decoding checks. `run --from` and restore additionally retain field presence
and enforce artifact ownership; a zero or empty explicit value is not always
equivalent to omission. Do not use unconstrained ordinary merging to replace an
artifact's disk graph.

| Input mode | Workload and state | Host overrides |
| --- | --- | --- |
| Explicit cold `run --config` | Configuration plus image defaults; validated before VM side effects | All valid fields for this invocation |
| Cold `run --from E` | E supplies portable defaults and a protected disk topology | [Presence-aware from rules](#from-rules); `--replace-boot` replaces the complete boot object explicitly |
| `run --restore S` | S supplies execution state; its E supplies immutable workload and disk graph | Current host policy and bindings; no launch, init, files or mounts replay |

Restore rejects explicitly supplied `boot.cmdline`, `resources.diff_cow.discard`,
launch (including ephemeral environment and plugins), mounts,
files/ephemeral_files, init and metadata.
Current host network identity/provider, resource execution, runtime local binding,
active writable bindings, prefetch, usage and timeout policy may be supplied
subject to the captured topology and identity checks. A legacy `boot.kernel`
field is accepted but ignored, without an external-kernel identity check.
Startup policy may be recorded, but captured `BudgetAtSnapshot` controls initial restore reservation.
`config --mode restore` produces a host projection; it does not create a second
portable schema. See [`FieldPresence`](../pkg/config/presence.go), [from application](../pkg/config/applyfrom.go) and [restore application](../pkg/config/applyrestore.go)
and the [restore operation](runtime.md#restore).

<a id="field-reference"></a>

## Field reference and effective defaults

The tables cover the sandbox-owned YAML fields in
[`pkg/config`](../pkg/config/config.go). A size is a parsed byte-size string
such as `512MiB`; a duration is a Go duration string such as `30s`.
Defaults below describe explicit cold loading unless the row says otherwise;
artifact-derived operations retain the ownership rules above. Omitted optional
objects stay absent unless a consumer resolves a documented default.

### Resource fields

| Field | Type / default | Validation and effect |
| --- | --- | --- |
| `resources.capacity.cpu` | Integer; `1` when zero/unset | Positive fixed vCPU count; supported kernel/VMM capacity still constrains deployment |
| `resources.capacity.memory` | Size; required | Fixed guest RAM address-space capacity |
| `resources.allocatable.cpu` | Number; capacity CPU when zero/unset | Finite, positive, at most capacity; without a cgroup must equal capacity |
| `resources.allocatable.memory` | Size; capacity memory | Positive settled guest headroom, at most capacity; not an RSS target |
| `resources.allocatable.deflate_on_oom` | Boolean; `true` | Applies when the selected policy creates a balloon; portable workload property |
| `resources.control.cgroup_path` | Absolute existing directory; empty | Empty disables cgroup operations; the runtime neither creates nor removes the directory |
| `resources.control.controller` | Filesystem Unix socket path; empty | Enables dynamic mode with a cgroup; abstract sockets are unsupported |
| `resources.control.sensor.mode` | `psi`, `events_poll`, `none`; `psi` | Selects local pressure signal; PSI trigger rejection falls back to events polling |
| `resources.control.sensor.psi_some_stall_us` | Unsigned integer; `10000` | Explicit positive stall requires a window; stall must not exceed the explicit window |
| `resources.control.sensor.psi_some_window_us` | Unsigned integer; `1000000` | Microsecond PSI window; zero resolves the consumer default |
| `resources.control.sensor.min_interval_ms` | Integer; `100` | Positive value overrides request debounce; nonpositive resolves default |
| `resources.overhead.memory` | Size; `32MiB` with a cgroup | Adds VMM overhead to `memory.max`; explicit object requires a cgroup for a live run |
| `resources.watermark_high.ratio` | Number; `0.875` | Strictly between zero and one; explicit object requires a cgroup |
| `resources.startup.memory` | Size; capacity memory | Positive, at most capacity; independent of allocatable headroom |
| `resources.diff_cow.cache_size` | Size; `32MiB` | Sandbox-wide cache, positive 4 KiB multiple |
| `resources.diff_cow.max_dirty_size` | Size; derived from final cache | Positive 4 KiB multiple no larger than total; [derivation and overlays](#diff-cow-policy) |
| `resources.diff_cow.discard` | `auto` or `manual`; `auto` | Cold-start policy for all managed writable ext4 mounts; explicit input is rejected on memory restore; [policy](#diff-cow-policy) |

`CgroupFD` is an inherited Go/runtime capability, not a YAML field. The equations,
reservation protocol and failure behavior are defined in [resources](resources.md).

### Network and boot fields

| Field | Type / default | Validation and effect |
| --- | --- | --- |
| `network.tap` | String; empty | Attach to an existing host TAP; mutually exclusive with `tapfd` |
| `network.tapfd.exec` | String list | Helper argv; exactly one of exec or socket; bare helper names prefer the sandbox-ctl directory, then `PATH` |
| `network.tapfd.socket` | Absolute path | Persistent provider socket; requires a nonempty request |
| `network.tapfd.request` | String | Provider fields in socket mode; forbidden with exec mode |
| `network.tapfd.timeout` | Duration; handoff default `5s` | Bounds the complete handoff; empty/nonpositive resolution uses the handoff default |
| `network.mac` | String; empty | TAP mode permits CH assignment; TAPFD metadata supplies the actual MAC |
| `network.ip` | CIDR string; empty | Guest IPv4/IPv6 address; TAPFD supplies the address while retaining the configured mask |
| `network.mtu` | Integer; `0` | Zero keeps the kernel default; node configuration owns MTU |
| `network.nexthop` | IP string; empty | Optional default route |
| `network.hostname` | String; empty | Guest hostname |
| `network.interface` | String; `eth0` when a source exists | Without a network source all guest network fields must be empty |
| `boot.kernel` | Local absolute `file://` reference | Cold-start kernel binding; accepted but ignored on memory restore, which needs no external kernel. Cold identity/sidecar rules: [Runtime](runtime.md#runtime-sdk) |
| `boot.runtime` | Local absolute `file://` reference | Runtime Bundle binding; restore matches the captured digest scheme and declared footer digest, not the basename. The original basename is a sibling-lookup hint; [selection](runtime.md#restore) |
| `boot.cmdline` | String; empty | Extra arguments merged with automatic boot arguments; cold-only host input |
| `boot.root.base` | Artifact reference; optional in single-disk mode | Required EROFS lower in overlay mode; optional ext4 lower in single-disk mode |
| `boot.root.overlay` | Object; omitted selects single disk | Presence selects separate read-only lower and writable ext4 upper |
| `boot.root.diff`, `boot.root.diff_template`, `boot.root.base_from_refs` | Local reference, local reference, ordered ref list | Single-disk-only writable binding, provisioning template and lower chain; mutually exclusive with overlay |
| `boot.root.overlay.base`, `.base_from_refs`, `.diff`, `.diff_template` | Ref, ref list, local ref, local ref | Writable upper's immutable source chain and active binding; same rules as the single-disk fields |
| `boot.disks[].name` | Unique nonempty string | At most eight data disks, in stable array order |
| `boot.disks[].base`, `.overlay`, `.diff`, `.diff_template`, `.base_from_refs` | Same disk shape as root | Each data disk uses the same single/overlay source rules and has one or more disjoint `type: disk` mount projections on cold start |
| `boot.disks[].slot` | `{}` or `{size: 8GiB}` | Single-device slot bound at runtime |
| `boot.disks[].disk` | Complete DiskV1 | Cold launch from the same disk definition with an instance-private writer |

`diff` and `diff_template` are absolute local file references. Immutable refs may
use the supported local or Manifest carriers. A lower chain requires its base.
Cold writable ext4 devices need a mountable base/template/existing diff; a blank
file is not a filesystem. In ordinary non-checkpoint mode, automatic active diffs
are runtime-owned and removed on exit; explicit paths remain caller-owned.
With `--enable-checkpoint`, writable files instead belong to the derived checkpoint
directory and survive normal exit, including automatically selected paths. Follow
[checkpoint ownership and cleanup](block-storage.md#checkpoints) for that mode.
Capacity comes from the selected disk source, not a quota or resize flag.
See [capacity and migration](#disk-capacity).

### Late binding and directory projections

This configuration reserves a virtio-blk device named `input`. The guest installs autofs triggers at launch. Access before binding returns a retryable error. First access after binding mounts ext4 and projects the requested directory. Ordinary single-device and overlay data disks use the same mount syntax. Targets must be disjoint, as must source subdirectories of the same disk.
```yaml
boot:
  disks:
    - name: input
      slot: {}
mounts:
  - {type: disk, source: input, subpath: data, target: /data}
  - {type: disk, source: input, subpath: results, target: /out}
```

A slot with omitted `size` adopts the source capacity at bind time. A declared size must match the source exactly. List the receiver’s disks, export a complete disk definition from a committed E or S, then bind it on the receiver. The same export command accepts a saved whole-machine checkpoint.
```bash
sandbox-ctl disk list --path-id receiver
sandbox-ctl export --source-ref "$PAUSED_E" --disk work --output "$OUT" > work.disk.json
sandbox-ctl disk bind --path-id receiver --disk input --disk-spec work.disk.json
```

Local delivery creates a Bundle containing the required block layers. For delivery across hosts, use `--upload` with the appropriate Manifest configuration on both ends. To initialize a new disk, supply a JSON object with `kind: disk`, `version: 1`, and a `diff_template` reference. Bind can register named locations with `--ref-location NAME=file:///dir`.

### Workload fields

| Field | Type / default | Validation and effect |
| --- | --- | --- |
| `launch.exec`, `launch.args` | String and string list | Override image entrypoint/arguments; single-disk roots need explicit exec unless placeholder is selected |
| `launch.env`, `launch.ephemeral_env` | String maps | Persistent environment and current-invocation override; [merge rules](#files-and-environment) |
| `launch.workdir` | String; `/` after ordinary loading | Application working directory; image/source projection follows the corresponding launch merge rules |
| `launch.restart` | `never`, `on-failure`, `always`; `never` | Primary process supervision |
| `launch.cgroup_control` | Boolean; `false` | Enable application cgroup delegation; [Guest topology](guest-abi.md#application-cgroups) |
| `launch.placeholder` | Boolean; `false` | No-exec anchor; excludes exec and always restarts |
| `launch.pid_namespace` | `private`, `shared`; `private` | Private makes the application namespace PID 1; shared uses sandbox-init's reaper |
| `launch.user` | Identity string; image user, otherwise root | `uid:gid` or `name:group` |
| `launch.stop_signal` | Signal name/number; image value, otherwise `SIGTERM` | Shutdown signal |
| `launch.stop_grace_period` | Duration; `10s` | Grace before SIGKILL, represented as whole seconds to the Guest |
| `launch.start_timeout` | Duration; empty/zero means no forced limit | Host wait for `launch_ack`, including ordered init work |
| `launch.plugin[].exec`, `.args`, `.env`, `.workdir`, `.user` | String/list/map/string/string | Companion process declaration in the same workload root/cgroup; process ABI is in [Guest ABI](guest-abi.md#launch-spec) |
| `launch.plugin[].restart` | `never`, `on-failure`, `always`; `always` | A plugin exit never reboots the sandbox |
| `mounts[].target`, `.type`, `.source`, `.options` | Strings; type defaults `empty` | Type is `empty`, `tmpfs` or `disk`; disk source names one data disk; no declarative NFS mount type |
| `files[].path`, `.content`, `.mode`, `.owner`, `.read_only` | Strings plus boolean; content empty, mode `0644`, owner `0:0`, read_only `false` | Injected file declaration; absolute path, octal mode and Guest identity rules follow [LaunchSpec](guest-abi.md#launch-spec) |
| `ephemeral_files[]` | Same shape as `files[]` | Overrides persistent declarations by path for this cold invocation; excluded from portable metadata |
| `init[].exec`, `.args`, `.env`, `.workdir`, `.user` | String/list/map/string/string | Ordered one-shot commands before application startup |
| `init[].timeout` | Duration; empty/zero means no timeout | Nonzero exit or timeout aborts initialization |
| `metadata` | String map; empty | Opaque portable values; runtime does not interpret them |

### Restore, usage and deadlines

| Field | Default | Scope |
| --- | --- | --- |
| `restore.prefetch` | `off` | `off` or `memory`; best-effort current-S warmup, [memory contract](memory.md#prefetch) |
| `usage.enabled` | `false` | Host sampling and persistence; not portable |
| `usage.sample_interval` | `1s` | Positive duration; [validation](#usage-policy) |
| `usage.flush_interval` | `5m` | Append schedule, at least sample interval; does not request fsync |
| `timeouts.ch_api` | `60s` if omitted | Local CH API response; explicit zero removes the forced response limit |
| `timeouts.api_ready` | No forced timeout | Complete API/restore-Paused readiness barrier, including a stalled connected response |
| `timeouts.va_report` | No forced timeout | CH-to-host UFFD handoff |
| `timeouts.restore` | No forced timeout | Guest restore ACK and MUX establishment |
| `timeouts.ping` | No forced timeout | Host/Guest ping round trip |
| `timeouts.app_notify` | No forced timeout | One Guest launch-port message read |
| `timeouts.app_start` | No extra overall deadline | Post-ACK initial-launch confirmation; positive opts in; negative/invalid is rejected |

All `timeouts.*` fields must parse as Go durations. Except `app_start`, current
consumers resolve nonpositive values as no forced timeout; dial/connect probes
retain their separate bounds. A completed operation context does not own the
subsequent Runtime lifetime. `--timeout` on capture commands is separately scoped
by the [Runtime operation](runtime.md#snapshot). A positive ping fatal threshold
needs a bounded ping round trip to detect a connected but nonresponding Guest.
Use the [production timeout example](../examples/timeouts-production.yaml) as an
explicit policy choice, not as the product default.

<a id="from-rules"></a>

## `run --from` rules

`ApplyFromRules` applies ownership using YAML field presence:

| Owner | Fields |
|---|---|
| Artifact strong | Immutable root/data graph, disk count/order/name, topology, self position, cmdline |
| Host strong | Actual kernel/runtime paths, active diff/template, cgroup/controller, network provider, timeouts, usage, Manifest/crypto/ref-location, CH binary |
| Persistent override allowed | Resource workload defaults, launch, mounts, files, init, metadata |
| Instance-only | IP/MAC/hostname, ephemeral files/env, stdio/forward |

Conflicts with protected fields return explicit field context. Hosts cannot silently change disk graphs through omission or YAML merging. Persistent `mounts` may change ordinary tmpfs/empty declarations, but data-disk mount sources, targets, names, and order must retain artifact topology. A mount override cannot move a disk.

Networking must satisfy:

```text
portable network.enabled == host provider presence
portable interface       == explicitly supplied host interface
```

A direct EROFS E contains only a read-only image. The destination host must supply a preformatted `diff_template` or an explicitly bound, already formatted diff. A missing mountable upper fails before controller/network/CH side effects.

The final call is ordinary `sandbox.Run`; `run --from` does not enter `restore.Run`. The source-derived cold path resolves the target kernel/runtime identities and records them in a new C0 while preserving the effective source workload and disk graph. Source boot identities do not require an old kernel/runtime for cold startup; this does not relax the protected fields above. Memory restore instead preserves the captured C0 and requires a compatible local runtime binding, without an external kernel.

Default `run --from` continues to enforce these strong ownership rules. `--replace-boot` selects a separate, explicit, atomic cold derivation:

```text
source E's non-boot portable defaults
  + presence-aware persistent overrides
  + the host configuration's complete boot value
  + host/instance-only bindings
  -> ordinary ValidateCold / artifact preflight / ProjectPortableCold
```

Complete replacement covers `boot.kernel`, `boot.runtime`, `boot.cmdline`, `boot.root`, and the entire `boot.disks[]` together. There is no root-only or leaf-level graph merge. An omitted host `boot.disks` means zero data disks, not inheritance of the source disks. If inherited mounts still reference removed/renamed disks, the caller must also replace mounts. Ordinary cold disk mount topology validation rejects an inconsistent result before controller, cgroup, run-directory, network, or VM side effects.

Replacement does not reuse the source E's default kernel/runtime bindings, `self`, `RunSourceBinding`, or Bundle reader/fetcher. Host boot refs still undergo ordinary cold canonicalization, local crypto, Manifest/Bundle lookup, and preflight. The new C0 and subsequent E/S disk closure depend only on replacement boot. `--replace-boot` requires both `--from` and explicit `--config` (or `SANDBOX_CONFIG`) and can never be combined with `--restore`.

<a id="files-and-environment"></a>

## Files, environment, and ephemeral data

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

If the restore host explicitly supplies `boot.cmdline`, `resources.diff_cow.discard`, persistent/ephemeral launch fields, mounts, files/ephemeral_files, init, or metadata, validation rejects them before side effects instead of silently ignoring them. `resources.startup` is host-only node policy and may be supplied on restore. It is included in the node reservation contract, but the Snapshot's captured `BudgetAtSnapshot` remains authoritative for the initial restore Budget.

<a id="usage-policy"></a>

## Usage policy

```yaml
usage:
  enabled: true
  sample_interval: 1s
  flush_interval: 5m
```

The default is `enabled: false`, `sample_interval: 1s`,
`flush_interval: 5m`. Durations must be positive Go duration strings,
`flush_interval >= sample_interval`, and twice the sample interval must fit
the supported signed duration range. Unknown keys, duplicate keys and wrong
scalar types are errors. File overlays apply in order; omitted usage members
retain the preceding value and explicit false disables sampling.
`flush_interval` schedules record appends, not filesystem synchronization or
device-cache flushing.

Usage is host policy in ordinary cold start, `run --from`, and `run --restore`.
It is excluded from `PortableSandboxConfig`, E and S. Current host policy,
not the artifact, selects it after restore. See the
[host-overlay example](../examples/usage-enabled.yaml).

When disabled, the host creates no usage sampler, long-lived usage connection
or periodic save. Existing resource-control reporting and balloon policy are
unchanged. Guest disk assembly still retains its bounded private filesystem
handles so a later memory restore can enable usage; retaining a handle does
not schedule reads.

<a id="diff-cow-policy"></a>

## `resources.diff_cow`: cache ownership and mount policy

```yaml
resources:
  diff_cow:
    cache_size: 32MiB
    max_dirty_size: 16MiB
    discard: auto
```

`discard` accepts `auto` or `manual`; omission resolves to `auto` after the host
overlays are merged. Ordinary cold start and `run --from` use the current host
policy for all managed writable ext4 mounts:

| Policy | Mount option | Effect |
| --- | --- | --- |
| `auto` (default) | `discard` | Filesystem block release can automatically notify the backend, reducing obsolete active payload and subsequent disk exports. |
| `manual` | `nodiscard` | Automatic discard on filesystem block release is disabled; explicit Guest trim remains available. Reclamation and export savings depend on when trim is requested. |

The policy does not disable virtio-blk DISCARD or change backend zero/read
semantics. `manual` requires a Guest runtime advertising support; see the
[mount scope and compatibility contract](guest-abi.md#guest-block-discard).
The field is excluded from artifacts' Portable configuration. A cold boot from an
exported Sandbox E therefore uses the destination host's selection. Snapshot S
still captures Guest mount state. Memory restore retains
the captured mounts and does not remount: explicitly supplying either `auto` or
`manual` to `run --restore` is rejected; omit the field. `config --mode restore`
removes this leaf while preserving `cache_size` and `max_dirty_size`.

Go SDK callers select the policy through `RuntimeSpec.Resources.DiffCOW.Discard`
before `StartRuntime`; the runtime uses that selection when the workload is
launched, including after a live `runtime_ready` prewarm.

The two cache budget fields are positive multiples of 4 KiB, with
`0 < max_dirty_size <= cache_size`. An omitted `cache_size` defaults to 32 MiB.
An omitted `max_dirty_size` is derived from the final effective cache size: half
the cache, rounded down to a 4 KiB multiple, with a minimum of 4 KiB
(`max(4096, floor(cacheBytes / 2 / 4096) * 4096)`). With both omitted, including
old configs without `diff_cow`, the effective defaults remain 32 MiB / 16 MiB.
Setting only `cache_size: 64MiB` gives a 32 MiB dirty limit; cache sizes of 4 KiB
and 12 KiB both give a 4 KiB dirty limit.

Explicitly setting `max_dirty_size: 16MiB` keeps that value even after a later
cache override. Omitted dirty limits are derived after all overrides; applying
defaults or loading a config does not write the derived value into the source
configuration. Explicit dirty limits are never clamped or recomputed, and the
final pair must satisfy the relationship above (so a dirty-only 64 MiB config
fails against the default 32 MiB cache). These are finite engineering
defaults, not a production SLA or a claim of optimal performance. There is no
zero/unlimited cache value, cache disable switch, failed-data-I/O buffered retry or
configurable alternate write mode.

`max_dirty_size` is a subset of `cache_size`, never an additional pool or a
reserved partition. Clean pages may use the entire cache when there are no dirty
pages. Adding a disk does not multiply either limit. These host process resources
are independent of guest capacity, allocatable/startup headroom and VMM overhead:
`sandbox-ctl` is outside the VMM cgroup. No resource-controller changes are needed.

Cold start, `run --from`, and restore resolve cache budgets for this instance.
Runtime configuration retains those budgets for that sandbox's lifecycle; a new
restored instance uses its current host cache configuration. Portable C0/E/S never
contains cache policy, pages, queues or LRU state. Strict restore validation,
config merging and host projection preserve this boundary.

Go callers can pass one `diffstore.Cache` with `diffstore.WithCache` to multiple `diffstore.OpenDisk`
calls. Close the COWs before closing that cache. Without this option, a standalone
COW owns a finite default cache and closes it itself.

<a id="disk-capacity"></a>

## Writable disk capacity and migration

Writable disk capacity is inherited from the selected filesystem source, not
from a separate size setting. An existing non-empty active `diff` keeps its
logical capacity. A fresh diff initialized from `diff_template` inherits the
template's logical capacity; without a template, a fresh diff over a COW `base`
inherits that base's logical capacity. An empty existing diff or a fresh disk
without a filesystem source remains invalid. In two-device overlay mode, this
COW base belongs to the writable ext4 upper, not the read-only EROFS image.
These rules apply to both `boot.root` and `boot.disks[]` and remain unchanged
for cold starts, `run --from`, and memory restores.

The former `diff_size` setting never enforced capacity or a quota in these
supported paths. It has been removed, together with its misleading 1 GiB
default. Configurations that explicitly supply `diff_size` or the mistaken
`size` spelling directly under a root/data disk or its `overlay` now fail with
an actionable error, including empty and null values. Remove these keys from
existing configuration and provision a filesystem source with the required
capacity. New configuration output does not emit them. Unrelated fields and
opaque metadata are unaffected.

For new disposable ext4 work disks (overlay uppers, single roots and scratch
or data-disk fixtures), format the sparse template with `mkfs.ext4 -O ^has_journal`.
This avoids filesystem journal allocation and metadata journal writes; it does
not disable journald or application logs. COW is not a replacement for a journal,
and this default does not promise crash recovery of an interrupted work disk.
Existing journaled templates, user-supplied images and snapshots remain compatible;
guest sync/quiesce and snapshot/restore semantics are unchanged. Do not reformat
an existing data disk to apply this recommendation.

For a **new**, empty 512 MiB scratch filesystem, prepare a new template and
select it without a size override:

```bash
truncate -s 512M /tmp/scratch-512m.ext4
mkfs.ext4 -F -O ^has_journal /tmp/scratch-512m.ext4
```

```yaml
boot:
  # Keep the other required boot fields from the complete configuration.
  disks:
    - name: scratch
      diff_template: file:///tmp/scratch-512m.ext4
mounts:
  - target: /scratch
    type: disk
    source: scratch
```

Provisioning templates remain immutable inputs, distinct from active DIFF work
files. Existing raw ext4 preparation and encrypted-template reading remain
supported. The runtime reads the template and creates a separate v3/v4 working
file. Writable logical capacity must be positive and 4 KiB aligned. Single-layer
v3 supports up to 1 TiB; enabling local checkpoints selects v4, which remains
limited to 32 GiB. A larger single-layer work disk is selected by provisioning
a larger filesystem template; no separate size setting is introduced.

Use a new active diff: an existing diff takes precedence, so changing a template
does not resize or replace existing data. Do not truncate an existing filesystem
to impose a smaller limit. Sandboxer does not automatically resize filesystems,
cap materialized COW storage (including tmpfs file memory/swap; pending cache writeback is bounded separately by `resources.diff_cow`), or change disk capacity at startup or restore. Logical block
device capacity is distinct from an encrypted file's physical length, host disk
allocation, and the guest filesystem's available file-data space. This migration
does not introduce arbitrary per-instance capacity selection from one template.

<a id="journal-output-targets"></a>

## Journal output targets

Each journal output is configured independently:

```text
journald=<tag>[,<FIELD>=<VALUE>...]
```

`run` accepts this target in `--stdout-to`, `--stderr-to`, `--console`, and
`--log-to`. `exec` accepts it in its existing `--stdout-to` and `--stderr-to`
options; the guest console belongs to the running sandbox, not an exec client.

```bash
sandbox-ctl run --config sandbox.yaml \
  --stdout-to 'journald=app,WORKLOAD_ID=worker-42,STREAM=stdout' \
  --stderr-to 'journald=app,WORKLOAD_ID=worker-42,STREAM=stderr' \
  --console 'journald=console,WORKLOAD_ID=worker-42' \
  --log-to 'journald=sandbox-ctl,WORKLOAD_ID=worker-42'
```

The tag becomes `SYSLOG_IDENTIFIER`. Extra fields belong only to that output:
there is no global field option, environment discovery, interpolation, or
inheritance between targets. Equal tags do not share fields or partial-line
buffers. A bare `journald=app` remains valid and has no extra fields. Callers
that previously relied on implicit identity environment variables must now
include their fields explicitly in every required target.

Sandboxer does not choose or interpret application, orchestration, or identity
fields. The caller selects the tag and non-secret values. They are host-side
output configuration: they do not enter guest environment variables, the guest
stdio protocol, sandbox YAML, portable artifacts, or snapshots. Every new run
or exec invocation supplies its own targets.

<a id="output-syntax"></a>

### Syntax and encoding

The tag is a nonempty `[A-Za-z0-9_-]` token. Field names are 1–64 ASCII bytes,
start with `A`–`Z`, and contain only uppercase letters, digits, and underscores.
Names beginning with `_` are reserved for journal trusted metadata. This
interface also reserves `MESSAGE`, `PRIORITY`, and `SYSLOG_IDENTIFIER` for the
writer and rejects duplicate field names. This single-value interface does
not imply that the underlying journal protocol forbids all repeated fields.

Fields are separated by literal commas. Each field is split at its first `=`:
`FIELD=a=b` has value `a=b`, and `FIELD=` explicitly supplies an empty value.
Values use percent encoding with Go `url.PathEscape`/`url.PathUnescape`
semantics. Splitting happens before decoding, and decoding happens once:

```text
journald=app,NOTE=a%2Cb,EXPR=x=y,PERCENT=100%25,PLUS=a+b,ONCE=%252C
```

This produces `NOTE=a,b`, `EXPR=x=y`, `PERCENT=100%`, `PLUS=a+b`, and
`ONCE=%2C`. A plus sign is not a space. `%20` is a space; encoded newlines and
other bytes remain field data, not new fields. There is no shell-variable or
backslash expansion inside the target parser. Quote the complete target when
writing shell commands; callers using `exec.Cmd` should pass it as one argument.

Missing `=`, empty names or segments, trailing commas, duplicate/reserved or
invalid names, malformed percent encodings, and invalid tags are command-line
errors. The encoded target is limited to 64 KiB and at most 64 extra fields.
These are local resource bounds, not claimed journal protocol limits. Ordinary
non-journal file paths are not decoded or split, including paths containing
commas, `=`, or `%`.

<a id="output-sources"></a>

### Component logs versus guest outputs

`run --log-to default` (also the default when omitted) retains ordinary component
diagnostics on stderr. `run --log-to journald=...` explicitly sends Go logger
output and run/restore operational diagnostics to that target, including
startup preparation failures and terminal errors after target parsing. It does
not require journal-connected stderr or any particular identity field.
Invalid flag syntax and invalid `--log-to` values can still be reported on the
original stderr before a usable target exists.

Explicit journal component diagnostics are best-effort and never wait for journal
queue space. Congestion or an unavailable journal drops the affected message,
without a synchronous stderr fallback that could block the same control path.
Later diagnostics can be sent when the journal recovers. This policy applies to
`--log-to journald=...`, not to Guest stdout/stderr or console streams.

`--stderr-to` remains the guest application's stderr destination. `--log-to`
does not take over process descriptors, change TTY/pipe selection, redirect
Cloud Hypervisor's own stderr, or capture runtime panic/system events. Existing
message text and log timestamp precision are retained. `exec`'s own diagnostic
stderr and error-capture behavior are unchanged. File, artifact, JSON, and other
non-journal stdout data remain unaffected.

<a id="output-lifetime"></a>

### Framing, failure, and lifetime

Each output has its own line buffer. Complete lines are emitted at info priority;
ordinary CRLF endings are normalized and empty lines are omitted, as before.
Very long lines are split into messages of at most 60 KiB without first copying
the whole input into an unbounded buffer. A forced size boundary does not strip
carriage returns from the middle of a line. A final partial line is flushed on
Close. Producers are drained before their writer is closed; Close is idempotent,
and writes after Close return `io.ErrClosedPipe`.

A successful native send is not duplicated to stderr. Guest/console streams
retain backpressure while the journal queue is full, without busy retrying.
A native-send error is followed by a best-effort write to the original fallback
sink, with the existing `[tag]` prefix. Neither a native-send error nor a
fallback-write error terminates the sandbox. Fallback text does not retain
structured fields. Component diagnostics use the nonblocking policy above and
do not call a fallback.

Each journal output owns its sending connection; a backpressured Guest stream
does not block the component diagnostic sender. Close releases that connection
after flushing its partial line under the selected output policy. SDK callers
use `journalio.Open(target, fallback)` for streams and
`journalio.OpenDiagnostic(target)` for best-effort component diagnostics.
These APIs do not provide an asynchronous queue or promise lossless,
exactly-once durable storage.

Use `journalctl WORKLOAD_ID=worker-42 -o json` to inspect the fields in the
journals available to that command. Cross-node history requires a separate
collector or access to the other nodes' journals; no exporter is added here.

Protocol references: [Journal Native Protocol](https://systemd.io/JOURNAL_NATIVE_PROTOCOL/)
and [Go net/url](https://pkg.go.dev/net/url).

<a id="related"></a>

## Related documentation

[Runtime: start, control, capture and restore](runtime.md) | [Sandbox and Snapshot artifacts](artifacts.md) | [Guest memory and lazy restore](memory.md) | [Block storage: devices, writable state and checkpoints](block-storage.md) | [Resource control and usage accounting](resources.md) | [Guest ABI: boot, applications and host protocols](guest-abi.md) | [VMM integration and Cloud Hypervisor patches](vmm.md)

[Component entry](../README.md) · [Native builds and patch materials](../native-deps/README.md)
