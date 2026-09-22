[English](README.md) | [简体中文](README_zh.md)

# sandboxer

`sandboxer` 是 [Kuasar Sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox) 的 **MicroVM 生命周期引擎**。它创建 Sandbox,捕获并恢复状态,控制 guest,提供块设备,按需加载内存和磁盘数据,并协调每个 Sandbox 的 cgroup、balloon 与 VMM 生命周期。

本仓既是完整 Kuasar Sandbox 平台的组件,也可作为独立 Runtime 引擎使用。它提供窄 Go package 和 host-local 协议,不会把编排、存储服务或 eBPF 实现引入 Runtime 依赖闭包。

## 提供的能力

- 从已展平 image 冷创建 Sandbox;
- 从 snapshot template 创建相互独立的实例;
- 对一个逻辑 Sandbox 执行 pause、snapshot、export、publish、restore 和 resume;
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
| `sandbox-ctl` | Host 控制面:run、snapshot、export、publish、restore、inspect、execute 和 render config |
| `sandbox-init` | Guest PID 1:分阶段初始化、应用监督、vsock 控制、stdio 多路复用和 guest 生命周期响应 |

`sandbox-init` 在本仓构建,再由 [`guest-runtime`](https://github.com/kuasar-sandbox/guest-runtime) 打包进 `sandbox-runtime.bundle`。

## 公开集成 package

| Package | 用途 |
| --- | --- |
| `pkg/resource` | 节点资源控制 wire contract 与 client,由 `orchestrator` 使用 |
| `pkg/ctl` | Host-local control socket 协议和 `ServeExecTunnel` policy callback;`ProxyExec` 仅 relay 已连接 backend |
| `pkg/usage` | Host-only 累计资源用量、Snapshot、Record 及离线读取 |
| `pkg/usagereader` | CLI 与本机 conductor adapter 共用的无损 live/saved/history 读取 |
| `pkg/sandbox`、`pkg/restore`、`pkg/snapshot` | 生命周期、restore 和 snapshot 编排 |
| `pkg/artifact` | 向 Manifest store 或 named-location Bundle 进行 typed publication |
| `pkg/uffd`、`pkg/memory` | 按需 memory 加载与 memfd 所有权 |
| `pkg/vhost` | vhost-user-blk backend 与 copy-on-write 数据路径 |
| `pkg/guestlink`、`pkg/mux`、`pkg/proto`、`pkg/fwd`、`pkg/stdio` | Host/guest 控制、多路复用和 forwarding |
| `pkg/config`、`pkg/resctl`、`pkg/chapi`、`pkg/tapfd` | Runtime 配置、资源执行、Cloud Hypervisor API 和 TAP 交接 |

## 数据路径

生命周期层通过同一 sparse-data interface 使用本地文件和 Manifest-backed content。因此部署可将 snapshot 放在本地存储、NAS/NFS 等共享文件系统,或带 cache 的 object storage 上。编排 API 与所选 backend 保持独立。

Memory 按 page fault-in,磁盘按 block 读取。未访问的数据无需在恢复后的 Sandbox 继续运行前全部加载。

## 构建与测试

构建使用环境提供的 Go，并继承 `GOROOT`、`GOTOOLCHAIN` 等工具链选择；发布自动化需要环境在 `PATH` 中提供支持 `api --slurp` 的 `gh`。项目不下载、替换或按固定二进制摘要认证这些环境工具。 Rust 构建使用环境中的 `cargo`、`rustc` 和目标链接器，不要求由 rustup 管理。

```bash
make build                      # sandbox-ctl and sandbox-init
make sandbox-ctl sandbox-init   # explicit binary targets
make build TARGET_ARCH=aarch64  # cross-compile; amd64/arm64 aliases are accepted
make vet test                   # static checks and unit tests
make test-e2e                   # component owner suite; requires the assembled project BIN
```

前置条件:

- 源码构建使用 Go 1.26.1+;
- 真实 Runtime 操作需要 Linux 和 root 权限;
- MicroVM E2E 需要 KVM 和相应 kernel interface;
- 需要 `guest-runtime` 提供 `sandbox-runtime.bundle` 和 VMLinux;
- 需要 `sandboxer/native-deps` 构建的 patched Cloud Hypervisor。

Unit/static 检查不能证明 privileged KVM、TAP、cgroup、vhost 或 restore 路径已执行。被 skip 的 privileged suite 不能表述为已完成集成验证。

## Cloud Hypervisor 边界

`sandboxer` 维护项目 Cloud Hypervisor patch set 和构建支持。Patch set 提供 Runtime 所需的 external-memory、userfaultfd、balloon 和生命周期合同。上游源码、精确 patch target、构建输入及许可证边界记录在 [`docs/cloud-hypervisor_zh.md`](docs/cloud-hypervisor_zh.md) 和 [`LICENSE_SCOPE_zh.md`](LICENSE_SCOPE_zh.md)。

Cloud Hypervisor 仍是采用上游许可证的第三方软件。项目原创 Go code 不会重许可上游代码,也不会重许可保留上游 copyright 和 license 的 patch。

## 跨仓依赖

Go import surface 保持窄边界:

| 仓库 | Import surface | 用途 |
| --- | --- | --- |
| `accelerator` | Manifest、cache/store client、sparse/image helper | Snapshot ingest/fetch、block read 和 flattened-image 配置 |
| `connector` | `pkg/tapfd` | 接收 TAP 与 network namespace file descriptor |

Go-only 源码构建需要本仓以及兄弟目录中的 `accelerator` 和 `connector`。即使设置 `GOWORK=off`,已有本地 `replace` 仍会生效。内部 `require` 版本描述各组件目标正式 Release(Daily Preview 使用去掉 preview 后缀的同一目标);目标 Tag 可以尚不存在,因为实际构建使用兄弟目录源码。验证必须记录实际源码 SHA,不能把版本标签当作编译使用的 revision。完整系统使用[项目工作区](https://github.com/kuasar-sandbox/kuasar-sandbox)。Patched Cloud Hypervisor、Guest Runtime 和 Kernel 是独立 Native/Runtime 前置,不是所有 Go-only 检查的依赖。

## Release 模型

包内来源记录只有在本地 Git Tag 与所选源码 commit 一致时才保留内部依赖的发行
版本。未打 Tag 的源码构建记录 `git:<commit>`,不要求创建目标发行 Tag。

`sandboxer` 独立发布 `vX.Y.Z` 组件版本。x86_64 archive 包含 `sandbox-ctl`、`sandbox-init` 和 patched `cloud-hypervisor` binary。组件文档与 E2E source 从选定 Tag 收集进项目 platform archive,不会在组件 archive 中重复交付。

项目仓独立发布 `release-vX.Y.Z` 聚合版本,精确选择一个 `sandboxer` Tag 和其他各发行单元版本,并验证组合后的完整系统。

版本关系见[项目 Release 文档](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/release_zh.md),可用的 Stable 聚合版本由 [GitHub Latest Release](https://github.com/kuasar-sandbox/kuasar-sandbox/releases/latest) 渠道解析。

## 文档

- [sandbox_zh.md](docs/sandbox_zh.md) — Host 控制与完整 Sandbox 生命周期：配置、输出目标、资源用量、COW、制品格式/发布、捕获、恢复、读取恢复和清理。
- [sandbox-init_zh.md](docs/sandbox-init_zh.md) — Guest PID 1 ABI、初始化、vsock、原始 usage 观测、stdio 和应用生命周期。
- [cloud-hypervisor_zh.md](docs/cloud-hypervisor_zh.md) — VMM 集成、补丁、构建和运行边界。

这三组 owner 文档提供完整英中版本。Native build 和 license scope 指南也有双语版本。章节归属与新增文档要求见英文 [Documentation policy](CONTRIBUTING.md#documentation)。

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
