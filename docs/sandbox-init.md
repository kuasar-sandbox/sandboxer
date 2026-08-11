# sandbox-init — guest PID 1 ABI

`sandbox-init` 是每个 sandbox guest 内的 PID 1,由 `sandboxer/cmd/sandbox-init`
构建,再由 `guest-runtime` 打包进 `sandbox-runtime.bundle`。本文档定义
`sandbox-init` 与 host 侧 `sandbox-ctl` 之间的 ABI:启动期 rootfs 组装、launch
握手、应用拉起、生命周期监督、stdio/console 转发、exec/attach/quiesce 控制面。

`sandbox-runtime.bundle` 的镜像打包、内置 guest payload、版本发布与构建流程见
`guest-runtime/docs/sandbox-runtime.md`。本文件只讨论镜像内 `/sbin/init` 的
运行契约,以及 `sandbox-ctl` 启动 microVM 后如何与它对接。

## 1. 概述

### 1.1 设计目标

| 目标 | 实现方式 |
|---|---|
| 启动快 | 单一静态 Go 二进制 PID=1,无 systemd / dracut / busybox 链路 |
| 跨实例可去重 | 启动期内存页内容确定;sandbox-init 自身镜像版本固化 |
| 跨 sandbox 共享 | virtio-pmem + DAX 让 host page cache 一份 RAM 跨 N 个 sandbox |
| 一 VM = 一 app | sandbox-init clone(NEWPID\|NEWNS) 让用户 app 看到自己 PID=1 |
| 生命周期可寻址 | exit / SIGTERM / quiesce / restore 都通过 vsock 通知 host |
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
        kernel dmesg  ◄── --console ───┤ ◄── hvc0 (virtio-con) ── │    phase 2: vsock launch handshake
        (host captures to stderr/file) │                          │              + app stdio wiring
                                       │                          │    phase 3: supervisor loop
                                       │                          │
                                       ├──── conn: launch ───────►│    fork/exec user app
                                       │     (conn → MUX) ◄══════►│      app stdin/out/err ↔ MUX
                                       ├──── conn: ping ─────────►│      app sees itself PID 1
                                       │◄─── conn: app_started ───┤
                                       │                          │
   /run/sandbox/<sid>/vsock.sock  ◄────┤◄─── conn: app_exited ────┤    user app exits
                                       │                          │    reboot()
                                       │◄─── CH exits ────────────┤
```

### 1.3 不做的事

- **无容器运行时**:平台直接管理 sandbox 生命周期,不引入 runc / crun / podman
- **无 systemd / OpenRC**:进程监督由 sandbox-init 自己写的 supervisor 完成
- **无 busybox / util-linux**:所有功能(mount / mkdir / chdir / chroot /
  reboot / openpty)通过 Go syscall 完成,镜像只装一个 sandbox-init
- **无 /etc / /usr**:guest rootfs 由用户镜像(blk0 base)提供;sandbox-runtime
  只提供 /sbin/init 和挂载点
- **不复用控制面短连接做 stdio**:管理操作各用一条短连接(§4.3);只有 launch /
  restore / attach / exec 四种操作的连接在握手后升级为长连接 MUX(§4.5)。应用会话
  那条 MUX 任一时刻至多一条(launch 生,restore/attach 续);exec 每次会话另起一条
  独立、短生命的 MUX,可并发多条(§3.6)

## 2. sandbox-runtime.bundle 镜像结构

```
/sbin/init                sandbox-init 静态 Go 二进制,~10-15 MiB
/proc/                    空挂载点
/sys/                     空挂载点
/dev/                     空挂载点
/overlay/lower/           空挂载点(blk0/vda EROFS 挂入点 = overlayfs lowerdir)
/overlay/upper/           空挂载点(blk1/vdb ext4 挂入点;内含 upperdir/ workdir/ volumes/)
/sysroot/                 空挂载点(overlayfs 合并目标 + chroot 目标)
/opt/sandbox-runtime/     Guest 侧发布件根(平台保留);phase1a 末 bind 进 /sysroot 同名路径
```

**除挂载点与 `/opt/sandbox-runtime/` 外无其他文件**——无 /etc、/usr、/var、
/lib、共享库等,所有额外功能由 sandbox-init 通过 Go syscall 实现。

`/opt/sandbox-runtime/` 是 **Guest 侧发布件根**:随 runtime 镜像出厂的平台运行时
组件放此处,经 virtio-pmem + DAX 跨 sandbox 共享一份、与 sandbox-init 原子同版;
phase1a 把它 bind 进新 root 同名路径(§3.1),应用在自身 rootfs 内以**只读**看到它,
且该路径遮蔽 app 镜像在此的任何内容(§5.2)。实际镜像由 `guest-runtime` 打包,
首版内置 `/opt/sandbox-runtime/bin/{envd,flatten-ctl,mkfs.erofs}`。

镜像小(~15 MiB)+ DAX 直接映射 host page cache,N 个 sandbox 共享同一份内存
工作集(实际 ~10 MiB 驻留)。EROFS 文件格式 endian-neutral,任意 host arch 上
的 mkfs.erofs 都可生成镜像;镜像内的 `/sbin/init` 是 target arch 二进制。

`sandbox-init` 由本仓 `make sandbox-init` 构建;`sandbox-runtime.bundle` 由
`guest-runtime` 消费该二进制并通过 `make sandbox-runtime` 打包。mkfs.erofs 构建详见
`guest-runtime/native-deps/docs/build.md` §2.1。

## 3. sandbox-init 三阶段

### 3.1 阶段 1:早期挂载 + 并发取 launch spec + switch-root

launch spec 携带 `mounts`(含 `empty` 卷)等"驱动 rootfs 组装"的字段,因此
**hello/launch 握手与 overlay 组装并发进行**:握手是纯 socket 操作(无任何路径
解析),与挂载链、乃至随后的 `chroot`(进程级,Go 线程共享 `CLONE_FS`)安全并发——
关键约束是握手 goroutine 全程不碰文件系统,且 `chroot` 在 join 之后才执行。

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

**为何能并发**:`bind+listen` 必须在 `launch` 写出前完成(host 在 `launch` 写完即起
ping ticker,listener 没起会落空),故提到最前;handshake 只做 socket 系统调用,挂载链
只做 `mount()`(改的是挂载命名空间,不解析路径),二者无共享路径解析,可安全并发。
`chroot` 是真正的进程级路径切换,排在 JOIN 之后单线程执行,届时 handshake goroutine
已退出。这一并发把 hello→launch 的往返叠在 overlay 组装之下。

**volume 卷的 source 与搬运**:`empty` 卷的 source 是 raw ext4 上 `/overlay/upper/volumes/<i>`
(与 overlay 的 `upperdir/` 物理隔离,同在 vdb、一起进磁盘快照),bind 到 sysroot 内的
target;switch-root 的 `MS_MOVE /sysroot → /` 会把该 bind 随整棵子树搬进新 `/`
(proc/sys/dev、`/opt/sandbox-runtime` 正是同理),无需单独 MS_MOVE。switch-root 后
`/overlay/upper` 路径被埋,但 bind 持有 ext4 inode 引用使卷内容在 target 处存活,且应用
看不到 raw ext4 内部结构。

**`/opt/sandbox-runtime` 的搬运与版本钉住**:同一通道——B6 在 switch-root 前把 pmem 内的
发布件根 bind 进 `/sysroot/opt/sandbox-runtime`,由 E 的 `MS_MOVE /sysroot → /` 随子树带进
新 `/`,bind 持有 pmem inode 引用使其在原挂载被遮蔽后仍存活;phase2 fork 前的 `MS_REC|MS_SHARED`
令其作为对等挂载传播进应用私有 mount ns(应用以只读看到)。因 payload 驻留 pmem,改它即改
`sandbox-runtime.bundle` 的 digest——而 restore 本就要求该 digest 与快照一致(同一份 pmem 必被
重挂),故恢复出来的 sandbox 看到同一份 payload、无版本偏斜。代价是 payload 与 runtime 镜像同
生命周期、无法独立热补丁(需独立版本时改用独立只读 EROFS 设备,见 §6)。

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
`boot.disks[]` 须与快照同数同序;mounts[] 在 restore 下不必带)。**快照**逐盘捕获其可写 diff
(root + 各数据盘),`snapshot.cfg` 的 `boot.disks[]` 按序记录每盘 `base_ref`/`overlay.base`/链
(与 `boot.root` 同结构);本地链逐盘 flatten-merge(同 root)。

### 3.2 阶段 2:spec 应用 + stdio 接线 + 应用拉起

阶段 1 的 JOIN 已拿到 LaunchSpec 与那条 vsock 连接(hello/launch 已收发)。阶段 2
在**同一条连接**上把 spec 应用完、发 launch_ack;此后该连接**不关闭**——升级成 MUX,
承载应用的 stdin/stdout/stderr(或一个伪终端)直到沙箱结束(协议见 §4.5)。

`launch_ack` 在 spec **全部应用完(含 init)之后**才发出,故它对 host 是"环境与
初始化全部就绪、即将 fork"的 settled 信号。spec 应用各步触碰文件系统,均在
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
       重拉(非 reboot);仅 host 停机(置 shutdown 后再杀)走 reboot。

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
**唯独 restore 注入靠它**。网络 restore 重配无此问题:app 只 `CLONE_NEWNS`、不
`CLONE_NEWNET`,与 PID1 共享网络 ns,netlink 改动天然可见。

**applyNetwork 不是 listener 的前置依赖**——LaunchSpec 不带 network 时直接跳过,
guest 仍有 `lo`;applyNetwork 只对应用层外部网络服务有意义,vsock 控制面与网络
配置正交.

**fast-fail**:一次性沙箱模型下,网络 / 挂载 / 文件 / init 任一失败都让应用悄悄跑
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
  待 shutting_down=false 且 非 quiescing(快照窗口)
  rewireApp:
    等旧代全部 app→host pump:读到 EOF 且最后字节已写入 MUX
    超过 2s 仍未 EOF(例如后代继承 writer)→ 强制关闭旧代 fd,有界继续
    安装新代 fd,唤醒 pump 接到**不变的 MUX stream** → phase2ForkApp:
    先 app_pid.store(newpid),再放行 helper;helper 以 CLONE_INTO_CGROUP 直接出生在最终
    target、加入 pinned cgroup namespace并挂 scoped cgroup2 → app_started{newpid}
  // host 的 run 链路不断,持续收到新实例输出

app_exit_then_reboot(status):
  收尾 MUX:应用 fd 已关 → 各 stdout/stderr/pty 流发 EOF → 等 host 排空(有界,带超时)
  short-conn dial host:5000 → write app_exited{code, term_signal} → wait ack(timeout) → close
  reboot(LINUX_REBOOT_CMD_POWER_OFF)   # ack 拿不到也照常 reboot;POWER_OFF → CH 干净退 0
```

