[English](cloud-hypervisor.md) | [简体中文](cloud-hypervisor_zh.md)

<a id="cloud-hypervisor--vmm-与平台-patches"></a>
# cloud-hypervisor — VMM and platform patches

The platform uses Cloud Hypervisor (CH) as its microVM monitor. Most paths use
upstream behavior. Externally managed memory, restore-safe vsock, and reliable VM
lifecycle barriers are supplied by seven patches maintained in this repository.
This document defines their scope, build procedure, and behavioral contracts.

<a id="1-概述"></a>
## 1. Overview

<a id="11-为什么需要-patch"></a>
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
model. This memory model also requires avoiding an `EVENT_REMOVE` storm when
balloon release encounters ranges that have never been resident (patch 0004,
§3.4).

<a id="12-patch-范围总结"></a>
### 1.2 Patch scope summary

The following line counts describe the reviewed patch set at sandboxer commit
`c29f9af875c595df68491b50e872007e9285ef48`; they are revision-specific estimates,
not a permanent size or compatibility guarantee.

| File | Approximate changed lines | Purpose |
| --- | --- | --- |
| `vmm/src/vm_config.rs` | ~19 | Add `fd` and `uffd_socket` to `MemoryZoneConfig` |
| `vmm/src/config.rs` | ~9 | Parse the two additional `MemoryConfig::parse` keys |
| `vmm/src/memory_manager.rs` | ~338 | Inherited backing fd, user-managed skip, in-process uffd creation, `va_report` with `SCM_RIGHTS`, and `UFFDIO_REGISTER` |
| `vmm/src/seccomp_filters.rs` | ~17 | Allow `userfaultfd`, `UFFDIO_API`, and `UFFDIO_REGISTER` |
| `virtio-devices/src/balloon.rs` | ~38 | Probe with `SEEK_DATA`; skip `PUNCH_HOLE` and `MADV_DONTNEED` for hole-only file-backed runs |
| `virtio-devices/src/seccomp_filters.rs` | ~8 | Allow the balloon thread's `SYS_lseek` probe |
| `virtio-devices/src/vsock/unix/muxer.rs` | ~20 | Persist the host local-port allocation cursor |
| `virtio-devices/src/vsock/device.rs` / `mod.rs` | ~330 | Stage transport reset at snapshot, reassert its IRQ at restore, and gate RX until guest acknowledgement |
| `hypervisor/src/cpu.rs` / `kvm/mod.rs` | ~110 | No-miss vCPU kicks with `KVM_SET_SIGNAL_MASK` and a blocked userspace signal mask |
| `vmm/src/cpu.rs` / `seccomp_filters.rs` | ~550 | Consume kicks at lifecycle safe points, per-request ACKs/deadlines, and KVM ioctl allowlisting |
| `virtio-devices/src/device.rs` / `epoll_helper.rs` / net / vhost-user | ~280 | Publish/wake pause events and provide a two-way resume barrier, including custom workers |

The seven patch files contain 1,665 insertions and 136 deletions in total and
apply to Cloud Hypervisor `v51.1`.

<a id="13-维护策略"></a>
### 1.3 Maintenance strategy

- The repository-relative patch directory is
  `native-deps/deps/ch-patches/000{1,2,3,4,5,6,7}-*.patch`.
- Run `make ch-patches-apply` from `sandboxer/native-deps`; it is also part of a
  fresh `make cloud-hypervisor` build. The development cycle and idempotency
  checks are described in [native-deps/README.md](../native-deps/README.md) §3.
- Review the patch set for each intended upstream upgrade. Resolve conflicts
  against the actual upstream changes; neither a fixed release cadence nor a
  fixed amount of rebase work is assumed.
- The external RAM/UFFD design uses in-process creation, `SCM_RIGHTS`, and ioctls
  in `create_ram_region`, and remains a project-maintained patch set. When an
  equivalent upstream vCPU-kick or worker-barrier fix is available, prefer it
  when upgrading rather than indefinitely maintaining duplicate patch 0007 logic.

