[English](diff-cow-cache.md) | [简体中文](diff-cow-cache_zh.md)

# 活动 COW I/O 与明文缓存

活动可写 diff（包括 tmpfs 上的文件）通过同一定位 I/O API 请求 O_DIRECT。
运行时策略不识别底层文件系统类型。
`sandbox-ctl` 为每个沙箱持有一份有界明文页缓存；root 与数据盘共享预算，包括混合使用
tmpfs 与磁盘的情况。这实现了 [issue #230](https://github.com/kuasar-sandbox/sandboxer/issues/230)，
并由 [issue #238](https://github.com/kuasar-sandbox/sandboxer/issues/238) 恢复 tmpfs 兼容性。
改动范围是活动数据访问、配置与生命周期；immutable base、template、历史 overlay、manifest
解密缓存、制品输出、tarstream 表示、cgroup、balloon 和 guest 资源预算保持现有行为。

## 配置与所有权

```yaml
resources:
  diff_cow:
    cache_size: 32MiB
    max_dirty_size: 16MiB
```

两个字段都必须是正数且为 4 KiB 的整数倍，满足 `0 < max_dirty_size <= cache_size`。
缺省字段各自采用以上默认值，包括没有 `diff_cow` 的旧配置。只提供一个字段时，补全默认值后
也必须满足大小关系。这些是有限的工程初值，不是生产 SLA 或最优性能声明。没有零值/无限制、
关闭开关、数据 I/O 失败后触发的 buffered 重试或可配置的其他写入模式。

`max_dirty_size` 是 `cache_size` 的子集，不是额外池或预留分区。没有脏页时 clean 可以使用
全部缓存。添加磁盘不会乘以预算。这些 host 进程资源独立于 guest capacity、allocatable/startup
headroom 和 VMM overhead：`sandbox-ctl` 不在 VMM cgroup 中，无需改资源控制器。

冷启动、`run --from` 与 restore 为当前实例解析 host resources。运行配置保留该沙箱生命周期的
策略；新恢复实例使用本次 host 配置。Portable C0/E/S 不包含缓存策略、页、队列或 LRU 状态。
严格 restore 校验、配置合并与 host projection 都保持此边界。

Go 调用者可通过 `WithCOWCache` 向多个 `OpenBlockCOW` 传入同一 `COWCache`。先关闭 COW，
再关闭共享缓存。不传该 option 时，独立 COW 持有有限默认缓存并自行关闭。

## 逻辑状态与记账

分层为 `BlockCOW -> 明文缓存 -> diffFile 编码 -> 自有 I/O 工作区 -> 活动文件`。
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
独立有界开销包括每沙箱一个 1 MiB 明文批工作区与最多 256 个页指针，每个活动文件两个独立
1 MiB MAP_SHARED I/O 工作区（每个额外对齐 padding 小于 1 MiB），以及随页数线性有界的
页元数据、索引和 Go 分配器开销。`cache_size` 不是进程 RSS、guest memfd 映射、
immutable-source cache、tmpfs 文件存储或快照输出内存的上限。

Tmpfs 文件页是位于内存（也可能位于 swap）中的文件系统存储，与这份有界进程缓存分开。
回写成功会释放脏额度，但不会释放已存入 tmpfs 的文件数据；淘汰 clean 缓存也不会删除它。
`cache_size` 不限制这些字节，也不承诺 tmpfs 页零驻留。该存储由现有 tmpfs 挂载的大小、
inode 限额和宿主内存控制约束。Sandboxer 不重挂 tmpfs、不改变 cgroup、不扩大缓存预算，
也不新增持久化保证。参见 [Linux tmpfs 文档](https://www.kernel.org/doc/html/v6.1/filesystems/tmpfs.html)。

Bitmap 表示**逻辑 upper-present**，独立于 clean/dirty 状态。新页完整发布到缓存及 bitmap
后才应答写入。回写成功或淘汰不清除此 bit。重开按现有格式从稀疏文件 extent 重建 upper
归属。缓存持有领先于物理文件状态的最新明文。

## 读写与背压

读取优先从 clean、dirty 或冻结的 writeback 页复制。只有 miss 才读活动文件并解密。
Read loading 预留总容量；没有可用 clean 槽时，可以用有界前台工作区直接服务读取而不入缓存。
绝不能绕过较新的缓存页。

运行读取与快照读取共用有界 upper 连续区间批处理。仅本次请求的连续冷 upper 页合并为
一次物理读取（最多 1 MiB）；缓存命中、base 或 hole 边界会结束该区间。按 stripe
编号排序的一次性范围读锁防止并发写入/Discard，Loading 预留防止淘汰。缓存容量小于
区间时，剩余冷页在相同 stripe 保护下直接读取但不入缓存。读取/解密保持在自有 MAP_SHARED I/O
工作区内，先从该工作区填充并发布 clean 页，再复制到可被调用者或 guest 修改的输出
内存。不新增与请求长度成比例的 payload 分配或 base 预取。

新写在构造前同时申请总量与脏额度。Clean 变脏只需脏额度；未被选中的 dirty 页重写不追加
额度或队列节点。完整页覆盖不读旧数据。首次部分写从 base（或零）构造完整页，保留现有
短 base/错误处理语义。后续部分写保留所有未修改字节。完成前复制 guest 数据，不持有
完成后的 descriptor buffer。大于缓存的请求逐页推进。

容量满时先淘汰最久未用 clean 页，否则等待回写。额度等待可取消，唤醒后重检状态；等待者
不持有 worker 推进所需的页预留。一页容量与所有页在途都必须可推进。请求不创建后台
goroutine 或无界 payload 队列。块条带锁序列化前台同块访问；worker 不取得这些锁。

## 回写与 FLUSH

每沙箱一个 worker 以最老脏页作为每批的锚点。热点覆盖不移到队尾。可合并同文件相邻脏页，
每批最多 1 MiB，不填 hole 或预分配间隙。固定 1 ms 聚合窗口保证低速流量推进；额度压力与
内部 Drain 立即唤醒。选中的页成为冻结 writeback，仍可读取，同页写等待。Worker 将明文
复制到有界批工作区并加密副本，在不持有 cache/global 锁时执行 I/O。前台读有独立工作区。

Worker 通过有界页索引收集同文件连续脏邻居，不要求到达顺序相邻；未选中页的 FIFO
年龄保持不变。普通写入通知既不提前结束，也不重启固定聚合截止时间。压力和 Drain
会中断等待。已聚合的积压立即推进，即使每批只有单页，也不会逐批再等待 1 ms。

健康且合法的 guest **FLUSH 是 no-op**，不发起回写、不等待脏页、不调用 Drain、fsync 或
fdatasync。请求顺序和 fatal 状态检查保留。为兼容 Cloud Hypervisor 快照，保留现有 wire
features；不增加 CONFIG_WCE 或 wire discard。

写入完成仅承诺完整最新字节已进入有界逻辑 COW 状态。写入与 FLUSH 都不保证
`sandbox-ctl` 崩溃、宿主断电或存储故障后的数据存续。这明确不同于
[Virtio 1.2 §5.2.6.2](https://docs.oasis-open.org/virtio/virtio/v1.2/virtio-v1.2.html)
要求的稳定存储 FLUSH。这是项目的非持久化 COW 契约，不是标准持久化 FLUSH 行为。
Direct I/O 本身也不是持久化屏障。

## 快照、discard 与关闭

CH pause 且前台 quiesce 后，`SnapshotView` 复制逻辑 bitmap 并提供 upper-only 明文，
优先读 dirty/writeback 缓存页，再读文件。不强制 Drain/fsync，也不复制整份缓存。
后台可继续搬运相同逻辑内容。只有文件成功完成后，最后一份缓存副本才能淘汰。捕获完成前
保持前台 quiesce 且 COW 打开。捕获期间后台 fatal 必须使发布失败并到达 runtime owner，
包括故障后没有新 guest 请求的情况。Export 不 rotate active diff 或改变 base。

底层 Discard 在对完整块打洞并清除 upper 归属前，与写入及在途回写同步。部分边缘不变。
Discard 使 base 再次可见，不是能遮蔽 base 的显式零。Wire profile 仍拒绝 DISCARD 和
WRITE_ZEROES。写零仍物化 upper 数据，不能视为 hole。

内部 `Drain` 等待已接受写入完成，不 fsync。普通 Close 停止新前台工作、取消额度等待、
排空健康已接受写入、join I/O、清除明文并关闭文件。前台取消不取消健康后台回写。
Close 幂等，排空及关闭错误沿 run/restore 传播。销毁也必须等真实在途 syscall 结束，才能
释放其 buffer。

回写与短写错误具有粘性：失败页仍计脏额度，新写失败，唤醒所有等待者，以不 self-join 的
非阻塞 fatal 通知报告 runtime。部分物理写可能已改变文件。清理首次物化块不能对同批
原有 upper 页打洞。不承诺写事务性或崩溃恢复，不静默 buffered 重试。

必须先记录原始错误并通知 owner，再执行清理 I/O。回滚期间仍持有在途 I/O 所有权、冻结页及其额度，Close 不能提前释放文件或缓冲。清理错误在结束后追加，不掩盖原始失败。

## 活动文件 I/O 契约

每个活动 body 都在其已打开的描述符上尝试 O_DIRECT，并始终使用相同的有界对齐工作区。
如果该 F_SETFL 请求返回 EINVAL 或 EOPNOTSUPP/ENOTSUP，描述符保持普通定位 I/O，
应用缓冲和缓存不变。这只是在初始化时处理操作是否受支持，不识别文件系统，也不是数据 I/O 失败后的重试。
新目标在 **seeding 前**完成上述初始化；已有活动文件校验、运行与快照读取使用同一路径。
运行时代码不检查文件系统类型、名称、挂载策略或文件系统专有 inode flags。
Header/格式探测在并发 body I/O 前有界完成；template/base 保留 buffered/read-only 行为。
明文与密文文件格式、固定 4 KiB 密文 header、本地加密 off/auto/required 策略和
512 字节 XTS 数据单元编号不变。

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

## 验证

用确定性 worker gate 和注入错误覆盖额度转换、FIFO/LRU、一页容量、取消、多盘竞争、
冻结页读写、FLUSH、快照、Discard 与 Close。真实 direct-I/O 检查覆盖对齐、稀疏边界、
template、明/密文重开，以及磁盘 fixture 上的 mincore 驻留。通用对齐测试覆盖正数、缺失、
双零及无效约束、不可用查询和真实错误。独立真实 tmpfs 测试验证 fixture 描述符类型及相同的 O_DIRECT 请求、
明/密文 I/O、有界工作区与 copyout、模板 seeding、稀疏 hole、混合文件系统共享额度/背压、
dirty/writeback 捕获、Drain/Close 与重开。`scripts/test-vhost-tmpfs-enospc.sh`
明确以 root 身份在新 mount namespace 中运行明文和加密的真实 ENOSPC 用例。测试使用
32 KiB 私有 tmpfs，绝不耗尽或重挂共享挂载；普通 `go test` 只跳过这个需要 capability
的用例，而不会隐式提权。入口强制检查明文/加密用例均成功，拒绝 Skip 或没有执行用例的
结果，限制运行时间，并在失败后清理本次临时文件。DIO/mincore 基准保留磁盘 fixture。
Tmpfs 功能测试不能作为磁盘缓存绕过证据。
执行 targeted tests、race、vet、build、broader tests，以及活动 diff 位于真实 tmpfs 的
CH/KVM guest 冷启动/读写和 pause/export/restore；明确报告跳过与基础设施故障。

性能比较的数据集必须远大于缓存，分别报告前台 admission latency 与含 Drain 总耗时。
覆盖顺序/随机 4 KiB、512 字节更新、热点覆盖、加密和多盘。报告尾延迟、逻辑/物理写量、
稳定额度以及 file-cache/dirty/writeback 或 mincore 证据。Buffered 基线与候选使用同一
存储。不得用 fsync/DONTNEED/drop_caches 人为制造低驻留；活动 diff 内存归因排除未改变的
模板输入、制品输出和共享 guest 映射。测量是观测，不是未测吞吐目标，也不能声称 skipped
E2E 通过。

基线/候选实测结果及尚缺的 E2E 前置条件见[验证观测](diff-cow-cache-validation_zh.md)。
