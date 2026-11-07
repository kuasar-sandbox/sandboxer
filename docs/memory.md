[English](memory.md) | [简体中文](memory_zh.md)

# Guest memory and lazy restore

This page owns the RAM data path: external memfd lifetime, missing-page
registration, layered fault reads, speculative population, released-page truth,
and checkpoint change tracking. It explains what can be loaded lazily and which
bytes a new Snapshot captures. The [Runtime restore workflow](runtime.md#restore)
owns startup and VMM readiness; the [capture barrier](runtime.md#capture-barrier)
owns the common freeze with devices and disks.

Memory capacity, Budget, cgroup enforcement and balloon target policy belong to
[resources](resources.md); the VMM patch contract belongs to [VMM](vmm.md).
The Snapshot metadata schema and publication identity belong to
[artifacts](artifacts.md#snapshot-format).

<a id="backing-memory"></a>

## Backing memory and ownership

[`memory.Create`](../pkg/memory/memfd.go) creates the host-owned memfd with
`MFD_CLOEXEC | MFD_ALLOW_SEALING`, truncates it to the configured RAM size, seals
shrink/grow/further seal changes, and maps it `MAP_SHARED` in sandboxer. It applies
`MADV_NOHUGEPAGE` for the handler's 4 KiB page semantics. A failure rolls back
completed construction steps. The inherited fd lets CH map the **same inode**;
vhost memory-table adoption checks that identity instead of accepting an unrelated
backing object.

The host mapping and CH mapping have different virtual addresses. Faults must be
resolved through the descriptor registered on CH's mapping. Creating a second
UFFD over the host mapping would not establish the required CH/KVM fault
ownership. [VMM patch 0001](vmm.md#patch-0001) owns inherited-fd adoption and
[patch 0003](vmm.md#patch-0003) owns handoff and acknowledgement.

`Memfd.Close` unmaps the host view and closes its fd. The kernel retains the inode
while CH still has an fd/mapping, but that fact is not permission to free the
host handler's buffers or borrowed artifact streams early. Runtime teardown stops
the VMM, cancels and joins service work, and only then releases its owned memory
and sources. Address access must remain within `Size()`; mapping lifetime and
operation-context cancellation are separate contracts.

Cold start uses the zero source; restore opens current S and ordered parents as
a sparse layered memory view. A Hole inherits, a Zero masks every lower layer,
and Data supplies bytes. Physical residency alone is insufficient after a Guest
release: the handler's logical release record prevents stale nonzero parent data
from reappearing. The following capture and fault rules preserve that distinction.

<a id="uffd"></a>

## UFFD handler

Memory restore uses one shared memfd and a host handler that adopts CH-side userfaultfd descriptors. CH can report multiple regions of the same memory zone, for example when x86 RAM is split around the PCI hole; those descriptors share the handler's epoll set and address map. S self and `from_refs` are opened as a layered sparse source: resident top-layer data overrides lower layers, a top Hole falls through to its parent, and Zero remains explicit zero.

<a id="uffd-registration"></a>

### Single UFFD contract

The single-UFFD design means faults are handled on CH's mapping rather than registering a second UFFD on sandbox-ctl's backend mapping. It does not require exactly one CH descriptor for every layout. CH registers missing-page events for its restored memory regions; the host receives the descriptors through the restore handshake and validates region bounds and nonoverlap against the shared memfd address map. The first report starts the handler; later valid regions use `AddUffd`. Out-of-range faults, duplicate/invalid region registration, and truncated sources fail closed. See [serveandwait.go](../pkg/sandbox/serveandwait.go), [handler.go](../pkg/uffd/handler.go), and [addrmap.go](../pkg/uffd/addrmap.go).

<a id="fault-path"></a>

### Read path

UFFD uses two-stage population: one fault page followed by a best-effort serial tail. The fault worker first completes the fault page. It may submit the tail only after that page is actually committed as `Loaded`. Each handler retains at most one tail reservation; concurrent faults handle only their fault page when the reservation is busy. The fault page does not wait for a tail reservation or tail-task completion. Its installation still shares `installMu` with tail installation and can wait for an in-flight tail ioctl to release that lock. Concurrent source I/O, shared buffers, and guest population remain bounded. To reuse one physical decode result, `ChunkRun` completes the buffered source read described below before the urgent copy.

Population bounds depend on the final visible Run type:

| Run type | Fault stage | Tail stage | Total maximum per fault |
|---|---|---|---:|
| `Hole`, `Zero`, `Released` | One zero-filled page; normally `UFFDIO_ZEROPAGE` | Up to 15 pages | 16 pages / 64 KiB |
| Ordinary `Data` | Read and `UFFDIO_COPY` one page | Deferred read/population of up to 15 pages | 16 pages / 64 KiB |
| Manifest `ChunkRun` | Read the final visible chunk window containing the fault once; populate the fault page separately first | Populate the following suffix, then the preceding prefix only after success | One current visible window, at most 1 MiB / 256 pages |
| Reclaimed `Loaded` | Restore only the current page | None | One page |

`ChunkRun` means the final serving leaf is a physical Manifest chunk. A chunk is the unit of decryption/decompression and, when enabled, ordinary content verification. The handler therefore avoids repeatedly reading it through ordinary Data's fixed window. `SnapshotReader` still returns only a forward anchor starting at the fault offset. Through a package-local optional capability, `StreamSnapshotSource` calls `fetch.ResolveChunkWindow` and uses metadata from the final composed Stream to expand that anchor to the largest contiguous visible window containing the fault within the same physical chunk. Resolution reads no payload. Upper Holes are transparent; upper Data/Zero, the memory section's end, and discontinuous visibility of the same lower chunk all truncate the window. Physical chunk bounds must not bypass the overlay.

The tail reservation is acquired before window resolution or payload reads. If busy, only the current 4 KiB fault page is read/populated from the original anchor. Even with a reservation, an invalid population candidate or one clipped to only the fault page releases the reservation and uses an urgent-only anchor read. Otherwise, the handler reads a window no larger than 1 MiB into its existing buffer synchronously; a required source read retries the complete window on temporary failure. For an entirely visible chunk within that bound, this is an exact whole-chunk read. If an overlay truncates visibility, the source still performs the necessary chunk-level decryption, decompression, and configured verification internally; the handler receives only the contiguous window that is safe to populate. The fault worker takes the current page from the middle of the buffer for urgent `UFFDIO_COPY`. The tail worker first fills the adjacent suffix with one batch; only if that batch succeeds completely does it fill the adjacent prefix with one batch. Before executing a tail, it rechecks states outward from the fault's adjacent page and truncates at the first mismatch. Any state conflict, partial completion, or ioctl error stops more distant population and later stages without retry. One `ChunkRun` therefore issues at most one urgent copy and two tail copies.

Before workers start, an ordinary Data handler allocates a 64 KiB shared buffer; a source with chunk-window capability uses 1 MiB. A cold `ZeroSource` allocates neither buffer. Fault/tail paths do not grow these buffers or allocate temporary payload buffers; each fault worker holds a fixed 4 KiB urgent buffer. This is a constraint on the handler's additional payload allocation, not on source internals: Run objects, cache leases, and partial-chunk decode allocations remain the `SnapshotReader` implementation's responsibility. On the healthy path the handler calls `SnapshotReader.RunAt` once for a nonzero source; the optional window resolver performs further metadata `Stream.RunAt` calls only within accelerator. Ordinary Data and zero-like runs remain limited by the 64 KiB state boundary. Bidirectional `ChunkRun` candidates contain complete pages only. One scan under the state read lock clips them to contiguous `PageState` on both sides, RAM bounds, and the current CH UFFD region. These guest-population boundaries do not shorten an already admitted whole-chunk source read.

Default CDC chunks have a 1 MiB maximum, but Manifest configuration can admit larger chunks within its decoded-format limit. The UFFD buffer/window cap remains 1 MiB regardless. An unavailable, invalid, or over-limit expanded window retains the original safe forward anchor; if a buffered window cannot fit, the handler makes urgent-page progress without allocating a larger buffer. No oversized-chunk-specific population path is introduced. See [fault.go](../pkg/uffd/fault.go) and accelerator's [chunker configuration](https://github.com/kuasar-sandbox/accelerator/blob/main/pkg/manifest/chunker/chunker.go).

On the normal path without a zero-fill fallback, a tail issues a single multipage `UFFDIO_COPY` or `UFFDIO_ZEROPAGE` for its contiguous valid range; `ChunkRun` suffix and prefix each issue at most one. The kernel processes pages individually and may return a completed page-aligned prefix. The handler conditionally commits only that prefix as `Loaded`, abandoning the remaining tail without retry after the first conflict/error. Batching therefore retains the successful kernel-completed prefix while reducing the ordinary 16-page policy's normal tail to one ioctl and a fully visible 256-page window's 255-page tail to at most two.

Zero filling has a compatibility fallback: when `UFFDIO_ZEROPAGE` returns zero progress with `EINVAL` or `EOPNOTSUPP`, the handler uses `UFFDIO_COPY` from a fixed, package-level zero scratch buffer. That adds an ioctl attempt, not a larger population window or a per-fault payload allocation. The 16-page zero-fill limit is unchanged. See [dirty.go](../pkg/uffd/dirty.go).

`EVENT_REMOVE` makes discarded ranges missing again. Conditional tail-state updates must not overwrite concurrently produced `Released` state. `Handler.Close` rejects new admission, cancels work, explicitly stops the event reader and joins workers/callers before closing the adopted UFFDs and epoll fd. Context cancellation alone does not perform that cleanup. The handler borrows its source and never closes it: the Runtime stops the VMM and drains backend/handler work before releasing the memfd, while restore cancels/joins prefetch before the owning scopes close the snapshot and parent streams.

UFFD directly retries required `Run.ReadAt` operations. The urgent page and any Chunk window containing it are required. A pure speculative tail makes one best-effort attempt. The existing `ChunkRun` capability, buffers, tail reservation and ioctl partial-progress/EEXIST/EAGAIN convergence remain in use. All vhost GPA/UVA accesses first wait for confirmed population through the owning CH UFFD; memory preparation failures terminate the request before used-ring publication. CPU urgent, mandatory backend and speculative tail installations share a gate with kernel REMOVE/UNMAP event read and state publication. A delayed source read is revalidated under that gate; observed release discards historical bytes and does not schedule a stale tail. UNMAP also wakes pending faults on the original UFFD range so kernel GUP/KVM waiters recheck the removed VMA and exit; it never installs replacement bytes or detaches resources before the runner stops. See [VMM integration](vmm.md#patch-0003) for registration, cancellation, ownership limits and the real shared-memfd/O_DIRECT regressions.

The shared [source read recovery policy](runtime.md#read-recovery) retains the operation context and first terminal cause.

<a id="prefetch"></a>

## Memory prefetch

`restore.prefetch: memory` is a host-only optimization. It warms the current S self's file page cache or Manifest chunks, excluding parent memory layers and disk streams. The call receives the opened root stream, so its scope may also include S's bounded ZIP metadata tail; it is not a memory-payload-only section. It changes neither sparse truth, fault ordering, C0, S, nor memory parents. The default is `off`. An invalid configured mode fails validation before side effects; lack of prefetch capability or a prefetch I/O failure is best-effort and does not fail restore. It is logged and restore continues on demand. The asynchronous task is canceled and joined before its streams close. See [prefetch.go](../pkg/restore/prefetch.go) and its call in [restore.go](../pkg/restore/restore.go).

<a id="checkpoint-changes"></a>

## Checkpoint changes, released pages and history merging

The current memory layer preserves released content even before it faults again.
After CH/device pause and backend quiescence, capture jointly samples memfd holes
and the existing UFFD page states under the installation lock. Resident Data wins;
Hole+Absent inherits the parent; Hole+Released or Loaded is explicit Zero. The
Loaded case covers zero installation followed by CH's final punch. This describes
the logical captured state; whether materialized Data is emitted also depends on
the capture mode and dirty selection described below. The immutable
layout is streamed after releasing that lock; no handler state is consumed or
reset on capture success/failure. Zero remains opaque through local merge,
tarstream, Bundle/ingest, publication and restore. No full-RAM materialization,
new ZIP entry or cumulative dirty log is required. See [release fix #378](https://github.com/kuasar-sandbox/sandboxer/issues/378).

With cumulative CH logging, default `merge_ref=true` captures changes since
the instance's fixed runtime baseline B, then absorbs the eligible local history.
`merge_ref=false` keeps the complete current working set plus necessary explicit
Zero, and compacts eligible local history into a separate H. A single local parent
with dirty metadata follows the same trimming rule as several parents; a single
legacy/all-dirty parent without that metadata can be reused directly as H. With
no eligible local prefix, existing memory refs are retained without creating H. Capture success, failure and mode changes
never reset CPU/CH/Go/release records or advance B. Compatibility without the new
CH API keeps the full current working set in both modes; historical trimming
still uses metadata already present in parent layers.

The exact optional `memory.dirty` encoding, presence semantics, limits and reader compatibility are defined by the [Snapshot artifact schema](artifacts.md#memory-dirty). The mechanisms below select and transform those labels; restore continues to use full Memory.

History uses the union of effective dirty candidates, but obtains final values
from the original complete newest-first layered Memory. A layer's clean labels
are used only when its declared ordered parent view equals the actual suffix;
otherwise all its explicit content is retained. Current Data and Zero override
history; Hole inherits. Only the newest S supplies execution state and sandbox_ref.
Only physical-ref changes preserve the bitmap verbatim. Changed memory or an
unproved rebase, including rewrite --skip-verify, recomputes labels or becomes
all dirty; content/metadata changes create a new S identity.

Before output, enforce the existing 64 MiB ZIP tail, carrier extent and index
budgets. A new bitmap alone exceeding the ZIP budget can fall back to a three-entry
all-dirty capture; ordinary publish must preserve existing metadata. Sparse-map
overflow retains extra Data only within the old per-artifact coverage, reading
final values from the full current view. Necessary Zero never becomes Hole.
Fallbacks report their reason and extra Data; unchanged budgets still apply.
Size evidence must include payload, bitmap/header/ZIP overhead, top/H storage and
identity-deduplicated reachable local artifacts under identical codec/parent/state.
The native path uses MISSING plus KVM/CH/Go logs on both architectures, with
conservative x86 read-materialization reporting. The Host Linux 5.10 baseline is
unchanged. See [CH capability and compatibility](vmm.md#patch-0012).

<a id="related"></a>

## Related documentation

[Runtime: start, control, capture and restore](runtime.md) | [Sandbox configuration reference](configuration.md) | [Sandbox and Snapshot artifacts](artifacts.md) | [Block storage: devices, writable state and checkpoints](block-storage.md) | [Resource control and usage accounting](resources.md) | [Guest ABI: boot, applications and host protocols](guest-abi.md) | [VMM integration and Cloud Hypervisor patches](vmm.md)

[Component entry](../README.md) · [Native builds and patch materials](../native-deps/README.md)
