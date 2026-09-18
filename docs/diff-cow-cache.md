[English](diff-cow-cache.md) | [简体中文](diff-cow-cache_zh.md)

# Active COW I/O and plaintext cache

Active writable diffs request O_DIRECT through one common positioned-I/O API,
including on tmpfs. Runtime policy does not identify the backing filesystem.
One bounded plaintext page cache is owned by `sandbox-ctl` for each sandbox; root and data disks share it, including mixed tmpfs/disk storage. This
implements [issue #230](https://github.com/kuasar-sandbox/sandboxer/issues/230), with
tmpfs compatibility restored by [issue #238](https://github.com/kuasar-sandbox/sandboxer/issues/238).
It changes active data access, configuration and lifecycle; immutable base images,
templates, historical overlays, manifest decryption caches, artifact outputs,
tarstream representation, cgroups, balloon and guest resource budgets retain
their existing behavior.

## Configuration and ownership

```yaml
resources:
  diff_cow:
    cache_size: 32MiB
    max_dirty_size: 16MiB
```

Both fields are positive multiples of 4 KiB, with
`0 < max_dirty_size <= cache_size`. Omitted fields independently default to the
values above, including old configs without `diff_cow`. A single supplied field
must still satisfy the relationship after defaults. These are finite engineering
defaults, not a production SLA or a claim of optimal performance. There is no
zero/unlimited value, disable switch, failed-data-I/O buffered retry or
configurable alternate write mode.

`max_dirty_size` is a subset of `cache_size`, never an additional pool or a
reserved partition. Clean pages may use the entire cache when there are no dirty
pages. Adding a disk does not multiply either limit. These host process resources
are independent of guest capacity, allocatable/startup headroom and VMM overhead:
`sandbox-ctl` is outside the VMM cgroup. No resource-controller changes are needed.

Cold start, `run --from`, and restore resolve host resources for this instance.
Runtime configuration retains the policy for that sandbox's lifecycle; a new
restored instance uses its current host configuration. Portable C0/E/S never
contains cache policy, pages, queues or LRU state. Strict restore validation,
config merging and host projection preserve this boundary.

Go callers can pass one `COWCache` with `WithCOWCache` to multiple `OpenBlockCOW`
calls. Close the COWs before closing that cache. Without this option, a standalone
COW owns a finite default cache and closes it itself.

## Logical state and accounting

The stack is `BlockCOW -> plaintext cache -> diffFile encoding -> owned I/O
workspace -> active file`. Pages are 4096 bytes. Base reads do not populate or
materialize the upper cache. Each cached page is indexed by active file and block number.

| State or transition | Total used | Dirty used |
| --- | --- | --- |
| Clean, dirty, writeback, read loading | One page each | Dirty/writeback only |
| New write construction/reservation | One page | One page |
| Clean becomes dirty | Unchanged | Add one page |
| Dirty overwrite or dirty becomes writeback | Unchanged | Unchanged |
| Successful writeback becomes clean | Unchanged | Remove one page |
| Evict clean | Remove one page | Unchanged |

At all times `dirty_used <= used <= cache_size` and
`dirty_used <= max_dirty_size`. Submitting I/O does not release reservations.
The page pool, index, clean LRU and dirty FIFO are bounded by capacity. Plaintext
is cleared on release. Separate bounded overhead consists of one 1 MiB batch
copy and at most 256 page pointers per sandbox, two independent 1 MiB MAP_SHARED
I/O workspaces per active file (each with less than 1 MiB alignment padding),
and page metadata/index/Go allocator overhead proportional to page capacity.
`cache_size` is not a cap on process RSS, guest memfd mappings, immutable-source
caches, tmpfs file storage or snapshot output memory.

Tmpfs file pages are filesystem storage in memory (and potentially swap), separate
from this bounded process cache. Successful writeback releases dirty quota but
does not release stored tmpfs data; clean-cache eviction does not remove it either.
`cache_size` does not bound those bytes or promise zero tmpfs page residency.
Existing tmpfs mount size/inode limits and host memory controls govern that
storage. Sandboxer does not remount tmpfs, change cgroups, inflate cache budgets
or add persistence guarantees. See the [Linux tmpfs documentation](https://www.kernel.org/doc/html/v6.1/filesystems/tmpfs.html).

The bitmap means **logical upper-present**, independent of clean/dirty state.
A complete new page is published to the cache and bitmap before acknowledging
the write. Successful writeback or eviction never clears that bit. Reopening
reconstructs upper presence from sparse file extents using the existing format.
The cache keeps latest plaintext ahead of physical file state.

## Reads, writes and backpressure

Reads copy from clean, dirty or frozen writeback pages first. Only a cache miss
may read/decrypt the active file. Read loading reserves total capacity; if no
clean slot is available, a bounded foreground workspace can service the read
without insertion. It never bypasses a newer cached page.

Live and snapshot reads share bounded upper-run batching. Only contiguous cold
pages of the requested upper range enter a physical read (at most 1 MiB); cache
hits and base/hole boundaries stop the run. Sorted stripe-range locks prevent
concurrent writes/discard, while reserved Loading pages prevent eviction. With
less cache capacity than the run, remaining cold pages bypass caching under the
same stripes. Read/decrypt stays in the owned MAP_SHARED I/O workspace: clean
pages are populated from that workspace before copying out to mutable caller or
guest memory. No request-sized payload allocation or base prefetch is added.

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

## Writeback and FLUSH

One sandbox worker anchors each batch on the oldest dirty page. Hot overwrites
do not move a page to the tail. It may batch adjacent dirty pages of the same
file up to 1 MiB, without filling holes or preallocating gaps. A fixed 1 ms
aggregation window allows low-rate traffic to progress; quota pressure and
internal Drain wake it immediately. Selected pages become frozen writeback:
reads remain possible and same-page writes wait. The worker copies plaintext to
a bounded batch workspace, encrypts the copy, and performs I/O without cache or
global locks. Foreground reads have a separate workspace.

The bounded page index supplies contiguous dirty neighbours from the same file,
regardless of arrival order; unselected FIFO age is unchanged. Ordinary write
notifications neither end nor restart the fixed aggregation deadline. Pressure
and Drain interrupt it. An already aggregated backlog proceeds without a new
1 ms wait per batch, including batches that contain only one page.

A healthy, valid guest **FLUSH is a no-op**. It neither initiates writeback,
waits for dirty pages nor invokes Drain, fsync or fdatasync. Request ordering and
fatal-state checks remain. Existing wire features are retained for Cloud
Hypervisor snapshot compatibility; CONFIG_WCE and wire discard are not added.

Write completion promises only acceptance of complete latest bytes into bounded
logical COW state. Neither writes nor FLUSH guarantee survival of a
`sandbox-ctl` crash, host power loss or storage failure. This deliberately differs
from the stable-storage FLUSH requirement in
[Virtio 1.2 §5.2.6.2](https://docs.oasis-open.org/virtio/virtio/v1.2/virtio-v1.2.html).
It is the project's non-durable COW contract, not standard durable FLUSH behavior.
Direct I/O alone is not a durability barrier either.

## Snapshots, discard and shutdown

After CH pause and frontend quiesce, `SnapshotView` copies the logical bitmap and
serves upper-only plaintext, reading dirty/writeback cache pages before the file.
It does not force Drain/fsync or copy the whole cache. Background writeback may
continue moving identical logical contents. The last cached copy becomes
evictable only after successful file completion. Keep frontend requests quiesced
and the COW open until capture finishes. Background fatal errors during capture
must fail publication and reach the runtime owner, including when no guest request
follows the failure. Export does not rotate the active diff or change its base.

Low-level Discard serializes with writes and in-flight writeback before punching
complete blocks and clearing upper presence. Partial edges retain contents.
Discard exposes base bytes again; it is not an explicit zero masking the base.
The wire profile still rejects DISCARD and WRITE_ZEROES. Writing zeros still
materializes upper data and cannot be treated as a hole.

Internal `Drain` waits for accepted writes to finish without fsync. Normal Close
stops new frontend work, cancels quota waits, drains accepted healthy writes,
joins I/O, clears plaintext and closes the file. Frontend cancellation does not
cancel healthy writeback. Close is idempotent and drain/close errors propagate
through run/restore. Destruction must also wait for actual in-flight syscalls
before freeing their buffers.

Writeback and short-write failures are sticky: failed pages remain dirty-accounted,
new writes fail, all waiters wake, and the runtime receives a non-blocking fatal
notification without self-joining. A partial physical write may already have changed
the file. Cleanup of newly materialized blocks cannot punch previously existing
upper pages in the same batch. There is no transactional-write or crash-recovery
promise and no silent buffered retry.

The original error is latched and the owner notified before cleanup I/O. While rollback runs, active-I/O ownership, frozen pages and their quotas remain held so Close cannot release the file or buffers early. Cleanup errors are joined afterward without masking the original failure.

## Active-file I/O contract

Every active body attempts O_DIRECT on its opened descriptor and always uses the
same bounded aligned workspaces. If that F_SETFL request returns EINVAL or
EOPNOTSUPP/ENOTSUP, the descriptor keeps ordinary positioned I/O; the application
buffer and cache are unchanged. This initialization-only capability decision
does not inspect the filesystem and is not a retry after failed data I/O. Fresh targets complete this setup **before seeding**; existing
active validation and runtime/snapshot reads use the same path. Runtime code does
not inspect filesystem types, names, mount policies or filesystem-specific inode
flags. Header and format probing are bounded before concurrent body I/O;
templates and base remain buffered/read-only. Plaintext and encrypted file
formats, the fixed 4 KiB encrypted header, local encryption off/auto/required
policy and 512-byte XTS data-unit numbering do not change.

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

## Validation

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
plain and encrypted real-ENOSPC cases as root in a new mount namespace. The test
uses a 32 KiB private tmpfs and never exhausts or remounts a shared mount; ordinary
`go test` runs skip only this capability-requiring case rather than attempting
implicit privilege escalation. The runner requires successful plaintext and encrypted
case markers (not a Skip/no-tests result), bounds execution, and removes its own
temporary files even after failure.
DIO/mincore benchmarks retain disk-backed fixtures. Tmpfs functional tests are
not evidence of disk cache bypass. Run targeted tests, race, vet, build, broader
tests and real CH/KVM tmpfs guest cold-start/read-write
and pause/export/restore; report skips and infrastructure failures explicitly.

Performance comparisons need datasets much larger than the cache and separate
frontend admission latency from total elapsed time including Drain. Cover
sequential/random 4 KiB, 512-byte updates, hot overwrites, encryption and multiple
disks. Report tail latency, logical/physical write volume, stable quota usage and
file-cache/dirty/writeback or mincore evidence. Compare buffered baseline and
candidate on the same storage. Do not manufacture low cache residency with
fsync/DONTNEED/drop_caches; exclude unchanged template input, artifact output and
shared guest mappings from active-diff memory attribution. Measurements are
observations, not unmeasured throughput targets or claims that skipped E2E passed.

Measured baseline/candidate results and outstanding E2E prerequisites are recorded
in the [validation observations](diff-cow-cache-validation.md).
