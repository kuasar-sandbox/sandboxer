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
  diff_cow: { cache_size: 32MiB, max_dirty_size: 0, discard: auto }

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
| `resources.diff_cow.max_dirty_size` | Size; `0` | Aggregate dirty bytes; zero disables this extra limit, positive values must be 4 KiB multiples no larger than `cache_size` |
| `resources.diff_cow.max_dirty_size_per_disk` | Size; `0` | Uniform dirty limit for each writable disk; positive values must be 4 KiB multiples |
| `resources.diff_cow.read_bandwidth`, `write_bandwidth` | Size per second; `0` | Aggregate admitted read/write payload bytes per second |
| `resources.diff_cow.read_iops`, `write_iops` | Nonnegative integer; `0` | Aggregate complete read/write requests per second |
| `resources.diff_cow.read_bandwidth_per_disk`, `write_bandwidth_per_disk` | Size per second; `0` | Uniform independent bandwidth limit for each logical disk |
| `resources.diff_cow.read_iops_per_disk`, `write_iops_per_disk` | Nonnegative integer; `0` | Uniform independent request rate for each logical disk |
| `resources.diff_cow.cache_idle_after` | Positive duration; `10s` | Time since the last Clean-page access before it counts as idle; does not invalidate or immediately reclaim pages |
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
| `boot.root.diff`, `boot.root.diff_template`, `boot.root.base_from_refs` | Local reference, template reference, ordered ref list | Single-disk-only writable binding, provisioning template and lower chain; mutually exclusive with overlay |
| `boot.root.overlay.base`, `.base_from_refs`, `.diff`, `.diff_template` | Ref, ref list, local ref, template ref | Writable upper's immutable source chain and active binding; same rules as the single-disk fields |
| `boot.disks[].name` | Unique nonempty string | At most eight data disks, in stable array order |
| `boot.disks[].base`, `.overlay`, `.diff`, `.diff_template`, `.base_from_refs` | Same disk shape as root | Each data disk uses the same single/overlay source rules and has one or more `type: disk` mount projections on cold start |
| `boot.disks[].slot` | `{}` or `{size: 8GiB}` | Single-device slot bound at runtime |
| `boot.disks[].disk` | Complete DiskV1 | Cold launch from the same disk definition with an instance-private writer |

`diff` is an absolute local file reference; `diff_template` selects one of the five
source types below. Immutable refs may
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

This configuration reserves a virtio-blk device named `input`. Both fixed and dynamic unbound slots require autofs in the guest kernel for first-access mounting. A guest without autofs cannot use these slots. Access before binding returns a retryable error. First access after binding mounts ext4 and projects the requested directory. Ordinary single-device and overlay data disks use the same mount syntax. A slot target must be disjoint from every other effective mount. Source directories projected from the same slot must also be disjoint. Other mounts retain their existing ordering. New mount declarations reject `.` and `..` path components in `target` and `subpath`. Omit `subpath` or use an empty string to project the whole disk.
```yaml
boot:
  disks:
    - name: input
      slot: {}
mounts:
  - {type: disk, source: input, subpath: data, target: /data}
  - {type: disk, source: input, subpath: results, target: /out}
```

A slot with omitted `size` adopts the source capacity at bind time. A declared size must match the source exactly. The host completes a bind when it has committed the backend and its disk record. A subsequent capture includes the bound medium.

A dynamic slot (`slot: {}`) initially reports zero capacity. If the VMM sends no capacity-change notification, it must report the bound disk’s capacity on request, and the guest kernel must refresh capacity when the disk opens. This requires the virtio-blk patch from guest-runtime #134 or equivalent support in the `vmlinux` actually running in the guest. Without that support, `disk bind` can succeed while first access to the directory fails. If the running kernel lacks this capability or you cannot confirm it, for example when reusing an older `vmlinux`, prefer a fixed slot such as `slot: {size: 8GiB}` and bind a disk of that size. [Guest kernel patch](https://github.com/kuasar-sandbox/guest-runtime/pull/134)