<a id="2-启用条件与命令行"></a>
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

<a id="3-patch-提交结构"></a>
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

Changed files: `memory_manager.rs` and `seccomp_filters.rs`.

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

<a id="34-0004--balloon-release-跳过-user-managed-zone-的空洞-run"></a>
### 3.4 0004 — skip hole-only runs during balloon release

Subject: **virtio-devices: balloon — skip PUNCH_HOLE/MADV_DONTNEED on already-sparse
user-managed ranges**.

Changed files: `virtio-devices/src/balloon.rs` and
`virtio-devices/src/seccomp_filters.rs`.

**Background**: balloon allocation in the configured guest does not request
`__GFP_ZERO`, and the platform guest configuration does not enable `init_on_alloc`.
The inflate path updates page metadata and PFN arrays rather than zeroing every
returned page. The externally managed zone is not prefaulted. Offsets never
previously touched by the guest can therefore still be holes in the memfd, with
no inode page or PTE in either process. This does not mean every balloon page at
cold start is necessarily a hole: the guest can have touched and released pages
before handing them to the balloon.

Without the probe, CH calls `release_memory_range` for each returned run:
`fallocate(PUNCH_HOLE|KEEP_SIZE)` on the memfd plus `madvise(MADV_DONTNEED)` on chVA.
In the documented uffd model, `MADV_DONTNEED` produces an `EVENT_REMOVE` even for an
already empty range and waits for the external handler to consume it. Inflating
across sparse `Capacity − InitialBudget` memory can thus create unnecessary
serialized work and back-pressure the balloon thread. The chVA `MADV_DONTNEED`,
not merely the memfd `fallocate`, is the source of this handshake, so avoiding it
requires skipping both operations for an empty run.

**Mechanism**:

- For a region with a file offset, `release_memory_range` probes
  `[file_off, file_off + len)` with `lseek(fd, file_off, SEEK_DATA)`:
  - `ENXIO`, or a next-data offset at or beyond `file_off + len`, means the whole
    run is a hole. Skip both `fallocate` and `madvise` and return `Ok(())`.
  - Data within the run retains the ordinary `PUNCH_HOLE` plus
    `MADV_DONTNEED` release of the complete run.
  - Other `lseek` errors fall through to the ordinary release path; they are not
    interpreted as proof that the range is empty.
- The actual predicate is `region.file_offset().is_some()`, not the
  `user_managed` flag. The probe can also apply to other file-backed regions.
  Regions without a file offset retain the original path.
- In the platform memfd use, CH uses mappings and explicitly positioned
  `fallocate`, not reads/writes dependent on the shared file position. Moving the
  position for the probe therefore does not change that data path.
- **Seccomp**: the balloon thread adds `SYS_lseek` to its allowlist. `fallocate`
  was already allowed and `madvise` comes from the virtio common set. Without this
  addition, the first probe could terminate the thread with `SIGSYS`.

**Correctness**: only a run found to contain no data is skipped. A resident run
that actually requires reclaiming retains the release operations. For the
untouched external-memory holes described above, the suppressed backendVA
reclaim would itself be a no-op, so the final host-memory state is unchanged.

**Effect and tradeoffs**: on the x86_64 4 KiB path, where `pbp` batching is bypassed,
releasing an entirely sparse balloon interval without the probe can entail about
`inflated bytes / 4 KiB` synchronous handler handshakes. The probe substitutes a
local `lseek` for those unnecessary release operations. Pages still resident from
previous guest activity are not skipped. Guest-exit unmapping and its events are
separate from inflation. On this path, probing can still cost one `lseek` per
page; it avoids the corresponding empty-range `madvise`, handler handshake, and
shared-inode invalidation work. The hole fraction and convergence time depend on
the workload and environment; this specification makes no universal 99% hole-rate
or instantaneous-convergence claim.

<a id="35-0005--持久化-vsock-host-local-port-游标"></a>
### 3.5 0005 — persist the vsock host local-port cursor

