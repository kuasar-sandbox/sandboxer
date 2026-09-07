[English](sandbox-init.md) | [简体中文](sandbox-init_zh.md)

# sandbox-init — guest PID 1 ABI

`sandbox-init` is PID 1 inside every sandbox guest. It is built from
`sandboxer/cmd/sandbox-init` and packaged by `guest-runtime` into
`sandbox-runtime.bundle`. This document defines its ABI with the host-side
`sandbox-ctl`: boot-time rootfs assembly, the launch handshake, application
startup and supervision, stdio/console forwarding, and exec/attach/quiesce control.

For image packaging, bundled guest payloads, versioning and builds, see
[the runtime bundle guide](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/sandbox-runtime.md).
This file defines the runtime contract of the image's `/sbin/init` and how
`sandbox-ctl` communicates with it after starting the microVM.

<a id="1-概述"></a>

## 1. Overview

<a id="11-设计目标"></a>

### 1.1 Design goals

| Goal | Implementation |
|---|---|
| Fast startup | One static Go binary as PID 1; no systemd / dracut / busybox startup chain |
| Verifiable startup state | A fixed sandbox-init version; explicit launch, quiesce, restore and attach handshakes and state barriers |
| Sharing across sandboxes | virtio-pmem + DAX lets sandboxes share file-backed read-only pages through the host page cache |
| One VM, one primary app | Default private PID mode uses clone(NEWPID\|NEWNS), making the app PID 1 in its namespace; shared PID mode is also available |
| Observable lifecycle | Application start/exit notifications and explicit quiesce/restore exchanges use vsock; host VMM shutdown is a separate path (§5.4) |
| Separate app I/O | Application stdin/stdout/stderr, or a pseudoterminal, is forwarded over vsock separately from kernel dmesg |

<a id="12-系统中的位置"></a>

### 1.2 Position in the system

```
   HOST                                                      GUEST VM
   ----                                                      --------
   sandbox-ctl -- spawn CH --> cloud-hypervisor               kernel boot
                                     |                           |
                                     | virtio-pmem / DAX          | mount root, exec /sbin/init
   sandbox-runtime.bundle <----------| (shared host file)         | = sandbox-init
                                     |                           |   phase 1: mount + overlayfs + chroot
        kernel dmesg <-- --console ---| <-- hvc0 (virtio-con) -----|   phase 2: apply launch spec
        (host stderr/file capture)   |                           |             + wire app stdio
                                     |                           |   phase 3: supervisor loop
                                     |                           |
                                     |------ conn: launch ------>|   fork/exec primary app
                                     |       (conn -> MUX) <=====>|     app stdin/out/err <-> MUX
                                     |------ conn: ping -------->|     app is PID 1 in private mode
                                     |<----- conn: app_started --|
                                     |                           |
   /run/sandbox/<sid>/vsock.sock <----|<----- conn: app_exited ----|   app exits without restart
                                     |                           |   reboot(POWER_OFF)
                                     |<----- CH exits -----------|
```

<a id="13-不做的事"></a>

### 1.3 Non-goals

- **No container runtime:** the platform manages sandbox lifecycles directly,
  without introducing runc / crun / podman.
- **No systemd / OpenRC:** sandbox-init implements process supervision.
- **No busybox / util-linux dependency for init operations:** mounting, directory
  creation, chdir, chroot, reboot and openpty use Go syscall interfaces.
  The runtime also carries the separately built guest payload tools listed below.
- **No distribution rootfs supplied by the outer runtime image:** the workload
  image supplies its `/etc`, `/usr` and other rootfs content. sandbox-runtime
  supplies init, staging mountpoints and `/opt/sandbox-runtime`.
- **No stdio on ordinary management short connections:** each operation uses
  its own connection (§4.3). Launch, restore, attach and exec upgrade their
  connections to a long-lived MUX after the handshake (§4.5). At most one
  primary-app MUX exists at a time, created by launch and renewed by
  restore/attach. Each exec session has a separate, independently concurrent
  MUX lasting for that command (§3.6).

<a id="2-sandbox-runtimebundle-镜像结构"></a>

## 2. sandbox-runtime.bundle image layout

```
/sbin/init                Static Go sandbox-init binary
/proc/                    Empty mountpoint
/sys/                     Empty mountpoint
/dev/                     Empty mountpoint
/overlay/lower/           Empty mountpoint: blk0/vda EROFS, the overlayfs lowerdir
/overlay/upper/           Empty mountpoint: blk1/vdb ext4; upperdir/, workdir/, volumes/
/sysroot/                 Empty mountpoint: merged root and chroot target
/sysdisks/disk-{0..7}{,-lower,-upper}/
                          24 prebuilt data-disk staging mountpoints
/opt/sandbox-runtime/     Reserved guest payload root, bound into /sysroot in phase 1a
/opt/sandbox-runtime/bin/{envd,flatten-ctl,mkfs.erofs}
                          Target-architecture guest payload executables
```

Apart from `/sbin/init`, the mountpoint directories and
`/opt/sandbox-runtime/`, the outer image does not provide a distribution's
`/etc`, `/usr`, `/var`, `/lib` or shared-library tree. Init's own filesystem
operations do not require external utilities; the bundled payload tools have
their own application-facing roles.

`/opt/sandbox-runtime/` is the **guest payload root**. Platform runtime
components shipped with the runtime image live here. Their read-only file
pages can be shared across sandboxes through virtio-pmem + DAX, and their version
is tied to sandbox-init's runtime artifact. Phase 1a bind-mounts this directory
at the same path in the new root (§3.1). Applications see it read-only, hiding
any image content at that path (§5.2). `guest-runtime` builds the image with
`envd`, `flatten-ctl` and `mkfs.erofs` under its `bin/` directory.

The earlier approximate figures of a 10–15 MiB init binary, 15 MiB image and
10 MiB resident working set are historical estimates, not measurements or size
guarantees for the current payload-inclusive bundle. Measure the actual artifact
and workload. DAX shares the same backing file's read-only pages, not each
guest's private writable RAM. EROFS is endian-neutral: a host-native
`mkfs.erofs` can construct either target architecture's image, while init and
the guest payload executables must match the target architecture.

