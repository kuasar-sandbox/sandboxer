# cloud-hypervisor — VMM 与平台 patches

平台用 cloud-hypervisor(CH)作为 microVM 监视器。绝大多数路径跑 upstream
行为,外部托管内存和 restore-safe vsock 通过本仓维护的 6 个 patch 实现。
本文档定义 patch 范围、构建方式及对应行为契约。

## 1. 概述

### 1.1 为什么需要 patch

平台对 VMM 有三条非 upstream 的诉求:

1. **外部托管 sandbox RAM**:host 进程(sandbox-ctl)持有 memfd inode,CH 通过
   继承 fd 把同一 inode mmap 到自己地址空间——而不是 CH 自己 `memfd_create`。
   这样 sandbox-ctl 拥有内存所有权,快照时直接通过 SEEK_DATA/HOLE 扫驻留页,
   绕过 CH→file→sandbox-ctl 的中转
2. **外部 uffd handler**:sandbox-ctl 接管 sandbox RAM 缺页,实现按需从快照
   或零页填充。CH 自己创建 uffd(必须绑到 CH mm),通过 SCM_RIGHTS 把 fd 传
   给 sandbox-ctl 的 handler
3. **restore-safe vsock**:CH restore 会重建空的 Unix backend,guest 却保留
   已连接 socket。快照时必须向 guest event queue 预发布 virtio-vsock transport
   reset,恢复时重发该事件的 IRQ,并在 guest 确认清理旧连接前阻止新 backend RX
   包进入

这些能力 upstream CH 都不直接支持。第二条尤其本质——upstream uffd handler 模型
是 CH 调外部 socket,handler 提供数据,但平台需要 handler 接管 fault 投递
路由,upstream 的 listener 模型不够。此外这一模型带来一条派生修复:balloon
释放路径对从未驻留的空洞 run 不能合成 `EVENT_REMOVE` 风暴(patch 0004,§3.4)。

### 1.2 patch 范围(总结)

| 文件 | 改动行数 | 内容 |
|------|----------|------|
| `vmm/src/vm_config.rs` | ~19 | `MemoryZoneConfig` 新增 `fd` / `uffd_socket` 字段 |
| `vmm/src/config.rs` | ~9 | `MemoryConfig::parse` 增加两个 key 解析 |
| `vmm/src/memory_manager.rs` | ~338 | fd 注入 + user_managed skip + create_ram_region 内创建 uffd + va_report sendmsg(SCM_RIGHTS) + UFFDIO_REGISTER |
| `vmm/src/seccomp_filters.rs` | ~17 | allowlist `userfaultfd` syscall + `UFFDIO_API` / `UFFDIO_REGISTER` ioctl |
| `virtio-devices/src/balloon.rs` | ~38 | balloon release 对 user-managed zone 的空洞 run 跳过 `PUNCH_HOLE`/`MADV_DONTNEED`(`SEEK_DATA` 探测)|
| `virtio-devices/src/seccomp_filters.rs` | ~8 | balloon 线程 seccomp 放行 `SYS_lseek`(skip-hole 探测所需)|
| `virtio-devices/src/vsock/unix/muxer.rs` | ~20 | 持久化 host local-port 分配游标 |
| `virtio-devices/src/vsock/device.rs` / `mod.rs` | ~330 | snapshot 时发布 transport reset,restore 重发 IRQ,guest 确认前 gate RX |

总计约 790 行 Rust、6 个 commit,基于 cloud-hypervisor `v51.1`。

### 1.3 维护策略

- patch 文件位置:`deps/ch-patches/000{1,2,3,4,5,6}-*.patch`
- 应用方式:`make ch-patches-apply`(在 `make cloud-hypervisor` 内自动调);
  开发循环与幂等 sanity 语义见 `sandboxer/native-deps/README.md` §3
- 跟 upstream rebase:每个 CH 大版本(~3 月)review 一次,几行 conflict
  人工 fix
- **不**尝试上游化:patch 设计选择(SCM_RIGHTS in-process + create_ram_region
  里跑 ioctl)与 upstream 风格偏差明显,本项目维持自有 fork

## 2. 启用条件与命令行

CH `--memory-zone` 增加两个 key:

| key | 类型 | 含义 |
|-----|------|------|
| `fd` | int | 由父进程通过 cmd.ExtraFiles 注入的文件描述符,用作 zone 的 backing |
| `uffd_socket` | path | 普通 UDS 路径,CH 在该 socket 上 sendmsg(va_report; SCM_RIGHTS=uffd_C) |

