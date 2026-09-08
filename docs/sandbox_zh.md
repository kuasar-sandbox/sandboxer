[English](sandbox.md) | [简体中文](sandbox_zh.md)

# sandbox — 沙箱控制与制品生命周期

`sandbox-ctl` 是 kuasar-sandbox 的单沙箱 host 控制面. 它负责显式冷启动、从 Sandbox 制品冷启动、内存恢复、live export、无 VM 的 image-to-Sandbox-E assembly、内存快照、制品发布以及运行期 `exec`/forward. Guest 侧协议见 [sandbox-init_zh.md](sandbox-init_zh.md)。

本文只描述当前格式和行为。当前 reader 拒绝旧 `snapshot.cfg` 磁盘图 schema，不提供双读、自动迁移或跨版本兼容保证；这一格式边界不表示项目从未发布版本。

## 1. 概述

### 1.1 逻辑角色

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

### 1.2 逻辑角色与物理 carrier

逻辑内容与物理 carrier 正交. 同一个 `.image`、`.overlay`、`.sandbox` 或 `.snapshot` logical source 可以由以下 carrier 承载:

| Carrier | Reference | Notes |
|---|---|---|
| Local tarstream | `file://<basename>@digest:<digest>` 或 `@hmac:<digest>` | sparse map 由 tarstream envelope 权威声明 |
| Manifest | `manifest://<key>` | chunk/Manifest encryption 和 verification 由 manifest config 控制 |
| Manifest Bundle | `file://<bundle>@manifest:<key>` | 一个 ZIP64 Bundle 可承载 root 及完整依赖 Manifest graph |

Local 模式物化的内容寻址文件名为 `<digest>.<role>`,其中immutable root carrier使用`.image`,`.overlay`只表示单独物化的可写disk layer. 当前root writable top已经是Sandbox E的payload,不会再复制为`.overlay`. Provisioned container image输入仍可使用`.erofs`等显式basename. `<sid>.sandbox` 和 `<sid>.snapshot` 是成功 commit 后更新的语义 symlink. Bundle 模式下语义 symlink 指向承载 root Manifest 的 `<key>.bundle`. Alias 的 SID 必须是单一安全 path component;commit 使用临时 symlink + atomic rename,并拒绝覆盖已有 regular file 或 directory. 这些是节点本地 output 语义;named ref-location 使用 §11.2 的独立 shared-location commit protocol,不创建 alias.

Block backend、restore 和 publisher 先打开 carrier,再按逻辑角色解析内容. 外层 ZIP magic 只说明 carrier 是 Bundle,不说明 logical role.

### 1.3 责任边界

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

Publisher 自动严格识别 local E/S carrier. 已经 portable 的 graph dependency 保持原 ref;
`manifest://` root 本身不再 materialize 到 named location,也不存在 Manifest tail rewrite.
它不读取 `artifact.json`,也不存在 artifact-kind registry.

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
```

普通 cold run 使用完整 validation. `run --from` 和 `run --restore` 先严格解析 artifact,再按字段 presence 应用各自 rules,不能使用无约束 `LoadMerged` 覆盖 artifact graph.

### 3.2 PortableSandboxConfig

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

- cgroup path/fd/controller、overhead/watermark/startup;
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

### 3.3 Strict encoding 与 limits

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

### 3.4 C0、C1 与 source binding

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

### 3.5 `self` 与 disk provenance

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

### 3.6 `.sandbox` logical format

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

### 3.7 `.snapshot` logical format

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

### 3.8 files、env 与 ephemeral

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

可预测的 config/ref/format 错误应在 preflight、controller/network/VM 副作用前失败。这不保证后续操作全部无副作用：创建 run directory、写 C0、创建 diff 和获取资源是可能失败并需要清理的后续步骤。已有 portable C0 的 kernel/runtime 验证差异见 §3.2。

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
| Host strong | kernel/runtime actual path, active diff/template, cgroup/controller, network provider, timeout, manifest/crypto/ref-location, CH binary |
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
- Local capture 使用 same-directory temp 并清理失败的 partial output；named-location publication 使用 §11.2 的独立所有权检查与清理规则。

### 6.3 `ctl.sock` protocol

socket 固定位于 `RunRoot/PathID/ctl.sock`；PathID 省略时等于 SandboxID。
PathID 是 host-side 定位参数，不进入以下 wire request：

Snapshot和export使用独立 request type:

```text
snapshot_request -> snapshot_done | error
export_request   -> export_done   | error
exec_request     -> exec_ack      | error
```

Export不是 `snapshot_request{memory:false}`. Request在 run process中执行,因此可以复用当前 lifecycle barrier、guest/MUX gate、CH API socket和live vhost SnapshotView.

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
T12 /vm.resume
T13 establish restore MUX and resume original process
T14 settle balloon/resource observation
T15 start pinger,forward,sensor and steady lifecycle
```

