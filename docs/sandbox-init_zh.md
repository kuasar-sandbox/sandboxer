[English](sandbox-init.md) | [简体中文](sandbox-init_zh.md)

# sandbox-init — guest PID 1 ABI

`sandbox-init` 是每个 sandbox guest 内的 PID 1,由 `sandboxer/cmd/sandbox-init`
构建,再由 `guest-runtime` 打包进 `sandbox-runtime.bundle`。本文档定义
`sandbox-init` 与 host 侧 `sandbox-ctl` 之间的 ABI:启动期 rootfs 组装、launch
握手、应用拉起、生命周期监督、stdio/console 转发、exec/attach/quiesce 控制面。

`sandbox-runtime.bundle` 的镜像打包、内置 guest payload、版本发布与构建流程见
[guest-runtime runtime bundle 文档](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/sandbox-runtime_zh.md)。本文件只讨论镜像内 `/sbin/init` 的
运行契约,以及 `sandbox-ctl` 启动 microVM 后如何与它对接。

## 1. 概述

### 1.1 设计目标

| 目标 | 实现方式 |
|---|---|
| 启动快 | 单一静态 Go 二进制 PID=1,无 systemd / dracut / busybox 链路 |
| 启动状态可验证 | sandbox-init 版本固定;launch、quiesce、restore 和 attach 使用显式握手与状态屏障 |
| 跨 sandbox 共享 | virtio-pmem + DAX 通过 host page cache 共享同一文件的只读页，不共享 guest 私有可写 RAM |
| 一 VM = 一 primary app | 默认 private PID 模式用 clone(NEWPID\|NEWNS) 让 app 看到自己 PID=1；也支持 shared PID 模式 |
| 生命周期可寻址 | app 启动/退出通知与 quiesce/restore 握手走 vsock；host VMM 停机是独立路径（§5.4） |
| app I/O 干净 | 应用 stdin/stdout/stderr(或一个伪终端)走 vsock 转发,与内核 dmesg 隔离 |

### 1.2 系统中的位置

```
   HOST                                                      GUEST VM
   ────                                                      ────────

   sandbox-ctl ── spawn CH ──►  cloud-hypervisor             kernel boot
                                       │                          │
                                       │ virtio-pmem / DAX        │  mount root, exec /sbin/init
   sandbox-runtime.bundle  ◄────────────┤  (host shared file)      │  = sandbox-init
                                       │                          │    phase 1: mount + overlayfs + chroot
        kernel dmesg  ◄── --console ───┤ ◄── hvc0 (virtio-con) ── │    phase 2: apply launch spec
        (host captures to stderr/file) │                          │              + app stdio wiring
                                       │                          │    phase 3: supervisor loop
                                       │                          │
                                       ├──── conn: launch ───────►│    fork/exec user app
                                       │     (conn → MUX) ◄══════►│      app stdin/out/err ↔ MUX
                                       ├──── conn: ping ─────────►│      app is PID 1 in private mode
                                       │◄─── conn: app_started ───┤
                                       │                          │
   /run/sandbox/<sid>/vsock.sock  ◄────┤◄─── conn: app_exited ────┤    primary exits without restart
                                       │                          │    reboot()
                                       │◄─── CH exits ────────────┤
```

### 1.3 不做的事

- **不在 Guest 归并用量**: 按请求返回原始 usage 观测, 不增加 Guest ticker、
  累计、面积、峰值或历史补发 (§4.11).
- **无容器运行时**:平台直接管理 sandbox 生命周期,不引入 runc / crun / podman
- **无 systemd / OpenRC**:进程监督由 sandbox-init 自己写的 supervisor 完成
- **init 操作不依赖 busybox / util-linux**:mount / mkdir / chdir / chroot /
  reboot / openpty 通过 Go syscall 完成；runtime 同时包含下节列出的独立 guest payload 工具。
- **外层 runtime 不提供发行版 rootfs**:应用镜像提供 /etc、/usr 等内容；
  sandbox-runtime 提供 init、挂载点与 /opt/sandbox-runtime。
- **不复用控制面短连接做 stdio**:管理操作各用一条短连接(§4.3);只有 launch /
  restore / attach / exec 四种操作的连接在握手后升级为长连接 MUX(§4.5)。应用会话
  那条 MUX 任一时刻至多一条(launch 生,restore/attach 续);exec 每次会话另起一条
  独立、短生命的 MUX,可并发多条(§3.6)

<a id="2-sandbox-runtimebundle-镜像结构"></a>

## 2. Runtime 镜像消费前提

完整的 sandbox-runtime.bundle 包装、文件清单、版本和构建规则由 [Runtime Bundle](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/sandbox-runtime_zh.md)维护。此处只定义 PID 1 必须满足的消费边界：

- `/sbin/init` 必须是匹配 Guest 架构的 sandbox-init；本仓 `make sandbox-init` 生成它。
- Runtime 预建早期挂载及数据盘 staging 目录；phase 1a 按下文构造实际用户 rootfs，外层 Runtime 不是通用发行版 `/etc`、`/usr` 或共享库环境。
- `/opt/sandbox-runtime/` 是保留的只读 Guest payload 根，phase 1a 将其 bind 到用户 rootfs 同名路径，遮蔽用户镜像已有内容；应用必须遵守应用环境章节的约束。
- virtio-pmem/DAX 复用相同 backing file 的只读页，不共享各 Guest 私有可写 RAM。init 和 payload 可执行文件都必须匹配目标架构。

