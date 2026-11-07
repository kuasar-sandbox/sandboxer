[English](vmm.md) | [简体中文](vmm_zh.md)

# VMM 集成与 Cloud Hypervisor 补丁

本页定义 Host/VMM 集成边界及维护中的 Cloud Hypervisor 补丁契约。使用匹配 VMM 时先读设备/启动
和行为章节，修改或移植集成时再读启用条件与各补丁。构建配方和原生材料采集仍见
[native-deps](../native-deps/README_zh.md)。

Runtime 提供外部 RAM、协调暂停/恢复并控制设备；Cloud Hypervisor 负责 KVM/vCPU/设备执行。
[内存](memory_zh.md)、[资源](resources_zh.md)和 [Runtime](runtime_zh.md)分别定义 Host 策略。
补丁实现 VMM 一侧的契约，包括 checkpoint 内存变化累计记录，并不保证任意上游版本的快照互通。

当前补丁系列以 Cloud Hypervisor **v51.1** 为目标，保留 Host Linux **5.10** 基线。0001–0008 提供外部 RAM/UFFD、vsock 与生命周期屏障，0009 保存 ARM 寄存器，0010/0011 回移 restore-order/seccomp 修复，0012 增加 checkpoint 内存变化累计记录。多数其他 VMM 路径沿用上游行为。

<a id="patch-purpose"></a>

## 为什么需要 patch

平台对 VMM 有四条非 upstream 的诉求:

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
4. **可靠 VM lifecycle**:pause、snapshot、resume 和 ordered shutdown 在数值
   `cpu.max` 与 host contention 下不能丢失 vCPU kick,也不能因 CPU 或 virtio
   worker 的旧 acknowledgement 提前返回或永久阻塞

