[English](usage.md) | [简体中文](usage_zh.md)

# usage — 沙箱资源用量

## 1. 概述

`sandbox-ctl` 通过 `pkg/usage` 唯一负责采样调度、CPU 差分、Gauge 积分、
采样峰值、归并和持久化. `usage.Snapshot` 是累计状态, `usage.Record` 是
自包含保存记录. `sandbox-init` 只按请求读取、解析原始观测, 不包含 usage
ticker、累计器、历史摘要或可靠补发队列.

Usage 是可选观测, 不是账单、资源限额或物理成本归属. 范围只有 Guest 总/逐
vCPU 执行时间、Guest 物理内存、受管理可写文件系统占用, 以及 CH/sandbox-ctl
进程 CPU 和独立的 `RssAnon`/`RssFile`. 不包含 UFFD、网络或磁盘操作次数、
字节和时延. 既有诊断遥测和资源控制保持独立. 不提供 daemon、exporter、
计费策略、消费 ACK、全局 WAL 或删除后保留服务.

## 2. CLI

```bash
# Read current in-memory and confirmed saved state from the running owner.
sandbox-ctl usage --sandbox-id s1

# PathID locates directories; SandboxID remains the file/record identity.
sandbox-ctl usage --sandbox-id s1 --path-id instance --saved

# Read a stopped sandbox without starting a VM or changing the file.
sandbox-ctl usage --sandbox-id s1 --offline
sandbox-ctl usage --file /var/lib/sandbox/s1/s1.usage

# Bounded history page; pass next_cursor as the next --cursor.
sandbox-ctl usage --sandbox-id s1 --history --limit 10 --cursor 0
```

输出是无损 JSON, `--json` 默认 true. `--run-root` 默认 `/run/sandbox`,
`--base-root` 默认 `/var/lib/sandbox`, `--path-id` 默认 SandboxID.
`--timeout` 限制在线查询, 默认 5 s 且必须为正. ctl socket 不存在或拒绝连接时
允许离线回退, 其他连接/协议错误明确失败. `--file` 隐含 `--offline`, 可以从
去掉 `.usage` 后缀的文件名推导 SandboxID.

`--saved` 省略 `live`. 历史查询采用非负字节 cursor 和 1–100 条记录的 limit,
默认 10. Host usage response 上限 1 MiB, 一页放不下时须减小 limit.
在线读取器每次只解码、编码一条有界记录, 在保留或编码完整请求记录集合前
拒绝超出 JSON 预算的页, 并计入字符串转义膨胀. 历史不返回活动尾部.
普通 ctl 请求的 framing 和大小上限不变.

View 包含 `enabled`, 可选的 `live`/`saved`, `saved_end`, `saving`,
`unknown_tail` 和可选 `save_error`/`read_error`. 在线 `saved` 是 owner 已采用
的基线: 启动时恢复的完整存活记录, 或该 owner 后续已确认保存的记录. 本进程
新写入的 CRC 可读不能单独推进基线. 离线 `saved` 是恢复校验通过的最后一条
完整存活记录, 不证明原 writer 已确认 Sync. `live` 包含已经接收但仍在保存或尚未提交的输入,
崩溃后可以回退到存活的 saved. 缺测、未持久化和文件尾部不确定是不同状态.

在线查询只复制已有状态或读取已确认历史, 不触发 Guest/CH 采集、累计推进或
flush. 离线 reader 获取非阻塞共享文件锁, 拒绝与活动 writer 并行读取; 此时应
查询该 writer 的 ctl socket. 关闭 usage 不删除已有文件, 仍可离线读取.

## 3. 配置

```yaml
usage:
  enabled: true
  sample_interval: 1s
  flush_interval: 5m
```

默认值为 `enabled: false`, `sample_interval: 1s`, `flush_interval: 5m`.
间隔必须是正的 Go duration 字符串, `flush_interval >= sample_interval`,
且两倍 sample interval 必须可由受支持的有符号 duration 表示. 未知 key、
重复 key 和错误 scalar 类型报错. 配置文件按顺序覆盖, 省略的 usage 成员保留
前值, 显式 false 关闭采样.

