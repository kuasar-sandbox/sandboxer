[English](diff-cow-cache-validation.md) | [简体中文](diff-cow-cache-validation_zh.md)

# Issue #230 验证观测

[缓存/direct I/O 契约](diff-cow-cache_zh.md) 为活动 diff 内存设定上限，同时改变 I/O 完成边界
和性能。以下测量显示文件 page-cache 驻留降低，但在本存储上，相比 buffered 基线存在显著
吞吐回退。这些是观测，不是性能目标，也不代表 KVM E2E 通过。

## 环境与方法

测量日期为 2026-09-17，Linux/amd64，Intel Xeon Gold 6266C（可见 88 个逻辑 CPU），
Go 1.26.5，kernel `6.6.0-159.4.6.157.20260713.a4e2472763b2.oe2403sp4.x86_64`。
源码隔离于 `/var/tmp/diff-cow-230-lb6t8B`；存储为 `/dev/sdb2` 上的 ext4，文件系统块为
4 KiB。`/tmp` 是 tmpfs。Go 测试使用任务持有、短且 canonical 的磁盘目录 `/var/q` 作为
TMPDIR，因为部分已有 Unix-socket fixture 在长 TMPDIR 下超出路径限制，另一些已有测试
比较 canonical 路径。没有为此弱化任何测试。

B 为精确 main `b1db083dee2cacac141d4033bbf41b518cf80c9e`，仅加入相同 benchmark harness
和基线 adapter。C 为本 issue 实现，使用默认共享总量 32 MiB / 脏子集 16 MiB。
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

## 耗时、吞吐与请求延迟

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

## 流量与内存证据

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
Discard、Close 和粘性错误。真实磁盘测试覆盖对齐、mmap buffer、拒绝 tmpfs、seeding、
明/密文重开及 mincore。制品 snapshot/export/readback 测试从未排空 dirty/writeback 数据
恢复，并在最后一次磁盘读取后发生故障时拒绝发布。这些测试模拟 CH API，并非真实 CH/KVM E2E。

Broader suite 保留一个已有 skip：`TestSendU64Reply_RoundTrip` 使用 net.Pipe 而非 UnixConn，
并注明由 `TestParseSetMemTable_Layout` 间接覆盖。另三个包没有测试文件。没有跳过新增测试。
本磁盘环境实际验证的是 ext4，而非真实 XFS 挂载；旧模式拒绝有确定性单测覆盖。
RocksDB/native 变体与 trusted exact-integration CI 仍是独立检查。

以 `REQUIRE_KVM=1` 尝试 `diff_template`、`encrypted_diff`、`snapshot`、`disks`，均因缺少
任务内 `bin/cloud-hypervisor` 前置依赖而退出 **1**。`/dev/kvm` 存在，但任务没有组装好的
CH/runtime/kernel BIN，BMS 命令 PATH 中也没有 cargo/rustc。没有借用其他运行中 worktree 的
内容，也没有全局安装。这是验证阻碍，不是四个通过或静默跳过的 E2E。准备好满足仓库 patched
VMM/runtime 要求的可信 BIN 后，执行：

```sh
export REQUIRE_KVM=1 TMPDIR=/absolute/disk-backed/task/tmp
export BIN=/absolute/task/assembled/bin
for test in diff_template encrypted_diff snapshot disks; do
  bash "test/e2e/e2e_sandbox_${test}.sh" || exit "$?"
done
```

发布/合并前仍须独立 review 及正常 exact-integration CI；这些本地观测不能替代它们。
