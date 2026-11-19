[English](disk-qos-linux.md) | [简体中文](disk-qos-linux_zh.md)

# Linux 宿主 I/O 控制

`resources.diff_cow` 管理 sandboxer 的速率和脏内存预算。本文另外说明 diffstore
线程 I/O 优先级，以及 runner/builder service 的宿主块 I/O 控制。它们作用于不同
边界，不提供最低吞吐量或延迟保证。

## Diffstore DIO 优先级

diffstore 的公共定位读写 helper 会在实际同步 I/O 期间尝试使用 best-effort class
的最高等级 `BE0`，原有 EINTR 重试循环包含在同一次作用域中，无新增配置项。
已有 RT、BE0，以及 I/O 优先级未设置但 CPU 策略为 FIFO、RR、DEADLINE 的线程，
保留已有的更高优先级，在原调用线程执行。

需要临时提升时，由 helper 独占生命周期的 goroutine 执行 I/O。结束后恢复执行
线程的完整原始优先级值。恢复失败时，该 goroutine 保留自己的线程锁并返回，
由 Go runtime 将该线程移出调度池：普通 OS 线程终止，初始 OS 线程则永久停用，
两者都不会再运行其他 goroutine。安全性不依赖 export/main 等任意调用方退出，
也不依赖已经固定线程的 vhost worker 如何执行自己的解锁 defer。

查询、设置、恢复优先级失败，只按有限的失败阶段各记录一次降级告警。它们不
替换真实 I/O 的字节数或错误，不使成功写变成失败，不引起重放，也不引入新的
cache fatal 条件。真实 I/O 错误继续按已有逻辑处理；部分完成的 I/O 不会因为
优先级管理失败而重新执行。

### 生效条件与成本

- 优先级效果依赖调度器。BFQ 区分 BE 等级；普通 `nice=0` 线程的未设置优先级
  通常对应 BE4。
- 按 class 调度的 mq-deadline 实现（包括已核对的 Linux 6.18）把 NONE 和 BE
  放入同一 class 队列，BE0 相比 BE4 不获得独立的 BE 等级调度份额。
- helper 不发现、验证或切换宿主调度器。`ioprio_set` 成功不代表物理设备收益。
  在不区分 BE 等级的设备上，helper 的执行成本仍然存在；cgroup 优先级策略及
  上层资源约束也可能影响实际结果。
- 请求 O_DIRECT 不等于证明绕过物理缓存。现有路径退回 buffered write 后，
  后续文件系统异步写回通常不继承生成脏页线程的临时 ioprio。
- 从已固定线程的 vhost worker 转交 I/O 有可测成本。cache hit 不进入该 syscall
  helper，但频繁的小 DIO 可能明显受影响。需要对目标设备和负载验收，不能预设
  端到端性能一定提升；实测见下文。

作用域只覆盖 diffstore 公共 DIO read/write 路径，不永久提升整个 vhost worker，
不额外为每盘固定一个长期 OS 线程，也不新增全局 I/O 队列。

## 确认宿主 backing device

在原生宿主上选择实际存在的 diff 文件，先做只读检查：

```sh
DIFF_PATH=/absolute/path/to/an/existing/diff-file
findmnt -T "$DIFF_PATH" -o TARGET,SOURCE,FSTYPE,MAJ:MIN
lsblk -o NAME,TYPE,PKNAME,MAJ:MIN,FSTYPE,MOUNTPOINTS
stat -fc %T /sys/fs/cgroup
cat /sys/fs/cgroup/cgroup.controllers
```

找到真正承载存储的设备。下面假设已经核实为 `/dev/nvme0n1`；部署时必须换成
实际设备，不能照抄：

```sh
QOS_DEVICE=/dev/nvme0n1
QOS_KNAME=$(lsblk -dn -o KNAME "$QOS_DEVICE")
lsblk -dn -o NAME,TYPE,MAJ:MIN "$QOS_DEVICE"
cat "/sys/class/block/$QOS_KNAME/queue/scheduler"
```

`[bfq]` 才表示 BFQ 已激活。这里不切换共享设备的调度器。dm、RAID 等叠加存储
必须核对完整设备映射；Guest 设备名或未经核实的文件系统设备反查结果不足以
确定宿主调度、限额对象。

## 配置 runner/builder 父 service

