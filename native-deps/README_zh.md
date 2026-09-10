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

发行工作流在上传前把已完成归档的 SHA-256 记录为 build job output。发布者通过
`RELEASE_ARCHIVE_SHA256` 接收这一独立值,在任何 Tag/Release 写入前核对;不能用
下载后从 bundle 重新计算的值代替。即使重算 bundle 自身的校验和,全部载荷与材料
仍须匹配该次已完成构建。本地打包和独立验证不要求这个发布输入。该记录不证明
编译器来源,也不构成对不可信候选代码的隔离。

Go 依赖及工具链下载使用全新的私有 module/VCS 状态、已启用的 checksum database
和 `GOAUTH=off`。它们清除持久化 Go 设置、私有 module 绕过规则、Git 配置与调用者凭据,仅保留已验证的
无凭据路由。下载来源或工具链之前,上传的 Go 记录键必须匹配官方载荷的精确名称;
路径别名会被拒绝。这些发行检查不改变普通开发中的 module 认证方式。
只有精确的 Accelerator 和 Connector module 使用单独认证的内部源码声明。
组织内的其他 module 与外部依赖一样,必须通过 module 校验和与许可材料验证。
来源清单在逐行处理前拒绝重复或过量记录;每份元数据表上限为 16 MiB,
来源清单上限为 16,384 行。
RPM 声明收集核对已安装包列表及每个同源兄弟包文件列表的真实退出状态。
即使另一个包已提供有效声明,部分枚举失败仍会终止收集,不把部分输出当作完整覆盖。

可信发布端根据已验证请求生成标准发行正文及来源/Preview 标记。下载的
`release-notes.md` 只是本地 bundle 辅助说明,不能决定公开发行正文或对账来源。

发行打包不复用上述开发二进制或 patch 工作区。它还从选定的 sandboxer、
accelerator、connector commit 建立全新 checkout,以 `GOWORK=off` 和只读 module
解析重新构建 `sandbox-ctl`、`sandbox-init`。不会复制被忽略的开发输入或旧兄弟
二进制,并拒绝 `RELEASE_BIN_DIR` 覆盖。暂存前会核对 Go VCS 信息是否匹配所选
项目 commit。Go 构建采用相同的凭据过滤策略及私有构建/module 缓存,可保留
无凭据的 HTTPS `GOPROXY` 路由。还保留配置的 `GOSUMDB` 标识及可选的无凭据
HTTPS 镜像,以及 `GOTOOLCHAIN` 选择;默认分别为 `sum.golang.org` 和 `local`,
不会使仅用本地工具链的 CI 静默启用编译器自动选择。格式非法或带认证的路由在构建前
即被拒绝。

发行打包记录全新构建上下文实际选定的 Go 编译器,在构建前后将其分发输入与匹配的
`golang.org/toolchain` 归档逐项比较;归档由配置的 checksum database 认证。这覆盖
编译器、标准库源码及该分发中的其他文件。完整 Go 安装中额外的非构建 `api`、
`doc`、`misc`、`test` 文件不在认证范围,也不作为发行许可来源;核对时处理标准的
`go.mod`/`_go.mod` 安装转换。Go 许可/NOTICE 正文来自已验证归档,包括编译器和
标准库内嵌依赖的材料,保留各自相对路径。独立验证还会
重新核对其字节、来源 URL 和 module h1。版本字符串或重算 bundle 校验和不能替代
来源核对。验证要求启用 checksum database 并取得匹配的归档/缓存;即使采用
`GOTOOLCHAIN=local`,也可能获取核验材料,但不切换构建编译器或静默启用工具链
自动选择。这些检查以可信构建主机为前提,不证明已失陷主机可信。