Usage 在普通 cold、`run --from` 和 `run --restore` 中都是 host policy,
不进入 `PortableSandboxConfig`、E 或 S. 恢复后使用当前 host policy, 不继承
制品中的开关. 参见 [host overlay 示例](../examples/usage-enabled.yaml).

关闭时 Host 不创建 usage sampler、usage 长连接或周期保存. 既有资源控制报告
和 balloon policy 不变. Guest 磁盘组装仍保留有界私有文件系统句柄, 使后续内存
恢复可以开启 usage; 保留句柄本身不调度读取.

## 4. 指标、单位和计算

本节是 CLI、协议和生命周期文档的单位与公式唯一说明来源. 所有 JSON 计数、
大小、时长、位置及 128 位值均为十进制字符串, 不使用浮点数. CPU 总量单位 ns,
占用单位 byte, 面积单位 byte·ns, span/covered 单位 ns. UTC 字段为 Unix ns,
只用于外部关联. 位置是相对 `Snapshot.run_epoch`/`started_utc_ns` 的有符号
单调 ns, 不能跨运行相减.

| Name | Source and meaning |
|---|---|
| `guest.cpu` | CH 进程 `/proc/PID/stat` 的 `guest_time` |
| `guest.vcpu.N` | 已验证 `vcpuN` task 的 `guest_time` |
| `ch.cpu`, `sandbox_ctl.cpu` | 对应进程 `utime + stime` |
| `guest.memory` | Guest 非空闲物理 RAM 减去 live balloon current |
| `filesystem.root`, `filesystem.disk-N` | 一个受管理可写 ext4 文件系统的占用 |
| `ch.rss_anon`, `ch.rss_file` | CH status 的 `RssAnon`、`RssFile`, 分项保存 |
| `sandbox_ctl.rss_anon`, `sandbox_ctl.rss_file` | sandbox-ctl status 的对应分项 |

### 4.1 CPU Counter

`guest_time` 已包含在 `utime` 中, 不重复加到进程 CPU. Host boot identity、
PID/TID 和 starttime 共同标识来源. Parser 先定位 `comm` 终点再读取字段,
正确处理空格和括号. 合法 vCPU 映射缓存并重新验证, 缺失线程不按进程总量摊差.

`/proc/self/auxv` 的 `AT_CLKTCK` 提供 USER_HZ, 不依赖 libc、cgo 或
`getconf` 子进程. Linux amd64/arm64 使用已核验的 little-endian ELF64
auxiliary-vector 布局, 错误、缺失或重复值使 reader 失败. 内核 `CONFIG_HZ`
不是这个换算尺度. 两个二进制继续使用 `CGO_ENABLED=0`.

Counter 保留已知累计 ns、原始 ticks、尺度、换算余数及来源/完整性. 已知创建
来源计入首次轮询前 CPU. 整数换算跨轮询保留余数, 不逐秒舍入后相加. 来源计数
回退不会下溢或悄悄扣除已知用量. 同一原生 Counter 可在漏轮询后恢复差值,
但这不恢复 Gauge coverage. 新来源的已知创建基线不证明旧来源末段已知.
已确认身份的 vCPU 暂时失读时保留已知基线, 后续同身份 Counter 可补齐漏读 ticks.
首次身份未知、确认线程退出/更换、映射歧义或终态读取失败仍标记 incomplete;
重新发现线程不能恢复已经丢失的完整性.
来源丢失的有效性标记按 vCPU 有界保留, 不因结果被拒收而遗忘; 被拒收结果本身
不计入 CPU ticks, 也不改变其时间戳.

