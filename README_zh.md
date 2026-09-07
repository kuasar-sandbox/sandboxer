[English](README.md) | [简体中文](README_zh.md)

# sandboxer

microVM 沙箱生命周期引擎:冷启动、快照、恢复,以及块设备(vhost-user-blk)与
按需内存(uffd 懒加载)的 host 侧编排;guest 侧由 PID 1
(`sandbox-init`)承接,并由 `guest-runtime` 打包进 `sandbox-runtime.bundle`。是
[kuasar-sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox) 平台的运行时
核心,独立演进。

控制相关的跨仓薄导出面包括:`pkg/resource` 提供节点资源控制协议
(wire + `Client`,由 `orchestrator` 的 **node-ctl** 作控制器侧 import),
`pkg/ctl` 提供 host-local `ctl.sock` 协议，以及通过 caller policy callback 在 backend dial 前完成鉴权的 `ServeExecTunnel`；`ProxyExec` 只 relay 已连接 backend。可信 node proxy 通过这些入口接入现有 exec/MUX 链路。资源协议
规范见 [`orchestrator/docs/node-resource_zh.md`](https://github.com/kuasar-sandbox/orchestrator/blob/main/docs/node-resource_zh.md)
§5;`ctl.sock` 与 `ServeExecTunnel` 合同见 [`docs/sandbox_zh.md`](docs/sandbox_zh.md) §6.3。

## 组成

| 路径 | 角色 |
|---|---|
| `cmd/sandbox-ctl` | host 控制平面:`run --config/--from/--restore` / `export` / `snapshot` / `publish` / `info` / `exec` / `config` |
| `cmd/sandbox-init` | guest PID 1:三阶段 init + vsock 控制面 + 应用监督;由 `guest-runtime` 打包进 `sandbox-runtime.bundle` |
| `pkg/sandbox` `pkg/restore` `pkg/snapshot` | 生命周期编排:显式/E冷启动、E/S同点快照、memory恢复与独立disk/memory provenance |
| `pkg/sandboxfile` | strict Sandbox E 与 flattened image parser，以及保留 sparse map 的顶层 E source 组装 |
| `pkg/artifact` | logical image/Sandbox source 的 Manifest store 与 named-location single-root Bundle 发布 API |
| `pkg/uffd` `pkg/memory` | uffd handler 与 memfd 统一内存所有权(懒加载) |
| `pkg/vhost` | vhost-user-blk 后端(file / manifest 块源 + CoW diff) |
| `pkg/{guestlink,mux,proto,fwd,stdio}` | host↔guest vsock 控制面、stdio MUX 与端口转发 |
| `pkg/ctl` | **导出面**:host-local `ctl.sock` 协议 + `ServeExecTunnel` request gate；`ProxyExec` 仅 relay 已连接 backend |
| `pkg/{config,resctl,chapi,tapfd}` | sandbox.yaml、cgroup+balloon 联动、CH API 客户端、tapfd 消费 |
| `pkg/util` | 内联工具(`ParseSize` / `LocateBinary` 等,跨模块导出) |
| `pkg/resource` | **导出面**:节点资源控制协议(`orchestrator` 的 node-ctl import) |

## 顶层 Sandbox E 组装与直接发布

上层构建器无需启动 VM，也无需先写完整 `.sandbox` 文件。`pkg/sandbox` 的
`OpenFlattenedImage`、`PrepareSandboxEConfig` 和 `AssembleSandboxE` 接受 local
digest-qualified tarstream、`manifest://` 或 located Manifest Bundle image，生成保留
Hole/Zero/Data map 的标准 Sandbox E `sparse.Source`。输入 `FlattenedImage` 由调用者
关闭；组装所得 source 借用它，必须在 image 关闭前消费完毕。原始 `config.json`
bytes 原样保留，`sandbox.runtime.cfg` 使用 canonical encoding，root 为 direct EROFS
`self` layout。每任务持有权威 key 的嵌入式调用者使用
`artifact.NewProcessStorageWithCustomerKey` 显式传入 resolver；该 resolver 至多求值一次，
不会退回或覆盖为进程级 `MANIFEST_KEY`。

`pkg/artifact.Publisher.PublishSource` 将已经组装的 `RoleImage` 或 `RoleSandbox`
直接 ingest 到 Manifest store。`NewSingleRootBundlePublisher` 则将同样的 logical
source 直接形成 named-location Manifest Bundle，返回
`file://<key>.bundle@manifest:<key>#<location>`；它不先生成 tarstream、不先上传
Manifest store，也不创建 `.image`、`.sandbox` 或 BuildID/SandboxID alias。调用者
保留 source 的所有权。

single-root Bundle 使用调用者预先取得的 write admission 与 customer key，在目标
目录内完成 ingest/finalize 和完整验证，再通过 shared-location exclusive-create
协议发布内容寻址 final；同 key 的并发 writer 收敛到
经过严格验证的同一 final，corrupt、mismatched、symlink 或 non-regular existing
final 均 fail closed。Local tarstream 是一个 role-specific transport file；Manifest
Bundle 则是带 admission、Manifest/chunk 与 crypto domain 的自包含 carrier，两者不能
仅凭扩展名互换。

## 本地目录身份

`sandbox-ctl run` 将 `--run-root` / `--base-root` 视为调用级 **RunRoot** /
**BaseRoot**。每个调用在两者下使用同一个 **PathID** leaf，形成实际
**RunDir** / **BaseDir**：

```text
RunDir  = RunRoot/PathID
BaseDir = BaseRoot/PathID
```

`--path-id` 省略时默认等于逻辑 **SandboxID**，因此既有调用的路径不变。
显式 PathID 只允许一个安全路径分量；它只负责 host 目录和 `ctl.sock` 定位，
不改变 SandboxID 在资源、日志、memfd、artifact 或 writable diff 文件名中的
逻辑身份。`exec`、`snapshot` 和 live `export` 可只用 `--path-id` 定位运行中
Sandbox；同时给出两者时 PathID 优先，ctl 协议不携带额外身份字段。

## 构建

```bash
make build                      # sandbox-ctl + sandbox-init
make sandbox-ctl sandbox-init   # 两个纯 Go 二进制(CGO_ENABLED=0)
make build TARGET_ARCH=aarch64  # 交叉编译(别名 amd64 / arm64)
make vet test
make test-e2e                   # 运行 test/e2e/run_all.sh;需要项目主仓组装的完整 BIN
```

独立版本通过仓库 `main` 上受信任的 `Release` workflow 发布为 `vX.Y.Z`;发布件
`sandboxer-vX.Y.Z-linux-x86_64.tar.gz` 包含 `sandbox-ctl`、`sandbox-init`、
`cloud-hypervisor`。本仓文档与 `test/e2e/` 仅由项目主仓从所选 tag 聚合进
platform 包。本地可用 `make release VERSION=vX.Y.Z`
生成并校验相同布局的 release bundle。
当前 Release 只发布已完成全量构建与 BMS 验证的 Linux x86_64 目标。项目主仓的
每日协调器显式传入源码分支和精确 SHA;组件 `main` 用于主线,`release/vX.Y.x`
用于对应组件维护线。Preview 和维护分支 Stable 不更新 GitHub Latest;独立的幂等
Reconcile Latest 工作流按 `main` 源码提交先后协调主线 Stable,同一提交才比较 SemVer。
组件版本与平台聚合版本独立,
平台始终按精确 Tag 选择本组件。
同版本发布与删除共用完整 workflow mutation group;若 GitHub 合并 pending 请求,项目主仓
协调器会把 cancelled 状态作为未完成操作自动重跑,不会把它当作发布或 GC 已完成。

构建需要 Go 1.24+;运行还需 **guest-runtime** 发布的 `sandbox-runtime.bundle`
和 `vmlinux`(guest 内核);patched `cloud-hypervisor` 由本仓
`sandboxer/native-deps` 构建并由 `sandbox-ctl` 启动。`mkfs.erofs` 是
guest-runtime 构建 runtime 镜像和 build sandbox 内展平镜像时使用的工具,
不是 sandboxer host 侧运行依赖。

## 跨仓依赖(薄)

| 依赖 | 用途 | 解析 |
|---|---|---|
| `accelerator/pkg/manifest`(+ `cache`/`store` client) | 快照 ingest/fetch、vhost 块读 | `replace => ../accelerator` |
| `accelerator/pkg/image` | 读取展平镜像内嵌的 RuntimeConfig | `replace => ../accelerator` |
| `connector/pkg/tapfd` | tapfd 交接消费侧(`RecvFdsWithNetns`) | `replace => ../connector` |

均为纯 Go、无 CGO 的导入面——整仓 `CGO_ENABLED=0` 构建,不引入 rocksdb / eBPF
等重依赖。`replace` 指向兄弟仓相对路径:clone 全组织为兄弟目录即可离线构建;
组织级 `go.work` 见 [kuasar-sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox)。

## 文档

- [docs/sandbox_zh.md](docs/sandbox_zh.md) — host 控制平面、PortableSandboxConfig、
  `.overlay`/Sandbox E/Snapshot S、carrier、export/snapshot/restore/publish与资源模型。
- [docs/sandbox-init_zh.md](docs/sandbox-init_zh.md) — guest PID 1 ABI:
  `sandbox-init` 三阶段、vsock 控制面 + stdio MUX 协议、应用契约。
- [docs/cloud-hypervisor_zh.md](docs/cloud-hypervisor_zh.md) — patched CH 与
  `sandbox-ctl` 的外部 memfd/uffd/balloon 契约。

## License

本仓库的项目原创内容采用 [Apache License 2.0](LICENSE).Cloud Hypervisor patch
中保留的上游许可证边界见 [LICENSE_SCOPE_zh.md](LICENSE_SCOPE_zh.md).
贡献授权说明见 [CONTRIBUTING.md（英文）](CONTRIBUTING.md).