List the receiver’s disks, export a complete disk definition from a committed E or S, then bind it on the receiver. The same export command accepts a saved whole-machine checkpoint.
```bash
sandbox-ctl disk list --path-id receiver
sandbox-ctl export --source-ref "$PAUSED_E" --disk work --output "$OUT" > work.disk.json
sandbox-ctl disk bind --path-id receiver --disk input --disk-spec work.disk.json
```

Disk export follows the same dependency policy as Sandbox publication. It retains published pure-block `manifest://` references and immutable named file references, including Bundle selectors, without opening their payloads. The destination controls where layers that still need materialization are written:

| Destination options | Newly materialized layers | Returned references for those layers |
| --- | --- | --- |
| `--output DIR` | One local Bundle | Absolute file paths with Bundle member selectors |
| `--upload` | Configured Manifest Store | `manifest://` references |
| `--upload --to-ref-location NAME=file:///absolute/dir` | One Bundle in the named directory | Relative Bundle member selectors with `@location:NAME` |

The named destination replaces the Manifest Store destination. Its syntax, validation, and input-location conflict rules match `sandbox-ctl publish --to-ref-location`. The target name also resolves source references with that name; an explicit `--ref-location` for the same name must identify the same directory. A file-only named export can omit Manifest configuration and use `MANIFEST_KEY` for the default Bundle encryption policy. A disk whose complete graph is already published needs no new Bundle, Store write, or named-directory write. The returned `DiskV1` always lists the complete ordered graph and can contain both new and retained references.