行为契约:

- **不写 fd**(也不写 uffd_socket):完全等同 upstream 行为(memfd_create 自管 RAM,
  无 uffd)。所有不依赖外部托管的调用者(测试用例、其他工具)无须改动
- **仅写 fd**:CH 把 fd 当 backing,跳过 memfd_create;snapshot 时跳过 dump;
  restore 时不 fill。这条独立成立(无 uffd 仍可外部托管)
- **fd + uffd_socket 同时写**:在 fd 行为基础上叠加,CH 内 `userfaultfd()`
  → `UFFDIO_API` → `UFFDIO_REGISTER MISSING on chVA` → `connect uffd_socket`
  → `sendmsg(va_report; SCM_RIGHTS=uffd_fd)` → 等 ack。ack 之前 vCPU 不允许
  跑

平台 sandbox-ctl 调用形式(冷启动 + 恢复完全相同):

```
cloud-hypervisor \
  --memory-zone size=8G,shared=on,fd=3,uffd_socket=/run/sandbox/<sid>/uffd.sock \
  ...
# fd=3 ← memfd from sandbox-ctl via cmd.ExtraFiles[0]
```

详细命令行(冷启动 / 恢复)见 `sandboxer/docs/sandbox.md` §5.2 与 §7。

## 3. patch 提交结构

### 3.1 0001 — externally allocated memfd-backed memory zone

主题:**memory: support externally allocated memfd-backed memory zone**

改动文件:`vm_config.rs` / `config.rs` / `memory_manager.rs`

要点:

- `MemoryZoneConfig` 新增 `pub fd: Option<i32>`(序列化时 `serde(skip)`,因为
  fd 不能跨 serialize)
- `MemoryConfig::parse` 识别 `fd=N` 形式,调 `File::from_raw_fd(N)`,**不**
  关闭原 fd(由调用方 ExtraFiles 管理生命周期)
- `MemoryManager::new` / `create_ram_region` 看到 `zone.fd.is_some()`:
  - 跳过 `memfd_create`,直接 `mmap(NULL, size, PROT_RW, MAP_SHARED, fd, 0)`
  - 在 `MemoryZone` 上标记 `user_managed: true`
- 多 region per zone(x86_64 zone > 3 GiB 时跨 PCI hole 切两段):
  每 region `try_clone()` 一份 fd,各自 `File` ownership;mmap 对同一 inode
  产生多个 VMA,共享 host page cache

`Transportable::send` 与 `fill_saved_regions` 都遍历 `snapshot_memory_ranges`
表,**这两处不需要单独 patch**——靠 commit 0002 在 `memory_range_table` 处过滤
即可同时影响快照和恢复路径。

### 3.2 0002 — snapshot skip user-managed memory zones

主题:**snapshot: skip user-managed memory zones**

改动文件:`memory_manager.rs`

要点:

- `memory_range_table` 在生成 `snapshot_memory_ranges` 时检测每个 zone 的
  `user_managed` 标志,user_managed=true 的 zone 在表中**不出现**
- 这样 `Transportable::send`(快照 dump 路径)看到的 ranges 不含 user-managed
  zone,自然 `Ok(())` 即返回——CH **不**写 8 GiB memory-ranges 文件
- 同理 restore 路径 `fill_saved_regions` 看到的 ranges 表不含此 zone,
  fill 操作变成空操作——guest 物理地址通过 mmap 直接映到 sandbox-ctl 的 memfd
  inode,内容由 sandbox-ctl 的 uffd handler 按需提供

upstream 已有"file-backed + MAP_SHARED + hardlink"的等价 skip 分支(用于
share-storage live migration 场景)。我们的 user_managed zone 满足"file-backed
+ MAP_SHARED",但 memfd 没有 hardlink(`st_nlink == 0`),所以不能复用现有
分支——添加平行分支识别 `user_managed` 标志。

### 3.3 0003 — external uffd handler via in-process create + SCM_RIGHTS

主题:**memory: external uffd handler via in-process create + SCM_RIGHTS handoff**

改动文件:`memory_manager.rs` / `seccomp_filters.rs`

要点:

