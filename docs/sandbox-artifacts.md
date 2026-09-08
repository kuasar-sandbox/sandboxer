[English](sandbox-artifacts.md) | [简体中文](sandbox-artifacts_zh.md)

# Sandbox artifacts — portable configuration, references and publication

This document defines persistent Sandbox E / Snapshot S, portable configuration, parent references, identity and carrier-publication rules in full. [Runtime](sandbox.md) owns cold start, execution, freezing, capture, restore and cleanup sequencing; [Guest ABI](sandbox-init.md) owns host/guest coordination. [Accelerator file artifacts](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/file-artifacts.md) own the generic carrier byte formats rather than a second definition here.

**Reading path**

- [Logical roles](#11-logical-roles)
- [Logical roles and physical carriers](#12-logical-roles-and-physical-carriers)
- [PortableSandboxConfig](#32-portablesandboxconfig)
- [Strict encoding and limits](#33-strict-encoding-and-limits)
- [C0, C1, and source binding](#34-c0-c1-and-source-binding)
- [`self` and disk provenance](#35-self-and-disk-provenance)
- [`.sandbox` logical format](#36-sandbox-logical-format)
- [`.snapshot` logical format](#37-snapshot-logical-format)
- [Provenance, publication, and carriers](#11-provenance-publication-and-carriers)
- [Atomicity and determinism](#141-atomicity-and-determinism)
- [Incompatibility](#144-incompatibility)




<a id="11-逻辑角色"></a>

<a id="11-logical-roles"></a>
## 1. Logical roles

The system distinguishes four logical roles:

| Role | Suffix | Logical content | Purpose |
|---|---|---|---|
| Container image | `.image` when materialized | `EROFS + ZIP(config.json)` | Read-only container rootfs and OCI image defaults |
| Disk overlay | `.overlay` | Sparse block payload | Writable layer of a root or data disk |
| Sandbox E | `.sandbox` | Root payload + strict ZIP | Complete portable workload for repeatable cold starts |
| Snapshot S | `.snapshot` | Memory payload + strict ZIP | Resume running processes and VMM/guest memory state |

`E` means a runnable Sandbox artifact; `S` means a memory Snapshot. Their responsibilities are not interchangeable:

```text
explicit sandbox.yaml ── run --config ──> cold start
Sandbox E             ── run --from   ──> cold start
Snapshot S ── sandbox_ref ──> E
           └──────── run --restore ────> memory restore

live export              ──> E
image-to-Sandbox-E build ──> E
memory snapshot          ──> E + S, S is operation root
```

The following models do not exist:

- There is no `snapshot --memory=false`.
- There is no zero-size memory Snapshot.
- `run --restore` has no cold-boot branch.
- `snapshot.cfg` does not store the disk graph, launch, mounts, files, or init.
- Cold boot is not wrapped as restore.



<a id="12-逻辑角色与物理-carrier"></a>

<a id="12-logical-roles-and-physical-carriers"></a>
## 2. Logical roles and physical carriers

Logical content and physical carrier are independent. The same `.image`, `.overlay`, `.sandbox`, or `.snapshot` logical source can use these carriers:

| Carrier | Reference | Notes |
|---|---|---|
| Local tarstream | `file://<basename>@digest:<digest>` or `@hmac:<digest>` | The tarstream envelope authoritatively declares the sparse map |
| Manifest | `manifest://<key>` | Manifest configuration controls chunk/Manifest encryption and verification |
| Manifest Bundle | `file://<bundle>@manifest:<key>` | One ZIP64 Bundle can carry the root and its complete dependent Manifest graph |

In local mode, materialized content-addressed filenames are `<digest>.<role>`. Immutable root carriers use `.image`; `.overlay` denotes a separately materialized writable disk layer. The current writable root top is already the Sandbox E payload and is not duplicated as an `.overlay`. Provisioned container-image inputs may still use an explicit basename such as `.erofs`. `<sid>.sandbox` and `<sid>.snapshot` are semantic symlinks updated after a successful commit. In Bundle mode, the semantic symlink points to the `<key>.bundle` carrying the root Manifest. An alias SID must be one safe path component. Commit uses a temporary symlink and atomic rename, and refuses to replace an existing regular file or directory. These are node-local output semantics; named ref-locations use the separate shared-location commit protocol in §9.2 and create no aliases.

The block backend, restore code, and publisher first open the carrier, then parse the content according to its logical role. Outer ZIP magic identifies a Bundle carrier, not its logical role.



<a id="32-portablesandboxconfig"></a>
## 3. PortableSandboxConfig

The sole portable filename is:

```go
const SandboxRuntimeConfigName = "sandbox.runtime.cfg"
```

The same canonical bytes are written to:

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

Portable content includes:

- `resources.capacity` and workload defaults in `resources.allocatable`.
- The `network.enabled/interface` topology facts.
- Kernel/runtime basenames and content identities.
- Boot cmdline.
- The immutable root/data graph, plus data-disk names, order, and mount topology.
- Persistent launch/plugin/mounts/files/init/metadata.

Portable content excludes:

- Cgroup paths/fds/controllers, overhead, watermark, and startup.
- TAP/TapFD providers, helpers, sockets, IP/MAC/hostname.
- The CH binary, run-root/base-root, and active diff paths.
- Manifest/customer keys, crypto secrets, access tokens, and ref-location host paths.
- Restore prefetch and host protocol timeouts.
- Stdio/forward endpoints.
- `ephemeral_files` and `launch.ephemeral_env`.

Local immutable refs must contain a basename and identity, for example:

```text
file://vmlinux@digest:<digest>
file://sandbox-runtime.bundle@digest:<digest>
file://<digest>.image@digest:<digest>
file://<digest>.overlay@hmac:<digest>
file://<digest>.overlay@digest:<digest>
```

The destination host binds these through actual paths, a source directory, or named ref-locations before controller, network, cgroup, VM, or run-directory side effects. Verification is specific to the input: explicit cold projection hashes the kernel; `run --from` and restore check its absolute file binding and regular-file existence but deliberately skip the full kernel re-hash. They compare the runtime Bundle footer identity with C0. Thus these paths do not independently prove the supplied kernel matches E's recorded digest; kernel deployment remains a trusted host responsibility. Carrier metadata and configured content verification retain their own checks. See [lifecycle.go](../pkg/sandbox/lifecycle.go) and [restore.go](../pkg/restore/restore.go).



<a id="33-strict-encoding-与-limits"></a>

<a id="33-strict-encoding-and-limits"></a>
## 4. Strict encoding and limits

Portable configuration uses deterministic YAML marshaling and strict known-fields parsing. The artifact reader rejects unknown fields/versions, duplicate keys, YAML aliases/merge keys, multiple documents, invalid refs, and noncanonical bytes. The schema parser alone accepts valid noncanonical YAML; `sandboxfile.Open` enforces canonical byte equality after re-marshaling.

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

These limits apply together: strict YAML's 64 KiB scalar-byte limit also applies to a file's inline `content` scalar; the separate 256 KiB per-file bound does not override it.

`<run_dir>/sandbox.runtime.cfg` is written using a same-directory temporary file, mode 0600, file fsync, no-replace rename, and directory fsync. The lifecycle baseline is write-once: any existing final path causes failure, even if its bytes would match. This is stricter than merely rejecting different bytes. See [portable.go](../pkg/config/portable.go).



<a id="34-c0c1-与-source-binding"></a>

<a id="34-c0-c1-and-source-binding"></a>
## 5. C0, C1, and source binding

`C0` is the immutable portable baseline of one run lifecycle:

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

C0 is written in the run directory before VM side effects and its bytes remain unchanged thereafter. The live runtime separately holds a nonserialized `RunSourceBinding`:

- The source E/root artifact corresponding to the current `self`.
- Root payload and optional image configuration.
- Resolved local path/source directory.
- Bundle reader/fetcher/source refs.
- Named ref-locations.
- Host-only disk bindings and current active diffs.

Memory restore additionally holds `MemorySourceBinding`; its Snapshot S identity and `from_refs` manage memory provenance only.

`C1 = Export(C0, disks at T)` is written only to a new E. `export --resume` and `snapshot --resume` do not change C0, active diffs, the lower graph, or memory parents. The same VM can therefore produce `E1@T1` and `E2@T2` while continuing to use its original C0 and writable diffs.



<a id="35-self-与-disk-provenance"></a>

<a id="35-self-and-disk-provenance"></a>
## 6. `self` and disk provenance

The portable graph must contain the reserved value `self` exactly once, and only at:

```text
boot.root.base
boot.root.overlay.base
```

Data disks and every `base_from_refs` list must not contain `self`. Three root relationships are valid:

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

`run --from` binds `self` to the current E payload through source binding. When exporting C1, it first materializes C0's old `self` as the original source ref, then sets the new root payload position to `self`. This avoids a self-referential E digest cycle.

`run --from --replace-boot` creates no source binding. After reading non-boot defaults, the source E no longer participates in the run. The new active writable root layer occupies the sole `self` through ordinary explicit-cold projection. Later export/snapshot therefore does not treat the source E as a disk parent or dependency.

When a parent `.sandbox` occurs in a disk-ref field, it supplies only `Payload`:

- For an ext4 upper/lower layer or data disk, its `sandbox.runtime.cfg` and `config.json` are not adopted.
- For a root EROFS base, the block backend still consumes only Payload. The host may read optional `config.json` for image defaults, but never adopts the parent E's portable configuration.

The current E must explicitly list the lower/data refs it actually depends on. The publisher does not recursively traverse a parent E's configuration graph merely because the extension is `.sandbox`.

Local merging has two categories:

- Disk merging operates only on E's sparse disk layers.
- Memory merging operates only on S's memory layers.

Three-state sparse semantics remain `Hole`, `Zero`, and `Data`. Holes come only from authoritative metadata; scanning zero bytes must not invent holes.



<a id="36-sandbox-logical-format"></a>
## 7. `.sandbox` logical format

Only two layouts are accepted.

Live ext4/sparse-root export:

```text
[root sparse payload]
[ZIP:
  sandbox.runtime.cfg
]
```

Top-level Sandbox E with a direct EROFS layout:

```text
[EROFS payload]
[ZIP:
  config.json
  sandbox.runtime.cfg
]
```

Image-to-Sandbox-E assembly locates the archive base of the original `EROFS + ZIP(config.json)`, preserves the EROFS prefix and validated `config.json` bytes, then rebuilds one canonical two-entry ZIP. `ZIP(config.json) + ZIP(sandbox.runtime.cfg)` is an invalid double ZIP.

The reader derives payload `[0, archiveBase)` from ZIP structure and does not trust a second `payload_size`. The strict ZIP contract requires:

- EOCD at logical EOF with an empty comment.
- No ZIP64 or multidisk ZIP.
- Exactly `{sandbox.runtime.cfg}` or `{config.json,sandbox.runtime.cfg}`.
- No duplicate, unknown, directory, or path-traversal entries.
- Store compression method.
- Fixed order, timestamps, and metadata.
- Bounded entry sizes.
- Matching CRC, local-header, and central-header values.
- Malformed/truncated inputs to fail before VM side effects.

An opened root exposes the following views; the actual type also carries parsed configuration and archive-boundary bookkeeping:

```go
type Root struct {
    FullStream    fetch.Stream
    Payload       fetch.Stream
    ImageConfig   []byte
    RuntimeConfig []byte
}
```

Payload is a section view preserving sparse `RunAt`/`ReadAt`. A shared close owner ensures that closing FullStream, Payload, or Root ultimately releases the carrier exactly once. Vhost sees only Payload, never the ZIP tail.

A live BlockCOW SnapshotView is an upper-only sparse delta. Its ext4-superblock offset can be a Hole, so not every live payload can be required to pass an independent ext4-magic check. The reader checks logical size, graph/self relationships, and the final composition. Direct EROFS payloads undergo strict EROFS logical-size/magic checks.



<a id="37-snapshot-logical-format"></a>
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

The complete V1 `snapshot.cfg` schema is only:

```yaml
version: 1
sandbox_ref: file://<digest>.sandbox@digest:<digest>
from_refs:
  - file://<parent>.snapshot@digest:<digest>
```

`sandbox_ref` points to the E produced at the same freeze point. `from_refs` is the top-to-bottom memory-parent chain. S does not duplicate capacity, runtime, root/data graphs, launch, mounts, files, init, or metadata.

Snapshot ZIP and configuration are also strict, bounded, and canonical. `config.json` and `state.json` are each limited to 16 MiB, `snapshot.cfg` to 1 MiB, and `from_refs` to 64 entries. See [snapshotfile.go](../pkg/snapshotfile/snapshotfile.go) and [snapshot/config.go](../pkg/snapshot/config.go). Old disk-schema inputs return `unsupported snapshot format/version`; there is no dual-read, migration, or cold fallback.



<a id="11-provenancepublish-与-carrier"></a>

<a id="11-provenance-publication-and-carriers"></a>
## 9. Provenance, publication, and carriers

<a id="111-disk-与-memory-provenance"></a>

<a id="111-disk-and-memory-provenance"></a>
### 9.1 Disk and memory provenance

```text
Disk provenance:   C0 + RunSourceBinding -> E/C1 disk graph
Memory provenance: MemorySourceBinding   -> S/from_refs
```

These use separate schemas. Re-snapshot can merge disk layers and memory layers independently; an old `SnapshotConfig` cannot be used to modify both graphs together.

Snapshot/export dependency planning materializes only node-local dependencies that lack portable provenance. Remote `manifest://` refs remain unchanged, as do already located file refs. If a logical Manifest comes from a located Bundle, its ref becomes that Bundle's canonical located `@manifest` selector. Consequently a Manifest-backed immutable root image is not duplicated as a new `.overlay` every time a snapshot is saved or published, and located parent chains are not copied into the new publication directory. Unlocated local tarstream/Bundle dependencies are still fully validated and materialized before freeze so the portable root does not depend on the calling node's private paths. Immutable root carriers materialize as `.image`; only root/data writable layers that must be retained as separate dependencies materialize as `.overlay`. The current writable root top is carried by Sandbox E's payload and does not produce another `.overlay`.

<a id="112-local-tarstream-与-crypto"></a>

<a id="112-local-tarstream-and-crypto"></a>
### 9.2 Local tarstream and crypto

Local immutable artifacts support `crypto.local=off|auto|required`:

- `off`: plaintext tarstream, with `digest` identity.
- `auto`: recognize plaintext or KDXTS-encrypted tarstreams; configured codec governs new output.
- `required`: reject plaintext and identities not bound to the key; use `hmac` identity.

The omitted policy defaults to `off`. With `auto` or `required`, storage construction resolves the customer key even for a file-only operation. No Manifest config means no local codec or lazy Manifest client; file-only operation does not by itself imply that configured crypto can omit its key. See [storage.go](../pkg/artifact/storage.go).

Reusing an existing file requires revalidation of its role, logical size, content identity, and complete stream. Local output and named ref-location commit strategies are deliberately separate. Artifact capture/publication defines logical completion, not stable-storage durability. Local output relies on complete writes, checked `Close()`, content-addressed no-replace rename, and atomic alias rename. Named locations rely on exclusive creation, checked `Close()`, final-path reopening/full verification, and path-identity checks. Neither artifact-publication path explicitly flushes files/directories; physical writeback depends on the filesystem/storage implementation. This differs from the fsynced run-directory C0 write in §4.

<a id="local-output"></a>
#### 9.2.1 Local output

For `snapshot/export --output`, `FileSink` writes a unique same-directory temporary file, completes all writes and checks `Close` errors, then uses `renameat2(RENAME_NOREPLACE)` for an O(1) final commit. `BundleSink` likewise commits a complete Bundle with an atomic no-replace rename. Neither path rereads and copies the entire artifact merely to commit it. After root success, a semantic alias is updated with a random temporary symlink and atomic rename. Alias targets and existing entries use `NOFOLLOW`/`lstat` checks that fail closed, refusing to replace regular files or directories with symlinks. Complete writes, checked `Close()`, and atomic rename define commit; there is no explicit artifact file/directory fsync. Local output therefore requires a node-local filesystem with these atomic rename and symlink semantics.

<a id="named-ref-location"></a>
#### 9.2.2 Named ref-location

`publish/upload-snapshot --to-ref-location` does not reuse `FileSink` or `BundleSink`. A tarstream carrier's marker records payload boundaries and a payload commitment. Reading the complete carrier verifies that declaration against payload bytes; opening the carrier already exposes its declared identity. When only E/S's dense metadata tail changes, the carrier combines the old payload commitment with the new tail to derive a new identity in O(tail) work. It does not read GiB-scale payloads just to derive this value or first encode to `io.Discard`. After obtaining the carrier's scheme/digest, the location target creates `<digest>.image|overlay|sandbox|snapshot` directly with `O_CREATE|O_EXCL`. Canonical encoding is the only complete write into the shared target. `.image` carries an immutable root image; `.overlay` carries only a separate writable-disk dependency. The Sandbox E payload is not published again as `.overlay`. Plaintext output uses `@digest`; codec-backed output uses `@hmac`. The target directory holds no complete staging copy for this tarstream path and creates no `<sid>.sandbox`, `<sid>.snapshot`, or other semantic alias.

Fresh finals have mode `0644`. During the sole write, `tarstream.WriteTo` checks source reads and destination writes, reproducing the carrier-provided scheme/digest. While the owned write fd is still open, `lstat` and `SameFile` confirm the canonical path still identifies the inode created by this invocation's `O_EXCL`. A metadata-only guard fd pins that inode across checked `Close`. The publisher then reopens with `O_RDONLY|O_NOFOLLOW` (also nonblocking so unexpected FIFOs cannot stall validation) and fully verifies regular-file type, role/payload name, logical size, canonical tarstream, marker, codec, crypto policy, digest scheme/digest, and the complete sequential stream. Path identity is checked again after validation. Existing finals undergo the same complete content validation; successful reuse does not change inode or bytes.

Manifest Bundles do not enter the tarstream E/S rebuilding path. Their root Manifest key supplies `@manifest` identity. The location target first forces verification of the selected Manifest closure, recorded admission, physical keys, and crypto domain. It then performs ordered exact-byte copying of same-directory dependency Bundles and publishes the root `<key>.bundle` last. Each shared final is written once, reopened, validated as a canonical Bundle/root, and compared byte-for-byte with its source; the selected closure is strictly reverified through the target fd. This creates no `.snapshot`/`.sandbox` substitute and does not rewrite the Bundle's `snapshot.cfg`.

The final path is briefly visible before writing finishes. Normal consumers must use only a root ref returned successfully by the publisher, which continues to publish dependencies first and root last. A concurrent publisher encountering a partial final reopens and validates it with bounded, context-aware exponential backoff. If the writer completes within that window, the final is reused. A still-incomplete or invalid final after bounded retries fails closed with an explicit cleanup/repair requirement; the publisher does not delete another owner's path. Symlinks, directories, FIFOs, and other nonregular finals are also rejected and preserved. Cleanup removes only a failed write for which this publisher successfully acquired `O_EXCL` and whose path still identifies the recorded inode. Abandoned finals of unknown ownership need explicit cleanup or an owning retention/GC policy.

<a id="single-root-imagesandbox-manifest-bundle"></a>
#### 9.2.3 Single-root image/Sandbox Manifest Bundle

`NewSingleRootBundlePublisher` is a typed named-location sink for an already assembled `RoleImage` or `RoleSandbox` `sparse.Source`:

```text
logical source
  -> Manifest ingest with fixed write admission/customer key
  -> Bundle root finalize + FullVerify in target-directory temporary file
  -> exclusive-create content-addressed <root>.bundle
  -> reopen/strict validate
  -> file://<root>.bundle@manifest:<root>#<location>
```

This path creates no role tarstream, uploads nothing to Manifest store, creates neither `<root>.image` nor `<root>.sandbox`, and creates no BuildID/SandboxID semantic alias. The Bundle contains only this logical root's Manifest/chunks. The typed call site determines the root's logical role, and the reader still applies the strict image or Sandbox parser. An existing same-key final may be reused only if its admission, root, crypto, and exact bytes all match the newly generated Bundle. Concurrent writers converge through the same bounded waiting, exclusive-create, and full-validation protocol as ordinary named publication. Failure/cancellation returns no ref and removes the publisher's target-directory temporary file and owned incomplete final.

Tarstream and Bundle are physical carriers, not logical roles. Tarstream contains a single role-specific payload and sparse envelope, with `@digest`/`@hmac` identity. Bundle contains Manifest/chunk records, write admission, and optional local encryption, with `@manifest` root identity. Single-root Bundle publication must not first build a tarstream; conversely, a Bundle root cannot be inferred just from an `.image`/`.sandbox` extension.

Named locations therefore require directory creation, exclusive file creation, writes/reads, stat/fstat/lstat, permission setting, seek/pread, close, and removal of incomplete files owned by this process. They do not rely on rename/renameat2, symlinks, hardlinks, reflinks, preservation of filesystem holes, advisory locks, or lock files, and perform no explicit file/directory sync. Successful publication is defined by exclusive writes, checked `Close()`, final-path reopening/full verification, and path-identity checks; the filesystem or underlying storage defines physical writeback.

Active encrypted `.overlay.diff` files remain in KDXTS format. Export reads only the decrypted BlockCOW SnapshotView and creates a new immutable logical artifact. It never appends ZIP data to an active diff.

<a id="113-manifest-upload"></a>
### 9.3 Manifest upload

An already assembled image or top-level Sandbox E can be ingested directly with `NewManifestPublisher(...).PublishSource(ctx, RoleImage|RoleSandbox, source)`. The caller retains source ownership. This path creates no local tarstream: chunks and deduplicated objects are written first, the root Manifest last, returning `manifest://<root>`.

Local tarstream graphs are ingested bottom-up:

```text
data/lower -> E -> S
```

Already portable Manifest/located dependencies retain their refs rather than being materialized and rewritten. Bundle roots use the exact-upload fast path: force verification of the selected Manifest closure, recorded admission, and physical objects; upload Chunk/Manifest objects unchanged; commit the root Manifest last. The root key and `snapshot.cfg` remain unchanged. Existing located selectors inside a Bundle therefore still require consumers to configure the corresponding ref-location; they are not silently rewritten into Manifest refs. Customer key, chunk/Manifest crypto, content verification, and store-generation admission follow Manifest configuration. E is the export root; S is the snapshot root.

<a id="114-manifest-bundle"></a>
### 9.4 Manifest Bundle

Before pause, a Bundle completes:

- Write admission.
- Retention of portable dependencies and rewriting of located Bundle selectors.
- The plan to materialize unlocated local dependencies.
- The dependency set for the current operation.
- Exact Manifest copying from an unlocated parent Bundle or remote fallback; located parents retain canonical selectors.
- Ingestion of local tarstream dependencies.
- All ref replacements.

The writer then emits the metadata prefix once, writes Manifest/Chunk data, and finalizes with E's or S's root key. `FullVerify` uses the complete expected Manifest set. V1 does not infer E/S from outer ZIP magic.

<a id="115-publish-graph"></a>
### 9.5 Publish graph

Publishing local tarstream E:

```text
parse E -> enumerate E explicit disk refs bottom-up
        -> publish node-local payload dependencies -> rewrite those refs
        -> rebuild E -> publish E last
```

`self` is never rewritten. A parent `.sandbox` used as a writable-disk layer is published as an `.overlay` payload. Used as the root-image carrier in a two-disk layout, its EROFS payload and image configuration are extracted and published as `.image`. Neither case recursively traverses the parent's configuration graph.

Publishing local tarstream S:

```text
publish memory from_refs as opaque memory layers
publish node-local S.sandbox_ref E graph
rewrite node-local sandbox_ref
rebuild S -> publish S last
```

A memory parent's historical `sandbox_ref` is not traversed recursively. The E graph referenced by the current S is current disk truth. `manifest://` and already located refs remain unchanged; there is no `Manifest -> named location` materialization or Manifest-tail rewriting. Bundle carriers use exact location copying or exact Store upload as a whole, bypassing these rebuilding flows.



<a id="141-atomicity-与-determinism"></a>

<a id="141-atomicity-and-determinism"></a>
## 10. Atomicity and determinism

Portable YAML and E/S ZIP use canonical order, fixed metadata, and bounded bytes. Local `FileSink`/`BundleSink` retain same-directory temporaries, complete writes/checked `Close()`, and atomic no-replace rename; final commit is O(1). The alias updates only after root commit. Artifact capture/publication defines logical completion, not stable-storage durability. Neither local artifact path explicitly fsyncs files/directories; the filesystem/storage implementation governs physical writeback. Named ref-locations use a separate exclusive-create, checked-write/copy-once, reopen-and-full-verify protocol. Tarstream carriers directly supply identity; Bundle copying preserves exact bytes. Neither uses the local sink's capture/commit path.

Multidisk ordering is fixed: data disks first, root E last. Snapshot then writes memory S last. This makes the S/E root an auditable graph commit point.



<a id="144-incompatibility"></a>
## 11. Incompatibility

The current provenance schema replaces the old snapshot provenance. An old `SnapshotConfig` containing resource/runtime/root/data/launch fields returns an explicit unsupported-format error. There is no old-format alias, dual reader, migration shim, feature flag, or zero-memory compatibility path. The `upload-snapshot` CLI alias in [Subcommands](sandbox.md#21-subcommands) does not provide format compatibility.

E2B mapping occurs only in orchestrator:

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