Restore recreates CH's Unix vsock backend. Resetting its local-port allocator to
`0x40000000` can make the first host-initiated connection reuse a tuple retained
in the guest snapshot and receive an RST. This patch stores `local_port_last` in
`VsockState` and continues from the next port after restoring the backend. Nested
snapshots preserve the cursor at each layer rather than relying on a process-local
cursor that only covers one restore.

<a id="36-0006--snapshot-时预发布-vsock-transport-reset"></a>
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

<a id="37-0007--vm-pauseresume-与-ordered-shutdown-可靠性"></a>
### 3.7 0007 — reliable VM pause/resume and ordered shutdown

CH v51.1 uses a no-op `SIGRTMIN` handler to interrupt `KVM_RUN`. The control thread
publishes pause/kill state, calls `pthread_kill` for each vCPU, and waits for loop
acknowledgement. A signal arriving in userspace after the state check but before
entry to `KVM_RUN` can be consumed too early, leaving that vCPU blocked in the
next call. Repeating it every 10 ms reduces probability but does not close the
race.

The patch closes the lifecycle-barrier races through three related changes:

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
3. Runtime pause publishes the shared pause event before explicitly unparking
   ordinary and custom workers, closing the startup race between a zero-time
   epoll poll and `thread::park`. Resume drains the event before waking workers.
   After returning from park, workers participate in a second barrier; the
   controller waits for resume ACKs before another pause. Custom net and
   vhost-user worker handles participate in both the pause wake and resume
   barrier. A restored device that has not received a runtime pause event does
   not wait for a nonexistent ACK.

This patch does not change sandbox configuration, the CH HTTP API, snapshot
format, or resource protocol, and does not adjust `cpu.max` around lifecycle
operations. Timeouts remain bounded failure protection, not the race fix.

<a id="4-构建工作流"></a>
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

<a id="5-启动协议per-arch"></a>
## 5. Boot protocols by architecture

CH selects the architecture-specific boot protocol; sandbox-ctl does not require
a different command-line interface per architecture:

| Architecture | Protocol | Kernel entry | Prepared by CH |
| --- | --- | --- | --- |
| x86_64 | PVH | ELF entry with a CH-populated zero page | E820 memory map, command line, and ACPI tables |
| aarch64 | EFI stub + ACPI | PE Image entry; the EFI stub parses ACPI | UEFI memory map, ACPI tables, and GICv3 description |

<a id="51-设备模型平台用法"></a>
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

<a id="52-vsock-hybrid-代理"></a>
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

<a id="6-行为契约总结"></a>
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
| Balloon release of a hole-only file-backed run | Ordinary release path | Skip `PUNCH_HOLE` and `madvise`; resident runs retain ordinary release |
| Balloon `deflate_on_oom=on` | Supported in v51.1 | Supported |
| `vm.resize` `desired_balloon` | Supported | Used by the sandbox-local BalloonController |
| virtio-mem `vm.resize` | Supported | Supported |
| Vsock local-port cursor across restore | Not persisted by this upstream version | `VsockState.local_port_last` |
| Matching backend/guest vsock transport epochs | Not provided by this upstream restore path | Reset event plus RX acknowledgement gate |

The platform does not use `free_page_reporting`. In the unified memfd/external
uffd model, its repeated `madvise(MADV_DONTNEED)` invalidations can create
mmu_notifier/EPT and IPI-shootdown pressure and interfere with guest vsock progress.
The rationale and replacement feedback loop are described in the
[guest kernel specification](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/vmlinux.md)
§5.5. The sandbox-local BalloonController instead pushes inflate targets through
`/vm.resize`, with at most one 64 MiB steady-shrink step for each fresh report.
Patch 0004 skips empty ranges covered by the cold-start target; runtime reclaim
of resident pages still performs the complete release operations.
`deflate_on_oom` is native to upstream v51.1 and needs no additional patch.

<a id="7-已知限制"></a>
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
- [System overview](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/kuasar-sandbox.md)
  §2.4 — VMM and guest environment within the system.
