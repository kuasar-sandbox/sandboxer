[English](memory.md) | [简体中文](memory_zh.md)

# Guest 内存与按需恢复

本页定义 RAM 数据路径：外部 memfd 生命周期、missing-page 注册、分层 fault 读取、
推测性填页、释放页的逻辑真值与 checkpoint 变化记录。它说明哪些数据能够按需读取，以及新 Snapshot
捕获哪些字节。[Runtime 恢复流程](runtime_zh.md#restore)负责启动和 VMM 就绪屏障，
[捕获屏障](runtime_zh.md#capture-barrier)负责与设备、磁盘共同冻结。

内存 capacity、Budget、cgroup 执行与 balloon 目标策略见[资源](resources_zh.md)；VMM 补丁契约见
[VMM](vmm_zh.md)；Snapshot 元数据 schema 和发布身份见[制品](artifacts_zh.md#snapshot-format)。

<a id="backing-memory"></a>

## 内存 backing 与所有权

[`memory.Create`](../pkg/memory/memfd.go)以 `MFD_CLOEXEC | MFD_ALLOW_SEALING` 创建 Host 所有的
memfd，truncate 到配置 RAM 大小，seal shrink/grow/继续改 seal，再以 `MAP_SHARED` 映射进 sandboxer。
它应用 `MADV_NOHUGEPAGE`，维持 handler 的 4 KiB 页语义；失败时回滚已完成构造步骤。
继承 fd 使 CH 映射**同一个 inode**；vhost memory-table 接入检查该身份，而不接受无关 backing object。

Host mapping 与 CH mapping 的虚拟地址不同，fault 必须通过注册在 CH mapping 上的 descriptor 解决。
在 Host mapping 上再创建一个 UFFD，不能建立所需的 CH/KVM fault 所有权。
[VMM 补丁 0001](vmm_zh.md#patch-0001)定义 inherited-fd 接入，
[补丁 0003](vmm_zh.md#patch-0003)定义交接及确认。

`Memfd.Close` unmap Host 视图并关闭自身 fd。CH 仍有 fd/mapping 时内核保留 inode，但这不授权提前
释放 Host handler 的缓冲区或借用制品 stream。Runtime teardown 先停止 VMM，取消并 join 服务工作，
再释放其内存和源。地址访问必须在 `Size()` 内；mapping 生命周期与 operation-context cancellation 是不同契约。

冷启动使用 zero source；恢复将当前 S 和有序 parent 打开为 sparse layered memory view。Hole 继承，Zero
遮蔽所有下层，Data 提供字节。Guest release 后只看物理驻留并不足够：handler 的逻辑释放记录阻止旧的
非零 parent 数据重新出现。下文捕获与 fault 规则共同维持这个区别。

<a id="uffd"></a>

## UFFD handler

Memory restore 使用一个共享 memfd，以及接收 CH-side userfaultfd 的 host handler。CH 可以为同一个 memory zone 报告多个 region，例如 x86 RAM 跨 PCI hole 分段；这些 fd 共用 handler 的 epoll set 和 address map。 S self及`from_refs`被打开为 layered sparse source;top resident data覆盖lower,top Hole向parent fall through,Zero仍是显式zero.

<a id="uffd-registration"></a>

### Single UFFD contract

这里的 single-UFFD 设计表示只在 CH mapping 上处理 fault，而不在 sandbox-ctl 的 backend mapping 上注册第二套 UFFD；不表示所有布局都只有一个 CH fd。CH 为 restored memory region 注册 missing-page events；host 通过 restore handshake 接收 fd，并按共享 memfd address map 验证 region 范围与不重叠。首个 report 启动 handler，后续合法 region 用 `AddUffd` 加入。超界 fault、重复/无效 region registration 或截断 source 均 fail closed。源码见 [serveandwait.go](../pkg/sandbox/serveandwait.go)、[handler.go](../pkg/uffd/handler.go)、[addrmap.go](../pkg/uffd/addrmap.go)。

<a id="fault-path"></a>

### Read path

UFFD使用`fault 1 page + best-effort serial tail`两阶段填充. Fault worker先保证
fault页完成,只有该页实际提交为`Loaded`后才能提交tail;每个handler最多保留一个tail
reservation,并发fault在reservation busy时只处理fault页. Fault页不等待tail worker或
tail ioctl,同时限制并发source I/O、共享buffer和guest population. `ChunkRun`为复用
一次物理解码结果,会在urgent copy前完成下述buffered source read.

填充上限按最终可见Run类型确定:

| Run类型 | fault阶段 | tail阶段 | 单次fault总上限 |
|---|---|---|---:|
| `Hole`、`Zero`、`Released` | 1页`UFFDIO_ZEROPAGE` | 最多15页 | 16页/64 KiB |
| ordinary `Data` | 读取并`UFFDIO_COPY` 1页 | deferred读取并填充最多15页 | 16页/64 KiB |
| manifest `ChunkRun` | 一次读取包含fault页的最终可见chunk窗口,先单独填充fault页 | 先填fault后的suffix,成功后再填fault前的prefix | 1个当前可见window，最多1 MiB/256页 |
| reclaimed `Loaded` | 只恢复当前页 | 无 | 1页 |

`ChunkRun`表示最终serving leaf是一个物理manifest chunk. Chunk 是解密、解压缩及启用时 ordinary content verification 的单位，因此 handler 不再把它按 ordinary Data 的固定窗口反复读取。 `SnapshotReader`
仍只返回从fault offset开始的forward anchor;`StreamSnapshotSource`通过包内可选
capability调用`fetch.ResolveChunkWindow`,以最终组合Stream的元数据把anchor扩展为
同一物理chunk中包含fault的最大连续可见窗口. 解析不读取payload;上层Hole透明,
上层Data或Zero、memory section末端以及不连续的同一lower chunk都会截断窗口,不能
直接使用绕过overlay的物理chunk边界.

Tail reservation在窗口解析及payload读取前取得. Busy时只按原anchor读取并填充当前
4 KiB fault页; 取得 reservation 后, handler 同步将不大于 1 MiB 的窗口读入已有 buffer.
必需源读取暂时失败时, 重新尝试整个窗口.
对完整可见chunk,这是一次精确的whole-chunk读取;若overlay截断可见性,source仍在
内部按 chunk 完成必要的解密、解压及配置启用的验证，handler 只接收可安全填充的连续窗口。 Fault
worker从buffer中间取当前页执行urgent `UFFDIO_COPY`,tail worker先用一个batch填充
fault后的邻接suffix;仅该batch完整成功时,再用一个batch填充fault前的邻接prefix.
Tail执行前从fault邻接页向外重检state,最多缩短为首次不匹配前的连续邻接段;
任一state冲突、partial completion或ioctl错误都会停止更远范围及后续阶段且不重试,
所以单个`ChunkRun`最多发出1次urgent和2次tail copy.

普通Data handler在worker启动前分配64 KiB共享buffer,具有chunk window capability的
manifest handler分配1 MiB,Cold `ZeroSource`不分配该buffer;fault和tail路径不扩容、
不创建临时payload buffer,每个fault worker只持有固定4 KiB urgent buffer. 这是 handler 自身的零新增 payload 分配约束；source内部的Run对象、cache lease及partial-chunk decode
allocation仍由`SnapshotReader`实现负责. 健康路径中, Handler对
非zero source只调用一次`SnapshotReader.RunAt`;可选chunk window resolver仅在
accelerator内部继续执行metadata `Stream.RunAt`. Ordinary Data和zero-like run仍分别
受64 KiB state boundary约束. `ChunkRun`双向候选只包含完整页,并在一次state读锁扫描中截断于
fault两侧连续`PageState`、RAM末端及当前CH UFFD region;这些guest population边界不
缩短已经获准的whole-chunk source read. 默认 CDC chunk 的最大值是 1 MiB，但 Manifest 配置可以在 decoded-format limit 内使用更大 chunk；UFFD buffer/window cap 仍为 1 MiB。扩展 window 不可用、无效或超限时保留原来的安全 forward anchor；buffered window 放不下时，只推进 urgent page，不分配更大 buffer，也不引入超大 chunk 专用 population 路径。源码见 [fault.go](../pkg/uffd/fault.go) 和 accelerator [chunker 配置](https://github.com/kuasar-sandbox/accelerator/blob/main/pkg/manifest/chunker/chunker.go)。

Tail对其连续有效范围只发出一次multi-page `UFFDIO_COPY`或
`UFFDIO_ZEROPAGE`;`ChunkRun`的suffix和prefix各自最多一次. Kernel按页处理并可能
返回已完成的page-aligned prefix;handler只把该prefix条件提交为`Loaded`,第一次冲突
或错误后放弃剩余tail且不重试. 因此batch保留内核已完成的成功前缀,同时把普通
16页策略的tail降为1次ioctl、完整可见 256 页 window 的 255 页 tail 降为最多 2 次 ioctl。

`EVENT_REMOVE`使已丢弃范围重新成为missing,tail提交使用条件状态更新,不能覆盖并发
产生的`Released`. Context cancellation停止worker并关闭fd/stream owner.

UFFD 直接重试必需的 `Run.ReadAt`. urgent 页及包含它的 Chunk window 属于必需读取. 纯 speculative tail 只执行一次最佳努力尝试. 保留现有 `ChunkRun` 能力、buffer、tail 预留和 ioctl 部分进展/EEXIST/EAGAIN 收敛. 所有 vhost GPA/UVA 访问首先通过所属 CH UFFD 等待确认填充；内存准备失败在发布 used-ring 完成记录前终止请求。CPU urgent、后端 mandatory 和 speculative tail 安装与内核 REMOVE/UNMAP 事件读取及状态发布共用同一把锁。延迟读取的来源数据在锁内重新校验；观察到释放后丢弃历史字节，不再安排过期 tail。UNMAP 还会唤醒原 UFFD 范围上的等待者，使内核 GUP/KVM 缺页请求重新检查已删除的 VMA 并退出；它不会安装替代字节，也不会在 runner 停止前 detach 资源。注册、取消、所有权边界与真实共享 memfd/O_DIRECT 回归见 [VMM 集成](vmm_zh.md#patch-0003)。

共享[源读取恢复策略](runtime_zh.md#read-recovery)保留操作 context 和首个终态原因。

<a id="prefetch"></a>

## Memory prefetch

`restore.prefetch: memory` 是 host-only optimization，预热当前 S self 的 file page cache 或 Manifest chunks，不包含 parent memory layers 和 disk streams。调用传入的是 opened root stream，因此范围也可能包含 S 的有界 ZIP metadata tail，并非仅 memory payload section。它不改变 sparse truth、fault ordering、C0、S或memory parents. 默认 `off`。Invalid configured mode 在副作用前验证失败；缺少 prefetch capability 或预取 I/O 失败属于 best-effort，不使 restore 失败，而是记录日志并继续 on-demand。异步任务在 stream 关闭前被 cancel 并 join。源码见 [prefetch.go](../pkg/restore/prefetch.go) 及 [restore.go](../pkg/restore/restore.go) 中的调用。

<a id="checkpoint-changes"></a>

## Checkpoint 内存变化、释放页与历史合并

当前内存层必须保留尚未再次缺页的释放内容。CH/设备 pause、backend quiesce 后，
捕获在安装锁下联合采样 memfd Hole 与现有 UFFD 页状态：驻留 Data 优先，
Hole+Absent 继承父层，Hole+Released 或 Loaded 表示显式 Zero。Loaded 情况覆盖
补零后又被 CH 最终打洞的页。释放锁后流式读取固定布局；捕获成功或失败都不消费、
重置 handler 状态。Zero 在本地合并、tarstream、Bundle/ingest、发布和恢复中保持
不透明。不需要全 RAM 物化、新 ZIP 条目或累计 dirty 日志。参见[释放修复 #378](https://github.com/kuasar-sandbox/sandboxer/issues/378)。

CH 具备累计日志时，默认 `merge_ref=true` 捕获相对实例固定运行基线 B 的变化，并吸收
可合并本地历史。`merge_ref=false` 保留完整当前工作集和必要显式 Zero，把本地历史裁剪成
独立 H。单本地父与多父遵守相同裁剪规则。捕获成功、失败和模式切换均不清空
CPU/CH/Go/释放累计记录，也不推进 B。没有新 CH API 时，两种模式均保留完整当前工作集；
历史仍利用父层已有元数据裁剪。

可选 `memory.dirty` 的精确编码、存在语义、限额与 reader 兼容由 [Snapshot 工件 schema](artifacts_zh.md#memory-dirty)统一定义。以下机制选择并变换这些标注；恢复继续使用完整 Memory。

历史对有效 dirty 候选取并集，最终值从原始完整 newest-first 层叠 Memory 获取。
只有层声明的有序父视图与实际后缀等价时才使用 clean 标注，否则保留该层全部显式内容。
当前 Data 和 Zero 覆盖历史，Hole 继承。执行状态和 sandbox_ref 只取最新 S。
仅物理引用变更时原样保留位图；内存改变或无法证明等价的 rebase（含 rewrite --skip-verify）
须重算或全 dirty。内容或元数据改变产生新 S identity。

输出前检查既有 64 MiB ZIP tail、载体 extent 和索引预算。仅新增位图导致超预算时，
新捕获可以全 dirty 三条目退化；普通 publish 必须保留已有元数据。稀疏映射超限时仅在
旧逐制品覆盖范围内保留额外 Data，字节取当前完整真值；必要 Zero 不得变成 Hole。
退化记录原因和额外 Data，仍受原预算约束。尺寸证据须在相同 codec/父链/状态下包含
payload、位图/头/ZIP 开销、顶层/H 及按身份去重的可达本地制品。
两架构统一 MISSING + KVM/CH/Go 日志，允许 x86 只读物化保守多报。Host Linux 5.10
基线保持不变。能力识别和兼容边界见 [CH 0012](vmm_zh.md#patch-0012)。

<a id="related"></a>

## 相关文档

[Runtime：启动、控制、捕获与恢复](runtime_zh.md) | [Sandbox 配置参考](configuration_zh.md) | [Sandbox 与 Snapshot 制品](artifacts_zh.md) | [块存储：设备、可写状态与 Checkpoint](block-storage_zh.md) | [资源控制与 Usage 计量](resources_zh.md) | [Guest ABI：启动、应用与 Host 协议](guest-abi_zh.md) | [VMM 集成与 Cloud Hypervisor 补丁](vmm_zh.md)

[组件入口](../README_zh.md) · [构建与补丁材料](../native-deps/README_zh.md)
