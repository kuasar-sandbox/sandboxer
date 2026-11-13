[English](resources.md) | [简体中文](resources_zh.md)

# Resource control and usage accounting

This page owns per-sandbox resource execution and cumulative usage: deployment
modes, Capacity/Allocatable/Demand/Budget, cgroup and balloon control, the node
resource protocol, measurements, persistence and the `usage` command. Start with
the memory terms to distinguish a VM's address space from its current reservation
or observed host RSS.

Field syntax/defaults live in [configuration](configuration.md). The
[Guest ABI](guest-abi.md#usage-observations) owns raw Guest observations; this page
owns their Host arithmetic and lifetime. [Runtime](runtime.md#capture-barrier)
coordinates resource mutations with capture, while [memory](memory.md) owns RAM
page state and fault handling.

<a id="deployment-modes"></a>

## Three deployment modes

| Mode | `cgroup_path` | `controller` | Behavior |
|---|---|---|---|
| No cgroup | Empty | Empty | No cgroup writes; allocatable CPU must equal capacity CPU, and allocatable memory must remain within capacity |
| Static cgroup | Set | Empty | Configure local CPU/memory limits and optionally run the local sensor |
| Dynamic | Set | Set | Use resource-protocol admission/leases/Budget and run the local sensor when configured |

`resources.capacity` is guest-visible VM capacity and enters Portable configuration. `resources.allocatable` is the cold-start workload default and also enters Portable configuration. `allocatable.cpu` must be finite and satisfy `0 < allocatable.cpu <= capacity.cpu`. Restore retains capacity and the captured `deflate_on_oom` from E, but can explicitly reapply the destination node's allocatable CPU/memory. This runtime policy does not rewrite E or C0. `control`, `overhead`, `watermark_high`, `startup`, and `diff_cow` are node policy and do not enter E.

Export/snapshot acquires the MemoryController mutation barrier and, before freezing, lifts and drains any `memory.high` operation that could race with CH pause. The host ping gate drains an admitted probe through the guest-EOF transport barrier. A response EOF after CONNECT and request write also completes this drain when no pong arrives; the probe remains a health failure, with unchanged counters and fatal-threshold policy. CONNECT EOF, cancellation, deadlines and other protocol failures still fail the drain. Capture must then obtain its own positive quiesce acknowledgment before pausing the VM. This drain has its own 8-second quiesce budget; capture cannot wait forever merely because ordinary `timeouts.ping` has no forced timeout. On expiry, the host cancels and joins the probe, and failed capture enters full recovery. Guest quiesce also drains and pauses periodic `mem_report`, preventing S from capturing a reporter that holds the stream lock while waiting on the old host vsock. Restore switches to a new observation epoch before ACK; `--resume` and failed-capture attach recover the original epoch. A live attach only reopens the pause gate and does not corrupt accounting for already admitted reports. Recovery releases the host barrier after VM, MUX, app, and backends recover.

<a id="memory-terms"></a>

## Memory terms

```text
CapacityMemory      = CH boot memory zone maximum
AllocatableMemory   = workload settled guest headroom
DemandMemory        = controller observation
Budget              = admitted/locally enforced working allowance
VMM memory.max      = CapacityMemory + host overhead
```

Memory S capture records CH configuration/state and sparse memfd content. Resource policy is not written to `snapshot.cfg`. Restore obtains capacity identity from E, and any explicitly supplied host capacity must match. Allocatable CPU/memory start from E and are replaced only for fields explicitly supplied by the current host. `startup`, `overhead`, `watermark_high`, and resource-controller bindings come from the current host. `deflate_on_oom` must remain consistent with captured VMM state. The Snapshot's `BudgetAtSnapshot` remains authoritative for the initial restore Budget.

<a id="cpu-and-balloon"></a>

## CPU and balloon

Capacity CPU determines vCPU topology. In cgroup mode, allocatable CPU maps to `cpu.weight`; without a cgroup it must equal capacity CPU, so a separate fractional allocation cannot be expressed. CH restore state authoritatively restores the current balloon state; the host does not invent another balloon state from S.

<a id="cgroups"></a>

## Cgroups and balloon

The VMM cgroup must already exist and have the required controllers delegated.
Its directory is owned by the operator/orchestrator, not sandboxer. Setup pins a
stable directory descriptor and writes limits through it; CH is created directly
in that cgroup with `CLONE_INTO_CGROUP`. Cleanup closes the descriptor without
removing the directory or restoring old limits. Setup is not transactional:
a later failure does not undo earlier successful writes. The owner must inspect
and restore the intended limits before reusing a failed setup. Setting
`memory.swap.max=0` is best-effort and logs failures; it is not a guarantee that
swap was disabled. See [cgroup.go](../pkg/resctl/cgroup.go).

<a id="memory-enforcement"></a>

### Memory enforcement

Static/dynamic cgroup modes lift a potentially competing `memory.high` before CH pause and wait for existing high events to drain. Snapshot/export recovery restores the previous value. The VMM cgroup contains CH, not sandbox-ctl itself; sandbox-ctl is not charged to that workload Budget.

<a id="cpu-enforcement"></a>

### CPU

`capacity.cpu` determines the vCPU count. `allocatable.cpu` must be finite, positive, and no greater than capacity CPU; cgroup mode maps it to a clamped `cpu.weight`. This weight is relative contention policy, not a hard fractional-core quota; the VMM's `cpu.max` is based on capacity. Controllers may adjust grants during a lifecycle but do not rewrite C0 or the artifact.

<a id="balloon-controller"></a>

### Balloon

Balloon mutations serialize with snapshot/export through the same barrier. The freeze window cannot overlap asynchronous balloon resize. Restore takes CH state as the actual current value, then enters a new observation epoch; host YAML does not reconstruct the restored balloon's instantaneous state.

The sandbox-local `MemoryController` combines one accepted Guest ABI `mem_report` with CH `vm.info`. Guest reports start immediately after the cold-launch barrier and repeat every five seconds by default; [Guest ABI](guest-abi.md) owns their epoch/sequence, retry and quiesce protocol. `Capacity` comes from the host-owned CH memory configuration, never Guest `MemTotal`. CH `config.balloon.size` is `AcceptedTarget`; `memory_actual_size` is `CurrentBudget`, so `BalloonCurrent = Capacity - CurrentBudget` and `TargetBudget = Capacity - AcceptedTarget`. `DesiredTarget` is separate local intent. The safe observed Budget is `B = max(TargetBudget, CurrentBudget)`. Exact equality is exposed only as the `TargetReached` diagnostic; it is not a convergence or settlement gate. A successful `vm.resize` accepts a target and does not prove current progress.

The local calculation is:

```text
DemandMemory = max(CurrentBudget - MemAvailable, 0)
RawRequested = min(Capacity, saturating_add(DemandMemory, AllocatableMemory))
DesiredTarget = align_down(Capacity - RawRequested, 64 MiB)
RequestedBudget = Capacity - DesiredTarget
```

Diagnostic Guest fields and host `memory.current` are not inputs to the demand formula above. Host `memory.current` supplies a safety lower bound for `memory.high`; the pressure sensor can also use it as a growth signal as described below.

Growth reserves Budget first, raises the required `memory.high` allowance, then lowers the balloon target through `PUT /api/v1/vm.resize` and confirms acceptance with `vm.info`. Partial node grants accumulate until a representable Budget fits within the reservation; rounding must never create unreserved memory. Safety growth can proceed after the lifecycle barrier even while target/current are unstable. Reservation, high or ambiguous-resize failures retain the forward growth objective for retry; a later smaller report does not implicitly roll it back.

Shrink requires a fresh, non-superseded report and known CH target/current. Each report permits at most one 64 MiB inflation step, retaining one step as a deadband. A new target must exceed the accepted target and is bounded by the desired target, capacity, `AcceptedTarget + 64 MiB`, and `BalloonCurrent + 64 MiB`, using checked or saturating arithmetic. Immediately before resize, CH and those bounds are checked again under the mutation gate; reversed actual movement or a changed accepted target invalidates the candidate. Once the target operation is confirmed, `B < Reservation` lowers `memory.high` for B and submits B as the absolute reservation baseline; `B == Reservation` does not resubmit, while `B > Reservation` neither releases nor treats actual movement as a node grant and instead leaves growth to the existing demand/pressure path. A partial observed advance can therefore settle the round without target equality; later reports handle later progress. A newer admitted report, unavailable CH state, or failed high/reservation operation prevents premature release. An unfinished grow retains its reservation, and growth after a partial commit requests the difference from the latest baseline. If a shrink response is lost, the submitted smaller baseline remains the conservative recovery value whether or not the node committed it. The mutation gate covers CH and `memory.high` changes, not the subsequent node-only commit RPC. Guest emergency deflation does not bypass these conditions.

A short allocation burst can trigger Guest emergency deflation and finish between reports. If inflation then stops below the accepted target, the controller can re-notify that same target after two fresh reports in one epoch observe an unchanged target and actual value. Fresh requested Budget must fit within TargetBudget, and TargetBudget must remain covered by the existing reservation. Normal bounded target steps and partial settlement take priority. Pending growth, restore normalization, pressure, superseded reports, and any actual or target change invalidate the retry candidate. The mutation gate still serializes the operation with snapshot/export, and CH state and report eligibility are checked again immediately before the write. This retry neither requests additional reservation nor lowers `memory.high` or releases reservation; subsequent actual observations use the normal settlement path. Each attempt consumes its two-report confirmation, so the reconciliation ticker cannot repeatedly resize a stalled Guest. An unchanged accepted target cannot confirm delivery after a lost response, and HTTP success never substitutes for observed actual progress.

See [memory.go](../pkg/resctl/memory.go), [memory_controller.go](../pkg/resctl/memory_controller.go) and [balloon.go](../pkg/resctl/balloon.go). Logical release notification precedes sparse physical reclaim as defined by [VMM patch 0004](vmm.md#patch-0004).

<a id="resource-protocol"></a>

## Resource protocol



The shared wire types, field names and framing are implemented by
[`pkg/resource`](../pkg/resource/proto.go), with client behavior in
[`client.go`](../pkg/resource/client.go). Messages use a four-byte little-endian
length followed by JSON, with a maximum payload of 64 KiB. The default controller
socket is `/run/sandbox-resource.sock`. The client surface includes admission,
settlement, Budget requests, heartbeat/release, OOM reporting, recovery state sync
and bounded administrative views.

This page owns the sandbox-side observation, mutation and lifecycle boundary.
The controller's admission decisions, peer authentication, leases, reconnect
reconciliation and node pool accounting are defined by
[orchestrator's controller protocol](https://github.com/kuasar-sandbox/orchestrator/blob/main/docs/node-resource.md#controller-protocol).
Wire names retain compatibility: `floor_memory_bytes` is settled headroom,
`startup_budget_memory` is cold InitialBudget, and `allocatable_at_snapshot`
is restore BudgetAtSnapshot; these must not be interpreted as interchangeable
RSS measurements. Unknown advertised client features are ignored for peer
compatibility. Portable E/S does not store this host ownership protocol.

<a id="resource-states"></a>

### Lifecycle states

```text
prepare -> admit -> start -> ready -> steady
                  \-> freeze -> capture -> resume|destroy
restore -> resume -> settle -> steady
```

Controller leases, heartbeats, and recovery inventory manage only host resource ownership and do not enter Portable configuration.

<a id="resource-capture-barrier"></a>

### Capture barrier

Before snapshot/export, the mutation gate blocks new CH balloon and
`memory.high` writes and waits for already admitted writes to complete. It does
not freeze all node reservation traffic: dynamic growth can reserve before
acquiring this gate, and dynamic shrink releases it before the node-only commit
RPC. Static settlement remains under the gate. Recovery releases the barrier
only when the guest/app can continue, so later local mutations can resume.

<a id="pressure-sensor"></a>

### Pressure sensor

Static/dynamic modes can use PSI or `memory.events.local` polling, or disable
the sensor with `resources.control.sensor.mode: none`. Host configuration
determines the PSI trigger and debounce; they are not written to E. The sensor
only submits coalesced pressure signals to the local controller and can continue
doing so while the capture gate is held. The gate serializes the resulting CH
and `memory.high` writes. In `events_poll` mode, a rising host charge above 95%
of a finite `memory.high` can request low-urgency predictive growth; this is a
pressure signal, not substitution of RSS for Guest demand. Guest-report
epoch/admission belongs to the controller and Guest lifecycle, not the sensor.

<a id="usage-cli"></a>

## `sandbox-ctl usage`

`sandbox-ctl` owns observation scheduling, CPU differences, gauge integration,
sampled peaks, merging and persistence through `pkg/usage`. `usage.Snapshot`
is cumulative state; `usage.Record` is a self-contained saved checkpoint.
`sandbox-init` only reads and parses raw observations on request. It has no
usage ticker, accumulator, historical summary or reliable replay queue.

Usage is optional and is not a bill, resource limit or physical-cost attribution.
It covers Guest execution CPU (total and per vCPU), Guest physical memory,
managed writable filesystem occupancy, and CH/sandbox-ctl process CPU and
separate `RssAnon`/`RssFile`. It excludes UFFD, network and disk-operation
counts, bytes and latency. Existing diagnostic telemetry and resource control
remain separate. There is no daemon, exporter, billing policy, consumer ACK,
global WAL or post-deletion retention service.

```bash
# Read current in-memory and confirmed saved state from the running owner.
sandbox-ctl usage --sandbox-id s1

# PathID locates directories; SandboxID remains the file/record identity.
sandbox-ctl usage --sandbox-id s1 --path-id instance --saved

# Read a stopped sandbox without starting a VM or changing the file.
sandbox-ctl usage --sandbox-id s1 --offline
sandbox-ctl usage --file /var/lib/sandbox/s1/s1.usage

# Bounded history page; pass next_cursor as the next --cursor.
sandbox-ctl usage --sandbox-id s1 --history --limit 10 --cursor 0
```

Output is lossless JSON. `--json` defaults to true. `--run-root` defaults to
`/run/sandbox`; `--base-root` defaults to `/var/lib/sandbox`. `--path-id`
defaults to SandboxID. `--timeout` bounds a query, including offline recovery/history (default 5 s,
strictly positive). A nonexistent/refused ctl socket permits offline fallback;
other connection/protocol failures are errors. `--file` implies `--offline`
and can infer SandboxID from the filename without `.usage`.

`--saved` omits `live`. History uses a nonnegative byte cursor and a record
limit of 1–100, default 10. The host usage response is bounded to 1 MiB; reduce
the page size if a page cannot fit. Both online and offline readers decode and encode one
bounded record at a time, rejecting an oversized JSON page before retaining
or encoding the complete requested record set, including string-escaping
expansion. History does not return the active tail.
Ordinary ctl requests retain their existing framing and limits.

The view contains `enabled`, optional `live` and `saved`, `saved_end`,
`saving`, `unknown_tail`, and optional `save_error`/`read_error`. Online `saved`
is the owner's adopted baseline: a complete surviving record recovered at
startup, or a later complete append this owner has confirmed. A readable CRC
from a pending write cannot alone advance that baseline. Offline `saved` is
the last complete surviving record validated by recovery, not proof that its
former writer confirmed the append. Neither view guarantees survival of host
crash or power loss; the writeback boundary is defined in [Usage persistence and file format](#usage-persistence). `live` includes
accepted input still in flight or not yet submitted for saving; it can roll
back to surviving saved state after a crash. Missing observation, unsaved
input and an uncertain file tail are different conditions.

Live state is copied between individual metric merges, not as an atomic
whole-round observation. Read each Gauge's own `last_request_id`, status and
position; memory may already reflect a recovered request while a filesystem
still describes the preceding missing request. Queries do not wait for a
whole-round boundary or change either metric's accepted input.

Online queries only copy existing state or read confirmed history. They do
not sample Guest/CH, advance counters, or flush. Offline readers take a
nonblocking shared file lock and reject an active writer: use its ctl socket
instead. Disabling usage does not remove an existing file; offline reading
remains available.

The CLI uses [`pkg/usagereader.Read`](../pkg/usagereader/read.go), the common
read entry for local conductor adapters. Callers resolve the current object's
control socket and saved-file path using their existing lifecycle authority,
then supply the exact SandboxID, context, snapshot/saved/history selection,
cursor and limit. The reader uses the existing `Recover`, `ReadHistory` and
file lock; [`MarshalHistory`](../pkg/usagereader/history.go) is the same bounded
page encoder used by the online owner. It does not add another codec, recovery
algorithm, accumulator or persistent state. Native JSON is retained without a
floating-point intermediate; a different SandboxID in live, saved or history
data is rejected. The ctl response envelope always includes the owner SandboxID, even when
usage is disabled, live/saved are absent or history is empty. This does not
change native View/Record or the file format. Empty history has `records: []`
and its validated next cursor. Online pages require an explicit, nonregressing
cursor: nonempty pages advance it, empty pages retain it, and record count cannot
exceed the requested limit. A saved view requires a positive `saved_end` exactly
when a saved record exists. Invalid owner replies never trigger offline fallback.
Offline open uses a nonblocking flag and rejects
FIFOs and other nonregular files without waiting for a writer beyond timeout.

Only failure to connect to an absent/refused socket permits fallback. Once
connected, EOF, invalid replies, owner errors, cancellation and deadlines fail
the query; a readable saved file does not replace them. Cancellation closes
the active ctl connection. The completed owner response is also checked for
cancellation after JSON decoding, validation and saved-view projection. Offline cancellation stops the caller's wait and is
checked between file reads. A blocked file syscall retains its execution slot
and shared lock until it returns; there are at most eight offline executions
per process, including canceled ones. Waiting for a slot also honors the query
deadline. This does not make file syscalls interruptible or change native saving.
Telemetry
consumes native stats through conductor, never this ctl/file reader directly.

<a id="usage-metrics"></a>

## Usage metrics, units and arithmetic

This section is the unit and formula reference for the CLI, protocol and
lifecycle documents. All JSON counters, sizes, durations, positions and
128-bit values are decimal strings, not floating-point numbers. CPU totals
use ns; occupancy uses bytes; area uses byte·ns; span/covered use ns. UTC
fields use Unix ns for correlation only. Positions are signed monotonic ns
relative to `Snapshot.run_epoch`/`started_utc_ns`, never times that can be
subtracted across runs.

| Name | Source and meaning |
|---|---|
| `guest.cpu` | CH process `/proc/PID/stat` `guest_time` |
| `guest.vcpu.N` | Validated `vcpuN` task's `guest_time` |
| `ch.cpu`, `sandbox_ctl.cpu` | Respective process `utime + stime` |
| `guest.memory` | Guest non-free physical RAM after subtracting live balloon current |
| `filesystem.root`, `filesystem.disk-N` | Occupancy of one managed writable ext4 filesystem |
| `ch.rss_anon`, `ch.rss_file` | CH status `RssAnon`, `RssFile`, separately |
| `sandbox_ctl.rss_anon`, `sandbox_ctl.rss_file` | sandbox-ctl status `RssAnon`, `RssFile`, separately |

<a id="cpu-counters"></a>

### CPU counters

`guest_time` is already part of `utime`; it is never added to process CPU a
second time. Host boot identity, PID/TID and starttime identify each source.
The parser locates the end of `comm` before indexing fields, including names
with spaces/parentheses. Valid vCPU mappings are cached and revalidated;
missing threads are not assigned a share of the process total.

`AT_CLKTCK` in `/proc/self/auxv` supplies USER_HZ without libc, cgo or a
`getconf` child. Linux amd64 and arm64 use their verified little-endian ELF64
auxiliary-vector layout. Invalid/missing/duplicate values fail the reader.
Kernel `CONFIG_HZ` is not this conversion scale. Both binaries remain
`CGO_ENABLED=0`.

Counters retain known cumulative ns, native raw ticks, scale, conversion
remainder and source/completeness state. A known-created source includes CPU
before its first poll. Integer conversion retains the remainder across polls
instead of rounding every interval independently. A source regression does
not underflow or silently subtract known usage. After a missed poll the same
native counter can recover its delta; this does not recover Gauge coverage.
Changing a source cannot certify the old source's unknown terminal segment.
For an already identified vCPU, transient reads retain the known baseline and
the next successful same-identity counter covers the missed ticks. An unknown
first identity, confirmed thread exit/replacement, ambiguous mapping or failed
terminal read remains incomplete; rediscovery never restores lost completeness.
Source-loss validity is bounded per vCPU and survives a rejected result; the
rejected result itself never contributes CPU ticks or changes its timestamp.

CH's sole wait owner uses `waitid(WNOWAIT)`, retries EINTR, attempts the final
read while proc state still exists, and then calls `cmd.Wait`. It does not
add another reaper or alter shutdown escalation/output draining. Unknown
thread terminal segments remain incomplete even when the total is available.
The live sandbox-ctl cannot observe CPU consumed after its own final read;
its saved terminal counter therefore retains a known total but is incomplete.
When a new Host owner reopens an existing usage file for further accumulation,
it preserves known CPU totals but marks cross-process history incomplete in
`live`. Saved/offline records retain their completeness flags as recorded;
those flags describe the stored prefix, not any later unknown tail.
Even a normally closed last record cannot
exclude an intervening run that consumed CPU and crashed without writing a
record. Empty files and partial appends have the same limitation. Only a
newly created file can establish the logical history's known starting point;
there is no synchronous startup marker or WAL to prove adjacency of runs.

<a id="memory-observations"></a>

### Guest memory and balloon

Guest observation sources are defined in the [raw usage ABI](guest-abi.md#usage-observations).

```text
G = (PresentPages - BuddyFreePages - PCPFreePages) * PageSize
B = validated CH Capacity - vm.info.memory_actual_size
M = G - B
```

Only `M` enters the Gauge and its peak. `max(G)-max(B)` is not its peak.
Missing fields, inconsistent domains, negative values and arithmetic overflow
invalidate the observation instead of clamping to zero. CH Capacity cannot
replace Guest present pages.

Confirmed no-device means `B=0`. An existing device with unknown, seeded or
stale actual means missing memory, not zero. Actual may be valid while target
differs; convergence is not an admission condition. Desired/accepted target,
resize arguments and controller Budget are not Guest usage.

Only a successful, range-checked CH read publishes actual observation start,
finish, sequence and live instance identity. Target writes and cached returns
do not refresh them; restored seeds are not new reads. The sampler first
reuses qualifying actual, otherwise attempts at most one bounded read-only
query through the existing client. Busy mutation/API locks reject admission
without waiting or spawning replacement workers. Existing control ordering
and single-writer rules remain intact.

The envelope containing the actual CH read and Guest request window must fit
one sample interval. This is a cross-source sampling approximation, not an
atomic physical-memory truth. It needs no Guest Balloon proc field, kernel
patch, inflate/deflate-event fallback or additional CH statistics ABI.

<a id="filesystem-and-rss"></a>

### Filesystems and host RSS

Guest observation sources are defined in the [raw usage ABI](guest-abi.md#usage-observations).

```text
filesystem occupancy = (f_blocks - f_bfree) * unit
unit = nonzero f_frsize, otherwise f_bsize
```

`f_bavail` is not used. Existing occupancy after restore is observed in full,
without subtracting a startup baseline. RSS reads only two process `status`
files, converting their kB units to bytes; it never scans `smaps`. These RSS
items are neither all non-Guest memory nor exclusive physical cost.

<a id="usage-sampling"></a>

## Usage sampling and interval summaries

Default observation is once per second and is merged immediately. There is
no second-resolution sequence in memory or on disk. One reusable dedicated
management connection carries `usage_request`/`usage_response`; raw fields,
admission, timeout margin and quiesce rules are in
[Guest ABI](guest-abi.md#usage-observations).

Each request has a run-scoped monotonic ID. Late, duplicate or old-generation
results are discarded; reconnect reads only new input. The representative
time is the actual Host request-window midpoint,
`start.Add(end.Sub(start)/2)`, preserving monotonic time. Guest UTC is not
subtracted from Host UTC. Summary `max_read_window_ns` records the largest
accepted window; individual windows are not stored.

For two adjacent valid Gauge observations in the same source/time domain,
with increasing request/time and a gap no greater than twice the sample
interval, use left-end integration:

```text
dt = right_time - left_time
integral_total += left_value * dt
covered_total  += dt
span_total     += dt
```

Failures, explicitly discarded observations, known missed scheduling slots,
source changes, nonincreasing time and long gaps break continuity. A failure
does not fill zero or indefinitely extend the last value. Span and covered
share one domain; missing elapsed time enters span once, not once per error
and again on recovery. Paused/unsupported domains do not add span. A first
sample establishes a baseline and sampled peak, but no area.

Area is a checked unsigned 128-bit integer. Averages between two cumulative
endpoints are `area_difference / covered_difference`, unavailable when the
denominator is zero. Span/covered expose sampling coverage separately.
Peaks are sampled peaks, not guarantees of the continuous maximum.

Saving never extrapolates to an unobserved five-minute boundary. It retains
the last valid endpoint, so an interval crossing a save is closed exactly
once when the right sample arrives. Its left value may participate in that
new interval's peak; an unrelated earlier historical peak does not carry
forward. Record windows contain area/span/covered, sample count, peak/value
position and maximum read window; cumulative totals do not reset on save.

<a id="usage-persistence"></a>

## Usage persistence and file format

The sole file is `BaseDir/<SandboxID>.usage`, where the existing lifecycle
derives BaseDir from BaseRoot/PathID. Logical identity is not RunID; normal
restart continues the same file. A clone with a new SandboxID starts separate
history. Usage is outside E/S, so restoring an old business snapshot does not
roll it back.

The host retains a constant number of states:

```text
S = confirmed saved baseline + file offset
F = immutable record currently being saved/reconciled
A = active observations merged while F is in flight
```

Live cumulative values include F and A. A complete `WriteAt` with no error
advances S only to F. A failed or short append can merge its interval
statistics back into A only after successful truncate-to-S establishes logical
rollback. A failed truncate retains F's exact identity/content and reconciles
the tail before the next append. A readable CRC cannot override the current
writer's failed or unfinished append. A new process recovering a complete
surviving record is a different situation.

Usage performs no explicit file, directory or filesystem synchronization:
no `fsync`, `fdatasync`, `syncfs`, `sync`, forced range writeback or synchronous
open flags. This applies to creation, periodic saves, rollback, recovery and
normal shutdown. The operating system and underlying storage govern writeback.
`saved` means logical append completion, not stable-storage durability. Host
crash or power loss may lose already acknowledged records or the file itself,
or leave corruption; recovery can use only the valid bytes that survive.

Each frame has an explicit 16-byte header: `KUUSAGE1`, little-endian uint16
format and algorithm versions (both 1), and uint32 total frame length. The
payload encodes Record/Snapshot fields, source baselines and interval
statistics with explicit signed/unsigned varints, bounded UTF-8 strings and
arrays; 128-bit values encode high/low uint64 halves, not memory dumps.
The 16-byte footer contains CRC32C of header+payload, little-endian uint32
total length, and `USAGEEND`. Maximum frame size is 512 KiB, strings 256
bytes, counters 1,027, and Gauges 14. The ordered codec is
[`format.go`](../pkg/usage/format.go); there is no database or compression
framework.

The Host manager checks identities and metric metadata against these limits
before admitting them; counter and Gauge names share one namespace. It reserves
space for numeric growth when adding metrics or lengthening sources. Individual
string/array limits do not imply that their largest combination fits a frame.
A saved record without sufficient growth space stays readable offline, but
opening a new live owner fails without changing its bytes and releases the lock.
Malformed metadata for an existing Gauge's newer request breaks continuity with
`invalid`, without storing the malformed string; zero, duplicate and older
requests remain no-ops. Empty Gauge sources and empty statuses remain encodable.

Current recovery reads at most two maximum frames at the tail to find the
last valid self-contained record and a recognizable incomplete append. It
does not scan or certify all history. History queries validate encountered
frames and report corruption explicitly; bad identities, versions, lengths
and checksums are not silently accepted. The first frame must have sequence 1.
A nonzero page cursor validates the immediately preceding complete frame and
the sequence across that boundary, including one-record pages; it does not
scan or certify the entire unrequested prefix. New runs retain cumulative endpoints
but rebuild current monotonic positions and Gauge baselines. They never
subtract a saved old monotonic position from the current clock.

<a id="usage-lifecycle"></a>

## Usage lifecycle and failure handling

If sampler initialization fails, usage reports the existing unavailable/error
state and releases its file ownership without saving or truncating a checkpoint.
Existing bytes remain readable offline; a newly created file may remain empty.
There is no uninitialized live/saved view or new closed record for that attempt.

If initialization succeeds but later Host setup or CH spawning fails, bounded
shutdown still attempts to read and save actual sandbox-ctl CPU/RSS. This is
Host process usage, not evidence that a Guest ran. Earlier cumulative totals and complete
record bytes remain intact; the new epoch uses the conservative completeness
rules in [CPU counters](#cpu-counters). Failing to spawn CH does not exempt consumed Host CPU.

Normal shutdown attempts final observations and saving with a bounded
budget. Closing the manager fences new observations before freezing the final
record, while its existing writer may finish F and seal A within that budget.
A previously saved closed record is not completion of this close attempt. Retrying
an uncertain F can exhaust the budget without saving A or a closed checkpoint.
Usage failure does not change the business result. The writer owns
pinned file/directory descriptors: a late write cannot recreate a deleted
directory or write a replacement instance at the same path. Deletion does
not wait for saving/export success and adds no retention policy. Usage never
adds business `fsync`, flush, discard, drop_caches or lazy-loading operations.

An abnormal exit can lose the unsaved tail. Five minutes is the normal save
cadence, not an unconditional loss bound under continuous faults or blocked
storage. There is one writer slot and no queue of five-minute batches.
Slow Guest filesystems likewise keep one slot per source: completed memory
and healthy filesystems remain reportable while the blocked item reports
timeout/busy. Successful saves also provide no host-crash or power-loss
durability guarantee, as specified in [Usage persistence and file format](#usage-persistence). SIGKILL tests cover process
termination, not physical power loss.

<a id="usage-generations"></a>

## Usage across execution generations

Cold and restored instances use the current [host usage policy](configuration.md#usage-policy) and the same Host owner. The [persistence contract](#usage-persistence) accumulates by logical SandboxID: restoring an old S does not roll usage back, and a clone with a new SandboxID does not inherit the original file. New runs retain cumulative endpoints while rebuilding clocks and Gauge baselines; they do not subtract monotonic positions across generations or subtract restored initial memory/filesystem occupancy.

Sampling starts with the Host processes; Guest rounds start only after launch/restore readiness, not merely ctl.sock existence. Before capture, Host closes usage admission and breaks Gauge continuity. Restore/attach and failed-capture recovery use the [Guest raw-usage admission lifecycle](guest-abi.md#usage-observations), including generation fencing, blocked source slots, ACK/thaw rollback and epoch ownership. Restored balloon seeds are not fresh actual observations.

<a id="usage-validation"></a>

## Usage performance and validation

Storage grows by cumulative checkpoints, not per-second samples. Record size
depends on vCPU/disk count, source strings and varint magnitudes; 1 KiB is not
a measured constant. CPU/Gauge aggregation and persistence tests are in
[`pkg/usage`](../pkg/usage); protocol and blocking-source tests live with
[`guestlink`](../pkg/guestlink) and [`sandbox-init`](https://github.com/kuasar-sandbox/guest-runtime/tree/main/cmd/sandbox-init).
The component-owned [usage E2E](../test/e2e/cases/telemetry.usage.sh) is discovered by
the platform prepared-workspace runner, requires real KVM, and verifies the
running Guest init hash against the supplied freshly rebuilt runtime bundle.
Its short-save cases exercise integration. Restore/clone checks use a one-minute
sample interval and require fresh memory/filesystem observations before the
first periodic tick, so retries cannot conceal a missed immediate ACK round.
The current E2E has no separate `defaults` case that waits for the default
five-minute save. Short-save integration and the immediate restore checks do
not establish that cadence was exercised, and are not production-density
performance measurements.

The OOM case pre-establishes an exec/MUX whose test probe waits for one stdin
byte before allocating pressure memory. A bounded test-only LaunchPort relay
forwards the ordinary hello/MUX stream and report/ACK frames unchanged. A new
epoch/sequence with a successfully forwarded ACK supplies a report-delivery
boundary, not proof that the Host control transaction has completed. The test
requires a real converged CH baseline and a fresh boundary before starting
pressure. Only actual growth before the first target change, with no accepted
or ambiguous resize in that interval, qualifies as autonomous deflation.
Duplicate reports, error ACKs, expired boundaries and later deflation after
a control resize do not qualify; the normal control loop remains running.
Recorded start-byte write endpoints use the Host monotonic clock; they are
not Guest allocation timestamps or a measurement of Guest scheduling latency.

The [storage-fault E2E](../test/e2e/cases/telemetry.usage-faults.sh) uses a private bounded
tmpfs for real ENOSPC and path-restricted `strace` injection for usage write
EIO and delayed writes. It also exercises SIGKILL and a sub-second Guest
run. Injection never targets the business writable disk's sync operations;
these tests do not simulate physical power loss. The Host-kill case pins and
verifies its CH child before the crash, then confirms that child's bounded
termination through a pidfd; it does not leave cleanup to the runner.
Linux/Python pidfd support, `strace`, mount privileges
and the ordinary KVM E2E prerequisites are required.

The [slow-source E2E](../test/e2e/cases/telemetry.source-faults.sh) selects one registered
data-filesystem handle by device identity and checks CLOEXEC before delaying
only that `fstatfs` return inside the disposable Guest. It requires a first
timeout, subsequent busy responses from the same occupied slot, fresh memory
and healthy-filesystem coverage, and exactly one traced filesystem call.
Both data disks receive real writes. Bytes written after the delayed read
distinguish a new observation from a replayed old buffer after release.
A single long-lived test exec records all Guest FDs, identities, threads and
RSS; it does not create an exec per observation or label threads as goroutines.
The filesystem case currently uses a fixed 30-second observation window and
a longer injected return delay; the script has no `--seconds` option. The
observation window is not the exact fault duration. This is a syscall-return
delay, not a physical-device failure or power-loss test.
The installed native `strace`, its `ldd`-resolved loader and libraries are
copied into the disposable application image only, never the product runtime.
Its `ch-info` and `ch-resize` cases place a test-only Unix HTTP relay before
the real CH executable. The relay forwards unchanged response bodies and
holds one actual reply for 12 seconds. The resize is driven by a real Guest
allocation, not by a test-issued resize. Selection requires a target lower
than the preceding real target; the held PUT reply or its immediate confirming
GET exercises the existing mutation-gate/API-lock transaction. Once the old observation expires, Guest
memory must be missing while filesystem/RSS coverage advances, without more
CH requests piling up. Recovery requires a new successful `vm.info` read.
These bounded functional faults do not measure uninstrumented API latency or
production-density overhead, nor identify which caller owns every info read.
The `vsock` case relays the real dedicated usage frames and preserves ordinary
management/MUX traffic. It exercises eight disconnects, one late reply, a
captured old response and slow fragments beyond the original round deadline,
while one managed filesystem syscall remains occupied. Raw Guest responses
must continue to report the same busy slot across new connections. Failed
rounds break Gauge coverage rather than filling zero; native CPU counters
remain a separate source. One observer exec streams resource diagnostics
across all eleven faults. Its first complete output establishes MUX readiness
before injection. Additional health/stop execs run outside the all-FD observation
window while the filesystem is still blocked; no FD samples are discarded.
The relay observes peer EOF while a late or fragmented reply is still held,
so the read-deadline witness is Host closure, not the next request's start.
One intervening tick may correctly remain missing while the original Host
read or Guest admission slot exits. New-connection recovery is checked separately.
Live-query evidence
retains changes to every metric's request ID and compares each metric with
its own raw request; it does not assume atomic publication of a whole round.
The uninjected root/second filesystem must independently return healthy raw
data and advance its own request identity and covered time; agreement between
two failed statuses or retention of an old successful value cannot pass.
The `restore` case keeps one occupied filesystem slot across same-process
quiesce/thaw and then lazy memory restore with the same SandboxID/usage file.
A bounded, test-only tracer owner joins the disposable Guest's root cgroup
before spawning `strace`; PID 1, the application and exec-join processes are
not moved. The Guest reaper owns the adopted helper, which alone waits for
its tracer. This avoids ordinary exec's intentional termination at quiesce.
The test-only CH wrapper relocates the vsock socket in private restore run
state, not in the immutable business snapshot. On the first restored response,
the relay replaces only the epoch with the real previous run's identity,
retaining the new request ID and values. This is explicit identity corruption,
not an unmodified historical frame. Host closure and a new request on another
connection must demonstrate rejection. Raw replies must still report the
original busy slot, while memory and healthy filesystems remain available.
Writes after the original read and after restore distinguish fresh recovery
from a late old buffer; the first new value cannot integrate the missing gap.
Both original and restored tracer owners must detach, and failure cleanup
still stops the owned VMs and reaps Host-side test subprocesses.
Before successful capture, the same case exhausts a test-owned snapshot-output
tmpfs after the immutable dependency preflight. The test requires a data-disk
capture ENOSPC after Guest quiesce/CH pause, MUX reattachment, a real health
exec and CH `Running` state. The occupied slot and its known usage must survive
this failed-capture rollback while healthy sources advance. The output limit
does not affect the usage-file directory; an earlier dependency error cannot
pass as rollback evidence. The temporary mount is unmounted on both paths.
Together with the repeated-operation and lifecycle regressions, these cases
check source isolation, bounded ownership and failure cleanup. Report their
actual observation windows; they are not physical power-loss experiments.

The performance harness is separate from product E2E. Prepare the test probe
and canonical artifact-identity reader in the source-build stage and provide
their executable paths:

```bash
make e2e-usage-probe e2e-artifact-ref
export USAGE_PROBE_BIN="$PWD/build/e2e-tools/$(uname -m)/usage-probe"
export ARTIFACT_REF_BIN="$PWD/build/e2e-tools/$(uname -m)/artifact-ref"
```

Run the [off/on harness](../test/perf/usage_perf.py) without concurrent test
loads, supplying the assembled `BIN` and root privileges:

```bash
python3 test/perf/usage_perf.py --densities 1,4 --seconds 30 --repeat 3
```

The baseline measures native process CPU, RssAnon/RssFile, FD/thread and
non-dead Go G counts, actual record bytes, concentrated-stop duration and
end-to-end exec p95/p99. Go G includes runtime system goroutines; it is not
`runtime.NumGoroutine`. Exact-binary DWARF/symbol inspection requires `gdb`
and Go tools. Diagnostic artifacts retain native proc stat text, individual
utime/stime/guest_time endpoints, read windows, boot identity and tick scale.
`proc-observations.json` preserves completed reads even when sampling fails;
incomplete pairs/windows are explicitly marked and missing endpoints are not
invented. These test artifacts are separate from the compact usage file.

Optional diagnostics can help investigate the baseline; they are not a separate
performance acceptance target or a prerequisite for merging this feature:

```bash
python3 test/perf/usage_perf.py --densities 1,4 --seconds 30 --repeat 3 --trace
```

This optional Linux amd64 tracing run requires a `bpftrace` build
with instruction-offset support, tracefs access and the initial PID namespace
(`BPFTRACE_BIN` can select an installed tool). A preflight rejects differing
kernel and `/proc` PID identities before attaching target probes. It verifies
the real sched exec event of its own short-lived child, not only bpftrace's
version-dependent bare `pid` builtin. Empty or
failed tracing is not zero overhead. It adds wakeups, mallocgc requests/requested bytes, usage
framing traffic, CH info requests, contended API mutex wait and save-worker
duration. It verifies ordinary instruction probes against the exact binary;
it does not insert Go return trampolines. Tracing perturbs timing, so its
latencies cannot replace the untraced baseline. The harness checks available
memory before admitting density and does not change host resource limits.

Measure off/on using the same source set, runtime/kernel/CH, configuration,
density and load. Record those inputs and report Guest/Host CPU, per-process
RssAnon/RssFile, FD/goroutines, actual record bytes, concentrated-stop duration
and business p95/p99 across idle, busy CPU, memory pressure, multiple disks
and saves. This is descriptive feature validation, with no added SLO or
production-density certification.

Wakeups, allocations, management traffic, CH API counts/lock timing and
per-save duration are optional diagnostics. Unavailable measurements stay
explicitly unmeasured, not zero or passed; obtaining a privileged profiling
environment is not a merge prerequisite. Functional correctness, bounded
source/writer ownership, lifecycle and fault regressions, and the repository's
normal review and exact-integration CI remain required. Unit models and
ordinary-process experiments cannot replace CH/KVM evidence. Keep exact
measurement revisions, environments and limitations in the PR and evidence
artifacts; no benchmark number here is inferred from the design.

<a id="related"></a>

## Related documentation

[Runtime: start, control, capture and restore](runtime.md) | [Sandbox configuration reference](configuration.md) | [Sandbox and Snapshot artifacts](artifacts.md) | [Guest memory and lazy restore](memory.md) | [Block storage: devices, writable state and checkpoints](block-storage.md) | [Guest ABI: boot, applications and host protocols](guest-abi.md) | [VMM integration and Cloud Hypervisor patches](vmm.md)

[Component entry](../README.md) · [Native builds and patch materials](../native-deps/README.md)