CH 的唯一 wait owner 使用 `waitid(WNOWAIT)`, 重试 EINTR, 在 proc 状态仍
存活时尽力最终读取, 然后调用 `cmd.Wait`. 不增加第二回收者, 不改变退出升级
或输出排空. 即使总量可用, 未知逐线程末段仍标记 incomplete. 活动的
sandbox-ctl 无法观察自身最后一次读取之后的退出/保存 CPU, 因而其终态记录保留
已知总量但不声称完整. 新 Host owner 重新打开已有 usage 文件供后续累计时,
保留已知 CPU 累计, 但将 `live` 的跨进程历史标记 incomplete. 已保存或离线读取
的记录保留写入时的完整性标志; 标志描述已记录的前缀, 不证明其后未知尾部完整.
即使最后记录正常封口, 也不能排除其后有一次
消耗 CPU 却未写出任何记录便崩溃的运行. 空文件和部分追加同样无法证明历史
完整. 只有新建文件能确认逻辑历史的已知起点; 本实现不增加同步启动标记或 WAL
来证明各次运行相邻.

### 4.2 Guest 内存和 Balloon

Guest 读取 `/proc/zoneinfo`、`/proc/buddyinfo`, 匹配 node/zone identity 及
各 CPU pageset, 返回 `PresentPages`、`BuddyFreePages`、`PCPFreePages`、
`PageSize` 和 domain/status. 已填充的受支持 RAM zone 为 DMA、DMA32、Normal;
排除 Device/PMEM. 其他已填充域为 unsupported, 不由 `spanned` 或 Host
capacity 猜测.

```text
G = (PresentPages - BuddyFreePages - PCPFreePages) * PageSize
B = validated CH Capacity - vm.info.memory_actual_size
M = G - B
```

只有 `M` 进入 Gauge 及峰值, `max(G)-max(B)` 不是它的峰值. 缺字段、域不一致、
负值及溢出使观测无效, 不 clamp 成零. CH Capacity 不能代替 Guest present.

确认 no-device 时 `B=0`. 设备存在但 actual 未知、只有 seed 或已过期时, 内存
缺测而不是零. actual 与 target 不同仍可有效, 不等待收敛. Desired/accepted
target、resize 参数及控制器 Budget 都不是 Guest usage.

只有真正成功且通过范围检查的 CH 读取更新 actual 起止时间、序号和 live
instance identity. 目标写入、缓存返回不刷新时间, restored seed 不是新读取.
Sampler 优先复用合格 actual, 否则用已有 client 至多补一次有界只读查询.
mutation/API 锁忙时立即拒绝准入, 不等待或创建替代 worker. 既有控制顺序和
单写者约束不变.

包含 CH 实际读取及 Guest 请求窗口的联合区间必须放进一个 sample interval.
这是跨来源采样近似, 不是原子物理内存真值. 不需要 Guest Balloon proc 字段、
内核补丁、inflate/deflate 事件差值 fallback 或新增 CH statistics ABI.

### 4.3 文件系统和 Host RSS

磁盘组装为 single 可写 ext4 root、overlay 背后的原始 ext4 upper 及每个
受管理 data disk 保留私有 CLOEXEC 句柄. switch-root 和恢复后的 mount 保持
这些句柄有效, 应用 exec 不继承它们. 同盘 bind/empty volume 不另登记文件系统.
不枚举任意 mount, 不查询用户 NFS/FUSE, 不运行 `du`.

```text
filesystem occupancy = (f_blocks - f_bfree) * unit
unit = nonzero f_frsize, otherwise f_bsize
```

不使用 `f_bavail`. 恢复后的已有占用完整计入, 不扣启动初值. RSS 只读取两个
进程的 status, 将 kB 换算为 byte, 不扫描 `smaps`. 这些分项既不是全部非 Guest
内存, 也不是独占物理成本.

## 5. 采样与区间摘要