- `create_ram_region` 在 mmap 完成后,如果 zone 配了 `uffd_socket`:

  ```
  uffd = userfaultfd(O_CLOEXEC | O_NONBLOCK)
  UFFDIO_API features = UFFD_FEATURE_MISSING_SHMEM
                      | UFFD_FEATURE_EVENT_REMOVE
                      | UFFD_FEATURE_EVENT_UNMAP
                      | UFFD_FEATURE_THREAD_ID
  UFFDIO_REGISTER(uffd, [chVA, +size], MISSING)
  conn = connect(uffd_socket)
  sendmsg(conn, iov=va_report{chVA, size}, cmsg=SCM_RIGHTS([uffd]))
  recvmsg(conn, expect ack)
  // 回到 create_ram_region 主流程,继续 vCPU 启动
  ```

- feature bits **必须严格按内核头位置**:

  ```
  UFFD_FEATURE_MISSING_SHMEM = 1 << 5
  UFFD_FEATURE_EVENT_REMOVE  = 1 << 3
  UFFD_FEATURE_EVENT_UNMAP   = 1 << 6
  UFFD_FEATURE_THREAD_ID     = 1 << 8
  ```

  位置写错会被内核解读成别的 feature。例如 `1 << 1` 是 `EVENT_FORK`,要求
  `CAP_SYS_PTRACE`,导致 UFFDIO_API 返回 EPERM。

- seccomp 放行:`SYS_userfaultfd` syscall + `UFFDIO_API` / `UFFDIO_REGISTER`
  ioctl(filter 在 `seccomp_filters.rs`)

**为什么 uffd 必须 CH 内创建**:Linux 内核把 uffd 上下文绑到 `userfaultfd()`
调用进程的 mm。sandbox-ctl 创建的 uffd 上 `UFFDIO_REGISTER` 只能 register
sandbox-ctl mm 的 VMA,不能 register CH mm 的 chVA。所以 uffd 必须在 CH 内
创建,然后通过 SCM_RIGHTS 把 fd(而非内核内部对象)传给 sandbox-ctl 让它读
事件。fd 表是进程级的,但 uffd ctx 的事件路由按创建者 mm 的 VA 解析,
sandbox-ctl 收到 fd 后读出来的事件 va 就是 CH 视角的 chVA。

### 3.4 0004 — balloon release 跳过 user-managed zone 的空洞 run

主题:**virtio-devices: balloon — skip PUNCH_HOLE/MADV_DONTNEED on already-sparse
user-managed ranges**

改动文件:`virtio-devices/src/balloon.rs` / `virtio-devices/src/seccomp_filters.rs`

**背景**。guest balloon 驱动充气分配的页**从不被 guest 写入**:
`balloon_page_alloc` 不带 `__GFP_ZERO`,平台 guest 内核未启用 `init_on_alloc`,
fill 路径只动 struct page 元数据与 PFN 数组。又因 user-managed zone 从不
prefault(memfd 稀疏,内容由 uffd handler 按需填),balloon 让出的 offset
绝大多数(冷启动时**全部**)在 memfd 上本就是空洞——无 inode 页,CH 与
sandbox-ctl 都无 PTE。

但 CH 的 balloon inflate 处理对每个让出 run 仍调 `release_memory_range`:
`fallocate(PUNCH_HOLE|KEEP_SIZE)` on memfd + `madvise(MADV_DONTNEED)` on chVA。
后者落在 uffd 注册的 chVA 上,内核**无条件**(与该 run 是否驻留无关)合成
一条 `EVENT_REMOVE`,且 `MADV_DONTNEED` **同步阻塞**到外部 handler 消费完
该事件才返回。冷启动充气覆盖整个 `capacity − allocatable` 区间时,这是一场
"对从未存在的页"的空 `EVENT_REMOVE` 风暴:压垮 handler 单 reader,并反压
CH 的 balloon 线程。`EVENT_REMOVE` 的真正来源是 chVA 上的 `MADV_DONTNEED`
(不是 memfd 的 fallocate)——所以**必须同时跳过两者**才能不发事件。

**要点**:

- `release_memory_range` 对**有 file_offset 的 region**(即 user-managed
  fd-backed zone)在动作前,用一次 `lseek(fd, file_off, SEEK_DATA)` 探测目标
  `[file_off, file_off + len)`:
  - `ENXIO`,或返回的下一数据字节偏移 `≥ file_off + len` ⇒ 整段空洞 ⇒
    **跳过 fallocate 与 madvise,直接 `Ok(())`**
  - 段内有数据 ⇒ 维持原逻辑(`PUNCH_HOLE` + `MADV_DONTNEED` 覆盖整 run)
  - `lseek` 其他错误不吞:落回原路径,不掩盖
