[English](README.md) | [简体中文](README_zh.md)

# sandboxer

首次运行跨组件沙箱或 Demo，推荐从项目[快速开始](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart_zh.md)
使用匹配聚合版本的 [workbench](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/workbench/README_zh.md)。
本组件仍可独立构建和部署，workbench 不是生产运行的强制依赖。预构建架构以所选
发布版的实际资产为准；源码支持某架构不意味着所有历史版本都提供该架构制品。

`sandboxer` 是 [Kuasar Sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox) 的 **MicroVM 生命周期引擎**。它创建 Sandbox,捕获并恢复状态,控制 guest,提供块设备,按需加载内存和磁盘数据,并协调每个 Sandbox 的 cgroup、balloon 与 VMM 生命周期。

本仓既是完整 Kuasar Sandbox 平台的组件,也可作为独立 Runtime 引擎使用。它提供窄 Go package 和 host-local 协议,不会把编排、存储服务或 eBPF 实现引入 Runtime 依赖闭包。

## 提供的能力

- 从已展平 image 冷创建 Sandbox;
- 从 snapshot template 创建相互独立的实例;
- 捕获 snapshot、在线 export 和发布，并可选择恢复原 runtime 继续运行;
- 通过外部 memfd/userfaultfd 路径按需加载 guest memory;
- 为本地文件或 Manifest-backed 数据提供 vhost-user block backend,并维护实例独立的 copy-on-write diff;
- 通过 vsock 和多路复用 stdio/port-forwarding 路径受控通信;
- 在平台授权后供可信 node service 使用的 host-local control socket;
- 通过 cgroup 与 virtio-balloon 协调每个 Sandbox 的资源执行;
- 接收 `connector` 交接的 TAP file descriptor;
- 组装和发布 image-class 与 snapshot artifact 的 package API。

Snapshot 复用由显式 parent/child 引用表示。从同一 template 创建的多个实例共享 immutable parent state,同时保有独立 writable change。Runtime 不要求多个独立运行的 VM 生成高度相同的 memory snapshot。

## 主要二进制

| 二进制 | 用途 |
| --- | --- |
| `sandbox-ctl` | Host CLI：run/restore、exec、snapshot、export、reset、checkpoint 管理、usage、info、publish 与配置 |
| `sandbox-init` | Guest PID 1:分阶段初始化、应用监督、vsock 控制、stdio 多路复用和 guest 生命周期响应 |

