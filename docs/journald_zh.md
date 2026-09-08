[English](journald.md) | [简体中文](journald_zh.md)

# 显式 journal 输出目标

每个 journal 输出独立配置：

```text
journald=<tag>[,<FIELD>=<VALUE>...]
```

`run` 的 `--stdout-to`、`--stderr-to`、`--console` 和 `--log-to` 接受这种目标。
`exec` 的既有 `--stdout-to` 和 `--stderr-to` 同样接受；guest console 属于正在运行的
沙箱，而不是 exec 客户端。

```bash
sandbox-ctl run --config sandbox.yaml \
  --stdout-to 'journald=app,WORKLOAD_ID=worker-42,STREAM=stdout' \
  --stderr-to 'journald=app,WORKLOAD_ID=worker-42,STREAM=stderr' \
  --console 'journald=console,WORKLOAD_ID=worker-42' \
  --log-to 'journald=sandbox-ctl,WORKLOAD_ID=worker-42'
```

tag 成为 `SYSLOG_IDENTIFIER`。附加字段只属于该输出：没有公共字段选项、环境变量
发现、插值或跨目标继承。相同 tag 不共享字段或半行缓冲。裸 `journald=app` 仍然合法，
但没有附加字段。原先依赖隐式身份环境变量的调用方，必须在每个需要的目标中显式提供字段。

sandboxer 不选择或解释应用、编排或身份字段。调用方选择 tag 及非秘密字段值。它们是
宿主侧输出配置，不进入 guest 环境、guest stdio 协议、sandbox YAML、可移植产物或
快照。每次新的 run 或 exec 调用都需要提供自己的目标。

## 语法和编码

tag 是非空的 `[A-Za-z0-9_-]` token。字段名为 1–64 个 ASCII 字节，以 `A`–`Z` 开头，
仅包含大写字母、数字和下划线。`_` 开头的名称保留给 journal 可信元数据。本接口还为
writer 保留 `MESSAGE`、`PRIORITY` 和 `SYSLOG_IDENTIFIER`，并拒绝重复字段名。
本接口的单值规则不意味着底层 journal 协议禁止所有重复字段。

字段由字面逗号分隔，每个字段只在第一个 `=` 处分割：`FIELD=a=b` 的值是 `a=b`，
`FIELD=` 则显式提供空值。值采用 Go `url.PathEscape`/`url.PathUnescape` 语义的
百分号编码。先分隔字段，后解码值，而且只解码一次：

```text
journald=app,NOTE=a%2Cb,EXPR=x=y,PERCENT=100%25,PLUS=a+b,ONCE=%252C
```

结果分别为 `NOTE=a,b`、`EXPR=x=y`、`PERCENT=100%`、`PLUS=a+b` 和 `ONCE=%2C`。
加号不是空格；`%20` 是空格。编码后的换行和其他字节仍是字段数据，不会成为新字段。
目标解析器不做 shell 变量或反斜杠展开。编写 shell 命令时应引用整个目标字符串；
使用 `exec.Cmd` 的调用方应将它作为一个独立参数传递。

缺少 `=`、空字段名或空段、尾随逗号、重复/保留/非法字段名、非法百分号编码和非法 tag
均是命令行错误。编码后的完整目标最多 64 KiB，附加字段最多 64 个。这些是本地资源
限制，不是对 journal 协议限制的声明。普通非 journal 文件路径不会被解码或分隔，
包括含有逗号、`=` 或 `%` 的路径。

## 组件日志与 guest 输出

`run --log-to default`（省略时也采用此默认值）保留普通组件诊断的 stderr 输出。
`run --log-to journald=...` 则显式将 Go logger 输出和 run/restore 运行诊断发送到
该目标，包括目标解析完成之后的启动准备失败和终态错误。它不要求 stderr 已连接
journal，也不要求存在任何特定身份字段。

在可用目标建立之前，非法 flag 语法和非法 `--log-to` 值仍可写到原始 stderr。
`--stderr-to` 始终是 guest 应用的 stderr 目标。`--log-to` 不接管进程描述符，不改变
TTY/pipe 选择，不重定向 Cloud Hypervisor 自身的 stderr，也不捕获 runtime panic 或
系统事件。既有消息文本和日志时间精度保持不变。`exec` 自身的诊断 stderr 及错误捕获
行为不变；文件、artifact、JSON 及其他非 journal stdout 数据不受影响。

## 分行、失败与生命周期

每个输出都有独立行缓冲。完整行以 info 等级发送；普通 CRLF 行尾会被规范化，空行会被
忽略，与既有行为一致。超长行被分成最多 60 KiB 的消息，不会先把整个输入复制进无界缓冲。
强制长度分段不会删除行中间的回车字节。Close 会刷新最后的半行。生产者必须先排空，
再关闭其 writer；Close 幂等，关闭后写入返回 `io.ErrClosedPipe`。

native 发送成功时不再复制到 stderr。发送失败时，尽力写入原始 fallback 目标。
guest/console fallback 保留 `[tag]` 前缀；组件 fallback 不在既有诊断文本外再加一层
前缀。native 发送或 fallback 写入错误均不会终止沙箱。fallback 文本不保留结构化字段。
native journal I/O 是同步的；这里不是异步队列，也不承诺非阻塞、无损或恰好一次持久化。

可以用 `journalctl WORKLOAD_ID=worker-42 -o json` 检查该命令可访问的 journal 字段。
跨节点历史需要单独的采集器或访问其他节点的 journal；本功能不增加 exporter。

协议参考：[Journal Native Protocol](https://systemd.io/JOURNAL_NATIVE_PROTOCOL/) 和
[Go net/url](https://pkg.go.dev/net/url)。