- 仅作用于 `region.file_offset().is_some()` 的 zone。无 fd 的 upstream 匿名
  zone 无可探测,**行为完全不变**(契约同 §6:不写 `fd=` 等同 upstream)
- 该 zone 的 memfd fd 在 CH 内仅经 mmap + `fallocate`(显式 offset)使用,
  无定位读写,故 `lseek` 移动文件位置无副作用
- **seccomp 放行**:balloon device 线程的 seccomp 仅允许 `fallocate`
  (madvise 来自 virtio 公共集),新增 `SYS_lseek`;否则首次探测即 `SIGSYS`
  杀死 balloon 线程,CH 退出(filter 在 `virtio-devices/src/seccomp_filters.rs`,
  与 §3.3 放行 `userfaultfd` 同理)

**正确性**:跳过只命中真空洞(无可释放物);任何**确需回收**的页必有数据,
永不被跳过。被抑制的 `EVENT_REMOVE` 本只驱动 handler 对 backendVA 的
process-level reclaim,而空洞 offset sandbox-ctl 也从未 fault → 那一步本就
是 no-op。跳过前后 host 内存终态完全一致。

**效果与取舍**:改动前 balloon 充满 `capacity − allocatable` 时,**每个 4K
页**(`pbp` 在 x86-4K 被旁路)都对 uffd VMA 做 `MADV_DONTNEED`,而该调用
**同步阻塞**到外部单 reader handler 消费完 `EVENT_REMOVE` 才返回——
`≈ 充气字节 / 4K` 次串行跨进程往返,正是数十秒收敛(及偶发 boot 软死锁)
的根因。改动后冷启动充气页**实测 ~99% 是从未触碰的空洞**,`lseek(SEEK_DATA)`
探测后整段跳过 `PUNCH_HOLE` 与 `MADV_DONTNEED`:无 madvise → 无同步握手 →
balloon 线程以内存速度扫过 → **收敛近乎瞬时**。剩 ~1% 是 guest 启动期经 vhost-blk / 内核进过 page
cache 又释放、folio 仍驻留 memfd 的页,`lseek` 见数据**不跳过**,照常回收
——有界合法,行为同 upstream。guest 退出时整 zone unmap 产生的大
`EVENT_REMOVE` 与本 patch 无关、不计入充气阶段。x86-4K 下空洞探测退化为每
页一次 `lseek`——纯 in-kernel xarray 走查,无事件 / 无 handler 握手 / 无
共享 inode madvise 争用,本地廉价。

### 3.5 0005 — 持久化 vsock host local-port 游标

CH 的 Unix vsock backend 在 restore 时重新创建。若 host local-port 分配器回到
`0x40000000`,首个 host-init 连接可能复用 guest 快照里仍存在的四元组并收到 RST。
patch 把 `local_port_last` 纳入 `VsockState`,恢复 backend 后从下一端口继续分配。
该状态在嵌套快照中逐层保存,不是只覆盖一次 restore 的进程内游标。

### 3.6 0006 — snapshot 时预发布 vsock transport reset

仅避免端口复用仍不完整:CH 不序列化 backend connection map,而 guest 内核会把
连接 socket、credit 与关闭状态一并带入快照。restore 后两端因此处于不同 transport
epoch。restore 的 guest RAM 又由外部 userfaultfd 惰性恢复,设备 `activate` 阶段读取
event virtqueue 页可能只看到尚未 fault-in 的空 avail ring,不能在这里要求新 descriptor。
patch 执行以下协议:

1. VM 已暂停后,`Vsock::snapshot` 从**源 VM 的 live event virtqueue**取一个 guest
   提供的 writable descriptor,写入 `VIRTIO_VSOCK_EVENT_TRANSPORT_RESET`,推进 used
   ring 并置 `transport_reset_pending`。普通 `/vm.pause` 不发布 reset。
2. pending 状态随 `VsockState` 持久化。源 VM resume 与目标设备 restore activation
   都只重发该 used-ring event 的 IRQ;restore 不再读取或消费 event descriptor。
3. pending 期间 backend 可接收 host UDS,但所有 RX packet 都留在 backend,不能进入
   guest RX queue。
4. Linux guest 处理 reset:关闭 connected sockets、重新读取 CID,listener 保持
   bind/listen;随后补回 event descriptor 并 kick event queue。CH 把该 kick 作为
   acknowledgement,解除 RX gate 并立即排空 pending RX。