归档名称仍标识请求的发行目标;项目来源记录
只有在本地 Tag 匹配所选 commit 时才使用该版本,否则记录 `git:<commit>`。
验证器将两个 Go 二进制及项目来源 URL/摘要绑定到该 commit;发布者传入预期
commit,在任何 Tag/Release 写入前拒绝不匹配的包。独立验证将完整项目许可/NOTICE
集合(含嵌套 `LICENSES`)与所选 commit 的 Git blob 比较;即使重算 bundle 校验和,
内容变化、缺失或额外文件仍会被拒绝。验证前须取得该精确 commit;可信发布者获取
源码历史用于检查,不执行候选源码或辅助脚本。验证还要求 Cloud Hypervisor pin 的
来源 URL/摘要、所选项目 patch 集的 URL/commit,以及 pin 的上游 `Cargo.lock`
URL 和字节。构建时也会用该预期 lock 摘要核对全新源码;更新 pin 或 lock 时必须
同步更新此绑定。提供 `RELEASE_DEPENDENCIES`
时,验证要求 accelerator、connector 的发行版本与请求完全一致;绑定缺失、重复、
包含其他组件或发生冲突时,发布前即失败。普通的本地 replace 源码构建仍不要求
远端目标 Tag。许可证收集拒绝不可读子目录和不完整遍历,不会仅发布可读的材料。
官方组件包不支持没有已认证 module 校验和的第三方本地 Go 替换,应选择带版本
的 module 替换。现有 Kuasar 兄弟仓本地替换和普通源码开发不变。
它把选定的 sandboxer commit
解到临时目录,验证 pin 的 Cloud Hypervisor tarball、应用该 commit 的 patch,
再用本次所属的私有 Cargo home 按锁文件重新构建。Python 3.11 或更新版本从
实际 Cargo 构建报告采集来源材料:registry crate 归档必须匹配 `Cargo.lock`
checksum,Git 依赖必须匹配锁文件中的完整 commit。清单只列本次实际构建输入,
包括构建期和过程宏依赖;不表示所列每个 crate 的代码都进入交付物。可修改的
解压缓存不是 registry 许可证的权威来源。
Git crate 材料包括 manifest 显式声明的 `license-file`,即使它采用非标准文件名,
或位于 crate 上层的 workspace 根目录。路径必须留在选定仓库内,且对应已跟踪的
普通文件;所有收集字节取自锁定的 Git commit,不取自可修改的 checkout 声明。
私有 Cargo home 只继承调用者源配置中无凭据的 HTTPS registry 路由,不复制
Token、凭据提供器、构建包装器或 directory/git source 覆盖项。
原生构建命令使用显式环境允许列表和私有 home,不继承 Cargo Token、云/发布凭据
或 SSH agent 设置。发行打包拒绝编译器及 wrapper 覆盖;全新发行构建还明确
拒绝非空的 `GOEXPERIMENT`、`RUSTFLAGS` 和
`CARGO_ENCODED_RUSTFLAGS`,不会静默丢弃请求的设置。普通开发构建继续接受已有
构建 flag。Cargo 和材料记录使用
选定工具链的同一个精确 `rustc` 可执行文件,并记录其摘要。这是可信发行输入的
凭据卫生措施,不能代替对不可信 CI 候选的隔离。
保留标准 `CARGO_NET_GIT_FETCH_WITH_CLI` 布尔选项;Git CLI 传输不可用时,
可设为 `false` 选择 Cargo 内置 Git 传输。
已发布的 `vhost` crate 未包含 workspace 根许可证。补充文件取自通过 checksum
验证的 crate 内 Cargo VCS 记录所指的精确 Git commit,且先将上游 package manifest
与该 crate 的 `Cargo.toml.orig` 对比。不会按当前分支或另行维护的版本清单选取材料。

组件归档按组件目录隔离这些 crate 的许可/NOTICE 文件及 Rust 工具链的版权和
许可材料。未知来源、材料缺失、归档被改动或构建未成功都会导致打包失败。
crate 目录还包含完整 Cargo 来源身份的摘要,不同 registry 或 Git commit 中同名、
同版本的包不会相互覆盖许可文件。
若两个不同原生链接输入将使用同一个系统材料名称,打包会在第二个输入覆盖声明
之前拒绝冲突。
Rustup 标准库声明取自官方 `rustc` 分发,不取自可修改的本地文档。其 HTTPS
发行清单必须匹配所选编译器的完整 Git commit、release 字符串及 host;完整归档
须匹配清单中的 SHA-256 后,才采集生成的标准库版权及许可正文。Stable 按精确
版本选择;beta/nightly 按已安装的固定日期定位清单,并核对同一完整 commit。
`RUST-NOTICES.tsv` 记录清单/归档 URL 和摘要。该检查不替换或安装工具链。
采集器采用构建所用的无凭据 HTTPS `RUSTUP_DIST_SERVER` 根地址,默认值为
`https://static.rust-lang.org`,支持镜像路径前缀。清单与编译器下载均使用该根地址;
镜像清单保留的上游 URL 也经相同镜像读取。其他来源或含凭据的服务器 URL
会被拒绝,不省略 commit/摘要检查。参见 [Rustup 环境变量说明](https://rust-lang.github.io/rustup/environment-variables.html)。
参见 Rust 的[分发布局](https://forge.rust-lang.org/infra/channel-layout.html)。
继续支持已安装的同源 Debian/RPM 包声明;材料缺失时给出安装提示并拒绝打包。
发行版 Rust 声明的字节必须匹配已安装包摘要和源包
身份。包所属的符号链接仅在解析后目标通过核验时复制为普通文件;Debian
Multi-Arch 共同所有者必须全部一致。引用的 common-license 正文保留自身的包
身份。这些检查不证明主机或包数据库可信。
最终链接映射确定所选目标的 sysroot。`RUST-STDLIB.tsv`
列出该目标完整 `.rlib` 输入集相对 sysroot 的路径及 SHA-256,包括 LTO 在最终
链接前消费的标准库 bitcode,不声称清单中每个归档都链入了结果。清单摘要绑定到
Rust 工具链记录。这些是实际工具链输入的摘要,不将本地修改过的工具链声称为
未经修改的上游发行物。
本次最终链接映射还用于选择 Cloud Hypervisor 实际使用的系统静态库和启动对象,
收录其已安装源包标识、输入摘要、版权及引用的许可正文。本次构建临时对象仍由
CH/Rust 来源记录覆盖。
每个已安装系统输入还必须匹配可信构建主机 Debian 或 RPM 数据库中的文件摘要,
仅有包归属不足以证明来源。文件记录缺失、歧义或内容变更都会导致打包失败。
这检查已安装文件的完整性,不证明已失陷主机或包数据库可信。
版权、许可和 NOTICE 正文字节也必须匹配已安装包的文件摘要,且所属源包与链接
输入一致。引用的 Debian common-license 正文按其自身所有者核验;Multi-Arch
共同所有者必须全部认同文件字节与要求的源包身份。许可记录缺失、内容变化或
归属冲突时拒绝收集。
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
