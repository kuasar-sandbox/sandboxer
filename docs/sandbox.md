# sandbox — 沙箱控制工具

`sandbox-ctl` 是平台沙箱的 host 端控制平面,管理一个 microVM 的完整生命周期
(冷启动、快照、恢复)。每个沙箱由一组 `sandbox-ctl + cloud-hypervisor` 双
进程承载,sandbox-ctl 是 CH 的父进程,负责 vhost-user-blk backend、uffd
handler、cgroup/balloon 联动(含 host 端 BalloonController)、与 node-ctl
的资源协议对话。

进程模型类 `runc run`:一个 `sandbox-ctl run` 命令就是一个 sandbox 的完整
生命周期。退出 = sandbox 销毁。

## 1. 概述

### 1.1 系统中的位置

```
   HOST
   ────

   node-shared  (one set per host)
       /opt/sandbox/vmlinux                  kernel image — each sandbox loads its own
       /opt/sandbox/sandbox-runtime.bundle    guest PID 1 image — DAX-shared host page cache
       /opt/sandbox/overlay-templates/*.ext4 pre-formatted COW upper templates (multiple sizes)
       cache-ctl  (tiered)                   chunk cache shared across sandboxes
       store-ctl                             content-addressed store backend (OBS proxy)
       node-ctl   (daemon)                   sandbox resource controller (dynamic mode)

   per-sandbox  (one process group per sandbox)

       ┌─ sandbox-ctl ───────────────────────────┐
       │   control plane (lifecycle)             │
       │   vhost-user-blk backend × 2            │
       │   uffd handler  +  va_report UDS server │
       │   resource executor   → node-ctl        │
       │   BalloonController   → CH vm.resize    │
       │   link: manifest.Fetcher  → cache-ctl   │
       │   link: manifest.Ingester → store-ctl   │   (snapshot upload only)
       └──────┬────────────┬────────────┬────────┘
              │ vhost      │ vhost      │ CH API UDS
              │ blk0       │ blk1       │
              ▼            ▼            ▼
       ┌─ cloud-hypervisor (patched) ────────────┐
       │   KVM + virtio devices                  │
       │   pmem  → sandbox-runtime.bundle         │
       │   blk0 / blk1 → sandbox-ctl backend     │
       └─────────────────────────────────────────┘

       /run/sandbox/<sid>/   ch.sock  blk0.sock  blk1.sock  uffd.sock  ctl.sock  vsock.sock (+ _5000)
                             snap-stage/ (snapshot) | snap-state/ (restore)   tmpfs run dir
       /var/lib/sandbox/<sid>/<sid>.overlay.diff    overlay writable upper layer (ext4 sparse, on disk)

   authorized remote exec

       client ── exec tunnel ──► node proxy ── pkg/ctl.ProxyExec ──► ctl.sock
                                      authenticate + select exact sandbox
```

Two host-side roots, kept distinct (overridable via --run-root / SANDBOX_RUN_ROOT
and --base-root / SANDBOX_BASE_ROOT):

- **run dir** `/run/sandbox/<sid>/` (tmpfs): sockets + the small CH-metadata
  staging (`snap-stage` for snapshot, `snap-state` for restore — config.json /
  state.json only; the multi-GiB memory/overlay never land here).
- **base dir** `/var/lib/sandbox/<sid>/` (disk): the overlay writable layer.
  The writable layer must be on disk, never the tmpfs run dir. An auto-created
  diff lives and dies with the sandbox; an explicitly-configured diff is left
  untouched. Snapshot **output** goes to `--output <dir>` (disk), separate again.

sandbox-ctl 是 CH 的父进程。CH 退出 → sandbox-ctl 收 SIGCHLD → 优雅 cleanup →
自身退出。退出码:优先用 guest 经 vsock 上报的 `app_exited{code, term_signal}`
(应用退出码 / 128+signal);拿不到时回退 CH 退出码映射。

### 1.2 设计原则

1. **统一内存所有权**:sandbox-ctl 拥有 memfd inode,CH 通过继承 fd 映射同一
   inode。冷启动 + 恢复用同一份内存代码路径(详见 §5);snapshot
   时 sandbox-ctl 直接 SEEK_DATA/HOLE 扫驻留页,不经 CH→file→sandbox-ctl 的
   中转
2. **单 uffd 模型**:CH 进程内创建 uffd(必须绑到 CH mm),通过 SCM_RIGHTS 把
   fd 传给 sandbox-ctl 的 handler;sandbox-ctl 自己 mmap 的 backendVA 不注册
   uffd,kernel 走默认 shmem 缺页路径。这从根本上避免跨 mm folio-creation
   race
3. **patch 影响面最小**:CH 改动约 630 行(6 个 commit,基于 v51.1);内存补丁仅在
   `--memory-zone fd=` 时激活,vsock 补丁仅改变 snapshot/restore 的 transport 状态
4. **资源可寻址**:任何运行所需文件(vmlinux 除外)都能选 `file://` 或
   `manifest://`,运行时无差别看待;snapshot 同样可上传至 manifest 存储
5. **三态资源控制**:是否启用 cgroup 限制、是否启用动态控制由 sandbox.yaml
   字段是否存在决定,无独立 mode 开关(详见 §4)
6. **per-sandbox 一进程**:类 `runc run` 语义,不是 daemon。故障域、资源
   记账、生命周期对齐自然清晰

### 1.3 故障域

| 故障 | 直接影响 | 自愈 / 处理 |
|------|---------|------------|
| CH 崩溃 | sandbox-ctl 收 SIGCHLD | 整 sandbox 销毁 |
| sandbox-ctl 崩溃 | CH 失去父进程 + uffd handler 没了 | vCPU 卡 fault → 上层 supervisor SIGKILL CH |
| stdio MUX 连接断(vsock 异常) | 应用 stdio 转发中断(应用因反压在 write 上阻塞) | sandbox-ctl 拨新连接 `attach` 重建并续传;`attach` 也失败 → 计入指标,由上层决策 |
| node-ctl 不可达(动态控制模式) | 长连断 | 退避重连 5×;持续 60s 失败切到无控制器降级模式 |
| 单沙箱 OOM | guest 内进程被 kill;deflate_on_oom 释放 balloon | 非平台级故障 |
| 资源开销 | sandbox-ctl Go runtime ~10-15 MiB;CH 自身 ~13 MiB | blk1.diff 是 sparse 文件,实际 = 已写 sectors |

## 2. 命令行接口

### 2.1 子命令总览

| 子命令 | 用途 |
|--------|------|
| `run` | 启动一个 sandbox 跑到退出。冷启动 = 不带 `--restore`;恢复 = 带 `--restore=<...>`,与冷启动共用同一进程模型与 stdio 接线 |
| `snapshot` | 暂停一个运行中的 sandbox 并 dump 到 snapshot |
| `exec` | 在运行中的 sandbox 内执行一条命令——应用的兄弟进程(不替换应用),加入应用的 mount + pid 命名空间,与 `run` 共用 stdio 模型 |
| `config` | 产出 / 合并 / 校验 sandbox.yaml(`--config a.yaml[:b...]` 或 `--template`;`--mode default\|restore`、`--check skip\|strict`、`-o`) |
| `info` | 打印 snapshot 内嵌的 `snapshot.cfg`(`manifest://<key>` 或本地 snapshot 路径;`--json`;`--manifest-config`) |
| `upload-snapshot` | 把本地快照图发布为 canonical portable ref:manifest 或 named ref location(不启动沙箱) |

### 2.2 `sandbox-ctl run`

```
sandbox-ctl run [flags]

  # 配置 + 身份
  --config <path[:path2...]>  sandbox.yaml 路径,':' 分隔多份时 front-to-back 深合并
                          (语义同 config 子命令,§2.5;SANDBOX_CONFIG env;flag 优先)
  --manifest-config <path>  存储配置 YAML(MANIFEST_CONFIG env);manifest 资源以及
                          crypto.local=auto|required 的本地工件需要
  --ref-location <name>=<file-URI>
                          可重复;把 located file ref 的逻辑名称映射到可信宿主目录
  --sandbox-id <sid>      覆盖 yaml 里的 sandbox.id

  # 执行环境
  --ch-binary <path>      cloud-hypervisor 二进制路径。不显式指定时按以下顺序查找:
                          1. SANDBOX_CH_PATH env(若非空)
                          2. <sandbox-ctl 自身可执行文件目录>/cloud-hypervisor
                          3. exec.LookPath("cloud-hypervisor")(走 PATH)
  --run-root <dir>        tmpfs run 根(SANDBOX_RUN_ROOT env;默认 /run/sandbox)。
                          sandbox-ctl 在 <run-root>/<sid>/ 下创建 ch.sock /
                          blk{0,1}.sock / vsock.sock / uffd.sock / ctl.sock 及
                          snap-stage/snap-state(CH 元数据中转)
  --base-root <dir>       磁盘 base 根(SANDBOX_BASE_ROOT env;默认 /var/lib/sandbox)。
                          overlay 写层默认落在 <base-root>/<sid>/<sid>.overlay.diff
                          (可写层必须落盘,不能用 tmpfs run 根)

  # 资源覆盖(运维临时调整)
  --cgroup-path <path>    覆盖 control.cgroup_path:写到该已存在的 cgroup 绝对路径,
                          并把 CH(仅 CH)move 进去
  --cgroup-adopt          采纳 sandbox-ctl 自己所在的 cgroup(其 systemd 单元的
                          cgroup):把资源上限写到该 cgroup,但**不**把 CH move 进去
                          (CH 作为 fork 出的子进程已是成员,AddPID 成为 no-op)。
                          cgroup 路径从 /proc/self/cgroup 解析(SelfCgroupV2Path),
                          置 control.adopt(YAML `adopt: true`)。注意:adopt 模式
                          让 sandbox-ctl 与 CH 同处一个 cgroup,重新暴露了常规解耦
                          路径所规避的 memory.high 节流死锁(见 pkg/resctl/cgroup.go
                          头注)——面向 run-sandbox 启动器路径

  # 诊断
  --stats-json <path>     退出时把各 backend + uffd 统计以 JSON 写到该路径
  --stats-interval <dur>  周期打印懒加载实时统计(uffd 缺页/换入速率、在飞与排队
                          fault 数、缺页换入取数时延 p50/p99/max、各 blk 读 IOPS/吞吐/
                          p99),便于实时观察慢的远程/缓存或积压的 fault 队列;无活动的
                          tick 跳过(warm 后自动安静)。0 = 关闭。flag >
                          SANDBOX_STATS_INTERVAL env > 默认 30s

  # 一次性启动通知(可选)
  --ready-fd <fd>         向继承 fd 写 control_ready\n、ready\n,成功后关闭该 fd。
                          fd 必须 >= 3;未提供时完全关闭。事件不写普通 stdout/stderr,
                          且 ready 后 run 仍继续拥有 VM、阻塞到 sandbox 退出

  # 可靠性兜底
  --ping-fatal-threshold N  连续 N 次 host→guest ping 失败后,sandbox-ctl 主动给 CH
                          发 SIGTERM(随后宽限升级 SIGKILL),让 cmd.Wait 返回而不是
                          在"还活着但卡死"的 guest 上无限等待。0 = 关闭(默认,等外部
                          信号)。flag > SANDBOX_PING_FATAL_THRESHOLD env > 0;默认
                          1 s ping 间隔下 30 ≈ 30 s 不可达

  # 恢复模式
  --restore <ref>         恢复模式:从 snapshot bundle (<sid>.snapshot 文件 / 或
                          manifest://<hex>)读 snapshot.cfg + 内存内容启动。
                          <ref> = 本地文件路径或 manifest://<hex>。不带此 flag
                          = 冷启动模式。restore 模式下 sandbox.yaml 字段语义
                          见 §11.0.当前内存 self 预取由 sandbox.yaml 的
                          restore.prefetch 显式控制,不增加 CLI flag(§7.1)

  # 应用 stdio(冷启动 + 恢复模式都生效;详见 docs/sandbox-init.md §3.5 / §4.5)
  #
  # 应用的 stdin/stdout/stderr(或一个伪终端)经 vsock MUX 与 sandbox-ctl 双向
  # 转发,不与内核 dmesg 混流。CH 进程自身的 stdio:stdin = /dev/null、stdout =
  # 一根匿名管道(承载内核 dmesg via hvc0)、stderr = sandbox-ctl 的 stderr。
  --tty                   给应用一个真伪终端(guest 内 openpty;isatty()=true、
                          有 job control、收 SIGWINCH),并把 sandbox-ctl 的控制
                          终端切到 raw 模式、与该伪终端逐字节桥接;SIGWINCH 经
                          MUX control 流推给 guest。默认值 = auto-detect:stdin 和
                          stdout 都是终端时为 true,否则 false。显式 --tty 但 stdin
                          或 stdout 不是终端 → 报错退出。raw 模式下键盘 ^C(0x03)
                          作为字节穿到 guest 伪终端、由 guest 行规程转 SIGINT 给
                          应用;杀沙箱另走 SIGTERM(kill <pid> / systemctl stop)
  --stdin                 pipe 模式(--tty=false):默认关闭,应用 fd 0 = /dev/null。
                          --stdin / --stdin=true 让应用 stdin 接 sandbox-ctl 的 stdin
  --stdout                pipe 模式:默认开启,应用 stdout → sandbox-ctl stdout。
                          --stdout=false 关闭(丢弃)
  --stderr                pipe 模式:默认开启,应用 stderr → sandbox-ctl stderr。
                          --stderr=false 关闭
  --stdin-from <file>     pipe 模式:应用 stdin 读自 <file>(隐含 --stdin=true)
  --stdout-to <T>         pipe 模式:应用 stdout 写到 T(隐含 --stdout=true)。
                          T = <file> 或 journald=<tag>(逐行写 journald、
                          SYSLOG_IDENTIFIER=<tag>、PRIORITY=info)
  --stderr-to <T>         pipe 模式:应用 stderr 写到 T(同 --stdout-to 的 T 语法)

  # guest 内核 dmesg(与应用 stdio 完全独立的一条道)
  --console <mode>        guest 内核控制台(hvc0)的去向。off:给 CH --console off,
                          丢弃;default(默认):写 sandbox-ctl 的 stderr(--tty raw
                          模式下做 \n→\r\n 转换);file=<path>:写 <path>;
                          journald=<tag>:逐行写 journald(SYSLOG_IDENTIFIER=<tag>)

  # 端口转发(冷启动 + 恢复模式都生效;详见 docs/sandbox-init.md §3.7)
  --connect <L:TARGET>    dial 模式:host 本地端点 L 转发到沙箱内 TARGET,可重复。
  --connect <L::TARGET>   accept 模式:guest 在 TARGET 上 Listen+Accept,与 L 的每条
                          本地连接配对。L = UDS 路径(@name 抽象)或 fd=N(继承的已
                          listen socket);TARGET = host:port(tcp)或 /path|@abstract
                          (unix,以 / 或 @ 起头)。每条被接受的本地连接经一条反向通道
                          发 connect、guest 拨号或 accept 后双向中继(保留 TCP 半关闭);
                          quiesce 时主动拆除、resume/restore 后恢复
```

**行为**:阻塞前台运行,直到 CH 退出或收到 SIGTERM/SIGINT。退出码:应用正常退出 →
应用退出码;应用被信号杀 → 128+signal;guest panic / CH 异常退出 → CH 退出码映射。
应用退出码来自 guest 经 vsock 发的 `app_exited{code, term_signal}`(见
[`sandbox-init.md`](sandbox-init.md) §4.3);拿不到时回退 CH 退出码。

**进程组与信号**:sandbox-ctl spawn CH 时给它一个新进程组(`Setpgid`),CH 因此不在
sandbox-ctl 控制终端的前台进程组——终端产生的 `^C` / `^\` / `^Z` 不会直达 CH。host
信号(SIGTERM/SIGINT)由 sandbox-ctl 统一处理(只在此一处注册),收到即转发 SIGTERM
给 CH、宽限后 SIGKILL(与 [`sandbox-init.md`](sandbox-init.md) §3.3 的退出
序列衔接)。`--tty` raw 模式下终端是 raw 的、`^C` 不产生 SIGINT(见上);非 raw 模式
下 `^C` 正常触发上述 SIGTERM 升级链。

**启动 readiness wire**:`--ready-fd=N` 是调用方提供的一次性观察通道。成功 wire
严格为 `control_ready\nready\nEOF`;每个事件最多一次且不能反序。语义如下:

- `control_ready`:仅在本次 `<run-root>/<sid>/ctl.sock` 已成功 `Listen` 后写出。
  此时 host-local socket 可连接(连接可先进入 accept queue),但不承诺 guest、exec、
  network、envd 或应用已就绪。
- cold `ready`:host 收到本次 run 的首个现有 `app_started`,并且成功把
  `TypeAck` 完整写回 guest 后写出。应用原地 restart 的后续 `app_started` 不重复
  `ready`;该边界不证明最终目标程序 `execve` 成功,也不表示业务服务健康。
- restore `ready`:依次完成 CH API `WaitReady`、`/vm.resume`、
  `OpenMUXViaRestore` 收到 `restore_ack`、host `EstablishMUX` 后写出。pinger、balloon、
  `SettledRestore`、heartbeat 与 sensor 不在该 barrier 内;不推断 guest thaw-complete。

失败不增加额外事件或 ping fallback:`control_ready` 前失败只得到 EOF;
`control_ready` 后失败得到该行后再 EOF。具体错误仍看 `run` 退出码与 stderr/journald。
若 reader 提前关闭,后续写的 `EPIPE` 只记录一次并禁用 notifier,不会终止 sandbox。
`sandbox-ctl` 在解析 flag 后立即校验 fd、设置 `FD_CLOEXEC`,自行拥有并在所有返回路径
关闭它;写完 `ready` 也立即关闭。未提供 flag 时没有新增日志、goroutine 或行为变化。

Bash 应把 readiness pipe 与应用 stdio 分开;重定向顺序必须先令 fd 3 指向 coprocess
stdout,再重定向普通 stdout/stderr:

```bash
#!/usr/bin/env bash
set -euo pipefail

sid=demo
tmp=$(mktemp -d)

coproc SB {
    exec sandbox-ctl run \
        --sandbox-id "$sid" \
        --config sandbox.yaml \
        --ready-fd=3 \
        3>&1 \
        >"$tmp/run.stdout" \
        2>"$tmp/run.stderr"
}

pid=$SB_PID
ready_fd=${SB[0]}

if ! IFS= read -r -t 10 -u "$ready_fd" event ||
   [[ "$event" != control_ready ]]; then
    echo "sandbox did not reach control_ready" >&2
    kill "$pid" 2>/dev/null || true
    wait "$pid" || true
    exit 1
fi

if ! IFS= read -r -t 60 -u "$ready_fd" event ||
   [[ "$event" != ready ]]; then
    echo "sandbox did not become ready" >&2
    kill "$pid" 2>/dev/null || true
    wait "$pid" || true
    exit 1
fi

sandbox-ctl exec --sandbox-id "$sid" -- /bin/true
```

不要把 readiness marker 写入普通 stdout/stderr:两者分别承载应用输出、运行日志与
guest console,无法提供无歧义的启动协议边界。

**stdio 决策表**:

| 模式 | 应用 fd 0/1/2 | MUX 流 |
|---|---|---|
| `--tty`(或 auto-detect 命中) | guest 伪终端从端(stdout/stderr 合并) | 1 条 PTY 流 + control 流 |
| pipe 默认(stdin 关) | 0=/dev/null,1→sandbox-ctl stdout,2→sandbox-ctl stderr | STDOUT + STDERR + control 流 |
| pipe + `--stdin` / `--stdin-from` | 0 = sandbox-ctl stdin / open(F, O_RDONLY) | + STDIN 流 |
| pipe + `--stdout=false` / `--stderr=false` | 该流不开,应用对应 fd 由 guest 接 /dev/null | 去掉对应流 |
| pipe + `--stdout-to` / `--stderr-to` = F | sandbox-ctl 把该流写 open(F, O_WRONLY\|O_CREATE\|O_APPEND, 0644);F=`journald=<tag>` 时逐行写 journald(SYSLOG_IDENTIFIER=<tag>,journald 不可用则回退带 `[tag]` 前缀的 stderr) | 不变 |

互斥规则:

- `--tty` 与 `--stdin` / `--stdout` / `--stderr` / `--stdin-from` / `--stdout-to`
  / `--stderr-to` 中任一显式赋值互斥(`--tty` 是伪终端模式,没有分流概念)
- 显式 `--tty`(或 `--tty=true`)但 stdin 或 stdout 不是终端 → 报错(host 侧 pty
  无意义,见 [`sandbox-init.md`](sandbox-init.md) §3.5)
- `--stdin=false` 与 `--stdin-from` 互斥;`--stdout=false` 与 `--stdout-to` 互斥;
  `--stderr=false` 与 `--stderr-to` 互斥

**sandbox-ctl 自身日志**:写 `os.Stderr`,单行 `[sandbox-ctl ...]` 前缀;`--tty` raw
模式下经 `\n`→`\r\n` 转换后再写(否则在 raw 终端上阶梯状错位)。`--console default`
透传的内核 dmesg 同样在写 raw 终端前做 `\n`→`\r\n`;应用的 PTY 流原样透传(guest 那侧
伪终端的 ONLCR 已把 `\r\n` 加好)。

**恢复模式**:`--restore=<snapshot-path|manifest://hex|file://basename@location:name>`
让 sandbox-ctl 走恢复路径
(详见 §7)。`<ref>` 形式:

- 本地 file path / `manifest://<hex>` / located file ref — 统一经
  `StreamSnapshotSource`:file 解析为
  稀疏文件流,manifest 经 cache-ctl + store-ctl 按 chunk 粒度 lazy fetch,再按
  snapshot.cfg 的 `from_refs` 叠成分层流(§3.5)写入 memfd。snapshot.cfg 内
  base_ref / overlay.base(+ base_from_refs)同理

**配置交付(文件 vs 内存)与 run-sandbox 启动模型**:上面的 `--config` / `SANDBOX_CONFIG`
是文件路径形态。除此之外,sandbox-ctl 还能在无 config-socket 的前提下接收**内存内**
交付的单份 sandbox.yaml:`pkg/config.LoadConfigBytes` 解析一份在内存中持有的
`SANDBOX_CONFIG` YAML 文档(不从磁盘读),敏感的 manifest 根密钥经 `MANIFEST_KEY`
env 传入(由 `pkg/manifest` 解析)。编排侧的 `node-ctl run-sandbox` 启动器即按此
模型工作:它在 `execve` 成 sandbox-ctl 之前,先把非密的 per-sandbox 配置(文件或内存)
与 `MANIFEST_KEY` env 备好。

### 2.3 `sandbox-ctl snapshot`

