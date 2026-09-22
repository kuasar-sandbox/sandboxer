[English](sandbox.md) | [简体中文](sandbox_zh.md)

# sandbox — 沙箱控制与制品生命周期

`sandbox-ctl` 是 kuasar-sandbox 的单沙箱 host 控制面. 它负责显式冷启动、从 Sandbox 制品冷启动、内存恢复、live export、无 VM 的 image-to-Sandbox-E assembly、内存快照、制品发布以及运行期 `exec`/forward. Guest 侧协议见 [sandbox-init_zh.md](sandbox-init_zh.md)。

本文只描述当前格式和行为。当前 reader 拒绝旧 `snapshot.cfg` 磁盘图 schema，不提供双读、自动迁移或跨版本兼容保证；这一格式边界不表示项目从未发布版本。

## 1. 概述

<a id="artifact-model"></a>
<a id="工件模型"></a>
### 1.1 工件模型

系统区分 4 个逻辑角色:

| Role | Suffix | Logical content | Purpose |
|---|---|---|---|
| Container image | `.image` when materialized | `EROFS + ZIP(config.json)` | 只读容器 rootfs 与 OCI image defaults |
| Disk overlay | `.overlay` | sparse block payload | 保存root/data disk可写层 |
| Sandbox E | `.sandbox` | root payload + strict ZIP | 可重复冷启动的完整 portable workload |
| Snapshot S | `.snapshot` | memory payload + strict ZIP | 恢复已运行进程和 VMM/guest memory state |

`E` 表示 runnable Sandbox artifact,`S` 表示 memory Snapshot. 两者职责不可互换:

```text
explicit sandbox.yaml ── run --config ──> cold start
Sandbox E             ── run --from   ──> cold start
Snapshot S ── sandbox_ref ──> E
           └──────── run --restore ────> memory restore

live export              ──> E
image-to-Sandbox-E build ──> E
memory snapshot          ──> E + S, S is operation root
```

不存在以下模型:

- 不存在 `snapshot --memory=false`.
- 不存在 zero-size memory Snapshot.
- 不存在 `run --restore` 的 cold-boot 分支.
- `snapshot.cfg` 不保存 disk graph、launch、mounts、files 或 init.
- cold boot 不包装成 restore.

#### 1.1.1 逻辑角色与物理 carrier

逻辑内容与物理 carrier 正交. 同一个 `.image`、`.overlay`、`.sandbox` 或 `.snapshot` logical source 可以由以下 carrier 承载:

| Carrier | Reference | Notes |
|---|---|---|
| Local tarstream | `file://<basename>@digest:<digest>` 或 `@hmac:<digest>` | sparse map 由 tarstream envelope 权威声明 |
| Manifest | `manifest://<key>` | chunk/Manifest encryption 和 verification 由 manifest config 控制 |
| Manifest Bundle | `file://<bundle>@manifest:<key>` | 一个 ZIP64 Bundle 可承载 root 及完整依赖 Manifest graph |

Local 模式物化的内容寻址文件名为 `<digest>.<role>`,其中immutable root carrier使用`.image`,`.overlay`只表示单独物化的可写disk layer. 当前root writable top已经是Sandbox E的payload,不会再复制为`.overlay`. Provisioned container image输入仍可使用`.erofs`等显式basename. `<sid>.sandbox` 和 `<sid>.snapshot` 是成功 commit 后更新的语义 symlink. Bundle 模式下语义 symlink 指向承载 root Manifest 的 `<key>.bundle`. Alias 的 SID 必须是单一安全 path component;commit 使用临时 symlink + atomic rename,并拒绝覆盖已有 regular file 或 directory. 这些是节点本地 output 语义;named ref-location 使用 §6.5 的独立 shared-location commit protocol,不创建 alias.

Block backend、restore 和 publisher 先打开 carrier,再按逻辑角色解析内容. 外层 ZIP magic 只说明 carrier 是 Bundle,不说明 logical role.



通用 carrier 字节格式由 [Accelerator 文件工件](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/file-artifacts_zh.md) 定义。本文的 Host 生命周期负责 E/S 逻辑 schema 和发布规则；[Guest ABI](sandbox-init_zh.md) 负责 Host/Guest 协调。

### 1.2 责任边界

`sandboxer` 提供完整 E/S 能力,但不决定 orchestrator 的 durable API. 上层的推荐映射是:

```text
memory=true  -> kind=snapshot, ref=<Snapshot S>
memory=false -> kind=sandbox,  ref=<Sandbox E>
```

普通数据面不得把 Sandbox E 当成可自动唤醒的内存状态. 从 E 启动是显式 cold `Connect` 语义.

## 2. 命令行接口

### 2.1 子命令总览

```text
sandbox-ctl run
sandbox-ctl export
sandbox-ctl snapshot
sandbox-ctl exec
sandbox-ctl usage
sandbox-ctl config
sandbox-ctl info
sandbox-ctl publish
sandbox-ctl upload-snapshot
```

`upload-snapshot` 是 `publish` 的 thin compatibility alias,两者调用同一 publisher 和 graph traversal.

### 2.2 `sandbox-ctl run`

三种输入模式严格分离:

```bash
# Explicit cold start.
sandbox-ctl run --config sandbox.yaml --sandbox-id s1

# Cold start from Sandbox E.
sandbox-ctl run --from ./s1.sandbox --config host.yaml --sandbox-id s2

# Cold derivation from Sandbox E defaults with a completely new boot graph.
sandbox-ctl run --from ./s1.sandbox --replace-boot \
  --config replacement.yaml --sandbox-id s2-rebased

# Resume memory execution state from Snapshot S.
sandbox-ctl run --restore ./s1.snapshot --config restore-host.yaml --sandbox-id s3
```

RunRoot/BaseRoot 是调用级根目录，RunDir/BaseDir 是本次 Sandbox 的实际目录。
`SandboxID` 始终是逻辑身份；`PathID` 仅是两个 root 下共同使用的目录 leaf：

```text
pathID  = PathID（若显式给出），否则为 SandboxID
RunDir  = RunRoot/pathID
BaseDir = BaseRoot/pathID
```

例如 Build phase 可保留全局唯一 SandboxID，同时使用固定私有目录：

```bash
sandbox-ctl run \
  --config phase-a.yaml \
  --sandbox-id build-01-phase-a \
  --path-id a \
  --run-root /run/sandbox/builds/build-01 \
  --base-root /var/lib/sandbox/builds/build-01
```

PathID 必须是非空、安全的单一路径分量，不能是 `.`、`..`，也不能包含
`/`、`\\` 或 NUL。它不与 SandboxID 比较，也没有 PathID→SandboxID 映射。
root/data disk 的默认 writable diff 位于 PathID 派生的 BaseDir，但文件名继续
使用逻辑 SandboxID，例如
`BaseRoot/a/build-01-phase-a.overlay.diff` 与
`BaseRoot/a/build-01-phase-a.disk0.diff`。cold、`--from` 和 `--restore` 使用同一
规则。Sandbox 正常退出只清理当前 PathID 的 RunDir 和 owned diff；不会递归
删除传入的 RunRoot/BaseRoot 或 sibling PathID。

`--from` 与 `--restore` 互斥. `--config` 在三种模式中的所有权不同:

- 普通 run:完整显式 cold config.
- `--from`:host/instance overlay;portable workload 来自 E.
- `--from --replace-boot`:E 只提供 non-boot portable defaults;host config 必须提供完整 boot.
- `--restore`:host-only restore bindings/policy;执行状态和 portable workload 来自 S 引用的 E.

`--config a.yaml:b.yaml` 继续按顺序 merge. `SANDBOX_CONFIG` 可替代 `--config`. `LoadConfigBytesWithPresence` 为 config-socket 等内存入口提供相同的 presence-aware 规则.

`--from` 和 `--restore` 支持:

- raw absolute/relative local path;
- scheme-qualified local tarstream;
- `manifest://<key>`;
- `file://<bundle>@manifest:<key>`;
- Bundle 默认 root key 推导;
- named `--ref-location name=file:///absolute/path`.

`run` 的 stdio、TTY、console、`--ready-fd`、connect forward 和资源参数在三种模式中保持相同 host 控制语义. Memory restore 不重新发送 cold launch spec.

### 2.3 `sandbox-ctl snapshot`

以下为命令语法；实际执行时选择一种 output 方式，并省略方括号记法：

```text
sandbox-ctl snapshot \
  --path-id a \
  --run-root /run/sandbox/builds/build-01 \
  (--output /artifacts | --upload) \
  [--mode local|bundle] \
  [--resume] \
  [--drop-caches=false] \
  [--merge-ref=true]
```

Snapshot 始终包含 memory execution state. 一次操作在同一 freeze point 产生 E 和 S,S 最后 commit. 默认成功后销毁 VM;`--resume` 恢复原 VM,但不把新 E/S 设为 live baseline.

Local human output同时列出 `Snapshot S` 与 `Sandbox E`. Upload stdout 仍只输出 S Manifest key,便于当前 orchestrator parser 使用. `snapshot_done` 同时返回 `snapshot_ref` 与 `sandbox_ref`;既有 `memory_size`、`memory_resident`、pause/dump timing 和 compatibility `overlay_*` response 字段保留. `overlay_*` 当前镜像 E identity,真正 disk graph 只在 E 中.

Capture 的 `snapshot --json` 恰好返回 `{snapshotRef,sandboxRef,removedRefs}`，
`export --json` 恰好返回 `{sandboxRef,removedRefs}`，capture 的 `removedRefs` 固定为
`[]`。身份直接来自成功的 runtime 响应（`export --from` 则来自 assembly 生产者）。CLI
不重新打开 S，也不根据扩展名推断 E。单根 Bundle 的两个 selector 都绑定实际最终
Bundle 文件；upload Snapshot 返回两个实际 Manifest 身份。本地引用仅公开 basename，
保留 `@digest`、`@hmac`、`@manifest` 及已有 location，输出目录留在调用方执行上下文。
缺失或无效的响应返回错误，不输出成功 JSON。`--resume` 使用同一身份契约；不带
`--json` 时继续保留原有人类可读输出和 upload key stdout。

RFC-142 配对部署必须同时升级 sandboxer 与 orchestrator。操作员升级前须清空旧
orchestrator 本地数据库并重新创建记录；两个组件均不迁移、不回填，也不自动删除用户
数据。缺少必填 `resumeSandboxRef` 字段的旧迁移 token（包括缺少 E 的 Snapshot token）必须废弃，从完整 S/E 记录重新签发。portable
Export 即使在工件不可用时也只使用已记录的对，绝不读取 S 来发现 E。

