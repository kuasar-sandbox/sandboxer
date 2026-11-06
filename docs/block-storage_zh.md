[English](block-storage.md) | [简体中文](block-storage_zh.md)

# 块存储：设备、可写状态与 Checkpoint

本页定义完整块存储子系统：不可变下层、vhost 设备边界、共享可写缓存、逻辑 Data/Zero/Hole
状态、DIFF 持久化、checkpoint reader 与垃圾回收。`pkg/vhost` 负责传输，
`pkg/diffstore.Disk` 独立负责可写存储。

库集成从分层/所有权表与 `OpenDisk` 开始；运行中的 sandbox 应先按 checkpoint 命令和完成边界
操作，再管理保留文件。[配置](configuration_zh.md#disk-capacity)定义容量输入及缓存策略，
[Runtime](runtime_zh.md#capture-barrier)定义跨磁盘与 VM 的原子边界，
[制品](artifacts_zh.md#offline-reset)定义离线 E 构造及依赖图保留。单盘提交成功不等于 VM snapshot 成功。

<a id="checkpoints"></a>

## 本地磁盘 Checkpoint

本地磁盘 Checkpoint 需要显式启用。先在启动时选择存储后端，再逐次选择捕获方式：

```bash
sandbox-ctl run --config sandbox.yaml --sandbox-id demo --path-id worker \
  --base-root /var/lib/sandbox --enable-checkpoint
sandbox-ctl snapshot --path-id worker --checkpoint cp-001 --json
# 只需磁盘和配置时改用 export；捕获后继续运行时添加 --resume。
```

`PathID` 默认等于 `SandboxID`。`--config`、`--from` 和 `--restore` 都沿用
`CheckpointDir = BaseRoot/PathID/checkpoint`；根盘和数据盘的可写文件位于
`CheckpointDir/diffs/<DiffID>.diff`，正常退出后保留。不增加第二个启动目录配置。
新启动分配新的磁盘身份。已有 raw/v1/v2 工作文件被拒绝；升级前用旧程序导出，
再把便携制品恢复到新文件。显式指定派生目录之外的可写路径，或文件系统不支持所需锁
操作，会在启动前失败。

启用后端不会改变默认捕获：不带 `--checkpoint` 的 snapshot/export 仍复制磁盘内容。
非空 `--checkpoint` 在同一捕获屏障内为全部可写磁盘创建这个 ID。允许
`--mode local|bundle`，不允许与 `--upload` 或 `export --from` 组合。省略
`--output` 时由运行 owner 提供目录；显式输出必须是同一派生目录。普通捕获仍必须
指定 `--output` 或 `--upload`。默认结束 VM；`--resume` 继续原运行基线。
运行实例不支持该能力时，CLI 返回专用状态码 **78**。

返回的 `sandboxRef` 用于 `run --from`，`snapshotRef` 用于 `run --restore`。
报告中只有文件名的引用必须相对真实输出目录解析。恢复先 pin 全部来源版本，再取得
目标目录 owner。启用 `--enable-checkpoint` 且真实目录相同时，只有每个磁盘的身份、
不可变 base 图、逻辑容量及活动 epoch 都仍与捕获相符，才重开已有 DIFF。捕获后发生
写入、目标目录不同或未启用后端时，会先把固定版本物化为普通 lower 产物。冷启动保留
调用者覆盖；内存恢复沿用已有执行状态规则。来源路径与目标 BaseRoot/PathID 相互独立。

```bash
sandbox-ctl checkpoint list --path-id worker --json
sandbox-ctl checkpoint delete --path-id worker --checkpoint cp-001
# 离线管理需要目录独占所有权；存在运行 owner 时明确报 busy。
sandbox-ctl checkpoint list --checkpoint-dir /var/lib/sandbox/worker/checkpoint
sandbox-ctl checkpoint delete --checkpoint-dir /var/lib/sandbox/worker/checkpoint \
  --checkpoint cp-001 --diff-id ROOT_DIFF_ID --diff-id DATA_DIFF_ID
```

在线管理通过 `RunRoot/PathID/ctl.sock` 定位（也接受 `--sandbox-id` 和 `--run-root`）。
List 一次返回当前及历史 DIFF 的全部可见版本，也包括捕获中断留下的版本；`--json` 输出
精确的 `{diff_id,id}`。删除在修改前固定这组引用。部分失败后，使用重复的 `--diff-id`
重试已报告的精确集合，不要重新查找可能变化的捕获名。逻辑删除后禁止新打开；已 pin
的导出仍可完成，并延后分批空间回收。因此删除会使依赖版本的本地 E/S 失效，但不保证
立即归还全部空间。

```bash
sandbox-ctl checkpoint gc --checkpoint-dir /var/lib/sandbox/worker/checkpoint --json
```

此离线命令取得现有目录独占锁；运行中的目录返回 busy。它使用仅操作元数据的目录
维护，不依赖制品下载、数据解密或密钥。不接受 Checkpoint ID 或删除过滤条件，不改变
保留集合。按 DiffID 输出 `diff_id`、`state` 和已提交的 `freed_slots`，失败时附加
`error`。命令持续处理 `more`，直到每份文件为 `complete` 或 `blocked_by_readers`；
后者是退出码 0 的正常本轮结果，明确表示仍有待回收内容，不保证空间已经释放。
Retire 文件输出 `skipped_retired` 并保持原样。忙、取消以及真实 I/O/元数据错误返回
非零；单个文件失败仍继续输出其他文件结果。纯文字输出明确解释读取者阻挡状态。

运行期维护在登记全部活动 DiffID 后独立启动：普通启动、池实例实际 Launch 和恢复
都借用已取得的目标目录 owner；尚未 Launch 的池实例不接管业务目录。恢复源导出
pin 与目标 owner 是不同资源。历史 GC 和在线 List/Delete 共用有界、串行访问的
元数据句柄，并预留管理容量。历史错误按 DiffID 记录，不终止无关活动沙箱。退出和
初始化失败都先停止新操作及调度，等待已进入操作与当前批次，关闭历史句柄、活动
磁盘，最后释放 owner。已删除版本描述、分组管理页、固定管理区和活动根继续保留；
GC 不截短文件，也不复用已使用的 Checkpoint ID。

```bash
cd /var/lib/sandbox/worker/checkpoint
sandbox-ctl publish --to-ref-location archive=file:///var/lib/sandbox/published \
  --delete-checkpoint --json '<snapshotRef>'
```

全部 publish 分支（包括引用替换/归并及 Bundle 输入）都会物化所选磁盘版本；普通输出
不含 `checkpoint.json`，输入产物保持不变。默认保留本地版本。`--delete-checkpoint`
只在发布及 publisher 关闭成功、自身导出 view 已释放后，删除已选定的精确磁盘/版本
集合，并核对原目录身份。存在运行 owner 时，通过本地 owner 身份路由转发。清理不重新
解析输入别名，也不从 `removedRefs` 推断权限。发布成功但清理失败时，CLI 返回 **79**，
同时保留有效的普通结果或 JSON；应重试清理，不应丢弃发布结果。

SDK 使用同一契约：`RuntimeSpec.EnableCheckpoint`、`RunOptions.EnableCheckpoint`、
`restore.Options.EnableCheckpoint`、`ctl.Request.CheckpointID`、
`Runtime.ListCheckpoints/DeleteCheckpoint` 和
`ProcessStorage.ListDiffCheckpoints/DeleteDiffCheckpoint`。在
`Publisher.PublishWithOptions` 与 `Publisher.Close` 均成功后，可调用
`ProcessStorage.DeletePublishedDiffCheckpoints(ctx, result)`，独立处理其错误，最后
关闭 storage。离线元数据管理及固定版本发布都无需打开 Disk。

嵌入调用方可在 `LaunchSpec` 中提供不透明的 `ProcessStorage`；Checkpoint 物化会
保留它的 manifest fetcher 与本地加密策略。`NewProcessStorageWithLocalAccess`
可以借用 fetcher，`Borrow` 保留完整存储访问能力。关闭借用对象不会关闭原始
客户端；所有借用方结束前，资源 owner 必须保持打开。

块层的 `Disk.Checkpoint(ctx, id)`、`ListCheckpoints(ctx)`、
`DeleteCheckpoint(ctx, id)` 和 `Export(ctx, id)` 使用 `diffstore.CheckpointID`。
命名 `Export` 返回持有 reader pin 的 sparse source 和 closer，用完后必须关闭。
`Export(ctx, diffstore.ActiveDiff)` 则要求调用者保持活动磁盘静止。单层 v3 后端的
命名版本操作返回 `diffstore.ErrCheckpointUnsupported`。

Checkpoint DIFF v4 每盘使用一个本地文件保存稀疏 4 KiB 映射及保留根；物理文件长度
不是磁盘容量。Hole 向下读取不可变 base，显式写零覆盖 lower 数据。加密沿用本地
crypto policy 和进程 customer key；读取磁盘内容需要 key，列举/删除元数据不解密
guest 数据。目录只有一个 writer owner，固定版本的独立 read pin 可与恢复后的活动
写入并存。这是本地 Linux 文件系统能力，不是分布式共享存储。创建版本仍需排空在途
写入并提交元数据；首次模板导入和内存捕获仍有耗时。保留版本占用空间，捕获后首写
可能增加索引和数据 COW。不承诺整个 pause 恒定耗时、所有负载都更快，也不扩大既有
FLUSH 契约之外的宿主崩溃或掉电 guest 数据持久性保证。

重开 v4 DIFF 会加载完整的 presence 位图 P，每个逻辑 4 KiB 页占一位，
32 GiB Checkpoint 容量上限对应最多 1 MiB 净位图。P=0 是 Hole，向下读取 base；
P=1 且没有 payload 是 Zero，遮蔽 base；P=1 且有 payload 是 Data。
已接受的缓存写入也算 Data。普通写零仍保存为 Data，不扫描或压缩零内容。
Open 返回前仍校验所有权、身份/密钥/容量、已提交 redo、根及当前 Checkpoint，
但不扫描整棵 B+ 树。信任不存在结果之前会校验访问的映射及相邻子树边界；
发现损坏进入 fatal 状态，不能返回 Zero 或回退 base。未访问子树的损坏可能延后发现。

原未知状态位图及恢复扫描已移除。Open 后 presence 覆盖完整，dirty-block 统计按 P
计数，包括 Zero 页。导出通过有界查询区分物理 Data；命名导出持有不可变根及 P 副本，
活动导出包含已接受但尚未回写的缓存内容。历史/跨目录恢复仍需物化选定版本，
完整 Zero 页保留为无 payload 的状态。这不消除模板、解密及 Data 复制成本。

恢复来源读者是临时资源。同目录热恢复和冷启动（CLI、SDK `Start` 以及
`StartRuntime` + `Launch`）持有完整来源集合，直到根盘和每个数据盘 writer 全部打开
并登记，再在对外报告成功前关闭来源 pins。历史/跨目录恢复，包括未启用 Checkpoint
的目标，在完整物化及其 publisher Close 成功后关闭。目录 owner、活动磁盘、借用的
存储访问能力及物化 lower 文件继续保留运行所需的寿命。pin 关闭失败会使启动失败；
错误清理也关闭部分已准备资源，重复 pin 清理安全。

释放这些读者不删除 Checkpoint 名称，也不关闭其他 publisher 的固定视图。显式
Delete 可以先撤销名称可见性，真正的导出读者仍保护旧页。最后一个这样的读者关闭后，
现有 GC 可以撤销保护并回收过时内容，无需等待恢复后的沙箱退出。在此之前，受保护
旧页的首次写入可能需要 CoW，已私有页的重复覆盖不会。Delete 仍不承诺同步回收空间。

预期收益最大的是已映射索引较大、启动只访问其中一部分的场景。很小的稀疏索引本来
扫描成本就低，按需状态管理反而可能增加首批负载的总耗时。启动即读取全盘或立即
执行普通导出时仍需完整索引；Open 缩短本身不能证明端到端收益。

`pkg/diffstore` 独立于 vhost 传输负责可写块存储。单层 v3 文件和支持 checkpoint 的 v4 文件
使用同一个 `Disk` 前端；两者均将逻辑 presence 与物理 payload 分开存储。旧 raw/v1/v2
工作文件会被拒绝且不被修改。升级前应使用旧可执行文件导出这些文件；已有可移植 tarstream
仍可读取，并可用于初始化新格式 DIFF。不提供原地迁移。

<a id="storage-layers"></a>

## 分层与依赖

| 层 | 负责 | 不得负责 |
| --- | --- | --- |
| Sandbox / restore / artifact | 磁盘拓扑、不可变 base 组合、VM pause、多盘 checkpoint 协调、发布 | 物理页布局或缓存状态 |
| `pkg/vhost` | 协议、virtqueue、Guest 内存、请求统计；将队列 context 传给 `Disk` | 可写文件、COW cache 或 checkpoint 事务 |
| `pkg/diffstore.Disk` | 操作准入与生命周期、逻辑 presence、部分写入、discard、共享 cache 接入、模板物化与导出边界 | VM 或多盘原子性 |
| 私有 `diffBackend` | 同步逻辑页 I/O、presence、格式相关失败清理、sync 与 close | 前端缓存页或借用 payload 的所有权 |
| 单层 `diffFile` | v3 header、presence 页、稀疏 payload 偏移、独占的 direct-I/O 工作区 | B+ tree 或 checkpoint 引用 |
| 经 `checkpointDiff` 使用的 `internal/versioned` | v4 presence、B+ tree、事务、恢复、版本引用、reader pin、垃圾回收 | 前端准入或 cache drain |
| `internal/directio`、`internal/sectorcrypt` | 共享 mmap 对齐/生命周期与逻辑 512-byte XTS 变换 | 池策略、文件头、认证或映射策略 |

依赖方向为传输/应用 → `pkg/diffstore` → 私有实现。文件系统 preflight helper 位于物理
backend 之上；两个物理 backend 均不解释 ext4。不可变 fetch stream 通过 `StreamReader`
适配为源。Go 的 `internal` 规则使其他包无法直接访问 v4 实现。

公开的 `Store`/`Options` 别名以及 `Create`、`Open`、`OpenMetadata`、`Probe`、`OpenExport`、
`LockDirectory` 函数暴露现有 v4 engine handle，用于**离线**导入、检查与管理。它们刻意与
`OpenDisk` 分开：既不接入 sandbox cache，也不提供在线磁盘的准入/drain 边界。应用不得为在线
`Disk` 打开第二个可写 `Store`，也不得通过离线 API 绕过该磁盘的 checkpoint 边界。行政管理 API
继续复用同一版本管理实现。v4 的 `ErrEncryptionRequired` 与 `ErrAuthentication` sentinel
在所有 v4 入口公开，支持 `errors.Is`；它们与单层 `ErrDiff*` sentinel 不同。

<a id="open-disk"></a>

## 选择格式并打开磁盘

```go
// 默认使用单层 v3；WithDiffEncryption 选择 XTS payload 加密。
disk, err := diffstore.OpenDisk(path, base, init,
    diffstore.WithCache(cache))

// 显式稳定身份选择支持 checkpoint 的 v4。
disk, err := diffstore.OpenDisk(path, base, init,
    diffstore.WithCache(cache),
    diffstore.WithCheckpoint(diffID, baseID),
    diffstore.WithDiffEncryption(customerKey, true))
```

使用磁盘前必须检查 `err`。`DiffInit{Existing: true}` 打开已有文件；`CreateSize` 或
`TemplatePath` 创建新文件。v4 open 会认证并检查预期 identity/base，绝不接管旧格式文件。
即使新 active file 必须加密，仍允许使用明文 raw provisioning template。

| 属性 | 单层 v3 | 版本化 v4 |
| --- | --- | --- |
| Runtime | 相同 `Disk` 与共享 `Cache` | 相同 `Disk` 与共享 `Cache` |
| 已有文件 presence | 完整加载逻辑 P；稀疏扫描仅识别物理 payload | 完整加载逻辑 P；有界 B+ 查询识别 payload |
| Writeback | 有界 batch 可包含不连续页；每个变化的 P 页每 batch 只发布一次 | 有界事务可包含不连续逻辑页 |
| 模板 staging | 最多 64 页（256 KiB） | 最多 `maxWritebackPages`（256）个 4 KiB 页 |
| Checkpoint 能力 | 不支持 | `CheckpointManager` |
| 物理失败清理 | 仅 punch 新创建的 upper 页 | 事务/分配 engine 负责恢复；不得在混合物理文件按 Guest 偏移 punch |

私有 batch 包含逻辑块号、借用的明文字节和 upper 页是否为新建，不暴露 `cachePage`。Cache
冻结并计费这些页，直到写入及失败清理结束；清理可能阻塞前，先发布 sticky failure。单层 backend
在现有工作区 staging；v4 执行其现有 mapping commit。缓冲池、请求 batch 上限及加密域沿用各自策略。

新建共享 claim/recheck/template/temp-file 流程；发布共享 file sync、原子 rename、claim 释放和
directory sync。物化使用同一 dispatcher 以及相同 stripe/presence 冲突检查。v4 保留分批模板提交；
单层每批最多 64 页，以摊销 presence 发布和连续 payload 写入。其临时 staging 只在物化期间从
4 KiB 增至 256 KiB。模板字节在获取写 stripe 前读取，随后重新检查 presence，让前端 write/discard
优先。因此，被完全覆盖的范围仍可能先被读取，再被跳过。`WaitMaterialReady` 与 `WaitMaterialized`
分别保留不同的就绪和发布边界。

新建且启用 checkpoint 的在线磁盘异步初始化格式，无论是否有模板。准备阶段仍校验 identity、
容量（包括模板推导容量）、加密和排他所有权。`OpenDisk` 返回前建立独立的空前端 P bitmap 和现有
有界 cache。离线 `Create` 同步运行同一个 initializer；已有文件 open 仍校验已提交元数据并加载
真实 P bitmap。不新增文件格式或配置开关。

| 边界 | 已完成的工作 |
| --- | --- |
| `OpenDisk` 返回 | 已校验所有权和逻辑视图；缺失的 upper 页读取 template/base/zero，缓存写入可进行；最终路径可能尚不存在 |
| 内部初始化完成 | truncation、固定预留及现有 header/control/redo/分批 bitmap 写入完成，或记录一个终态错误；backend I/O 与 GC 等待成功 |
| `WaitMaterialReady` 成功 | 初始化完成，视图不再依赖 template；此边界早于 cache drain，使 checkpoint 能冻结写者 |
| `WaitMaterialized` 成功 | 初始化、可选模板导入、所需 writeback、file sync、rename、claim 释放和 directory sync 完成；不同步后续写入 |

空 `Drain` 不等待初始化。物理路径交接/重开、active export 和 snapshot 发布使用
`WaitMaterialized`。不可变设备 identity、size、health 检查及新 checkpoint 的失效操作不等待格式
I/O。Discard 与模板 backfill 在获取前端 stripe 前等待初始化。唯一 cache worker 在有界队列中
选取最早且可执行的 dirty 页，冻结前跳过尚未 ready 的 backend；即使 pending 前缀超过一个 batch，
也不能遮蔽后面的 ready 工作。Pending 页仍可读写，但占用共享额度。全部工作 pending 时，worker
休眠，直到完成、新的可执行工作、失败或 shutdown 唤醒。

初始化在可能失败的前端接入之前只准入一次，构造失败时直接 join。请求取消仅影响等待者；Close
必须 join 实际初始化 I/O 后才能释放文件、锁或缓冲区。成功初始化先建立 GC 所有权，再发布完成。
Guest 进程创建与这些工作重叠时，现有有界 MAP_SHARED direct-I/O 工作区仍安全。不引入逐页初始化状态。

迟到的初始化/发布 I/O 错误是 sticky runtime failure，可能在缓存写入已被接受后发生。完成状态先于
fatal callback 发布，晚注册 handler 的 replay 也遵守此顺序。返回后失败的磁盘保留 template reader
和私有资源，直到 Close 停止准入并 join 现有使用者。Disposable Close 可在初始化之后停止未完成模板
工作并阻止发布，但不能取消 initializer，也不能隐藏已准入发布的真实错误。Retained Close 正常 drain
并发布。主动丢弃可以让 Close 成功，但未达到边界的 readiness/publication waiter 收到 cancellation。

异步流程移除可避免的前台等待，不减少初始化工作。提前使用 backend、缓存压力、立即 checkpoint/Close
以及并发磁盘初始化仍可能等待或争用 I/O。比较同步分批创建基线与候选实现时，必须使用相同 bitmap
路径、容量、加密、模板和 cache budget；除 logical open、首次有效 I/O、Ready、publication 与 Close
耗时，还要保留端到端启动尾延迟和失败数据。

<a id="ownership-and-completion"></a>

## 所有权与完成边界

- `Disk` 拥有 backend、materializer 及其 cache attachment。不可变 `BlockReader` base 是**借用**的；
  必须保持打开直到 `Disk.Close` 完成，再单独关闭。`StreamReader.Close` 关闭被包装 stream，不关闭独立持有的 fetcher/cache client。
- 未使用 `WithCache` 时，`Disk` 创建并关闭私有 cache。使用 `WithCache` 时，调用者拥有共享 cache，
  必须先关闭所有接入磁盘，再关闭 cache。一个 sandbox 的可写设备仍共享一个 budget 和一个 worker。
- `ReadAtContext`/`WriteAtContext` 将队列取消传播跨过包边界；`ReadAt`/`WriteAt` 保持常规无 context 入口。
- Guest `Flush` 仅检查 health，不承诺介质持久化。`Drain` 等待已接受的缓存写入完成；checkpoint
  还执行版本化持久边界。请求被接受或 Drain 完成均不等于 durable checkpoint。
- `Disk.Close` 排除新操作并 join 已准入工作后释放存储。`WithDiscardOnClose` 允许为可丢弃且由 runtime
  持有的磁盘取消未完成模板工作，不转移 base 所有权。

<a id="checkpoint-readers"></a>

## Checkpoint 与导出 reader

```go
manager, supported := disk.Checkpoints()
if supported {
    err = manager.Checkpoint(ctx, diffstore.CheckpointID("snapshot-1"))
}
```

Manager 与磁盘共享生命周期，经在线准入/cache 边界执行操作。Checkpoint 先等待 template ready，
暂停新前端操作、drain 已接受写入、等待 materialization publication，再提交命名版本。多盘仍需应用层
协调。现有直接 `Disk` checkpoint 方法继续可用；`Checkpoints()` 让调用者显式判断格式能力，无需用错误试探。

`OpenActiveView(ctx)` 返回**仅含 upper** 的稀疏视图，包含已接受的缓存写入。调用者须保持磁盘打开且
quiescent，直到视图关闭；它既不是持久 checkpoint，也不是与 base 组合后的完整磁盘镜像。
`OpenCheckpoint(ctx, id)` 要求非空 ID，返回带独立 pin 的不可变 reader。后续写入或 owner close 不影响
该视图；关闭视图才释放 pin。`Export(ctx, ActiveDiff)` 仍是 active view 的低层写法；
`OpenCheckpoint` 拒绝空 ID。

在线重开加载完整逻辑 presence bitmap，不全扫描 B+ tree。命名导出加载自己的不可变 bitmap 和 root。
Payload 查询校验访问到的 index 页及相邻子树边界；损坏始终为错误，不能当作 Zero 或下层 Hole。
Open 不审计未访问的子树。

<a id="presence-and-layout"></a>

## 逻辑 presence 与物理布局

可写逻辑容量必须为正且可被 4096 整除。单层 v3 最大 1 TiB；checkpoint v4 仍限 32 GiB，因为 active
bitmap 位于 record area 前的固定预留区。限制适用于 active DIFF 与模板推导的可写容量，不包括只读设备、
RAM、制品元数据尾部或旧版本保留的物理存储。每 4 KiB 页一位 presence：1 TiB 磁盘净 bitmap 最多
32 MiB，32 GiB 磁盘最多 1 MiB。这是每个 bitmap 的大小，不是整个 backend、前端或 cache 的内存上限。

共享容量检查允许最大 1 TiB；v4 backend 在创建和 open 时独立检查 32 GiB 上限，并在新 checkpoint
文件初始化前拒绝。上层 template 或 runtime 准备可能在此拒绝之前进行。

| P | 有效 payload | 逻辑结果 |
| --- | --- | --- |
| 0 | 忽略，即使存在过期物理字节 | Hole；读取下层 |
| 1 | 无 | Zero；不占 payload 页但遮蔽下层 |
| 1 | 存在，包括已接受缓存写入 | Data |

普通写入设置 P 并存 Data，不扫描零字节。对 Zero 的部分写入从全零字节开始，不读下层。Template 与
merge 组合遇 Zero 停止；完整模板 Zero 页只设 P，不写 Data；部分 Zero 范围按普通页组合处理。
Modification fence 同样覆盖仅 P 变化。Guest FLUSH 与物化持久性保持原语义。

单层 v3 以 4096-byte header 开始，描述逻辑容量、body offset、bitmap offset/length/page count 和加密。
其后为带 checksum 的 bitmap 页；每页包含 32-byte header、4060 bitmap bytes 和 4-byte CRC。
全部 bitmap 页之后才是 body。Payload extent 枚举排除 header/P allocation。XTS 使用逻辑 512-byte
sector，绝不解密物理 hole。先写 payload，再发布变化的 P 页；discard 先 punch，再发布 P。
即使从 P=0 转为 Zero，也先移除残余 payload。Zero 的首次 Data 写入视为 fresh，使失败清理可恢复其已提交 P 状态。

版本化 v4 在物理页 1600 放置相同 bitmap 页，位于有界 redo 区之后、页 2048 开始的 record area 之前。
创建时预留，Open 时加载。变化 P 页使用现有原子元数据事务；恢复和只读 redo 解释均校验其精确合法范围。
Backend 串行化 bitmap 更新并合并变化 bits/pages；前端绝不以过期完整 bitmap 覆盖 backend P。

Checkpoint 在普通 allocator record 中原子保存 root 和独立完整 P 副本（最多 259 页、净 1 MiB），
不复制 Data。开销包含 bitmap 复制、分配及事务元数据，也包含碎片 free slot 的成本；redo 上限仍为
1536 个已有 page image。Reader pin 同时保护 root 可达 Data 和 bitmap record。Delete 仅撤销版本并
标记 pending。Bounded GC 将独占 P record 直接转入全局 reclaimable list、持久化部分进度，并使该版本
保持 pending 直到副本全部处理完成；此流程不需要 payload 解密 key。

<a id="discard-and-reclamation"></a>

## Discard 与回收

`Disk.Discard(offset, length)` 只修改完整覆盖的 4 KiB 页，保留部分边缘页内容。它移除 active payload
并设置 P=1，返回 Zero 而不暴露 base 字节。公开 `Store.Discard(ctx, offset, length)` 提供同一完整操作，
要求页对齐范围。PayloadPresence 为 false，不再存在 active Data mapping；保留的 Checkpoint 仍可能
保护旧 record。Guest DISCARD 使用同一操作，不引入 WRITE_ZEROES 或零压缩。

每 batch 最多 64 个完整页，按数值顺序取得不同 stripe 锁，在一个 cache 锁下检查所有已有缓存页，
随后一次性冻结它们。若有 loading 或 writeback，整个 batch 等待而不冻结任何部分。未缓存页不需要
placeholder，不相关 dirty 页不必 drain。Backend I/O 在 cache 锁外执行并接收 context。取消可在冻结
前停止 batch；冻结后必须发布整个 batch 或进入现有 sticky failure 路径并唤醒等待者。成功时为全部页
发布 Zero，包括此前 absent 的页。普通 presence 位本身阻止 template Data/Zero backfill 覆盖前台内容，
不另设 template-resolution bitmap。未改动的 template Hole 仍落到 base。不同 batch 之间不保证整个
range 全有或全无。普通 presence 发布只置位；首次 payload 写入失败则在清理新分配 payload 后恢复旧的
已提交 bitmap：Hole 保持 P=0，Zero 保持 P=1。

`DeleteCheckpoints` 只撤销可见性，无 reader pin 时立即释放保护；既不执行也不等待 GC。
`OpenMetadata` 与 List 同样不调度 GC。Active store 有独立 worker；metadata-only owner 须显式调度维护。

`Store.GC(ctx, budget)` 返回 `GCResult{State, FreedSlots}`。`Complete` 表示没有 pending version 或
全局 reclaimable record。`More` 表示有界工作尚未结束，即使 `FreedSlots` 为零也必须重试。
只有全局 reclaimable chain 为空，且一个有效完整轮次发现所有剩余 pending version 都被 pin 时，
才返回 `BlockedByReaders`。稍后 reader close 即可推进，无需再次 delete。`FreedSlots` 仅统计已提交到
可复用 free list 的位置，不统计扫描、转移或 punch 尝试。

Version visit、predecessor 查找及 ownership transfer 均有界；全局释放与 version 工作轮流使用完整
有界 batch（包括 budget 1），避免二者均 ready 时做两次半 batch 元数据提交。Active worker 在 `More`
batch 之间于 store 锁外等待 1 ms，shutdown 可中断，以减少前台竞争。显式 GC 与有界前台分配协助不受
此延迟影响；代价是后台峰值回收吞吐降低。

纯 cursor 和 predecessor 搜索进度存内存，不写 redo。一轮捕获自己的 tail，新 Checkpoint 不能无限延长
该轮。普通写入不重置轮次，新 delete 会使过期的 blocked 结论失效。重开可重复纯扫描，已持久化的
ownership 变化仍可恢复。Active worker 在 `More` batch 间让出执行，并降低 reader-blocked 工作检查频率。
分配失败可执行有限前台协助，仅在已提交释放后重试；GC 不得经协助路径递归分配。

仅 punch 全局 reclaimable chain。每批校验候选唯一性和合法状态，按物理文件页地址排序并合并物理相邻
范围。每 128 个 record 位置之间的 descriptor 页和所有 live 页均分隔范围。Punch 完成后，使用一次现有
metadata/redo 事务移除旧 chain entry 并发布 free descriptor。Batch 按地址升序连接，再接原 free head。
提交前失败不发布 free slot；重开可安全重复 punch。GC 不引入新格式版本、更大 redo 区、payload relocation、
文件截断或全局 free-range index。加密和 direct-I/O 对齐沿用现有规则。

`DirectoryMaintenance` 借用已取得的 `DirectoryOwner`，校验稳定锁身份，并在枚举前注册全部 active
DiffID。它不为 active disk 获取第二个 writer，也不释放 owner。历史枚举按有界 batch 进行；两个 GC
handle 加一个临时管理 handle 限制同时打开的 metadata writer 数量。List/Delete 共享已有 GC handle，
在 GC batch 之间串行执行请求。即使两个 GC slot 都被占用，另一个历史文件的管理仍可使用预留 handle。
启动和周期 sweep 按 DiffID 报错并继续其他文件；跳过 retired 文件，不调用 Retire 或 unlink。
Close 停止准入和调度、join 已准入操作/当前 batch，再关闭历史 handle。调用者随后关闭 active disk，最后关闭 directory owner。

回收不决定保留策略。已删除 Checkpoint descriptor、descriptor group 页及固定管理区仍保留，历史 DIFF
的 active root 也保留。文件不必恢复初始分配大小或长度；已使用的 Checkpoint ID 不可复用。独立的 Retire
仍负责整文件撤销/删除。在线与离线管理用法见[磁盘 Checkpoint](block-storage_zh.md#checkpoints)。

<a id="migration-and-validation"></a>

## API 迁移与验证

| 旧入口 | 当前入口 |
| --- | --- |
| `vhost.OpenBlockCOW`、`vhost.BlockCOW` | `diffstore.OpenDisk`、`diffstore.Disk` |
| `vhost.BlockCOWOption` | `diffstore.DiskOption` |
| `vhost.NewCOWCache`、`WithCOWCache`、`COWCache` | `diffstore.NewCache`、`WithCache`、`Cache` |
| `vhost.COWCacheStats` | `diffstore.CacheStats` |
| `vhost.NewStreamReader` / `StreamReader` | `diffstore.NewStreamReader` / `StreamReader` |
| `internal/diffstore` 管理 API | `pkg/diffstore` 管理 API |
| 原 `pkg/vhost` 存储测试 | `pkg/diffstore` |
| 原 `internal/diffstore` B+ engine 测试 | `pkg/diffstore/internal/versioned` |

表中的早期包迁移改变 Go import，不改变 CLI、配置或磁盘字节。引入 presence 的 v3/v4 格式是独立且不兼容
的工作文件变更：升级前必须用旧版本导出保留 raw/v1/v2 工作文件。Provisioning template 仍是不可变输入，
保留既有 raw/encrypted 输入契约；reader 不把源重新打开为 active work file，也不升级源。现有 raw ext4
准备与旧 portable tarstream 导入继续支持。

仓内调用者均已使用迁移后的 API。`vhost.BlockReader` 仍是只读传输设备所接受源的类型别名。存储 benchmark
flag 和 `VHOST_*` 环境变量名保持兼容，但编译测试包现为 `./pkg/diffstore`。现有 ENOSPC 脚本名仍为 CI 入口。

运行 `go test ./...`、`go test -race ./pkg/diffstore/... ./pkg/vhost ./pkg/sandbox ./pkg/restore ./pkg/artifact ./pkg/snapshot`、
`go vet ./...`、`make test-e2e-scripts`、`bash scripts/test-vhost-tmpfs-runner.sh` 以及真实
`scripts/test-vhost-tmpfs-enospc.sh`。Direct-I/O 测试要求 `TMPDIR` 所在文件系统受支持；userfaultfd transport
测试和隔离 ENOSPC 要求相应文档列出的权限。Skip 或前提失败不能算作验证通过。

契约测试覆盖两种格式的 plaintext/XTS、base 上部分写入、共享 cache 所有权、取消、active sparse export、
named-view pin 和不支持 checkpoint 的能力结果。格式、损坏、恢复、short-I/O、失败清理、并发和传输测试
保留在各自所属层。性能方法与解释见本页[可重复 COW 测量](block-storage_zh.md#cow-measurement)和
[Checkpoint 验证](block-storage_zh.md#checkpoint-validation)。<a id="payload-boundary"></a>

## Payload boundary

Vhost backend接收`fetch.Stream` Payload section,不是FullStream. 任何 `.sandbox` ZIP tail进入block logical size都属于bug,由format/unit tests覆盖.

<a id="layered-reads"></a>

## Layered reads

Read 顺序为 active diff -> captured top -> `base_from_refs` -> root image（适用时）。Hole fall through，Zero/Data stop traversal。同一 writable block device 中组合的 immutable layers 的 logical size 必须一致。EROFS + ext4 拓扑中，read-only EROFS base 是独立 vhost device，不是 writable ext4 Disk device 最后一层 fall-through。源码见 [disks.go](../pkg/sandbox/disks.go) 与 [serveandwait.go](../pkg/sandbox/serveandwait.go)。

`StreamReader` 同步重试完整读取. 运行队列 context 仅在本次调用替代准备阶段 context, 因此旧 master session 停止时先取消挂起读取, 再 join worker 并替换 memory table. COW 适配层将该 context 传入 base materialization, 同时保留块锁、dirty bitmap 和部分写算法. 不重放整个 `WriteAt`、`FLUSH`、Store `Put` 或快照操作.

`processChain` 和 `processQueue` 都识别停止或终态的必需读取. 此时 status byte、used ring 和队列 base 保持未完成. COW 写入内部的基底读取同样遵守该规则. 普通不支持请求及独立的可写 diff 错误保留现有协议行为.

共享[源读取恢复策略](runtime_zh.md#read-recovery)保留操作 context 和首个终态原因。

<a id="snapshot-view"></a>

## SnapshotView

CH pause 且前台 quiesce 后，`SnapshotView` 复制逻辑 bitmap 并提供 upper-only 明文，
优先读 dirty/writeback 缓存页，再读文件。不强制 Drain/fsync，也不复制整份缓存。
后台可继续搬运相同逻辑内容。只有文件成功完成后，最后一份缓存副本才能淘汰。捕获完成前
保持前台 quiesce 且 COW 打开。捕获期间后台 fatal 必须使发布失败并到达 runtime owner，
包括故障后没有新 guest 请求的情况。Export 不 rotate active diff 或改变 base。

<a id="disk-state"></a>

## Disk 状态与 I/O

统一存储入口为 [`pkg/diffstore`](../pkg/diffstore)，详见[抽象层次、所有权与迁移说明](#storage-layers)。`pkg/vhost` 仅负责传输协议。

活动可写 diff（包括 tmpfs 上的文件）通过同一定位 I/O API 请求 O_DIRECT。
运行时策略不识别底层文件系统类型。
`sandbox-ctl` 为每个沙箱持有一份有界明文页缓存；root 与数据盘共享预算，包括混合使用
tmpfs 与磁盘的情况。这实现了 [issue #230](https://github.com/kuasar-sandbox/sandboxer/issues/230)，
并由 [issue #238](https://github.com/kuasar-sandbox/sandboxer/issues/238) 恢复 tmpfs 兼容性。
改动范围是活动数据访问、配置与生命周期；immutable base、template、历史 overlay、manifest
解密缓存、制品输出、tarstream 表示、cgroup、balloon 和 guest 资源预算保持现有行为。

<a id="logical-state"></a>

### 逻辑状态与记账

分层为 `Disk -> 明文缓存 -> diffBackend -> 单层或版本化存储`。
页大小为 4096 字节。Base 读取不会填充缓存或物化 upper。缓存按活动文件与块号索引。

| 状态或转换 | 总占用 | 脏占用 |
| --- | --- | --- |
| Clean、dirty、writeback、read loading | 每页计一页 | 仅 dirty/writeback |
| 新写入构造/预留 | 一页 | 一页 |
| Clean 变 dirty | 不变 | 加一页 |
| Dirty 覆盖或变为 writeback | 不变 | 不变 |
| 回写成功变 clean | 不变 | 减一页 |
| 淘汰 clean | 减一页 | 不变 |

始终满足 `dirty_used <= used <= cache_size` 且 `dirty_used <= max_dirty_size`。
提交 I/O 不释放预留。页池、索引、clean LRU 与 dirty FIFO 都以容量为界。释放时清除明文。
独立有界开销包括每沙箱最多 256 个写回页指针, 每个活动文件两个独立
1 MiB MAP_SHARED I/O 工作区（每个额外对齐 padding 小于 1 MiB），以及随页数线性有界的
页元数据、索引和 Go 分配器开销。`cache_size` 不是进程 RSS、guest memfd 映射、
immutable-source cache、tmpfs 文件存储或快照输出内存的上限。

Tmpfs 文件页是位于内存（也可能位于 swap）中的文件系统存储，与这份有界进程缓存分开。
回写成功会释放脏额度，但不会释放已存入 tmpfs 的文件数据；淘汰 clean 缓存也不会删除它。
`cache_size` 不限制这些字节，也不承诺 tmpfs 页零驻留。该存储由现有 tmpfs 挂载的大小、
inode 限额和宿主内存控制约束。Sandboxer 不重挂 tmpfs、不改变 cgroup、不扩大缓存预算，
也不新增持久化保证。参见 [Linux tmpfs 文档](https://www.kernel.org/doc/html/v6.1/filesystems/tmpfs.html)。

Bitmap 表示**逻辑 upper-present**，独立于 clean/dirty 状态。新页完整发布到缓存及 bitmap
后才应答写入。回写成功或淘汰不清除此 bit。重开时从已提交 P bitmap 加载逻辑 upper presence；稀疏文件 extent 只描述物理 payload。缓存持有领先于物理文件状态的最新明文。

<a id="cache-and-backpressure"></a>

### 读写与背压

读取优先从 clean、dirty 或冻结的 writeback 页复制。只有 miss 才读活动文件并解密。
Read loading 预留总容量；没有可用 clean 槽时，可以用有界前台工作区直接服务读取而不入缓存。
绝不能绕过较新的缓存页。

运行读取与快照读取共用有界 upper 连续区间批处理。仅本次请求的连续冷 upper 页合并为
一次物理读取（最多 1 MiB）；缓存命中、base 或 hole 边界会结束该区间。按 stripe
编号排序的一次性范围读锁防止并发写入/Discard，Loading 预留防止淘汰。缓存容量小于
区间时，剩余冷页在相同 stripe 保护下直接读取但不入缓存。读取/解密保持在自有 MAP_SHARED I/O
工作区内. Loading 页在 cache mutex 外从该工作区填充, 再持锁发布为 clean,
之后才复制到可被调用者或 guest 修改的输出内存. 本次有界请求范围内的连续 base 块
合并交给 `BlockReader`, 下层读取边界仍由该 reader 决定. 不新增与请求长度成比例的
payload 分配或 base 预取.

新写在构造前同时申请总量与脏额度。Clean 变脏只需脏额度；未被选中的 dirty 页重写不追加
额度或队列节点。完整页覆盖不读旧数据。首次部分写从 base（或零）构造完整页，保留现有
短 base/错误处理语义。后续部分写保留所有未修改字节。完成前复制 guest 数据，不持有
完成后的 descriptor buffer。大于缓存的请求逐页推进。

容量满时先淘汰最久未用 clean 页，否则等待回写。额度等待可取消，唤醒后重检状态；等待者
不持有 worker 推进所需的页预留。一页容量与所有页在途都必须可推进。请求不创建后台
goroutine 或无界 payload 队列。块条带锁序列化前台同块访问；worker 不取得这些锁。

<a id="writeback"></a>

### 回写与 FLUSH

每沙箱一个 worker 以最老脏页作为每批的锚点。热点覆盖不移到队尾。可合并同文件相邻脏页，
每批最多 1 MiB，不填 hole 或预分配间隙。固定 1 ms 聚合窗口保证低速流量推进；额度压力与
内部 Drain 立即唤醒. 选中的页成为冻结 writeback, 仍可读取, 同页写等待. Worker 将冻结页
直接复制到 active diff 已有的 aligned MAP_SHARED 写工作区, 在该副本上加密,
然后在不持有 cache/global 锁时执行 I/O. Cache page payload 保持明文, 不直接作为
syscall buffer. 前台读有独立工作区.

Worker 通过有界页索引收集同文件连续脏邻居，不要求到达顺序相邻；未选中页的 FIFO
年龄保持不变。普通写入通知既不提前结束，也不重启固定聚合截止时间。压力和 Drain
会中断等待。已聚合的积压立即推进，即使每批只有单页，也不会逐批再等待 1 ms。

健康且合法的 guest **FLUSH 是 no-op**，不发起回写、不等待脏页、不调用 Drain、fsync 或
fdatasync。请求顺序和 fatal 状态检查保留。为兼容 Cloud Hypervisor 快照，保留现有 wire
features。新建可写设备公布 DISCARD，只读设备不公布；不增加 CONFIG_WCE 或
WRITE_ZEROES。参见 [Guest discard ABI](guest-abi_zh.md#guest-block-discard)。

写入完成仅承诺完整最新字节已进入有界逻辑 COW 状态。写入与 FLUSH 都不保证
`sandbox-ctl` 崩溃、宿主断电或存储故障后的数据存续。这明确不同于
[Virtio 1.2 §5.2.6.2](https://docs.oasis-open.org/virtio/virtio/v1.2/virtio-v1.2.html)
要求的稳定存储 FLUSH。这是项目的非持久化 COW 契约，不是标准持久化 FLUSH 行为。
Direct I/O 本身也不是持久化屏障。

<a id="active-file-io"></a>

### 活动文件 I/O 契约

每个活动 body 都在其已打开的描述符上尝试 O_DIRECT，并始终使用相同的有界对齐工作区。
如果该 F_SETFL 请求返回 EINVAL 或 EOPNOTSUPP/ENOTSUP，描述符保持普通定位 I/O，
应用缓冲和缓存不变。这只是在初始化时处理操作是否受支持，不识别文件系统，也不是数据 I/O 失败后的重试。
新目标在 **seeding 前**完成上述初始化；已有活动文件校验、运行与快照读取使用同一路径。
运行时代码不检查文件系统类型、名称、挂载策略或文件系统专有 inode flags。
Header/格式探测在并发 body I/O 前有界完成；template/base 保留 buffered/read-only 行为。
明文与密文分别采用单层 v3 或多版本 v4 格式，均含固定 4 KiB header 及带校验的 P 页。
本地加密 off/auto/required 策略和 512 字节逻辑 XTS 数据单元编号不变。

对已打开 fd 查询 `STATX_DIOALIGN`，区分 buffer 地址与 offset/length 约束。
报告的正数约束必须满足工作区上限与 4 KiB COW 几何要求；偏移对齐必须整除 4096，
且 body 边界和大小满足要求。缺少 mask、查询返回不可用的 ENOSYS/EINVAL/EOPNOTSUPP，
或两个对齐字段同时为零时，选择保守的 4096 字节对齐。这仅是对齐选择，不证明缓存绕过。
只有一个字段为零或其他无效约束、真实 statx/描述符错误、
其他设置标志错误及实际 I/O 失败仍然报错。应用层不会用 buffered 重试掩盖 EIO、ENOSPC、
对齐错误或短 I/O。参见 [statx(2)](https://man7.org/linux/man-pages/man2/statx.2.html)。

内核及底层实现决定如何满足 O_DIRECT 请求。成功设置标志并完成对齐 I/O 证明操作可用，
不证明所有底层存储都会绕过物理磁盘缓存。Tmpfs 文件页仍是 `cache_size` 之外的内存/swap
存储。没有文件系统白名单、专用 tmpfs 后端、新配置模式或数据 I/O 失败后的回退。
参见 [open(2)](https://man7.org/linux/man-pages/man2/open.2.html)。

I/O 使用有界、对齐的匿名 `MAP_SHARED` 工作区，其 lifetime 覆盖 syscall 完成，避免
private heap/fork 风险。任意调用者切片和子页读取经过这些 buffer，大操作分块。
缓存整页回写避免读改写。短写报错，不重试未对齐余下切片；参见
[write(2)](https://man7.org/linux/man-pages/man2/write.2.html)。

<a id="versioned-format"></a>

## 多版本 DIFF v4 存储与恢复

可选后端位于 [`pkg/diffstore/internal/versioned`](../pkg/diffstore/internal/versioned)。每个逻辑 4 KiB 块经
B+ 树映射到物理槽。4 KiB 索引页包含 16 字节页头和最多 255 个 16 字节项；
叶项保存逻辑块到数据槽的映射，内部项保存子树最大逻辑 key 及对应子索引槽。
项中的整数使用 little-endian；数据槽与索引槽独立。header 绑定 DiffID、不可变 BaseID、
逻辑容量和加密策略；control 页保存活动根、epoch、当前捕获标记、版本链、空闲/垃圾链
。原 cursor 字段保留，纯 GC 扫描进度改为只保存在内存。版本记录保存 ID、根、epoch、前驱/后继及 visible/retained 状态；
槽 descriptor 保存出生 epoch、种类和链指针。512 页有界元数据缓存不为全部版本
建立全盘逻辑块映射。原始页和解码节点共用此预算，每个 owner/view 约 4 MiB。
四个共享匿名 8 MiB direct-I/O 工作区将进程级对齐暂存空间固定为 32 MiB，
不随版本数增加；共享映射可安全跨越并发 fork。XTS 使用 512 字节逻辑扇区编号，
不随 COW 选择的物理槽改变。

活动读取仅在相邻逻辑页对应的物理数据页连续时合并，每次 direct read 最多
256 页（1 MiB）。分配器 descriptor 页、元数据槽、孔洞和碎片映射会截断连续段。
活动 owner 在查询、读取、解密及复制期间一直持有 Store 锁，防止并发写入和 GC
改变槽的生命周期。payload 直接从既有共享工作区复制，该工作区归还前会清零，
不再为每页 payload 分配 Go 堆副本。短读只交付完整页前缀，未完成后缀保持原样。
此优化不改变 DIFF 格式或回写、持久化语义。较大的冷连续读取可获益；单页随机
读取、碎片数据及 COW cache 命中几乎没有合并机会。XTS 仍需逐逻辑扇区解密，
整次调用持锁也仍会使并发存储操作串行执行。

物理页号与 guest 逻辑块号不同：

| 物理页 | 内容 |
| --- | --- |
| 0 | 不可变 header：既有 DIFF magic、big-endian version 4 前缀、身份/容量和封装密钥 |
| 1 | 可变 control 页 |
| 2 | redo commit descriptor |
| 3–5 | redo home-address 索引，不是 B+ 索引页 |
| 6–1541 | 最多 1,536 个已有元数据页镜像 |
| 1600 起，最多 259 页 | 活动 P 位图，确切页数由容量决定 |
| 2048 起 | 每组一页 descriptor，随后 128 个槽 |

创建时预留 0–1541 页（6,316,032 字节）及活动 P 页，使新 payload 分配遇到空间不足时仍能写
redo 和既有 control 页。初始稀疏文件长度为 8 MiB，不是 guest 容量。元数据页包含
校验和；加密 header 使用 AES-GCM 封装随机 64 字节 XTS key，不可变 header 字段
作为 associated data，并使用 v4 专用 header-key 派生域。

创建版本原子登记活动根及独立完整 P 副本并推进 epoch，不扫描或复制 Data。
P 副本最多使用 259 个普通分配记录；空闲槽碎片化时也使用原 redo 预算。Pin 同时保护
根和 P。Delete 不回收 P 记录；有界 GC 持久化 P 副本的部分回收游标，将这些独占记录
直接转入全局垃圾链，再结束已删除版本的待处理状态。出生时间晚于最新 retained
epoch 的槽属于活动版本私有；反复覆盖可原位写入，不改索引、不产生元数据 redo。
共享的数据槽及其共享索引路径通过 COW 保护。guest 写入修改明文 COW cache 之前，
`MarkModified` 每个捕获周期先清除一次当前捕获标记；直接后端写入也执行相同屏障。
仅比较根指针相等不能证明磁盘未变化。

写入批次最多 64 页。元数据事务在任何数据或元数据落盘之前检查固定 redo 预算：
最多 1,536 个已有元数据页镜像，由三页 redo home-address 索引寻址。先为新的不可达数据/索引槽
预留空间并写入，再发布 redo；只有已有元数据页进入 redo 镜像。带校验的 commit
descriptor 授权重放，随后更新 home metadata 并清除 descriptor。不能为了适应预算
拆开一次结构变更。分配空间不足时可执行有界前台 GC 后重试，不能覆盖保留版本的数据。
私有 payload 覆盖在一次 epoch 屏障之后无需索引事务。guest FLUSH 仍是非持久化
健康检查，不触发 writeback、Drain 或文件系统 sync。初次发布使用显式内部 Sync。
进程被杀后的元数据重放不承诺主机掉电时 guest payload 原子性或 checkpoint 持久化。

Linux OFD 锁范围分别承担 DIFF writer 所有权（byte 0）、短期元数据串行化
（byte 1）、整文件 reader 生命周期（byte 2）及各版本 pin（`16 + record slot`）。
目录 writer 使用独立稳定的 `diffs/.owner.lock`。导出持有自己的文件描述符和版本
read pin，可以跨越原 writer 退出及 owner 交接。删除先隐藏版本；已有 pin 仍使其
retained 并继续强制 COW。整盘 retire 先禁止新 reader，再在全部旧 reader 退出后
unlink 数据文件；锁 inode 保持稳定。
已有 reader 未退出时 `Retire` 返回 busy；后续 `OpenMetadata` owner 可重试删除。
固定导出通过只读 overlay 解释已提交但尚未应用到 home 的 redo，不取得 writer 所有权，
也不执行恢复写入。模板发布可以重命名数据文件，同时保留最终稳定 lock 路径；导出
独立重开已拥有的 inode。同一 DIFF 内已删除的 Checkpoint ID 不可复用。
`Export(ActiveDiff)` 是由调用者保证静止的活动视图；命名导出则拥有不可变保留根。

GC 与删除分开处理持久退休链。已删除版本的记录仅在 retained 前驱 epoch
仍可能引用其出生时间时转移到该前驱，否则进入全局可回收链。有界 `More` 批次即使
没有已提交释放量也继续推进；完整有效扫描可以返回 `BlockedByReaders`，定期重试
使读取者释放后无需再次删除或 guest 写入即可回收。纯扫描状态只保存在内存，不写
redo。GC 按实际文件地址排序安全候选，合并打洞但不跨管理页或有效页，并在打洞成功
后才提交按地址升序连接的空闲批次。metadata-only Open/List/Delete 不启动 GC。
完整批量、崩溃与调度契约由 [diffstore](#discard-and-reclamation) 维护。
`GCRead/GCWritten` 与索引/redo 总字节重叠；`GCPunchCalls` 计已完成打洞调用，
`GCFreed` 计已提交、可复用的位置。

命名捕获收集本地依赖时保留原内容身份，包括作为磁盘 lower 的普通 E。必要时复制完整
原 carrier，不将其剥成 payload 或转换哈希域；已归本目录所有的载体直接复用。
Bundle checkpoint 可以引用相邻 tarstream，后续普通 publish 再生成可移植图。
因此 DIFF 的 BaseID 在捕获前后稳定，成功捕获收集完依赖后可在运行退出时清理临时
恢复物化文件。

<a id="quiesce-and-close"></a>

## Quiesce / Resume

[OpenDisk 就绪表](#open-disk)定义异步初始化、脱离模板依赖及最终发布；
[所有权与完成边界](#ownership-and-completion)定义 Close 和共享 cache 生命周期；
[Discard](#discard-and-reclamation)定义分批 Zero 变更。传输准入 quiesce 时仍须满足这些存储边界。
[Runtime 磁盘捕获](runtime_zh.md#disk-capture)协调共同 VM 冻结及结果不确定的多盘根提交。

内部 `Drain` 等待已接受写入完成，不 fsync。普通 Close 停止新前台工作、取消额度等待、
排空健康已接受写入、join I/O、清除明文并关闭文件。前台取消不取消健康后台回写。
Close 幂等，排空及关闭错误沿 run/restore 传播。销毁也必须等真实在途 syscall 结束，才能
释放其 buffer。

回写与短写错误具有粘性：失败页仍计脏额度，新写失败，唤醒所有等待者，以不 self-join 的
非阻塞 fatal 通知报告 runtime。部分物理写可能已改变文件。清理首次物化块不能对同批
原有 upper 页打洞。前端请求接受不等于整个请求的持久事务；v4 backend 保留其[已提交事务恢复契约](#versioned-format)，不静默 buffered 重试。

必须先记录原始错误并通知 owner，再执行清理 I/O。回滚期间仍持有在途 I/O 所有权、冻结页及其额度，Close 不能提前释放文件或缓冲。清理错误在结束后追加，不掩盖原始失败。

<a id="segmented-vhost-reads"></a>

### vhost-user-blk 多段读

冷启动时，每个磁盘声明 `VIRTIO_BLK_F_SEG_MAX`，最多支持 64 个数据段。当前固定版本的
Cloud Hypervisor（CH）使用 128 项 direct ring。协商该位时，队列必须能容纳 64 个数据
描述符、独立请求头和状态字节。配置在队列设置期间保持固定。旧前端仍可协商原能力子集并
使用较小的队列。RO、FLUSH 和有条件的 DISCARD 延用已有能力位。

每条读请求固定使用同一个后端，按连续逻辑磁盘范围分窗，经现有缓存、对齐和加解密路径
读取。每窗最多 1 MiB。单一描述符或同一 Host 映射中地址连续的多个描述符可直接接收数据。若候选窗口
起始描述符已有至少 256 KiB，且窗内还剩至少 256 KiB，则直接读该片段，避免大段热读的
复制与清理，同时防止单独留下小尾窗。仍然跨越不连续片段的窗口先读到
队列私有工作区，再按描述符顺序散拷有效前缀并清理该窗。Guest 地址可以有间隙或倒序。
一条请求可跨多个窗口，窗口也可在描述符中间结束。64 个不连续的 4 KiB 段聚合为一次
256 KiB 后端调用。512 B 段后接一个不连续的 1 MiB 段则分成 1 MiB 和 512 B 两次调用。
4 个不连续的 256 KiB 段可直接读 4 次。最后一个可写字节用于状态。实际聚合的
队列各保留 1 MiB 工作区，多盘分别计数，位于 `diff_cow.cache_size` 预算之外。碎片读
会增加复制和明文清理的 CPU 与内存流量，热读也有这项成本。缓存与脏页上限沿用现有默认值，
增大预算不会改变缓存未命中时 Guest 拆分请求的方式。

完整读取成功，满长度伴普通 EOF/EAGAIN 也可完成。短读现在只交付有效前缀并报告 IOERR，
修正了旧循环可能将短计数报告为成功的行为。其他普通错误即使返回满长度也报告 IOERR。
used 长度只计有效交付字节和一个状态字节。vhost 不重读失败窗口。终止性错误或已观察到的
取消保留待处理请求，停止写状态、发布 used 和推进 base。同步 pread 可能仍需等待返回，
已经写入 Guest 的字节不会回滚。Guest FLUSH 沿用既有语义，前台写完成和 Guest FLUSH
均不提供额外的 Host Drain 或 fsync 保证。

CH 在内存快照中保存实际协商能力和设备配置。旧快照恢复后保留原有段数限制，新冷启动可
协商 64 并在恢复后保留该值。不支持 SEG_MAX 的旧后端会拒绝带此能力位的快照。

<a id="cow-validation"></a>

## COW 验证

用确定性 worker gate 和注入错误覆盖额度转换、FIFO/LRU、一页容量、取消、多盘竞争、
冻结页读写、FLUSH、快照、Discard 与 Close。真实 direct-I/O 检查覆盖对齐、稀疏边界、
template、明/密文重开，以及磁盘 fixture 上的 mincore 驻留。通用对齐测试覆盖正数、缺失、
双零及无效约束、不可用查询和真实错误。独立真实 tmpfs 测试验证 fixture 描述符类型及相同的 O_DIRECT 请求、
明/密文 I/O、有界工作区与 copyout、模板 seeding、稀疏 hole、混合文件系统共享额度/背压、
dirty/writeback 捕获、Drain/Close 与重开。`scripts/test-vhost-tmpfs-enospc.sh`
明确以 root 身份在新 mount namespace 中运行明文和加密的真实 ENOSPC 用例；
保留 30 秒测试期限，并以外部有界超时处理异常卡住。脚本检查两个子用例均实际通过，
失败、缺失和 Skip 均不能作为必需 CI 成功。测试使用
32 KiB 私有 tmpfs，绝不耗尽或重挂共享挂载；普通 `go test` 只跳过这个需要 capability
的用例，而不会隐式提权。DIO/mincore 基准保留磁盘 fixture。
Tmpfs 功能测试不能作为磁盘缓存绕过证据。
执行 targeted tests、race、vet、build、broader tests，以及活动 diff 位于真实 tmpfs 的
CH/KVM guest 冷启动/读写和 pause/export/restore；明确报告跳过与基础设施故障。

性能比较的数据集必须远大于缓存，分别报告前台 admission latency 与含 Drain 总耗时。
覆盖顺序/随机 4 KiB、512 字节更新、热点覆盖、加密和多盘。报告尾延迟、逻辑/物理写量、
稳定额度以及 file-cache/dirty/writeback 或 mincore 证据。Buffered 基线与候选使用同一
存储。不得用 fsync/DONTNEED/drop_caches 人为制造低驻留；活动 diff 内存归因排除未改变的
模板输入、制品输出和共享 guest 映射。测量是观测，不是未测吞吐目标，也不能声称 skipped
E2E 通过。

<a id="cow-measurement"></a>

### 可重复的 COW 测量方法

历史运行保留在原始 Git commit 和 [COW 工作](https://github.com/kuasar-sandbox/sandboxer/issues/230)中；实测 revision 表不是维护中的规格。重复比较时记录 revision、kernel/filesystem/storage、缓存配置、加密、workload 和前置条件。文档中的方法不代表已经在当前 revision 执行。

使用现有 `blk_cow_workingset_bench_test.go` working-set driver，并为每个 revision 提供匹配 adapter。没有 sandbox cache 的 buffered baseline 使用 no-op cache drain/close，不提供 cache statistics；请求完成不代表介质持久化。分别记录候选实现的准入和最终 Drain：

```bash
go test -tags no_rocksdb ./pkg/diffstore -run '^$' \
  -bench '^BenchmarkCOWWorkingSet$' -benchtime=1x -count=1 -timeout 15m
```

使用 256 MiB 逻辑数据集，为 32 MiB cache 的八倍：顺序 1 MiB 写；跨 65,536 页的确定性随机 4 KiB 和 512-byte 更新；90% 落在 4 MiB、10% 落在其余范围的热覆盖；以及预填充读取。512-byte workload 传输 32 MiB，但物化 256 MiB 页面。热写/读取的预填充排除在计时外。两个 128 MiB 磁盘交替发出 1 MiB 请求，验证 sandbox 共享预算。覆盖明文和 XTS。这是单前台 backend driver，不是 Guest 测量。

排除 fixture/setup，测量含额度等待的请求分位数、逻辑吞吐和包含 Drain 的总耗时。Buffered baseline 完成表示 kernel 接受；预填充读取可能命中 kernel cache。不得用 fsync、DONTNEED 或 drop_caches 制造缓存驻留结果。统计活动文件 body 读写次数和字节；`/proc/self/io` 是文件系统记账，不代表持久介质写入或自动证明写放大。mincore 观察活动文件驻留时不触碰映射页，逻辑 cache/dirty 峰值与 heap/RSS 分开报告。

现有 `cow_direct_workingset_bench_test.go` 提供无缓存的 direct-I/O 对照，使用相同 active-I/O 和 XTS 规则，不是可选择的生产模式：

```bash
go test -tags no_rocksdb ./pkg/diffstore -run '^$' \
  -bench '^BenchmarkCOWDirectWorkingSet$' -benchtime=1x -count=1 -timeout 15m
```

Direct-I/O 预填充分离 cache 读取和温热 kernel file cache 的影响。热路径检查覆盖 nil/background/cancelable context 和收到信号的 waiter。确定性测试验证固定批处理 deadline、pressure/Drain 绕过等待、backlog 时无每批延迟、最老 dirty 进展、小 cache 下有界相邻读取、cache/base/hole 混合、重叠/取消、sub-page 和 short-read 边界，以及与 Guest 修改输出 buffer 隔离的 copyout。

实际 Guest 验证使用现有 owner E2E 的 diff template、encrypted diff、snapshot/restore 和 data disk 用例，配合 `REQUIRE_KVM`、私有 `TMPDIR` 及匹配的 `BIN`/native/runtime 输入。每项缺失前置条件均记录为缺少验证。Guest probe 可先用 1 MiB 请求写入 64 MiB 范围，再预热并对前 4 MiB 执行 4,096 次对齐的 4 KiB 覆盖，使用 mmap-backed buffer 和 O_DIRECT，核对字节数和 payload。Guest descriptor 分段不等于 1 MiB Host batch 测量；没有 Drain/fsync 的准入计时不代表完整回写耗时。原始运行结果保留在原 PR/Issue 或不可变 Git 历史中。

新建 Checkpoint 初始化的分阶段探针位于既有
`checkpoint_template_perf_test.go`，需要显式启用：

```bash
CHECKPOINT_INIT_PERF=1 CHECKPOINT_INIT_DISKS=4 CHECKPOINT_INIT_TEMPLATE=1 \
CHECKPOINT_INIT_ENCRYPTED=1 CHECKPOINT_INIT_ACTION=drain \
CHECKPOINT_INIT_LABEL=candidate CHECKPOINT_INIT_ROUND=1 \
go test ./pkg/diffstore -run '^TestCheckpointInitializationPhases$' -count=1 -v
```

分别使用 1、4 个磁盘，模板/加密取值 0、1，以及 `drain`、`checkpoint`、
`close` 操作，交替安排对照与候选并重复执行。32 GiB 只是稀疏逻辑容量，
启用模板时也只写入 64 KiB 模板数据。准备工作排除在计时之外；结果包含逻辑打开、
首次有效 I/O、完成事件观察时间、立即调用边界的等待、CPU、分配量、RSS、
物理占用及进程 I/O。完成事件观察时间包含观察者调度延迟。对照必须采用当前
同步批量初始化及相同位图路径，不能使用历史逐页串行写入作为基线。保留全部尾部
样本和失败；还应在单实例及并发启动下测量真实 Guest 到首次健康检查/工具响应，
不能只用后端打开时间证明启动收益。

<a id="segmented-read-evidence"></a>

### vhost 多段读的 Guest 对照（2026-11-03）

基线 A 是 `185f50b6`。它与调查提交 `3fa03fc7` 的 `pkg/vhost` 相同。本轮使用当前主干的缓存配置与本地加密策略。H 是最终实现。每种模式在同机分别冷启动两次 A 和两次 H。2 vCPU、2 GiB Guest 使用 CH v51.1.0、Linux 6.1.169、fio 3.39、libaio、Direct I/O 和单 job。工作盘为 ext4 / v3 DIFF，缓存和脏页上限为 32/16 MiB。每次实际检查 DIFF 头、CRC 和 O_DIRECT FD。碎片负载使用已验证的倒序 PFN 缓冲，每组实际检查相邻正序 PFN 对数为零。

各格为 **A 的两次吞吐 → H 的两次吞吐**，单位 MiB/s。1M、4K 分别表示 1 MiB 和 4 KiB 应用 I/O，Q 表示应用队列深度。除 8 MiB 预热外，每个表中作业均实际传输 512 MiB，热读循环使用已预热的 8 MiB 工作集。单独的 CRC32C 校验轮完整写入并读回 512 MiB。表中只列该轮写阶段，用于区分正式写与内容校验。首轮基线未执行的自然布局正式写取自同配置的独立补测。准备、写、正式读和热读均未附加 trace。

| 任务 | WSL x86 / 明文 | WSL x86 / 加密 | 原生 x86 / 明文 |
|:---|:---|:---|:---|
| 准备写，1M Q32 | 151.03/178.83 → 442.14/316.44 | 85.83/73.21 → 104.38/73.52 | 157.64/157.83 → 165.27/170.89 |
| 预读，1M Q32 | 413.90/403.79 → 570.79/481.66 | 121.79/97.23 → 123.82/113.83 | 195.20/186.18 → 158.32/171.41 |
| 自然读，1M Q1 | 48.54/22.70 → 269.47/368.88 | 12.10/19.78 → 106.82/105.68 | 149.84/154.22 → 91.44/182.40 |
| 自然读，1M Q32 | 513.54/248.54 → 583.14/459.61 | 118.38/111.96 → 93.93/117.89 | 159.01/182.40 → 194.23/153.20 |
| 碎片读，1M Q1 | 21.24/12.88 → 225.15/163.58 | 12.97/13.60 → 73.94/81.01 | 16.15/12.51 → 144.31/174.45 |
| 碎片读，1M Q32 | 12.52/14.14 → 244.04/190.41 | 12.74/11.75 → 76.44/64.82 | 16.12/12.81 → 161.36/173.50 |
| 4K 读，Q1 | 6.13/5.39 → 6.28/6.59 | 5.84/5.75 → 5.07/6.23 | 14.81/11.99 → 12.01/12.37 |
| 4K 读，Q32 | 14.44/13.02 → 10.97/12.34 | 12.96/11.08 → 9.53/10.98 | 16.27/13.06 → 13.29/13.28 |
| 2M 应用读，Q1 | 32.64/89.25 → 268.34/208.47 | 63.17/31.50 → 104.47/101.71 | 178.58/169.42 → 161.41/159.60 |
| 8M 工作集预热，Q1 | 37.74/88.89 → 156.86/170.21 | 14.36/30.19 → 88.89/85.11 | 163.27/222.22 → 285.71/421.05 |
| 8M 热工作集，1M Q1 | 1387.53/224.66 → 521.92/881.24 | 281.32/251.35 → 1233.73/567.63 | 5019.61/2415.09 → 3261.15/3240.51 |
| 自然写，1M Q1 | 515.09/170.89 → 372.63/405.71 | 111.21/109.35 → 142.38/96.66 | 160.85/183.12 → 186.66/185.84 |
| 自然写，1M Q32 | 505.93/172.74 → 350.93/405.71 | 129.33/108.89 → 147.93/107.88 | 186.86/164.00 → 159.35/174.15 |
| 碎片写，1M Q1 | 146.49/154.68 → 268.20/263.24 | 110.42/105.76 → 117.38/102.24 | 184.57/182.27 → 181.24/165.59 |
| 碎片写，1M Q32 | 151.75/171.99 → 308.99/295.95 | 112.21/108.31 → 134.99/70.17 | 176.01/180.54 → 174.74/171.41 |
| CRC32C 校验轮（写阶段） | 143.74/136.90 → 276.61/230.53 | 104.00/104.85 → 129.33/80.58 | 179.52/159.50 → 185.31/188.79 |

| 任务 | ARM / 明文 | ARM / 加密 |
|:---|:---|:---|
| 准备写，1M Q32 | 162.75/194.23 → 206.20/206.37 | 153.62/152.25 → 156.05/152.15 |
| 预读，1M Q32 | 174.92/150.94 → 146.75/147.00 | 148.49/132.06 → 149.93/159.90 |
| 自然读，1M Q1 | 108.43/27.57 → 185.31/183.12 | 119.51/79.04 → 143.62/154.31 |
| 自然读，1M Q32 | 161.77/197.23 → 149.27/149.01 | 161.11/149.80 → 150.81/149.75 |
| 碎片读，1M Q1 | 19.62/19.70 → 179.59/177.35 | 19.55/19.64 → 143.10/141.91 |
| 碎片读，1M Q32 | 19.73/19.74 → 165.00/162.85 | 19.56/19.68 → 144.35/141.79 |
| 4K 读，Q1 | 19.63/19.55 → 19.58/19.48 | 19.61/19.60 → 18.22/19.07 |
| 4K 读，Q32 | 19.74/19.74 → 19.77/19.74 | 19.67/19.55 → 19.55/19.70 |
| 2M 应用读，Q1 | 29.09/155.72 → 201.89/203.66 | 146.79/43.31 → 166.18/146.96 |
| 8M 工作集预热，Q1 | 26.67/148.15 → 500.00/470.59 | 89.89/133.33 → 160.00/190.48 |
| 8M 热工作集，1M Q1 | 350.93/2473.43 → 3555.56/1450.42 | 2072.87/1896.30 → 3657.14/3605.63 |
| 自然写，1M Q1 | 185.78/177.65 → 205.46/164.10 | 163.47/162.39 → 164.47/153.80 |
| 自然写，1M Q32 | 148.88/152.29 → 188.03/196.39 | 153.57/153.71 → 153.34/164.95 |
| 碎片写，1M Q1 | 158.61/159.06 → 177.59/154.17 | 153.66/164.31 → 153.34/164.21 |
| 碎片写，1M Q32 | 158.22/159.20 → 161.26/213.33 | 153.98/165.75 → 153.85/164.16 |
| CRC32C 校验轮（写阶段） | 183.97/185.10 → 188.37/149.27 | 153.16/163.16 → 164.47/153.20 |

各组 QD1 的平均总延迟（µs）。每个值为两个独立 Guest 的中位数，按 fio 的 slat + clat 计算。

| 平台和 DIFF | 自然 1M/Q1 | 碎片 1M/Q1 | 4K/Q1 |
|:---|---:|---:|---:|
| wsl / off | 32318.4 → 3202.1 | 62361.7 → 5270.2 | 675.7 → 604.1 |
| wsl / auto | 66584.6 → 9403.6 | 75306.0 → 12926.6 | 668.1 → 695.0 |
| native / off | 6577.2 → 8208.0 | 70934.5 → 6329.0 | 294.2 → 319.8 |
| arm / off | 22743.3 → 5427.3 | 50866.4 → 5601.4 | 199.0 → 199.6 |
| arm / auto | 10507.8 → 6720.1 | 51023.0 → 7015.9 | 198.8 → 209.2 |

在原 x86 主机另建两种消融版本。B 只启用多段协商，C 只使用正式读路径。诊断轮应用发出 64 次 1 MiB 读取。按读取数据 DIFF 的工作线程匹配用户态探针与 strace，起止边界也涵盖少量辅助调用。pread 按线程配对 unfinished/resumed 后统计完整返回。

| 代码 | 512 MiB 无跟踪读 MiB/s | 64 MiB 诊断的 chain / ReadAt | data.diff pread |
|:---|---:|---:|:---|
| A | 16.15 / 12.51 | 16,387 / 16,388 | 16,384 × 4 KiB |
| B | 16.40 / 13.92 | 256 / 16,397 | 16,384 × 4 KiB |
| C | 13.19 / 13.17 | 16,390 / 16,395 | 16,384 × 4 KiB |
| H | 144.31 / 174.45 | 256 / 257 | 256 × 256 KiB |

固定 PFN 的热读补测令每个 1 MiB 读恰好包含 4 个互不连续的 256 KiB 物理范围。8 MiB 工作集预热后，每个独立 Guest 内交替测量连续及四段布局，每轮传输 512 MiB。四段热读的两次 Guest 内中位数（MiB/s）为 A 5100/4837，始终聚合不连续段的中间版 3429/3414，保留大段直读的 H 5438/5708。同布局顺序扫过 512 MiB 时分别为 150/148、161/163、161/160。这组比较决定了 256 KiB 大段直读规则。自然热读仍受实际物理布局影响。

本机 WSL 还以常驻内存后端隔离了描述符遍历、复制和清理成本。单 CPU，A/H 交替测量六对，按每请求中位数 µs 计，单段 4 KiB 0.68 → 0.82，单段 1 MiB 86.71 → 80.82，倒序 64×4 KiB 26.60 → 53.15，倒序 4×256 KiB 74.82 → 69.48。碎片工作区增加的内存流量在无存储等待时仍可测量。

主矩阵 ARM 加密 4 KiB/QD1 中位数为 19.61 → 18.65 MiB/s。为检验单段路径，另让两个版本轮流打开同一份已准备的加密数据 DIFF。每次冷启动先完整执行 4 KiB/QD32 扫描，再执行两次 QD1、一次 QD32，每次 512 MiB。所有启动的文件 SHA-256 一致。固定数据和预读顺序后，结果如下。主矩阵的差值仍保留在表中。

| 独立启动 | 两次 4 KiB/QD1 读，MiB/s | 4 KiB/QD32，MiB/s |
|:---|---:|---:|
| H1 | 19.61 / 19.54 | 19.69 |
| A1 | 19.61 / 19.59 | 19.55 |
| A2 | 19.63 / 19.58 | 19.55 |
| H2 | 19.59 / 19.59 | 19.68 |

另以同一份明文数据 DIFF 复测相邻负载，四次独立启动按 A/H/H/A 顺序完整读取，逐次核验文件 SHA-256。表内为 MiB/s。自然 1 MiB/QD32 和 2 MiB/QD1 各取同一 Guest 内两次读取的中位数，WSL 的 4 KiB/QD32 为一次完整扫描。

| 同一明文数据盘 / 作业 | A 的两个独立启动 | H 的两个独立启动 |
|:---|---:|---:|
| wsl / 4 KiB Q32 | 17.12/13.31 | 11.12/13.00 |
| wsl / 1 MiB Q32 | 297.93/303.68 | 490.47/369.48 |
| wsl / 2 MiB Q1 | 34.93/110.51 | 310.60/234.97 |
| native / 1 MiB Q32 | 172.47/171.33 | 155.51/184.17 |
| native / 2 MiB Q1 | 38.48/141.68 | 183.19/152.21 |
| arm / 1 MiB Q32 | 158.94/154.30 | 149.23/147.20 |
| arm / 2 MiB Q1 | 54.70/31.16 | 181.92/187.63 |

固定后端数据后，自然布局的 Guest 页仍可能不同。ARM 明文 QD32 的差距在该序列中缩小，仍偏低。再对同一数据连续执行三次自然 1 MiB/QD32 扫描，两个 Guest 各自的中位数为 A 153.16/152.61、H 151.48/148.71 MiB/s。仅在诊断构建中把大段门槛改为 128 KiB，结果为 157.06/147.55。提前切分长片段之前的短前缀为 147.42/151.93。这些数值依赖扫描次序和实际请求形状，未证明另一种规则能稳定改善自然布局。原生 x86 的独立写专项中，受控 1 MiB/QD1 写为 A 156.81/180.85、H 185.71/156.15 MiB/s。

WSL 的主矩阵中，4 KiB/QD32 明文和加密的中位数分别约低 15%。同一明文数据盘上采用 A/B/C/H 四种构建再测，各两个独立启动依次为 17.46/10.42、13.78/10.81、11.80/13.30、13.81/10.61 MiB/s。该路径每个 4 KiB 读只有一个数据片段，无工作区复制。保留这组较低结果作为嵌套 WSL 的性能边界，不能据此宣布所有环境的小请求都无回退。

1280 KiB 专项探针使用只读数据盘 preadv，每页独立读回比较，检查设备恰好完成 1 次读、2560 个扇区。最终 x86 和 ARM 均观察到单请求。另在 x86 把 4 KiB 页放在长物理段前，单独跟踪到数据 DIFF 的 1 × 1024 KiB, 1 × 256 KiB 调用。仅凭应用的 2 MiB 作业不能判定单条 vhost 请求跨窗。WSL 真快照中，旧/新可写盘的已协商掩码分别为 `0x140002200`/`0x140002204`，保存的 seg_max 为 0/64，ring 都是 128。旧→新、新→新恢复后均核对 Guest 段数限制和磁盘内容。新→旧因不支持位 `0x4` 被拒绝。WSL 的一次独立 Guest 测试完成 3 次 FLUSH 和 32 次 DISCARD，状态均成功，保留文件的内容正确。

fio 计应用 I/O，Guest 盘的 stat 计 Linux 块请求，blk2 rd/s 和 logical_backend 计整条 vhost 请求。ReadAt 与实际文件 pread 分别在调用层和系统调用层测量。两台原生主机主矩阵中的 84 次自然、碎片、4 KiB 和 2 MiB 读扫描，其进程存储读取字节均与 fio 字节相等。WSL 的 56 次扫描缺少这个内核计数。即便它等于 fio 字节数，也无法据此判定物理介质缓存为空。各组两次独立启动可揭示波动，无法估计生产环境碎片频率。本轮吞吐限于所列 CH、内核、v3 DIFF 和缓存预算。ARM 快照恢复及 v4 DIFF 的真实 Guest 性能未单独测量。

在与 Guest libc/架构匹配的环境编译探针。准备 root.ref、两个 ext4 模板 root.template/data.template 和含私有 run-fio 加载器的 fio-runtime.tar。每次使用全新输出目录，off 和 auto 各运行两次独立启动。

```bash
cc -O2 -Wall -Wextra -Werror -fPIC -shared test/perf/fio_reverse_pfn.c -o /tmp/libgeometry.so
cc -O2 -static test/perf/vhost_large_read.c -o /tmp/large-read
sudo python3 test/perf/vhost_guest_fio.py --inputs "$FIXTURES" --output "$NEW_RUN" \
  --ctl "$CTL" --ch "$CH" --kernel "$KERNEL" --runtime "$RUNTIME" \
  --crypto auto --pfn-lib /tmp/libgeometry.so --large-probe /tmp/large-read --trace
```

调查原型的 x86 碎片 QD1 中位数为明文 A/B/C/D 16.82/15.68/13.18/168.62 MiB/s，加密 A/D 15.46/102.68。每组两轮共用同一个 Guest。上述验收表使用本次独立启动的实际结果。 [#398](https://github.com/kuasar-sandbox/sandboxer/issues/398)

<a id="checkpoint-validation"></a>

## Checkpoint 验证与测量

`snapshot.checkpoint.sh` 在 v3/v4、明文/XTS 配置下对非零不可变基底执行 Guest fstrim，
验证可移植导出及冷恢复后的 Zero，并在 v4 下经过本地内存 Checkpoint 恢复。
单元测试补充模板 Data/Zero 在导入前、导入中及导入后的可控交错，以及缓存回写、
取消和首次载荷写入失败后的旧状态恢复。

源码检查与预构建产物 E2E 分开执行。direct I/O 和空间回收检查应使用原生 Linux ext4
scratch；Windows 挂载目录、仅 tmpfs 测试或跳过特权用例不能证明这些性质。

```bash
# 准备好的 Go 源码环境；TMPDIR 应位于待测文件系统：
CGO_ENABLED=0 go test -count=1 ./pkg/diffstore/... ./pkg/vhost ./pkg/artifact \
  ./pkg/config ./pkg/sandboxfile ./pkg/snapshot ./pkg/restore ./pkg/sandbox ./pkg/ctl ./cmd/sandbox-ctl
CGO_ENABLED=1 go test -race -count=1 ./pkg/diffstore/... ./pkg/vhost ./pkg/artifact ./pkg/restore
# Workbench system mode，具备 mount/namespace 权限的完整源码 gate：
bash scripts/ci-source-checks.sh

# 独占、可丢弃的 64 MiB ext4 文件系统；不要使用共享开发挂载点：
DIFFSTORE_FULL_FS=/path/to/owned/finite-ext4 go test ./pkg/diffstore/internal/versioned \
  -run '^TestRealFilesystemFullRecovery$' -count=1 -v

# root shell；PREPARED 包含准确构建的产物与平台 E2E runner：
python3 "$PREPARED/test/e2e/e2e" run --workdir "$PREPARED" \
  --suite snapshot --include snapshot.checkpoint.sh

# 实际 payload 历史；默认规模为 0、1、10 GiB：
go test -c -o /path/to/compiled-vhost.test ./pkg/diffstore
go run ./test/perf/diffstore --directory /path/to/new-owned-directory \
  --sizes-gib 0,1,10 --rounds 5 \
  --vhost-test-binary /path/to/compiled-vhost.test

python3 test/perf/checkpoint_cow.py --tmpdir /path/to/native-ext4/scratch \
  --output /path/to/results --rounds 3 --operations 32768 \
  --modes 'raw|v1|v2-no-version|v2-shared|v2-private|v2-xts-no-version|v2-xts-shared|v2-xts-private'

python3 test/perf/checkpoint_template.py --tmpdir /path/to/native-ext4/scratch \
  --output /path/to/template-results

# 基线/候选使用相同环境、payload 和保留版本集合。
# 三个独立轮次，每轮三次写入/checkpoint/删除/GC/复用循环：
python3 test/perf/checkpoint_cow.py --tmpdir /path/to/native-ext4/scratch \
  --output /path/to/reclamation-results --rounds 3 --gc-cycles 3 \
  --modes 'v2-no-version|v2-xts-no-version' \
  --workloads 'sequential|gc-write|reclamation|background-gc|discard'

# 基线/候选二进制共用同一 fixture，交替执行至少三轮。
# CASE：dense-plain/dense-xts（1 GiB payload），sparse-plain/sparse-xts
# （逻辑容量 30 GiB，仅映射 240 页）。
CHECKPOINT_PRESENCE_DIR=/path/to/owned/native-ext4/fixtures \
CHECKPOINT_PRESENCE_CASE=dense-plain CHECKPOINT_PRESENCE_PHASE=seed \
  go test ./pkg/diffstore -run '^TestCheckpointPresencePhases$' -count=1 -v
CHECKPOINT_PRESENCE_DIR=/path/to/owned/native-ext4/fixtures \
CHECKPOINT_PRESENCE_CASE=dense-plain CHECKPOINT_PRESENCE_PHASE=measure \
CHECKPOINT_PRESENCE_LABEL=candidate \
  go test ./pkg/diffstore -run '^TestCheckpointPresencePhases$' -count=1 -v

# 活动密集读取：两版二进制共用 fixture，交替执行至少三轮。
# CASE：plain-shared、plain-private、xts-shared、xts-private。
CHECKPOINT_READ_DIR=/path/to/owned/native-ext4/dense-fixtures \
CHECKPOINT_READ_CASE=plain-shared CHECKPOINT_READ_PHASE=seed \
  go test ./pkg/diffstore -run '^TestCheckpointDenseReadPhases$' -count=1 -v
CHECKPOINT_READ_DIR=/path/to/owned/native-ext4/dense-fixtures \
CHECKPOINT_READ_CASE=plain-shared CHECKPOINT_READ_PHASE=measure \
CHECKPOINT_READ_LABEL=candidate \
  go test ./pkg/diffstore -run '^TestCheckpointDenseReadPhases$' -count=1 -v
```

源码测试覆盖独立进程 pin 和 writer 交接、杀死 owner 后 redo 重开、真实短写、空间
预留失败、树分裂/合并、retire/GC 中断恢复、零覆盖、加密及 metadata-only 操作。
artifact 测试覆盖精确发布后删除、Bundle/外部来源、重写、来源目录身份和同目录重开/
跨目录物化。产物用例使用根盘加两个数据盘，串起冷启动、命名捕获、继续运行、并发
固定版本发布、历史/跨目录恢复以及移除原来源后的恢复。
它还在交接后查询内核 OFD 版本 pin 锁范围，并在明文及 XTS 模式下串起热/冷恢复、
显式 Delete、继续写入、再次捕获、发布及便携产物恢复，从而区分 List 为空与读者
已真正释放。

Checkpoint 用例还先把非零文件写入不可变下层，再删除文件并执行 Guest `fstrim`。
通过块设备直接读取，验证 trim、本地/历史/跨目录恢复及便携发布/恢复后仍为零。
载体检查沿导出的分层引用链查找，要求第一个提供内容的层返回零并屏蔽非零下层。
旧 tarstream 实写零值 Data；tarstream 独立的 Zero 优化可以省略这些载荷。
存储测试在序列化前验证 Zero，载体测试负责验证载荷省略。不可变零掩码上方的
活动层可以为空。单层与 Checkpoint 恢复路径均覆盖。

presence 回归覆盖全量 P 加载、Zero 与已分配零页、上层/base 块的部分写入、已接受但未回写的
整页覆盖、并发 discard、取消、sticky 索引故障及普通导出完整性。presence harness
分别记录 Open/索引字节、首读、带延迟分位数的 1,024 次启动读取、重复热读及完整
活动 `SnapshotView` 构建。应比较 Open 加首批负载的总时间，不能只比较 Open；这些
是块层测量，并非 VM 到业务就绪的耗时。稀疏 30 GiB 逻辑样本不能代表实际 30 GiB
payload 的验证。

密集读取探针使用实际 128 MiB payload 和 32 MiB COW cache，逐字节验证内容，
分别报告冷顺序读、全量重复读、16 MiB 尾部热读、冷随机 4 KiB 读及其缓存重复读。
`api_MiB_s` 为字节数除以 ReadAt 累计服务时间，`wall_with_validation_ns` 还包含
内容校验。CPU 字段包含校验；结束时 RSS、进程生命周期峰值 RSS 和 Go 累计分配量
是不同指标。应以相同测试源码、相同构建环境比较准确的基线/候选生产源码。
只初始化一次，每个用例重新打开，保留原始样本及工作集大小，不能把短时热读称为
冷盘吞吐。写入 driver 支持 `--working-set-mib`（默认 64）；增大工作集时，
`--operations` 至少为 `working-set-mib * 512`，以保留两次遍历。

性能 driver 记录主机、文件系统、构建身份、原始日志、前端及 checkpoint 的
p50/p95/p99、包含最终 Drain 的吞吐、数据/索引/redo/GC 物理字节计数、cache hit
及进程 CPU/RSS。工作集 64 MiB 大于 32 MiB COW cache；有意设计的 hot 用例为
4 MiB。至少运行三轮。覆盖用例每轮遍历工作集两次；`first-sequential` 和 `first-random`
在新磁盘上每页只写一次。为便于主干对照，保留 `raw`、`v1`、`v2-*` 历史模式标签，
候选分别使用 v3 明文、v3 XTS 和 v4。单层与 Checkpoint 的明文/XTS 分别比较，
并区分无版本、共享首写、已私有覆盖。这些命令提供复现方法，不代表
性能门槛或全部特权测试已经通过。

以下配套探针测量完整 Zero discard，并记录 trim 前、已删除
Checkpoint 仍被读者固定、读者释放并完成 GC 后，以及复用后的实际分配空间：

```bash
go test ./pkg/diffstore -run '^$' -bench '^BenchmarkCheckpointDiscard$' -benchtime=3x
go test ./pkg/diffstore/internal/versioned -run '^$' -bench '^BenchmarkCheckpointZeroRetention$' -benchtime=3x
```

Discard 记录进程 CPU、后端调用、位图/索引/redo 字节及结构提交数。版本化计数可能
包含并发后台 GC。已有 Data 转为 Zero 时 P 保持为 1，无需重写位图。对同时提供两种
模式的旧版本，应使用其 `BenchmarkCheckpointGuestDiscard` 比较相同的 Zero 语义；
历史 Hole 结果不可直接比较。COW 首写基准在停止计时后关闭并重建同配置 DIFF，
而不是通过 Discard 重置；Zero 已逻辑存在，会改变被测工作。报告将
读者保留空间及不变的文件长度与可回收 payload 分开。运行探针时避免并发执行产品
E2E 或全量 race，并把原始结果保留在 PR/Issue。

模板驱动单独测量三轮全新 64 MiB 单层明文/XTS 和 Checkpoint 明文/XTS 初始化。
它记录 Open、Ready、Materialized 和总耗时，总耗时包含首次同步及正式发布，
随后在计时窗口之外验证全量 payload SHA256。来源准备不计入耗时；进程峰值
RSS 仍包含这部分准备阶段。

规模 driver 在进入下一档前删除上一档历史，分别报告 checkpoint 注册、control 重开
和 presence 重建；其中显式全扫描的耗时会随映射页数增长。v4 Disk 重开加载 P，
不再从树重建 presence；payload 查询仍有界。vhost helper
在回写前固定恰好 32 个脏页（128 KiB），分别记录模板 readiness、Drain、最终
materialization、元数据登记、捕获总耗时和重开。不使用 helper 时，登记样本的待写
payload 为零。捕获总耗时不含最初 open 和 guest 写入准入，也不含 VM 内存捕获。
更大规模（如 30 GiB）可显式通过 `--sizes-gib` 启用。

<a id="reclamation-measurement"></a>

## 回收性能测量

reclamation 工作负载停止独立 GC worker，测量已提交的空闲位置、复用、打洞调用、
元数据提交/写入和积压处理。删除时保留读取者，测量受阻期间的元数据写入，再释放
读取者，不重复删除。`gc-ns/cycle` 累加全部 GC 调用耗时，包括读取者保护下的尝试；
`backlog-wall-ns/cycle` 测量释放读取者后的积压处理，包括观察已提交空闲链的开销。
顺序读取和当前版本导出读取观察复用后的局部性；保留版本导出读取是数据未移动的
控制项。读取指标只计时 `ReadAt`，完整内容比较与预分配的期望数据不计入这些耗时。
文件长度与实际占用分别报告：保留的描述
记录和管理页不回收。`gc-write` 在普通后台 GC 运行时写入并保留最近四个版本，
报告前台尾延迟及索引/redo 写放大。`background-gc` 使用固定 4 MiB payload 积压，
每轮一次，测量真实 worker 从启动到完成的耗时，包括轮询与调度间隔。
活动 worker 在可继续推进的批次之间留出可取消的 1 ms 间隔，以降低前台争抢；
显式 GC 不增加间隔。这以峰值后台吞吐换取调度机会，不构成前台延迟保证。
Discard 固定使用 4 MiB 范围和 1 MiB 缓存；
后端调用数可以归因于 Discard，但 `total-commits/range` 包含并发 GC 提交。
专用 reclamation 和 Discard 用例会略过不支持的 shared/private 模式组合。
各轮原始结果及准确的基线/候选身份保留在 PR 或 Issue；这些负载提供实测观察，
不构成普遍加速倍数承诺。

<a id="related"></a>

## 相关文档

[Runtime：启动、控制、捕获与恢复](runtime_zh.md) | [Sandbox 配置参考](configuration_zh.md) | [Sandbox 与 Snapshot 制品](artifacts_zh.md) | [Guest 内存与按需恢复](memory_zh.md) | [资源控制与 Usage 计量](resources_zh.md) | [Guest ABI：启动、应用与 Host 协议](guest-abi_zh.md) | [VMM 集成与 Cloud Hypervisor 补丁](vmm_zh.md)

[组件入口](../README_zh.md) · [构建与补丁材料](../native-deps/README_zh.md)
