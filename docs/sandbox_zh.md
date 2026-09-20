[English](sandbox.md) | [简体中文](sandbox_zh.md)

# sandbox — 沙箱控制与制品生命周期

`sandbox-ctl` 是 kuasar-sandbox 的单沙箱 host 控制面. 它负责显式冷启动、从 Sandbox 制品冷启动、内存恢复、live export、无 VM 的 image-to-Sandbox-E assembly、内存快照、制品发布以及运行期 `exec`/forward. Guest 侧协议见 [sandbox-init_zh.md](sandbox-init_zh.md)。

本文只描述当前格式和行为。当前 reader 拒绝旧 `snapshot.cfg` 磁盘图 schema，不提供双读、自动迁移或跨版本兼容保证；这一格式边界不表示项目从未发布版本。

## 1. 概述

<a id="工件模型"></a>
### 1.1 工件模型

运行时消费 image、Sandbox E 和 Snapshot S。E 描述便携沙箱配置及磁盘图，S 关联 E 与捕获的 VMM/内存状态；逻辑对象与文件/命名位置/Manifest/Bundle 载体是独立维度。完整角色、字段和验证规则见[沙箱工件](sandbox-artifacts_zh.md)。


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

`--drop-caches` 只属于 memory snapshot,默认 false. `--merge-ref` 只控制 local memory parent merge,不改变 disk provenance.

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