`--drop-caches` 只属于 memory snapshot,默认 false. `--merge-ref=false` 独立录制当前工作集并保留一个历史合并层；true 将当前内存与本沙箱 checkpoint 的本地历史前缀合并。两者均在外部归属处停止，磁盘合并不受该开关影响。完整规则见[checkpoint 历史与清理](sandbox_zh.md#checkpoint-history)。

local `snapshot` 只需 `--sandbox-id` 或 `--path-id` 之一。两者同时给出时
PathID 只选择 `RunRoot/PathID/ctl.sock`，不做身份一致性校验。`--output` 可以位于
BaseRoot 文件系统（例如 `BuildBaseDir/checkpoint`）；运行进程只清理自己的
RunDir/owned diff，不会把 output 当作运行目录删除。

### 2.4 `sandbox-ctl export`

Live export 语法：

```text
sandbox-ctl export \
  --path-id a \
  --run-root /run/sandbox/builds/build-01 \
  (--output /artifacts | --upload) \
  [--mode local|bundle] \
  [--resume]
```

Image-to-Sandbox-E assembly 语法（不启动 VM）：

```text
sandbox-ctl export \
  --from ./flattened.img \
  --config sandbox.yaml \
  --sandbox-id base \
  (--output /artifacts | --upload) \
  [--mode local|bundle]
```

Live mode不接受 `--config`;它复用当前 run 进程已验证的 manifest/ref-location/crypto binding,因此显式 `--manifest-config` 和 `--ref-location` 仅属于 assembly mode. Assembly mode要求 `--config` 或 `SANDBOX_CONFIG` 且拒绝 `--resume`。两种模式都支持 `--timeout`：live mode 限制客户端等待 ctl socket 的时间，assembly 则给自己的 operation context 设置 deadline；0 关闭各自的 CLI 限制。两者都不会给 live-export request 增加服务端 deadline。Export 不接受 `drop_caches` 或 memory merge 参数,不调用 CH `/vm.snapshot`,不读取 memfd,不生成 memory refs.

live mode 与 local snapshot 相同，可只给 PathID；同时给出 SandboxID/PathID 时
PathID 只定位 ctl socket。Assembly mode 不接受 PathID，原有 `--sandbox-id` 仍仅是
输出 artifact alias，语义不变。

### 2.5 `sandbox-ctl exec`

```bash
sandbox-ctl exec --path-id a --run-root /run/sandbox/builds/build-01 -- /bin/sh -c 'id'
```

local exec 可只给 `--path-id`；若同时给
`--sandbox-id build-01-phase-a --path-id a`，仍只拨号
`RunRoot/a/ctl.sock`，请求本身不增加 SandboxID 或一致性检查。未给 PathID 时
继续使用 `RunRoot/SandboxID/ctl.sock`。

`exec` 通过当前 ctl/MUX 创建 sibling process. Export/snapshot 的 quiesce gate 原子阻止新 exec/forward 进入不稳定窗口,并关闭、join 已放行的 exec/forward session;在飞 exec 被终止且不会在 `--resume` 后自动重跑. Restore/attach 只有在新 MUX 建立且应用 cgroup 已 thaw 后才重新开放 exec、forward、plugin 和 app restart;ACK 与 thaw 之间抢先到达的请求会被 gate 拒绝,不会向 frozen cgroup fork. Guest `attach` 是幂等恢复操作;host 在 request/ACK 边界不明确时立即重试一次,且整个 dial/ACK 过程受 lifecycle context cancellation 控制.

远程授权 exec 使用 `pkg/ctl.ServeExecTunnel(ctx, options)`,固定以下顺序:

```text
Authorize -> AcceptDownstream -> ReadExecRequestFrame -> AuthorizeRequest
          -> DialBackend -> write frame.Raw once -> duplex relay
```

Request gate 通过前不得拨号 backend 或触发 Sandbox lifecycle. 首帧的 JSON payload 上限为 64 KiB，另有四字节长度前缀；必须是单一、字段集严格且无 duplicate 的 `exec_request`，包含非空 `exec.argv` 数组；读取最多等待 10s,调用方只能缩短. 通过后原始 frame 只写入一次,不 decode/re-encode. 已识别的 request/backend rejection 只对外返回脱敏 `exec request rejected`;`ProxyExec` 仅保留给已预连接 backend 的调用方.

### 2.6 `sandbox-ctl config`

```bash
sandbox-ctl config --template
sandbox-ctl config --config host.yaml:instance.yaml --check strict
sandbox-ctl config --config restore.yaml --mode restore --check strict
```

Template 同时标明 portable、host-only 和 ephemeral 字段. Restore mode 输出 host-only 文档并删除 cold-only workload 与 immutable disk refs;它不会生成第二套 restore schema.

### 2.7 `sandbox-ctl info`

```bash
sandbox-ctl info [--json] [--manifest-config manifest.yaml] <artifact>
```

- 对 E 输出 `sandbox.runtime.cfg`.
- 对 S 的默认输出是原始、精简的 `snapshot.cfg`,其中至少有 `sandbox_ref`.
- 对 S 的 `--json` 输出保留现有机器调用方需要的 resolved view: `Version`、`SandboxRef`、memory `FromRefs` 来自 S,`Resources`、`Boot`、`Launch` 和 `Metadata` 只从 S 引用的 E 派生. 该 view 不会写回 S,也不是第二套 snapshot provenance.
- 对 malformed、ambiguous 或既不是 E 也不是 S 的 logical root fail closed.

Local crypto、Manifest、Bundle、selector 和 named ref-location 与 run/publish 使用相同 opener.

### 2.8 `sandbox-ctl publish`

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

#### 2.8.1 引用改写与整链归并

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

<a id="publication-result"></a>

#### 2.8.2 发布结果报告

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

纯创建路径没有原根，差集为空；不会重新扫描已组装 source，source 所有权继续属于调用方。

```bash
sandbox-ctl publish --json --quiet --to-ref-location release=file:///srv/sandbox-artifacts ./s1.snapshot
```

<a id="usage-query"></a>

### 2.9 `sandbox-ctl usage`

`sandbox-ctl` 通过 `pkg/usage` 唯一负责采样调度、CPU 差分、Gauge 积分、
采样峰值、归并和持久化. `usage.Snapshot` 是累计状态, `usage.Record` 是
自包含保存记录. `sandbox-init` 只按请求读取、解析原始观测, 不包含 usage
ticker、累计器、历史摘要或可靠补发队列.

Usage 是可选观测, 不是账单、资源限额或物理成本归属. 范围只有 Guest 总/逐
vCPU 执行时间、Guest 物理内存、受管理可写文件系统占用, 以及 CH/sandbox-ctl
进程 CPU 和独立的 `RssAnon`/`RssFile`. 不包含 UFFD、网络或磁盘操作次数、
字节和时延. 既有诊断遥测和资源控制保持独立. 不提供 daemon、exporter、
计费策略、消费 ACK、全局 WAL 或删除后保留服务.

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
`--timeout` 限制查询, 包括离线 recovery/history, 默认 5 s 且必须为正. ctl socket 不存在或拒绝连接时
允许离线回退, 其他连接/协议错误明确失败. `--file` 隐含 `--offline`, 可以从
去掉 `.usage` 后缀的文件名推导 SandboxID.

`--saved` 省略 `live`. 历史查询采用非负字节 cursor 和 1–100 条记录的 limit,
默认 10. Host usage response 上限 1 MiB, 一页放不下时须减小 limit.
在线和离线读取器每次只解码、编码一条有界记录, 在保留或编码完整请求记录集合前
拒绝超出 JSON 预算的页, 并计入字符串转义膨胀. 历史不返回活动尾部.
普通 ctl 请求的 framing 和大小上限不变.

View 包含 `enabled`, 可选的 `live`/`saved`, `saved_end`, `saving`,
`unknown_tail` 和可选 `save_error`/`read_error`. 在线 `saved` 是 owner 已采用
的基线: 启动时恢复的完整存活记录, 或该 owner 后续已确认完整追加的记录.
在途写入的 CRC 可读不能单独推进基线. 离线 `saved` 是恢复校验通过的最后一条
完整存活记录, 不证明原 writer 已确认追加. 两种视图都不保证宿主崩溃或掉电后
仍存活, 写回边界见 §4.7. `live` 包含已经接收但仍在保存或尚未提交的输入,
崩溃后可以回退到存活的 saved. 缺测、未保存输入和文件尾部不确定是不同状态.

Live 状态可在逐项指标归并之间复制, 不是整轮观测的原子发布. 应分别读取
各 Gauge 的 `last_request_id`、状态和位置; 内存可能已反映恢复后的请求,
而文件系统仍描述前一次缺测请求. 查询不等待整轮边界, 也不改变任一指标
已接收的输入.

在线查询只复制已有状态或读取已确认历史, 不触发 Guest/CH 采集、累计推进或
flush. 离线 reader 获取非阻塞共享文件锁, 拒绝与活动 writer 并行读取; 此时应
查询该 writer 的 ctl socket. 关闭 usage 不删除已有文件, 仍可离线读取.

CLI 使用 [pkg/usagereader.Read](../pkg/usagereader/read.go), 这是 CLI 和本机
conductor adapter 的公共读取入口. 调用方沿既有生命周期权威定位当前对象的
control socket 和保存文件路径, 再传入精确 SandboxID、context、snapshot/saved/history
选择、cursor 和 limit. Reader 复用既有 `Recover`、`ReadHistory` 和文件锁;
[MarshalHistory](../pkg/usagereader/history.go) 与在线 owner 共用同一个有界分页编码器.
不增加第二套 codec、恢复算法、累计器或持久状态. Native JSON 保持无损, 不经过
浮点中间值; live、saved 或 history 数据中的 SandboxID 不匹配时拒绝.
ctl response envelope 始终携带 owner 的 `sandbox_id`, 即使 usage 关闭、
live/saved 为空或历史为空, 也必须核对身份. 该字段不改变 native View/Record 或文件格式.
空历史返回 `records: []` 及已校验的 next cursor. 在线分页必须显式返回不回退的
cursor: 非空页推进 cursor, 空页保持 cursor, 记录数不得超过请求 limit.
saved view 必须在且仅在存在 saved record 时携带正数 `saved_end`.
非法 owner response 不允许回退离线读取. 离线 open 使用非阻塞标志,
先拒绝 FIFO 等非 regular file, 不会为等待 FIFO writer 而绕过 timeout.

只有 socket 不存在或拒绝连接时允许回退. 一旦已连接 owner, EOF、非法 response、
owner 错误、取消或超时都使查询失败, 不能被可读的保存文件替代. 取消会关闭当前
ctl connection. Owner response 完成 JSON 解码、验证和 saved view 投影后,
也会检查取消. 离线取消会停止调用方等待, 并在文件读取之间检查取消.
阻塞的文件系统调用继续持有执行槽和共享锁, 直到实际返回; 每进程最多有 8 个
离线执行, 包括调用方已取消的执行. 等待执行槽同样遵守查询 deadline.
这不会使文件系统调用变成可中断操作, 也不改变 native saving.
Telemetry 通过 conductor 消费原生 stats,
不直接调用此 ctl/file reader.

<a id="journal-output-targets"></a>

### 2.10 Journal 输出目标

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

#### 2.10.1 语法和编码

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



#### 2.10.2 组件日志与 Guest 输出

`run --log-to default`（省略时也采用此默认值）保留普通组件诊断的 stderr 输出。
`run --log-to journald=...` 则显式将 Go logger 输出和 run/restore 运行诊断发送到
该目标，包括目标解析完成之后的启动准备失败和终态错误。它不要求 stderr 已连接
journal，也不要求存在任何特定身份字段。

在可用目标建立之前，非法 flag 语法和非法 `--log-to` 值仍可写到原始 stderr。
`--stderr-to` 始终是 guest 应用的 stderr 目标。`--log-to` 不接管进程描述符，不改变
TTY/pipe 选择，不重定向 Cloud Hypervisor 自身的 stderr，也不捕获 runtime panic 或
系统事件。既有消息文本和日志时间精度保持不变。`exec` 自身的诊断 stderr 及错误捕获
行为不变；文件、artifact、JSON 及其他非 journal stdout 数据不受影响。



#### 2.10.3 分行、失败与生命周期

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

## 3. 配置与制品格式

### 3.1 `sandbox.yaml`

`sandbox.yaml` 是 host 输入,可以包含 portable workload、host policy 和 instance data. 以下示例展示字段归属,不是固定部署值:

```yaml
resources:
  capacity: { cpu: 2, memory: 2GiB }
  allocatable: { cpu: 1.5, memory: 1536MiB }
  # Host-only policy:
  control: { cgroup_path: /sys/fs/cgroup/sandboxes/s1 }
  overhead: { memory: 128MiB }
  watermark_high: { ratio: 0.875 }
  startup: { memory: 512MiB }
  diff_cow: { cache_size: 32MiB, max_dirty_size: 16MiB }

network:
  # Host provider and current identity:
  tap: tap0
  interface: eth0
  ip: 169.254.1.1/31
  mac: "02:00:00:00:00:01"
  hostname: sandbox-1

boot:
  kernel: file:///opt/kuasar/vmlinux
  runtime: file:///opt/kuasar/sandbox-runtime.bundle
  cmdline: "console=hvc0"
  root:
    base: file:///images/root.erofs@digest:<digest>
    overlay:
      base: file:///layers/parent.overlay@digest:<digest>
      base_from_refs: []
      # Host-only active binding:
      diff: file:///var/lib/kuasar/s1.overlay.diff
      diff_template: file:///opt/kuasar/empty.ext4
  disks:
    - name: data
      base: file:///layers/data.overlay@digest:<digest>
      diff: file:///var/lib/kuasar/s1.data.diff

mounts:
  - { target: /data, type: disk, source: data }
  - { target: /tmp, type: tmpfs }
  - { target: /var/cache/app, type: empty }

files:
  - { path: /etc/app/config, content: "portable" }
ephemeral_files:
  - { path: /run/instance-token, content: "current-run-only" }

init:
  - { exec: /bin/sh, args: ["-c", "prepare-data"] }

launch:
  exec: /usr/bin/app
  args: ["serve"]
  env: { MODE: production }
  ephemeral_env: { REQUEST_ID: current-run-only }
  plugin:
    - { exec: /usr/bin/sidecar, restart: always }
  restart: never

metadata: { workload.kind: api }

restore:
  prefetch: off
timeouts:
  ch_api: 30s
usage:
  enabled: false
  sample_interval: 1s
  flush_interval: 5m
```

普通 cold run 使用完整 validation. `run --from` 和 `run --restore` 先严格解析 artifact,再按字段 presence 应用各自 rules,不能使用无约束 `LoadMerged` 覆盖 artifact graph.

### 3.2 files、env 与 ephemeral

Cold launch merge:

```text
files < ephemeral_files       # same path: ephemeral wins
launch.env < ephemeral_env    # same key: ephemeral wins
```

每个 files 列表内部 duplicate path 都拒绝. Persistent `files` 和 `launch.env` 进入 C0/C1;ephemeral 字段不进入 Portable schema.

Runtime semantics:

- `files` 由 sandbox-init 使用 tmpfs backing + bind 注入.
- Export 保存 file declaration,不保存 injected file 的运行期修改.
- `run --from` 再次注入 persistent files,并再次应用 persistent env.
- `ephemeral_files` 和 `ephemeral_env` 只影响当前 cold invocation.
- Ephemeral 不表示从 memory snapshot 中擦除；RAM 仍可能含其内容。若 guest 把它复制进导出的 disk-backed file，排除声明也不会擦除这份副本。
- disk-backed `type: empty` volume随所属 root/data disk export.
- `type: tmpfs` 内容不随 export.
- `init` 在 `run --from` 时重新执行.
- plugin/app 按 cold semantics 重新启动.
- `run --restore` 不重跑 launch/files/init/plugin.

Restore host若显式提供 `boot.cmdline`、launch persistent/ephemeral fields、mounts、files/ephemeral_files、init 或 metadata,会在副作用前拒绝,而不是静默忽略. `resources.startup` 是 host-only node policy,可在 restore 时提供;它写入 node reservation contract,但 Snapshot 捕获的 `BudgetAtSnapshot` 仍是 restore initial Budget 的权威值.

<a id="usage-policy"></a>

### 3.3 Usage policy

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
`flush_interval` 调度记录追加, 不表示文件系统同步或设备缓存刷盘.

Usage 在普通 cold、`run --from` 和 `run --restore` 中都是 host policy,
不进入 `PortableSandboxConfig`、E 或 S. 恢复后使用当前 host policy, 不继承
制品中的开关. 参见 [host overlay 示例](../examples/usage-enabled.yaml).

关闭时 Host 不创建 usage sampler、usage 长连接或周期保存. 既有资源控制报告
和 balloon policy 不变. Guest 磁盘组装仍保留有界私有文件系统句柄, 使后续内存
恢复可以开启 usage; 保留句柄本身不调度读取.

<a id="diff-cow-policy"></a>

### 3.4 `resources.diff_cow` 与缓存所有权

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

<a id="portable-config"></a>

### 3.5 PortableSandboxConfig

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

目标 host 使用实际 path、source directory 或 named ref-location 在 controller、network、cgroup、VM 或 run directory 副作用前完成 binding。验证依输入而异：显式 cold projection 会计算 kernel hash；`run --from` 与 restore 只检查 kernel 的绝对 file binding、存在性和 regular-file 类型，刻意跳过完整内核重哈希，同时将 runtime Bundle footer identity 与 C0 比较。因此后两条路径不独立证明所给内核匹配 E 中记录的 digest，内核部署仍是受信 host 的责任。Carrier metadata 和按配置启用的内容验证保留各自检查。源码见 [lifecycle.go](../pkg/sandbox/lifecycle.go) 与 [restore.go](../pkg/restore/restore.go)。

<a id="strict-encoding"></a>

### 3.6 Strict encoding 与 limits

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

### 3.7 C0、C1 与 source binding

`C0` 是一次 run lifecycle 的 immutable portable baseline:

```text
explicit cold config + persistent fields + canonical identities
  - host-only - ephemeral = C0

E.sandbox.runtime.cfg + allowed persistent overrides
  + canonical host bindings - ephemeral = C0 for run --from

E.sandbox.runtime.cfg non-boot defaults + allowed persistent overrides
  + complete replacement boot - ephemeral = a new explicit-cold C0

S.sandbox_ref -> E.sandbox.runtime.cfg
  + allowed restore host bindings = C0 for run --restore
```

`C0` 在 VM side effect 前写入 run directory,之后字节不变. Live runtime 另持有不序列化的 `RunSourceBinding`:

- current `self` 对应的 source E/root artifact;
- root payload 与 optional image config;
- resolved local path/source directory;
- Bundle reader/fetcher/source refs;
- named ref locations;
- host-only disk bindings和当前 active diff.

Memory restore另持有 `MemorySourceBinding`,其中 Snapshot S identity 与 `from_refs` 只管理 memory provenance.

`C1 = Export(C0, disks at T)` 只写入新 E. `export --resume` 和 `snapshot --resume` 不修改 C0、active diff、lower graph 或 memory parent. 因此同一 VM 可以产生 `E1@T1` 和 `E2@T2`,同时继续使用原 C0 + writable diffs.

<a id="self-provenance"></a>

### 3.8 `self` 与 disk provenance

Portable graph 必须恰好出现一次保留值 `self`,且只能位于:

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

`run --from` 通过 source binding 把 `self` 绑定到当前 E payload. Export C1 先把 C0 的旧 `self` 物化为原 source ref,再把新 root payload 位置设为 `self`;这避免 E digest 自引用循环.

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

### 3.9 `.sandbox` logical format

只接受两种 layout.

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
- exact entry set只能是 `{sandbox.runtime.cfg}` 或 `{config.json,sandbox.runtime.cfg}`;
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
}
```

`Payload` 是保留 sparse `RunAt`/`ReadAt` 的 section view. Shared close owner保证 FullStream/Payload/Root 任一 close 最终只释放 carrier 一次. Vhost 只能看到 Payload,不能看到 ZIP tail.

Live BlockCOW SnapshotView 是 upper-only sparse delta,ext4 superblock offset 可以是 Hole,所以不能要求每个 live payload独立通过 ext4 magic. Reader 校验 logical size、graph/self 关系和最终 composition. Direct EROFS payload则严格校验 EROFS logical size/magic.

<a id="snapshot-format"></a>

### 3.10 `.snapshot` logical format

Snapshot S layout:

```text
[memory sparse payload]
[ZIP:
  config.json
  state.json
  snapshot.cfg
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

## 4. 资源模型

### 4.1 三种部署模式

| Mode | `cgroup_path` | `controller` | Behavior |
|---|---|---|---|
| No cgroup | empty | empty | 不写 cgroup；allocatable CPU 必须等于 capacity CPU，allocatable memory 必须处于 capacity 范围内 |
| Static cgroup | set | empty | 本地设置 CPU/memory limit,可运行 local sensor |
| Dynamic | set | set | 通过 resource protocol admission/lease/Budget,并运行 local sensor |

`resources.capacity` 是 guest-visible VM capacity,进入 Portable config. `resources.allocatable` 是 cold start 的 workload 默认值,也进入 Portable config;其中 `allocatable.cpu` 必须是有限数,且满足 `0 < allocatable.cpu <= capacity.cpu`. Restore 保持 E 中的 capacity 和已捕获的 `deflate_on_oom`,但可以从目标节点显式重新应用 allocatable CPU/memory;该运行时 policy 不改写 E 或 C0. `control`、`overhead`、`watermark_high` 和 `startup` 是 node policy,不进入 E.

Export/snapshot 获取 MemoryController mutation barrier,并在 freeze 前 lift/drain 可能与 CH pause 竞争的 `memory.high`. Host ping gate 会让已入场探测完成 `pong` + guest EOF transport barrier;该排空独立受 8 s quiesce budget 约束,即使普通 `timeouts.ping` 关闭强制超时也不会无限阻塞捕获. 到期时 host cancel并join该探测,捕获失败后走完整 recovery. Guest quiesce 还会排空并暂停周期 `mem_report`,防止 S 捕获持有 stream lock、仍等待旧 host vsock 的 reporter. Restore 在 ACK 前切换到新 observation epoch;`--resume`/失败 attach 恢复原 epoch,live attach只重开pause gate且不破坏已入场报告的计数. Recovery 在 VM、MUX、app 和 backend 恢复后释放 host barrier.

### 4.2 Memory terms

```text
CapacityMemory      = CH boot memory zone maximum
AllocatableMemory   = workload settled guest headroom
DemandMemory        = controller observation
Budget              = admitted/locally enforced working allowance
VMM memory.max      = CapacityMemory + host overhead
```

Memory S capture记录 CH config/state 和 memfd sparse content;资源 policy 不写入 `snapshot.cfg`. Restore capacity identity来自 E,host若显式给出必须一致. Allocatable CPU/memory、`startup`、`overhead`、`watermark_high` 和 resource controller binding来自当前 host;`deflate_on_oom` 保持与已捕获 VMM state 一致;Snapshot 中的 `BudgetAtSnapshot` 仍是 restore initial Budget 的权威值.

### 4.3 CPU 与 balloon

Capacity CPU 决定 vCPU topology. Allocatable CPU 在 cgroup 模式映射为 `cpu.weight`；无 cgroup 时必须等于 capacity CPU，不能表达独立的 fractional allocation。 Balloon current state由 CH restore state权威恢复,host不从 S 发明第二个 balloon state.

<a id="writable-disk-capacity"></a>

### 4.4 可写磁盘容量与配置迁移

可写磁盘的容量继承自所选文件系统来源，而不是独立的大小配置。已有且非空的活动
`diff` 保留其逻辑容量；从 `diff_template` 初始化的新 diff 继承模板的逻辑容量；
没有模板时，基于 COW `base` 创建的新 diff 继承该 base 的逻辑容量。已有的空 diff，
以及没有文件系统来源的新磁盘，仍然属于无效输入。在双设备 overlay 模式下，这里的
COW base 属于可写 ext4 upper，不是只读 EROFS 镜像。这些规则同时适用于 `boot.root`
和 `boot.disks[]`，冷启动、`run --from` 以及内存快照恢复均保持原有行为。

原来的 `diff_size` 在这些受支持的路径上从未实施容量或配额限制。该配置及其具有误导性
的 1 GiB 默认值现已删除。在 root、数据盘或其 `overlay` 下显式配置 `diff_size`，
或使用错误的 `size` 写法，现在都会返回附带迁移指引的错误，包括空字符串和 null 值。
请从已有配置中删除这些键，并准备具有所需容量的文件系统来源。新生成的配置不再输出
这些键；无关配置字段和不透明的 metadata 不受影响。

对于新建的可丢弃 ext4 工作盘（overlay upper、单盘 root，以及 scratch 或数据盘测试
文件），使用 `mkfs.ext4 -O ^has_journal` 格式化稀疏模板，避免文件系统 journal
占用及元数据日志写入；这不影响 journald 或应用日志。COW 不能替代 journal，
此默认约定不承诺中断后工作盘的崩溃恢复。已有带 journal 的模板、用户自带镜像和
快照继续兼容；guest sync/quiesce 与快照/恢复语义保持不变。不要为应用此建议而
重新格式化已有数据盘。

对于一个**新建的**、空白的 512 MiB scratch 文件系统，可以准备新模板，使用时不再指定
大小覆盖项：

```bash
truncate -s 512M /tmp/scratch-512m.ext4
mkfs.ext4 -F -O ^has_journal /tmp/scratch-512m.ext4
```

```yaml
boot:
  # 保留完整配置中的其他必需 boot 字段。
  disks:
    - name: scratch
      diff_template: file:///tmp/scratch-512m.ext4
mounts:
  - target: /scratch
    type: disk
    source: scratch
```

必须使用新的活动 diff：已有 diff 优先，因此更换模板不会调整容量或替换已有数据。
不要通过截断已有文件系统来实施更小的限制。Sandboxer 不会自动调整文件系统大小，
不会限制 COW 已物化的存储占用（包括 tmpfs 文件的内存/swap；待回写缓存由 `resources.diff_cow` 单独限制），也不会在启动或恢复时改变磁盘容量。逻辑块设备容量与加密
文件的物理长度、宿主磁盘实际分配空间、guest 文件系统可用于文件数据的空间并不是
同一个概念。本次迁移不提供从同一个模板任意选择各实例容量的新能力。

<a id="usage-metrics"></a>

### 4.5 Usage 指标、单位和计算

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

#### 4.5.1 CPU Counter

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

#### 4.5.2 Guest 内存和 Balloon

Guest 观测来源见[原始 usage ABI](sandbox-init_zh.md#usage-observations)。

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

#### 4.5.3 文件系统和 Host RSS

Guest 观测来源见[原始 usage ABI](sandbox-init_zh.md#usage-observations)。

```text
filesystem occupancy = (f_blocks - f_bfree) * unit
unit = nonzero f_frsize, otherwise f_bsize
```

不使用 `f_bavail`. 恢复后的已有占用完整计入, 不扣启动初值. RSS 只读取两个
进程的 status, 将 kB 换算为 byte, 不扫描 `smaps`. 这些分项既不是全部非 Guest
内存, 也不是独占物理成本.

<a id="usage-sampling"></a>

### 4.6 Usage 采样与区间摘要

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

<a id="usage-persistence"></a>

### 4.7 Usage 持久化与文件格式

文件唯一位置为 `BaseDir/<SandboxID>.usage`, BaseDir 按既有生命周期从
BaseRoot/PathID 派生. 逻辑身份不是 RunID, 正常重启延续同文件. 新 SandboxID
的克隆具有独立历史. Usage 不进入 E/S, 恢复旧业务快照不会回滚用量文件.

Host 只保留常数个状态:

```text
S = confirmed saved baseline + file offset
F = immutable record currently being saved/reconciled
A = active observations merged while F is in flight
```

Live 累计包含 F/A. `WriteAt` 完整写入且没有错误时, 只把 S 推进到 F.
追加失败或短写后, 只有 truncate-to-S 成功确定逻辑回退, 才能把区间统计并回 A.
截断失败保留 F 原身份及内容, 在下次追加前先处理尾部. CRC 可读不能推翻
当前 writer 的追加失败或未完成状态; 新进程采用完整存活记录是另一种情形.

Usage 不执行显式文件、目录或文件系统同步: 不调用 `fsync`、`fdatasync`、
`syncfs`、`sync`, 不强制范围写回, 不使用同步打开标志. 此规则覆盖创建、
周期保存、回退、恢复和正常退出. 写回由操作系统及底层存储决定.
`saved` 仅表示逻辑追加完成, 不表示稳定存储持久化. 宿主崩溃或掉电可能丢失
已经确认的记录甚至整个文件, 也可能留下损坏; 恢复只能使用实际存活的有效字节.

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

<a id="usage-lifecycle"></a>

### 4.8 Usage 生命周期与失败处理

Sampler 初始化失败时报告既有的不可用/错误状态, 释放文件所有权, 不保存或截断
checkpoint. 原有字节仍可离线读取, 新建文件可能保持为空. 该次尝试不暴露未初始化
的 live/saved 视图, 也不写入新的 closed 记录.

若初始化成功, 随后的 Host 设置或 CH 启动失败, 收尾仍按既有预算尽力读取并
保存实际 sandbox-ctl CPU/RSS. 这是 Host 进程用量, 不代表 Guest 已运行. 之前的累计值
和完整记录字节保持不变; 新 epoch 遵循§4.5.1的保守完整性规则. CH 未能启动
不免除已经消耗的 Host CPU.

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
timeout/busy. 成功保存同样不提供宿主崩溃或掉电持久化保证, 具体见§4.7.
SIGKILL 测试覆盖进程终止, 不模拟物理掉电.

## 5. Cold start 与 `run --from`

### 5.1 显式 cold start

普通 `run --config` 的准备与启动顺序：

```text
T0 parse/merge/validate config and limits
T1 open/check immutable refs and image defaults; derive kernel/runtime identities
T2 project portable C0 and validate exactly one self
T3 preflight active diff requirements and source binding
T4 create run directory and atomically write sandbox.runtime.cfg once
T5 acquire controller/cgroup resources; materialize active diffs and network binding
T6 construct vhost block devices from payload-only streams
T7 spawn/configure CH
T8 launch sandbox-init spec and app
T9 establish MUX/pinger/forward/resource lifecycle
```

可预测的 config/ref/format 错误应在 preflight、controller/network/VM 副作用前失败。这不保证后续操作全部无副作用：创建 run directory、写 C0、创建 diff 和获取资源是可能失败并需要清理的后续步骤。已有 portable C0 的 kernel/runtime 验证差异见 [PortableSandboxConfig](sandbox_zh.md#portable-config)。

### 5.2 CH command line boundary

Cold start 用 host-bound kernel/runtime paths、memfd、vhost sockets、active diffs 和 optional net provider构建 CH argv. Portable config从不保存这些绝对 path/fd.

Block devices只绑定 logical Payload:

- Container image或 direct EROFS E:EROFS prefix only.
- Parent E作为 ext4 layer:E root payload only.
- Current run-from E:self resolves to E payload section only.
- ZIP tail永不暴露给 vhost.

CH stdin固定 `/dev/null`;console output由 sandbox-ctl bridge. Net provider为 TAP 或 TapFD,而 portable topology只说明 NIC是否存在和 interface name.

### 5.3 `run --from` rules

`ApplyFromRules` 使用 YAML field presence执行 ownership:

| Owner | Fields |
|---|---|
| Artifact strong | root/data immutable graph, disk count/order/name, topology, self position, cmdline |
| Host strong | kernel/runtime actual path, active diff/template, cgroup/controller, network provider, timeout, usage, manifest/crypto/ref-location, CH binary |
| Persistent override allowed | resources workload defaults, launch, mounts, files, init, metadata |
| Instance-only | IP/MAC/hostname, ephemeral files/env, stdio/forward |

受保护字段有冲突时明确报出 field context. Host不能通过省略或 YAML merge静默改变 disk graph. Persistent `mounts` 可以修改普通 tmpfs/empty mount 的声明,但 data-disk mount 的 source、target、name 和 order 必须保持 artifact topology,不能借 mount override移动磁盘.

Network必须满足:

```text
portable network.enabled == host provider presence
portable interface       == explicitly supplied host interface
```

Direct EROFS E只有 read-only image. 目标 host必须提供 pre-formatted `diff_template`,或已格式化的 explicit diff. 缺少可挂载 upper时在 controller/network/CH side effect 前失败.

最终调用普通 `sandbox.Run`;`run --from` 不进入 `restore.Run`.

默认 `run --from` 继续执行上述强 ownership rules. `--replace-boot` 只选择另一条显式、原子的 cold derivation:

```text
source E 的 non-boot portable defaults
  + presence-aware persistent overrides
  + host config 的完整 boot 值
  + host/instance-only bindings
  -> ordinary ValidateCold / artifact preflight / ProjectPortableCold
```

完整 replacement 同时覆盖 `boot.kernel`、`boot.runtime`、`boot.cmdline`、`boot.root` 和整个 `boot.disks[]`;不存在 root-only 或 leaf-level graph merge. Host config 没有写 `boot.disks` 就表示零个 data disk，而不是继承来源 disks. 若来源 mounts 仍引用已移除或改名的 disk，调用方必须同时替换 mounts；普通 cold disk/mount 1:1 validation 会在 controller、cgroup、run directory、network 或 VM side effect 前拒绝不一致结果.

Replacement 不复用来源 E 的 kernel/runtime 默认 binding、`self`、`RunSourceBinding` 或 Bundle reader/fetcher. Host boot refs仍通过普通 cold canonicalization、local crypto、Manifest/Bundle lookup和preflight. 生成的新 C0及后续 E/S disk closure只依赖replacement boot. `--replace-boot` 必须与`--from`和显式`--config`（或`SANDBOX_CONFIG`）一起使用，且永远不能与`--restore`一起使用.

## 6. Export 与 snapshot 数据流

### 6.1 Output graph 与 commit point

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

<a id="disk-memory-provenance"></a>

#### 6.1.1 Disk 与 memory provenance

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

#### 6.1.2 托管 checkpoint 历史合并与选择性清理

`merge_ref=false` 独立记录本轮驻留工作集。已有 `S1 -> S0` 时，下一次捕获先流式生成新的
不可变历史 Snapshot `S1′ = S1(memory) 覆盖 S0(memory)`，再写出
`S2(当前工作集) -> S1′`。后续本地捕获重复该组合，最新 S 下最多保留一个本沙箱拥有的本地
内存 lower。`merge_ref=true` 将当前驻留内存与整个可合并本地前缀合并；false → true → false
切换同样收敛。reader 仍支持旧的多层输入。恢复执行状态和 `sandbox_ref` 只来自最新 S，
历史 S 只提供 memory。

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

保留集合包含当前 S/E、历史内存载体、当前磁盘/upper/不可变 base 载体及复用文件。历史 S
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

### 6.2 Freeze sequence 与 failure recovery

所有 predictable preflight在 guest freeze 前完成:

```text
T0 output/manifest/local-crypto/ref/Bundle/merge/config/source preflight
   Close usage admission and break Gauge continuity before the resource barrier.
T1 enter MemoryController/Budget mutation barrier
T2 lock and lift/drain memory.high
T3 pause pinger and gate new host exec/forward;guest drains exec/forward and mem_report, freezes app, syncs,
   closes MUX;after quiesced+EOF host joins residual exec/forward handlers
T4 pause CH, quiesce all block backends
T5 capture every data disk exactly once
T6 build C1 from immutable C0 + source binding + captured disk refs
T7 capture root once and emit Sandbox E
```

Export continues:

```text
T8 commit E last
T9 destroy, or --resume backend -> CH -> MUX/pinger/forward -> app -> barrier
```

Snapshot continues in the same T4 freeze:

```text
T8  CH /vm.snapshot -> config.json/state.json
T9  scan/capture memory self
T10 build S(snapshot.cfg.sandbox_ref=E, from_refs=memory parents)
T11 commit S last
T12 destroy, or --resume full recovery
```

Export固定不执行 drop_caches,不调用 `/vm.snapshot`,不读取 CH snapshot files或 memfd. Snapshot只读取一次 disks,不会先为 E捕获一次再走旧 disk path第二次.

BlockCOW SnapshotView只在 backend quiesced时稳定. V1在整个 sink read窗口保持 VM paused;不能恢复 VM后继续读取 view.

Failure semantics:

- Data artifacts/E 可能成为 content-addressed orphan。回收属于 owning storage/retention policy；sandboxer 不保证自动、按完整图回收 local output 或 shared location。
- Root alias只在 E或S成功后提交.
- `--resume` success/failure按 backend -> CH -> MUX -> pinger/forward -> app -> resource barrier恢复.
- `attach` ACK不明确时host幂等重试一次;若仍不能重建MUX,该capture变为terminal failure,host在memory/memory.high guards仍持有时请求VMM shutdown,再以SIGTERM/SIGKILL有界兜底. 失败不会留下可接受新请求但guest仍冻结的VM.
- 默认destroy也等待CH退出;`/vmm.shutdown`失败或超时后使用SIGTERM/SIGKILL有界兜底,然后才释放lifecycle guards.
- C0、active diff和live lower graph保持不变.
- Local capture 使用 same-directory temp 并清理失败的 partial output；named-location publication 使用 [Local tarstream 与 crypto](sandbox_zh.md#artifact-publication) 的独立所有权检查与清理规则。

### 6.3 `ctl.sock` protocol

socket 固定位于 `RunRoot/PathID/ctl.sock`；PathID 省略时等于 SandboxID。
PathID 是 host-side 定位参数，不进入以下 wire request：

Snapshot和export使用独立 request type:

```text
snapshot_request -> snapshot_done | error
export_request   -> export_done   | error
exec_request     -> exec_ack      | error
usage_request    -> usage_response | error
resource_stats_request -> resource_stats_response | error
```

唯一 reaper 在 `waitid(WEXITED|WNOWAIT)` 确认 VMM 退出后立即撤销 resource 的 live 观测,然后按既有顺序执行 usage 最后采样与 `cmd.Wait`. usage 关闭时也使用同一退出通知;不增加采样或改变停止/保存语义. 退出期间仅保留有效规格字段,不为已退出 VMM 返回新的宿主观测时间.

Export不是 `snapshot_request{memory:false}`. Request在 run process中执行,因此可以复用当前 lifecycle barrier、guest/MUX gate、CH API socket和live vhost SnapshotView.

Usage 只读取 owner 已有 `usage.Snapshot`/`usage.Record` 或已确认文件历史,
不采样、不保存、不获取 capture/CH mutation barrier. 只有其 response 采用
独立的 1 MiB JSON 上限, 普通 ctl framing 和上限不变.

Reader 拒绝缺失或无效的必填规格: CPU capacity 和 allocatable 必须为正,allocatable 不得超过 capacity;memory capacity/headroom 必须为正,headroom 不得超过 capacity. 这些校验不会拒绝合法的宿主零计数.

`resource_stats_request` 是独立的窄只读请求. cold/from/restore 共用的 runtime
返回本次运行最终生效的 capacity CPU、allocatable CPU、capacity memory 和
allocatable memory headroom, 包括 host override. Allocatable CPU 对应相对
调度规格 `cpu.weight`, 不是 fractional-core quota 或无条件性能保证.
Headroom 是 `resources.allocatable.memory`, 不是当前 Budget、Guest free
或节点 reservation. Reservation 仍以节点 controller 为权威, 本响应不猜测它.

有 live VMM 且已配置 cgroup 时, `memory.current` 和 `cpu.stat.usage_usec`
使用同一个已固定的 cgroup descriptor. 响应的 `memory_used`、`cpu_usage_usec`
以十进制字符串无损保留无符号整数, conductor HTTP adapter 将 CPU 表达为秒.
读取不纳入宿主 ctl 进程, 不重复加 Guest CPU, 不扣除 inactive file/balloon,
不把 VMM memory 截断到 Guest capacity. CPU 是当前统计来源的累计值,
来源重建可以重置; 生命周期累计仍由 native usage 负责.

响应携带 `sandbox_id`, 供 [ctl.ReadResourceStats](../pkg/ctl/resource_stats.go)
核对精确身份. 两个宿主 counter 分别保留有效性: 缺测省略, 合法零值保留.
时间戳必须与至少一个宿主 counter 一同存在; ctl reader 拒绝其他组合.
实际宿主读取提供 `timestamp_unix`; 只有规格、无 cgroup 或无 live VMM 时,
不伪造宿主观测和时间戳. 已配置 counter 无法读取或格式非法时明确失败.
存在但缺少 `usage_usec` 的 `cpu.stat` 属于格式非法, 包括空文件, 不作为缺测处理.
此读取没有 sampler、历史、Guest 请求、CH resize 或 controller mutation.
Static/dynamic control 共用同一读取路径, usage 和 telemetry 可以独立关闭.
既有 ctl connection context/deadline 限制客户端等待.

真实 memory-budget E2E 将重复 ctl 读取与已冻结 VMM 的实际 cgroup 文件对照,
检查 ctl 进程位于该 cgroup 之外, 并验证 usage 保持关闭、读取不修改资源控制.
冻结仅属于测试操作, 生产 reader 不冻结 VM.

Response 中不回显 secret。Remote Manifest upload 可以耗时较长。Local snapshot/live export 的 CLI `--timeout=0` 不给 ctl connection 设置 deadline；正数只限制该客户端的等待。两者都不是 wire request 中的服务端 deadline 字段。服务端 lifecycle context 仍控制支持取消的 I/O，CLI 超时不能证明没有 artifact 被 commit。Image-to-Sandbox-E assembly 则将正数 timeout 应用于自己的 operation context（§2.4）。

### 6.4 Image-to-Sandbox-E assembly

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

<a id="artifact-publication"></a>

### 6.5 Local tarstream 与 crypto

Local immutable artifact支持 `crypto.local=off|auto|required`:

- `off`:plaintext tarstream,identity `digest`.
- `auto`:自动识别plaintext或KDXTS encrypted tarstream;新输出按配置codec.
- `required`:拒绝plaintext和未绑定key的identity;identity使用`hmac`.

未配置 policy 时默认 `off`；`auto` 或 `required` 在 storage 构造阶段就解析 customer key，即使是 file-only operation。没有 Manifest config 就没有 local codec 或 lazy Manifest client；file-only 不表示已配置的 crypto 可以省略 key。源码见 [storage.go](../pkg/artifact/storage.go)。

Existing-file reuse 必须重新验证 role、logical size、content identity 和完整 stream。 Local output与named ref-location的commit策略刻意分离. Artifact capture/publication只定义logical completion,不定义stable-storage durability:local output依赖完整写入、`Close()`检查、内容寻址no-replace rename与alias atomic rename;named-location依赖`O_EXCL`写入、`Close()`检查、最终路径reopen/full verification与路径身份检查. 两种 artifact-publication 路径都不执行显式 file/directory flush，物理写回由文件系统或底层存储实现定义；这不同于 §3.6 中执行 fsync 的 run-directory C0 写入。

<a id="local-output"></a>

#### 6.5.1 Local output

`snapshot/export --output` 的 `FileSink` 在output directory中写unique same-directory temp,完整写入并检查`Close`错误,再以`renameat2(RENAME_NOREPLACE)`完成O(1) final commit. `BundleSink`同样以当前atomic no-replace rename提交完整Bundle;两条本地路径都不会为了final commit再读取并复制完整artifact. Root成功后,semantic alias用随机temporary symlink + atomic rename更新. Alias target和existing entry都以`NOFOLLOW`/`lstat` fail closed,不会把regular file或directory替换成symlink;commit由完整写入、`Close()`检查与atomic rename定义,不执行显式file/directory fsync. 因此local output要求节点本地filesystem提供这些atomic rename和symlink语义.



<a id="named-ref-location"></a>

#### 6.5.2 Named ref-location

`publish/upload-snapshot --to-ref-location`不复用`FileSink`或`BundleSink`. Tarstream carrier在自身marker中保存payload boundary和payload commitment;完整读取会用payload bytes复验该声明. 打开carrier后可直接提供identity. E/S只替换dense metadata tail时,carrier用旧payload commitment和新tail以O(tail)工作量推导新identity,不读取GiB级payload,也没有首次`io.Discard`编码. Location target取得carrier给出的scheme/digest后,以`O_CREATE|O_EXCL`直接创建`<digest>.image|overlay|sandbox|snapshot`;canonical encoding是shared target中的唯一完整write. `.image`承载immutable root image,`.overlay`只承载独立的writable disk dependency;Sandbox E payload不会重复发布为`.overlay`. Plaintext输出使用`@digest`,codec-backed输出使用`@hmac`. Target directory中没有完整temp/staging副本,也不创建`<sid>.sandbox`、`<sid>.snapshot`或任何其他semantic alias.

Fresh final固定`0644`. `tarstream.WriteTo`在唯一一次写入过程中检查source read和destination write,并重新产生与carrier预先提供值一致的scheme/digest;在 owned write fd 仍打开时以 `lstat` + `SameFile` 确认 canonical path 仍指向本次 `O_EXCL` 创建的 inode；metadata-only guard fd 在 checked `Close` 前固定该 inode。Publisher 随后以 `O_RDONLY|O_NOFOLLOW`（另含 nonblocking，避免意外 FIFO 阻塞验证）重新打开并完整验证regular file、role/payload name、logical size、canonical tarstream、marker、codec、crypto policy、digest scheme/digest和完整 sequential stream；验证后还会再次核对 path identity。Existing final 走同一完整验证；验证成功后直接复用,inode和bytes不改变.

Manifest Bundle不进入tarstream E/S重建路径. Carrier以root Manifest key提供`@manifest` identity;location target先强制验证所选Manifest closure、recorded admission、physical keys和crypto domain,再对same-directory依赖Bundle按顺序做exact byte copy,root `<key>.bundle`最后发布. 每个共享final仍只写一次,copy后重新打开、验证canonical Bundle/root并与source逐字节比较,最后在target fd上再次强制验证所选closure. 该路径不创建`.snapshot/.sandbox`替身,也不改写Bundle内的`snapshot.cfg`.

Final path在write完成前会短暂可见. 正常consumer只能使用publisher成功返回的root ref;publisher仍按dependencies first、root last顺序发布. Concurrent publisher遇到partial final时重新打开并做有限、context-aware exponential-backoff验证;若writer在窗口内完成则复用. bounded retry后仍不完整或invalid时fail closed并提示显式cleanup/repair,不会删除unknown owner的path. Symlink、directory、FIFO和其他non-regular final同样拒绝且不删除. Publisher只在自身`O_EXCL`成功且path仍指向所记录inode时清理自己的失败写入;abandoned unknown final由显式cleanup/GC处理.



<a id="single-root-imagesandbox-manifest-bundle"></a>

#### 6.5.3 Single-root image/Sandbox Manifest Bundle

`NewSingleRootBundlePublisher` 是已经组装好的 `RoleImage` 或 `RoleSandbox`
`sparse.Source` 的 typed named-location sink。数据流是：

```text
logical source
  -> Manifest ingest with fixed write admission/customer key
  -> deterministic identity pass to a streaming hash sink
  -> replay the fixed source with the same key, admission and encoding options
  -> exclusive-create content-addressed <root>.bundle
  -> reopen/strict validate
  -> file://<root>.bundle@manifest:<root>#<location>
```

source 支持重复读取。第一遍保留根 Manifest key、完整 Bundle 的物理摘要及格式元数据，编码后的 Chunk 字节随处理释放。第二遍直接写入最终 `<root>.bundle`，并核对生成根与预计算身份一致。一次 publish 只解析一次客户密钥，两遍共用相同 admission 与编码参数。

唯一创建的文件是最终 Bundle，包含这个 logical root 的 Manifest 和 Chunk。发布使用有界 Chunk 工作缓冲并保留 payload 稀疏性。typed 调用点提供逻辑角色；image 和 Sandbox reader 保持各自 schema 校验。复用已有目标时，验证 admission、根、认证内容与完整物理身份。并发 writer 通过有界等待、exclusive create 与完整校验收敛。取消、源变化、写入及关闭错误使发布失败，并仅清理本次拥有的不完整 final。路径被替换时保留替换者的 inode，返回所有权错误。

Tarstream 与 Bundle 都是 physical carrier，不是 logical role。Tarstream 包含单一
role-specific payload 与 sparse envelope，使用 `@digest`/`@hmac` identity；Bundle
包含 Manifest/chunk records、write admission与可选local encryption，使用
`@manifest` root identity。single-root Bundle publication不得先建立 tarstream，反之也
不得仅按 `.image`/`.sandbox` 扩展名推导 Bundle root。

因此named location只依赖`mkdir`、exclusive create、write、read、stat/fstat/lstat、权限设置、seek/pread、close，以及删除本进程拥有的不完整file. 它不依赖rename/renameat2、symlink、hardlink、reflink、sparse-file preservation、advisory lock或lock file,也不执行显式file/directory sync;publication的success contract由`O_EXCL`写入、`Close()`检查、最终路径reopen/full verification与路径身份检查定义,物理写回由文件系统或底层存储实现定义.

Active encrypted `.overlay.diff` 保持KDXTS格式. Export只读取decrypt后的BlockCOW SnapshotView并创建新的immutable logical artifact,绝不把ZIP追加到active diff.

<a id="manifest-upload"></a>

### 6.6 Manifest upload

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

### 6.7 Manifest Bundle

Bundle在pause前完成:

- write admission;
- portable dependency retention和located Bundle selector rewrite;
- unlocated local dependency materialization plan;
- current operation依赖集合;
- unlocated parent Bundle精确Manifest copy或remote fallback;located parent保留canonical selector;
- local tarstream dependency ingest;
- all ref replacements.

然后writer一次性emit metadata prefix,写入Manifest/Chunk,最后以E或S root key finalize. `FullVerify`使用完整expected Manifest集合. V1不以外层ZIP magic推断E/S.

<a id="publish-graph"></a>

### 6.8 Publish graph

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

<a id="artifact-commit"></a>

### 6.9 Atomicity 与 determinism

Portable YAML和E/S ZIP使用canonical order、fixed metadata和bounded bytes. Local `FileSink`/`BundleSink`保持same-directory temp、完整写入/`Close()`检查和atomic no-replace rename,final commit是O(1);alias只在root commit后更新. artifact capture/publication只定义logical completion,不定义stable-storage durability;两条本地路径都不执行显式file/directory fsync,物理写回由文件系统或底层存储实现定义. Named ref-location采用独立的exclusive-create + checked-write/copy-once + reopen-full-verify协议;tarstream由carrier直接提供identity,Bundle保持exact bytes,两者都不进入local sink的capture/commit路径.

多盘顺序固定为data disks first、root E last. Snapshot随后写memory S last. 这让S/E root成为可审计的graph commit point.

## 7. Memory restore 数据流

`run --restore S` 只处理真正的 memory Snapshot:

```text
T0 parse host config with presence
T1 open carrier and strict-parse S
T2 parse canonical snapshot.cfg
T3 open S.sandbox_ref and strict-parse E
T4 apply restore host-only rules to E config -> immutable lifecycle C0
T5 preflight kernel binding, compare runtime identity, and check disk sources
T6 atomically write run-dir C0
T7 bind self and reconstruct root/data disks from E
T8 create memfd and layer S.memory over S.from_refs
T9 create UFFD/va_report endpoints and CH restore argv
T10 start CH from config.json/state.json
T11 transfer UFFD descriptor(s) and complete restore handshake
T12 轮询 GET /vm.info，直到已恢复 VM 明确处于 Paused
T13 /vm.resume
T14 establish restore MUX and resume original process
T15 settle balloon/resource observation
T16 start pinger,forward,sensor and steady lifecycle
```

Cloud Hypervisor v51.1 会在 CLI 提交 `VmRestore` 前绑定 API socket，因此 socket
可连接不代表 restore ready。`vm.info` 的 Paused 检查是既有外部 `/vm.resume`
之前的正确性 barrier；只有该版本的结构化 VM-not-created 响应和 socket 尚未监听
可视为 pending。`timeouts.api_ready` 覆盖整个 barrier，包括连接成功但响应阻塞；
`timeouts.ch_api` 独立约束后续 resume 响应，`timeouts.restore` 独立约束 Guest
restore ACK 与 MUX 建立。这完成 #72 的 restore-readiness 正确性部分；消除 resume
往返仍是未来优化，并且需要兼容的 Cloud Hypervisor 版本。

Restore不会:

- 从 `snapshot.cfg` 读取 disk graph;
- 对 zero memory执行 cold start;
- 重跑 init/files/launch/plugin;
- 把 E的 `config.json` 当成新的 process launch request;
- 更新 memory parent或C0作为 re-snapshot side effect.

Host-only允许项包括 network provider/current identity、cgroup/controller、allocatable CPU/memory 与 resource enforcement、kernel/runtime actual path、active diff/template、restore prefetch和timeouts. Immutable disk graph、capacity identity、`deflate_on_oom`、network topology 由 E 拥有。Kernel 重哈希例外与 runtime-footer 比较见 [PortableSandboxConfig](sandbox_zh.md#portable-config)。

从 S0 restore后:

```text
C0 = E0 portable config
memory parent = S0

next snapshot:
  E1 = Export(C0,current disks)
  S1 = memory self + from_refs=[S0,...] + sandbox_ref=E1
```

从 explicit cold或E cold start后,memory parent为空. `snapshot --resume`不偷偷把 S1设为当前 parent.

### 7.1 Memory prefetch

`restore.prefetch: memory` 是 host-only optimization，预热当前 S self 的 file page cache 或 Manifest chunks，不包含 parent memory layers 和 disk streams。调用传入的是 opened root stream，因此范围也可能包含 S 的有界 ZIP metadata tail，并非仅 memory payload section。它不改变 sparse truth、fault ordering、C0、S或memory parents. 默认 `off`。Invalid configured mode 在副作用前验证失败；缺少 prefetch capability 或预取 I/O 失败属于 best-effort，不使 restore 失败，而是记录日志并继续 on-demand。异步任务在 stream 关闭前被 cancel 并 join。源码见 [prefetch.go](../pkg/restore/prefetch.go) 及 [restore.go](../pkg/restore/restore.go) 中的调用。

<a id="usage-generations"></a>

### 7.2 跨运行代次的 Usage

Cold 和 restore 使用当前 [Host usage 配置](#usage-policy)及同一 Host owner；[持久化契约](#usage-persistence)按逻辑 SandboxID 累计。恢复旧 S 不回滚 usage，新 SandboxID 的克隆不继承原实例文件。新运行保留累计端点，重建时钟与 Gauge 基线，不跨代次相减单调位置，也不从内存/文件系统占用中扣除恢复初值。

采样接入 Host 进程生命周期；Guest round 仅在 launch/restore ready 后开始，ctl.sock 存在不算就绪。捕获前 Host 关闭 usage 准入并断开 Gauge 连续性。Restore/attach 和捕获失败恢复遵循 [Guest 原始 usage 准入生命周期](sandbox-init_zh.md#usage-observations)，包括代次隔离、阻塞 source slot、ACK/thaw 回滚和 epoch 所有权。恢复的 balloon seed 不是新 actual 观测。

## 8. UFFD handler

Memory restore 使用一个共享 memfd，以及接收 CH-side userfaultfd 的 host handler。CH 可以为同一个 memory zone 报告多个 region，例如 x86 RAM 跨 PCI hole 分段；这些 fd 共用 handler 的 epoll set 和 address map。 S self及`from_refs`被打开为 layered sparse source;top resident data覆盖lower,top Hole向parent fall through,Zero仍是显式zero.

### 8.1 Single UFFD contract

这里的 single-UFFD 设计表示只在 CH mapping 上处理 fault，而不在 sandbox-ctl 的 backend mapping 上注册第二套 UFFD；不表示所有布局都只有一个 CH fd。CH 为 restored memory region 注册 missing-page events；host 通过 restore handshake 接收 fd，并按共享 memfd address map 验证 region 范围与不重叠。首个 report 启动 handler，后续合法 region 用 `AddUffd` 加入。超界 fault、重复/无效 region registration 或截断 source 均 fail closed。源码见 [serveandwait.go](../pkg/sandbox/serveandwait.go)、[handler.go](../pkg/uffd/handler.go)、[addrmap.go](../pkg/uffd/addrmap.go)。

### 8.2 Read path

UFFD使用`fault 1 page + best-effort serial tail`两阶段填充. Fault worker先保证
fault页完成,只有该页实际提交为`Loaded`后才能提交tail;每个handler最多保留一个tail
reservation,并发fault在reservation busy时只处理fault页. Fault页不等待tail worker或
tail ioctl,同时限制并发source I/O、共享buffer和guest population. `ChunkRun`为复用
一次物理解码结果,会在urgent copy前完成下述buffered source read.

填充上限按最终可见Run类型确定:

| Run类型 | fault阶段 | tail阶段 | 单次fault总上限 |
|---|---|---|---:|
| `Hole`、`Zero`、`Released` | 1页`UFFDIO_ZEROPAGE` | 最多15页 | 16页/64 KiB |
| ordinary `Data` | 读取并`UFFDIO_COPY` 1页 | deferred读取并填充最多15页 | 16页/64 KiB |
| manifest `ChunkRun` | 一次读取包含fault页的最终可见chunk窗口,先单独填充fault页 | 先填fault后的suffix,成功后再填fault前的prefix | 1个当前可见window，最多1 MiB/256页 |
| reclaimed `Loaded` | 只恢复当前页 | 无 | 1页 |

`ChunkRun`表示最终serving leaf是一个物理manifest chunk. Chunk 是解密、解压缩及启用时 ordinary content verification 的单位，因此 handler 不再把它按 ordinary Data 的固定窗口反复读取。 `SnapshotReader`
仍只返回从fault offset开始的forward anchor;`StreamSnapshotSource`通过包内可选
capability调用`fetch.ResolveChunkWindow`,以最终组合Stream的元数据把anchor扩展为
同一物理chunk中包含fault的最大连续可见窗口. 解析不读取payload;上层Hole透明,
上层Data或Zero、memory section末端以及不连续的同一lower chunk都会截断窗口,不能
直接使用绕过overlay的物理chunk边界.

Tail reservation在窗口解析及payload读取前取得. Busy时只按原anchor读取并填充当前
4 KiB fault页; 取得 reservation 后, handler 同步将不大于 1 MiB 的窗口读入已有 buffer.
必需源读取暂时失败时, 重新尝试整个窗口.
对完整可见chunk,这是一次精确的whole-chunk读取;若overlay截断可见性,source仍在
内部按 chunk 完成必要的解密、解压及配置启用的验证，handler 只接收可安全填充的连续窗口。 Fault
worker从buffer中间取当前页执行urgent `UFFDIO_COPY`,tail worker先用一个batch填充
fault后的邻接suffix;仅该batch完整成功时,再用一个batch填充fault前的邻接prefix.
Tail执行前从fault邻接页向外重检state,最多缩短为首次不匹配前的连续邻接段;
任一state冲突、partial completion或ioctl错误都会停止更远范围及后续阶段且不重试,
所以单个`ChunkRun`最多发出1次urgent和2次tail copy.

普通Data handler在worker启动前分配64 KiB共享buffer,具有chunk window capability的
manifest handler分配1 MiB,Cold `ZeroSource`不分配该buffer;fault和tail路径不扩容、
不创建临时payload buffer,每个fault worker只持有固定4 KiB urgent buffer. 这是 handler 自身的零新增 payload 分配约束；source内部的Run对象、cache lease及partial-chunk decode
allocation仍由`SnapshotReader`实现负责. 健康路径中, Handler对
非zero source只调用一次`SnapshotReader.RunAt`;可选chunk window resolver仅在
accelerator内部继续执行metadata `Stream.RunAt`. Ordinary Data和zero-like run仍分别
受64 KiB state boundary约束. `ChunkRun`双向候选只包含完整页,并在一次state读锁扫描中截断于
fault两侧连续`PageState`、RAM末端及当前CH UFFD region;这些guest population边界不
缩短已经获准的whole-chunk source read. 默认 CDC chunk 的最大值是 1 MiB，但 Manifest 配置可以在 decoded-format limit 内使用更大 chunk；UFFD buffer/window cap 仍为 1 MiB。扩展 window 不可用、无效或超限时保留原来的安全 forward anchor；buffered window 放不下时，只推进 urgent page，不分配更大 buffer，也不引入超大 chunk 专用 population 路径。源码见 [fault.go](../pkg/uffd/fault.go) 和 accelerator [chunker 配置](https://github.com/kuasar-sandbox/accelerator/blob/main/pkg/manifest/chunker/chunker.go)。

Tail对其连续有效范围只发出一次multi-page `UFFDIO_COPY`或
`UFFDIO_ZEROPAGE`;`ChunkRun`的suffix和prefix各自最多一次. Kernel按页处理并可能
返回已完成的page-aligned prefix;handler只把该prefix条件提交为`Loaded`,第一次冲突
或错误后放弃剩余tail且不重试. 因此batch保留内核已完成的成功前缀,同时把普通
16页策略的tail降为1次ioctl、完整可见 256 页 window 的 255 页 tail 降为最多 2 次 ioctl。

`EVENT_REMOVE`使已丢弃范围重新成为missing,tail提交使用条件状态更新,不能覆盖并发
产生的`Released`. Context cancellation停止worker并关闭fd/stream owner.

UFFD 直接重试必需的 `Run.ReadAt`. urgent 页及包含它的 Chunk window 属于必需读取. 纯 speculative tail 只执行一次最佳努力尝试. 保留现有 `ChunkRun` 能力、buffer、tail 预留和 ioctl 部分进展/EEXIST/EAGAIN 收敛. 延迟读取后, urgent 安装重新检查页状态; 已观察到的 REMOVE 使旧数据失效, 此时使用现有零页收敛, 并且不再从过期计划安排 tail.

共享[源读取恢复策略](#read-recovery)保留操作 context 和首个终态原因。

## 9. cgroup 与 balloon

### 9.1 Memory enforcement

Static/dynamic cgroup模式在CH pause前lift可能竞争的`memory.high`,并等待已有high事件drain. Snapshot/export recovery恢复原值. VMM cgroup只承载CH进程,不把`sandbox-ctl`自身算入workload Budget.

### 9.2 CPU

`capacity.cpu` 决定 vCPU count. `allocatable.cpu` 必须是有限正数且不大于 `capacity.cpu`;在 cgroup 模式映射到 clamped `cpu.weight`。这是相对 contention policy，不是硬性的 fractional-core quota；VMM 的 `cpu.max` 以 capacity 为基础。Controller 可以在lifecycle内调整grant,但不会改写C0或artifact.

### 9.3 Balloon

Balloon mutation与snapshot/export通过同一barrier序列化. Freeze window不会与async balloon resize并发. Restore以CH state为真实current,再进入新的observation epoch;不会用host YAML重建已恢复balloon瞬时值.

Sandbox-local `MemoryController` 将一份已接收的 Guest ABI `mem_report` 与 CH `vm.info` 合并。Guest 在 cold-launch barrier 后立即报告，此后默认每 5 秒报告一次；epoch/sequence、重试和 quiesce 协议由 [Guest ABI](sandbox-init_zh.md) 定义。`Capacity` 取自 host 权威的 CH memory 配置，不能从 Guest `MemTotal` 反推。CH `config.balloon.size` 是 `AcceptedTarget`，`memory_actual_size` 是 `CurrentBudget`，因此 `BalloonCurrent = Capacity - CurrentBudget`，`TargetBudget = Capacity - AcceptedTarget`。`DesiredTarget` 是独立的本地意图。安全观测 Budget 为 `B = max(TargetBudget, CurrentBudget)`。精确相等只作为 `TargetReached` 诊断，不是收敛或结算门槛。`vm.resize` 成功仅表示 target 已接受，不证明 current 进展。

本地计算为：

```text
DemandMemory = max(CurrentBudget - MemAvailable, 0)
RawRequested = min(Capacity, saturating_add(DemandMemory, AllocatableMemory))
DesiredTarget = align_down(Capacity - RawRequested, 64 MiB)
RequestedBudget = Capacity - DesiredTarget
```

Guest 诊断字段与 host `memory.current` 不决定 Budget；host `memory.current` 仅作为 `memory.high` 的安全下界。

增长先预留 Budget，再提高所需 `memory.high` 额度，随后通过 `PUT /api/v1/vm.resize` 降低 balloon target，并以 `vm.info` 确认接受。Node 的 partial grant 必须累积到可表示且不超过 reservation 的 Budget，不能取整产生未预留的内存。通过 lifecycle barrier 后，即使 target/current 尚不稳定，安全增长仍可执行。Reservation、high 或不明确的 resize 失败保留向前增长目标并重试，后续较小报告不会隐式回滚它。

收缩要求 fresh、未被更新报告取代的观测以及已知的 CH target/current。每份报告最多允许一个 64 MiB inflate step，并保留一步 deadband。新 target 必须大于 accepted target，且同时不超过 desired target、capacity、`AcceptedTarget + 64 MiB` 与 `BalloonCurrent + 64 MiB`，计算采用 checked 或 saturating 算术。Resize 前须在 mutation gate 内重新检查 CH 和这些边界；actual 反向变化或 accepted target 改变会使候选失效。Target 操作确认后，`B < Reservation` 时按 B 降低 `memory.high` 并把 B 作为绝对 reservation baseline 提交；`B == Reservation` 不重复提交；`B > Reservation` 时既不返还，也不把 actual 移动当作 node grant，而是交给既有 demand/pressure grow 路径。因而部分观测进展无需 target 相等即可结束本轮，后续报告继续处理进展。更新的已接收报告、不可用的 CH 状态以及 high/reservation 失败都会阻止过早返还。未完成 grow 保留其 reservation；部分提交后的 grow 从最新 baseline 请求差额。Shrink 响应丢失时，无论 node 是否已提交，已发送的较小 baseline 都是保守恢复值。Mutation gate 覆盖 CH 与 `memory.high` 变更，但不覆盖随后的纯 node commit RPC。Guest 应急 deflate 不能绕过这些条件。

源码见 [memory.go](../pkg/resctl/memory.go)、[memory_controller.go](../pkg/resctl/memory_controller.go) 和 [balloon.go](../pkg/resctl/balloon.go)。稀疏 `PUNCH_HOLE`/`MADV_DONTNEED` 与 hole-only skipping 仍由 [VMM patch 0004](cloud-hypervisor_zh.md#34-0004--balloon-release-跳过-user-managed-zone-的空洞-run) 定义。

## 10. Resource protocol

### 10.1 Lifecycle states

```text
prepare -> admit -> start -> ready -> steady
                  \-> freeze -> capture -> resume|destroy
restore -> resume -> settle -> steady
```

Controller lease、heartbeat和recovery inventory只管理host resource ownership,不进入Portable config.

### 10.2 Capture barrier

Snapshot/export开始前阻止新的Budget mutation和balloon transition. 已在进行的mutation完成后才能freeze. Recovery必须在guest/app可继续运行后释放barrier,否则会出现resume后永久失去resource updates的liveness bug.

### 10.3 Pressure sensor

Static/dynamic模式可使用PSI或`memory.events.local` polling. PSI默认trigger与debounce由host config决定,不写入E. Sensor在capture gate期间停止发起growth,restore后建立new observation epoch.

## 11. vhost-user-blk backend

### 11.1 Payload boundary

Vhost backend接收`fetch.Stream` Payload section,不是FullStream. 任何 `.sandbox` ZIP tail进入block logical size都属于bug,由format/unit tests覆盖.

### 11.2 Layered reads

Read 顺序为 active diff -> captured top -> `base_from_refs` -> root image（适用时）。Hole fall through，Zero/Data stop traversal。同一 writable block device 中组合的 immutable layers 的 logical size 必须一致。EROFS + ext4 拓扑中，read-only EROFS base 是独立 vhost device，不是 writable ext4 BlockCOW device 最后一层 fall-through。源码见 [disks.go](../pkg/sandbox/disks.go) 与 [serveandwait.go](../pkg/sandbox/serveandwait.go)。

`StreamReader` 同步重试完整读取. 运行队列 context 仅在本次调用替代准备阶段 context, 因此旧 master session 停止时先取消挂起读取, 再 join worker 并替换 memory table. COW 适配层将该 context 传入 base materialization, 同时保留块锁、dirty bitmap 和部分写算法. 不重放整个 `WriteAt`、`FLUSH`、Store `Put` 或快照操作.

`processChain` 和 `processQueue` 都识别停止或终态的必需读取. 此时 status byte、used ring 和队列 base 保持未完成. COW 写入内部的基底读取同样遵守该规则. 普通不支持请求及独立的可写 diff 错误保留现有协议行为.

共享[源读取恢复策略](#read-recovery)保留操作 context 和首个终态原因。

### 11.3 SnapshotView

CH pause 且前台 quiesce 后，`SnapshotView` 复制逻辑 bitmap 并提供 upper-only 明文，
优先读 dirty/writeback 缓存页，再读文件。不强制 Drain/fsync，也不复制整份缓存。
后台可继续搬运相同逻辑内容。只有文件成功完成后，最后一份缓存副本才能淘汰。捕获完成前
保持前台 quiesce 且 COW 打开。捕获期间后台 fatal 必须使发布失败并到达 runtime owner，
包括故障后没有新 guest 请求的情况。Export 不 rotate active diff 或改变 base。

<a id="cow-backend"></a>

### 11.4 BlockCOW 状态与 I/O

活动可写 diff（包括 tmpfs 上的文件）通过同一定位 I/O API 请求 O_DIRECT。
运行时策略不识别底层文件系统类型。
`sandbox-ctl` 为每个沙箱持有一份有界明文页缓存；root 与数据盘共享预算，包括混合使用
tmpfs 与磁盘的情况。这实现了 [issue #230](https://github.com/kuasar-sandbox/sandboxer/issues/230)，
并由 [issue #238](https://github.com/kuasar-sandbox/sandboxer/issues/238) 恢复 tmpfs 兼容性。
改动范围是活动数据访问、配置与生命周期；immutable base、template、历史 overlay、manifest
解密缓存、制品输出、tarstream 表示、cgroup、balloon 和 guest 资源预算保持现有行为。

#### 11.4.1 逻辑状态与记账

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
后才应答写入。回写成功或淘汰不清除此 bit。重开按现有格式从稀疏文件 extent 重建 upper
归属。缓存持有领先于物理文件状态的最新明文。



#### 11.4.2 读写与背压

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



#### 11.4.3 回写与 FLUSH

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
features；不增加 CONFIG_WCE 或 wire discard。

写入完成仅承诺完整最新字节已进入有界逻辑 COW 状态。写入与 FLUSH 都不保证
`sandbox-ctl` 崩溃、宿主断电或存储故障后的数据存续。这明确不同于
[Virtio 1.2 §5.2.6.2](https://docs.oasis-open.org/virtio/virtio/v1.2/virtio-v1.2.html)
要求的稳定存储 FLUSH。这是项目的非持久化 COW 契约，不是标准持久化 FLUSH 行为。
Direct I/O 本身也不是持久化屏障。



#### 11.4.4 活动文件 I/O 契约

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

### 11.5 Quiesce / Resume

Quiesce等待in-flight block request退出并阻止新request. 所有data/root views在同一quiesce窗口读取. Recovery顺序先恢复backend可服务状态,再恢复CH和guest连接,避免VM恢复后block request永久阻塞.

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

## 12. Validation、错误与安全

### 12.1 Cold validation

普通 cold config至少验证:

- capacity/allocatable和resource mode一致;
- kernel/runtime为absolute unlocated file binding;
- network provider唯一且identity字段有provider;
- root/data topology合法,数据盘name/order和mount 1:1;
- active diff/template/size组合可生成mountable filesystem;
- local/Manifest/Bundle ref与crypto policy一致;
- launch/files/init/plugin/metadata limits.

### 12.2 `run --from` 与 restore validation

两种模式都先严格parse logical artifact和canonical config,再验证host ownership. `--from`允许persistent workload override;restore拒绝所有cold-only字段. Kernel binding preflight、runtime identity comparison、network topology/provider、disk count/name/topology 和 active diff binding 检查在 external lifecycle side effect 前完成；这不增加两种模式的 kernel digest 重哈希，见 [PortableSandboxConfig](sandbox_zh.md#portable-config)。

### 12.3 CLI mutual exclusion

- `run --from` 与 `--restore` 互斥.
- `run --replace-boot` 仅允许与`--from`一起使用，要求host config，并拒绝`--restore`.
- export/snapshot都要求`--output`与`--upload`二选一.
- Explicit `--mode` 与 `--upload`互斥.
- Live export拒绝`--config`;image-to-Sandbox-E assembly 要求 `--from` 加 `--config` 或 `SANDBOX_CONFIG`，且拒绝 `--resume`。
- Snapshot没有memory toggle.
- `exec` command必须位于`--`之后;local与proxy target rules互斥.

### 12.4 Failure contract

- Error包含field/entry/ref/disk index context,但不打印inline file/env value、customer key或plaintext digest.
- Local crypto错误保持protected presentation.
- ZIP/path traversal/symlink/regular-file checks fail closed.
- Remote I/O 接收 operation context；取消遵循 owning I/O implementation，并不表示已提交对象被回滚。
- Stream/fetcher/Bundle reader共享明确close owner,失败路径不泄漏fd/mmap/goroutine.
- Freeze后的任一failure必须恢复app、CH/backend/MUX/pinger/forward并释放resource locks;若MUX恢复已不可证明,必须在locks仍持有时终止CH并将capture gate置为terminal.
- Operation root 成功发布是 commit point；local output 随后更新 semantic alias，named location 没有 alias。Dependency orphan 不伪装成功。

<a id="cow-validation"></a>

### 12.5 COW 验证

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

#### 12.5.1 可重复的 COW 测量方法

历史运行保留在原始 Git commit 和 [COW 工作](https://github.com/kuasar-sandbox/sandboxer/issues/230)中；实测 revision 表不是维护中的规格。重复比较时记录 revision、kernel/filesystem/storage、缓存配置、加密、workload 和前置条件。文档中的方法不代表已经在当前 revision 执行。

使用现有 `blk_cow_workingset_bench_test.go` working-set driver，并为每个 revision 提供匹配 adapter。没有 sandbox cache 的 buffered baseline 使用 no-op cache drain/close，不提供 cache statistics；请求完成不代表介质持久化。分别记录候选实现的准入和最终 Drain：

```bash
go test -tags no_rocksdb ./pkg/vhost -run '^$' \
  -bench '^BenchmarkCOWWorkingSet$' -benchtime=1x -count=1 -timeout 15m
```

使用 256 MiB 逻辑数据集，为 32 MiB cache 的八倍：顺序 1 MiB 写；跨 65,536 页的确定性随机 4 KiB 和 512-byte 更新；90% 落在 4 MiB、10% 落在其余范围的热覆盖；以及预填充读取。512-byte workload 传输 32 MiB，但物化 256 MiB 页面。热写/读取的预填充排除在计时外。两个 128 MiB 磁盘交替发出 1 MiB 请求，验证 sandbox 共享预算。覆盖明文和 XTS。这是单前台 backend driver，不是 Guest 测量。

排除 fixture/setup，测量含额度等待的请求分位数、逻辑吞吐和包含 Drain 的总耗时。Buffered baseline 完成表示 kernel 接受；预填充读取可能命中 kernel cache。不得用 fsync、DONTNEED 或 drop_caches 制造缓存驻留结果。统计活动文件 body 读写次数和字节；`/proc/self/io` 是文件系统记账，不代表持久介质写入或自动证明写放大。mincore 观察活动文件驻留时不触碰映射页，逻辑 cache/dirty 峰值与 heap/RSS 分开报告。

现有 `cow_direct_workingset_bench_test.go` 提供无缓存的 direct-I/O 对照，使用相同 active-I/O 和 XTS 规则，不是可选择的生产模式：

```bash
go test -tags no_rocksdb ./pkg/vhost -run '^$' \
  -bench '^BenchmarkCOWDirectWorkingSet$' -benchtime=1x -count=1 -timeout 15m
```

Direct-I/O 预填充分离 cache 读取和温热 kernel file cache 的影响。热路径检查覆盖 nil/background/cancelable context 和收到信号的 waiter。确定性测试验证固定批处理 deadline、pressure/Drain 绕过等待、backlog 时无每批延迟、最老 dirty 进展、小 cache 下有界相邻读取、cache/base/hole 混合、重叠/取消、sub-page 和 short-read 边界，以及与 Guest 修改输出 buffer 隔离的 copyout。

实际 Guest 验证使用现有 owner E2E 的 diff template、encrypted diff、snapshot/restore 和 data disk 用例，配合 `REQUIRE_KVM`、私有 `TMPDIR` 及匹配的 `BIN`/native/runtime 输入。每项缺失前置条件均记录为缺少验证。Guest probe 可先用 1 MiB 请求写入 64 MiB 范围，再预热并对前 4 MiB 执行 4,096 次对齐的 4 KiB 覆盖，使用 mmap-backed buffer 和 O_DIRECT，核对字节数和 payload。Guest descriptor 分段不等于 1 MiB Host batch 测量；没有 Drain/fsync 的准入计时不代表完整回写耗时。原始运行结果保留在原 PR/Issue 或不可变 Git 历史中。

<a id="usage-validation"></a>

### 12.6 Usage 性能与验证

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
ENOSPC, 并用限定 usage 路径的 `strace` 注入写入 EIO 和延迟写入; 还覆盖
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
中继在仍暂扣迟到/分段响应时观察 peer EOF, 以 Host 关闭连接而非下一请求
起点验证读取 deadline. 原 Host 读取或 Guest 准入槽退出期间, 中间一个 tick
可以按设计继续缺测; 新连接恢复另行验证. Live 查询证据保留每项指标 request ID 的变化,
并分别对应各自的原始请求, 不假定整轮归并为原子发布.
未注入故障的 root/第二文件系统必须独立返回正常原始数据, 且各自的 request
身份和覆盖时间继续推进; 两端同为失败或保留旧成功值都不能通过.
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
成功捕获前, 同一案例在不可变依赖预检查完成后耗尽测试自有快照输出 tmpfs.
测试要求 Guest quiesce/CH pause 之后的数据盘捕获 ENOSPC、MUX 重连、真实
health exec 和 CH `Running` 状态. 此次捕获失败回滚必须保留已占用槽及其
已知用量, 同时健康来源继续推进. 输出限制不影响用量文件目录; 更早的依赖
错误不能算作回滚证据. 成功和失败路径都会卸载临时挂载.
这些案例结合重复操作和生命周期回归, 验证来源隔离、有界所有权及失败收尾.
报告应注明实际观测窗口; 它们不是物理掉电实验.

在没有其他并发测试负载时, 以 root 权限和已组装的 `BIN` 运行
[off/on 测量脚本](../test/e2e/usage_perf.py):

```bash
python3 test/e2e/usage_perf.py --densities 1,4 --seconds 30 --repeat 3
```

基线测量原生进程 CPU、RssAnon/RssFile、FD/thread、非 dead 的 Go G 数量、
实际记录字节数、集中停止耗时和端到端 exec p95/p99. Go G 包含 runtime 系统
goroutine, 不等于 `runtime.NumGoroutine`. 精确二进制 DWARF/symbol 检查需要
`gdb` 和 Go tools. 诊断产物保留原生 proc stat 文本、独立的 utime/stime/
guest_time 端点、读取窗口、boot identity 和 tick 尺度.
`proc-observations.json` 在采样失败时仍保留已完成的读取; 不完整的配对/窗口
明确标记, 不伪造缺失端点. 这些测试产物与紧凑的 usage 文件独立.

可选诊断可以帮助分析基线, 不是独立的性能验收目标, 也不是本功能合入的前置条件:

```bash
python3 test/e2e/usage_perf.py --densities 1,4 --seconds 30 --repeat 3 --trace
```

此可选 Linux amd64 跟踪运行需要具备指令偏移支持的
`bpftrace` 构建、tracefs 权限及 initial PID namespace (`BPFTRACE_BIN` 可选择
已安装工具). 预检查在挂接目标探针前拒绝 kernel 与 `/proc` PID 身份不一致的
环境; 它核验自有短生命周期子进程的真实 sched exec 事件, 不只检查语义随
bpftrace 版本变化的裸 `pid` builtin. 空结果或失败不能解释为零开销.
跟踪补充唤醒、mallocgc 请求/请求字节、
usage framing 通信、CH info 请求、
有争用的 API mutex 等待及保存 worker 耗时. 普通指令探针按精确二进制核验,
不插入 Go 返回跳板. 跟踪会扰动时序, 其延迟不能替代无跟踪基线. 脚本检查可用
内存再准入密度, 不修改宿主资源上限.

Off/on 对比必须使用相同源集、runtime/kernel/CH、配置、密度及负载. 记录这些
输入, 报告空闲、忙 CPU、内存压力、多盘和保存负载下的 Guest/Host CPU、
各进程 RssAnon/RssFile、FD/goroutine、实际记录字节数、集中停止耗时及业务
p95/p99. 这是本功能的描述性验证, 不增加 SLO 或生产密度认证.

唤醒、分配、管理通信、CH API 次数/锁等待及单次保存耗时属于可选诊断.
无法获得的测量明确保留为未测, 不记作零或通过; 取得特权 profiling 环境不是
合入前置条件. 功能正确性、来源/writer 所有权有界、生命周期和故障回归, 以及
仓库正常 review 和精确集成 CI 仍然必需. 单元模型和普通进程实验不能替代
CH/KVM 证据. PR 和证据产物保留实际测量的提交、环境和限制,
本文不从设计推导虚构 benchmark 数字.

<a id="read-recovery-validation"></a>

### 12.7 源读取恢复验证

回归覆盖 ring/completion 不变量、COW materialization、EOF/EAGAIN 包装、后端取消、仅成功初始化缓存、队列/冻结关闭, 以及真实内核 REMOVE/COPY 的页内容. owner E2E 进一步使用匹配的组件二进制、runtime image 和 native 依赖验证 CH/KVM 行为. 前置条件缺失代表缺少验证, 不代表测试通过.

## 13. Reliability、performance 与兼容边界

### 13.1 Streaming 与 memory use

- Sparse tarstream不spool logical stream到disk.
- Manifest ingest只读取resident extents.
- E/S 大 payload 不暂存到 per-run `/run` directory；CH 有界 config/state staging files 放在该处。调用者选择的 output directory 属于独立路径，并占用所选文件系统容量。named-location Bundle publication 通过流式预计算身份，仅写入最终内容寻址文件。
- Bundle dependency plan在pause前执行remote I/O和admission.
- V1 `--resume` 可以在sink写完整期间保持VM paused,换取SnapshotView稳定性.

### 13.2 Performance observations

关键指标:

- preflight latency;
- guest freeze ack latency;
- CH pause latency;
- disk/E emit time;
- CH memory snapshot与resident memory emit time;
- resume/reattach latency;
- Manifest stored/dedup chunks;
- UFFD fault latency和restore-ready latency.

Benchmark分别覆盖local tarstream/Bundle create、Bundle read、sparse merge和UFFD fault. 性能优化不能改变Hole/Zero/Data、commit order、identity verification或freeze safety.

<a id="read-recovery"></a>

### 13.3 同步源读取恢复

`sandboxer` 在 `fetch.Stream` 和 `sparse.Run` 之上唯一负责同步重试. 文件、Manifest Bundle、cache 和 Store 来源使用同一策略. `accelerator` 每次只执行一次业务读取并保留错误链; 后续调用可以从同一个已选来源恢复.

必需运行读取等待时保留原请求、buffer 和 inflight 所有权. 暂时失败不会完成 Guest 磁盘请求, 也不会填入缺失内存页. 必需读取不可恢复时, VM 所有者终止整个 CH 进程和沙箱, 并保留首个读取原因.

该行为覆盖冷启动, 包括 `run --from manifest://...`, 以及内存恢复和随后的磁盘、内存读取. 只读制品打开和元数据检查也使用同一 helper. 非法命令参数向调用者返回错误. 管理输入错误或可选预取失败不会通知运行 fatal.

不增加命令、重试次数参数或恢复模式. 操作由现有 context 或沙箱关闭结束. 来源停机可能延迟就绪、exec 或快照排空, 因为这些操作可能需要尚不可用的数据.

退避为内部固定策略: 从 10 ms 开始, 每次失败后翻倍, 上限 1 s; 每次实际等待均匀取该档位的 50% 到 100%. 等待观察操作 context. 不设置次数或累计时间耗尽后将挂起读取转为 Guest IOErr 的机制.

后端 socket/RPC deadline 仍只约束单次尝试. 后端内部取消或超时不会结束仍存活的操作. 已配置的健康策略保持独立; 读取恢复不屏蔽健康检查、不重启服务、不改选 endpoint. 每个 `ProcessStorage` 的 customer key 保持固定, 包括首次密钥解析错误.

`internal/readretry` 只返回终态原因, 不杀进程. 未标注访问错误保守重试. `accelerator/pkg/readerr` 提供最小的 `Retryable() bool` 标记和 `Unwrap`. 明确永久原因包括已验证的几何错误、完整帧结构错误、必需查找确认的不可变对象缺失, 以及已确认的内容、格式和认证失败. 自定义 decryptor 和传输失败保留实际原因, 不仅按操作名称分类.

每次尝试可以改写同一个 buffer, 但后续尝试重新读取整个请求范围, 不拼接部分响应. 并行后端读取在返回前 join 全部 worker; 派生取消不能遮蔽原始原因或随后发现的永久原因. 合法完整读取附带普通 EOF 时保留原合同. EOF 或 EAGAIN 的终态包装先于兼容和补零分支检查.

只读覆盖包括 location 来源验证、EROFS 构建前缀探测及合并父层的 sparse 元数据. 这些边界只重试单次来源读取, 不重放发布或捕获写入. Merge/seeker 适配层和 Ext4 校验保留终态错误, 即使失败尝试已填满请求 buffer. ZIP 元数据解析在本次解析中保留来源首个终态, 因为库内 helper 可能丢弃完整读取附带的错误. 制品格式探测遇到该原因立即结束; 来源数据缺失不能当作可选镜像配置缺省. 普通格式探测和可选配置缺省保留原行为.

进程惰性 Fetcher 初始化、引用 Bundle 解析和 Bundle Chunk 索引准备只缓存成功结果. 首次失败或取消后, 后续调用仍可尝试. 所有者关闭后, 初始化或资源发布不能使对象复活. Manifest 来源选定后, 数据读取失败仍局限于该来源.

健康路径的 retry helper 不分配 timer, 不启动 goroutine. 失败的同步读取持有一个复用 timer 及原有请求/buffer. 单次退避有上限, 但操作可以无限期保持挂起. 连接额度包含空闲、借出和拨号中的连接; 有界维护 worker 不随停机时长增长. 验证覆盖健康路径分配/时延对比和故障期资源检查，不构成独立性能认证。

后端 marker 契约仍由 [Accelerator 读取错误](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/accelerator-read-recovery_zh.md) 维护；另见原始[提案与验收清单](https://github.com/kuasar-sandbox/sandboxer/issues/225)。

#### 13.3.1 Fatal 所有权与捕获

worker 在释放 inflight 所有权前报告必需读取 fatal; 后发生的 queue stop 不能遮蔽后端已经返回的永久原因. `ServeAndWait` 记录首因, 取消相关等待和 PostSpawn/握手工作, 然后直接杀死 CH. 现有唯一 `cmd.Wait` 负责回收和输出排空. worker 不同步等待自己的清理. Ready 提交和 fatal 登记共用一把锁, 已登记 fatal 会阻止新的 Ready 状态转换. 已提交 Ready 事件的通知在锁外交付; 通知回调阻塞不会延迟 fatal 取消或 CH 终止.

快照和 export 保留冻结、排空和一致性条件. 重试可以延长排空, 不减少 inflight 来通过冻结. 捕获在提交前和成功返回前检查操作是否结束; 无法满足捕获条件时失败. 队列停止先取消读取及快照 gate 等待, 再 join; 旧 worker 不能向替换后的 master memory table 写入. UFFD 队列投递也观察取消. 关闭仍先结束 reader, 再让 remove flusher 最终 drain, 最后回收资源.

<a id="artifact-compatibility"></a>

### 13.4 制品兼容边界

本切换直接替换旧snapshot provenance. 旧`SnapshotConfig`若包含resource/runtime/root/data/launch字段会返回明确unsupported error. 不提供 old-format alias、dual reader、migration shim、feature flag 或 zero-memory compatibility path。[子命令总览](sandbox_zh.md#21-子命令总览) 的 `upload-snapshot` CLI alias 不提供格式兼容。

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


S/E 后缀几何、确定性 ZIP 编码、有序角色条目、大小上限及 sparse prefix/append 视图由 `accelerator/pkg/tailzip` 实现。Sandbox/Snapshot 模块保留 portable config、JSON/state 和设备拓扑校验。逻辑引用位置共用 `manifest.RefLocations`。

## 14. See Also

- [sandbox-init_zh.md](sandbox-init_zh.md) — guest PID 1、launch/quiesce/MUX 协议。
- [cloud-hypervisor_zh.md](cloud-hypervisor_zh.md) — CH build、API 与 restore 边界。
- [Connector TAPFD 协议](https://github.com/kuasar-sandbox/connector/blob/main/docs/tapfd_zh.md) — TAP descriptor handoff 与 network namespace，由 connector 维护。
- [timeouts-production.yaml](../examples/timeouts-production.yaml) — production host timeout 示例。
- [restore-prefetch-memory.yaml](../examples/restore-prefetch-memory.yaml) — explicit memory prefetch 示例。
- [README_zh.md](../README_zh.md) — build、release 与 repository 入口。