Build `sandbox-init` in this repository with `make sandbox-init`.
`guest-runtime` consumes that binary and packages it with
`make sandbox-runtime`. See its
[native build guide, §2.1](https://github.com/kuasar-sandbox/guest-runtime/blob/main/native-deps/docs/build.md)
for `mkfs.erofs`.

<a id="3-sandbox-init-三阶段"></a>

## 3. The three sandbox-init phases

<a id="31-阶段-1早期挂载--并发取-launch-spec--switch-root"></a>

### 3.1 Phase 1: early mounts, concurrent launch-spec fetch and switch-root

The launch spec contains fields such as `mounts`, including `empty` volumes,
that drive rootfs assembly. The **hello/launch handshake runs concurrently
with the spec-independent mount chain**. Its goroutine performs only socket
operations and no filesystem path lookup. The mount chain does resolve paths;
the safety condition is that the handshake does not use those paths and
finishes before the process-wide `chroot`. Go threads share `CLONE_FS`, so
switch-root must follow the join.

```
A. Before everything else (sockets, no rootfs dependency):
   - raw netlink RTM_NEWLINK(lo, IFF_UP)
       Kernel supplies 127.0.0.1/8 and ::1/128; no RTM_NEWADDR.
       Failure is fatal: a guest whose loopback cannot start is unusable.
   - AF_VSOCK bind+listen :5000
       The host-to-guest reverse listener must open before hello.
   - go handshake{ connect(CID=2:5000) -> write hello -> read launch }
       Return spec and conn through a channel.

B. Concurrent spec-independent mount chain:
   1. mount proc/sysfs/devtmpfs at /proc /sys /dev
      Skip /dev EBUSY when CONFIG_DEVTMPFS_MOUNT=y already mounted it.
   2. Wait for /dev/vda and /dev/vdb: stat polling, 10 s timeout.
   3. mount -t erofs -o ro /dev/vda /overlay/lower
      mount -t ext4 /dev/vdb /overlay/upper
   4. Create /overlay/upper/{upperdir,workdir} if absent.
   5. mount -t overlay overlay -o lowerdir=/overlay/lower,
        upperdir=/overlay/upper/upperdir,workdir=/overlay/upper/workdir /sysroot
   6. mkdir -p /sysroot/opt/sandbox-runtime
      mount --bind /opt/sandbox-runtime /sysroot/opt/sandbox-runtime
      The pmem payload root is projected into the new root. Its source loses
      pathname reachability after switch-root, but E's MS_MOVE carries this
      bind with the subtree, like D's volume binds. Its EROFS source is read-only.

C. JOIN: spec, conn := <-handshake
   Obtain LaunchSpec and the connection reused through launch_ack.

D. Spec-dependent work before switch-root (empty volumes need the raw ext4 source):
   for each mounts[].type == empty:
     mkdir /overlay/upper/volumes/<i>
       Raw ext4, alongside upperdir and separate from the overlay write layer.
     mkdir -p /sysroot/<target>
     mount --bind /overlay/upper/volumes/<i> /sysroot/<target>
       The empty directory hides image content at the target.

E. switch-root:
   MS_MOVE /proc /sys /dev -> /sysroot/{proc,sys,dev}
   chdir(/sysroot) -> MS_MOVE . / -> chroot(.)
   MS_MOVE carries the whole subtree: proc/sys/dev, B6's payload bind,
   and D's volume binds all become part of the new /.

F. Basic mounts after switch-root:
   mount -t cgroup2 cgroup2 /sys/fs/cgroup
   mkdir /sys/fs/cgroup/app             # app cgroup namespace and freezer root
   With launch.cgroup_control=true, also create /sys/fs/cgroup/app/init
     and strictly delegate advertised controllers (§3.2.1).
   mount -t devpts devpts /dev/pts -o newinstance,ptmxmode=0666
                                        # needed by tty-mode openpty
   mount -t tmpfs tmpfs /run -o nosuid,nodev
   mount -t tmpfs tmpfs /run/shm -o nosuid,nodev,mode=1777
                                        # automatic mounts, like /proc
```

All operations use syscall interfaces such as
`golang.org/x/sys/unix.Mount` and `unix.Chroot`, without external programs.

`/dev/vda`, the blk0 base, is the application's read-only EROFS image;
`/dev/vdb`, the blk1 overlay ext4, is the write layer. The merged
`/sysroot` becomes the final guest rootfs and then the new `/`.
Initially the kernel mounts the init-bearing `sandbox-runtime.bundle` through
virtio-pmem at `/` using `root=/dev/pmem0 ... rootflags=dax=always`.
Phase 1 replaces that view with the overlay, retaining
`/opt/sandbox-runtime` through the bind carried into the new root.

**Why concurrency is safe:** bind/listen must precede the host's launch write,
which starts the ping ticker. Otherwise an early probe can miss the listener.
The handshake does only socket work; the mount chain operates on the original
root's paths. There is no concurrent filesystem use by the handshake goroutine.
The process-wide chroot happens after JOIN, once that goroutine has exited.
This overlaps the hello→launch round trip with overlay assembly.

**Volume sources and relocation:** an `empty` volume's source is
`/overlay/upper/volumes/<i>` on raw ext4, separate from `upperdir/` but on the
same vdb and in the same disk capture. Its bind is installed under sysroot.
`MS_MOVE /sysroot → /` carries it with the whole subtree, just like
proc/sys/dev and the runtime payload bind; no separate move is needed.
After switch-root hides `/overlay/upper`, the bind retains an ext4 inode
reference, leaving volume content accessible at its target without exposing
the raw ext4 layout.

**Payload relocation and version pinning:** B6 binds the pmem payload root
under sysroot before switch-root. E carries it into `/`; the bind's pmem inode
reference keeps it alive after the old mount loses pathname reachability.
Before phase 2 forks the app, recursive shared propagation makes the mount
available in the app's private mount namespace, read-only. Because the payload
is part of `sandbox-runtime.bundle`, changing it changes that artifact's
identity. Restore checks the runtime footer identity against E/C0 and requires
the corresponding runtime artifact to be remapped; this is not a full payload
re-hash on every restore (see [sandbox lifecycle](sandbox.md)). With the trusted,
matching artifact, restored sandboxes retain the same payload version.
The payload shares the runtime artifact's lifecycle and cannot be independently
hot-patched; an independently versioned payload would need a separate read-only
EROFS device (§6).

**Single-disk mode** omits `boot.root.overlay`. The host creates no blk1,
passes one writable `--disk` for the root ext4 CoW device, and adds
`sandbox.root.layout=single` to the kernel command line. Phase 1a reads
`/proc/cmdline` after mounting proc. Since disk assembly runs before the launch
spec arrives, the layout must come from cmdline, not the spec.
This path waits only for `/dev/vda` and mounts it as ext4 directly at
`/sysroot`: no lower/upper staging, overlayfs or vdb. Payload binding,
switch-root and phase 1b's basic mounts remain the same. `empty` volume sources
move to `/sysroot/.sandbox-volumes/<i>` on the writable root itself, then bind
over their targets and move with the subtree. The single root disk is writable
ext4, with no EROFS image or embedded config.json, so host validation requires
`launch.exec` unless `launch.placeholder` is enabled.

**Data disks, `boot.disks[]`:** besides the root, at most
`MaxDataDisks=8` disks are supported. Each uses the same single-disk diff or
two-device overlay rules as `boot.root`, including `base_from_refs`.
The host prepares them through the same PrepareDiff / CoW / reader machinery.

```
Device order = CH --disk order = guest /dev/vd[a,b,c,...]:
  root (1 device or 2 for overlay)
    -> boot.disks[0] (1/2) -> boot.disks[1] (1/2) -> ...
Example: overlay root (vda=base, vdb=upper), single data disk (vdc),
         overlay data disk (vdd=base, vde=upper).

The host resolves mounts[].source (name) -> ordinal -> concrete /dev/vdX
into MountSpec.DiskIndex / DiskOverlay / DiskDevs. The name is not on the wire.
Device positions depend on boot.disks[] order, not mounts[] order.
```

`applyVolumeMounts` assembles these mounts **before switch-root**, using the
same prepare-source → bind-under-sysroot → move-subtree sequence as empty
volumes. Staging mountpoints are prebuilt into the runtime EROFS image:
`/sysdisks/disk-{0..7}{,-lower,-upper}`, 24 empty directories whose count must
match `config.MaxDataDisks`. Missing directories are fatal; rebuild the
mismatched runtime image or reduce the disk count.

```
Single data disk N:
  mount ext4 /dev/vdX -> /sysdisks/disk-N
Overlay data disk N:
  erofs ro -> /sysdisks/disk-N-lower
  ext4 rw  -> /sysdisks/disk-N-upper
  overlayfs(lower, upper/upperdir, upper/workdir) -> /sysdisks/disk-N
Both:
  bind /sysdisks/disk-N -> /sysroot<target>
```

The `/sysdisks` subtree becomes invisible after switch-root, but binds and
overlayfs lower/upper references keep its mounts alive, just as for the root
overlay and payload bind. **Restore** resumes the memory snapshot with guest
mounts already established; the host recreates and serves devices in the same
order. Explicit active diff bindings in restore host YAML's `boot.disks[]`
must match Sandbox E's count and order; restore prohibits replaying
`mounts[]`. **Capture** takes each writable diff, root and data disks, at the
same pause/quiesce point, creates Sandbox E with the complete disk graph,
then creates memory Snapshot S. S's `snapshot.cfg` references E through
`sandbox_ref` and records memory `from_refs`. Local flatten-merge operates
separately on E's disk layers and S's memory layers.

<a id="32-阶段-2spec-应用--stdio-接线--应用拉起"></a>

### 3.2 Phase 2: apply the spec, wire stdio and start the app

Phase 1's JOIN supplies LaunchSpec and the vsock connection that exchanged
hello/launch. Phase 2 applies the spec and writes launch_ack on **that same
connection**. It then remains open as the application's stdio MUX (§4.5).

`launch_ack` follows **all spec application, including init commands**.
It means the environment and initialization are prepared and the app is about
to be forked; it is the host's settled trigger, **not proof of successful app
exec or application health**. Filesystem-dependent spec steps execute serially
after switch-root.

```
1. If spec.network is present, applyNetwork(spec.network); otherwise skip.
   Cold start is additive configuration of an optional new interface:
     - Sethostname(network.hostname)
     - raw netlink RTM_NEWLINK(interface UP [+IFLA_MTU])
     - RTM_NEWADDR(IP/CIDR), optional RTM_NEWROUTE(nexthop)
   Cold-start failure is fatal. Restore with network uses best-effort
   flush-and-replace instead (§4.3).
2. applyFsMounts(spec.mounts where type==tmpfs)
   Empty volumes were installed before switch-root.
3. applyFiles(spec.files)
   Stage on tmpfs (/run/.inject), write content + chmod/chown, bind at target,
   optionally remount read-only, then MNT_DETACH staging. Content is memory-only,
   not written to vdb. Failure is fatal.
4. runInit(spec.init)
   Run one-shot commands sequentially, output to console.
   Per-command env/workdir/user and timeout are supported; timeout sends SIGKILL.
   Any nonzero exit or timeout is fatal (early initContainers-style setup).
5. Prepare app stdio from spec.stdio (§3.5):
   tty: openpty(), retain master/slave; initial winsize comes from the spec.
   pipe: pipe/socketpair per declared channel; undeclared stdin uses /dev/null.
6. write launch_ack{stdio: actual enabled channels}
   This connection is retained for MUX traffic.
7. read ack: host confirms the transition to MUX.
8. Start the first primary helper through exec.Cmd:
   SysProcAttr.Cloneflags = CLONE_NEWNS [| CLONE_NEWPID in default private mode]
     private: the app is PID 1 in its own PID namespace.
     shared: the app stays in sandbox-init's PID namespace; PID 1 reaps orphans.
   SysProcAttr.UseCgroupFD=true
   CgroupFD = O_CLOEXEC directory fd for /sys/fs/cgroup/app
     clone3(CLONE_INTO_CGROUP) creates the helper directly in real /app.
   SysProcAttr.Unshareflags includes CLONE_NEWCGROUP:
     Go's fork child unshares after clone3 and before re-exec; real /app becomes
     the cgroup namespace root. No anchor process is required.
   tty: Setctty + setsid; slave -> fd 0/1/2; close the child's master copy.
   pipe: child pipe/socketpair ends -> fd 0/1/2.
   argv: [/proc/self/exe, "exec-child", "bootstrap", isolated, cgroupControl,
          placeholder, cred, workdir, exec, args...]
   cred: "uid:gid:sg1,sg2", resolved from spec.user against guest /etc/passwd,
         or "-" for no credential drop.
   placeholder: identical helper setup, but finally waits for signals instead
                of executing an external program.
   env: spec.Env, adding default PATH if absent.
9. The helper waits at the internal SOCK_STREAM start gate until the parent
   registers its PID. In its private mount namespace it marks / recursively
   slave to prevent reverse propagation, unmounts inherited guest-global
   cgroup2 and remounts cgroup2 at /sys/fs/cgroup. The new cgroup namespace
   scopes real /app as /.
   Only with cgroup_control=true, the bootstrap helper writes 0 once to
   virtual /init/cgroup.procs, moving itself to real /app/init. This is the
   sole cgroup.procs placement write in sandbox-init's implementation.
   In private PID mode mount proc at /proc; shared mode retains the existing
   proc mount. Then wait at the ready gate.
10. At that ready gate, the parent:
    - pins /proc/<pid>/ns/cgroup with O_CLOEXEC for sandbox-init's lifetime;
    - with cgroup_control=true, verifies real /app/cgroup.procs is empty,
      enables each advertised controller from /app/cgroup.controllers in
      cgroup.subtree_control and reads back every item. Any failure aborts
      the helper before the final app runs;
    - sends go. The helper then chdirs to workdir, applies
      setgroups -> setgid -> setuid if cred != "-", and syscall.Exec's argv.
      Credential drop follows mount /proc and precedes execve.
    For placeholder: identical namespace/credential setup, then
      signal.Notify(SIGTERM,SIGINT) -> receive signal -> exit(0).
      Do not use select{}: Go detects a deadlock with no live goroutines.
    The placeholder is still supervised, in the app cgroup and snapshot
    freezer. The host forces restart=always, so killing it from exec restarts
    it in place. Guest PID 1's shutdown path sets shutdown before stopping it.
    Host VMM teardown is a separate path (§5.4).
11. Parent PID 1 waits for EOF on the internal CLOEXEC socket to confirm the
    final execve, then:
    - starts app-fd <-> MUX bridge goroutines (§3.5). App ends are replaceable
      by generation; an in-place restart drains the old output generation
      before installing new fds, with a bounded fallback (§3.3).
      The same MUX session and streams survive the restart;
    - short-conn dials host:5000, sends app_started{pid}, waits for ack, closes;
    - starts launch.plugin[] companions, then enters phase 3.
```

<a id="321-应用-cgroup-namespace-与-controller-拓扑"></a>

#### 3.2.1 Application cgroup namespace and controller topology

Real `/sys/fs/cgroup/app` is always both the **application cgroup namespace
root** and the **recursive snapshot freezer root**. Capture freezes/thaws
that path, including descendants; it does not call the application's or
envd's `/freeze`. sandbox-init and top-level one-shot `init: []` commands
stay in the guest-global cgroup root, outside `/app`.

`launch.cgroup_control` defaults to `false`. The real hierarchy and scoped
application view are:

```text
cgroup_control=false

Real guest-global                         primary/plugin/native exec view
/sys/fs/cgroup/                           /sys/fs/cgroup/ (real /app)
+-- cgroup.procs: sandbox-init, init[]     +-- cgroup.procs: primary/plugin/exec
+-- app/                                  +-- /proc/self/cgroup: 0::/
    +-- cgroup.procs: primary, restarts, plugins, native exec, descendants
         ^ namespace root + snapshot freezer root

cgroup_control=true

Real guest-global                         primary/plugin/native exec view
/sys/fs/cgroup/                           /sys/fs/cgroup/ (real /app)
+-- cgroup.procs: sandbox-init, init[]     +-- cgroup.procs: empty
+-- app/                                  +-- cgroup.subtree_control: all advertised
    +-- cgroup.procs: empty               +-- init/
    +-- cgroup.subtree_control: all       |   +-- cgroup.procs: primary/plugin/exec
    +-- init/                             +-- /proc/self/cgroup: 0::/init
        +-- cgroup.procs: primary, restarts, plugins, native exec, descendants
         ^ /app is the namespace/freezer root;
           /init contains sandbox-init-managed long-lived processes
```

True mode strictly enables and verifies the guest-global root's advertised
controllers before creating app subgroups. After bootstrap migration it does
the same at real `/app`; any missing controller fails startup. False mode
does not promise app-side delegation and retains only the guest-global root's
historical best-effort delegation.

The cgroup `/init` is **not** top-level `init: []` in sandbox.yaml.
The former contains long-lived app processes; the latter still runs once,
sequentially under sandbox-init before primary startup.
`cgroup_control=true` leaves an empty namespace root and delegated
controllers for an app-side manager. For example, envd's default
`/user`, `/ptys` and `/socats` become real `/app/{user,ptys,socats}`,
siblings of `/init`. There is no extra `/exec`; native exec shares the
primary's group.

Only the first primary uses the special bootstrap path. Primary restarts,
plugin starts/restarts and native exec's `exec-join` use the pinned cgroup
namespace fd and `clone3(CLONE_INTO_CGROUP)` to be **born in the final target**,
`/app` or `/app/init`. There is no Start → write-cgroup.procs migration window.
Setup, namespace, mount or handshake failures fail that launch. Passed namespace
and socket fds immediately regain CLOEXEC and are closed after use; the final
app does not inherit them. The app's ordinary `/sys/fs/cgroup` is the scoped
view above, without guest-global `/sys/fs/cgroup/app`. Inspection through
`/proc/1/root` in shared PID mode is the non-security-boundary exception below.

A cgroup namespace scopes the **cgroupfs view and management root**; it is not
an additional security boundary. In shared PID mode the app shares sandbox-init's
PID namespace and can see sandbox-init as PID 1. Its `/proc/1/cgroup` can even
show `/..` relative to the app's cgroup namespace. That does not contradict
the app itself seeing `/` or `/init`.

**Companions, `launch.plugin[]`:** after app bootstrap, sandbox-init starts
plugins sequentially as its own children, directly reaped by PID 1. They use
the same guest rootfs, network and app cgroup, and freeze with the app.
Output goes to the console; env/workdir/user are configurable. Each has its
own restart policy, never / on-failure / always (default always), with the
shared backoff scheme below. **Plugin exit never controls the sandbox
lifecycle**; only the primary's exit and `launch.restart` decide between
reboot and in-place restart. They are peers: even with a private-PID primary,
plugins remain in sandbox-init's PID namespace, sharing rootfs/network/cgroup
but not the primary's PID namespace. Each plugin is created in the final
cgroup target, then a lightweight re-exec helper joins the pinned cgroup
namespace in a private mount namespace and mounts scoped cgroup2.
Placement/setup/exec failures are plugin startup failures; there is no
fallback that runs a plugin outside snapshot freezing.

**Credential-drop timing:** do not apply the resolved app uid/gid as
`SysProcAttr.Credential` on the outer clone. A non-root child would fail the
new PID namespace's proc mount with EPERM. Instead, the child applies
setgroups → setgid → setuid after mounting proc and before execve.
`init[].user` can use Credential directly because init commands retain
PID 1's existing namespaces and do not remount proc. Named users such as
`nobody` are resolved guest-side, where the image rootfs's `/etc/passwd`
is authoritative.

**Mount propagation for restore injection:** `CLONE_NEWNS` gives the app
a private copy of the mount tree at fork. Later bind mounts made by PID 1
would not normally enter that namespace. Before forking, PID 1 marks
`/` `MS_REC|MS_SHARED`; before mounting proc, the child marks its copy
`MS_REC|MS_SLAVE`. PID 1 mount events then propagate one way into the app,
while app mounts such as proc do not propagate back. Cold-start injection
precedes fork and is inherited without this mechanism; restore injection
needs it. Network replacement does not: the app uses CLONE_NEWNS, not
CLONE_NEWNET, so it shares PID 1's network namespace and sees netlink changes.

**applyNetwork is not a listener prerequisite.** An absent LaunchSpec network
is skipped; loopback still exists. External app networking and the vsock control
plane are independent.

**Cold-start failures are fatal:** network, mount, file or init setup failure
must not silently launch an app with the wrong environment. The scheduler
needs a failed startup it can reschedule. Restore's best-effort clock/network
handling is a distinct contract (§4.3).

<a id="33-阶段-3supervisor"></a>

### 3.3 Phase 3: supervisor

```
loop:
  signal.Notify(sigchld, sigterm, sigint)
  select:
    sigchld:
      pid, status = waitpid(-1, WNOHANG)             // one reaper for all children
      if pid == app_pid(atomic):                    // primary application
        if !shutting_down && wantRestart(launch.restart, status):
          go restartApp(backoff)                   // asynchronous; reaper continues
        else:
          app_exit_then_reboot(status)             // never / clean on-failure exit
      elif pluginReg.onExit(pid, status):           // plugin policy + restart backoff
        // handled
      else:
        execReg.deliver(pid, status)                // exec child, or silently reaped orphan
    sigterm/sigint:                                 // signals received by guest PID 1
      shutting_down = true                         // prevent pending restart forks
      send spec.stop_signal to app_pid             // default SIGTERM
      wait spec.stop_grace_period (default 10 s); SIGKILL on timeout
      app_exit_then_reboot(status)

restartApp(backoff):
  sleep(backoff)                                   // reaper still handles plugin/exec exits
  return if shutting_down; wait while quiescing
  rewireApp:
    wait for every old app->host pump to reach EOF and write its final bytes to MUX
    after 2 s without EOF (e.g. an inherited writer), force-close old fds and continue
    install new-generation fds; pumps use the unchanged MUX streams
  phase2ForkApp:
    store new app_pid before releasing the helper
    helper is born in final cgroup target with CLONE_INTO_CGROUP,
    joins pinned cgroup namespace and mounts scoped cgroup2
    -> app_started{newpid}
  // host run connection survives and receives the new instance's output

app_exit_then_reboot(status):
  mark bridge shutdown, let output pumps consume natural EOF and flush (bounded)
  close remaining app ends after the drain; emit stream EOF where possible
  short-conn dial host:5000 -> app_exited{code, term_signal} -> wait ack(timeout) -> close
  reboot(LINUX_REBOOT_CMD_POWER_OFF)
  # Reboot even without ack; POWER_OFF lets CH exit cleanly with status 0.
```

**Backoff** is shared by apps and plugins in `supervise.go`: start at 10 ms,
double after each failed/short-lived run, cap at 60 s, and reset to 10 ms after
60 s of uptime. **Atomic app_pid:** the restart path registers the new PID before
releasing its helper, so the reaper does not misclassify its exit as plugin/exec.
**Snapshot gate:** quiesce sets quiescing and prevents new supervised forks while
plugins freeze with the app cgroup. Restore/attach reopens gates only after a
successful thaw; restartApp waits through that window. With restart=always,
app exit does not reboot the VM.

Generation drain covers app→host output only, without a stdin barrier. Output
ordering is retained within each MUX stream and a generation switch emits no
stream EOF, subject to the bounded forced-close fallback. Input arriving while
the app is between generations can be dropped. If shutdown catches a newly
installed generation, pumps consume that existing generation but do not wait
for a future one.

The terminal path uses **POWER_OFF, not RESTART**. When policy says the sandbox
has ended, CH should exit cleanly. RESTART would invoke CH's in-place VM reboot
and reconnect vhost-user-blk backends, which accept only one connection for this
single-VM lifecycle; failed reconnection makes CH exit unsuccessfully.

**The reverse listener runs in a separate goroutine** alongside the supervisor,
handling ping, restore, quiesce and attach, plus exec/connect sessions (§4.3–4.4).
It remains across cold start, capture/restore and MUX replacement until init
powers off.

**The mem_report goroutine** also runs alongside supervision. It samples
`/proc/meminfo` immediately after the launch barrier, then every 5 s by default.
Each observation carries an epoch and strictly increasing seq. Before
restore_ack, restore creates a new report epoch and clears the old pending
observation. Quiesce takes the same stream lock, drains an in-flight
guest→host exchange and pauses sampling before freeze. This is a capture
barrier: S must not retain a held reporter mutex waiting on an old host's vsock.
After thaw, restore resumes the new epoch; attach used for same-VM resume or
failed-capture recovery resumes the original epoch.

Reports contain MemAvailable plus diagnostic MemTotal, MemFree, Cached,
AnonPages and SReclaimable. They do not read or report balloon current.
Host Capacity must not be derived from MemTotal. The host combines the report
with CH `vm.info` to compute the sandbox-local Budget.

A failed send retains exactly the same epoch/seq/payload for the next ticker.
Only a successful ACK clears pending and permits another sample. EAGAIN,
timeout or a lost ACK does not permanently leave reporting in progress.
See [sandbox lifecycle](sandbox.md), §4.2 / §9.3.

<a id="34-quiesce-处理"></a>

### 3.4 Quiesce handling

Quiesce is the last guest cleanup opportunity before host `/vm.pause`.
It has two goals:

1. Freeze the app and synchronize filesystems so memory and disk artifacts
   correspond to one consistent capture point.
2. Tear down MUX and port-forward sessions before capture. A handshake or live
   connection whose peer is absent after restore must not be retained as a
   usable session (§4.6 / §3.7). Registry drains, bounded close handling and
   CH's restore transport reset cooperate to establish this boundary; an
   individual SO_LINGER return is not proof that all kernel socket state vanished.

Accepted management connections are handled concurrently. Exec/connect/report
registries and quiesce gates order their lifecycles. The host proceeds to
`/vm.pause` only after receiving quiesced, observing that control connection's
EOF, and completing its own admitted-handler drains.

```
0. Host atomically closes exec/forward admission and pauses the ping ticker.
   Drain admitted ping through pong plus guest EOF transport barrier.
   This drain has an independent 8 s quiesce budget, even if ordinary ping
   timeout is disabled. Expiry cancels and joins the exchange and fails capture;
   it does not preempt a normally completing transport before guest confirmation.
   Guest rejects new exec, SIGKILLs in-flight exec helpers, closes their lingered
   vsock sessions and joins complete session goroutines (§3.6).
   This drain is bounded: failure means no freezer entry and no quiesced.
0a. Guest drains the in-flight mem_report exchange and pauses reporting.
    The bounded exchange completes before S can retain a reporter lock or
    guest->host observation connection.
0b. Freeze the app:
    write /sys/fs/cgroup/app/cgroup.freeze = 1
    poll cgroup.events for frozen 1, within a bounded wait.
    Freezing before sync prevents new app dirty pages after sync.
    The recursive freezer covers /init, envd user/ptys/socats subtrees and
    children forked within the subtree; do not call envd /freeze.
1. [prep] sync(2)                         // flush dirty ext4 data
2. [prep] if quiesce.skip_drop_caches == false:
     open("/proc/sys/vm/drop_caches", O_WRONLY) -> write("3\n")
                                         // clean page cache plus dentry/inode cache
3. Park app-output forwarding as the MUX is closed.
   The app is frozen and residual output is bounded.
4. Tear down all connect forwards (§3.7):
   gate new connect; close each target and reverse-vsock connection.
   SO_LINGER is bounded (2 s) and best-effort; sessions close concurrently
   within the quiesce budget.
   Accept mode also closes and clears cached guest listeners, waking parked
   Accept calls; listeners are recreated lazily after resume.
   Host Forwarder only gates new admission before sending quiesce.
   After quiesced plus guest EOF, close remaining host halves and join all
   admitted exec/dial/accept/relay handlers before /vm.pause.
5. On the primary MUX: MUX_CLOSE -> MUX_CLOSE_ACK -> close(MUX).
   ACK wait is bounded at 5 s; timeout/error forces close.
   Host responds and immediately closes; guest uses bounded SO_LINGER to help
   drain vsock teardown. A dead connection is hard-dropped.
6. WriteMessage(quiesced) on the quiesce control connection.
7. Close that control connection; host must observe EOF.
```

**Why prep has this order:**

- **Sync first:** drop_caches discards clean cache; syncing first moves dirty
  data to disk so disk and memory capture refer to consistent contents.
- **drop_caches=3, when requested:** clean cache reflects the instance's access
  history and prefetch timing rather than dirty process state that restore
  requires. Dropping it can reduce resident cache in capture, at the cost of
  cold reads from the relevant backing filesystem/device after restore,
  including root and data disks. Measure that tradeoff for the workload.
  The default skips the write. Use `--drop-caches=true` explicitly to request
  it; freeze and sync still occur when cache dropping is skipped.

`quiesced.drop_caches_result` reports skipped / succeeded / failed.
An absent value means an older guest without result reporting, interpreted as
unknown. If skip was requested but the result is unknown, the host warns and
continues: an older guest may have dropped caches under its historical protocol.
The result describes this action, not a guarantee against reclaim by memory
pressure or ballooning.

**Extensions, not implemented:**

| Action | Proposal |
|---|---|
| App-level quiesce hook | A proposed sandbox.yaml `quiesce.signal`, empty by default, would signal the app and wait a fixed window, proposed default 100 ms. It needs explicit opt-in: an unhandled SIGUSR1 can terminate the app. There is no completion ACK or application-consistency guarantee. |
| Reset `/tmp` tmpfs | Add a phase-1 tmpfs mount at /tmp, then MNT_DETACH and remount at quiesce. |
| Close outbound connections | These are app-owned fds; sandbox-init has no general active-close authority over them. An application hook would be needed. |

**Excluded actions:**

- Do not close the vsock listener: restore/attach needs it.
- Do not terminate or signal the primary app. Quiesce freezes it recursively
  through cgroup v2; it is not SIGTERM and does not rely on stdout backpressure
  to stop execution.
- Do not reset the RNG/entropy pool here. Restore-time reseeding is not
  implemented (§4.3).
- Do not clean runtime logs such as /var/log; they are app-owned.

**Errors and guarantees:** drop_caches open/write failures are logged and
reported as failed, without failing capture. `sync(2)` has no returned error
value in this path; do not describe an unimplemented sync-error check.
Cache dropping is best-effort and affects capture size and later reads.

Freeze confirmation and the host's response/EOF/handler barriers are required.
If cgroup.events does not reach frozen 1 in time, the guest sends no quiesced.
Exec/report drain failure likewise prevents it. Primary MUX close has a bounded
ACK wait and hard-close fallback; SO_LINGER setup/close does not provide a
separately verified, unbounded socket-removal guarantee. CH's transport reset
handles residual connected-socket state (§4.6).
If quiesced is absent or cannot be written, capture fails. The host attempts
same-VM recovery and reattach; it continues running only if that recovery
succeeds, otherwise the host terminates the failed recovery. See §4.10 and
[sandbox lifecycle §6.2](sandbox.md).

<a id="35-应用-stdio--console-接线"></a>

### 3.5 Application stdio and console wiring

sandbox-init creates the application's stdin/stdout/stderr or pseudoterminal
inside the guest, then bridges it bidirectionally over MUX streams (§4.5).
The primary app does not inherit init's console fds, which point at
`/dev/hvc0`; its output is separated from kernel dmesg in both directions.

**The two modes are mutually exclusive**, selected by stdio in the launch
protocol (§4.4 / §5.1):

| Mode | Guest setup | App view | MUX streams |
|---|---|---|---|
| tty | openpty; setsid + TIOCSCTTY(slave); slave → fd 0/1/2; initial TIOCSWINSZ from spec | A real pseudoterminal, isatty=true, job control and SIGWINCH | Bidirectional PTY plus control frames |
| pipe | A pipe/socketpair per enabled channel; child ends → fd 0/1/2; absent stdin → /dev/null | Ordinary pipes, isatty=false | Enabled stdin host→guest and stdout/stderr guest→host, plus control frames |

Init retains the PTY master or its pipe ends and runs bridge goroutines.
SET_WINSIZE causes TIOCSWINSZ on the PTY master, and the kernel sends SIGWINCH
to the app's foreground process group.

**Backpressure:** if the host stops consuming output, for example a Ctrl-S
terminal, a blocked output destination or an absent MUX, credit is exhausted,
the guest pump stops draining, kernel pipe/PTY buffers fill and app writes
block. There is no intentional rolling-buffer/drop policy for normal output
backpressure. This is not an end-to-end losslessness guarantee across transport
failure, forced drain timeouts or the restart input gap (§3.3 / §4.5).

**Kernel dmesg** travels separately through virtio-console `/dev/hvc0`.
CH writes it to the stdout pipe provided by sandbox-ctl, which discards it,
writes it to stderr or writes it to a file according to `--console`
([sandbox lifecycle](sandbox.md), §2.2 / §5.2).
`--serial off` disables the 8250 UART.

<a id="36-exec-会话sandbox-ctl-exec"></a>

### 3.6 Exec sessions (`sandbox-ctl exec`)

`sandbox-ctl exec`, whose host CLI and ctl.sock are specified in
[sandbox lifecycle](sandbox.md), §2.5 / §6.3, starts a temporary command inside
an already running sandbox. It is a sibling of the app, without replacing it
or restarting the VM. The host sends exec{spec} over the reverse channel.
The guest prepares stdio, starts the command and replies exec_ack; that
connection then becomes this command's independent MUX. Each exec has its own
handler goroutine, and multiple sessions can run concurrently.

**Joining app namespaces and cgroup:** commands must use the app's mount, PID
and cgroup namespaces so ps, /proc and PID-addressed signals refer to the
app's process tree and filesystem view, analogous to docker exec.
setns(CLONE_NEWPID) affects only subsequently forked children, and mount-namespace
setns is thread-local. Performing those operations directly in long-lived,
multithreaded sandbox-init would be unsafe.

Instead, sandbox-init creates `exec-join` with
clone3(CLONE_INTO_CGROUP) against the pinned final target, registers its PID,
then releases its private handshake. The outer helper joins the pinned cgroup
namespace and selects the app PID namespace for its future child.
An inner re-exec child joins the app mount namespace, reuses its existing proc
and scoped cgroup2 mounts, resolves the image user, then finally execs.
Private PID mode requires both helpers: if the outer helper entered the app
mount namespace first, its proc mount would not expose the outer PID and
/proc/self/exe could not launch the inner child.

After mount join, the inner helper waits at a ready gate. The outer helper
then joins that same mount namespace and releases the inner final exec only
after success; it subsequently waits/reaps and forwards the exit code.
Any join failure prevents the user command from running, without changing
sandbox-init's own namespaces.

Both exec-join and the command inherit the final app cgroup from birth,
`/app` or `/app/init`, seen as `0::/` or `0::/init`.
There is no separate exec cgroup or later cgroup.procs placement.

**Exit and reaping:** after command exit, the guest sends EOF on all session
stdout/stderr/PTY streams, then EXIT_STATUS (exit code, or 128+signal), then
the MUX_CLOSE handshake (§4.6). The host obtains the exit code from the explicit
frame instead of relying on transport close. Exec completion never reboots
the sandbox. Primary exit follows its own restart policy (§3.3).
The sole reaper routes non-primary child exits to their exec session rather
than treating them as primary exit.

**Capture interaction:** before setup the inner helper sets a parent-death
signal. If dropping credentials can clear it, it resets the signal afterward
and uses a nonblocking EOF check on its private handshake socket to ensure the
original exec-join still lives before final exec. Killing that helper therefore
also kills the command tied to it. A mid-session MUX failure, including a lost
host exec client, causes the guest to SIGKILL that exec command.

At quiesce, the host first gates new requests and closes/joins registered exec
handlers. The guest then rejects new exec, kills in-flight exec helpers, closes
lingered session sockets and waits for the full fork/MUX/session cleanup.
Killing only the child is not a freeze barrier: unreleased process-wide fork
state or half-closed vsock state could otherwise enter S.
Resume/restore reopens admission only after thaw (§4.3).
An exec interrupted by capture is not automatically rerun.

<a id="37-connect-端口转发会话sandbox-ctl-run---connect"></a>

### 3.7 Connect port-forward sessions (`sandbox-ctl run --connect`)

Repeatable `--connect` pairs a host-local endpoint LOCAL with a guest endpoint.
**All four forms behave identically on the host:** listen on LOCAL, and for
each accepted local connection send connect{ConnectSpec} over a new reverse
connection. After connect_ack that connection becomes the forward's independent
long-lived fwd channel (§4.7), preserving TCP half-close.
The modes differ only in how the guest obtains its target connection:

| Form | Guest target action | Use |
|---|---|---|
| `LOCAL:host:port` | TCP Dial | Host client → already-listening guest TCP service |
| `LOCAL:/path`, `LOCAL:@n` | Unix Dial | Host client → already-listening guest UDS service |
| `LOCAL::host:port` | TCP Listen + Accept | Host client ↔ a guest TCP client that connects inward |
| `LOCAL::/path`, `LOCAL::@n` | Unix Listen + Accept | Host client ↔ a guest UDS client that connects inward |

**Syntax:** LOCAL ends at the first colon and cannot contain one itself.
It is either `fd=N`, an inherited already-listening socket wrapped with
net.FileListener before closing the original fd to avoid leaking it into CH,
or a UDS path. `@name` denotes abstract Unix addressing; stale filesystem
socket nodes are removed for non-abstract paths. A colon immediately after
the delimiter selects accept mode; otherwise the mode is dial.
A TARGET beginning with `/` or `@` is Unix, absolute or abstract.
Otherwise it is TCP host:port parsed by net.SplitHostPort, including
`[::1]:port`. Relative Unix paths are unsupported.
One local connection maps 1:1 to one reverse-vsock connection and one target
connection. Concurrent forwards remain independent, like exec sessions.

**Dial mode, `LOCAL:TARGET`:** on connect the guest dials network/address with
a 5 s limit, below proto.DeadlineConnect. Success returns connect_ack.
The host's DeadlineConnect covers the complete handshake.

```
Local client       sandbox-ctl run (host)        CH proxy       sandbox-init        Target
    | connect          |                                          |
    |----------------->| accept(UDS/fd)                           |
    |                  | DialRaw + "CONNECT 5000\n"                |
    |                  |-------------------------->| accept :5000 |
    |                  | connect{address:"127.0.0.1:49983"} ------->| net.Dial(tcp,address)
    |                  |                            |              |------------>| :49983
    |                  |<-------------- connect_ack (or error) ----|<------------|
    |<================== fwd frames (§4.7), TCP half-close ======================>|
```

**Accept mode, `LOCAL::TARGET`:** the guest lazily listens on address, caching
the listener by (network,address), and accepts one connection.
It sends connect_ack only after a guest client connects and Accept returns.
Accept can wait indefinitely. After sending connect, the host clears the
handshake deadline and parks waiting for ACK. It registers that pre-relay
reverse connection in Forwarder.pending so capture/shutdown can drain it.
The guest registers the parked session in connReg with no relay yet, promoting
it once Accept returns.

```
Local client       sandbox-ctl run (host)        CH proxy       sandbox-init        Guest client
    | connect          |                                          | lazy cached Listen(address)
    |----------------->| accept(UDS/fd)                           |
    |                  | DialRaw + "CONNECT 5000\n"                |
    |                  |-------------------------->| accept :5000 |
    |                  | connect{accept:true,address:"/run/up.sock"} -> Accept <-| connect
    |                  | (park without deadline)                  | connB ready |
    |                  |<---------------- connect_ack (or error) --|             |
    |<===================== fwd frames (§4.7), TCP half-close ==================>|
```

**Host leads the first pairing:** an accept-mode listener is bound only when
the first host connect{accept} reaches the guest, then remains until quiesce.
The host must therefore arrive first for the first pairing. Later the cached
listener's backlog can accommodate a guest client arriving first.
A guest client connecting before listener creation gets ECONNREFUSED.

**Why frame the data channel:** generic forwarding must preserve
shutdown(SHUT_WR): a peer that finished sending can still receive.
CH's userspace hybrid-vsock byte pump does not translate transport SHUT_WR
across the UDS↔vsock boundary; even peer closure may surface only on later I/O.
A raw byte relay relying on transport closure would lose half-close semantics.
Fwd instead encodes EOF/RST as in-band frame types that the proxy transports as
opaque bytes. This preserves explicit half-close on a functioning channel;
it is not a guarantee against transport errors. A forward has only one logical
stream, so no stream IDs or application-level windows are needed; kernel vsock
buffer backpressure supplies flow control.

**Capture interaction:** forwards are torn down with the app MUX (§3.4 step 4),
because a captured live forward has no peer after restore.
The guest gates new connects and closes target/vsock pairs using bounded
SO_LINGER for the latter. Accept mode also closes cached listeners, waking
parked Accept calls whose sessions are already registered, then clears the
cache. These listeners are not retained in S and are rebuilt lazily after
resume. The host gates new forwarding before quiesce and drains active relays
plus pending dial/accept reverse connections at the control/EOF barrier.
Resume/restore/attach reopens admission after thaw, including the guest listener
cache. Host forwarding listeners survive same-process quiesce; a separate
restore process must recreate/take ownership through equivalent `--connect`
arguments.

<a id="4-vsock-控制面--console-mux-协议"></a>

## 4. Vsock control plane and console MUX protocol

<a id="41-两类连接"></a>

### 4.1 Connection classes

```
(1) Management connection: a fresh connection for an operation's handshake.
    Ordinary operations finish request/response and close; no keepalive or multiplexing.
    hello/launch, app_started, app_exited, ping, mem_report, quiesce,
    restore, attach, exec, connect                                  (§4.3 / §4.4)
    Wire: [4B LE length][JSON].
    Launch has its explicit four-message handshake; upgrade operations retain the conn.

(2) MUX connection: one session's stdin/stdout/stderr, or a PTY.
    A launch / restore / attach / exec connection remains open after its ACK exchange
    and switches to framed mode                                    (§4.5 / §4.6).
    Primary app: at most one, created by launch and replaced by restore/attach.
    Each exec: an independent command-lifetime MUX; 0..N concurrent (§3.6).
    Wire: [stream:u8][type:u8][len:u16 BE][payload], per-stream flow control.

(3) Forward connection: one local connection paired with a guest endpoint.
    A connect handshake dials, or accepts for LOCAL::TARGET, then retains the
    connection after connect_ack as a fwd relay                     (§3.7 / §4.7).
    One per accepted local --connect connection; 0..N concurrent.
    Wire: [type:u8][len:u16 BE][payload], half-close, no application window.
```

Management operations are not multiplexed over the primary MUX.
Even while it is active, quiesce, app_exited and other operations use separate
connections and can proceed concurrently. A MUX carries only its session's
stdio/PTY and related control frames. Each exec has an independent MUX,
concurrent with the primary and other exec sessions.
Connect also upgrades its handshake connection, but uses the thinner,
single-stream fwd protocol, with no application window.

<a id="42-通道与寻址"></a>

### 4.2 Channels and addressing

Both directions use **port 5000**, with endpoints distinguished by direction:

```
                         sandbox-ctl (host)                       sandbox-init (guest)
guest -> host:           listen <vsock-base>_5000 UDS  <- CH <-   AF_VSOCK dial CID=2:5000
host -> guest:           dial <vsock-base> UDS         -> CH ->   AF_VSOCK listen :5000
                         write "CONNECT 5000\n"
                         drain "OK <port>\n"
Example <vsock-base>:    /run/sandbox/<sid>/vsock.sock
```

- **Guest→host:** guest connect(SockaddrVM{CID=2, Port=5000}); host accepts on
  `<vsock-base>_5000`. Carries hello/launch, whose connection upgrades to MUX,
  and app_started / app_exited / mem_report.
- **Host→guest:** host connects to the base UDS and its first write is ASCII
  `CONNECT 5000\n`, the CH hybrid-vsock preamble. CH returns
  `OK <port>\n`; the host must drain that line before reading protocol payload.
  CH proxies remaining bytes to the guest port-5000 listener. Carries ping,
  quiesce, restore/attach, per-command exec MUX and connect fwd sessions.
- The directions have independent addressing. Host→guest ping and
  guest→host app_started can run simultaneously on separate new connections.

<a id="43-管理操作集"></a>

### 4.3 Management operations

Each operation has its own connection and explicit exchange. Ordinary operations
have one request and response; cold launch has the four-message sequence below.
The last column describes whether the connection closes or upgrades.

| Operation | Dial direction | Sequence | After ACK | Purpose |
|---|---|---|---|---|
| Cold start | guest→host | `hello` → `launch{spec}` → `launch_ack{stdio}` → `ack` | MUX | Guest announces readiness; host supplies LaunchSpec; guest applies it and prepares stdio before launch_ack. |
| App start notification | guest→host | `app_started{pid}` → `ack` | Close | The app fork/exec handshake succeeded. |
| App exit notification | guest→host | `app_exited{code,term_signal}` → `ack` | Close | Terminal primary exit; guest waits for ACK up to its budget before powering off. Host uses this for its exit status. |
| Health probe | host→guest | `ping{id,t_send_ns}` → `pong{id,t_send_ns}` | Close | Host measures RTT, timeouts and failures (§4.9). |
| Memory report | guest→host | `mem_report{mem_report:{epoch,seq,mem_*}}` → `mem_report_ack` | Close | Guest observation; host validates epoch/seq and combines it with CH vm.info in the sandbox-local controller. |
| Before capture | host→guest | `quiesce{skip_drop_caches}` → `quiesced{drop_caches_result}` | Close | Freeze app, run prep and tear down sessions (§3.4); host then requires control EOF and its handler barriers before pause. |
| After restore | host→guest | `restore{epoch,wallclock_ns,network?}` → `restore_ack{epoch,stdio,app_state}` | MUX | After vCPUs resume, guest advances the report epoch, replies, reconnects MUX and thaws last. Host enables memory policy only after ACK/MUX setup. Clock/network updates are best-effort (§4.8); launch/files/init/plugins are cold-only and not replayed. RNG reseeding is not implemented. |
| MUX replacement | host→guest | `attach{epoch}` → `attach_ack{epoch,stdio,app_state}` | MUX | Close/drop old MUX and attach the new connection. If the guest is still frozen, thaw it before reopening gates, including same-process resume and failed-capture recovery; otherwise skip thaw. Attach and VM resume are distinct operations. |
| Execute command | host→guest | `exec{spec}` → `exec_ack{stdio}` | Independent MUX | Start a sibling command and wire its stdio; on completion send EXIT_STATUS then close (§3.6 / §4.6). Sessions can run concurrently. |
| Port forward | host→guest | `connect{spec}` → `connect_ack` | Fwd relay | Dial the guest target, or Listen+Accept with spec.accept. Accept may park indefinitely without a host ACK deadline. Each forward is independent and is torn down for quiesce (§3.7 / §4.7). |
| Error | Either | `error{msg}` | Close | Human-readable rejection where the handler sends one; malformed framing/JSON can instead terminate the connection. |

Attach is an internal mechanism of the existing sandbox-ctl run lifecycle,
not an interface for another process to take over a session. The current
integration calls reattach from capture resume/recovery. The protocol permits
replacement after a broken MUX, but there is no general autonomous background
reconnect loop for every unexpected MUX error.

The current guest echoes the request epoch in restore_ack/attach_ack and
reports `app_state:"running"` in those handlers. The schema defines an exited
state, but the handlers do not currently derive a live exited state or return
structured `exited{code,term_signal}`. ACK is not an application-health or
successful-thaw guarantee: thaw is attempted afterward and failure leaves the
launch/forward/report gates closed.

<a id="44-管理消息-wire-format-与字段"></a>

### 4.4 Management wire format and fields

The format is **[4-byte little-endian uint32 length][JSON payload]**.
`pkg/proto.MaxMessageBytes` is **1 MiB of JSON payload**, plus the 4-byte
prefix. This is distinct from the host ctl.sock exec request's 64 KiB JSON
limit ([sandbox lifecycle §6.3](sandbox.md)).
Low message volume makes readable JSON sufficient without protobuf tooling.

The following is an annotated schema sketch, not executable JSON; each message
populates only fields relevant to its type. The complete types in
[`pkg/proto/proto.go`](../pkg/proto/proto.go) are authoritative.

```jsonc
{
  "type": "<one of §4.3>",
  "phase": "ready",                    // hello: optional hint
  "launch": { ... LaunchSpec ... },    // launch, including stdio (§5.1)
  "exec": { "argv": [...], "env": {}, "cwd": "", "user": "", "stdio": {} },
  "connect": { "network": "tcp", "address": "127.0.0.1:49983", "accept": false },
                                       // accept=true: guest Listen+Accept
  "stdio": { ... },                    // *_ack: actual enabled channels
  "app_state": "running",              // current restore/attach handlers always send running
  "pid": 4711,                         // app_started
  "code": 0, "term_signal": 0,          // app_exited; separate scalar fields
  "id": 42,                            // ping/pong: monotonically assigned host ID
  "t_send_ns": 1715000000000000000,     // host UnixNano wall-clock value, echoed unchanged
  "skip_drop_caches": true,            // quiesce: preserve caches after freeze+sync
  "drop_caches_result": "skipped",     // quiesced: skipped/succeeded/failed; absent -> unknown
  "epoch": 3,                          // restore/attach uint32 request epoch, echoed in ACK
  "wallclock_ns": 1715000000000000000, // restore: best-effort CLOCK_REALTIME update; absent on attach
  "network": {
    "ip_cidr": "169.254.4.1/31", "mtu": 1450, "nexthop": "",
    "hostname": "c1", "interface": "eth0"
  },                                  // restore: optional best-effort flush-and-replace
  "mem_report": {
    "epoch": 2, "seq": 17,             // independent uint64 observation epoch/sequence
    "mem_total_bytes": 8589934592,
    "mem_available_bytes": 4294967296,
    "mem_free_bytes": 1073741824,
    "cached_bytes": 2147483648,
    "anon_pages_bytes": 536870912,
    "s_reclaimable_bytes": 67108864
  },
  "msg": "<reason>"                    // error
}
```

The codec uses encoding/json plus the small internal wireio helper, without
heavy serialization dependencies. ReadMessage rejects zero-length, oversized,
truncated or malformed JSON payloads. It uses json.Unmarshal: it does not
generically reject unknown fields or validate all type-specific requirements.
The guest ping handler echoes even zero/missing id or timestamp; the host
pinger validates response type and its expected ID.
The restore/attach request epoch is echoed, not an implemented generic
duplicate-request filter; memory reporting has its own epoch/seq validation.

<a id="45-mux-子协议"></a>

### 4.5 MUX sub-protocol

**Upgrade:** launch / restore / attach / exec retain their connection after
the specified ACK exchange and interpret following bytes as MUX frames.
At most one primary MUX exists, originating from launch and replaced by
restore/attach. Each exec has a separate MUX, independently concurrent.
The following stream sets, frames, flow control and close handshake apply
to all of them.

**Frame layout:**

```
Byte offset: 0       1       2               4                       4+len
             +-------+-------+---------------+-----------------------+
             |stream | type  | len (u16 BE)  | payload (len bytes)   |
             | (u8)  | (u8)  |               |                       |
             +-------+-------+---------------+-----------------------+

stream: 0=CONTROL, 1=STDIN, 2=STDOUT, 3=STDERR, 4=PTY
type:   DATA=0, EOF=1, RESET=2, WINDOW_UPDATE=3,
        SET_WINSIZE=4, MUX_CLOSE=5, MUX_CLOSE_ACK=6, EXIT_STATUS=7
DATA/EOF/RESET/WINDOW_UPDATE use the relevant data stream ID (1..4).
SET_WINSIZE/MUX_CLOSE/MUX_CLOSE_ACK/EXIT_STATUS use CONTROL (0).
len:    u16 wire cap = 65535 bytes; local DATA writes split at 32 KiB.
WINDOW_UPDATE payload: u32 BE credit delta.
SET_WINSIZE payload: cols:u16 BE, rows:u16 BE.
EXIT_STATUS payload: u32 BE exit code (128+signal); used only by exec flows.
```

**Stream set = PTY XOR pipe**, declared by the relevant ACK's stdio:

- PTY: CONTROL(0) + PTY(4). Terminal stdout/stderr are merged; there is no
  separate stderr stream.
- Pipe: CONTROL(0) plus the enabled STDIN(1), STDOUT(2), STDERR(3).
  A data or window-update frame for an unnegotiated stream is a protocol error.

**Direction conventions:** the host/guest bridges use these directions.
The shared Session codec is symmetric and does not independently enforce
host-versus-guest roles.

| Stream/frame | Host→guest | Guest→host |
|---|---|---|
| STDIN(1) | DATA app input; EOF host input close | RESET refusal; WINDOW_UPDATE credit |
| STDOUT(2), STDERR(3) | RESET refusal; WINDOW_UPDATE credit | DATA; EOF on final stream close |
| PTY(4) | DATA keyboard bytes; WINDOW_UPDATE credit | DATA; EOF on final stream close; WINDOW_UPDATE credit |
| CONTROL(0) | SET_WINSIZE; MUX_CLOSE_ACK | MUX_CLOSE; EXIT_STATUS for exec |

**Flow control:** each data stream has its own receive window, default
64 KiB, also used as initial send credit. Sending N DATA bytes consumes N
credits. With no credit, that stream's writer waits; other stream writers
can proceed. Reads replenish credit with WINDOW_UPDATE carrying the **data
stream ID** and a u32 delta, currently after consuming at least half the window.
There is no connection-wide application window. Local DATA writes are split
at 32 KiB and constrained by available credit, limiting individual writes.
This does not promise strict round-robin fairness. Control frames bypass data
credit, but all frames share a serialized underlying socket write; blocked
transport I/O can therefore delay control frames too.

**Detached output blocks:** without a current MUX during quiesce or pending
reattach, guest output pumps wait, kernel pipe/PTY buffers eventually fill and
app writes block. There is no deliberate ring buffer or “N bytes dropped”
output policy. Pump-held unwritten bytes and kernel pipe bytes are bounded
and can survive memory capture until a new MUX drains them.
Do not infer that a dead MUX's internal stream buffers are migrated or that
bytes already written to a failed transport are replayed: transport failure
has no end-to-end delivery ACK. Forced generation/shutdown drain timeouts can
discard residual output, and stdin in a restart gap can be dropped (§3.3).
The expected normal detached window is controlled and short; a caller that
never reattaches can leave output blocked indefinitely.

**Per-stream state rules:**

- Data/EOF/RESET/WINDOW_UPDATE on an invalid or unnegotiated data stream is
  a protocol error and tears down the whole MUX.
- DATA received after EOF or RESET is a protocol error, as is data exceeding
  the receive window.
- RESET marks the stream reset in both directions. The bridge determines
  local fd closure and resulting application EOF/SIGPIPE; the codec itself
  does not send an OS signal.
- A resource-level refusal can omit a stream during negotiation or RESET an
  enabled stream without tearing down other streams.
- EOF half-closes one direction: host EOF on stdin closes the app input writer;
  guest EOF on output denotes final close of that stream. Primary in-place
  restarts keep streams open rather than sending an EOF for every instance.

**SET_WINSIZE:** on host SIGWINCH, a CONTROL frame carries cols/rows.
The guest applies TIOCSWINSZ to the PTY master, causing a foreground-process-group
SIGWINCH. It matters only in PTY mode. The host sends initial size on entering
MUX, including after restore/attach.

<a id="46-mux-优雅关闭握手"></a>

### 4.6 MUX graceful-close handshake

The orderly-close protocol is a small application-layer FIN/FIN-ACK exchange,
followed by closing both sockets. **Guest initiates; host responds** in these
flows. Frames arriving while the guest awaits ACK are processed normally.
The host closes immediately after ACK; the guest then closes with bounded
SO_LINGER. Normal peer reset helps remove vsock state, but linger timeout or
socket-option failure must not be described as independently verified removal.

```
guest (sandbox-init)                                    host (sandbox-ctl)
  | -- MUX_CLOSE (CONTROL) ---------------------------> |
  |    stop new DATA writes                             |
  | <-- WINDOW_UPDATE / residual STDIN DATA may arrive  |
  | <-- MUX_CLOSE_ACK --------------------------------- | write ACK, immediately close
  |    ACK terminates the initiator read loop           | close -> transport reset
  |    close(MUX), with SO_LINGER bounded at 2 s         |
  |    normal reset completes teardown; timeout/error uses forced-close fallback
  v
listener remains; primary app remains alive (frozen during quiesce); no active MUX.
```

The host need not know the reason for closure: on MUX_CLOSE it writes ACK
through the serialized writer and closes. The current responder does not
implement an additional general application-buffer flush before ACK.

The guest treats MUX_CLOSE_ACK as terminal for its MUX read loop, then closes
in the calling flow. It must not issue another read after ACK: a raw vsock fd
can be reused by a newly accepted connection, and a stale reader could steal
the new connection's 4-byte proto length or MUX header.

**Why both close and linger matter:** a guest-only virtio-vsock close can leave
an 8 s deferred-removal state waiting for peer reset or timeout. Host close
after ACK supplies the peer teardown; guest SO_LINGER waits up to its configured
bound. Primary close waits at most 5 s for MUX_CLOSE_ACK and then forces close;
linger is 2 s, with setup failures logged. Exec MUX uses the same bounded
close approach, and its complete session drain precedes quiesced. The combined
host/guest barriers prevent admitted MUX, exec, forward and handshake handlers
from remaining live across pause; they are not an assertion that every kernel
socket remnant has been separately observed as removed.

That barrier alone cannot guarantee the guest kernel has no old connection
state. A short connection completed just before admission closed may already
have left its registry, and the final quiesced control connection closes only
after writing the response. CH restore recreates an empty Unix-vsock backend
rather than serializing its connection map. Platform CH patches enforce two
separate invariants: persist `local_port_last` to avoid reusing old tuples,
and prepublish `VIRTIO_VSOCK_EVENT_TRANSPORT_RESET` in the guest used event ring
at snapshot. Restore activation reissues the IRQ; Linux resets connected
sockets while CH gates backend RX until the guest event-queue kick acknowledges
processing. Listeners survive. The first restore REQUEST enters only after
that barrier. This removes reliance on sleeps for transport reset itself;
the separate bounded pre-request CONNECT retry still exists (§4.10).

**Three triggers use the same close handshake:**

1. Quiesce closes the primary MUX near the end of guest cleanup (§3.4 step 5).
2. Exec completion sends EOF on all output/PTY streams, then EXIT_STATUS,
   then MUX_CLOSE. The host learns the exit code from EXIT_STATUS but continues
   to process MUX_CLOSE, sends ACK and closes locally before returning and
   restoring the terminal. This local protocol barrier does not depend on peer
   close propagating through CH's proxy, where it may surface only on later I/O.
3. Attach tries to gracefully close the old MUX before attach_ack on the new
   connection. A dead old transport errors or reaches the bounded close timeout
   and is hard-dropped. The new MUX uses fresh default per-stream windows
   (there is no separate window negotiation field), initial winsize is resent,
   pumps resume and retained unwritten app output can flow.

**Unexpected transport loss** has no graceful exchange. The guest invalidates
the old session and its pumps wait for another attach/restore, with the listener
still active. The current host does not run a universal automatic reconnect loop;
capture resume/recovery explicitly invokes reattach, while other callers must
handle interrupted forwarding (§4.3 / §4.10).

<a id="47-connect-转发帧子协议fwd"></a>

### 4.7 Connect forwarding frame sub-protocol (fwd)

After connect_ack, the connection switches to shared `pkg/fwd` framing and
splices the local and guest-target connections in both directions.
One vsock connection carries one logical forward: no stream ID or application
window, with flow control supplied by the underlying kernel buffers.

```
Frame: +--------+---------------+------------------------+
       | type   | len (u16 BE)  | payload (len bytes)    |
       | (u8)   |               |                        |
       +--------+---------------+------------------------+
Wire payload cap: 65535 bytes; local DATA writes split at 32 KiB.
DATA(0): payload bytes.
EOF (1): sender finished writing; peer calls CloseWrite()/SHUT_WR on its plain
         connection but may keep sending in the other direction.
RST (2): abort both directions.
```

As in §3.7, CH does not translate SHUT_WR across its UDS↔vsock boundary.
The close event must therefore travel as in-band framed data.

**Half-close relay**, symmetric on host and guest: framed always means vsock;
plain means the local connection on the host and the target connection on the
guest.

```
plain -> framed: bytes -> DATA; clean EOF -> EOF frame; read error -> RST frame
framed -> plain: DATA -> plain write; EOF -> plain.CloseWrite(); RST/transport error -> abort
finish: close completely after both directions EOF; any abort closes both to unblock I/O
```

A peer can still receive after shutdown(SHUT_WR), until the other direction
also half-closes. Clean completion, internal error and external quiesce converge
through sync.Once, closing each underlying connection once. Double-closing a
guest raw fd could otherwise close an unrelated connection that reused its
number. Quiesce arms bounded SO_LINGER before guest vsock close; the wider
transport-reset contract remains §4.6.

<a id="48-时序"></a>

### 4.8 Timelines

In these diagrams, ordinary arrows are management exchanges and double arrows
are MUX traffic. “This conn → MUX” marks the retained connection's upgrade.
Host→guest dialing always means the CH base UDS plus CONNECT/OK exchange,
not a host dial to guest CID 2; CID 2 denotes the host from inside the guest.

**Cold start:**

```
sandbox-ctl                                               sandbox-init (guest)
listen <base>_5000
spawn CH -----------------------------------------------> kernel boot
                                                          bring up lo; bind/listen :5000
                      <--- hello{ready} ------------------ dial CID=2:5000 [conn A]
launch{spec,stdio} --------------------------------------> concurrent mounts, join, switch-root
start ping ticker after launch write                      apply network/mounts/files/init; stdio
                      <--- launch_ack{stdio} ------------- prepared, before final app exec
ack ----------------------------------------------------> conn A -> MUX
initial SET_WINSIZE =====================================> tty mode; fork/exec primary app
                                                          app fds = PTY slave or pipes; bridge up
                      <--- app_started{pid} [conn B] ------
ack; close conn B
ping -> pong; guest EOF [conn C..k]                        MUX stdio + WINDOW_UPDATE
                      <--- app_exited{code,term_signal} --- terminal exit; bounded output drain
ack ----------------------------------------------------> POWER_OFF -> CH exits 0
```

**MUX replacement, ATTACH:**

```
sandbox-ctl                                               sandbox-init
capture resume/recovery invokes reattach                   old MUX may be absent or broken
dial <base> UDS, CONNECT 5000, drain OK
attach{epoch} ------------------------------------------> gracefully close/drop old MUX
                      <--- attach_ack{epoch,stdio,app_state}
initial SET_WINSIZE =====================================> this conn -> MUX; resume output pumps
                                                          query frozen state; thaw only if frozen
                                                          after successful thaw:
                                                          reopen exec/forward/plugin/app-restart
                                                          and report gates
normal MUX flow                                           normal MUX flow
If frozen-state query or thaw fails, ACK may already have been sent;
guest logs the failure and leaves those gates closed.
```

**Quiesce → snapshot:**

```
sandbox-ctl                                               sandbox-init
pause/drain ping; gate new exec/forward
dial <base> UDS, CONNECT 5000, drain OK
quiesce{skip_drop_caches} -------------------------------> drain exec and mem_report first
                                                          freeze app; await frozen 1
                                                          sync; drop_caches=3 only if requested
                                                          tear down forwards / accept listeners
                      <=== MUX_CLOSE ==================== close primary MUX
MUX_CLOSE_ACK =========================================> ACK wait bounded, then lingered close
host immediately closes its MUX half
                      <--- quiesced{drop_caches_result} --- reply on control conn; close it
wait control EOF; join admitted host exec/forward halves
all required barriers complete -> /vm.pause, /vm.snapshot

Snapshot: listener remains, primary app frozen, no active MUX;
          CH local_port_last persisted;
          TRANSPORT_RESET prepublished in used ring, reset-pending state persisted.
Bounded guest close fallback and CH transport reset cover distinct responsibilities.
```

**Restore:**

```
sandbox-ctl                                               sandbox-init
CH restore activation re-signals snapshotted TRANSPORT_RESET (no new descriptor consumed)
/vm.resume OK; backend RX remains gated ----------------> reset connected sockets, retain listener
dial <base> UDS, CONNECT 5000
  REQUEST waits behind RX gate
                      <--- event-queue kick -------------- reset processed; CH releases RX gate
restore{epoch,wallclock_ns,network?} --------------------> best-effort clock_settime if >0
                                                          optional best-effort network replacement
                                                          advance paused mem_report to new epoch
                      <--- restore_ack{epoch,stdio,app_state}
initial SET_WINSIZE =====================================> this conn -> MUX; resume output pumps
                                                          thaw app last
                                                          reopen gates only after successful thaw
SafeTarget normalization; open observation epoch           new report epoch/seq resumes after barrier
(re)start ping ticker                                      listener unchanged across capture
Clock/network failures are logged and do not prevent ACK;
ACK precedes thaw and does not prove application health or successful thaw.
```

**The listener remains across capture.** Closing it during quiesce would leave
no receiver for restore/attach. Transport reset visits connected sockets without
closing bind/listen sockets. CH also resumes local port allocation from saved
local_port_last + 1. Resetting the transport epoch and keeping port allocation
continuous are separate responsibilities.

<a id="49-ping-健康探测"></a>

### 4.9 Ping health probes

Ping measures guest-agent liveness and response latency, **not application
health**. The default fatal threshold is zero, so failures record statistics
without automatically killing the VM. A positive `--ping-fatal-threshold`
enables a lifecycle callback that sends SIGTERM to CH after that many consecutive
failed probes, followed by host shutdown escalation if necessary.
It requires a bounded `timeouts.ping`.

| Event | Ticker state |
|---|---|
| Host finishes writing launch | Start; first probe runs immediately |
| Successful restore ACK/MUX setup | Start/restart |
| Capture admission closes | Pause; drain admitted ping through pong and guest EOF within the independent 8 s capture budget; expiry cancels/joins and fails capture |
| Successful same-VM recovery/resume | Resume after the required barriers |
| CH exits | Stop |

**Parameters:**

| Parameter | Value | Meaning |
|---|---|---|
| interval | CLI uses the 1 s default; package PingerConfig can override it | Ticker cadence. A blocking probe can delay processing, so it is not a guarantee of exactly 1 s between starts. |
| timeout | sandbox.yaml timeouts.ping, default not forcibly bounded; production profile 200 ms | Probe dial/write/read/guest-EOF budget. Package defaults alone use proto.DeadlinePing=200 ms; the lifecycle supplies its configured behavior. A positive fatal threshold requires a bound. |

Ordinary probe timeout and capture drain budget are independent.
An unbounded ordinary wait must not make export/snapshot's barrier unbounded.

**Actual statistics surface:** sandbox-ctl stats JSON exposes a `ping` object
with `attempts`, `success`, `timeout`, `dial_error`,
`rtt_avg_ns`, `rtt_max_ns`, `rtt_p50_ns`, `rtt_p95_ns` and
`rtt_p99_ns`, defined in
[`pkg/guestlink/launchclient.go`](../pkg/guestlink/launchclient.go).
Counts, average and max span the sandbox lifetime; percentiles use the last
256 successful samples. Names such as ping_attempts_total or ping_rtt_ms_p99
are not the fields exported by this implementation.

The wire `t_send_ns` is host UnixNano wall time, echoed by the guest.
RTT is computed using host `time.Since(tSend)`, retaining Go's monotonic
component, after the complete pong/guest-EOF exchange. It neither subtracts
the echoed wall-clock value nor requires guest clock synchronization.
Errors containing deadline / i/o timeout / timed out are classified as timeout;
other failures, including an unexpected pong type/ID, count as dial_error.
This is best-effort classification, not a typed network-error taxonomy.

<a id="410-失败语义"></a>

### 4.10 Failure semantics

**Guest, sandbox-init:**

- Listener accept errors are logged without killing init.
- Unknown message types receive error{msg}; malformed length/JSON reads are
  logged and closed without a guaranteed error response. Validation is
  operation-specific, not blanket unknown-field rejection. Ping with a missing
  ID is echoed as zero rather than rejected.
- MUX protocol errors, including unnegotiated streams, DATA after EOF/RESET,
  receive-window overflow or malformed required payloads, tear down the MUX.
  Guest pumps can await an explicit attach/restore replacement.
- The terminal primary path attempts app_exited before POWER_OFF; a failed
  notification or missing ACK does not prevent poweroff.
- Exec during quiesce, with empty argv, or with startup failure is rejected.
  Mid-session transport loss kills the exec command tied to its namespace
  helper through the parent-death mechanism (§3.6).
- Restore clock/network failures log and continue. Restore/attach thaw failure
  leaves gates closed even though ACK may already have been sent (§4.3).

**Host, sandbox-ctl:**

- Failures are handled by the owning operation. Ping has the statistics and
  optional fatal threshold above; there is no universal corresponding
  `*_error_total` metric or “never kill on control failure” rule.
- Before sending restore, if CH accepted CONNECT but returns EOF/reset before
  OK, the host retries connection setup with initial 25 ms backoff, for at most
  2 s within the total restore deadline. No restore payload has been sent at
  that stage. Once writing restore begins, any failure fails closed and the
  request is never replayed. Failure to obtain restore_ack terminates that
  restore through the CH VM shutdown path and returns an error.
- An unexpected MUX error can interrupt forwarding and leave app output
  backpressured. Current capture recovery/resume calls reattach explicitly;
  there is no general automatic reconnect loop or standalone takeover API.
- Missing quiesced or failure of the control-EOF/admitted-handler barriers
  aborts capture. The host attempts resume/reattach recovery; successful
  recovery leaves the same VM running, while failed recovery terminates it.
  “Capture failed” alone does not guarantee the VM continues.

Host items managed by sandbox.yaml `timeouts.*` use their configured value,
defaulting to no forced response deadline where documented; connection setup
still has its own bounds ([sandbox lifecycle §3.1](sandbox.md)).
Other budgets are protocol constants or guest-side waits:

| Message | Deadline/budget | Notes |
|---|---|---|
| hello | Guest dial retry budget 5 s; host initial read uses timeouts.app_notify, default no forced deadline | Early host-listener absence is retried with exponential backoff. |
| launch_ack, host wait | launch.start_timeout; empty/0 means unbounded | ACK follows all spec setup, including potentially long init commands. Set a production bound if a stuck guest must not wait forever. The connection then becomes MUX. |
| app_started | Guest sets 200 ms socket I/O timeouts after dial; dial has a separate 5 s retry budget. Host read uses timeouts.app_notify. | This is not a single 200 ms total dial+exchange deadline; raw AF_VSOCK uses per-I/O socket timeouts. |
| app_exited | Same as app_started | Missing ACK does not prevent POWER_OFF. |
| ping | timeouts.ping, default no forced bound; production profile 200 ms; capture drain at most 8 s | Ordinary failure updates ping stats; capture expiry cancels/joins and fails capture. Fatal threshold requires a bounded probe. |
| quiesce | 8 s | Exec/report drain, freeze, sync, optional cache drop, forwarding teardown and MUX close must fit the capture protocol. |
| restore | timeouts.restore, default no forced response bound | Connection setup waits for guest acceptance; pre-request EOF/reset retries at most 2 s inside the total budget. No replay after request write. Demand paging can lengthen restoration; connection then becomes MUX. |
| attach | 5 s | Covers the replacement handshake; connection then becomes MUX. |
| exec | 10 s | Guest forks/execs and resolves PATH before ACK. Handshake only; clear deadline for MUX command execution. |
| connect | 10 s for dial-mode handshake, including guest target dial ≤5 s | Clear deadline for fwd. Accept mode clears the ACK deadline after request write and may wait indefinitely; the pending connection remains registered for cancellation/quiesce. |
| mem_report | Guest has a separate 5 s dial retry budget and a 4 s post-connect exchange deadline; host read uses timeouts.app_notify | Immediate sample after launch barrier, then every 5 s; failures retain the same payload for retry. |

<a id="5-应用契约"></a>

## 5. Application contract

<a id="51-launch-配置launchspec"></a>

### 5.1 Launch configuration (LaunchSpec)

The host merges the OCI runtime config.json embedded in the ZIP trailer of
boot.root.base with sandbox.yaml's launch section: YAML overrides take priority,
with environment merging. It sends the resulting LaunchSpec over launch.
Host run flags --tty / --stdin / --stdout / --stderr and their -from/-to
variants determine stdio ([sandbox lifecycle §2.2](sandbox.md)).
A single ext4 root has no image config and needs an explicit executable or
placeholder (§3.1).

This is an annotated JSON example; comments are explanatory, not literal JSON:

```jsonc
{
  "exec": "/usr/bin/foo",
  "args": ["arg1", "arg2"],
  "env": {"PATH": "...", "HOME": "/root"},
  "workdir": "/",
  "restart": "never",                    // never|on-failure|always; in-place restart with backoff
  "cgroup_control": false,               // false: processes see /; true: empty delegated root, /init
  "placeholder": false,                  // true: no external exec; host rejects explicit launch.exec,
                                        // ignores image command and forces restart=always
  "share_pid": false,                    // true: sandbox-init's PID namespace (pid_namespace=shared)
  "user": "0:0",                         // uid:gid or name:group, guest /etc/passwd; empty -> root
  "stop_signal": 15,                     // signal number, resolved host-side; 0 -> SIGTERM
  "stop_grace_sec": 10,                  // guest PID 1's grace period; 0 -> default 10 s
  "network": {
    "interface": "eth0", "ip_cidr": "169.254.1.1/31",
    "mtu": 1500, "nexthop": "", "hostname": "my-sandbox"
  },
  "mounts": [
    {"target": "/tmp", "type": "tmpfs", "options": "nosuid,nodev,mode=1777"},
    {"target": "/var/log", "type": "empty"}
  ],
  "files": [
    {"path": "/etc/resolv.conf", "mode": "0644", "owner": "0:0", "content": "nameserver ..."}
  ],
  "init": [
    {"exec": "/bin/sh", "args": ["-c", "..."], "env": {}, "workdir": "/", "user": "0:0", "timeout_ms": 0}
  ],
  "plugins": [
    {"exec": "/usr/bin/sidecar", "args": [], "env": {}, "workdir": "/", "user": "0:0", "restart": "always"}
  ],
  "stdio": {
    "tty": true,                         // true: PTY; false: pipe mode
    "winsize": {"cols": 80, "rows": 24},  // initial PTY dimensions
    "stdin": false,                      // pipe: enable stdin, otherwise app fd 0 uses /dev/null
    "stdout": true,                      // pipe: enable stdout
    "stderr": true                       // pipe: enable stderr
  }
}
```

See §3.2 for mounts/files/init application. Empty volumes are established from
their raw ext4 source before switch-root (§3.1). Disk mount specs additionally
carry resolved disk_index/disk_overlay/disk_devs (§3.1).
FileSpec also supports read_only; omission leaves its bind writable.
These tmpfs-backed files avoid direct writable-disk injection, but processes
can copy them to disk and their memory may be captured; see the lifecycle
document's persistent/ephemeral disclosure boundaries.
The wire NetworkSpec key is **ip_cidr**, not the host YAML key ip.
`start_timeout` is absent from LaunchSpec because it controls only the
host's launch_ack wait (§4.10).

<a id="52-用户应用看到的环境"></a>

### 5.2 The application environment

- **PID 1:** in default pid_namespace=private, the app is PID 1 of its own
  CLONE_NEWPID namespace. In shared mode it is not PID 1; sandbox-init reaps
  its orphan descendants and remains visible as PID 1. Plugins always remain
  in sandbox-init's PID namespace, though they share rootfs/cgroup/network;
  a private-PID app cannot see them.
- **Cgroup namespace:** real /sys/fs/cgroup/app is always namespace/freezer root.
  With cgroup_control=false, primary/restarts/plugins/native exec live in
  real /app and see 0::/. With true, real /app has no direct processes and
  verified subtree delegation; those processes live in /app/init and see
  0::/init. This /init is a long-lived cgroup, not top-level init commands.
  Cgroup namespacing is a view/management scope, not an extra security boundary.
- **Mount namespace:** private, initially based on init's root view (overlay
  or single ext4), with its own proc mount in private PID mode.
