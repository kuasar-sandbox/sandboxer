[English](README.md) | [简体中文](README_zh.md)

# sandboxer/native-deps

本目录构建 `sandboxer` 运行期直接消费、但不属于 Go module 的原生产物。
当前包含 patched `cloud-hypervisor`。`vmlinux`、`mkfs.erofs`、`fsck.erofs`
和 `envd` 属于 `guest-runtime/native-deps`;`librocksdb` 属于 `accelerator`。

## 1. 产物

| 产物 | 来源 | 消费方 |
|---|---|---|
| `cloud-hypervisor` | Cloud Hypervisor v51.1 + `deps/ch-patches/` | `sandbox-ctl` VMM 子进程 |

输出路径:

```
native-deps/bin/<arch>/cloud-hypervisor
../bin/<arch>/cloud-hypervisor
```

顶层 `sandboxer` Makefile 会把 native-deps 输出同步到 `sandboxer/bin/<arch>/`,
使 release 包把它放在 `sandbox-ctl` 旁边,也匹配 `sandbox-ctl --ch-binary`
的默认查找路径。

## 2. 构建

以下命令从 `sandboxer/native-deps` 目录运行。

```bash
make build
make cloud-hypervisor
make cloud-hypervisor TARGET_ARCH=aarch64
```

`make cloud-hypervisor` 流程:

1. 下载并缓存 Cloud Hypervisor pin 版本 tarball。
2. 解压到 `build/src/cloud-hypervisor`。
3. 初始化 git 基线并应用 `deps/ch-patches/*.patch`。
4. 若目标文件不存在,用 cargo 构建 `cloud-hypervisor`。
5. 拷贝到 `bin/<arch>/cloud-hypervisor`。

已有目标文件时构建会跳过。需要强制重建时删除
`bin/<arch>/cloud-hypervisor` 或执行 `make clean` 后重跑。格式化 patch 本身不会使
已有 binary 失效。`make clean` 删除 native 构建输出与 bin,保留 patch 源码工作区和 tarball 缓存。

发行打包不复用上述开发二进制或 patch 工作区。它还从选定的 sandboxer、
accelerator、connector commit 建立全新 checkout,以 `GOWORK=off` 和只读 module
解析重新构建 `sandbox-ctl`、`sandbox-init`。不会复制被忽略的开发输入或旧兄弟
二进制,并拒绝 `RELEASE_BIN_DIR` 覆盖。暂存前会核对 Go VCS 信息是否匹配所选
项目 commit。Go 构建采用相同的凭据过滤策略及私有构建/module 缓存,可保留
无凭据的 HTTPS `GOPROXY` 路由。归档名称仍标识请求的发行目标;项目来源记录
只有在本地 Tag 匹配所选 commit 时才使用该版本,否则记录 `git:<commit>`。
验证器将两个 Go 二进制及项目来源 URL/摘要绑定到该 commit;发布者传入预期
commit,在任何 Tag/Release 写入前拒绝不匹配的包。它把选定的 sandboxer commit
解到临时目录,验证 pin 的 Cloud Hypervisor tarball、应用该 commit 的 patch,
再用本次所属的私有 Cargo home 按锁文件重新构建。Python 3.11 或更新版本从
实际 Cargo 构建报告采集来源材料:registry crate 归档必须匹配 `Cargo.lock`
checksum,Git 依赖必须匹配锁文件中的完整 commit。清单只列本次实际构建输入,
包括构建期和过程宏依赖;不表示所列每个 crate 的代码都进入交付物。可修改的
解压缓存不是 registry 许可证的权威来源。
私有 Cargo home 只继承调用者源配置中无凭据的 HTTPS registry 路由,不复制
Token、凭据提供器、构建包装器或 directory/git source 覆盖项。
原生构建命令使用显式环境允许列表和私有 home,不继承 Cargo Token、云/发布凭据
或 SSH agent 设置。发行打包拒绝编译器及 wrapper 覆盖;Cargo 和材料记录使用
选定工具链的同一个精确 `rustc` 可执行文件,并记录其摘要。这是可信发行输入的
凭据卫生措施,不能代替对不可信 CI 候选的隔离。
保留标准 `CARGO_NET_GIT_FETCH_WITH_CLI` 布尔选项;Git CLI 传输不可用时,
可设为 `false` 选择 Cargo 内置 Git 传输。
已发布的 `vhost` crate 未包含 workspace 根许可证。补充文件取自通过 checksum
验证的 crate 内 Cargo VCS 记录所指的精确 Git commit,且先将上游 package manifest
与该 crate 的 `Cargo.toml.orig` 对比。不会按当前分支或另行维护的版本清单选取材料。

