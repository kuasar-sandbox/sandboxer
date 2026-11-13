[English](guest-abi.md) | [简体中文](guest-abi_zh.md)

# Guest ABI：启动、应用与 Host 协议

本页为历史链接保留。PID 1 源码和协议规范已迁至 guest-runtime。

[阅读完整规范](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md)。


## 原章节入口

<a id="goals"></a>

[设计目标](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#goals)

<a id="system-boundary"></a>

[系统中的位置](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#system-boundary)

<a id="non-goals"></a>

[不做的事](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#non-goals)

<a id="runtime-prerequisites"></a>

[Runtime 镜像消费前提](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#runtime-prerequisites)

<a id="application-contract"></a>

[应用契约](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#application-contract)

<a id="launch-spec"></a>

[launch 配置(LaunchSpec)](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#launch-spec)

<a id="application-environment"></a>

[用户应用看到的环境](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#application-environment)

<a id="exit"></a>

[退出语义](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#exit)

<a id="signals"></a>

[信号处理](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#signals)

<a id="initialization"></a>

[sandbox-init 三阶段](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#initialization)

<a id="base-and-root"></a>

[阶段 1：基础 Runtime、等待 launch、工作负载 root 与 switch-root](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#base-and-root)

<a id="guest-block-discard"></a>

[Guest 块设备 DISCARD](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#guest-block-discard)

<a id="apply-and-launch"></a>

[阶段 2:spec 应用 + stdio 接线 + 应用拉起](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#apply-and-launch)

<a id="application-cgroups"></a>

[应用 cgroup namespace 与 controller 拓扑](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#application-cgroups)

<a id="supervision"></a>

[阶段 3:supervisor](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#supervision)

<a id="quiesce"></a>

[quiesce 处理](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#quiesce)

<a id="stdio"></a>

[应用 stdio / console 接线](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#stdio)

<a id="exec-sessions"></a>

[exec 会话(`sandbox-ctl exec`)](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#exec-sessions)

<a id="forward-sessions"></a>

[connect 端口转发会话(`sandbox-ctl run --connect`)](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#forward-sessions)

<a id="control-and-mux"></a>

[vsock 控制面 + console MUX 协议](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#control-and-mux)

<a id="connections"></a>

[连接分类](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#connections)

<a id="channels"></a>

[通道与寻址](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#channels)

<a id="management-operations"></a>

[管理操作集](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#management-operations)

<a id="management-wire"></a>

[管理消息 wire format 与字段](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#management-wire)

<a id="mux-wire"></a>

[MUX 子协议](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#mux-wire)

<a id="mux-close"></a>

[MUX 优雅关闭握手](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#mux-close)

<a id="forward-wire"></a>

[connect 转发帧子协议(fwd)](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#forward-wire)

<a id="timelines"></a>

[时序](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#timelines)

<a id="health"></a>

[ping 健康探测](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#health)

<a id="failures"></a>

[失败语义](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#failures)

<a id="usage-observations"></a>

[原始 Usage 观测](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#usage-observations)

<a id="extensions"></a>

[扩展点](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#extensions)

<a id="related"></a>

[相关文档](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi_zh.md#related)
