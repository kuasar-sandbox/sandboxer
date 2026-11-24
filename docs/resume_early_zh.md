# 运行中快照：提前恢复

`snapshot --resume-early` 在固定快照输入后恢复沙箱运行，随后完成持久化。调用在保存完成后返回成功。

## 用法

```sh
# 保存到本地
sandbox-ctl snapshot --resume-early --sandbox-id parent --output /path/to/snapshots --json

# 保存到 Manifest/chunk Store
sandbox-ctl snapshot --resume-early --sandbox-id parent --upload --json
```

`--output` 与 `--upload` 互斥。本地输出支持 `--mode local|bundle`；remote 使用沙箱启动时 `--manifest-config` 指定的存储、分块和加密配置。

Go SDK 调用 `runtime.Snapshot(ctx, ctl.Request{OutDir: directory, ResumeEarly: true})`；remote 使用 `Upload: true`。控制协议使用 `snapshot_request` 的 `resume_early` 字段。

## 行为与限制

`--resume-early` 隐含恢复运行，不能与 `--checkpoint` 或宿主退役请求一起使用。普通 `snapshot` 和 `snapshot --resume` 的行为不变。

执行过程如下：

1. 暂停应用、VMM 和磁盘后端，在同一冻结边界捕获状态。
2. 将需要捕获的 RAM 数据复制到不可修改的临时 memfd；版本化磁盘通过临时 checkpoint 和 CoW 固定状态，未版本化磁盘复制可写上层数据。
3. 恢复后端、VMM 和应用，从独立输入完成快照保存，避免后续写入改变快照内容。
4. 提交快照并清理临时资源，返回快照引用。

暂停耗时和临时内存占用随捕获数据量增长；保存期间继续写入可能增加磁盘 CoW 占用。同一沙箱的其他捕获操作仍受互斥保护。快照格式、存储路径和持久化保证沿用现有实现。

## 失败与清理

保存失败或取消返回错误。应用恢复失败沿用现有的终止保护，避免留下无法恢复的沙箱。

临时磁盘 checkpoint 的标记与 checkpoint 在同一事务中提交。正常结束时关闭读取视图并删除临时 checkpoint；若进程退出或清理失败，重新打开磁盘时回收这些临时版本。普通 checkpoint 和仍被读取视图引用的数据不受影响。

快照已成功提交，但临时资源清理失败时，返回有效快照引用及 `cleanup_warning`，CLI 同时在 stderr 打印警告。

## 计时

JSON 输出中的计时单位为毫秒：

- `application_pause_ms`：从发出 guest quiesce 前，到应用解冻完成，包含控制通信时间。
- `save_complete_ms`：服务端从进入捕获流程，到保存、提交和必要收尾完成，包含暂停时间。

客户端请求到收到结果的耗时还包含命令进程启动与 RPC 开销。

## 测试

`scripts/test-resume-early-guest.py` 需要 Linux/KVM、相应权限，以及可启动的 Python 镜像、内核和 guest runtime：

```sh
sudo python3 scripts/test-resume-early-guest.py \
  --bin /path/to/bin \
  --config /path/to/python-cold.json \
  --output /path/to/new-test-results
```

`--bin` 包含 `sandbox-ctl`、`cloud-hypervisor` 和自动启动测试 Store 所需的 `store-ctl`。`--config` 是包含资源、内核、runtime、镜像和可写磁盘设置的冷启动 JSON 配置；测试会替换工作负载。`--output` 必须尚不存在，保存日志、快照、结果和清理状态。

默认在 local、remote 下执行共 14 项检查，覆盖：

- 保存期间原沙箱继续运行，恢复实例的内存和文件状态正确，独立写入及两种销毁顺序互不影响。
- 保存期间覆写并同步 32 MiB 文件，恢复实例仍读取捕获时的内容；写入未与保存重叠则测试失败。
- 恢复后的实例再次创建快照，包括跨 local/remote 恢复。

未传 `--manifest-config` 时，脚本启动独立的本机 fs Store，使用 CDC 分块和 AES 加密；这不代表远端对象存储的性能。脚本只清理自己启动的实例和 Store，保留测试数据。测试不覆盖宿主断电恢复。

性能测试使用 `scripts/bench-resume-early.py`，每种存储方式默认 1 次预热加 50 次正式测量。并发写入集成测试的耗时不与轻负载性能测试混用。脚本参数见各自的 `--help`。

故障回归测试位于 `pkg/snapshot/resume_early_cleanup_test.go` 和 `pkg/diffstore/internal/versioned/temporary_checkpoint_test.go`，覆盖保存失败、取消、清理失败及进程退出后的临时版本回收；CLI 参数与结果校验见 `cmd/sandbox-ctl/resume_early_test.go`。