Restore不会:

- 从 `snapshot.cfg` 读取 disk graph;
- 对 zero memory执行 cold start;
- 重跑 init/files/launch/plugin;
- 把 E的 `config.json` 当成新的 process launch request;
- 更新 memory parent或C0作为 re-snapshot side effect.

Host-only允许项包括 network provider/current identity、cgroup/controller、allocatable CPU/memory 与 resource enforcement、kernel/runtime actual path、active diff/template、restore prefetch和timeouts. Immutable disk graph、capacity identity、`deflate_on_oom`、network topology 由 E 拥有。Kernel 重哈希例外与 runtime-footer 比较见 §3.2。

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
4 KiB fault页;取得reservation后,handler把不大于1 MiB的窗口一次读入已有buffer.
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
allocation仍由`SnapshotReader`实现负责. Handler对
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

## 11. Provenance、publish 与 carrier

### 11.1 Disk 与 memory provenance

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
freeze前完整校验并物化,避免产生依赖调用节点私有路径的portable root. 其中immutable
root carrier物化为`.image`;只有需要作为独立dependency保存的root/data writable layer物化为
`.overlay`. 当前root writable top由Sandbox E payload承载,不生成第二份`.overlay`.

### 11.2 Local tarstream 与 crypto

Local immutable artifact支持 `crypto.local=off|auto|required`:

- `off`:plaintext tarstream,identity `digest`.
- `auto`:自动识别plaintext或KDXTS encrypted tarstream;新输出按配置codec.
- `required`:拒绝plaintext和未绑定key的identity;identity使用`hmac`.

未配置 policy 时默认 `off`；`auto` 或 `required` 在 storage 构造阶段就解析 customer key，即使是 file-only operation。没有 Manifest config 就没有 local codec 或 lazy Manifest client；file-only 不表示已配置的 crypto 可以省略 key。源码见 [storage.go](../pkg/artifact/storage.go)。

Existing-file reuse 必须重新验证 role、logical size、content identity 和完整 stream。 Local output与named ref-location的commit策略刻意分离. Artifact capture/publication只定义logical completion,不定义stable-storage durability:local output依赖完整写入、`Close()`检查、内容寻址no-replace rename与alias atomic rename;named-location依赖`O_EXCL`写入、`Close()`检查、最终路径reopen/full verification与路径身份检查. 两种 artifact-publication 路径都不执行显式 file/directory flush，物理写回由文件系统或底层存储实现定义；这不同于 §3.3 中执行 fsync 的 run-directory C0 写入。

#### Local output

`snapshot/export --output` 的 `FileSink` 在output directory中写unique same-directory temp,完整写入并检查`Close`错误,再以`renameat2(RENAME_NOREPLACE)`完成O(1) final commit. `BundleSink`同样以当前atomic no-replace rename提交完整Bundle;两条本地路径都不会为了final commit再读取并复制完整artifact. Root成功后,semantic alias用随机temporary symlink + atomic rename更新. Alias target和existing entry都以`NOFOLLOW`/`lstat` fail closed,不会把regular file或directory替换成symlink;commit由完整写入、`Close()`检查与atomic rename定义,不执行显式file/directory fsync. 因此local output要求节点本地filesystem提供这些atomic rename和symlink语义.

#### Named ref-location

`publish/upload-snapshot --to-ref-location`不复用`FileSink`或`BundleSink`. Tarstream carrier在自身marker中保存payload boundary和payload commitment;完整读取会用payload bytes复验该声明. 打开carrier后可直接提供identity. E/S只替换dense metadata tail时,carrier用旧payload commitment和新tail以O(tail)工作量推导新identity,不读取GiB级payload,也没有首次`io.Discard`编码. Location target取得carrier给出的scheme/digest后,以`O_CREATE|O_EXCL`直接创建`<digest>.image|overlay|sandbox|snapshot`;canonical encoding是shared target中的唯一完整write. `.image`承载immutable root image,`.overlay`只承载独立的writable disk dependency;Sandbox E payload不会重复发布为`.overlay`. Plaintext输出使用`@digest`,codec-backed输出使用`@hmac`. Target directory中没有完整temp/staging副本,也不创建`<sid>.sandbox`、`<sid>.snapshot`或任何其他semantic alias.