Runtime 生产、host/target mkfs 区分和 Native 构建操作见 [Runtime 构建](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/sandbox-runtime_zh.md#3-构建)及 [Native 指南](https://github.com/kuasar-sandbox/guest-runtime/blob/main/native-deps/README_zh.md)。Host–Guest 握手、挂载次序与 ABI 继续由本文完整定义。


## 3. sandbox-init 三阶段

### 3.1 阶段 1:早期挂载 + 并发取 launch spec + switch-root

launch spec 携带 `mounts`(含 `empty` 卷)等"驱动 rootfs 组装"的字段,因此
**hello/launch 握手与不依赖 spec 的挂载链并发进行**。握手 goroutine 只做 socket 操作，不访问文件系统；
挂载链会解析原 root 下的路径。Go 线程共享 CLONE_FS，因此进程级 chroot 必须在 JOIN 之后、握手 goroutine 退出后执行。

```
A. 先于一切(纯 socket,无 rootfs 依赖):
   - raw netlink RTM_NEWLINK(lo, IFF_UP)   ← 拉起回环;内核自动补 127.0.0.1/8、::1/128,
                                              无需 RTM_NEWADDR;失败即 die(lo 起不来 = guest 损坏)
   - AF_VSOCK bind+listen :5000             ← 反向通道(host→guest)必须早于 hello 开门
   - go handshake{ connect(CID=2:5000) → write hello → read launch }  ← spec, conn 经 channel 交回

B. 与 handshake 并发(spec-independent 挂载链):
   1. mount -t proc/sysfs/devtmpfs  /proc /sys /dev  (CONFIG_DEVTMPFS_MOUNT=y 时 /dev EBUSY,跳过)
   2. wait /dev/vda、/dev/vdb 出现(轮询 stat,timeout 10s)
   3. mount -t erofs -o ro /dev/vda /overlay/lower;mount -t ext4 /dev/vdb /overlay/upper
   4. mkdir /overlay/upper/{upperdir,workdir} 若不存在
   5. mount -t overlay overlay -o lowerdir=/overlay/lower,upperdir=/overlay/upper/upperdir,
                                    workdir=/overlay/upper/workdir  /sysroot
   6. mkdir -p /sysroot/opt/sandbox-runtime
      mount --bind /opt/sandbox-runtime /sysroot/opt/sandbox-runtime  ← pmem 内发布件根投影进新 root;
                                              源在 pmem(switch-root 后无路径可达),由 E 的 MS_MOVE
                                              随子树带进新 /(与 D volume 同理);源 ro EROFS,bind 天然只读

C. JOIN:spec, conn := <-handshake               ← 拿到 LaunchSpec(及复用至 launch_ack 的连接)

D. spec-dependent、switch-root 之前(empty/volume 卷需 raw ext4 source):
   for each mounts[].type == empty:
     mkdir /overlay/upper/volumes/<i>            ← raw ext4(与 upperdir 同级,物理隔离于 overlay 写层)
     mkdir -p /sysroot/<target>
     mount --bind /overlay/upper/volumes/<i> /sysroot/<target>   ← 空目录遮蔽镜像该路径内容

E. switch-root:
   MS_MOVE /proc /sys /dev → /sysroot/{proc,sys,dev}
   chdir(/sysroot) → MS_MOVE . / → chroot(.)      ← MS_MOVE 携带整个子树:proc/sys/dev、
                                                    B6 的 /opt 与 D 的 volume binds 一并进入新 /

F. switch-root 之后的基础挂载:
   mount -t cgroup2 cgroup2 /sys/fs/cgroup;mkdir /sys/fs/cgroup/app   ← 应用 cgroup namespace/freezer 根
   launch.cgroup_control=true 时另 mkdir /sys/fs/cgroup/app/init,并严格下放 advertised controllers(§3.2.1)
   mount -t devpts devpts /dev/pts (newinstance,ptmxmode=0666)        ← tty 模式 openpty 需要
   mount -t tmpfs  tmpfs  /run     (nosuid,nodev)                     ← 自动挂载(类 /proc)
   mount -t tmpfs  tmpfs  /run/shm (nosuid,nodev,mode=1777)           ← 自动挂载
```

全部通过 `golang.org/x/sys/unix.Mount` / `unix.Chroot` 等 syscall 完成,
不依赖任何外部二进制。

`/dev/vda`(blk0 base)是用户应用的镜像 erofs(只读);`/dev/vdb`(blk1 overlay
ext4)是写层。overlay 合并后 `/sysroot` 是 guest rootfs 的最终视图,switch-root
之后这套视图变成新的 `/`。承载 sandbox-init 自身的 `sandbox-runtime.bundle` 由内核经
virtio-pmem 挂在 `/`(`root=/dev/pmem0 ... rootflags=dax=always`),阶段 1 把它让位给 overlay
(其中 `/opt/sandbox-runtime` 经 bind 在让位时随子树保留进新 root)。

**为何能并发**：bind+listen 必须在 host 写出 launch 前完成，因为 host 此时即启动 ping。
握手 goroutine 只执行 socket 操作，不访问文件系统；挂载链会解析原 root 下的路径，并非 mount 不解析路径。
真正的进程级 chroot 位于 JOIN 之后，此时握手 goroutine 已退出，不会与它并发使用文件系统。
这一安排把 hello→launch 往返与 overlay 组装重叠。

**volume 卷的 source 与搬运**:`empty` 卷的 source 是 raw ext4 上 `/overlay/upper/volumes/<i>`
(与 overlay 的 `upperdir/` 物理隔离,同在 vdb、一起进磁盘快照),bind 到 sysroot 内的
target;switch-root 的 `MS_MOVE /sysroot → /` 会把该 bind 随整棵子树搬进新 `/`
(proc/sys/dev、`/opt/sandbox-runtime` 正是同理),无需单独 MS_MOVE。switch-root 后
`/overlay/upper` 路径被埋,但 bind 持有 ext4 inode 引用使卷内容在 target 处存活,且应用
看不到 raw ext4 内部结构。

**`/opt/sandbox-runtime` 的搬运与版本钉住**:同一通道——B6 在 switch-root 前把 pmem 内的
发布件根 bind 进 `/sysroot/opt/sandbox-runtime`,由 E 的 `MS_MOVE /sysroot → /` 随子树带进
新 `/`,bind 持有 pmem inode 引用使其在原挂载被遮蔽后仍存活;phase2 fork 前的 `MS_REC|MS_SHARED`
令其作为对等挂载传播进应用私有 mount ns(应用以只读看到)。因 payload 驻留 pmem，改它会改变 sandbox-runtime.bundle 的工件 identity。
restore 将 runtime footer identity 与 E/C0 核对并要求重挂相应 runtime；这不是每次 restore 都对整个 payload
重新 hash（见 [sandbox 生命周期](sandbox_zh.md)）。在可信且匹配的工件前提下，恢复后 payload 版本保持一致。
payload 与 runtime 同生命周期，不能独立热补丁；若需独立版本须改用独立只读 EROFS 设备（§6）。

**单磁盘模式**(`boot.root.overlay` 省略时)。host 不建 blk1、只发一个可写 `--disk`(blk0=
root 盘的 ext4 CoW),并在 cmdline 加 `sandbox.root.layout=single`。phase1a 读 `/proc/cmdline`
(/proc 已在最前挂好)判定模式——因组盘与 launch 握手并发、早于 launch spec 到达,模式只能走
cmdline,不能走 spec。单盘路径:只 wait `/dev/vda`,`mount -t ext4 /dev/vda /sysroot` 直接挂为
可写根(无 `/overlay/lower`、`/overlay/upper`、无 overlayfs、无 vdb),其余(`/opt/sandbox-runtime`
bind、switch-root、phase1b 基础挂载)不变。`empty` 卷的 source 改落在写根自身的
`/sysroot/.sandbox-volumes/<i>`(无独立 ext4 upper),仍 bind 遮蔽 target、随 switch-root 子树搬运。
单盘根盘恒为可写 ext4,无 erofs 镜像 ⇒ 无内嵌 config.json ⇒ launch.exec 必填(host 侧 validate 强制;
`launch.placeholder` 占位模式除外——它本就不跑外部程序)。

**数据盘 `boot.disks[]`**(root 之外,最多 `MaxDataDisks=8` 块)。每块盘配置规则与 `boot.root` 相同
(单盘 diff / 双盘 overlay,含 `base_from_refs`),host 侧用同一套 `PrepareDiff`/CoW/reader 准备。

```
设备序(= CH --disk 顺序 = guest /dev/vd[a,b,c…]):
  root(单盘 1 个 / overlay 2 个) → boot.disks[0](1/2) → boot.disks[1](1/2) → …
  例:overlay root(vda=base, vdb=upper)+ single 数据盘(vdc)+ overlay 数据盘(vdd=base, vde=upper)
host 把 mounts[].source(name)→序号→具体 /dev/vdX 解析进 MountSpec(DiskIndex/DiskOverlay/DiskDevs);
name 不过线。盘的设备位由它在 boot.disks[] 的下标决定,与 mounts[] 顺序无关。
```

挂载在 `applyVolumeMounts` 内、switch-root **之前**完成(与 `empty` 卷统一成「备好 source → bind
到 `/sysroot<target>` → switch-root 的 MS_MOVE 携带」)。组装挂点 **预先 bake 进 runtime erofs**
(`/sysdisks/disk-{0..7}{,-lower,-upper}`,24 个空目录;数量须与 `config.MaxDataDisks` 一致),
缺失即 die(erofs 版本不符,重建或减盘):

```
单盘数据盘 N:  mount ext4 /dev/vdX → /sysdisks/disk-N
overlay 数据盘 N: erofs ro → /sysdisks/disk-N-lower;ext4 rw → /sysdisks/disk-N-upper;
                 overlayfs(lower, upper/upperdir, upper/workdir) → /sysdisks/disk-N
两者收尾:bind /sysdisks/disk-N → /sysroot<target>
```

`/sysdisks` 子树 switch-root 后不可见,但被 bind(及 overlayfs 对 lower/upper 的引用)持活——与
root overlay 的 `/overlay/lower+upper` 隐藏后仍活、`/opt/sandbox-runtime` bind 同一机制。**恢复**时
guest 从内存快照续跑、盘已挂好(不重挂),host 只需按同序重建并 serve N 个设备(restore host yaml 的
`boot.disks[]` 若显式提供active diff binding,须与 Sandbox E 同数同序;mounts[] 在 restore 下禁止
重放)。**快照**在同一 pause/quiesce 点逐盘捕获可写 diff(root + 各数据盘),先生成包含完整disk
graph的Sandbox E,再生成memory Snapshot S;S的`snapshot.cfg`只用`sandbox_ref`引用E并记录
memory `from_refs`。本地flatten-merge分别作用于E的disk layers和S的memory layers。

Usage 在上述受控 root/data 组装处保留每个可写盘的私有 CLOEXEC 文件系统句柄,
先于 switch-root 隐藏 staging path. Single 指向 ext4 本身, overlay 指向
原始 upper 而不是合并 root. Bind/empty volume 不增加来源. 句柄随内存恢复
保留, 不进入应用 exec, 与 init 一同关闭; 不可取消的在途读取保留原句柄/槽,
直至实际结束 (§4.11).

### 3.2 阶段 2:spec 应用 + stdio 接线 + 应用拉起

阶段 1 的 JOIN 已拿到 LaunchSpec 与那条 vsock 连接(hello/launch 已收发)。阶段 2
在**同一条连接**上把 spec 应用完、发 launch_ack;此后该连接**不关闭**——升级成 MUX,
承载应用的 stdin/stdout/stderr(或一个伪终端)直到沙箱结束(协议见 §4.5)。

`launch_ack` 在 spec **全部应用完(含 init)之后**才发出,故它对 host 是"环境与
初始化全部就绪、即将 fork"的 settled 信号，**不证明 final exec 已成功或应用健康**。spec 应用各步触碰文件系统,均在
switch-root 之后单线程执行。

```
1. spec.network 非空时 applyNetwork(spec.network);为空时跳过
                                           ← 冷启动:可选全新网卡,additive
     - Sethostname(network.hostname)
     - raw netlink RTM_NEWLINK(interface UP[+IFLA_MTU]) / RTM_NEWADDR(IP/CIDR) /
       可选 RTM_NEWROUTE(nexthop)
     失败 fast-fail —— 一次性沙箱模型下"网络配置失败"必须立刻 die
     (restore 出来时若带 network,改走 flush-and-replace 重配,见 §4.3 restore)
2. applyFsMounts(spec.mounts: type==tmpfs)  ← 内存盘挂载(empty 卷已在阶段 1 switch-root 前完成)
3. applyFiles(spec.files)                 ← 暂存 tmpfs(/run/.inject)→ 写内容 + chmod/chown →
                                            bind 到 target →(read_only 时 remount-ro)→ MNT_DETACH 暂存;
                                            内容仅在内存、不落 vdb;失败 fast-fail
4. runInit(spec.init)                      ← 顺序执行一次性命令;输出走 console;init[].{env,
                                            workdir,user} 可设;init[].timeout 超时则 SIGKILL;
                                            任一条非零退出/超时 = die(initContainers 语义,早期执行)
5. 按 spec.stdio 准备应用 stdio fd(§3.5):
     - tty 模式:openpty();记 master/slave fd;初始 winsize 来自 spec
     - pipe 模式:为每个声明通道建 pipe / socketpair;未声明 stdin → fd 0 接 /dev/null
6. write  launch_ack{stdio: 实际启用的 channel 集合}  ← 此后连接进入 MUX 帧收发态,不再关闭
7. read   ack                            ← host 确认进入 MUX 态

8. 通过 exec.Cmd 拉起首次 primary helper:
     SysProcAttr.Cloneflags = CLONE_NEWNS [ | CLONE_NEWPID 当 pid_namespace=private(默认) ]
       private:app 是自身 PID ns 的 PID 1。shared(launch.pid_namespace=shared):app 留在
       sandbox-init 的 PID ns,由 PID1 reaper 收割 app 的孤儿后代(复用 init reaper)。
     SysProcAttr.UseCgroupFD=true,CgroupFD=/sys/fs/cgroup/app 的 O_CLOEXEC 目录 fd
       → clone3(CLONE_INTO_CGROUP)保证 helper 从出生起就在真实 /app;
     SysProcAttr.Unshareflags 含 CLONE_NEWCGROUP
       → Go 的 fork child 在 clone3 返回后、re-exec 前执行 unshare(CLONE_NEWCGROUP),
          故新 cgroup namespace 以真实 /app 为根,不需要 anchor 进程。
     tty 模式: Setctty + setsid + slave 作为 fd 0/1/2;关闭 master 副本于子进程
     pipe 模式: 各 pipe/socketpair 的 child 端作为 fd 0/1/2
     argv: [/proc/self/exe, "exec-child", "bootstrap", isolated, cgroupControl,
            placeholder, cred, workdir, exec, args...]
       cred = "uid:gid:sg1,sg2"(由 spec.user 在 guest 侧 /etc/passwd 解析)或 "-"(不降权)
       placeholder 时同一 helper 末步等待信号,不 exec 外部程序
     env:  spec.Env(默认补 PATH)

9. helper 先阻塞在内部 SOCK_STREAM 握手的 start gate;父进程完成 pid 登记后才放行。
   helper 在已有 private mount ns 内先把 / 标为 rslave(禁止反向传播),再卸载继承的
   guest-global cgroup2 mount并重新 mount cgroup2 /sys/fs/cgroup。新 mount 受刚创建的
   cgroup namespace 限定,真实 /app 在 helper 视图中成为 /。仅 cgroup_control=true 时,
   bootstrap helper 向虚拟 /init/cgroup.procs 写一次 0,自迁移到真实 /app/init;这是
   sandbox-init 实现中唯一一次 cgroup.procs 写入。
   随后 isolated 时 mount -t proc proc /proc(新 PID ns 必需;shared 沿用
     sandbox-init 的 /proc),再在 ready gate 阻塞。

10. 父进程在 helper 的 ready gate 上:
     - 固定打开 /proc/<pid>/ns/cgroup(O_CLOEXEC),由 sandbox-init 长期持有;
     - cgroup_control=true 时确认真实 /app/cgroup.procs 为空,把真实 /app/
       cgroup.controllers 中每个 advertised controller 写入 cgroup.subtree_control,
       再回读逐项校验;任何一步失败都 abort helper,绝不启动最终应用;
     - 发 go。helper 才执行 chdir(workdir) → 若 cred≠"-":setgroups → setgid → setuid
     (降权放在挂载 /proc 之后、execve 之前)→ syscall.Exec(exec, args...)
     placeholder 时:同样的 ns/cred 准备,但末步不 execve,改
       signal.Notify(SIGTERM,SIGINT) → <-sig → exit(0)(不可用 select{}:无活 goroutine
       会触发 Go 死锁检测 panic)。占位仍是被监督的 app:进 app cgroup、随快照冻结、
       stop 时收信号退出。host 侧强制 restart=always ⇒ 从 exec 会话 kill 占位会原地
       重拉(非 reboot);guest PID 1 收到停机信号时先置 shutdown 再停止占位；host VMM teardown 是独立路径（§5.4）。

11. 父进程(sandbox-init pid=1)等内部 CLOEXEC socket 以 EOF 确认最终 execve 成功,然后:
     - 启动 stdio 桥接 goroutine:app 端 fd ↔ MUX 流(§3.5)。桥的 app 侧 fd 按"代"
       可换;in-place 重启先等旧代 stdout/stderr 或 PTY pump 读到 EOF 并把尾部写入
       MUX,再安装新 fd。MUX 会话和 stream 不变 → 重启不断 host 链路
     - 短连接 dial host:5000 发 app_started{pid} → 等 ack → close
     - 拉起 launch.plugin[] 伴生进程(见下),再进入阶段 3 supervisor
```

#### 3.2.1 应用 cgroup namespace 与 controller 拓扑

真实 `/sys/fs/cgroup/app` 在两种模式下始终同时是**应用 cgroup namespace 根**和
snapshot 的**递归 freezer 根**;snapshot 始终只 freeze/thaw 这个路径,不调用应用或
`envd` 的 `/freeze`。`sandbox-init` 自身与顶层 `init: []` 一次性准备命令留在 guest-global
cgroup 根,不属于 `/app`。

`launch.cgroup_control` 默认 `false`。真实布局与应用内 scoped cgroup2 视图如下:

```text
cgroup_control=false

真实 guest-global                         primary/plugin/native exec 所见
/sys/fs/cgroup/                           /sys/fs/cgroup/        (真实 /app 的 scoped mount)
├── cgroup.procs: sandbox-init, init[]     ├── cgroup.procs: primary/plugin/exec
└── app/                                  └── /proc/self/cgroup: 0::/
    └── cgroup.procs: primary、restart、plugin、native exec 及其后代
         ↑ namespace root + snapshot freeze root

cgroup_control=true

真实 guest-global                         primary/plugin/native exec 所见
/sys/fs/cgroup/                           /sys/fs/cgroup/        (真实 /app 的 scoped mount)
├── cgroup.procs: sandbox-init, init[]     ├── cgroup.procs: 空
└── app/                                  ├── cgroup.subtree_control: 全部 advertised controllers
    ├── cgroup.procs: 空                   ├── init/
    ├── cgroup.subtree_control: 全部       │   └── cgroup.procs: primary/plugin/exec
    └── init/                             └── /proc/self/cgroup: 0::/init
        └── cgroup.procs: primary、restart、plugin、native exec 及其后代
         ↑ /app 是 namespace/freezer 根;/init 是 sandbox-init 直接管理的长期进程组
```

`true` 模式在创建应用子组前先严格启用并回读 guest-global 根 advertised controllers,
bootstrap 自迁移后再对真实 `/app` 做同样校验;任一 controller 缺失即启动失败。
`false` 模式不承诺应用侧 delegation,只保留 guest-global 根的历史 best-effort 下放。

图中的 cgroup `/init` **不是** sandbox.yaml 顶层 `init: []`:前者是长期应用进程组,
后者仍由 sandbox-init 在 primary 启动前顺序执行一次。`cgroup_control=true` 给应用侧
manager 留出空的 namespace 根及 controller delegation;例如 envd 默认创建的
`/user`、`/ptys`、`/socats` 对应真实 `/app/{user,ptys,socats}`,与 `/init` 同级。
不创建额外 `/exec`,native exec 仍与 primary 同组。

首次 primary 的 bootstrap 是唯一特殊路径;之后 primary restart、plugin start/restart、
native exec 的 `exec-join` 都用 pinned cgroup namespace fd,并通过
`clone3(CLONE_INTO_CGROUP)`**直接出生在最终 target**(`/app` 或 `/app/init`)。没有
`Start → 写 cgroup.procs` 的迁移窗口,setup/namespace/mount/握手失败均使该次启动失败。
所有传递给 helper 的 namespace/socket fd 都立即恢复 CLOEXEC 并在使用后关闭,最终应用
不会继承它们。应用正常访问的 `/sys/fs/cgroup` 只有上述 scoped 视图,其中不存在
guest-global 路径 `/sys/fs/cgroup/app`;shared PID 下通过 `/proc/1/root` 检查 PID 1 的
mount namespace 属于下述非安全边界例外。

cgroup namespace 只限定 cgroupfs 的**视图与管理根**,不是额外安全边界。特别是
`pid_namespace=shared` 时应用仍与 sandbox-init 共用 PID namespace,所以能看到
sandbox-init 为 PID 1;其 `/proc/1/cgroup` 相对应用 namespace 甚至可能显示 `/..`。
这与应用自身看到 cgroup 路径 `/` 或 `/init` 并不矛盾。

**伴生进程(launch.plugin[])**。app 完成 bootstrap 后,sandbox-init 顺序拉起每个 plugin
作为**自身的子进程**(故 reaper 直接收割),跑在同一 guest rootfs + app cgroup(随快照一起
冻结)+ 网络;stdout/stderr 走 console;支持 env/workdir/user。每个 plugin 按自身
`restart`(never|on-failure|always,默认 always)+ 共享退避(下文)独立监督。**plugin
退出绝不影响沙箱生命周期**——只有 app(launch.exec)的退出按 launch.restart 决定 reboot
或原地重启。plugin 与 app 是"对等体":app 隔离(private)时 plugin 仍在 sandbox-init 的
PID ns(共享 rootfs/网络/cgroup,但不在 app 的 PID ns 内)。每次 plugin 启动先以
`CLONE_INTO_CGROUP` 进入最终 target,再由轻量 re-exec helper 在 private mount ns 中加入
pinned cgroup namespace并挂 scoped cgroup2;placement/setup/exec 失败按 plugin 启动失败
处理,不存在“继续运行但未冻结”的降级。

**降权时机**:`spec.user` 解析后的 uid/gid 不在外层 clone 用 `SysProcAttr.Credential`
——否则子进程会以非 root 身份执行 `mount /proc`(新 PID ns 必需)而 EPERM 失败。
故降权放到子进程内、`mount /proc` 之后、`execve` 之前(`setgroups→setgid→setuid`)。
`init[].user` 不受此限(init 在 PID1 既有 ns、无 proc 重挂),直接用 `Credential`。
命名用户("nobody")在 guest 侧解析(`/etc/passwd` 权威地在镜像 rootfs 内)。

**挂载传播(为 restore 注入铺路)**:app 用 `CLONE_NEWNS` fork,拿到的是 fork 那一刻
的挂载树**私有副本**;restore 时(app 已在运行)PID1 里新建的 bind 默认不会进入这个
私有 ns。为让 restore 注入能到达运行中的 app:fork **之前** PID1 把 `/` 标为
`MS_REC|MS_SHARED`(rshared),app 子进程在 `mount /proc` **之前**把自己的副本标为
`MS_REC|MS_SLAVE`(rslave)——PID1 的挂载事件单向传播给 app,app 自己的挂载(如
`/proc`)不外泄回 PID1。冷启动注入不依赖此机制(那时 bind 早于 fork、随副本带入);
**后续注入会需要它，但当前 restore 不重放 LaunchSpec files/mounts/init**。网络 restore 重配无此问题:app 只 `CLONE_NEWNS`、不
`CLONE_NEWNET`,与 PID1 共享网络 ns,netlink 改动天然可见。

**applyNetwork 不是 listener 的前置依赖**——LaunchSpec 不带 network 时直接跳过,
guest 仍有 `lo`;applyNetwork 只对应用层外部网络服务有意义,vsock 控制面与网络
配置正交.

**cold-start fast-fail**：冷启动的网络 / 挂载 / 文件 / init 任一失败都让应用悄悄跑
是反模式——上层调度器期望"沙箱起不来 = 重新调度",而不是"起来了但环境不对"。

### 3.3 阶段 3:supervisor

```
loop:
  signal.Notify(sigchld, sigterm, sigint)
  select:
    sigchld:
      pid, status = waitpid(-1, WNOHANG)            // 单 reaper 收割所有子进程
      if pid == app_pid(atomic):                     // 用户应用
        if !shutting_down && wantRestart(launch.restart, status):
          go restartApp(backoff)                     // 原地重启(异步),reaper 继续收割
        else:
          app_exit_then_reboot(status)               // never / on-failure-clean-exit
      elif pluginReg.onExit(pid, status):            // 伴生 plugin → 自身策略 + 退避重拉
        // handled
      else:                                          // exec 子进程 → 投递其会话;
        execReg.deliver(pid, status)                 //   或 shared-PID app 的孤儿 → 静默收割
    sigterm/sigint:
      shutting_down = true                           // 阻止在飞的 restart 再 fork
      send spec.stop_signal to app_pid               // 默认 SIGTERM;覆盖镜像 StopSignal
      wait up to spec.stop_grace_period (默认 10s);超时 SIGKILL
      app_exit_then_reboot(status)

restartApp(backoff):                                  // app 原地重启,launch.restart=always/on-failure
  sleep(backoff)                                      // 退避;期间 reaper 继续收 plugin/exec
  shutting_down 时直接返回；quiescing（快照窗口）期间等待
  rewireApp:
    等旧代全部 app→host pump:读到 EOF 且最后字节已写入 MUX
    超过 2s 仍未 EOF(例如后代继承 writer)→ 强制关闭旧代 fd,有界继续
    安装新代 fd,唤醒 pump 接到**不变的 MUX stream** → phase2ForkApp:
    先 app_pid.store(newpid),再放行 helper;helper 以 CLONE_INTO_CGROUP 直接出生在最终
    target、加入 pinned cgroup namespace并挂 scoped cgroup2 → app_started{newpid}
  // host 的 run 链路不断,持续收到新实例输出

app_exit_then_reboot(status):
  标记 bridge 停机，先让输出 pump 读自然 EOF 并有界排空，再关闭剩余 app fd；可完成时发 stream EOF
  short-conn dial host:5000 → write app_exited{code, term_signal} → wait ack(timeout) → close
  reboot(LINUX_REBOOT_CMD_POWER_OFF)   # ack 拿不到也照常 reboot;POWER_OFF → CH 干净退 0
```

**退避**(app 与 plugin 共用,supervise.go):退出即重拉,延迟 10ms 起、每次 ×2、封顶 60s;
进程存活满 60s 再退出则重置回 10ms。**app_pid 原子化**:restart goroutine fork 后即写,reaper
按其路由,故新实例的退出不会被误判为 plugin/exec。**快照门**:quiesce 置 quiescing(plugin
随 app cgroup 冻结、supervisor 不再 fork 新进程),restore/attach 解除——restartApp 会等过这个
窗口再 fork,避免冻结遗漏新进程。`launch.restart=always` 下 app 退出**不 reboot**,沙箱长活。
generation drain 只确认 app→host 输出，不对 stdin 建 barrier；正常排空时各 stream 保序且代际切换不发 EOF，
但 2 s 强制关闭兜底可能丢弃残留输出，应用代际间隔中的 stdin 也可能被丢弃。停机若追上刚安装的新代,pump 仍先消费该已存在代,
但不会再等待未来代。

`reboot(POWER_OFF)`(而非 `RESTART`):一次性沙箱模型下应用退出即沙箱结束,
CH 应随之干净退出。`RESTART` 会触发 CH 的"原地重启"流程,试图重连 vhost-user-blk
后端——而后端只接受一次连接(sandbox = 单 VM 生命周期),重连失败 CH 非零退出。

**反向 listener 在独立 goroutine 中持续运行**,与 supervisor signal loop 并行;
处理 host 下发的 `ping` / `restore` / `quiesce` / `attach` 短连接(§4.3、§4.4)。
listener 整个沙箱生命周期(冷启动 + snapshot/restore + MUX 重连 + 退出)持续存在,
唯一退出点是进程 reboot。

**mem_report 上报 goroutine**:与 supervisor 并行的第二个常驻 goroutine。
Launch barrier 后立即采样一次,随后默认每 5 s 读取 `/proc/meminfo`。每份报告
携带 observation `epoch` 和严格递增 `seq`;restore 在写 `restore_ack` 前建立
新 epoch并清除旧 pending 报告。Quiesce 在 freeze 前持有同一 stream lock 等待
已在途的 guest→host 交换结束,然后暂停新采样。这是 memory capture barrier
的一部分:不允许 S 保存“互斥锁已持有 + 旧 host vsock 等待中”的状态。
`restore` 切换到新 epoch 后恢复采样;`attach`(同一 VM 的
`--resume`/失败恢复)则恢复原 epoch。

报告包含 `MemAvailable` 以及 `MemTotal`、`MemFree`、`Cached`、`AnonPages`、
`SReclaimable` 诊断字段,不读取或上报 balloon current。Host Capacity 也不能从
MemTotal 推导;host 把报告与 CH `vm.info` 组合后在 sandbox 本地计算 Budget。

发送失败时保留完全相同的 epoch/seq/payload,下一 ticker 重试;成功 ACK 后才清除
pending 并允许下次采样。EAGAIN、timeout 或 ACK 丢失不会把 reporter 永久卡在
in-progress 状态。详见 [`sandbox_zh.md`](sandbox_zh.md) §4.2 / §9.3。

`mem_report` 仍是既有资源控制交换. Usage 不复用其 retained payload、周期或
ACK 队列; Host 通过独立连接请求新的原始 usage 观测 (§4.11).

### 3.4 quiesce 处理

quiesce 是 host /vm.pause 之前的最后一次 guest 清理机会，目标是：

1. 冻结应用并同步文件系统，让 memory 与 disk 工件对应同一个一致 capture point。
2. 在捕获前拆除 MUX 与端口转发会话，不把恢复后没有对端的握手中途或活跃连接当作可继续使用的会话。
   registry drain、有界 close 与 CH restore transport reset 共同建立边界；单次 SO_LINGER 返回不证明所有内核 socket 状态均已移除（§4.6 / §3.7）。

Listener 并发处理每个已 accept 的管理连接，exec/connect/mem_report registry 与 quiesce gate
负责生命周期排序。Host 只有收到 quiesced、看到该控制连接 EOF 并完成自己的 admitted-handler drain 后，才进入 /vm.pause。

**quiesce 流程**：

```
0. host 原子关闭 exec/forward admission，暂停 ping ticker；
   Usage admission is paused before the Host memory barrier; Guest invalidates
   its generation and closes/joins the connection. Blocked reads retain their
   original slots without locks waiting on the old Host.
   等已入场 ping 完成 pong + guest EOF transport barrier。
   排空独立受 8 s quiesce budget 约束，即使普通 ping timeout 不强制也一样。
   到期 cancel + join 并令捕获失败；不在 guest 确认前抢先拆能正常完成的 transport。
   guest 拒绝新 exec、SIGKILL 在飞 exec helper、关闭 lingered vsock，
   join 完整 session goroutine（§3.6）。排空有界，失败不进 freezer、不发 quiesced。
0a. guest 等待在途 mem_report exchange 完成并暂停 reporter。
    有界交换完成后，S 不会保留等待旧 host 的 reporter mutex 或观测连接。
0b. freeze 应用：
    write /sys/fs/cgroup/app/cgroup.freeze = 1
    有界轮询 cgroup.events 至 frozen 1。
    先 freeze 再 sync，防止 sync 后 app 再写脏页；递归覆盖 /init、
    envd 的 user/ptys/socats 子树和子树内新 fork 的进程，不调用 envd /freeze。
1. [prep] sync(2)                              // 刷新 ext4 dirty data
2. [prep] 若 quiesce.skip_drop_caches == false：
     open("/proc/sys/vm/drop_caches", O_WRONLY) -> write("3\n")
                                                // clean page cache + dentry/inode cache
3. 随 MUX 关闭暂停 app 输出转发；应用已冻结，残留输出有界。
4. 拆除 connect 转发（§3.7）：
   gate 新 connect；逐条关闭 target + reverse vsock。
   SO_LINGER 有界（3 s，connectLingerSec）且 best-effort；多会话并发关闭，受 quiesce budget 限制。
   accept 模式另关闭并清空缓存 guest listener，唤醒 park 的 Accept；
   resume 后懒重建。
   host 发 quiesce 前只 gate 新 forward；收到 quiesced + guest EOF 后，
   关闭剩余 host half，join 所有已入场 exec/dial/accept/relay handler，再 /vm.pause。
5. primary MUX：MUX_CLOSE -> MUX_CLOSE_ACK -> close(MUX)。
   ACK 最多等 5 s，错误/超时强制 close。Host 回 ACK 后立即关；
   guest 用有界 SO_LINGER 协助 vsock teardown，已断连接硬丢。
6. 在 quiesce 控制连接 WriteMessage(quiesced)。
7. 关闭控制连接，host 必须观察 EOF。
```

**为什么 prep 按此顺序**：

- **先 sync**：drop_caches 只丢 clean cache，先落盘使 disk 与 memory capture 的内容一致。
- **请求时 drop_caches=3**：clean cache 取决于访问历史和 prefetch 时序，不是恢复进程状态必须的 dirty 数据。
  清理可减少 capture resident cache，但 restore 后需要从 backing filesystem/device 冷读，包括 root 与数据盘。
  应按 workload 测量大小与恢复读取成本的取舍。默认跳过写入；显式 --drop-caches=true 才请求。
  不论是否 drop，freeze 与 sync 都执行。

quiesced.drop_caches_result 回报 skipped / succeeded / failed；缺失代表旧 guest 无结果回报，按 unknown 处理。
显式 skip 却得到 unknown 时 host 告警并继续，因为旧 guest 可能按历史协议已经 drop。
这个结果只描述此次动作，不保证内存压力或 balloon 不会回收缓存。

**扩展项（未实现）**：

| 动作 | 提案 |
|---|---|
| 应用层 quiesce hook | 拟议 sandbox.yaml quiesce.signal，默认空，向 app 发信号并等待固定窗口（拟议默认 100 ms）。必须显式声明：未注册 handler 的 SIGUSR1 可能终止应用。没有完成 ACK，不构成应用一致性屏障。 |
| /tmp tmpfs 重置 | phase1 挂载 /tmp tmpfs，quiesce 时 MNT_DETACH 后重挂。 |
| outbound 连接关闭 | fd 属于应用，sandbox-init 无通用主动关闭权；需要应用 hook。 |

**不做项**：

- 不关闭 vsock listener，restore/attach 仍需要它。
- 不终止或信号通知 primary app；通过 cgroup v2 递归冻结，不是 SIGTERM，也不依赖输出反压停住应用。
- 此处不重置 RNG/熵池；restore reseed 也尚未实现（§4.3）。
- 不清理 /var/log 等应用拥有的日志。

**错误与保证**：drop_caches open/write 失败会记录并回报 failed，不阻塞 capture。
本路径 sync(2) 没有返回错误值，不能声称实现了 sync-error 检查；drop 的 best-effort 结果影响捕获大小与之后的读取。

freeze 确认和 host 响应/EOF/handler 屏障必须成功。cgroup.events 未及时到 frozen 1、
exec/report drain 失败时都不发 quiesced。Primary MUX 关闭有有界 ACK 等待与硬关兜底；
SO_LINGER 设置/close 不提供另行验证、无限等待的 socket-removal 保证。
CH transport reset 处理残留 connected-socket state（§4.6）。
若 quiesced 未到或无法写出，捕获失败。Host 尝试同 VM recovery 和 reattach；只有恢复成功才继续运行，
恢复失败则终结该 VM，不能把 snapshot 失败等同于 VM 一定继续运行。见 §4.10 与 [sandbox 生命周期 §6.2](sandbox_zh.md)。

### 3.5 应用 stdio / console 接线

应用看到的 stdin/stdout/stderr(或一个伪终端)由 sandbox-init 在 guest 内创建,
经 MUX 流(§4.5)与 host 双向桥接。**不**让应用继承 sandbox-init 的 fd(那是
`/dev/hvc0` = 内核 dmesg 控制台),应用输出因此不被内核刷屏污染,host 侧也不混入
内核日志。

**两种模式,互斥**(由 launch 协议的 `stdio` 字段决定,见 §4.4 / §5.1):

| 模式 | guest 内 | 应用看到 | MUX 流 |
|---|---|---|---|
| **tty** | `openpty()`;子进程 `setsid()` + `TIOCSCTTY(slave)` + slave→fd 0/1/2;初始 `TIOCSWINSZ` 来自 spec | 一个真伪终端:`isatty()`=true、有 job control、收 SIGWINCH | 1 条:pty 流(双向,master ↔ host)+ control 流(`SET_WINSIZE` 等) |
| **pipe** | 为声明的通道各建一对 pipe/socketpair;child 端→fd 0/1/2;未声明 stdin → fd 0 接 /dev/null | 普通管道:`isatty()`=false | 按声明:stdin(host→guest)/ stdout / stderr(guest→host)+ control 流 |

sandbox-init 持有 pty master 端 / 各 pipe 的 sandbox-init 端,起桥接 goroutine
在它与 MUX 流之间双向拷贝;收到 control 流上的 `SET_WINSIZE` → 对 pty master 做
`TIOCSWINSZ`(内核自动给应用进程组发 SIGWINCH)。

**反压**：host 不消费输出（如 Ctrl-S、输出目的地阻塞或 MUX 不存在）时，信用耗尽，
guest pump 停止排空，pipe/pty 缓冲填满，app write 阻塞。正常输出反压没有滚动丢弃 buffer 策略，
但这不等于跨 transport failure、强制 drain timeout 或重启输入空档的端到端不丢字节保证（§3.3 / §4.5）。

**内核 dmesg**:走 `/dev/hvc0`(virtio-console),与应用 stdio 是完全独立的一条道;
host 侧由 CH 把它写到 sandbox-ctl 给 CH 的 stdout(一根匿名管道),sandbox-ctl
按 `--console` 标志决定丢弃 / 写 stderr / 写文件(详见 [`sandbox_zh.md`](sandbox_zh.md)
§2.2 / §5.2)。`--serial off`——没有 8250 UART。

### 3.6 exec 会话(`sandbox-ctl exec`)

`sandbox-ctl exec`(host 侧 CLI + ctl.sock 见 [`sandbox_zh.md`](sandbox_zh.md) §2.5 /
§6.3)在一个**已运行**的沙箱内拉起一条临时命令,它是用户应用的**兄弟进程**,
既不替换应用、也不重启沙箱。host 经反向通道发 `exec{spec}`(§4.3);sandbox-init
为这次会话准备 stdio、起进程、回 `exec_ack`,该连接随即成为这条会话**独立**的
stdio MUX(§4.5)。每条反向 `exec` 连接由各自的 goroutine 服务,多会话并发独立。

**加入应用的命名空间与 cgroup**。命令必须运行在**应用自己的 mount + pid + cgroup
命名空间**内,
这样 `ps` / `/proc/<pid>` / 按 pid 发信号都能看见并作用于应用进程树与其文件系统
视图(语义同 `docker exec`)。但 `setns(CLONE_NEWPID)` 只对调用进程**之后 fork
的子进程**生效、`setns(CLONE_NEWNS)` 是**线程级**——在长寿、多线程的 sandbox-init
进程里直接 setns 既不安全也会污染自身。因此 sandbox-init 先用 pinned target cgroup fd
对 `exec-join` 做 `clone3(CLONE_INTO_CGROUP)`,注册 pid 后才通过内部握手放行;helper
加入 pinned cgroup namespace并为其未来 child 选择应用 PID namespace。真正命令由一个
内层 re-exec child 拉起:它先进入应用 mount namespace,沿用其中**已挂载的** `/proc` 与
scoped cgroup2(不重挂),再解析 image 内用户并 final exec。private PID 模式下必须保留这
两段 helper:外层若先进入应用 mount namespace,其 `/proc` 看不见外层 pid,
`/proc/self/exe` 将无法用于 fork 内层 child。内层完成 mount join 后先停在 ready gate;
外层此时加入同一 mount namespace,成功后才放行内层 final exec,然后只负责等待回收与
转述退出码;任一 join 失败都不会执行用户命令,对 sandbox-init 主进程零副作用。

`exec-join` 与最终命令从出生起都继承最终应用 cgroup(`/app` 或 `/app/init`),应用内分别
看到 `0::/` 或 `0::/init`。没有独立 `/exec` cgroup,也没有任何事后
`cgroup.procs` placement。

**退出与回收**。命令退出后,guest 在该会话 MUX 上**先 EOF 全部 stdout/stderr/
pty**,**再发 `EXIT_STATUS`**(退出码;被信号杀为 128+signo),**再**走 §4.6 的
`MUX_CLOSE` 握手;host 收到 `EXIT_STATUS` 即得退出码(为何用显式帧而非"连接关掉"
见 §4.6)。exec 命令退出**不**触发沙箱 reboot——只有**用户应用**退出会按其 restart 策略决定原地重启或 reboot
(§3.3);supervisor 的子进程回收器把"非应用子进程"的退出态路由给对应 exec 会话,
不误判为应用退出。

**与 snapshot 的关系**。内层 helper 在 setup 前设置父死信号;降权可能清除此信号时,
它在降权后重设,并通过私有握手 socket 的无阻塞 EOF 检查确认原 `exec-join` 仍存活,
之后才 final exec。因而外层辅助进程被杀会一并带走那条命令。会话
MUX 中途断(host 侧 `sandbox-ctl exec` 退出 / 失联)→ guest SIGKILL 该命令,命令
不会比其会话存活更久。snapshot quiesce(§3.4)开始时,host 先原子 gate 新请求、关闭并
join 已登记的 exec handler;guest 随后**拒绝新的 exec、SIGKILL 所有在飞的 exec
辅助进程、关闭 lingered session socket,并等待整个 fork/MUX/session cleanup 退出**。
只杀 child 而不 join session 不构成 freeze barrier:它可能把尚未释放的进程级 fork
状态或半关闭 vsock 一同捕获。沙箱在 resume / restore(§4.3 `attach` / `restore`)后
解除拒绝、重新受理;被 capture 中止的单次 exec 不会自动重跑。

### 3.7 connect 端口转发会话(`sandbox-ctl run --connect`)

`sandbox-ctl run --connect`(可重复)把一个 **host 本地端点** `LOCAL` 与沙箱内一个
端点对接。**host 侧四种模式完全一致**:在 `LOCAL` 上 listen,对**每条**被接受的本地
连接开一条反向通道发 `connect{ConnectSpec}`(§4.3),该连接握手(`connect_ack`)后成为
这条转发的**独立长连接数据通道**(fwd 帧子协议,§4.7,保留 TCP 半关闭)。模式只区分
**guest 侧拿到那条目标连接的方式**——dial 一个已存在的服务,还是 listen 等一个连入:

| 写法 | guest 目标动作 | 用途 |
|---|---|---|
| `LOCAL:host:port` | `Dial` tcp | host 客户端 → 沙箱内已 listen 的 tcp 服务 |
| `LOCAL:/path`、`LOCAL:@n` | `Dial` unix | host 客户端 → 沙箱内已 listen 的 uds 服务 |
| `LOCAL::host:port` | `Listen`+`Accept` tcp | host 客户端 ↔ 沙箱内主动连入的 tcp 客户端 |
| `LOCAL::/path`、`LOCAL::@n` | `Listen`+`Accept` unix | host 客户端 ↔ 沙箱内主动连入的 uds 客户端 |

**语法**。`LOCAL` 在第一个 `:` 处切出(自身不得含 `:`):`fd=N`(继承来的**已 listen**
socket,host 用 `net.FileListener` 包装后关掉原 fd,避免泄漏进 CH)或 UDS 路径(`@name`
抽象;非抽象先删陈旧节点)。其后以 `:` 起头(即 `LOCAL::TARGET`)选 **accept 模式**,
否则 **dial 模式**。`TARGET` 以 `/` 或 `@` 起头 ⇒ **unix**(绝对/抽象路径),否则 ⇒ **tcp**
`host:port`(`net.SplitHostPort`,支持 `[::1]:port`;相对路径 unix 不支持)。一本地连接 ↔
一反向 vsock 连接 ↔ 一目标连接,全程 1:1;多条并发独立——是 exec 会话的端口转发类比。

**dial 模式**(`LOCAL:TARGET`)。guest 收 `connect` 即 `net.Dial(network, address)`
(5s 内,须 < `proto.DeadlineConnect`),成功回 `connect_ack`;host 用 `DeadlineConnect`
覆盖整个握手。

```
 本地客户端       sandbox-ctl run (host)         CH proxy      sandbox-init (guest)       目标
    │ connect        │                                            │
    ├───────────────►│ accept(UDS/fd)                             │
    │                │ DialRaw vsock + "CONNECT 5000\n"           │
    │                ├──────────────────────────────►│ accept :5000│
    │                │ connect{address:"127.0.0.1:49983"}│───────────►│ net.Dial(tcp, addr)
    │                │                                │            ├──────────►│ 49983
    │                │ connect_ack │ (或 error)       │◄───────────┤  ok       │◄─────────┤
    │                │◄──────────────────────────────┤            │
    │ ◄═══════════ fwd 帧子协议(§4.7),保留 TCP 半关闭 ════════════════════►│
```

**accept 模式**(`LOCAL::TARGET`)。guest 收 `connect{accept}` 时在 `address` 上 `Listen`
(**懒创建**:首条用到时建,按 `(network,address)` 缓存复用)并 `Accept` 一条;`Accept`
返回(沙箱内有 client 连入)**才**回 `connect_ack`。`Accept` 可**无限期阻塞**,故:host 发完
`connect` 即清握手 deadline、**无限期 park** 等 `connect_ack`,并把这条 pre-relay 反向连接
登记进 `Forwarder.pending`,使快照/关停能收拢它;guest 把这条**驻留中**会话登记进 connReg
(relay 暂空),待 `Accept` 返回再 promote 为中继。

```
 本地客户端       sandbox-ctl run (host)         CH proxy      sandbox-init (guest)    沙箱内 client
    │ connect        │                                            │ Listen(address) 懒建+缓存
    ├───────────────►│ accept(UDS/fd)                             │
    │                │ DialRaw vsock + "CONNECT 5000\n"           │
    │                ├──────────────────────────────►│ accept :5000│
    │                │ connect{accept:true,address:"/run/up.sock"}│──────►│ Accept(address) ◄────────┤ connect
    │                │  (park,无 deadline)             │          │  └ 返回 connB            │
    │                │ connect_ack │ (或 error)        │◄──────────┤                          │
    │                │◄──────────────────────────────┤            │
    │ ◄═══════════ fwd 帧子协议(§4.7),保留 TCP 半关闭 ═══════════════════════════════════►│
```

**host-leads 语义**。accept 模式 guest 侧 listener **懒创建**——某条转发首次收到 host 的
`connect{accept}` 才 bind,此后常驻(直到 quiesce)。故每条转发的**首次配对须 host 先到**;
此后 listener 常驻,其 backlog 可吸收"沙箱内 client 先连"的情形。沙箱内 client 在 listener
尚未建立时连 `address` 会被拒(`ECONNREFUSED`)。

**数据通道为何加帧**。转发要做**通用**端口转发,须忠实保留 TCP 半关闭
(`shutdown(SHUT_WR)`:一端发完仍可继续收)。但 CH hybrid vsock proxy 是用户态字节
泵,**不**把传输层的 `SHUT_WR` 翻译过 UDS↔vsock 边界(它连"对端已关"都只在下次 I/O
才浮现——同 §4.6 注),裸中继靠转发传输层关闭会丢半关闭。故数据通道走 `fwd` 帧子协议
(§4.7):关闭信号以带 3-byte header 的 **EOF/RST 帧**走 in-band 数据面,proxy 当不透明字节原样搬运,
在正常传输上保留显式半关闭；不保证 transport error 下仍完整交付。单条转发=单条逻辑流,无需流 ID 与应用窗口——vsock 连接自身的内核缓冲背压
即流控(与 §4.5 MUX 的多流窗口不同)。

**与 snapshot 的关系**。转发中继与应用 MUX 同列入 quiesce 拆除(§3.4 step 4):
快照不能带在飞的转发连接,否则 restore 出来无对端、成半开 vsock 残留。quiesce 时
guest 标记 quiescing 拒绝新 connect,逐条关 target + vsock(后者使用有界 SO_LINGER 协助拆除，残留 transport state 还依赖 §4.6 reset);
**accept 模式额外**关闭所有缓存的 accept listener(唤醒 park 中的 `Accept`,其会话已在
connReg 中一并拆除)并清空缓存——listener 不随快照留存,resume 后**懒重建**。host 侧
Forwarder 在发送 quiesce 前暂停新建,并收拢、等待活跃中继**与 pending 的 dial/accept
反向连接**;`resume`/`restore`/
`attach`(§4.3)后解除拒绝、重新受理(并 reopen accept listener 缓存)。host 的转发 listener 在同进程 quiesce 中不关闭；独立 restore 进程须用等价 `--connect` 参数重新创建/接管，不能把原进程 listener 视作随工件迁移。

## 4. vsock 控制面 + console MUX 协议

<a id="41-两类连接"></a>

### 4.1 连接分类

```
  (1) management handshake — one new conn per operation; ordinary request/response closes
        │  ops:  hello/launch · app_started · app_exited · ping · mem_report ·
        │        quiesce · restore · attach · exec · connect          (§4.3 / §4.4)
        │  wire: [4B LE len][JSON]; launch has four messages; upgrade operations retain conn

  (2) MUX long-conn          — carries one session's stdin/stdout/stderr (or a pty)
        │  born from a launch / restore / attach / exec conn, which stays open
        │  after the *_ack and switches to framed mode                (§4.5 / §4.6)
        │  the app session: at most one MUX (launch; re-established by restore/attach)
        │  each exec: its own independent short-lived MUX — 0..N concurrent (§3.6)
        │  wire: [stream:u8][type:u8][len:u16 BE][payload]   ·   per-stream flow control

  (3) forward long-conn      — splices one port-forward connection to a guest endpoint
        │  born from a `connect` conn (guest dials the target, or accepts on it
        │  for `LOCAL::TARGET`), which stays open after connect_ack and
        │  switches to the fwd frame sub-protocol                     (§3.7 / §4.7)
        │  one per accepted `--connect` local connection — 0..N concurrent
        │  wire: [type:u8][len:u16 BE][payload]   ·   TCP half-close preserved, no window

  (4) usage connection      — one reusable Host-initiated management connection
        │  sequential usage_request/usage_response; one round in flight
        │  wire: [4B LE len][JSON]; no MUX or history replay (§4.11)
```

管理连接不做帧复用；普通操作一次请求/响应，冷启动有 hello→launch→launch_ack→ack 四消息握手，升级操作保留连接。MUX 是带帧多路复用与流控
的连接,只承载某条会话的 stdin/stdout/stderr 或伪终端,**不**替代任何管理操作:
即使 MUX 开着,`quiesce` / `app_exited` 等仍各起各的短连接、并行发生。应用会话
那条 MUX 任一时刻至多一条;`exec` 每次会话另起一条独立、短生命的 MUX,与应用
会话及彼此并发互不影响(§3.6)。`connect` 端口转发(§3.7)与 MUX 同属"握手后升级
为长连接"一类,但走的是更薄的 fwd 帧子协议(§4.7,单流、无窗口、保留半关闭),
每条被接受的本地连接一条、0..N 并发。

### 4.2 通道与寻址

vsock 端口固定 `5000`,**两个方向都复用同一端口号**,身份按方向区分:

```
                      ┌────────────────────────────┐                    ┌─────────────────────────┐
                      │ sandbox-ctl (host)         │                    │ sandbox-init (guest)    │
                      │                            │                    │                         │
  guest → host  ────► │ listen UDS                 │ ◄──── CH proxy ─── │ AF_VSOCK dial CID=2:5000│
                      │ /run/sandbox/<sid>/vsock.sock_5000 │                    │                         │
                      │                            │                    │                         │
  host → guest  ────► │ dial UDS                   │ ───── CH proxy ──► │ AF_VSOCK listen :5000   │
                      │ /run/sandbox/<sid>/vsock.sock      │ + "CONNECT 5000\n" │                         │
                      └────────────────────────────┘                    └─────────────────────────┘
```

- **guest → host**:guest `connect(SockaddrVM{CID=2, Port=5000})`,host 在
  `<vsock-base>_5000` UDS 上 accept。承载:`hello/launch` 握手(其连接升级 MUX)、
  `app_started` / `app_exited` / `mem_report`
- **host → guest**:host `connect(<vsock-base>)`,**第一笔写入**为 ASCII
  `CONNECT 5000\n`(CH hybrid vsock 协议头;CH 回一行 `OK <port>\n`,host 须先排空
  再读后续 payload),CH 把其余字节代理到 guest port 5000 listener。承载:`ping` /
  `quiesce` / `restore`(其连接升级 MUX)/ `attach`(其连接升级 MUX)/ `exec`
  (其连接升级为该 exec 会话的独立 MUX)/ `connect`(其连接升级为该转发的 fwd 数据
  通道,§3.7)
- 两个方向独立寻址,互不干扰——同一时刻 host→guest `ping` 与 guest→host
  `app_started` 可并行,各用一条新连接

### 4.3 管理操作集

每个操作使用独立连接和显式握手；普通操作一请求/响应，冷启动是下表的四消息序列。“ACK 后”标明关闭或升级。

| 操作 | 拨号 | 序列 | ACK 后 | 用途 |
|---|---|---|---|---|
| **冷启动** | guest→host | `hello` → `launch{spec}` → `launch_ack{stdio}` → `ack` | **升级 MUX** | guest 报 ready,host 回 LaunchSpec;guest 准备好 app stdio 后回 launch_ack,该连接成为 MUX |
| **应用启动通知** | guest→host | `app_started{pid}` → `ack` | 关 | guest 已 fork/exec 用户进程 |
| **应用退出通知** | guest→host | `app_exited{code, term_signal}` → `ack` | 关 | primary 终态退出；guest 在预算内等 ACK，超时仍 POWER_OFF；host 收到通知后用作退出码 |
| **健康探测** | host→guest | `ping{id, t_send_ns}` → `pong{id, t_send_ns}` | 关 | host 计 RTT / 超时 / 失败数(§4.9) |
| **mem 报告** | guest→host | `mem_report{mem_report:{epoch,seq,mem_*}}` → `mem_report_ack` | 关 | guest observation;host sandbox-local controller 验证 epoch/seq 后结合 CH `vm.info` |
| **用量观测** | host→guest | `usage_request{usage_request}` → `usage_response{usage_response}` | 复用 | 只返回新的原始内存/文件系统观测, Host 唯一归并 (§4.11) |
| **快照前** | host→guest | `quiesce{skip_drop_caches}` → `quiesced{drop_caches_result}` | 关 | guest 冻结应用进程树 + 跑 prep + 关闭 MUX(§3.4),host 还须控制连接 EOF 和自身 admitted-handler drain 才可 `/vm.pause` |
| **恢复后** | host→guest | `restore{epoch, wallclock_ns, network?}` → `restore_ack{epoch,stdio,app_state}` | **升级 MUX** | 快照恢复 vCPU 起跑后 host 通知 guest;guest 先推进 mem-report epoch,再回 ACK、重连 MUX并最后 thaw。Host 在 ACK+MUX 前不启用 memory policy。clock/network 更新是 best-effort，失败也可能发 ACK；thaw 在 ACK 后，失败则 gate 保持关闭（§4.8）。`launch`/files/init/plugin 是 cold-only 配置,恢复时不重放。**RNG 重播种未实现** |
| **MUX 重连** | host→guest | `attach{epoch}` → `attach_ack{epoch,stdio,app_state}` | **升级 MUX** | 有界尝试关闭/硬丢旧 MUX，接上新连接；guest 仍冻结时先 thaw 再重新开 gate，涵盖同进程 resume 和失败 capture recovery，未冻结则跳过。Attach 是 stdio 传输替换，与 VM resume 不同；冻结查询或 thaw 失败时 ACK 可能已发，但 gate 保持关闭。 |
| **执行命令** | host→guest | `exec{spec}` → `exec_ack{stdio}` | **升级 MUX(独立会话)** | guest 为这条 `exec` 起一个兄弟进程并准备其 stdio,回 `exec_ack`,该连接成为这次 exec 会话**独立**的 MUX;并发多条互不影响;命令结束 guest 在 MUX 上发 EXIT_STATUS 再走 §4.6 关闭。详见 §3.6 |
| **端口转发** | host→guest | `connect{spec}` → `connect_ack` | **升级转发数据通道** | guest 为这条 `connect` 取得 `ConnectSpec.address` 上的目标连接——dial(默认)或 `Accept`(`spec.accept`,accept 模式可无限期阻塞,host 无 deadline park)——回 `connect_ack`,该连接成为这条转发的 fwd 帧数据通道(§4.7),保留 TCP 半关闭;并发多条互不影响;quiesce 时主动拆除(§3.4)。详见 §3.7 |
| `error` | 任意 | (终止) | 关 | handler 显式拒绝时的人类可读原因；坏 framing/JSON 也可能直接关连接，不保证总发 error |

`ATTACH` 是已有 sandbox-ctl run 生命周期的内部机制，不是其他进程接管会话的接口。
当前集成由 capture resume/recovery 显式调用 reattach；协议可以替换坏 MUX，但没有覆盖所有异常断链的自主后台重连 loop。

当前 guest restore/attach handler 回显 request epoch，并固定发送 `app_state:"running"`。
Schema 定义了 exited 常量，但 handler 未据实时状态产生 exited，也没有 `exited{code,term_signal}` 结构。
ACK 不证明应用健康或 thaw 成功，thaw 发生在 ACK 后，失败时 exec/forward/report 等 gate 保持关闭。

### 4.4 管理消息 wire format 与字段

`[4 字节 little-endian uint32 长度] [JSON payload]`,`pkg/proto.MaxMessageBytes` = **1 MiB JSON payload**，另加 4-byte prefix。
这与 host ctl.sock exec 请求的 64 KiB JSON 上限不同（[sandbox 生命周期 §6.3](sandbox_zh.md)）。
JSON 可读、调试友好，消息量少，无需 protobuf 工具链。
下方是带注释及省略号的 schema sketch，不是可直接执行的 JSON；每条消息仅填对应 type 的字段。
完整类型以 [`pkg/proto/proto.go`](../pkg/proto/proto.go) 为准。

```jsonc
{
  "type":     "<one of §4.3>",
  "phase":    "ready",                 // hello: optional hint
  "launch":   { ... LaunchSpec ... },  // launch (含 stdio 节,见 §5.1)
  "exec":     { "argv":[...], "env":{}, "cwd":"", "user":"", "stdio":{} },  // exec: ExecSpec(§3.6)
  "connect":  { "network":"tcp", "address":"127.0.0.1:49983", "accept":false },  // connect: ConnectSpec(§3.7; accept=true ⇒ guest Listen+Accept)
  "stdio":    { ... },                 // launch_ack / restore_ack / attach_ack / exec_ack: 实际启用的 channel 集合
  "app_state":"running",               // 当前 restore/attach handler 固定 running，不是实时健康保证
  "pid":      4711,                    // app_started
  "code":     0, "term_signal": 0,     // app_exited
  "id":       42,                      // ping/pong: 单调递增,host 分配
  "t_send_ns":1715000000000000000,     // host UnixNano 墙钟值，guest 原样回显；RTT 用 host time.Since
  "skip_drop_caches": true,            // quiesce: freeze+sync 后保留缓存
  "drop_caches_result":"skipped",       // quiesced: skipped/succeeded/failed;缺失为 unknown
  "epoch":    3,                       // restore/attach uint32 request epoch，ACK 回显，非通用去重
  "wallclock_ns":1715000000000000000,  // restore: 尽力更新 CLOCK_REALTIME，失败也可 ACK；attach 不带
  "network": { "ip_cidr": "169.254.4.1/31", "mtu": 1450, "nexthop": "", "hostname": "c1", "interface": "eth0" }, // restore 可选:尽力 flush-and-replace，失败 log 并继续；省略保留网络
  "mem_report": {                       // mem_report payload
    "epoch": 2, "seq": 17,              // 独立 uint64 observation epoch/seq
    "mem_total_bytes": 8589934592,
    "mem_available_bytes": 4294967296,
    "mem_free_bytes": 1073741824,
    "cached_bytes": 2147483648,
    "anon_pages_bytes": 536870912,
    "s_reclaimable_bytes": 67108864
  },
  "msg":      "<reason>"               // error
}
```

权威字段与 codec 在 `pkg/proto/proto.go`。它使用 encoding/json 与小型 internal/wireio helper，没有重量级序列化依赖。
ReadMessage 拒绝零长度、超长、截断或语法错误的 JSON；json.Unmarshal 并不通用拒绝未知字段，字段校验由各操作完成。
Guest ping 甚至会回显缺省 id/timestamp 的零值；host pinger 检查 pong 类型与预期 ID。
Restore/attach request epoch 被回显，不是通用重复请求过滤；mem_report 另有 observation epoch/seq 校验。

### 4.5 MUX 子协议

**升级**:`launch` / `restore` / `attach` / `exec` 四种操作,在双方交换完 `*_ack`
之后,这条 vsock 连接**不关闭**——后续字节进入 MUX 帧收发态。应用会话那条 MUX
任一时刻至多一条(冷启动由 launch 那条而生;快照恢复后由 restore 那条而生;MUX
因故断了由 attach 那条重建)。`exec` 每次会话另起一条**独立**的 MUX,与应用会话
及彼此可并发(§3.6)。下文 stream 集合 / 帧 / 流控 / 关闭握手对所有 MUX 通用。

**帧格式**:

```
 0       1       2               4                       4+len
 ┌───────┬───────┬───────────────┬───────────────────────┐
 │stream │ type  │  len  (u16 BE)│   payload (len bytes)  │
 │ (u8)  │ (u8)  │               │                       │
 └───────┴───────┴───────────────┴───────────────────────┘

 stream:  0 = CONTROL   1 = STDIN   2 = STDOUT   3 = STDERR   4 = PTY
 type: DATA=0, EOF=1, RESET=2, WINDOW_UPDATE=3,
       SET_WINSIZE=4, MUX_CLOSE=5, MUX_CLOSE_ACK=6, EXIT_STATUS=7
 DATA/EOF/RESET/WINDOW_UPDATE 使用相关 data stream ID(1..4)。
 SET_WINSIZE/MUX_CLOSE/MUX_CLOSE_ACK/EXIT_STATUS 使用 CONTROL(0)。
 len: u16 wire 上限 65535 bytes；本地 DATA write 按 32 KiB 切块。
 WINDOW_UPDATE payload = u32 BE credit delta。
 SET_WINSIZE payload = cols:u16 BE + rows:u16 BE。
 EXIT_STATUS payload = u32 BE 退出码(128+signal)，仅 exec flow 使用。
```

**stream 集合 = pty 模式 XOR pipe 模式**(在 `launch_ack` / `restore_ack` /
`attach_ack` / `exec_ack` 的 `stdio` 字段里声明):

- **pty 模式**:CONTROL(0)+ PTY(4)。终端没有独立 stderr——应用的 stdout+stderr
  都到这个伪终端,合并在 PTY 流上
- **pipe 模式**:CONTROL(0)+ 按声明的 STDIN(1)/STDOUT(2)/STDERR(3)。未声明的
  stream 上出现任何帧 = 协议违规(见下文状态机)

**方向约定**：host/guest bridge 按下表使用；共享 Session codec 是对称的，不独立校验 host/guest 角色。

| stream | host→guest | guest→host |
|---|---|---|
| STDIN(1) | DATA（应用输入）、EOF（host 输入结束） | RESET、WINDOW_UPDATE（补信用） |
| STDOUT(2) / STDERR(3) | RESET、WINDOW_UPDATE（补信用） | DATA、最终 stream EOF |
| PTY(4) | DATA（键盘字节）、WINDOW_UPDATE | DATA、最终 EOF、WINDOW_UPDATE |
| CONTROL(0) | SET_WINSIZE、MUX_CLOSE_ACK | MUX_CLOSE、EXIT_STATUS（exec） |

**流控**：每条 data stream 独立接收窗口，默认 64 KiB，也是初始 send credit。
发 N bytes DATA 扣 N credit，信用空时该 stream writer 等待，其他 writer 可继续。
Read 消费到半窗口后用带 **data stream ID** 与 u32 delta 的 WINDOW_UPDATE 补信用。
没有 connection-level application window。本地 DATA write 最多 32 KiB，且受实际可用 credit 限制；
不承诺严格 round-robin 公平。Control frame 不受 DATA credit 限制，但所有 frame 共享串行 socket write，
底层 transport I/O 阻塞仍会拖住 control。

**detached 输出阻塞**：quiesce 或等待 reattach 时无当前 MUX，guest output pump 等待，
pipe/pty 最终填满，app write 阻塞。没有滚动 buffer 或“丢 N 字节”正常输出策略。
Pump 持有的未写字节与内核 pipe 字节有界，可随 memory capture 保留并在新 MUX 排空。
但不会把死 MUX 的内部 stream buffer 整体迁移，也没有已写入故障 transport 的端到端 ACK/replay；
强制 generation/shutdown drain timeout 可丢残留输出，重启空档 stdin 可丢（§3.3）。
正常预期是受控短窗口，调用者若永不重连，输出可无限阻塞。

**per-stream 状态机**:

- 无效或未协商的 data stream 上收到 DATA/EOF/RESET/WINDOW_UPDATE 是协议违规，拆整条 MUX。
- 某方向 EOF 或 RESET 后再收 DATA、或 DATA 超过接收窗口，均为协议违规并拆连接
- 收到 RESET → 该 stream 双向 reset；bridge 决定本地 fd 关闭及后续 EOF/SIGPIPE，codec 本身不发送 OS signal
- 对端不要你 offer 的某条流(资源级,非协议违规)→ 不开它 / 回 `RESET` 该流,连接继续
- `EOF` 是单向半关:STDIN 上 host→guest 的 `EOF` 关应用 stdin 的写端;STDOUT/STDERR
  / PTY 上 guest→host EOF 表示最终 stream close；primary 原地重启不为每代发 EOF

**SET_WINSIZE**:host 侧 SIGWINCH 时 host 在 CONTROL 流上发 `SET_WINSIZE{cols,rows}`
→ guest 对 pty master 做 `TIOCSWINSZ` → 内核给应用进程组发 SIGWINCH。仅 pty 模式有意义。
进入 MUX 态(含 restore/attach 后)host 先发一次初始 winsize。

### 4.6 MUX 优雅关闭握手

MUX 有序关闭是应用层 FIN/FIN-ACK 握手，再由两端 close。当前 flow 始终 guest 发起、host 响应；
guest 等 ACK 期间仍处理到达帧。Host 回 ACK 后立即关；guest 用有界 SO_LINGER close。
正常 peer reset 有助移除 vsock 状态，但 linger 超时或 socket-option 失败不能当作已独立验证全部移除。

```
  guest (sandbox-init)                                     host (sandbox-ctl)
    │
    │ ── MUX_CLOSE (CONTROL frame) ────────────────────►    (guest sends no more data frames after this)
    │ ◄── may still receive WINDOW_UPDATE / leftover STDIN DATA   (guest processes these normally)
    │ ◄── MUX_CLOSE_ACK (CONTROL frame) ──────────────      host: serialized ACK → immediate close(MUX)
    │     on ACK → close(MUX) [SO_LINGER]                    host close → RST ─┐
    │ ◄── RST ────────────────────────────────────────────────────────────────┘
    ▼     normal RST drains teardown; guest SO_LINGER is bounded at 2s, not a removal proof
    back to:  listener up  ·  app session alive (app blocked on write — or frozen, if quiesce §3.4)  ·  no MUX
```

host 收到 MUX_CLOSE 后，通过串行 writer 发 ACK 并立即 close，不需知道关闭原因。
当前 responder 没有额外的通用应用 buffer flush 步骤。

guest 发起端收到 `MUX_CLOSE_ACK` 后将该帧作为 MUX read loop 的终态,先停止读取,
再由调用流程 close 连接。禁止 ACK 后再发起一次 read:raw vsock fd 的 close 与新 accept
可能复用相同 fd 号,旧会话若残留一次读取就会抢走新连接的 4-byte proto 长度头或首个
4-byte MUX frame header。

**为什么两端 close 与 linger 都重要**：guest 单方 virtio-vsock close 可能保留 8 s deferred-removal，等待 peer reset 或超时。
Host 回 ACK 后 close 提供 peer teardown；guest SO_LINGER 最多等 2 s，设置失败会 log。
Primary MUX ACK 最多等 5 s，失败/超时强制关；exec MUX 使用同样有界机制，完整 session drain 在 quiesced 前完成。
这些组合 barrier 防止已入场 MUX/exec/forward/handshake handler 继续跨 pause 存活，
不等同于逐一观察证明全部内核 socket remnant 已消失。

这个 barrier **不能单独保证 guest 内核里没有任何旧连接状态**:刚在 gate 前正常结束
的短连接已经离开 registry,最终承载 `quiesced` 的控制连接也只能在 ACK 写出后关闭。
Cloud Hypervisor restore 又会重建空的 Unix vsock backend,不会序列化 connection map。
平台 CH 补丁用两条独立不变量收口:保存 `local_port_last`,避免新连接复用旧四元组;
snapshot 时向 guest used event ring 预发布 `VIRTIO_VSOCK_EVENT_TRANSPORT_RESET`,
restore activation 只重发 IRQ,让 Linux 清理全部 connected sockets,同时在 guest
event-queue kick 确认前 gate backend RX。listener 不受 reset 影响。首个 restore
REQUEST 只能在清理确认后进入 guest，不依赖 sleep 等待 transport reset；§4.10 的独立、有限 pre-request CONNECT retry 仍存在。

**三个触发点**(同一握手):

1. **quiesce 流程**(§3.4 第 5 步)——guest 在 quiesce 流程靠后一步关闭 MUX。
2. **exec 会话结束**(§3.6)——exec 命令退出后,guest 在该会话 MUX 上**先把
   stdout/stderr/pty 全部 EOF**,**再发 `EXIT_STATUS`**(payload = 退出码,被信号杀
   为 128+signo),**然后**走同一套 guest 发起的 `MUX_CLOSE` 握手。host(`sandbox-ctl
   exec`)收到 `EXIT_STATUS` 即得知命令退出码;继续处理紧随其后的 `MUX_CLOSE`,
   回应 ACK 并关闭本端连接后再结束、还原终端。这个本地协议屏障**不依赖底层
   对端连接关闭的传播**(经 CH hybrid-vsock 代理时,对端关闭往往要等下一次 I/O 才被
   察觉,故用显式 `EXIT_STATUS` 帧而非"连接关掉"作为命令完成信号)。
3. **ATTACH 流程**——host 拨新连接发 `attach`,guest 在回 `attach_ack` 之前先把
   **旧 MUX 连接**优雅关闭。兜底:旧 MUX 大概率正因 vsock 断了才触发重连,这时对它
   发 `MUX_CLOSE` 直接报错 → guest **降级为硬丢弃**旧连接,不阻塞 attach。统一逻辑:
   收到 `attach` → 尝试优雅关旧 MUX、失败或有界超时后硬丢 → 新连接 attach_ack → 新 MUX 采用默认 stream window
   （没有另一个 window 协商字段）、重发 winsize、恢复 pump 与尚未写出的 app output。

**MUX 因 vsock 异常突然断**：没有优雅握手，guest 失效旧 session，pump 等显式 attach/restore，listener 仍在。
当前 host 不存在覆盖全部意外断链的自动重连 loop；capture resume/recovery 显式 reattach，其他调用者须处理转发中断（§4.3 / §4.10）。

### 4.7 connect 转发帧子协议(fwd)

`connect` 连接在 `connect_ack` 后切到 fwd 帧子协议(包 `pkg/fwd`,host 与
guest 共享),把这条转发的本地连接与 guest 目标连接双向 splice。**单流**——一条
vsock 连接只承载一条转发流,故无流 ID、无应用窗口;流控就是 vsock 连接自身的内核
缓冲背压(与 §4.5 MUX 多流共享一条连接才需窗口不同)。

```
 帧:  ┌────────┬───────────────┬────────────────────────┐
      │  type  │  len (u16 BE) │   payload (len bytes)  │
      │  (u8)  │               │                        │
      └────────┴───────────────┴────────────────────────┘
   wire 载荷上限 65535 bytes；本地 DATA write 每帧最多 32 KiB
   DATA(0)  载荷字节(本地更大 write 切多帧)
   EOF (1)  半关闭:发送方写方向结束 → 对端 splice 连接 CloseWrite()(SHUT_WR),仍可继续收
   RST (2)  异常中止:双向硬关
```

**为何用帧而非转发传输层关闭**:见 §3.7——CH hybrid vsock proxy 不把 `SHUT_WR`
翻译过 UDS↔vsock 边界,关闭信号必须以帧(数据面字节)承载才忠实保真。

**半关闭中继**(host/guest 对称:framed 侧恒为 vsock 连接,plain 侧 host 为本地
连接、guest 为目标连接):

```
 plain → framed:  字节 → DATA 帧;干净 EOF → EOF 帧;读错 → RST 帧
 framed → plain:  DATA → plain 写;EOF → plain.CloseWrite();RST/传输错 → abort
 收尾:两个方向各自 EOF(半关闭)后才整体 close;任一 abort 立即双关解阻塞
```

故一端 `shutdown(SHUT_WR)` 后另一端仍可回数据,直到它也半关——通用端口转发语义。
所有关闭(干净收尾 / 内部错误 / 外部 quiesce 拆除)经单个 `sync.Once` 收口,底层
连接恰好关一次(guest 的裸 fd `vsockConn` 双关会误伤复用 fd)。quiesce 时(§3.4)
guest 对 vsock 连接使用有界 SO_LINGER；完整 transport reset 契约仍见 §4.6。

### 4.8 时序

普通箭头表示管理消息，双线表示 MUX；“this conn → MUX”标明握手连接升级。
Host→guest 始终 dial CH base UDS 并完成 CONNECT/OK；不是 host dial CID 2，CID 2 是 guest 视角的 host。

**冷启动**：

```
sandbox-ctl                                               sandbox-init (guest)
listen <base>_5000
spawn CH -----------------------------------------------> kernel boot
                                                          lo UP；bind/listen :5000
                      <--- hello{ready} ------------------ dial CID=2:5000 [conn A]
launch{spec,stdio} --------------------------------------> 并发组盘；join；switch-root
launch 写完即起 ping ticker                                network/mounts/files/init；准备 stdio
                      <--- launch_ack{stdio} ------------- 环境就绪，尚未 final exec
ack ----------------------------------------------------> conn A -> MUX
initial SET_WINSIZE =====================================> tty；fork/exec primary
                                                          app fd 为 PTY slave/pipes；bridge 启动
                      <--- app_started{pid} [conn B] ------
ack；close conn B
ping -> pong；guest EOF [conn C..k]                        MUX stdio + WINDOW_UPDATE
                      <--- app_exited{code,term_signal} --- 终态退出，有界输出 drain
ack ----------------------------------------------------> POWER_OFF -> CH exit 0
```

**MUX 重连（ATTACH）**：

```
sandbox-ctl                                               sandbox-init
capture resume/recovery 调 reattach                        旧 MUX 可能不存在或已坏
dial <base> UDS；CONNECT 5000；读完 OK
attach{epoch} ------------------------------------------> 有界优雅关闭/硬丢旧 MUX
                      <--- attach_ack{epoch,stdio,app_state}
initial SET_WINSIZE =====================================> this conn -> MUX；恢复输出 pump
                                                          查冻结状态，仅仍冻结时 thaw
                                                          thaw 成功后再开 exec/forward/plugin/
                                                          app-restart/report gate
恢复 MUX 流                                               恢复 MUX 流
冻结状态查询或 thaw 失败时 ACK 可能已发；guest log，gate 仍关闭。
```

**quiesce → snapshot**：

```
sandbox-ctl                                               sandbox-init
暂停/排空 ping；gate 新 exec/forward
dial <base> UDS；CONNECT 5000；读完 OK
quiesce{skip_drop_caches} -------------------------------> 先排空 exec、mem_report
                                                          freeze app；等 frozen 1
                                                          sync；仅显式请求才 drop_caches=3
                                                          拆 forward/accept listener
                      <=== MUX_CLOSE ==================== 关闭 primary MUX
MUX_CLOSE_ACK =========================================> 有界等 ACK，再 lingered close
host 立即关本端 MUX
                      <--- quiesced{drop_caches_result} --- 控制连接回复并 close
等控制 EOF；join 已入场 host exec/forward half
必需 barrier 完成 -> /vm.pause、/vm.snapshot

快照：listener 保留，primary 冻结，无 active MUX；
      CH local_port_last 已保存；
      TRANSPORT_RESET 已写 used ring，reset-pending 状态已保存。
Guest 有界 close 兜底与 CH transport reset 分别承担不同职责。
```

**restore**：

```
sandbox-ctl                                               sandbox-init
CH restore activation 重发已入快照的 TRANSPORT_RESET IRQ（不再消费 descriptor）
/vm.resume OK；backend RX 仍 gate ----------------------> 重置 connected socket，保留 listener
dial <base> UDS；CONNECT 5000
  REQUEST 等 RX gate
                      <--- event-queue kick -------------- reset 已处理；CH 放开 RX
restore{epoch,wallclock_ns,network?} --------------------> >0 时尽力 clock_settime
                                                          可选尽力 network replace
                                                          暂停的 mem_report 切新 epoch
                      <--- restore_ack{epoch,stdio,app_state}
initial SET_WINSIZE =====================================> this conn -> MUX；恢复输出 pump
                                                          最后 thaw app
                                                          仅 thaw 成功才重新开 gate
SafeTarget normalization；开放 observation epoch           barrier 后新 epoch/seq 恢复采样
重启 ping ticker                                          listener 跨 capture 不变
Clock/network 失败 log 并不阻止 ACK；ACK 先于 thaw，不证明应用健康或 thaw 成功。
```

**listener 跨快照保持打开**：quiesce 若关掉它，restore/attach 无人接收。
Transport reset 遍历 connected sockets，不关 bind/listen sockets。
CH 从保存的 local_port_last + 1 继续分配 host local port；
重置 transport epoch 与保持端口分配连续性是两个独立职责。

### 4.9 ping 健康探测

探测 guest agent 的存活与延迟，**不是应用健康检查**。
默认 fatal threshold=0，仅记录统计而不自动杀 VM。
正的 --ping-fatal-threshold 会在连续失败达到阈值后调用 lifecycle callback，向 CH 发 SIGTERM，
必要时继续 host shutdown escalation；必须配有界 timeouts.ping。

| 事件 | ticker 状态 |
|---|---|
| host 写完 launch | start，立即发首个 probe |
| restore ACK/MUX 设置成功 | start/restart |
| capture admission 关闭 | pause；等待在途 pong + guest EOF，独立 8 s capture budget，超时 cancel/join 并令捕获失败 |
| 同 VM recovery/resume 成功 | 必需 barrier 后恢复 |
| CH 退出 | stop |

**参数**：

| 参数 | 值 | 含义 |
|---|---|---|
| interval | CLI 默认 1 s；package PingerConfig 可覆盖 | ticker 节拍，不保证每次起点严格间隔 1 s；阻塞中的 probe 可延后 tick 处理 |
| timeout | sandbox.yaml timeouts.ping 默认不强制；production profile 200 ms | 探测 dial/write/read/guest-EOF 预算。仅 package 默认是 proto.DeadlinePing=200 ms，lifecycle 会传入配置行为；正 fatal threshold 要求有界值 |

普通 probe timeout 与 capture drain budget 独立；前者可不强制，但不能让 capture 屏障无限延长。

**实际统计接口**：sandbox-ctl stats JSON 的 ping 对象字段为 attempts、success、timeout、dial_error、
rtt_avg_ns、rtt_max_ns、rtt_p50_ns、rtt_p95_ns、rtt_p99_ns，
定义见 [pkg/guestlink/launchclient.go](../pkg/guestlink/launchclient.go)。
Count/average/max 覆盖 sandbox 生命周期，percentile 只取最近 256 个成功样本。
ping_attempts_total、ping_rtt_ms_p99 等并非当前实现导出的字段名。

Wire t_send_ns 是 host UnixNano 墙钟，guest 原样回显。
RTT 在完整 pong/guest-EOF exchange 后用 host time.Since(tSend) 计算，保留 Go monotonic 时间分量，
不减回显的墙钟值，也不要求 guest 时钟同步。
错误文字含 deadline / i/o timeout / timed out 时记 timeout；其他失败（包括错误 pong type/ID）记 dial_error。
这是 best-effort 分类，不是完整 typed network-error 分类。

### 4.10 失败语义

**Guest（sandbox-init）**：

- Listener accept 错误记日志，不杀 init。
- 未知 type 回 error{msg}；长度/JSON 解码失败记日志并关连接，不保证 error 响应。
  校验由操作实现，不是统一拒绝未知字段；ping 缺 id 会回显 0。
- MUX 未协商 stream、EOF/RESET 后 DATA、窗口超限或必要 payload 格式错误会拆 MUX；
  pump 可等待显式 attach/restore。
- Primary 终态路径在 POWER_OFF 前尝试 app_exited；通知失败或 ACK 未到不阻止 poweroff。
- Exec 在 quiesce 中、空 argv 或启动失败时拒绝；会话丢失会杀与 namespace helper 通过父死信号绑定的命令（§3.6）。
- Restore clock/network 失败 log 并继续；restore/attach thaw 失败时 gate 保持关闭，即使 ACK 已发（§4.3）。

**Host（sandbox-ctl）**：

- 各操作分别处理失败。Ping 有前述统计与可选 fatal threshold；
  没有通用相应 *_error_total 指标，也不能笼统声称控制失败绝不终止 VM。
- Restore 请求发送前，CH 已收 CONNECT 但在 OK 前 EOF/reset 时，host 从 25 ms 起步退避，
  指数退避上限 200 ms；每次 CONNECT/OK 尝试最多 2 s，且不超过剩余总 restore budget。
  可重试错误会一直重拨至总 deadline 或 context cancel，而非整段只重试 2 s；此时尚未写 restore。
  一旦开始写请求，任何失败都 fail closed，绝不重放。最终没有 restore_ack，
  则通过 CH VM shutdown 路径终结本次恢复并返回错误。
- 意外 MUX error 可使转发中断、app 输出反压。当前只有 capture recovery/resume 显式调用 reattach；
  不存在通用自动重连 loop 或独立 takeover API。
- 没有 quiesced，或控制 EOF/admitted-handler 屏障失败，均放弃捕获。
  Host 尝试 resume/reattach；成功才让同一 VM 继续运行，失败则终止 VM。
  “capture 失败”本身不保证 VM 继续。

Host 的 timeouts.* 项以 sandbox.yaml 配置为准，文档指明的默认是不强制 response deadline，
连接建立仍有独立限制（[sandbox 生命周期 §3.1](sandbox_zh.md)）。
重试实现见 [guestlink/pinger.go](../pkg/guestlink/pinger.go)，forward teardown 见
[sandbox-init/connect.go](../cmd/sandbox-init/connect.go)。其他预算来自 proto 常量或 guest 等待：

| 消息 | deadline/budget | 备注 |
|---|---|---|
| hello | guest dial retry 5 s；host initial read 用 timeouts.app_notify，默认不强制 | 早期 host listener 缺席时指数退避 |
| launch_ack（host 等待） | launch.start_timeout；空/0 无界 | 所有 spec/init 完成才 ACK；生产可设 bound 防止 guest 卡死后无限等，之后转 MUX |
| app_started | guest dial 后设置 200 ms socket I/O timeout，dial 另有 5 s retry；host 用 timeouts.app_notify | 不是总共 200 ms 的 dial+exchange deadline；raw AF_VSOCK 使用 per-I/O socket timeout |
| app_exited | 同 app_started | 没有 ACK 仍 POWER_OFF |
| ping | timeouts.ping 默认不强制，production 200 ms；capture drain 至多 8 s | 普通失败更新统计；capture 超时 cancel/join 并失败；fatal threshold 须有界 |
| quiesce | 8 s | exec/report drain、freeze、sync、可选 drop、forward 与 MUX teardown 的协议预算 |
| restore | timeouts.restore 默认不强制 response deadline | request 前 EOF/reset 可重试至总 deadline 或 context cancel；每次 CONNECT/OK 最多 2 s 或更短的剩余预算；写请求后不重放；demand paging 可延后恢复；之后转 MUX |
| attach | 5 s | 替换握手，之后转 MUX |
| exec | 10 s | guest fork/exec 与 PATH 解析后 ACK；只限握手，运行命令前清 deadline |
| connect | dial 模式握手 10 s，含 guest target dial ≤5 s | 转 fwd 后清 deadline；accept 模式写请求后清 ACK deadline，可无限 park，但 pending conn 仍登记以便 cancel/quiesce |
| mem_report | guest dial retry 5 s + 独立连接后 4 s exchange deadline；host 用 timeouts.app_notify | launch barrier 后立即采样，此后每 5 s；失败保留同 payload 重试 |

<a id="usage-observations"></a>

### 4.11 原始 Usage 观测

Host 使用同一管理 listener 和带长度前缀的 JSON framing, 但连接专用且可复用.
不改变普通短连接、ping、mem_report 或 MUX. `pkg/proto/usage.go` 定义原始
payload, 单位、指标计算、输出及记录格式由
[usage](usage_zh.md#4-指标单位和计算) 统一说明.

```jsonc
{
  "type": "usage_request",
  "usage_request": {
    "run_epoch": "<host-run-identity>",
    "request_id": "17",
    "read_budget_ns": "250000000"
  }
}
{
  "type": "usage_response",
  "usage_response": {
    "run_epoch": "<same-host-run-identity>",
    "request_id": "17",
    "memory": {
      "status": "ok", "read_duration_ns": "<elapsed>",
      "domain": "<validated-node-zone-domain>",
      "present_pages": "<raw>", "buddy_free_pages": "<raw>",
      "pcp_free_pages": "<raw>", "page_size": "<raw>"
    },
    "filesystems": [{
      "disk": "root", "incarnation": "<pinned-filesystem-identity>",
      "status": "ok", "read_duration_ns": "<elapsed>",
      "blocks": "<raw>", "bfree": "<raw>", "block_size": "<raw>",
      "fragment_size": "<raw>", "fs_type": "<raw>"
    }]
  }
}
```

以上是 schema 示意, 不是实测数据. 磁盘 ID 为按受管理盘顺序排列的 `root` 和
`disk-0` 至 `disk-7`, 最多九项; Host 拒绝重复/未知 ID. 每项状态为 `ok`、
`busy`、`timeout`、`unsupported`、`invalid` 或 `error`. 缺少必需字段不
代表零. 整轮请求失败成为 Host Gauge missing, 不重放以前的原始 payload.

至多一个连接和请求活动. 每来源只有一个属于 Guest 实例的执行槽, 不随连接/
请求新建. 各来源独立读取, 不持 mutex 做文件系统 I/O. 到分项 deadline 时
返回内存及健康盘已经完成的新值, 尚未结束的项返回 timeout. 后续请求在原 syscall
完成前返回 busy; 旧请求结果丢弃, 不换时间戳重新发布. 重连/超时不创建替代
worker, 不积累无界结果队列.

Host 整轮 deadline 为 `min(sample_interval, 1s)`, 包含 dial、握手及所有
读写. 建立连接后至多将剩余预算的一半分配给 Guest 读取. Guest 另外保留其
read budget 的四分之一用于编码/传输, 仍早于 Host 剩余 deadline. Socket
操作按绝对 deadline 剩余量执行, 不在每次 partial Read/Write 或 EINTR 后
重获完整预算. 空闲复用连接没有继承握手 timeout, 等待下一请求或 close/quiesce;
每个新请求建立新的有界 round, 不增加 keepalive ticker. Host 断连/deadline
结束交换, 不等于原始来源 syscall 已取消.

Run epoch 和严格递增 request ID 跨重连保留. Restore transition 授权之前,
Guest 拒绝重复/旧 ID 或不同 Host epoch. Host 核对 response 类型、epoch、ID、
分项 identity 和 duration, 丢弃旧/迟到结果. Guest 返回 elapsed read duration,
不发送用于与 Host UTC 相减的时间戳; 实际请求窗口及单调中点由 Host 提供.

Freeze/restore 前关闭 usage 准入、推进代次, 并有界 shutdown/join 旧连接.
即使读取不可取消, 原 source slot 仍保留. 不捕获持锁等待旧 Host 的 worker,
不声称网络 FD 关闭会取消 statfs/proc. 成功 thaw 后重新开放, 同 VM attach
保留 Host epoch, true restore 清除旧 epoch/ID 边界. Thaw 失败保持关闭.
该原始协议不改变业务文件系统同步或既有资源控制器.

## 5. 应用契约

### 5.1 launch 配置(LaunchSpec)

来源:`boot.root.base` 末尾 ZIP 内嵌 `config.json`(OCI image runtime config)⊕
sandbox.yaml `launch:` 节(yaml override 优先,Env merge),host sandbox-ctl 合并后
通过 launch 协议下发。`stdio` 节由 host 侧 `sandbox-ctl run` 的 `--tty` / `--stdin`
/ `--stdout` / `--stderr`(及它们的 `-from`/`-to`)解析决定(详见
[`sandbox_zh.md`](sandbox_zh.md) §2.2)。

```jsonc
{
  "exec":    "/usr/bin/foo",
  "args":    ["arg1", "arg2"],
  "env":     {"PATH": "...", "HOME": "/root"},
  "workdir": "/",
  "restart": "never",                    // never|on-failure|always(in-place 重启 + 退避)
  "cgroup_control": false,                // 默认 false:进程见 /;true:空根下放 controller,进程见 /init
  "placeholder": false,                  // true → 不 exec 外部程序,占位锚点等待停机;exec 互斥;host 强制 restart=always
  "share_pid": false,                    // true → 应用进 sandbox-init 的 PID ns(pid_namespace=shared)
  "user":    "0:0",                      // uid:gid 或 name:group(guest 侧 /etc/passwd 解析);空 → root
  "stop_signal":   15,                   // 停机信号编号(host 已从名字解析);0 → SIGTERM
  "stop_grace_sec": 10,                  // guest PID 1 停机宽限秒数；0 → 默认 10s，不保证 host teardown 走此路径
  "network": { "interface": "eth0", "ip_cidr": "169.254.1.1/31", "mtu": 1500, "nexthop": "", "hostname": "my-sandbox" },
  "mounts":  [ {"target": "/tmp", "type": "tmpfs", "options": "nosuid,nodev,mode=1777"},
               {"target": "/var/log", "type": "empty"} ],
  "files":   [ {"path": "/etc/resolv.conf", "mode": "0644", "owner": "0:0", "content": "nameserver ..."} ],
  "init":    [ {"exec": "/bin/sh", "args": ["-c", "..."], "env": {}, "workdir": "/", "user": "0:0", "timeout_ms": 0} ],
  "plugins": [ {"exec": "/usr/bin/sidecar", "args": [], "env": {}, "workdir": "/", "user": "0:0", "restart": "always"} ],
  "stdio":   {
    "tty":     true,                     // true: 给应用一个伪终端(pty 模式);false: pipe 模式
    "winsize": {"cols": 80, "rows": 24}, // tty 模式的初始窗口大小
    "stdin":   false,                    // pipe 模式:是否开 stdin 通道(否则 app fd 0 = /dev/null)
    "stdout":  true,                     // pipe 模式:是否开 stdout 通道
    "stderr":  true                      // pipe 模式:是否开 stderr 通道
  }
}
```

`mounts` / `files` / `init` 的应用见 §3.2;`mounts[].type==empty` 的卷在 switch-root
前以 raw ext4 source 建立(§3.1)。Disk MountSpec 另携带解析后的 disk_index/disk_overlay/disk_devs（§3.1）。
FileSpec 还支持 read_only，省略时 bind 可写。Tmpfs 注入避免直接写盘，但进程可复制到磁盘、内容也可进入 memory snapshot，
参见 sandbox 生命周期的 persistent/ephemeral 暴露边界。Wire NetworkSpec 使用 **ip_cidr**，不是 host YAML 的 ip。
`start_timeout` 不在 LaunchSpec——它只约束 host 侧
等待 launch_ack(见 §4.10)。

### 5.2 用户应用看到的环境

- **PID 1**:`pid_namespace=private`(默认)时用户应用是自身 PID ns 的 PID 1
  (CLONE_NEWPID);`shared` 时应用在 sandbox-init 的 PID ns(非 PID 1),由 sandbox-init
  做 reaper 收割其孤儿后代,并仍能看到 sandbox-init PID 1。伴生 plugin 与应用同 rootfs/cgroup/网络,但始终在
  sandbox-init 的 PID ns(private 应用看不到它们)。
- **cgroup namespace**:真实 `/sys/fs/cgroup/app` 始终是 namespace/freezer 根。
  `cgroup_control=false` 时 primary/restart/plugin/native exec 在真实 `/app`,scoped
  cgroup2 与 `/proc/self/cgroup` 显示 `0::/`;`true` 时真实 `/app` 无直属进程且
  subtree controllers 已校验启用,上述进程在真实 `/app/init`,显示 `0::/init`。
  这里的 `/init` 是 cgroup 长期进程组,不是顶层 `init: []`;cgroup namespace 是视图与
  管理根,不是额外安全边界。
- **mount namespace**:私有挂载 ns,起始视图与 sandbox-init 相同(overlayfs 合并的
  /，或 single ext4 根，以及 isolated 时自挂的 /proc)
- **网络**:配置网络源时为 eth0(virtio-net,host TAP 后端),IP 由 sandbox-init 配好;
  无网络源时不挂 virtio-net,仅保留 `lo`,vsock 控制面仍可用
- **/dev**:`devtmpfs`(/dev/null、/dev/random、/dev/urandom 等);`/dev/pts`(devpts)
- **/run**、**/run/shm**:runtime 自动挂载的 tmpfs(无需声明)
- **/opt/sandbox-runtime**(平台保留):随 runtime 镜像出厂的 Guest 侧发布件根,经 bind 以
  **只读**出现在该路径(遮蔽 app 镜像在此的内容);应用可执行其中的工具(如
  `/opt/sandbox-runtime/bin/envd`),但不应把自己的文件放到此路径下
- **声明的挂载与注入文件**:`mounts` 的 tmpfs / empty 卷已就位(empty 卷遮蔽镜像该路径
  原内容);`files` 注入的文件已 bind 到目标路径(只读卷不可写);均在应用启动前完成
- **运行身份**:由 `launch.user` 决定(默认 root);非 root 时已 setgroups/setgid/setuid
- **stdin/stdout/stderr**:
  - tty 模式:fd 0/1/2 是同一个伪终端的从端,`isatty()`=true,有控制终端与 job
    control,窗口变化收 SIGWINCH;stdout 与 stderr 在该终端上合并
  - pipe 模式:fd 0/1/2 是普通管道(`isatty()`=false);未声明 stdin 时 fd 0 = /dev/null
  - **不**是 `/dev/console` / `/dev/hvc0`——应用输出不混入内核 dmesg,反之亦然
- **vsock**：CID=3 + port 5000 属于平台保留约定；这不阻止 privileged guest 创建 AF_VSOCK socket，不能作为安全边界
- **balloon / mem hotplug**：平台管理，不要求应用实现控制协议，但内存可用量、reclaim 和时间影响仍可观测

### 5.3 退出语义

- `restart: never`:应用退出 → sandbox-init 收尾 MUX、发 `app_exited{code,term_signal}`
  → reboot → CH 退 → sandbox-ctl 退,sandbox 销毁。退出码(或致死信号)透传到
  sandbox-ctl 的进程退出码，前提是通知已成功到达；失败时的传播限制见 §4.10
- `restart: always`:应用退出 → **原地重启**(同进程内 refork + rewireApp,§3.3),
  沙箱长活、stdio MUX 跨重启存活,host 链路不断
- `restart: on-failure`:非零退出 / 被信号杀 → 原地重启;干净退出 → 同 `never`
  (通知 + reboot)

原地重启是同进程内 fork,**不**重新走整个 sandbox 启动序(退避 10ms→60s,存活满
60s 重置;停机信号或 quiesce 窗口期间不重拉,§3.3)。

### 5.4 信号处理

- sandbox-ctl 通过 vsock 发 quiesce / restore / attach / ping，这些消息不直接给 primary app 发信号。
- **Host VMM 停机与 guest app 停机是两条路径**。Host 收 SIGTERM/SIGINT 后，先取消 pre-spawn/controller work；
  CH 已运行时，lifecycle 做 memory.high 准备后尝试 CH API 的 vmm.shutdown，有需要时退回向 CH 发 SIGTERM，
  请求后 5 s 未退升级 SIGKILL，第二次信号立即升级。这不保证向 guest PID 1 送达 SIGTERM 或完成 app stop grace。
  见 [lifecycle shutdown](../pkg/sandbox/lifecycle.go) 与 [run signal handling](../pkg/sandbox/run_signal.go)。
- **Guest PID 1 自身收到 SIGTERM/SIGINT 时**，置 shutdown，向 primary PID 转发 launch.stop_signal
  （默认 SIGTERM，来自已解析的 image/YAML 策略），等待 launch.stop_grace_period（默认 10 s），
  超时 kill，随后 poweroff。这些配置约束 guest 路径，不是 host VMM teardown 的已实现握手保证。
- tty 模式 host 终端 raw 时，Ctrl-C（0x03）按字节经 MUX PTY 送入 guest，
  由 guest line discipline 转成应用 SIGINT。Host 停 sandbox 或 terminal escape 是另一路径
  （[sandbox 生命周期 §2.2](sandbox_zh.md)）。
- quiesce.signal（§3.4）是未实现提案。

## 6. 扩展点

| 扩展 | 引入条件 | 影响章节 |
|---|---|---|
| 应用 quiesce hook | 应用需要在 freeze 前接收 best-effort preparation signal;当前提案没有完成 ACK,不能作为自定义一致性屏障 | §3.4 quiesce 扩展项表 |
| 应用 stderr 旁路 | pipe 模式已经用独立 MUX stream 与 host sink 分流 stdout/stderr，不需额外 vsock 旁路；PTY 天然合并，若要独立 stderr 须另设计 I/O 契约 | §3.5 / §4.5 |
| 自带 vmlinux | 用户需要平台 kernel 未带的特性(nested userfaultfd / user·net 命名空间 / 别的 kernel 特性);平台 kernel 已含 cgroup cpu/memory/io/pids 控制器 + NFS(v3/v4) + FUSE | sandbox-ctl `boot.kernel: file://...` |
| 自带 sandbox-runtime | 用户应用对 PID 1 / supervisor 有特殊要求(罕见) | 须满足 guest ABI、架构与工件 identity 约束；兼容只读自定义 runtime 仍可共享 DAX 页，并非自定义必然丢失共享 |
| 独立发布件设备 | Guest 侧 payload(envd 等)需独立于 runtime 镜像迭代（提案，不是已有独立热补丁支持） | §2 / §3.1:bind 源改为独立只读 EROFS 设备(多一 virtio 盘 + vhost 后端) |

## 7. See Also

- [usage_zh.md](usage_zh.md) — Host-only 归并、单位、查询及保存.
- [`sandbox_zh.md`](sandbox_zh.md) §2.2(`run` 的 `--tty` / `--console` / stdio 标志)、
  §5.2(CH 冷启动命令行)、§6.2 / §6.3(snapshot 时序 / ctl.sock 协议)、§7(恢复)
- [guest kernel 文档](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/vmlinux_zh.md) —— guest kernel 启用的 namespace /
  文件系统 / virtio-console / 网络功能为何如此
- [Cloud Hypervisor 文档](cloud-hypervisor_zh.md) §5.2 —— vsock hybrid 代理:host
  侧映射到 UDS 的 CONNECT 行格式;`--console` / `--serial` 的用法
- [guest-runtime native build](https://github.com/kuasar-sandbox/guest-runtime/blob/main/native-deps/README_zh.md) §2.1 —— mkfs.erofs 构建(`make -C guest-runtime sandbox-runtime` 的前置工具)
- [项目系统设计](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/kuasar-sandbox_zh.md) §4 —— 模板实例化与暂停/恢复的用户语义
