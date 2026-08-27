# sandbox — 沙箱控制与制品生命周期

`sandbox-ctl` 是 kuasar-sandbox 的单沙箱 host 控制面. 它负责显式冷启动、从 Sandbox 制品冷启动、内存恢复、在线/离线导出、内存快照、制品发布以及运行期 `exec`/forward. Guest 侧协议见 `sandbox-init.md`.

本文只描述当前格式和行为. 项目尚未发布,旧 `snapshot.cfg` 磁盘图不属于兼容输入.

## 1. 概述

### 1.1 逻辑角色

系统区分 4 个逻辑角色:

| Role | Suffix | Logical content | Purpose |
|---|---|---|---|
| Container image | image-specific | `EROFS + ZIP(config.json)` | 只读容器 rootfs 与 OCI image defaults |
| Disk layer | `.overlay` | sparse block payload | root/data disk 的不可变 block layer |
| Sandbox E | `.sandbox` | root payload + strict ZIP | 可重复冷启动的完整 portable workload |
| Snapshot S | `.snapshot` | memory payload + strict ZIP | 恢复已运行进程和 VMM/guest memory state |

`E` 表示 runnable Sandbox artifact,`S` 表示 memory Snapshot. 两者职责不可互换:

```text
explicit sandbox.yaml ── run --config ──> cold start
Sandbox E             ── run --from   ──> cold start
Snapshot S ── sandbox_ref ──> E
           └──────── run --restore ────> memory restore

live/offline export ──> E
memory snapshot      ──> E + S, S is operation root
```

不存在以下模型:

- 不存在 `snapshot --memory=false`.
- 不存在 zero-size memory Snapshot.
- 不存在 `run --restore` 的 cold-boot 分支.
- `snapshot.cfg` 不保存 disk graph、launch、mounts、files 或 init.
- cold boot 不包装成 restore.

### 1.2 逻辑角色与物理 carrier

逻辑内容与物理 carrier 正交. 同一个 `.overlay`、`.sandbox` 或 `.snapshot` logical source 可以由以下 carrier 承载:

| Carrier | Reference | Notes |
|---|---|---|
| Local tarstream | `file://<basename>@sha256:<digest>` 或 `@hmac:<digest>` | sparse map 由 tarstream envelope 权威声明 |
| Manifest | `manifest://<key>` | chunk/Manifest encryption 和 verification 由 manifest config 控制 |
| Manifest Bundle | `file://<bundle>@manifest:<key>` | 一个 ZIP64 Bundle 可承载 root 及完整依赖 Manifest graph |

Local 模式的内容寻址文件名为 `<digest>.<role>`. `<sid>.sandbox` 和 `<sid>.snapshot` 是成功 commit 后更新的语义 symlink. Bundle 模式下语义 symlink 指向承载 root Manifest 的 `<key>.bundle`.

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

# Resume memory execution state from Snapshot S.
sandbox-ctl run --restore ./s1.snapshot --config restore-host.yaml --sandbox-id s3
```

`--from` 与 `--restore` 互斥. `--config` 在三种模式中的所有权不同:

- 普通 run:完整显式 cold config.
- `--from`:host/instance overlay;portable workload 来自 E.
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

```bash
sandbox-ctl snapshot \
  --sandbox-id s1 \
  (--output /artifacts | --upload) \
  [--mode local|bundle] \
  [--resume] \
  [--drop-caches=false] \
  [--merge-ref=true]
```

Snapshot 始终包含 memory execution state. 一次操作在同一 freeze point 产生 E 和 S,S 最后 commit. 默认成功后销毁 VM;`--resume` 恢复原 VM,但不把新 E/S 设为 live baseline.

Local human output同时列出 `Snapshot S` 与 `Sandbox E`. Upload stdout 仍只输出 S Manifest key,便于当前 orchestrator parser 使用. 既有 `memory_size`、`memory_resident`、pause/dump timing 和 compatibility `overlay_*` response 字段保留;`overlay_*` 当前镜像 E identity,真正 disk graph 只在 E 中.

`--drop-caches` 只属于 memory snapshot,默认 false. `--merge-ref` 只控制 local memory parent merge,不改变 disk provenance.

### 2.4 `sandbox-ctl export`

Live export:

```bash
sandbox-ctl export \
  --sandbox-id s1 \
  (--output /artifacts | --upload) \
  [--mode local|bundle] \
  [--resume]