```
sandbox-ctl snapshot [flags]

  --sandbox-id <sid>    必填,目标 sandbox
  --output <out_dir>    本地输出目录。crypto.local=off 产出 <sha256>.*,
                        auto|required 产出 <hmac>.*;另建 <sid>.snapshot 符号链接
  --upload              上传至 manifest 存储:overlay 流式 ingest 拿 manifest key,
                        <sid>.snapshot 内嵌 snapshot.cfg 的 overlay.base 写为
                        manifest://<key>;stdout 输出 snapshot manifest key
  --resume              默认 false(快照后销毁沙箱);`--resume` / `--resume=true`
                        保留沙箱继续运行
  --run-root <dir>      与 run 一致;SANDBOX_RUN_ROOT env;默认 /run/sandbox。snapshot 通过
                        <run-root>/<sid>/ctl.sock 联系运行中的 sandbox-ctl run 进程
  --timeout <sec>       等 snapshot_done 的客户端超时。默认 0 = 无限期等待:一次真实
                        的多 GiB 内存 + overlay ingest 上传动辄数分钟,固定客户端
                        deadline 会误杀一个仍在健康推进的上传。需要兜底时显式
                        `--timeout N` 重新设上界
```

**进度输出**:`--upload` / `--output` 期间,sandbox-ctl run 进程按节流周期(≥2 s)
向其 stderr 打 ingest 进度——`upload: <段名> <已处理> MiB (<百分比>) <速率> MiB/s`,
段名为 `overlay` / `memory section`;结束再打一行吞吐 profile(`ingest profile:
<数据量> MiB in <耗时>s = <速率> MiB/s`)。让长时间的静默上传变得可观测,避免被
误判为卡死。

**`--output` 与 `--upload` 严格互斥**——必须二选一,两者都给或都不给均报错
退出。两者代表"持久化目的地"的两种形态(本地 vs manifest 存储),混用会让
snapshot.cfg 的 overlay.base 引用与实际数据位置脱节。

**销毁路径**(`--resume=false` 默认):snapshot 完成后,sandbox-ctl run 进程
通过 CH `/vm.shutdown` 优雅关机(ACPI shutdown → guest sandbox-init 收到事件
→ reboot syscall),等 CH 退出后 sandbox-ctl run 进程自身也退出。`--resume=true`
则跳过 shutdown,调 `/vm.resume` 让沙箱继续运行,sandbox-ctl run 进程不退出。

### 2.4 `sandbox-ctl exec`

在一个运行中的 sandbox 内执行一条临时命令——它是用户应用的**兄弟进程**,
不替换应用,不重启沙箱。命令运行在**应用自己的 mount + pid 命名空间**内,
因此能看见应用的进程树与文件系统、能按 pid 给应用发信号(语义同 `docker exec`
/ `kubectl exec`)。

```
sandbox-ctl exec [--sandbox-id <sid>] [--proxy <http[s]://host[:port]>] [flags] -- CMD [ARGS...]

  --sandbox-id <sid>    本地模式必填,目标 sandbox;远程模式不把它转换为任何 Header
  --run-root <dir>      与 run 一致;SANDBOX_RUN_ROOT env;默认 /run/sandbox。exec 通过
                        <run-root>/<sid>/ctl.sock 联系运行中的 sandbox-ctl run 进程
  --proxy <url>         远程模式:直连最终接收业务 Header 的 HTTP/HTTPS CONNECT endpoint
  --proxy-header <h>    远程 CONNECT Header,格式为 "Name: value",可重复;同名值按 Add
                        语义保留.客户端不解析或生成 SID/service/token/group/route-key
  --cwd <dir>           命令在 guest 内的工作目录(默认 guest 根)
  --user <u>            run-as 身份:"uid[:gid]" 或沙箱 /etc/passwd 用户名(默认 root)
  --env KEY=VAL         追加/覆盖一个环境变量,可重复。在一个默认 PATH 基线上叠加
                        (exec 命令不继承应用的 image env,故 PATH 总是注入,裸命令
                        名才能 PATH 查找)
  # 应用 stdio:与 `run` 完全相同的一组 flag 与决策表(§2.2 stdio 决策表 / 互斥
  # 规则),经各自独立的一条 stdio MUX 双向转发
  --tty / --stdin / --stdout / --stderr / --stdin-from / --stdout-to / --stderr-to
                        语义同 §2.2。`--tty` 是无值布尔 flag:关 pipe 模式要写
                        `--tty=false`,`--tty false` 会把 false 当成命令(Go flag
                        语义,与 run / snapshot 一致)
  -- CMD [ARGS...]      `--` 之后是要执行的命令与参数(至少一个)
```

**退出码**:exec 以 guest 内命令的退出码退出;命令被信号杀则 128+signal。
命令结束的退出码经 stdio MUX 的 EXIT_STATUS 控制帧回传(在所有 stdout/stderr/
pty EOF 之后发出),exec 收到即结束并还原终端,不依赖底层连接关闭的传播。

**传输路径**:`sandbox-ctl exec` → `<run-dir>/<sid>/ctl.sock`(§6.3,与 snapshot
共用该 UDS 协议)→ sandbox-ctl run 进程 → 经反向 vsock 通道发 `exec` 操作给
sandbox-init → guest 为这次 exec 起一条**独立的 stdio MUX**(详见
[`sandbox-init.md`](sandbox-init.md) §3.6 / §4.3)。run 进程在握手后只做
ctl.sock ↔ guest vsock 的透明字节转发,MUX 端到端跑在 `sandbox-ctl exec` 与
guest 之间。多个 exec 会话并发互不影响。远程授权 exec 由可信 node proxy
在完成鉴权并选定目标 sandbox 后,调用 `pkg/ctl.ProxyExec` 对下游首帧做
`exec_request` gate,再接入同一条 `ctl.sock` 与 MUX 路径;不定义另一套 guest
wire。

远程模式只替换上述第一跳拨号:

```text
sandbox-ctl exec --proxy
  → HTTP/HTTPS CONNECT sandbox:443
  → 已鉴权的数据面 tunnel
  → 原有 exec_request → exec_ack → MUX
```

`--proxy`直接指向最终data-plane endpoint,不表示企业HTTP proxy再嵌套第二层CONNECT.
HTTP与HTTPS都使用标准HTTP/1.1 CONNECT;HTTPS使用系统CA和hostname verification,不提供
跳过证书校验选项.任意2xx表示tunnel建立;成功响应忽略HTTP消息体分帧字段,CONNECT响应头
通过有界解析,并保留`bufio.Reader`已经预读的tunnel bytes.非2xx响应按HTTP分帧严格解析,
正文有界读取,错误不会输出`--proxy-header`值.

`--proxy-header`拒绝`Host`,`Connection`,`Proxy-Connection`,`Content-Length`,
`Transfer-Encoding`和`Trailer`等transport-owned字段.这些参数可能包含bearer credential,不会写入
普通日志;携带任意该Header时,HTTP拒绝正文和CONNECT 200后的ctl握手详情均不输出.
命令行参数仍可能被本机进程列表观察;生产SDK后续可用进程内API避免该暴露.

**与 snapshot 的关系**:snapshot quiesce 期间拒绝新的 exec,并 SIGKILL 在飞的
exec 子进程(快照不能带运行中的 exec 兄弟进程);沙箱 resume / restore 后恢复
受理。详见 §6.2 与 [`sandbox-init.md`](sandbox-init.md) §3.6。

### 2.5 `sandbox-ctl config`

从给定来源产出 / 合并 / 校验一份 sandbox.yaml(默认 stdout),不启动沙箱。

```
sandbox-ctl config [flags]

  --config a.yaml[:b...]  读入一组 sandbox.yaml(':' 分隔),按 front-to-back 深合并
                          (后者覆盖前者、嵌套 map 递归合并)后重新序列化输出
                          (SANDBOX_CONFIG env;与 --template 互斥)
  --template              改为输出一份带注释的骨架配置(注释保留;与 --config 互斥)
  --mode default|restore  restore 模式剥掉 restore 会忽略的冷启动专属键:顶层
                          launch / mounts / files / init、boot.kernel / boot.cmdline、
                          boot.root.overlay.base
  --check skip|strict     skip(原样输出)| strict(按 mode 校验:default→ValidateCold,
                          restore→ValidateRestoreHostConfig;不通过则报错且非零退出)
  -o <file>               写到文件(默认 stdout)
```

`--config` 合并路径输出的是规范化后的配置(经解析 + 重新序列化,**注释不保留**);
要带注释的可编辑骨架走 `--template`。

### 2.6 `sandbox-ctl info`

打印一个 snapshot bundle 内嵌的 `snapshot.cfg`(§3.4 的 post-quiesce 平台契约)。
对齐 `flatten-ctl info`。

```
sandbox-ctl info <snapshot-ref | snapshot-path> [flags]

  <snapshot-ref | snapshot-path>  本地路径、manifest://<hex> 或 located file ref
  --json                  默认输出 snapshot.cfg 原始 YAML;--json 改为重新输出解析后的结构
  --manifest-config <p>   存储配置 YAML(MANIFEST_CONFIG env);manifest 输入以及
                          crypto.local=auto|required 的本地工件需要
  --ref-location <name>=<file-URI>  可重复;located file ref 的可信宿主目录映射
```

### 2.7 `sandbox-ctl upload-snapshot`

把一个本地 snapshot graph 离线发布为 canonical portable ref,不启动沙箱、不需
`/dev/kvm`。发布到 manifest 时 local file refs 改写为 `manifest://`;发布到 named
location 时改写为
`file://<content-addressed-basename>@<scheme>:<digest>@location:<name>`。已有
manifest/located refs 原样保留,因此三者可以混合。root snapshot 总会重建并发布,
stdout 只打印新的 canonical root ref。`runtime_ref` 是节点级启动工件,仍按摘要
钉住并随平台分发,不进入租户工件发布域。

```
sandbox-ctl upload-snapshot [flags] <snapshot-path>

  <snapshot-path>         本地 <sid>.snapshot(或其指向的 <digest>.snapshot)
  --manifest-config <p>   存储配置 YAML(MANIFEST_CONFIG env);$MANIFEST_KEY 提供客户密钥
  --to-ref-location <name>=<file-URI>
                          与 --manifest-config 二选一;目标目录只写内容寻址文件
  --quiet                 抑制 stderr 进度日志
```

显式 `--manifest-config` 与 `--to-ref-location` 互斥,因为前者选择 manifest 发布
目标。使用 named location 时仍可由 `MANIFEST_CONFIG` 环境变量提供
`crypto.local` policy;若未提供则按默认 `off`。

local file refs 按 bundle 同目录解析;已有 portable ref 是本次发布边界。发布器按
`snapshot.cfg` 字段区分三种角色:只解析并重建命令行指定的 root snapshot graph;
`from_refs` 中的本地项是已经 flatten 的 memory-only lower,作为 opaque snapshot
tarstream 独立发布,不再读取其中历史 `snapshot.cfg` 或追踪它的旧 disk refs;当前
root/data disk 字段中的本地项同样作为 opaque leaf 发布。相同 realpath 的缓存键还包含
角色,避免同一工件在 root graph 与 memory layer 两种语义间错误复用。输出 root 的
所有本地依赖均被改写为目标 portable ref,原有顺序与重复项保持不变。

发布前对每个本地 tarstream 执行 sequential full validation,再从逻辑 plaintext
稀疏视图重新编码;不会 raw-copy 工件。named location publisher 以 exclusive create
直接取得最终内容寻址文件,顺序复制后执行 `Sync`、`Close` 并重新打开做完整验证;
失败时只清理本次 publisher 成功 exclusive create 的不完整文件。终态已存在时先
验证 tarstream 格式、logical size、digest scheme/digest、marker、codec 和本地加密
策略:全部一致则复用;不一致的普通文件直接删除后以 exclusive create 重试,直到上传
成功或调用 context 取消。context 取消或超时不作为内容不一致的证据,不会触发删除。
打开、读取、stat 或 sync 的系统错误同样不证明内容不一致:publisher 返回错误并保留
现有终态,由后续上传重试。复用成功前会同步已验证的文件和父目录。
并发修复不引入 advisory lock 或锁文件:竞争期间 publisher 可以删除彼此尚未完成的
普通 final,竞争停止后最后仍在执行的上传完成发布。symlink 和非普通文件始终拒绝且
不会删除。

因此 named ref-location 的共享文件系统只需支持 `mkdir`、create、exclusive create、
write、read、stat、pread/seek、chmod、file/directory sync,以及删除不一致或不完整的
普通文件;不要求 rename、renameat2、symlink、hardlink、reflink、advisory lock 或
sparse file。节点本地 checkpoint 的
`SnapshotSink`/`FileSink` 和运行期 active writable vhost diff 仍依赖现有原子提交及
本地文件系统语义,不属于 named ref-location 的共享文件系统兼容范围。

## 3. 配置

### 3.1 sandbox.yaml

```yaml
# 资源规格
resources:
  capacity:                    # vCPU/内存数(guest 看到的"声明规格")
    cpu: 2
    memory: 8GiB
  allocatable:                 # ≤ capacity,默认 = capacity
    cpu: 0.1                   # ≤ capacity.cpu;无 cgroup 模式必须 == capacity.cpu
    memory: 128MiB             # balloon 初始膨胀 = capacity.memory − 此值
    deflate_on_oom: true       # CH --balloon 是否带 deflate_on_oom

  control:                     # 部署模式驱动
    cgroup_path: ""            # 空 = 无 cgroup 模式;非空 = 必须已存在的 cgroup 绝对路径
    controller: ""             # 空 = 无 cgroup / 静态 cgroup 模式;非空 = 动态控制模式(UDS 路径)
    sensor:                    # 压力感知器(动态控制模式;§10.3),可选,缺省 psi 默认值
      mode: psi                # psi | events_poll | none;psi 失败自动回落 events_poll
      psi_some_stall_us: 10000 # PSI trigger:1s 窗口累计 10ms stall 触发(密集 workload 实测甜点)
      psi_some_window_us: 1000000
      min_interval_ms: 100     # 两次 RequestBudget 最小间隔;PSI 抖动去抖

  overhead:                    # 仅 cgroup_path 已设时允许;默认 32 MiB
    memory: 32MiB              # memory.max = capacity.memory + 此值
  watermark_high:              # 仅 cgroup_path 已设时允许;默认 allocatable.memory × 0.875
    memory: 128MiB             # cgroup memory.high 初始值
  startup:               # 仅 controller 已设时允许;默认 = allocatable.memory
    memory: 256MiB             # 启动期 allocatable_now;约束 floor ≤ 此 ≤ capacity

# 网络:整个 network 块可省略或写成 {},表示不挂 virtio-net 设备.
# 启用网络时源最多选一个(tap 名 / tapfd 交接);属性在 tapfd 模式下被交接元数据覆盖.
network:
  # 源(最多一个):
  tap: tap0                    # 预创建的 host TAP 名;CH 按名打开(dev/e2e,无 provider)
  tapfd:                       # tapfd 交接(docs/tapfd.md §3/§4):exec helper 或持久 provider socket
    exec: ["connector-ctl", "vswitch", "open-port", "sw0", "--port=3"]
    # socket: /run/kuasar/connector/sw0/tapfd.sock
    # request: "VSWITCH=sw0 PORT=3"
    # timeout: 5s              # 交接超时(Go duration);默认 5s

  # 属性:tap 模式按下值生效;tapfd 模式下 mac/ip 被交接元数据覆盖——
  #   meta.mac→mac、meta.ip→ip(仅替换地址,保留下方掩码)
  mac: ""                      # virtio-net MAC(CH --net mac=);空 + tap 模式 → CH 自动分配
  ip: 169.254.1.1/31           # guest CIDR(IPv4 或 IPv6);空则不配 IP
  mtu: 1500                    # guest 网卡 MTU;0 用内核默认
  nexthop: ""                  # 默认路由下一跳;空则不配默认路由
  hostname: my-sandbox         # guest hostname(sethostname)
  interface: eth0              # guest 内网卡名,默认 eth0

# 无网络源时不得保留 mac/ip/mtu/nexthop/hostname/interface 等属性.

# Guest 启动 + rootfs
boot:
  kernel: file:///opt/sandbox/vmlinux              # 仅冷启动需要
  runtime: file:///opt/sandbox/sandbox-runtime.bundle  # 仅 file://
  cmdline: ""                                      # 追加项(quiet/loglevel= 等);init=/root=/rootflags=/console=hvc0 平台已自动注入
  root:
    base: file:///container-snapshot.erofs    # 自动挂为 disk0(vhost-user-blk ro)
                                              # flattened image,尾部附加的 zip 内含
                                              # config.json 定义默认容器启动配置
    overlay:                                  # 自动挂为 disk1(vhost-user-blk rw)
      base: file:///container-snapshot.ext4   # 可选,快照恢复时常用 manifest://
      diff: ""                                # 可选,file:// only。空 → 默认落盘
                                              # file:///var/lib/sandbox/<sid>/<sid>.overlay.diff
                                              # (自动创建的随沙箱销毁;显式给定的不删)
      diff_template: file:///opt/sandbox/overlay-templates/basic-1G.ext4
                                              # 可选,file:// only。diff 不存在时把该预格式化
                                              # ext4 的逻辑稀疏视图写入新 diff;目标编码由
                                              # crypto.local 决定。已存在的 diff 忽略此项
      diff_size: 1GiB                         # 可选,默认 1GiB。仅在"创建空白 diff"(无模板、
                                              # 无 base)时用于定尺寸;已有 diff 保持自身大小
    # —— 单磁盘模式:省略上面的 overlay 即启用(详见 docs/sandbox-init.md §3.1)——
    # 不写 boot.root.overlay 时,root 盘本身是可写 ext4,直接挂为 disk0(无 overlayfs、无 disk1)。
    # 下面三项是 overlay.{diff,diff_template,diff_size} 的 root 层等价物,给 root 盘写能力;
    # 与 overlay.* 互斥。base 可选(须是 ext4 镜像作 CoW 下层);无 erofs 镜像 ⇒ 无内嵌
    # config.json ⇒ launch.exec 必填(launch.placeholder 占位模式除外)。
    #   diff:          file:///var/lib/sandbox/<sid>/<sid>.overlay.diff  # 可写盘;空→自动落盘
    #   diff_template: file:///opt/sandbox/root-templates/app-2G.ext4    # 预格式化 ext4,seed 新盘
    #   diff_size:     2GiB                                              # 同 overlay.diff_size
    #   base_from_refs: []                                              # 单盘快照链(snapshot.cfg 自动填)
  # —— 数据盘 boot.disks[](root 之外的附加盘,最多 8 块)——
  # 每块盘的配置规则与 boot.root 完全相同(单盘 diff / 双盘 overlay,含 base_from_refs);
  # 多一个 name(仅配置期用,运行期转序号)。每块盘必须被 mounts[].type=disk 挂载恰好一次
  # (定义却不挂载 = 配置错误)。设备序:root 在前(单盘 1 个 / overlay 2 个),再按本数组序,
  # 决定 guest /dev/vd[a,b,c…];详见 docs/sandbox-init.md §3.1。
  disks:
    - name: scratch                            # 单盘:可写 ext4(diff/diff_template/diff_size)
      diff_template: file:///opt/sandbox/disk-templates/scratch-50G.ext4
      diff_size: 50GiB
    - name: dataset                            # 双盘 overlay:ro erofs base + rw ext4 upper
      base: file:///opt/sandbox/datasets/models.erofs   # 只读共享数据集(跨沙箱去重)
      overlay:
        diff_template: file:///opt/sandbox/disk-templates/blank-10G.ext4
        diff_size: 10GiB

# 容器应用启动配置(覆盖 boot.root.base 内嵌的 config.json 默认值;单磁盘模式无内嵌配置,exec 必填;
# 占位模式 placeholder:true 例外——不跑外部程序)
launch:
  exec: /usr/bin/foo
  args: ["arg1", "arg2"]
  # placeholder: true         # 占位模式:不 exec 任何外部程序,app 仅做完 ns/cgroup/stdio 准备后
  #                           #   等待停机信号——"空白锚点"沙箱,完全靠 `exec` 驱动(磁盘规则不变,
  #                           #   仍需照常配 boot.root)。与 exec 互斥;恒为 restart=always
  #                           #   (从 exec 会话里 kill 掉占位 ⇒ 原地重拉,而非 reboot 整个沙箱)
  env:
    PATH: /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
    HOME: /root
  workdir: /
  restart: never              # never|on-failure|always。never=应用退出即沙箱 reboot(一次性);
                              #   always=原地重启;on-failure=非0/信号退出重启、干净退出 reboot。
                              #   重启用退避 10ms→60s、存活满 60s 重置;stdio MUX 跨重启存活
  pid_namespace: private      # private(默认)=应用是自身 PID ns 的 PID 1;
                              #   shared=应用在 sandbox-init 的 PID ns,复用 PID1 reaper 收割孤儿
  user: "0:0"                 # uid:gid 或 name:group(覆盖镜像 User);命名用户由 guest 侧读 /etc/passwd 解析
  stop_signal: SIGTERM        # 停机信号(覆盖镜像 StopSignal);信号名或编号;空 → SIGTERM
  stop_grace_period: 10s      # 发停机信号后等应用退出的宽限,超时则 SIGKILL;默认 10s
  start_timeout: ""           # host 等待 launch_ack(含 init 全程)的超时;空 / 0 = 无限期
                              #   (见 sandbox-init.md §4.10)
  # 伴生进程(plugin):与 launch.exec 同 rootfs/cgroup/网络运行的常驻 sidecar,各自独立监督。
  # plugin 退出不影响沙箱生命周期(只有 launch.exec 退出才按 launch.restart 决定 reboot/重启)。
  plugin:
    - exec: /usr/bin/sidecar
      args: ["--serve"]
      env: { LOG: info }        # 可选,合并入默认 PATH
      workdir: /                # 可选
      user: "0:0"               # 可选
      restart: always           # never|on-failure|always;省略 → always(伴生进程默认常驻)

# 本次 host restore 的机会式优化策略,不写入 snapshot.cfg.默认关闭;
# 仅为经过工作集评估的特定 sandbox 显式改为 memory(§7.1).冷启动忽略此策略.
restore:
  prefetch: off                 # off(默认) | memory(当前 memory self)

# host 侧恢复/生命周期超时;guest/远程耦合项默认 0 = 不强制(host 等待 guest/懒加载所需的
# 任意时长,dial/connect 探测仍有界),便于慢速/降级环境:慢的远程仓库/缓存、或调试器暂停都不会
# 误中止一个仍在健康推进的 restore。生产环境按 examples/timeouts-production.yaml 设正值快速失败。
timeouts:
  restore: ""       # 等 guest restore_ack(/vm.resume 之后);恢复时大量缺页换入会拉长此段
  ch_api: ""        # 所有 CH HTTP API 调用(pause/resume/snapshot/shutdown)的单次响应死线;
                    #   dial 仍 5s 有界。**例外:默认 60s**(CH API 是本地管理调用、快且不受远程/
                    #   缓存影响,60s 是安全网而非热限);置 "0" 表示不强制
  api_ready: ""     # spawn 后等 CH API socket 可连接(轮询);0 = 轮询至 ctx 取消(如 CH 退出 / SIGINT)
  va_report: ""     # CH→host 交接 uffd fd 的握手
  ping: ""          # host→guest ping 往返;默认不强制=vCPU 被慢缺页短暂阻塞时不误判。
                    #   注意:若启用 --ping-fatal-threshold,须设有界值(生产档 200ms),
                    #   否则卡死但仍连通的 guest 不会触发兜底(ping 会一直等而非失败)
  app_notify: ""    # host 读一条 guest→host launch 端口消息(hello / app_started /
                    #   app_exited / mem_report)的死线;默认不强制=慢 guest 的 mem_report
                    #   连接不会在 restore 中途被丢弃

# 声明式挂载:在 rootfs 组装后、应用拉起前应用,顺序即列表序
mounts:
  - { target: /tmp,     type: tmpfs, options: "nosuid,nodev,mode=1777" }
  - { target: /var/log, type: empty }      # 空目录卷:遮蔽镜像该路径原内容,落 vdb(磁盘),
                                            # 模拟容器 VOLUME / k8s emptyDir(仅"初始化为空"语义)
  - { target: /scratch, type: disk, source: scratch }  # 挂 boot.disks[].name=scratch 的整块数据盘
  - { target: /data,    type: disk, source: dataset }  # 每块 boot.disks[] 须恰好挂一次(1:1)
  # type 省略 = empty;tmpfs = 内存盘;disk = 挂 boot.disks[] 数据盘(source 关联其 name)。
  # /run 与 /run/shm 由 runtime 自动挂载,无需声明。
  # 镜像 config.json 的 Volumes 自动并入(等价 type: empty);显式声明同 target 时以显式为准。

# 文件注入:内容写入内存盘后 bind 到目标路径——仅在内存、不落 vdb,适合 secret
files:
  - path: /etc/resolv.conf
    mode: "0644"              # 八进制;默认 0644
    owner: "0:0"              # uid:gid 或 name:group;默认 0:0
    read_only: false          # true → bind 后 remount 只读
    content: |
      nameserver 169.254.169.253
      options timeout:2 attempts:2

# 应用拉起前顺序执行的一次性初始化命令(类 initContainers);任一条非零退出/超时 = 沙箱启动失败。
# 一次性、早期执行语义(跑完即走);常驻进程请用 launch.plugin[]。
init:
  - exec: /bin/sh
    args: ["-c", "echo provisioning"]
    env: { STAGE: init }      # 可选,合并入默认 PATH
    workdir: /                # 可选
    user: "0:0"               # 可选,默认 root
    timeout: 30s              # 可选,Go duration;超时则 SIGKILL 并判失败;空/0 = 不限时
```

