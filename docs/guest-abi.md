[English](guest-abi.md) | [简体中文](guest-abi_zh.md)

# Guest ABI: boot, applications and host protocols

This page preserves existing links. The PID 1 source and protocol specification have moved to guest-runtime.

[Read the complete specification](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md).


## Previous section links

<a id="goals"></a>

[Design goals](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#goals)

<a id="system-boundary"></a>

[Position in the system](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#system-boundary)

<a id="non-goals"></a>

[Non-goals](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#non-goals)

<a id="runtime-prerequisites"></a>

[Runtime image consumption prerequisites](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#runtime-prerequisites)

<a id="application-contract"></a>

[Application contract](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#application-contract)

<a id="launch-spec"></a>

[Launch configuration (LaunchSpec)](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#launch-spec)

<a id="application-environment"></a>

[The application environment](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#application-environment)

<a id="exit"></a>

[Exit semantics](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#exit)

<a id="signals"></a>

[Signal handling](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#signals)

<a id="initialization"></a>

[The three sandbox-init phases](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#initialization)

<a id="base-and-root"></a>

[Phase 1: base runtime, wait for launch, workload root and switch-root](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#base-and-root)

<a id="guest-block-discard"></a>

[Guest block DISCARD](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#guest-block-discard)

<a id="apply-and-launch"></a>

[Phase 2: apply the spec, wire stdio and start the app](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#apply-and-launch)

<a id="application-cgroups"></a>

[Application cgroup namespace and controller topology](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#application-cgroups)

<a id="supervision"></a>

[Phase 3: supervisor](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#supervision)

<a id="quiesce"></a>

[Quiesce handling](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#quiesce)

<a id="stdio"></a>

[Application stdio and console wiring](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#stdio)

<a id="exec-sessions"></a>

[Exec sessions (`sandbox-ctl exec`)](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#exec-sessions)

<a id="forward-sessions"></a>

[Connect port-forward sessions (`sandbox-ctl run --connect`)](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#forward-sessions)

<a id="control-and-mux"></a>

[Vsock control plane and console MUX protocol](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#control-and-mux)

<a id="connections"></a>

[Connection classes](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#connections)

<a id="channels"></a>

[Channels and addressing](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#channels)

<a id="management-operations"></a>

[Management operations](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#management-operations)

<a id="management-wire"></a>

[Management wire format and fields](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#management-wire)

<a id="mux-wire"></a>

[MUX sub-protocol](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#mux-wire)

<a id="mux-close"></a>

[MUX graceful-close handshake](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#mux-close)

<a id="forward-wire"></a>

[Connect forwarding frame sub-protocol (fwd)](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#forward-wire)

<a id="timelines"></a>

[Timelines](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#timelines)

<a id="health"></a>

[Ping health probes](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#health)

<a id="failures"></a>

[Failure semantics](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#failures)

<a id="usage-observations"></a>

[Raw usage observations](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#usage-observations)

<a id="extensions"></a>

[Extension points](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#extensions)

<a id="related"></a>

[Related documentation](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/guest-abi.md#related)