**退避**(app 与 plugin 共用,supervise.go):退出即重拉,延迟 10ms 起、每次 ×2、封顶 60s;
进程存活满 60s 再退出则重置回 10ms。**app_pid 原子化**:restart goroutine fork 后即写,reaper
按其路由,故新实例的退出不会被误判为 plugin/exec。**快照门**:quiesce 置 quiescing(plugin
随 app cgroup 冻结、supervisor 不再 fork 新进程),restore/attach 解除——restartApp 会等过这个
窗口再 fork,避免冻结遗漏新进程。`launch.restart=always` 下 app 退出**不 reboot**,沙箱长活。
generation drain 只确认 app→host 输出,不对 stdin 建 barrier;新旧输出在每条 MUX stream 上
保持顺序,代际切换不会发送 stream EOF。停机若追上刚安装的新代,pump 仍先消费该已存在代,
但不会再等待未来代。

`reboot(POWER_OFF)`(而非 `RESTART`):一次性沙箱模型下应用退出即沙箱结束,
CH 应随之干净退出。`RESTART` 会触发 CH 的"原地重启"流程,试图重连 vhost-user-blk
后端——而后端只接受一次连接(sandbox = 单 VM 生命周期),重连失败 CH 非零退出。

**反向 listener 在独立 goroutine 中持续运行**,与 supervisor signal loop 并行;
处理 host 下发的 `ping` / `restore` / `quiesce` / `attach` 短连接(§4.3、§4.4)。
listener 整个沙箱生命周期(冷启动 + snapshot/restore + MUX 重连 + 退出)持续存在,
唯一退出点是进程 reboot。

**mem_report 上报 goroutine**:与 supervisor 并行的第二个常驻 goroutine,默认
每 5 s 读一次 `/proc/meminfo` 的 `MemAvailable:` 和 `MemTotal:`,短连接发
`mem_report` 给 host(协议见 §4.4)。host 端 BalloonController 据此把 balloon
target 锚定在合理水位(详见 [`sandbox.md`](sandbox.md) §9.3);失败仅记 stderr。

### 3.4 quiesce 处理

quiesce 是 host `/vm.pause` 之前的最后一次清理机会,目标两件事:

1. 把跨实例 snapshot 的内存与磁盘状态推向"确定性",让分块去重率从 50-70% 升至
   >90%(`platform/docs/kuasar-sandbox.md` §4.6)。
2. **让 MUX 与端口转发连接在快照前彻底关闭**——快照绝不能捕获一条半开/握手中途的
   MUX 连接,或一条仍在飞的 `connect` 端口转发连接(restore 出来后无对端,成为
   悬挂状态;§4.6 / §3.7)。

