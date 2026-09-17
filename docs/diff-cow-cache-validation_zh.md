[English](diff-cow-cache-validation.md) | [简体中文](diff-cow-cache-validation_zh.md)

# Issue #230 验证观测

[缓存/I/O 契约](diff-cow-cache_zh.md) 为 sandbox-ctl 明文缓存设定上限，同时改变 I/O 完成边界
和性能。以下原始 d601fa0 测量显示文件 page-cache 驻留降低，但在本存储上，相比 buffered
基线存在显著吞吐回退。保留这些历史观测，不作为性能目标。修订版测量及 parent 独立提供的
d601 CH/KVM 证据在下文单独记录。

## 兼容性修正（#238）

下文历史上的 tmpfs 拒绝及文件系统专有 legacy 限制已被
[issue #238](https://github.com/kuasar-sandbox/sandboxer/issues/238) 取代。每个活动 body
现在通过同一对齐定位 I/O API 请求 O_DIRECT，使用通用 STATX_DIOALIGN 约束；约束不可用
或双零时使用保守的 4096 字节对齐。运行时不再识别文件系统类型、inode flags 或挂载策略。
不支持 O_DIRECT 设置时，通过同一应用缓冲继续普通文件 I/O；其他设置错误和数据 I/O 错误
仍然报错，不在数据 I/O 失败后进行 buffered 重试。成功的 O_DIRECT 请求不证明所有底层
实现都会绕过物理磁盘缓存。Tmpfs 文件在内存/swap 中的存储与有界共享进程缓存分开；
参见[当前内存与 I/O 契约](diff-cow-cache_zh.md)。下文历史数据、源码哈希及磁盘上的
DIO/mincore 测量保持不变，不是 tmpfs 存储驻留或 tmpfs guest 行为的测量。

## 环境与方法

测量日期为 2026-09-17，Linux/amd64，Intel Xeon Gold 6266C（可见 88 个逻辑 CPU），
Go 1.26.5，kernel `6.6.0-159.4.6.157.20260713.a4e2472763b2.oe2403sp4.x86_64`。
源码隔离于 `/var/tmp/diff-cow-230-lb6t8B`；存储为 `/dev/sdb2` 上的 ext4，文件系统块为
4 KiB。`/tmp` 是 tmpfs。Go 测试使用任务持有、短且 canonical 的磁盘目录 `/var/q` 作为
TMPDIR，因为部分已有 Unix-socket fixture 在长 TMPDIR 下超出路径限制，另一些已有测试
比较 canonical 路径。没有为此弱化任何测试。

B 为精确 main `b1db083dee2cacac141d4033bbf41b518cf80c9e`，仅加入相同 benchmark harness
和基线 adapter。历史 C 为 `d601fa0`，即本 issue 的初版实现，使用默认共享总量 32 MiB / 脏子集 16 MiB。
[公共 harness](../pkg/vhost/blk_cow_workingset_bench_test.go) 在两版本中保持相同，搭配
[候选 adapter](../pkg/vhost/cow_cache_bench_adapter_test.go) 或以下基线替代文件：

```go
package vhost

func newWorkingSetCache() (workingSetCache, error) {
    return workingSetCache{
        drain: func() error { return nil },
        close: func() error { return nil },
        stats: func() map[string]float64 { return nil },
    }, nil
}
```

在同一磁盘、磁盘上的 TMPDIR 中先运行基线，再单独运行候选。每个 case 完整执行一次工作
负载；这是单次样本，不是置信区间。此前不含附加 `/proc/self/io` 计数器的首次运行也已完成；
以下表格统一报告随后含计数器的完整运行，没有挑选较快样本。

```sh
go test -tags no_rocksdb ./pkg/vhost -run '^$'   -bench '^BenchmarkCOWWorkingSet$' -benchtime=1x -count=1 -timeout 15m
```

每个工作负载跨越 256 MiB，即总缓存的八倍。顺序请求为 1 MiB。随机 4 KiB 和 512 字节请求
按确定性排列访问 65,536 页；512 字节请求传输 32 MiB，但物化 256 MiB 数据占用。
热点覆盖先填满全部数据集，再将 90% 的 4 KiB 更新发送到 4 MiB 热区，10% 分布于全数据集。
读 case 也先填满数据集。多盘 case 交替向两个 128 MiB diff 发出 1 MiB 请求，两盘共用一份
预算。加密采用现有本地 XTS 格式。前台只有一个请求者；这是后端测量，不是 guest/应用基准。

Setup 不计入耗时与 I/O 差值。请求延迟包含额度等待；admission 截止于最后一个前台请求
返回。C total 包含 Drain，吞吐按该总耗时计算。B 同步执行 buffered syscall，没有用户态
回写队列，所以其 adapter 的 Drain 为空：**B total 衡量 buffered 完成，不是介质完成**。
两版本都没有为比较强制 fsync、DONTNEED 或 drop_caches。读样本有意保留 B 自然变热的
文件缓存。新文件元数据初始化在测量外保留原有 sync 行为；guest FLUSH、回写和 Drain 不增加 sync。

## d601 历史耗时、吞吐与请求延迟

在所示精度下 B admission 与 total 相等。吞吐为逻辑 MiB 除以 total；p50/p99 对应单个请求，
不包括 Drain 本身。

| 工作负载 | XTS | B admission/total (s) | C admission (s) | C total (s) | B / C (MiB/s) | B p50 / p99 (µs) | C p50 / p99 (µs) |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `seq-write` | false | 0.1329 | 1.192 | 1.255 | 1926 / 204 | 500.7 / 701 | 3795 / 7374 |
| `random-4k` | false | 0.1847 | 27.44 | 29.26 | 1386 / 8.749 | 2.628 / 4.373 | 388.9 / 1530 |
| `partial-512` | false | 0.3087 | 26.92 | 28.73 | 103.7 / 1.114 | 3.932 / 12.6 | 386.7 / 1501 |
| `hot-overwrite` | false | 0.0683 | 2.113 | 3.901 | 3748 / 65.62 | 0.806 / 2.473 | 0.87 / 436 |
| `seq-read` | false | 0.06802 | 13.92 | 13.92 | 3764 / 18.4 | 244.7 / 335.1 | 54004 / 63544 |
| `random-read` | false | 0.07078 | 16.1 | 16.1 | 3617 / 15.9 | 0.986 / 1.501 | 196.1 / 604.1 |
| `multi-disk` | false | 0.1299 | 0.9948 | 1.059 | 1971 / 241.8 | 496.4 / 605.5 | 3984 / 6361 |
| `seq-write` | true | 1.177 | 1.88 | 1.994 | 217.5 / 128.4 | 4592 / 4716 | 7398 / 10808 |
| `random-4k` | true | 1.24 | 28.38 | 30.22 | 206.5 / 8.472 | 18.62 / 21.28 | 406.2 / 1464 |
| `partial-512` | true | 1.366 | 27.92 | 29.75 | 23.43 / 1.076 | 19.94 / 29.53 | 403.4 / 1443 |
| `hot-overwrite` | true | 1.108 | 2.148 | 3.946 | 231 / 64.88 | 16.68 / 18.82 | 0.527 / 450.3 |
| `seq-read` | true | 1.137 | 14.66 | 14.66 | 225.1 / 17.46 | 4462 / 4481 | 57156 / 68747 |
| `random-read` | true | 1.155 | 17.46 | 17.46 | 221.6 / 14.66 | 17.49 / 19.24 | 218.1 / 614.5 |
| `multi-disk` | true | 1.182 | 1.939 | 2.069 | 216.5 / 123.7 | 4602 / 4796 | 7916 / 11218 |

## d601 历史流量与内存证据

Body write 是成功活动 body syscall 的字节数，不含 setup。`proc write` / `proc read`
是 Linux `/proc/self/io` 中 `write_bytes` / `read_bytes` 的差值；属于文件系统 I/O 记账，
不是设备持久化或下层存储写放大的测量。尤其 buffered 脏页记账并不能证明计时结束时这些
字节已到达介质。每个测量区间的 `cancelled_write_bytes` 都为零。读行的逻辑流量表示读取。

| 工作负载 | XTS | 逻辑 MiB | B / C body write MiB | B / C proc write MiB | B / C proc read MiB |
| --- | --- | ---: | ---: | ---: | ---: |
| `seq-write` | false | 256 | 256 / 256 | 256 / 256 | 0 / 0 |
| `random-4k` | false | 256 | 256 / 256 | 256 / 256 | 0 / 0 |
| `partial-512` | false | 32 | 256 / 256 | 256 / 256 | 0 / 0 |
| `hot-overwrite` | false | 256 | 256 / 35.523 | 0 / 35.523 | 0 / 0 |
| `seq-read` | false | 256 | 0 / 0 | 0 / 0 | 0 / 256 |
| `random-read` | false | 256 | 0 / 0 | 0 / 0 | 0 / 253.91 |
| `multi-disk` | false | 256 | 256 / 256 | 256 / 256 | 0 / 0 |
| `seq-write` | true | 256 | 256 / 256 | 256 / 256 | 0 / 0 |
| `random-4k` | true | 256 | 256 / 256 | 256 / 256 | 0 / 0 |
| `partial-512` | true | 32 | 256 / 256 | 256 / 256 | 0 / 0 |
| `hot-overwrite` | true | 256 | 256 / 35.523 | 0 / 35.523 | 0 / 0 |
| `seq-read` | true | 256 | 0 / 0 | 0 / 0 | 0 / 256 |
| `random-read` | true | 256 | 0 / 0 | 0 / 0 | 0 / 253.91 |
| `multi-disk` | true | 256 | 256 / 256 | 256 / 256 | 0 / 0 |

顺序读的 body 调用在两版本中都传输 256 MiB。随机读中 B 传输 256 MiB，C 传输
253.914 MiB，少量余下请求命中 C 缓存。热点覆盖使 C 的 body 写量从 B 的 256 MiB 降至
35.523 MiB，但 C 含 Drain 的总耗时仍较长。首次 512 字节写保留整页物化语义：两版本都
将 32 MiB 逻辑流量物化为 256 MiB 写入。

`mincore` 在测量后只查询活动 body，不触碰映射页：B 的 **14 个 case 均驻留 256 MiB**，
C 的 **14 个 case 均为零**。没有计入 immutable template/base 输入、导出制品或 guest memfd。
C 的总量峰值及最终缓存 payload 在**所有 case 均为 32 MiB**，脏峰值为 **16 MiB**，
最终脏占用为**零**，包括两个多盘 case。读/热点 case 的峰值包含预填充。这些计数证明
在 8 倍工作集下预算共享，并不表示进程 RSS 上限。

样本末尾 Go HeapInuse 范围为 **B 3.33–5.31 MiB**、
**C 45.34–53.05 MiB**；进程 RSS 范围为
**B 18.61–25.55 MiB**、**C 71.34–94.94 MiB**。
这些是整个 benchmark 进程的观测，包含 Go 分配器保留、页/索引元数据、固定 I/O 映射和
harness buffer，不能单独归因为缓存开销，也不承诺最大 RSS。Buffered 文件缓存独立于
进程 RSS。没有根据这些样本虚构生产吞吐阈值。

## 正确性检查与剩余验证

已用 `-tags no_rocksdb` 执行 vhost/config/sandbox/restore/snapshot/CLI targeted tests、
六包 race suite、全仓 vet/build 和 broader Go suite。确定性 gate 覆盖新页/clean/dirty/
loading/writeback 额度、一页容量、大于缓存的请求、FIFO/LRU、冻结页读取、取消等待、
Discard、Close 和粘性错误。真实磁盘测试覆盖对齐、mmap buffer、当时要求的 tmpfs 拒绝（已被 #238 取代）、seeding、
明/密文重开及 mincore。制品 snapshot/export/readback 测试从未排空 dirty/writeback 数据
恢复，并在最后一次磁盘读取后发生故障时拒绝发布。这些测试模拟 CH API，并非真实 CH/KVM E2E。

Broader suite 保留一个已有 skip：`TestSendU64Reply_RoundTrip` 使用 net.Pipe 而非 UnixConn，
并注明由 `TestParseSetMemTable_Layout` 间接覆盖。另三个包没有测试文件。没有跳过新增测试。
本磁盘环境实际验证的是 ext4，而非真实 XFS 挂载；当时的测试套件包含确定性旧模式拒绝
测试，已被 #238 的通用对齐测试取代。
Parent 随后在不可变的 `d601fa0` 上通过默认 tag 的 targeted/全仓测试、vet、build
和六包 race，`GOFLAGS` 为空、`CGO_ENABLED=1`。该版本原先的 native 检查缺口已关闭；
trusted exact-integration CI 仍是独立要求。

最初以 `REQUIRE_KVM=1` 尝试 `diff_template`、`encrypted_diff`、`snapshot`、`disks`，
均因缺少任务内 `bin/cloud-hypervisor` 前置依赖而退出 **1**。保留这些失败尝试记录，
但**制品缺失阻碍现已解除**：parent 组装任务专用 CH/runtime/kernel BIN 后，在精确
`d601fa0` 上运行了全部四个真实 CH/KVM case，均退出 **0**。证据位于任务的
`kvm-d601-results.log`，以及 BMS 不可变目录
`/var/tmp/diff-cow-230-kvm-lb6t8B/d601/evidence`。默认/native 结果位于
`native-d601-tests.log` 和 `native-d601-full.log`（均退出 0），parent 也确认六包 race
通过。这些是 d601 的历史结果；文末另行记录最终修订运行代码的 KVM 证据。
通用调用方式仍为：

```sh
export REQUIRE_KVM=1 TMPDIR=/absolute/disk-backed/task/tmp
export BIN=/absolute/task/assembled/bin
for test in diff_template encrypted_diff snapshot disks; do
  bash "test/e2e/e2e_sandbox_${test}.sh" || exit "$?"
done
```

发布/合并前仍须独立 review 及正常 exact-integration CI；这些本地观测不能替代它们。

## 修订后的批处理测量

以下是在同一 host、磁盘、256 MiB 数据模式、请求大小和 32/16 MiB 限额下新执行的完整
顺序测量。B 仍为精确 `b1db083`，d601 为精确 `d601fa0`，C 为与本报告同一提交的局部
修订，D 为**仅供测试的无缓存同步 direct-I/O 参考**。公共 harness 新增实际 body 调用
计数；B 和 d601 使用隔离归档的生产源码及相同 harness。每个模式均完整执行全部 14 个
case 一次。上面的原始测量保持不变，没有挑选更快样本。

D 使用相同的活动 diff、direct 对齐工作区和 XTS 编码；首次 512 字节写仍构造完整 4 KiB
页。它不保留明文缓存页，写入同步完成。该参考不是可选的生产缓存模式。读取前的预填充也
使用 DIO，从而避免把有界缓存 miss 与 B 自然保留的 256 MiB warm 内核缓存混为一谈。
Setup 仍在计时之外；B 完成仍表示 buffered 接收，不表示介质持久化。测量没有调用 fsync、
DONTNEED 或 drop_caches。所有原始报告指标（含 `/proc/self/io`）保存于
[修订数据](diff-cow-cache-revision-data.json)。

运行使用任务的 `run-bms-check` helper、canonical 磁盘 TMPDIR `/var/q`，`GOFLAGS`
为空、`CGO_ENABLED=1`。Benchmark 使用 `-tags no_rocksdb`，与原始比较保持一致。
在各源码上按前文运行公共 benchmark；D 改用 `-bench '^BenchmarkCOWDirectWorkingSet$'`，
其余参数相同。参考实现位于
[cow_direct_workingset_bench_test.go](../pkg/vhost/cow_direct_workingset_bench_test.go)。

### 总耗时与吞吐

每格为**含 Drain 秒数 / 逻辑 MiB/s**。D 没有待处理队列；B 和 D 的 admission 与 total
在显示精度下相同。

| 工作负载 | XTS | B | d601 | C | D |
| --- | --- | --- | --- | --- | --- |
| `seq-write` | false | 0.132 / 1939 | 1.135 / 225.5 | 1.303 / 196.5 | 1.193 / 214.5 |
| `random-4k` | false | 0.1841 / 1390 | 29.86 / 8.574 | 29.64 / 8.637 | 29.09 / 8.801 |
| `partial-512` | false | 0.2966 / 107.9 | 29.39 / 1.089 | 29.76 / 1.075 | 29.24 / 1.094 |
| `hot-overwrite` | false | 0.06408 / 3995 | 3.908 / 65.5 | 2.994 / 85.51 | 25.82 / 9.916 |
| `seq-read` | false | 0.0814 / 3145 | 13.89 / 18.43 | 1.345 / 190.3 | 1.199 / 213.6 |
| `random-read` | false | 0.1086 / 2358 | 15.86 / 16.14 | 16.26 / 15.74 | 16.2 / 15.8 |
| `multi-disk` | false | 0.1295 / 1976 | 1.052 / 243.4 | 1.057 / 242.2 | 1.005 / 254.7 |
| `seq-write` | true | 1.196 / 214 | 1.903 / 134.5 | 2.304 / 111.1 | 2.036 / 125.7 |
| `random-4k` | true | 1.228 / 208.4 | 30.94 / 8.273 | 30.65 / 8.353 | 30.5 / 8.392 |
| `partial-512` | true | 1.383 / 23.14 | 31.04 / 1.031 | 30.83 / 1.038 | 30.41 / 1.052 |
| `hot-overwrite` | true | 1.154 / 221.9 | 4.061 / 63.04 | 3.092 / 82.78 | 26.52 / 9.653 |
| `seq-read` | true | 1.176 / 217.7 | 14.75 / 17.36 | 2.241 / 114.2 | 2.109 / 121.4 |
| `random-read` | true | 1.142 / 224.2 | 16.93 / 15.12 | 17.09 / 14.98 | 17.6 / 14.55 |
| `multi-disk` | true | 1.182 / 216.7 | 2.061 / 124.2 | 2.064 / 124.1 | 2.007 / 127.5 |

### 接收耗时与请求延迟

每格为 **admission 秒数 / p50 µs / p99 µs**。百分位针对前台请求；上表 total 包含剩余 Drain。

| 工作负载 | XTS | B | d601 | C | D |
| --- | --- | --- | --- | --- | --- |
| `seq-write` | false | 0.132 / 499.8 / 679.8 | 1.083 / 3229 / 6952 | 1.23 / 4089 / 6676 | 1.193 / 3791 / 6061 |
| `random-4k` | false | 0.1841 / 2.595 / 4.918 | 28.03 / 395.2 / 1586 | 27.79 / 392.6 / 1526 | 29.09 / 389 / 1714 |
| `partial-512` | false | 0.2966 / 3.725 / 12.64 | 27.54 / 390.4 / 1545 | 27.89 / 393.5 / 1675 | 29.24 / 392.2 / 1641 |
| `hot-overwrite` | false | 0.06408 / 0.826 / 1.747 | 2.145 / 1.103 / 441.9 | 1.575 / 0.794 / 432.2 | 25.82 / 351.2 / 1092 |
| `seq-read` | false | 0.0814 / 301.2 / 420.6 | 13.89 / 5.332e+04 / 6.724e+04 | 1.345 / 4019 / 5646 | 1.199 / 3018 / 4378 |
| `random-read` | false | 0.1086 / 1.566 / 2.264 | 15.86 / 193.7 / 576.6 | 16.26 / 197.9 / 645.8 | 16.2 / 195.6 / 645.7 |
| `multi-disk` | false | 0.1295 / 496.8 / 585.7 | 0.994 / 3792 / 6981 | 0.9919 / 3982 / 5933 | 1.005 / 3805 / 6917 |
| `seq-write` | true | 1.196 / 4645 / 4942 | 1.788 / 7246 / 1.226e+04 | 2.164 / 7982 / 1.014e+04 | 2.036 / 7716 / 1.199e+04 |
| `random-4k` | true | 1.228 / 18.43 / 22.66 | 28.94 / 409.8 / 1693 | 28.72 / 410 / 1652 | 30.5 / 406.8 / 1736 |
| `partial-512` | true | 1.383 / 20.25 / 29.16 | 29.1 / 412.1 / 1788 | 28.94 / 409.9 / 1809 | 30.41 / 406.7 / 1737 |
| `hot-overwrite` | true | 1.154 / 16.84 / 27.2 | 2.191 / 0.979 / 460.8 | 1.626 / 0.74 / 440 | 26.52 / 361.7 / 1155 |
| `seq-read` | true | 1.176 / 4473 / 5663 | 14.75 / 5.683e+04 / 6.944e+04 | 2.241 / 8724 / 1.095e+04 | 2.109 / 8379 / 1.025e+04 |
| `random-read` | true | 1.142 / 17.26 / 20.98 | 16.93 / 211 / 590 | 17.09 / 211.3 / 618.1 | 17.6 / 217 / 641.1 |
| `multi-disk` | true | 1.182 / 4604 / 4789 | 1.933 / 7956 / 1.027e+04 | 1.937 / 7979 / 1.011e+04 | 2.007 / 7755 / 1.104e+04 |

### 实际 body 调用与字节量

每格为**读调用数/读取 MiB；写调用数/写入 MiB**，不含 setup。这些是活动 body syscall，
不代表设备持久化或更底层的放大率。

| 工作负载 | XTS | B | d601 | C | D |
| --- | --- | --- | --- | --- | --- |
| `seq-write` | false | 0/0; 65536/256 | 0/0; 257/256 | 0/0; 256/256 | 0/0; 256/256 |
| `random-4k` | false | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 |
| `partial-512` | false | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 |
| `hot-overwrite` | false | 0/0; 65536/256 | 0/0; 9103/35.56 | 0/0; 6493/41.41 | 0/0; 65536/256 |
| `seq-read` | false | 65536/256; 0/0 | 65536/256; 0/0 | 256/256; 0/0 | 256/256; 0/0 |
| `random-read` | false | 65536/256; 0/0 | 65002/253.9; 0/0 | 65002/253.9; 0/0 | 65536/256; 0/0 |
| `multi-disk` | false | 0/0; 65536/256 | 0/0; 257/256 | 0/0; 256/256 | 0/0; 256/256 |
| `seq-write` | true | 0/0; 65536/256 | 0/0; 257/256 | 0/0; 256/256 | 0/0; 256/256 |
| `random-4k` | true | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 |
| `partial-512` | true | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 |
| `hot-overwrite` | true | 0/0; 65536/256 | 0/0; 9094/35.52 | 0/0; 6533/42.02 | 0/0; 65536/256 |
| `seq-read` | true | 65536/256; 0/0 | 65536/256; 0/0 | 256/256; 0/0 | 256/256; 0/0 |
| `random-read` | true | 65536/256; 0/0 | 65002/253.9; 0/0 | 65002/253.9; 0/0 | 65536/256; 0/0 |
| `multi-disk` | true | 0/0; 65536/256 | 0/0; 257/256 | 0/0; 256/256 | 0/0; 256/256 |

### 内存与分配证据

范围覆盖全部 14 个 case；峰值含预填充。缓存计数表示共享 payload 预算，不是 RSS；
进程 heap/RSS 还包含元数据、固定映射、harness buffer 和分配器保留空间。D 保留常规
 diff/owner 结构，但不接收明文缓存页。`proc` 列是文件系统记账范围，不是设备级完成；
JSON 保留每个 case。所有测量的 `cancelled_write_bytes` 差值为零。mincore 不包含
base/template、export 或 guest 内存驻留。

| 模式 | mincore MiB | 缓存 / 脏峰值 MiB | 结束脏量 MiB | HeapInuse MiB | RSS MiB | proc read / write MiB |
| --- | --- | --- | --- | --- | --- | --- |
| B | 256–256 | — | — | 3.336–5.945 | 18.65–25.33 | 0–0 / 0–256 |
| d601 | 0–0 | 32–32 / 16–16 | 0–0 | 45.67–66.08 | 71.45–94.88 | 0–256 / 0–256 |
| C | 0–0 | 32–32 / 16–16 | 0–0 | 43.9–51.88 | 65.71–80.21 | 0–256 / 0–256 |
| D | 0–0 | — | — | 4.602–8.617 | 32.29–38.02 | 0–256 / 0–256 |

聚焦的 `BenchmarkCOWCacheHot` 在两个源码版本上每条路径运行三个样本。表中使用 ns/op
中位数，三个样本的分配计数相同。无等待者的通知现在仅在等待者注册时创建广播 channel；
普通/background-context 命中避免完成回调闭包和不必要的 context 合并。可取消请求仍通过
AfterFunc 将取消与 Close 合并，保留五次分配，本次样本略慢。未牺牲取消保证以换取不可取消
路径的速度。

| 路径 | d601 ns/op; B/op; allocs/op | C ns/op; B/op; allocs/op |
| --- | --- | --- |
| `context=nil` | 216.2; 16; 1 | 199.2; 0; 0 |
| `context=background` | 724.7; 256; 5 | 203.4; 0; 0 |
| `context=cancelable` | 830.3; 256; 5 | 852.3; 248; 5 |
| `signal-without-waiters` | 100.9; 112; 1 | 15.68; 0; 0 |

### 解读与验证

256 MiB 冷顺序读现使用 256 次 body syscall，d601 为 65,536 次。C 测得明文 190.3 MiB/s、
密文 114.2 MiB/s；D 分别为 213.6 和 121.4。剩余缓存/索引/复制成本和存储波动仍可见。
C 仍显著慢于 warm buffered B。部分顺序写耗时也较本次 d601 样本变差（明文 1.303 秒，
d601 为 1.135 秒）。随机排列下同时为脏的邻页很少：d601 和 C 仍发出 65,536 次写入。
C 的随机写和部分写总耗时仍接近 D 的物理 I/O 参考，而非逼近 buffered B。热点覆盖的写
调用从 9,103 降到 6,493/6,533（明/密文），总耗时降至 2.994/3.092 秒。这些是单次样本，
不是置信区间，也不声称跨硬件性能一致。

`processChain` 仍逐个 guest descriptor segment 调用后端。1 MiB host ReadAt 可以使用
新批处理；一系列 4 KiB guest segment 仍可能分别执行冷读。本 benchmark 未测 guest
顺序吞吐；此次修订没有新增 virtqueue gather/scatter、readahead、MQ 或 io_uring 改动。

修订通过默认 tag 的 `go test -json -count=1 -timeout 180s ./...`（29 个有测试包）、
六包 `-race`、`go vet ./...` 和 `go build ./...`，均通过 helper 执行，`GOFLAGS` 为空、
`CGO_ENABLED=1`。Cache race 子集另通过十次重复。新增确定性测试覆盖手动聚合截止时间、
非紧急通知、压力/Drain 推进、积压单页不逐批等待、乱序空间合并和最老页公平性、有界
syscall 数、容量小于区间、cached/dirty/writeback/loading 混合、重叠读取、取消、
base/hole 边界、未对齐请求、短读预留清理。明/密文恶意输出修改测试确认 clean 缓存内容
在 copyout 前来自拥有所有权的 DIO 映射，从不信任可变 guest/调用者输出。Guest FLUSH、
额度、Close、fatal 和未排空快照测试继续通过。仅保留已有
`TestSendU64Reply_RoundTrip` skip；三个包没有测试文件。

前述 CH/KVM 及 native 通过适用于 d601；文末记录了不可变修订运行代码187974b 的
重复验证结果。两个实现修订均已发布至 PR #231；可信 exact-integration CI 仍是独立的
合入要求。

## 修订运行代码的最终验证

经过独立检视的运行代码及测试适配提交为 `187974bba30b636050a909a7f2ffa0cced85f5f1`（批处理提交为 `c0bbe9e2642a3285d3447bf19d8e56a567b500f5`）。默认构建模式全仓测试、与源码 CI 等价的 `CGO_ENABLED=0` 全仓测试、九个相关包的 race 检查、全仓 vet 和 build 均通过。源码测试使用磁盘支持的 `TMPDIR=/var/tmp`；源码套件及独立 usage 入口现在提供该默认值，同时保留显式覆盖。现有 guestlink 半关闭测试的两个 socket 文件名已缩短，避免超过 Unix socket 路径长度限制，测试行为和断言没有改变。没有新增测试被跳过。

基于该提交的不可变源码归档，四组真实 CH/KVM 用例再次全部通过：`diff_template`、`encrypted_diff`、`snapshot`（包括默认 100 次 CH pause/resume 屏障循环）和 `disks`（根盘、数据盘及本地工作集恢复）。测试在独立 PID、mount、network namespace 中执行，设置 `REQUIRE_KVM=1`，活动文件位于磁盘文件系统。源码归档 SHA-256 为 `1b894ed56e8831aae8d5697d2d2e7bdb4d0b21396d47b5ecbf026a51bd22d1c7`；默认构建模式 sandbox-ctl 二进制 SHA-256 为 `ea19b2b101b86107aca5159cb4b6df4c10338a118c9609fd0fa58d1842fd6bcf`。任务保留了 `kvm-187974b-results.log`、`final-ci-equivalent-tests.log` 及不可变 `187974b/evidence` 目录。这些本地检查不替代可信的 exact-integration CI。

同时用该二进制运行了未修改的真实 guest 探针：Python 使用 guest `O_DIRECT` 和对齐 mmap 缓冲，以 1 MiB 请求顺序写入、读取 64 MiB，然后在前 4 MiB 区域执行 4,096 次 4 KiB 读取。热点区域阶段包含最初的冷数据预热，并不是纯缓存命中基准。所有请求检查完整传输数量，读取检查内容字节。探针没有请求 guest fsync 或宿主 Drain，因此写入计时衡量接收速度，不代表后台回写全部完成。

| Guest 阶段 | Buffered 基线 MiB/s | d601 MiB/s | 修订 187974b MiB/s | 修订 p50 / p99 µs |
| --- | ---: | ---: | ---: | ---: |
| 1 MiB 顺序写 | 1779.77 | 308.81 | 303.02 | 3780.20 / 6834.05 |
| 1 MiB 顺序读 | 3714.38 | 18.31 | 220.36 | 3753.00 / 17637.55 |
| 4 KiB 热点区域读，包含初始预热 | 232.45 | 64.64 | 59.07 | 15.20 / 371.13 |

每个源码版本仅运行一次该探针，且宿主为共享测试环境，不能作为生产分位数或性能持平保证。未提交中间预览版的顺序读观测（208.72 MiB/s）不再用于最终提交报告，而由上表不可变提交的结果替代；没有把它悄悄挑选成额外样本。Guest descriptor 分段方式保持不变。探针确认实际 guest 顺序读获得改进，但热点区域吞吐和顺序写两行也说明并非每项指标都改善。Buffered 基线仍可使用超过配置 COW 缓存大小的宿主文件缓存；分析实际物理路径额外开销时，应另看四组后端对照中的无缓存 DIO 参考。