- **Network:** with a configured network source, eth0 is virtio-net with a host
  TAP backend and cold-start IP configuration from init. Without a network
  source, no virtio-net is attached; loopback and vsock control still work.
  Restore network replacement is best-effort (§4.3).
- **/dev:** devtmpfs supplies null/random/urandom and other device nodes;
  /dev/pts uses devpts.
- **/run and /run/shm:** automatically mounted tmpfs, without declarations.
- **/opt/sandbox-runtime, reserved:** read-only bind of the runtime-bundled guest
  payload, hiding image content at that path. Apps may execute tools such as
  /opt/sandbox-runtime/bin/envd but should not store their own files there.
- **Declared mounts/files:** tmpfs and empty volumes are installed, with empty
  volumes hiding image content; injected files are bound at their targets
  before app startup and are unwritable if read_only is set.
- **Identity:** launch.user, default root; non-root execution has already
  applied setgroups/setgid/setuid.
- **Stdio:** in tty mode fd 0/1/2 share one PTY slave, isatty=true, with controlling
  terminal, job control and SIGWINCH; stdout/stderr are merged. Pipe mode uses
  the declared pipes, isatty=false, with /dev/null for absent stdin.
  These are not /dev/console or /dev/hvc0; primary output and kernel logs remain
  separate.