snapshot staging 会先排空 reset 之前已经到达的 eventfd kick。设备线程 resume 后若
同一批 epoll 仍带着该旧通知,非阻塞 read 返回 `EAGAIN` 并保持 gate,不会把旧 kick
误当成新 reset 的 acknowledgement。

因此首个 `restore` 控制连接可以在 VM resume 后立即发起,但其 REQUEST 必定在 guest
完成旧连接清理后才可见。这里没有 retry、sleep 或放宽 deadline。源 VM snapshot
阶段若 descriptor 缺失或非法会明确失败;目标 restore activation 即使没有任何新
available descriptor 也必须成功,因为 reset 已经存在于快照的 used ring 中。

## 4. 构建工作流

产物由 `sandboxer/native-deps` 构建:`make cloud-hypervisor` = 取 pin 的 v51.1 tarball +
`git am deps/ch-patches/*.patch` + `cargo build --release --locked`,冷构建
~5-10 min、热(cargo 缓存)秒级,产物 `bin/<arch>/cloud-hypervisor`。构建以
`--remap-path-prefix` 把源树与 registry 依赖映射为相对路径 / `/cargo` 前缀,
panic 消息与 DWARF 不泄漏构建机绝对路径。

patch 开发循环(`ch-fetch` / `ch-patches-format`、`patches-apply` 的幂等
sanity 检查)、产物同步和边界说明见 `sandboxer/native-deps/README.md`。

## 5. 启动协议(per-arch)

CH 自适应启动协议,sandbox-ctl 命令行不区分 arch:

| arch | 协议 | kernel 入口 | CH 准备 |
|------|------|-------------|---------|
| x86_64 | PVH | ELF entry,zero page 由 CH 填 | memmap(E820) + cmdline + ACPI 表 |
| aarch64 | EFI stub + ACPI | PE Image start,EFI stub 解析 ACPI | UEFI memmap + ACPI 表 + GICv3 节点 |

### 5.1 设备模型(平台用法)

CH 暴露给 guest 的设备清单(冷启动):

```
virtio-pmem    → sandbox-runtime.erofs (DAX, MAP_SHARED 共享 host page cache)
virtio-blk × 2 → blk0 (base, ro) + blk1 (overlay COW, rw),vhost-user backend
virtio-net     → 可选;配置网络源时为 eth0,host TAP 后端;无源时不创建设备
virtio-console → hvc0,内核 dmesg;--console tty(写到 CH 进程的 stdout = sandbox-ctl
                 给的匿名管道),--serial off(无 8250 UART)。CH 进程的 stdin=/dev/null
                 故 CH 不 raw 化任何宿主终端。应用 stdio 不走此设备(走 vsock MUX)
virtio-vsock   → CID=3。控制面短连接(launch / ping / app_started / app_exited /
                 mem_report / quiesce / restore / attach)+ launch/restore/attach 那条
                 连接握手后升级而成的应用 stdio MUX(详见 sandbox-init.md §4)
virtio-balloon → size=0 [+ deflate_on_oom=on];host BalloonController 通过
                 /vm.resize 推 target(见 `sandboxer/docs/sandbox.md` §9.3);free_page_reporting
                 不启用(广播 mmu_notifier 会饿死 guest vsock kthread)
virtio-mem     → host-driven 主动 unplug(扩展点)
```

restore 沿用 `config.json` 中的设备拓扑,不能新增或删除 virtio-net.sandboxer 在
启动 CH 前要求 host restore 配置是否提供网络源与快照中的 NIC 是否存在一致.

恢复路径设备拓扑通过 `--restore source_url=<state.json dir>` 从 snapshot
state 还原,不需要重新指定 `--kernel` / `--vsock`。

详细命令行示例与冷启动/恢复差异见 `sandboxer/docs/sandbox.md` §5(冷启动
数据流)与 §7(恢复数据流)。

### 5.2 vsock hybrid 代理

vsock 在 host 端通过 hybrid 代理映射到 UDS:

```
guest VM (CID=3) → CID=2 (host) → CH 把流量转发到
  /run/sandbox/<sid>/vsock.sock_<port>     guest → host 方向(host 在该 UDS 上 listen)
  /run/sandbox/<sid>/vsock.sock + "CONNECT <port>\n" 行    host → guest 方向(guest 在 port 上 listen)
```

