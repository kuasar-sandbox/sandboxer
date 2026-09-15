[English](sandboxer-read-recovery.md) | [简体中文](sandboxer-read-recovery_zh.md)

# 同步源读取恢复

## 1. 概述

`sandboxer` 在 `fetch.Stream` 和 `sparse.Run` 之上唯一负责同步重试. 文件、Manifest Bundle、cache 和 Store 来源使用同一策略. `accelerator` 每次只执行一次业务读取并保留错误链; 后续调用可以从同一个已选来源恢复.

必需运行读取等待时保留原请求、buffer 和 inflight 所有权. 暂时失败不会完成 Guest 磁盘请求, 也不会填入缺失内存页. 必需读取不可恢复时, VM 所有者终止整个 CH 进程和沙箱, 并保留首个读取原因.

## 2. CLI

该行为覆盖冷启动, 包括 `run --from manifest://...`, 以及内存恢复和随后的磁盘、内存读取. 只读制品打开和元数据检查也使用同一 helper. 非法命令参数向调用者返回错误. 管理输入错误或可选预取失败不会通知运行 fatal.

不增加命令、重试次数参数或恢复模式. 操作由现有 context 或沙箱关闭结束. 来源停机可能延迟就绪、exec 或快照排空, 因为这些操作可能需要尚不可用的数据.

## 3. 配置

退避为内部固定策略: 从 10 ms 开始, 每次失败后翻倍, 上限 1 s; 每次实际等待均匀取该档位的 50% 到 100%. 等待观察操作 context. 不设置次数或累计时间耗尽后将挂起读取转为 Guest IOErr 的机制.

后端 socket/RPC deadline 仍只约束单次尝试. 后端内部取消或超时不会结束仍存活的操作. 已配置的健康策略保持独立; 读取恢复不屏蔽健康检查、不重启服务、不改选 endpoint. 每个 `ProcessStorage` 的 customer key 保持固定, 包括首次密钥解析错误.

## 4. 读取与所有权

`internal/readretry` 只返回终态原因, 不杀进程. 未标注访问错误保守重试. `accelerator/pkg/readerr` 提供最小的 `Retryable() bool` 标记和 `Unwrap`. 明确永久原因包括已验证的几何错误、完整帧结构错误、必需查找确认的不可变对象缺失, 以及已确认的内容、格式和认证失败. 自定义 decryptor 和传输失败保留实际原因, 不仅按操作名称分类.

`StreamReader` 同步重试完整读取. 运行队列 context 仅在本次调用替代准备阶段 context, 因此旧 master session 停止时先取消挂起读取, 再 join worker 并替换 memory table. COW 适配层将该 context 传入 base materialization, 同时保留块锁、dirty bitmap 和部分写算法. 不重放整个 `WriteAt`、`FLUSH`、Store `Put` 或快照操作.

UFFD 直接重试必需的 `Run.ReadAt`. urgent 页及包含它的 Chunk window 属于必需读取. 纯 speculative tail 只执行一次最佳努力尝试. 保留现有 `ChunkRun` 能力、buffer、tail 预留和 ioctl 部分进展/EEXIST/EAGAIN 收敛. 延迟读取后, urgent 安装重新检查页状态; 已观察到的 REMOVE 使旧数据失效, 此时使用现有零页收敛, 并且不再从过期计划安排 tail.

每次尝试可以改写同一个 buffer, 但后续尝试重新读取整个请求范围, 不拼接部分响应. 并行后端读取在返回前 join 全部 worker; 派生取消不能遮蔽原始原因或随后发现的永久原因. 合法完整读取附带普通 EOF 时保留原合同. EOF 或 EAGAIN 的终态包装先于兼容和补零分支检查.

进程惰性 Fetcher 初始化、引用 Bundle 解析和 Bundle Chunk 索引准备只缓存成功结果. 首次失败或取消后, 后续调用仍可尝试. 所有者关闭后, 初始化或资源发布不能使对象复活. Manifest 来源选定后, 数据读取失败仍局限于该来源.

## 5. 可靠性与捕获

`processChain` 和 `processQueue` 都识别停止或终态的必需读取. 此时 status byte、used ring 和队列 base 保持未完成. COW 写入内部的基底读取同样遵守该规则. 普通不支持请求及独立的可写 diff 错误保留现有协议行为.

worker 在释放 inflight 所有权前报告必需读取 fatal; 后发生的 queue stop 不能遮蔽后端已经返回的永久原因. `ServeAndWait` 记录首因, 取消相关等待和 PostSpawn/握手工作, 然后直接杀死 CH. 现有唯一 `cmd.Wait` 负责回收和输出排空. worker 不同步等待自己的清理. Ready 提交和 fatal 登记共用一把锁, 已登记 fatal 会阻止新的 Ready 状态转换. 已提交 Ready 事件的通知在锁外交付; 通知回调阻塞不会延迟 fatal 取消或 CH 终止.

快照和 export 保留冻结、排空和一致性条件. 重试可以延长排空, 不减少 inflight 来通过冻结. 捕获在提交前和成功返回前检查操作是否结束; 无法满足捕获条件时失败. 队列停止先取消读取及快照 gate 等待, 再 join; 旧 worker 不能向替换后的 master memory table 写入. UFFD 队列投递也观察取消. 关闭仍先结束 reader, 再让 remove flusher 最终 drain, 最后回收资源.

回归覆盖 ring/completion 不变量、COW materialization、EOF/EAGAIN 包装、后端取消、仅成功初始化缓存、队列/冻结关闭, 以及真实内核 REMOVE/COPY 的页内容. owner E2E 进一步使用匹配的组件二进制、runtime image 和 native 依赖验证 CH/KVM 行为. 前置条件缺失代表缺少验证, 不代表测试通过.

## 6. 性能

健康路径的 retry helper 不分配 timer, 不启动 goroutine. 失败的同步读取持有一个复用 timer 及原有请求/buffer. 单次退避有上限, 但操作可以无限期保持挂起. 连接额度包含空闲、借出和拨号中的连接; 有界维护 worker 不随停机时长增长. 本变更配套健康路径分配/时延对比和故障期资源检查, 不扩展为独立性能认证.

## 7. See Also

- [Sandbox 生命周期](sandbox_zh.md)
- [制品合同](sandbox-artifacts_zh.md)
- [Accelerator 读取错误与恢复](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/accelerator-read-recovery_zh.md)
- [提案与验收清单](https://github.com/kuasar-sandbox/sandboxer/issues/225)