- **Vsock:** CID 3 and port 5000 are reserved by platform convention.
  This contract does not prohibit a privileged guest process from creating
  AF_VSOCK sockets; do not treat the reservation as a security boundary.
- **Balloon / memory hotplug:** managed by the platform without an app protocol
  requirement. Memory availability, reclaim effects and timing can still be
  observable to applications.

<a id="53-退出语义"></a>

### 5.3 Exit semantics

- **restart: never:** app exit → bounded MUX drain → app_exited notification →
  POWER_OFF → CH exit → sandbox-ctl exit and sandbox destruction.
  The reported app exit status or fatal signal determines sandbox-ctl's exit
  status when that notification is received; notification failure limits
  propagation (§4.10).
- **restart: always:** app exit triggers an in-place refork and rewireApp (§3.3).
  The VM stays alive and the same stdio MUX survives.
- **restart: on-failure:** nonzero or signaled exit restarts in place; clean exit
  follows never's notification/poweroff path.

An in-place restart forks within the existing guest lifecycle rather than
repeating the complete sandbox boot. Backoff is 10 ms→60 s, reset after 60 s
uptime. Shutdown suppresses further restarts, while a quiesce window delays
them until thaw (§3.3).

<a id="54-信号处理"></a>

### 5.4 Signal handling