orchestrator 的 `resource_listen.pools[].unit_dropins` 接收 systemd fragment
的绝对路径。同一资源池的 fragment 会复制到选择该资源池的 runner、builder
模板。

示例 `/etc/kuasar/pools/io.conf`，设备采用上面已经核实的假设：

```ini
[Service]
IOAccounting=yes
IOReadBandwidthMax=/dev/nvme0n1 50M
IOWriteBandwidthMax=/dev/nvme0n1 20M
IOReadIOPSMax=/dev/nvme0n1 1000
IOWriteIOPSMax=/dev/nvme0n1 500
```

数值仅为每个 service 实例的示例上限，不是默认值。这里 systemd 的 `M` 使用
十进制。限额计量宿主块 I/O，其请求数和字节数不同于 sandboxer 的逻辑磁盘 I/O。

可按需增加 `IOWeight=200`。它是该层级兄弟 cgroup 之间的相对权重，不是保底
带宽。BFQ group scheduling 需要内核支持且设备正在使用 BFQ。支持 BFQ 的
systemd 版本也通过 `io.bfq.weight` 应用该设置，部署时应读回验证。不同 slice
中 service 的权重，不直接定义这些 slice 彼此之间的资源份额。

最小相关 orchestrator 配置：

```yaml
resource_listen:
  enabled: true
  pools:
    - name: default
      unit_dropins:
        - /etc/kuasar/pools/io.conf
units:
  install: true
  dir: /etc/systemd/system
  runner_pools:
    - name: runner-io
      resource_pool: default
      unit: runner-io@.service
      size: 0
  builder_pools:
    - name: builder-io
      resource_pool: default
      unit: builder-io@.service
      size: 0
```

现有部署应在已有目标资源池中增加 fragment，保留其 capacity、allocatable
预算和其他 pool 条目，不要用此最小示例替换完整列表。`size: 0` 只是不维持
空闲预热实例，不代表执行容量为零。

conductor 启动时调用 `InstallUnits`，安装模板和 fragment，文件变化时 reload
systemd。示例会生成：

```text
/etc/systemd/system/runner-io@.service.d/50-kuasar-resource-pool-0000.conf
/etc/systemd/system/builder-io@.service.d/50-kuasar-resource-pool-0000.conf
```

通过现有 conductor 配置发布流程加载。`units.install: false` 时由运维流程
负责安装，程序不会读取或安装上述源 fragment。

**每个 service 实例各自获得限额。** 同一 pool 中多个实例不会自动共享一个
50 MB/s 总额度。共同 slice 的聚合限制属于另一个 cgroup 层级的部署决策。

生成的父 service 内有兄弟 `ctl` 和 `vmm` 子组。`node-ctl` 及其直接启动的
`sandbox-ctl` 位于 `ctl`，VMM 启动于 `vmm`。父 service 是两者共同的边界；
只限制 `vmm` 会漏掉 sandbox-ctl 中的 diffstore I/O。

## 检查运行中的实例

```sh
systemctl cat runner-io@.service builder-io@.service
systemctl list-units --all --no-pager \
  'runner-io@*.service' 'builder-io@*.service'
```

从输出中选择实际存在的实例，再检查属性和 cgroup：

```sh
QOS_UNIT='runner-io@<actual-run-id>.service'
systemctl show "$QOS_UNIT" \
  -p ControlGroup -p Slice -p Delegate -p IOAccounting -p IOWeight \
  -p IOReadBandwidthMax -p IOWriteBandwidthMax \
  -p IOReadIOPSMax -p IOWriteIOPSMax
QOS_CGROUP=$(systemctl show "$QOS_UNIT" -p ControlGroup --value)
cat "/sys/fs/cgroup$QOS_CGROUP/io.max"
cat "/sys/fs/cgroup$QOS_CGROUP/io.stat"
cat "/sys/fs/cgroup$QOS_CGROUP/ctl/cgroup.procs"
cat "/sys/fs/cgroup$QOS_CGROUP/vmm/cgroup.procs"
```

使用 BFQ 权重时，还应检查
`/sys/fs/cgroup$QOS_CGROUP/io.bfq.weight`。核实 `io.max` 的 major:minor、
进程归属和隔离测试期间的计量增量，不能只凭 fragment 存在判断生效。
`io.max` 允许短时 burst，不是精确的逐请求完成时间表。