`attach`/`quiesce` 等短连接由 listener 单线程顺序处理;host 看到的语义是
"`quiesced` 一回来即可继续 `/vm.pause`"。

**quiesce 流程**:

```
0. (先按 §3.6 拒绝新 exec 并 SIGKILL 在飞 exec 辅助进程)freeze 应用:
   write /sys/fs/cgroup/app/cgroup.freeze = 1,轮询 cgroup.events 至 frozen 1
   (有界等待)。在 sync 前冻结 ⇒ sync 之后应用不再产生新脏页,镜像更确定;
   freezer 原子覆盖整棵子树,含 /init、envd 创建的 user/ptys/socats 等子树和
   冻结期 fork 出的子进程;不调用 envd /freeze
1. [prep] sync(2)                                  // ~ms,把 ext4 upperdir 全部 dirty 落地
2. [prep] 若 quiesce.skip_drop_caches=false:
   open("/proc/sys/vm/drop_caches", O_WRONLY) → write("3\n")
                                                    // 同时丢 page cache + dentry/inode cache
3. 停止读应用的 stdout/stderr pipe(或 pty master) // 应用已冻结,残留有界
   (停读是 MUX 关闭的前置动作)
4. 拆除所有 `connect` 端口转发中继(§3.7):标记 quiescing 拒绝新 connect,逐条
   关闭 target 连接 + 反向通道 vsock 连接,后者带 SO_LINGER **阻塞至 socket 移除**
   ——与 MUX 同理,不留半开 vsock 残留。多会话**并发**关闭,有界于 quiesce 预算。
   accept 模式额外关闭缓存的 guest listener(唤醒 park 中的 Accept,清空缓存,resume
   后懒重建)。host 侧 Forwarder 在发送 quiesce 前暂停新建、关闭并等待所有在途 dial/accept
   握手与活跃中继退出，确保之后不再产生 host 侧 teardown
5. 在 MUX 连接上发起优雅关闭握手(§4.6):MUX_CLOSE → 收 MUX_CLOSE_ACK → close(MUX)。
   close 带 SO_LINGER,**阻塞至该 vsock socket 真正从内核移除**(host 响应方回 ACK
   后立即关闭其连接,RST 回到 guest → 这端 socket 移除),而非"发起关闭即返回"——
   保证 `quiesced` 时连接已彻底拆除,不留半关闭残留(§4.6 详述其必要性)。
   连接已断则降级硬丢
6. WriteMessage(quiesced) 于 quiesce 短连接
7. close(quiesce 短连接)
```

**为什么 prep 必须做这两步**:

- **sync 在前**:`drop_caches` 只丢 clean,先 sync 把 dirty 转 clean,disk dump
  与 memory dump 看到的是一致状态
- **drop_caches=3(默认)**:page cache 是确定性 snapshot 的核心污染源;同一应用不同启动
  序的 page cache 内容按访问顺序、prefetch 时序差异化堆积,跨实例 ~90% 不同;drop
  后每实例 restore 后 page cache 初值统一为空,直接对应 kuasar-sandbox.md §4.6 量化的"确定性
  50→90% dedup"差距来源。单次 snapshot 可用 `--drop-caches=false` 请求跳过此写入,
  但 freeze 与 sync 仍照常执行,用于保留 warm-up 形成的 guest cache 状态。

`quiesced.drop_caches_result` 回报 `skipped | succeeded | failed`;空值表示旧 guest
未实现回报(`unknown`)。显式请求 skip 而收到 unknown 时 host 继续快照并告警,因为旧
guest 可能已经按旧协议执行了 drop。该结果只描述动作,不承诺 cache 未被内存压力或
balloon 回收。

**扩展项(未实现;协议预留扩展位)**:

| 动作 | 说明 |
|---|---|
| 应用层 quiesce hook(信号通知 user app)| sandbox.yaml `quiesce.signal`(默认空=skip);收到 quiesce 时向 app_pid 发信号,等固定窗口(默认 100 ms)。默认不发——SIGUSR1 等信号没注册 handler 时默认终止应用,需用户显式声明才安全 |
| `/tmp` tmpfs 重置 | 阶段 1 加 `mount -t tmpfs tmpfs /tmp`;quiesce 时 `umount2(MNT_DETACH)` + remount |
| outbound 连接关闭 | 应用层 fd,sandbox-init 无主动关闭权,依赖 hook |

**不做项**:

- **不**关闭 vsock listener(后续 `restore`/`attach` 依赖它)
- **不**终止 user app、**不**向其发信号——quiesce 不是 sigterm;应用是被 cgroup
  v2 freezer **冻结**(step 0,对应用透明,非信号、非终止),不再依赖"停读输出"
  的自然反压
- **不**重置 RNG / 熵池——熵池重新播种属 restore 路径职责(未实现,见
  §4.3 `restore` 行)
- **不**清理 /var/log 等运行时日志——应用职责

**错误处理**:

- prep 的 sync / drop_caches 任一失败 → stderr 记录,继续后续步骤(best-effort,
  质量不到位反映在 dedup 率指标上,**不**阻塞快照)
- **freeze 确认、MUX_CLOSE 握手与 `quiesced` 不是 best-effort**:`quiesced` 写出
  意味着"应用已冻结、MUX 连接已确认彻底拆除(socket 移除,非仅发起关闭)、guest
  处于稳态"——发起动作不等于完成,quiesce ack 时刻必须已进入稳态。若 cgroup.events 在有界等待
  内未到 `frozen 1` → **不发 `quiesced`**(半冻结的快照恰是要消除的 resume-vs-
  env 竞态源)。若 MUX_CLOSE 握手因连接已断而走不通 → 按硬丢处理(对端也看到了
  断链),仍可发 `quiesced`;若 `quiesced` 未发或写不出去(host 侧不可达)→ host
  在 deadline 内拿不到响应 → host 视为协议失败、放弃此次 snapshot,sandbox 继续
  运行(详见 §4.10 与 [`sandbox.md`](sandbox.md) §6.2)

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

**反压**:当 host 不消费某条流(终端被 Ctrl-S、`--stdout-to` 的盘满、或 MUX 暂时
不存在),该流的接收窗口耗尽 → sandbox-init 那侧停止排空 → 内核 pipe / pty 缓冲
填满 → 应用在 `write` 上阻塞。不丢字节、不做环形 buffer(详见 §4.5)。

**内核 dmesg**:走 `/dev/hvc0`(virtio-console),与应用 stdio 是完全独立的一条道;
host 侧由 CH 把它写到 sandbox-ctl 给 CH 的 stdout(一根匿名管道),sandbox-ctl
按 `--console` 标志决定丢弃 / 写 stderr / 写文件(详见 [`sandbox.md`](sandbox.md)
§2.2 / §5.2)。`--serial off`——没有 8250 UART。