`sandbox-init` 在本仓构建,再由 [`guest-runtime`](https://github.com/kuasar-sandbox/guest-runtime) 打包进 `sandbox-runtime.bundle`。

## 公开集成 package

| Package | 用途 |
| --- | --- |
| `pkg/runtime` | 公开 Runtime SDK：`StartRuntime`、`Launch`、`Start`、`Restore`、在线操作与 owner 生命周期 |
| `pkg/resource` | 节点资源控制 wire contract 与 client,由 `orchestrator` 使用 |
| `pkg/ctl` | Host-local control socket 协议和 `ServeExecTunnel` policy callback;`ProxyExec` 仅 relay 已连接 backend |
| `pkg/usage` | Host-only 累计资源用量、Snapshot、Record 及离线读取 |
| `pkg/usagereader` | CLI 与本机 conductor adapter 共用的无损 live/saved/history 读取 |
| `pkg/sandbox`、`pkg/restore`、`pkg/snapshot` | 公开 SDK 后的生命周期实现、源准备与捕获 helper |
| `pkg/artifact` | 向 Manifest store 或 named-location Bundle 进行 typed publication |
| `pkg/uffd`、`pkg/memory` | 按需 memory 加载与 memfd 所有权 |
| `pkg/vhost` | vhost-user-blk 传输、队列与 Guest 内存 |
| `pkg/diffstore` | [可写磁盘、共享缓存、单层/checkpoint 存储](docs/block-storage_zh.md) |
| `pkg/guestlink`、`pkg/mux`、`pkg/proto`、`pkg/fwd`、`pkg/stdio` | Host/guest 控制、多路复用和 forwarding |
| `pkg/config`、`pkg/resctl`、`pkg/chapi`、`pkg/tapfd` | Runtime 配置、资源执行、Cloud Hypervisor API 和 TAP 交接 |

## 数据路径

生命周期层通过同一 sparse-data interface 使用本地文件和 Manifest-backed content。因此部署可将 snapshot 放在本地存储、NAS/NFS 等共享文件系统,或带 cache 的 object storage 上。编排 API 与所选 backend 保持独立。

Memory 按 page fault-in,磁盘按 block 读取。未访问的数据无需在恢复后的 Sandbox 继续运行前全部加载。

## 构建与测试

构建使用环境提供的 Go，并继承 `GOROOT`、`GOTOOLCHAIN` 等工具链选择；发布自动化需要环境在 `PATH` 中提供支持 `api --slurp` 的 `gh`。项目不下载、替换或按固定二进制摘要认证这些环境工具。 Rust 构建使用环境中的 `cargo`、`rustc` 和目标链接器，不要求由 rustup 管理。

CI 在一次运行中为各原生架构选择已通过验证的不可变 Workbench 镜像.
发布构建、普通测试和打包使用 build 模式, 在原生 x86_64 和 aarch64 Runner 上
以 ordinary UID 执行. 完整的 `bash scripts/ci-source-checks.sh` 门禁在 Workbench
system 模式运行, 保留源码测试、race、vet 及真实 mount namespace ENOSPC 检查.
门禁需要这些测试实际使用的 namespace 和 mount 能力.
`KUASAR_BUILD_JOBS` 限制构建并发,源码、临时目录、Cargo 与 native
cache 均属于本次任务. 产品 E2E 仍使用精确预构建制品. 发布凭据只用于 Actions
编排步骤;publisher 通过 `RELEASE_ARCHIVE_VALIDATOR` 执行由 Workbench 编译的
受信 workflow revision 对应的归档解析器.

```bash
make build                      # sandbox-ctl and sandbox-init
make sandbox-ctl sandbox-init   # explicit binary targets
make build TARGET_ARCH=aarch64  # cross-compile; amd64/arm64 aliases are accepted
make vet test                   # static checks and unit tests
make test-e2e-scripts           # 源码与测试辅助代码回归；不是产品 E2E
```

产品 E2E 使用发布/预构建制品准备的平台工作区；`PREPARED` 指向该工作区。
手工和 CI 使用相同运行器；执行过程不需要兄弟仓源码或 Go/Rust 编译器。
请在 root shell 中执行以下命令，并将所需运行工具放在 `PATH` 中。统一运行器
不会自动提权；已选运行时 case 缺少 root 权限或 KVM 时会失败。

```bash
python3 "$PREPARED/test/e2e/e2e" run --workdir "$PREPARED" \
  --suite sandbox --suite snapshot --suite telemetry \
  --include basic.sandbox-cli.sh --include image.manifest-boot.sh \
  --include image.sandbox-assembly.sh --include network.tapfd.sh
```

前置条件:

- 源码构建使用 Go 1.26.1+;
- 真实 Runtime 操作需要 Linux 和 root 权限;
- MicroVM E2E 需要 KVM 和相应 kernel interface;
- 需要 `guest-runtime` 提供 `sandbox-runtime.bundle` 和 VMLinux;
- 需要 `sandboxer/native-deps` 构建的 patched Cloud Hypervisor。

Unit/static 检查不能证明 privileged KVM、TAP、cgroup、vhost 或 restore 路径已执行。被 skip 的 privileged suite 不能表述为已完成集成验证。

## Cloud Hypervisor 边界

`sandboxer` 维护项目 Cloud Hypervisor patch set 和构建支持。Patch set 提供 Runtime 所需的 external-memory、userfaultfd、balloon 和生命周期合同。上游源码、精确 patch target、构建输入及许可证边界记录在 [`docs/vmm_zh.md`](docs/vmm_zh.md) 和 [`LICENSE_SCOPE_zh.md`](LICENSE_SCOPE_zh.md)。

Cloud Hypervisor 仍是采用上游许可证的第三方软件。项目原创 Go code 不会重许可上游代码,也不会重许可保留上游 copyright 和 license 的 patch。

## 跨仓依赖

Go import surface 保持窄边界:

| 仓库 | Import surface | 用途 |
| --- | --- | --- |
| `accelerator` | Manifest、cache/store client、sparse/image helper | Snapshot ingest/fetch、block read 和 flattened-image 配置 |
| `connector` | `pkg/tapfd` | 接收 TAP 与 network namespace file descriptor |

独立 Go 构建使用 `go.mod` 中真实可获取的依赖版本。协调 Preview 构建时，检出选定的 `sandboxer`、`accelerator`、`connector` Tag，并只为这三个模块生成工作区：

```bash
GOWORK="$(python3 ../kuasar-sandbox/ci/integration/go_workspace.py --root .. --owners sandboxer)" || exit 1
export GOWORK
make build
```

工作区通过 `use` 选择本地模块，不额外添加 `replace`，也不改写 `go.mod`/`go.sum`；helper 构建继承同一选择。Patched Cloud Hypervisor、Guest Runtime、Kernel 仍是独立的 Native/Runtime 前置。完整系统见[项目工作区](https://github.com/kuasar-sandbox/kuasar-sandbox)。


## Release 模型

共享 Preview 保留、清理、资产完整性检查与 Latest 协调由[项目发行契约](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release_zh.md)统一定义。以下保留本组件的构建输入、包内容与 helper 选择规则。

所选 owner 源码在 `release/helper-inputs.json` 声明完整的编译型 E2E helper 清单。配方 `scripts/build-e2e-helpers.sh` 接收目标架构和全新的输出目录，仅在源码构建阶段使用已准入的依赖工作区执行；打包 prepare 和用例执行消费经校验的二进制。内部构建目标和包路径由本仓维护。

`make release` 先核对 `ACCELERATOR_VERSION`、`CONNECTOR_VERSION` 与干净的相邻源码 checkout，生成同一 Go workspace，再编译并打包本次输出。项目 checkout 不在 `../kuasar-sandbox` 时设置 `KUASAR_PLATFORM_DIR`。缺 Tag、缺模块或源码目录冲突会在编译前停止。包内保留许可证及运行文件，不携带来源或历史测试记录。

`sandboxer` 独立发布 `vX.Y.Z` 组件版本。已发布原生架构的 archive 包含 `sandbox-ctl`、`sandbox-init` 和 patched `cloud-hypervisor` binary。聚合 platform 包按显式文档/测试选择携带用户指南与规范 E2E 运行输入，内部设计文档和源码专用测试保留在源码仓。

项目仓独立发布 `release-vX.Y.Z` 聚合版本,精确选择一个 `sandboxer` Tag 和其他各发行单元版本,并验证组合后的完整系统。

版本关系见[项目 Release 文档](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release_zh.md),可用的 Stable 聚合版本由 [GitHub Latest Release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases/latest) 渠道解析。

## 文档

独立使用从 [Runtime 完整流程](docs/runtime_zh.md#standalone-workflow)开始；组合多个组件的平台部署
从[项目快速开始](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/quickstart_zh.md)开始。
组件手册平铺于 `docs/`，每个主题同时保留理解一个状态 owner 或稳定边界所需的操作、精确契约和机制。

| 主题 | 适用任务 | 精确契约归属 |
| --- | --- | --- |
| [Runtime](docs/runtime_zh.md) | 启动、Launch、run、exec、snapshot、在线 export、恢复与关闭单个 VM | Runtime 状态、API/CLI 操作语义、context 与跨资源捕获 |
| [配置](docs/configuration_zh.md) | 准备和检查 sandbox.yaml 或 Host overlay | 字段、默认值、校验、合并和输出目标 |
| [制品](docs/artifacts_zh.md) | 组装 E、reset 保留状态、检查和发布 E/S | Portable schema、身份、依赖图、发布与保留 |
| [内存](docs/memory_zh.md) | 理解按需恢复和 checkpoint 内存 | Memfd 所有权、UFFD、分层读取、释放页与 dirty history |
| [块存储](docs/block-storage_zh.md) | 集成 Disk、操作磁盘 checkpoint 和诊断块 I/O | Vhost 传输、不可变 lower、cache、DIFF v3/v4 与回收 |
| [资源](docs/resources_zh.md) | 配置资源策略并读取 Usage | Capacity/Budget、cgroup、balloon、资源协议与累计计量 |
| [Guest ABI](docs/guest-abi_zh.md) | 准备工作负载或修改 Host/Guest 集成 | sandbox-init 阶段、进程、文件系统、vsock、MUX 与原始观测 |
| [VMM](docs/vmm_zh.md) | 构建匹配 monitor 或维护补丁 | Cloud Hypervisor/KVM 集成、设备模型、交接与补丁契约 |

八个主题均维护完整中英文版本；Native 构建与 license scope 也有双语指南。指南应与二进制使用相同源码或
所选发行版。[贡献策略](CONTRIBUTING.md#documentation)规定主题归属，以及重组时如何保留维护中的契约。

## 项目边界

- Node/cluster 编排属于 [`orchestrator`](https://github.com/kuasar-sandbox/orchestrator);
- 数据访问、存储、加密与缓存属于 [`accelerator`](https://github.com/kuasar-sandbox/accelerator);
- MicroVM 网络属于 [`connector`](https://github.com/kuasar-sandbox/connector);
- Guest Runtime image 与 Kernel artifact 属于 [`guest-runtime`](https://github.com/kuasar-sandbox/guest-runtime);
- 系统设计、共享集成测试与聚合 Release 属于 [`kuasar-sandbox/kuasar-sandbox`](https://github.com/kuasar-sandbox/kuasar-sandbox)。

## 贡献与安全

阅读[组织贡献指南](https://github.com/kuasar-sandbox/.github/blob/main/CONTRIBUTING.md)。修改 exported package 或跨仓合同必须双向关联 companion PR,并通过精确源码组合的项目验证。

不要在公开 Issue 报告漏洞。按 [Kuasar Sandbox 安全策略](https://github.com/kuasar-sandbox/kuasar-sandbox/security/policy)使用 GitHub private vulnerability reporting。

## License

项目原创内容采用 [Apache License 2.0](LICENSE)。Cloud Hypervisor 和其他第三方边界记录在 [`LICENSE_SCOPE_zh.md`](LICENSE_SCOPE_zh.md)。保留上游 copyright、attribution、NOTICE 和 SPDX declaration。
