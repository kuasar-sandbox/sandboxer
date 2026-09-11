[English](usage.md) | [简体中文](usage_zh.md)

# usage — Sandbox resource usage

## 1. Overview

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

## 2. CLI

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
defaults to SandboxID. `--timeout` bounds an online query (default 5 s,
strictly positive). A nonexistent/refused ctl socket permits offline fallback;
other connection/protocol failures are errors. `--file` implies `--offline`
and can infer SandboxID from the filename without `.usage`.

`--saved` omits `live`. History uses a nonnegative byte cursor and a record
limit of 1–100, default 10. The host usage response is bounded to 1 MiB; reduce
the page size if a page cannot fit. The online reader decodes and encodes one
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
crash or power loss; the writeback boundary is defined in Section 6. `live` includes
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

## 3. Configuration

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

## 4. Metrics, units and arithmetic

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

### 4.1 CPU counters

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

### 4.2 Guest memory and balloon

Guest reads `/proc/zoneinfo` and `/proc/buddyinfo`, matching node/zone identities
and per-CPU pagesets. It returns `PresentPages`, `BuddyFreePages`,
`PCPFreePages`, `PageSize` and domain/status. Populated supported RAM zones
are DMA, DMA32 and Normal; Device/PMEM are excluded. Other populated domains
are unsupported, not guessed from `spanned` or host capacity.

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

### 4.3 Filesystems and host RSS

Disk assembly pins a private CLOEXEC handle to the single writable ext4 root
or the raw ext4 upper behind an overlay, and to each managed data disk.
Switch-root and restored mounts keep those handles valid. Application exec
does not inherit them. Bind/empty volumes on the same disk do not register
additional filesystems. No arbitrary mount enumeration, NFS/FUSE query or
`du` is performed.

```text
filesystem occupancy = (f_blocks - f_bfree) * unit
unit = nonzero f_frsize, otherwise f_bsize
```

`f_bavail` is not used. Existing occupancy after restore is observed in full,
without subtracting a startup baseline. RSS reads only two process `status`
files, converting their kB units to bytes; it never scans `smaps`. These RSS
items are neither all non-Guest memory nor exclusive physical cost.

## 5. Sampling and interval summaries

