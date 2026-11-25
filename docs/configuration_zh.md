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
  diff_cow: { cache_size: 32MiB, max_dirty_size: 0, discard: auto }

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

Template 同时标明 portable、host-only 和 ephemeral 字段。Restore mode 输出 host-only 文档，删除 cold-only workload、`resources.diff_cow.discard` 与 immutable disk refs，同时保留缓存预算；它不会生成第二套 restore schema。

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

Restore 拒绝显式提供 `boot.cmdline`、`resources.diff_cow.discard`、launch（包括 ephemeral
environment 和 plugin）、mounts、files/ephemeral_files、init、metadata。可以提供当前 Host 网络身份/provider、资源执行、runtime
本地绑定、active writable binding、prefetch、usage 和 timeout 策略，但仍须满足捕获拓扑和身份检查。
旧版 `boot.kernel` 字段被接受但忽略，不检查外部 kernel 身份。
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
| `resources.diff_cow.max_dirty_size` | Size；`0` | 整体脏页额度；0 不额外限制，正值为 4 KiB 整数倍且不超过 `cache_size` |
| `resources.diff_cow.max_dirty_size_per_disk` | Size；`0` | 统一的单盘脏页额度；正值为 4 KiB 整数倍 |
| `resources.diff_cow.read_bandwidth`、`write_bandwidth` | Size/秒；`0` | 整体读/写入口 payload 字节率 |
| `resources.diff_cow.read_iops`、`write_iops` | 非负整数；`0` | 整体完整读/写请求次数/秒 |
| `resources.diff_cow.read_bandwidth_per_disk`、`write_bandwidth_per_disk` | Size/秒；`0` | 每块逻辑盘独立使用的统一带宽上限 |
| `resources.diff_cow.read_iops_per_disk`、`write_iops_per_disk` | 非负整数；`0` | 每块逻辑盘独立使用的统一请求速率上限 |
| `resources.diff_cow.cache_idle_after` | 正时长；默认 `10s` | Clean 页多久未访问后计入闲置缓存统计；不失效、不立即回收 |
| `resources.diff_cow.discard` | `auto` 或 `manual`；`auto` | 所有受管可写 ext4 的冷启动挂载策略；内存恢复拒绝显式输入；见[策略说明](#diff-cow-policy) |

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
| `boot.kernel` | 本地绝对 `file://` 引用 | 冷启动 kernel binding；内存恢复接受但忽略此字段，不依赖外部 kernel。冷启动身份/sidecar 规则见 [Runtime](runtime_zh.md#runtime-sdk) |
| `boot.runtime` | 本地绝对 `file://` 引用 | Runtime Bundle binding；恢复匹配捕获的 digest scheme 和 footer 声明摘要，不比较 basename。原 basename 仅用于同目录候选查找；见[选择规则](runtime_zh.md#restore) |
| `boot.cmdline` | String；空 | 与自动启动参数合并的额外参数；仅供冷启动 Host 输入 |
| `boot.root.base` | Artifact ref；single-disk 模式可省略 | Overlay 模式必须的 EROFS lower；single-disk 模式可选 ext4 lower |
| `boot.root.overlay` | Object；省略选择 single disk | 存在时选择独立只读 lower 和可写 ext4 upper |
| `boot.root.diff`、`boot.root.diff_template`、`boot.root.base_from_refs` | Local ref、模板引用、有序 ref list | 仅 single-disk 的可写绑定、provisioning template 和 lower chain；与 overlay 互斥 |
| `boot.root.overlay.base`、`.base_from_refs`、`.diff`、`.diff_template` | Ref、ref list、local ref、模板引用 | 可写 upper 的不可变源链及 active binding；与 single-disk 同类字段规则相同 |
| `boot.disks[].name` | 唯一非空 String | 最多八个 data disk，按稳定 array 顺序排列 |
| `boot.disks[].base`、`.overlay`、`.diff`、`.diff_template`、`.base_from_refs` | 与 root 相同的 disk shape | 每个 data disk 遵循相同 single/overlay 源规则；每块盘在冷启动时至少有一个 `type: disk` 挂载目标 |
| `boot.disks[].slot` | `{}` 或 `{size: 8GiB}` | 运行期绑定的单设备盘位 |
| `boot.disks[].disk` | 完整的 DiskV1 | 从相同盘定义冷启动，使用本实例的私有可写层 |

`diff` 是本地绝对文件引用；`diff_template` 使用下文的五类模板引用。不可变 ref 可用受支持的 local/Manifest carrier。
Lower chain 要求 base。冷启动的可写 ext4 设备需要可挂载 base/template/已有 diff；空白文件不是文件系统。
普通非 checkpoint 模式下，自动 active diff 归 runtime 所有并在退出时移除；显式路径归调用者所有。
启用 `--enable-checkpoint` 后，可写文件归于派生的 checkpoint 目录，包括自动选择的路径，正常退出后也保留。
该模式的[归属与清理规则](block-storage_zh.md#checkpoints)由块存储契约定义。容量来自所选磁盘源，
不是 quota 或 resize flag，见[容量与迁移](#disk-capacity)。

### 延迟绑定与子目录挂载

以下配置为 `input` 保留一个 virtio-blk 设备。固定容量和动态容量的未绑定盘位都依赖 Guest 内核的 autofs 在首次访问时挂载目录。缺少 autofs 的 Guest 无法使用这些盘位。绑定前访问目录返回可重试错误，绑定后第一次访问时挂载 ext4 并投影对应子目录。普通 single/overlay 数据盘也使用同一组 `mounts`。slot 的目标与其他有效挂载不能重叠，同一 slot 投影的源目录也不能重叠。其余挂载沿用既有顺序。新挂载声明中的 `target` 和 `subpath` 遇到 `.` 或 `..` 路径分量会报错，省略 `subpath` 或使用空串表示整盘根。
```yaml
boot:
  disks:
    - name: input
      slot: {}
mounts:
  - {type: disk, source: input, subpath: data, target: /data}
  - {type: disk, source: input, subpath: results, target: /out}
```

省略 `slot.size` 时，盘位在绑定后采用来源容量。指定时，容量必须与来源完全匹配。Host 接入块后端并提交盘记录时，绑定完成。随后捕获的快照包含已绑定的盘。

动态盘位（`slot: {}`）的初始容量为零。若 VMM 没有主动通知容量变化，它必须在查询时返回已绑定盘的实际容量，Guest 内核也必须在打开块设备时刷新容量。实际运行的 Guest `vmlinux` 须包含 guest-runtime #134 的 virtio-blk 补丁或具备等效能力。缺少这项能力时，`disk bind` 可以成功，但首次访问目录可能失败。若实际运行的内核缺少这项能力，或无法确认（例如复用了旧版 `vmlinux`），建议使用 `slot: {size: 8GiB}` 等固定容量盘位，并接入等大的盘。[Guest 内核补丁](https://github.com/kuasar-sandbox/guest-runtime/pull/134)

列出目标实例的盘位后，可以导出已保存 E 或 S 中的一块盘，将完整 JSON 交给另一个实例。整机 checkpoint 也支持此导出命令。
```bash
sandbox-ctl disk list --path-id receiver
sandbox-ctl export --source-ref "$PAUSED_E" --disk work --output "$OUT" > work.disk.json
sandbox-ctl disk bind --path-id receiver --disk input --disk-spec work.disk.json
```

单盘导出与 Sandbox 发布遵循相同的依赖规则。已发布的纯块层 `manifest://` 引用，以及带不可变身份的具名文件引用（包括 Bundle 选择器），会原样保留，不打开其 payload。本地 `--output DIR` 只将需要物化的层写入 Bundle；`--upload` 将这些层写入配置的 Manifest Store；`--upload --to-ref-location NAME=file:///dir` 则发布到指定具名目录，语义与 `sandbox-ctl publish --to-ref-location` 一致。完整盘图已经发布时，不新建 Bundle，也不写入 Store 或具名目录。返回的 `DiskV1` 始终列全有序层图，可以同时包含新生成和原样保留的引用。

更换输出目录不会搬迁已经发布的父层。接收端仍需能够访问这些父层，并提供相应的命名位置映射和密钥。源沙箱私有的 checkpoint 版本和未发布 Bundle 成员需要物化，清理源沙箱后导出的盘仍可使用。缺少已记录容量时读取顶层元数据、转换旧式 Sandbox 包装的磁盘层，或在层数超限时合并完整视图，仍会读取所需来源。初始化新盘时，`--disk-spec` 也可读取含 `kind: disk`、`version: 1` 以及 `diff_template` 或 size-only 创建容量的 JSON。绑定可以通过 `--ref-location NAME=file:///dir` 提供命名位置。

### 工作负载字段

| 字段 | 类型 / 默认值 | 校验与效果 |
| --- | --- | --- |
| `launch.systemd` | `off`、`auto`、`runtime`、`provided`；省略继承 E，否则 `off` | 选择环境 PID 1；active 模式要求实际 Guest 宣告 `systemd_v1`。拒绝 null、Boolean、空字符串及其他值。 |
| `launch.exec`、`launch.args` | String、string list | 覆盖镜像 entrypoint/参数；single-disk root 要求显式 exec，除非使用 placeholder |
| `launch.env`、`launch.ephemeral_env` | String map | 持久环境与当前调用覆盖；见[合并规则](#files-and-environment) |
| `launch.workdir` | String；普通加载后 `/` | 应用工作目录；image/source projection 服从相应 launch merge 规则 |
| `launch.restart` | `never`、`on-failure`、`always`；`never` | 主进程监督 |
| `launch.cgroup_control` | Boolean；`false` | true 在用户可见 cgroup 根下放 controllers，将直接管理的进程放入 `/init` 叶组；false 直接使用 scoped 根。systemd 模式固定开启委托，同时保留此原始值供后续 direct 冷启动使用。全局层级由 Guest Runtime 决定。见 [Guest 拓扑](guest-abi_zh.md#application-cgroups)。 |
| `launch.placeholder` | Boolean；`false` | 无 exec anchor；与 exec 互斥并始终 restart |
| `launch.pid_namespace` | `private`、`shared`；`private` | Private 使应用成为 namespace PID 1；shared 暴露负责回收孤儿的 Runtime PID 1 |
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

### Systemd 启动策略

```yaml
launch:
  systemd: auto
```

`off` 使用直接监督；`runtime` 要求 Runtime 自带的 systemd；`provided` 要求工作负载
rootfs 提供 systemd。`auto` 在 rootfs 存在安装时选择它，否则选择 Runtime 安装；
选中安装校验失败则启动失败。active 模式共享 systemd 环境 PID namespace，并开启
cgroup 委托，但不重写 E 中的原始 `pid_namespace` 或 `cgroup_control` 值。

E 冷启动中，省略继承来源策略，显式 `off` 关闭继承策略；新的完整定义保存有效值。
S 恢复保持捕获时 manager 和进程状态，拒绝包括 `off` 在内的模式覆盖。
只有实际选中 Runtime 安装时，native exec 的默认 PATH 才包含私有工具目录；
调用者显式 PATH（包括空值）始终保留覆盖权。原有 Launch/OCI 环境无需新增来源标记。

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

受保护字段有冲突时明确报出 field context. Host不能通过省略或 YAML merge静默改变 disk graph. Persistent `mounts` 可以修改普通 tmpfs/empty mount 的声明,但 data-disk mount 的 source、target、subpath 和顺序 必须保持 artifact topology,不能借 mount override移动磁盘.

Network必须满足:

```text
portable network.enabled == host provider presence
portable interface       == explicitly supplied host interface
```

Direct EROFS E只有 read-only image. 目标 host必须提供可用 `diff_template`（含 `mkfs://ext4?size=...`），或已格式化的 explicit diff. 缺少可挂载 upper时在 controller/network/CH side effect 前失败.

最终调用普通 `sandbox.Run`;`run --from` 不进入 `restore.Run`. 从来源派生的冷启动解析目标 kernel/runtime 身份，并将其写入新 C0，同时保留生效的来源工作负载与磁盘图。来源 boot 身份不要求冷启动继续使用旧 kernel/runtime，也不放宽上面的受保护字段。内存恢复则保留捕获的 C0，需要兼容的本地 runtime 绑定，但不依赖外部 kernel。

默认 `run --from` 继续执行上述强 ownership rules. `--replace-boot` 只选择另一条显式、原子的 cold derivation:

```text
source E 的 non-boot portable defaults
  + presence-aware persistent overrides
  + host config 的完整 boot 值
  + host/instance-only bindings
  -> ordinary ValidateCold / artifact preflight / ProjectPortableCold
```

完整 replacement 同时覆盖 `boot.kernel`、`boot.runtime`、`boot.cmdline`、`boot.root` 和整个 `boot.disks[]`;不存在 root-only 或 leaf-level graph merge. Host config 没有写 `boot.disks` 就表示零个 data disk，而不是继承来源 disks. 若来源 mounts 仍引用已移除或改名的 disk，调用方必须同时替换 mounts；普通 cold disk mount topology validation 会在 controller、cgroup、run directory、network 或 VM side effect 前拒绝不一致结果.

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

Restore host若显式提供 `boot.cmdline`、`resources.diff_cow.discard`、launch persistent/ephemeral fields、mounts、files/ephemeral_files、init 或 metadata,会在副作用前拒绝,而不是静默忽略. `resources.startup` 是 host-only node policy,可在 restore 时提供;它写入 node reservation contract,但 Snapshot 捕获的 `BudgetAtSnapshot` 仍是 restore initial Budget 的权威值.

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

## `resources.diff_cow`：缓存、磁盘 QoS 与挂载策略

```yaml
resources:
  diff_cow:
    cache_size: 32MiB
    cache_idle_after: 10s
    discard: auto
    max_dirty_size: 0
    read_bandwidth: 0
    write_bandwidth: 0
    read_iops: 0
    write_iops: 0
    max_dirty_size_per_disk: 0
    read_bandwidth_per_disk: 0
    write_bandwidth_per_disk: 0
    read_iops_per_disk: 0
    write_iops_per_disk: 0
```

`discard` 只接受 `auto` 或 `manual`；合并 Host 覆盖后，省略时取 `auto`。普通冷启动和
`run --from` 使用本次 Host 策略控制所有受管可写 ext4 挂载：

| 策略 | 挂载选项 | 效果 |
| --- | --- | --- |
| `auto`（默认） | `discard` | 文件系统释放块时可自动通知后端，减少过期的活动 payload 及后续磁盘导出大小。 |
| `manual` | `nodiscard` | 关闭文件系统释放块时的自动 discard，仍可在 Guest 中显式 trim；回收和导出收益取决于执行 trim 的时间。 |

该策略不关闭 virtio-blk DISCARD，也不改变后端 Zero 与读取语义。`manual` 要求 Guest
runtime 声明支持；参见[挂载范围与兼容性契约](guest-abi_zh.md#guest-block-discard)。
该字段不写入制品的 Portable 配置，因此从导出的 Sandbox E 冷启动时采用目标 Host
的选择。Snapshot S 仍捕获 Guest 挂载状态。内存恢复保留捕获时的挂载，不重新挂载：
`run --restore` 显式提供 `auto` 或
`manual` 均会被拒绝，应省略此字段。`config --mode restore` 删除此叶字段，保留
所有缓存及 QoS 配置。

Go SDK 调用方应在 `StartRuntime` 之前通过 `RuntimeSpec.Resources.DiffCOW.Discard`
选择策略；runtime 在工作负载 Launch 时使用该选择，包括对仍存活的 `runtime_ready`
预热实例执行 Launch。

`cache_size` 必须是正的 4 KiB 整数倍，默认为 32 MiB。十个限制字段最终默认值均为
`0`，表示不对该维度增加限制。分层配置中，省略某个叶字段会保留前层值，显式 0 会清除
该叶字段继承的限制；所有 Host 覆盖完成后才解析最终默认值。IOPS 必须是非负整数；带宽
沿用 size 单位，隐含“每秒”。`50M`、`50MB` 和 `50MiB` 均表示每秒 50 × 1024² 字节，
不要求按 4 KiB 对齐，但必须能表示为整数个字节。

正值 `max_dirty_size` 必须按 4 KiB 对齐且不大于 `cache_size`。正值
`max_dirty_size_per_disk` 只要求按 4 KiB 对齐并能由 Host 地址空间表示，允许大于整体
脏页额度或缓存大小；实际使用仍受更小的适用额度限制。脏页额度不创建独立池、不预留
容量，Clean 页仍共用缓存。在支持节点缓存授额的环境中，实际占用还受当前授额约束，
增加磁盘不会成倍增加整体预算。脏页额度包含正在构造的预留以及回写中的冻结页，提交
I/O 不释放额度；覆盖已脏页以及 Data/Zero 转换不重复计费。每个活动可写盘有独立的
异步回写 worker，共享缓存及全局 fatal 错误语义保持一致。

读写请求在进入存储前，必须同时满足整体与所属逻辑盘的带宽、IOPS 条件。每个完整
请求按全部 payload 计费一次，包括缓存命中及远程 base 读取。Overlay root 的 lower
与 upper 共用一份逻辑盘额度；内部回写、远程重试、FLUSH 与 DISCARD 不再扣除一份
读写入口额度。它限制的是进入后续链路的需求，不等同于物理盘实测速率，不承诺每盘
最小份额，也不为其他磁盘预留缓存。

入口允许一个完整请求的初始突发，之后按其完整成本控制后续请求准入；空闲不会积累
突发额度。超过一秒带宽额度的大请求允许执行，其完整成本会推迟下一个请求。排队中的
额度等待可被 stop、queue disable 与 snapshot quiesce 打断，恢复后无需 Guest 再次
kick 即可重试尚未开始的请求；已开始的 I/O 仍须完成排空。策略在创建 runtime 时固定，
预热实例 Launch 与磁盘 slot 绑定不会重置额度；内存恢复使用目标 Host 合并后的 QoS
策略，制品不携带该策略。

**升级注意：** 旧版省略脏页额度时取最终缓存的一半，本轮改为不增加额外限制，实际
脏页内存仍受缓存授额约束。旧版规范化配置中已写出的显式正值继续生效；重新生成配置
或覆盖 `max_dirty_size: 0` 才会采用新默认。应先升级 `sandbox-ctl` / Sandboxer SDK，
再使用新 Orchestrator 默认配置：旧版既拒绝新增字段，也拒绝显式的 0 脏页额度，而新版
默认 Host YAML 会包含这个 0。缓存本身没有无限制大小、关闭开关或 DIO 数据操作失败
后的 buffered 重试。上述控制是 Host policy，不进入 Sandbox E 与 Snapshot S 的
portable 身份。

缓存物理页仍计入 `sandbox-ctl` 所属 `ctl/` cgroup，不修改 Guest 内存预算、balloon
或 `vmm/` 控制。共享物理盘的 runner/builder unit 配置与有条件生效的 DIO 优先级见
[磁盘 QoS 部署指南](disk-qos-linux_zh.md)。


`cache_idle_after` 仅控制 Clean 页闲置状态的统计时间，允许按业务复用周期调优。
超过时长仍保持页可读、可命中；再次访问即撤销闲置标记。闲置容量累计达到一个
4 MiB 内部单位时，沙箱才启动温和的异步回收；优先归还空闲缓冲，必要时
直接按 Clean LRU 淘汰，不再检查该页的闲置时长。
节点遇到内存竞争时也仅提出建议回收额度，使用同一回收流程；实际可以不足额
甚至归还 0 字节，回收尝试完成后通知节点。只有物理内存真正释放后才扣减授额。
首次和运行期均使用 4 MiB 的内部授额单位，允许合法的更小缓存尾部。

弹性缓存要求宿主页大小为 4 KiB，并在自己的内存映射上禁用透明大页，
使每次分配和归还都能按授额计量。宿主无法提供该粒度、未配置控制器或对端
未协商缓存授额时，使用固定容量缓存。
默认 `cache_size` 仍为 32 MiB；需要 500 MiB–1 GiB 高峰工作集时，应显式设置
`cache_size: 1GiB`，由节点资源池决定实际可获准的当前额度。

冷启动、`run --from` 与 restore 为当前实例解析缓存预算。运行配置保留该沙箱生命周期的
预算；新恢复实例使用本次 Host 缓存配置。Portable C0/E/S 不包含缓存策略、页、队列或 LRU 状态。
严格 restore 校验、配置合并与 host projection 都保持此边界。

Go 调用者可通过 `diffstore.WithCache` 向多个 `diffstore.OpenDisk` 传入同一 `diffstore.Cache`。先关闭 COW，
再关闭共享缓存。不传该 option 时，独立 COW 持有有限默认缓存并自行关闭。

<a id="disk-capacity"></a>

## 可写磁盘容量与配置迁移

已有且有效的活动 `diff` 保留其逻辑容量和内容，不打开初始化模板：缺失模板文件、不可用的
Store、模板密钥及未使用的生成器限制都不影响重开。已有空盘或损坏 writer 直接报错，绝不
自动格式化。没有 writer 时由所选模板确定容量；没有模板时，新 COW 层继承 base 容量。
恢复使用捕获的磁盘图，不会用初始化模板替代缺失或损坏的捕获内容。

### 模板引用

现有 `diff_template` 字符串唯一选择一个逻辑来源，不增加 `diff_template_format`，不按
扩展名猜测，也不在失败后尝试另一种格式。

| 引用 | 所选来源 |
| --- | --- |
| `file:///path/empty.ext4` | raw 稀疏磁盘，兼容现有文件写法 |
| `file:///path/empty.tar@digest:<digest>` | 明文 canonical tarstream |
| `file:///path/empty.tar@hmac:<digest>` | 加密 canonical tarstream |
| `file:///path/empty.bundle@manifest:<content-key>` | Bundle 内显式选择的 Manifest |
| `manifest://<content-key>` | Manifest Store/cache |
| `mkfs://ext4?size=10GiB` | 内建确定性空 ext4 |

文件沿用绝对路径或命名位置规则。例如 `file://empty.ext4@location:seeds` 使用
`--ref-location seeds=file:///templates`。身份 selector 位于 `@location` 之前。裸
`.bundle` 路径仍选择 raw；Bundle 模板必须显式提供 `@manifest`。所选 Bundle 必须含有
root 和所需全部 Chunk，不从其他 Store 或 Bundle 补齐缺失内容。base/snapshot 的通用
文件检测行为不变。

tarstream 身份承诺 canonical 载体及其稀疏逻辑内容，不是 raw 文件或展开磁盘的 SHA256。
Bundle selector 是内部 Manifest ContentKey，不是包文件哈希。模板强制验证 Manifest/Chunk
身份，即使普通读取的 `manifest.verify_content` 为 false。tarstream 随机读取和 ext4 预检
不等于完整摘要校验；完整验证必须在 `WaitMaterialReady` 前成功。源加密沿用 local/ref/Manifest
规则，与目标 DIFF 加密独立；明文 raw 可以初始化加密 DIFF。

`mkfs://ext4?size=10GiB` 只接受一个必填 `size` 参数。`10G`、`10GB`、`10GiB` 和
`10240MiB` 都精确表示 10,737,418,240 字节，内部规范化为字节 URI。只有实际选择新建时才
应用生成器的 4 KiB 对齐及 1 MiB–1 TiB 范围限制。活动单层 v3 最大 1 TiB，checkpoint v4
仍最大 32 GiB。生成器不调用宿主 mkfs，也不创建中间镜像；它计算布局，并将请求的元数据
直接编码到调用方缓冲区。Hole 不产生 payload 读取或填充。

新建一个空白的 512 MiB scratch 文件系统：

```yaml
boot:
  # 保留完整配置中的其他必需 boot 字段。
  disks:
    - name: scratch
      diff_template: mkfs://ext4?size=512MiB
mounts:
  - { target: /scratch, type: disk, source: scratch }
```

使用 mkfs 时，所选可写设备不能有 COW base；双设备 overlay 的独立只读 EROFS lower 可以
存在。外部模板保留 Hole 继承 base、Zero 遮蔽为零、Data 读取模板的语义，包括子页边界；
逻辑容量不能小于其 COW base。载体文件长度不是磁盘容量。普通 raw 模板继续支持，需要
外部镜像时，`truncate` 加 `mkfs.ext4 -O ^has_journal` 的制作方式仍有效。

### DiskV1 按容量创建

没有 base、父层和模板的 DiskV1，可以在必需的 kind/version 标记之外只给出字节容量：

```json
{"kind":"disk","version":1,"size":10737418240}
```

它等价于补入 `"diff_template":"mkfs://ext4?size=10GiB"`。创建归一化发生在解码校验、
绑定和来源身份计算前，使等价单位、size-only 简写和显式 mkfs 识别为同一创建。有显式来源时，
`size` 仍是容量断言，不能补救错误来源或孤立父层。断言也检查已有 writer；未使用的 mkfs URI
容量则不限制已有盘。只有 slot 声明仍未绑定，即使已声明固定容量。绑定需要完整显式磁盘；
固定 slot 容量必须匹配，动态 slot 采用所选盘容量。

旧 root/overlay 的 `diff_size` 和 `size` 继续停用。删除这些键，选择所需容量的文件系统来源、
使用 mkfs URI，或在完整 DiskV1 场景使用 `boot.disks[].disk.size`。不引入自动文件系统扩容、
截断或物理存储配额。

### 确定性空文件系统与兼容性

内部 `empty-ext4/v1` profile 固定 UUID、目录 hash seed、初始及扩展时间、hash signedness、
保留字节、元数据布局与稀疏区间分类。它使用 4 KiB block、256 字节 inode、目标 8 KiB/inode
及 flex_bg=8；初始根目录只有 `.`、`..`，无 journal、resize_inode 和 metadata checksum。
字节仅由容量与 profile 决定，不跟随宿主 mkfs 默认值。修改 profile 需要新的内部版本，
不提供 UUID、feature 或 seed 调整参数。

相同初始盘可在相同分块和加密代际下复用 Chunk/Manifest。文件系统 UUID 不承担实例身份。
Guest 写入仍使用正常时间、inode generation 和 overlay UUID；活动 DIFF 密钥及 nonce
保留既有随机性。无 journal 的 profile 不为运行中工作盘增加崩溃恢复保证。已有带 journal
的外部文件系统与捕获盘继续支持。

DIFF v1 加密 envelope 和 DIFF v3 工作文件不再作为模板载体。升级前须使用支持旧输入的版本
导出逻辑内容，不能改名或去掉头部冒充 raw。受支持 v3/v4 writer 的活动读写和恢复不受影响。
成功物化后，重开、导出和恢复不再依赖模板。逻辑容量与载体长度、物理占用及 Guest 文件系统
可用空间仍是不同概念。

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

显式 journal 组件诊断采用尽力发送，不等待 journal 队列空间。journal 拥塞或不可用时，
丢弃受影响的消息，不同步回退到 stderr，以免再次阻塞同一控制路径。journal 恢复后，
后续诊断可以继续发送。此策略适用于 `--log-to journald=...`，不适用于 Guest
stdout/stderr 或 console 输出流。

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

native 发送成功时不再复制到 stderr。journal 队列满时，Guest/console 流保留背压，
不进行忙重试。native 发送出错后，尽力写入原始 fallback 目标，并保留既有 `[tag]`
前缀。native 发送或 fallback 写入错误均不会终止沙箱。fallback 文本不保留结构化字段。
组件诊断采用上述非阻塞策略，不调用 fallback。

每个 journal 输出独立拥有发送连接；Guest 流的背压不会阻塞组件诊断发送端。
Close 按所选输出策略刷新最后半行后释放该连接。SDK 调用方对流输出使用
`journalio.Open(target, fallback)`，对尽力发送的组件诊断使用
`journalio.OpenDiagnostic(target)`。这些 API 不提供异步队列，也不承诺无损或恰好一次
持久化。

可以用 `journalctl WORKLOAD_ID=worker-42 -o json` 检查该命令可访问的 journal 字段。
跨节点历史需要单独的采集器或访问其他节点的 journal；本功能不增加 exporter。

协议参考：[Journal Native Protocol](https://systemd.io/JOURNAL_NATIVE_PROTOCOL/) 和
[Go net/url](https://pkg.go.dev/net/url)。

<a id="related"></a>

## 相关文档

[Runtime：启动、控制、捕获与恢复](runtime_zh.md) | [Sandbox 与 Snapshot 制品](artifacts_zh.md) | [Guest 内存与按需恢复](memory_zh.md) | [块存储：设备、可写状态与 Checkpoint](block-storage_zh.md) | [资源控制与 Usage 计量](resources_zh.md) | [Guest ABI：启动、应用与 Host 协议](guest-abi_zh.md) | [VMM 集成与 Cloud Hypervisor 补丁](vmm_zh.md)

[组件入口](../README_zh.md) · [构建与补丁材料](../native-deps/README_zh.md)