### 3.6 exec 会话(`sandbox-ctl exec`)

`sandbox-ctl exec`(host 侧 CLI + ctl.sock 见 [`sandbox.md`](sandbox.md) §2.4 /
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
见 §4.6)。exec 命令退出**不**触发沙箱 reboot——只有**用户应用**退出才 reboot
(§3.3);supervisor 的子进程回收器把"非应用子进程"的退出态路由给对应 exec 会话,
不误判为应用退出。

**与 snapshot 的关系**。内层 helper 在 setup 前设置父死信号;降权可能清除此信号时,
它在降权后重设,并通过私有握手 socket 的无阻塞 EOF 检查确认原 `exec-join` 仍存活,
之后才 final exec。因而外层辅助进程被杀会一并带走那条命令。会话
MUX 中途断(host 侧 `sandbox-ctl exec` 退出 / 失联)→ guest SIGKILL 该命令,命令
不会比其会话存活更久。snapshot quiesce(§3.4)开始时**拒绝新的 exec 并 SIGKILL
所有在飞的 exec 辅助进程**(快照不能带运行中的 exec 兄弟进程);沙箱在 resume /
restore(§4.3 `attach` / `restore`)后解除拒绝、重新受理。

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
    │                │ connect{addr:"127.0.0.1:49983"}│───────────►│ net.Dial(tcp, addr)
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
    │                │ connect{accept,addr:"/run/up.sock"}│──────►│ Accept(address) ◄────────┤ connect
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
(§4.7):关闭信号以 **EOF/RST 帧**走数据面一个字节,proxy 当不透明字节原样搬运,
100% 保真。单条转发=单条逻辑流,无需流 ID 与应用窗口——vsock 连接自身的内核缓冲背压
即流控(与 §4.5 MUX 的多流窗口不同)。

**与 snapshot 的关系**。转发中继与应用 MUX 同列入 quiesce 拆除(§3.4 step 4):
快照不能带在飞的转发连接,否则 restore 出来无对端、成半开 vsock 残留。quiesce 时
guest 标记 quiescing 拒绝新 connect,逐条关 target + vsock(后者 SO_LINGER 确认拆除);
**accept 模式额外**关闭所有缓存的 accept listener(唤醒 park 中的 `Accept`,其会话已在
connReg 中一并拆除)并清空缓存——listener 不随快照留存,resume 后**懒重建**。host 侧
Forwarder 在发送 quiesce 前暂停新建,并收拢、等待活跃中继**与 pending 的 dial/accept
反向连接**;`resume`/`restore`/
`attach`(§4.3)后解除拒绝、重新受理(并 reopen accept listener 缓存)。host 的转发
listener 本身**不**随 quiesce 关闭——跨快照存活,restore 进程以同样 `--connect` 重新接管。

## 4. vsock 控制面 + console MUX 协议

### 4.1 两类连接

```
  (1) management short-conn  — one new conn per management op; request/response; close
        │  ops:  hello/launch · app_started · app_exited · ping · mem_report ·
        │        quiesce · restore · attach · exec                    (§4.3 / §4.4)
        │  wire: [4B LE len][JSON]    ·    no multiplexing    ·    no keepalive

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
```

管理连接不做帧复用——每条连接就一次请求 + 一次响应。MUX 是带帧多路复用与流控
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

请求/响应严格 1:1,在同一条短连接上完成;"ACK 后" 一栏标明该连接是关闭还是升级为 MUX:

| 操作 | 拨号 | 序列 | ACK 后 | 用途 |
|---|---|---|---|---|
| **冷启动** | guest→host | `hello` → `launch{spec}` → `launch_ack{stdio}` → `ack` | **升级 MUX** | guest 报 ready,host 回 LaunchSpec;guest 准备好 app stdio 后回 launch_ack,该连接成为 MUX |
| **应用启动通知** | guest→host | `app_started{pid}` → `ack` | 关 | guest 已 fork/exec 用户进程 |
| **应用退出通知** | guest→host | `app_exited{code, term_signal}` → `ack` | 关 | 用户进程退出;guest 收 ack 后再 reboot;host 用作自身退出码 |
| **健康探测** | host→guest | `ping{id, t_send_ns}` → `pong{id, t_send_ns}` | 关 | host 计 RTT / 超时 / 失败数(§4.9) |
| **mem 报告** | guest→host | `mem_report{mem_avail, mem_total}` → `mem_report_ack` | 关 | guest 周期上报 `/proc/meminfo`,喂 host BalloonController |
| **快照前** | host→guest | `quiesce` → `quiesced` | 关 | guest 冻结应用进程树 + 跑 prep + 关闭 MUX(§3.4),`quiesced` ⇒ 应用已冻结、可安全 `/vm.pause` |
| **恢复后** | host→guest | `restore{epoch, wallclock_ns, network?, files?}` → `restore_ack{stdio, app_state}` | **升级 MUX** | 快照恢复 vCPU 起跑后 host 通知 guest;应用此时仍处 freezer 冻结态(冻结态随快照保存,`/vm.resume` 不解冻);`restore` 携带 host 发送前一刻的墙钟 `wallclock_ns`,guest 收到后先 `clock_settime` 把 `CLOCK_REALTIME` 跳到该值(CH 把快照里的旧钟原样载回,不纠正则落后整个静置区间;单调钟不受影响);若带 `network`,以 **flush-and-replace** 重配 L3(克隆取新 IP/MTU/nexthop/hostname;MAC 沿用快照设备状态不变);若带 `files`,把该实例专属文件(per-instance secret / resolv.conf)注入(同冷启动的内存盘 + bind 机制,仅落克隆内存、不入黄金快照)。两者均 best-effort + 记日志、thaw 前完成;回 `restore_ack`(ATTACH_ACK 的超集 + "恢复完成"信号,host 据此判定 restore 完成)、重连 MUX,**最后 thaw 应用**(write `cgroup.freeze=0`)——故应用绝不会观察到旧墙钟、错误网络、缺失的 per-instance 文件或未重连的 MUX。**RNG 重播种未实现**;该连接成为新 MUX |
| **MUX 重连** | host→guest | `attach{epoch}` → `attach_ack{stdio, app_state}` | **升级 MUX** | 纯 stdio-MUX 传输重连:MUX 因 vsock 异常断了,host 拨新连接重建;guest 优雅关旧 MUX(已断则硬丢)、回 ack,该连接成为新 MUX(§4.6)。**attach ≠ 快照后 resume**——活 VM 上从未 quiesce 的断线兜底也走它。thaw 不属 attach 语义,而属 quiesce 生命周期(freeze 的逆),**由 guest 自身冻结状态驱动**:仍冻结才补 thaw(仅 `resume_after=true` 同进程续跑路径——VM 原地 resume,attach 恰为首个 post-resume 接触),活 VM 重连本未冻结即跳过 |
| **执行命令** | host→guest | `exec{spec}` → `exec_ack{stdio}` | **升级 MUX(独立会话)** | guest 为这条 `exec` 起一个兄弟进程并准备其 stdio,回 `exec_ack`,该连接成为这次 exec 会话**独立**的 MUX;并发多条互不影响;命令结束 guest 在 MUX 上发 EXIT_STATUS 再走 §4.6 关闭。详见 §3.6 |
| **端口转发** | host→guest | `connect{spec}` → `connect_ack` | **升级转发数据通道** | guest 为这条 `connect` 取得 `ConnectSpec.address` 上的目标连接——dial(默认)或 `Accept`(`spec.accept`,accept 模式可无限期阻塞,host 无 deadline park)——回 `connect_ack`,该连接成为这条转发的 fwd 帧数据通道(§4.7),保留 TCP 半关闭;并发多条互不影响;quiesce 时主动拆除(§3.4)。详见 §3.7 |
| `error` | 任意 | (终止) | 关 | 任一端拒绝/出错的兜底响应,`msg` 人类可读 |

