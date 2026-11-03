[English](cloud-hypervisor.md) | [简体中文](cloud-hypervisor_zh.md)

# cloud-hypervisor — VMM and platform patches

The platform uses Cloud Hypervisor (CH) as its microVM monitor. Most paths use
upstream behavior. Externally managed memory, restore-safe vsock, and reliable VM
lifecycle barriers are supplied by patches 0001–0008 maintained in this repository.
Patch 0009 preserves ARM snapshot registers; 0010 and 0011 backport upstream
restore-order and HTTP allocator seccomp fixes. The series remains on CH v51.1
and the Host Linux 5.10 baseline.
This document defines their scope, build procedure, and behavioral contracts.

## 1. Overview

### 1.1 Why patches are required

The platform has four requirements beyond the pinned upstream VMM behavior:

1. **Externally owned sandbox RAM**: the host process (`sandbox-ctl`) owns the
   memfd inode. CH maps that same inode through an inherited descriptor rather
   than allocating the backing with its own `memfd_create`. The owner can scan
   resident data with `SEEK_DATA`/`SEEK_HOLE` during snapshotting, without a
   CH-to-file-to-sandbox-ctl intermediate copy.
2. **An external uffd handler**: sandbox-ctl handles sandbox RAM faults and fills
   pages from a snapshot or with zeroes on demand. CH creates the uffd because it
   must be associated with CH's memory map, then passes the descriptor to the
   sandbox-ctl handler with `SCM_RIGHTS`.
3. **Restore-safe vsock**: CH restore recreates an empty Unix backend, while the
   guest retains connected sockets. Snapshotting must stage a virtio-vsock
   transport reset in the guest event queue. Restore reasserts its IRQ and blocks
   new backend RX packets until the guest acknowledges cleanup of old connections.
4. **Reliable VM lifecycle operations**: pause, snapshot, resume, and ordered
   shutdown must not lose vCPU kicks under a numeric `cpu.max` and host contention.
   Old acknowledgements from vCPUs or virtio workers must not cause premature
   completion or indefinite waits.

The pinned upstream interface does not directly provide this combination. The
second requirement is particularly important: the platform needs to own fault
handling and delivery, not just supply data to an upstream external-listener
model. This memory model requires logical release notification even for pages
that have never been resident: their lazy source may contain nonzero bytes.
Patch 0004 may skip redundant inode punching, never the required REMOVE (§3.4).

### 1.2 Patch scope and future evolution

Each patch owns a complete intent, its corrections and its tests. Old numbers in
this table refer to the original twelve-patch series restored by #377. This
first eleven patches retain the #379 maintenance scope. Patch 0012 owns the
complete cumulative memory log; UFFD adoption retains the original ACK.

| Patch | Purpose and scope | Origin | Future maintenance and removal condition |
| --- | --- | --- | --- |
| 0001 | Inherited RAM fd adoption, mapping and ownership | Old 0001, without unused UFFD configuration | Project-maintained. Discuss a generic external-RAM interface upstream; replace only after a released equivalent passes integration. |
| 0002 | Delegate externally owned RAM snapshot contents | Old 0002 | Project-maintained with 0001. An upstream proposal must preserve the external owner's save/restore responsibility; retain the existing virtio-mem scope. |
| 0003 | Complete legacy in-process MISSING UFFD handoff, configuration and permissions | Old 0003 plus the socket configuration from 0001 | Project-maintained protocol. Track upstream external paging interfaces; any migration must preserve peer compatibility and ownership. |
| 0004 | Notify logical release before sparse physical reclaim; errors and real UFFD tests | Old 0004, corrected in place for #378 | Maintain the project optimization. Any upstream sparse-reclaim proposal must preserve REMOVE semantics; this is not an upstream bug caused by unmodified CH. |
| 0005 | Persist vsock local-port allocation cursor | Old 0005 | Submit the restored-port collision fix and its reproducer upstream. Remove after adopting and validating an equivalent released fix. |
| 0006 | Stage vsock reset, persist acknowledgement and gate restored RX | Old 0006 | Propose reset timing and lazy-restore semantics upstream separately from port allocation. Maintain until accepted, released and validated. |
| 0007 | vCPU lifecycle kick, request-specific ACK, timeout and recovery | CPU half of old 0007 | Upstream bugfix candidate with lost-kick and repeated-lifecycle reproducers. Prefer an equivalent upstream fix after verification. |
| 0008 | Device-worker pause publication and resume acknowledgement | Worker half of old 0007 | Independent upstream synchronization fix candidate. Remove only after worker quiescence and resume-barrier regressions pass with the replacement. |
| 0009 | Complete ARM SVE/FPSIMD/FP-control snapshot state | Old 0008–0011 combined | SVE is already upstream; submit only the remaining FPCR/FPSR correction and native tests. Retire backported portions when the selected release covers each part. |
| 0010 | Restore clock, then devices, then vCPUs | Old 0012 | Temporary upstream backport. Remove when the pinned release includes the fix and restore-order validation passes. |
| 0011 | Restricted HTTP allocator reads and mapping growth, with real BPF tests | Upstream seccomp fixes adapted to v51.1 | Temporary upstream backport. Remove after equivalent permissions and positive/negative filter tests pass on the selected release. |
| 0012 | Lifetime CPU/CH dirty ledger, raw writers, initialization, cumulative API and tests | Dirty-only parts of withdrawn #375, adapted to #379 | Maintain the fixed-baseline API locally while discussing an upstream interface. Align raw writers with upstream 3777ba8d; retire equivalent portions only after source and integration verification. |