Fresh final固定`0644`. `tarstream.WriteTo`在唯一一次写入过程中检查source read和destination write,并重新产生与carrier预先提供值一致的scheme/digest;在 owned write fd 仍打开时以 `lstat` + `SameFile` 确认 canonical path 仍指向本次 `O_EXCL` 创建的 inode；metadata-only guard fd 在 checked `Close` 前固定该 inode。Publisher 随后以 `O_RDONLY|O_NOFOLLOW`（另含 nonblocking，避免意外 FIFO 阻塞验证）重新打开并完整验证regular file、role/payload name、logical size、canonical tarstream、marker、codec、crypto policy、digest scheme/digest和完整 sequential stream；验证后还会再次核对 path identity。Existing final 走同一完整验证；验证成功后直接复用,inode和bytes不改变.

Manifest Bundle不进入tarstream E/S重建路径. Carrier以root Manifest key提供`@manifest` identity;location target先强制验证所选Manifest closure、recorded admission、physical keys和crypto domain,再对same-directory依赖Bundle按顺序做exact byte copy,root `<key>.bundle`最后发布. 每个共享final仍只写一次,copy后重新打开、验证canonical Bundle/root并与source逐字节比较,最后在target fd上再次强制验证所选closure. 该路径不创建`.snapshot/.sandbox`替身,也不改写Bundle内的`snapshot.cfg`.

Final path在write完成前会短暂可见. 正常consumer只能使用publisher成功返回的root ref;publisher仍按dependencies first、root last顺序发布. Concurrent publisher遇到partial final时重新打开并做有限、context-aware exponential-backoff验证;若writer在窗口内完成则复用. bounded retry后仍不完整或invalid时fail closed并提示显式cleanup/repair,不会删除unknown owner的path. Symlink、directory、FIFO和其他non-regular final同样拒绝且不删除. Publisher只在自身`O_EXCL`成功且path仍指向所记录inode时清理自己的失败写入;abandoned unknown final由显式cleanup/GC处理.

#### Single-root image/Sandbox Manifest Bundle

`NewSingleRootBundlePublisher` 是已经组装好的 `RoleImage` 或 `RoleSandbox`
`sparse.Source` 的 typed named-location sink。数据流是：

```text
logical source
  -> Manifest ingest with fixed write admission/customer key
  -> Bundle root finalize + FullVerify in target-directory temporary file
  -> exclusive-create content-addressed <root>.bundle
  -> reopen/strict validate
  -> file://<root>.bundle@manifest:<root>#<location>
```

该路径不生成 role tarstream，不上传 Manifest store，不创建
`<root>.image`/`<root>.sandbox`，也不创建 BuildID/SandboxID semantic alias。Bundle
只含这个 logical root 的 Manifest/chunks；root logical role由 typed调用点决定，reader
仍以 strict image 或 Sandbox parser验证。existing same-key final必须与新生成 Bundle 的
admission、root、crypto和exact bytes全部一致才可复用；并发writer使用与普通 named
publication 相同的有限等待、exclusive-create和full validation
协议收敛。失败或取消不返回 ref，并清理自身 target-directory temporary file与
owned incomplete final。

Tarstream 与 Bundle 都是 physical carrier，不是 logical role。Tarstream 包含单一
role-specific payload 与 sparse envelope，使用 `@digest`/`@hmac` identity；Bundle
包含 Manifest/chunk records、write admission与可选local encryption，使用
`@manifest` root identity。single-root Bundle publication不得先建立 tarstream，反之也
不得仅按 `.image`/`.sandbox` 扩展名推导 Bundle root。

因此named location只依赖`mkdir`、exclusive create、write、read、stat/fstat/lstat、权限设置、seek/pread、close，以及删除本进程拥有的不完整file. 它不依赖rename/renameat2、symlink、hardlink、reflink、sparse-file preservation、advisory lock或lock file,也不执行显式file/directory sync;publication的success contract由`O_EXCL`写入、`Close()`检查、最终路径reopen/full verification与路径身份检查定义,物理写回由文件系统或底层存储实现定义.

Active encrypted `.overlay.diff` 保持KDXTS格式. Export只读取decrypt后的BlockCOW SnapshotView并创建新的immutable logical artifact,绝不把ZIP追加到active diff.