Default stdout and local `--output --json` remain a bare DiskV1. Explicit
`--upload --json` returns `disk: DiskV1` with `refs`, `diskRefs` and
`removedRefs`; consumers binding that JSON must select its `disk` field.
See the [upload report contract](artifacts.md#publication-report).

`export --to-ref-location` is supported only with `--source-ref E-or-S --disk NAME --upload`; live capture, `--checkpoint`, image assembly with `--from`, and `--output` do not accept it. For example:

```bash
sandbox-ctl export --source-ref "$PAUSED_E" --disk work \
  --upload --to-ref-location shared=file:///srv/disk-exports > work.disk.json
sandbox-ctl disk bind --path-id receiver --disk input --disk-spec work.disk.json \
  --ref-location shared=file:///srv/disk-exports
```

Changing the output directory does not move existing published parents. A receiver needs access to those parents and the corresponding named-location mappings and keys. Source-private checkpoint versions and unpublished Bundle members are materialized so deleting the source sandbox does not invalidate the exported disk. Reading the top layer to determine an otherwise unknown capacity, converting a legacy Sandbox-wrapped disk layer, or compacting a graph that exceeds the layer limit still reads the required sources. To initialize a new disk, supply a JSON object with `kind: disk`, `version: 1`, and either a `diff_template` reference or a size-only creation capacity. Bind can register named locations with `--ref-location NAME=file:///dir`.

### Workload fields

| Field | Type / default | Validation and effect |
| --- | --- | --- |
| `launch.systemd` | `off`, `auto`, `runtime`, `provided`; omitted inherits E, otherwise `off` | Choose environment PID 1; active modes require the live Guest `systemd_v1` capability. Null, booleans, empty strings and other values are rejected. |
| `launch.exec`, `launch.args` | String and string list | Override image entrypoint/arguments; single-disk roots need explicit exec unless placeholder is selected |
| `launch.env`, `launch.ephemeral_env` | String maps | Persistent environment and current-invocation override; [merge rules](#files-and-environment) |
| `launch.workdir` | String; `/` after ordinary loading | Application working directory; image/source projection follows the corresponding launch merge rules |
| `launch.restart` | `never`, `on-failure`, `always`; `never` | Primary process supervision |
| `launch.cgroup_control` | Boolean; `false` | When true, delegate controllers at the user-visible cgroup root and place directly managed processes in its `/init` leaf; false uses the scoped root directly. Systemd modes always enable delegation while preserving this raw policy for a later direct boot. The Guest Runtime owns the global hierarchy. See [Guest topology](guest-abi.md#application-cgroups). |
| `launch.placeholder` | Boolean; `false` | No-exec anchor; excludes exec and always restarts |
| `launch.pid_namespace` | `private`, `shared`; `private` | Private makes the application namespace PID 1; shared exposes a Runtime PID 1 that reaps orphaned processes |
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

### Systemd launch policy

```yaml
launch:
  systemd: auto
```

`off` uses direct supervision. `runtime` requires the packaged Runtime systemd;
`provided` requires the workload rootfs installation. `auto` selects the workload
installation when present and otherwise the Runtime installation; an invalid
selected installation fails instead of falling back. Active modes share the
systemd environment PID namespace and enable delegated cgroup control. They do
not rewrite the raw `pid_namespace` or `cgroup_control` values saved in E.

An E cold start inherits an omitted mode; explicit `off` overrides an inherited
active mode. A complete new definition persists the effective mode. An S restore
continues its captured manager and process state and rejects any mode override,
including `off`. Runtime systemd adds its tool directory to the native
exec default PATH; explicit caller PATH values are never rewritten. Existing
Launch/OCI environment values remain authoritative without new provenance metadata.

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

A direct EROFS E contains only a read-only image. The destination host must supply a `diff_template` (including `mkfs://ext4?size=...`) or an explicitly bound, already formatted diff. A missing mountable upper fails before controller/network/CH side effects.

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

## `resources.diff_cow`: cache, disk QoS and mount policy

```yaml
resources:
  diff_cow:
    cache_size: 32MiB
    cache_idle_after: 10s
    discard: auto
    max_dirty_size: 0
    read_bandwidth: 0
    write_bandwidth: 0
    read_iops: 0
    write_iops: 0
    max_dirty_size_per_disk: 0
    read_bandwidth_per_disk: 0
    write_bandwidth_per_disk: 0
    read_iops_per_disk: 0
    write_iops_per_disk: 0
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
removes this leaf while preserving all cache and QoS settings.

Go SDK callers select the policy through `RuntimeSpec.Resources.DiffCOW.Discard`
before `StartRuntime`; the runtime uses that selection when the workload is
launched, including after a live `runtime_ready` prewarm.

`cache_size` is a positive multiple of 4 KiB and defaults to 32 MiB. All ten
limit fields default to `0`: no additional limit for that dimension. Layered
configuration preserves an earlier value when a leaf is omitted; an explicit
zero clears that leaf's inherited limit. Resolve defaults only after all
host overlays have been merged. IOPS values must be nonnegative integers;
bandwidth values use the existing size syntax with an implicit `/s`. Thus
`50M`, `50MB` and `50MiB` all mean 50 × 1024² bytes/s. Bandwidth does not require
4 KiB alignment. All size values must represent whole bytes.

A positive `max_dirty_size` must be a 4 KiB multiple no larger than `cache_size`.
A positive `max_dirty_size_per_disk` must be a 4 KiB multiple fitting host address
space. It may exceed the aggregate dirty limit or cache size: the smaller
applicable capacity still constrains actual consumption. Neither dirty limit
creates a separate pool or reserves capacity. Clean pages share the same cache;
with negotiated node cache grants, actual use remains bounded by the current
grant as well as `cache_size`. Adding a disk does not multiply the aggregate
budget. Dirty reservations include pages being constructed and frozen pages
under writeback; submission alone does not release the quota. Overwrites and
Data/Zero transitions of an already dirty page do not charge it again. Each
active writable disk has its own asynchronous writeback worker. The shared
cache and fatal-error contract remain in effect.

Read and write requests must jointly satisfy their aggregate and logical-disk
bandwidth/IOPS limits before entering storage. A request is charged once for
its entire payload, including cache hits and reads from the remote base. An
overlay root's lower and upper devices share one logical-disk budget. Internal
writeback, remote fetch retries, FLUSH and DISCARD do not consume another
read/write request quota. This limits admitted demand; it is not a measurement
or guarantee of physical device throughput. Limits do not provide a minimum
share or reserve cache for other disks.

Admission permits one complete request initially, then paces subsequent
requests by the admitted request's full cost. Idle time does not accumulate
burst credit. A request larger than a second's bandwidth quota can complete;
its complete cost delays the next request. Waiting quota is interruptible by
queue stop, disable and snapshot quiesce. Resume retries the unstarted request
without requiring another guest kick. Actual started I/O still drains before
quiesce completes. Policies are fixed when the runtime is created; launching a
prewarmed runtime or binding a disk slot does not reset its budget. Memory
restore uses the destination host's merged QoS policy, not artifact policy.

**Upgrade:** the previous omitted dirty limit was half the final cache size.
It now means no extra dirty limit; physical dirty memory is still bounded by
the cache grant. Previously normalized configs containing an explicit positive
value retain it. Regenerate them or use `max_dirty_size: 0` to opt into the new
default. Upgrade `sandbox-ctl`/the Sandboxer SDK before using the new
Orchestrator defaults: old versions reject both the new keys and an explicit
zero dirty limit, and the new default Host YAML includes that zero. There is
no unlimited cache size, cache disable switch, or buffered retry after a failed
DIO data operation. These controls are host policy, excluded from portable
Sandbox E and Snapshot S identities.

The payload remains charged to `sandbox-ctl` in the existing `ctl/` cgroup;
Guest balloon, Guest reservation and `vmm/` controls are unchanged. For shared
physical disk control on runner/builder units and the conditional DIO priority
behavior, see [disk QoS deployment](disk-qos-linux.md).


`cache_idle_after` controls only the Clean-page idle statistic. Clean
pages remain valid and may be read or touched again without a forced cache miss.
When observed idle Clean memory reaches one 4 MiB internal unit, the sandbox
starts the same gentle asynchronous reclaim worker used for node requests.
The worker first returns unused physical buffers, then uses Clean LRU without
checking idle age again. A node reclaim amount is advisory: the sandbox may
return fewer bytes or none, reports attempt completion, and the node only
credits physically released bytes. Both initial and runtime cache grants use
a 4 MiB internal unit, with valid smaller configured tails supported.

Elastic backing requires 4 KiB host pages and disables transparent huge pages
on its own mappings so each fault and return respects the grant. A host that
cannot provide this granularity, or a controller connection that does not
negotiate cache grants, uses the fixed-capacity cache. The default maximum is
32 MiB. Configure `cache_size: 1GiB` explicitly for 500 MiB–1 GiB peak working sets.

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

An existing valid active `diff` keeps its logical capacity and contents. Its
initializer is not opened: missing template files, unavailable Stores, template
keys and unused generator limits do not affect reopening it. An empty or damaged
existing writer is an error; it is never reformatted automatically. Without a
writer, the selected template determines capacity; without a template, a fresh
COW layer inherits its base. Restores use the captured disk graph and never
substitute an initializer for missing or corrupt captured content.

### Template references

The existing `diff_template` string selects one logical source. There is no
`diff_template_format` setting, extension sniffing, or fallback to another format.

| Reference | Selected source |
| --- | --- |
| `file:///path/empty.ext4` | Raw sparse disk; preserves existing file syntax |
| `file:///path/empty.tar@digest:<digest>` | Plaintext canonical tarstream |
| `file:///path/empty.tar@hmac:<digest>` | Encrypted canonical tarstream |
| `file:///path/empty.bundle@manifest:<content-key>` | The explicitly selected Manifest in that Bundle |
| `manifest://<content-key>` | Manifest Store/cache |
| `mkfs://ext4?size=10GiB` | Built-in deterministic empty ext4 |

Files retain the ordinary absolute-path or named-location contract. For example,
`file://empty.ext4@location:seeds` uses `--ref-location seeds=file:///templates`.
Identity selectors precede `@location`. A bare `.bundle` path still selects raw;
Bundle templates require an explicit `@manifest` selector. A selected Bundle
must contain the root and all of its required Chunks; missing content is not
filled from another Store or Bundle. Base/snapshot file detection is unchanged.

The tarstream identity commits to its canonical carrier, including the sparse
logical content; it is not a raw-file or expanded-disk SHA256. A Bundle selector
is the inner Manifest ContentKey, not the Bundle file hash. Template reads force
Manifest/Chunk identity verification even when ordinary `manifest.verify_content`
is false. Tarstream random reads and ext4 preflight are not full commitment
verification: complete verification must succeed before `WaitMaterialReady`.
Source crypto follows the existing local/ref/Manifest policies independently of
the target DIFF's encryption. Plaintext raw can initialize an encrypted DIFF.

`mkfs://ext4?size=10GiB` accepts exactly one `size` query parameter. `10G`, `10GB`,
`10GiB` and `10240MiB` denote the same exact 10,737,418,240 bytes and normalize to
a byte-valued URI. Only a selected creation applies the generator's 4 KiB
alignment and 1 MiB–1 TiB range. Active single-layer v3 supports up to 1 TiB;
checkpoint v4 retains its 32 GiB limit. The generator never calls host mkfs or
creates an intermediate image. It calculates layout and encodes requested
metadata directly into caller buffers; holes incur no payload reads or fills.

For a new empty 512 MiB scratch filesystem:

```yaml
boot:
  # Keep the other required boot fields from the complete configuration.
  disks:
    - name: scratch
      diff_template: mkfs://ext4?size=512MiB
mounts:
  - { target: /scratch, type: disk, source: scratch }
```

The selected writable device must have no COW base when using mkfs. A separate
read-only EROFS lower in two-device overlay mode is allowed. External templates
retain Hole → base, Zero → zero, and Data → template semantics, including partial
pages; their logical size must not be smaller than their COW base. Carrier file
length is never the disk capacity. Ordinary raw templates remain supported;
`truncate` plus `mkfs.ext4 -O ^has_journal` is still valid when an external image
is desired.

### DiskV1 creation by size

A DiskV1 with no base, parent layers or template can specify only its creation
size, in bytes, alongside the required kind/version markers:

```json
{"kind":"disk","version":1,"size":10737418240}
```

It is equivalent to adding `"diff_template":"mkfs://ext4?size=10GiB"`.
Normalization occurs before decoding validation, binding and source identity,
so equivalent units and the size-only shorthand identify the same creation.
With an explicit source, `size` remains a capacity assertion; it never rescues
an invalid source or orphan parent list. The assertion also checks an existing
writer, whereas an unused mkfs URI's size does not. A slot alone remains unbound,
even if it declares fixed capacity. A complete explicit disk is required to bind
it; fixed slot capacity must match and dynamic slots adopt the selected capacity.

The former root/overlay `diff_size` and `size` fields remain retired. Remove them
and select a filesystem source of the desired capacity, use a mkfs URI, or use
`boot.disks[].disk.size` where a complete DiskV1 is appropriate. No automatic
filesystem resize, truncation or physical-storage quota is introduced.

### Deterministic empty filesystem and compatibility

The internal `empty-ext4/v1` profile fixes UUID, directory hash seed, initial and
extended timestamps, hash signedness, reserved bytes, metadata placement and
sparse run classification. It uses 4 KiB blocks, 256-byte inodes, a target of
8 KiB per inode and flex_bg=8. The initial root contains only `.` and `..`; the
profile has no journal, resize_inode or metadata checksum. Its bytes depend only
on capacity and profile, and do not follow host mkfs defaults. Profile changes
require a new internal version; there are no UUID/feature/seed tuning options.

Identical initial disks can reuse storage Chunks/Manifests under the same
chunking and encryption generation. Filesystem UUID is not an instance identity.
Guest writes retain normal timestamps, inode generation and overlay UUID;
active DIFF keys and nonces retain their existing randomness. The journal-free
profile does not add crash-recovery guarantees to a live work disk. Existing
journaled external filesystems and captured disks remain supported.

DIFF v1 encrypted envelopes and DIFF v3 work files are no longer template
carriers. Export their logical content with a version that supports the old
input before upgrading; do not rename or strip headers to pretend they are raw.
Activity and restore of supported v3/v4 writers are unaffected. Successful
materialization removes the template dependency from reopen, export and restore.
Logical capacity remains distinct from carrier length, physical allocation and
Guest filesystem free space.

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