`ATTACH` 仅用于 sandbox-ctl 自身的可靠性兜底(同一进程在 MUX 连接坏掉后重建转发),
**不**用于另一个进程接管会话。

### 4.4 管理消息 wire format 与字段

`[4 字节 little-endian uint32 长度] [JSON payload]`,`MaxMessageBytes` = 64 KiB。
JSON 可读、调试友好;消息量极少,无需 protobuf 工具链。

```json
{
  "type":     "<one of §4.3>",
  "phase":    "ready",                 // hello: optional hint
  "launch":   { ... LaunchSpec ... },  // launch (含 stdio 节,见 §5.1)
  "exec":     { "argv":[...], "env":{}, "cwd":"", "stdio":{} },  // exec: ExecSpec(§3.6)
  "connect":  { "network":"tcp", "address":"127.0.0.1:49983", "accept":false },  // connect: ConnectSpec(§3.7; accept=true ⇒ guest Listen+Accept)
  "stdio":    { ... },                 // launch_ack / restore_ack / attach_ack / exec_ack: 实际启用的 channel 集合
  "app_state":"running",               // restore_ack / attach_ack: running | exited{code,term_signal}
  "pid":      4711,                    // app_started
  "code":     0, "term_signal": 0,     // app_exited
  "id":       42,                      // ping/pong: 单调递增,host 分配
  "t_send_ns":1715000000000000000,     // ping: host 单调时钟 ns;guest 原样回填到 pong
  "epoch":    3,                       // restore / attach: 第 N 次;每次 +1,用于去重 in-flight
  "wallclock_ns":1715000000000000000,  // restore: host 墙钟,guest 落 CLOCK_REALTIME(attach 不带)
  "network": { "ip": "169.254.4.1/31", "mtu": 1450, "nexthop": "", "hostname": "c1", "interface": "eth0" }, // restore 可选:重配 L3(flush-and-replace);省略则保留快照网络
  "mem_avail_bytes": 4294967296,       // mem_report
  "mem_total_bytes": 8589934592,       // mem_report
  "msg":      "<reason>"               // error
}
```

字段集合的权威定义即本节 + §4.3 的消息表;wire 是 stdlib-only 的
长度前缀 JSON,guest sandbox-init 不依赖任何重量级编解码库即可解析。

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
 type (数据流 1..4):  DATA   EOF   RESET
 type (CONTROL 0):    WINDOW_UPDATE   SET_WINSIZE   MUX_CLOSE   MUX_CLOSE_ACK   EXIT_STATUS
 len:  payload 长度;DATA 帧 ≤ 16–32 KiB(多路之间公平,也是天然读块大小)
       EXIT_STATUS payload = u32 BE 退出码(被信号杀为 128+signo);仅 exec 用
```

**stream 集合 = pty 模式 XOR pipe 模式**(在 `launch_ack` / `restore_ack` /
`attach_ack` 的 `stdio` 字段里声明):

- **pty 模式**:CONTROL(0)+ PTY(4)。终端没有独立 stderr——应用的 stdout+stderr
  都到这个伪终端,合并在 PTY 流上
- **pipe 模式**:CONTROL(0)+ 按声明的 STDIN(1)/STDOUT(2)/STDERR(3)。未声明的
  stream 上出现任何帧 = 协议违规(见下文状态机)

**方向约定**(违反即协议违规):

| stream | host→guest | guest→host |
|---|---|---|
| STDIN(1) | `DATA`(应用输入)、`EOF`(host stdin 关) | `RESET`(拒收) |
| STDOUT(2) / STDERR(3) | `RESET`(拒收) | `DATA`、`EOF`(应用退出/关闭其 fd) |
| PTY(4) | `DATA`(键盘字节,逐字节透传) | `DATA`、`EOF`(应用退出) |
| CONTROL(0) | `WINDOW_UPDATE`(对 guest→host 流补信用)、`SET_WINSIZE`、`MUX_CLOSE_ACK` | `WINDOW_UPDATE`(对 host→guest 流补信用)、`MUX_CLOSE`、`EXIT_STATUS`(仅 exec 会话:命令退出码) |

**流控**:每条数据流一个**接收窗口**(= 该方向接收 buffer 容量,例如 64 KiB)。
发送方维护每流剩余信用,DATA 每发 N 字节扣 N;信用为 0 的流跳过、去发别的流。
接收方排空一部分就发 `WINDOW_UPDATE{stream, delta}` 补回。**无连接级窗口**(流就
那么几条、量也小);DATA 帧大小上限保证 muxer 在多条流之间轮转公平。这样一条流的
sink 卡住只卡它自己,CONTROL 帧(`SET_WINSIZE` 等)永远能流。

**detached = 应用阻塞**:MUX 连接不存在的那段时间(quiesce 窗口、或连接刚断尚未
attach),没有对端给信用 → sandbox-init 那侧停止排空 → 内核 pipe / pty 缓冲填满 →
应用在 `write` 阻塞。**不丢字节、不做环形 buffer、不做"丢了 N 字节"标记** —— detached
只发生在受控短窗口,阻塞是可接受的、也是最简单自洽的。残留字节(per-stream buffer
里没冲完的 + 内核 pipe 里的)有上限(buffer 容量 + 一个 pipe 大小),随内存快照
被捕获,restore 后 attach 时排空、应用解除阻塞。

**per-stream 状态机**:

- 未在协商集合里的 stream 上收到任何帧 = **协议违规** → 拆掉整条 MUX 连接(这是
  bug,要响)
- 某方向发过 `EOF` 后,在该方向再发 `DATA` = 协议违规 → 拆连接
- 收到 `RESET` → 该流标 CLOSED,本地 fd 关 / 给应用 SIGPIPE
- 对端不要你 offer 的某条流(资源级,非协议违规)→ 不开它 / 回 `RESET` 该流,连接继续
- `EOF` 是单向半关:STDIN 上 host→guest 的 `EOF` 关应用 stdin 的写端;STDOUT/STDERR
  / PTY 上 guest→host 的 `EOF` 表示应用关了对应 fd / 退出

**SET_WINSIZE**:host 侧 SIGWINCH 时 host 在 CONTROL 流上发 `SET_WINSIZE{cols,rows}`
→ guest 对 pty master 做 `TIOCSWINSZ` → 内核给应用进程组发 SIGWINCH。仅 pty 模式有意义。
进入 MUX 态(含 restore/attach 后)host 先发一次初始 winsize。

### 4.6 MUX 优雅关闭握手

MUX 连接的**有序关闭**是一个两端同步的小协议(相当于应用层的 FIN / FIN-ACK),
之后接标准 socket orderly close。**永远 guest 发起、host 响应;发起到收响应之间
到达的帧照常处理。两端都关闭各自的 socket——host 回 ACK 后立即 close 其连接(RST
回 guest),guest 的 close 带 SO_LINGER 阻塞至本端 socket 真正从内核移除。**

```
  guest (sandbox-init)                                     host (sandbox-ctl)
    │
    │ ── MUX_CLOSE (CONTROL frame) ────────────────────►    (guest sends no more data frames after this)
    │ ◄── may still receive WINDOW_UPDATE / leftover STDIN DATA   (guest processes these normally)
    │ ◄── MUX_CLOSE_ACK (CONTROL frame) ──────────────      host: flush pending → ACK → close(MUX)
    │     on ACK → close(MUX) [SO_LINGER]                    host close → RST ─┐
    │ ◄── RST ────────────────────────────────────────────────────────────────┘
    ▼     RST removes the vsock socket → guest close returns (teardown confirmed)
    back to:  listener up  ·  app session alive (app blocked on write — or frozen, if quiesce §3.4)  ·  no MUX