### 11.3 Manifest upload

已经组装好的 image 或顶层 Sandbox E 可通过
`NewManifestPublisher(...).PublishSource(ctx, RoleImage|RoleSandbox, source)`
直接 ingest。调用者保留 source ownership；该路径不生成 local tarstream，Chunks
与去重对象先写，root Manifest最后写入并返回`manifest://<root>`。

Local tarstream graph按照bottom-up顺序ingest:

```text
data/lower -> E -> S
```

已经portable的Manifest或located dependency保持原ref,不会先materialize再重写. Bundle root走exact-upload快路径:强制验证选择的Manifest closure、recorded admission和physical objects,原样上传Chunk/Manifest,root Manifest最后提交;root key和`snapshot.cfg`不变. 因此Bundle内已有的located selector仍要求consumer配置对应ref-location,不会被暗中改写成Manifest ref. Customer key、chunk/Manifest crypto、content verification和store generation admission沿用manifest config. E是export root,S是snapshot root.

### 11.4 Manifest Bundle

Bundle在pause前完成:

- write admission;
- portable dependency retention和located Bundle selector rewrite;
- unlocated local dependency materialization plan;
- current operation依赖集合;
- unlocated parent Bundle精确Manifest copy或remote fallback;located parent保留canonical selector;
- local tarstream dependency ingest;
- all ref replacements.

然后writer一次性emit metadata prefix,写入Manifest/Chunk,最后以E或S root key finalize. `FullVerify`使用完整expected Manifest集合. V1不以外层ZIP magic推断E/S.

### 11.5 Publish graph

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

Memory parent的historical `sandbox_ref` 不递归;当前S引用的E graph是当前disk truth. `manifest://`和已located ref保持不变,不会产生`Manifest -> named location`或Manifest tail rewrite. Bundle carrier整体走exact location copy或exact Store upload,不进入上述重建流程.

## 12. vhost-user-blk backend

### 12.1 Payload boundary

Vhost backend接收`fetch.Stream` Payload section,不是FullStream. 任何 `.sandbox` ZIP tail进入block logical size都属于bug,由format/unit tests覆盖.

### 12.2 Layered reads

Read 顺序为 active diff -> captured top -> `base_from_refs` -> root image（适用时）。Hole fall through，Zero/Data stop traversal。同一 writable block device 中组合的 immutable layers 的 logical size 必须一致。EROFS + ext4 拓扑中，read-only EROFS base 是独立 vhost device，不是 writable ext4 BlockCOW device 最后一层 fall-through。源码见 [disks.go](../pkg/sandbox/disks.go) 与 [serveandwait.go](../pkg/sandbox/serveandwait.go)。

### 12.3 SnapshotView

`BlockCOW.SnapshotView` 暴露decrypt后的upper-only logical view和authoritative hole map. View不重新打开active diff path,并且只在backend quiesced期间稳定.

### 12.4 BlockCOW state

BlockCOW 以 dirty bitmap 跟踪 4 KiB active-upper block，并非 clean/dirty/discard 三态 map。Dirty block 从 diff 读取；clean block 回落到 base，没有 base 才返回零。底层 `Discard` helper 只对完整 block 打洞并清除 dirty bit，使 base 再次可见；它不持久化能遮蔽 lower layer 的显式 Zero。当前 vhost profile 不公告 DISCARD 或 WRITE_ZEROES，request dispatcher 对两者都返回 unsupported，不调用这个 helper。写入 zero bytes 仍使 block 保持 dirty，不能扫描为 Hole。Export/snapshot 不 rotate active diff，也不把新 E 设为 backend base。源码见 [blk_cow.go](../pkg/vhost/blk_cow.go)、[server.go](../pkg/vhost/server.go) 和 [worker.go](../pkg/vhost/worker.go)。

### 12.5 Quiesce / Resume

Quiesce等待in-flight block request退出并阻止新request. 所有data/root views在同一quiesce窗口读取. Recovery顺序先恢复backend可服务状态,再恢复CH和guest连接,避免VM恢复后block request永久阻塞.

## 13. Validation、错误与安全

### 13.1 Cold validation

普通 cold config至少验证:

- capacity/allocatable和resource mode一致;
- kernel/runtime为absolute unlocated file binding;
- network provider唯一且identity字段有provider;
- root/data topology合法,数据盘name/order和mount 1:1;
- active diff/template/size组合可生成mountable filesystem;
- local/Manifest/Bundle ref与crypto policy一致;
- launch/files/init/plugin/metadata limits.

