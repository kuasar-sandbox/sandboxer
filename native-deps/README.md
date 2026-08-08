# sandboxer/native-deps

本目录构建 `sandboxer` 运行期直接消费、但不属于 Go module 的原生产物。
首版只包含 patched `cloud-hypervisor`。`vmlinux`、`mkfs.erofs`、`fsck.erofs`
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
`bin/<arch>/cloud-hypervisor` 或执行 `make clean` 后重跑。

## 3. Patch 开发循环

```bash
make ch-fetch
cd build/src/cloud-hypervisor
# edit + git commit
cd ../../..
make ch-patches-format
make cloud-hypervisor
```

约定:

- patch 只覆盖平台必须改动的 CH 行为:外部 memfd memory-zone、snapshot 跳过
  user-managed memory zone、通过 unix fd 交接 uffd、balloon 不对外部托管内存
  `PUNCH_HOLE/MADV_DONTNEED`。
- patch 文件按 commit 顺序落在 `deps/ch-patches/`。
- 升级 CH 版本时先更新 pin,再重新执行 `ch-fetch`、应用 patch、构建、跑
  sandboxer 和 platform e2e。
- `build/src/cloud-hypervisor` 是 patch 工作区;不要在未 format patch 前
  清理该目录。

patch 语义和设备模型详见 `sandboxer/docs/cloud-hypervisor.md`。

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
../bin/$(uname -m)/cloud-hypervisor --version
```

完整验证应在 `sandboxer/` 仓库根运行:

```bash
make test
make -C ../platform test-e2e-sandbox-cold
```

真实 e2e 需要 `/dev/kvm`、guest runtime、vmlinux 和 tap/network 前置条件。
缺失时脚本会 skip;发布前应在具备 KVM 的环境跑完整 platform e2e。