```

host 对 MUX 关闭的全部职责:收到 `MUX_CLOSE` → 把要发的发完 → 回 `MUX_CLOSE_ACK`
→ **立即 close 其连接**。不需要知道为什么关、什么时候关。

guest 发起端收到 `MUX_CLOSE_ACK` 后将该帧作为 MUX read loop 的终态,先停止读取,
再由调用流程 close 连接。禁止 ACK 后再发起一次 read:raw vsock fd 的 close 与新 accept
可能复用相同 fd 号,旧会话若残留一次读取就会抢走新连接的 4-byte proto 长度头或首个
4-byte MUX frame header。

**为什么 host 必须主动 close、guest 必须 SO_LINGER**:virtio-vsock 对 guest 单方
关闭的连接不立即回收——内核挂起延迟移除(默认 8s),等对端 RST 或超时。故 host 回
ACK 后立即 close(RST 令 guest 端连接进入移除),guest 的 close 用 SO_LINGER 阻塞至
移除完成。这样 `quiesced`(§3.4)可保证已纳入 quiesce 的 MUX、forward 和在途握手
全部拆除,没有关连接产生的 transport packet 再与 `/vm.pause` 竞争。

这个 barrier **不能单独保证 guest 内核里没有任何旧连接状态**:刚在 gate 前正常结束
的短连接已经离开 registry,最终承载 `quiesced` 的控制连接也只能在 ACK 写出后关闭。
Cloud Hypervisor restore 又会重建空的 Unix vsock backend,不会序列化 connection map。
平台 CH 补丁用两条独立不变量收口:保存 `local_port_last`,避免新连接复用旧四元组;
snapshot 时向 guest used event ring 预发布 `VIRTIO_VSOCK_EVENT_TRANSPORT_RESET`,
restore activation 只重发 IRQ,让 Linux 清理全部 connected sockets,同时在 guest
event-queue kick 确认前 gate backend RX。listener 不受 reset 影响。首个 restore
REQUEST 只能在清理确认后进入 guest,无需 retry、sleep 或延长 timeout。

**三个触发点**(同一握手):

1. **quiesce 流程**(§3.4 第 4 步)——guest 在 quiesce 流程靠后一步关闭 MUX。
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
   收到 `attach` → 尝试优雅关旧 MUX、失败即硬丢 → 回 `attach_ack` 于新连接 → per-stream
   window 重新协商、winsize 重发、恢复读 app pipe、残留回放 → 续传。

**MUX 因 vsock 异常突然断**(非 quiesce、非 attach 主动关):两端各自看到错误,无优雅
握手;guest 回到"等 host 来 attach 重建"的状态(listener 一直在),host 检测到后拨
新连接发 `attach`。

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
   DATA(0)  载荷字节(单帧 ≤ 32 KiB,更大切多帧)
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
guest 对 vsock 连接 arm SO_LINGER 再关,阻塞至 host RST 确认拆除,不留半开残留。

### 4.8 时序

每段图里 `─►` 是普通管理短连接的请求/响应,`══►` 是 MUX 帧流动;一条连接做完
管理握手后转为 MUX 用 "(this conn ⇒ MUX)" 标注。

**冷启动**:

```
  sandbox-ctl                                              sandbox-init (guest)
  ───────────                                              ────────────────────
  listen <base>_5000
  spawn CH ─────────────────────────────────────────────►  kernel boot → mount + chroot
                                                           AF_VSOCK bind+listen :5000
                       ◄── hello{ready} ──────────────────  dial CID=2:5000      [conn A]
  ── launch{spec, stdio} ───────────────────────────────►  applyNetwork; openpty / pipes
                       ◄── launch_ack{stdio} ─────────────  prepare app stdio fds
  ── ack ───────────────────────────────────────────────►  (conn A ⇒ MUX)
  send initial SET_WINSIZE  ════════════════════════════►  (tty mode);  fork/exec user app
  ping ticker (1 Hz) start                                 app fd 0/1/2 = pty slave / pipes; MUX bridge up
                       ◄── app_started{pid} ── [conn B] ──  ;  reply ack; close conn B
  ── ping ──► ◄── pong ──  [conn C..k, repeats]            ║  MUX: STDIN/STDOUT/STDERR (or PTY) + WINDOW_UPDATE flow
                       ◄── app_exited{code,sig} [conn L] ─  user app exits → MUX flushed (EOF)
  ── ack ───────────────────────────────────────────────►  reboot(POWER_OFF) → CH exits 0