默认每秒观测并立即归并, 内存和磁盘均不保存秒级序列. 专用可复用管理连接承载
`usage_request`/`usage_response`; 原始字段、准入、deadline 余量及 quiesce
规则见 [Guest ABI §4.11](sandbox-init_zh.md#usage-observations).

每个请求有运行代次内单调 ID. 迟到、重复或旧代次结果丢弃, 重连只读新数据.
代表时间使用实际 Host 请求窗口中点 `start.Add(end.Sub(start)/2)`, 保留
单调语义, 不跨 Host/Guest UTC 相减. 摘要 `max_read_window_ns` 保留合格
窗口的最大宽度, 不保存逐次窗口.

同源、同时间域的两个相邻有效 Gauge 观测, 请求/时间递增且间隔不超过两倍
sample interval 时, 使用左端积分:

```text
dt = right_time - left_time
integral_total += left_value * dt
covered_total  += dt
span_total     += dt
```

失败、明确作废、已知遗漏调度槽、来源变化、时间非递增及长间隔都会断开连续性.
缺测不补零, 不无限延续旧值. span/covered 同域, 缺失时间只进入 span 一次,
不在失败和恢复时重复计算. paused/unsupported 域不增加 span. 首样本只建立
基线和采样峰值, 不产生面积.

面积为检查溢出的无符号 128 位整数. 两个累计端点的平均值为
`area_difference / covered_difference`, 分母为零时不可用. span/covered
单独反映覆盖率. 峰值是采样峰值, 不保证连续时间内真正最大值.

保存不外推到尚未观测的五分钟边界. 最后有效端点跨保存保留, 跨边界区间在右端
样本到来时只封闭一次; 左端值可以参与该新区间峰值, 更早无关历史峰值不延续.
记录窗口包含面积、span/covered、样本数、峰值及位置、最大读窗口, 保存不重置
累计总量.

## 6. 持久化与文件格式

文件唯一位置为 `BaseDir/<SandboxID>.usage`, BaseDir 按既有生命周期从
BaseRoot/PathID 派生. 逻辑身份不是 RunID, 正常重启延续同文件. 新 SandboxID
的克隆具有独立历史. Usage 不进入 E/S, 恢复旧业务快照不会回滚用量文件.

Host 只保留常数个状态:

```text
S = confirmed saved baseline + file offset
F = immutable record currently being saved/reconciled
A = active observations merged while F is in flight
```

Live 累计包含 F/A. 成功只把 S 推进到 F. 追加失败后, 只有 truncate-to-S 加
Sync 确定回退, 才能把区间统计并回 A. Write/Sync/truncate 结果不确定时保留
F 原身份及内容, 先处理尾部. CRC 可读不是当前进程的 Sync 确认; 新进程采用
完整存活记录是另一种情形.

每个 frame 的 16-byte header 依次为 `KUUSAGE1`, little-endian uint16
format/algorithm version (均为 1), uint32 frame 总长度. Payload 用显式
signed/unsigned varint、有界 UTF-8 字符串和数组编码 Record/Snapshot、来源
基线及区间统计; 128 位值编码 high/low uint64, 不 dump 内存. 16-byte footer
依次为 header+payload 的 CRC32C、little-endian uint32 总长度、`USAGEEND`.
frame 上限 512 KiB, 字符串 256 byte, Counter 1,027 个, Gauge 14 个.
字段顺序由 [format.go](../pkg/usage/format.go) 定义, 不引入数据库或压缩框架.

Host manager 在接纳身份和指标元数据前检查这些限制; Counter 和 Gauge 名称共用
一个命名空间. 新增指标或加长来源时为数值增长预留空间. 单项字符串/数组上限不
代表其最大组合也能放进一条 frame. 缺少增长空间的已保存记录仍可离线读取,
但新 live owner 打开失败, 释放锁且不改变已有字节. 已有 Gauge 的较新请求若
元数据非法, 以 `invalid` 断开连续性, 不保存非法字符串; 零、重复和更旧请求仍
不改变状态. Gauge 空来源和空状态仍可编码.

当前恢复至多读取尾部两个最大 frame 的范围, 找到最后有效自包含记录及可识别的
不完整追加. 不扫描或声明校验全部历史. 历史查询逐条验证遇到的 frame, 损坏
明确报错; 不接受错误身份、版本、长度或校验. 文件首条必须为 sequence 1.
非零分页 cursor 验证紧邻的前一完整帧及跨边界序号, 包括每页仅一条记录的情况;
不会扫描或声明校验未请求的整个前缀. 新运行保留累计端点, 重建当前
单调位置和 Gauge 基线, 不用旧单调位置与当前时钟相减.

## 7. 可靠性与生命周期

Sampler 初始化失败时报告既有的不可用/错误状态, 释放文件所有权, 不保存或截断
checkpoint. 原有字节仍可离线读取, 新建文件可能保持为空. 该次尝试不暴露未初始化
的 live/saved 视图, 也不写入新的 closed 记录.

若初始化成功, 随后的 Host 设置或 CH 启动失败, 收尾仍按既有预算尽力读取并
保存实际 sandbox-ctl CPU/RSS. 这是 Host 进程用量, 不代表 Guest 已运行. 之前的累计值
和完整记录字节保持不变; 新 epoch 遵循第 4.1 节的保守完整性规则. CH 未能启动
不免除已经消耗的 Host CPU.

采样接入 Host 进程生命周期. Host 只有在 launch/restore ready 后才开始 Guest
round, ctl.sock 存在不算 Guest ready. 捕获前 Host 关闭 usage 准入并断开 Gauge
连续性. Guest 作废旧代次并排空连接, 不无限等待阻塞文件系统读取; 原执行槽
直到实际读取完成前一直 busy. Restore/attach 在发送 ACK 前重新开放原始 usage
准入, 避免 Host 的立即首轮请求碰到尚未开放的 gate. True memory restore 在
ACK 前仅重置一次旧 Host epoch/request ID; 同 VM attach 保留它们. 原始读取
可早于应用 thaw, exec、plugin、app 和资源控制器 mem_report 准入仍等待 thaw
成功. ACK/thaw 失败时关闭该次新开放的 usage 准入, 作废连接并有界 join; 普通
MUX 重连不会因此暂停原本活动的 usage 流. 重试继承尚未完成的开放操作的收尾
责任, 因此两次尝试都失败时仍关闭准入; 旧失败不会回滚已成功的重试或后继代次.
不重建 worker 绕过阻塞槽的有界性.

正常退出在有界预算内尽力最终观测和保存. Manager 开始关闭时先禁止接纳新观测,
再冻结最终记录; 既有 writer 仍可在预算内完成 F 并封闭 A. 已保存的 closed
记录不代表本次收尾已完成. 重试不确定 F 可能耗尽预算, 未保存 A 或 closed
checkpoint. Usage 失败不改变业务结果. Writer
持有 pinned 文件/目录 FD, 迟到写入不能重建已删除目录, 也不能写到同名新实例.
删除不等待保存/导出成功, 不新增保留策略. Usage 不增加业务 fsync、flush、
discard、drop_caches 或懒加载操作.

异常退出可丢失未保存尾部. 五分钟是正常保存周期, 不是持续故障或存储卡住时的
无条件损失上限. Writer 只有一个执行槽, 不排队积累五分钟批次. Guest 慢盘
同样每来源只有一个槽, 内存及健康文件系统已完成的新值仍可报告, 阻塞项报告
timeout/busy. 不保证掉电下无损持久化, SIGKILL 测试不能证明掉电语义.

## 8. 性能与验证

磁盘按累计 checkpoint 增长, 不按秒级样本增长. 记录大小取决于 vCPU/磁盘数、
来源字符串及 varint 数值, 1 KiB 不是实测常数. CPU/Gauge 归并及保存测试位于
[pkg/usage](../pkg/usage), 协议及阻塞来源测试属于
[guestlink](../pkg/guestlink) 和 [sandbox-init](../cmd/sandbox-init).
组件自有 [usage E2E](../test/e2e/e2e_usage.sh) 由
[run_all.sh](../test/e2e/run_all.sh) 发现, 必须使用真实 KVM, 并核验 Guest 内
运行的 init 哈希与所提供的新 runtime bundle 一致. 短保存周期案例验证集成.
Restore/clone 检查采用一分钟采样周期, 要求第一次周期 tick 前已得到新的内存/
文件系统观测, 不允许周期重试掩盖 ACK 后立即首轮漏采.
独立的 `defaults` 案例等待实际默认五分钟保存. 两者都不是生产密度性能测量.

OOM 案例先建立 exec/MUX, 测试 probe 在分配压力内存前等待 stdin 的一个字节.
有界的测试专用 LaunchPort 中继原样转发普通 hello/MUX 流及 report/ACK 帧.
新 epoch/sequence 的 ACK 成功转发只提供报告交付边界, 不证明 Host 控制事务
已完成. 测试要求真实 CH 收敛基线及新鲜交付边界, 然后才启动压力.
只有在首次 target 变化之前 actual 增长, 且该区间没有已接受或结果不明的
resize, 才计为自主 deflate. 重复报告、错误 ACK、过期边界及控制 resize 之后
的 deflate 均不合格; 正常控制循环始终运行.
记录的起跑字节写入端点使用 Host 单调时钟, 不是 Guest 分配时间戳,
也不是 Guest 调度延迟的测量.

[存储故障 E2E](../test/e2e/e2e_usage_faults.sh) 使用私有有界 tmpfs 产生真实
ENOSPC, 并用限定 usage 路径的 `strace` 注入 Sync 失败和延迟写入; 还覆盖
SIGKILL 与亚秒级 Guest 运行. 注入不作用于业务可写盘的同步操作, 也不模拟
物理掉电. Host 强杀案例在崩溃前固定并验证自己的 CH 子进程, 然后通过
pidfd 确认该子进程在有界清理预算内退出, 不把清理留给 runner.
需要 Linux/Python pidfd 支持、`strace`、mount 权限及普通 KVM E2E 前置条件.

[慢来源 E2E](../test/e2e/e2e_usage_sources.sh) 按设备身份选择一个已登记的
数据文件系统句柄, 核对 CLOEXEC 后, 仅在可丢弃 Guest 内延迟该 `fstatfs`
返回. 测试要求首次 timeout、同一个已占用槽后续返回 busy、内存及健康文件
系统持续获得新覆盖, 且 trace 中只有一次该文件系统调用. 两块数据盘均执行
真实写入; 延迟读取后新增的字节用于区分释放后的新观测与旧 buffer 重放.
单个长存测试 exec 记录 Guest 全部 FD、身份、线程和 RSS, 不为每次观测创建
exec, 也不把线程数称为 goroutine 数. 文件系统案例默认故障持续 30 秒,
`--seconds 60` 可延长验证. 这是系统调用返回延迟, 不是物理设备故障或掉电测试.
已安装的原生 `strace` 及 `ldd` 解析的 loader、依赖库仅复制到可丢弃应用镜像,
不进入产品 runtime.
其中 `ch-info` 和 `ch-resize` 案例在真实 CH 可执行文件前放置仅用于测试的
Unix HTTP 中继, 原样转发响应正文并暂扣一次实际响应 12 秒. Resize 由真实
Guest 分配触发, 不是测试直接调用 resize; 暂扣其响应覆盖既有 mutation gate/
API 锁控制事务. 选择时要求 target 低于先前真实 target, 暂扣的是 PUT 响应
或紧随其后的确认 GET. 旧观测过期后, Guest 内存必须缺测, 文件系统/RSS 覆盖继续
增长, 且不能堆积更多 CH 请求. 恢复必须经过新的成功 `vm.info` 读取.
这些有界功能故障不测量无测试中继时的 API 时延或生产密度开销, 也不声称
已逐一确定所有 info 读取的调用者.
`vsock` 案例中继真实的专用 usage 帧并保持普通管理/MUX 流量. 在一个受管理
文件系统调用仍被占用时, 执行八次断连、一次迟到响应、已捕获的旧响应重放,
以及超过原始整轮 deadline 的慢分段传输. 跨新连接的 Guest 原始响应必须
继续报告同一个 busy 槽. 请求失败断开 Gauge 覆盖, 不补零; 原生 CPU Counter
仍是独立来源. 一个观察 exec 跨全部十一项故障持续传送资源诊断,
注入前以第一份完整输出确认 MUX 已建立. 额外的健康检查/停止 exec 位于
全部 FD 的观测窗口之外, 此时文件系统仍阻塞; 不丢弃任何 FD 样本.
`restore` 案例让同一个已占用文件系统槽跨同进程 quiesce/thaw, 再以相同
SandboxID/usage 文件执行 lazy 内存恢复. 有界的测试专用 tracer owner 在
启动 `strace` 前仅将自己加入可丢弃 Guest 的 root cgroup; 不移动 PID 1、
应用或 exec-join 进程. Guest reaper 负责被收养的 helper, helper 是其 tracer
的唯一 wait 所有者. 这样不会受到普通 exec 在 quiesce 时按设计被终止的影响.
测试专用 CH wrapper 只改私有恢复运行状态中的 vsock 路径, 不改不可变业务
快照. 中继在首份恢复后响应中仅将 epoch 替换为真实上一运行代次, 保留新
request ID 和观测值. 这是明确的身份字段破坏, 不是原样重放历史帧.
必须通过 Host 关闭连接以及另一连接上的新请求证明拒绝; 原始响应仍须报告
原来的 busy 槽, 同时内存及健康文件系统可用. 原始读取之后及恢复之后的写入
用于区分新观测与迟到旧 buffer, 首个新值不能积分缺测区间. 原实例和恢复实例
的 tracer owner 都必须脱离; 失败清理仍停止自有 VM 并回收 Host 测试子进程.
这些是有限时长的功能/资源边界检查, 不是通宵耐久、恢复失败回滚或物理掉电证据.

在没有其他并发测试负载时, 以 root 权限和已组装的 `BIN` 运行
[off/on 测量脚本](../test/e2e/usage_perf.py):

```bash
python3 test/e2e/usage_perf.py --densities 1,4 --seconds 30 --repeat 3
python3 test/e2e/usage_perf.py --densities 1,4 --seconds 30 --repeat 3 --trace
```

基线测量原生进程 CPU、RssAnon/RssFile、FD/thread、非 dead 的 Go G 数量、
实际记录字节数、集中停止耗时和端到端 exec p95/p99. Go G 包含 runtime 系统
goroutine, 不等于 `runtime.NumGoroutine`. 精确二进制 DWARF/symbol 检查需要
`gdb` 和 Go tools. 诊断产物保留原生 proc stat 文本、独立的 utime/stime/
guest_time 端点、读取窗口、boot identity 和 tick 尺度.
`proc-observations.json` 在采样失败时仍保留已完成的读取; 不完整的配对/窗口
明确标记, 不伪造缺失端点. 这些测试产物与紧凑的 usage 文件独立.

独立的 Linux amd64 跟踪运行需要具备指令偏移支持的
`bpftrace` 构建、tracefs 权限及 initial PID namespace (`BPFTRACE_BIN` 可选择
已安装工具). 预检查在挂接目标探针前拒绝 kernel 与 `/proc` PID 身份不一致的
环境; 它核验自有短生命周期子进程的真实 sched exec 事件, 不只检查语义随
bpftrace 版本变化的裸 `pid` builtin. 空结果或失败不能解释为零开销.
跟踪补充唤醒、mallocgc 请求/请求字节、
usage framing 通信、CH info 请求、
有争用的 API mutex 等待及保存 worker 耗时. 普通指令探针按精确二进制核验,
不插入 Go 返回跳板. 跟踪会扰动时序, 其延迟不能替代无跟踪基线. 脚本检查可用
内存再准入密度, 不修改宿主资源上限.

Off/on 对比必须使用相同源集、runtime/kernel/CH、配置、密度及负载, 报告
Guest/Host CPU、唤醒、分配、FD/goroutine、常驻内存、管理通信、CH 查询次数/
锁等待、记录字节数、保存耗时及业务 p95/p99. 单元模型和普通进程实验不能替代
CH/KVM 证据. 提交/run 对应证据及未通过验收保留在 PR 和可信 CI artifact,
本文不从设计推导虚构 benchmark 数字.

## 9. See Also

- [Sandbox 生命周期](sandbox_zh.md) — 配置归属、捕获及恢复.
- [Guest ABI](sandbox-init_zh.md) — 原始观测和传输预算.
- [Cloud Hypervisor](cloud-hypervisor_zh.md) — 既有 actual/control 合同.
- [README](../README_zh.md) — 构建、测试及发布入口.