```

Offline flattened EROFS export:

```bash
sandbox-ctl export \
  --from ./flattened.img \
  --config sandbox.yaml \
  --sandbox-id base \
  (--output /artifacts | --upload) \
  [--mode local|bundle]
```

Live mode不接受 `--config`;offline mode要求 `--config` 或 `SANDBOX_CONFIG` 且拒绝 `--resume`. Export 不接受 `drop_caches` 或 memory merge 参数,不调用 CH `/vm.snapshot`,不读取 memfd,不生成 memory refs.

### 2.5 `sandbox-ctl exec`

```bash
sandbox-ctl exec --sandbox-id s1 --run-root /run/sandbox -- /bin/sh -c 'id'
```

`exec` 通过当前 ctl/MUX 创建 sibling process. Export/snapshot 的 quiesce gate 阻止新 exec/forward 进入不稳定窗口. 已接受的连接在 resume recovery 后恢复服务.

远程授权 exec 使用 `pkg/ctl.ServeExecTunnel(ctx, options)`,固定以下顺序:

```text
Authorize -> AcceptDownstream -> ReadExecRequestFrame -> AuthorizeRequest
          -> DialBackend -> write frame.Raw once -> duplex relay
```

Request gate 通过前不得拨号 backend 或触发 Sandbox lifecycle. 首帧上限 64 KiB,必须是单一、字段集严格且无 duplicate 的 `exec_request`,包含非空 `exec.argv`;读取最多等待 10s,调用方只能缩短. 通过后原始 frame 只写入一次,不 decode/re-encode. 已识别的 request/backend rejection 只对外返回脱敏 `exec request rejected`;`ProxyExec` 仅保留给已预连接 backend 的调用方.

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
- 对 S 输出 `snapshot.cfg`,其中至少有 `sandbox_ref`.
- 对 malformed、ambiguous 或既不是 E 也不是 S 的 logical root fail closed.

Local crypto、Manifest、Bundle、selector 和 named ref-location 与 run/publish 使用相同 opener.

### 2.8 `sandbox-ctl publish`

```bash
# Publish to Manifest store.
sandbox-ctl publish --manifest-config manifest.yaml ./s1.snapshot

# Publish to a trusted named file location.
sandbox-ctl publish \
  --to-ref-location release=file:///srv/sandbox-artifacts \
  --ref-location source=file:///srv/source \
  ./s1.sandbox
```

Publisher 自动严格识别 E 或 S. 它不读取 `artifact.json`,也不存在 artifact-kind registry.

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
    base: file:///images/root.erofs@sha256:<digest>
    overlay:
      base: file:///layers/parent.overlay@sha256:<digest>
      base_from_refs: []
      # Host-only active binding:
      diff: file:///var/lib/kuasar/s1.overlay.diff
      diff_template: file:///opt/kuasar/empty.ext4
      diff_size: 1GiB
  disks:
    - name: data
      base: file:///layers/data.overlay@sha256:<digest>
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
  kernel: file://vmlinux@sha256:<digest>
  runtime: file://sandbox-runtime.bundle@sha256:<digest>
  cmdline: "console=hvc0"
  root:
    base: file://root.erofs@sha256:<digest>
    overlay:
      base: self
      base_from_refs:
        - file://parent.overlay@sha256:<digest>
  disks:
    - name: data
      base: file://data.overlay@sha256:<digest>
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
file://vmlinux@sha256:<digest>
file://sandbox-runtime.bundle@sha256:<digest>
file://<digest>.overlay@sha256:<digest>
file://<digest>.overlay@hmac:<digest>
```