```

**MUX 重连(ATTACH)**:

```
  sandbox-ctl                                              sandbox-init
  ───────────                                              ────────────
  MUX read/write error                                     MUX read/write error  (old conn dead on both ends)
  dial CID=2:5000 ── attach{epoch} ─────────────────────►  try graceful MUX_CLOSE on old conn (err → hard-drop)
                       ◄── attach_ack{stdio, app_state} ──  reply on this new conn
  send SET_WINSIZE  ════════════════════════════════════►  (this conn ⇒ MUX);  resume reading app pipes; replay residual
                                                           thaw iff still quiesce-frozen: cgroup.freeze=0  (else no-op — attach≠resume)
  resume normal MUX flow                                   resume normal MUX flow
```

**quiesce → snapshot**:

```
  sandbox-ctl                                              sandbox-init
  ───────────                                              ────────────
  ping ticker stop
  dial CID=2:5000 ── quiesce ───────────────────────────►  freeze app: cgroup.freeze=1, await frozen
                                                           prep: sync ; echo 3 > drop_caches
                                                           stop reading app stdout/stderr (pty master)
                       ◄══ MUX: MUX_CLOSE ════════════════  on the (separate) MUX conn: send MUX_CLOSE
  ══ MUX: MUX_CLOSE_ACK ════════════════════════════════►  recv ACK → close(MUX);  host: read → EOF → close(MUX)
                       ◄── quiesced ─────────────────────  reply on the quiesce conn; close it
  ✓ MUX closed + quiesced received  →  /vm.pause  /vm.snapshot
  snapshot state:  listener up · app session alive (app frozen) · no MUX
                   · CH vsock local_port_last persisted
                   · TRANSPORT_RESET in used ring · reset pending persisted
```

**restore**:

```
  sandbox-ctl                                              sandbox-init
  ───────────                                              ────────────
  CH restore activation: re-signal snapshotted TRANSPORT_RESET (consume no descriptor)
  /vm.resume OK   (RX gated)                          ───►  reset connected sockets; listener stays up
  dial CID=2:5000 (REQUEST buffered behind RX gate)
                       ◄── event queue kick (ack) ─────────  reset complete; CH ungates pending REQUEST
                   ── restore{epoch,wallclock} ──────────►  clock_settime(CLOCK_REALTIME, wallclock_ns)
                       ◄── restore_ack{stdio, app_state} ─  reply on this conn
  send SET_WINSIZE  ════════════════════════════════════►  (this conn ⇒ MUX);  resume reading app pipes; replay residual
                                                           thaw app: cgroup.freeze=0  ← last, env ready
  ping ticker (re)start                                    (listener unchanged across the snapshot)
```

**listener 跨快照不关闭**——若 quiesce 把 listener 关掉,host 之后下发的 `restore`
/ `attach` 就无人 accept,guest agent 不可达。transport reset 只遍历 connected sockets,
不会关闭 bind/listen socket。CH 同时从快照保存的 `local_port_last + 1` 继续分配 host
local port;前者重置 transport epoch,后者维持分配连续性,两者职责不同。

### 4.9 ping 健康探测

**目的**:用 host→guest 探针检测 guest agent 存活与响应延迟。**不**作为应用层
心跳,不主动 kill VM,只产指标。

| 事件 | ping ticker 状态 |
|---|---|
| sandbox-ctl 完成 `launch` 写入 | start |
| sandbox-ctl 收到 `restore_ack` 响应 | start |
| sandbox-ctl 完成 `quiesce` 写入 | stop |
| CH 进程退出 | stop |

**参数**:

| 参数 | 值 | 含义 |
|---|---|---|
| `interval` | 1 s(固定) | 两次 ping 起始时刻间隔 |
| `timeout` | sandbox.yaml `timeouts.ping`;默认不强制(生产档 200 ms) | 单次 dial+write+read 总预算;到点视为失败。启用 `--ping-fatal-threshold` 时须设有界值(sandbox.md §2.2 / §3.1) |

**指标**(sandbox-ctl 暴露,统计窗口 = 沙箱生命周期):`ping_attempts_total` /
`ping_success_total` / `ping_timeout_total` / `ping_dial_error_total` /
`ping_rtt_ms_{p50,p95,p99,max}`(pong 到达时 `now - t_send_ns` 计算)。

### 4.10 失败语义

**guest 端**(sandbox-init):

- listener accept 错误 → 记录 stderr,不退出 init(避免单次连接异常杀整个沙箱)
- 收到未知 type / 字段不合规 → 回 `error{msg}` → close;ping 缺 id 直接拒绝
- MUX 上协议违规(协商外 stream、EOF 后又 DATA、坏帧)→ 拆 MUX 连接;此后等 host
  来 `attach` 重建
- `app_exited` 必须在 reboot 前发出;host 未在 timeout 内 ack → guest 仍照常 reboot
- `exec`:沙箱正在 quiesce 或 argv 为空 / 起进程失败 → 回 `error{msg}` 关连接;
  会话 MUX 中途断(host 侧 `sandbox-ctl exec` 退出/失联)→ guest SIGKILL 该 exec
  命令(命令经父死信号绑定到其 nsenter 辅助进程,辅助进程被杀即一并带走,§3.6)

**host 端**(sandbox-ctl):

- 任意 host→guest 短连接失败计入对应 `*_error_total`,不立刻 kill VM;由更上层
  health checker(本文档不覆盖)按指标决策
- `restore` 在请求发送前若 CH hybrid-vsock 已接收 `CONNECT`、却在返回
  `OK <port>` 前以 EOF/reset 断开,host 在总 deadline 内以 25 ms 起步退避、最多
  2 s 重拨;此阶段尚未写 `restore`,不会重放请求。首次写 `restore` 后的任何失败均
  fail closed,不重试。最终未收到 `restore_ack` → sandbox-ctl 调 CH
  `/vm.shutdown` 终结此次恢复并向调用方返回错误
- MUX 连接读/写出错 → host 拨新连接发 `attach` 重建;`attach` 也失败 → 计入指标,
  应用 stdio 转发中断(应用因反压阻塞),由上层决策
- `quiesce` 在 deadline 内未收到 `quiesced` → host 视为协议失败,**放弃此次 snapshot**
  (绝不带半状态/半开 MUX 快照),sandbox 继续运行

host 侧凡由 sandbox.yaml `timeouts.*` 接管的项以配置为准,默认不强制(host 等待
任意时长,dial 仍有界;详见 [`sandbox.md`](sandbox.md) §3.1);其余为 `pkg/proto`
协议常量:

| 消息 | 单次 deadline | 备注 |
|---|---|---|
| `hello` | guest dial 重试预算 5 s;host 读死线 `timeouts.app_notify`(默认不强制) | 冷启动早期 host listener 可能短暂未起,guest 指数退避重试 |
| `launch_ack`(host 等待) | `launch.start_timeout`,空 / 0 = **无限期** | launch_ack 在 guest 把 spec 全部应用完(含可能很长的 `init`)后才发,故 host 读它的 deadline 由 start_timeout 控制;默认无限期(init 可任意长),生产建议显式设值,否则卡死的 guest 无 host 侧超时。此连接随后转 MUX |
| `app_started` | guest 侧 200 ms(dial+write+读 ack);host 读死线 `timeouts.app_notify`(默认不强制) | 健康路径 µs 级;guest 侧短预算作"host 已死"的快速失败 |
| `app_exited` | 同 `app_started` | ack 拿不到也照常 reboot |
| `ping` | `timeouts.ping`(默认不强制;生产档 200 ms) | 到点计入 `ping_timeout_total`;启用 `--ping-fatal-threshold` 须设有界值 |
| `quiesce` | 8 s | guest 要 drop caches + 停读 app pipe + MUX_CLOSE 一来回;留足头部 |
| `restore` | `timeouts.restore`(默认不强制) | kernel vsock 层在此期间 hold 住连接请求等 vCPU 跑起来 accept;请求前 EOF/reset 在同一总 deadline 内短时重拨(最多 2 s),`restore` 一经写出绝不重放;恢复期大量缺页换入会拉长;此连接随后转 MUX |
| `attach` | 5 s | 同 `restore` 的 hold 语义;此连接随后转 MUX |
| `exec` | 10 s | 比 attach 宽:guest 要 fork+exec 子进程并 PATH 解析后才回 `exec_ack`;仅覆盖握手段,连接转 MUX 后 deadline 清除 |
| `connect` | 10 s | 仅覆盖握手段(内含 guest 侧 dial 目标 ≤ 5 s);连接转 fwd 通道后 deadline 清除 |
| `mem_report` | 同 `app_started` | guest 每 5 s 一次,host 失败仅记日志、controller 在下一 tick 用旧 hint |

## 5. 应用契约

### 5.1 launch 配置(LaunchSpec)

来源:`boot.root.base` 末尾 ZIP 内嵌 `config.json`(OCI image runtime config)⊕
sandbox.yaml `launch:` 节(yaml override 优先,Env merge),host sandbox-ctl 合并后
通过 launch 协议下发。`stdio` 节由 host 侧 `sandbox-ctl run` 的 `--tty` / `--stdin`
/ `--stdout` / `--stderr`(及它们的 `-from`/`-to`)解析决定(详见
[`sandbox.md`](sandbox.md) §2.2)。

```json
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
  "stop_grace_sec": 10,                  // 停机宽限秒数;0 → 默认 10s
  "network": { "interface": "eth0", "ip": "169.254.1.1/31", "mtu": 1500, "nexthop": "", "hostname": "my-sandbox" },
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
前以 raw ext4 source 建立(§3.1)。`start_timeout` 不在 LaunchSpec——它只约束 host 侧
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
  / + isolated 时自挂的 /proc)
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
- **vsock**:无,guest 应用不应直接用 vsock(平台保留 CID=3 + port 5000)
- **balloon / mem hotplug**:透明,应用不可见

