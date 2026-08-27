# sandboxer

microVM 沙箱生命周期引擎:冷启动、快照、恢复,以及块设备(vhost-user-blk)与
按需内存(uffd 懒加载)的 host 侧编排;guest 侧由 PID 1
(`sandbox-init`)承接,并由 `guest-runtime` 打包进 `sandbox-runtime.bundle`。是
[kuasar-sandbox](https://github.com/kuasar-sandbox/kuasar-sandbox) 平台的运行时
核心,独立演进。

控制相关的跨仓薄导出面包括:`pkg/resource` 提供节点资源控制协议
(wire + `Client`,由 `orchestrator` 的 **node-ctl** 作控制器侧 import),
`pkg/ctl` 提供 host-local `ctl.sock` 协议与 `ProxyExec` 入口,供可信
node proxy 在完成远程 exec 鉴权后接入现有 exec/MUX 链路。资源协议
规范见 [`orchestrator/docs/node-resource.md`](https://github.com/kuasar-sandbox/orchestrator/blob/main/docs/node-resource.md)
§5;`ctl.sock` 与 `ProxyExec` 合同见 [`docs/sandbox.md`](docs/sandbox.md) §6.3。

## 组成

| 路径 | 角色 |
|---|---|
| `cmd/sandbox-ctl` | host 控制平面:`run --config/--from/--restore` / `export` / `snapshot` / `publish` / `info` / `exec` / `config` |
| `cmd/sandbox-init` | guest PID 1:三阶段 init + vsock 控制面 + 应用监督;由 `guest-runtime` 打包进 `sandbox-runtime.bundle` |
| `pkg/sandbox` `pkg/restore` `pkg/snapshot` | 生命周期编排:显式/E冷启动、E/S同点快照、memory恢复与独立disk/memory provenance |
| `pkg/uffd` `pkg/memory` | uffd handler 与 memfd 统一内存所有权(懒加载) |
| `pkg/vhost` | vhost-user-blk 后端(file / manifest 块源 + CoW diff) |
| `pkg/{guestlink,mux,proto,fwd,stdio}` | host↔guest vsock 控制面、stdio MUX 与端口转发 |
| `pkg/ctl` | **导出面**:host-local `ctl.sock` 协议 + 经鉴权 exec 隧道的 `ProxyExec` gate/relay |
| `pkg/{config,resctl,chapi,tapfd}` | sandbox.yaml、cgroup+balloon 联动、CH API 客户端、tapfd 消费 |
| `pkg/util` | 内联工具(`ParseSize` / `LocateBinary` 等,跨模块导出) |
| `pkg/resource` | **导出面**:节点资源控制协议(`orchestrator` 的 node-ctl import) |

## 构建

```bash
make build                      # sandbox-ctl + sandbox-init
make sandbox-ctl sandbox-init   # 两个纯 Go 二进制(CGO_ENABLED=0)
make build TARGET_ARCH=aarch64  # 交叉编译(别名 amd64 / arm64)
make vet test
make test-e2e                   # 运行 test/e2e/run_all.sh;需要项目主仓组装的完整 BIN
```

独立版本通过仓库的 `Release` workflow 发布为 `vX.Y.Z`;发布件
`sandboxer-vX.Y.Z-linux-x86_64.tar.gz` 包含 `sandbox-ctl`、`sandbox-init`、
`cloud-hypervisor`。本仓文档与 `test/e2e/` 仅由项目主仓从所选 tag 聚合进
platform 包。本地可用 `make release VERSION=vX.Y.Z`
生成并校验相同布局的 release bundle。
当前 Release 只发布已完成全量构建与 BMS 验证的 Linux x86_64 目标。正式版之前,
项目主仓的每日协调器按上海日期触发
`v0.1.0-preview.YYYYMMDD` prerelease;preview 不更新 GitHub Latest,正式
`v0.1.0` 由独立构建发布。

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

- [docs/sandbox.md](docs/sandbox.md) — host 控制平面、PortableSandboxConfig、
  `.overlay`/Sandbox E/Snapshot S、carrier、export/snapshot/restore/publish与资源模型。
- [docs/sandbox-init.md](docs/sandbox-init.md) — guest PID 1 ABI:
  `sandbox-init` 三阶段、vsock 控制面 + stdio MUX 协议、应用契约。
- [docs/cloud-hypervisor.md](docs/cloud-hypervisor.md) — patched CH 与
  `sandbox-ctl` 的外部 memfd/uffd/balloon 契约。

## License

本仓库的项目原创内容采用 [Apache License 2.0](LICENSE).Cloud Hypervisor patch
中保留的上游许可证边界见 [LICENSE_SCOPE.md](LICENSE_SCOPE.md).
贡献授权说明见 [CONTRIBUTING.md](CONTRIBUTING.md).
