[English](configuration.md) | [简体中文](configuration_zh.md)

# Sandbox 配置参考

本页定义 Host `sandbox.yaml`、有序覆盖、有效默认值和输出目标。先理解字段归属示例，
再渲染最终配置并运行。配置选择策略；[Runtime](runtime_zh.md) 定义消费配置的操作，
[资源](resources_zh.md)定义 Budget 与计量，[块存储](block-storage_zh.md)定义存储生命周期。

E/S 的可移植 schema 由[制品](artifacts_zh.md#portable-config)定义；Guest 消息和进程语义由
[Guest ABI](guest-abi_zh.md)定义。Manifest/store/crypto/cache 配置仍由 accelerator 负责，
共用 YAML 输入并不意味着这些字段属于 sandbox schema。

<a id="sandbox-yaml"></a>

## `sandbox.yaml`

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

<a id="render-config"></a>

## `sandbox-ctl config`

```bash
sandbox-ctl config --template
sandbox-ctl config --config host.yaml:instance.yaml --check strict
sandbox-ctl config --config restore.yaml --mode restore --check strict
```

Template 同时标明 portable、host-only 和 ephemeral 字段. Restore mode 输出 host-only 文档并删除 cold-only workload 与 immutable disk refs;它不会生成第二套 restore schema.

<a id="loading-and-overrides"></a>

## 加载、覆盖与校验

`sandbox-ctl run --config a.yaml:b.yaml` 与 `config --config a.yaml:b.yaml` 按顺序加载文件；
未给 `--config` 时，由 `SANDBOX_CONFIG` 提供路径列表。普通 [`LoadMerged`](../pkg/config/config.go)
向同一个值依次解码：后面的 scalar 和 list 替换前面的值，map 和 struct 递归合并，省略的 key 保留前值。
全部文件加载完后只应用一次默认值。渲染会规范化 YAML，不保留注释；`config --template` 则输出带注释的编写模板。

`config --check strict` 按 `--mode default|restore` 校验值。此 flag 不保证历史普通顶层 YAML loader
拒绝所有未知字段；`usage`、`resources.diff_cow`、`resources.watermark_high` 等严格子 schema 有各自
解码检查。`run --from` 和 restore 还保留字段存在性并执行制品归属规则，因此显式零/空值不总等于省略。
不得用不受约束的普通 merge 替换制品磁盘图。

| 输入模式 | 工作负载与状态 | Host 覆盖 |
| --- | --- | --- |
| 显式冷启动 `run --config` | 配置与镜像默认值；在 VM side effect 前校验 | 本次调用所有合法字段 |
| 冷启动 `run --from E` | E 提供可移植默认值和受保护磁盘拓扑 | [基于存在性的 from 规则](#from-rules)；`--replace-boot` 显式替换完整 boot 对象 |
| `run --restore S` | S 提供执行状态，引用的 E 提供不可变工作负载和磁盘图 | 当前 Host 策略/绑定；不重放 launch、init、files 或 mounts |

Restore 拒绝显式提供 `boot.cmdline`、launch（包括 ephemeral environment 和 plugin）、mounts、
files/ephemeral_files、init、metadata。可以提供当前 Host 网络身份/provider、资源执行、kernel/runtime
本地路径、active writable binding、prefetch、usage 和 timeout 策略，但仍须满足捕获拓扑和身份检查。
Startup 策略可记录，初始恢复预留仍由捕获的 `BudgetAtSnapshot` 决定。`config --mode restore` 生成
Host projection，不创建第二套 portable schema。实现见 [`FieldPresence`](../pkg/config/presence.go)、[from 配置应用](../pkg/config/applyfrom.go)和[restore 配置应用](../pkg/config/applyrestore.go)，
操作顺序见[恢复](runtime_zh.md#restore)。

<a id="field-reference"></a>

## 字段参考与有效默认值

下表覆盖 [`pkg/config`](../pkg/config/config.go) 中由 sandbox 拥有的 YAML 字段。
Size 是 `512MiB` 等字节大小字符串；duration 是 `30s` 等 Go duration 字符串。
除非注明，默认值针对显式冷启动加载；制品派生操作仍服从上述归属规则。可选对象省略后保持不存在，
直到 consumer 解析其已定义默认值。

### 资源字段

| 字段 | 类型 / 默认值 | 校验与效果 |
| --- | --- | --- |
| `resources.capacity.cpu` | 整数；零/省略时 `1` | 正的固定 vCPU 数；部署仍受所选 kernel/VMM 能力约束 |
| `resources.capacity.memory` | Size；必填 | 固定 Guest RAM 地址空间容量 |
| `resources.allocatable.cpu` | 数值；零/省略时 capacity CPU | 有限、为正、不超过 capacity；无 cgroup 时必须等于 capacity |
| `resources.allocatable.memory` | Size；capacity memory | 正的稳定期 Guest headroom，不超过 capacity；不是 RSS 目标 |
| `resources.allocatable.deflate_on_oom` | Boolean；`true` | 所选策略创建 balloon 时生效；属于 portable workload |
| `resources.control.cgroup_path` | 已存在的绝对目录路径；空 | 空值关闭 cgroup 操作；runtime 不创建或删除该目录 |
| `resources.control.controller` | 文件系统 Unix socket 路径；空 | 与 cgroup 一起启用动态模式；不支持 abstract socket |
| `resources.control.sensor.mode` | `psi`、`events_poll`、`none`；`psi` | 选择本地压力信号；PSI trigger 被拒时回退 events polling |
| `resources.control.sensor.psi_some_stall_us` | 无符号整数；`10000` | 显式正 stall 要求 window；stall 不得超过显式 window |
| `resources.control.sensor.psi_some_window_us` | 无符号整数；`1000000` | PSI 微秒窗口；零值由 consumer 解析为默认值 |
| `resources.control.sensor.min_interval_ms` | 整数；`100` | 正值覆盖请求去抖间隔，非正值取默认 |
| `resources.overhead.memory` | Size；有 cgroup 时 `32MiB` | 向 `memory.max` 增加 VMM overhead；在线运行显式对象要求 cgroup |
| `resources.watermark_high.ratio` | 数值；`0.875` | 严格位于零和一之间；显式对象要求 cgroup |
| `resources.startup.memory` | Size；capacity memory | 正值、不超过 capacity；与 allocatable headroom 独立 |
| `resources.diff_cow.cache_size` | Size；`32MiB` | 全 sandbox 共享 cache；正的 4 KiB 整数倍 |
| `resources.diff_cow.max_dirty_size` | Size；从最终 cache 推导 | 正的 4 KiB 整数倍且不超过总额；见[推导和覆盖](#diff-cow-policy) |

`CgroupFD` 是继承的 Go/runtime capability，不是 YAML 字段。方程、预留协议和失败行为见[资源](resources_zh.md)。

### 网络与启动字段

| 字段 | 类型 / 默认值 | 校验与效果 |
| --- | --- | --- |
| `network.tap` | String；空 | 接入已有 Host TAP；与 `tapfd` 互斥 |
| `network.tapfd.exec` | String list | Helper argv；exec/socket 恰选一个；裸 helper 名先查 sandbox-ctl 同目录，再查 `PATH` |
| `network.tapfd.socket` | 绝对路径 | 常驻 provider socket；要求非空 request |
| `network.tapfd.request` | String | Socket 模式的 provider 字段；exec 模式禁止 |
| `network.tapfd.timeout` | Duration；handoff 默认 `5s` | 限制完整 handoff；空/非正值解析使用 handoff 默认值 |
| `network.mac` | String；空 | TAP 模式允许 CH 分配；TAPFD metadata 提供实际 MAC |
| `network.ip` | CIDR string；空 | Guest IPv4/IPv6 地址；TAPFD 提供地址并保留配置 mask |
| `network.mtu` | 整数；`0` | 零保持内核默认；MTU 由节点配置拥有 |
| `network.nexthop` | IP string；空 | 可选默认路由 |
| `network.hostname` | String；空 | Guest hostname |
| `network.interface` | String；有 source 时 `eth0` | 无网络 source 时所有 Guest 网络字段必须为空 |
| `boot.kernel` | 本地绝对 `file://` 引用 | Kernel binding；冷启动身份和可选 sidecar 规则见 [Runtime](runtime_zh.md#runtime-sdk) |
| `boot.runtime` | 本地绝对 `file://` 引用 | Runtime Bundle binding；恢复检查捕获的 basename/footer identity |
| `boot.cmdline` | String；空 | 与自动启动参数合并的额外参数；仅供冷启动 Host 输入 |
| `boot.root.base` | Artifact ref；single-disk 模式可省略 | Overlay 模式必须的 EROFS lower；single-disk 模式可选 ext4 lower |
| `boot.root.overlay` | Object；省略选择 single disk | 存在时选择独立只读 lower 和可写 ext4 upper |
| `boot.root.diff`、`boot.root.diff_template`、`boot.root.base_from_refs` | Local ref、local ref、有序 ref list | 仅 single-disk 的可写绑定、provisioning template 和 lower chain；与 overlay 互斥 |
| `boot.root.overlay.base`、`.base_from_refs`、`.diff`、`.diff_template` | Ref、ref list、local ref、local ref | 可写 upper 的不可变源链及 active binding；与 single-disk 同类字段规则相同 |
| `boot.disks[].name` | 唯一非空 String | 最多八个 data disk，按稳定 array 顺序排列 |
| `boot.disks[].base`、`.overlay`、`.diff`、`.diff_template`、`.base_from_refs` | 与 root 相同的 disk shape | 每个 data disk 遵循相同 single/overlay 源规则；冷启动恰好一个 `mounts[].type: disk` 消费它 |

`diff` 与 `diff_template` 是本地绝对文件引用；不可变 ref 可用受支持的 local/Manifest carrier。
Lower chain 要求 base。冷启动的可写 ext4 设备需要可挂载 base/template/已有 diff；空白文件不是文件系统。
自动 active diff 归 runtime 所有并在退出时移除；显式路径归调用者所有。容量来自所选磁盘源，
不是 quota 或 resize flag，见[容量与迁移](#disk-capacity)。

### 工作负载字段

| 字段 | 类型 / 默认值 | 校验与效果 |
| --- | --- | --- |
| `launch.exec`、`launch.args` | String、string list | 覆盖镜像 entrypoint/参数；single-disk root 要求显式 exec，除非使用 placeholder |
| `launch.env`、`launch.ephemeral_env` | String map | 持久环境与当前调用覆盖；见[合并规则](#files-and-environment) |
| `launch.workdir` | String；普通加载后 `/` | 应用工作目录；image/source projection 服从相应 launch merge 规则 |
| `launch.restart` | `never`、`on-failure`、`always`；`never` | 主进程监督 |
| `launch.cgroup_control` | Boolean；`false` | 启用应用 cgroup delegation；见 [Guest 拓扑](guest-abi_zh.md#application-cgroups) |
| `launch.placeholder` | Boolean；`false` | 无 exec anchor；与 exec 互斥并始终 restart |
| `launch.pid_namespace` | `private`、`shared`；`private` | Private 使应用成为 namespace PID 1；shared 使用 sandbox-init reaper |
| `launch.user` | 身份 String；镜像 user，否则 root | `uid:gid` 或 `name:group` |
| `launch.stop_signal` | 信号名/数字；镜像值，否则 `SIGTERM` | 停止信号 |
| `launch.stop_grace_period` | Duration；`10s` | SIGKILL 前宽限期，以整秒传给 Guest |
| `launch.start_timeout` | Duration；空/零无强制限制 | Host 等待 `launch_ack`，包含有序 init 工作 |
| `launch.plugin[].exec`、`.args`、`.env`、`.workdir`、`.user` | String/list/map/string/string | 同 workload root/cgroup 内的伴随进程；进程 ABI 见 [Guest ABI](guest-abi_zh.md#launch-spec) |
| `launch.plugin[].restart` | `never`、`on-failure`、`always`；`always` | Plugin 退出不 reboot sandbox |
| `mounts[].target`、`.type`、`.source`、`.options` | String；type 默认 `empty` | Type 为 `empty`、`tmpfs` 或 `disk`；disk source 命名一个 data disk；无声明式 NFS mount type |
| `files[].path`、`.content`、`.mode`、`.owner`、`.read_only` | String 与 Boolean；content 空，mode `0644`，owner `0:0`，read_only `false` | 注入文件声明；绝对路径、八进制 mode 和 Guest 身份规则见 [LaunchSpec](guest-abi_zh.md#launch-spec) |
| `ephemeral_files[]` | 同 `files[]` | 本次冷启动按路径覆盖持久声明；不进入 portable metadata |
| `init[].exec`、`.args`、`.env`、`.workdir`、`.user` | String/list/map/string/string | 应用启动前按顺序执行的一次性命令 |
| `init[].timeout` | Duration；空/零无限制 | 非零退出或 timeout 中止初始化 |
| `metadata` | String map；空 | Opaque portable 值，runtime 不解释 |

### Restore、Usage 与 deadline

| 字段 | 默认值 | 范围 |
| --- | --- | --- |
| `restore.prefetch` | `off` | `off` 或 `memory`；尽力预热当前 S，见[内存契约](memory_zh.md#prefetch) |
| `usage.enabled` | `false` | Host 采样和持久化，不可移植 |
| `usage.sample_interval` | `1s` | 正 duration；见[校验](#usage-policy) |
| `usage.flush_interval` | `5m` | Append 周期，至少等于 sample interval；不请求 fsync |
| `timeouts.ch_api` | 省略时 `60s` | 本地 CH API response；显式零移除强制 response 限制 |
| `timeouts.api_ready` | 无强制 timeout | 完整 API/restore-Paused 就绪屏障，包括连接后停滞的 response |
| `timeouts.va_report` | 无强制 timeout | CH 向 Host 交接 UFFD |
| `timeouts.restore` | 无强制 timeout | Guest restore ACK 与 MUX 建立 |
| `timeouts.ping` | 无强制 timeout | Host/Guest ping 往返 |
| `timeouts.app_notify` | 无强制 timeout | 单次 Guest launch-port 消息读取 |
| `timeouts.app_start` | 无额外整体 deadline | ACK 后 initial-launch 确认；正值显式启用，负值/非法值拒绝 |

所有 `timeouts.*` 字段必须可解析为 Go duration。除 `app_start` 外，当前 consumer 将非正值解析为无强制
timeout；dial/connect probe 仍有独立边界。已完成操作的 context 不拥有之后 Runtime 的生命周期。
Capture 命令的 `--timeout` 由 [Runtime 操作](runtime_zh.md#snapshot)单独定义。正的 ping fatal threshold
需要有界 ping 往返，才能检测已连接但无响应 Guest。[生产 timeout 示例](../examples/timeouts-production.yaml)
是显式策略选择，不是产品默认值。

<a id="from-rules"></a>

## `run --from` rules

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

<a id="files-and-environment"></a>

## files、env 与 ephemeral

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

## Usage policy

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

## `resources.diff_cow` 与缓存所有权

```yaml
resources:
  diff_cow:
    cache_size: 32MiB
    max_dirty_size: 16MiB
```

两个字段都必须是正数且为 4 KiB 的整数倍，满足 `0 < max_dirty_size <= cache_size`。
省略 `cache_size` 时默认为 32 MiB。省略 `max_dirty_size` 时，根据最终生效的缓存大小推导：
取缓存的一半，向下取整到 4 KiB 的整数倍，最小为 4 KiB
（`max(4096, floor(cacheBytes / 2 / 4096) * 4096)`）。两个字段均省略时，包括没有
`diff_cow` 的旧配置，实际默认值仍为 32 MiB / 16 MiB。只设置 `cache_size: 64MiB` 时，
脏缓存上限为 32 MiB；缓存大小为 4 KiB 或 12 KiB 时，脏缓存上限均为 4 KiB。

显式设置 `max_dirty_size: 16MiB` 后，即使后续覆盖缓存大小，该值也保持不变。
省略的脏缓存上限在所有覆盖完成后推导；应用默认值或加载配置不会将推导值写入源配置。
显式脏缓存上限不会被截断或重新计算，最终两个值必须满足上述大小关系（因此只设置
64 MiB 脏缓存上限时，会因默认缓存仅为 32 MiB 而校验失败）。这些是有限的工程初值，
不是生产 SLA 或最优性能声明。没有零值/无限制、
关闭开关、数据 I/O 失败后触发的 buffered 重试或可配置的其他写入模式。

`max_dirty_size` 是 `cache_size` 的子集，不是额外池或预留分区。没有脏页时 clean 可以使用
全部缓存。添加磁盘不会乘以预算。这些 host 进程资源独立于 guest capacity、allocatable/startup
headroom 和 VMM overhead：`sandbox-ctl` 不在 VMM cgroup 中，无需改资源控制器。

冷启动、`run --from` 与 restore 为当前实例解析 host resources。运行配置保留该沙箱生命周期的
策略；新恢复实例使用本次 host 配置。Portable C0/E/S 不包含缓存策略、页、队列或 LRU 状态。
严格 restore 校验、配置合并与 host projection 都保持此边界。

Go 调用者可通过 `diffstore.WithCache` 向多个 `diffstore.OpenDisk` 传入同一 `diffstore.Cache`。先关闭 COW，
再关闭共享缓存。不传该 option 时，独立 COW 持有有限默认缓存并自行关闭。

<a id="disk-capacity"></a>

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

模板是只读的初始化输入，与活动 DIFF 工作文件不同。既有 raw ext4 准备方式及加密
模板读取继续受支持。运行时读取模板，另行创建 v3/v4 工作文件。可写逻辑容量必须为
正且按 4 KiB 对齐。单层 v3 支持最大 1 TiB；启用本地 Checkpoint 时使用 v4，其容量
上限仍为 32 GiB。需要更大的单层工作盘时，准备更大的文件系统模板即可，不引入独立
的大小配置项。

必须使用新的活动 diff：已有 diff 优先，因此更换模板不会调整容量或替换已有数据。
不要通过截断已有文件系统来实施更小的限制。Sandboxer 不会自动调整文件系统大小，
不会限制 COW 已物化的存储占用（包括 tmpfs 文件的内存/swap；待回写缓存由 `resources.diff_cow` 单独限制），也不会在启动或恢复时改变磁盘容量。逻辑块设备容量与加密
文件的物理长度、宿主磁盘实际分配空间、guest 文件系统可用于文件数据的空间并不是
同一个概念。本次迁移不提供从同一个模板任意选择各实例容量的新能力。

<a id="journal-output-targets"></a>

## Journal 输出目标

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

<a id="output-syntax"></a>

### 语法和编码

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

<a id="output-sources"></a>

### 组件日志与 Guest 输出

`run --log-to default`（省略时也采用此默认值）保留普通组件诊断的 stderr 输出。
`run --log-to journald=...` 则显式将 Go logger 输出和 run/restore 运行诊断发送到
该目标，包括目标解析完成之后的启动准备失败和终态错误。它不要求 stderr 已连接
journal，也不要求存在任何特定身份字段。

在可用目标建立之前，非法 flag 语法和非法 `--log-to` 值仍可写到原始 stderr。
`--stderr-to` 始终是 guest 应用的 stderr 目标。`--log-to` 不接管进程描述符，不改变
TTY/pipe 选择，不重定向 Cloud Hypervisor 自身的 stderr，也不捕获 runtime panic 或
系统事件。既有消息文本和日志时间精度保持不变。`exec` 自身的诊断 stderr 及错误捕获
行为不变；文件、artifact、JSON 及其他非 journal stdout 数据不受影响。

<a id="output-lifetime"></a>

### 分行、失败与生命周期

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

<a id="related"></a>

## 相关文档

[Runtime：启动、控制、捕获与恢复](runtime_zh.md) | [Sandbox 与 Snapshot 制品](artifacts_zh.md) | [Guest 内存与按需恢复](memory_zh.md) | [块存储：设备、可写状态与 Checkpoint](block-storage_zh.md) | [资源控制与 Usage 计量](resources_zh.md) | [Guest ABI：启动、应用与 Host 协议](guest-abi_zh.md) | [VMM 集成与 Cloud Hypervisor 补丁](vmm_zh.md)

[组件入口](../README_zh.md) · [构建与补丁材料](../native-deps/README_zh.md)