host → guest 方向需要在第一笔写入发 ASCII `CONNECT <port>\n`,CH 回一行
`OK <local_port>\n`(host 须先排空再读后续 payload),之后 CH 把流量代理到 guest
对应 port 的 listener。两个方向的连接对 CH 而言都是普通字节流——`launch` /
`restore` / `attach` 这三种连接在应用层握手后由 sandbox-ctl / sandbox-init 自行
转入帧收发态(stdio MUX),CH 不感知。详细见 `sandboxer/docs/sandbox.md` §5.2 与
`sandboxer/docs/sandbox-init.md` §4.2。

## 6. 行为契约总结

平台代码(sandbox-ctl + node-ctl)依赖的 CH 行为(包含 patch):

| 行为 | upstream | patched |
|------|----------|---------|
| 命令行不写 `fd=` | ✓(memfd_create 自管 RAM) | ✓(同 upstream)|
| 命令行写 `fd=` | ✗(parse 错误) | ✓(用 fd mmap,标记 user_managed)|
| 命令行写 `fd=` + `uffd_socket=` | ✗ | ✓(创建 uffd + sendmsg + 等 ack)|
| `/vm.snapshot` 对 user_managed zone | (不适用) | 跳过 dump,memory-ranges 表无此 zone |
| `/vm.restore` 对 user_managed zone | (不适用) | 跳过 fill;mmap 直接 fault 触发 uffd |
| balloon release 对 user_managed zone 的空洞 run | (不适用) | 跳过 `PUNCH_HOLE`+`madvise`,不合成 `EVENT_REMOVE`;有数据的 run 同 upstream |
| balloon `deflate_on_oom=on` | ✓(v51.1 已就绪) | ✓ |
| `vm.resize` `desired_balloon` | ✓ | ✓(平台周期调用,host BalloonController)|
| virtio-mem `vm.resize` | ✓ | ✓ |
| vsock local-port cursor 跨 restore | ✗ | ✓(`VsockState.local_port_last`) |
| vsock backend/guest transport epoch 对齐 | ✗ | ✓(reset event + RX acknowledgement gate) |

平台**不**使用 `free_page_reporting`——upstream 支持完好,但在统一 memfd /
外部 uffd 模型下其持续 `madvise(MADV_DONTNEED)` 会广播 mmu_notifier 失效到
KVM EPT,IPI shootdown 饿死 guest vsock kthread(机理与替代反馈环见
`guest-runtime/docs/vmlinux.md` §5.5)。改由 host 端 BalloonController
经 `/vm.resize` 推 inflate target,事件量被反馈环 `MaxStep` 限速;冷启动充气
覆盖的稀疏区间由 patch 0004(§3.4)跳过,不产生 `madvise` 广播与
`EVENT_REMOVE`,`MaxStep` 仅对运行时回收**已驻留**工作集页仍有意义。
`deflate_on_oom` 是 upstream v51.1 原生,无需新 patch。

## 7. 已知限制

- **CH v52+ 升级窗口**:每次 CH 大版本会有 `vmm/src/memory_manager.rs` 内部
  重构,patch 0001/0002 通常需要小幅 rebase。0003(uffd ioctl)改动较大,需要
  多花时间 review。0004 仅触 `virtio-devices/src/balloon.rs` 单函数
  (`release_memory_range`),rebase 面最小。0005/0006 需要随 upstream vsock device
  queue/restore 生命周期变化一起复核
- **多 fd-backed zone**:当前限定单 zone(整段 sandbox RAM 一个 memfd)。多
  zone(NUMA / virtio-mem 横向扩展)需要在 patch 0003 处对每 zone 各自 sendmsg
  一次,sandbox-ctl 端各自维护 addrMap

## 8. See Also

- `sandboxer/docs/sandbox.md` §5(冷启动数据流,§5.2 CH 命令行)/ §7(恢复
  数据流)—— sandbox-ctl 怎么用 patched CH 跑沙箱;命令行示例
- `sandboxer/docs/sandbox.md` §8(uffd handler)—— sandbox-ctl 接收到 uffd_C
  之后如何处理 fault 事件
- `guest-runtime/docs/vmlinux.md` —— guest kernel 如何配合 CH 启动
  协议(PVH / EFI stub)
- `sandboxer/native-deps/README.md` —— `make cloud-hypervisor` 工作流与 patch
  开发循环
- `orchestrator/release-builder/docs/kuasar-sandbox.md` §2.4 —— VMM 与 Guest 环境在系统中的位置