这些组合能力在所固定的 upstream CH 版本中未直接提供。第二条尤其本质——upstream uffd handler 模型
是 CH 调外部 socket,handler 提供数据,但平台需要 handler 接管 fault 投递
路由,upstream 的 listener 模型不够。此外这一模型带来一条派生修复:balloon
释放路径对从未驻留的空洞也必须通知 REMOVE，因为惰性源仍可能含非零内容。
0004 仅省略冗余 inode 打洞，不能省略必要释放通知（[0004 — 先保留逻辑释放通知，再稀疏回收](#patch-0004)）。

<a id="patch-scope"></a>

## 补丁范围与未来演进

每项补丁包含完整意图、对应修正及测试。下表旧编号指 #377 恢复的原十二项补丁。
前十一项继续保持 #379 的维护范围。0012 独立维护完整累计内存日志；UFFD 接管继续使用原 ACK。

| 补丁 | 目的与范围 | 来源 | 未来维护及移除条件 |
| --- | --- | --- | --- |
| 0001 | 接收外部 RAM fd，完成映射及所有权管理 | 旧 0001，移出未使用的 UFFD 配置 | 项目自行维护。可向上游讨论通用外部 RAM 接口；仅在已发布等价实现通过集成验证后替换。 |
| 0002 | 将外部 RAM 的快照内容交给外部所有者 | 旧 0002 | 随 0001 自行维护。上游方案须保留外部保存/恢复职责；不扩展现有 virtio-mem 范围。 |
| 0003 | 完整旧版 MISSING UFFD 进程内创建、交接、配置与权限 | 旧 0003，加旧 0001 的 socket 配置 | 自行维护协议。跟踪上游外部缺页接口；迁移须保持通信端兼容和所有权语义。 |
| 0004 | 先通知逻辑释放，再稀疏物理回收；含错误路径和真实 UFFD 测试 | 在旧 0004 内修复 #378 | 自行维护项目优化。若向上游提交稀疏回收，须保持 REMOVE 语义；不能把本地优化引入的问题归为未修改 CH 的缺陷。 |
| 0005 | 持久化 vsock 本地端口分配游标 | 旧 0005 | 将恢复后的端口冲突修复及复现提交上游。采用已发布等价修复并验证后移除。 |
| 0006 | 预发布 vsock reset、保存确认状态并限制恢复 RX | 旧 0006 | 与端口分配分开向上游讨论 reset 时机及惰性恢复语义。接受、发布并验证前自行维护。 |
| 0007 | vCPU 生命周期 kick、请求对应 ACK、超时和恢复 | 旧 0007 的 CPU 部分 | 通用上游修复候选，附丢失 kick 和重复生命周期复现。等价实现验证后优先采用上游。 |
| 0008 | worker 暂停发布与恢复确认 | 旧 0007 的 worker 部分 | 独立上游同步修复候选。替代实现通过设备静止和恢复 barrier 回归后再移除。 |
| 0009 | 完整 ARM SVE/FPSIMD/浮点控制状态 | 合并旧 0008–0011 | SVE 已在上游，仅提交剩余 FPCR/FPSR 修正及原生测试。选定版本覆盖各部分后逐项移除回补。 |
| 0010 | 先恢复时钟，再设备，最后 vCPU | 旧 0012 | 临时上游回补。固定版本包含修复且恢复顺序验证通过后移除。 |
| 0011 | HTTP allocator 受限读取与映射增长，含真实 BPF 测试 | 适配 v51.1 的上游 seccomp 修复 | 临时上游回补。选定版本具备等价权限且正反过滤测试通过后移除。 |
| 0012 | CPU/CH 生命周期累计位图、原始写入、初始化、累计 API 及测试 | 已撤回 #375 的日志部分，适配 #379 | 固定基线 API 暂自行维护并推动上游接口讨论；原始写入对齐上游 3777ba8d。仅在等价源码及集成验证成立后移除对应部分。 |

上游核对固定于 2026-10-29 的
[`0674238bb`](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/0674238bb09082dfe55a4d3215eebf35ee0f5bb1)。
表中的计划提交不表示上游已经接受。编号不是二进制兼容标识；应记录上游源码、完整
补丁哈希和构建身份。#374 所指的原始旧 CH 仍是冻结、独立的制品。

<a id="maintenance"></a>

## 维护策略

- 按顺序应用，每个前缀须可构建且完整实现已经声明的能力；配置随实际消费者引入。
- 后续修复和回归测试并入功能所属补丁，不累积 fixup。保留原作者、上游链接及
  sign-off；不能将本地新增内容归给上游作者。
- 比较完整应用后源码树验证纯整理的等价性，单独记录有意行为变化：本次为释放通知/
  顺序、捕获保留释放零、ARM SVE capability 检查及 HTTP allocator seccomp 权限。

- 仓库相对 patch 路径:`native-deps/deps/ch-patches/*.patch`,按编号顺序应用
- 在 `sandboxer/native-deps` 中执行 `make ch-patches-apply`(全新 `make cloud-hypervisor` 构建也会执行);
  开发循环与幂等 sanity 语义见 [Native 构建 §3](../native-deps/README_zh.md#3-patch-开发循环)
- 每次计划升级 upstream 时复核补丁,依据实际上游变化解决冲突;不假定固定发布周期或固定 rebase 工作量
- 外部 RAM/UFFD patch 的设计选择(SCM_RIGHTS in-process +
  `create_ram_region` 里跑 ioctl)与 upstream 风格偏差明显,本项目维持自有
  patch;vCPU kick 与 worker barrier 若有 upstream 等价修复,升级时应优先替换
  `0007`/`0008`，不长期维护重复实现

<a id="boot-protocols"></a>

## 启动协议(per-arch)

CH 自适应启动协议,sandbox-ctl 命令行不区分 arch:

| arch | 协议 | kernel 入口 | CH 准备 |
|------|------|-------------|---------|
| x86_64 | PVH 直接启动 | ELF note 中的 PVH entry；`%rbx` 指向 `hvm_start_info` | PVH memory map、cmdline 和 ACPI RSDP 引用 |
| aarch64 | 通过 CH PE loader 直接启动 Linux Image | Boot CPU PC 指向载入的 Image；X0 指向 FDT | 描述内存、CPU、cmdline 和设备（含 GIC）的 FDT |

这是固定 v51.1 的直接内核启动路径。x86 zero page 属于独立的 bzImage 路径，不属于 PVH。
ARM Image 即使包含 EFI stub，也不代表 CH 进入该 stub 或提供 UEFI memory map。
ARM 直接内核路径构造 FDT，与回退的 UEFI firmware 路径不同。参见
[CH 内核加载](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v51.1/vmm/src/vm.rs#L1425-L1543)、
[PVH 初始化](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v51.1/arch/src/x86_64/mod.rs#L1079-L1178)、
[ARM FDT 初始化](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v51.1/arch/src/aarch64/mod.rs#L123-L159)
和 [ARM 入口寄存器](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v51.1/hypervisor/src/kvm/mod.rs#L2234-L2265)。

<a id="device-model"></a>

### 设备模型(平台用法)

冷启动设备清单取决于 root 拓扑和 host 策略：

```
virtio-pmem    → sandbox-runtime.bundle（DAX；discard_writes=on、MAP_PRIVATE backing）
virtio-blk     → vhost-user backend；single root：一个可写 ext4 COW 设备；
                 overlay root：只读 base + 可写 ext4 COW upper；
                 配置的数据盘跟在 root 之后，采用相同的一/双设备布局
virtio-net     → 可选;配置网络源时为 eth0,host TAP 后端;无源时不创建设备
virtio-console → 启用时由 hvc0 输出内核 dmesg；--console tty 写到 CH stdout。
                 stderr/journald sink 使用匿名管道，file sink 使用指定文件。
                 Console off 选择 --console off；--serial off 禁用 8250 UART。
                 CH stdin=/dev/null；应用 stdio 使用 vsock MUX，不走此 console。
virtio-vsock   → CID=3。控制面短连接(launch / ping / app_started / app_exited /
                 mem_report / quiesce / restore / attach)+ launch/restore/attach 那条
                 连接握手后升级而成的应用 stdio MUX(详见 vsock 控制面 + console MUX 协议)
virtio-balloon → 仅 memory control 启用时：size=<cold InitialTarget>
                 [+ deflate_on_oom=on]；sandbox-local
                 BalloonController 通过 /vm.resize 推 target,并以 vm.info 的
                 memory_actual_size 观察 current(见 Runtime Balloon);free_page_reporting
                 不启用(mmu_notifier 广播压力可能影响 guest vsock 进展)
virtio-mem     → host-driven 主动 unplug(CH 能力;当前固定 Capacity Budget 模型不启用)
```

Sandboxer 设置 `discard_writes=on`，因此 runtime backing file 只读打开并采用 MAP_PRIVATE
映射。未修改的 file-backed 页仍可共享 host page cache；私有写入不会修改 backing artifact。
Guest 将 runtime 作为只读 EROFS 使用。这与可写 Guest RAM 使用的 shared memfd 不同。
参见 [CH pmem 映射](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v51.1/vmm/src/device_manager.rs#L3246-L3306)。

只有 `Headroom < Capacity` 或冷启动 `InitialTarget > 0` 时，sandboxer 才添加 balloon；
初始 target 为零也可能因后续控制而需要此设备。参见 [`ch.go`](../pkg/sandbox/ch.go)、
[Balloon 控制](resources_zh.md#balloon-controller)、[console 接线](../pkg/stdio/stdio.go)
和 [Guest stdio MUX](guest-abi_zh.md#control-and-mux)。

restore 沿用 `config.json` 中的设备拓扑,不能新增或删除 virtio-net.sandboxer 在
启动 CH 前要求 host restore 配置是否提供网络源与快照中的 NIC 是否存在一致.

恢复路径设备拓扑通过 `--restore source_url=file://<snapshot-directory>` 从 snapshot
state 还原,不需要重新指定 `--kernel` / `--vsock`。

详细命令行示例与冷启动/恢复差异见 Runtime 的[冷启动 SDK](runtime_zh.md#runtime-sdk)
与[恢复流程](runtime_zh.md#restore)。

<a id="vsock-proxy"></a>

### vsock hybrid 代理

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
转入帧收发态(stdio MUX),CH 不感知。详细见 [Runtime](runtime_zh.md) [CH command line boundary](#vmm-command-line) 与
[Guest ABI](guest-abi_zh.md) [通道与寻址](guest-abi_zh.md#channels)。

<a id="behavior"></a>

## 行为契约总结

平台代码(sandbox-ctl + node-ctl)依赖的 CH 行为(包含 patch):

| 行为 | upstream | patched |
|------|----------|---------|
| 不写 `fd=` 时的内存 backing | upstream memfd/file 路径 | 同一分配路径;不表示其他补丁禁用 |
| 命令行写 `fd=` | ✗(parse 错误) | ✓(用 fd mmap,标记 user_managed)|
| 命令行写 `fd=` + `uffd_socket=` | ✗ | ✓(创建 uffd + sendmsg + 等 ack)|
| `/vm.snapshot` 对 user_managed zone | (不适用) | 跳过 dump,memory-ranges 表无此 zone |
| `/vm.restore` 对 user_managed zone | (不适用) | 跳过 fill;配置 uffd 时由映射缺页触发外部 handler |
| balloon release 对 file-backed region 的空洞 run | 原 release 路径 | 始终通知 REMOVE；仅省略冗余 `PUNCH_HOLE` |
| balloon `deflate_on_oom=on` | ✓(v51.1 已就绪) | ✓ |
| `vm.resize` `desired_balloon` | ✓ | ✓(sandbox-local BalloonController 调用)|
| virtio-mem `vm.resize` | ✓ | ✓ |
| vsock local-port cursor 跨 restore | ✗ | ✓(`VsockState.local_port_last`) |
| vsock backend/guest transport epoch 对齐 | ✗ | ✓(reset event + RX acknowledgement gate) |

平台**不**使用 `free_page_reporting`。在统一 memfd / 外部 uffd 模型下,持续
`madvise(MADV_DONTNEED)` 可能产生 mmu_notifier/EPT 与 IPI shootdown 压力,
影响 guest vsock 进展(机理与替代反馈环见
[Guest Kernel §5.5](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/vmlinux_zh.md#为什么不启用-free_page_reporting以及-virtio_mem-的角色))。改由 sandbox-local BalloonController
经 `/vm.resize` 推 inflate target;steady shrink 每份 fresh report 最多一个
64MiB step。冷启动命令行 target
覆盖的稀疏区间与运行时驻留回收都由 0004 保留 REMOVE，只跳过冗余打洞。4 KiB Host 上的冷创建 VM 合并连续 PFN，
以减少同步 REMOVE 处理次数（[0004 — 先保留逻辑释放通知，再稀疏回收](#patch-0004)）。
`deflate_on_oom` 是 upstream v51.1 原生,无需新 patch。

<a id="activation"></a>

## 启用条件与命令行

CH `--memory-zone` 增加两个 key:

| key | 类型 | 含义 |
|-----|------|------|
| `fd` | int | 由父进程通过 cmd.ExtraFiles 注入的文件描述符,用作 zone 的 backing |
| `uffd_socket` | path | 普通 UDS 路径,CH 在该 socket 上 sendmsg(va_report; SCM_RIGHTS=uffd_C) |

行为契约:

- **不写 fd**(也不写 uffd_socket):内存分配沿用 upstream 的 backing-file/memfd 路径,
  不启用这里的外部 uffd handler。这仅描述内存 backing,不表示 lifecycle、vsock 等其他补丁被禁用
- **仅写 fd**:CH 把 fd 当 backing,跳过 memfd_create;snapshot 时跳过 dump;
  restore 时不 fill。这条独立成立(无 uffd 仍可外部托管)
- **fd + uffd_socket 同时写**:在 fd 行为基础上叠加,CH 内 `userfaultfd()`
  → `UFFDIO_API` → `UFFDIO_REGISTER MISSING on chVA` → `connect uffd_socket`
  → `sendmsg(va_report; SCM_RIGHTS=uffd_fd)` → 等 ack。ack 之前 vCPU 不允许
  跑

冷启动与恢复采用相同的 backing/handler 组合，但 argv 不同。冷启动包括：

```
cloud-hypervisor \
  --memory size=0,shared=on \
  --memory-zone id=ram0,size=8G,shared=on,fd=3,uffd_socket=/run/sandbox/<sid>/uffd.sock \
  ...
# fd=3 ← memfd from sandbox-ctl via cmd.ExtraFiles[0]
```

Restore 使用 `--restore source_url=file://<snapshot-directory>`，不重复传 `--memory-zone`。
Sandboxer 重写捕获配置中 zone 的 `uffd_socket`，并将继承的 memfd 传给新的 CH 进程。
序列化的 fd 数字本身不是有效描述符。参见 [`restore.go`](../pkg/restore/restore.go)
和 [`configrewrite.go`](../pkg/restore/configrewrite.go)。

完整补丁系列对外部 fd-owned RAM 要求 **4 KiB Host 页**：0012 在初始化 checkpoint ledger
时拒绝其他 Host 页大小，冷启动和恢复均如此。这与 4 KiB Guest 页大小是不同约束。

解析器要求 zone `id`，平台使用 `ram0`。详细命令行见 [CH 参数边界](#vmm-command-line)，
恢复顺序见 [Runtime 恢复流程](runtime_zh.md#restore)。

<a id="patches"></a>

## patch 提交结构



<a id="patch-0001"></a>

### 0001 — externally allocated memfd-backed memory zone

主题:**memory: support externally allocated memfd-backed memory zone**

改动文件:`vm_config.rs` / `config.rs` / `memory_manager.rs`

要点:

- `MemoryZoneConfig` 新增 `pub fd: Option<i32>`,当前补丁使用 `#[serde(default)]`,
  并非 `serde(skip)`。序列化的 fd 数字不是可迁移的内核引用;新的 CH 进程仍需继承相应 fd。
- `MemoryConfig::parse` 解析 `fd=N`;实际 `File::from_raw_fd` 在 memory-manager
  构建路径中接管继承的描述符,不在 CLI parser 中执行。CH 拥有这些 `File` 及其 clone;
  父进程另持自己的描述符,生命周期相互独立。
- zone 提供 fd 时,memory manager 将该 backing 传给 `create_ram_region`,不另建 memfd;
  patch 0002 再把 zone 记为 `user_managed: true`。
- 一个 zone 可含多个 region,例如较大的 x86_64 zone 跨越 PCI MMIO hole。
  每个 region 通过 `try_clone()` 获得自己的 `File` ownership;各映射及其 file offset
  仍指向同一 backing inode,共享 host page-cache 数据。

`Transportable::send` 与 `fill_saved_regions` 都遍历 `snapshot_memory_ranges`
表,**这两处不需要单独 patch**——靠 commit 0002 在 `memory_range_table` 处过滤
即可同时影响快照和恢复路径。

<a id="patch-0002"></a>

### 0002 — snapshot skip user-managed memory zones

主题:**snapshot: skip user-managed memory zones**

改动文件:`memory_manager.rs`

要点:

- `memory_range_table` 在生成 `snapshot_memory_ranges` 时检测每个 zone 的
  `user_managed` 标志,user_managed=true 的 zone 在表中**不出现**
- 这样 `Transportable::send`(快照 dump 路径)看到的 ranges 不含 user-managed
  zone,自然 `Ok(())` 即返回——CH **不**写 8 GiB memory-ranges 文件
- 同理 restore 路径 `fill_saved_regions` 看到的 ranges 表不含此 zone,
  fill 操作变成空操作——guest 物理地址通过 mmap 直接映到 sandbox-ctl 的 memfd
  inode;配置了外部 uffd 时,内容由 sandbox-ctl handler 按需提供

upstream 已有"file-backed + MAP_SHARED + hardlink"的等价 skip 分支(用于
share-storage live migration 场景)。我们的 user_managed zone 满足"file-backed
+ MAP_SHARED",但 memfd 没有 hardlink(`st_nlink == 0`),所以不能复用现有
分支——添加平行分支识别 `user_managed` 标志。

<a id="patch-0003"></a>

### 0003 — external uffd handler via in-process create + SCM_RIGHTS

主题:**memory: external uffd handler via in-process create + SCM_RIGHTS handoff**

改动文件：`memory_manager.rs` / `vm_config.rs` / `config.rs` / `seccomp_filters.rs`

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
  `CAP_SYS_PTRACE`,权限不足时可能导致 UFFDIO_API 返回 EPERM。

- seccomp 放行:`SYS_userfaultfd` syscall + `UFFDIO_API` / `UFFDIO_REGISTER`
  ioctl(filter 在 `seccomp_filters.rs`)

**为什么 uffd 必须 CH 内创建**:Linux 内核把 uffd 上下文绑到 `userfaultfd()`
调用进程的 mm。sandbox-ctl 创建的 uffd 上 `UFFDIO_REGISTER` 只能 register
sandbox-ctl mm 的 VMA,不能 register CH mm 的 chVA。所以 uffd 必须在 CH 内
创建,然后通过 SCM_RIGHTS 把 fd(而非内核内部对象)传给 sandbox-ctl 让它读
事件。fd 表是进程级的,但 uffd ctx 的事件路由按创建者 mm 的 VA 解析,
sandbox-ctl 收到 fd 后读出来的事件 va 就是 CH 视角的 chVA。

**惰性恢复中的后端首次访问。** 后端 mmap 有意不注册 UFFD。普通 shmem
访问可能直接创建零页，而未读取快照字节，因此读取或只写一个状态字节也必须先准备
内存。所有 vhost GPA/UVA 地址转换都在返回切片前调用带上下文的
`EnsureLoaded(ctx, memfdOffset, length)` 屏障，覆盖描述符、间接描述符表、
avail/used ring、请求头、两个方向的数据和状态。缺少回调、范围无效或填充失败都
使队列终止，不能发布 used-ring 完成记录。所有磁盘通过统一的服务器创建路径绑定
该回调，CLI 与 SDK 的冷启动和恢复均使用此路径。队列重置取消待完成加载，等待
worker 退出后才替换内存表。

屏障检查所有相交页，包括部分访问之外的原有字节。Loaded 范围批量检查状态，
保留实时内容而不读取快照。Absent 页由现有 fault worker 使用其私有 4 KiB
缓冲区读取权威来源。Released 页永不读取历史快照内容。整页安装使用所属 CH
注册的 `UFFDIO_COPY`，shmem 需要时也通过零缓冲区 COPY。`EEXIST` 保留已经
存在的页并唤醒等待者；它**不证明**此前无保护的后端访问曾恢复快照。必须校验
实际完成字节数。`EAGAIN` 在 worker 外可取消地重试，使事件读取和 CPU fault
队列能够推进；`ENOENT` 或无效注册使请求失败。真正的 CPU MISSING fault
遇到 Loaded 页仍走独立的已回收页处理路径。

`va_report` 的线格式不变。已计算的 memfd 偏移通过内部回调传递，并与实际 CH
VA 区间及持有的 UFFD fd 关联。只有成功接管后，新注册才可用于加载；接管失败
回滚暂存的地址映射，由回调调用方清理 fd。加载可取消地等待注册就绪，并在区域
边界拆分。UNMAP 使对应注册区间失效；REMOVE 释放页内容但保留注册。

安装互斥锁协调 CPU urgent、后端 mandatory、speculative tail 的 ioctl 与
非阻塞内核事件读取和 REMOVE/UNMAP 状态发布。来源读取后、实际安装前再次校验，
防止在观察到释放后安装过期快照缓冲区。来源读取、重试等待、注册等待和可能阻塞的
队列发送均不持有此锁。保留普通数据和 chunk 的有界 tail 批量填充。
锁顺序审查参考 [Linux 5.10 的 UFFD 事件与 COPY 路径](https://github.com/torvalds/linux/blob/v5.10/fs/userfaultfd.c)：
REMOVE 在等待用户态前释放 mmap 读锁，映射变化期间 COPY 可以返回 `EAGAIN`。
此锁**不是 DMA pin**：要求 guest 正常持有未完成描述符的缓冲区；屏障返回后任意
并发 balloon/discard 不在此保证范围内。

Close 同步关闭准入和取消操作，等待调用者及所有 worker 退出，并通过有界 epoll
轮询停止 reader，之后才关闭 UFFD fd。来源和后端映射必须存活至上述等待完成。
启动早期失败也会取消就绪与握手等待。此方案不增加第二套 UFFD，不使用
MINOR/CONTINUE，不预先填充整块 RAM，不提高内核最低版本，也不改变资源策略。

可重复验证包括 `pkg/vhost` 中的
`TestRealMissingShmemRestoredUsedRingPreservesUntouchedBytes`：两个共享 memfd
别名、真实的 CH 侧 MISSING_SHMEM 注册及实际 `publishUsed` 路径，在 CPU-first
和 backend-first 两种顺序下都必须保留周围字节，并把保存的索引从 7 推进至 8。
UFFD 和地址转换测试覆盖范围、取消、注册、释放及并发失败。现有
`snapshot.restore.sh` E2E 还在快照/恢复之间保留 O_DIRECT 文件描述符和 32 个
匿名缓冲区。在页内偏移 512 处读取 512 字节后，其余 3,584 字节必须保持原值。
该用例包含冷启动对照、DMA 前用户态未访问的十六个候选页，以及恢复后显式通过
CPU 预热的十六个对照页；`dio-before.json`/`dio-after.json` 保留完整性结果。
guest 的 pinning/prefetch 可能填充候选页，因此 E2E 本身不能证明 backend-first
顺序；真实 used-ring 测试提供确定的访问顺序。应遵守既有准备来源/产物契约，
结合现有 KVM 内存压力覆盖执行两类测试，并验证 Linux 5.10 最低版本。
这些是验证要求，不代表尚未执行的测试已经通过。

<a id="patch-0004"></a>

### 0004 — 先保留逻辑释放通知，再稀疏回收

释放实现和 seccomp 策略位于 `virtio-devices`。
`vmm/src/vm.rs` 和 `device_manager.rs` 将 VM 创建来源传给 balloon。
问题报告：[ #378](https://github.com/kuasar-sandbox/sandboxer/issues/378)。

原优化在 SEEK_DATA 证明物理范围为空时，同时跳过 PUNCH_HOLE 和 MADV_DONTNEED，
减少冷启动 balloon 收敛中的同步 REMOVE 工作。但其等价性前提错误：物理 Hole
仍可能从惰性快照源继承非零内容。抑制 REMOVE 会保留该源，后续 CPU fault 或
后端加载便可能复活已释放内容。balloon inflate 和 free-page reporting 都调用
受影响的释放 helper。

修正后的顺序：

1. 验证完整 guest 范围，在 chVA 上执行 MADV_DONTNEED。即使全是 Hole，也必须
   产生逻辑释放通知。advice 失败直接返回，不进入探测或打洞。
2. file-backed region 使用 `file_offset().start()` 转换偏移后执行 SEEK_DATA。
   ENXIO 或下一个 Data 位于区间末尾及以后，只允许省略冗余 PUNCH_HOLE。其他探测
   错误进入正常打洞路径，不能直接成功返回。
3. 必要时对完整释放区间打洞并传递错误。打洞失败可能保留驻留字节，调用方必须
   观察失败。

现有 Go handler 在同一 installMu 下读取 REMOVE、发布 Released 和安装 COPY；
已在进行的源读取在安装前重验状态。通知后，CH 探测/打洞会覆盖此前已安装内容。
这里复用既有同步，不新增 dirty ledger。实际 file-backed 判定是
`region.file_offset().is_some()`，不是 user_managed；无 file offset 的区域仍执行
advice。映射/显式偏移打洞不依赖共享 file position。balloon seccomp 继续允许
lseek，fallocate 和公共 madvise 原本已允许。

在 4 KiB Host 上，普通冷创建 VM 和冷 Build producer 对每个 inflate descriptor 分块，
每块最多排序、去重 8192 个 PFN。同一内存 region 中相邻 PFN 的 backing offset 连续，
可一次释放。worker 完整处理所有块和尾批后才 ACK。32 KiB 暂存上限针对单块，
一次 queue callback 可以处理多个块和 descriptor。

VMM 在提取 DeviceManager 子树前根据顶层 VM snapshot 是否存在记录创建来源。
Balloon 还要求自身没有恢复状态。该运行时选择持续整个 VM 生命周期，包含后续充气。
新恢复实例即便缺少任一子树也逐 PFN 释放。0004 自身对较大 Host 页保留局部页所有权跟踪，
但完整平台的外部 RAM 配置受 0012 限制，只支持 4 KiB Host 页（[启用条件](#activation)）。

执行普通错误路径回归及显式原生 UFFD 测试：

```sh
cargo test --locked -p virtio-devices --features kvm --lib release_tests -- --include-ignored --nocapture
```

原生测试调用生产 4 KiB PFN inflate 和区间释放 helper，使用真实 shmem MISSING/
REMOVE，覆盖非零 backing offset、ENXIO、区间外 Data、已物化及未物化非零源，
比较完整释放页和邻页。它要求实际 UFFD 能力，属于内核组件测试，不能冒充完整 VM
E2E。普通测试覆盖对齐错误和真实打洞失败。保留旧实现失败对照，并运行现有 Go
释放/安装测试及使用重建 CH 的集成测试。

释放修复同时覆盖捕获边界：CH/backend 冻结后，在 installMu 下联合采集物理 Hole
和现有页状态。Data 取当前字节，Hole+Absent 继承父层，Hole+Released 或 Loaded
保存为显式 Zero；Loaded+Hole 覆盖补零后又被 CH 最终打洞的情况。捕获成功或失败
均不重置状态。既有层叠与载体保留 Zero，保证无需 refault 或预填全部 RAM 即可
正确捕获、恢复。不增加 memory.dirty、新 dirty API 或历史裁剪；这些仍留在 #374。

<a id="patch-0005"></a>

### 0005 — 持久化 vsock host local-port 游标

CH 的 Unix vsock backend 在 restore 时重新创建。若 host local-port 分配器回到
`0x40000000`,首个 host-init 连接可能复用 guest 快照里仍存在的四元组并收到 RST。
patch 把 `local_port_last` 纳入 `VsockState`,恢复 backend 后从下一端口继续分配。
该状态在嵌套快照中逐层保存,不是只覆盖一次 restore 的进程内游标。

<a id="patch-0006"></a>

### 0006 — snapshot 时预发布 vsock transport reset

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

<a id="patch-0007"></a>

### 0007 — vCPU 生命周期请求与确认

CH v51.1 用 no-op `SIGRTMIN` handler 中断 `KVM_RUN`:控制线程先发布 pause/kill
状态,再 `pthread_kill` 每个 vCPU,等待 vCPU loop 写入 acknowledgement。若 signal
在 vCPU 检查状态之后、进入下一次 `KVM_RUN` 之前到达 userspace,handler 返回后该
vCPU 仍可阻塞在 `KVM_RUN`;每 10 ms 重发只能降低概率,不能关闭竞态窗口。

patch 通过两组 vCPU lifecycle barrier 修复关闭该问题:

1. 创建线程在 spawn vCPU 前配置 `KVM_SET_SIGNAL_MASK` 并阻塞 kick signal,使子线程
   从第一条指令起继承 blocked mask;配置失败直接返回,不会让一个提前退出的 vCPU
   留在启动 barrier 之外。vCPU loop 在读取 lifecycle 状态前保持 signal blocked;
   `KVM_RUN` 通过已配置的 mask 原子 unblock,退出时恢复 blocked mask。signal 在状态
   检查与 `KVM_RUN` 之间到达会保持 pending 并中断同一次调用;KVM 返回后处理 PIO/MMIO
   等 userspace exit 时仍不消费该 signal。outer loop 观察到对应 lifecycle 请求并完成
   必要的 pending KVM I/O 后,才短暂执行 `SIG_UNBLOCK`/`SIG_BLOCK`,由空 handler 消费
   本次 kick 并立即恢复 blocked mask,避免后续 `KVM_RUN` 因旧 pending signal 持续
   立即返回。VMM seccomp 的公共 KVM ioctl 集显式放行 `KVM_SET_SIGNAL_MASK`。
2. pause、shutdown、NMI 和 vCPU removal 在发布请求前清除旧 ACK;vCPU 只有进入当前
   请求的 pause/NMI/kill 分支后才写 ACK,自然 reset、shutdown、run error 或 panic
   不能冒充请求确认。KVM no-miss 路径每个请求只发一次 kick;pause/NMI 分支在 ACK 前
   消费已 pending 的本次 kick,并在 park/unpark 后再次消费,覆盖 vCPU 先看到共享状态、
   控制线程后发送 kick 的顺序。NMI 使用 ACK set/clear 双向 barrier,控制线程在所有
   vCPU 清除本次 ACK 后才返回,
   因此前一请求的 drain 不会吞掉下一次 pause 的 kick。已经自然结束但尚未 join 的
   vCPU thread 由 `JoinHandle::is_finished()` 从 signal barrier 排除,其 ACK 仍保持
   false,避免正常 shutdown 清理等待一个不可能到达的 ACK。pause 失败会撤销请求、
   unpark 并等待已参与 vCPU 恢复;NMI 即使 signal 超时也会完成 ACK-clear cleanup,
   两条错误路径都使用独立的新 deadline。非 KVM backend 保留 10 ms retry。所有等待
   统一使用 `CLOCK_MONOTONIC` 语义的 1 s deadline。

<a id="patch-0008"></a>

### 0008 — virtio worker 暂停与恢复握手

runtime pause 先发布共享 pause event,再显式 unpark 所有普通与自定义 worker,
   关闭 worker 的 zero-time epoll poll 到 `thread::park` 之间的启动竞态。resume 先
   drain 该 event 再唤醒 worker;worker 从 park 恢复后参加第二次 barrier,control
   thread 收齐 resume ACK 才允许下一次 pause。net 与 vhost-user 的自定义 worker
   handle 同时加入 pause wake 和 resume barrier。恢复态设备若尚未收到 runtime
   pause event,不会等待一个不存在的 ACK。

这些生命周期 patch 不改变 sandbox 配置、CH HTTP API、snapshot 格式或资源协议,也不在
lifecycle 前后修改 `cpu.max`。超时仍是最终有界失败保护,不是竞态修复方法。

<a id="patch-0009"></a>

### 0009 — 完整 ARM CPU 快照寄存器

启用 SVE 的 ARM 主机通过 KVM 的 SVE Z/P/FFR 寄存器暴露向量状态。
访问旧 FPSIMD V 寄存器偏移会返回 `EINVAL`;因此 VM 可以正常冷启动运行,
但在捕获快照时失败。

0009 合并回移 Ruben Hakobyan 已合并的
[Cloud Hypervisor PR #8268](https://github.com/cloud-hypervisor/cloud-hypervisor/pull/8268),
保留原作者与签署。审阅的上游提交为
`083b41905ea59b335c4f211c4186312e30bbf022`、
`e9d044730524d12e243737a1888767b3edf123c8` 和
`4fc414cc8bc56a4c5275b3dd44610e5c75dfbbd1`;上下文适配 v51.1 与前置平台补丁。
补丁枚举 KVM 寄存器、保存 SVE 状态,并在 vCPU 初始化之后、SVE finalization
之前恢复向量长度选择。不启用 SVE 时仍使用原有向量路径。

同一补丁也回移上游
[48834e1](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/48834e139e9d1882cdefb6cafa42155a13d173bc)：
SVE finalize 同时检查 host HWCAP 和 VM 的 KVM ArmSve capability，与初始化一致。
v51.1 适配直接传递已有 VM 引用，不引入无关的 Vcpu 所有权重构。

同一补丁额外为两条路径保留 FPSR/FPCR:向量寄存器族改变时,这些控制和状态寄存器
仍然可访问。补丁包含设置非零寄存器状态的原生 KVM 往返测试:

```sh
cd native-deps/build/src/cloud-hypervisor
cargo test --locked -p hypervisor --features kvm test_arm_ -- --ignored --nocapture
```

这些测试需要原生 ARM KVM(SVE 测试还需要 SVE);ignored 标记不代表在没有
`/dev/kvm` 的托管 ARM runner 上验证成功。平台最终验收还使用所选已发布的
Cloud Hypervisor 二进制执行真实冷启动、guest exec、快照与恢复。

快照 CPU 状态新增带默认值的 pre-finalize 与 extended-register 数组,可读取旧的
非 SVE 状态,但不承诺跨主机 CPU 特性兼容,也不承诺向未打补丁的 VMM 反向恢复。
SVE 保持启用,不兼容寄存器状态通过已有 KVM 错误失败。

<a id="patch-0010"></a>

### 0010 — 在 vCPU 前恢复 KVM 时钟

补丁 0010 回移上游 [commit ff20f183647c602bf5e5028d99959e5547a7dd84](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/ff20f183647c602bf5e5028d99959e5547a7dd84)，
保留 CMGS 的作者归属和 sign-off。固定的 v51.1 恢复路径可能在还原已保存的 x86
KVM 时钟前执行 vCPU。回移后先还原时钟，再恢复设备，最后恢复 vCPU。时钟操作仍仅
适用于 x86；设备先于 vCPU 的顺序也适用于 ARM。快照 schema、Guest ABI 和固定的
上游版本均不变。

验证前应应用完整编号补丁序列并重建原生 VMM。补丁内容参与 native build/cache
identity；不具备对应身份的既有二进制不能用于验证回移。重复执行单 vCPU 的
`sandbox.memory-budget` 恢复、pause/wake，以及保留当前 direct-I/O 完整性断言的
`snapshot.restore`，之后执行普通全量 E2E。记录原生架构和产品来源；仅交叉编译不会
执行真实 KVM resume。小样本成功不能确定故障率，也不能据此诊断其他恢复错误。

<a id="patch-0011"></a>

### 0011 — HTTP allocator seccomp 兼容

大 HTTP 分配或释放可能触发 glibc 惰性读取 `/proc/sys/vm/overcommit_memory`，
也可能通过 mremap 扩展映射。即使 API 操作有效，v51.1 的 HTTP filter 仍可能使
线程因 SIGSYS 退出。回移上游
[`660b18e`](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/660b18eaace7953721c8ecf371973fc6eb6e2f66)
（原 HTTP 修复 [`ec157d7`](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/ec157d7eb9f5915512efcd0660a78b41be8bc87e)）及
[`f05f0c2`](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/f05f0c249e69568dea3ef8e7208d63f6ef8922ea)，
不引入新版公共 filter 架构。

仅在 HTTP filter 允许 flags 严格等于 O_RDONLY|O_CLOEXEC 的 openat，以及 read、
mremap。这限制 flags，不是 pathname 白名单；写入、创建、截断打开仍被拦截。
子进程测试安装真实生产 BPF，执行读取和映射增长，并验证禁止 flags 触发 SIGSYS：

```sh
cargo test --locked -p vmm --no-default-features --features kvm --lib seccomp_filters::tests::http_allocator_probe_is_read_only -- --nocapture
```

这是 allocator 兼容回补，不新增脏页 API 或响应格式。mremap 是上游兼容修复，
并不能证明它是此前每次 API EOF 所触发的系统调用。

<a id="patch-0012"></a>

### 0012 — checkpoint 内存变化累计记录

外部 RAM 在内存初始化阶段启用 KVM 日志，并在任何清理前保存软件 seed 写入。
CPU/KVM、CH AtomicBitmap 和原始写入（net RX、vsock buffer、快照 reset/event/used ring）
共同进入生命周期累计集合。校验后的可写范围在交出原始指针前先标记，保守覆盖部分写入和失败。
上游 [3777ba8d](https://github.com/cloud-hypervisor/cloud-hypervisor/commit/3777ba8ded440cca2d953cd40e8b1ee43ae6c682)
已修复新代码的 net readv 覆盖；本 v51.1 适配还保留失败时的候选，不引入无关 net 重构。

`GET /api/v1/vm.dirty-memory` 返回 version=1、page_shift=12 以及 slot/GPA/size/
file_offset/bitmap。每个已消费来源先转移到累计集合，再读取下一个来源或 slot。
失败 slot 在报错前全部记脏；registration 变化保留全部已知范围。HTTP 响应丢失和重试
不清除累计记录。迁移 start/read/stop 不能消费或关闭 checkpoint 拥有的日志：外部
checkpoint RAM 在访问迁移目标或进入失败清理前即拒绝 live migration。

`va_report` 保留现有字段和 FD，仅增加可选 `file_offset`。ACK 仍为
`{"type":"ack"}`，只确认接管，不声明日志能力。不新增版本 ACK、WP 协商、预物化或
内核基线。新 CH 自行初始化累计记录。宿主首次实际捕获时识别 API：完整合法响应选择原生
日志，完整 404 选择工作集兼容，其他错误使捕获失败。结果固定于当前 CH 进程；原生模式
后续遇到错误不得降级。API 数据版本、页粒度和区域校验仍必须执行。

兼容基线是 v51.1 加 #379 的 0001–0011，不含 0012。它沿用相同惰性源和释放安全的
Data/Zero/Hole 捕获，全部显式工作集内容保守按 dirty 处理。冻结的 #379 之前 0004
二进制仍有已单独修复的释放缺陷；404 不能证明已修复。撤回的 #375 版本 ACK 协议
不属于支持目标。restore 在启动 CH 前校验单 ram0、shared、非 prefault、4 KiB、
按序连续 memfd offset 和空 CH memory-ranges；两架构的 GPA 缺口仍遵守旧无偏移报告契约。

累计日志后续修正和测试均回折 0012；释放顺序属于 0004，HTTP allocator 权限属于 0011。
通用原始写入修正跟随上游，生命周期 API 在上游接口达成一致前自行维护。上游核对到
d1719ac8，相比 ad5f3eea 仅新增无关的迁移超时处理；继续固定 CH v51.1。

验证覆盖原生 KVM slot 失败/重试、迁移日志所有权拒绝、net/vsock 原始写入、初始化，
以及 `snapshot.dirty-memory` 的完整 RAM 比较、同状态尺寸和响应丢失。
常规 CI 的 `snapshot.ch-compat` 通过既有原生构建脚本，从正式前十一项补丁构建仅用于
测试的 CH。两种原生架构均验证旧 CH 冷捕获和新四条目快照由旧 CH 恢复，包括一次性
能力识别、释放与重用。helper 记录应用后源码 tree、补丁及二进制哈希，不作为产品 VMM。

<a id="build"></a>

## 构建工作流

`sandboxer/native-deps` 从固定 v51.1 tarball 构建,应用
`git am deps/ch-patches/*.patch`,再执行 `cargo build --release --locked`。
输出为 `native-deps/bin/<arch>/cloud-hypervisor`;从 sandboxer 根目录执行
`make cloud-hypervisor` 还会同步到 `bin/<arch>/cloud-hypervisor`。
构建时间取决于工具链、机器和缓存。

构建使用 `--remap-path-prefix` 将源树映射为相对路径、Cargo 依赖映射为 `/cargo`,
避免把这些构建机绝对源路径嵌入 panic 消息和 DWARF。

`ch-fetch` / `ch-patches-format`、幂等检查、产物同步与边界见
[Native Build 文档](../native-deps/README_zh.md)。

<a id="limitations"></a>

## 已知限制

- **包括 v52 及之后的 upstream 升级**:依据实际 `vmm/src/memory_manager.rs` 变化
  复核 0001/0002;0003 的 uffd ioctl 需要重点检查。0004 同时修改 balloon release
  函数及 seccomp allowlist。0005/0006 随 vsock queue/restore lifecycle 变化复核;
  0007 与可靠的 upstream immediate_exit 实现一起评估,不承诺固定 rebase 工作量。
- **vCPU kick 掩码开销**:KVM vCPU 每轮 userspace exit 仍执行一次幂等 signal block;
  unblock/reblock 只发生在已观察 lifecycle 请求的安全点。CPU-bound guest 不增加
  VM exit;PIO/MMIO 密集负载会承担额外 syscall 成本。该路径用于在 sound
  `immediate_exit` 接口可用前保持无漏唤醒语义
- **多 fd-backed zone**:当前限定单 zone(整段 sandbox RAM 一个 memfd)。多
  zone(NUMA / virtio-mem 横向扩展)需要在 patch 0003 处对每 zone 各自 sendmsg
  一次,sandbox-ctl 端各自维护 addrMap

<a id="vmm-command-line"></a>

### CH command line boundary

Cold start 用 host-bound kernel/runtime paths、memfd、vhost sockets、active diffs 和 optional net provider构建 CH argv. Portable config从不保存这些绝对 path/fd.

Block devices只绑定 logical Payload:

- Container image或 direct EROFS E:EROFS prefix only.
- Parent E作为 ext4 layer:E root payload only.
- Current run-from E:self resolves to E payload section only.
- ZIP tail永不暴露给 vhost.

CH stdin固定 `/dev/null`;console output由 sandbox-ctl bridge. Net provider为 TAP 或 TapFD,而 portable topology只说明 NIC是否存在和 interface name.

<a id="related"></a>

## 相关文档

[Runtime：启动、控制、捕获与恢复](runtime_zh.md) | [Sandbox 配置参考](configuration_zh.md) | [Sandbox 与 Snapshot 制品](artifacts_zh.md) | [Guest 内存与按需恢复](memory_zh.md) | [块存储：设备、可写状态与 Checkpoint](block-storage_zh.md) | [资源控制与 Usage 计量](resources_zh.md) | [Guest ABI：启动、应用与 Host 协议](guest-abi_zh.md)

[组件入口](../README_zh.md) · [构建与补丁材料](../native-deps/README_zh.md)