普通 buffered writeback 需要文件系统支持 cgroup writeback，并按 inode
跟踪归属。内核文档列出的支持文件系统包括 ext2、ext4、btrfs、f2fs、xfs；多个
cgroup 同时写同一 inode 时，归属不一定准确。仅确认进程位于父 unit 内，不足以
证明所有关联文件系统写回均准确归入该 unit。

## 验证范围与实测

2026-11-19 的原生验证使用 Linux 6.6、Go 1.26.4 和 x86-64 宿主。普通及 race
测试均通过。测试真实设置 BE0，并注入恢复失败，确认对应线程从
`/proc/self/task` 消失；设置失败保留真实 I/O 结果。RT 保留分支采用状态注入
验证，没有把宿主线程实际提升为 RT。

另外的 Go 1.26.4 线程停用回归分别强制 helper 在普通线程和初始线程执行，检查普通
线程消失、初始线程保留但不被复用、32 个同时固定的探测线程保持原优先级，
以及后续定位读取成功并交付完整数据。

短时只读设备测试使用本任务创建的 64 MiB loop 设备、ext4 和 16 MiB 数据文件。
只给该 loop 选择 BFQ，共享 backing device 保持原来的 `none`。loop 的 backing
file 已缓存在内存中，因此以下数据属于快速虚拟设备测试，不是物理盘性能，也
不是生产容量推荐。

| 4 KiB 读取场景 | 裸调用，caller 已 pin | 优先级 helper，caller 已 pin |
| --- | ---: | ---: |
| `/dev/zero` syscall 开销 | 0.416–0.418 µs | 13.10–13.16 µs |
| 自有 BFQ loop，无竞争读者 | 10.47–11.53 µs | 23.47–23.61 µs；首个样本 79.73 µs |
| 自有 BFQ loop，BE4 后台读者 | 11.63–18.77 µs | 24.75–38.29 µs |

该基准中，完整 DIO wrapper 的每次提升调用产生 224 字节、3 次分配。caller 已
处于 BE0 时避免转交，空载 loop 测得 10.81–11.18 µs。结果表明 helper 成本不可
忽略，**该快速 loop 负载没有获得端到端加速**。

独立的固定线程 fio 对照中，BE4/BE4 约为 78.5k/78.5k IOPS，BE0/BE4 约为
73.5k/16.1k。这验证了该自有 BFQ loop 能区分 BE 等级；它不是承诺的比例，也
不能抵消上述实际 helper 成本。

另一个独立 transient 父 service 在未改变调度器的 backing device 上设置
`8:0 rbps=2097152`。其 `ctl`、`vmm` 子组内的 direct reader 在 5.007 秒内分别
约为 1.047、1.060 MB/s，父 `io.stat` 记录两者合计 10,551,296 读字节。父层
subtree 只启用 `cpu memory`，仍由父层自身的 IO 限额覆盖两支。测试后已删除
transient service、卸载文件系统并解绑 loop。

可在准备好的独立数据文件上复现只读 DIO benchmark：

```sh
SANDBOXER_DIO_BENCH_FILE=/absolute/path/to/an/isolated/data-file \
  go test ./pkg/diffstore/internal/directio -run '^$' \
  -bench '^BenchmarkIOPriorityDirectRead$' -benchtime=1000x -count=3
```

该 benchmark 不创建文件、不切换调度器、不清空宿主缓存。目标生产设备上的
竞争效果与延迟仍需按实际部署条件验收。

## 参考

- [Go runtime 线程停用实现（`mexit`）](https://go.dev/src/runtime/proc.go)
- [Linux I/O priorities](https://docs.kernel.org/block/ioprio.html)
- [BFQ 调度与 group support](https://docs.kernel.org/block/bfq-iosched.html)
- [Linux v6.18 mq-deadline 实现](https://github.com/torvalds/linux/blob/v6.18/block/mq-deadline.c)
- [Cgroup v2 IO 与 writeback](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- [systemd resource-control](https://www.freedesktop.org/software/systemd/man/latest/systemd.resource-control.html)
- [Orchestrator 资源池字段](https://github.com/kuasar-sandbox/orchestrator/blob/1d82400e202d894390e409b385233a8f1b7937e7/config/resource_pools.go)
- [Orchestrator unit 安装](https://github.com/kuasar-sandbox/orchestrator/blob/1d82400e202d894390e409b385233a8f1b7937e7/internal/orch/units.go)
