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
4. 若目标文件或材料记录不存在,用 Cargo 构建 `cloud-hypervisor`。
5. 拷贝到 `bin/<arch>/cloud-hypervisor`。

目标文件、构建报告和链接映射均存在时构建会跳过。需要强制重建时删除
`bin/<arch>/cloud-hypervisor` 或执行 `make clean` 后重跑。格式化 patch 本身不会使
已有 binary 失效。`make clean` 删除 native 构建输出与 bin,保留 patch 源码工作区和 tarball 缓存。

使用组件 Makefile 从选定源码构建。`release.sh package` 使用匹配的
`bin/<arch>` 二进制,或显式指定的 `RELEASE_BIN_DIR`;只收集材料并生成 bundle,
不重新构建二进制,不重置源码或构建缓存。所选源码 checkout、依赖版本、原生
构建记录与产物应一并保留。

打包记录实际 Go 版本和生效的 module 替换。Go/module LICENSE、NOTICE 取自所选
编译器安装和匹配的 module 源码,保留嵌套路径。模块解析沿用正常 Go 缓存与路由,
下载模块的校验和须匹配二进制记录。只有明确单独采集的内部兄弟组件使用其自身
源码材料;组织命名空间本身不豁免其他模块。官方包中不受支持的第三方本地替换
需要改用带版本的 module 输入。现有 Kuasar 本地 `replace` 继续使用。

材料放在 `share/licenses/<component>` 和 `share/sources/<component>`。
后者包含 `SOURCES.tsv`、`GO-BUILD-INFO.tsv`、`GO-MODULES.tsv` 与
`MATERIALS.sha256`。声明缺失、子目录不可读或遍历不完整时收集失败。
独立验证检查交付清单、校验和、必需文件、来源记录相符性、载荷身份及归档路径/
类型/权限。它不获取源码 checkout 或 Go 模块,不与远端源码树比较许可正文,
也不下载或认证编译器分发。校验和及 VCS 记录是相符性检查,不能证明任意生产者的身份。

归档名称标识请求的发行目标。项目及内部依赖记录在本地 Tag 匹配所选 commit 时
使用发行版本,否则记录 `git:<commit>`;打包不要求创建未来目标 Tag。
发布者在 Tag/Release 写入前把选定项目 SHA 传入验证器,采用 bundle 中
`release-notes.md` 正文,追加既有来源/Preview 标记。可信源码选择、构建/发布
权限分离及拒绝替换已发布资产的要求保持不变。

组件包包含 `sandbox-ctl`、`sandbox-init` 和 `cloud-hypervisor`。使用已有的
`RELEASE_*_SOURCE_DIR`、`RELEASE_*_SOURCE_SHA` 与 `RELEASE_*_VERSION`
选择匹配的 Accelerator/Connector 源码。普通本地替换构建不要求远端目标 Tag。
Go VCS 记录必须匹配所选 sandboxer commit。

Cloud Hypervisor 打包使用所选预构建二进制及其源码树,默认源码位置为
`native-deps/build/src/cloud-hypervisor`;可用已有的
`RELEASE_CLOUD_HYPERVISOR_SOURCE_DIR` 或 `CLOUD_HYPERVISOR_SRC` 选择另一个
匹配位置。正常 Cargo 构建在 `native-deps/build/<arch>/cloud-hypervisor`
保留 `build-report.jsonl` 与 `link.map`。
`CLOUD_HYPERVISOR_BUILD_OUT`、`CH_BUILD_REPORT`、`CH_LINK_MAP` 可选择已有
输出及记录的位置。旧二进制没有这些记录时,用匹配源码执行正常的
`make -C native-deps ch-build` (在 sandboxer 根目录);打包本身不建立新 checkout 或重建。
构建 flag、正常 Cargo 缓存及路由继续可用。

Python 3.11 或更新版本从实际 Cargo 构建报告和锁定的依赖图收集材料。
Registry crate 归档须匹配 `Cargo.lock` checksum,Git 依赖使用其中的完整
commit。清单只列实际观察到的输入,包括构建期及过程宏依赖;不表示每个 crate
的代码都进入交付物。收集的 `Cargo.lock` 与其来源记录须一致。原生 pin 变化
需要同步更新配方、来源记录与材料。

Git crate 材料包含 manifest 显式声明的 `license-file`,即使文件名非标准或
位于 crate 上层的 workspace 根目录。路径须留在所选仓库内并指向已跟踪的普通
文件;收集读取锁定的 Git commit。Registry 声明取自匹配的 crate 归档。
已发布的 `vhost` crate 缺少 workspace 根许可证:补充文件取自通过 checksum
验证的 Cargo VCS 记录所指的精确 Git commit,且先将上游 package manifest 与
该 crate 的 `Cargo.toml.orig` 比较,不由移动分支选择材料。

Crate 材料目录包含完整 Cargo 来源身份的摘要,不同 registry 或 Git commit 中
同名、同版本的包不能相互覆盖声明。Rust 版权及许可正文取自所选安装的
`share/doc/rust/COPYRIGHT-library.html`、`licenses/`,或对应的 Debian/RPM
源包声明。记录实际 Rust 版本与编译器报告的源码 commit。声明缺失时给出安装
提示;收集文件均为普通文件。

匹配的 Cloud Hypervisor 链接映射选择实际使用的系统静态库及启动对象。打包记录
文件摘要和已安装源包身份,收集版权/NOTICE 及引用的许可正文。本次构建临时对象
由 CH/Rust 来源记录覆盖。不同输入不得覆盖同名材料。RPM 收集检查已安装包列表
及每个同源兄弟包文件列表;即使其他兄弟包已提供有效声明,部分枚举仍算失败。
已安装包归属用于来源归类,不用于与 dpkg/RPM 文件摘要逐字节认证或共同所有者认证。

独立验证读取 bundle 自身的记录和声明,不需要项目/依赖 checkout、Cargo 下载或
原生构建。组件载荷/材料命名空间、来源记录、权限及校验和检查保留。这些材料
服务于发行检视,不构成法律认证,也不构成对不可信 CI 候选的隔离。

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