### 13.2 `run --from` 与 restore validation

两种模式都先严格parse logical artifact和canonical config,再验证host ownership. `--from`允许persistent workload override;restore拒绝所有cold-only字段. Kernel binding preflight、runtime identity comparison、network topology/provider、disk count/name/topology 和 active diff binding 检查在 external lifecycle side effect 前完成；这不增加两种模式的 kernel digest 重哈希，见 §3.2。

### 13.3 CLI mutual exclusion

- `run --from` 与 `--restore` 互斥.
- `run --replace-boot` 仅允许与`--from`一起使用，要求host config，并拒绝`--restore`.
- export/snapshot都要求`--output`与`--upload`二选一.
- Explicit `--mode` 与 `--upload`互斥.
- Live export拒绝`--config`;image-to-Sandbox-E assembly 要求 `--from` 加 `--config` 或 `SANDBOX_CONFIG`，且拒绝 `--resume`。
- Snapshot没有memory toggle.
- `exec` command必须位于`--`之后;local与proxy target rules互斥.

### 13.4 Failure contract

- Error包含field/entry/ref/disk index context,但不打印inline file/env value、customer key或plaintext digest.
- Local crypto错误保持protected presentation.
- ZIP/path traversal/symlink/regular-file checks fail closed.
- Remote I/O 接收 operation context；取消遵循 owning I/O implementation，并不表示已提交对象被回滚。
- Stream/fetcher/Bundle reader共享明确close owner,失败路径不泄漏fd/mmap/goroutine.
- Freeze后的任一failure必须恢复app、CH/backend/MUX/pinger/forward并释放resource locks;若MUX恢复已不可证明,必须在locks仍持有时终止CH并将capture gate置为terminal.
- Operation root 成功发布是 commit point；local output 随后更新 semantic alias，named location 没有 alias。Dependency orphan 不伪装成功。

## 14. Reliability、performance 与兼容边界

### 14.1 Atomicity 与 determinism

Portable YAML和E/S ZIP使用canonical order、fixed metadata和bounded bytes. Local `FileSink`/`BundleSink`保持same-directory temp、完整写入/`Close()`检查和atomic no-replace rename,final commit是O(1);alias只在root commit后更新. artifact capture/publication只定义logical completion,不定义stable-storage durability;两条本地路径都不执行显式file/directory fsync,物理写回由文件系统或底层存储实现定义. Named ref-location采用独立的exclusive-create + checked-write/copy-once + reopen-full-verify协议;tarstream由carrier直接提供identity,Bundle保持exact bytes,两者都不进入local sink的capture/commit路径.

多盘顺序固定为data disks first、root E last. Snapshot随后写memory S last. 这让S/E root成为可审计的graph commit point.

### 14.2 Streaming 与 memory use

- Sparse tarstream不spool logical stream到disk.
- Manifest ingest只读取resident extents.
- E/S 大 payload 不暂存到 per-run `/run` directory；CH 有界 config/state staging files 放在该处。调用者选择的 output directory 和 named-location temporary Bundle file 属于独立路径，并占用所选文件系统容量。
- Bundle dependency plan在pause前执行remote I/O和admission.
- V1 `--resume` 可以在sink写完整期间保持VM paused,换取SnapshotView稳定性.

### 14.3 Performance observations

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

### 14.4 Incompatibility

本切换直接替换旧snapshot provenance. 旧`SnapshotConfig`若包含resource/runtime/root/data/launch字段会返回明确unsupported error. 不提供 old-format alias、dual reader、migration shim、feature flag 或 zero-memory compatibility path。§2.1 的 `upload-snapshot` CLI alias 不提供格式兼容。

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

## 15. See Also

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

对于一个**新建的**、空白的 512 MiB scratch 文件系统，可以准备新模板，使用时不再指定
大小覆盖项：

```bash
truncate -s 512M /tmp/scratch-512m.ext4
mkfs.ext4 -F /tmp/scratch-512m.ext4
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
不会限制 COW 脏数据字节数，也不会在启动或恢复时改变磁盘容量。逻辑块设备容量与加密
文件的物理长度、宿主磁盘实际分配空间、guest 文件系统可用于文件数据的空间并不是
同一个概念。本次迁移不提供从同一个模板任意选择各实例容量的新能力。