发布入口从本地路径、located ref、Bundle selector 或 Manifest ref 识别 E/S 逻辑根。普通发布保留 portable 依赖；引用替换使用可重复的 `--replace-ref OLD=NEW`，整链归并使用 `--reduce-ref A=X`、`--reduce-ref A` 或 `--reduce-ref=any`。替换规则之间的源/目标集合、替换与归并的作用范围均须互不重叠，归并范围包含全部原始 lower 和目标；`any` 与替换规则互斥。Snapshot 发布同时处理当前 E 的磁盘引用，并在 E 发布后更新 `sandbox_ref`。`--skip-verify-ref` 默认 false，优先采用可信可比身份，否则流式验证稀疏内容；skip 模式继续校验新输入身份和 schema。详细语义见[制品发布](sandbox-artifacts_zh.md#引用改写与整链归并)。

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

`--json` 返回最终发布报告：Sandbox 恰好包含 `sandboxRef`、`removedRefs`；Snapshot
额外包含 `snapshotRef`，其中 `sandboxRef` 是 S 实际引用的最终 E（含 Bundle 绑定）。
空差集为 `[]`。默认输出仍是一行根引用；`upload-snapshot` 别名和 `--quiet` 共用此契约。
本地引用只有 basename 并保留既有身份后缀，调用方在外部提供 checkpoint 目录。
差集是已知原拓扑减去最终保留引用，包含本地旧根，但不授予删除权限。skip 边界、共享引用、
路径隐私和零额外 I/O 约束见[发布结果报告](sandbox-artifacts_zh.md#发布结果报告)。

```bash
sandbox-ctl publish --json --quiet --to-ref-location release=file:///srv/sandbox-artifacts ./s1.snapshot
```

### 2.9 `sandbox-ctl usage`

```bash
sandbox-ctl usage --sandbox-id s1
sandbox-ctl usage --sandbox-id s1 --saved
sandbox-ctl usage --sandbox-id s1 --offline --history --limit 10
```

该只读命令返回已有 live/saved 用量或有界历史. `--path-id` 选择既有
RunDir/BaseDir, 逻辑 SandboxID 标识用量文件及记录. 查询不触发 Guest/CH
采集或保存. 完整参数、JSON 单位、有效性及文件格式由 [usage](usage_zh.md) 统一定义.

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

### 3.3 Usage policy

`usage` 是严格解析的 host-only policy. 在 cold/from/restore 中遵循配置顺序
覆盖, 不进入 Portable/E/S. 默认关闭不增加 sampler、usage 长连接或周期写盘,
既有资源控制仍工作, 旧文件仍可离线读取. 参见
[usage 配置](usage_zh.md#3-配置) 和 [host overlay 示例](../examples/usage-enabled.yaml).

## 4. 资源模型

### 4.1 三种部署模式

| Mode | `cgroup_path` | `controller` | Behavior |
|---|---|---|---|
| No cgroup | empty | empty | 不写 cgroup；allocatable CPU 必须等于 capacity CPU，allocatable memory 必须处于 capacity 范围内 |
| Static cgroup | set | empty | 本地设置 CPU/memory limit,可运行 local sensor |
| Dynamic | set | set | 通过 resource protocol admission/lease/Budget,并运行 local sensor |

`resources.capacity` 是 guest-visible VM capacity,进入 Portable config. `resources.allocatable` 是 cold start 的 workload 默认值,也进入 Portable config;其中 `allocatable.cpu` 必须是有限数,且满足 `0 < allocatable.cpu <= capacity.cpu`. Restore 保持 E 中的 capacity 和已捕获的 `deflate_on_oom`,但可以从目标节点显式重新应用 allocatable CPU/memory;该运行时 policy 不改写 E 或 C0. `control`、`overhead`、`watermark_high` 和 `startup` 是 node policy,不进入 E.

Export/snapshot 获取 MemoryController mutation barrier,并在 freeze 前 lift/drain 可能与 CH pause 竞争的 `memory.high`. Host ping gate 会让已入场探测完成 `pong` + guest EOF transport barrier;该排空独立受 8 s quiesce budget 约束,即使普通 `timeouts.ping` 关闭强制超时也不会无限阻塞捕获. 到期时 host cancel并join该探测,捕获失败后走完整 recovery. Guest quiesce 还会排空并暂停周期 `mem_report`,防止 S 捕获持有 stream lock、仍等待旧 host vsock 的 reporter. Restore 在 ACK 前切换到新 observation epoch;`--resume`/失败 attach 恢复原 epoch,live attach只重开pause gate且不破坏已入场报告的计数. Recovery 在 VM、MUX、app 和 backend 恢复后释放 host barrier.

`resources.diff_cow` 是不进入 E 的 host 策略，为 root/data 活动 diff 提供一份共享有界明文缓存，与 guest 内存及 VMM overhead 无关。该缓存由 tmpfs 与磁盘上的 diff 共享，不限制 tmpfs 文件占用的内存/swap。配置、非持久化 FLUSH 和文件系统要求见[完整契约](diff-cow-cache_zh.md)。

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

可预测的 config/ref/format 错误应在 preflight、controller/network/VM 副作用前失败。这不保证后续操作全部无副作用：创建 run directory、写 C0、创建 diff 和获取资源是可能失败并需要清理的后续步骤。已有 portable C0 的 kernel/runtime 验证差异见 [PortableSandboxConfig](sandbox-artifacts_zh.md#3-portablesandboxconfig)。

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
- Local capture 使用 same-directory temp 并清理失败的 partial output；named-location publication 使用 [Local tarstream 与 crypto](sandbox-artifacts_zh.md#92-local-tarstream-与-crypto) 的独立所有权检查与清理规则。

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

Host-only允许项包括 network provider/current identity、cgroup/controller、allocatable CPU/memory 与 resource enforcement、kernel/runtime actual path、active diff/template、restore prefetch和timeouts. Immutable disk graph、capacity identity、`deflate_on_oom`、network topology 由 E 拥有。Kernel 重哈希例外与 runtime-footer 比较见 [PortableSandboxConfig](sandbox-artifacts_zh.md#3-portablesandboxconfig)。

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

### 7.2 跨运行代次的 Usage

Cold/restore 进入同一 Host usage owner. CPU 观测随进程开始, Guest 观测只在
真正 launch/restore ready 后开始. 捕获前 Host 暂停准入, Guest 关闭旧 usage
连接但不替换阻塞 source worker. 成功 thaw 和失败恢复重新开放准入, restore
接受新 Host epoch. restored balloon seed 不是新 actual 观测.

`BaseDir/<SandboxID>.usage` 跟随逻辑身份且位于 E/S 之外. 恢复旧 S 不回滚
用量, 新 SandboxID 的克隆不继承用量. 新运行重建时钟和 Gauge 基线, 当前
内存/文件系统占用不扣除恢复初值. 正常停止有界尽力最终读取/保存, 删除不等待
成功也不单独保留用量. live/saved 回退、未知尾部及损失边界见
[usage 可靠性](usage_zh.md#7-可靠性与生命周期).

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

### 11.3 SnapshotView

`BlockCOW.SnapshotView` 暴露 upper-only 明文逻辑视图及 authoritative hole map。读取优先最新 dirty/writeback 缓存页。捕获只复制 bitmap，不复制整份缓存，全程要求前台 quiesce。不执行 Drain 或 fsync；后台 fatal 使捕获失败并通知 runtime owner。

### 11.4 BlockCOW state

Bitmap 跟踪逻辑 upper-present 4 KiB 块，在明文缓存接受完整页时发布，与 cache clean/dirty/writeback 状态独立。缺失块回落 base 或零。每沙箱 root/data diff 共享 `resources.diff_cow.cache_size`（默认 32MiB）及其 `max_dirty_size` 子集（16MiB）。活动 body（包括 tmpfs 文件）通过相同的对齐定位 I/O API 尝试支持的 O_DIRECT；不支持设置时仍通过同一应用缓冲进行普通 I/O，不采用文件系统专属策略；接受标志不保证物理缓存绕过；guest FLUSH 在健康时是 no-op，明确不提供持久化语义。普通 Close 排空已接受写入，不 fsync。底层 Discard 等在途回写结束后，对完整块打洞并重新暴露 base；wire DISCARD/WRITE_ZEROES 仍不支持。写零仍是 upper 数据。Export/snapshot 不 rotate diff 或改变 base。完整契约见[缓存、文件系统 I/O 与生命周期](diff-cow-cache_zh.md)。

### 11.5 Quiesce / Resume

Quiesce等待in-flight block request退出并阻止新request. 所有data/root views在同一quiesce窗口读取. Recovery顺序先恢复backend可服务状态,再恢复CH和guest连接,避免VM恢复后block request永久阻塞.

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

两种模式都先严格parse logical artifact和canonical config,再验证host ownership. `--from`允许persistent workload override;restore拒绝所有cold-only字段. Kernel binding preflight、runtime identity comparison、network topology/provider、disk count/name/topology 和 active diff binding 检查在 external lifecycle side effect 前完成；这不增加两种模式的 kernel digest 重哈希，见 [PortableSandboxConfig](sandbox-artifacts_zh.md#3-portablesandboxconfig)。

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

## 14. See Also

- [usage_zh.md](usage_zh.md) — Host-only 资源用量、查询、单位和持久化.
- [sandbox-init_zh.md](sandbox-init_zh.md) — guest PID 1、launch/quiesce/MUX 协议。
- [cloud-hypervisor_zh.md](cloud-hypervisor_zh.md) — CH build、API 与 restore 边界。
- [Connector TAPFD 协议](https://github.com/kuasar-sandbox/connector/blob/main/docs/tapfd_zh.md) — TAP descriptor handoff 与 network namespace，由 connector 维护。
- [timeouts-production.yaml](../examples/timeouts-production.yaml) — production host timeout 示例。
- [restore-prefetch-memory.yaml](../examples/restore-prefetch-memory.yaml) — explicit memory prefetch 示例。
- [README_zh.md](../README_zh.md) — build、release 与 repository 入口。

## 可写磁盘容量与配置迁移

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

必需源读取、终态 completion 规则和关闭顺序见[同步源读取恢复](sandboxer-read-recovery_zh.md).