**`launch` 增强字段的来源与合并**:`user` / `stop_signal` 与 `exec` / `args`
一样遵循"镜像 config.json 默认值 ⊕ yaml override(override 优先)";`stop_signal`
的信号名在 **host 侧**解析成编号下发(信号名与 rootfs 无关);`user` 的命名用户在
**guest 侧**解析(`/etc/passwd` 权威地在镜像 rootfs 内,host 不假设)。
`stop_grace_period` 下发 guest 用于停机宽限;`start_timeout` 只在 host 侧约束
launch 握手,不下发 guest。

**`timeouts` 的设计取舍(为何默认不强制)**:uffd 缺页与 vhost 块读这两条懒加载
取数路径**本身不设 runtime 超时**——它们阻塞在 accelerator 客户端上,而该客户端
`cache.timeout` / `store.timeout` **`<=0` 即"无逐操作 deadline"**(dial 仍有界),
故慢的远程/缓存只会让换入变慢、不会误超时;恶劣环境把这两个值调大或置 0 即可。会
**非自愿误触发**的是 host 侧写死的管理/恢复面 deadline(restore 握手、CH `/vm.resume`
响应、CH API socket 就绪、va_report 交接,以及 **host→guest ping** 与 **guest→host
管理消息(mem_report / app_started / app_exited)** 的读死线——后两者在 vCPU 被慢缺页
阻塞时最易误判为 ping 超时 / mem_report broken-pipe)——这些一律由
`timeouts` 接管,默认 0 = 不强制(host 等待所需任意时长,dial/connect 探测仍有界,可经
SIGINT / CH 退出取消)。这样默认配置在慢速或降级环境下"开箱即用、不误杀";生产环境按
[`examples/timeouts-production.yaml`](../examples/timeouts-production.yaml) 设正值,
让 restore 在真正卡死时快速失败。

所有 CH API 调用(restore 的 /vm.resume、snapshot 的 pause/snapshot/resume、关停的
vmm.shutdown)统一经**单一 `pkg/chapi` 客户端**(`chapi.Client`,`timeouts.ch_api` 作
其响应死线)。`ch_api` 是唯一**默认有界(60s)**的项:
CH API 是本地管理调用、快且不受远程/缓存慢影响,60s 是安全网而非热限;置 `"0"` 才不强制。

> **`ping` 与 `--ping-fatal-threshold` 的配合**:`ping` 默认不强制时,卡死但仍连通的
> guest 不会触发 fatal 兜底(ping 一直等而非失败)。若启用 `--ping-fatal-threshold`,
> 须同时把 `timeouts.ping` 设为有界值(生产档为 200ms)。`app_notify` 只约束 host 读;
> guest 侧(sandbox-init)写通知仍保留短的有界写死线作为"host 已死"的快速失败。
> `snapshot` 子命令另有 `--timeout` 自管上界(§2.3)。

**`mounts` / `files` / `init` 的应用时机**:三者均在 guest 收到 LaunchSpec 后、
应用进程拉起前生效(`init` 在 `launch_ack` 之前完成,故 host 的 "settled" 信号代表
"环境与 init 全部就绪",详见 [`sandbox-init.md`](sandbox-init.md) §3.2)。
`mounts` 的 `empty` 卷落 vdb(磁盘、不耗内存),`files` 落内存盘(不进磁盘快照层)。

**冷启动(golden) vs restore(per-instance)注入**:`mounts` / `init` / 静态
`files` 在冷启动期应用,会被黄金快照捕获、由 1:N 克隆共享。需要**逐实例不同且排除出
黄金快照**的文件(per-instance secret、实例专属 resolv.conf),由 sandbox-ctl 在
**restore 时**经 restore 通知把该实例的 `files` 推送给 guest,guest 在 thaw 前注入
(与网络 flush-and-replace 同一窗口,见 §11.0);此类内容仅落克隆自身内存、不入黄金快照。
路由由 sandbox-ctl 编排决定(冷启动 → LaunchSpec,restore → restore 通知),与网络
字段一致,schema 无需 per-instance 标记。

存储配置(manifest/store/crypto/cache 节)单独存在,不放入 sandbox.yaml——它
是节点级配置,所有 CLI 共享(sandbox-ctl 通过 `--manifest-config` 或
`MANIFEST_CONFIG` 环境变量引用,详见 `accelerator/docs/manifest.md`)。

本地 immutable tarstream 复用同一 `crypto` 节:

```yaml
crypto:
  chunk: aes
  manifest: aes
  local: off             # off | auto | required;默认 off
```

`crypto.local` 是兼容/强制 policy,不是算法选择。`off` 不解析本地 customerKey,
也不构造 codec;`auto` 用固定 AES-SIV codec 写 encrypted v1,读取时兼容历史
plaintext;`required` 同样加密写入,并拒绝 plaintext 与 legacy `@sha256` ref。
`auto|required` 必须在进程启动配置阶段取得有效 `$MANIFEST_KEY`,否则 fail closed。
同一 sandbox-ctl 进程只解析一次 key,并把同一固定值交给 manifest key table、
tarstream codec、HMAC identity 和 active diff header key wrapping。构造本地 codec
不连接 store/cache;manifest 客户端在第一次实际 fetch/ingest 时才建立。

**绝对路径要求**:sandbox.yaml 中所有 `file://` URL 必须是绝对路径
(`file:///abs/path/to/file`)。配置校验阶段拒绝 `file://relative/path`
——避免不同启动目录(systemd unit / shell pwd / orchestrator workdir)解释路径
不一致。restore 模式下 `boot.runtime` / `boot.root.base` / `boot.root.overlay.diff`
若提供也仍要求绝对路径(只是允许整字段不写,由 snapshot.cfg 复制,详见 §11.0)。

**自动注入的 kernel cmdline**(用户不写、不可改):

```
init=/sbin/init root=/dev/pmem0 ro rootfstype=erofs rootflags=dax=always console=hvc0
```

锁定 `sandbox-runtime.bundle` 通过 virtio-pmem DAX 挂为 `/`、由 `/sbin/init`
(sandbox-init 二进制)接管;`console=hvc0` 让内核 dmesg 走 virtio-console(CH
`--serial off`,没有 8250 UART)。`boot.cmdline` 的内容追加在后,用户控制 `quiet`
/ `loglevel=` 等(`console=` 与 `init=` / `root=` 已被平台占用,用户不应再写)。

**guest 内核 dmesg 的去向**:CH 把 hvc0 写到 CH 进程的 stdout = sandbox-ctl 给它
的一根匿名管道;sandbox-ctl 按 `run --console` 标志决定丢弃(`off`)/ 写 stderr
(`default`,密度部署 / stdout 容量受限场景照样可以 `off`)/ 写文件(`file=<path>`),
详见 §2.2。应用的 stdin/stdout/stderr 是另一条道(vsock MUX),不混入内核 dmesg。

**网络配置不进 cmdline**:IP/MTU/Nexthop/Hostname/Interface 通过 vsock launch 协议
下发给 sandbox-init;phase 2 由 sandbox-init 用 raw netlink 配置(IFF_UP[+IFLA_MTU] +
RTM_NEWADDR + 可选 RTM_NEWROUTE)。restore 时同一组字段可经 restore 通知重新下发,
sandbox-init 以 flush-and-replace 重配网卡(克隆取新 L3 身份,见 §11.0)。kernel 不含
`CONFIG_IP_PNP*`,不支持 `ip=...` cmdline 形式。

**`launch.*` / `mounts` / `files` / `init` 不进 cmdline**:容器启动配置
(exec/args/env/workdir/restart/user/stop_signal/stop_grace_period)及挂载 / 文件 /
初始化命令均通过 vsock 在运行时下发,见 [`sandbox-init.md`](sandbox-init.md)
§3.2。`start_timeout` 仅在 host 侧约束 launch 握手等待。

### 3.2 file:// vs manifest:// truth table

`file://` 有两种语义:不带 qualifier 的路径是 node-local ref;带
`@location:<name>` 的 ref 只允许 basename,实际目录必须由可信的可重复
`--ref-location name=file:///absolute/path` 提供。digest qualifier 是
`@sha256:<digest>` 或 `@hmac:<digest>`,如存在必须位于 `@location` 之前。located
file ref 与 `manifest://<key>` 都是 portable ref;
`manifest://` 只允许单个 key,分层通过 `base_from_refs` 等显式数组表达。

| 字段 | `file://` | `manifest://` | 备注 |
|------|-----------|---------------|------|
| `boot.kernel` | ✓ | ✗ | vmlinux 体积小且节点级共享,manifest 化没有收益 |
| `boot.runtime` | ✓ | ✗ | sandbox-runtime.bundle 节点级共享,DAX 直接用 host 文件 |
| `boot.root.base` | ✓ | ✓ | overlay 模式:erofs 镜像;单盘模式:可选 ext4 CoW 下层。跨 sandbox 复用率高,manifest 化收益最大 |
| `boot.root.overlay.base` | ✓ | ✓ | 快照恢复时常用 manifest:// |
| `boot.root.overlay.diff` | ✓ (only) | ✗ | 运行时 dirty 数据,本地 sparse 文件;可选,空→落盘 base 目录 |
| `boot.root.overlay.diff_template` | ✓ (only) | ✗ | 预格式化 ext4 逻辑初始化源,seed 新建 diff |
| `boot.root.diff`(单盘) | ✓ (only) | ✗ | 单盘可写根盘,overlay.diff 的 root 层等价;省略 overlay 时启用 |
| `boot.root.diff_template`(单盘) | ✓ (only) | ✗ | 单盘根盘的预格式化 ext4 逻辑初始化源;与 overlay.* 互斥 |
| `boot.root.base_from_refs`(单盘) | ✓ | ✓ | 单盘快照链(snapshot.cfg 自动填,§3.5) |
| `run --restore=<ref>` | ✓ | ✓ | 本地路径、located file ref 或 manifest://<key> |
| `restore.prefetch: memory` | ✓ | ✓ | 默认 `off`;仅预热当前 memory self:file 提交 `FADV_WILLNEED`,manifest 预热 chunk cache,§7.1 |

### 3.2.1 file artifact identity

本地磁盘、overlay 和 snapshot 使用 tarstream 工件:第一个 entry 是稀疏 payload,
第二个 entry 是 size=0 的 `.kuasar.sha256.<plainDigest>` marker。`plainDigest` 是
marker header 之前 canonical plaintext tar bytes 的 SHA256;marker 和两个 trailer
blocks 也属于 canonical tarstream。marker 在加密工件中位于密文内部。

本地 identity 通过 `tarstream.Digester.Digest()` 返回 `(scheme,digest)`:

- `crypto.local=off`:不传 codec,输出保持历史 plaintext byte-identical,scheme 是
  `sha256`,digest 是 `plainDigest`
- `crypto.local=auto|required`:传固定 AES-SIV codec,writer 把完整 canonical
  tarstream 包入 encrypted v1 envelope,固定 record size 4096。scheme 是 `hmac`,
  digest 是 `HMAC-SHA256(customerKey,plainDigestRaw32Bytes)`

`hmac` 只是 key-bound identity scheme,不是物理 encoding flag。`auto` 读取历史
plaintext 时也只向上层返回 `hmac`,不暴露内部 `plainDigest`;是否拒绝 plaintext
只由 `required` 决定。新生成的 file ref 一律携带显式 digest qualifier。兼容矩阵:

| `crypto.local` | plaintext / legacy `@sha256` | encrypted / `@hmac` | 新写入 |
|----------------|--------------------------------|----------------------|--------|
| `off` | 接受 | 拒绝(无 codec) | plaintext + `@sha256` |
| `auto` | 接受,内部转换为 expected `hmac` | 接受 | encrypted v1 + `@hmac` |
| `required` | 拒绝 | 接受 | encrypted v1 + `@hmac` |

随机访问经 `fetch.OpenTarStream` + `SourceAt` 保持 lazy 和 O(1) record 定位,读取过的
record 分别认证,适合 restore、merge 和 info。upload、named-location publish 与
conversion 使用 `SourceFrom` 顺序消费到 outer EOF,重算 inner digest,验证 marker、
trailer、expected identity 和额外尾字节。encrypted magic 一旦命中,认证或格式失败
都不会回退 plaintext。

`boot.runtime` 使用专用 bundle:

```text
raw EROFS | zero padding | ZIP(.kuasar.sha256.<hex>)
```

EROFS 保持从 offset 0 开始,bundle 总大小保持 2 MiB 对齐。runtime digest 覆盖
ZIP 前的 EROFS + padding,启动和恢复只从 EOF 读取 ZIP marker,Cloud Hypervisor
仍把整个 bundle 直接作为 virtio-pmem backing。

### 3.2.2 active diff encryption

active diff 是 mutable sparse block device backing,不使用 tarstream record envelope,
也没有 Digester、`hmac` ref 或内容寻址文件名。`crypto.local=off` 新建历史 plaintext
sparse 文件;`auto|required` 新建 encrypted v1 文件。`auto` 可打开历史 plaintext,
`required` 拒绝历史 plaintext。两种加密 policy 都要求当前进程启动时取得的同一
customerKey。

encrypted v1 的物理布局固定为:

```text
[4096-byte wrapped header region][length-preserving sparse XTS body]
```

前 16 bytes 是 authenticated prefix:magic `89 4b 44 58 54 53 31 0a`、big-endian
version `1`、prefix size `16`、flags `0`。其后的 wrapped plaintext 固定 128 bytes,
包含 logical size、block size `4096`、XTS data-unit size `512`、body offset `4096`、
key size `64`、每文件随机 64-byte XTS key 和 40 bytes 零保留区。该 plaintext 使用
accelerator #27 AES-SIV codec 和 AAD `"kuasar/diff/header/v1\0" || prefix` 加密认证,header region
剩余 bytes 必须为零。body 使用 AES-256-XTS,每个 512-byte logical sector 的 sector
number 作为 tweak;body 物理长度与 logical disk 相等,guest offset 0 对应物理
offset 4096。

AES-XTS 只提供磁盘扇区保密性,不认证 mutable body,也不承诺检测 sector replay、
relocation 或回滚。header 认证只保护格式、logical size 和 wrapped XTS key。每个新
target 都生成独立随机 XTS key;不使用 HKDF、per-file salt 或额外 header HMAC。

`diff_template` 是逻辑初始化源而非目标物理编码。existing non-empty diff 优先并
完全忽略 template。新目标按下面矩阵创建:

| `crypto.local` | plaintext template | encrypted template |
|----------------|--------------------|--------------------|
| `off` | 按逻辑 sparse view 创建 plaintext target | 拒绝(没有 codec) |
| `auto|required` | 按逻辑 plaintext 读取,以新随机 XTS key 创建 encrypted target | 用当前 customerKey 解 header,按逻辑 plaintext 读取,以新随机 XTS key 重新加密 |

任何 template data extent 涉及的 4 KiB block 都完整初始化 8 个 XTS data units;
template hole 保持 target hole。seed 先写同目录临时文件并执行 Sync + Close,再以
no-replace 原子提交并同步父目录;失败不修改 final target。禁止 raw-copy encrypted
template 或复用其 XTS key。encrypted header 写入并 Sync 后、template seed 之前必须
确认 body 仍全为 hole;若目标文件系统的 allocation / SEEK_DATA 粒度让 4 KiB header
extent 跨入 body,创建直接失败,不引入 persistent bitmap 或零扫描兜底。

### 3.3 flattened image 内嵌 config.json

`boot.root.base` 指向的 erofs 文件结构借鉴 `<sid>.snapshot`:

```
偏移 [0, erofs_end)              erofs 文件系统
偏移 [erofs_end, EOF)             ZIP archive
                                    / config.json     OCI image runtime config
                                    / ...
```

ZIP 中央目录在文件末尾,erofs 内核驱动从文件起点读到 erofs superblock 标记的
尺寸,自然忽略后缀;ZIP 解析器从尾部倒推。同一个文件:guest 把 erofs 部分
挂为 / 的 lower 层,sandbox-ctl 在启动前读 ZIP 取 config.json 作为 launch
默认值,与 sandbox.yaml `launch.*` 合并(yaml 优先)。

honor 的 OCI config 子集:`Entrypoint` / `Cmd` / `Env` / `WorkingDir` /
`User` / `StopSignal` / `Volumes`。合并规则:`exec`/`args` 按 Docker
`--entrypoint` 语义;`env` 镜像在下、override 在上;`workdir` / `user` /
`stop_signal` override 优先否则取镜像;`Volumes` 的每个目录自动并入 `mounts`
(等价 `type: empty`),与显式 `mounts` 按 target 去重(显式为准)。

`launch.exec` 不再必填——若 image config 有 Entrypoint/Cmd 即可省略。

### 3.4 snapshot.cfg(snapshot 内嵌)

`snapshot.cfg` 是 `<sid>.snapshot` 末尾 ZIP 内的一个 entry,记录 snapshot 时
**guest 那一侧不可重生的契约**——capacity、runtime / image base 内容指针、
overlay 数据指针。**host 侧可重选的字段一概不存**(network、launch、cgroup、
controller、cmdline 等都由 restore 调用者通过 sandbox.yaml 重新提供)。

**字段 schema**(YAML):

```yaml
# 资源规格仅留 capacity
resources:
  capacity:
    cpu: 2
    memory: 8GiB

# 平台透传元数据(可选):runtime 永不解释,sandbox.yaml 的 metadata 原样带入,
# 跨 restore 继承(host yaml 显式给 metadata 则整体覆盖),upload 重渲染时保留,
# info --json 可读。键名建议带命名空间(编排层自用,如 e2b.start_cmd)。
metadata:
  e2b.start_cmd: "npm run start"
  e2b.ready_cmd: "curl -sf localhost:3000/health"

# 内存快照链(增量分层):本快照的内存段是链顶,from_refs 是其下各层
# (自顶向下,不含自身)。冷启动产生的首个快照 = [];从 s1 恢复再存的 s2 =
# [s1.snapshot];从 s2 恢复再存的 s3 = [s2.snapshot, s1.snapshot]。
from_refs: []
  # - manifest://<key-of-parent.snapshot>
  # - file://<digest>.snapshot@<scheme>:<digest>  # scheme = sha256 | hmac

# Guest 启动 + rootfs
boot:
  runtime_ref: file://sandbox-runtime.bundle@sha256:<digest>
                 # boot.runtime 仅支持 file://(见 §3.2 truth table),
                 # snapshot.cfg 保存的 runtime_ref 也只会是 file:// 形式
  root:
    base_ref:    file://container-image.erofs@<scheme>:<digest>
                 # 或 manifest://<key>(原引用是 manifest:// 时原样保留)
    overlay:
      base:      file://<digest>.overlay@<scheme>:<digest>
                 # 或 manifest://<key>(--upload 模式)。本快照捕获的 diff = 链顶
      base_from_refs: []
                 # 磁盘 diff 链(base 之下,自顶向下,不含 base)。与 from_refs 对称:
                 # s1 = [];s2 = [s1.ext4];s3 = [s2.ext4, s1.ext4]

  # 单磁盘模式(快照取自单盘 sandbox):无 base_ref、无 overlay 节,改用 root 层:
  # root:
  #   base:           file://<digest>.overlay@<scheme>:<digest>  # root diff 链顶(或 manifest://)
  #   base_from_refs: []                         # 单盘磁盘链;冷启动会把 root.base(CoW 下层)入链
  # 数据盘 boot.disks[]:有序数组(不含 name,按序号 = cold boot.disks[] 序),每项与 root 同结构
  # (single → base+base_from_refs;overlay → base_ref+overlay{base,base_from_refs})。每块可写
  # diff 单独捕获为一个 .overlay 工件(本地)/ manifest key(上传)。恢复时与 restore host yaml 的
  # boot.disks[] 按序号合并(数同序同)。
  # disks:
  #   - { base: file://<digest>.overlay@<scheme>:<digest> }                  # 单盘数据盘
  #   - { base_ref: file://ds.erofs@<scheme>:<d>, overlay: { base: file://<digest>.overlay@<scheme>:<digest> } }  # overlay 数据盘
```

**字段说明**:

- `resources.capacity.{cpu,memory}`:guest 看到的物理规格。restore 时
  sandbox.yaml 若提供必须严格相等,不一致拒绝启动(详见 §11.0、§13)
