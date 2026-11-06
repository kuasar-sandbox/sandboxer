[English](block-storage.md) | [简体中文](block-storage_zh.md)

# Block storage: devices, writable state and checkpoints

This page owns the complete block-storage subsystem: immutable lower layers,
the vhost device boundary, shared writable cache, logical Data/Zero/Hole state,
DIFF persistence, checkpoint readers and garbage collection. `pkg/vhost` owns
transport; `pkg/diffstore.Disk` owns writable storage independently of transport.

For library integration, begin with the layer/ownership tables and `OpenDisk`.
For a running sandbox, use the checkpoint commands and completion boundaries
before administering retained files. [Configuration](configuration.md#disk-capacity)
owns capacity inputs and cache policy; [Runtime](runtime.md#capture-barrier) owns
multi-disk/VM atomicity; [artifacts](artifacts.md#offline-reset) owns offline E
construction and selective graph retention. A single-disk commit alone is not a
successful VM snapshot.

<a id="checkpoints"></a>

## Local disk Checkpoints

Local disk Checkpoints are opt-in. Enable the storage backend at launch, then
choose each capture explicitly:

```bash
sandbox-ctl run --config sandbox.yaml --sandbox-id demo --path-id worker \
  --base-root /var/lib/sandbox --enable-checkpoint
sandbox-ctl snapshot --path-id worker --checkpoint cp-001 --json
# Use export instead of snapshot for disk/config only; add --resume to keep running.
```

`PathID` defaults to `SandboxID`. All three launch modes (`--config`, `--from`,
`--restore`) derive `CheckpointDir = BaseRoot/PathID/checkpoint`; writable root
and data disks live in `CheckpointDir/diffs/<DiffID>.diff`. These files survive
normal exit. There is no second launch directory setting. A fresh run assigns
new disk identities. Existing raw/v1/v2 working files are rejected;
export with the old executable and restore the portable artifact into new files.
An explicit writable path outside this derived
directory, or a filesystem without the required locking, fails before launch.

Enabling the backend does not change the default capture: without `--checkpoint`,
snapshot/export still copies disk contents. A nonempty `--checkpoint` creates that
ID on every writable disk at the same capture barrier. It accepts `--mode local`
or `--mode bundle`, but not `--upload` or `export --from`. The running owner supplies
its directory when `--output` is omitted; an explicit output must name that same
derived directory. Ordinary captures still require `--output` or `--upload`.
The default stops the VM; `--resume` retains the original running baseline.
An unsupported instance returns the dedicated CLI status **78**.

Use the returned `sandboxRef` with `run --from` or `snapshotRef` with
`run --restore`. Resolve basename-only reports relative to their actual output
directory. Restore pins all source versions before acquiring the target directory
owner. With `--enable-checkpoint`, the same physical directory can reopen its
existing DIFFs only when every disk's identity, immutable base graph, logical
capacity and active epoch still match the capture. A later write, another target
directory, or a launch without the enable flag instead materializes the pinned
versions into ordinary lower artifacts. Cold start preserves caller overrides;
memory restore retains its existing execution-state rules. Source paths and the
target's BaseRoot/PathID are independent.

```bash
sandbox-ctl checkpoint list --path-id worker --json
sandbox-ctl checkpoint delete --path-id worker --checkpoint cp-001
# Offline management takes exclusive directory ownership; a live owner means busy.
sandbox-ctl checkpoint list --checkpoint-dir /var/lib/sandbox/worker/checkpoint
sandbox-ctl checkpoint delete --checkpoint-dir /var/lib/sandbox/worker/checkpoint \
  --checkpoint cp-001 --diff-id ROOT_DIFF_ID --diff-id DATA_DIFF_ID
```

Online management selects `RunRoot/PathID/ctl.sock` (also accepts `--sandbox-id`
and `--run-root`). List returns all visible versions across current and historical DIFFs,
including versions left by interrupted captures; `--json` emits exact
`{diff_id,id}` pairs. Deletion selects that fixed set before making changes.
For a partial deletion, retry the reported identities with repeatable `--diff-id`;
do not rediscover a changed capture name. New opens fail after logical deletion,
while already pinned exports finish and delay bounded reclamation. Deletion can
therefore invalidate a local E/S without immediately returning all space.

```bash
sandbox-ctl checkpoint gc --checkpoint-dir /var/lib/sandbox/worker/checkpoint --json
```

This offline command takes the existing exclusive directory lock; a running
directory returns busy. It uses metadata-only directory maintenance, with no
artifact download or data decryption/key dependency. It accepts no Checkpoint ID
or deletion filter and does not change retention. Per-DiffID output contains
`diff_id`, `state` and committed `freed_slots` (plus `error` on failure). It drains
`more` work until each file is `complete` or `blocked_by_readers`; the latter is a
normal exit-0 result with pending reclamation, not a promise that space is free.
Retired files report `skipped_retired` and remain untouched. Busy, cancellation and
real I/O/metadata errors return nonzero; other files still receive results when a
single file fails. Text output explicitly explains reader-blocked results.

Runtime maintenance starts independently after all active DiffIDs are bound:
normal startup, actual pooled Launch, and restore all borrow the acquired target
directory owner. A parked pool does not take a business directory. Restore source
export pins and the target owner are separate resources. Historical GC and online
List/Delete share bounded, serialized metadata handles with reserved management
capacity. Historical errors are logged by DiffID without terminating an unrelated
active sandbox. Shutdown and initialization failure stop new operations/scheduling,
join accepted operations and the current batch, close historical handles, close
active disks, then release the owner. Deleted version descriptors, group metadata,
fixed areas and active roots remain; GC does not truncate files or recycle IDs.

```bash
cd /var/lib/sandbox/worker/checkpoint
sandbox-ctl publish --to-ref-location archive=file:///var/lib/sandbox/published \
  --delete-checkpoint --json '<snapshotRef>'
```

Every publish path, including reference replacement/reduction and Bundle inputs,
materializes selected disk versions into ordinary output with no `checkpoint.json`.
The input artifacts stay unchanged. Default publication retains local versions.
`--delete-checkpoint` waits for successful publication, successful publisher close
and release of its export views, then deletes only the exact selected disk/version
set and checked source-directory identity. A live owner receives the request
through its owner-token-checked local route. Cleanup never re-resolves input
aliases and is independent of `removedRefs`. If publication succeeds but cleanup
fails, CLI status **79** accompanies the still-valid ordinary result/JSON; retry
cleanup without discarding that result.

SDK entry points use the same rules: `RuntimeSpec.EnableCheckpoint`,
`RunOptions.EnableCheckpoint`, `restore.Options.EnableCheckpoint`,
`ctl.Request.CheckpointID`, `Runtime.ListCheckpoints/DeleteCheckpoint`, and
`ProcessStorage.ListDiffCheckpoints/DeleteDiffCheckpoint`. After both
`Publisher.PublishWithOptions` and `Publisher.Close` succeed, callers may invoke
`ProcessStorage.DeletePublishedDiffCheckpoints(ctx, result)` and report that error
separately before closing storage. No caller needs to open a Disk to manage
offline metadata or publish a fixed version.

Embedding callers may supply an opaque `ProcessStorage` in `LaunchSpec`;
Checkpoint materialization preserves its manifest fetcher and local encryption
policy. `NewProcessStorageWithLocalAccess` can borrow a fetcher, and `Borrow`
retains the complete storage access capability. Closing a borrower does not
close the original clients; the owner must remain open until borrowers finish.

At the block layer, `Disk.Checkpoint(ctx, id)`, `ListCheckpoints(ctx)`,
`DeleteCheckpoint(ctx, id)` and `Export(ctx, id)` use `diffstore.CheckpointID`.
A named `Export` returns a sparse source and closer that owns its reader pin;
close it when finished. `Export(ctx, diffstore.ActiveDiff)` instead requires the
caller to keep the active disk quiescent. Single-layer v3 backends report
`diffstore.ErrCheckpointUnsupported` for named checkpoint operations.

Checkpoint DIFF v4 stores sparse 4 KiB mappings and retained roots in one local
file per disk. Its physical length is not disk capacity. Hole falls through to
the immutable lower graph; an explicitly written zero masks that lower data.
Encryption uses the process customer key under the existing local-crypto policy;
opening data needs the key, while listing/deleting metadata does not decrypt guest
data. A directory has one writer owner, but fixed-version exports hold independent
read pins and can coexist with resumed writes. This is a local Linux filesystem
feature, not distributed shared storage. Creating a checkpoint drains writes and
commits metadata; initial template import and memory capture still take time.
Retained versions cost space and post-capture writes may incur index/data COW.
It does not promise constant total pause time, universal speedups, or guest-data
durability across host crashes/power loss beyond the existing FLUSH contract.

Reopening a v4 DIFF loads the complete one-bit-per-4-KiB presence bitmap P
(at most 1 MiB net for the 32 GiB checkpoint capacity limit). P=0 is Hole and falls through
to base. P=1 with no payload is Zero and masks base; P=1 with payload is Data.
Accepted cache writes count as Data. Ordinary zero writes remain Data; they are
not scanned or compressed. Open still validates ownership, identity/key/capacity,
committed redo, the root and current Checkpoint before returning. It does not
scan the complete B+ tree. Visited mappings and adjacent subtree boundaries are
validated before trusting absence; detected corruption is fatal, never Zero or
a base fallback. Unvisited subtree corruption can be detected later.

The former unknown-presence bitmap and recovery scan are removed. Presence
coverage is complete after Open, and dirty-block statistics count P, including
Zero pages. Export classifies physical Data in bounded queries. A named export
owns an immutable root and P copy; an active export includes accepted unflushed
cache writes. Historical/cross-directory restore still materializes its version,
preserving full Zero pages without allocating payload. This does not remove
template, decryption or Data-copy costs.

Restore source readers are temporary. Same-directory hot restore and cold launch
(CLI, SDK `Start`, and `StartRuntime` + `Launch`) retain the complete source set
until the root and every data-disk writer have been opened and registered. They
then close those source pins before reporting success. Historical/cross-directory
restore, including a target without Checkpoint enabled, closes them after full
materialization and its publisher Close succeed. The directory owner, active
disks, borrowed storage access and materialized lower files keep their required
runtime lifetimes. A pin-close failure fails startup; error cleanup also closes
partially prepared resources, and repeated pin cleanup is safe.

Releasing these readers does not delete a Checkpoint name or close another
publisher's fixed view. Explicit Delete can remove name visibility while real
export readers still protect its old pages. Once the last such reader closes,
the existing GC can retire that protection and reclaim obsolete content without
waiting for the resumed sandbox to exit. Until then, first writes to protected
old pages may require CoW; already-private repeated overwrites do not. Delete is
still not synchronous space reclamation.

The expected benefit is largest when a large mapped index would otherwise be
scanned but startup touches only a subset. A small sparse index is already cheap
to scan, so lazy bookkeeping can instead increase the first workload's total
time. Full-disk startup reads or an immediate ordinary export still need the
complete index; shorter Open alone does not establish an end-to-end improvement.

`pkg/diffstore` owns writable block storage independently of vhost transport. Both
single-layer v3 files and checkpoint-capable v4 files use the same `Disk`
frontend. Both store logical presence separately from physical payload. Legacy
raw/v1/v2 working files are rejected without modification. Export
them with the old executable before upgrading; existing portable tarstreams remain
readable and can seed a new-format DIFF. There is no in-place migration.

<a id="storage-layers"></a>

## Layers and dependencies

| Layer | Owns | Must not own |
| --- | --- | --- |
| Sandbox / restore / artifact | Disk topology, immutable base composition, VM pause, multi-disk checkpoint coordination, publication | Physical page layout or cache state |
| `pkg/vhost` | Protocol, virtqueues, guest memory, request statistics; queue context forwarded to `Disk` | Writable files, COW cache or checkpoint transactions |
| `pkg/diffstore.Disk` | Admission/lifetime, logical presence, partial writes, discard, shared cache attachment, template materialization and export boundaries | VM or multi-disk atomicity |
| Private `diffBackend` | Synchronous logical-page I/O, presence, format-specific failure cleanup, sync and close | Frontend cache pages or ownership of borrowed page payloads |
| Single-layer `diffFile` | v3 header, presence pages, sparse payload offsets, owned direct-I/O workspaces | B+ tree or checkpoint references |
| `internal/versioned` via `checkpointDiff` | v4 presence, B+ tree, transactions, recovery, version references, reader pins, garbage collection | Frontend admission or cache draining |
| `internal/directio`, `internal/sectorcrypt` | Shared mmap alignment/lifetime and logical 512-byte XTS transforms | Pool policy, file headers, authentication or mapping policy |

The dependency direction is transport/application -> `pkg/diffstore` -> its
private implementations. Filesystem preflight helpers sit above the physical
backends; neither physical backend interprets ext4. Immutable fetch streams use
`StreamReader` as a source adapter. The v4 implementation is inaccessible to other
packages through Go's `internal` rule.

The public `Store`/`Options` aliases and `Create`, `Open`, `OpenMetadata`, `Probe`,
`OpenExport`, and `LockDirectory` functions expose the existing v4 engine handle
for **offline** import, inspection and administration. They are intentionally
separate from `OpenDisk`: they neither attach the sandbox cache nor provide a
live disk's admission/drain boundary. Applications must not open a second writable
`Store` for a live `Disk`, or use offline APIs to bypass that disk's checkpoint
boundary. The administrative API reuses the same version-management implementation. The v4 `ErrEncryptionRequired` and
`ErrAuthentication` sentinels are public for `errors.Is` checks through all v4
entry points; they are distinct from the single-layer `ErrDiff*` sentinels.

<a id="open-disk"></a>

## Choosing a format

```go
// Single-layer v3 by default; WithDiffEncryption selects XTS payload encryption.
disk, err := diffstore.OpenDisk(path, base, init,
    diffstore.WithCache(cache))

// Explicit stable identities select checkpoint-capable v4.
disk, err := diffstore.OpenDisk(path, base, init,
    diffstore.WithCache(cache),
    diffstore.WithCheckpoint(diffID, baseID),
    diffstore.WithDiffEncryption(customerKey, true))
```

Check `err` before using a disk. `DiffInit{Existing: true}` opens an existing file;
`CreateSize` or `TemplatePath` provisions a fresh file. v4 opens authenticate and
check the expected identity/base and never adopt older files. A plaintext raw
provisioning template remains allowed when the new active file must be encrypted.

| Property | Single layer (v3) | Versioned (v4) |
| --- | --- | --- |
| Runtime | Same `Disk` and shared `Cache` | Same `Disk` and shared `Cache` |
| Existing-file presence | Full logical P load; sparse scan identifies physical payload only | Full logical P load; bounded B+ queries identify payload |
| Writeback | Bounded batches may contain disjoint pages; each changed P page is published once per batch | Bounded transactions may contain disjoint logical pages |
| Template staging | Up to 64 pages (256 KiB) | Up to `maxWritebackPages` (256) 4 KiB pages |
| Checkpoint capability | Unavailable | `CheckpointManager` |
| Physical failure cleanup | Hole-punch only newly created upper pages | Transaction/allocation engine owns recovery; never punch guest offsets in the mixed physical file |

The private batch describes a logical block and either borrowed plaintext Data
plus its fresh-page flag, or an explicit Zero with no payload. It exposes no
`cachePage`. The cache freezes and charges pages until write and failure cleanup
finish, and publishes sticky failure before cache cleanup can block. The
single-layer backend stages Data into its existing workspace; v4 commits Data or
Zero through its mapping engine. See [Discard](#discard-and-reclamation) for
Zero batching and the bounded pre-write space-pressure recovery.

Fresh creation shares the claim/recheck/template/temp-file flow, and publication
shares file sync, atomic rename, claim release and directory sync. Materialization
uses one dispatcher and the same stripe/presence conflict checks. v4 retains
batched template commits; single-layer batches at most 64 pages to amortize
presence publication and contiguous payload writes. Its temporary staging grows
from 4 KiB to 256 KiB only while materialization runs. Template bytes
are read before taking write stripes, then presence is rechecked so frontend
writes/discards win. A fully superseded range may consequently be read and then
skipped. `WaitMaterialReady` and `WaitMaterialized` retain distinct readiness and
publication boundaries.

Fresh checkpoint-backed live disks initialize the format asynchronously, with
or without a template. Preparation still validates identity, capacity (including
template-derived capacity), encryption and exclusive ownership. It establishes
an independent empty frontend P bitmap and the existing bounded cache before
`OpenDisk` returns. Offline `Create` runs the same initializer synchronously;
existing-file open still validates committed metadata and loads the real P bitmap.
No file format or configuration switch is added.

| Boundary | Completed work |
| --- | --- |
| `OpenDisk` returns | Validated ownership and logical view; absent upper pages read template/base/zero and cached writes may proceed. The final path may not exist yet. |
| Internal initialization completion | Truncation, fixed reservation and the existing header/control/redo/batched bitmap writes completed, or one terminal error was recorded. Backend I/O and GC wait for success. |
| `WaitMaterialReady` succeeds | Initialization completed and the view no longer depends on the template. This precedes cache drain so checkpoint can quiesce writers. |
| `WaitMaterialized` succeeds | Initialization, optional template import, required writeback, file sync, rename, claim release and directory sync completed. This does not synchronize subsequent writes. |

An empty `Drain` does not wait for initialization. Physical path handoff/reopen,
active export and snapshot publication use `WaitMaterialized`. Immutable device
identity, size, health checks and fresh checkpoint invalidation do not wait for
format I/O. Discard and template backfill wait before taking frontend stripes.
The one cache worker selects the oldest eligible dirty page across its bounded
queue, skipping pending backends before freezing pages; a pending prefix longer
than a batch cannot hide ready work. Pending pages remain readable and rewritable,
but still consume the shared quota. If all work is pending, the worker sleeps
until completion, new eligible work, failure or shutdown wakes it.

Initialization is admitted once before fallible frontend attachment and is joined
directly on construction failure. Request cancellation affects only the waiter;
Close joins actual initialization I/O before releasing the file, lock or buffers.
Successful initialization establishes GC ownership before publishing completion.
The existing bounded MAP_SHARED direct-I/O workspaces remain safe while Guest
process creation overlaps this work. There is no per-page initialization state.

Late initialization/publication I/O errors are sticky runtime failures and may
follow previously accepted cached writes. Completion is published before fatal
callbacks, including late handler replay. A failed returned disk retains template
readers and private resources until Close stops admission and joins existing
users. Disposable Close may stop unfinished template work and prevent publication
after initialization, but cannot cancel the initializer or hide a real error from
an already admitted publication. Retained Close drains and publishes normally.
Intentional disposal is a successful Close, but readiness/publication waiters
receive cancellation when their boundary was not completed.

This removes avoidable foreground waiting, not initialization work. Early backend
use, cache pressure, immediate checkpoint/Close and concurrent disk initialization
can still wait or contend for I/O. Compare a change against a synchronous batched-creation baseline
using the same bitmap path, capacity, encryption, templates
and cache budget; retain end-to-end startup tails and failures as well as logical
open, first useful I/O, Ready, publication and Close timings.

<a id="ownership-and-completion"></a>

## Ownership and completion

- `Disk` owns its backend, materializer and its attachment to the cache. Its
  immutable `BlockReader` base is **borrowed**; keep it open until `Disk.Close`
  completes, then close it separately. `StreamReader.Close` closes the wrapped
  stream, not separately owned fetcher/cache clients.
- Without `WithCache`, `Disk` creates and closes a private cache. With `WithCache`,
  the caller owns the shared cache: close all attached disks before the cache.
  A sandbox's writable devices continue sharing one budget and one worker.
- `ReadAtContext`/`WriteAtContext` carry queue cancellation across the package
  boundary. `ReadAt`/`WriteAt` remain conventional context-free entry points.
- Guest `Flush` checks health; it does not promise media persistence. `Drain`
  waits for accepted Data and Zero writeback to finish. A checkpoint additionally performs
  the versioned persistence boundary. Do not equate request acceptance or Drain
  with a durable checkpoint.
- `Disk.Close` excludes new operations and joins admitted work before releasing
  storage. `WithDiscardOnClose` permits cancellation of unfinished template work
  for disposable runtime-owned disks; it does not transfer ownership of a base.

<a id="checkpoint-readers"></a>

## Checkpoints and exports

```go
manager, supported := disk.Checkpoints()
if supported {
    err = manager.Checkpoint(ctx, diffstore.CheckpointID("snapshot-1"))
}
```

The manager shares the disk's lifetime and routes operations through the live
admission/cache boundary. A checkpoint waits for template readiness, pauses new
frontend operations, drains accepted Data and Zero, waits for materialization publication,
and commits the named version. Multiple disks still require application-level
coordination. Existing direct `Disk` checkpoint methods remain available, but
`Checkpoints()` makes format capability explicit without probing via errors.

`OpenActiveView(ctx)` returns an **upper-only** sparse view that includes accepted
cache Data and Zero. The caller must keep the disk open and quiescent until the view is
closed; this is neither a persistent checkpoint nor a base-composed disk image.
`OpenCheckpoint(ctx, id)` requires a nonempty ID and returns an immutable reader
with an independent pin. It survives subsequent writes and owner close; close the
view to release its pin. `Export(ctx, ActiveDiff)` remains the lower-level spelling
of an active view, while an empty ID is rejected by `OpenCheckpoint`.

Live reopen loads the complete logical presence bitmap, without a full B+ scan.
Named exports load their own immutable bitmap and root. Payload lookup validates
visited index pages and adjacent subtree boundaries; corruption is an error,
never a Zero or lower-layer Hole. Unvisited subtrees are not audited at Open.

<a id="presence-and-layout"></a>

## Presence and physical layout

Writable logical capacity must be positive and divisible by 4096. Single-layer
v3 supports up to 1 TiB; checkpoint v4 remains limited to 32 GiB because its
active bitmap occupies a fixed reserved region before the record area. These
limits apply to active DIFFs and template-derived writable capacity, and exclude
read-only devices, RAM, artifact metadata tails and physical storage retained
by old versions. One presence bit per 4 KiB page uses at most 32 MiB of net
bitmap bytes for a 1 TiB disk, or 1 MiB for a 32 GiB disk. These are per-bitmap
sizes, not total backend, frontend or cache memory bounds.

The shared capacity check admits up to 1 TiB. The v4 backend separately checks
its 32 GiB limit on creation and open, before initializing a fresh checkpoint
file. Upper-level template or runtime preparation can precede that rejection.

| P | Effective payload | Logical result |
| --- | --- | --- |
| 0 | Ignored, even if stale physical bytes exist | Hole; read the lower layer |
| 1 | None | Zero; mask the lower layer without a payload page |
| 1 | Present, including accepted cache writes | Data |

Ordinary writes set P and store Data without scanning for zero bytes. A partial
write to Zero starts with zero-filled bytes, never lower data. Template and merge
composition stop at Zero. Full template Zero pages set P without writing Data;
partial Zero spans use ordinary page composition. The modification fence also
applies to P-only changes. Guest FLUSH and materialization durability are unchanged.

Single-layer v3 starts with a 4096-byte header describing logical capacity, body
offset, bitmap offset/length/page count and encryption. Checksummed bitmap pages
follow it; each has a 32-byte header, 4060 bitmap bytes and a 4-byte CRC. The body
starts after all bitmap pages. Header/P allocation is excluded from payload extent
enumeration. XTS uses logical 512-byte sectors and never decrypts a physical hole.
Payload is written before publishing changed P pages; discard punches before P
publication. Even P=0 to Zero removes residual payload first. A Zero's first Data
write is fresh, allowing failed-write cleanup to restore its committed P state.

Versioned v4 places the same bitmap pages at physical page 1600, after the bounded
redo region and before the record area at page 2048. They are reserved on creation
and loaded on Open. Changed P pages use the existing atomic metadata transaction;
recovery and read-only redo interpretation validate their exact permitted range.
The backend serializes bitmap updates and merges changed bits/pages; the frontend
never overwrites backend P with a stale full bitmap.

A Checkpoint atomically stores the root plus an independent complete P copy in
ordinary allocator records (at most 259 pages, 1 MiB net). It copies no Data. Its
cost includes bitmap copying, allocation and transaction metadata, including
fragmented free slots; the redo limit remains 1536 existing page images. Reader
pins protect both root-reachable data and bitmap records. Delete only withdraws
the version and marks work pending. Bounded GC sends exclusive P records directly
to the global reclaimable list, persists partial progress, and leaves the version
pending until that copy is exhausted. It needs no payload decryption key.

<a id="discard-and-reclamation"></a>

## Discard and reclamation

Managed writable ext4 mounts enable `discard` by default, so filesystem block
release can notify the backend automatically; see the [mount scope and rollout
boundary](guest-abi.md#guest-block-discard). This reduces obsolete active payload
and payload in subsequent disk exports. It does not shrink immutable parents or
existing snapshots, reclaim an EROFS lower when overlayfs creates a whiteout, or
automatically trim all space that was already free before enabling the option.
Deleting a file, committing Zero and reclaiming host allocation are separate
boundaries. Measure allocated blocks separately from the DIFF file's logical
length, which need not shrink.

`Disk.Discard(offset, length)` publishes logical Zero with P=1 for fully covered
4 KiB pages, masking even nonzero base data; partial edge pages retain their
contents. With v4, completion normally means admission into the bounded cache,
like `WriteAt`, rather than completed backend mapping removal. The single-layer
path remains synchronous. The public, page-aligned
`Store.Discard(ctx, offset, length)` also remains synchronous. Neither cache
admission nor backend batching makes the whole range atomic; cancellation or a
later failure can leave an accepted or committed prefix. Ordinary writes of zero
bytes remain Data; this adds neither WRITE_ZEROES nor content-based compression.

Each pending v4 Zero consumes one logical page of the existing shared total and
dirty quotas. Replacing an unselected dirty page reuses its reservation and FIFO
position. Same-page loading, construction or frozen writeback must finish first;
quota waits release the frontend stripe. The cache publishes Zero and frontend
presence before ACK, so older Data writeback cannot resurrect a discarded page.
Zero has no 4 KiB payload. Data-to-Zero clears and returns the old buffer to the
bounded Data pool; a later partial write acquires a zeroed buffer and preserves
zero outside the written span without reading the old backend, template or base.

The existing single worker batches one disk and one kind at a time: at most 256
Data pages in the 1 MiB workspace, or 1024 Zero pages without a payload workspace.
Normal selection retains the oldest eligible FIFO anchor. `Store.DiscardPages`
validates a bounded set of distinct blocks, combines adjacent ranges and removes
their mappings with presence and retirement updates. Its exact existing-page
redo-image count is checked before preallocation or data/redo writes; an oversized
window is rebuilt as smaller transactions within the unchanged 1536-page budget.
This bounds batch size, not Store-lock wait time or the latency of a fragmented
range.

Only `Store.WritePagesForCache` can defer exhausted Data-reservation
ENOSPC/EDQUOT as `SpacePressureError`: failed-slot cleanup must have succeeded,
the failing transaction must have written no Data/redo, and the Store must remain
healthy. Earlier 64-page transactions may already have committed. The worker
keeps the original Data window frozen, captures the same disk's currently dirty
Zero pages, completes that finite cohort in batches of at most 1024, then retries
the original Data window exactly once. New Zero arrivals cannot extend this
recovery. Active-I/O ownership and unfinished quotas remain held throughout;
Zero failure, retry failure or an independently fatal cache stops progress through
the existing sticky-failure path. Ordinary `Store.WritePages` retains its original
validation, duplicate-block behavior and sticky reservation-failure contract.

When either quota blocks admission of a cold/clean Zero and no already charged
Zero on that disk can release quota, DISCARD uses the synchronous single-page
path without a new reservation or first kicking queued Data. It still waits for
same-page loading/construction/writeback. This avoids a quota deadlock; it does
not guarantee recovery from arbitrary host-space exhaustion or supersede I/O
already in progress. No extra worker, unbounded request queue or tuning option is
introduced.

Successful Zero writeback removes the cache entry and releases both quotas; it
does not enter the clean LRU. Data retains its normal clean-cache behavior.
`Drain`, named Checkpoint and normal Close wait for accepted Zero writeback. Pages
that remain Zero at that boundary have no active Data mapping and report
PayloadPresence=false. Retained Checkpoints/readers can still protect old records,
and actual host allocation falls only when GC can reclaim them. After initial
publication, [active export](#snapshot-view) can already omit pending Zero's old
payload without another Drain. Fresh disks still require `WaitMaterialized`,
including its initialization drain. DISCARD and Guest FLUSH add no stable-storage
durability guarantee.

The synchronous single-layer path retains batches of at most 64 full pages. It
takes distinct stripe locks in numeric order, checks all existing cache pages
under one cache lock, and only then freezes them together. Loading or writeback
makes the whole batch wait without freezing any part. Uncached pages require no
placeholder and unrelated dirty pages need no drain. Backend I/O runs outside
the cache lock. Cancellation can stop a batch before freezing; afterward it must
publish the whole batch or enter sticky failure and wake waiters. Success
publishes Zero for every page, including previously absent pages.

Both paths use the normal presence bit to prevent template Data/Zero backfill
from overwriting foreground content, without a separate template-resolution
bitmap. Unmodified template Holes still fall through to the base. Normal presence
publication only sets bits. Failed first-payload writes restore the old committed
bitmap after cleanup of newly allocated payload: Hole retains P=0 and Zero P=1.

`DeleteCheckpoints` only withdraws visibility and immediately releases protection
when no reader pins the version. It never runs or waits for GC. `OpenMetadata`
and List also do not schedule GC. Active stores have an independent worker;
metadata-only owners explicitly schedule maintenance.

`Store.GC(ctx, budget)` returns `GCResult{State, FreedSlots}`. `Complete` means no
pending versions or globally reclaimable records remain. `More` means bounded
work remains and must be retried even if `FreedSlots` is zero. `BlockedByReaders`
is returned only when the global reclaimable chain is empty and a valid complete
round found all remaining pending versions pinned. A later reader close permits
progress without another deletion. `FreedSlots` counts only locations committed
to the reusable free list, never scans, transfers or punch attempts.

Version visits, predecessor discovery and ownership transfers have bounded work;
global release and version work take turns using full bounded batches, including
budget 1, avoiding two half-sized metadata commits when both are ready.
The active worker waits 1 ms outside the store lock between `More` batches,
interruptibly on shutdown, to reduce competition with foreground work. Explicit
GC and bounded foreground allocation assistance do not incur this delay; the
tradeoff is lower peak background reclamation throughput.
Pure cursors and predecessor search progress live in memory, not redo. A round
captures its tail; new Checkpoints cannot extend it indefinitely. Ordinary writes
do not reset it, and new deletion invalidates a stale blocked conclusion. Reopen
can repeat pure scanning while persisted ownership changes remain recoverable.
The active worker yields between `More` batches and checks reader-blocked work
less frequently. Allocation failure may perform a finite amount of foreground
assistance, retrying only after committed frees; GC never recursively allocates
through that assistance path.

Only the global reclaimable chain is punched. A batch validates unique candidates
and legal states, sorts by physical file page address, and merges physically
adjacent ranges. Descriptor pages between each 128 record positions and all live
pages split ranges. Punching finishes before one existing metadata/redo transaction
removes the old chain entries and publishes free descriptors. The batch links in
ascending address order, then joins the former free head. Failure before commit
publishes no free slots; reopening may safely repeat a punch. GC itself adds no
format version, larger redo area, payload relocation, file truncation or global
free-range index. Encryption and direct-I/O alignment retain their existing rules.

`DirectoryMaintenance` borrows an acquired `DirectoryOwner`, validates its stable
lock identity, and registers all active DiffIDs before enumeration. It never
acquires a second writer for an active disk or releases the owner. Historical
enumeration uses bounded batches; two GC handles and one transient management
handle bound open metadata writers. List/Delete share an existing GC handle, with
serialized requests between GC batches. Management of another historical file
can use the reserved handle even when both GC slots are occupied. Startup and
periodic sweeps report per-DiffID errors and continue other files. They skip
retired files without calling Retire or unlinking them. Close stops admission and
scheduling, joins admitted operations/current batches, and closes historical
handles. The caller then closes active disks and finally the directory owner.

Reclamation does not choose retention. Deleted Checkpoint descriptors, descriptor
group pages and fixed management areas remain, as does the active root in a
historical DIFF. Files need not return to their initial allocated size or length;
used Checkpoint IDs remain unavailable for reuse. Independent Retire still owns
whole-file withdrawal/deletion. Runtime and offline usage belongs to
[sandbox lifecycle and CLI](#checkpoints).

<a id="migration-and-validation"></a>

## Migration and validation

| Previous entry | Current entry |
| --- | --- |
| `vhost.OpenBlockCOW`, `vhost.BlockCOW` | `diffstore.OpenDisk`, `diffstore.Disk` |
| `vhost.BlockCOWOption` | `diffstore.DiskOption` |
| `vhost.NewCOWCache`, `WithCOWCache`, `COWCache` | `diffstore.NewCache`, `WithCache`, `Cache` |
| `vhost.COWCacheStats` | `diffstore.CacheStats` |
| `vhost.NewStreamReader` / `StreamReader` | `diffstore.NewStreamReader` / `StreamReader` |
| `internal/diffstore` administrative APIs | `pkg/diffstore` administrative APIs |
| Storage tests formerly in `pkg/vhost` | `pkg/diffstore` |
| B+ engine tests formerly in `internal/diffstore` | `pkg/diffstore/internal/versioned` |

The earlier package relocation in this table changed Go source imports without
changing CLI/configuration or disk bytes. The presence-enabled v3/v4 formats are a
separate, incompatible working-file change: export retained raw/v1/v2 work-file
content with the old version before upgrading. Provisioning templates remain
immutable inputs with their existing raw/encrypted input contract; their readers
do not reopen or upgrade the source as an active working file. Existing raw ext4
preparation and old portable tarstream import remain supported.

All repository callers use the relocated API. `vhost.BlockReader` remains a type alias
for the source accepted by read-only transport devices. Storage benchmark flags
and `VHOST_*` environment names remain compatible; their compiled test package is
now `./pkg/diffstore`. The existing ENOSPC script name remains a CI entry point.

Run `go test ./...`, `go test -race ./pkg/diffstore/... ./pkg/vhost ./pkg/sandbox
./pkg/restore ./pkg/artifact ./pkg/snapshot`, `go vet ./...`, `make test-e2e-scripts`,
`bash scripts/test-vhost-tmpfs-runner.sh`, and the real
`scripts/test-vhost-tmpfs-enospc.sh`. Direct-I/O tests need a supported filesystem
for `TMPDIR`; userfaultfd transport tests and isolated ENOSPC need their documented
privileges. A skip or prerequisite failure is not passing coverage.

Contract tests cover both formats with plaintext/XTS, partial writes over a base,
shared-cache ownership, cancellation, active sparse exports, named-view pins and
unsupported checkpoint capability. Existing format, corruption, recovery,
short-I/O, failure cleanup, concurrency and transport tests remain at their owning
layer. Performance methods and interpretation remain in [sandbox validation](runtime.md).
<a id="payload-boundary"></a>

## Payload boundary

The vhost backend receives the `fetch.Stream` Payload section, not FullStream. Including a `.sandbox` ZIP tail in the block device's logical size is a bug covered by format/unit tests.

<a id="layered-reads"></a>

## Layered reads

Read order is active diff → captured top → `base_from_refs` → root image where applicable. Hole falls through; Zero/Data stop traversal. The immutable layers composed within one writable block device must have matching logical sizes. In the EROFS-plus-ext4 topology, the read-only EROFS base is a separate vhost device; it is not the final fall-through layer of the writable ext4 Disk device. See [disks.go](../pkg/sandbox/disks.go) and [serveandwait.go](../pkg/sandbox/serveandwait.go).

`StreamReader` retries a complete read synchronously. Runtime queue contexts replace preparation contexts for that call, so stopping an old master session cancels its pending reads before its workers are joined and its memory table is replaced. The COW adapter passes that context into base materialization while retaining block locks, the dirty bitmap and partial-write algorithm. It never retries an entire `WriteAt`, `FLUSH`, Store `Put` or snapshot operation.

Both `processChain` and `processQueue` recognize stopped/terminal required reads. They leave the status byte, used ring and queue base uncompleted. This also applies to the implicit base read inside a COW write. Ordinary unsupported requests and independent writable-diff errors keep their existing protocol behavior.

The shared [source read recovery policy](runtime.md#read-recovery) retains the operation context and first terminal cause.

<a id="snapshot-view"></a>

## SnapshotView

After CH pause and frontend quiesce, `SnapshotView` copies the logical bitmap and
serves upper-only plaintext, reading dirty/writeback cache pages before the file.
It does not force Drain/fsync or copy the whole cache. Background writeback may
continue moving identical logical contents. The last cached copy becomes
evictable only after successful file completion. Keep frontend requests quiesced
and the COW open until capture finishes. Background fatal errors during capture
must fail publication and reach the runtime owner, including when no guest request
follows the failure. Export does not rotate the active diff or change its base.
Accepted v4 cache Data/Zero overrides older backend payload presence for both
bytes and sparse-run classification. Ordinary all-zero Data is still classified
as Data. Fresh disks await the existing `WaitMaterialized` publication boundary,
including its initial drain; creating an active view after publication adds no
Drain.

<a id="disk-state"></a>

## Disk state and I/O

The public storage API is in [`pkg/diffstore`](../pkg/diffstore); see the
[package architecture and migration guide](block-storage.md). `pkg/vhost` owns transport only.

Active writable diffs request O_DIRECT through one common positioned-I/O API,
including on tmpfs. Runtime policy does not identify the backing filesystem.
One bounded plaintext page cache is owned by `sandbox-ctl` for each sandbox; root and data disks share it, including mixed tmpfs/disk storage. This
implements [issue #230](https://github.com/kuasar-sandbox/sandboxer/issues/230), with
tmpfs compatibility restored by [issue #238](https://github.com/kuasar-sandbox/sandboxer/issues/238).
It changes active data access, configuration and lifecycle; immutable base images,
templates, historical overlays, manifest decryption caches, artifact outputs,
tarstream representation, cgroups, balloon and guest resource budgets retain
their existing behavior.

<a id="logical-state"></a>

### Logical state and accounting

The stack is `Disk -> plaintext cache -> diffBackend -> single-layer or versioned storage`. Pages are 4096 bytes. Base reads do not populate or
materialize the upper cache. Each cached page is indexed by active file and block number.

| State or transition | Total used | Dirty used |
| --- | --- | --- |
| Clean, dirty, writeback, read loading | One page each | Dirty/writeback only |
| New write construction/reservation | One page | One page |
| v4 cached Zero admission | One page | One page |
| Clean becomes dirty | Unchanged | Add one page |
| Dirty overwrite or dirty becomes writeback | Unchanged | Unchanged |
| Successful Data writeback becomes clean | Unchanged | Remove one page |
| Successful explicit Zero writeback | Remove one page | Remove one page |
| Evict clean | Remove one page | Unchanged |

At all times `dirty_used <= used <= cache_size` and
`dirty_used <= max_dirty_size`. Submitting I/O does not release reservations.
The metadata pool, Data-buffer pool, index, clean LRU and dirty FIFO are bounded
by capacity. Explicit Zero stores metadata with no payload, but is charged as a
full logical page. Data owns a separate 4 KiB buffer, cleared before pooling.
The strong Data pool retains its previous high-water allocation until cache
close; Data-to-Zero does not promise heap/RSS reclamation. A cold Zero-only
workload avoids those payload allocations, but Zero-to-Data needs a buffer and
may allocate if the pool is empty. Reading a committed Zero after its cache entry
was released uses the normal clean-Data read cache and can allocate a buffer too.

Selection and operation arrays are each bounded by 1024 entries. Space-pressure
recovery additionally captures at most `max_dirty_size / 4096` existing Zero-page
pointers, without copying payload. Other bounded overhead includes two independent
1 MiB MAP_SHARED I/O workspaces per active file (each with less than 1 MiB
alignment padding), and page metadata/index/Go allocator overhead proportional
to page capacity.
`cache_size` is not a cap on process RSS, guest memfd mappings, immutable-source
caches, tmpfs file storage or snapshot output memory.

Tmpfs file pages are filesystem storage in memory (and potentially swap), separate
from this bounded process cache. Successful Data writeback releases dirty quota but
does not release stored tmpfs data; clean-cache eviction does not remove it either.
`cache_size` does not bound those bytes or promise zero tmpfs page residency.
Existing tmpfs mount size/inode limits and host memory controls govern that
storage. Sandboxer does not remount tmpfs, change cgroups, inflate cache budgets
or add persistence guarantees. See the [Linux tmpfs documentation](https://www.kernel.org/doc/html/v6.1/filesystems/tmpfs.html).

The bitmap means **logical upper-present**, independent of clean/dirty state.
A complete new page is published to the cache and bitmap before acknowledging
the write. Successful writeback or eviction never clears that bit. Reopening
loads upper logical presence from the committed P bitmap; sparse file extents describe physical payload only using the existing format.
The cache keeps latest plaintext ahead of physical file state.

<a id="cache-and-backpressure"></a>

### Reads, writes and backpressure

Reads copy from clean, dirty or frozen writeback pages first; explicit Zero hits
clear the caller's requested bytes without a cached payload. Only a cache miss
may read/decrypt the active file. Read loading reserves total capacity; if no
clean slot is available, a bounded foreground workspace can service the read
without insertion. It never bypasses a newer cached page.

Live and snapshot reads share bounded upper-run batching. Only contiguous cold
pages of the requested upper range enter a physical read (at most 1 MiB); cache
hits and base/hole boundaries stop the run. Sorted stripe-range locks prevent
concurrent writes/discard, while reserved Loading pages prevent eviction. With
less cache capacity than the run, remaining cold pages bypass caching under the
same stripes. Read/decrypt stays in the owned MAP_SHARED I/O workspace.
Loading pages are filled from that workspace outside the cache mutex, then
published as clean under the mutex before copying out to mutable caller or guest memory.
Contiguous base blocks within the requested bounded range are passed together
to `BlockReader`, which retains its own lower read boundaries. No request-sized
payload allocation or base prefetch is added.

Writes acquire total and dirty capacity together before constructing a new page.
Clean promotion needs only dirty quota; rewriting an unselected dirty page needs
no extra quota or queue node. Full-page overwrites need no old-data read. First
partial writes build the full page from base (or zeros), preserving the existing
short-base/error behavior. Later partial writes preserve every untouched byte.
The cache copies guest bytes before completion and never retains descriptor
buffers. Requests larger than the cache advance page by page.

When full, evict least-recently-used clean pages; otherwise wait for writeback.
Quota waits are cancellable and recheck state after waking. Waiters hold no page
reservation needed by the worker. One-page capacity and all-pages-in-flight must
make progress. No request creates a background goroutine or an unbounded payload
queue. Block stripes serialize foreground same-block operations; the worker does
not acquire these locks.

<a id="writeback"></a>

### Writeback and FLUSH

One sandbox worker normally anchors each batch on the oldest eligible dirty page.
Hot overwrites do not move a page to the tail. It batches one file and one kind:
up to 256 Data pages (1 MiB) or 1024 Zero pages, without filling holes or
preallocating gaps. A fixed 1 ms
aggregation window allows low-rate traffic to progress; quota pressure and
internal Drain wake it immediately. Selected pages become frozen writeback:
reads remain possible and same-page writes wait. The worker copies frozen Data pages
directly into the active diff's existing aligned MAP_SHARED write workspace,
encrypts that copy, and performs I/O without cache or global locks. Cache page
payloads stay plaintext and never become syscall buffers. Foreground reads have
a separate workspace.

The bounded page index supplies contiguous dirty neighbours from the same file,
regardless of arrival order; unselected FIFO age is unchanged. Ordinary write
notifications neither end nor restart the fixed aggregation deadline. Pressure
and Drain interrupt it. An already aggregated backlog proceeds without a new
1 ms wait per batch, including batches that contain only one page.

Zero batches write metadata without copying or encrypting payload, and successful
Zero entries release both quotas rather than occupying the clean LRU. Pending
Zero still consumes quota, can evict unrelated clean Data and can increase
same-page write waits. `WrittenBytes` counts completed logical Data only, excluding
Zero and replayed prefixes during the bounded [space-pressure recovery](#discard-and-reclamation).
Backend metadata work remains serialized inside the Store, so unrelated cache
misses can still wait. A cached ACK does not promise immediately reusable host
space.

A healthy, valid guest **FLUSH is a no-op**. It neither initiates writeback,
waits for dirty pages nor invokes Drain, fsync or fdatasync. Request ordering and
fatal-state checks remain. Existing wire features are retained for Cloud
Hypervisor snapshot compatibility. New writable devices advertise DISCARD;
read-only devices do not. CONFIG_WCE and WRITE_ZEROES are not added. See the
[Guest discard ABI](guest-abi.md#guest-block-discard).

Write completion promises only acceptance of complete latest bytes into bounded
logical COW state. Neither writes nor FLUSH guarantee survival of a
`sandbox-ctl` crash, host power loss or storage failure. This deliberately differs
from the stable-storage FLUSH requirement in
[Virtio 1.2 §5.2.6.2](https://docs.oasis-open.org/virtio/virtio/v1.2/virtio-v1.2.html).
It is the project's non-durable COW contract, not standard durable FLUSH behavior.
Direct I/O alone is not a durability barrier either.

<a id="active-file-io"></a>

### Active-file I/O contract

Every active body attempts O_DIRECT on its opened descriptor and always uses the
same bounded aligned workspaces. If that F_SETFL request returns EINVAL or
EOPNOTSUPP/ENOTSUP, the descriptor keeps ordinary positioned I/O; the application
buffer and cache are unchanged. This initialization-only capability decision
does not inspect the filesystem and is not a retry after failed data I/O. Fresh targets complete this setup **before seeding**; existing
active validation and runtime/snapshot reads use the same path. Runtime code does
not inspect filesystem types, names, mount policies or filesystem-specific inode
flags. Header and format probing are bounded before concurrent body I/O;
templates and base remain buffered/read-only. Plaintext and encrypted file
formats are now single-layer v3 and versioned v4. Both have a fixed 4 KiB
header plus checksummed P pages. Local encryption off/auto/required policy and
512-byte logical XTS data-unit numbering remain unchanged.

`STATX_DIOALIGN` on the open fd supplies separate address and offset/length
constraints. Positive reported constraints must fit the workspace bound and
4 KiB COW geometry; offset alignment must divide 4096 and fit body boundary and
size. A missing mask, query-unavailable ENOSYS/EINVAL/EOPNOTSUPP, or both-zero
alignment fields selects conservative 4096-byte alignment. This is an alignment
choice, not proof of cache bypass. One-zero or
other invalid constraints, genuine statx/descriptor errors, other flag-setup errors and
actual I/O failures remain errors. No application-side buffered retry hides EIO,
ENOSPC, alignment errors or short I/O. See [statx(2)](https://man7.org/linux/man-pages/man2/statx.2.html).

The kernel and backing implementation determine how an O_DIRECT request is
fulfilled. Successful flag setup and aligned I/O establish usable operations,
not universal physical disk-cache bypass. Tmpfs file pages remain memory/swap
storage outside `cache_size`. There is no filesystem allowlist, special tmpfs
backend, new configuration mode or fallback after failed data I/O. See
[open(2)](https://man7.org/linux/man-pages/man2/open.2.html).

I/O uses bounded, aligned anonymous `MAP_SHARED` workspaces with lifetime through
syscall completion, avoiding the private-heap/fork hazard. Arbitrary caller slices
and subpage reads are copied through these buffers; large operations are chunked.
Full-page cache writeback avoids read/modify/write. Short writes fail rather than
retrying an unaligned remainder; see [write(2)](https://man7.org/linux/man-pages/man2/write.2.html).

<a id="versioned-format"></a>

## Versioned DIFF v4 storage and recovery

The optional backend lives in [`pkg/diffstore/internal/versioned`](../pkg/diffstore/internal/versioned).
Each logical 4 KiB block maps through a B+ tree to a physical slot. A 4 KiB index
page has a 16-byte header and at most 255 16-byte entries. Leaves map logical
block numbers to data slots; internal entries pair each child's maximum logical
key with its slot. Entry integers are little-endian. Data and index slots are distinct.
The header binds DiffID, immutable BaseID, logical capacity and encryption policy;
the control page names the active root, epoch, current-capture marker, checkpoint
chain and free/garbage lists. The existing cursor field is reserved; pure GC progress is now in memory. Each checkpoint record stores
its ID, root, epoch, predecessor/successor and visible/retained state. Slot
descriptors record birth epoch, kind and chain linkage. The 512-page bounded
metadata cache does not build an all-block in-memory map of every version.
Raw and decoded pages share this budget (about 4 MiB per owner/view). Four shared
anonymous 8 MiB direct-I/O workspaces bound process-wide aligned scratch at
32 MiB, independently of checkpoint count; shared mappings remain safe across
concurrent fork. XTS uses 512-byte logical sector numbers, regardless of the
physical slot selected by COW.

Active reads combine adjacent logical pages only when their physical data pages
are contiguous, with at most 256 pages (1 MiB) per direct read. Allocator descriptor
pages, metadata slots, holes and fragmented mappings split runs. The active owner
keeps its Store lock through lookup, read, decryption and copying, protecting slot
lifetimes against writes and GC. Payloads are consumed directly from the existing
shared workspace, which is cleared before reuse; no per-page payload copy is
allocated on the Go heap. Short reads return only the complete-page prefix and
leave the unfinished suffix untouched. This changes neither the DIFF format nor
writeback/durability semantics. Large cold contiguous reads benefit; single-page
random reads, fragmented data and COW-cache hits have little or no batching
opportunity. XTS still decrypts each logical sector, and full-call locking still
serializes concurrent storage operations.

Physical page numbers are distinct from guest logical block numbers:

| Physical pages | Contents |
| --- | --- |
| 0 | Immutable header: existing DIFF magic, big-endian version 4 prefix, identity/capacity and wrapped key |
| 1 | Mutable control page |
| 2 | Redo commit descriptor |
| 3–5 | Redo home-address index, not B+ index pages |
| 6–1541 | Up to 1,536 existing metadata page images |
| 1600 onward, at most 259 pages | Active P bitmap; exact count follows capacity |
| 2048 onward | Groups of one descriptor page followed by 128 slots |

Creation reserves pages 0–1541 (6,316,032 bytes) and the active P pages, so redo and existing control
pages remain writable when new payload allocation runs out of space. The initial
sparse file length is 8 MiB, not the guest capacity. Metadata pages carry
checksums. An encrypted header wraps a random 64-byte XTS key with AES-GCM,
using the immutable header fields as associated data and the v4-specific
header-key derivation domain.

Snapshot creation records the active root and an independent full P copy, and
advances the epoch without scanning or copying Data. The P copy uses at most 259
ordinary allocator records and the unchanged redo budget, even with fragmented
free slots. Pins protect both root and P. Delete does not collect P records;
bounded GC persists its partial P-copy cursor and sends these exclusive records
directly to global garbage before finishing the deleted version. A slot born after the latest retained epoch is private;
repeated overwrites can update that slot in place without rewriting the index or
emitting metadata redo. Shared data slots and their shared index path use COW.
Before a guest write can dirty the plaintext COW cache, `MarkModified` clears the
current-capture marker once per capture cycle. The same fence applies to direct
backend writes: root-pointer equality alone never proves an unchanged disk.

Physical Data transactions retain a limit of 64 pages; a pure Zero transaction
can contain up to 1024 pages when its metadata fits. Metadata transactions enforce a
fixed redo budget of 1,536 existing metadata page images, addressed by three
redo home-address pages,
before any data/metadata write. New reserved, unreachable data/index slots are
written before publishing redo; only existing metadata pages enter the redo
images. A checksummed commit descriptor authorizes replay, then home metadata
is applied and the descriptor cleared. The transaction cannot split a structural
change merely to fit the budget. Fresh slots are reserved before writes; ENOSPC
can drive bounded foreground GC and retry without overwriting retained data.
Private payload overwrite has no index transaction after its one-time epoch
fence. Guest FLUSH remains a non-durable health check and does not start
writeback, Drain or filesystem sync. Explicit internal Sync is used for initial
publication. Metadata replay after a killed process does not promise atomic
guest payload writes or checkpoint durability across host power failure.

Linux OFD lock ranges separate DIFF writer ownership (byte 0), short metadata
serialization (byte 1), whole-file reader lifetime (byte 2), and each checkpoint
pin (byte `16 + record slot`). The directory writer uses a separate stable
`diffs/.owner.lock`. An export holds its own descriptor and per-version read pin;
it can outlive the original writer and survive owner handoff. Deletion hides a
version before reclamation, while a pin keeps it retained and continues to force
COW. Retiring an entire DIFF first prohibits new readers and unlinks its data
file only after existing reader locks are released; lock inodes remain stable.
`Retire` reports busy while readers remain; a later `OpenMetadata` owner can retry
removal. A fixed export interprets committed but unapplied redo without taking
writer ownership or applying recovery. Template publication may rename the data
file while retaining the final stable lock path; exports reopen the owned inode
independently. Deleted checkpoint IDs cannot be reused within the same DIFF.
`Export(ActiveDiff)` is a caller-quiescent active view, while a named export owns
an immutable retained root.

GC processes persisted retirement chains separately from deletion. A deleted
version transfers records only to a retained predecessor whose epoch can still
reference their birth; otherwise they enter the global reclaimable chain.
Bounded `More` batches continue even with zero committed frees. A full valid scan
can report `BlockedByReaders`; periodic retries allow reader release to resume
work without another delete or guest write. Pure scan state stays in memory and
does not write redo. GC sorts safe candidates by physical address, coalesces holes
without crossing management/live pages, and commits an ascending batch to the free
list only after punching succeeds. Metadata-only Open/List/Delete never start GC.
The complete batching, crash and scheduling contract is maintained in
[diffstore](#discard-and-reclamation). `GCRead/GCWritten` overlap total
index/redo bytes; `GCPunchCalls` counts completed hole-punch calls, while `GCFreed`
counts committed reusable slots.

Named capture preserves immutable dependency identities when collecting local
sources. It copies the original carrier when necessary, including an ordinary E
used as a disk lower, instead of converting its payload or changing hash domains.
Already owned carriers are reused. A Bundle checkpoint may reference neighboring
tarstreams; later ordinary publication materializes a portable graph. This keeps
the DIFF BaseID stable across capture and permits temporary restore materialization
files to be removed after the completed capture has collected their dependencies.

<a id="quiesce-and-close"></a>

## Quiesce / Resume

The [OpenDisk readiness table](#open-disk) defines asynchronous initialization,
template independence and final publication. [Ownership and completion](#ownership-and-completion)
defines Close and shared-cache lifetime; [Discard](#discard-and-reclamation)
defines the batched Zero transition. These storage boundaries remain in force
when transport admission is quiesced. [Runtime disk capture](runtime.md#disk-capture)
coordinates the common VM freeze and uncertain multi-disk root commit.

Internal `Drain` waits for accepted Data and Zero writeback without fsync or
waiting for all GC. Retained versions and unfinished GC can still occupy disk
space after mapping removal; named Checkpoint and normal Close also wait for
accepted Zero. Normal Close
stops new frontend work, cancels quota waits, drains accepted healthy writes,
joins I/O, clears plaintext and closes the file. Frontend cancellation does not
cancel healthy writeback. Close is idempotent and drain/close errors propagate
through run/restore. Destruction must also wait for actual in-flight syscalls
before freeing their buffers.

Apart from the pre-write reservation recovery described under
[Discard](#discard-and-reclamation), writeback and short-write failures are sticky:
failed pages remain dirty-accounted,
new writes fail, all waiters wake, and the runtime receives a non-blocking fatal
notification without self-joining. A partial physical write may already have changed
the file. Cleanup of newly materialized blocks cannot punch previously existing
upper pages in the same batch. Frontend request acceptance is not a durable whole-request transaction. The v4 backend retains its [committed-transaction recovery contract](#versioned-format); there is no silent buffered retry.

Fatal errors are latched and the owner notified before cache-layer cleanup I/O. While rollback runs, active-I/O ownership, frozen pages and their quotas remain held so Close cannot release the file or buffers early. Cleanup errors are joined afterward without masking the original failure.

<a id="segmented-vhost-reads"></a>

### Segmented reads over vhost-user-blk

On cold boot each disk offers `VIRTIO_BLK_F_SEG_MAX` with 64 data segments.
The pinned Cloud Hypervisor (CH) uses a 128-entry direct ring. Negotiating the
bit requires room for 64 data descriptors, a header and a status byte. The
configuration stays fixed throughout queue setup. A frontend can negotiate
the older subset with a smaller ring. RO, FLUSH and conditional DISCARD keep
their existing feature bits.

A read pins the disk backend for the whole request. Vhost reads successive
logical disk ranges of up to 1 MiB through the existing cache, alignment and
encryption paths. A single descriptor or a run of descriptors covering adjacent bytes in one
host mapping can receive a window directly. When the first descriptor already
provides at least 256 KiB and at least another 256 KiB remain within the
candidate window, vhost can read that descriptor directly. This avoids a
copy and clear on large cache hits while keeping small tails together.
A discontiguous window uses a private queue buffer, copies the
valid prefix in descriptor order and clears the window before reuse. Guest
addresses can decrease or have gaps. A request can span several windows, and a
window can end inside a descriptor. For example, 64 discontiguous 4 KiB
descriptors produce one 256 KiB backend call. A 512 B descriptor followed by
a discontiguous 1 MiB descriptor produces 1 MiB and 512 B calls. Four
discontiguous 256 KiB descriptors can use four direct calls. The last writable
byte belongs to status.
Each queue that first needs aggregation retains 1 MiB outside
`diff_cow.cache_size`. Count each disk separately. Copying and clearing add
CPU and memory traffic on fragmented reads, including cache hits. Cache and
dirty limits keep their existing defaults. Increasing either budget does not
change how the Guest forms requests on a cache miss.

Full reads, including a full count with ordinary EOF/EAGAIN, complete normally.
A short read now delivers the valid prefix and reports IOERR. The old loop
could report success after a short count. Another ordinary error also reports
IOERR, even at full count. The used length includes only delivered bytes and
one status byte. Vhost never retries a failed window. A terminal read or
observed cancellation leaves the request pending without status, used-ring
publication or base advancement. A synchronous pread may still take time to
return, and bytes already in guest memory can remain there. Guest FLUSH keeps
its existing semantics. Foreground write completion and Guest FLUSH provide
no additional host Drain or fsync guarantee.

CH saves the negotiated feature set and device configuration in memory
snapshots. Restoring an older snapshot preserves its original segment limit.
A new cold boot can negotiate 64 and preserve that limit across restore.
An older backend rejects a saved SEG_MAX bit that it cannot implement.

<a id="cow-validation"></a>

## COW validation

Use deterministic worker gates and injected failures for quota transitions,
FIFO/LRU, one-page capacity, cancellation, multi-disk contention, frozen-page
reads/rewrites, FLUSH, snapshots, Discard and Close. Real direct-I/O checks cover
alignment, sparse boundaries, templates, plaintext/encrypted reopen and mincore
page residency on a disk-backed fixture. Generic alignment tests cover positive,
missing, both-zero and malformed constraints, unavailable queries and genuine
errors. Dedicated real-tmpfs tests verify the fixture descriptor type and the
same O_DIRECT request, plaintext/encrypted I/O, bounded staging and copyout,
template seeding,
sparse holes, shared mixed-filesystem quotas/backpressure, dirty/writeback capture,
Drain/Close and reopen. `scripts/test-vhost-tmpfs-enospc.sh` explicitly runs the
plain and encrypted real-ENOSPC cases as root in a new mount namespace. The
30-second test deadline is retained, with an outer bounded timeout for abnormal
hangs. The runner requires both subcases to actually pass; failure, omission or
Skip cannot satisfy required CI. The test
uses a 32 KiB private tmpfs and never exhausts or remounts a shared mount; ordinary
`go test` runs skip only this capability-requiring case rather than attempting
implicit privilege escalation.
DIO/mincore benchmarks retain disk-backed fixtures. Tmpfs functional tests are
not evidence of disk cache bypass. Run targeted tests, race, vet, build, broader
tests and real CH/KVM tmpfs guest cold-start/read-write
and pause/export/restore; report skips and infrastructure failures explicitly.

Performance comparisons need datasets much larger than the cache and separate
frontend admission latency from total elapsed time including Drain. For DISCARD,
also distinguish cache acceptance, backend mapping removal and host reclamation;
compare no-discard, synchronous discard and cached discard as separate baselines.
For Zero payload representation, measure cold Zero-only and Data-warmed caches,
Zero-to-Data allocations and latency, and committed-Zero readback separately.
Pool reuse and allocator/RSS behavior must not be reported as additional disk or
export savings. Cover
sequential/random 4 KiB, 512-byte updates, hot overwrites, encryption and multiple
disks. Report tail latency, logical/physical write volume, stable quota usage and
file-cache/dirty/writeback or mincore evidence. Compare buffered baseline and
candidate on the same storage. Do not manufacture low cache residency with
fsync/DONTNEED/drop_caches; exclude unchanged template input, artifact output and
shared guest mappings from active-diff memory attribution. Measurements are
observations, not unmeasured throughput targets or claims that skipped E2E passed.

<a id="cow-measurement"></a>

### Repeatable COW measurement method

The historical runs remain in their original Git commits and [COW work](https://github.com/kuasar-sandbox/sandboxer/issues/230); measured revision tables are not a maintained specification. Repeat a comparison with recorded revisions, kernel/filesystem/storage, cache configuration, encryption, workload and prerequisites. A documented method is not evidence that it ran on the current revision.

Use the existing `blk_cow_workingset_bench_test.go` working-set driver and a matching adapter for each revision. A buffered baseline without a sandbox cache uses no-op cache drain/close and no cache statistics; do not imply that its request completion is media durability. Keep candidate admission and final Drain separate:

```bash
go test -tags no_rocksdb ./pkg/diffstore -run '^$' \
  -bench '^BenchmarkCOWWorkingSet$' -benchtime=1x -count=1 -timeout 15m
```

Use a 256 MiB logical dataset, eight times a 32 MiB cache: sequential 1 MiB writes; deterministic random 4 KiB and 512-byte updates across 65,536 pages; hot overwrites with 90% in 4 MiB and 10% across the remainder; and prefilled reads. The 512-byte workload transfers 32 MiB while materializing 256 MiB of pages. Prefill hot/read workloads outside timing. Alternate 1 MiB requests between two 128 MiB disks to check a shared sandbox budget. Cover plaintext and XTS. This is a single-foreground backend driver, not a guest measurement.

Exclude fixture/setup work; measure request percentiles including quota waits, logical throughput and total elapsed including Drain. Buffered baseline completion means kernel acceptance; buffered prefilled reads can be warm in the kernel cache. No fsync, DONTNEED or drop_caches may manufacture a cache-residency result. Count active-file body read/write calls and bytes; `/proc/self/io` describes filesystem accounting, not durable media writes or an automatic write-amplification claim. Use mincore without touching mapped pages for active-file residency, and report logical cache/dirty peaks separately from heap/RSS.

The existing `cow_direct_workingset_bench_test.go` provides an uncached direct-I/O reference with the same active-I/O and XTS rules, not a selectable production mode:

```bash
go test -tags no_rocksdb ./pkg/diffstore -run '^$' \
  -bench '^BenchmarkCOWDirectWorkingSet$' -benchtime=1x -count=1 -timeout 15m
```

Direct-I/O prefill distinguishes cached reads from warm kernel-file-cache effects. Hot-path checks cover nil/background/cancelable contexts and signaled waiters. Deterministic tests verify fixed batching deadlines, pressure/Drain bypass, no per-batch delay with backlog, oldest-dirty progress, bounded adjacent reads with small caches, mixed cache/base/hole reads, overlap/cancellation, sub-page and short-read boundaries, and copyout isolation from guest-mutated output buffers.

For actual guest validation, use the existing owner E2E cases for diff templates, encrypted diffs, snapshot/restore and data disks, with `REQUIRE_KVM`, private `TMPDIR` and matching `BIN`/native/runtime inputs. Record each unavailable prerequisite as missing validation. A guest probe can write a 64 MiB span in 1 MiB requests, then warm and overwrite the first 4 MiB with 4,096 aligned 4 KiB operations using mmap-backed buffers and O_DIRECT. Verify byte counts and payloads. Guest descriptor segmentation does not measure 1 MiB host batches, and admission-only timing without Drain/fsync is not full writeback timing. Keep raw run results in the originating PR/Issue or immutable Git history.

Fresh checkpoint initialization has an opt-in phase probe in the existing
`checkpoint_template_perf_test.go`:

```bash
CHECKPOINT_INIT_PERF=1 CHECKPOINT_INIT_DISKS=4 CHECKPOINT_INIT_TEMPLATE=1 \
CHECKPOINT_INIT_ENCRYPTED=1 CHECKPOINT_INIT_ACTION=drain \
CHECKPOINT_INIT_LABEL=candidate CHECKPOINT_INIT_ROUND=1 \
go test ./pkg/diffstore -run '^TestCheckpointInitializationPhases$' -count=1 -v
```

Use 1 and 4 disks, template/encryption values 0 and 1, and actions `drain`,
`checkpoint` and `close`; repeat in alternating control/candidate order.
The 32 GiB capacity is sparse, with only 64 KiB of template data when selected.
Setup is excluded; results contain logical open, first useful I/O, completion
observations, immediate boundary waits, CPU, allocations, RSS, physical extent
accounting and process I/O. Completion observations include observer scheduling
delay. Compare the same current synchronous batched initializer/bitmap baseline,
not the historical serial bitmap writer. Keep all tails and failures. Also
measure actual Guest startup through first health/tool response under single and
concurrent launches; backend open timing alone cannot establish startup benefit.

<a id="segmented-read-evidence"></a>

### Guest measurements for segmented vhost reads (2026-11-03)

A is main at `185f50b6`. Its `pkg/vhost` matches investigation commit `3fa03fc7`. These runs used main's effective cache configuration and local encryption policy. H is the final implementation. Each host and storage mode had two independent cold boots of each build. Guests used two vCPUs, 2 GiB, CH v51.1.0, Linux 6.1.169 and fio 3.39 with libaio, direct I/O and one job. The data disk used ext4 on a v3 DIFF with a 32 MiB cache and a 16 MiB dirty limit. Every boot checked the actual DIFF headers, CRCs and O_DIRECT file descriptors. Fragmented jobs verified descending PFNs and zero forward-adjacent pairs.

Cells give **the two A runs → the two H runs** in MiB/s. 1M and 4K denote application I/O sizes in binary units. Q denotes application queue depth. Each tabulated job transferred 512 MiB except the 8 MiB warm-up. Hot reads repeatedly accessed that warmed 8 MiB set. The independent CRC32C pass wrote and verified a full 512 MiB. Its write phase is listed separately from the formal writes. Matched supplemental boots supply natural-layout formal writes missing from early baseline runs. All tabulated throughput was measured without tracing.

| Job | WSL x86 / plain | WSL x86 / encrypted | Native x86 / plain |
|:---|:---|:---|:---|
| Prepare, 1M Q32 | 151.03/178.83 → 442.14/316.44 | 85.83/73.21 → 104.38/73.52 | 157.64/157.83 → 165.27/170.89 |
| Precondition, 1M Q32 | 413.90/403.79 → 570.79/481.66 | 121.79/97.23 → 123.82/113.83 | 195.20/186.18 → 158.32/171.41 |
| Natural read, 1M Q1 | 48.54/22.70 → 269.47/368.88 | 12.10/19.78 → 106.82/105.68 | 149.84/154.22 → 91.44/182.40 |
| Natural read, 1M Q32 | 513.54/248.54 → 583.14/459.61 | 118.38/111.96 → 93.93/117.89 | 159.01/182.40 → 194.23/153.20 |
| Fragmented read, 1M Q1 | 21.24/12.88 → 225.15/163.58 | 12.97/13.60 → 73.94/81.01 | 16.15/12.51 → 144.31/174.45 |
| Fragmented read, 1M Q32 | 12.52/14.14 → 244.04/190.41 | 12.74/11.75 → 76.44/64.82 | 16.12/12.81 → 161.36/173.50 |
| 4K read, Q1 | 6.13/5.39 → 6.28/6.59 | 5.84/5.75 → 5.07/6.23 | 14.81/11.99 → 12.01/12.37 |
| 4K read, Q32 | 14.44/13.02 → 10.97/12.34 | 12.96/11.08 → 9.53/10.98 | 16.27/13.06 → 13.29/13.28 |
| 2M application read, Q1 | 32.64/89.25 → 268.34/208.47 | 63.17/31.50 → 104.47/101.71 | 178.58/169.42 → 161.41/159.60 |
| 8M warm-up, Q1 | 37.74/88.89 → 156.86/170.21 | 14.36/30.19 → 88.89/85.11 | 163.27/222.22 → 285.71/421.05 |
| 8M hot set, 1M Q1 | 1387.53/224.66 → 521.92/881.24 | 281.32/251.35 → 1233.73/567.63 | 5019.61/2415.09 → 3261.15/3240.51 |
| Natural write, 1M Q1 | 515.09/170.89 → 372.63/405.71 | 111.21/109.35 → 142.38/96.66 | 160.85/183.12 → 186.66/185.84 |
| Natural write, 1M Q32 | 505.93/172.74 → 350.93/405.71 | 129.33/108.89 → 147.93/107.88 | 186.86/164.00 → 159.35/174.15 |
| Fragmented write, 1M Q1 | 146.49/154.68 → 268.20/263.24 | 110.42/105.76 → 117.38/102.24 | 184.57/182.27 → 181.24/165.59 |
| Fragmented write, 1M Q32 | 151.75/171.99 → 308.99/295.95 | 112.21/108.31 → 134.99/70.17 | 176.01/180.54 → 174.74/171.41 |
| CRC32C pass (write phase) | 143.74/136.90 → 276.61/230.53 | 104.00/104.85 → 129.33/80.58 | 179.52/159.50 → 185.31/188.79 |

| Job | ARM / plain | ARM / encrypted |
|:---|:---|:---|
| Prepare, 1M Q32 | 162.75/194.23 → 206.20/206.37 | 153.62/152.25 → 156.05/152.15 |
| Precondition, 1M Q32 | 174.92/150.94 → 146.75/147.00 | 148.49/132.06 → 149.93/159.90 |
| Natural read, 1M Q1 | 108.43/27.57 → 185.31/183.12 | 119.51/79.04 → 143.62/154.31 |
| Natural read, 1M Q32 | 161.77/197.23 → 149.27/149.01 | 161.11/149.80 → 150.81/149.75 |
| Fragmented read, 1M Q1 | 19.62/19.70 → 179.59/177.35 | 19.55/19.64 → 143.10/141.91 |
| Fragmented read, 1M Q32 | 19.73/19.74 → 165.00/162.85 | 19.56/19.68 → 144.35/141.79 |
| 4K read, Q1 | 19.63/19.55 → 19.58/19.48 | 19.61/19.60 → 18.22/19.07 |
| 4K read, Q32 | 19.74/19.74 → 19.77/19.74 | 19.67/19.55 → 19.55/19.70 |
| 2M application read, Q1 | 29.09/155.72 → 201.89/203.66 | 146.79/43.31 → 166.18/146.96 |
| 8M warm-up, Q1 | 26.67/148.15 → 500.00/470.59 | 89.89/133.33 → 160.00/190.48 |
| 8M hot set, 1M Q1 | 350.93/2473.43 → 3555.56/1450.42 | 2072.87/1896.30 → 3657.14/3605.63 |
| Natural write, 1M Q1 | 185.78/177.65 → 205.46/164.10 | 163.47/162.39 → 164.47/153.80 |
| Natural write, 1M Q32 | 148.88/152.29 → 188.03/196.39 | 153.57/153.71 → 153.34/164.95 |
| Fragmented write, 1M Q1 | 158.61/159.06 → 177.59/154.17 | 153.66/164.31 → 153.34/164.21 |
| Fragmented write, 1M Q32 | 158.22/159.20 → 161.26/213.33 | 153.98/165.75 → 153.85/164.16 |
| CRC32C pass (write phase) | 183.97/185.10 → 188.37/149.27 | 153.16/163.16 → 164.47/153.20 |

Mean total QD1 latency in µs, calculated from fio slat + clat. Each entry is the median across the two boots.

| Host and DIFF | Natural 1M/Q1 | Fragmented 1M/Q1 | 4K/Q1 |
|:---|---:|---:|---:|
| wsl / off | 32318.4 → 3202.1 | 62361.7 → 5270.2 | 675.7 → 604.1 |
| wsl / auto | 66584.6 → 9403.6 | 75306.0 → 12926.6 | 668.1 → 695.0 |
| native / off | 6577.2 → 8208.0 | 70934.5 → 6329.0 | 294.2 → 319.8 |
| arm / off | 22743.3 → 5427.3 | 50866.4 → 5601.4 | 199.0 → 199.6 |
| arm / auto | 10507.8 → 6720.1 | 51023.0 → 7015.9 | 198.8 → 209.2 |

Two additional builds on native x86 isolate the changes. B enables only multi-segment negotiation. C uses only the final read path. Each diagnostic issues 64 application reads of 1 MiB. Uprobes and strace were matched by the thread reading the data DIFF. The probe interval also includes a few auxiliary calls. File preads count complete returns after matching unfinished/resumed records by thread.

| Build | Untraced 512 MiB reads, MiB/s | Chains / ReadAt calls in 64 MiB diagnostic | data.diff preads |
|:---|---:|---:|:---|
| A | 16.15 / 12.51 | 16,387 / 16,388 | 16,384 × 4 KiB |
| B | 16.40 / 13.92 | 256 / 16,397 | 16,384 × 4 KiB |
| C | 13.19 / 13.17 | 16,390 / 16,395 | 16,384 × 4 KiB |
| H | 144.31 / 174.45 | 256 / 257 | 256 × 256 KiB |

A separate PFN-controlled hot test used four discontiguous 256 KiB physical spans per 1 MiB read. Each boot alternated contiguous and four-span layouts after warming an 8 MiB set. Every repetition transferred 512 MiB. The two within-boot medians for four spans were 5100/4837 MiB/s on A, 3429/3414 when all discontiguous spans used scratch, and 5438/5708 on H. Full 512 MiB scans with the same layout gave 150/148, 161/163 and 161/160 MiB/s respectively. These measurements motivated keeping large direct spans. Natural hot throughput still varies with physical layout.

A resident-memory backend on WSL isolates descriptor processing, copying and clearing. Six alternating pairs on one CPU yielded the following median µs/request on A → H. Single 4 KiB was 0.68 → 0.82, single 1 MiB was 86.71 → 80.82, reversed 64×4 KiB was 26.60 → 53.15, and reversed 4×256 KiB was 74.82 → 69.48. Small discontiguous segments still incur measurable memory traffic when storage latency is absent.

Encrypted ARM 4 KiB/QD1 measured 19.61 → 18.65 MiB/s in the primary workload sequence. To isolate single-segment reads, both builds reopened the same prepared encrypted data DIFF. Each cold boot ran a full 4 KiB/QD32 preconditioning scan before two QD1 scans and one QD32 scan, each of 512 MiB. All boots verified the same file SHA-256. The matched-data runs follow. The primary table retains the original difference.

| Independent boot | Two 4 KiB/QD1 reads, MiB/s | 4 KiB/QD32, MiB/s |
|:---|---:|---:|
| H1 | 19.61 / 19.54 | 19.69 |
| A1 | 19.61 / 19.59 | 19.55 |
| A2 | 19.63 / 19.58 | 19.55 |
| H2 | 19.59 / 19.59 | 19.68 |

An additional plaintext replay used the same data DIFF across four independent boots in A/H/H/A order and verified the same file SHA-256 each time. Values are MiB/s. Natural 1 MiB/QD32 and 2 MiB/QD1 are within-boot medians of two complete scans. WSL 4 KiB/QD32 transferred one full dataset per boot.

| Matched plain DIFF / job | Two A boots | Two H boots |
|:---|---:|---:|
| wsl / 4 KiB Q32 | 17.12/13.31 | 11.12/13.00 |
| wsl / 1 MiB Q32 | 297.93/303.68 | 490.47/369.48 |
| wsl / 2 MiB Q1 | 34.93/110.51 | 310.60/234.97 |
| native / 1 MiB Q32 | 172.47/171.33 | 155.51/184.17 |
| native / 2 MiB Q1 | 38.48/141.68 | 183.19/152.21 |
| arm / 1 MiB Q32 | 158.94/154.30 | 149.23/147.20 |
| arm / 2 MiB Q1 | 54.70/31.16 | 181.92/187.63 |

Reusing a DIFF still permits different natural Guest page layouts. Plaintext ARM QD32 remained lower in that sequence, with a smaller gap. In a separate sequence of three consecutive natural 1 MiB/QD32 scans of the same disk, the two within-boot medians were A 153.16/152.61 and H 151.48/148.71 MiB/s. Diagnostic builds using a 128 KiB direct-span threshold gave 157.06/147.55. Ending a small gathered prefix before a long span gave 147.42/151.93. Scan order and request shape affected these samples. They did not establish a consistently faster replacement rule for natural layouts. A separate native x86 write test measured fragmented 1 MiB/QD1 writes at A 156.81/180.85 and H 185.71/156.15 MiB/s.

In WSL the primary 4 KiB/QD32 medians were about 15% lower in both storage modes. A subsequent four-build comparison on a common plaintext DIFF gave A 17.46/10.42, B 13.78/10.81, C 11.80/13.30 and H 13.81/10.61 MiB/s across two independent boots each. These single-segment reads do not use scratch. The lower WSL observations remain a performance boundary for this nested environment.

The 1280 KiB raw-device preadv probe verifies every page by separate reads and requires the device to complete one read of 2560 sectors. Both final x86 and ARM runs produced a single request. An additional native x86 diagnostic placed one 4 KiB page before a long physical span and recorded 1 × 1024 KiB, 1 × 256 KiB data DIFF calls. The 2 MiB application workload alone cannot certify a multi-window vhost request. In actual WSL snapshots the writable disks saved masks `0x140002200` and `0x140002204`, seg_max 0 and 64, and ring size 128. Old-to-new and new-to-new restores verified guest segment limits and file content. Downgrade rejected unsupported bit `0x4`. A separate WSL guest completed three FLUSH and 32 DISCARD commands successfully and verified a retained file.

fio counts application operations. Guest disk stat counts Linux block requests. blk2 rd/s and logical_backend count whole vhost requests. ReadAt dispatch and backing pread were measured separately. Across the 84 primary natural, fragmented, 4 KiB and 2 MiB read scans on the two native hosts, process storage-read bytes matched fio bytes in every scan. The WSL kernel lacked that counter for its 56 scans. Equality with fio bytes cannot establish an empty physical-media cache. Two independent boots per cell show sample variation. They cannot estimate how often production buffers fragment. These throughput results cover the listed CH, kernel, v3 DIFF and cache budget. ARM snapshot restore and v4 DIFF guest performance were not separately measured.

Build the probes for the Guest ISA and libc. Supply root.ref, the ext4 root.template and data.template, and fio-runtime.tar containing the private run-fio loader. Use a fresh output directory per boot and run two independent boots for both off and auto.

```bash
cc -O2 -Wall -Wextra -Werror -fPIC -shared test/perf/fio_reverse_pfn.c -o /tmp/libgeometry.so
cc -O2 -static test/perf/vhost_large_read.c -o /tmp/large-read
sudo python3 test/perf/vhost_guest_fio.py --inputs "$FIXTURES" --output "$NEW_RUN" \
  --ctl "$CTL" --ch "$CH" --kernel "$KERNEL" --runtime "$RUNTIME" \
  --crypto auto --pfn-lib /tmp/libgeometry.so --large-probe /tmp/large-read --trace
```

The investigation prototype reported plaintext A/B/C/D medians of 16.82/15.68/13.18/168.62 MiB/s and encrypted A/D medians of 15.46/102.68 on x86. Its two repetitions shared a Guest. The tables above use fresh measurements of the production code. [#398](https://github.com/kuasar-sandbox/sandboxer/issues/398)

<a id="checkpoint-validation"></a>

## Checkpoint validation and measurement

`snapshot.checkpoint.sh` exercises Guest fstrim over a nonzero immutable base in
v3/v4 and plaintext/XTS configurations, verifies Zero after portable export and
cold restore, and crosses a local memory Checkpoint restore for v4. Unit tests
add controlled template Data/Zero interleavings before, during and after import,
as well as cached writeback, cancellation and failed first-payload restoration.
Cached-Zero regressions cover reads/export before writeback, frozen Data ordering,
finite space-pressure recovery, completed-Zero quota release and payload-pool
reuse without cross-page or cross-disk plaintext exposure.

Run source checks separately from prebuilt-product E2E. Use a native Linux ext4
scratch directory for direct I/O and space-reclamation checks; a Windows-mounted
path, tmpfs-only run or skipped privileged test does not establish those results.

```bash
# In the prepared Go source environment, with TMPDIR on the filesystem under test:
CGO_ENABLED=0 go test -count=1 ./pkg/diffstore/... ./pkg/vhost ./pkg/artifact \
  ./pkg/config ./pkg/sandboxfile ./pkg/snapshot ./pkg/restore ./pkg/sandbox ./pkg/ctl ./cmd/sandbox-ctl
CGO_ENABLED=1 go test -race -count=1 ./pkg/diffstore/... ./pkg/vhost ./pkg/artifact ./pkg/restore
# Required full source gate in Workbench system mode with mount/namespace capability:
bash scripts/ci-source-checks.sh

# Dedicated, disposable 64 MiB ext4 filesystem; never a shared developer mount:
DIFFSTORE_FULL_FS=/path/to/owned/finite-ext4 go test ./pkg/diffstore/internal/versioned \
  -run '^TestRealFilesystem(FullRecovery|CacheSpaceRecovery)$' -count=1 -v

# Root shell; PREPARED contains exact built artifacts and the platform E2E runner:
python3 "$PREPARED/test/e2e/e2e" run --workdir "$PREPARED" \
  --suite snapshot --include snapshot.checkpoint.sh

# Actual payload histories; default sizes are 0,1,10 GiB:
go test -c -o /path/to/compiled-vhost.test ./pkg/diffstore
go run ./test/perf/diffstore --directory /path/to/new-owned-directory \
  --sizes-gib 0,1,10 --rounds 5 \
  --vhost-test-binary /path/to/compiled-vhost.test

python3 test/perf/checkpoint_cow.py --tmpdir /path/to/native-ext4/scratch \
  --output /path/to/results --rounds 3 --operations 32768 \
  --modes 'raw|v1|v2-no-version|v2-shared|v2-private|v2-xts-no-version|v2-xts-shared|v2-xts-private'

python3 test/perf/checkpoint_template.py --tmpdir /path/to/native-ext4/scratch \
  --output /path/to/template-results

# Compare the same baseline/candidate environment, payload and retained versions.
# Three independent rounds, each with three write/checkpoint/delete/GC/reuse cycles:
python3 test/perf/checkpoint_cow.py --tmpdir /path/to/native-ext4/scratch \
  --output /path/to/reclamation-results --rounds 3 --gc-cycles 3 \
  --modes 'v2-no-version|v2-xts-no-version' \
  --workloads 'sequential|gc-write|reclamation|background-gc|discard'

# Reuse the same fixture for baseline/candidate binaries; alternate >=3 rounds.
# Cases: dense-plain/dense-xts (1 GiB payload), sparse-plain/sparse-xts
# (30 GiB logical capacity with only 240 mapped pages).
CHECKPOINT_PRESENCE_DIR=/path/to/owned/native-ext4/fixtures \
CHECKPOINT_PRESENCE_CASE=dense-plain CHECKPOINT_PRESENCE_PHASE=seed \
  go test ./pkg/diffstore -run '^TestCheckpointPresencePhases$' -count=1 -v
CHECKPOINT_PRESENCE_DIR=/path/to/owned/native-ext4/fixtures \
CHECKPOINT_PRESENCE_CASE=dense-plain CHECKPOINT_PRESENCE_PHASE=measure \
CHECKPOINT_PRESENCE_LABEL=candidate \
  go test ./pkg/diffstore -run '^TestCheckpointPresencePhases$' -count=1 -v

# Active dense reads: same fixture for both binaries, >=3 alternating rounds.
# CASE: plain-shared, plain-private, xts-shared, xts-private.
CHECKPOINT_READ_DIR=/path/to/owned/native-ext4/dense-fixtures \
CHECKPOINT_READ_CASE=plain-shared CHECKPOINT_READ_PHASE=seed \
  go test ./pkg/diffstore -run '^TestCheckpointDenseReadPhases$' -count=1 -v
CHECKPOINT_READ_DIR=/path/to/owned/native-ext4/dense-fixtures \
CHECKPOINT_READ_CASE=plain-shared CHECKPOINT_READ_PHASE=measure \
CHECKPOINT_READ_LABEL=candidate \
  go test ./pkg/diffstore -run '^TestCheckpointDenseReadPhases$' -count=1 -v
```

The source suite checks independent-process pins and writer handoff, killed-owner
redo reopening, real short writes, failed reservations, split/merge boundaries,
retirement/GC restart, zero masking, encryption and metadata-only access. Artifact
tests cover exact publication deletion, Bundle/external sources, rewriting,
source-directory identity and same-directory versus materialized restore. The
product case exercises root plus two data disks through cold boot, named capture,
resume, concurrent fixed-version publication, historical/cross-directory restore
and restoration after removing the original sources. It also queries the kernel
OFD version-pin range after handoff, then exercises hot/cold restore, explicit
Delete, continued writes, re-capture, publication and portable restore in plaintext
and XTS modes. This distinguishes an empty List from an actually released reader.

The checkpoint case also writes a nonzero file into an immutable lower, unlinks
it and executes Guest `fstrim`. Direct block-device reads verify zero after trim,
local/historical/cross-directory restore, and portable publication/restore. Carrier
inspection follows the exported layer chain: its first present layer must provide
zero and mask the nonzero lower. Legacy tarstream stores explicit zero Data;
tarstream's independent Zero optimization can omit that payload. Storage tests
verify Zero before serialization; carrier tests own payload omission. An empty
active layer above that immutable mask is valid. Both single-layer and checkpoint
restore paths are exercised.

Presence regressions cover allocated Data zeros, payload-free Zero, upper/base partial
writes, accepted but unflushed overwrites, concurrent discard, cancellation,
sticky index failures and ordinary-export completeness. The presence harness
separately records Open/index bytes, first read, 1,024 startup reads with latency
percentiles, a warm repeat and complete active SnapshotView construction. Compare
Open plus the first workload, not Open alone; these are block-layer measurements,
not VM-to-business-ready timings. A sparse 30 GiB logical fixture does not validate
30 GiB of actual payload.

The dense-read probe uses 128 MiB of real payload with a 32 MiB COW cache.
It verifies every byte and reports cold sequential reads, a full repeat, a hot
16 MiB tail, cold random 4 KiB reads and their cached repeat. `api_MiB_s` divides
bytes by cumulative ReadAt service time; `wall_with_validation_ns` also includes
content verification. CPU fields include verification; end RSS, process-lifetime
peak RSS and cumulative Go allocations are distinct measurements. Compare exact
baseline/candidate production sources with identical test sources and binaries
built in the same environment. Seed once, reopen for each case, and report raw
samples and workload size rather than treating a short warm read as cold disk
throughput. The write driver accepts `--working-set-mib` (default 64); increase
`--operations` to at least `working-set-mib * 512` to retain two traversals.

The benchmark driver records host/filesystem/build identity, raw logs, frontend
and checkpoint p50/p95/p99, throughput including final Drain, physical data/index/
redo/GC byte counters, cache hits and process CPU/RSS. Its 64 MiB working set
exceeds the 32 MiB COW cache, except the intentional 4 MiB hot case. At least three
runs are required. Overwrite cases use two working-set traversals; `first-sequential`
and `first-random` write each page exactly once on a fresh disk. Historical mode
labels `raw`, `v1`, `v2-*` remain stable for baseline harness comparison; in the
candidate they select v3 plaintext, v3 XTS and v4 respectively. Compare single-layer
versus checkpoint plaintext and XTS separately, including no-version, protected first
write and already-private overwrite. These commands describe reproducible checks;
they do not claim that performance targets or every privileged test passed.

The following companion probes separate DISCARD ACK from completion including
Drain, and report actual allocation before trim, while a deleted checkpoint is
reader-pinned, after reader release/GC, and after reuse:

```bash
go test ./pkg/diffstore -run '^$' -bench '^BenchmarkCheckpointDiscard$' -benchtime=3x
go test ./pkg/diffstore/internal/versioned -run '^$' -bench '^BenchmarkCheckpointZeroRetention$' -benchtime=3x
```

`BenchmarkCheckpointDiscard` includes Discard plus Drain in `ns/op`, and reports
admission separately as `ack-ns/range`. v4 ACK can precede mapping removal; Drain
does not prove that reader-retained payload or all GC work has been reclaimed.
Use the same completion boundary across revisions.
`backend-calls/range` counts synchronous `diffBackend.discard` calls; it excludes
cached v4 Zero writeback through `writePages`, so zero does not mean no backend
work. Discard also reports process CPU, bitmap/index/redo bytes and structure
commits. Background GC may contribute to versioned counters. Zero over existing
Data leaves P set and needs no bitmap rewrite. For an older revision that offers
both modes, use its `BenchmarkCheckpointGuestDiscard` for the same Zero semantics;
historical Hole results are not comparable. Fresh-write COW benchmarks close and
recreate the same-config DIFF outside the timer instead of resetting it with
Discard; Zero is already logically present and would change the measured work.
Reader-retained space and unchanged file length are reported
separately from reclaimable payload. Run these probes without concurrent product
E2E or full race suites, and retain raw results with the PR/Issue.

The template driver separately measures three rounds of fresh 64 MiB single-layer
plaintext/XTS and checkpoint plaintext/XTS initialization. It records Open, Ready,
Materialized and total time, including initial synchronization and publication,
then verifies the entire payload SHA256 outside the timing window. Source
preparation is excluded; process peak RSS still includes that setup.

The scaling driver removes each history before the next size. It reports
checkpoint registration, control reopen and presence reconstruction separately;
its explicit full-scan measurement scales with mapped pages. V4 Disk reopen loads
P without reconstructing presence from the tree; payload queries remain bounded.
The vhost helper freezes exactly 32 dirty pages (128 KiB) before writeback and
reports template readiness, Drain, final materialization, metadata registration,
capture total and reopen separately. Without the helper, registration samples
have zero queued payload. The capture total excludes initial open and guest-write
admission; it does not include VM memory capture. Larger sizes, such as 30 GiB,
can be requested explicitly with `--sizes-gib`.

<a id="reclamation-measurement"></a>

## Reclamation measurements

The reclamation workload stops its independent GC worker to measure committed
free slots, reuse, punch calls, metadata commits/writes and backlog work. It holds
a reader during deletion, measures metadata writes while blocked, then releases
the reader without another deletion. `gc-ns/cycle` sums all GC calls, including
blocked-reader attempts; `backlog-wall-ns/cycle` measures post-unpin draining,
including observing the committed free chain. Sequential
and current-version export reads measure locality after reuse; retained-version
export reads are an unchanged-data control. Read metrics time only `ReadAt`, with
full content comparisons and preallocated expected data outside those durations.
File length and allocated blocks are
reported separately: retained descriptors and management pages are not reclaimed.
`background-gc` measures actual worker startup through completion for a fixed
4 MiB payload backlog, one cycle per round, including polling and scheduling gaps.
The active worker leaves a cancellable 1 ms gap between progressing batches to
reduce foreground contention; explicit GC remains unpaced. The gap trades peak
background throughput for scheduling opportunities, not a foreground latency guarantee.
`gc-write` keeps four recent versions while writing with normal background GC and
reports frontend tail latency and index/redo amplification. Discard uses a fixed
4 MiB range and 1 MiB cache; its synchronous backend-call counter excludes cached
Zero writeback, while `total-commits/range` also includes concurrent GC. Unsupported shared/private
mode combinations are omitted for the dedicated reclamation and Discard cases.
Keep raw per-round results and exact baseline/candidate identities in the PR or
Issue; these workloads establish observations, not a universal speedup claim.

<a id="related"></a>

## Related documentation

[Runtime: start, control, capture and restore](runtime.md) | [Sandbox configuration reference](configuration.md) | [Sandbox and Snapshot artifacts](artifacts.md) | [Guest memory and lazy restore](memory.md) | [Resource control and usage accounting](resources.md) | [Guest ABI: boot, applications and host protocols](guest-abi.md) | [VMM integration and Cloud Hypervisor patches](vmm.md)

[Component entry](../README.md) · [Native builds and patch materials](../native-deps/README.md)
