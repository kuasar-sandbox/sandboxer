[English](artifacts.md) | [简体中文](artifacts_zh.md)

# Sandbox 与 Snapshot 制品

本页以不可变状态对象为中心，定义可运行的 Sandbox E 与内存 Snapshot S：schema、源身份、
依赖图、离线构造、发布及保留状态清理。先阅读角色与载体模型，再选择组装、离线 reset 或发布。
下文各操作分别说明根提交点和失败时可能保留的状态。

运行中 VM 的捕获与冻结/恢复协调见 [Runtime](runtime_zh.md#capture-barrier)；单盘 checkpoint
持久化和管理见[块存储](block-storage_zh.md#checkpoints)；内存页捕获与恢复见[内存子系统](memory_zh.md)。
通用 tarstream、Manifest、Bundle 字节格式由 accelerator 定义，本页定义它们的 E/S 含义。

<a id="artifact-model"></a>

## 工件模型

系统区分 4 个逻辑角色:

| Role | Suffix | Logical content | Purpose |
|---|---|---|---|
| Container image | `.image` when materialized | `EROFS + ZIP(config.json)` | 只读容器 rootfs 与 OCI image defaults |
| Disk overlay | `.overlay` | sparse block payload | 保存root/data disk可写层 |
| Sandbox E | `.sandbox` | root payload + strict ZIP | 可重复冷启动的完整 portable workload |
| Snapshot S | `.snapshot` | memory payload + strict ZIP | 恢复已运行进程和 VMM/guest memory state |

`E` 表示 runnable Sandbox artifact,`S` 表示 memory Snapshot. 两者职责不可互换:

| 输入 / 操作 | 结果 | 执行语义 |
| --- | --- | --- |
| 显式 `sandbox.yaml` + `run --config` | 在线 Runtime | 冷启动 |
| Sandbox E + `run --from` | 在线 Runtime | 使用可移植默认值冷启动 |
| Snapshot S + `run --restore` | 在线 Runtime | 恢复内存与执行状态；S 经 `sandbox_ref` 引用 E |
| 在线 export | E | 可冷启动的工作负载和磁盘状态 |
| Image-to-E 组装 | E | 不启动 VM |
| 内存 snapshot | E + S | S 是操作根 |

不存在以下模型:

- 不存在 `snapshot --memory=false`.
- 不存在 zero-size memory Snapshot.
- 不存在 `run --restore` 的 cold-boot 分支.
- `snapshot.cfg` 不保存 disk graph、launch、mounts、files 或 init.
- cold boot 不包装成 restore.

<a id="carriers"></a>

### 逻辑角色与物理 carrier

逻辑内容与物理 carrier 正交. 同一个 `.image`、`.overlay`、`.sandbox` 或 `.snapshot` logical source 可以由以下 carrier 承载:

| Carrier | Reference | Notes |
|---|---|---|
| Local tarstream | `file://<basename>@digest:<digest>` 或 `@hmac:<digest>` | sparse map 由 tarstream envelope 权威声明 |
| Manifest | `manifest://<key>` | chunk/Manifest encryption 和 verification 由 manifest config 控制 |
| Manifest Bundle | `file://<bundle>@manifest:<key>` | 一个 ZIP64 Bundle 可承载 root 及完整依赖 Manifest graph |

Local 模式物化的内容寻址文件名为 `<digest>.<role>`,其中immutable root carrier使用`.image`,`.overlay`只表示单独物化的可写disk layer. 当前root writable top已经是Sandbox E的payload,不会再复制为`.overlay`. Provisioned container image输入仍可使用`.erofs`等显式basename. `<sid>.sandbox` 和 `<sid>.snapshot` 是成功 commit 后更新的语义 symlink. Bundle 模式下语义 symlink 指向承载 root Manifest 的 `<key>.bundle`. Alias 的 SID 必须是单一安全 path component;commit 使用临时 symlink + atomic rename,并拒绝覆盖已有 regular file 或 directory. 这些是节点本地 output 语义;named ref-location 使用 [本地与命名位置的 tarstream 加密](#artifact-publication) 的独立 shared-location commit protocol,不创建 alias.

Block backend、restore 和 publisher 先打开 carrier,再按逻辑角色解析内容. 外层 ZIP magic 只说明 carrier 是 Bundle,不说明 logical role.

通用 carrier 字节格式由 [Accelerator 文件工件](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/artifacts_zh.md) 定义。本文的 Host 生命周期负责 E/S 逻辑 schema 和发布规则；[Guest ABI](guest-abi_zh.md) 负责 Host/Guest 协调。

<a id="platform-boundary"></a>

## 责任边界

`sandboxer` 提供完整 E/S 能力,但不决定 orchestrator 的 durable API. 上层的推荐映射是:

```text
memory=true  -> kind=snapshot, ref=<Snapshot S>
memory=false -> kind=sandbox,  ref=<Sandbox E>
```

普通数据面不得把 Sandbox E 当成可自动唤醒的内存状态. 从 E 启动是显式 cold `Connect` 语义.

<a id="assemble"></a>

## Image-to-Sandbox-E assembly

使用 `export --from` 在不启动 VM 的情况下组装 E：

```text
sandbox-ctl export \
  --from ./flattened.img \
  --config sandbox.yaml \
  --sandbox-id base \
  (--output /artifacts | --upload) \
  [--mode local|bundle]
```

Assembly 要求 `--config` 或 `SANDBOX_CONFIG`，拒绝 `--resume` 和 PathID；`--sandbox-id`
只提供输出 alias。此模式允许显式 `--manifest-config` 与 `--ref-location`。`--timeout`
限制自身 operation context，零关闭限制，与[在线 export](runtime_zh.md#export)仅限制客户端等待
不同。两种模式都不接受 `drop_caches` 或 memory-merge 参数，不调用 CH snapshot，不读 memfd，
不生成 memory ref。

输入必须是 flattened `EROFS + ZIP(config.json)`,不能是已包装 `.sandbox`. 流程:

```text
open carrier -> validate flattened image -> retain config.json bytes
load explicit config -> apply image defaults -> build direct-EROFS C1
rebuild EROFS + ZIP(config.json,sandbox.runtime.cfg)
emit local/Manifest/Bundle E -> commit E root
```

它不创建 VM、不需要 freeze,但仍执行 portable identity、limits、carrier admission和output atomicity检查。CLI 是 package API 的薄包装：

```text
OpenFlattenedImage
  -> PrepareSandboxEConfig
  -> AssembleSandboxE (logical sparse.Source)
  -> caller-selected direct publisher
```

`AssembleSandboxE` 不创建完整中间 `.sandbox`。它借用调用者持有的
`FlattenedImage`，保留 payload 的 Hole/Zero/Data map 和 byte-for-byte `config.json`，
只追加 canonical `sandbox.runtime.cfg`。context cancellation 可中断 open、config
ref canonicalization、image validation 和后续 sink consumption。malformed image、JSON
或 portable config 在发布 root ref 前 fail closed。嵌入式多任务进程不得通过修改全局
`MANIFEST_KEY` 切换租户；它应以 `NewProcessStorageWithCustomerKey` 传入 task-scoped
resolver。`ProcessStorage` 至多求值一次并为其拥有的 fetch、ingest 与 local codec 固定
同一个 customer key。

<a id="offline-reset"></a>

### 当前磁盘保留与离线 Reset

受管理的父对象通过 `RuntimeSpec.CurrentRun`（或 `RunOptions.CurrentRun` /
`restore.Options.CurrentRun`）显式启用当前磁盘保留，提供 `SandboxID`、唯一父对象
`InstanceID` 和精确的 `RunID`。必须启用 Checkpoint 模式并使用匹配的 SandboxID。
CLI 提供相同合同：

```bash
sandbox-ctl run --config sandbox.yaml --sandbox-id demo --enable-checkpoint \
  --retain-instance INSTANCE_ID --run-id RUN_ID --base-root /var/lib/sandbox
# 隔离并释放这一精确 runner 后：
sandbox-ctl reset --checkpoint-dir /var/lib/sandbox/demo/checkpoint \
  --sandbox-id demo --instance-id INSTANCE_ID --run-id RUN_ID \
  --operation-id RESET_OPERATION_ID --json
```

共享的冷启动/image/E 与内存恢复路径在向 Guest 暴露可写磁盘前，保存规范 portable
C0、完整根盘/具名数据盘图、DIFF 身份和容量、父对象/运行绑定、目录身份及必要的
不可变依赖。描述位于父对象的 checkpoint 目录中，脱离 RunDir 存活；每轮运行均
更新，包括复用同一组 DIFF 的情况。本地物化 lower 和 Bundle 载体保留其不可变
身份，同文件系统收集优先使用硬链接。Close 和失败清理可以释放原临时路径。
未设置 `CurrentRun` 的调用方保持原有临时数据语义。

`artifact.ProcessStorage.ResetCurrentDisks` 是唯一离线实现，CLI 只是薄适配层。
它取得目录所有权，校验精确父对象/运行及完整盘集/依赖，并打开已有且初始化完成
的 DIFF。它不会创建缺失盘、等待已退出的初始化器，或回退到模板、旧运行和用户
checkpoint。同一持久操作 ID 为每盘固定一个版本并生成不含内存的标准 E。
它不启动 VM，不消费用户 checkpoint，历史版本保持不变。调用方仅在成功后将
返回的 `sandbox_ref` 提交为 E-only 来源；SDK 不持有父对象生命周期数据库。

CLI 退出码 75 表示操作中断或结果投递丢失，必须以同一持久操作 ID 重开，不可记为
明确失败；包括已处理的 SIGTERM/SIGINT。普通校验失败返回 1，参数错误返回 2。

调用方必须持久保存操作 ID，并从接纳开始直至最终提交或明确失败，持续隔离所有
生命周期/来源写者，包括进程重启期间。目录锁覆盖每次 SDK 调用，不能替代两次
重试之间的持久生命周期隔离。`CurrentCheckpoint` 不是通用的写入代次计数器。
重复同一操作会校验其绑定描述和部分版本后续作，不能仅凭文件或版本存在判定
成功。操作记录、部分版本、E 输出及依赖始终归父对象所有，由父对象完整删除
回收。选择性 checkpoint 清理保留当前依赖和 active DIFF，同时仍校验请求保留
的历史版本。沿用现有 Guest FLUSH/capture 合同，不新增宿主断电持久性保证。

在线版本回收也必须在启动成功后保留原 ResumeSource。共享 SDK 的
`ProcessStorage.CheckpointVersions` 复用清理的完整元数据遍历，返回该来源及
`Retain` 捕获所需的精确本地 `DiffID`/版本集合。调用方在来源变更隔离下查询，
并从删除候选排除全部返回项；遍历失败不会返回部分集合。

<a id="inspect"></a>

## `sandbox-ctl info`

```text
sandbox-ctl info [--json] [--manifest-config manifest.yaml] <artifact>
```

- 对 E 输出 `sandbox.runtime.cfg`.
- 对 S 的默认输出是原始、精简的 `snapshot.cfg`,其中至少有 `sandbox_ref`.
- 对 S 的 `--json` 输出保留现有机器调用方需要的 resolved view: `Version`、`SandboxRef`、memory `FromRefs` 来自 S,`Resources`、`Boot`、`Launch` 和 `Metadata` 只从 S 引用的 E 派生. 该 view 不会写回 S,也不是第二套 snapshot provenance.
- 对 malformed、ambiguous 或既不是 E 也不是 S 的 logical root fail closed.

Local crypto、Manifest、Bundle、selector 和 named ref-location 与 run/publish 使用相同 opener.

<a id="publish"></a>

## `sandbox-ctl publish`

```bash
# Publish to Manifest store.
sandbox-ctl publish --manifest-config manifest.yaml ./s1.snapshot

# Publish to a trusted named file location.
sandbox-ctl publish \
  --manifest-config manifest.yaml \
  --to-ref-location release=file:///srv/sandbox-artifacts \
  --ref-location source=file:///srv/source \
  ./s1.sandbox
```

<a id="reference-rewriting"></a>

### 引用改写与整链归并

`publish` 接受本地路径、located tarstream ref、Bundle selector 和 `manifest://`
根，按 Sandbox E 或 Snapshot S 校验逻辑内容。`PublishSource` 也支持已经组装好的
Snapshot，并保留调用方的 source 所有权。

```text
sandbox-ctl publish [原有存储/location 参数]
  [--replace-ref OLD=NEW ...]
  [--reduce-ref A=X | A | any ...]
  [--skip-verify-ref=false|true] [--json] [--quiet]
  SOURCE
```

`upload-snapshot` 共用同一实现。一对一替换保留有序层位置。输入为 Snapshot 时，
匹配范围包括当前 E 内部的磁盘引用：发布 E 后将新 ref 写入 `sandbox_ref`，最后
发布 S。每个原始引用位置只应用一次规则。

每条替换规则独占 OLD 和 NEW 两个引用，规则间的源、目标集合必须互不相交。
重复规则、共享源或目标、首尾相接和循环替换均属于重叠。替换规则与归并规则的
作用范围也必须互不相交：归并范围包含原始 top、全部 lower 和显式目标，自动
归并同样遵守此约束。`--reduce-ref=any` 单独于替换规则使用。引用和设备均独立
的操作可以在同一次调用中执行。

规范化引用端点的重叠在参数阶段拒绝；链内部重叠依据原始 S/E 元数据判定，发生
在读取替换内容、等价性验证和输出写入之前。`--skip-verify-ref` 的两种取值采用
完全相同的规则合法性检查。

归并选择链的 top。`A=X` 验证 X 代表完整 `[A, lowers...]` 视图，再安装 X 并清空
lower 列表；`A` 自动生成该结果；`any` 自动归并当前内存及每个设备的完整显式链。
根盘的 self top 使用源 E ref 选择，生成的 payload 保留在新 E 内，配置继续使用
`self`。源 S ref 选择其内嵌 memory，原顶层执行状态随新 memory 保留。EROFS 与
可写 upper 保持独立设备，各数据盘分别处理。

执行计划先解析来源上下文、检查配置/角色/容量、确认所有显式匹配，并验证请求的
替换，随后写入依赖。默认验证优先采用可信且范围一致的 payload commitment，必要时
流式比较尺寸、Hole/Present 布局和有效字节。显式 Zero 与存储的零字节等价；Hole
保留向下透传语义。`sandbox_ref` 验证还比较非引用配置和各设备有效视图。不同加密
派生域产生的 Manifest key 可以对应相同逻辑内容。

`--skip-verify-ref` 将内容等价确认交给调用方；新输入身份、认证、schema 及适用的
容量检查继续执行。已确认的单 ref 替换或整链目标可以修复不可访问的旧引用。自动
归并实际读取源层以生成结果。未匹配的 selector 与矛盾规则在创建输出前返回错误。

```bash
sandbox-ctl publish --replace-ref "$OLD_BASE=$NEW_BASE" "$SNAPSHOT_REF"
sandbox-ctl publish --reduce-ref "$TOP_REF=$MERGED_REF" "$ROOT_REF"
sandbox-ctl publish --reduce-ref=any \
  --to-ref-location archive=file:///srv/artifacts "$SNAPSHOT_REF"
```

located 输出直接使用可复用载体身份。尚无身份的 Manifest 或组合 source 先流式
计算身份，再以固定 source/codec 重读并直接写最终内容寻址路径。payload 使用有界
工作缓冲，内存仅保留有界 tail 与格式元数据。依赖完成后发布新根；已有目标完整
校验，错误保持旧根有效，并报告 source/writer 的关闭错误。输出绑定独立于输入
Bundle 的临时解析上下文。

<a id="publication-report"></a>

### 发布结果报告

`publish --json` 与 `upload-snapshot --json` 别名在发布、验证和资源关闭全部成功后，
输出且只输出一个成功 JSON 对象。进度保留在 stderr；`--quiet` 仅抑制进度，不改变结果。
未指定 `--json` 时，stdout 继续只输出最终根引用和换行。输出写入失败仍是命令失败。

Sandbox E：

```json
{"sandboxRef":"manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","removedRefs":["file://old.sandbox@digest:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"]}
```

Snapshot S：

```json
{"snapshotRef":"manifest://bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","sandboxRef":"manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","removedRefs":["file://old.sandbox@digest:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","file://old.snapshot"]}
```

Sandbox 仅有 `sandboxRef` 和 `removedRefs`；Snapshot 额外包含 `snapshotRef`。
Snapshot 的 `sandboxRef` 是最终 S 实际引用的 E，包括复用的 E 或实际选中 Bundle 的
selector。例如，最终 located Bundle 中的 E 为
`file://<carrier>.bundle@manifest:<E-key>@location:<name>`；从远端选中的 E 仍为
`manifest://<E-key>`。不同 carrier、location 或身份域中相同的内容 key 本身不代表
同一个引用。空 `removedRefs` 必须为 `[]`，不能为 `null`。

`removedRefs` 是本次操作已有解析边界内，原拓扑减去最终保留拓扑的差集，包含退出拓扑的
旧根和已知本地/远端磁盘、内存引用。原配置字段和原生 lower 列表在修改前记录；最终采用、
复用且未写入对象的引用同样属于新拓扑，仍被其他设备或输出链使用的引用不会误报。
普通发布、替换、三种归并形式、portable 快路径、memo 复用和 Bundle exact 传输均采用
同一套单次操作记账，重复调用 publisher 不继承上次旧引用。历史 S 作为内存层、历史 E
作为磁盘层时保持既有的仅 payload 用途。`self` 属于 E，不是单独引用。未改变的不透明
分支沿用已知边界，不扫描后代。

启用 `--skip-verify-ref` 后，被替换的旧引用无论可读还是缺失，均按叶子处理；报告不会
为了发现退出引用而打开旧 E 或其后代。显式原生配置
`(base=A, base_from_refs=[B,C])` 仍已知 A/B/C 三个引用，显式 `--reduce-ref A=X`
快路径也必须计入。默认验证模式可复用等价性证明已经解析的原配置。两种模式下，报告均
不增加对象扫描、payload 读取、摘要计算或临时 payload 文件，只保存有界引用/绑定元数据。
身份、认证、schema、容量检查和替换/归并范围互斥规则不变；自动归并仍读取构造结果所需数据。

所有公开的无 location 文件引用都只有 basename，保留既有 `@digest`、`@hmac` 或
`@manifest` 身份后缀。绝对/相对目录、挂载路径和 traversal 不会输出；报告不补造
location，也不补算缺失身份。已具名 location 引用保留合法表示。先使用完整内部来源上下文
比较，再投影为 basename，最后对公开引用排序、去重。调用方在外部提供 checkpoint 目录。
JSON 不增加 path、context、identifier 或字段映射对象，成功结果绝不嵌入诊断 stderr。

报告不删除对象，也不授予删除权限。一个 Bundle selector 退出拓扑不代表整个物理 Bundle
已无人使用；其他 checkpoint、保留版本或 sandbox 仍可能引用它。源保留和垃圾回收仍由
原有职责方负责。

库保留 `PublishResult.Role`/`Ref`，增加 `SandboxRef`、`RemovedRefs`，通过
`result.Report()` 生成安全的公开 `PublishReport`。仅发布镜像的
`PublishSource(ctx, RoleImage, source)` 调用不变。对于已组装 Snapshot，调用方传入已知
的最终 E：

```go
result, err := publisher.PublishSource(ctx, artifact.RoleSnapshot, source, cfg.SandboxRef)
```

纯创建路径没有原根，差集为空。发布器使用调用方提供的 role 和最终 E，不扫描已组装 source，也不打开其中已经发布的依赖。已组装 source 的所有权仍归调用方。

```bash
sandbox-ctl publish --json --quiet --to-ref-location release=file:///srv/sandbox-artifacts ./s1.snapshot
```

<a id="portable-config"></a>

## PortableSandboxConfig

唯一 portable 文件名是:

```go
const SandboxRuntimeConfigName = "sandbox.runtime.cfg"
```

同一 canonical bytes 写入:

```text
<run_dir>/sandbox.runtime.cfg
.sandbox trailing ZIP/sandbox.runtime.cfg
```

Portable schema V1:

```yaml
version: 1
resources:
  capacity: { cpu: 2, memory: 2GiB }
  allocatable: { cpu: 1.5, memory: 1536MiB }
network:
  enabled: true
  interface: eth0
boot:
  kernel: file://vmlinux@digest:<digest>
  runtime: file://sandbox-runtime.bundle@digest:<digest>
  cmdline: "console=hvc0"
  root:
    base: file://root.erofs@digest:<digest>
    overlay:
      base: self
      base_from_refs:
        - file://parent.overlay@digest:<digest>
  disks:
    - name: data
      base: file://data.overlay@digest:<digest>
launch:
  exec: /usr/bin/app
  args: [serve]
  env: { MODE: production }
  restart: never
mounts:
  - { target: /data, type: disk, source: data }
files:
  - { path: /etc/app/config, content: "portable" }
init:
  - { exec: /bin/sh, args: ["-c", "prepare-data"] }
metadata: { workload.kind: api }
```

Portable 内容包括:

- `resources.capacity` 和 workload 默认 `resources.allocatable`;
- `network.enabled/interface` topology fact;
- kernel/runtime basename + content identity;
- boot cmdline;
- root/data immutable graph,以及 data disk name/order/mount topology;
- persistent launch/plugin/mounts/files/init/metadata.

Portable 内容排除:

- cgroup path/fd/controller、overhead/watermark/startup、`resources.diff_cow` 缓存策略;
- TAP/TapFD provider、helper、socket以及 IP/MAC/hostname;
- CH binary、run-root/base-root、active diff path;
- manifest/customer key、crypto secret、access token、ref-location host path;
- restore prefetch、host protocol timeout;
- stdio/forward endpoint;
- `ephemeral_files` 和 `launch.ephemeral_env`.

Local immutable ref 必须是 basename + identity,例如:

```text
file://vmlinux@digest:<digest>
file://sandbox-runtime.bundle@digest:<digest>
file://<digest>.image@digest:<digest>
file://<digest>.overlay@hmac:<digest>
file://<digest>.overlay@digest:<digest>
```

目标 host 通过实际 path、source directory 或 named ref-location 解析磁盘引用。启动组件策略取决于启动方式：

- 所有冷启动都使用目标 host 选定的 kernel/runtime。有效的 `<configured-kernel-path>.sha256` sidecar 提供 kernel identity；sidecar 缺失时，在 CH 启动前对原始 kernel 完整计算一次 hash。runtime Bundle 提供其声明的 footer identity。
- 冷启动 `run --from` 与 SDK 的 `RunSource.Apply` 路径使用目标身份派生新的 C0，同时保留来源 workload、磁盘图与 ancestry。源 E 不变；其旧 boot identity 不约束新冷启动。
- 内存 restore 保持原 C0，并恢复捕获的 kernel；不要求、打开或 hash 外部 kernel，也不读取 kernel sidecar。外部 runtime 必须按 **digest scheme 和 digest** 匹配 C0；序列化的 basename 只是查找提示，配置的文件可以改名。

源 E 冷启动的身份重投影要求保留 `RunSource.Apply` 返回的原始 `LaunchSpec`；
手工重建公开字段不能重建内部来源凭据。没有该凭据而手工提供的 `PortableConfig`
仍须匹配目标 kernel 的完整 canonical ref，以及 runtime 的内容身份。
源码见 [runtime_sdk.go](../pkg/sandbox/runtime_sdk.go)。

内存 restore 先检查显式 host path。预检失败（包括文件缺失、不可读、格式损坏或身份不匹配）后，至多在该配置路径的词法目录内尝试原 runtime basename，不跟随符号链接来选择目录。非法引用与取消不触发回退。全部允许的候选失败时，只有全为缺失或身份不匹配才返回 `ErrRuntimeIncompatible`；损坏元数据和 I/O 错误仍是错误，不是兼容性结论。不会额外查找 Snapshot S 所在目录、扫描其他目录或下载 runtime。既有引用编码和制品 schema 不变。runtime footer 检查验证 envelope 和声明身份，不扫描 EROFS payload；carrier metadata 与配置的内容验证保持各自检查。源码见 [source_identity.go](../pkg/sandbox/source_identity.go)、[runtime.go](../pkg/restore/runtime.go) 和 [runtime binding 策略](runtime_zh.md#restore)。

<a id="encoding-limits"></a>

## Strict encoding 与 limits

Portable config 使用 deterministic YAML marshal 和 strict known-fields parse。Artifact reader 拒绝 unknown field、unknown version、duplicate key、YAML alias/merge key、多 document、非法 ref 和 non-canonical bytes。单独 schema parser 可以接受有效但非 canonical 的 YAML；`sandboxfile.Open` 在重新 marshal 后检查字节相等。

V1 limits:

| Item | Limit |
|---|---:|
| Raw/decoded portable config | 1 MiB |
| `files` count | 256 |
| One inline file content | 256 KiB |
| Total inline file content | 1 MiB |
| Environment entries | 512 |
| Mounts | 128 |
| Init commands | 64 |
| Plugins | 64 |
| Metadata entries | 256 |
| Scalar bytes | 64 KiB |
| Refs per layer chain | 64 |
| Total artifact refs | 256 |
| Data disks | 8 |

上述限制同时生效：strict YAML 的 64 KiB scalar-byte 限制也约束 inline `content`；独立的 256 KiB 单文件上限不覆盖它。

`<run_dir>/sandbox.runtime.cfg` 用 same-directory temp、0600、file fsync、no-replace rename 和 directory fsync 写入。同一 lifecycle 只允许 write once；已有任何 final path 都会失败，即使 bytes 相同也不能重复写入。源码见 [portable.go](../pkg/config/portable.go)。

<a id="source-binding"></a>

## C0、C1 与 source binding

`C0` 是一次 run lifecycle 的 immutable portable baseline:

```text
explicit cold config + persistent fields + canonical identities
  - host-only - ephemeral = C0

E.sandbox.runtime.cfg + allowed persistent overrides
  + target kernel/runtime identities - ephemeral = new C0 for run --from

E.sandbox.runtime.cfg non-boot defaults + allowed persistent overrides
  + complete replacement boot - ephemeral = a new explicit-cold C0

S.sandbox_ref -> E.sandbox.runtime.cfg = unchanged C0 for run --restore
  (allowed restore host bindings apply only to the runtime configuration)
```

`C0` 在完整 launch 输入已知后、工作负载消费前写入 run directory，之后字节不变。基础 Runtime 不生成不完整的占位 C0。 Live runtime 另持有不序列化的 `RunSourceBinding`:

- current `self` 对应的 source E/root artifact;
- root payload 与 optional image config;
- resolved local path/source directory;
- Bundle reader/fetcher/source refs;
- named ref locations;
- host-only disk bindings和当前 active diff.

Memory restore另持有 `MemorySourceBinding`,其中 Snapshot S identity 与 `from_refs` 只管理 memory provenance.

`C1 = Export(C0, disks at T)` 只写入新 E. `export --resume` 和 `snapshot --resume` 不修改 C0、active diff、lower graph 或 memory parent. 因此同一 VM 可以产生 `E1@T1` 和 `E2@T2`,同时继续使用原 C0 + writable diffs.

<a id="disk-provenance"></a>

## `self` 与 disk provenance

普通 Portable graph 必须恰好出现一次保留值 `self`,且只能位于:

```text
boot.root.base
boot.root.overlay.base
```

Data disk 和所有 `base_from_refs` 禁止 `self`. 三种合法 root 关系:

```yaml
# Direct EROFS Sandbox E.
root:
  base: self
  overlay: {}

# E payload is the ext4 upper top.
root:
  base: file://root.erofs@digest:<digest>
  overlay:
    base: self
    base_from_refs: []

# Single-disk ext4.
root:
  base: self
  base_from_refs: []
```

`run --from` 通过 source binding 把 `self` 绑定到当前 E payload。为新 C0 选择目标 kernel/runtime 身份时保留这份磁盘 ancestry。Export C1 先把 C0 的旧 `self` 物化为原 source ref,再把新 root payload 位置设为 `self`;这避免 E digest 自引用循环.

`run --from --replace-boot` 不建立 source binding. 来源 E 在读取 non-boot defaults 后即不再参与 run；新的 root active writable layer 经普通 explicit-cold projection 占据唯一 `self`. 后续 export/snapshot 因此不会把来源 E 当作 disk parent 或 dependency.

父 `.sandbox` 出现在 disk-ref 字段时只提供 `Payload`:

- ext4 upper/lower 或 data disk:忽略其 `sandbox.runtime.cfg` 和 `config.json`.
- root EROFS base:block backend 仍只消费 Payload;host 可以读取 optional `config.json` 作为 image defaults,但绝不采用父 E 的 portable config.

当前 E 必须显式列出它真正依赖的 lower/data refs. Publisher 不因扩展名 `.sandbox` 而递归父 E 的 config graph.

Local merge 分两类:

- Disk merge只作用于 E 的 sparse disk layers.
- Memory merge只作用于 S 的 memory layers.

三态 sparse 语义保持 `Hole`、`Zero`、`Data`. Hole 只来自 authoritative metadata;不得扫描 zero bytes 发明 Hole.

<a id="sandbox-format"></a>

## `.sandbox` logical format

接受三种 layout。普通 payload 布局要求恰好一个 `self`；本地磁盘 Checkpoint 布局要求零个。

Live ext4/sparse root export:

```text
[root sparse payload]
[ZIP:
  sandbox.runtime.cfg
]
```

Top-level Sandbox E direct EROFS layout:

```text
[EROFS payload]
[ZIP:
  config.json
  sandbox.runtime.cfg
]
```

Image-to-Sandbox-E assembly定位原 `EROFS + ZIP(config.json)` 的 archive base,原样保留 EROFS prefix 和经过校验的 `config.json` bytes,然后重建一个 canonical two-entry ZIP. `ZIP(config.json) + ZIP(sandbox.runtime.cfg)` 是非法 double ZIP.

Reader 从 ZIP structure 推导 `[0, archiveBase)` payload,不信任第二个 `payload_size`. Strict ZIP contract:

- EOCD 必须位于 logical EOF且 comment 为空;
- 拒绝 ZIP64 和 multi-disk ZIP;
- exact entry set只能是 `{sandbox.runtime.cfg}`、`{config.json,sandbox.runtime.cfg}` 或 `{checkpoint.json,sandbox.runtime.cfg}`;
- duplicate、unknown、directory、path traversal entry 均拒绝;
- method固定 Store;
- fixed order/time/metadata;
- entry size有界;
- CRC、local header、central header必须一致;
- malformed/truncated input在 VM side effect 前失败.

Opened root 提供以下 view；实际类型还包含解析后的配置和 archive boundary 等字段：

```go
type Root struct {
    FullStream    fetch.Stream
    Payload       fetch.Stream
    ImageConfig   []byte
    RuntimeConfig []byte
    Checkpoint    *Checkpoint
}
```

`Payload` 是保留 sparse `RunAt`/`ReadAt` 的 section view. Shared close owner保证 FullStream/Payload/Root 任一 close 最终只释放 carrier 一次. Vhost 只能看到普通 Payload，不能看到 ZIP tail。Checkpoint root 另外提供已解析的 `Checkpoint` 元数据；通用 payload 读取会拒绝此布局，要求先显式物化版本。

Live Disk SnapshotView 是 upper-only sparse delta,ext4 superblock offset 可以是 Hole,所以不能要求每个 live payload独立通过 ext4 magic. Reader 校验 logical size、graph/self 关系和最终 composition. Direct EROFS payload则严格校验 EROFS logical size/magic.

本地磁盘 Checkpoint E 没有根盘 payload，ZIP 恰好包含 `checkpoint.json` 和
`sandbox.runtime.cfg`。前者是 canonical JSON，包含 `version: 1`、`checkpoint_id`、
`root: {diff_id}` 和 `disks: [{name,diff_id}, ...]`；数据盘按名称与 portable config
精确对应，不允许重复名称或 DiffID。捕获 ID 为非空 UTF-8、最多 128 字节且无控制字符；DiffID 必须是
安全文件名 token。不允许主机路径、第二份磁盘图、容量字段、`config.json` 或数据
payload。runtime config 只保留不可变图；reader 根据外层布局选择零 self 解析器。
DIFF 从真实本地 carrier（包括所选 Bundle 成员）所在目录解析。发布会转换为普通
单 self E。

<a id="snapshot-format"></a>

## `.snapshot` logical format

Snapshot S layout:

```text
[memory sparse payload]
[ZIP:
  config.json
  state.json
  snapshot.cfg
  memory.dirty  # optional
]
```

V1 `snapshot.cfg` 完整 schema只有:

```yaml
version: 1
sandbox_ref: file://<digest>.sandbox@digest:<digest>
from_refs:
  - file://<parent>.snapshot@digest:<digest>
```

`sandbox_ref` 指向同一 freeze point 生成的 E. `from_refs` 是 top-to-bottom memory parent chain. S 不重复 capacity、runtime、root/data graph、launch、mounts、files、init 或 metadata.

Snapshot ZIP 与 config 同样 strict、bounded、canonical。`config.json` 和 `state.json` 各限 16 MiB，`snapshot.cfg` 限 1 MiB，`from_refs` 最多 64 项。源码见 [snapshotfile.go](../pkg/snapshotfile/snapshotfile.go) 和 [snapshot/config.go](../pkg/snapshot/config.go)。旧 disk-schema input 返回 `unsupported snapshot format/version`,不 dual-read、不 migration、不 cold fallback.

<a id="memory-dirty"></a>

### 可选的 `memory.dirty` 元数据

规范 Snapshot S 包含三个条目，或在 `config.json`、`state.json`、`snapshot.cfg`
之后追加第四个条目 `memory.dirty`。`snapshot.cfg.version` 仍为 `1`，该元数据
不引入另一套 snapshot 配置 schema。

`memory.dirty` 以以下 16 字节头开始。全部整数字段采用小端；随后位图在每个字节
内部按低位优先排列。

| 字节偏移 | 宽度 | 字段 | 必须满足的值 |
| --- | --- | --- | --- |
| `0` | 4 字节，uint32 | `encoding_version` | `1` |
| `4` | 4 字节，uint32 | `page_shift` | `12`（每页 4096 字节） |
| `8` | 8 字节，uint64 | `memory_size` | 逻辑 memory payload 的精确字节数 |
| `16` | `ceil((memory_size / 4096) / 8)` 字节 | Dirty 位图 | 每个逻辑 4 KiB 页一位 |

条目存在时，`memory_size` 必须大于零、可被 4096 整除，并等于权威内存视图的大小。
条目长度必须精确为 `16 + ceil((memory_size / 4096) / 8)`，且不超过 64 MiB。
最后一个字节未使用的高位必须为零。置位页只能覆盖显式 Data/Zero；与任何 Hole
重叠均无效，包括仅占部分页的 Hole。覆盖校验扫描 sparse map，不读取 memory
payload。包含其他条目及全部 ZIP header 的完整规范 ZIP 还必须独立满足既有
64 MiB tail 预算；位图自己的上限不代表为它预留了同等空间。

条目**缺失**表示全部显式 Data/Zero 按 dirty；条目**存在且位图全零**表示没有 dirty，
两者不能互换。无论哪种情况，恢复始终读取完整层叠 Memory。这些标注用于捕获和历史
选集，不决定哪些字节组成恢复后的地址空间。新 reader 接受规范的三/四条目布局；
旧 reader 仅承诺其原有三条目图。

完整工件 reader 校验规范 ZIP 元数据与 CRC、位图编码和未用位，以及 sparse 覆盖。
`ReadConfig` 是 keep graph 选择使用的有界元数据路径：校验三/四条目布局、限额、
local/central header、`snapshot.cfg` CRC 及 dirty header 的几何，不读取位图 body、
CPU/state body 或 memory payload，不能替代完整工件验证。权威源码为
[dirty.go](../pkg/snapshotfile/dirty.go)、[snapshotfile.go](../pkg/snapshotfile/snapshotfile.go)
和 [config.go](../pkg/snapshotfile/config.go)。捕获、候选选择、重标注与预算退化由
[内存历史机制](memory_zh.md#checkpoint-changes)定义。

<a id="capture-graph"></a>

## Output graph 与 commit point

Live export graph:

```text
data/lower disk artifacts first
              |
              v
          Sandbox E last  <-- operation root / alias commit
```

Memory snapshot graph:

```text
disk dependencies -> Sandbox E -> Snapshot S
                                  ^ operation root / alias commit
```

Snapshot Bundle root Manifest是 S;Export Bundle root Manifest是 E. E、data/lower Manifest和 S memory dependencies属于同一个 planned Bundle graph. Writer emit metadata prefix前必须完成 admission、ordered source、parent copy和ref replacement plan.

<a id="capture-provenance"></a>

### Disk 与 memory provenance

```text
Disk provenance:   C0 + RunSourceBinding -> E/C1 disk graph
Memory provenance: MemorySourceBinding   -> S/from_refs
```

二者不共享schema. Re-snapshot可以独立merge disk layer或memory layer,不能借由一个旧`SnapshotConfig`同时修改两张graph.

Snapshot/export的dependency planning只物化没有portable provenance的节点本地依赖. 远端
`manifest://`保持原ref;已经located的file ref保持原ref;若logical Manifest来自located
Bundle,则改写为该Bundle的canonical located `@manifest` selector. 因此Manifest-backed
immutable root image不会在每次保存或发布快照时再生成一份`.overlay`,located parent chain
也不会被复制到新的publication directory. 未located的本地tarstream或Bundle依赖仍在
freeze前完整校验，在目标目录尚未拥有依赖时物化，避免产生依赖调用节点私有路径的portable root. 其中immutable
root carrier物化为`.image`;只有需要作为独立dependency保存的root/data writable layer物化为
`.overlay`. 当前root writable top由Sandbox E payload承载,不生成第二份`.overlay`.

<a id="checkpoint-history"></a>

### 托管 checkpoint 历史合并与选择性清理

内存选择机制由[checkpoint 内存变化](memory_zh.md#checkpoint-changes)定义；`memory.dirty` 的精确格式归[工件 schema](#memory-dirty)。

前缀必须属于当前沙箱的规范 `BaseDir/checkpoint`。先按完整来源绑定和 Bundle 实际成员选择
物理载体，再比较路径。named location 或外部模板是边界，即使文件可在本机读取、甚至映射
到同一路径也不能越界合并。该边界及其后的 lower refs 保持原顺序。local tarstream、Bundle
和混合载体使用相同规则。可写磁盘链始终吸收同设备的连续本地前缀，不受内存开关影响；
不可变 EROFS base 与 ext4 upper 保持分离。Data 和不透明 Zero 覆盖下层，Hole 向下穿透。
历史读取走宿主制品 stream，不读 guest memfd，因此不会扩大本轮工作集。

历史组合在 guest freeze 前准备并直接流入最终 sink，复用 `fetch.NewLayered` 和 sparse run，
不缓存整镜像，也不生成多余整镜像中间副本。local 输出对 checkpoint 已拥有的复用依赖保留物理
selector；Bundle 输出继续使用既有成员复制发布路径。历史合并生成新内容身份；sink commit/close 和数据库提交之前，
旧来源始终有效。捕获或数据库提交失败都不能授权删除旧文件。

托管 Pause 提交精确 S/E 对（或 E-only 根）后，生命周期 owner 先 fence 旧 runner 及全部
读写使用者，再执行选择性清理。通用 FileSink、任意 `--output`、独立 `snapshot --resume`
和共享 build 阶段输入都不因此取得清理权。sandboxer 制品库解释保留集合；conductor 只通过
短生命周期 `node-ctl checkpoint-cleanup` 工具提供目录归属、路径、已提交的来源对及既有
lifecycle fence。portable Export 继续只使用已存储的来源对，不读取制品。token、公开结果、
base 格式和持久化 cleanup schema 均不改变。

对本地磁盘 Checkpoint E，keep plan 还记录它精确引用的 DIFF 版本。当前 E 和显式保留的 E
共同贡献磁盘版本. 调用方可通过 `artifact.Checkpoint.Retain` 提供额外的不可变 E 或 S/E 对,
清理取这些来源与当前来源的文件和 DIFF 版本依赖并集. 删除前先规范化并校验全部来源.
固定 `.sandbox` 和 `.snapshot` 别名仍只跟随当前来源. 仅为内存保留历史 S 不会顺带
保留其旧磁盘 Checkpoint. 清理先取得目录
owner，验证完整保留集，再通过 metadata backend 分批删除无用版本、retire 不再
需要的 DIFF。已有 read pin 延后物理回收。此过程不打开活动 Disk，也不解密
guest 页；保留集或目录身份检查失败时不会开始修改。

保留集合包含当前及显式保留的 S/E、历史内存载体、对应磁盘/upper/不可变 base 载体及复用文件。历史 S
的旧 E 不是磁盘依赖。Bundle 任一成员仍被使用就保留整个物理载体。比较 basename 前先解析
source/location 绑定。选择只读取有界的当前小型元数据与 Bundle 索引，不读取旧候选 payload，
不计算整镜像摘要；此操作无需 Snapshot 的 CPU/state body。kernel/runtime 的
basename identity 绑定宿主提供的启动文件，不是 checkpoint payload 依赖。keep plan 或 reader Close 出错时
不删除任何文件。

Bundle 候选来源的 location 仅在既有选择器访问该来源时解析：先当前 Bundle，再按顺序
查询 refs，最后查询远端。当前 Bundle 命中时不要求无用 location 可解析；不可用候选可继续
查找，损坏候选和已选定来源的错误仍然失败。删除前仍必须确认显式当前 S/E 绑定及完整保留集合。

checkpoint 目录及其祖先路径都必须不含符号链接，解析后的物理路径应与配置路径相同。
沙箱 base 目录应配置为实际规范路径；拒绝目录内部的异常链接是另一项保护，不代表支持
通过符号链接配置 base 路径。

只处理已验证专属 checkpoint 的直接目录项：64 位小写十六进制 digest/key 加 `.snapshot`、
`.sandbox`、`.overlay`、`.image`、`.bundle` 的成品必须是普通文件；捕获 partial 必须完整匹配
`<producer-SandboxID>.<kind>.<uint32十进制>.partial`（包括 bundle，除 `0` 外不得有前导零）；
固定 `<sid>.snapshot`/`<sid>.sandbox` 别名和 `.<sid>.<role>.<32位小写hex>.tmp` 必须是符合生产端
basename target 约定的符号链接。SandboxID 不等于 PathID 或 StableID。未知名字、其他 SID、
前缀碰撞、格式近似但非法的名字、目录和异常链接一律保留。未完成 partial 无需内容校验。
固定别名只有指向对应当前 S/E root 的载体时才保留；即使 E 成员仍使用同一 Bundle，
E-only 也会删除旧 S 别名。Snapshot 捕获只提交 S 别名，E 身份取自持久 S/E 对，不要求
存在 E 别名。清理只 unlink 可识别别名本身，不跟随链接，不递归删除目录；dirfd 操作保证路径发生竞态时
unlink 仍被限制在原目录内。

新来源已提交后，即使清理失败，Pause 仍成功。最后一个持久 RunDir ownership 标记保留到
checkpoint 清理和既有 paused 收尾均成功。既有 worker 在 SID lifecycle lock 下重新读取当前
来源重试，重启后也如此。active 或 detached export 的读取 fence 在其完成前持续有效；清理
不会持锁等待需要同一锁完成的 export。Resume/Wake/Exec 遵守既有 pending-cleanup admission
合同。候选已不存在视为完成；权限、I/O 和身份错误继续保留重试责任。普通 Kill 和 BuildBaseDir
终态删除仍由原 finalizer 负责；此步骤不回收任何共享或外部制品。

<a id="artifact-publication"></a>

## 本地与命名位置的 tarstream 加密

两项策略共用同一把客户密钥，各自有独立的默认值。

```yaml
crypto:
  local: off
  ref: required
```

`crypto.local` 管理本地工作文件、活动 DIFF、checkpoint 和本地物化文件。默认 `off` 读写明文。设为 `auto` 时可以读取明文和认证通过的密文，新写入加密。设为 `required` 时还会拒绝本地明文输入。checkpoint 的内部临时物化文件在发布前始终属于本地域。

`crypto.ref` 管理实际从命名位置读取或向其写出的 tarstream。其默认值为 `auto`，与 `local` 的取值无关。原样保留已发布依赖的引用不属于读取，也不会对该父层重新应用加密策略。

| `crypto.ref` | 读取明文 | 以正确密钥读取密文 | 新 tarstream |
| --- | --- | --- | --- |
| `off` | 允许 | 拒绝 | 明文，`@digest` |
| `auto` | 允许 | 允许 | 密文，`@hmac` |
| `required` | 拒绝 | 允许 | 密文，`@hmac` |

使用上例配置，VM 可以生成本地明文 checkpoint，发布密文 E/S，用同一把密钥恢复，再写出新的本地明文 DIFF。不提供配置文件时，`sandbox-ctl publish --to-ref-location` 使用 `ref: auto`，需要有效的 `MANIFEST_KEY`。无配置调用 `info`、`run --from` 或 `run --restore` 打开该命名引用时，传入同一把密钥。密钥缺失或错误、认证失败、身份不匹配都会报错。SDK 调用方通过 `NewProcessStorageWithCustomerKey` 显式传入密钥解析器。

已有 `local: off` 的部署升级后，新发布到命名位置的 tarstream 需要密钥。要沿用升级前的发布策略，请将原 `local` 的值显式填入 `ref`。原先使用 `local: required` 且仍需在读取命名位置文件时拒绝明文的部署，应明确配置 `ref: required`。请与支持该字段的 sandboxer 一起启用。旧 loader 可能接受这份 YAML 并忽略该字段。

发布时原样保留已发布的磁盘和内存父引用，不打开或校验对应文件。普通 E/S 发布、Bundle 发布和直接 `PublishSource` 都遵循此规则；不会仅为保留引用而要求父位置映射、父文件可用或父层密钥。发布不会验证或迁移整个历史引用图。真正发生的读取仍执行配置的加密策略和预期身份校验，包括物化 checkpoint、转换源，以及为确认 checkpoint 元数据而读取当前选中 E。新目标及目标碰撞复用仍要完整校验数据流。Manifest 和 Bundle 沿用原有编码、认证与精确复制语义；来源选择和传输实际需要的 carrier 读取仍然保留。

复用发布目标位置已经存在的输出文件时，必须重新验证 role、logical size、content identity 和完整 stream。 Local output与named ref-location的commit策略刻意分离. Artifact capture/publication只定义logical completion,不定义stable-storage durability:local output依赖完整写入、`Close()`检查、内容寻址no-replace rename与alias atomic rename;named-location依赖`O_EXCL`写入、`Close()`检查、最终路径reopen/full verification与路径身份检查. 两种 artifact-publication 路径都不执行显式 file/directory flush，物理写回由文件系统或底层存储实现定义；这不同于 [Strict encoding 与 limits](#encoding-limits) 中执行 fsync 的 run-directory C0 写入。

<a id="local-output"></a>

### Local output

`snapshot/export --output` 的 `FileSink` 在output directory中写unique same-directory temp,完整写入并检查`Close`错误,再以`renameat2(RENAME_NOREPLACE)`完成O(1) final commit. `BundleSink`同样以当前atomic no-replace rename提交完整Bundle;两条本地路径都不会为了final commit再读取并复制完整artifact. Root成功后,semantic alias用随机temporary symlink + atomic rename更新. Alias target和existing entry都以`NOFOLLOW`/`lstat` fail closed,不会把regular file或directory替换成symlink;commit由完整写入、`Close()`检查与atomic rename定义,不执行显式file/directory fsync. 因此local output要求节点本地filesystem提供这些atomic rename和symlink语义.

<a id="named-output"></a>

### Named ref-location

`publish/upload-snapshot --to-ref-location`不复用`FileSink`或`BundleSink`. Tarstream carrier在自身marker中保存payload boundary和payload commitment;完整读取会用payload bytes复验该声明. 打开carrier后可直接提供identity. E/S只替换dense metadata tail时,carrier用旧payload commitment和新tail以O(tail)工作量推导新identity,不读取GiB级payload,也没有首次`io.Discard`编码. Location target取得carrier给出的scheme/digest后,以`O_CREATE|O_EXCL`直接创建`<digest>.image|overlay|sandbox|snapshot`;canonical encoding是shared target中的唯一完整write. `.image`承载immutable root image,`.overlay`只承载独立的writable disk dependency;Sandbox E payload不会重复发布为`.overlay`. Plaintext输出使用`@digest`,codec-backed输出使用`@hmac`. Target directory中没有完整temp/staging副本,也不创建`<sid>.sandbox`、`<sid>.snapshot`或任何其他semantic alias.

Fresh final固定`0644`. `tarstream.WriteTo`在唯一一次写入过程中检查source read和destination write,并重新产生与carrier预先提供值一致的scheme/digest;在 owned write fd 仍打开时以 `lstat` + `SameFile` 确认 canonical path 仍指向本次 `O_EXCL` 创建的 inode；metadata-only guard fd 在 checked `Close` 前固定该 inode。Publisher 随后以 `O_RDONLY|O_NOFOLLOW`（另含 nonblocking，避免意外 FIFO 阻塞验证）重新打开并完整验证regular file、role/payload name、logical size、canonical tarstream、marker、codec、crypto policy、digest scheme/digest和完整 sequential stream；验证后还会再次核对 path identity。Existing final 走同一完整验证；验证成功后直接复用,inode和bytes不改变.

Manifest Bundle 不进入 tarstream E/S 重建路径，以 root Manifest key 提供 `@manifest` 身份。来源选择保持当前 Bundle、按序 refs、最后 remote 的优先级。远端 Manifest 引用原样保留，不获取其 payload。选中的命名 Bundle 保留在原位置：来源选择可能读取成员索引，但 location 发布不会仅为保留该绑定而验证或复制其 payload。Location target 验证本次实际复制的所选 Manifest，包括 recorded admission、physical keys 和 crypto domain，再按顺序精确复制同目录依赖 Bundle，最后发布 root `<key>.bundle`。每个共享 final 仍只写一次；复制后重新打开，验证 canonical Bundle/root 并与 source 逐字节比较，最后通过 target fd 再次严格验证复制的所选内容。将选中的 Bundle 上传到 Manifest store 时，仍读取并验证实际传输的物理对象。该路径不创建 `.snapshot`/`.sandbox` 替身，也不改写 Bundle 内的 `snapshot.cfg`。

Final path在write完成前会短暂可见. 正常consumer只能使用publisher成功返回的root ref;publisher仍按dependencies first、root last顺序发布. Concurrent publisher遇到partial final时重新打开并做有限、context-aware exponential-backoff验证;若writer在窗口内完成则复用. bounded retry后仍不完整或invalid时fail closed并提示显式cleanup/repair,不会删除unknown owner的path. Symlink、directory、FIFO和其他non-regular final同样拒绝且不删除. Publisher只在自身`O_EXCL`成功且path仍指向所记录inode时清理自己的失败写入;abandoned unknown final由显式cleanup/GC处理.

<a id="single-root-bundle"></a>

### Single-root image/Sandbox/Snapshot Manifest Bundle

`NewSingleRootBundlePublisher` 是已经组装好的 `RoleImage`、`RoleSandbox` 或 `RoleSnapshot`
`sparse.Source` 的 typed named-location sink。数据流是：

```text
logical source
  -> Manifest ingest with fixed write admission/customer key
  -> deterministic identity pass to a streaming hash sink
  -> replay the fixed source with the same key, admission and encoding options
  -> exclusive-create content-addressed <root>.bundle
  -> reopen/strict validate
  -> file://<root>.bundle@manifest:<root>@location:<name>
```

已组装的 E/S 可以包含已经发布的命名引用。`NewSingleRootBundlePublisher` 原样保留这些引用，不需要输入位置映射，也不读取父文件，只把新的 Bundle 写入指定输出位置。`RoleSnapshot` 的 `PublishSource` 还要求传入已知的 portable `sandbox_ref`，见[发布结果报告](#publication-report)。

source 支持重复读取。第一遍保留根 Manifest key、完整 Bundle 的物理摘要及格式元数据，编码后的 Chunk 字节随处理释放。第二遍直接写入最终 `<root>.bundle`，并核对生成根与预计算身份一致。一次 publish 只解析一次客户密钥，两遍共用相同 admission 与编码参数。

唯一创建的文件是最终 Bundle，包含这个 logical root 的 Manifest 和 Chunk。发布使用有界 Chunk 工作缓冲并保留 payload 稀疏性。typed 调用点提供逻辑角色；image、Sandbox 和 Snapshot reader 保持各自 schema 校验。复用已有目标时，验证 admission、根、认证内容与完整物理身份。并发 writer 通过有界等待、exclusive create 与完整校验收敛。取消、源变化、写入及关闭错误使发布失败，并仅清理本次拥有的不完整 final。路径被替换时保留替换者的 inode，返回所有权错误。

Tarstream 与 Bundle 都是 physical carrier，不是 logical role。Tarstream 包含单一
role-specific payload 与 sparse envelope，使用 `@digest`/`@hmac` identity；Bundle
包含 Manifest/chunk records、write admission与可选local encryption，使用
`@manifest` root identity。single-root Bundle publication不得先建立 tarstream，反之也
不得仅按 `.image`/`.sandbox` 扩展名推导 Bundle root。

因此named location只依赖`mkdir`、exclusive create、write、read、stat/fstat/lstat、权限设置、seek/pread、close，以及删除本进程拥有的不完整file. 它不依赖rename/renameat2、symlink、hardlink、reflink、sparse-file preservation、advisory lock或lock file,也不执行显式file/directory sync;publication的success contract由`O_EXCL`写入、`Close()`检查、最终路径reopen/full verification与路径身份检查定义,物理写回由文件系统或底层存储实现定义.

Active encrypted `.overlay.diff` 保持KDXTS格式. Export只读取decrypt后的Disk SnapshotView并创建新的immutable logical artifact,绝不把ZIP追加到active diff.

<a id="manifest-upload"></a>

## Manifest upload

已经组装好的 image 或顶层 Sandbox E 可通过
`NewManifestPublisher(...).PublishSource(ctx, RoleImage|RoleSandbox, source)`
直接 ingest。调用者保留 source ownership；该路径不生成 local tarstream，Chunks
与去重对象先写，root Manifest最后写入并返回`manifest://<root>`。

Local tarstream graph按照bottom-up顺序ingest:

```text
data/lower -> E -> S
```

已经portable的Manifest或located dependency保持原ref,不会先materialize再重写. Bundle root走exact-upload快路径:强制验证选择的Manifest closure、recorded admission和physical objects,原样上传Chunk/Manifest,root Manifest最后提交;root key和`snapshot.cfg`不变. 因此Bundle内已有的located selector仍要求consumer配置对应ref-location,不会被暗中改写成Manifest ref. Customer key、chunk/Manifest crypto、content verification和store generation admission沿用manifest config. E是export root,S是snapshot root.

<a id="manifest-bundle"></a>

## Manifest Bundle

Bundle在pause前完成:

- write admission;
- portable dependency retention和located Bundle selector rewrite;
- unlocated local dependency materialization plan;
- current operation依赖集合;
- unlocated parent Bundle精确Manifest copy或remote fallback;located parent保留canonical selector;
- local tarstream dependency ingest;
- all ref replacements.

然后writer一次性emit metadata prefix,写入Manifest/Chunk,最后以E或S root key finalize. `FullVerify`使用完整expected Manifest集合. V1不以外层ZIP magic推断E/S.

<a id="publication-graph"></a>

## Publish graph

Local tarstream Publish E:

```text
parse E -> enumerate E explicit disk refs bottom-up
        -> publish node-local payload dependencies -> rewrite those refs
        -> rebuild E -> publish E last
```

`self` 永不重写. 父 `.sandbox` 作为writable disk layer时发布为`.overlay` payload;作为two-disk root image carrier时提取EROFS payload与image config并发布为`.image`. 两种情况都不递归父config graph.

Local tarstream Publish S:

```text
publish memory from_refs as opaque memory layers
publish node-local S.sandbox_ref E graph
rewrite node-local sandbox_ref
rebuild S -> publish S last
```

当前 S 定义有序内存列表，并选择当前 E 磁盘图。内存父层贡献自身 memory payload。普通发布可以保留 portable 依赖并采用 Bundle exact-copy/upload。请求引用改写时，根据选定目标重建逻辑 S/E，Bundle 范围内的依赖获得有效的输出绑定。

<a id="atomicity"></a>

## Atomicity 与 determinism

Portable YAML和E/S ZIP使用canonical order、fixed metadata和bounded bytes. Local `FileSink`/`BundleSink`保持same-directory temp、完整写入/`Close()`检查和atomic no-replace rename,final commit是O(1);alias只在root commit后更新. artifact capture/publication只定义logical completion,不定义stable-storage durability;两条本地路径都不执行显式file/directory fsync,物理写回由文件系统或底层存储实现定义. Named ref-location采用独立的exclusive-create + checked-write/copy-once + reopen-full-verify协议;tarstream由carrier直接提供identity,Bundle保持exact bytes,两者都不进入local sink的capture/commit路径.

多盘顺序固定为data disks first、root E last. Snapshot随后写memory S last. 这让S/E root成为可审计的graph commit point.

<a id="streaming"></a>

## Streaming 与 memory use

- Sparse tarstream不spool logical stream到disk.
- Manifest ingest只读取resident extents.
- E/S 大 payload 不暂存到 per-run `/run` directory；CH 有界 config/state staging files 放在该处。调用者选择的 output directory 属于独立路径，并占用所选文件系统容量。named-location Bundle publication 通过流式预计算身份，仅写入最终内容寻址文件。
- Bundle dependency plan在pause前执行remote I/O和admission.
- V1 `--resume` 可以在sink写完整期间保持VM paused,换取SnapshotView稳定性.

<a id="compatibility"></a>

## 制品兼容边界

本切换直接替换旧snapshot provenance. 旧`SnapshotConfig`若包含resource/runtime/root/data/launch字段会返回明确unsupported error. 不提供 old-format alias、dual reader、migration shim、feature flag 或 zero-memory compatibility path。[子命令总览](runtime_zh.md#operations) 的 `upload-snapshot` CLI alias 不提供格式兼容。

E2B mapping只发生在orchestrator层:

```text
E2B memory=false
  -> sandbox-ctl export
  -> Sandbox resume source
  -> explicit sandbox-ctl run --from

E2B memory=true
  -> sandbox-ctl snapshot
  -> Snapshot resume source
  -> sandbox-ctl run --restore
```

S/E 后缀几何、确定性 ZIP 编码及 sparse prefix/append 视图由 `accelerator/pkg/tailzip` 实现。Sandboxer 提供有序角色条目与大小上限；Sandbox/Snapshot 模块保留 portable config、JSON/state 和设备拓扑校验。逻辑引用位置共用 `manifest.RefLocations`。

<a id="related"></a>

## 相关文档

[Runtime：启动、控制、捕获与恢复](runtime_zh.md) | [Sandbox 配置参考](configuration_zh.md) | [Guest 内存与按需恢复](memory_zh.md) | [块存储：设备、可写状态与 Checkpoint](block-storage_zh.md) | [资源控制与 Usage 计量](resources_zh.md) | [Guest ABI：启动、应用与 Host 协议](guest-abi_zh.md) | [VMM 集成与 Cloud Hypervisor 补丁](vmm_zh.md)

[组件入口](../README_zh.md) · [构建与补丁材料](../native-deps/README_zh.md)