目标 host 使用实际 path、source directory 或 named ref-location 完成 binding,并在创建 controller、network、cgroup、VM 或 run directory 副作用前验证 identity.

### 3.3 Strict encoding 与 limits

Portable config 使用 deterministic YAML marshal 和 strict known-fields parse. Reader 拒绝 unknown field、unknown version、duplicate key、YAML alias/merge key、多 document、非法 ref 和 non-canonical bytes.

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

`<run_dir>/sandbox.runtime.cfg` 用 same-directory temp、0600、file fsync、rename 和 directory fsync 写入. 同一 lifecycle 只允许 write once;已存在且 bytes 不相同会失败.

### 3.4 C0、C1 与 source binding

`C0` 是一次 run lifecycle 的 immutable portable baseline:

```text
explicit cold config + persistent fields + canonical identities
  - host-only - ephemeral = C0

E.sandbox.runtime.cfg + allowed persistent overrides
  + canonical host bindings - ephemeral = C0 for run --from

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
  base: file://root.erofs@sha256:<digest>
  overlay:
    base: self
    base_from_refs: []

# Single-disk ext4.
root:
  base: self
  base_from_refs: []
```

`run --from` 通过 source binding 把 `self` 绑定到当前 E payload. Export C1 先把 C0 的旧 `self` 物化为原 source ref,再把新 root payload 位置设为 `self`;这避免 E digest 自引用循环.

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

Offline flattened EROFS export:

```text
[EROFS payload]
[ZIP:
  config.json
  sandbox.runtime.cfg
]
```

Offline conversion定位原 `EROFS + ZIP(config.json)` 的 archive base,原样保留 EROFS prefix 和经过校验的 `config.json` bytes,然后重建一个 canonical two-entry ZIP. `ZIP(config.json) + ZIP(sandbox.runtime.cfg)` 是非法 double ZIP.

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

Opened root 提供:

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
sandbox_ref: file://<digest>.sandbox@sha256:<digest>
from_refs:
  - file://<parent>.snapshot@sha256:<digest>