- sandbox-ctl sends quiesce / restore / attach / ping over vsock. These messages
  do not directly signal the primary app.
- **Host shutdown and guest-app shutdown are separate.** On host SIGTERM/SIGINT,
  sandbox-ctl's lifecycle first cancels pre-spawn/controller work. With CH
  running, its shutdown path attempts ordered `vmm.shutdown` through the CH
  API after the memory.high preparation, falls back to SIGTERM to CH if needed,
  and escalates to SIGKILL after a 5 s shutdown grace; a second signal escalates
  immediately. This does not promise delivery of SIGTERM to guest PID 1 or
  completion of the app's stop grace. See
  [lifecycle shutdown](../pkg/sandbox/lifecycle.go) and
  [run signal handling](../pkg/sandbox/run_signal.go).
- **If guest PID 1 itself receives SIGTERM/SIGINT**, it sets shutdown,
  forwards launch.stop_signal (default SIGTERM, resolved from image/YAML
  policy) to the primary PID, waits launch.stop_grace_period (default 10 s),
  kills on timeout and powers off. These settings govern that guest path;
  they are not a guaranteed host VMM teardown handshake.
- In tty mode the host terminal is raw. Keyboard Ctrl-C (0x03) travels as a byte
  over the PTY stream, and the guest terminal line discipline turns it into
  SIGINT for the application. Host sandbox termination or terminal escape uses
  the separate lifecycle controls ([sandbox lifecycle §2.2](sandbox.md)).