### 5.3 退出语义

- `restart: never`:应用退出 → sandbox-init 收尾 MUX、发 `app_exited{code,term_signal}`
  → reboot → CH 退 → sandbox-ctl 退,sandbox 销毁。退出码(或致死信号)透传到
  sandbox-ctl 的进程退出码
- `restart: always`:应用退出 → **原地重启**(同进程内 refork + rewireApp,§3.3),
  沙箱长活、stdio MUX 跨重启存活,host 链路不断
- `restart: on-failure`:非零退出 / 被信号杀 → 原地重启;干净退出 → 同 `never`
  (通知 + reboot)

原地重启是同进程内 fork,**不**重新走整个 sandbox 启动序(退避 10ms→60s,存活满
60s 重置;停机信号或 quiesce 窗口期间不重拉,§3.3)。

### 5.4 信号处理

- sandbox-ctl 通过 vsock 发 `quiesce`(snapshot 前)/ `restore` / `attach` / `ping`,
  **不**直接给 user app 发信号
- 来自 host 的 SIGTERM 通过 cloud-hypervisor 传到 sandbox-init,sandbox-init 转发
  `launch.stop_signal`(默认 SIGTERM,可被镜像 StopSignal / yaml 覆盖)给 user app,
  等 `launch.stop_grace_period`(默认 10s)后超时 SIGKILL
- tty 模式下,host 终端在 raw 态时键盘 `^C`(0x03)作为字节经 MUX PTY 流送到 guest
  伪终端,由 guest 的行规程转成 SIGINT 发给应用——这是想要的;杀沙箱另走 SIGTERM
  或转义序列(详见 [`sandbox.md`](sandbox.md) §2.2)
- `quiesce.signal`(§3.4 扩展项,未实现)发给 user app 做应用层清理

## 6. 扩展点

| 扩展 | 引入条件 | 影响章节 |
|---|---|---|
| 应用 quiesce hook | 跨实例去重率超过 kuasar-sandbox.md §4.6 量化的"非确定性 50-70%" 上限的用例 | §3.4 quiesce 扩展项表 |
| 应用 stderr 旁路 | 需要 host 侧 stdout 与 stderr 分流(终端模式天然无此区分,pipe 模式可加一条 vsock 旁路) | §3.5 / §4.5 |
| 自带 vmlinux | 用户需要平台 kernel 未带的特性(nested userfaultfd / user·net 命名空间 / 别的 kernel 特性);平台 kernel 已含 cgroup cpu/memory/io/pids 控制器 + NFS(v3/v4) + FUSE | sandbox-ctl `boot.kernel: file://...` |
| 自带 sandbox-runtime | 用户应用对 PID 1 / supervisor 有特殊要求(罕见) | 平台不阻止,但失去 DAX 共享收益 |
| 独立发布件设备 | Guest 侧 payload(envd 等)需独立于 runtime 镜像迭代 / 热补丁 | §2 / §3.1:bind 源改为独立只读 EROFS 设备(多一 virtio 盘 + vhost 后端) |

## 7. See Also

- [`sandbox.md`](sandbox.md) §2.2(`run` 的 `--tty` / `--console` / stdio 标志)、
  §5.2(CH 冷启动命令行)、§6.2 / §6.3(snapshot 时序 / ctl.sock 协议)、§7(恢复)
- `guest-runtime/docs/vmlinux.md` —— guest kernel 启用的 namespace /
  文件系统 / virtio-console / 网络功能为何如此
- `sandboxer/docs/cloud-hypervisor.md` §5.2 —— vsock hybrid 代理:host
  侧映射到 UDS 的 CONNECT 行格式;`--console` / `--serial` 的用法
- `guest-runtime/native-deps/docs/build.md` §2.1 —— mkfs.erofs 构建(`guest-runtime make sandbox-runtime` 的前置工具)
- `platform/docs/kuasar-sandbox.md` §4.6 —— quiesce prep 必做项的目标依据
  (确定性 guest 配置)