```

`sandbox_ref` 指向同一 freeze point 生成的 E. `from_refs` 是 top-to-bottom memory parent chain. S 不重复 capacity、runtime、root/data graph、launch、mounts、files、init 或 metadata.

Snapshot ZIP 与 config 同样 strict、bounded、canonical. 旧 disk-schema input 返回 `unsupported snapshot format/version`,不 dual-read、不 migration、不 cold fallback.

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
- Ephemeral 不表示从 memory snapshot 中擦除;RAM 仍可能含其内容.
- disk-backed `type: empty` volume随所属 root/data disk export.
- `type: tmpfs` 内容不随 export.
- `init` 在 `run --from` 时重新执行.
- plugin/app 按 cold semantics 重新启动.
- `run --restore` 不重跑 launch/files/init/plugin.

Restore host若显式提供 `boot.cmdline`、`resources.startup`、launch persistent/ephemeral fields、mounts、files/ephemeral_files、init 或 metadata,会在副作用前拒绝,而不是静默忽略.

## 4. 资源模型

### 4.1 三种部署模式

| Mode | `cgroup_path` | `controller` | Behavior |
|---|---|---|---|
| No cgroup | empty | empty | 不写 cgroup;allocatable 必须与 capacity 约束一致 |
| Static cgroup | set | empty | 本地设置 CPU/memory limit,可运行 local sensor |
| Dynamic | set | set | 通过 resource protocol admission/lease/Budget,并运行 local sensor |

`resources.capacity` 是 guest-visible VM capacity,进入 Portable config. `resources.allocatable` 是 workload 默认值,也进入 Portable config. `control`、`overhead`、`watermark_high` 和 `startup` 是 node policy,不进入 E.

Export/snapshot 获取 MemoryController mutation barrier,并在 freeze 前 lift/drain 可能与 CH pause 竞争的 `memory.high`. Recovery 在 VM、MUX、app 和 backend 恢复后释放 barrier.

### 4.2 Memory terms

```text
CapacityMemory      = CH boot memory zone maximum
AllocatableMemory   = workload settled guest headroom
DemandMemory        = controller observation
Budget              = admitted/locally enforced working allowance
VMM memory.max      = CapacityMemory + host overhead
```

Memory S capture记录 CH config/state 和 memfd sparse content;资源 policy 不写入 `snapshot.cfg`. Restore capacity/allocatable identity来自 E,host若显式给出必须一致.

### 4.3 CPU 与 balloon

Capacity CPU 决定 vCPU topology. Allocatable CPU 在 cgroup 模式映射为 `cpu.weight`;无 cgroup时不能表达 fractional CPU. Balloon current state由 CH restore state权威恢复,host不从 S 发明第二个 balloon state.

## 5. Cold start 与 `run --from`

### 5.1 显式 cold start

普通 `run --config` 的 preflight 顺序:

```text
T0 parse/merge/validate config and limits
T1 open and verify immutable refs, kernel/runtime identities and image defaults
T2 project portable C0 and validate exactly one self
T3 prepare active diffs and source binding
T4 atomically write <run_dir>/sandbox.runtime.cfg once
T5 acquire controller/cgroup/network resources
T6 construct vhost block devices from payload-only streams
T7 spawn/configure CH
T8 launch sandbox-init spec and app
T9 establish MUX/pinger/forward/resource lifecycle
```

所有可预测的 config/ref/identity/format 错误应在 T4 之前或最迟在任何 external side effect 前返回.

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

受保护字段有冲突时明确报出 field context. Host不能通过省略或 YAML merge静默改变 disk graph.

Network必须满足:

```text
portable network.enabled == host provider presence
portable interface       == explicitly supplied host interface
```

Direct EROFS E只有 read-only image. 目标 host必须提供 pre-formatted `diff_template`,或已格式化的 explicit diff. 缺少可挂载 upper时在 controller/network/CH side effect 前失败.

最终调用普通 `sandbox.Run`;`run --from` 不进入 `restore.Run`.

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
T3 gate exec/forward, freeze guest app, guest sync
T4 pause pinger/MUX, pause CH, quiesce all block backends
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

- Data artifacts/E可能成为 content-addressed orphan,由正常 GC回收.
- Root alias只在 E或S成功后提交.
- `--resume` success/failure都恢复 backend、CH、MUX、pinger、forward、app和memory barrier.
- C0、active diff和live lower graph保持不变.
- Partial files使用 same-directory temp并清理.

### 6.3 `ctl.sock` protocol

Snapshot和export使用独立 request type:

```text
snapshot_request -> snapshot_done | error
export_request   -> export_done   | error
exec_request     -> exec_ack      | error
```

Export不是 `snapshot_request{memory:false}`. Request在 run process中执行,因此可以复用当前 lifecycle barrier、guest/MUX gate、CH API socket和live vhost SnapshotView.

Response中的 secret不回显. Remote Manifest upload可以耗时较长,CLI `--timeout=0` 表示不设置 operation deadline;context cancel仍中断 read/write.

### 6.4 Offline EROFS export

Offline input必须是 flattened `EROFS + ZIP(config.json)`,不能是已包装 `.sandbox`. 流程:

```text
open carrier -> validate flattened image -> retain config.json bytes
load explicit config -> apply image defaults -> build direct-EROFS C1
rebuild EROFS + ZIP(config.json,sandbox.runtime.cfg)
emit local/Manifest/Bundle E -> commit E root
```

它不创建 VM、不需要 freeze,但仍执行 portable identity、limits、carrier admission和output atomicity检查.

## 7. Memory restore 数据流

`run --restore S` 只处理真正的 memory Snapshot:

```text
T0 parse host config with presence
T1 open carrier and strict-parse S
T2 parse canonical snapshot.cfg
T3 open S.sandbox_ref and strict-parse E
T4 apply restore host-only rules to E config -> immutable lifecycle C0
T5 verify kernel/runtime/ref identities and every disk source
T6 atomically write run-dir C0
T7 bind self and reconstruct root/data disks from E
T8 create memfd and layer S.memory over S.from_refs
T9 create UFFD/va_report endpoints and CH restore argv
T10 start CH from config.json/state.json
T11 transfer UFFD and complete restore handshake
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