组件归档按组件目录隔离这些 crate 的许可/NOTICE 文件及 Rust 工具链的版权和
许可材料。未知来源、材料缺失、归档被改动或构建未成功都会导致打包失败。
本次最终链接映射还用于选择 Cloud Hypervisor 实际使用的系统静态库和启动对象,
收录其已安装源包标识、输入摘要、版权及引用的许可正文。本次构建临时对象仍由
CH/Rust 来源记录覆盖。
每个已安装系统输入还必须匹配可信构建主机 Debian 或 RPM 数据库中的文件摘要,
仅有包归属不足以证明来源。文件记录缺失、歧义或内容变更都会导致打包失败。
这检查已安装文件的完整性,不证明已失陷主机或包数据库可信。
发行打包器不接受 `RELEASE_CLOUD_HYPERVISOR_SOURCE_DIR` 和上游 tarball 环境覆盖项;
源码开发仍可通过 Makefile 覆盖输入。变更 pin 必须同时更新、验证构建配方和来源
记录。这些检查支持发行检视,不构成法律认证。

## 3. Patch 开发循环

```bash
make ch-fetch
cd build/src/cloud-hypervisor
# edit + git commit
cd ../../..
make ch-patches-format
make clean
make cloud-hypervisor
```

约定:

- patch 只覆盖平台必须改动的 CH 行为:外部 memfd memory-zone、snapshot 跳过
  user-managed memory zone、通过 unix fd 交接 uffd、balloon 对已经为空的 file-backed
  run 跳过 `PUNCH_HOLE/MADV_DONTNEED`(驻留数据仍正常回收)、restore-safe vsock,以及可靠的 VM
  pause/resume/ordered shutdown barrier。
- patch 文件按 commit 顺序落在 `deps/ch-patches/`。
- 升级 CH 版本时先更新 pin,再重新执行 `ch-fetch`、应用 patch、构建、跑
  sandboxer 和 platform e2e。
- `build/src/cloud-hypervisor` 是 patch 工作区;不要在未 format patch 前
  清理该目录。

patch 语义和设备模型详见 [cloud-hypervisor 文档](../docs/cloud-hypervisor_zh.md)。

## 4. 与其他 native-deps 的边界

```
guest-runtime/native-deps ──► vmlinux / mkfs.erofs / fsck.erofs / envd
sandboxer/native-deps     ──► cloud-hypervisor
accelerator               ──► librocksdb
```

`cloud-hypervisor` 不进入 guest runtime 镜像;它是 host 侧 VMM 子进程。
`vmlinux` 也不在本目录构建;它由 `guest-runtime` 独立发布,供 `sandbox-ctl`
通过配置引用。

## 5. 验证

最低验证:

```bash
make cloud-hypervisor
./bin/$(uname -m)/cloud-hypervisor --version
```

完整验证应在 `sandboxer/` 仓库根运行:

```bash
make test
make test-e2e
```

组件 E2E 使用 `E2E_BIN` 指定的聚合二进制目录。完整平台构建与门禁是在 sandboxer
根目录执行 `make -C ../kuasar-sandbox test-e2e`;旧的 `test-e2e-sandbox-cold` 目标不存在。

真实 E2E 需要 `/dev/kvm`、guest runtime、vmlinux 和相关 TAP/network 前置条件。
某些单项脚本直接运行时可以 skip,但 `test/e2e/run_all.sh` 设置 `REQUIRE_KVM=1`,
组件及发布门禁遇到缺失条件会失败,不会把跳过计为成功。发布前应在具备 KVM 的环境运行完整平台门禁。
