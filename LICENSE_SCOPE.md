# 许可证范围

除下述上游内容外,本仓库中未另行声明许可证的项目原创内容适用根目录的 Apache License 2.0.

## Cloud Hypervisor patch

`native-deps/deps/ch-patches/` 中的 patch 基于 Cloud Hypervisor v51.1.本项目新增的修改按 Apache-2.0 提交;mbox patch 中保留的上游上下文继续适用对应 Cloud Hypervisor 源文件已有的许可证声明.

Cloud Hypervisor v51.1 的源码以 Apache-2.0 为主,部分文件包含 BSD-3-Clause 代码.因此本仓库不为整组 patch 声明一个会误导的统一 `Apache-2.0 OR BSD-3-Clause` SPDX 表达式.根目录 `LICENSE` 提供 Apache-2.0 全文,[`LICENSES/BSD-3-Clause.txt`](LICENSES/BSD-3-Clause.txt) 保留 patch 所涉及上游源码使用的 BSD-3-Clause 文本及归属声明.

构建时下载且未跟踪在本仓库中的 Cloud Hypervisor 源码继续适用其上游逐文件许可证声明.