Host-only允许项包括 network provider/current identity、cgroup/controller、resource enforcement、kernel/runtime actual path、active diff/template、restore prefetch和timeouts. Immutable disk graph、capacity/allocatable identity、network topology由E拥有.

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

`restore.prefetch: memory` 是 host-only optimization,只预热当前 S memory self的 file page cache或Manifest chunks. 它不改变 sparse truth、fault ordering、C0、S或memory parents. 默认 `off`;不满足资格或invalid mode在副作用前失败.

## 8. UFFD handler

Memory restore使用一个 memfd和一个 userfaultfd管理 CH memory zone. S self及`from_refs`被打开为 layered sparse source;top resident data覆盖lower,top Hole向parent fall through,Zero仍是显式zero.

### 8.1 Single UFFD contract

CH为同一 restored memory mapping注册 missing-page events. Host通过 restore handshake接收 UFFD,并用 CH state中的memory zone bounds验证 fault address. 超界、重复协议或截断 source fail closed.

### 8.2 Read path

Fault worker优先处理当前缺页;serial tail用于可预测顺序填充. `EVENT_REMOVE` 使已丢弃范围重新成为missing,避免把旧page state误当resident. Context cancellation停止worker并关闭fd/stream owner.

## 9. cgroup 与 balloon

### 9.1 Memory enforcement

Static/dynamic cgroup模式在CH pause前lift可能竞争的`memory.high`,并等待已有high事件drain. Snapshot/export recovery恢复原值. VMM cgroup只承载CH进程,不把`sandbox-ctl`自身算入workload Budget.

### 9.2 CPU

`capacity.cpu` 决定 vCPU count. `allocatable.cpu` 在cgroup模式映射到clamped `cpu.weight`;controller可以在lifecycle内调整grant,但不会改写C0或artifact.

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

### 11.2 Local tarstream 与 crypto

Local immutable artifact支持 `crypto.local=off|auto|required`:

- `off`:plaintext tarstream,identity `sha256`.
- `auto`:自动识别plaintext或KDXTS encrypted tarstream;新输出按配置codec.
- `required`:拒绝plaintext和未绑定key的identity;identity使用`hmac`.

Existing-file reuse必须重新验证role、logical size、content identity和完整stream. Content file使用no-replace atomic commit;semantic alias在root成功后atomic rename. Symlink、regular-file和directory fsync检查fail closed.

Active encrypted `.overlay.diff` 保持KDXTS格式. Export只读取decrypt后的BlockCOW SnapshotView并创建新的immutable logical artifact,绝不把ZIP追加到active diff.

### 11.3 Manifest upload

Upload按照 bottom-up顺序ingest:

```text
data/lower -> E -> S
```

Customer key、chunk/Manifest crypto、content verification和store generation admission沿用manifest config. E是export root,S是snapshot root.

### 11.4 Manifest Bundle

Bundle在pause前完成:

- write admission;
- external ordered refs plan;
- current operation依赖集合;
- parent Bundle精确Manifest copy或remote fallback;
- local tarstream dependency ingest;
- all ref replacements.

然后writer一次性emit metadata prefix,写入Manifest/Chunk,最后以E或S root key finalize. `FullVerify`使用完整expected Manifest集合. V1不以外层ZIP magic推断E/S.

### 11.5 Publish graph

Publish E:

```text
parse E -> enumerate E explicit disk refs bottom-up
        -> publish payload dependencies -> rewrite refs
        -> rebuild E -> publish E last
```