Default observation is once per second and is merged immediately. There is
no second-resolution sequence in memory or on disk. One reusable dedicated
management connection carries `usage_request`/`usage_response`; raw fields,
admission, timeout margin and quiesce rules are in
[Guest ABI §4.11](sandbox-init.md#usage-observations).

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

## 6. Persistence and file format

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

## 7. Reliability and lifecycle

If sampler initialization fails, usage reports the existing unavailable/error
state and releases its file ownership without saving or truncating a checkpoint.
Existing bytes remain readable offline; a newly created file may remain empty.
There is no uninitialized live/saved view or new closed record for that attempt.

If initialization succeeds but later Host setup or CH spawning fails, bounded
shutdown still attempts to read and save actual sandbox-ctl CPU/RSS. This is
Host process usage, not evidence that a Guest ran. Earlier cumulative totals and complete
record bytes remain intact; the new epoch uses the conservative completeness
rules in Section 4.1. Failing to spawn CH does not exempt consumed Host CPU.

Sampling starts from the Host process lifecycle; the Host starts Guest rounds
only after launch/restore readiness, not merely ctl.sock existence.
Before capture, Host closes usage admission and breaks Gauge continuity.
Guest invalidates the old generation and drains the connection without
waiting indefinitely for a blocked filesystem read. The original source slot
remains busy until the actual operation returns. Restore/attach reopens raw
usage admission before sending its ACK, so the Host's immediate first round
does not race a still-closed gate. True memory restore resets the old Host
epoch/request IDs once before ACK; same-VM attach preserves them. Raw reads
may precede app thaw; exec, plugin, app and resource-controller mem_report
gates still wait for successful thaw. ACK/thaw failure closes newly reopened
usage admission and invalidates that connection with a bounded join, without
pausing an already-live usage stream on ordinary MUX reconnect. A retry
inherits an unfinished reopening's rollback responsibility, so two failed
attempts still close admission. An older failure cannot undo a successful
retry or a later lifecycle generation. No worker is replaced to bypass a
blocked slot.

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
durability guarantee, as specified in Section 6. SIGKILL tests cover process
termination, not physical power loss.

## 8. Performance and validation

Storage grows by cumulative checkpoints, not per-second samples. Record size
depends on vCPU/disk count, source strings and varint magnitudes; 1 KiB is not
a measured constant. CPU/Gauge aggregation and persistence tests are in
[`pkg/usage`](../pkg/usage); protocol and blocking-source tests live with
[`guestlink`](../pkg/guestlink) and [`sandbox-init`](../cmd/sandbox-init).
The component-owned [usage E2E](../test/e2e/e2e_usage.sh) is discovered by
[`run_all.sh`](../test/e2e/run_all.sh), requires real KVM, and verifies the
running Guest init hash against the supplied freshly rebuilt runtime bundle.
Its short-save cases exercise integration. Restore/clone checks use a one-minute
sample interval and require fresh memory/filesystem observations before the
first periodic tick, so retries cannot conceal a missed immediate ACK round.
A separate `defaults` case waits
for the actual default five-minute save. Neither is a production-density
performance measurement.

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

The [storage-fault E2E](../test/e2e/e2e_usage_faults.sh) uses a private bounded
tmpfs for real ENOSPC and path-restricted `strace` injection for usage write
EIO and delayed writes. It also exercises SIGKILL and a sub-second Guest
run. Injection never targets the business writable disk's sync operations;
these tests do not simulate physical power loss. The Host-kill case pins and
verifies its CH child before the crash, then confirms that child's bounded
termination through a pidfd; it does not leave cleanup to the runner.
Linux/Python pidfd support, `strace`, mount privileges
and the ordinary KVM E2E prerequisites are required.

The [slow-source E2E](../test/e2e/e2e_usage_sources.sh) selects one registered
data-filesystem handle by device identity and checks CLOEXEC before delaying
only that `fstatfs` return inside the disposable Guest. It requires a first
timeout, subsequent busy responses from the same occupied slot, fresh memory
and healthy-filesystem coverage, and exactly one traced filesystem call.
Both data disks receive real writes. Bytes written after the delayed read
distinguish a new observation from a replayed old buffer after release.
A single long-lived test exec records all Guest FDs, identities, threads and
RSS; it does not create an exec per observation or label threads as goroutines.
The filesystem case's default fault lasts 30 seconds; `--seconds 60` extends it.
It is a syscall-return delay, not a physical-device failure or power-loss test.
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

Run the [off/on harness](../test/e2e/usage_perf.py) without concurrent test
loads, supplying the assembled `BIN` and root privileges:

```bash
python3 test/e2e/usage_perf.py --densities 1,4 --seconds 30 --repeat 3
python3 test/e2e/usage_perf.py --densities 1,4 --seconds 30 --repeat 3 --trace
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

The separate Linux amd64 tracing run requires a `bpftrace` build
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
density and load. Report Guest/Host CPU, wakeups, allocations, FD/goroutines,
resident memory, management traffic, CH query count/lock wait, bytes per
record, save latency and business p95/p99. Unit models and ordinary-process
experiments cannot replace CH/KVM evidence. Commit/run-specific evidence and
unpassed acceptance remain in the PR and trusted CI artifacts; no benchmark
number here is inferred from the design.

## 9. See Also

- [Sandbox lifecycle](sandbox.md) — configuration ownership, capture and recovery.
- [Guest ABI](sandbox-init.md) — raw observations and transport budgets.
- [Cloud Hypervisor](cloud-hypervisor.md) — existing actual/control contract.
- [README](../README.md) — build, test and release entry points.