The upstream audit is pinned to
[`0674238bb`](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/0674238bb09082dfe55a4d3215eebf35ee0f5bb1)
(2026-10-29); planned submissions above are not claims of upstream acceptance.
Patch numbering is not a binary compatibility identifier. Record the upstream
source, complete patch hashes and resulting build identity; the original legacy
CH used by #374 remains a frozen, distinct artifact.

### 1.3 Maintenance strategy

- Apply the series in order. Each prefix must build and must fully implement any
  capability it exposes; configuration is introduced with its actual consumer.
- Fold subsequent fixes and regression tests into their functional owner instead
  of accumulating fixup patches. Preserve original authorship, upstream links and
  sign-offs; do not attribute local additions to an upstream author.
- Verify pure reorganization by comparing complete applied source trees. Record
  intentional behavioral deltas separately: this maintenance revision changes
  release notification/order, preservation of released zeros in capture, the ARM
  SVE capability guard, and HTTP allocator seccomp permissions.

- The repository-relative patch directory is
  `native-deps/deps/ch-patches/*.patch`, applied in numeric order.
- Run `make ch-patches-apply` from `sandboxer/native-deps`; it is also part of a
  fresh `make cloud-hypervisor` build. The development cycle and idempotency
  checks are described in [native-deps/README.md §3](../native-deps/README.md#3-patch-development-cycle).
- Review the patch set for each intended upstream upgrade. Resolve conflicts
  against the actual upstream changes; neither a fixed release cadence nor a
  fixed amount of rebase work is assumed.
- The external RAM/UFFD design uses in-process creation, `SCM_RIGHTS`, and ioctls
  in `create_ram_region`, and remains a project-maintained patch set. When an
  equivalent upstream vCPU-kick or worker-barrier fix is available, prefer it
  when upgrading rather than indefinitely maintaining duplicate patch 0007/0008 logic.

## 2. Activation and command line

CH `--memory-zone` accepts two additional keys:

| Key | Type | Meaning |
| --- | --- | --- |
| `fd` | int | Backing descriptor inherited from the parent, for example through `cmd.ExtraFiles` |
| `uffd_socket` | path | Ordinary UDS path used by CH for `sendmsg(va_report; SCM_RIGHTS=uffd_C)` |

The memory-backing and external-fault-handler contracts are:

- **Neither `fd` nor `uffd_socket` is set**: memory allocation follows the
  upstream backing-file/memfd path and this external uffd handler is not enabled.
  This is not a claim that every other patch, such as the lifecycle or vsock
  fixes, is disabled.
- **Only `fd` is set**: CH uses the supplied backing instead of creating a memfd.
  Snapshot does not dump this user-managed zone and restore does not fill it.
  Externally owned backing does not itself require uffd.
- **Both `fd` and `uffd_socket` are set**: in addition to the backing behavior,
  CH performs `userfaultfd()` → `UFFDIO_API` →
  `UFFDIO_REGISTER MISSING on chVA` → `connect uffd_socket` →
  `sendmsg(va_report; SCM_RIGHTS=uffd_fd)` → wait for acknowledgement.
  vCPUs must not run before the acknowledgement.

The backing/handler combination used by sandbox-ctl is the same for cold start
and restore:

```text
cloud-hypervisor \
  --memory-zone id=ram0,size=8G,shared=on,fd=3,uffd_socket=/run/sandbox/<sid>/uffd.sock \
  ...
# fd=3 is the memfd inherited through sandbox-ctl cmd.ExtraFiles[0].
```

The zone `id` is required by the parser; the platform uses `ram0`. Complete cold
start and restore commands are described in [sandbox.md](sandbox.md) §5.2 and §7.

## 3. Patch organization

### 3.1 0001 — externally allocated memfd-backed memory zone

Subject: **memory: support externally allocated memfd-backed memory zone**.

Changed files: `vm_config.rs`, `config.rs`, and `memory_manager.rs`.

Key points:

- `MemoryZoneConfig` adds `pub fd: Option<i32>` with `#[serde(default)]` in the
  maintained patch, not `serde(skip)`. A serialized descriptor number is not a
  transferable kernel reference; each new CH process still needs the appropriate
  inherited descriptor.
- `MemoryConfig::parse` parses `fd=N`. The memory-manager construction paths,
  rather than the CLI parser, adopt the inherited descriptor with
  `File::from_raw_fd`. CH owns those `File` handles and clones; the parent has its
  own descriptor and lifetime.
- When the zone supplies a descriptor, the memory manager passes that backing to
  `create_ram_region` instead of creating another memfd and records the zone as
  `user_managed: true` through patch 0002.
- A zone can contain multiple regions, for example when a large x86_64 zone
  crosses the PCI MMIO hole. Each region obtains a `try_clone()` and owns its
  `File`; the mappings and their file offsets refer to the same backing inode and
  share host page-cache data.

`Transportable::send` and `fill_saved_regions` both traverse
`snapshot_memory_ranges`. They do not need separate patches: patch 0002 filters
`memory_range_table`, affecting both snapshot and restore.

### 3.2 0002 — snapshot skip user-managed memory zones

Subject: **snapshot: skip user-managed memory zones**.

Changed file: `memory_manager.rs`.

Key points:

- `memory_range_table` checks each zone's `user_managed` flag when constructing
  `snapshot_memory_ranges`. A `user_managed=true` zone is omitted.
- The snapshot dump path, `Transportable::send`, therefore has no ranges to dump
  for that zone and returns successfully without writing its RAM to a
  memory-ranges file. For the 8 GiB example, CH does not write an 8 GiB RAM dump.
- Restore's `fill_saved_regions` similarly has nothing to fill for the zone.
  Guest physical addresses are backed directly by the sandbox-ctl memfd mapping;
  sandbox-ctl's uffd handler supplies the contents on demand when configured.

Upstream already has a skip path for a file-backed, `MAP_SHARED`, hard-linked
region used in shared-storage migration. The external memfd is file-backed and
shared but has no hard link (`st_nlink == 0`), so the project adds the explicit
`user_managed` branch instead of reusing that hard-link test.

### 3.3 0003 — external uffd handler via in-process create + SCM_RIGHTS

Subject: **memory: external uffd handler via in-process create + SCM_RIGHTS handoff**.

Changed files: `memory_manager.rs`, `vm_config.rs`, `config.rs` and `seccomp_filters.rs`.

Key points:

- After mapping a region, `create_ram_region` performs the following when
  `uffd_socket` is configured:

  ```text
  uffd = userfaultfd(O_CLOEXEC | O_NONBLOCK)
  UFFDIO_API features = UFFD_FEATURE_MISSING_SHMEM
                      | UFFD_FEATURE_EVENT_REMOVE
                      | UFFD_FEATURE_EVENT_UNMAP
                      | UFFD_FEATURE_THREAD_ID
  UFFDIO_REGISTER(uffd, [chVA, +size], MISSING)
  conn = connect(uffd_socket)
  sendmsg(conn, iov=va_report{chVA, size}, cmsg=SCM_RIGHTS([uffd]))
  recvmsg(conn, expect ack)
  // Return to create_ram_region and continue vCPU startup.
  ```

- Feature-bit positions must match the kernel headers exactly:

  ```text
  UFFD_FEATURE_MISSING_SHMEM = 1 << 5
  UFFD_FEATURE_EVENT_REMOVE  = 1 << 3
  UFFD_FEATURE_EVENT_UNMAP   = 1 << 6
  UFFD_FEATURE_THREAD_ID     = 1 << 8
  ```

  A wrong position requests a different feature. For example, `1 << 1` is
  `EVENT_FORK`, which requires `CAP_SYS_PTRACE` and can cause `UFFDIO_API` to
  return `EPERM` when that capability is unavailable.
- The seccomp filter allows `SYS_userfaultfd` and the `UFFDIO_API` /
  `UFFDIO_REGISTER` ioctls.

**Why uffd is created inside CH**: the uffd context belongs to the memory map of
its creating process. An uffd created by sandbox-ctl cannot register CH's chVA
mappings. CH therefore creates and registers the uffd, then transfers the
file descriptor with `SCM_RIGHTS` so sandbox-ctl can consume its events. Descriptor
tables are process-local, but the context still routes events using the creator's
virtual addresses; the event addresses received by sandbox-ctl are CH's chVA.

**Backend first-touch during lazy restore.** The backend mmap is deliberately
not UFFD-registered. Ordinary shmem access can create a zero folio without
fetching snapshot bytes, so even a read or a one-byte status write must prepare
memory first. Every vhost GPA/UVA translation calls the context-aware
`EnsureLoaded(ctx, memfdOffset, length)` barrier before returning a slice. This
includes descriptors and indirect tables, avail/used rings, headers, both data
directions and status. Missing callback wiring, invalid ranges and population
failures are terminal for the queue; they cannot publish a used-ring completion.
The central server constructor path binds the barrier for every disk, for both
CLI and SDK cold/restore runs. Queue reset cancels pending loads and drains
workers before replacing the memory table.

The barrier checks intersecting pages, including the untouched part of a partial
access. Loaded ranges are scanned in batches and preserve live bytes without
reading the snapshot. Absent pages read the authoritative source into an existing
fault worker's private 4 KiB buffer. Released pages never read historical source
bytes. Full-page installation uses the owning CH `UFFDIO_COPY`, including a zero
scratch page when necessary for shmem. `EEXIST` preserves the winning folio and
wakes its waiters; it does **not** establish that an earlier unguarded backend
access restored the snapshot. Completed byte counts are validated. `EAGAIN`
retries cancelably outside the worker, permitting the event reader and CPU fault
queues to progress; `ENOENT`/invalid registration fails the request. A genuine
CPU MISSING fault on a Loaded page remains a separate reclaimed-page path.

`va_report` retains its wire format. Its computed memfd offset is passed through
the internal callbacks and associated with the actual CH VA interval and owned
UFFD descriptor. New registrations become usable only after successful adoption;
failed adoption rolls back the staged map and leaves fd cleanup to the sender of
the callback. Loads wait cancelably for registration readiness and split at
region boundaries. UNMAP invalidates the affected registration interval; REMOVE
releases page contents while retaining registration.

An installation mutex coordinates CPU urgent, backend mandatory and speculative
tail ioctls with the nonblocking kernel event read and publication of
REMOVE/UNMAP state. Revalidation occurs after source reads and immediately before
installation, so stale snapshot buffers cannot be installed after an observed
release. Source reads, retry waits, registration waits and blocking queue sends
are outside this mutex. Bounded ordinary/chunk tail batching is retained. The
lock review relies on the [Linux 5.10 UFFD event and COPY paths](https://github.com/torvalds/linux/blob/v5.10/fs/userfaultfd.c):
REMOVE drops the mmap read lock before waiting for userspace, and COPY can return
`EAGAIN` while mappings change. This mutex is **not a DMA pin**: normal guest
ownership of outstanding descriptor buffers is required, and arbitrary concurrent
balloon/discard after the barrier returns is outside this guarantee.

Close synchronizes admission with cancellation, drains callers and all workers,
and stops the reader through its bounded epoll poll before closing UFFD fds.
The source and backend mapping outlive that drain. Readiness/handshake waits also
unwind on early startup errors. No second UFFD, MINOR/CONTINUE, eager whole-RAM
population, kernel-floor increase or resource-policy change is involved.

Repeatable validation uses `TestRealMissingShmemRestoredUsedRingPreservesUntouchedBytes`
in `pkg/vhost`: two shared memfd aliases, a real CH-only MISSING_SHMEM
registration, and the actual `publishUsed` path must preserve all surrounding
bytes and advance saved index 7 to 8 for both CPU-first and backend-first orders.
UFFD/translation tests cover range, cancellation, registration, release and
concurrency failures. The existing `snapshot.restore.sh` E2E additionally retains
an O_DIRECT file descriptor and 32 anonymous buffers across snapshot/restore. A
512-byte read at page offset 512 must preserve the other 3,584 bytes; it includes
a cold control, sixteen candidates untouched by userspace before DMA and sixteen
explicitly CPU-warmed restored controls. `dio-before.json`/`dio-after.json` retain
the integrity results. Candidate pinning/prefetch can populate pages, so the E2E
alone does not prove backend-first ordering; the real used-ring test supplies
that deterministic ordering. Run both with the prepared source/artifact contracts
and the existing KVM pressure coverage, including the Linux 5.10 floor. These are
validation requirements, not a claim that an unexecuted run passed.

### 3.4 0004 — preserve logical release before sparse reclaim

The release implementation and seccomp policy live in `virtio-devices`.
`vmm/src/vm.rs` and `device_manager.rs` pass VM creation provenance to the balloon. Problem report: [#378](https://github.com/kuasar-sandbox/sandboxer/issues/378).

The original optimization skipped both `PUNCH_HOLE` and `MADV_DONTNEED` when
`SEEK_DATA` proved the backing range was physically empty. It reduced synchronous
REMOVE processing during cold-start balloon convergence, but its equivalence
argument was wrong: a physical Hole can still inherit nonzero content from a
lazy snapshot source. Suppressing REMOVE leaves that source valid, so a later
CPU fault or backend load can resurrect released content. Both balloon inflate
and free-page reporting call the affected release helper.

The corrected sequence is:

1. Validate the complete guest range and issue `MADV_DONTNEED` on chVA. Even an
   all-hole range must produce its logical release notification. Advice errors
   return before probing or punching.
2. For a file-backed region, translate the guest offset using the region's
   `file_offset().start()`, then probe with `SEEK_DATA`. `ENXIO`, or Data at/after
   the end, permits skipping only redundant `PUNCH_HOLE`. Other probe errors
   fall through to the ordinary punch operation, not to a success return.
3. Punch the complete released backing range when necessary and propagate errors.
   A failed punch may leave resident bytes; callers must observe the failure.

The existing Go handler serializes reading REMOVE and publishing `Released`
with COPY installation using `installMu`. In-flight source reads recheck state
before installation. After the notification, the CH probe/punch catches earlier
installed content. This reuses existing synchronization and adds no dirty ledger.
The actual file-backed predicate is `region.file_offset().is_some()`, not
`user_managed`; non-file-backed regions still receive advice. The shared file
position is not used by the mapping/positioned-punch data path. Balloon seccomp
continues to allow `lseek`; `fallocate` and common `madvise` were already allowed.

On a 4 KiB host, ordinary cold VMs and cold Build producers sort and deduplicate
up to 8192 PFNs at a time within each inflate descriptor. Adjacent PFNs in one
memory region have consecutive backing offsets and share a release. The worker
consumes every chunk, including the tail, before acknowledging the descriptor.
The 32 KiB scratch limit bounds one chunk. A queue callback can process many
chunks and descriptors.

The VMM records whether the top-level VM snapshot was absent before looking up
the device manager. The balloon also requires absent balloon state. This
runtime choice lasts for that VM, including subsequent inflation. Every new
restore instance uses per-PFN release even if either child snapshot is missing.
Larger host pages retain partial-page ownership tracking.

Run the ordinary error-path regression and the explicit native UFFD tests:

```sh
cargo test --locked -p virtio-devices --features kvm --lib release_tests -- --include-ignored --nocapture
```

The native tests use the production 4 KiB-PFN inflate and range-release helpers,
real shmem MISSING/REMOVE, nonzero backing offsets, ENXIO and out-of-range Data.
They compare complete released and neighboring pages, including resident and
unmaterialized nonzero sources. They require actual UFFD capabilities and are
kernel component tests, not full VM E2E. The ordinary test checks alignment and
real punch failure. Retain a failing original-implementation control and also run
the existing Go release/install tests and rebuilt-CH integration suites.

The release fix includes the associated capture boundary: after CH/backend
freeze, snapshot samples physical holes and existing page state under installMu.
Data uses current bytes; Hole+Absent inherits the parent; Hole+Released or Loaded
is explicit Zero. Loaded+Hole covers a zero installation followed by CH's final
punch. Capture does not reset state, including on failure. Snapshot layering and
the existing carriers preserve Zero through capture and restore without refault
or full-RAM prefilling. This requires no memory.dirty entry, new dirty API or
history trimming; those remain in #374.

### 3.5 0005 — persist the vsock host local-port cursor

Restore recreates CH's Unix vsock backend. Resetting its local-port allocator to
`0x40000000` can make the first host-initiated connection reuse a tuple retained
in the guest snapshot and receive an RST. This patch stores `local_port_last` in
`VsockState` and continues from the next port after restoring the backend. Nested
snapshots preserve the cursor at each layer rather than relying on a process-local
cursor that only covers one restore.

### 3.6 0006 — stage a vsock transport reset before snapshot

Avoiding port reuse alone is insufficient. CH does not serialize the backend
connection map, while the guest snapshot retains sockets, credits, and closing
state. The two endpoints would therefore resume in different transport epochs.
Guest RAM is restored lazily through external userfaultfd; device activation
cannot depend on finding a fresh event-queue descriptor in the restored avail
ring. The patch uses this protocol:

1. After the VM is paused, `Vsock::snapshot` consumes a guest-provided writable
   descriptor from the **source VM's live event virtqueue**, writes
   `VIRTIO_VSOCK_EVENT_TRANSPORT_RESET`, advances the used ring, and sets
   `transport_reset_pending`. An ordinary `/vm.pause` does not stage a reset.
2. The pending state is saved in `VsockState`. Both source-VM resume and target
   restore activation only reassert the IRQ for the already-used event. Restore
   neither reads nor consumes another event descriptor.
3. While pending, the backend may accept host UDS connections, but it retains all
   RX packets instead of placing them in the guest RX queue.
4. The Linux guest processes the reset: connected sockets are closed, the CID is
   reread, and listeners remain bound/listening. It then replenishes the event
   descriptor and kicks the event queue. CH treats this kick as acknowledgement,
   lifts the RX gate, and immediately drains pending RX.

Snapshot staging first drains eventfd kicks that predate the reset. If an old
notification is still present in the same epoll batch after the device thread
resumes, a nonblocking read returns `EAGAIN` and keeps the gate closed; it is not
mistaken for the new reset acknowledgement.

The first `restore` control connection can therefore be initiated immediately
after VM resume, but its REQUEST becomes visible only after guest cleanup of the
old connections. This protocol adds no retry, sleep, or relaxed deadline. A
missing or invalid descriptor makes the source snapshot fail explicitly. Target
restore activation must succeed without any new available descriptor because the
reset already exists in the snapshot's used ring.

### 3.7 0007 — vCPU lifecycle requests and acknowledgements

CH v51.1 uses a no-op `SIGRTMIN` handler to interrupt `KVM_RUN`. The control thread
publishes pause/kill state, calls `pthread_kill` for each vCPU, and waits for loop
acknowledgement. A signal arriving in userspace after the state check but before
entry to `KVM_RUN` can be consumed too early, leaving that vCPU blocked in the
next call. Repeating it every 10 ms reduces probability but does not close the
race.

The patch closes the lifecycle-barrier races through two related changes:

1. Before spawning a vCPU, the creating thread configures `KVM_SET_SIGNAL_MASK`
   and blocks the kick signal so the child inherits the mask from its first
   instruction. Configuration failure returns directly, avoiding an early-exited
   child outside the startup barrier. The vCPU keeps the signal blocked while
   reading lifecycle state. `KVM_RUN` atomically unblocks it with the configured
   mask and restores the blocked mask on exit. A signal between the state check
   and `KVM_RUN` remains pending and interrupts that same call. Userspace PIO/MMIO
   exit handling does not consume it. Only after the outer loop observes the
   corresponding lifecycle request and completes pending KVM I/O does it briefly
   `SIG_UNBLOCK`/`SIG_BLOCK`, consume the kick through the empty handler, and
   restore the mask. This prevents an old pending kick from repeatedly causing
   immediate `KVM_RUN` returns. The common VMM KVM ioctl seccomp set explicitly
   allows `KVM_SET_SIGNAL_MASK`.
2. Pause, shutdown, NMI, and vCPU removal clear old ACKs before publishing a
   request. A vCPU writes an ACK only in the corresponding pause/NMI/kill branch;
   a natural reset, shutdown, run error, or panic is not an acknowledgement.
   The KVM no-miss path sends one kick per request. Pause/NMI consume an already
   pending kick before acknowledging and again after park/unpark, covering the
   ordering in which the vCPU sees shared state before the controller sends the
   signal. NMI uses a two-way ACK-set/ACK-clear barrier, and the controller waits
   for all vCPUs to clear the current ACK. A previous request's drain cannot then
   swallow the next pause kick. A naturally finished, unjoined vCPU is excluded
   from the signal barrier using `JoinHandle::is_finished()` while its ACK remains
   false; shutdown cleanup does not wait for an impossible acknowledgement.
   Failed pause revokes the request, unparks participants, and waits for recovery.
   NMI completes ACK-clear cleanup even after a signal timeout. Both failure
   paths receive a separate fresh deadline. Non-KVM backends retain the 10 ms
   retry. All waits use a 1 s deadline with `CLOCK_MONOTONIC` semantics.
### 3.8 0008 — virtio worker pause and resume handshakes

Runtime pause publishes the shared pause event before explicitly unparking
   ordinary and custom workers, closing the startup race between a zero-time
   epoll poll and `thread::park`. Resume drains the event before waking workers.
   After returning from park, workers participate in a second barrier; the
   controller waits for resume ACKs before another pause. Custom net and
   vhost-user worker handles participate in both the pause wake and resume
   barrier. A restored device that has not received a runtime pause event does
   not wait for a nonexistent ACK.

These lifecycle patches do not change sandbox configuration, the CH HTTP API, snapshot
format, or resource protocol, and does not adjust `cpu.max` around lifecycle
operations. Timeouts remain bounded failure protection, not the race fix.

### 3.9 0009 — complete ARM CPU snapshot registers

On SVE-enabled ARM hosts, KVM exposes vector state through SVE Z/P/FFR
registers. Accessing legacy FPSIMD V-register offsets returns `EINVAL`; a
cold-booted VM can run successfully while snapshot capture fails.

Patch 0009 combines Ruben Hakobyan's merged
[Cloud Hypervisor PR #8268](https://github.com/cloud-hypervisor/cloud-hypervisor/pull/8268),
retaining authorship and sign-offs. The reviewed upstream commits are
`083b41905ea59b335c4f211c4186312e30bbf022`,
`e9d044730524d12e243737a1888767b3edf123c8`, and
`4fc414cc8bc56a4c5275b3dd44610e5c75dfbbd1`; context is rebased onto v51.1
and the preceding platform patches. They enumerate KVM registers, preserve SVE state and
restore vector-length selection after vCPU initialization and before SVE
finalization. The legacy vector path remains available without SVE.

The same patch also backports upstream
[48834e1](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/48834e139e9d1882cdefb6cafa42155a13d173bc):
SVE finalization checks the VM's KVM ArmSve capability as well as host HWCAP,
matching feature initialization. The v51.1 adaptation passes the existing VM
reference rather than importing an unrelated Vcpu ownership refactor.

The same patch additionally preserves FPSR/FPCR for both paths: those control and
status registers remain accessible even when the vector register family changes.
It includes explicit native KVM round-trip tests with nonzero register state:

```sh
cd native-deps/build/src/cloud-hypervisor
cargo test --locked -p hypervisor --features kvm test_arm_ -- --ignored --nocapture
```

These tests require native ARM KVM (including SVE for the SVE case); their ignored
marker does not constitute successful validation on hosted ARM runners without
`/dev/kvm`. Final platform qualification also uses real cold start, guest exec,
snapshot and restore with the selected released Cloud Hypervisor binary.

Snapshot CPU state gains defaulted pre-finalize and extended-register arrays.
This permits reading older non-SVE state; it does not promise cross-host CPU
feature compatibility or backwards restore into an unpatched VMM. SVE remains
enabled, and incompatible register state fails through the existing KVM errors.

### 3.10 0010 — restore the KVM clock before vCPUs

Patch 0010 backports upstream [commit ff20f183647c602bf5e5028d99959e5547a7dd84](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/ff20f183647c602bf5e5028d99959e5547a7dd84),
preserving CMGS's authorship and sign-off. In the pinned v51.1 resume path,
vCPUs could execute before the saved x86 KVM clock was restored. The backport
restores that clock first, resumes devices, and then resumes vCPUs. The clock
operation remains x86-specific; the device-before-vCPU ordering also applies
on ARM. It changes no snapshot schema, Guest ABI or pinned upstream version.

Apply the complete numbered series and rebuild the native VMM before validating
this ordering. Patch content participates in the native build/cache identity;
an existing binary without that identity is not validation of the backport.
Run repeated single-vCPU `sandbox.memory-budget` restores, pause/wake, and
`snapshot.restore` with its current direct-I/O integrity assertions, followed
by the ordinary full E2E selection. Retain the native architecture and product
provenance; cross-compilation alone does not exercise KVM resume. A successful
small sample does not establish a failure rate or diagnose unrelated restore
errors.

### 3.11 0011 — HTTP allocator seccomp compatibility

Large HTTP allocations or frees can trigger glibc's lazy read of
`/proc/sys/vm/overcommit_memory`, or mapping growth through `mremap`. The v51.1
HTTP filter can terminate that thread with SIGSYS even though the API operation
itself is valid. Backport upstream
[`660b18e`](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/660b18eaace7953721c8ecf371973fc6eb6e2f66)
(original HTTP fix [`ec157d7`](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/ec157d7eb9f5915512efcd0660a78b41be8bc87e)) and
[`f05f0c2`](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/f05f0c249e69568dea3ef8e7208d63f6ef8922ea)
without importing the newer common-filter architecture.

Allow `openat` only with exactly `O_RDONLY|O_CLOEXEC`, plus `read` and `mremap`,
in the HTTP filter. This restricts flags, not pathnames; it does not promise a
pathname allowlist. Writable, creating and truncating opens remain trapped.
The patch's child-process tests install the real production BPF, exercise a
read and mapping growth, and verify SIGSYS for rejected flags:

```sh
cargo test --locked -p vmm --no-default-features --features kvm --lib seccomp_filters::tests::http_allocator_probe_is_read_only -- --nocapture
```

This is an allocator compatibility backport, with no new dirty-memory API or
response format. `mremap` is an upstream compatibility fix, not evidence that it
was the syscall in every previously observed API EOF.

### 3.12 0012 — cumulative checkpoint memory changes

External RAM enables KVM logging during memory initialization and preserves
software seed writes before any reset. CPU/KVM, CH AtomicBitmap and raw writers
(net RX, vsock buffers and snapshot reset/event/used-ring writes) contribute to
one lifetime accumulator. Validated writable ranges are marked before lending
raw pointers, conservatively covering partial writes and failure. Upstream
[3777ba8d](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/3777ba8ded440cca2d953cd40e8b1ee43ae6c682)
fixes net readv coverage in newer code; this v51.1 adaptation also retains
candidates when the operation fails. It does not import unrelated net refactors.

`GET /api/v1/vm.dirty-memory` returns version 1, page_shift 12 and slot/GPA/size/
file_offset/bitmap records. Every consumed source is transferred to the lifetime
accumulator before another source or slot is read. A failed slot becomes wholly
dirty before returning an error; registration changes retain all known ranges.
Lost HTTP responses and retries never clear the accumulator. Migration start,
read and stop hooks cannot consume or disable checkpoint-owned logging: external
checkpoint RAM rejects live migration before destination I/O or failure cleanup.

`va_report` keeps its existing fields and FD, adding only optional `file_offset`.
ACK remains `{"type":"ack"}`; it confirms adoption, not logging capability.
There is no version ACK, WP negotiation, eager memory preparation or new kernel
baseline. New CH initializes its ledger independently. The host checks capability
with the first actual capture request: a complete valid response selects native
logging; a complete 404 selects working-set compatibility. Other errors fail the
capture. The result is fixed for that CH process; native logging never downgrades
after an error. API data geometry/version validation remains mandatory.

The compatibility baseline is v51.1 plus #379's 0001–0011, without 0012. It uses
the same lazy source and release-safe Data/Zero/Hole capture, with every explicit
working-set byte conservatively dirty. Frozen pre-#379 0004 binaries still have
the independently repaired release bug; 404 does not prove that fix is present.
The withdrawn #375 version-ACK protocol is not a supported interface target.
Restore validates the single ram0 shared, non-prefaulted, 4 KiB mapping, ordered
contiguous memfd offsets and empty CH memory-ranges before starting CH. This
retains the legacy offset-less report contract across GPA gaps on both CPUs.

Keep all future corrections to this ledger and its tests in 0012. Release order
belongs to 0004; HTTP allocator permissions belong to 0011. Generic raw-write
corrections should follow upstream; the lifetime API remains project-maintained
pending upstream interface agreement. Audit through upstream d1719ac8 found only
unrelated migration timeout additions since ad5f3eea; CH stays pinned to v51.1.

Validation includes native KVM slot-failure/retry and migration-owner rejection,
raw net/vsock writers, initialization, and the sandbox `snapshot.dirty-memory`
case's full-RAM comparisons, same-state size accounting and response loss. The
`snapshot.ch-compat` case uses a test-only CH built from the formal first eleven
patches via the existing native build recipe. On both native CI architectures it
checks legacy cold capture and four-entry native snapshots restored by that CH,
including one-time capability discovery, release and reuse. The helper records
its applied source tree, patch hashes and binary hash; it is not a product VMM.

## 4. Build workflow

`sandboxer/native-deps` builds the artifact from the pinned v51.1 tarball,
`git am deps/ch-patches/*.patch`, and `cargo build --release --locked`.
Its output is `native-deps/bin/<arch>/cloud-hypervisor`; running
`make cloud-hypervisor` from the sandboxer root also copies it to
`bin/<arch>/cloud-hypervisor`. Build duration depends on the toolchain, machine,
and caches.

The build uses `--remap-path-prefix` to map the source tree to relative paths and
Cargo dependencies to `/cargo`, rather than embedding those build-machine
absolute source paths in panic messages and DWARF.

See [native-deps/README.md](../native-deps/README.md) for `ch-fetch`,
`ch-patches-format`, idempotency checks, output synchronization, and ownership
boundaries.

## 5. Boot protocols by architecture

CH selects the architecture-specific boot protocol; sandbox-ctl does not require
a different command-line interface per architecture:

| Architecture | Protocol | Kernel entry | Prepared by CH |
| --- | --- | --- | --- |
| x86_64 | PVH | ELF entry with a CH-populated zero page | E820 memory map, command line, and ACPI tables |
| aarch64 | EFI stub + ACPI | PE Image entry; the EFI stub parses ACPI | UEFI memory map, ACPI tables, and GICv3 description |

### 5.1 Device model used by the platform

The cold-start device layout is:

```text
virtio-pmem    → sandbox-runtime.bundle (DAX, MAP_SHARED host page-cache sharing)
virtio-blk × 2 → blk0 (read-only base) + blk1 (writable overlay COW), vhost-user backend
virtio-net     → optional eth0 with host TAP backing; absent when no network source is configured
virtio-console → hvc0 for kernel dmesg; --console tty writes to CH stdout, an anonymous
                 pipe supplied by sandbox-ctl; --serial off disables the 8250 UART.
                 CH stdin is /dev/null, so CH does not raw-mode a host terminal.
                 Application stdio uses the vsock MUX, not this console.
virtio-vsock   → CID=3; short control connections (launch / ping / app_started /
                 app_exited / mem_report / quiesce / restore / attach), plus the
                 application stdio MUX upgraded from launch/restore/attach handshakes
virtio-balloon → size=<cold InitialTarget> [+ deflate_on_oom=on]; the sandbox-local
                 BalloonController sets targets with /vm.resize and observes current
                 memory_actual_size through vm.info; free_page_reporting is not enabled
virtio-mem     → host-driven unplug supported by CH; not enabled in the fixed-Capacity
                 Budget model used here
```

The two block devices above describe the root base/overlay pair; configured data
disks add their corresponding device slots. See [sandbox.md](sandbox.md) §9.3
for balloon control and [sandbox-init.md](sandbox-init.md) §4 for the stdio MUX.

Restore retains the topology from `config.json`; it cannot add or remove the
virtio-net device. Before starting CH, sandboxer checks that the presence of a
host network source matches NIC presence in the snapshot.

`--restore source_url=<state.json dir>` restores the saved device topology, so
`--kernel` and `--vsock` do not have to be repeated. Complete cold/restore data
flows are described in [sandbox.md](sandbox.md) §5 and §7.

### 5.2 Vsock hybrid proxy

The host-side hybrid proxy maps vsock traffic to UDS endpoints:

```text
guest VM (CID=3) → host CID=2 → CH forwards to:
  /run/sandbox/<sid>/vsock.sock_<port>       guest → host; host listens on this UDS
  /run/sandbox/<sid>/vsock.sock + "CONNECT <port>\n"
                                           host → guest; guest listens on the port
```

For host-to-guest traffic, the first write is ASCII `CONNECT <port>\n`. CH returns
`OK <local_port>\n`, which the host must consume before reading subsequent
payload, then proxies to the guest listener. Both directions are ordinary byte
streams to CH. The `launch`, `restore`, and `attach` connections become framed
stdio MUX streams only after the application handshake between sandbox-ctl and
sandbox-init; CH does not interpret those frames. See [sandbox.md](sandbox.md)
§5.2 and [sandbox-init.md](sandbox-init.md) §4.2.

## 6. Behavioral contract summary

Platform code (`sandbox-ctl` and `node-ctl`) relies on the following pinned CH
behavior, including the patches:

| Behavior | Upstream | Patched |
| --- | --- | --- |
| Memory backing without `fd=` | Managed memfd/file backing | Same allocation path; not a statement that all other patches are disabled |
| Explicit `fd=` | Not accepted by the unpatched parser | Map the inherited backing and mark the zone user-managed |
| `fd=` plus `uffd_socket=` | Not available | Create uffd, send it with metadata, and wait for ACK |
| `/vm.snapshot` of a user-managed zone | Not applicable | Omit the zone from memory-ranges and skip the RAM dump |
| `/vm.restore` of a user-managed zone | Not applicable | Skip fill; the mapping faults into the external handler when configured |
| Balloon release of a hole-only file-backed run | Ordinary release path | Always notify REMOVE; skip only redundant `PUNCH_HOLE` |
| Balloon `deflate_on_oom=on` | Supported in v51.1 | Supported |
| `vm.resize` `desired_balloon` | Supported | Used by the sandbox-local BalloonController |
| virtio-mem `vm.resize` | Supported | Supported |
| Vsock local-port cursor across restore | Not persisted by this upstream version | `VsockState.local_port_last` |
| Matching backend/guest vsock transport epochs | Not provided by this upstream restore path | Reset event plus RX acknowledgement gate |

The platform does not use `free_page_reporting`. In the unified memfd/external
uffd model, its repeated `madvise(MADV_DONTNEED)` invalidations can create
mmu_notifier/EPT and IPI-shootdown pressure and interfere with guest vsock progress.
The rationale and replacement feedback loop are described in the
[guest kernel specification §5.5](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/vmlinux.md#55-why-free_page_reporting-is-not-negotiated-and-the-role-of-virtio_mem). The sandbox-local BalloonController instead pushes inflate targets through
`/vm.resize`, with at most one 64 MiB steady-shrink step for each fresh report.
Patch 0004 preserves REMOVE for empty cold-start ranges as well as resident
runtime reclaim. Cold-created VMs on 4 KiB hosts combine adjacent PFNs to reduce synchronous
REMOVE processing (§3.4).
`deflate_on_oom` is native to upstream v51.1 and needs no additional patch.

## 7. Known limitations

- **Upstream upgrades, including v52 and later**: inspect actual
  `vmm/src/memory_manager.rs` changes before rebasing 0001/0002. Patch 0003's uffd
  ioctls require particular review. Patch 0004 changes the balloon release
  function and its seccomp allowlist. Review 0005/0006 with upstream vsock queue
  and restore-lifecycle changes, and 0007 with any sound upstream
  `immediate_exit` implementation. No fixed rebase cost is promised.
- **vCPU signal-mask overhead**: the KVM vCPU userspace-exit loop still performs
  an idempotent signal block. Unblock/reblock occurs only at a safe point after
  observing a lifecycle request. CPU-bound guests gain no new VM exit from this
  mechanism, but PIO/MMIO-heavy workloads incur additional syscall work. This is
  the no-miss path until a suitable `immediate_exit` interface can replace it.
- **Multiple fd-backed zones**: the platform currently uses one zone and one
  memfd for all sandbox RAM. Multiple zones for NUMA or virtio-mem expansion
  require corresponding per-zone handoffs in patch 0003 and address maps in
  sandbox-ctl; they are not implied by the current single-zone integration.

## 8. See also

- [sandbox.md](sandbox.md) §5, §5.2, and §7 — cold start, CH commands, and restore.
- [sandbox.md](sandbox.md) §8 — handling faults after receiving `uffd_C`.
- [Guest kernel](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/vmlinux.md)
  — PVH/EFI-stub integration.
- [Native build](../native-deps/README.md) — Cloud Hypervisor build and patch cycle.
- [System overview §3.2](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/kuasar-sandbox.md#32-component-responsibilities) — VMM and guest environment within the system.