`self` 永不重写. 父 `.sandbox` 作为disk ref时发布为`.overlay` payload,不递归父config graph.

Publish S:

```text
publish memory from_refs as opaque memory layers
publish S.sandbox_ref E graph
rewrite sandbox_ref
rebuild S -> publish S last
```

Memory parent的historical `sandbox_ref` 不递归;当前S引用的E graph是当前disk truth. Named location publication将所有重写ref绑定到target location.

## 12. vhost-user-blk backend

### 12.1 Payload boundary

Vhost backend接收`fetch.Stream` Payload section,不是FullStream. 任何 `.sandbox` ZIP tail进入block logical size都属于bug,由format/unit tests覆盖.

### 12.2 Layered reads

Read顺序为active diff -> captured top -> `base_from_refs` -> root image. Hole fall through,Zero/Data stop traversal. Layer logical size必须一致.

### 12.3 SnapshotView

`BlockCOW.SnapshotView` 暴露decrypt后的upper-only logical view和authoritative hole map. View不重新打开active diff path,并且只在backend quiesced期间稳定.

### 12.4 BlockCOW state

BlockCOW使用clean/dirty/discard三态跟踪active upper. Discard对应Hole语义;写入zero bytes仍是Data/Zero事实,不能扫描为Hole. Export/snapshot不rotate active diff,也不把新E设为backend base.

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

两种模式都先严格parse logical artifact和canonical config,再验证host ownership. `--from`允许persistent workload override;restore拒绝所有cold-only字段. Kernel/runtime identity、network topology/provider、disk count/name/topology和active diff bindings在side effect前完成.

### 13.3 CLI mutual exclusion

- `run --from` 与 `--restore` 互斥.
- export/snapshot都要求`--output`与`--upload`二选一.
- Explicit `--mode` 与 `--upload`互斥.
- Live export拒绝`--config`;offline export要求`--from + --config`且拒绝`--resume`.
- Snapshot没有memory toggle.
- `exec` command必须位于`--`之后;local与proxy target rules互斥.

### 13.4 Failure contract

- Error包含field/entry/ref/disk index context,但不打印inline file/env value、customer key或plaintext digest.
- Local crypto错误保持protected presentation.
- ZIP/path traversal/symlink/regular-file checks fail closed.
- Remote read/write接受context cancellation.
- Stream/fetcher/Bundle reader共享明确close owner,失败路径不泄漏fd/mmap/goroutine.
- Freeze后的任一failure必须thaw app、resume CH/backend/MUX/pinger/forward并释放resource locks.
- Root alias是commit point;dependency orphan不伪装成功.

## 14. Reliability、performance 与兼容边界

### 14.1 Atomicity 与 determinism

Portable YAML和E/S ZIP使用canonical order、fixed metadata和bounded bytes. Local artifact边写边计算content identity,使用same-directory temp、fsync和no-replace commit. Alias只在root commit后更新.

多盘顺序固定为data disks first、root E last. Snapshot随后写memory S last. 这让S/E root成为可审计的graph commit point.

### 14.2 Streaming 与 memory use

- Sparse tarstream不spool logical stream到disk.
- Manifest ingest只读取resident extents.
- E/S大payload不进入`/run`;只有CH小型config/state staging files进入tmpfs.
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

本切换直接替换旧snapshot provenance. 旧`SnapshotConfig`若包含resource/runtime/root/data/launch字段会返回明确unsupported error. 不提供alias、dual reader、migration shim、feature flag或zero-memory compatibility path.

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

- `docs/sandbox-init.md` — guest PID 1、launch/quiesce/MUX协议.
- `docs/cloud-hypervisor.md` — CH build、API与restore边界.
- `docs/tapfd.md` — TapFD handoff与network namespace.
- `examples/timeouts-production.yaml` — production host timeout示例.
- `examples/restore-prefetch-memory.yaml` — explicit memory prefetch示例.
- `README.md` — build、release与repository入口.
