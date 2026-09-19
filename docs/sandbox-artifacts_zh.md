[English](sandbox-artifacts.md) | [简体中文](sandbox-artifacts_zh.md)

# 沙箱工件 — 便携配置、引用与发布

本文完整定义持久 Sandbox E / Snapshot S、便携配置、父引用、身份和载体发布规则。[运行规范](sandbox_zh.md)定义冷启、执行、冻结、捕获、恢复和清理的动作顺序；[Guest ABI](sandbox-init_zh.md)定义 Host–Guest 协作。通用文件载体字节格式归 [accelerator 文件工件](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/file-artifacts_zh.md)，不在此重复定义。

**阅读顺序**

- [逻辑角色](#1-逻辑角色)
- [逻辑角色与物理 carrier](#2-逻辑角色与物理-carrier)
- [PortableSandboxConfig](#3-portablesandboxconfig)
- [Strict encoding 与 limits](#4-strict-encoding-与-limits)
- [C0、C1 与 source binding](#5-c0c1-与-source-binding)
- [`self` 与 disk provenance](#6-self-与-disk-provenance)
- [`.sandbox` logical format](#7-sandbox-logical-format)
- [`.snapshot` logical format](#8-snapshot-logical-format)
- [Provenance、publish 与 carrier](#9-provenancepublish-与-carrier)
- [Atomicity 与 determinism](#10-atomicity-与-determinism)
- [Incompatibility](#11-incompatibility)




## 1. 逻辑角色

系统区分 4 个逻辑角色:

| Role | Suffix | Logical content | Purpose |
|---|---|---|---|
| Container image | `.image` when materialized | `EROFS + ZIP(config.json)` | 只读容器 rootfs 与 OCI image defaults |
| Disk overlay | `.overlay` | sparse block payload | 保存root/data disk可写层 |
| Sandbox E | `.sandbox` | root payload + strict ZIP | 可重复冷启动的完整 portable workload |
| Snapshot S | `.snapshot` | memory payload + strict ZIP | 恢复已运行进程和 VMM/guest memory state |

`E` 表示 runnable Sandbox artifact,`S` 表示 memory Snapshot. 两者职责不可互换:

```text
explicit sandbox.yaml ── run --config ──> cold start
Sandbox E             ── run --from   ──> cold start
Snapshot S ── sandbox_ref ──> E
           └──────── run --restore ────> memory restore

live export              ──> E
image-to-Sandbox-E build ──> E
memory snapshot          ──> E + S, S is operation root
```

不存在以下模型:

- 不存在 `snapshot --memory=false`.
- 不存在 zero-size memory Snapshot.
- 不存在 `run --restore` 的 cold-boot 分支.
- `snapshot.cfg` 不保存 disk graph、launch、mounts、files 或 init.
- cold boot 不包装成 restore.



## 2. 逻辑角色与物理 carrier

逻辑内容与物理 carrier 正交. 同一个 `.image`、`.overlay`、`.sandbox` 或 `.snapshot` logical source 可以由以下 carrier 承载:

| Carrier | Reference | Notes |
|---|---|---|
| Local tarstream | `file://<basename>@digest:<digest>` 或 `@hmac:<digest>` | sparse map 由 tarstream envelope 权威声明 |
| Manifest | `manifest://<key>` | chunk/Manifest encryption 和 verification 由 manifest config 控制 |
| Manifest Bundle | `file://<bundle>@manifest:<key>` | 一个 ZIP64 Bundle 可承载 root 及完整依赖 Manifest graph |

Local 模式物化的内容寻址文件名为 `<digest>.<role>`,其中immutable root carrier使用`.image`,`.overlay`只表示单独物化的可写disk layer. 当前root writable top已经是Sandbox E的payload,不会再复制为`.overlay`. Provisioned container image输入仍可使用`.erofs`等显式basename. `<sid>.sandbox` 和 `<sid>.snapshot` 是成功 commit 后更新的语义 symlink. Bundle 模式下语义 symlink 指向承载 root Manifest 的 `<key>.bundle`. Alias 的 SID 必须是单一安全 path component;commit 使用临时 symlink + atomic rename,并拒绝覆盖已有 regular file 或 directory. 这些是节点本地 output 语义;named ref-location 使用 §9.2 的独立 shared-location commit protocol,不创建 alias.

Block backend、restore 和 publisher 先打开 carrier,再按逻辑角色解析内容. 外层 ZIP magic 只说明 carrier 是 Bundle,不说明 logical role.



## 3. PortableSandboxConfig

唯一 portable 文件名是:

```go
const SandboxRuntimeConfigName = "sandbox.runtime.cfg"
```

同一 canonical bytes 写入:

```text
<run_dir>/sandbox.runtime.cfg
.sandbox trailing ZIP/sandbox.runtime.cfg
```

Portable schema V1:

```yaml
version: 1
resources:
  capacity: { cpu: 2, memory: 2GiB }
  allocatable: { cpu: 1.5, memory: 1536MiB }
network:
  enabled: true
  interface: eth0
boot:
  kernel: file://vmlinux@digest:<digest>
  runtime: file://sandbox-runtime.bundle@digest:<digest>
  cmdline: "console=hvc0"
  root:
    base: file://root.erofs@digest:<digest>
    overlay:
      base: self
      base_from_refs:
        - file://parent.overlay@digest:<digest>
  disks:
    - name: data
      base: file://data.overlay@digest:<digest>
launch:
  exec: /usr/bin/app
  args: [serve]
  env: { MODE: production }
  restart: never
mounts:
  - { target: /data, type: disk, source: data }
files:
  - { path: /etc/app/config, content: "portable" }
init:
  - { exec: /bin/sh, args: ["-c", "prepare-data"] }
metadata: { workload.kind: api }
```

Portable 内容包括:

- `resources.capacity` 和 workload 默认 `resources.allocatable`;
- `network.enabled/interface` topology fact;
- kernel/runtime basename + content identity;
- boot cmdline;
- root/data immutable graph,以及 data disk name/order/mount topology;
- persistent launch/plugin/mounts/files/init/metadata.

Portable 内容排除:

- cgroup path/fd/controller、overhead/watermark/startup、`resources.diff_cow` 缓存策略;
- TAP/TapFD provider、helper、socket以及 IP/MAC/hostname;
- CH binary、run-root/base-root、active diff path;
- manifest/customer key、crypto secret、access token、ref-location host path;
- restore prefetch、host protocol timeout;
- stdio/forward endpoint;
- `ephemeral_files` 和 `launch.ephemeral_env`.

Local immutable ref 必须是 basename + identity,例如:

```text
file://vmlinux@digest:<digest>
file://sandbox-runtime.bundle@digest:<digest>
file://<digest>.image@digest:<digest>
file://<digest>.overlay@hmac:<digest>
file://<digest>.overlay@digest:<digest>
```

目标 host 使用实际 path、source directory 或 named ref-location 在 controller、network、cgroup、VM 或 run directory 副作用前完成 binding。验证依输入而异：显式 cold projection 会计算 kernel hash；`run --from` 与 restore 只检查 kernel 的绝对 file binding、存在性和 regular-file 类型，刻意跳过完整内核重哈希，同时将 runtime Bundle footer identity 与 C0 比较。因此后两条路径不独立证明所给内核匹配 E 中记录的 digest，内核部署仍是受信 host 的责任。Carrier metadata 和按配置启用的内容验证保留各自检查。源码见 [lifecycle.go](../pkg/sandbox/lifecycle.go) 与 [restore.go](../pkg/restore/restore.go)。



## 4. Strict encoding 与 limits

Portable config 使用 deterministic YAML marshal 和 strict known-fields parse。Artifact reader 拒绝 unknown field、unknown version、duplicate key、YAML alias/merge key、多 document、非法 ref 和 non-canonical bytes。单独 schema parser 可以接受有效但非 canonical 的 YAML；`sandboxfile.Open` 在重新 marshal 后检查字节相等。

V1 limits:

| Item | Limit |
|---|---:|
| Raw/decoded portable config | 1 MiB |
| `files` count | 256 |
| One inline file content | 256 KiB |
| Total inline file content | 1 MiB |
| Environment entries | 512 |
| Mounts | 128 |
| Init commands | 64 |
| Plugins | 64 |
| Metadata entries | 256 |
| Scalar bytes | 64 KiB |
| Refs per layer chain | 64 |
| Total artifact refs | 256 |
| Data disks | 8 |

上述限制同时生效：strict YAML 的 64 KiB scalar-byte 限制也约束 inline `content`；独立的 256 KiB 单文件上限不覆盖它。

`<run_dir>/sandbox.runtime.cfg` 用 same-directory temp、0600、file fsync、no-replace rename 和 directory fsync 写入。同一 lifecycle 只允许 write once；已有任何 final path 都会失败，即使 bytes 相同也不能重复写入。源码见 [portable.go](../pkg/config/portable.go)。



## 5. C0、C1 与 source binding

`C0` 是一次 run lifecycle 的 immutable portable baseline:

```text
explicit cold config + persistent fields + canonical identities
  - host-only - ephemeral = C0

E.sandbox.runtime.cfg + allowed persistent overrides
  + canonical host bindings - ephemeral = C0 for run --from

E.sandbox.runtime.cfg non-boot defaults + allowed persistent overrides
  + complete replacement boot - ephemeral = a new explicit-cold C0

S.sandbox_ref -> E.sandbox.runtime.cfg
  + allowed restore host bindings = C0 for run --restore
```

`C0` 在 VM side effect 前写入 run directory,之后字节不变. Live runtime 另持有不序列化的 `RunSourceBinding`:

- current `self` 对应的 source E/root artifact;
- root payload 与 optional image config;
- resolved local path/source directory;
- Bundle reader/fetcher/source refs;
- named ref locations;
- host-only disk bindings和当前 active diff.

Memory restore另持有 `MemorySourceBinding`,其中 Snapshot S identity 与 `from_refs` 只管理 memory provenance.

`C1 = Export(C0, disks at T)` 只写入新 E. `export --resume` 和 `snapshot --resume` 不修改 C0、active diff、lower graph 或 memory parent. 因此同一 VM 可以产生 `E1@T1` 和 `E2@T2`,同时继续使用原 C0 + writable diffs.



## 6. `self` 与 disk provenance

Portable graph 必须恰好出现一次保留值 `self`,且只能位于:

```text
boot.root.base
boot.root.overlay.base
```

Data disk 和所有 `base_from_refs` 禁止 `self`. 三种合法 root 关系:

```yaml
# Direct EROFS Sandbox E.
root:
  base: self
  overlay: {}

# E payload is the ext4 upper top.
root:
  base: file://root.erofs@digest:<digest>
  overlay:
    base: self
    base_from_refs: []

# Single-disk ext4.
root:
  base: self
  base_from_refs: []
```

`run --from` 通过 source binding 把 `self` 绑定到当前 E payload. Export C1 先把 C0 的旧 `self` 物化为原 source ref,再把新 root payload 位置设为 `self`;这避免 E digest 自引用循环.

`run --from --replace-boot` 不建立 source binding. 来源 E 在读取 non-boot defaults 后即不再参与 run；新的 root active writable layer 经普通 explicit-cold projection 占据唯一 `self`. 后续 export/snapshot 因此不会把来源 E 当作 disk parent 或 dependency.

父 `.sandbox` 出现在 disk-ref 字段时只提供 `Payload`:

- ext4 upper/lower 或 data disk:忽略其 `sandbox.runtime.cfg` 和 `config.json`.
- root EROFS base:block backend 仍只消费 Payload;host 可以读取 optional `config.json` 作为 image defaults,但绝不采用父 E 的 portable config.

当前 E 必须显式列出它真正依赖的 lower/data refs. Publisher 不因扩展名 `.sandbox` 而递归父 E 的 config graph.

Local merge 分两类:

- Disk merge只作用于 E 的 sparse disk layers.
- Memory merge只作用于 S 的 memory layers.

三态 sparse 语义保持 `Hole`、`Zero`、`Data`. Hole 只来自 authoritative metadata;不得扫描 zero bytes 发明 Hole.



## 7. `.sandbox` logical format

只接受两种 layout.

Live ext4/sparse root export:

```text
[root sparse payload]
[ZIP:
  sandbox.runtime.cfg
]
```

Top-level Sandbox E direct EROFS layout:

```text
[EROFS payload]
[ZIP:
  config.json
  sandbox.runtime.cfg
]
```

Image-to-Sandbox-E assembly定位原 `EROFS + ZIP(config.json)` 的 archive base,原样保留 EROFS prefix 和经过校验的 `config.json` bytes,然后重建一个 canonical two-entry ZIP. `ZIP(config.json) + ZIP(sandbox.runtime.cfg)` 是非法 double ZIP.

Reader 从 ZIP structure 推导 `[0, archiveBase)` payload,不信任第二个 `payload_size`. Strict ZIP contract:

- EOCD 必须位于 logical EOF且 comment 为空;
- 拒绝 ZIP64 和 multi-disk ZIP;
- exact entry set只能是 `{sandbox.runtime.cfg}` 或 `{config.json,sandbox.runtime.cfg}`;
- duplicate、unknown、directory、path traversal entry 均拒绝;
- method固定 Store;
- fixed order/time/metadata;
- entry size有界;
- CRC、local header、central header必须一致;
- malformed/truncated input在 VM side effect 前失败.

Opened root 提供以下 view；实际类型还包含解析后的配置和 archive boundary 等字段：

```go
type Root struct {
    FullStream    fetch.Stream
    Payload       fetch.Stream
    ImageConfig   []byte
    RuntimeConfig []byte
}
```

`Payload` 是保留 sparse `RunAt`/`ReadAt` 的 section view. Shared close owner保证 FullStream/Payload/Root 任一 close 最终只释放 carrier 一次. Vhost 只能看到 Payload,不能看到 ZIP tail.

Live BlockCOW SnapshotView 是 upper-only sparse delta,ext4 superblock offset 可以是 Hole,所以不能要求每个 live payload独立通过 ext4 magic. Reader 校验 logical size、graph/self 关系和最终 composition. Direct EROFS payload则严格校验 EROFS logical size/magic.



## 8. `.snapshot` logical format

Snapshot S layout:

```text
[memory sparse payload]
[ZIP:
  config.json
  state.json
  snapshot.cfg
]
```

V1 `snapshot.cfg` 完整 schema只有:

```yaml
version: 1
sandbox_ref: file://<digest>.sandbox@digest:<digest>
from_refs:
  - file://<parent>.snapshot@digest:<digest>
```

`sandbox_ref` 指向同一 freeze point 生成的 E. `from_refs` 是 top-to-bottom memory parent chain. S 不重复 capacity、runtime、root/data graph、launch、mounts、files、init 或 metadata.

Snapshot ZIP 与 config 同样 strict、bounded、canonical。`config.json` 和 `state.json` 各限 16 MiB，`snapshot.cfg` 限 1 MiB，`from_refs` 最多 64 项。源码见 [snapshotfile.go](../pkg/snapshotfile/snapshotfile.go) 和 [snapshot/config.go](../pkg/snapshot/config.go)。旧 disk-schema input 返回 `unsupported snapshot format/version`,不 dual-read、不 migration、不 cold fallback.



## 9. Provenance、publish 与 carrier

### 9.1 Disk 与 memory provenance

```text
Disk provenance:   C0 + RunSourceBinding -> E/C1 disk graph
Memory provenance: MemorySourceBinding   -> S/from_refs
```

二者不共享schema. Re-snapshot可以独立merge disk layer或memory layer,不能借由一个旧`SnapshotConfig`同时修改两张graph.

Snapshot/export的dependency planning只物化没有portable provenance的节点本地依赖. 远端
`manifest://`保持原ref;已经located的file ref保持原ref;若logical Manifest来自located
Bundle,则改写为该Bundle的canonical located `@manifest` selector. 因此Manifest-backed
immutable root image不会在每次保存或发布快照时再生成一份`.overlay`,located parent chain
也不会被复制到新的publication directory. 未located的本地tarstream或Bundle依赖仍在
freeze前完整校验并物化,避免产生依赖调用节点私有路径的portable root. 其中immutable
root carrier物化为`.image`;只有需要作为独立dependency保存的root/data writable layer物化为
`.overlay`. 当前root writable top由Sandbox E payload承载,不生成第二份`.overlay`.

### 9.2 Local tarstream 与 crypto

Local immutable artifact支持 `crypto.local=off|auto|required`:

- `off`:plaintext tarstream,identity `digest`.
- `auto`:自动识别plaintext或KDXTS encrypted tarstream;新输出按配置codec.
- `required`:拒绝plaintext和未绑定key的identity;identity使用`hmac`.

未配置 policy 时默认 `off`；`auto` 或 `required` 在 storage 构造阶段就解析 customer key，即使是 file-only operation。没有 Manifest config 就没有 local codec 或 lazy Manifest client；file-only 不表示已配置的 crypto 可以省略 key。源码见 [storage.go](../pkg/artifact/storage.go)。

Existing-file reuse 必须重新验证 role、logical size、content identity 和完整 stream。 Local output与named ref-location的commit策略刻意分离. Artifact capture/publication只定义logical completion,不定义stable-storage durability:local output依赖完整写入、`Close()`检查、内容寻址no-replace rename与alias atomic rename;named-location依赖`O_EXCL`写入、`Close()`检查、最终路径reopen/full verification与路径身份检查. 两种 artifact-publication 路径都不执行显式 file/directory flush，物理写回由文件系统或底层存储实现定义；这不同于 §4 中执行 fsync 的 run-directory C0 写入。

<a id="local-output"></a>
#### 9.2.1 Local output

`snapshot/export --output` 的 `FileSink` 在output directory中写unique same-directory temp,完整写入并检查`Close`错误,再以`renameat2(RENAME_NOREPLACE)`完成O(1) final commit. `BundleSink`同样以当前atomic no-replace rename提交完整Bundle;两条本地路径都不会为了final commit再读取并复制完整artifact. Root成功后,semantic alias用随机temporary symlink + atomic rename更新. Alias target和existing entry都以`NOFOLLOW`/`lstat` fail closed,不会把regular file或directory替换成symlink;commit由完整写入、`Close()`检查与atomic rename定义,不执行显式file/directory fsync. 因此local output要求节点本地filesystem提供这些atomic rename和symlink语义.

<a id="named-ref-location"></a>
#### 9.2.2 Named ref-location

`publish/upload-snapshot --to-ref-location`不复用`FileSink`或`BundleSink`. Tarstream carrier在自身marker中保存payload boundary和payload commitment;完整读取会用payload bytes复验该声明. 打开carrier后可直接提供identity. E/S只替换dense metadata tail时,carrier用旧payload commitment和新tail以O(tail)工作量推导新identity,不读取GiB级payload,也没有首次`io.Discard`编码. Location target取得carrier给出的scheme/digest后,以`O_CREATE|O_EXCL`直接创建`<digest>.image|overlay|sandbox|snapshot`;canonical encoding是shared target中的唯一完整write. `.image`承载immutable root image,`.overlay`只承载独立的writable disk dependency;Sandbox E payload不会重复发布为`.overlay`. Plaintext输出使用`@digest`,codec-backed输出使用`@hmac`. Target directory中没有完整temp/staging副本,也不创建`<sid>.sandbox`、`<sid>.snapshot`或任何其他semantic alias.

Fresh final固定`0644`. `tarstream.WriteTo`在唯一一次写入过程中检查source read和destination write,并重新产生与carrier预先提供值一致的scheme/digest;在 owned write fd 仍打开时以 `lstat` + `SameFile` 确认 canonical path 仍指向本次 `O_EXCL` 创建的 inode；metadata-only guard fd 在 checked `Close` 前固定该 inode。Publisher 随后以 `O_RDONLY|O_NOFOLLOW`（另含 nonblocking，避免意外 FIFO 阻塞验证）重新打开并完整验证regular file、role/payload name、logical size、canonical tarstream、marker、codec、crypto policy、digest scheme/digest和完整 sequential stream；验证后还会再次核对 path identity。Existing final 走同一完整验证；验证成功后直接复用,inode和bytes不改变.

Manifest Bundle不进入tarstream E/S重建路径. Carrier以root Manifest key提供`@manifest` identity;location target先强制验证所选Manifest closure、recorded admission、physical keys和crypto domain,再对same-directory依赖Bundle按顺序做exact byte copy,root `<key>.bundle`最后发布. 每个共享final仍只写一次,copy后重新打开、验证canonical Bundle/root并与source逐字节比较,最后在target fd上再次强制验证所选closure. 该路径不创建`.snapshot/.sandbox`替身,也不改写Bundle内的`snapshot.cfg`.

Final path在write完成前会短暂可见. 正常consumer只能使用publisher成功返回的root ref;publisher仍按dependencies first、root last顺序发布. Concurrent publisher遇到partial final时重新打开并做有限、context-aware exponential-backoff验证;若writer在窗口内完成则复用. bounded retry后仍不完整或invalid时fail closed并提示显式cleanup/repair,不会删除unknown owner的path. Symlink、directory、FIFO和其他non-regular final同样拒绝且不删除. Publisher只在自身`O_EXCL`成功且path仍指向所记录inode时清理自己的失败写入;abandoned unknown final由显式cleanup/GC处理.

<a id="single-root-imagesandbox-manifest-bundle"></a>
#### 9.2.3 Single-root image/Sandbox Manifest Bundle

`NewSingleRootBundlePublisher` 是已经组装好的 `RoleImage` 或 `RoleSandbox`
`sparse.Source` 的 typed named-location sink。数据流是：

```text
logical source
  -> Manifest ingest with fixed write admission/customer key
  -> deterministic identity pass to a streaming hash sink
  -> replay the fixed source with the same key, admission and encoding options
  -> exclusive-create content-addressed <root>.bundle
  -> reopen/strict validate
  -> file://<root>.bundle@manifest:<root>#<location>
```

source 支持重复读取。第一遍保留根 Manifest key、完整 Bundle 的物理摘要及格式元数据，编码后的 Chunk 字节随处理释放。第二遍直接写入最终 `<root>.bundle`，并核对生成根与预计算身份一致。一次 publish 只解析一次客户密钥，两遍共用相同 admission 与编码参数。

唯一创建的文件是最终 Bundle，包含这个 logical root 的 Manifest 和 Chunk。发布使用有界 Chunk 工作缓冲并保留 payload 稀疏性。typed 调用点提供逻辑角色；image 和 Sandbox reader 保持各自 schema 校验。复用已有目标时，验证 admission、根、认证内容与完整物理身份。并发 writer 通过有界等待、exclusive create 与完整校验收敛。取消、源变化、写入及关闭错误使发布失败，并仅清理本次拥有的不完整 final。路径被替换时保留替换者的 inode，返回所有权错误。

Tarstream 与 Bundle 都是 physical carrier，不是 logical role。Tarstream 包含单一
role-specific payload 与 sparse envelope，使用 `@digest`/`@hmac` identity；Bundle
包含 Manifest/chunk records、write admission与可选local encryption，使用
`@manifest` root identity。single-root Bundle publication不得先建立 tarstream，反之也
不得仅按 `.image`/`.sandbox` 扩展名推导 Bundle root。

因此named location只依赖`mkdir`、exclusive create、write、read、stat/fstat/lstat、权限设置、seek/pread、close，以及删除本进程拥有的不完整file. 它不依赖rename/renameat2、symlink、hardlink、reflink、sparse-file preservation、advisory lock或lock file,也不执行显式file/directory sync;publication的success contract由`O_EXCL`写入、`Close()`检查、最终路径reopen/full verification与路径身份检查定义,物理写回由文件系统或底层存储实现定义.

Active encrypted `.overlay.diff` 保持KDXTS格式. Export只读取decrypt后的BlockCOW SnapshotView并创建新的immutable logical artifact,绝不把ZIP追加到active diff.

### 9.3 Manifest upload

已经组装好的 image 或顶层 Sandbox E 可通过
`NewManifestPublisher(...).PublishSource(ctx, RoleImage|RoleSandbox, source)`
直接 ingest。调用者保留 source ownership；该路径不生成 local tarstream，Chunks
与去重对象先写，root Manifest最后写入并返回`manifest://<root>`。

Local tarstream graph按照bottom-up顺序ingest:

```text
data/lower -> E -> S
```

已经portable的Manifest或located dependency保持原ref,不会先materialize再重写. Bundle root走exact-upload快路径:强制验证选择的Manifest closure、recorded admission和physical objects,原样上传Chunk/Manifest,root Manifest最后提交;root key和`snapshot.cfg`不变. 因此Bundle内已有的located selector仍要求consumer配置对应ref-location,不会被暗中改写成Manifest ref. Customer key、chunk/Manifest crypto、content verification和store generation admission沿用manifest config. E是export root,S是snapshot root.

### 9.4 Manifest Bundle

Bundle在pause前完成:

- write admission;
- portable dependency retention和located Bundle selector rewrite;
- unlocated local dependency materialization plan;
- current operation依赖集合;
- unlocated parent Bundle精确Manifest copy或remote fallback;located parent保留canonical selector;
- local tarstream dependency ingest;
- all ref replacements.

然后writer一次性emit metadata prefix,写入Manifest/Chunk,最后以E或S root key finalize. `FullVerify`使用完整expected Manifest集合. V1不以外层ZIP magic推断E/S.

### 9.5 Publish graph

Local tarstream Publish E:

```text
parse E -> enumerate E explicit disk refs bottom-up
        -> publish node-local payload dependencies -> rewrite those refs
        -> rebuild E -> publish E last
```

`self` 永不重写. 父 `.sandbox` 作为writable disk layer时发布为`.overlay` payload;作为two-disk root image carrier时提取EROFS payload与image config并发布为`.image`. 两种情况都不递归父config graph.

Local tarstream Publish S:

```text
publish memory from_refs as opaque memory layers
publish node-local S.sandbox_ref E graph
rewrite node-local sandbox_ref
rebuild S -> publish S last
```

Memory parent的historical `sandbox_ref` 不递归;当前S引用的E graph是当前disk truth. `manifest://`和已located ref保持不变,不会产生`Manifest -> named location`或Manifest tail rewrite. Bundle carrier整体走exact location copy或exact Store upload,不进入上述重建流程.



## 10. Atomicity 与 determinism

Portable YAML和E/S ZIP使用canonical order、fixed metadata和bounded bytes. Local `FileSink`/`BundleSink`保持same-directory temp、完整写入/`Close()`检查和atomic no-replace rename,final commit是O(1);alias只在root commit后更新. artifact capture/publication只定义logical completion,不定义stable-storage durability;两条本地路径都不执行显式file/directory fsync,物理写回由文件系统或底层存储实现定义. Named ref-location采用独立的exclusive-create + checked-write/copy-once + reopen-full-verify协议;tarstream由carrier直接提供identity,Bundle保持exact bytes,两者都不进入local sink的capture/commit路径.

多盘顺序固定为data disks first、root E last. Snapshot随后写memory S last. 这让S/E root成为可审计的graph commit point.



## 11. Incompatibility

本切换直接替换旧snapshot provenance. 旧`SnapshotConfig`若包含resource/runtime/root/data/launch字段会返回明确unsupported error. 不提供 old-format alias、dual reader、migration shim、feature flag 或 zero-memory compatibility path。[子命令总览](sandbox_zh.md#21-子命令总览) 的 `upload-snapshot` CLI alias 不提供格式兼容。

E2B mapping只发生在orchestrator层:

```text
E2B memory=false
  -> sandbox-ctl export
  -> Sandbox resume source
  -> explicit sandbox-ctl run --from

E2B memory=true
  -> sandbox-ctl snapshot
  -> Snapshot resume source
  -> sandbox-ctl run --restore
```


S/E 后缀几何、确定性 ZIP 编码及 sparse prefix/append 视图由 `accelerator/pkg/tailzip` 实现。Sandbox/Snapshot 模块提供有序角色条目与大小上限，保留 portable config、JSON/state 和设备拓扑校验。逻辑引用位置共用 `manifest.RefLocations`。

### 发布已有 Manifest 根

`publish` 输入可以是本地/located 载体、Bundle selector 或 `manifest://` 根。打开后按 Sandbox E 或 Snapshot S 校验逻辑角色。`PublishSource` 同时支持已经组装完成的 Snapshot source，source 所有权由调用方保留。

located tarstream 输出直接使用可用的载体身份。尚无可用身份的 Manifest 或组合 source 先流式计算身份，再以相同 source 和 codec 重读并写入最终内容寻址路径。仅创建最终文件。调用方保持 source 不变且可重复读取。已有目标继续完整校验，实际写入中内容变化时发布失败。