- `runtime_ref`:始终 `file://<basename>@sha256:<digest>` 形式
  (`boot.runtime` 本身只支持 file://)。restore 时 basename 用于在
  `<sid>.snapshot` 同目录定位文件,digest 与 runtime bundle 内 marker 比较,
  防止选择不同版本
- `base_ref`(file:// 类):`file://<basename>@<scheme>:<digest>`,其中 tarstream
  工件的 scheme 由 §3.2.1 的本地 policy 决定
- `base_ref`(manifest:// 类):原样保留 manifest key(`manifest://<key>`),
  无需 digest(manifest key 已是 content-addressable)
- `overlay.base`(file:// 类):指向 snapshot 输出目录中那个 `<digest>.overlay`
  文件,并显式携带 `@sha256:<digest>` 或 `@hmac:<digest>`
- `overlay.base`(manifest:// 类):上传后的 overlay manifest key
- `from_refs` / `overlay.base_from_refs`:增量分层链(见 §3.5)。每项是
  `manifest://<key>` 或内容寻址的
  `file://<digest>.<ext>@<scheme>:<digest>`,可在一条链内混用(如 s1 本地文件、
  s2 已上传)。顺序严格自顶向下(新→旧)

**故意不存的字段**:

| 字段 | 为什么不存 |
|---|---|
| `sandbox.id` | 由 host 传入(`run --sandbox-id` 或 yaml) |
| `network.{tap\|tapfd,mac,ip,mtu,nexthop,hostname,interface}` | host-localized,restore 时由 sandbox.yaml 提供;无源表示无 NIC,有源时最多一个且须与 config.json 中的快照 NIC 拓扑一致;tapfd 元数据覆盖 mac/ip |
| `launch.{exec,args,env,workdir,restart}` | 应用启动配置在 guest 内存里已经反映为运行中进程,restore 后不再走 launch 协议 |
| `control.{cgroup_path,controller}` | host-localized 资源策略 |
| `overhead` / `watermark_high` / `startup` / `allocatable` | 同上,host 资源策略 |
| `boot.kernel` / `boot.cmdline` | restore 不重新 boot,kernel 在 snapshot 内存中 |
| `boot.root.overlay.diff` | host 本地写层路径,restore 时新建一个(空→落盘 base 目录) |
| `boot.root.overlay.diff_size` | 仅"创建空白 diff"时用;restore 的新 diff 尺寸取 base 大小,与之无关 |

**版本字段**:不引入显式 schema version。snapshot 是 ephemeral 资产
(host 重启即丢,跨主机复制只在调度场景),hard cut over;旧版 snapshot 解析
失败时报清晰错误"snapshot.cfg not found in <file>; produced by old sandbox-ctl?"。

restore 时 host sandbox.yaml 与 snapshot.cfg 的字段语义合并规则详见 §11.0。

### 3.5 增量分层快照

恢复时用的是**全新、惰性填充的 memfd**:运行期只有被缺页加载或写入的页才常驻。
因此再次保存时,`SparseCopy(memfd)` **天然**只捕获本次运行触碰过的页,其余是
空洞——**增量是惰性加载的副产物,无需脏页跟踪**。磁盘同理:blk1.diff 是稀疏
CoW diff,只有写过的块是数据。

代价是:这样产生的 s2.snapshot / s2.ext4 **不能独立提供完整数据**——未触碰区是
空洞,需穿透到父快照。这正是 `from_refs`(内存链)与 `overlay.base_from_refs`
(磁盘链)的用途:恢复时把本快照与其祖先叠加为一个分层视图。

```
                      offset →
   s3.snap(本快照) │ data │··· hole ···│ data │·· hole ··│   本次增量
   s2.snapshot      │ hole │ data │·· hole ··········│ data │   ↓ 穿透
   s1.snapshot      │ data │ data │ data │·· hole ···│ data │   ↓ 穿透
   ──────────────────────────────────────────────────────────
   有效内存          │ s3   │ s2   │ s1   │ ZEROPAGE │ s3/.. │   合并空洞→零页
```

叠加语义(自顶向下,顶层优先):某层有数据则用该层;**声明空洞穿透**到下层;
某层是零页(IsZero,内存为非常驻区、磁盘为未写块)亦视作该位置无内容、继续穿透;
**每层皆空洞 = 合并空洞**,内存恢复为 ZEROPAGE、磁盘读为零块。正确性:某页本次
运行未触碰 ⇒ 其内容 = 恢复起点内容 = 父快照内容(或零),故穿透严格正确。

**链的取舍**:不实现祖先 pin——某层缺失(被删/损坏)即视该快照**整体失效**,清晰
报错而非部分恢复。**远程链**不做通用折叠(compaction):`manifest://` 链能长到多深
就多深,深链恢复时每次缺页逐层查空洞(纯内存,无 RPC)直到命中层发一次取数;浅链
(典型 s1→s3)无感,长链自行承担读放大。平台若在意代次深度,自行控制再保存的次数。

#### 活动工作集提示与当前顶层 Prefetch

增量内存顶层还提供一个机会式的工作集提示:从父快照恢复时使用全新的稀疏 memfd,
本次运行中被 UFFD 换入或被 guest 写入,并在再次 snapshot 时仍驻留的页才出现在新
顶层;未触碰的页继续是 Hole 并穿透到 `from_refs`.因此 "S1 基线 → 恢复 S1 → 运行
代表性业务窗口 → 保存 S2" 得到的 S2 顶层,通常比完整逻辑内存小,并近似表达该窗口
激活的内存集合.它不记录访问频率或先后顺序,也不保证代表未来请求.

`restore.prefetch: memory` 可在恢复 S2 时机会式预热**当前内存顶层**:manifest
self 温热 chunk cache,file self 对已打开的 artifact FD 提交 `FADV_WILLNEED`.
多层快照的顶层经过上述筛选,是主要价值场景;`from_refs` 为空的单层快照在用户
显式启用后也允许 Prefetch,但其候选是全部已保存 resident memory,不宣称为活动
工作集.父内存层和 root/data disk 始终不在选择范围内.完整资格,执行边界和验证
方法见 §7.1.

#### 本地层不变量(memory 有限放宽,disk 保持单层)

`file://` 本地层与 portable 层(manifest/located file)可混用。disk chain 仍要求本地层至多一个且
必须是顶层;本地 disk top 每次保存都 flatten-merge,`base_from_refs` 不新增本地 ref。
memory chain 则允许 self 以下出现多个本地 `file://*.snapshot` ref,仅用于本地
working-set 生成与验证。这组 artifact 必须一起保留;缺任一层即整条快照失效。离线上传
后 root 展平列表中的本地 refs 会逐项改写成目标 portable refs。

- **默认本地导出 = 替换直接父 self(合并,非递归 compaction)**:若沙箱本身从**本地**
  `file://` 快照懒加载,
  再次 `snapshot --output` 时把本次稀疏增量**叠加合并**进父本地层(顶层逐页优先、父层
  穿透、两层皆空洞才穿透祖父),新 self **替换直接父 self**并原样继承父
  `from_refs`。已有 local lowers 不递归压平,因此默认 merge 只保证不增加既有本地深度,
  不保证整条 memory chain 的 local depth=1。依赖 artifact 生命周期仍由这组本地 bundle
  的使用者维护。(从**远程** `manifest://` 父恢复后导出,仍是「新顶层压在父 ref 之上」
  的增量分层——远程父不触发合并。)
- **工作集 opt-in**:从本地父恢复后使用
  `snapshot --output ... --drop-caches=false --merge-ref=false`,本次 memory self
  不合并父 memory,而是保留 `from_refs=[父]++父.from_refs`;root 与所有 data disk
  仍按上条规则 merge。第一版不复制依赖:创建前要求输出目录已包含每个本地 memory
  ref basename。`--merge-ref=false` 配合本地父不支持直接 `--upload`,应先本地输出
  再运行 `upload-snapshot`。
- **live upload 检查最终 memory chain**:无论 `merge-ref` 取值,只要计算后的
  `resultMemoryRefs` 仍包含本地 ref,就在 quiesce 前拒绝 `snapshot --upload`。可先
  `--output` 做本地验证,再用 `upload-snapshot` 递归发布整个 snapshot graph。
- **离线提升 `sandbox-ctl upload-snapshot <本地快照>`**(§2.7):重建 root graph,
  把 `from_refs` 的 local memory lowers 作为 opaque 工件发布,并升级当前 root/data
  disk 的 local leaves;已有 manifest/located refs 原样带过,最终打印 canonical
  portable root ref。历史 memory lower 内已被 flatten 掉的旧 disk graph 不再是依赖。

## 4. 资源模型

### 4.1 三种部署模式

是否启用 cgroup 限制、是否启用动态控制由 `control` 字段是否存在驱动:

| 模式 | 字段配置 | 适用场景 |
|---|---------|---------|
| **无 cgroup** | `control.cgroup_path` 未设 | 开发环境、调试、单租户独占节点 |
| **静态 cgroup** | `control.cgroup_path` 已设,`control.controller` 未设 | 生产但无超分需求;cgroup 隔离即可 |
| **动态控制** | `control.cgroup_path` 已设,`control.controller` 已设 | 生产高密度、多租户共节点 |

**能力对比**:

| 能力 | 无 cgroup | 静态 cgroup | 动态控制 |
|------|----------|------------|----------|
| cgroup PSI 反压 | 无 | ✓ memory.high | ✓(随 allocatable 动态) |
| balloon + deflate_on_oom | ✓ | ✓ | ✓ |
| CPU 公平共享 | ✗(allocatable.cpu == capacity.cpu) | ✓ cpu.weight | ✓ |
| 跨沙箱仲裁 | ✗ | ✗ | ✓ |
| 防惊群 | ✗ | ✗ | ✓ 节点限速 + emergency_pool |
| 创建期资源管控 | ✗ | ✗ | ✓ |
| 超分密度提升 | ✗ | 有限(allocatable 必须保守) | ✓ allocatable 可贴近真实工作集 |

切换模式通过加 / 减 sandbox.yaml 字段。`control.controller`(动态控制模式开关)
只能在 sandbox.yaml 里设;命令行只在 cgroup 维度给运维一个临时覆盖入口
(`--cgroup-path` 覆盖 cgroup_path;`--cgroup-adopt` 采纳 sandbox-ctl 自身所在的
cgroup,见 §2.2 / §9.2)。

### 4.2 资源量

每沙箱在每个维度(memory / cpu)上有三个量:

| 量 | 内存 | CPU | 含义 |
|----|------|-----|------|
| `capacity` | resources.capacity.memory | resources.capacity.cpu(整数 vCPU) | guest 看到的"声明规格";cgroup 上界 |
| `floor` | resources.allocatable.memory | resources.allocatable.cpu | 最小保留(K8s request 类比) |
| `allocatable_now` | balloon 与 cgroup memory.high 实时反映 | (CPU 维度无 allocatable_now) | 内存独有的运行时 budget,在 [floor, capacity] 浮动;**仅动态控制模式存在** |

CPU 通过 cpu.max + cpu.weight 静态表达;无 cgroup / 静态 cgroup 模式内存也
不浮动。详见 §6 / §7。

### 4.3 内存与 CPU 的不对称性

| 维度 | 内存 | CPU |
|------|------|-----|
| 物理强制机制 | balloon | cgroup cpu.max |
| 反压机制 | cgroup memory.high(PSI) | cgroup cpu.max(throttle) |
| 灾难性后果 | OOM kill | 仅减速,无 kill |
| 调整生效延迟 | balloon 数 ms-数十 ms | cpu.max 立即(下一 period) |
| 释放路径 | balloon inflate(host-driven via vm.resize)→ fallocate+madvise,异步 | 无需释放;period 边界自动 |
| 安全网 | deflate_on_oom | 不需要 |
| 是否有 burst 状态机 | 是 | 否 |

CPU 维度本质比内存简单——没有不可逆失败、调整即时、释放廉价。本设计利用
内核 cgroup v2 的 cpu.weight 公平共享语义,**让 CPU 控制完全静态化**,只有
内存有 burst/recover 状态机。

## 5. 冷启动数据流

### 5.1 时序

```
T0   sandbox-ctl run --config sandbox.yaml 启动
T1   解析 yaml(可选 --sandbox-id 覆盖)→ 构造完整 SandboxConfig
T2   tap 源验证已存在 TAP;tapfd 与无网络源跳过 TAP 名验证;
     随后准备 /run/sandbox/<sid>/ 目录
T3   准备 overlay diff:已存在→按 crypto.local policy 打开(绝不 truncate,忽略 template);
     不存在→从 diff_template 的逻辑 sparse view 初始化 / 按 base 大小新建 blank upper;
     新文件编码由 crypto.local 决定(详见 §3.2.2)
T4   动态控制模式:dial controller, send Admit, 收 grant 后继续(详见 §10)
T5   cgroup setup:写 cgroup limits + 把自身 PID 加入 cgroup.procs
     (后续 fork 的 CH 自然在同 cgroup)
T6   memory 准备(统一模型,冷启动 + 恢复同):
     T6a memfd_create("sandbox-<sid>-ram", MFD_CLOEXEC | MFD_ALLOW_SEALING)
     T6b ftruncate(memfd, ramSize)
     T6c F_ADD_SEALS: F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL
     T6d backendVA = mmap(NULL, ramSize, RW, MAP_SHARED, memfd, 0)
     T6e madvise(backendVA, ramSize, MADV_NOHUGEPAGE)
         (uffd 在 4 KiB 粒度操作,THP 把多页折成 2 MiB 大页会破坏 fault 路由)
     T6f addrMap = NewAddressMap(ramSize)(此时尚未 register VMA)
         pageStates 切片初始全 Absent,ramSize/4KiB 个元素
         snapshotReader = ZeroSource(冷启动 sentinel)
T7   构造 BlockReader for blk0(file 或 manifest)
T8   起 blk0 backend goroutine:listen /run/sandbox/<sid>/blk0.sock
T9   构造 BlockBaseReader for blk1.base(可能为空)
T10  起 blk1 backend goroutine:listen /run/sandbox/<sid>/blk1.sock
     blk1 backend 内部对 blk1.diff 做 SEEK_DATA 扫描重建 dirty bitmap
T11  读 boot.root.base 末尾 ZIP 拿 ImageConfig(缺 ZIP 软失败返回空)
     合并 ImageConfig 与 sandbox.yaml `launch:` → LaunchSpec
T12  起 launch server goroutine:listen /run/sandbox/<sid>/vsock.sock_5000
T13  起 va_report UDS server: listen /run/sandbox/<sid>/uffd.sock
     OnReady callback 内将 adopt uffd_C(从 SCM_RIGHTS)+ 起 epoll/worker
T14  起 ctl.sock UDS server: listen /run/sandbox/<sid>/ctl.sock(snapshot / exec 请求入口);
     Listen 成功且 cleanup 已注册后,若启用 --ready-fd 则写 control_ready
T15  构造 CH 命令行(详见 §5.2):
     `--memory-zone size=<ramSize>,shared=on,fd=3,uffd_socket=/run/sandbox/<sid>/uffd.sock`
     `--console tty --serial off`,cmdline `... console=hvc0`(内核 dmesg 走 hvc0)
     cmd.ExtraFiles = [memfd] 让 fd=3 在 CH 进程中可见;tapfd 模式再追加 tap fd,
     无网络源时不追加
     CH 进程 stdio:stdin = /dev/null(CH 因此不 raw 化任何宿主终端)、
     stdout = 一根匿名管道(承载 hvc0 dmesg,sandbox-ctl 按 `--console` 决定去向)、
     stderr = sandbox-ctl 的 stderr;cmd.SysProcAttr.Setpgid = true(CH 不在
     sandbox-ctl 控制终端的前台进程组)
T16  fork+exec cloud-hypervisor (patched),读 CH 的 stdout(dmesg 管道)+ 转发 stderr
     (tapfd 模式若交接带回 netns fd,fork/exec 在锁定线程 setns(CLONE_NEWNET) 进该
     netns 后进行 → CH 在 tap 所在 network namespace 内运行;见 §5.2)
T17  CH (patched) 启动:
     T17a 解析 --memory-zone fd=3 → 跳过 memfd_create,mmap 同一 inode → chVA
     T17b userfaultfd() → uffd_C(绑到 CH 的 mm)
          UFFDIO_API features = MISSING_SHMEM | EVENT_REMOVE | EVENT_UNMAP | THREAD_ID
          UFFDIO_REGISTER(uffd_C, [chVA, +ramSize], MISSING)
     T17c connect uffd_socket → sendmsg(va_report; SCM_RIGHTS=uffd_C) → 等 ack
     T17d memory_range_table 检测 user_managed → snapshot_memory_ranges 不含此 zone
     T17e 配 virtio-pmem(sandbox-runtime.bundle DAX)、virtio-vsock(cid=3)、
          可选 virtio-net(仅有网络源时)、virtio-balloon
     T17f vhost-user-blk 握手:SET_OWNER → SET_FEATURES → SET_MEM_TABLE [memfd fd]
          backend fstat 比对 inode = sandbox-ctl 启动时记下的 memfd inode
          → 复用 backendVA → 不再 mmap
     T17g boot vCPU(此时 sandbox-ctl 端 ack 已回,handler 已就绪)
T18  va_report server 收到 sendmsg:
     T18a recvmsg → 解出 uffd_C fd 和 va_report 内容
     T18b addrMap.RegisterVMA(ProcessCH, chVA, ramSize)
     T18c epoll_create1 → add uffd_C → 起 reader + N worker
     T18d 回 ack 给 CH
T19  Guest 内 kernel 启动 → mount /dev/pmem0 → exec /sbin/init = sandbox-init
     sandbox-init 三阶段(详见 sandbox-init.md §3):
     T19a phase 1 mount + overlay + chroot;AF_VSOCK bind+listen :5000
     T19b phase 2 dial host:5000 → hello → host 回 launch{spec, stdio} → guest 备好
          app stdio(tty: openpty / pipe: socketpair)→ launch_ack{stdio} → host 回 ack
          → **这条连接升级为 stdio MUX**;host 发一次初始 SET_WINSIZE,起 app stdio
          桥接,ping ticker start;guest fork/exec user app(app fd 0/1/2 = 伪终端从端
          或 pipe 子端)→ 短连接发 app_started{pid}→ host 成功写回 ACK→ 本次 cold run
          首次通知写 ready 并关闭 ready fd(后续原地 restart 不重复)
     T19c phase 3 supervisor + 反向 listener(ping/restore/quiesce/attach/exec)+ mem_report
T20  vCPU 跑过程中:
     · stdio MUX:STDIN / STDOUT / STDERR(或 PTY)+ WINDOW_UPDATE / SET_WINSIZE
       帧在 sandbox-ctl ↔ sandbox-init 间双向流动;MUX 因故断 → sandbox-ctl 拨新
       连接发 attach 重建(详见 sandbox-init.md §4.5 / §4.6)
     · vCPU 首次访问页 → uffd_C MISSING fault → handler 走 Absent → ZEROPAGE
     · backend 访问 backendVA → kernel 默认 shmem 缺页:folio 已存在(handler 装的)→
       直接装 sandbox-ctl mm PTE,无 uffd 事件
     · sandbox-init 周期(默认 5s)从 /proc/meminfo 读 MemAvailable/MemTotal,
       走短连接发 mem_report(sandbox-init.md §4.3)给 host
     · host BalloonController 按策略推 desired_balloon target(详见 §9.3),通过
       CH HTTP API PUT /api/v1/vm.resize 落到 guest;guest balloon 驱动 inflate
       → CH 在 memfd 上 fallocate(PUNCH_HOLE) + 在 chVA 上 madvise(DONTNEED)
       → uffd_C 投 EVENT_REMOVE → handler push 到 removeQ → flusher batch+merge
       后 madvise(DONTNEED, backendVA)
T21  user app 退出 → sandbox-init reboot → CH vCPU shutdown → CH 进程退出
T22  sandbox-ctl cmd.Wait() 返回
T23  cleanup:停 backend / 关 uffd / munmap / unlink sockets / 移除 cgroup
T24  sandbox-ctl 退出,exit code = guest 上报的 app_exited{code,term_signal}(应用退出码
     / 128+signal);拿不到时回退 CH exit code
```

**关于冷启动 uffd 开销**:每个首次访问页要走一次 uffd 往返,单次 ~5-10 µs。
典型 sandbox 工作集 ~50-200 MiB → 12K-50K 次 ZEROPAGE,累加 60-500 ms。
这部分**摊在 vCPU 运行过程**而不是 pre-boot,对端到端冷启动 P50 几乎无影响。

### 5.2 CH 命令行(冷启动)

```
cloud-hypervisor \
  --api-socket  /run/sandbox/<sid>/ch.sock \
  --kernel      /opt/sandbox/vmlinux \
  --pmem        file=/opt/sandbox/sandbox-runtime.bundle,discard_writes=on,iommu=off \
  --memory-zone size=8G,shared=on,fd=3,uffd_socket=/run/sandbox/<sid>/uffd.sock \
  --balloon     size=0[,deflate_on_oom=on] \
  --disk        vhost_user=on,socket=/run/sandbox/<sid>/blk0.sock,readonly=on \
  --disk        vhost_user=on,socket=/run/sandbox/<sid>/blk1.sock \
  --vsock       cid=3,socket=/run/sandbox/<sid>/vsock.sock \
  --net         fd=4,mac=<from tapfd>,id=_net0,iommu=off \
  --console     tty \
  --serial      off \
  --cmdline     "init=/sbin/init root=/dev/pmem0 ro rootfstype=erofs rootflags=dax=always
                 console=hvc0"

# --net: 无网络源时整项省略;tapfd 模式用 fd=<N>(memfd 之后继承的 fd,通常 fd=4)+
#        mac=<交接元数据>,id=_net0 供 restore 经 net_fds 重新绑定该网卡;
#        tap 名模式则 --net tap=<name>.
# tapfd 交接(docs/tapfd.md §3/§4):exec helper 时置 TAPFD_SOCKET + TAPFD_WANT_NETNS=1;
#        socket 模式发送 TAPFD/1 OPEN want_netns=1 ...。provider 的 tap 处于独立 netns
#        时回带该 fd,sandbox-ctl 据此在该 netns 内 fork/exec CH(T16);否则 CH 在 host netns 启动。
# CH 进程的 stdio(sandbox-ctl 设置):
#   stdin  = /dev/null            ← 关键:CH 的 --console tty 只在 stdin 是终端时才会
#                                     raw 化那个终端;接 /dev/null 故 CH 不碰任何终端
#   stdout = 匿名管道(os.Pipe)    ← 承载内核 dmesg(经 hvc0);sandbox-ctl 从读端拿到,
#                                     按 run --console 决定去向(§2.2)
#   stderr = sandbox-ctl 的 stderr ← CH 自己的 WARN
#   ExtraFiles[0] = memfd → CH 见 fd=3(--memory-zone fd=3)
```

要点:
- `--memory-zone size=8G` 是 capacity;balloon boot 时 `size=0`,sandbox-ctl
  端的 BalloonController 在 settled 后通过 `/api/v1/vm.resize` 把 target 推到
  `capacity − allocatable_now`,过程受 mem_report 驱动的反馈策略约束(§9.3)
- balloon 行只在 `allocatable_now < capacity` 时出现;两者相等时省略
  balloon 设备,无 host 端 RAM 回收路径
- `--memory-zone fd=3,uffd_socket=...`:patched CH 跳过 memfd_create,直接用
  sandbox-ctl 传入的 fd 当 backing;在 create_ram_region 内自己创建 uffd,
  通过 uffd_socket 发 va_report + SCM_RIGHTS,等 sandbox-ctl ack 后才允许
  vCPU 跑(详见 `sandboxer/docs/cloud-hypervisor.md`)
- `--pmem discard_writes=on` 让 guest 写 pmem 不影响 host 文件
- blk0 readonly=on 在 vhost-user 协议层告知 guest 这是只读盘
- `--vsock cid=3,socket=...`:CH 创建 virtio-vsock 设备,guest CID=3,通过 hybrid
  代理把 guest port 5000 流量映射到 host UDS。承载控制面短连接(launch / ping /
  app_started / app_exited / mem_report / quiesce / restore / attach),以及
  launch / restore / attach 那条连接握手后升级而成的应用 stdio MUX(详见
  [`sandbox-init.md`](sandbox-init.md) §4)
- `--console tty`:CH 把 guest 的 virtio-console(hvc0)接到 CH 进程自身的 stdout。
  sandbox-ctl 给 CH 的 stdout 是一根匿名管道,从读端拿到内核 dmesg 流,按
  `run --console` 决定去向(§2.2)。CH 进程的 **stdin = /dev/null** 是关键——CH 的
  `--console tty` 只在它自己的 stdin 是终端时才会 raw 化那个终端;stdin=/dev/null
  故 CH 不碰任何终端(此前 `--tty` 路径下出现的"宿主 ^C 失灵"正是因为终端被 raw 化)。
  CH 的 stderr = sandbox-ctl 的 stderr(CH 自己的 WARN)
- `--serial off`:没有 8250 UART;内核控制台的唯一出口是 hvc0。应用的
  stdin/stdout/stderr 不经此路,走 vsock MUX(详见
  [`sandbox-init.md`](sandbox-init.md) §3.5 / §4.5)
- cmdline 中的 `console=hvc0` 由平台自动注入(§3.1),内核 dmesg 始终走 hvc0

## 6. snapshot 数据流

### 6.1 输出文件命名与字节布局

**输出目录布局**(`--output <out_dir>` 模式):

```
<out_dir>/
├── <digest>.snapshot                     # tarstream 工件(条目 "snapshot" = [内存][ZIP])
├── <sid>.snapshot → <digest>.snapshot    # 符号链接:按 sid 寻址的"最新"指针
└── <digest>.overlay                      # tarstream 工件(条目 "overlay" = ext4 diff)
```

snapshot 与 overlay 都**按内容摘要命名**(content-addressed),彼此不覆盖,故可作为
`from_refs` / `overlay.base_from_refs` 链里**稳定、可校验**的父引用(file 模式)——
同一 sid 的 s1/s2/s3 名字各异,链才能成立。`<sid>.snapshot` 符号链接指向本次产出的
`<digest>.snapshot`,给人和工具一个按 sid 寻址的"最新"入口
(`<sid>` = `sandbox-ctl run --sandbox-id` 设的或 yaml 里的)。

**工件容器 = tarstream**(`accelerator/pkg/tarstream`,GNU PAX sparse payload +
空 digest marker):逻辑视图的洞进信封洞图,线上只有数据字节。工件文件本身**致密**
——`cp`/`rsync`/非稀疏文件系统都不再能破坏语义,洞的权威从此是信封而非 OS。

**内容 identity `<scheme>:<digest>`** 由 §3.2.1 定义。`off` 使用 plaintext
`sha256`;`auto|required` 使用 customerKey-bound `hmac`,并把完整 canonical
tarstream 写入固定 4 KiB record 的 AES-SIV envelope。两种路径都只读驻留数据 +
ZIP 段,8 GiB 镜像里的零页不读不写;加密路径不生成 plaintext staging。

**`snapshot` 条目逻辑布局**(信封内的逻辑视图;洞在信封图里):

```
逻辑偏移 [0, ramSize)        memfd 内容(SEEK_DATA/HOLE 捕获:零页是洞,
                              进信封洞图,不占线上字节)
逻辑偏移 [ramSize, 条目末)   标准 ZIP archive
                                / config.json     CH 设备拓扑 + memory layout
                                / state.json      vCPU 寄存器、virtio queue、IRQ
                                / snapshot.cfg    §3.4 schema
```

`ramSize` 是 zone 的逻辑大小;ZIP append 在该逻辑偏移之后。依赖 ZIP 中央目录
(End of Central Directory)在文件末尾的特性:

- ZIP 解析器调 `archive/zip.NewReader(ReaderAt, size)`,从末尾扫 EOCD,定位
  中央目录,再定位每个 entry 的 local header
- ZIP 内部 offset 都相对 ZIP 起点,NewReader 不需要知道 ZIP 起点——从末尾
  倒推
- 因此 prefix 长度(`ramSize`)不影响 ZIP 解析;memory 区域 + ZIP 共存于一个
  逻辑条目(读取经 `fetch.OpenTarStream` 的 Stream + `NewReaderAt`)

**关键**:工件文件物理大小 ≈ 驻留页数 × 4 KiB + ZIP 字节 + 信封开销。一个
8 GiB sandbox 实际驻留 200 MiB → 文件 ~200 MiB,且这一性质**不依赖文件系统稀疏
支持**。`manifest.Ingester` 摄取时洞图来自信封(进 manifest 洞表,`sparse.Extent`),
manifest key 与容器无关(= f(逻辑内容, 洞图))。

**单 zone 假设**:限定单 memory zone(固定 spec,见 §14.1)。

**`<digest>.overlay` / `<digest>.snapshot` 写入路径**(pack-while-hash):

file 模式下 overlay 与 snapshot bundle 走同一条落盘路径:

1. quiesce 完成后源内容稳定。snapshot memory 源是 memfd,洞图经
   SEEK_DATA/HOLE 取得;overlay 源由 live `BlockCOW.SnapshotView()` 提供,
   dirty bitmap 生成 upper-only 洞图,dirty block 读取 plaintext diff,clean block
   防御性读零。active diff path 只用于日志、统计和清理,不会作为 snapshot 数据重开;
   工件落定后洞的权威归 tarstream 信封
2. `tarstream.WriteTo` 把源直接写到同目录 `<sid>.<ext>.partial`,单遍计算 inner
   plaintext digest;只有 data extent 上线,洞进信封图;snapshot 的 ZIP 段拼接在
   `ramSize` 逻辑偏移后。启用 codec 时 writer 在同一遍直接输出 encrypted v1,
   不落 plaintext 中间件
3. `.partial` 经 `Sync` + `Close` 后以 no-replace 原子提交为
   `<out_dir>/<digest>.<ext>`,再同步父目录;snapshot 另建/更新
   `<sid>.snapshot` 符号链接指向它。终态已存在时顺序完整验证 scheme、digest、
   marker、trailer、outer EOF 和 logical size,一致才复用

`.partial` 是同目录瞬态名,落定后输出目录只见内容寻址的终态文件。同一沙箱多次
snapshot 内容不变时 identity 相同 → 验证后复用**同名文件**,绝不覆盖不一致终态。
整条路径只读源的数据 extent + ZIP 段,8 GiB / 200 MiB 驻留的沙箱只触约 200 MiB。

### 6.2 snapshot 时序

时序原则:
- **overlay 在 memory 之前完整处理**(包括可能的 ingest)→ snapshot.cfg 拿到
  overlay.base 引用一次写入 ZIP,不需要事后回填重写
- **config.json + state.json + snapshot.cfg 全程在内存暂存**,只在最末把 ZIP
  一次性 append
- staging 目录(`<run-dir>/<sid>/snap-stage`,tmpfs)只容纳 CH 产出的
  config.json/state.json(KB 级);GiB 级 memory 段与 overlay 不经此目录——
  --output 直接落 `<out_dir>`(写 `.partial` 再 no-replace commit),--upload 流式喂 ingest
- pause 窗口 = quiesce + CH dump + overlay export + memory dump + zip append。
  overlay + memory 写都是 SEEK_DATA/HOLE 驱动的数据 extent 打包,稀疏 sandbox
  8 GiB → 驻留 200 MiB → ~100 ms

```
T0  sandbox-ctl snapshot --sandbox-id <sid> [--output <out_dir>] [--upload]
        [--drop-caches=true|false] [--merge-ref=true|false]
T1  通过 <run-dir>/<sid>/ctl.sock 联系目标 sandbox-ctl run 进程
T2  目标进程串行:
    T2a 通过 vsock 短连接发 quiesce 给 sandbox-init,等 quiesced 响应。sandbox-init
        收到后:拒绝新的 exec 并 SIGKILL 在飞的 exec 子进程(快照不能带运行中的
        exec 兄弟进程;沙箱 resume/restore 后解除)→ **freeze 应用进程树**
        (cgroup.freeze=1,等 cgroup.events 至 frozen 1)→ sync + optional drop_caches →
        停读应用 stdout/stderr(pty master)→ 拆除所有 connect 端口转发中继(SO_LINGER
        确认拆除,sandbox-init.md §3.7)→ 在 stdio
        MUX 上发起优雅关闭握手(sandbox-ctl 的 MUX 端响应 MUX_CLOSE_ACK 并读到 EOF
        确认 MUX 已彻底关闭)→ 回 quiesced。quiesced 一回来即表示"应用已冻结、MUX
        与转发已关、guest 干净态",可继续 T2b;deadline(见 sandbox-init.md §4.10)内未
        收到 quiesced(含 freeze 在有界等待内未确认 frozen)→ 视为协议失败,**放弃
        此次 snapshot**(绝不带半冻结/半开 MUX 快照),sandbox 继续运行
    T2b CH /vm.pause:vCPU 暂停,virtio 设备 quiesce
    T2c srv0.Quiesce() + srv1.Quiesce()(vhost-user-blk backend 排空 inflight)
T3  CH /vm.snapshot { destination_url=file://<run-dir>/<sid>/snap-stage/ }
    patched CH 在该目录写(memory-ranges 自动跳过):
      config.json   - VM 配置(devices, memory layout, ...)
      state.json    - vCPU 寄存器、virtio queue 状态、IRQ 等
    sandbox-ctl 把这两个文件读进内存作为 ZIP 内容暂存,不再落盘
T4  overlay → sink(在 memory 前处理,snapshot.cfg 才能拿到终态 overlay.base):
    若 --output:BlockCOW upper-only SnapshotView 的 dirty extent 打包 tarstream →
        <sid>.overlay.partial →
        writer 返回 scheme + digest(§6.1)→ no-replace commit
        <out_dir>/<digest>.overlay;
        overlay_ref = file://<digest>.overlay@<scheme>:<digest>
    若 --upload:同一 SnapshotView 数据 extent 流式喂 manifest.Ingester(空洞编码进
        manifest,不落盘)→ overlay_manifest_key;
        overlay_ref = manifest://<overlay_manifest_key>
T5  生成最终 snapshot.cfg(在内存中,§3.4 schema):
    resources.capacity:        从 sandbox 当前 SandboxConfig
    boot.runtime_ref:           file://<basename>@sha256:<digest>(从 bundle marker
                                读取)或 manifest://<key>(原引用)
    boot.root.base_ref:         同上规则
    boot.root.overlay.base:     T4 的 overlay_ref(本次 diff = 磁盘链顶)
    from_refs / overlay.base_from_refs:  增量分层链(§3.5)。冷启动 = [];否则按
                                运行进程持有的 provenance(恢复时记下的"从何而来")
                                计算:默认本地 memory merge 只替换直接父 self,
                                from_refs=父.from_refs(已有 local lowers 原样保留);
                                `--merge-ref=false` 时
                                from_refs=[父快照 ref]++父.from_refs。
                                每块 root/data disk 分别计算 base_from_refs:若本地
                                parent disk top 已实际 merge(diskMerged[i]=true),则
                                base_from_refs=父.base_from_refs,删除已吸收的父 top;
                                若 parent disk 为 portable 或未 merge,则
                                base_from_refs=[父 top]++父.base_from_refs。
                                子快照只引用父(其 ref 在恢复时已知),无鸡生蛋
T6  生成 snapshot 内容:
    [memory 段]  ramSize 字节,SEEK_DATA/HOLE 驱动的稀疏数据
    [ZIP 段]     从 T3/T5 内存副本一次性写出 config.json / state.json /
                  snapshot.cfg(三个 entries)
    若 --upload:走流式构造,直接 io.Reader 喂 ingest,不落盘;
                  stdout 输出 snapshot_manifest_key(= 链中本快照的内容寻址名)
    若 --output:先写到 <sid>.snapshot.partial → writer 返回 scheme + digest(§6.1)→ no-replace commit
                  <out_dir>/<digest>.snapshot,再建/更新符号链接
                  <out_dir>/<sid>.snapshot → <digest>.snapshot
T7  srv0.Resume() + srv1.Resume()
T8  resume_after=true:CH /vm.resume,沙箱原地续跑;quiesce 时 guest 冻结了
                  应用并关了 stdio MUX,这里 sandbox-ctl 拨新连接发 attach 重建
                  MUX(attach_ack → per-stream window 重协商、winsize 重发、续传
                  残留 + 应用 stdio)。attach 只管 MUX 传输、不等同"快照后 resume";
                  guest 因仍处 quiesce 冻结态(attach 是其首个 post-resume 接触)
                  据自身冻结状态补做 thaw——属 quiesce 生命周期而非 attach 语义,
                  见 sandbox-init.md §4.3 / §4.6
    resume_after=false(默认):CH /vm.shutdown,等 CH 退出 → sandbox-ctl run
                  进程也退出
T9  ctl.sock 回 snapshot_done
T10 sandbox-ctl snapshot(发起方进程)收到 done:
    若 --upload:stdout 输出 snapshot_manifest_key
    否则:本地产物在 <out_dir>/(<digest>.snapshot + <sid>.snapshot 符号链接 + <digest>.overlay)
```

**关键差异 vs 一般 VMM 快照**:
- patched CH **不写** memory-ranges
- sandbox-ctl 持有 memfd 自己 sparse 拷贝,**不经 CH→file→sandbox-ctl 的中转**
- ZIP 内 snapshot.cfg 在 T5 一次写入即终态,**没有"事后回填重写 ZIP"步骤**
- 8 GiB sandbox / 200 MiB 驻留:物理 I/O ~200 MiB(CH 通用快照路径 ~24 GiB),
  延迟 ~1 s 量级
- overlay 文件名内嵌 policy 选择的 digest(content-addressable),同沙箱多次 snapshot 内容
  不变时自动同名

### 6.3 ctl.sock 协议

`<run-dir>/<sid>/ctl.sock` 是 sandbox-ctl run 进程在 §5 启动时建立的 host-local
UDS,承载两类宿主侧控制请求:`snapshot`(一问一答)与 `exec`(握手后该连接升级
为端到端 stdio MUX)。请求 / 响应都是 JSON,长度前缀(4 字节 LE uint32)+ payload。
同一 UDS 上多个请求各自独立的连接、可并发(每连接一 goroutine)。

**远程授权 exec 入口**:`pkg/ctl.ProxyExec(ctx, downstream, ctlConn)` 是
HTTP 无关的 server-side gate/relay。调用方负责在调用前完成用户鉴权、
目标 sandbox 选择与生命周期准备,并将 `ctlConn` 连到该 sandbox 的
`<run-dir>/<sid>/ctl.sock`;`ProxyExec` 本身不解析身份或签发凭据。

```text
authorized downstream                       existing sandbox path
        │                                             │
        ▼                                             ▼
node proxy ──► ProxyExec ──► ctl.sock ──► sandbox-ctl run ──► guest exec
                │
                ├─ first frame: exec_request only
                └─ accepted: transparent ctl/MUX relay
```

gate 按现有 `ctl.sock` framing 先完整读取 4-byte LE 长度和 payload:

- payload 上限与其他 ctl 消息一致,为 64 KiB;长度前缀或 payload 截断、
  超限或非法 JSON 都拒绝。
- 只接受首帧对象的 `type == "exec_request"`;`snapshot_request` 及其他类型
  不得向 `ctlConn` 写入任何字节。
- 验证通过后,原长度前缀和原 JSON payload 逐字节写入 `ctlConn`,不做
  decode/re-encode;已跟在首帧后的 buffered ctl/MUX 字节也不丢失。

首帧通过后,两个方向使用有界 buffer 透明复制。一侧正常 EOF 时,如目标
支持 `CloseWrite`,只传播写侧半关闭,继续排空反向数据;若流不支持
`CloseWrite`,则不以全关闭代替半关闭。任一方向 I/O 失败或 `ctx` 取消时
关闭两条流,等待两个复制方向退出后返回。`ProxyExec` 从调用开始即取得
`downstream` 和 `ctlConn` 的所有权;包括参数错误、gate 拒绝、转发失败和
取消在内,所有返回路径都关闭两个非 nil 流。

**snapshot_request**(snapshot 子命令 → sandbox-ctl run):

| 字段 | 类型 | 说明 |
|---|---|---|
| `type` | string | `"snapshot_request"` |
| `out_dir` | string | `--output` 目录;snapshot 在该目录写 `<digest>.snapshot`(+ `<sid>.snapshot` 符号链接)+ `<digest>.overlay`;与 `upload` 互斥 |
| `upload` | bool | true 时走流式 ingest 到 manifest store;与 `out_dir` 互斥 |
| `resume_after` | bool | 默认 false(零值即销毁);CLI 默认与之一致。`--resume` 触发 true |

响应 `snapshot_done`:

```json
{
  "type": "snapshot_done",
  "memory_size":         8589934592,
  "memory_resident":     209715200,
  "wallclock_pause_ms":  12,
  "wallclock_dump_ms":   840,
  "overlay_ref":         "file://<digest>.overlay@<scheme>:<digest>" 或 "manifest://<key>",
  "snapshot_key":        "<hex>"   // 仅 upload 模式
}
```

**exec_request**(exec 子命令 → sandbox-ctl run):

| 字段 | 类型 | 说明 |
|---|---|---|
| `type` | string | `"exec_request"` |
| `exec` | object | `{ argv:[...], env:{K:V}, cwd, user, stdio }`——要执行的命令与协商的 stdio(对应 §2.4 的 flag;user 在 app ns 内按镜像 /etc/passwd 解析) |

run 进程收到后向 guest 反向通道发 `exec` 操作(见
[`sandbox-init.md`](sandbox-init.md) §4.3),拿到 guest 实际建立的 stdio
规格后回 `exec_ack`(`{ "type":"exec_ack", "stdio":{...} }`),**此后该 ctl.sock
连接不再收发 JSON,而是被 run 进程透明转发为 `sandbox-ctl exec` ↔ guest 之间
的端到端 stdio MUX**(含 EXIT_STATUS 帧与优雅 MUX_CLOSE,详见
[`sandbox-init.md`](sandbox-init.md) §4.5 / §4.6)。

任一请求出错回 `{"type": "error", "msg": "<reason>"}`:snapshot 不可达状态
(CH 已退出 / 配置不一致 / out_dir+upload 都给或都没给)、或 exec 被拒
(沙箱正在 quiesce / argv 为空 / guest 起进程失败)。

## 7. 恢复数据流(`run --restore=`)

恢复模式与冷启动共用同一进程入口(`sandbox-ctl run`),只是带 `--restore=<ref>`
flag。复用冷启动 §5.1 的 T6-T18 整段(memfd 准备 → backend 起 → CH 启动 →
va_report → uffd_C 就绪);区别:

1. 配置来源是 `<sid>.snapshot` 末尾 ZIP 内嵌的 config.json / state.json /
   snapshot.cfg,与 host sandbox.yaml 按 §11.0 规则合并
2. snapshotReader 是 `StreamSnapshotSource`(统一形态,不是 `ZeroSource`):它包裹一个
   读取层 `Stream`——file:// 与 manifest:// 都先解析为 `Stream`(分别是稀疏文件流、
   chunk 粒度的 manifest 流),再按 `from_refs` 叠成分层流(§3.5)。本快照内存段是
   链顶,空洞穿透到祖先,合并空洞才 ZEROPAGE
3. CH 命令行带 `--restore source_url=...`,不带 `--kernel` / `--vsock`
4. config.json 内捕获了原 run 的 paths(uffd_socket / blk0.sock / blk1.sock /
   vsock.sock),restore 前必须**重写为本次 run 的 paths**(基于 host
   `<run-dir>`)
5. `restore.prefetch: memory` 可为当前内存 self 启动后端相关的机会式后台 Prefetch(§7.1);
   它不改变 snapshotReader,UFFD demand 或恢复正确性

```
T0  sandbox-ctl run --restore <ref> --config sandbox.yaml [--run-root <dir>] ...;
    解析 restore.prefetch,非法值在 cgroup/目录/远程读取等副作用前失败
T1  解析其余 host sandbox.yaml(本地化字段:可选 network、overlay.diff、cgroup/控制器等)
T2  打开 <ref>:
    file path: os.Open + Stat → ReaderAt
    manifest://: 通过 store + cache 客户端取 manifest → 解封 → fetch.Fetcher 包成 ReaderAt
T3  archive/zip.NewReader(ReaderAt, totalSize) → 解出 config.json / state.json /
    snapshot.cfg。解析 from_refs / overlay.base_from_refs(§3.5),逐项解析为
    Stream(file:// 校验摘要、manifest:// 内容自校验),校验全链 capacity 一致。
    root snapshot.cfg 的展平 from_refs 列表是 memory chain 的唯一权威;preflight
    把每项作为 opaque memory layer 打开验证,不解析 lower 内历史 snapshot.cfg。
    file 模式:若 <ref> 是 <sid>.snapshot 符号链接,follow 解析出真实
    <digest>.snapshot 名,读取工件取得实际 scheme + digest,作为本次的内容寻址
    self ref(供将来再保存时写入子快照
    from_refs);from_refs 各项在该 .snapshot 同目录定位
T4  restore.ApplyRules(host sandbox.yaml, snapshot.cfg, snapshotPath):
    - 验证 capacity 一致(host 提供时)
    - 验证 boot.runtime / boot.root.base 协议 + basename 匹配(host 提供时)
    - runtime 从 bundle 尾部 ZIP、base 从 tarstream identity 读取 scheme + digest 并与 ref 比较;
      全程不读取 payload
    - 未提供时从 snapshot.cfg 复制(file 模式解析为 .snapshot 同目录文件)
    - 详见 §11.0 字段语义表 + §13 校验矩阵
    restore.Run 随后读取 config.json.net,要求快照 NIC 拓扑与 host network source
    是否存在一致;restore 不允许新增或删除 NIC
T5  从 snapshot.cfg 拿 capacity 推 ramSize;从 state.json 解出 balloon 状态推
    allocatable_at_snapshot(详见 §11.1)
T6  动态控制模式:Admit{floor, allocatable_at_snapshot} → grant
T7  cgroup setup + blk1.diff(全新)准备
T8  state.json 直接写到 <run-dir>/<sid>/snap-state/
    config.json 经路径重写后写入(uffd_socket / blk0/1.sock / vsock.sock 都改为
    本次 <run-dir>/<sid>/ 下的对应名)
T9  memory 准备:同冷启动 §5.1 T6,**唯一差别** snapshotReader =
    StreamSnapshotSource(包裹 [本快照内存段] ++ from_refs 叠成的分层流)。
    最终 memory source 构造成功后,若满足 §7.1 资格则异步 Prefetch 当前
    selfStream;不等待其完成,继续后续磁盘与 VM 恢复准备.
    blk0 / blk1 base 同理:overlay.base 与 base_from_refs 叠成分层只读基座,
    其上新建本次 blk1.diff(CoW)。provenance(父 ref + 两条链)前向传给本运行
    进程,供其将来再保存时算链(T5)
    联网快照的 tapfd 模式在此重新交接并取得新 fd;tap 名模式由 CH 按快照配置
    重开;无 NIC 时两者均跳过
T10 blk0 + blk1 backend 起;launch server UDS(<vsock-base>_5000)同样起——restore
    与冷启动共用同一后半段(memfd/uffd/blk/launch/pinger/ctl/信号/stats),仅 uffd
    source、CH 命令行、settle 协议不同。**差别**仅在于 restore 不走 hello/launch 握手
    (应用已在跑,该连接不升级 MUX),但 launch server 仍承接 guest→host 的周期
    mem_report 与 app_exited 短连接(host 端 BalloonController 据此调 balloon);
    va_report UDS server 起,OnReady 内 adopt CH 送来的 uffd_C
T11 spawn cloud-hypervisor (patched):
      --api-socket <run-dir>/<sid>/ch.sock
      --memory-zone size=<ramSize>,shared=on,fd=3,uffd_socket=<run-dir>/<sid>/uffd.sock
      --restore source_url=<run-dir>/<sid>/snap-state/
      --console tty --serial off
    有 tapfd 的联网快照在 restore 参数追加 net_fds=[_net0@[4]];
    无 NIC 快照不追加 net_fds
    CH 进程 stdio 同冷启动(§5.2):stdin=/dev/null、stdout=匿名管道(dmesg)、
    stderr=sandbox-ctl stderr;Setpgid(CH 不在前台进程组)
T12 CH (patched) 启动:同冷启动 T17a-T17c(创建 uffd_C,sendmsg va_report);
    fill_saved_regions 看到 user_managed zone 不在 ranges 表,自然空操作
    CH 进入 paused 状态
T13 sandbox-ctl 在 va_report 收到 sendmsg 后:
    addrMap.RegisterVMA(ProcessCH, chVA),起 epoll(uffd_C)+ worker pool,回 ack
T14 sandbox-ctl 调 PUT /api/v1/vm.resume → vCPU 从 snapshot 时刻继续
    首访 RAM → fault → handler Absent 分支 → snapshotReader.ReadAt → UFFDIO_COPY。
    分层流内部:命中本快照 / 某祖先则取该层数据;合并空洞 → UFFDIO_ZEROPAGE(无取数)
T15 vsock 连接发 restore{epoch=N, wallclock_ns} 给 sandbox-init(guest:5000 listener
     跨快照保留),等 restore_ack{stdio, app_state} 响应作为 guest agent ready 信号
     (deadline = `timeouts.restore`,默认不强制,§3.1)。CH 把快照里的
     CLOCK_REALTIME 原样载回,guest 墙钟落后
     整个静置区间;sandbox-init 收到 restore 后先 clock_settime 把 CLOCK_REALTIME
     跳到 wallclock_ns(host 发送前一刻的墙钟,残留传播偏差亚毫秒),再回 ack——
     应用解除阻塞前墙钟已纠正。单调时钟不受影响(Go 定时器、ping RTT、mem_report
     ticker 照常)。restore_ack 是 attach_ack 的超集(含 channel 集合 + 应用状态)
     外加"恢复完成"信号。**这条连接随后升级为新的 stdio MUX**:host 发一次初始
     SET_WINSIZE,重建应用 stdio 桥接,per-stream window 重新协商,残留字节回放,
     host 成功建立 MUX 后,本次 restore run 写 ready 并关闭 ready fd,随后 host
     (re)start ping ticker。guest 在写 ACK 后自行 reattach/thaw;本 ready 不确认其
     thaw 完成。deadline 到点未收到 restore_ack →
     restore 失败回退:CH /vm.shutdown 并向调用方报错
T16 vCPU 跑,fault 流转见 §8 uffd handler;balloon EVENT_REMOVE 同冷启动
T17 user app 退出 / 接收外部信号 → 退出流程同冷启动
```

### 7.1 当前内存 self Prefetch

Prefetch 是默认关闭的后端相关 warm-up 策略,不是恢复前置条件.只有字面值
`restore.prefetch: memory` 才提出请求;空值和 `off` 都关闭.其他值是配置错误,
必须在 `restore.Run` 产生 cgroup,目录或远程读取等副作用前拒绝.该字段属于本次
host restore policy,不写入或继承自 `snapshot.cfg`;冷启动不执行 Prefetch.默认
示例保持 `off`,需要启用时可把
[`examples/restore-prefetch-memory.yaml`](../examples/restore-prefetch-memory.yaml)
仅合并到目标 sandbox 的恢复配置中.

启动资格为:

```text
restore.prefetch == "memory"
AND final layered memory source was built successfully
AND current selfStream implements fetch.Prefetcher
```

`from_refs` 非空不是门槛.单层快照显式启用后同样执行,但候选是全部已保存 resident
memory,不宣称为活动工作集.多层快照只预热当前 self,该层更接近一次代表性业务窗口
的活动工作集,通常是收益更明确的场景.父层始终由 `from_refs` 显式表达;统一 ref
parser 只接受单个 manifest key,复合 `manifest://k1:k2` ref 在打开 Stream 前失败,
不进入 Prefetch 资格判断.

sandboxer 只把当前 `selfStream` 交给 task,并统一调用 `Prefetch(ctx)`,不对最终
`fetch.NewLayered([self] ++ from_refs...)` 调用 Prefetch.因此 `from_refs` 中的父
内存层保持零 Prefetch;root/data disk,EROFS 和 CoW 也不参与.后端行为为:

- manifest self:预热当前 Stream 的全部可见 Data chunks;manifest key 只写日志,
  不作为 selector,单-key合法性已由统一 parser 保证;
- file self:对 `OpenTarStream` 已打开的同一个 FD 的完整 physical artifact range
  提交一次 `FADV_WILLNEED`;范围来自 `f.Stat().Size()`,不是逻辑 `Stream.Size()`.

file advice 只表示 syscall 成功,不表示整个 artifact 已驻留,也不 pin page cache.
`OpenTarStream` 只暴露 capability,不会自动 Prefetch,所以 memory parents、disk
artifacts 和离线 inspect/upload 不会因 open 产生投机 I/O.两种后端都不增加 range
API;snapshot bundle 的 ZIP/tar 尾部也在当前 self 的 whole-bundle 预热边界内.

任务在最终内存 source 成功构造后启动 goroutine,立即让恢复主流程继续.它不阻塞
`/vm.resume` 或 `restore_ack`,可以与磁盘重建,网络准备,CH restore 以及恢复后的
首批 UFFD demand 重叠.Prefetch 失败采用 fail-open:只结束后台任务并记录日志,
不取消 restore,不得让 `restore.Run` 返回错误;后续缺页仍通过原有 demand ReadAt
完成校验,解密和 `UFFDIO_COPY`.不重试,不 pin,不在 sandboxer 增加 chunk 去重,
singleflight 或独立调度器.

生命周期必须满足:

```text
restore 退出或失败
    ↓
cancel Prefetch context
    ↓
wait Prefetch 返回
    ↓
Close from_refs / selfStream
    ↓
restore.Run 返回;caller 才能 Close shared Fetcher
```

不能用 "等待超时后遗留 goroutine" 的方式绕过取消.accelerator Stream/Fetcher 与
Prefetch 不能并发 Close;任务句柄的 `Stop` 必须幂等并完成 cancel + wait.

默认关闭时日志保持安静.显式启用时日志区分后端与完成语义:

| 状态 | 日志语义 |
|---|---|
| 不满足资格 | `skipped reason=no_capability backend=file\|manifest parent_layers=N` |
| 开始 | `started backend=file\|manifest parent_layers=N [key=...]` |
| file advice 已提交 | `advised backend=file ... duration=...` |
| manifest cache fill 已完成 | `completed backend=manifest ... key=... duration=...` |
| 失败降级 | `failed backend=... duration=... error=... restore_continues=on_demand` |
| 生命周期取消 | `canceled backend=... duration=...` |

收益只能通过请求级 A/B 验证.远程组应固定 cache 初始状态和容量、网络/store 条件;
本地组应固定同一个 physical artifact 并尽量统一宿主 page-cache 初始状态.两组都
固定 S1/S2 工件、代表性业务脚本和 VM 配置,随机化顺序并重复采样,比较 restore
ack、应用 ready、首个代表性请求、UFFD demand 延迟与读取量及额外 I/O.
`/vm.resume` 和 `restore_ack` 不以 Prefetch 完成为屏障;单次 benchmark、manifest
`completed` 或 file `advised` 日志都不能作为默认开启依据.

CH patched 在 create_ram_region 严格按以下顺序确保 va_report ordering:

```
1. addr = mmap(NULL, ramSize, ..., fd=memfd_fd, 0)
2. uffd_C = userfaultfd();UFFDIO_API;UFFDIO_REGISTER(uffd_C, [addr, +ramSize], MISSING)
3. sendmsg(va_report{addr, ramSize}; SCM_RIGHTS=uffd_C fd) → wait_ack
4. return addr
```

为什么必须 register 在 send 之前:第 2 步之后,任何对 [addr, +ramSize) 的访问
都会触发 fault → 路由到 uffd_C → sandbox-ctl handler。但 handler 此刻还没拿到
fd 也没在 addrMap 里 register chVA,会丢事件。

ack 之前 sandbox-ctl 必须完成:recvmsg → uffd_C fd → addrMap.RegisterVMA →
epoll_create1 + add uffd_C + 起 worker pool,然后才 ack。

## 8. uffd handler

冷启动 + 恢复**统一存在**。架构核心是**单 uffd 模型**:CH 进程内创建 uffd
注册 chVA,通过 SCM_RIGHTS 把 fd 传给 sandbox-ctl 的 handler;sandbox-ctl 自己
mmap 得到的 backendVA **不**单独注册 uffd,kernel 直接走 shmem 缺页路径。

### 8.1 单 uffd 为何足够

典型 cold-start / restore 顺序是 vCPU 先触碰 RAM(kernel boot 扫内存、
sandbox-init 起来、vCPU 走 guest kernel boot)→ chVA fault → uffd_C fire →
handler 通过 UFFDIO_COPY/ZEROPAGE **同时**把 folio 装进 shmem inode + chVA
装 PTE。之后 vhost backend 第一次 memcpy 到 backendVA[N] 时:

```
kernel 缺页处理:
  - sandbox-ctl mm 中 backendVA[N] 没 PTE
  - 该 VMA 没注册 uffd → 走默认 shmem fault path
  - shmem inode 在 offset N 已有 folio(handler 装的) → 直接装 sandbox-ctl mm 的 PTE
  - 不投 uffd 事件,无 race,无需 handler 介入
```

backendVA 上**完全不注册 uffd**就根除了 cross-mm folio-creation race。

### 8.2 SnapshotReader 与 executable Run

冷启动和恢复共用一个只解析 metadata 的接口:

```go
type SnapshotReader interface {
    RunAt(
        memfdOffset uint64,
        limit uint64,
    ) (sparse.Run, error)
}
```

`sparse.Run` 描述最终逻辑可见的 `[Offset(), End())` 区段,`Kind()` 为
`Hole`、`Zero` 或 `Data`。payload 读取通过
`Run.ReadAt(ctx, buf, innerOffset)` 完成,`innerOffset` 相对
`Run.Offset()`;读取不得越过 `Run.End()`。Run 不可变,生命周期不超过
所属 snapshot Stream。

`RunAt` 仅做 metadata 解析,不会触发 cache/store Get、hash 校验、解密或
Prefetch。Handler 因此可以先确定所有硬边界和 Run 能力,再决定 urgent 与 tail
的读取方式。两种 source 的行为如下:

- `ZeroSource` 返回受 caller limit 限制的 `sparse.Zero` Run。它不自行选择
  tail window。
- `StreamSnapshotSource` 对 Data 直接返回 `fetch.Stream.RunAt` 的最终
  serving Run。manifest Data 保留 `fetch.ChunkRun` 能力;file、NFS 或 tar
  Data 是普通 Run。
- 最终可见的 Hole 和 manifest Zero 在 UFFD 边界合并为一个 no-read
  `sparse.Zero` Run。
- Run 不越过 memory section 的 `ramSize`。若 no-read/data 边界位于 faulting
  page 内,返回仅覆盖该页的普通 Data Run,由 `Stream.ReadAt` 组合该页,避免把
  真实 Data 错误地交给 `UFFDIO_ZEROPAGE`。
- 普通 Data 不再跨多个 Data Run 隐式扩展。layered stream 透传最终 leaf 的
  Run,上层 Hole 只收紧 visibility bound。

Handler 持有生命周期 context,所有 urgent 和 deferred
`Run.ReadAt` 都使用该 context。Handler 关闭并 join tail worker 后,restore
调用方才关闭底层 Stream,因此 tail task 持有 Run 期间 Stream 始终有效。

### 8.3 PageState 状态机

```go
type PageState uint8
const (
    StateAbsent   = 0   // folio 尚未由 handler 安装
    StateLoaded   = 1   // folio 已安装,或 EEXIST 后已收敛
    StateReleased = 2   // balloon EVENT_REMOVE 后,再次 fault 时装零页
)
```

PageState 使用 `[]uint8`,size = ramSize / PageSize。每 4 GiB RAM 对应
1 MiB 状态表。

| 当前 | 事件 | urgent 处理 | tail | 新态 |
|------|------|-------------|------|------|
| Absent | PAGEFAULT,Data Run | `Run.ReadAt(ctx,pageBuf,0)` 或 ChunkRun 完整读取后 `UFFDIO_COPY` 首页 | buffered/deferred Data | 条件提交 Loaded |
| Absent | PAGEFAULT,Hole/Zero | 单页 `UFFDIO_ZEROPAGE` | Zero | 条件提交 Loaded |
| Released | PAGEFAULT | 单页 `UFFDIO_ZEROPAGE` | Released Zero | 条件提交 Loaded |
| Loaded | PAGEFAULT | 单页 `UFFDIO_ZEROPAGE`,不扩展 | 无 | Loaded |
| 任意 | EVENT_REMOVE/UNMAP | 同步标记范围为 Released,提交 removeQ | 陈旧 tail 不得覆盖 | Released |

范围操作为:

```go
RunLength(start, max uint64, want PageState) uint64
SetRangeIf(start, end uint64, old, new PageState) uint64
```

`RunLength` 在一次读锁内扫描连续 expected-state 范围;`SetRangeIf` 在一次
写锁内只提交仍等于 old 的页。urgent 和 tail 均按 ioctl 明确完成的页数调用
`SetRangeIf`,因此 EVENT_REMOVE 后的 Released 页不会被陈旧 tail
无条件改回 Loaded。

### 8.4 fault-first worker 与 serial tail

```text
                            ┌──────── fault worker 0 ── 4 KiB urgent buffer
UFFD PAGEFAULT ── hash ─────┤
                            └──────── fault worker N ── 4 KiB urgent buffer
                                          │
                                          │ urgent COPY/ZEROPAGE
                                          ▼
                                  non-blocking reserve
                                          │
                                          ▼
                                  one serial tail worker
                                  one 1 MiB shared buffer
```

每个 Handler 只有以下 speculative 资源:

```go
tailBusy atomic.Bool
tailQ    chan tailTask // capacity = 1
tailBuf  []byte        // MaxTailBytes = 1 MiB
```

`InitialTailBytes = 64 KiB`。全 Handler 最多一个 tail task 处于 reserved、
queued 或 running。reservation 失败时 fault worker 直接放弃 tail,不等待、不
新增队列。remove flusher 使用独立的 mandatory reclaim 队列和 goroutine,不被
tail 占用或反压。

Run 类型决定数据准备方式:

| Run/状态 | fault worker | serial tail worker |
|----------|--------------|--------------------|
| manifest `fetch.ChunkRun` | slot 可用时读取完整当前可见 Run 到共享 buffer,先 COPY 首页;slot 忙时只读 4 KiB | `tailBufferedData`,复用 `tailBuf[PageSize:]` |
| file/tar/NFS 普通 Data | 只读 4 KiB 并 COPY 首页 | `tailDeferredData`,按 data window 调用同一 Run 的相对子范围 ReadAt |
| Hole/Zero/ZeroSource | ZEROPAGE 首页,不读 source | `tailZero(expected=StateAbsent)` |
| StateReleased | ZEROPAGE 首页,不读 source | `tailZero(expected=StateReleased)` |
| StateLoaded | ZEROPAGE 首页 | 无 |

所有 Run 和 tail hard end 同时受以下边界限制:

```text
MaxTailBytes
当前 CH UFFD region end
RAM end
连续 expected-state range
Run.End()
layered visibility bound
```

ChunkRun 不使用 adaptive window。它的完整可见 Run 已经限制在 1 MiB 内;
若物理 chunk end 不是页边界,完整 Run 仍只读取一次,tail 只提交其中页对齐的
完整页面。普通 Data 和 Zero/Released 分别维护独立窗口:

```text
64 KiB → 128 KiB → 256 KiB → 512 KiB → 1 MiB
```

上一 tail 成功完成到 `completedEnd`,且下一次真实 fault 恰好位于该地址时,
对应窗口翻倍。地址不连续、task drop、无完成量、ioctl 冲突、mode 或 expected
state 变化都会重置为 64 KiB。ChunkRun 不增长或消费这两个窗口。

urgent ioctl 只提交 faulting page,承担恢复正确性:

- 完整成功后按实际完成量条件提交当前页。
- `EEXIST` 执行 `UFFDIO_WAKE`,沿用 folio 已由 backend/其他 fault 安装的
  收敛语义。
- `EAGAIN` 和 `ENOENT` 执行 WAKE,不把未完成页标为 Loaded,允许后续 fault
  重试。
- 其他错误进入 handler 原有错误路径。

`ioctlUffdCopy` 和 `ioctlUffdZeropage` 返回 kernel result struct 中的实际
完成字节数。调用方严格要求 `0 <= completed <= requested`,且 completed
按 PageSize 对齐。`pages_copied` 和 `pages_zeroed` 只累计该明确完成量。

tail worker 严格串行且 best-effort。执行前和 source read 后都重新检查
PageState,只处理仍为 expected 的连续前缀。普通 COPY/ZEROPAGE 会自动唤醒已
等待的其他 fault;`EEXIST/EAGAIN/ENOENT` 记录 conflict 并停止当前 task,
不逐页重试,也不升级为 sandbox 恢复失败。每条完成、冲突、取消、drop 和关闭
路径都清除 `tailBusy`。

关闭顺序为:

1. 设置 closing,拒绝新 reservation。
2. 取消 Handler context,丢弃 queued tail,等待 reserved/running task 释放并
   join tail worker。
3. 停止并 join UFFD reader 和 fault workers。
4. remove flusher drain reader 已提交的 mandatory reclaim 后退出。
5. Handler 返回后,restore 关闭 snapshot Stream。

可观测指标分为:

```text
fault_queue_wait_ns
fault_queue_wait_p50 / p95 / p99
fault_queue_depth / fault_queue_depth_hwm
fault_inflight / fault_inflight_hwm

source_read_calls / source_read_bytes / source_read_ns
urgent_copy_calls / urgent_copy_ns
urgent_zero_calls / urgent_zero_ns

tail_submitted / tail_dropped_busy / tail_canceled
tail_buffered_data / tail_deferred_data / tail_zero
tail_pages_planned / tail_pages_completed
tail_copy_ns / tail_zero_ns
tail_conflicts / tail_partial
tail_window_current / tail_window_grows / tail_window_resets
```

stderr 的 `[uffd-stats]` 和 `--stats-json` 使用相同字段。旧的同步
`batch_*` 指标不再存在。标准化 in-process A/B/C benchmark 命令为:

```bash
go test ./pkg/uffd -run '^$' \
  -bench '^BenchmarkUFFDFaultStrategies$' \
  -benchmem -benchtime=300ms -count=5
```

其子项统一比较:

```text
A_SyncFullBatch
B_FaultFirstNoTail
C_FaultFirstSerialTail
```

fixture 覆盖 ordinary Data、manifest hit、合成 cold-copy、local plaintext /
encrypted tar、Zero/Released、顺序/随机和双 vCPU。设置
`KUASAR_UFFD_BENCH_NFS_ARTIFACT` 可加入位于 NFS 上的 canonical tarstream
artifact。该微基准报告 `source-B/op` 和 `uffd-B/op`;真实 cache miss、
cold page cache、UFFD wake、host CPU/RSS、guest resident pages、restore
readiness 和首请求延迟仍由需要 KVM 的跨仓 e2e/perf matrix 测量,不能用微基准
替代。

**5.10 内核兼容**:仅依赖 4.11+ 引入的 `UFFD_FEATURE_MISSING_SHMEM` /
`EVENT_REMOVE` / `EVENT_UNMAP` / `THREAD_ID`。不使用
`MINOR_SHMEM`(5.13+)或 `UFFDIO_CONTINUE`(5.13+)。

### 8.5 EVENT_REMOVE 处理

CH 的 balloon inflate 处理对每个让出 run 调一次 release:

1. 在 memfd 上 `fallocate(FALLOC_FL_PUNCH_HOLE | KEEP_SIZE)` —— 释放 inode 页
2. 在 chVA 上 `madvise(MADV_DONTNEED)` —— 清 CH 自己进程的 PTE

`EVENT_REMOVE` 的**唯一来源是 (2)**:chVA 是 uffd_C 注册的 VMA,内核在其上
处理 `MADV_DONTNEED` 时**无条件**合成一条 `EVENT_REMOVE`(覆盖整个 madvise
区间,**与该区间是否驻留无关**),并且 `MADV_DONTNEED` 会**同步阻塞**到外部
handler 消费完该事件才返回。(1) 的 fallocate 只丢 inode 页,**不**经此路径
产生 chVA 上的 `EVENT_REMOVE`——所以要止住事件必须同时跳过 (1)(2)。

handler 收到 `EVENT_REMOVE` 后做 **process-level reclaim**:对 sandbox-ctl
自己的 backendVA mmap 做 `madvise(MADV_DONTNEED)`,把进程级 PTE/RSS 份额清掉。

**空洞跳过为何决定冷启动收敛速度**。`release_memory_range` 在 x86-4K
下**逐 4K 页**调用(`pbp` 合并被旁路)。若不跳过空洞,把 balloon 充到 `capacity −
allocatable_now`(1.5 GiB 量级)时**每个 4K 页**都走 (1)(2),而 (2) 的
`MADV_DONTNEED` 在 uffd VMA 上**同步阻塞**到单 reader handler 消费完该
`EVENT_REMOVE` 才返回——balloon 线程要做 `≈ 充气字节 / 4K`(1.5 GiB ≈ 40 万)
次**串行的跨进程同步往返**,收敛达数十秒,且可在 boot 期形成软死锁。瓶颈是
这串行握手,而非回收真实内存本身。

guest balloon 驱动充气分配的页**从不被 guest 写入**(`balloon_page_alloc` 无
`__GFP_ZERO`,平台 guest 内核未启用 `init_on_alloc`,fill 路径只动元数据),
且 user-managed zone 从不 prefault。因此 balloon 让出的 offset **压倒性多数**
(实测冷启动 ~99%)在 memfd 上是从未触碰的空洞——无 inode 页、无 PTE、无可
释放物。CH 的 release 用一次 `lseek(SEEK_DATA)` 探测该 run 是否整段空洞,
空洞则**跳过 (1)(2)**(平台 patch,见
`sandboxer/docs/cloud-hypervisor.md` §3.4):无 madvise → 无同步握手,
balloon 线程以内存速度扫过这 ~99% 的页 → **收敛近乎瞬时**。

剩下少量(实测 ~1%)是 guest 启动期经 vhost-blk 后端 / 内核拉进 page cache
又释放、但 folio 仍驻留 memfd 的页:`lseek` 正确发现有数据,**不跳过**,照常
PUNCH+madvise 回收——这是**有界的合法回收**,非浪费。另外 guest 退出时整个
zone 被 unmap,会有一条覆盖整 zone 的大 `EVENT_REMOVE`/`EVENT_UNMAP`,与本
patch 无关、也不属于充气阶段(勿与充气期事件混计)。

**双端职责划分**(仅运行时回收已驻留页时发生):

| 端 | 动作 | 释放对象 | 触发时机 |
|---|---|---|---|
| CH(balloon inflate 处理) | run 段内有数据:`fallocate(PUNCH_HOLE)` on memfd + `madvise(MADV_DONTNEED)` on chVA;**整段空洞:跳过两者** | inode 页 + CH 自己的 PTE | host 通过 `/vm.resize` 推高 target → guest inflate 让出**已用过**的页(运行时回收 / node-ctl reclaim)|
| sandbox-ctl handler | `madvise(MADV_DONTNEED)` on backendVA | sandbox-ctl 自己的 PTE/RSS | 收到 `EVENT_REMOVE`(仅 CH 未跳过、即段内有数据时)|

**关键不变量**:

- CH 的 fallocate(PUNCH_HOLE) 与 sandbox-ctl 的 madvise(DONTNEED) 是**互补**
  的,不是 redundant:前者管 file pages,后者管 process PTE,缺任一边都泄漏。
  此不变量只在"段内有数据、确需回收"时才进入——空洞 run 两边都无可释放。
- 跳过只命中真空洞:任何**确需回收**的页必有数据,永不被跳过;被抑制的
  `EVENT_REMOVE` 本只驱动 handler 对 backendVA 的 reclaim,而空洞 offset
  sandbox-ctl 也从未 fault → 那一步本就 no-op。跳过前后 host 内存终态一致。
- `EVENT_REMOVE` 路径仍异步化(reader 只更新 pageStates + push removeQ;
  flusher batch+merge 后批量 madvise),该吞吐能力**服务运行时回收**已驻留
  工作集页与少量 boot 残留;冷启动 ~99% 的空洞充气页在 CH 源头被跳过,
  不进入此路径(故不再是收敛瓶颈)。
- `EVENT_REMOVE → pageStates=Released` 的状态写入与 guest 对同一页的再
  fault 之间存在竞态:PUNCH 已回收 folio,但状态表尚未置 Released 时,fault
  以 **StateLoaded** 进入 handler。uffd MISSING **仅在无 folio 时触发**,故
  Loaded 上的 fault **证明** folio 已被 balloon 回收,与 Released 等价——
  handler 按单页 `UFFDIO_ZEROPAGE` 重填(让出页对 guest 即新页,`balloon_
  page_alloc` 无 `__GFP_ZERO`,零页即正确语义;回放 source 内容会把旧数据
  复活到复用页,是 bug)。此处 **WAKE-only 必然 fault↔WAKE 活锁**:无 folio
  可供内核重 fault 装回,guest 永久重 fault。这是 post-settled aggressive
  inflate(§9.3 BalloonController 推高 target,回收已用过的工作集页)下的
  必经路径,非异常兜底。

## 9. cgroup 与 balloon 联动

### 9.1 cgroup 内存设置(静态 cgroup / 动态控制模式)

```
memory.max       ← capacity_bytes + overhead.memory       # 不变
memory.high      ← watermark_high.memory                  # 静态 cgroup 模式恒为该值;
                                                              动态控制模式随 allocatable_now × ratio 变化
memory.swap.max  ← 0                                      # 禁 swap
```

**关键决策点**:

- `memory.max = capacity + overhead`,**不等于 allocatable**。把 memory.max
  设成 allocatable 时,guest 合法使用到 allocatable 上限会让 cgroup OOM kill
  CH 进程,等同于平台主动终结沙箱
- `memory.high < memory.max` 留出反压窗口:guest 内存接近 high 时内核给 CH
  进程内存分配加 PSI 延迟,但不 kill
- 禁用 swap:超分语义下 swap 会让 OOM 决策路径模糊

### 9.2 cgroup CPU 设置

cgroup_path 已设的情况下,sandbox-ctl 在该 cgroup 内写两项,启动后**全程
不变**:

```
cpu.max     ← capacity.cpu × period " " period               # period = 100ms
cpu.weight  ← clamp(round(allocatable.cpu × 100), 1, 10000)
```

**意义**:
- `cpu.max` 是 cgroup 的硬上限。设成 capacity 意味着**无 CPU 竞争时,沙箱可以
  跑满 capacity 核**——这是平台对应用的承诺
- `cpu.weight` 是公平共享权重,仅在多个 cgroup 同时争用 CPU 时生效。
  `allocatable.cpu × 100` 让 1 核 allocatable 对应权重 100(等于内核默认),
  0.1 核 → 10,2 核 → 200

**结合后语义**:
- 节点 CPU 充裕(总用量 < 物理核数):每沙箱可达 cpu.max(= capacity)
- 节点 CPU 紧张:内核 CFS 按 cpu.weight 比例分配,在 admission 保证
  `Σ allocatable.cpu ≤ physical_cpu` 的前提下,每沙箱至少等于 allocatable.cpu

这套静态模型实现了"无竞争时给 capacity / 有竞争时给 floor",**完全不需要
运行时调整 cpu.max,也不需要 CPU 维度的 burst/recover 状态机或 RPC**。

**cgroup 归属:解耦(默认)vs 采纳(`--cgroup-adopt`)**。两种写法都把上面的
memory/cpu 上限写到目标 cgroup,区别在于谁进这个 cgroup:

- **解耦(默认,含 `--cgroup-path`)**:sandbox-ctl **从不**把自己加入沙箱
  cgroup,只在 CH `exec.Start` 后用 `AddPID` 把 CH(且仅 CH)move 进去。
  这样 guest 内存逼近 `memory.high` 触发的内核节流只压到 CH,sandbox-ctl 仍可
  正常被调度、收发信号(否则 `mem_cgroup_handle_over_high` 会把 sandbox-ctl 卡在
  TASK_KILLABLE D-state,既杀不了 CH 也 reap 不了 `cmd.Wait`,全盘死锁)。
- **采纳(`--cgroup-adopt`)**:目标 cgroup 就是 sandbox-ctl 自身所在的(其
  systemd 单元的)cgroup——上限写到该 cgroup,CH 作为 fork 出的子进程已是成员,
  故 `AddPID` 成为 no-op。代价是 sandbox-ctl 与 CH 同处一个 cgroup,**重新暴露了
  上面解耦路径所规避的 `memory.high` 节流死锁**(见 pkg/resctl/cgroup.go 头注)。
  面向 run-sandbox 启动器路径(单元自身即沙箱 cgroup,无需预建)。

### 9.3 balloon 配置与 BalloonController

CH 命令行(三种模式都用,跟 cgroup 解耦):

```
--balloon size=0[,deflate_on_oom=on]
```

仅当 `allocatable_now < capacity` 时附加 `--balloon`;两者相等时省略,无 host
端 RAM 回收路径。

- `size=0` boot 期 guest 看到 capacity 等额内存,balloon 尚未持有页。host 端
  BalloonController 在 settled 之后(launch 握手完成)接管 target,把 balloon
  推到 `capacity − allocatable_now`。这批让出的页 ~99%(实测)是从未写过的
  空洞(memfd 稀疏未 prefault),CH 的 release 对空洞 run 跳过 PUNCH/madvise
  → 省去同步 `EVENT_REMOVE` 握手,充气收敛近乎瞬时;少量确驻留的
  瞬态 page cache 仍合法回收(机制见 §8.5 与
  `sandboxer/docs/cloud-hypervisor.md` §3.4)
- **不**启用 `free_page_reporting`。FPR 让 guest 在每轮 page reclaim 中把空闲
  页号高频推到 host,CH 的 `release_memory_range` 对自身 mmap 做
  `madvise(MADV_DONTNEED)` 广播 mmu_notifier 失效到 KVM EPT,持续的 IPI
  shootdown 饿死 guest vsock kthread → host→guest ping 在十数秒内全部 timeout。
  改由 host 端按周期主动推 inflate target,事件量被速率限制,问题消除
- `deflate_on_oom=on` 由 `allocatable.deflate_on_oom` 决定(默认 on)

**BalloonController(host 侧反馈环)**:

```
guest sandbox-init  ─ mem_report (vsock, 5 s) ─►  Controller.Hint
                       MemAvailable/MemTotal               │
                                                           ▼
       ◄─── PUT /api/v1/vm.resize {desired_balloon} ─── Reconcile (5 s tick)
```

策略要点:

- **目标自由缓冲**`TargetFreeBuffer`:默认 `max(64 MiB, Capacity/32)`。Hint 根据
  `delta = MemAvailable − TargetFreeBuffer` 调整 target,把 guest 的自由内存
  锚定在该值附近
- **anti-hunting**:`|delta| < Slack`(默认 32 MiB)的样本直接丢弃
- **MaxStep 限速**:单次 Hint 调整 ≤ `MaxStep`(默认 256 MiB)。boot 充气
  ~99% 走空洞跳过(无同步握手),故此限速实质作用于**运行时回收已驻留
  工作集页**时的 mmu_notifier / `EVENT_REMOVE` 突发量
- **stale 防护**:刚推过一次 inflate,guest MemTotal 还没收到本次让出量的反映,
  此时 MemAvailable 偏大。若 `MemAvailable > (Capacity − max(target, actual)) +
  64 MiB`(visible slack)判为 stale,跳过该样本,避免反馈环正反馈失控
- **Reconcile**:5 s ticker;`target != actual` 时一次 `PUT /api/v1/vm.resize`,
  成功后写回 `actual`。Start 立即跑一次以便 settled 后尽快进入 target

**手动覆盖**:动态控制模式下 sandbox-ctl 也可以通过 `Controller.SetTarget`
直接设值(node-ctl grant/reclaim 时使用),Hint 与 SetTarget 互不干扰。

### 9.4 deflate_on_oom 安全网

deflate_on_oom 触发链路:

1. Guest 应用申请内存,guest 内 RAM 用尽
2. Guest 内核 OOM killer 即将触发,balloon driver 拦截
3. Balloon driver 从 balloon 池释放页给 guest 进程
4. CH 在 host 上 RSS 增长,但 < memory.max(静态 cgroup / 动态控制模式)

定位:**双重失败的最后防御**。动态控制模式正常路径下:
- 第一道:cgroup memory.high PSI 给 CH 进程内存 alloc 加延迟
- 第二道:sandbox-ctl 检测 high 事件 → 控制器 grant → balloon deflate

只有当反馈环路追不上 guest 增长时,deflate_on_oom 才生效,代价是 guest 内
进程被杀。无 cgroup / 静态 cgroup 模式没有反馈环路,deflate_on_oom 是唯一
防线,所以默认开启。

## 10. 与 node-ctl 的资源协议

详细协议规范见 `orchestrator/docs/node-resource.md` §5;本节描述 sandbox-ctl 侧的执行器
行为。

### 10.1 沙箱状态机

动态控制模式下沙箱经过 7 个阶段;静态 cgroup 模式只走 admitted → creating →
startup → settled,不进入 burst / recover;restoring 仅在快照恢复路径出现:

| 阶段 | 触发 | allocatable_now 内存策略(动态控制模式) | sandbox-ctl 行为 |
|------|------|----------------------------------|-------------------|
| **admitted** | 收到 Admit grant | reservation 占用预算,沙箱未启动 | 验证 cgroup_path,准备 socket |
| **creating** | 开始创建 CH 等 | reservation 持有 | 拉起 CH、handshake |
| **startup** | CH 已启动,等 launch hello | startup.memory | 等 launch protocol hello |
| **restoring** | CH /vm.restore + /vm.resume 完成,等 restore_ack | allocatable_at_snapshot 或降级值 | 发 `restore{epoch}` 等 `restore_ack`(该连接随后升级为新 stdio MUX) |
| **settled** | hello 收到 / restore_ack 收到 | 渐缩到 floor + 工作集余量 | 周期上报 RSS;发 Settled |
| **burst** | 检测到压力 | 申请扩展,可达 capacity | resize-balloon、改 memory.high |
| **recover** | 压力消退 + 冷却 | 不主动收回,等被动回缩 | 继续上报 |

**settled 触发是事件驱动,不依赖定时器**:

- **冷启动 startup → settled**:由 sandbox-init phase 2 拨号 launch server
  发 `hello` 消息触发——sandbox-init 在 mount/network 等平台初始化完成、即将
  拿到 launch spec 启动用户进程的时刻
- **恢复 restoring → settled**:由 host 收到 sandbox-init 的 `restore_ack` 触发
  (host→guest `restore{epoch}` 短连接,sandbox-init 立即 reply `restore_ack`,
  该连接随后升级为新的 stdio MUX,见 §7 / sandbox-init.md §4.3)

### 10.2 各阶段的 cgroup 与 balloon

动态控制模式下(静态 cgroup 模式全程对应 settled 列):

| 阶段 | memory.max | memory.high | balloon target | cpu.max / cpu.weight |
|------|-----------|-------------|----------------|---------------------|
| admitted | capacity+overhead | startup × ratio | (未启动 CH) | 静态(永不变) |
| creating | 同上 | 同上 | (CH 启动中) | 同上 |
| startup | 同上 | 同上 | capacity − startup | 同上 |
| restoring | 同上 | allocatable_at_snapshot × ratio | capacity − allocatable_at_snapshot | 同上 |
| settled | 同上(永不变) | allocatable_now × ratio | capacity − allocatable_now | 同上 |
| burst | 同上 | allocatable_now × ratio(allocatable_now ↑) | capacity − allocatable_now ↓ | 同上 |
| recover | 同上 | 同上 | 同上 | 同上 |

`ratio = watermark_high.memory / allocatable.memory`,默认 0.875。memory.max、
cpu.max、cpu.weight 全程不变。memory.high 与 balloon target 随 allocatable_now
同步变化。

### 10.3 压力信号(动态控制模式)

sensor 是 sandbox-ctl 的 goroutine,Settled 之后启动,根据 cgroup 内存压力发
`RequestBudget` 给 node-ctl。数据源由 `resources.control.sensor.mode` 选择:

**`psi` 模式(默认)** —— 推荐。epoll on `memory.pressure`:

| 信号源 | 触发方式 | 用途 |
|---|---|---|
| `memory.pressure` 注册的 PSI trigger | epoll EPOLLPRI 唤醒(sub-ms) | 主信号:`urgency=normal` 申请扩展 |
| `memory.events.local` oom 计数差 | 1s sidecar tick | 紧急信号:`urgency=high` |
| `memory.events.local` high 计数差 | 1s sidecar tick | 仅 log warn(说明 PSI 触发阈值过松) |

PSI trigger 写入格式 `some <stall_us> <window_us>`,缺省 `some 10000 1000000`
(1 s 窗口累计 10 ms stall 即触发——dense workload 实测甜点)。trigger 写入失败
(老内核 / CONFIG_PSI=n)→ 自动回落 `events_poll` 模式。

**阈值取舍**(density-perf N=16 BURST=2GiB on 8 vCPU host 实测):

| `psi_some_stall_us` | hang 数/16 | 备注 |
|---|---|---|
| 50000 (50 ms) | 9 | 单次 over_high < 1ms,1s 内累计不到 50ms,大量信号漏掉 |
| 10000 (10 ms) | 3 | 甜点,sidecar 漏报警显著减少 |
| 1000 (1 ms) | 3 | 灵敏度饱和,无进一步收益(瓶颈转移到 allocator throughput) |

**`events_poll` 模式** —— 兼容回落。100 ms 周期读 cgroup 文件:

| 文件 | 信号 | 用途 |
|---|---|---|
| `memory.events.local` | high 计数差 | 主信号:`urgency=normal` |
| `memory.events.local` | oom 计数差 | 紧急信号:`urgency=high` |
| `memory.current` + `memory.high` | RSS 上升斜率且 RSS/high > 0.95 | 预测信号:`urgency=low` |

**`none` 模式** —— 关闭 sensor(admission + balloon 仍工作)。

**为什么 PSI 是主路径**: `events_poll` 100 ms tick + 多源 RPC 调度让 burst 反应
延迟 ~50 ms 平均;PSI epoll 让 trigger 触发到 RequestBudget 在 ms 级。在重负载
host 上更早调高 `memory.high` → 缩短 / 消除 `mem_cgroup_handle_over_high`
同步回收的 D-state 窗口。

**`min_interval_ms`(默认 100 ms)** —— PSI 唤醒去抖,确保两次 RPC 间最小间
隔。避免 PSI 触发抖动产生 RPC 风暴。

不采纳:guest balloon STATS_VQ(5 s 周期太粗);uffd fault rate(信号扭曲);
guest 内进程级压力(跨 host/guest 边界,接口复杂)。

### 10.4 长连维持与降级

**长连维持**:

- 沙箱进入 startup 后,连接保持活跃;sandbox-ctl 在此连接上发后续 RPC,并经
  Heartbeat ack 的 `new_allocatable` 接收 reclaim/admin 的 allocatable 调整
- 30s 周期 Heartbeat;控制器 90s(3 个周期)未收到 → 视为掉线

**断连降级**:

- 连接断开,sandbox-ctl 退避重试(1s, 2s, 5s, 10s, 10s, ...)
- 持续 60s 重连失败 → 切到无控制器模式继续:保持当前 allocatable 不变,接管
  cgroup memory.high 设置,不再申请 burst
- 此时即使有新压力,只能靠 cgroup PSI 反压 + deflate_on_oom 兜底
- 重连成功后自动恢复联动

## 11. 恢复时的资源衔接

恢复路径与冷启动有两层差别:**(a)** sandbox.yaml 里大量字段在 restore 模式下
有特殊语义(与 snapshot.cfg 合并、覆盖、断言式校验);**(b)** guest 在快照里
**已有 in-memory 状态**(driver 缓冲、应用堆、page cache),`/vm.resume` 之后
cloud-hypervisor 按快照 page 索引把这些页 fault 回 guest 物理地址,host
allocatable 初值必须够大才能避免 PSI 节流 / sensor 反复 burst。

§11.0 解决 (a),§11.1-§11.3 解决 (b)。

### 11.0 restore 模式下 sandbox.yaml 字段语义

`run --restore=<ref>` 时,sandbox.yaml 与内嵌 snapshot.cfg 按下表合并。
"提供时"指 yaml 里该字段非零值,"未提供"指空值或字段缺失。

| sandbox.yaml 字段 | 提供时 | 未提供时 |
|---|---|---|
| `resources.capacity.{cpu,memory}` | 与 snapshot.cfg 严格相等才允许;不一致拒绝启动(error: "capacity mismatch") | 直接用 snapshot.cfg.resources.capacity |
| `resources.allocatable.*` | 与冷启动语义相同(host 资源策略) | 沿用冷启动默认(等于 capacity) |
| `restore.prefetch` | `memory` 在满足 §7.1 资格时异步预热当前 memory self(file page cache 或 manifest chunk cache);`off` 显式关闭 | 默认关闭;不从 snapshot.cfg 继承 |
| `network.{tap\|tapfd}` | 最多一个且存在性必须与 config.json 中的快照 NIC 拓扑一致;tapfd 模式重新交接(docs/tapfd.md §4,幂等)取新 fd,经 `--restore net_fds=[_net0@[4]]` 注入 CH;tap 名模式 CH 按名重开 | 仅无 NIC 快照允许;联网快照报拓扑不匹配 |
| `network.{ip,mtu,nexthop,hostname,interface}` | 仅存在网络源时允许;经 restore 通知重新下发,guest flush-and-replace 重配(克隆取新 L3 身份);MAC 不变(沿用快照设备状态,故 provider 须用稳定 per-port MAC) | 无 NIC 快照保持无 NIC;联网快照保留快照网络不变 |
| `boot.kernel` | 静默忽略(restore 不 boot) | 同 |
| `boot.runtime`(仅 file://) | basename 与 snapshot.cfg.runtime_ref 匹配,且 bundle marker 必须与 digest 一致 | 用 snapshot.cfg.runtime_ref:basename 解析为 `<sid>.snapshot` 同目录文件 |
| `boot.root.base`(file://) | 协议 + basename 与 snapshot.cfg.base_ref 一致,且 tarstream scheme + digest 必须一致 | 用 snapshot.cfg.base_ref:basename 解析为 `<sid>.snapshot` 同目录文件 |
| `boot.root.base`(manifest://) | manifest key 与 snapshot.cfg.base_ref 一致才允许 | 用 snapshot.cfg.base_ref 原值 |
| `boot.root.overlay.base` | **静默忽略** | 用 snapshot.cfg.overlay.base |
| `from_refs` / `boot.root.overlay.base_from_refs` | 无此 yaml 字段(增量分层链纯由 snapshot.cfg 提供,§3.5) | 用 snapshot.cfg 原值 |
| `boot.root.overlay.diff` | 可选 file:// 绝对路径;空→落盘 base 目录(随沙箱销毁) | 默认落盘 base 目录 |
| `boot.root.overlay.diff_size` | restore 新 diff 取 base 大小,此项不参与 | 取 base 大小 |
| `boot.cmdline` | 静默忽略(restore 不 boot) | 同 |
| `launch.*` | 静默忽略(应用在 guest 内存里) | 同 |
| `control.cgroup_path` / `control.controller` | 用作本次恢复的资源策略 | 同冷启动默认 |
| `overhead` / `watermark_high` / `startup` | 同 control 规则 | 同冷启动默认 |
| 其他 | 静默忽略 | — |

**为什么 capacity 必须严格相等(而不是 max)**:guest 内存中已经按当时
capacity 决定了页面布局、kernel 内部数据结构(NR_CPUS / per-cpu data /
zone watermarks)。变了 capacity 等于换了一套硬件假设,行为未定义。

**为什么 file:// runtime / base 要 identity 校验**:跨主机移动 snapshot 时,
目标 host 上同名 sandbox-runtime.bundle / container-image.erofs 可能是不同
版本。runtime 仍比较 bundle SHA marker;base 与其他 tarstream 工件按 §3.2.1
比较 scheme + digest。随机路径只读取所需 metadata/record,没有跳过校验的性能
模式;发布路径再执行 sequential full validation。发布流程通过写入时计算、fsync、
no-replace 原子提交和终态不可变保证 payload 与声明一致。

**boot.root.overlay.base 为什么忽略 yaml**:overlay 数据是 sandbox 自己的写
状态,与镜像 base 等价物,只能从 snapshot 内部走。让 yaml 强制提供没意义,
反而引入"不一致"风险面。

**绝对路径要求**:restore 模式下 yaml 提供的 file:// URL 仍要求绝对路径
(允许的是"整字段不写",不是"写相对路径")。

### 11.1 唯一来源:CH 自己的 balloon 状态

`sandbox-ctl` 在运行期通过 BalloonController(§9.3)维护 `balloon.target =
capacity − allocatable_now`(node-ctl grant/reclaim 走 SetTarget,稳态由
mem_report Hint 驱动),因此 balloon target/current 已经把 allocatable_now
的语义编码进去了。CH 在 `/vm.snapshot` 时把 balloon 设备状态(含 `num_pages`
= host 想拿走的页数 / target、`actual` = guest 已交还的页数 / current)写入
bundle 内的 `state.json`,**无须再额外保存**。

恢复时 sandbox-ctl 从 bundle 解出:

| 字段 | 来源 | 含义 |
|---|---|---|
| `capacity` | `snapshot.cfg` 的 `resources.capacity.memory` | 快照时的 memory-zone 容量 |
| `balloon.target` | `state.json` `snapshots["device-manager"].snapshots["__balloon"]` 内的 `config.num_pages` × 4 KiB | host 当时想保留多少页(对应 capacity − allocatable_now) |
| `balloon.current` | 同上 `config.actual` × 4 KiB | guest balloon 驱动当时已实际交还的页数 |

派生:

```
allocatable_at_snapshot = capacity − min(balloon.target, balloon.current)
```

取 `min` 是为了在 balloon 还没收敛时拿到更宽裕的 allocatable:
- **inflating**(host 想拿更多,guest 还没让出,target > current):取 current
  → guest 此刻仍持有较大有效内存,fault 重放时给它这个量,平滑过渡
- **deflating**(host 想还给 guest,guest 还没扩张,target < current):取
  target → host 已经计划放出,sensor/heartbeat 会让 guest 后续扩到该值

恢复后,BalloonController 通过 `SetTarget(capacity − allocatable)` 把 balloon
target 重新设回该值,首次 Reconcile 通过 `/vm.resize` 落到 guest,使 host
与 guest 视图一致;mem_report 反馈环在 guest 恢复 sandbox-init supervisor
循环后自动续上。

### 11.2 无 cgroup / 静态 cgroup 模式(无控制器)的恢复决策

```
initial_alloc = max(yaml.allocatable.memory, allocatable_at_snapshot)
```

`max` 的语义:从动态控制模式拍下的快照可能 allocatable_at_snapshot >
yaml.allocatable(运行期 burst 过),恢复到静态部署时若直接用 yaml,guest
的工作集会被压回 yaml,触发 PSI 节流。`max` 让恢复保留 burst 后的工作集
大小;若运营策略要求严格遵守 yaml,可在 `sandbox.yaml` 里把 capacity 也调小
到 yaml.allocatable,那时 allocatable_at_snapshot ≤ yaml(由 capacity 自然
约束)。

### 11.3 动态控制模式(有控制器)的恢复决策

`sandbox-ctl` 在 `Admit` 中携带:

| 字段 | 值 | 角色 |
|---|---|---|
| `floor_memory_bytes` | `yaml.allocatable.memory` | 控制器降级时的回退下限 |
| `allocatable_at_snapshot` | 上一节派生值 | 控制器优先尝试授予的 QoS 目标 |

控制器复用 admit 逻辑——把 `allocatable_at_snapshot` 当 requestedInitial、
`floor` 当 fallback——**恢复路径没有新分支**:

| 余量情况 | granted_initial_alloc |
|---|---|
| 可分配余量 ≥ allocatable_at_snapshot | = allocatable_at_snapshot(完美还原) |
| floor ≤ 可分配余量 < allocatable_at_snapshot | = floor(降级) |
| 可分配余量 < floor | status=rejected,上层调度器换节点 |

降级路径下,sensor 在 restoring 阶段已激活,若 fault 重放压满 cgroup memory.high
就立即 RequestBudget(urgency=normal),逐步把 allocatable 拉回 burst 状态;
预算不允许时,沙箱以 floor 长期运行,直到节点预算释放或被调度走。

## 12. vhost-user-blk backend

### 12.1 协议子集

实现以下 vhost-user 消息(其他不实现,收到回 not_supported):

| Message | 用途 |
|---------|------|
| GET/SET_FEATURES | feature negotiation |
| GET/SET_PROTOCOL_FEATURES | protocol-level extensions |
| SET_OWNER / RESET_OWNER | ownership 锁 |
| SET_MEM_TABLE | guest physical memory layout(关键,带 fd) |
| SET_VRING_NUM/ADDR/BASE/KICK/CALL/ENABLE | virtq 编程 |
| GET_QUEUE_NUM | virtq 数量 |
| GET_CONFIG / SET_CONFIG | virtio-blk 配置区 |

### 12.2 SET_MEM_TABLE 行为契约

由于统一内存所有权模型,sandbox-ctl 永远先于 CH mmap memfd 并 register uffd,
backend 收到 SET_MEM_TABLE 时**永远**走"复用 backendVA"路径:

```
对每个 region:
  fstat(fd).Ino 与启动时记录的 sandbox-ctl-mmap'd memfd inode 比对
    匹配 → 注册 (gpa, hva = backendVA + mmap_offset, size) 进 GPA→HVA 表
            不再 mmap,不再 register uffd
    不匹配 → 拒绝(协议错误,违反统一模型 invariant)
关闭收到的 fd
回 ack
```

只有一种正确路径,没有冷启动 / 恢复分支。

### 12.3 BlockReader 接口契约

```
BlockReader:
  ReadAt(buf []byte, offset int64) → (int, error)
  Size() int64
  Close() error
```

所有只读块来源统一为一个读取层抽象 `Stream`(本地文件、manifest、或多层叠加),
由一个适配器包装为 `BlockReader`,Close 时释放底层 `Stream`(文件句柄;manifest
形态的 store/cache 客户端归 Fetcher 所有,另行释放)。`Stream` 形态:

- **本地文件**:`pread(2)`;经 `SEEK_DATA`/`SEEK_HOLE` 探测稀疏空洞,空洞由内核读为零。
- **manifest**:从 store-ctl + cache-ctl 拿 manifest blob → 解码 + 解封 keys →
  按需取块解密;ReadAt 时零填充 hole 与 IsZero 区段(块设备语义下等价于零页,
  不走 store)。Size 来自 manifest 元数据。
- **多层叠加**:上述任意 `Stream` 自顶向下叠加——顶层数据优先,声明空洞穿透到下层,
  IsZero 为顶层拥有的真实零数据不穿透,越界等同空洞,Size 取各层最大。

blk0 backend:写请求(IN/DISCARD/FLUSH)拒绝,返回 IO_ERR;读请求经 BlockReader
拿数据,写到 guest buffer (HVA)。

manifest:// 路径需要 sandbox-ctl 持有 store + cache 客户端 + chunk + key-table
加密器 + customer key —— 整套从清单配置在 sandbox 启动时一次性建立,
blk0 / overlay base 共享同一套客户端连接池,sandbox 退出时统一释放。

### 12.4 blk1 base+diff COW

blk1 是可写盘,基础语义为"上层 ext4 sparse 文件 + 可选 base 层":

```
读路径:
  for each 4K block in [off, off+len(buf)):
    acquire shared stripe lock
    if dirtyBitmap[blk]:    read logical diff view at blk*4K
    elif baseReader != nil: baseReader.ReadAt(slice, blk*4K)
    else:                   memset(slice, 0)

写路径:
  for each 4K block in [off, off+len(buf)):
    acquire exclusive stripe lock
    if clean 且本次未完整覆盖该 block:
      从 baseReader 或零完整初始化 4K scratch block
      合并本次 slice
      write logical diff view at blk*4K, 4K
    else:
      write logical diff view at 对应 offset
    完整写成功后 dirtyBitmap[blk] |= 1
```

固定数量的 stripe lock 让同一 4K block 的 read/materialize/write 串行化,
不同 block 仍可并发。plaintext diff 的 logical offset 等于 physical offset;
encrypted diff 的 private diff-file helper 把 logical offset 映射到 4096-byte header
之后,再按 512-byte XTS data unit 解密/加密。guest、BlockCOW 和 snapshot 均看不到
header offset 或 ciphertext。

启动时通过 SEEK_DATA/HOLE 扫 active diff body 重建 dirtyBitmap;encrypted 文件从
physical offset 4096 开始扫描,header allocation 绝不标记 logical block dirty。因此
dirty bit 始终表示对应 4K body block 已完整初始化。clean block 第一次 partial write
先从 base 或零读取完整 4 KiB、合并修改、写完 8 个 512-byte units,成功后才标 dirty。

snapshot 在全部 vhost backend quiesce 后调用 `BlockCOW.SnapshotView()`:view 复制
当前 dirty bitmap,只暴露 upper,不包含 base。dirty block 从 live BlockCOW 读取完整
plaintext 4K 内容;clean block 作为 hole,即使调用方防御性读取也返回零。snapshot
plumbing 携带该 view provider,不再通过 raw diff path 重开文件;path 只保留给日志、
统计和清理。preflight 直接携带 BlockCOW logical size,不提前构造 hole map;
capture 读取时把相邻 dirty block 合并为连续 range,每个涉及的 stripe 只取一次读锁,
并以一次底层 `ReadAt` 读取该 range。

当前 virtio-blk DISCARD / WRITE_ZEROES 保持 v1 的有效 no-op:worker 接受请求并返回
成功,但不把 guest range 传给 BlockCOW,不改变 body 或 bitmap。本期不引入三态
`{ clean, dirty, discard }` 和新的 hole-punch 语义。

### 12.5 Quiesce / Resume

snapshot 路径要求 `/vm.pause` 之后内存内容稳定,但 backend worker 可能正在
`processChain` 写 backendVA(virtio IN 完成阶段把 disk 数据 memcpy 到 GPA)。
若不同步会导致快照内存与 vCPU 看到的状态不一致。

- `Server.Quiesce()`:返回时,该 server 没有任何 worker 在 `processChain`
  中,且后续不会有新一轮 `processChain` 进入(直到 `Resume()`)
- `Server.Resume()`:解除 Quiesce,worker 在下一次 KICK 时正常处理
- 两者**只用于 snapshot 路径**,主流程 cold-start 不调

调用顺序见 §6.2 T2c。

## 13. 配置校验矩阵

### 13.1 冷启动校验(`ValidateCold`)

冷启动模式下 `ValidateCold` 必须强制以下规则,违反任一即报错并退出:

| 规则 | 错误消息 |
|------|---------|
| 所有 file:// URL 必须绝对路径(filepath.IsAbs) | "<field> file:// must be absolute" |
| `cgroup_path` 为空时,`controller` 必须为空 | "controller requires cgroup_path" |
| `cgroup_path` 为空时,`overhead` / `watermark_high` / `startup` 必须未设 | "<field> requires cgroup_path" |
| `cgroup_path` 为空时,`allocatable.cpu == capacity.cpu` | "fractional cpu requires cgroup_path" |
| `cgroup_path` 已设时,该路径必须存在(系统调用检查) | "cgroup_path <p> does not exist" |
| `cgroup_path` 已设但 `controller` 为空时,`startup` 必须未设 | "startup requires controller" |
| `controller` 已设时,`startup.memory` 满足 `floor ≤ ≤ capacity` | "startup.memory out of [floor, capacity]" |
| `allocatable.cpu ≤ capacity.cpu` 且均 > 0 | "allocatable.cpu must be in (0, capacity.cpu]" |
| `allocatable.memory ≤ capacity.memory` | "allocatable.memory must be ≤ capacity.memory" |
| `overhead.memory ≥ 0` | "overhead.memory must be non-negative" |
| `watermark_high.memory > 0` 且 `≤ allocatable.memory`(静态 cgroup / 动态控制模式启动初值) | "watermark_high.memory out of (0, allocatable.memory]" |
| `mounts[].target` / `files[].path` 必须绝对路径 | "<field> must be absolute" |
| `network.tap` 与 `network.tapfd` 最多设置一个;两者均未设置表示无 NIC | "network: `tap` and `tapfd` are mutually exclusive" |
| 无网络源时不得设置 `mac/ip/mtu/nexthop/hostname/interface` | "network.<field> requires network.tap or network.tapfd" |
| `mounts[].type` ∈ {tmpfs, empty}(省略 = empty);`nfs` 暂未实现 | "mount type <t> not yet implemented" / "unknown mount type <t>" |
| `files[].mode` 若设须为合法八进制 | "files[].mode invalid octal" |
| `init[].exec` 非空 | "init[].exec is required" |
| `launch.stop_signal` 若设须可解析为信号 | "unknown stop_signal <s>" |
| `launch.stop_grace_period` / `launch.start_timeout` 若设须为合法 duration | "<field> invalid duration" |

`allocatable.memory == capacity.memory` 时整个 balloon 设备不挂载
(`--balloon` 不出现于 CH 命令行),BalloonController 不启动。若
`deflate_on_oom` 显式写 true(默认值)不报错,记 warn 日志:
"deflate_on_oom set but balloon not configured"。

### 13.2 stdio flag 互斥(`run` 冷启动 / 恢复模式 / `exec` 同)

| 规则 | 错误消息 |
|------|---------|
| `--tty` 与 `--stdin` / `--stdout` / `--stderr` / `--stdin-from` / `--stdout-to` / `--stderr-to` 任一显式赋值互斥 | "--tty conflicts with explicit pipe-mode stdio flags" |
| 显式 `--tty`(或 `--tty=true`)但 stdin 或 stdout 不是终端 | "--tty requires stdin and stdout to be a terminal" |
| `--stdin=false` 与 `--stdin-from` 互斥 | "--stdin=false conflicts with --stdin-from" |
| `--stdout=false` 与 `--stdout-to` 互斥 | "--stdout=false conflicts with --stdout-to" |
| `--stderr=false` 与 `--stderr-to` 互斥 | "--stderr=false conflicts with --stderr-to" |
| `--console` 取值不是 `off` / `default` / `file=<path>` | "--console must be off, default, or file=<path>" |

### 13.3 snapshot / exec 子命令校验

| 规则 | 错误消息 |
|------|---------|
| snapshot:`--output` 与 `--upload` 必须二选一,不能都给或都不给 | "--output and --upload are mutually exclusive; one is required" |
| exec:`--sandbox-id` 必填 | "exec: --sandbox-id required" |
| exec:`--` 之后必须给出命令 | "exec: missing command" |
| exec:`--env` 必须是 `KEY=VALUE` 形式 | "--env must be KEY=VALUE" |
| exec:沙箱正在 quiesce(snapshot 进行中)时拒绝 | "exec: sandbox quiescing (snapshot in progress)" |

### 13.4 restore 模式校验(`SandboxConfig.ValidateRestoreHostConfig` + `restore.ApplyRules`)

`run --restore=<ref>` 触发。host yaml 先经 `ValidateRestoreHostConfig` 自校验
(可选网络源、引用格式等),与 snapshot.cfg 的交叉校验(capacity 相等、runtime/base
ref 与 marker 匹配)随后在 `restore.ApplyRules` 拿到 bundle 时进行;
config.json.net 提供快照 NIC 拓扑,restore.Run 在任何 tapfd 交接或 CH 启动前校验.
`restore.prefetch` 还会在 `restore.Run` 入口复用同一个 parser,保证普通
`run --restore` 即使未预先调用 validator,也会在副作用前拒绝非法值.额外:

| 规则 | 错误消息 |
|------|---------|
| `restore.prefetch` 只允许空值,`off`,`memory` | "restore.prefetch <value> invalid (want off\|memory)" |
| `memory` + self 无 Prefetcher 能力 | 不作为配置错误;按 §7.1 记录 `no_capability` 并继续正常 restore |
| 复合 `manifest://k1:k2` ref | 统一 ref parser 在打开 Stream 前拒绝;不进入 Prefetch task |
| sandbox.yaml 提供 `boot.runtime` 时,协议必须与 snapshot.cfg.runtime_ref 一致 | "boot.runtime scheme mismatch with snapshot.cfg" |
| sandbox.yaml 提供 `boot.root.base` 时,协议必须与 snapshot.cfg.base_ref 一致 | "boot.root.base scheme mismatch with snapshot.cfg" |
| sandbox.yaml 提供 file:// runtime / base 时,basename(filepath.Base)必须与 snapshot.cfg ref 中 basename 一致 | "<field> basename mismatch with snapshot.cfg" |
| sandbox.yaml 提供 file:// runtime 时,bundle marker 必须与 snapshot.cfg ref 中 `@sha256:<digest>` 一致 | "boot.runtime digest mismatch: sha256 marker mismatch ..." |
| sandbox.yaml 提供 file:// base 时,tarstream 的 scheme + digest 必须与 snapshot.cfg ref 中 `@sha256:<digest>` 或 `@hmac:<digest>` 一致 | "boot.root.base digest mismatch: ..." |
| sandbox.yaml 提供 manifest:// runtime / base 时,manifest key 必须与 snapshot.cfg ref 一致 | "<field> manifest key mismatch with snapshot.cfg" |
| sandbox.yaml 提供 `resources.capacity.{cpu,memory}` 时,与 snapshot.cfg 严格相等 | "capacity mismatch with snapshot.cfg" |
| `boot.root.overlay.diff` 可选;空→落盘 base 目录新建(随沙箱销毁) | — |
| `network.tap` 与 `network.tapfd` 最多设置一个;无源时不得设置其他 network 属性 | "network: `tap` and `tapfd` are mutually exclusive" / "network.<field> requires network.tap or network.tapfd" |
| config.json 有 NIC 当且仅当 sandbox.yaml 提供一个网络源;restore 不得增删 NIC | "network topology mismatch: ..." |

## 14. 已知限制与扩展点

按根因分三类。**§14.1** 是固有架构约束(本系统不做);**§14.2** 是平台 ABI
边界(用户应用要更多功能须自带 kernel);**§14.3** 是预留的扩展点。

### 14.1 固有架构约束

- **THP for sandbox 内存**:uffd 在 4 KiB 粒度操作,THP 把多页折成 2 MiB 大页
  会破坏 fault 路由。`MADV_NOHUGEPAGE` 在 mmap 之后立即下
- **confidential VM**(SEV-SNP / TDX):需要 `guest_memfd` 而非普通 memfd,
  跟 uffd 协议不兼容
- **跨节点 live migration**:page server / source-on-demand 协议是另一个产品
  级特性。本系统是单节点 snapshot + restore
- **多 memory zone**:NUMA / virtio-mem 用例;本设计是固定 spec、单 zone
- **多 sandbox 共享 sandbox-ctl**:per-sandbox 一进程是有意的架构选择(故障
  域、资源记账)

### 14.2 平台 ABI 边界(用户应用须自带 kernel)

guest kernel 是平台提供的最小镜像,以下功能默认不开。需要的用户 app 自带
vmlinux 通过 `boot.kernel: file://...` 提供:

- **in-guest USER / NET / UTS / IPC / TIME 命名空间**:相关 `CONFIG_*_NS`
  未启用,`unshare(CLONE_NEW*)` 返回 EINVAL
- **in-guest userfaultfd 系统调用**:`CONFIG_USERFAULTFD` 未启用
- **NR_CPUS 上限 4**

详见 `guest-runtime/docs/vmlinux.md` §3、§5。

### 14.3 扩展点(用例驱动)

| 扩展点 | 引入条件 | 影响章节 |
|---|---|---|
| **DISCARD with overlay.base layer** | 引入 manifest:// base + 本地 diff 部署形态 | 三态 stateMap(clean/dirty/discard,2 bits/block) |
| **memory hotplug / virtio-mem** | 弹性扩缩用例 | uffd 动态 register、状态表扩容 |
| **incremental snapshot** | 频繁 snapshot 同一 sandbox 的用例 | KVM_GET_DIRTY_LOG 接入 + log-mode CH 协调 |
| **应用 quiesce hook** | 跨实例去重率超过 kuasar-sandbox.md §4.6 量化的"非确定性 50-70%"上限的用例 | sandbox-init.md §3.4 quiesce 扩展项表 |

## 15. See Also

- [`sandbox-init.md`](sandbox-init.md) —— guest 内 sandbox-init 三阶段
  与 vsock 控制面协议
- `sandboxer/docs/cloud-hypervisor.md` —— CH patches、命令行选项、
  外部托管内存契约
- `guest-runtime/docs/vmlinux.md` —— guest kernel defconfig、平台
  ABI 边界
- `guest-runtime/native-deps/docs/build.md` —— 原生依赖(mkfs.erofs / vmlinux /
  envd)的构建流程
- `orchestrator/docs/node-resource.md` —— 资源控制协议规范、节点级仲裁、
  admission、reclaimer
- `accelerator/docs/manifest.md` —— manifest:// 资源拉取通道
  (blk0 base、snapshot)
- `accelerator/docs/cache.md` —— sandbox-ctl 通过 cache-ctl 客户端做
  chunk-level 请求
- `guest-runtime/docs/flatten.md` —— 构建 boot.root.base 的 EROFS 镜像
- `platform/docs/perf.md` —— 沙箱性能基线与密度调优
- `platform/docs/kuasar-sandbox.md` §2.4 / §3.3 / §4.6 —— 沙箱在系统中的
  位置与目标