- The proposed quiesce.signal hook (§3.4) is not implemented.

<a id="6-扩展点"></a>

## 6. Extension points

| Extension | When needed | Affected contract |
|---|---|---|
| App quiesce hook | Best-effort app preparation signal before freeze. The proposal lacks a completion ACK and cannot serve as a custom consistency barrier. | §3.4 extension table |
| App stderr side channel | Pipe mode already separates stdout and stderr into distinct MUX streams and host sinks; no extra vsock channel is needed. PTY inherently merges them, so independent PTY stderr would require a different I/O contract. | §3.5 / §4.5 |
| Custom vmlinux | Features absent from the platform kernel, such as particular nested userfaultfd or user/net namespace needs. The platform kernel already includes cgroup cpu/memory/io/pids, NFS v3/v4 and FUSE. | sandbox-ctl boot.kernel: file://...; verify the actual kernel feature/config contract |
| Custom sandbox-runtime | Special PID 1 or supervisor requirements, uncommon. | Preserve the guest ABI, architecture and artifact-identity requirements. A compatible read-only custom runtime can still share DAX pages; customization does not inherently disable sharing. |
| Independent payload device | Guest payloads such as envd need a release lifecycle independent of runtime. | Proposed §2 / §3.1 change: bind from a separate read-only EROFS device, adding a virtio disk and vhost backend. This is not current independent hot-patching support. |

## 7. See Also

- [sandbox lifecycle](sandbox.md), §2.2 (run tty/console/stdio flags),
  §5.2 (CH cold-start command), §6.2 / §6.3 (capture ordering and ctl.sock),
  and §7 (restore).
- [guest kernel guide](https://github.com/kuasar-sandbox/guest-runtime/blob/main/docs/vmlinux.md):
  namespaces, filesystems, virtio-console and networking features.
- [Cloud Hypervisor guide](cloud-hypervisor.md), §5.2: hybrid-vsock CONNECT
  addressing and console/serial options.
- [guest-runtime native build guide](https://github.com/kuasar-sandbox/guest-runtime/blob/main/native-deps/docs/build.md),
  §2.1: mkfs.erofs for make sandbox-runtime.
- [project system design](https://github.com/kuasar-sandbox/kuasar-sandbox/blob/main/docs/kuasar-sandbox.md),
  §4: user-visible template instantiation, pause and restore semantics.
