[English](sandbox.md) | [简体中文](sandbox_zh.md)

# sandbox — Sandbox control and artifact lifecycle

`sandbox-ctl` is kuasar-sandbox's host control plane for one sandbox. It handles explicit cold starts, cold starts from Sandbox artifacts, memory restore, live export, image-to-Sandbox-E assembly without a VM, memory snapshots, artifact publication, and runtime `exec`/forwarding. The guest protocol is documented in [sandbox-init.md](sandbox-init.md).

This document describes the current format and behavior. The current reader rejects the old `snapshot.cfg` disk-graph schema; it provides no dual reader, automatic migration, or cross-version compatibility guarantee. This format boundary does not mean that the project has never published releases.

## 1. Overview

<a id="artifact-model"></a>
### 1.1 Artifact model

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

#### 1.1.1 Logical roles and physical carriers

Logical content and physical carrier are independent. The same `.image`, `.overlay`, `.sandbox`, or `.snapshot` logical source can use these carriers:

| Carrier | Reference | Notes |
|---|---|---|
| Local tarstream | `file://<basename>@digest:<digest>` or `@hmac:<digest>` | The tarstream envelope authoritatively declares the sparse map |
| Manifest | `manifest://<key>` | Manifest configuration controls chunk/Manifest encryption and verification |
| Manifest Bundle | `file://<bundle>@manifest:<key>` | One ZIP64 Bundle can carry the root and its complete dependent Manifest graph |

In local mode, materialized content-addressed filenames are `<digest>.<role>`. Immutable root carriers use `.image`; `.overlay` denotes a separately materialized writable disk layer. The current writable root top is already the Sandbox E payload and is not duplicated as an `.overlay`. Provisioned container-image inputs may still use an explicit basename such as `.erofs`. `<sid>.sandbox` and `<sid>.snapshot` are semantic symlinks updated after a successful commit. In Bundle mode, the semantic symlink points to the `<key>.bundle` carrying the root Manifest. An alias SID must be one safe path component. Commit uses a temporary symlink and atomic rename, and refuses to replace an existing regular file or directory. These are node-local output semantics; named ref-locations use the separate shared-location commit protocol in §6.5 and create no aliases.

The block backend, restore code, and publisher first open the carrier, then parse the content according to its logical role. Outer ZIP magic identifies a Bundle carrier, not its logical role.



Generic carrier bytes are defined by [Accelerator file artifacts](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/file-artifacts.md). The host lifecycle here owns the E/S logical schemas and publication rules; [Guest ABI](sandbox-init.md) owns host/Guest coordination.

### 1.2 Responsibility boundaries

`sandboxer` provides the complete E/S capabilities but does not define orchestrator's durable API. The recommended upper-layer mapping is:

```text
memory=true  -> kind=snapshot, ref=<Snapshot S>
memory=false -> kind=sandbox,  ref=<Sandbox E>
```

The ordinary data plane must not treat Sandbox E as memory state that can wake automatically. Starting from E requires explicit cold `Connect` semantics.

## 2. Command-line interface

### 2.1 Subcommands

```text
sandbox-ctl run
sandbox-ctl export
sandbox-ctl snapshot
sandbox-ctl exec
sandbox-ctl usage
sandbox-ctl config
sandbox-ctl info
sandbox-ctl publish
sandbox-ctl upload-snapshot
```

`upload-snapshot` is a thin compatibility alias for `publish`; both call the same publisher and graph traversal.

### 2.2 `sandbox-ctl run`

The three input modes are strictly separate:

```bash
# Explicit cold start.
sandbox-ctl run --config sandbox.yaml --sandbox-id s1

# Cold start from Sandbox E.
sandbox-ctl run --from ./s1.sandbox --config host.yaml --sandbox-id s2

# Cold derivation from Sandbox E defaults with a completely new boot graph.
sandbox-ctl run --from ./s1.sandbox --replace-boot \
  --config replacement.yaml --sandbox-id s2-rebased

# Resume memory execution state from Snapshot S.
sandbox-ctl run --restore ./s1.snapshot --config restore-host.yaml --sandbox-id s3
```

RunRoot/BaseRoot are invocation-level roots; RunDir/BaseDir are the actual directories for this Sandbox. `SandboxID` always denotes logical identity. `PathID` is only the directory leaf shared under the two roots:

```text
pathID  = explicitly supplied PathID, otherwise SandboxID
RunDir  = RunRoot/pathID
BaseDir = BaseRoot/pathID
```

For example, a Build phase can retain a globally unique SandboxID while using a fixed private directory:

```bash
sandbox-ctl run \
  --config phase-a.yaml \
  --sandbox-id build-01-phase-a \
  --path-id a \
  --run-root /run/sandbox/builds/build-01 \
  --base-root /var/lib/sandbox/builds/build-01
```

PathID must be a nonempty, safe, single path component: neither `.` nor `..`, and containing no `/`, backslash, or NUL. It is not compared with SandboxID, and there is no PathID-to-SandboxID mapping. Default writable diffs for root/data disks reside in the PathID-derived BaseDir, but their filenames still use logical SandboxID, for example `BaseRoot/a/build-01-phase-a.overlay.diff` and `BaseRoot/a/build-01-phase-a.disk0.diff`. Cold, `--from`, and `--restore` modes use the same rule. Normal Sandbox exit cleans up only the current PathID's RunDir and owned diffs; it does not recursively remove the supplied RunRoot/BaseRoot or sibling PathIDs.

`--from` and `--restore` are mutually exclusive. Ownership of `--config` differs by mode:

- Ordinary run: a complete explicit cold configuration.
- `--from`: a host/instance overlay; E supplies the portable workload.
- `--from --replace-boot`: E supplies only non-boot portable defaults; the host configuration must supply the complete boot definition.
- `--restore`: host-only restore bindings/policy; S and its referenced E supply execution state and the portable workload.

`--config a.yaml:b.yaml` still merges files in order. `SANDBOX_CONFIG` can replace `--config`. `LoadConfigBytesWithPresence` supplies the same presence-aware rules for in-memory inputs such as config-socket.

`--from` and `--restore` support:

- Raw absolute or relative local paths.
- Scheme-qualified local tarstreams.
- `manifest://<key>`.
- `file://<bundle>@manifest:<key>`.
- Inference of a Bundle's default root key.
- Named `--ref-location name=file:///absolute/path` bindings.

Run's stdio, TTY, console, `--ready-fd`, connect forwarding, and resource parameters retain the same host control semantics in all three modes. Memory restore does not resend the cold launch specification.

### 2.3 `sandbox-ctl snapshot`

The following is command syntax; choose one output alternative and omit the square-bracket notation when executing:

```text
sandbox-ctl snapshot \
  --path-id a \
  --run-root /run/sandbox/builds/build-01 \
  (--output /artifacts | --upload) \
  [--mode local|bundle] \
  [--resume] \
  [--drop-caches=false] \
  [--merge-ref=true]
```

A Snapshot always contains memory execution state. One operation produces E and S at the same freeze point, committing S last. On success, the default is to destroy the VM. `--resume` resumes the original VM without making the new E/S its live baseline.

Human-readable local output lists both `Snapshot S` and `Sandbox E`. Default upload stdout still contains only the S Manifest key for shell compatibility. `snapshot_done` returns both `snapshot_ref` and `sandbox_ref`; existing `memory_size`, `memory_resident`, pause/dump timing, and compatibility `overlay_*` response fields remain. `overlay_*` currently mirrors E's identity; only E contains the actual disk graph.

`snapshot --json` emits exactly `{snapshotRef,sandboxRef,removedRefs}` and
`export --json` emits exactly `{sandboxRef,removedRefs}`, with `removedRefs:[]`
for capture. These identities come from the completed runtime response (or the
assembly producer for `export --from`). The CLI does not reopen S or infer E
from a filename. A single-root Bundle binds both selectors to the actual final
Bundle file; upload returns both actual Manifest identities for Snapshot. Local
refs expose only basenames and preserve `@digest`, `@hmac`, `@manifest` and any
location. The output directory stays in the caller's execution context. Invalid
or incomplete responses produce an error and no successful JSON. `--resume`
uses the same identity contract; existing default human output and upload-key
stdout remain available without `--json`.

For paired RFC-142 deployment, upgrade sandboxer together with orchestrator.
Operators must clear the old orchestrator local database before upgrading and
recreate records; neither component migrates/backfills it or automatically
removes user data. Old migration tokens missing the required `resumeSandboxRef` field, including
Snapshot tokens without E, must be discarded and reissued from complete S/E records. Portable Export uses that recorded pair even
when artifacts are unavailable; it never reads S to discover E.


`--drop-caches` belongs only to memory snapshot and defaults to false. `--merge-ref` selects current-plus-one-history working-set capture (`false`) or current-plus-local-history absorption (`true`). Both stop at the current checkpoint ownership boundary. Disk compaction is independent; see [checkpoint history](sandbox.md#checkpoint-history).

A local `snapshot` needs either `--sandbox-id` or `--path-id`. When both are supplied, PathID only selects `RunRoot/PathID/ctl.sock`; no identity consistency check is performed. `--output` may reside on the BaseRoot filesystem, for example `BuildBaseDir/checkpoint`. The running process cleans its own RunDir/owned diffs and does not treat output as a runtime directory to delete.

### 2.4 `sandbox-ctl export`

Live export syntax:

```text
sandbox-ctl export \
  --path-id a \
  --run-root /run/sandbox/builds/build-01 \
  (--output /artifacts | --upload) \
  [--mode local|bundle] \
  [--resume]
```

Image-to-Sandbox-E assembly syntax, without starting a VM:

```text
sandbox-ctl export \
  --from ./flattened.img \
  --config sandbox.yaml \
  --sandbox-id base \
  (--output /artifacts | --upload) \
  [--mode local|bundle]
```

Live mode rejects `--config` and reuses the running process's validated Manifest/ref-location/crypto bindings. Explicit `--manifest-config` and `--ref-location` therefore belong only to assembly mode. Assembly requires `--config` or `SANDBOX_CONFIG` and rejects `--resume`. Both modes support `--timeout`: live mode bounds the client's ctl-socket wait, while assembly sets a context deadline for its own operation; zero disables the respective CLI limit. Neither setting adds a server-side deadline to a live-export request. Export accepts neither `drop_caches` nor memory-merge parameters, does not call CH `/vm.snapshot`, does not read the memfd, and produces no memory refs.

Like local snapshot, live mode may use PathID alone. When SandboxID and PathID are both supplied, PathID only locates the ctl socket. Assembly rejects PathID; its existing `--sandbox-id` remains only an output-artifact alias.

### 2.5 `sandbox-ctl exec`

```bash
sandbox-ctl exec --path-id a --run-root /run/sandbox/builds/build-01 -- /bin/sh -c 'id'
```

Local exec may use only `--path-id`. Supplying both `--sandbox-id build-01-phase-a --path-id a` still dials only `RunRoot/a/ctl.sock`; the request gains neither a SandboxID nor a consistency check. Without PathID it continues to use `RunRoot/SandboxID/ctl.sock`.

Exec creates a sibling process through the current ctl/MUX. The export/snapshot quiesce gate atomically prevents new exec/forward sessions from entering the unstable window, and closes and joins already admitted sessions. In-flight execs are terminated and do not automatically rerun after `--resume`. Restore/attach reopens exec, forwarding, plugins, and app restart only after establishing the new MUX and thawing the application cgroup. Requests racing between ACK and thaw are rejected by the gate rather than forking into the frozen cgroup. Guest `attach` is an idempotent recovery operation. The host immediately retries once when the request/ACK boundary is ambiguous; lifecycle-context cancellation controls the entire dial/ACK sequence.

Remote authorized exec uses `pkg/ctl.ServeExecTunnel(ctx, options)` in this fixed order:

```text
Authorize -> AcceptDownstream -> ReadExecRequestFrame -> AuthorizeRequest
          -> DialBackend -> write frame.Raw once -> duplex relay
```

Before request admission succeeds, the caller must not dial the backend or trigger Sandbox lifecycle changes. The first frame's JSON payload is limited to 64 KiB, plus its four-byte length prefix. It must contain one `exec_request` with strict field sets, no duplicate fields, and a nonempty `exec.argv` array. Reading waits at most 10 seconds; callers may only shorten that limit. After admission, the original frame is written exactly once without decode/re-encode. Recognized request/backend rejections expose only the sanitized `exec request rejected`; `ProxyExec` remains available only for callers with an already connected backend.

### 2.6 `sandbox-ctl config`

```bash
sandbox-ctl config --template
sandbox-ctl config --config host.yaml:instance.yaml --check strict
sandbox-ctl config --config restore.yaml --mode restore --check strict
```

The template identifies portable, host-only, and ephemeral fields. Restore mode emits a host-only document, removing cold-only workload fields and immutable disk refs. It does not introduce a second restore schema.

### 2.7 `sandbox-ctl info`

```bash
sandbox-ctl info [--json] [--manifest-config manifest.yaml] <artifact>
```

- For E, output is `sandbox.runtime.cfg`.
- For S, default output is the original minimal `snapshot.cfg`, containing at least `sandbox_ref`.
- For S, `--json` preserves the resolved view needed by existing machine callers: `Version`, `SandboxRef`, and memory `FromRefs` come from S; `Resources`, `Boot`, `Launch`, and `Metadata` derive only from the E referenced by S. This view is not written back to S and is not a second snapshot provenance schema.
- Malformed, ambiguous, or logical roots that are neither E nor S fail closed.

Local crypto, Manifest, Bundle, selectors, and named ref-locations use the same opener as run/publish.

### 2.8 `sandbox-ctl publish`

```bash
# Publish to Manifest store.
sandbox-ctl publish --manifest-config manifest.yaml ./s1.snapshot

# Publish to a trusted named file location.
sandbox-ctl publish \
  --manifest-config manifest.yaml \
  --to-ref-location release=file:///srv/sandbox-artifacts \
  --ref-location source=file:///srv/source \
  ./s1.sandbox
```

<a id="reference-rewriting"></a>

#### 2.8.1 Reference rewriting and whole-chain reduction

`publish` accepts local paths, located tarstream refs, Bundle selectors and
`manifest://` roots. The logical root is Sandbox E or Snapshot S. `PublishSource`
also accepts an already assembled Snapshot while retaining caller ownership.

```text
sandbox-ctl publish [existing storage/location options]
  [--replace-ref OLD=NEW ...]
  [--reduce-ref A=X | A | any ...]
  [--skip-verify-ref=false|true] [--json] [--quiet]
  SOURCE
```

`upload-snapshot` uses the same implementation. One-to-one replacement keeps
ordered layer positions. For Snapshot input, the current E's internal disk
references are included: E is republished and the resulting ref is installed in
`sandbox_ref` before the new S is published. Rules match original positions once.

Each replacement rule exclusively owns both its OLD and NEW references. Rule
pairs must have disjoint endpoints: repeated rules, shared sources or targets,
chained replacements and cycles are overlap errors. A replacement and a
reduction must also be disjoint across the reduction's original top, every
lower and its explicit target. This includes automatically merged chains.
`--reduce-ref=any` is used without replacement rules. Independent operations
on disjoint references/devices may share one invocation.

Canonical endpoint overlaps are rejected during argument validation. Chain
membership is checked from the original S/E metadata before replacement reads,
equivalence checks or writes. Both values of `--skip-verify-ref` use the same
rule validity checks.

A reduction selects a chain's top. `A=X` verifies that X represents the full
`[A, lowers...]` view, installs X and clears the lower list. `A` generates that
result automatically. `any` applies automatic reduction to the current memory
and each device's complete explicit chain. A self-bound root uses the source E
ref as its top selector and keeps its generated payload inside the new E, with
`self` preserved. The source S ref selects its embedded memory; its existing
execution state accompanies the new memory payload. EROFS and writable upper
remain separate devices, as do individual data disks.

The plan resolves scopes, validates configuration/roles/capacities, tracks all
explicit matches and proves requested replacements before writing dependencies.
Default equivalence first compares usable trusted payload commitments, then
streams size, Hole/Present layout and present bytes when needed. Explicit Zero
and stored zero bytes are equivalent; Hole retains transparent lower-layer
semantics. `sandbox_ref` equivalence also checks non-reference configuration and
each device's effective view. Different Manifest keys across encryption domains
can represent equivalent content.

`--skip-verify-ref` transfers the equivalence assertion to the caller. New input
identity, authentication, schema and applicable capacity checks remain active.
It permits a known replacement or explicit whole-chain target to repair
unavailable old refs. Automatic reduction reads its input layers to build the
result. Unmatched selectors and contradictory rules return errors before any
output is created.

```bash
sandbox-ctl publish --replace-ref "$OLD_BASE=$NEW_BASE" "$SNAPSHOT_REF"
sandbox-ctl publish --reduce-ref "$TOP_REF=$MERGED_REF" "$ROOT_REF"
sandbox-ctl publish --reduce-ref=any \
  --to-ref-location archive=file:///srv/artifacts "$SNAPSHOT_REF"
```

Located output uses an available carrier identity directly. A generated or
Manifest source without one is streamed to compute identity, then reread with
fixed source/codec settings directly into its final content-addressed path.
Payload uses bounded working buffers; only bounded tails and format metadata
are retained. Dependencies complete before the new root. Existing targets are
fully validated, errors preserve old roots, and the operation reports all
source/writer close errors. Published results have valid bindings independent
of transient input-Bundle scope.

<a id="publication-result"></a>

#### 2.8.2 Publication result report

`publish --json` and the `upload-snapshot --json` alias emit exactly one success
JSON object after publication, validation and resource closes succeed. Progress
stays on stderr; `--quiet` suppresses progress and does not change the result.
Without `--json`, stdout remains the single final root reference followed by a
newline. An output write failure is a command failure.

Sandbox E:

```json
{"sandboxRef":"manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","removedRefs":["file://old.sandbox@digest:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"]}
```

Snapshot S:

```json
{"snapshotRef":"manifest://bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","sandboxRef":"manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","removedRefs":["file://old.sandbox@digest:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","file://old.snapshot"]}
```

Sandbox has only `sandboxRef` and `removedRefs`; Snapshot additionally has
`snapshotRef`. Snapshot's `sandboxRef` is the E actually referenced by final S,
including a reused E or the selector of its chosen Bundle carrier. For example,
an E in the final located Bundle is `file://<carrier>.bundle@manifest:<E-key>@location:<name>`;
an E chosen from remote storage remains `manifest://<E-key>`. Equal content keys
in different carriers, locations or domains do not by themselves identify the
same reference. Empty `removedRefs` is `[]`, never `null`.

`removedRefs` is the original topology minus the retained final topology within
the operation's existing parsing boundary. It includes old roots and known local
or remote disk/memory refs that leave that topology. Original config fields and
native lower lists are recorded before mutation. Final adopted and reused refs
count even when no object is written; a ref still used by another device or
output chain is excluded. Ordinary publication, replacement, all three reduction
forms, portable shortcuts, memo reuse and exact Bundle transfer use this same
operation-local accounting. Repeated publisher calls do not inherit old refs.
Historical S used as memory and historical E used as disk retain their existing
payload-only interpretation. `self` belongs to E and is not a separate ref.
Unchanged opaque branches retain their known boundaries without descendant scans.

With `--skip-verify-ref`, an old replaced ref is a leaf whether readable or
missing. Reporting never opens old E or its descendants to discover removals.
An explicit native `(base=A, base_from_refs=[B,C])` still knows all of A/B/C,
including the explicit `--reduce-ref A=X` shortcut. Default verification may
reuse the original config it already parsed for equivalence. Reporting adds no
object scans, payload reads, digests or temporary payload files in either mode;
it keeps only bounded ref/binding metadata. Identity/authentication/schema/size
checks and disjoint replace/reduce legality are unchanged. Automatic reduction
still reads the data required to construct its result.

Every public unlocated file ref contains only its basename, preserving an
existing `@digest`, `@hmac` or `@manifest` qualifier. Absolute/relative directory
components, mount paths and traversal are never emitted, and reporting does not
invent a location or compute a missing identity. Named-location refs retain
their valid spelling. Comparison first uses the complete internal source scope;
basename projection happens only afterward, followed by public sorting and
deduplication. Callers supply the checkpoint directory externally. No path,
context, identifier or field-mapping objects are added to JSON, and diagnostic
stderr is never embedded in successful results.

The report does not delete objects or grant deletion authority. A Bundle
selector leaving the topology does not mean the physical Bundle is unreferenced;
other checkpoints, retained versions or sandboxes can still use it. Source
retention and garbage collection remain their owners' responsibilities.

The library retains `PublishResult.Role`/`Ref` and adds `SandboxRef` and
`RemovedRefs`; `result.Report()` produces the safe public `PublishReport`.
Image-only `PublishSource(ctx, RoleImage, source)` callers are unchanged. For an
already assembled Snapshot, the caller supplies its known final E:

```go
result, err := publisher.PublishSource(ctx, artifact.RoleSnapshot, source, cfg.SandboxRef)
```

This creation path owns no original root, so its removals are empty. It does not
rescan the assembled source and leaves source ownership with the caller.

```bash
sandbox-ctl publish --json --quiet --to-ref-location release=file:///srv/sandbox-artifacts ./s1.snapshot
```

<a id="usage-query"></a>

### 2.9 `sandbox-ctl usage`

`sandbox-ctl` owns observation scheduling, CPU differences, gauge integration,
sampled peaks, merging and persistence through `pkg/usage`. `usage.Snapshot`
is cumulative state; `usage.Record` is a self-contained saved checkpoint.
`sandbox-init` only reads and parses raw observations on request. It has no
usage ticker, accumulator, historical summary or reliable replay queue.

Usage is optional and is not a bill, resource limit or physical-cost attribution.
It covers Guest execution CPU (total and per vCPU), Guest physical memory,
managed writable filesystem occupancy, and CH/sandbox-ctl process CPU and
separate `RssAnon`/`RssFile`. It excludes UFFD, network and disk-operation
counts, bytes and latency. Existing diagnostic telemetry and resource control
remain separate. There is no daemon, exporter, billing policy, consumer ACK,
global WAL or post-deletion retention service.

```bash
# Read current in-memory and confirmed saved state from the running owner.
sandbox-ctl usage --sandbox-id s1

# PathID locates directories; SandboxID remains the file/record identity.
sandbox-ctl usage --sandbox-id s1 --path-id instance --saved

# Read a stopped sandbox without starting a VM or changing the file.
sandbox-ctl usage --sandbox-id s1 --offline
sandbox-ctl usage --file /var/lib/sandbox/s1/s1.usage

# Bounded history page; pass next_cursor as the next --cursor.
sandbox-ctl usage --sandbox-id s1 --history --limit 10 --cursor 0
```

Output is lossless JSON. `--json` defaults to true. `--run-root` defaults to
`/run/sandbox`; `--base-root` defaults to `/var/lib/sandbox`. `--path-id`
defaults to SandboxID. `--timeout` bounds a query, including offline recovery/history (default 5 s,
strictly positive). A nonexistent/refused ctl socket permits offline fallback;
other connection/protocol failures are errors. `--file` implies `--offline`
and can infer SandboxID from the filename without `.usage`.

`--saved` omits `live`. History uses a nonnegative byte cursor and a record
limit of 1–100, default 10. The host usage response is bounded to 1 MiB; reduce
the page size if a page cannot fit. Both online and offline readers decode and encode one
bounded record at a time, rejecting an oversized JSON page before retaining
or encoding the complete requested record set, including string-escaping
expansion. History does not return the active tail.
Ordinary ctl requests retain their existing framing and limits.

The view contains `enabled`, optional `live` and `saved`, `saved_end`,
`saving`, `unknown_tail`, and optional `save_error`/`read_error`. Online `saved`
is the owner's adopted baseline: a complete surviving record recovered at
startup, or a later complete append this owner has confirmed. A readable CRC
from a pending write cannot alone advance that baseline. Offline `saved` is
the last complete surviving record validated by recovery, not proof that its
former writer confirmed the append. Neither view guarantees survival of host
crash or power loss; the writeback boundary is defined in §4.7. `live` includes
accepted input still in flight or not yet submitted for saving; it can roll
back to surviving saved state after a crash. Missing observation, unsaved
input and an uncertain file tail are different conditions.

Live state is copied between individual metric merges, not as an atomic
whole-round observation. Read each Gauge's own `last_request_id`, status and
position; memory may already reflect a recovered request while a filesystem
still describes the preceding missing request. Queries do not wait for a
whole-round boundary or change either metric's accepted input.

Online queries only copy existing state or read confirmed history. They do
not sample Guest/CH, advance counters, or flush. Offline readers take a
nonblocking shared file lock and reject an active writer: use its ctl socket
instead. Disabling usage does not remove an existing file; offline reading
remains available.

The CLI uses [`pkg/usagereader.Read`](../pkg/usagereader/read.go), the common
read entry for local conductor adapters. Callers resolve the current object's
control socket and saved-file path using their existing lifecycle authority,
then supply the exact SandboxID, context, snapshot/saved/history selection,
cursor and limit. The reader uses the existing `Recover`, `ReadHistory` and
file lock; [`MarshalHistory`](../pkg/usagereader/history.go) is the same bounded
page encoder used by the online owner. It does not add another codec, recovery
algorithm, accumulator or persistent state. Native JSON is retained without a
floating-point intermediate; a different SandboxID in live, saved or history
data is rejected. The ctl response envelope always includes the owner SandboxID, even when
usage is disabled, live/saved are absent or history is empty. This does not
change native View/Record or the file format. Empty history has `records: []`
and its validated next cursor. Online pages require an explicit, nonregressing
cursor: nonempty pages advance it, empty pages retain it, and record count cannot
exceed the requested limit. A saved view requires a positive `saved_end` exactly
when a saved record exists. Invalid owner replies never trigger offline fallback.
Offline open uses a nonblocking flag and rejects
FIFOs and other nonregular files without waiting for a writer beyond timeout.

Only failure to connect to an absent/refused socket permits fallback. Once
connected, EOF, invalid replies, owner errors, cancellation and deadlines fail
the query; a readable saved file does not replace them. Cancellation closes
the active ctl connection. The completed owner response is also checked for
cancellation after JSON decoding, validation and saved-view projection. Offline cancellation stops the caller's wait and is
checked between file reads. A blocked file syscall retains its execution slot
and shared lock until it returns; there are at most eight offline executions
per process, including canceled ones. Waiting for a slot also honors the query
deadline. This does not make file syscalls interruptible or change native saving.
Telemetry
consumes native stats through conductor, never this ctl/file reader directly.

<a id="journal-output-targets"></a>

### 2.10 Journal output targets

Each journal output is configured independently:

```text
journald=<tag>[,<FIELD>=<VALUE>...]
```

`run` accepts this target in `--stdout-to`, `--stderr-to`, `--console`, and
`--log-to`. `exec` accepts it in its existing `--stdout-to` and `--stderr-to`
options; the guest console belongs to the running sandbox, not an exec client.

```bash
sandbox-ctl run --config sandbox.yaml \
  --stdout-to 'journald=app,WORKLOAD_ID=worker-42,STREAM=stdout' \
  --stderr-to 'journald=app,WORKLOAD_ID=worker-42,STREAM=stderr' \
  --console 'journald=console,WORKLOAD_ID=worker-42' \
  --log-to 'journald=sandbox-ctl,WORKLOAD_ID=worker-42'
```

The tag becomes `SYSLOG_IDENTIFIER`. Extra fields belong only to that output:
there is no global field option, environment discovery, interpolation, or
inheritance between targets. Equal tags do not share fields or partial-line
buffers. A bare `journald=app` remains valid and has no extra fields. Callers
that previously relied on implicit identity environment variables must now
include their fields explicitly in every required target.

Sandboxer does not choose or interpret application, orchestration, or identity
fields. The caller selects the tag and non-secret values. They are host-side
output configuration: they do not enter guest environment variables, the guest
stdio protocol, sandbox YAML, portable artifacts, or snapshots. Every new run
or exec invocation supplies its own targets.

#### 2.10.1 Syntax and encoding

The tag is a nonempty `[A-Za-z0-9_-]` token. Field names are 1–64 ASCII bytes,
start with `A`–`Z`, and contain only uppercase letters, digits, and underscores.
Names beginning with `_` are reserved for journal trusted metadata. This
interface also reserves `MESSAGE`, `PRIORITY`, and `SYSLOG_IDENTIFIER` for the
writer and rejects duplicate field names. This single-value interface does
not imply that the underlying journal protocol forbids all repeated fields.

Fields are separated by literal commas. Each field is split at its first `=`:
`FIELD=a=b` has value `a=b`, and `FIELD=` explicitly supplies an empty value.
Values use percent encoding with Go `url.PathEscape`/`url.PathUnescape`
semantics. Splitting happens before decoding, and decoding happens once:

```text
journald=app,NOTE=a%2Cb,EXPR=x=y,PERCENT=100%25,PLUS=a+b,ONCE=%252C
```

This produces `NOTE=a,b`, `EXPR=x=y`, `PERCENT=100%`, `PLUS=a+b`, and
`ONCE=%2C`. A plus sign is not a space. `%20` is a space; encoded newlines and
other bytes remain field data, not new fields. There is no shell-variable or
backslash expansion inside the target parser. Quote the complete target when
writing shell commands; callers using `exec.Cmd` should pass it as one argument.

Missing `=`, empty names or segments, trailing commas, duplicate/reserved or
invalid names, malformed percent encodings, and invalid tags are command-line
errors. The encoded target is limited to 64 KiB and at most 64 extra fields.
These are local resource bounds, not claimed journal protocol limits. Ordinary
non-journal file paths are not decoded or split, including paths containing
commas, `=`, or `%`.



#### 2.10.2 Component logs versus guest outputs

`run --log-to default` (also the default when omitted) retains ordinary component
diagnostics on stderr. `run --log-to journald=...` explicitly sends Go logger
output and run/restore operational diagnostics to that target, including
startup preparation failures and terminal errors after target parsing. It does
not require journal-connected stderr or any particular identity field.
Invalid flag syntax and invalid `--log-to` values can still be reported on the
original stderr before a usable target exists.

`--stderr-to` remains the guest application's stderr destination. `--log-to`
does not take over process descriptors, change TTY/pipe selection, redirect
Cloud Hypervisor's own stderr, or capture runtime panic/system events. Existing
message text and log timestamp precision are retained. `exec`'s own diagnostic
stderr and error-capture behavior are unchanged. File, artifact, JSON, and other
non-journal stdout data remain unaffected.



#### 2.10.3 Framing, failure, and lifetime

Each output has its own line buffer. Complete lines are emitted at info priority;
ordinary CRLF endings are normalized and empty lines are omitted, as before.
Very long lines are split into messages of at most 60 KiB without first copying
the whole input into an unbounded buffer. A forced size boundary does not strip
carriage returns from the middle of a line. A final partial line is flushed on
Close. Producers are drained before their writer is closed; Close is idempotent,
and writes after Close return `io.ErrClosedPipe`.

A successful native send is not duplicated to stderr. A failed send is followed
by a best-effort write to the original fallback sink. Guest/console fallback
retains its `[tag]` prefix; component fallback does not add another prefix to
existing diagnostic text. Neither a native-send error nor a fallback-write
error terminates the sandbox. Fallback text does not retain structured fields.
Native journal I/O is synchronous: this is not an asynchronous queue or a
promise of nonblocking, lossless, exactly-once durable storage.

Use `journalctl WORKLOAD_ID=worker-42 -o json` to inspect the fields in the
journals available to that command. Cross-node history requires a separate
collector or access to the other nodes' journals; no exporter is added here.

Protocol references: [Journal Native Protocol](https://systemd.io/JOURNAL_NATIVE_PROTOCOL/)
and [Go net/url](https://pkg.go.dev/net/url).

## 3. Configuration and artifact formats

### 3.1 `sandbox.yaml`

`sandbox.yaml` is host input and may contain a portable workload, host policy, and instance data. This example illustrates ownership, not prescribed deployment values:

```yaml
resources:
  capacity: { cpu: 2, memory: 2GiB }
  allocatable: { cpu: 1.5, memory: 1536MiB }
  # Host-only policy:
  control: { cgroup_path: /sys/fs/cgroup/sandboxes/s1 }
  overhead: { memory: 128MiB }
  watermark_high: { ratio: 0.875 }
  startup: { memory: 512MiB }
  diff_cow: { cache_size: 32MiB, max_dirty_size: 16MiB }

network:
  # Host provider and current identity:
  tap: tap0
  interface: eth0
  ip: 169.254.1.1/31
  mac: "02:00:00:00:00:01"
  hostname: sandbox-1

boot:
  kernel: file:///opt/kuasar/vmlinux
  runtime: file:///opt/kuasar/sandbox-runtime.bundle
  cmdline: "console=hvc0"
  root:
    base: file:///images/root.erofs@digest:<digest>
    overlay:
      base: file:///layers/parent.overlay@digest:<digest>
      base_from_refs: []
      # Host-only active binding:
      diff: file:///var/lib/kuasar/s1.overlay.diff
      diff_template: file:///opt/kuasar/empty.ext4
  disks:
    - name: data
      base: file:///layers/data.overlay@digest:<digest>
      diff: file:///var/lib/kuasar/s1.data.diff

mounts:
  - { target: /data, type: disk, source: data }
  - { target: /tmp, type: tmpfs }
  - { target: /var/cache/app, type: empty }

files:
  - { path: /etc/app/config, content: "portable" }
ephemeral_files:
  - { path: /run/instance-token, content: "current-run-only" }

init:
  - { exec: /bin/sh, args: ["-c", "prepare-data"] }

launch:
  exec: /usr/bin/app
  args: ["serve"]
  env: { MODE: production }
  ephemeral_env: { REQUEST_ID: current-run-only }
  plugin:
    - { exec: /usr/bin/sidecar, restart: always }
  restart: never

metadata: { workload.kind: api }

restore:
  prefetch: off
timeouts:
  ch_api: 30s
usage:
  enabled: false
  sample_interval: 1s
  flush_interval: 5m
```

Ordinary cold run performs full validation. `run --from` and `run --restore` first strictly parse the artifact, then apply their respective field-presence rules. An unconstrained `LoadMerged` must not overwrite the artifact graph.

### 3.2 Files, environment, and ephemeral data

Cold-launch merging:

```text
files < ephemeral_files       # same path: ephemeral wins
launch.env < ephemeral_env    # same key: ephemeral wins
```

Duplicate paths within either files list are rejected. Persistent `files` and `launch.env` enter C0/C1; ephemeral fields do not enter the Portable schema.

Runtime semantics:

- sandbox-init injects `files` using tmpfs backing and bind mounts.
- Export saves file declarations, not runtime modifications to injected files.
- `run --from` reinjects persistent files and reapplies persistent environment.
- `ephemeral_files` and `ephemeral_env` affect only the current cold invocation.
- Ephemeral does not mean erased from a memory snapshot: RAM can still contain the data. If the guest copies it into an exported disk-backed file, excluding the declaration also does not erase that copy.
- Disk-backed `type: empty` volumes export with their owning root/data disk.
- `type: tmpfs` contents do not accompany export.
- `init` reruns on `run --from`.
- Plugins/apps restart with cold semantics.
- `run --restore` does not rerun launch/files/init/plugin.

If the restore host explicitly supplies `boot.cmdline`, persistent/ephemeral launch fields, mounts, files/ephemeral_files, init, or metadata, validation rejects them before side effects instead of silently ignoring them. `resources.startup` is host-only node policy and may be supplied on restore. It is included in the node reservation contract, but the Snapshot's captured `BudgetAtSnapshot` remains authoritative for the initial restore Budget.

<a id="usage-policy"></a>

### 3.3 Usage policy

```yaml
usage:
  enabled: true
  sample_interval: 1s
  flush_interval: 5m
```

The default is `enabled: false`, `sample_interval: 1s`,
`flush_interval: 5m`. Durations must be positive Go duration strings,
`flush_interval >= sample_interval`, and twice the sample interval must fit
the supported signed duration range. Unknown keys, duplicate keys and wrong
scalar types are errors. File overlays apply in order; omitted usage members
retain the preceding value and explicit false disables sampling.
`flush_interval` schedules record appends, not filesystem synchronization or
device-cache flushing.

Usage is host policy in ordinary cold start, `run --from`, and `run --restore`.
It is excluded from `PortableSandboxConfig`, E and S. Current host policy,
not the artifact, selects it after restore. See the
[host-overlay example](../examples/usage-enabled.yaml).

When disabled, the host creates no usage sampler, long-lived usage connection
or periodic save. Existing resource-control reporting and balloon policy are
unchanged. Guest disk assembly still retains its bounded private filesystem
handles so a later memory restore can enable usage; retaining a handle does
not schedule reads.

<a id="diff-cow-policy"></a>

### 3.4 `resources.diff_cow` and cache ownership

```yaml
resources:
  diff_cow:
    cache_size: 32MiB
    max_dirty_size: 16MiB
```

Both fields are positive multiples of 4 KiB, with
`0 < max_dirty_size <= cache_size`. Omitted fields independently default to the
values above, including old configs without `diff_cow`. A single supplied field
must still satisfy the relationship after defaults. These are finite engineering
defaults, not a production SLA or a claim of optimal performance. There is no
zero/unlimited value, disable switch, failed-data-I/O buffered retry or
configurable alternate write mode.

`max_dirty_size` is a subset of `cache_size`, never an additional pool or a
reserved partition. Clean pages may use the entire cache when there are no dirty
pages. Adding a disk does not multiply either limit. These host process resources
are independent of guest capacity, allocatable/startup headroom and VMM overhead:
`sandbox-ctl` is outside the VMM cgroup. No resource-controller changes are needed.

Cold start, `run --from`, and restore resolve host resources for this instance.
Runtime configuration retains the policy for that sandbox's lifecycle; a new
restored instance uses its current host configuration. Portable C0/E/S never
contains cache policy, pages, queues or LRU state. Strict restore validation,
config merging and host projection preserve this boundary.

Go callers can pass one `COWCache` with `WithCOWCache` to multiple `OpenBlockCOW`
calls. Close the COWs before closing that cache. Without this option, a standalone
COW owns a finite default cache and closes it itself.

<a id="portable-config"></a>

### 3.5 PortableSandboxConfig

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

- Cgroup paths/fds/controllers, overhead, watermark, startup, and `resources.diff_cow` cache policy.
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

<a id="strict-encoding"></a>

### 3.6 Strict encoding and limits

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

<a id="source-binding"></a>

### 3.7 C0, C1, and source binding

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

<a id="self-provenance"></a>

### 3.8 `self` and disk provenance

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

<a id="sandbox-format"></a>

### 3.9 `.sandbox` logical format

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

<a id="snapshot-format"></a>

### 3.10 `.snapshot` logical format

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

## 4. Resource model

### 4.1 Three deployment modes

| Mode | `cgroup_path` | `controller` | Behavior |
|---|---|---|---|
| No cgroup | Empty | Empty | No cgroup writes; allocatable CPU must equal capacity CPU, and allocatable memory must remain within capacity |
| Static cgroup | Set | Empty | Configure local CPU/memory limits and optionally run the local sensor |
| Dynamic | Set | Set | Use resource-protocol admission/leases/Budget and run the local sensor |

`resources.capacity` is guest-visible VM capacity and enters Portable configuration. `resources.allocatable` is the cold-start workload default and also enters Portable configuration. `allocatable.cpu` must be finite and satisfy `0 < allocatable.cpu <= capacity.cpu`. Restore retains capacity and the captured `deflate_on_oom` from E, but can explicitly reapply the destination node's allocatable CPU/memory. This runtime policy does not rewrite E or C0. `control`, `overhead`, `watermark_high`, `startup`, and `diff_cow` are node policy and do not enter E.

Export/snapshot acquires the MemoryController mutation barrier and, before freezing, lifts and drains any `memory.high` operation that could race with CH pause. The host ping gate lets an admitted probe complete both `pong` and the guest-EOF transport barrier. This drain has its own 8-second quiesce budget; capture cannot wait forever merely because ordinary `timeouts.ping` has no forced timeout. On expiry, the host cancels and joins the probe, and failed capture enters full recovery. Guest quiesce also drains and pauses periodic `mem_report`, preventing S from capturing a reporter that holds the stream lock while waiting on the old host vsock. Restore switches to a new observation epoch before ACK; `--resume` and failed-capture attach recover the original epoch. A live attach only reopens the pause gate and does not corrupt accounting for already admitted reports. Recovery releases the host barrier after VM, MUX, app, and backends recover.

### 4.2 Memory terms

```text
CapacityMemory      = CH boot memory zone maximum
AllocatableMemory   = workload settled guest headroom
DemandMemory        = controller observation
Budget              = admitted/locally enforced working allowance
VMM memory.max      = CapacityMemory + host overhead
```

Memory S capture records CH configuration/state and sparse memfd content. Resource policy is not written to `snapshot.cfg`. Restore obtains capacity identity from E, and any explicitly supplied host capacity must match. Allocatable CPU/memory, `startup`, `overhead`, `watermark_high`, and resource-controller bindings come from the current host. `deflate_on_oom` must remain consistent with captured VMM state. The Snapshot's `BudgetAtSnapshot` remains authoritative for the initial restore Budget.

### 4.3 CPU and balloon

Capacity CPU determines vCPU topology. In cgroup mode, allocatable CPU maps to `cpu.weight`; without a cgroup it must equal capacity CPU, so a separate fractional allocation cannot be expressed. CH restore state authoritatively restores the current balloon state; the host does not invent another balloon state from S.

<a id="writable-disk-capacity"></a>

### 4.4 Writable disk capacity and migration

Writable disk capacity is inherited from the selected filesystem source, not
from a separate size setting. An existing non-empty active `diff` keeps its
logical capacity. A fresh diff initialized from `diff_template` inherits the
template's logical capacity; without a template, a fresh diff over a COW `base`
inherits that base's logical capacity. An empty existing diff or a fresh disk
without a filesystem source remains invalid. In two-device overlay mode, this
COW base belongs to the writable ext4 upper, not the read-only EROFS image.
These rules apply to both `boot.root` and `boot.disks[]` and remain unchanged
for cold starts, `run --from`, and memory restores.

The former `diff_size` setting never enforced capacity or a quota in these
supported paths. It has been removed, together with its misleading 1 GiB
default. Configurations that explicitly supply `diff_size` or the mistaken
`size` spelling directly under a root/data disk or its `overlay` now fail with
an actionable error, including empty and null values. Remove these keys from
existing configuration and provision a filesystem source with the required
capacity. New configuration output does not emit them. Unrelated fields and
opaque metadata are unaffected.

For new disposable ext4 work disks (overlay uppers, single roots and scratch
or data-disk fixtures), format the sparse template with `mkfs.ext4 -O ^has_journal`.
This avoids filesystem journal allocation and metadata journal writes; it does
not disable journald or application logs. COW is not a replacement for a journal,
and this default does not promise crash recovery of an interrupted work disk.
Existing journaled templates, user-supplied images and snapshots remain compatible;
guest sync/quiesce and snapshot/restore semantics are unchanged. Do not reformat
an existing data disk to apply this recommendation.

For a **new**, empty 512 MiB scratch filesystem, prepare a new template and
select it without a size override:

```bash
truncate -s 512M /tmp/scratch-512m.ext4
mkfs.ext4 -F -O ^has_journal /tmp/scratch-512m.ext4
```

```yaml
boot:
  # Keep the other required boot fields from the complete configuration.
  disks:
    - name: scratch
      diff_template: file:///tmp/scratch-512m.ext4
mounts:
  - target: /scratch
    type: disk
    source: scratch
```

Use a new active diff: an existing diff takes precedence, so changing a template
does not resize or replace existing data. Do not truncate an existing filesystem
to impose a smaller limit. Sandboxer does not automatically resize filesystems,
cap materialized COW storage (including tmpfs file memory/swap; pending cache writeback is bounded separately by `resources.diff_cow`), or change disk capacity at startup or restore. Logical block
device capacity is distinct from an encrypted file's physical length, host disk
allocation, and the guest filesystem's available file-data space. This migration
does not introduce arbitrary per-instance capacity selection from one template.

<a id="usage-metrics"></a>

### 4.5 Usage metrics, units and arithmetic

This section is the unit and formula reference for the CLI, protocol and
lifecycle documents. All JSON counters, sizes, durations, positions and
128-bit values are decimal strings, not floating-point numbers. CPU totals
use ns; occupancy uses bytes; area uses byte·ns; span/covered use ns. UTC
fields use Unix ns for correlation only. Positions are signed monotonic ns
relative to `Snapshot.run_epoch`/`started_utc_ns`, never times that can be
subtracted across runs.

| Name | Source and meaning |
|---|---|
| `guest.cpu` | CH process `/proc/PID/stat` `guest_time` |
| `guest.vcpu.N` | Validated `vcpuN` task's `guest_time` |
| `ch.cpu`, `sandbox_ctl.cpu` | Respective process `utime + stime` |
| `guest.memory` | Guest non-free physical RAM after subtracting live balloon current |
| `filesystem.root`, `filesystem.disk-N` | Occupancy of one managed writable ext4 filesystem |
| `ch.rss_anon`, `ch.rss_file` | CH status `RssAnon`, `RssFile`, separately |
| `sandbox_ctl.rss_anon`, `sandbox_ctl.rss_file` | sandbox-ctl status `RssAnon`, `RssFile`, separately |

#### 4.5.1 CPU counters

`guest_time` is already part of `utime`; it is never added to process CPU a
second time. Host boot identity, PID/TID and starttime identify each source.
The parser locates the end of `comm` before indexing fields, including names
with spaces/parentheses. Valid vCPU mappings are cached and revalidated;
missing threads are not assigned a share of the process total.

`AT_CLKTCK` in `/proc/self/auxv` supplies USER_HZ without libc, cgo or a
`getconf` child. Linux amd64 and arm64 use their verified little-endian ELF64
auxiliary-vector layout. Invalid/missing/duplicate values fail the reader.
Kernel `CONFIG_HZ` is not this conversion scale. Both binaries remain
`CGO_ENABLED=0`.

Counters retain known cumulative ns, native raw ticks, scale, conversion
remainder and source/completeness state. A known-created source includes CPU
before its first poll. Integer conversion retains the remainder across polls
instead of rounding every interval independently. A source regression does
not underflow or silently subtract known usage. After a missed poll the same
native counter can recover its delta; this does not recover Gauge coverage.
Changing a source cannot certify the old source's unknown terminal segment.
For an already identified vCPU, transient reads retain the known baseline and
the next successful same-identity counter covers the missed ticks. An unknown
first identity, confirmed thread exit/replacement, ambiguous mapping or failed
terminal read remains incomplete; rediscovery never restores lost completeness.
Source-loss validity is bounded per vCPU and survives a rejected result; the
rejected result itself never contributes CPU ticks or changes its timestamp.

CH's sole wait owner uses `waitid(WNOWAIT)`, retries EINTR, attempts the final
read while proc state still exists, and then calls `cmd.Wait`. It does not
add another reaper or alter shutdown escalation/output draining. Unknown
thread terminal segments remain incomplete even when the total is available.
The live sandbox-ctl cannot observe CPU consumed after its own final read;
its saved terminal counter therefore retains a known total but is incomplete.
When a new Host owner reopens an existing usage file for further accumulation,
it preserves known CPU totals but marks cross-process history incomplete in
`live`. Saved/offline records retain their completeness flags as recorded;
those flags describe the stored prefix, not any later unknown tail.
Even a normally closed last record cannot
exclude an intervening run that consumed CPU and crashed without writing a
record. Empty files and partial appends have the same limitation. Only a
newly created file can establish the logical history's known starting point;
there is no synchronous startup marker or WAL to prove adjacency of runs.

#### 4.5.2 Guest memory and balloon

Guest observation sources are defined in the [raw usage ABI](sandbox-init.md#usage-observations).

```text
G = (PresentPages - BuddyFreePages - PCPFreePages) * PageSize
B = validated CH Capacity - vm.info.memory_actual_size
M = G - B
```

Only `M` enters the Gauge and its peak. `max(G)-max(B)` is not its peak.
Missing fields, inconsistent domains, negative values and arithmetic overflow
invalidate the observation instead of clamping to zero. CH Capacity cannot
replace Guest present pages.

Confirmed no-device means `B=0`. An existing device with unknown, seeded or
stale actual means missing memory, not zero. Actual may be valid while target
differs; convergence is not an admission condition. Desired/accepted target,
resize arguments and controller Budget are not Guest usage.

Only a successful, range-checked CH read publishes actual observation start,
finish, sequence and live instance identity. Target writes and cached returns
do not refresh them; restored seeds are not new reads. The sampler first
reuses qualifying actual, otherwise attempts at most one bounded read-only
query through the existing client. Busy mutation/API locks reject admission
without waiting or spawning replacement workers. Existing control ordering
and single-writer rules remain intact.

The envelope containing the actual CH read and Guest request window must fit
one sample interval. This is a cross-source sampling approximation, not an
atomic physical-memory truth. It needs no Guest Balloon proc field, kernel
patch, inflate/deflate-event fallback or additional CH statistics ABI.

#### 4.5.3 Filesystems and host RSS

Guest observation sources are defined in the [raw usage ABI](sandbox-init.md#usage-observations).

```text
filesystem occupancy = (f_blocks - f_bfree) * unit
unit = nonzero f_frsize, otherwise f_bsize
```

`f_bavail` is not used. Existing occupancy after restore is observed in full,
without subtracting a startup baseline. RSS reads only two process `status`
files, converting their kB units to bytes; it never scans `smaps`. These RSS
items are neither all non-Guest memory nor exclusive physical cost.

<a id="usage-sampling"></a>

### 4.6 Usage sampling and interval summaries

Default observation is once per second and is merged immediately. There is
no second-resolution sequence in memory or on disk. One reusable dedicated
management connection carries `usage_request`/`usage_response`; raw fields,
admission, timeout margin and quiesce rules are in
[Guest ABI §4.11](sandbox-init.md#usage-observations).

Each request has a run-scoped monotonic ID. Late, duplicate or old-generation
results are discarded; reconnect reads only new input. The representative
time is the actual Host request-window midpoint,
`start.Add(end.Sub(start)/2)`, preserving monotonic time. Guest UTC is not
subtracted from Host UTC. Summary `max_read_window_ns` records the largest
accepted window; individual windows are not stored.

For two adjacent valid Gauge observations in the same source/time domain,
with increasing request/time and a gap no greater than twice the sample
interval, use left-end integration:

```text
dt = right_time - left_time
integral_total += left_value * dt
covered_total  += dt
span_total     += dt
```

Failures, explicitly discarded observations, known missed scheduling slots,
source changes, nonincreasing time and long gaps break continuity. A failure
does not fill zero or indefinitely extend the last value. Span and covered
share one domain; missing elapsed time enters span once, not once per error
and again on recovery. Paused/unsupported domains do not add span. A first
sample establishes a baseline and sampled peak, but no area.

Area is a checked unsigned 128-bit integer. Averages between two cumulative
endpoints are `area_difference / covered_difference`, unavailable when the
denominator is zero. Span/covered expose sampling coverage separately.
Peaks are sampled peaks, not guarantees of the continuous maximum.

Saving never extrapolates to an unobserved five-minute boundary. It retains
the last valid endpoint, so an interval crossing a save is closed exactly
once when the right sample arrives. Its left value may participate in that
new interval's peak; an unrelated earlier historical peak does not carry
forward. Record windows contain area/span/covered, sample count, peak/value
position and maximum read window; cumulative totals do not reset on save.

<a id="usage-persistence"></a>

### 4.7 Usage persistence and file format

The sole file is `BaseDir/<SandboxID>.usage`, where the existing lifecycle
derives BaseDir from BaseRoot/PathID. Logical identity is not RunID; normal
restart continues the same file. A clone with a new SandboxID starts separate
history. Usage is outside E/S, so restoring an old business snapshot does not
roll it back.

The host retains a constant number of states:

```text
S = confirmed saved baseline + file offset
F = immutable record currently being saved/reconciled
A = active observations merged while F is in flight
```

Live cumulative values include F and A. A complete `WriteAt` with no error
advances S only to F. A failed or short append can merge its interval
statistics back into A only after successful truncate-to-S establishes logical
rollback. A failed truncate retains F's exact identity/content and reconciles
the tail before the next append. A readable CRC cannot override the current
writer's failed or unfinished append. A new process recovering a complete
surviving record is a different situation.

Usage performs no explicit file, directory or filesystem synchronization:
no `fsync`, `fdatasync`, `syncfs`, `sync`, forced range writeback or synchronous
open flags. This applies to creation, periodic saves, rollback, recovery and
normal shutdown. The operating system and underlying storage govern writeback.
`saved` means logical append completion, not stable-storage durability. Host
crash or power loss may lose already acknowledged records or the file itself,
or leave corruption; recovery can use only the valid bytes that survive.

Each frame has an explicit 16-byte header: `KUUSAGE1`, little-endian uint16
format and algorithm versions (both 1), and uint32 total frame length. The
payload encodes Record/Snapshot fields, source baselines and interval
statistics with explicit signed/unsigned varints, bounded UTF-8 strings and
arrays; 128-bit values encode high/low uint64 halves, not memory dumps.
The 16-byte footer contains CRC32C of header+payload, little-endian uint32
total length, and `USAGEEND`. Maximum frame size is 512 KiB, strings 256
bytes, counters 1,027, and Gauges 14. The ordered codec is
[`format.go`](../pkg/usage/format.go); there is no database or compression
framework.

The Host manager checks identities and metric metadata against these limits
before admitting them; counter and Gauge names share one namespace. It reserves
space for numeric growth when adding metrics or lengthening sources. Individual
string/array limits do not imply that their largest combination fits a frame.
A saved record without sufficient growth space stays readable offline, but
opening a new live owner fails without changing its bytes and releases the lock.
Malformed metadata for an existing Gauge's newer request breaks continuity with
`invalid`, without storing the malformed string; zero, duplicate and older
requests remain no-ops. Empty Gauge sources and empty statuses remain encodable.

Current recovery reads at most two maximum frames at the tail to find the
last valid self-contained record and a recognizable incomplete append. It
does not scan or certify all history. History queries validate encountered
frames and report corruption explicitly; bad identities, versions, lengths
and checksums are not silently accepted. The first frame must have sequence 1.
A nonzero page cursor validates the immediately preceding complete frame and
the sequence across that boundary, including one-record pages; it does not
scan or certify the entire unrequested prefix. New runs retain cumulative endpoints
but rebuild current monotonic positions and Gauge baselines. They never
subtract a saved old monotonic position from the current clock.

<a id="usage-lifecycle"></a>

### 4.8 Usage lifecycle and failure handling

If sampler initialization fails, usage reports the existing unavailable/error
state and releases its file ownership without saving or truncating a checkpoint.
Existing bytes remain readable offline; a newly created file may remain empty.
There is no uninitialized live/saved view or new closed record for that attempt.

If initialization succeeds but later Host setup or CH spawning fails, bounded
shutdown still attempts to read and save actual sandbox-ctl CPU/RSS. This is
Host process usage, not evidence that a Guest ran. Earlier cumulative totals and complete
record bytes remain intact; the new epoch uses the conservative completeness
rules in §4.5.1. Failing to spawn CH does not exempt consumed Host CPU.

Normal shutdown attempts final observations and saving with a bounded
budget. Closing the manager fences new observations before freezing the final
record, while its existing writer may finish F and seal A within that budget.
A previously saved closed record is not completion of this close attempt. Retrying
an uncertain F can exhaust the budget without saving A or a closed checkpoint.
Usage failure does not change the business result. The writer owns
pinned file/directory descriptors: a late write cannot recreate a deleted
directory or write a replacement instance at the same path. Deletion does
not wait for saving/export success and adds no retention policy. Usage never
adds business `fsync`, flush, discard, drop_caches or lazy-loading operations.

An abnormal exit can lose the unsaved tail. Five minutes is the normal save
cadence, not an unconditional loss bound under continuous faults or blocked
storage. There is one writer slot and no queue of five-minute batches.
Slow Guest filesystems likewise keep one slot per source: completed memory
and healthy filesystems remain reportable while the blocked item reports
timeout/busy. Successful saves also provide no host-crash or power-loss
durability guarantee, as specified in §4.7. SIGKILL tests cover process
termination, not physical power loss.

## 5. Cold start and `run --from`

### 5.1 Explicit cold start

The ordinary `run --config` preparation and launch sequence is:

```text
T0 parse/merge/validate config and limits
T1 open/check immutable refs and image defaults; derive kernel/runtime identities
T2 project portable C0 and validate exactly one self
T3 preflight active diff requirements and source binding
T4 create run directory and atomically write sandbox.runtime.cfg once
T5 acquire controller/cgroup resources; materialize active diffs and network binding
T6 construct vhost block devices from payload-only streams
T7 spawn/configure CH
T8 launch sandbox-init spec and app
T9 establish MUX/pinger/forward/resource lifecycle
```

Predictable config/ref/format failures are intended to fail in preflight before controller/network/VM side effects. This does not promise that every later operation is side-effect-free: creating the run directory, writing C0, creating diffs, and acquiring resources are explicit subsequent steps that can fail and require cleanup. Kernel/runtime verification differs for an existing portable C0 as explained in [PortableSandboxConfig](sandbox.md#portable-config).

### 5.2 CH command-line boundary

Cold start constructs CH argv from host-bound kernel/runtime paths, memfd, vhost sockets, active diffs, and an optional network provider. Portable configuration never stores these absolute paths/fds.

Block devices bind only logical Payload:

- Container image or direct EROFS E: EROFS prefix only.
- Parent E as an ext4 layer: its root payload only.
- Current run-from E: `self` resolves only to the E payload section.
- The ZIP tail is never exposed to vhost.

CH stdin is fixed to `/dev/null`; sandbox-ctl bridges console output. The network provider is TAP or TapFD; portable topology records only NIC presence and interface name.

### 5.3 `run --from` rules

`ApplyFromRules` applies ownership using YAML field presence:

| Owner | Fields |
|---|---|
| Artifact strong | Immutable root/data graph, disk count/order/name, topology, self position, cmdline |
| Host strong | Actual kernel/runtime paths, active diff/template, cgroup/controller, network provider, timeouts, usage, Manifest/crypto/ref-location, CH binary |
| Persistent override allowed | Resource workload defaults, launch, mounts, files, init, metadata |
| Instance-only | IP/MAC/hostname, ephemeral files/env, stdio/forward |

Conflicts with protected fields return explicit field context. Hosts cannot silently change disk graphs through omission or YAML merging. Persistent `mounts` may change ordinary tmpfs/empty declarations, but data-disk mount sources, targets, names, and order must retain artifact topology. A mount override cannot move a disk.

Networking must satisfy:

```text
portable network.enabled == host provider presence
portable interface       == explicitly supplied host interface
```

A direct EROFS E contains only a read-only image. The destination host must supply a preformatted `diff_template` or an explicitly bound, already formatted diff. A missing mountable upper fails before controller/network/CH side effects.

The final call is ordinary `sandbox.Run`; `run --from` does not enter `restore.Run`.

Default `run --from` continues to enforce these strong ownership rules. `--replace-boot` selects a separate, explicit, atomic cold derivation:

```text
source E's non-boot portable defaults
  + presence-aware persistent overrides
  + the host configuration's complete boot value
  + host/instance-only bindings
  -> ordinary ValidateCold / artifact preflight / ProjectPortableCold
```

Complete replacement covers `boot.kernel`, `boot.runtime`, `boot.cmdline`, `boot.root`, and the entire `boot.disks[]` together. There is no root-only or leaf-level graph merge. An omitted host `boot.disks` means zero data disks, not inheritance of the source disks. If inherited mounts still reference removed/renamed disks, the caller must also replace mounts. Ordinary cold disk/mount 1:1 validation rejects an inconsistent result before controller, cgroup, run-directory, network, or VM side effects.

Replacement does not reuse the source E's default kernel/runtime bindings, `self`, `RunSourceBinding`, or Bundle reader/fetcher. Host boot refs still undergo ordinary cold canonicalization, local crypto, Manifest/Bundle lookup, and preflight. The new C0 and subsequent E/S disk closure depend only on replacement boot. `--replace-boot` requires both `--from` and explicit `--config` (or `SANDBOX_CONFIG`) and can never be combined with `--restore`.

## 6. Export and snapshot data flow

### 6.1 Output graph and commit point

Live export graph:

```text
data/lower disk artifacts first
              |
              v
          Sandbox E last  <-- operation root / alias commit
```

Memory snapshot graph:

```text
disk dependencies -> Sandbox E -> Snapshot S
                                  ^ operation root / alias commit
```

The Snapshot Bundle's root Manifest is S; the Export Bundle's root is E. E, data/lower Manifests, and S's memory dependencies belong to one planned Bundle graph. Before emitting the metadata prefix, the writer must complete admission, ordered source selection, parent copying, and the ref-replacement plan.

<a id="disk-memory-provenance"></a>

#### 6.1.1 Disk and memory provenance

```text
Disk provenance:   C0 + RunSourceBinding -> E/C1 disk graph
Memory provenance: MemorySourceBinding   -> S/from_refs
```

These use separate schemas. Re-snapshot can merge disk layers and memory layers independently; an old `SnapshotConfig` cannot be used to modify both graphs together.

Snapshot/export dependency planning materializes only node-local dependencies that lack portable provenance. Remote `manifest://` refs remain unchanged, as do already located file refs. If a logical Manifest comes from a located Bundle, its ref becomes that Bundle's canonical located `@manifest` selector. Consequently a Manifest-backed immutable root image is not duplicated as a new `.overlay` every time a snapshot is saved or published, and located parent chains are not copied into the new publication directory. Unlocated local tarstream/Bundle dependencies are validated before freeze and materialized when the destination does not already own them so the portable root does not depend on the calling node's private paths. Immutable root carriers materialize as `.image`; only root/data writable layers that must be retained as separate dependencies materialize as `.overlay`. The current writable root top is carried by Sandbox E's payload and does not produce another `.overlay`.

<a id="checkpoint-history"></a>

#### 6.1.2 Managed checkpoint history and selective cleanup

`merge_ref=false` records the current resident working set separately. Starting with
`S1 -> S0`, the next capture streams a new immutable historical Snapshot
`S1′ = S1(memory) over S0(memory)` and writes `S2(current working set) -> S1′`.
Further local captures repeat this composition: at most one owned local memory
lower remains beneath the newest S. `merge_ref=true` absorbs the current resident
memory and the entire eligible local prefix; switching false → true → false has
the same bound. Restore still accepts old multi-layer inputs. Only the newest S
supplies execution state and `sandbox_ref`; historical S contributes memory only.

The prefix belongs to the current sandbox's canonical `BaseDir/checkpoint`.
Selection uses complete source bindings and physical Bundle membership before
comparing paths. A named location or external template is a boundary even if its
files are on this host, including a mapping to the same pathname. The boundary
and remaining lower refs preserve their order. Local tarstream, Bundle, and mixed
carriers follow the same rules. Writable disk chains always absorb their eligible
same-device local prefix, independently of the memory flag; immutable EROFS bases
remain separate from ext4 uppers. Data and opaque Zero override lower layers;
Hole falls through. Historical reads use host artifact streams, never the guest
memfd, and therefore do not enlarge the captured working set.

History is composed before guest freeze and streamed into the final sink. It
uses `fetch.NewLayered` and sparse runs without a full-image buffer or intermediate
image copy. Local output reuses dependencies already owned by that checkpoint through their
physical selectors. Bundle output keeps its existing member-copy publication path. New history has a new content
identity; the old source remains valid through sink commit/close and database
commit. Failed capture or database commit cannot authorize old-file deletion.

After a managed Pause commits its exact S/E pair (or E-only root), the lifecycle
owner fences the old runner and all readers/writers, then invokes selective
cleanup. Generic FileSink output, arbitrary `--output`, independent
`snapshot --resume`, and shared build inputs confer no cleanup rights. The
sandboxer artifact library interprets the keep set; conductor supplies ownership,
paths, the committed pair, and the existing lifecycle fences through the
short-lived `node-ctl checkpoint-cleanup` tool. Portable Export continues to use
its stored pair without reading artifacts. No token, public result, base format,
or persistent cleanup schema changes.

The keep set contains current S/E, memory-history carriers, and current disk,
upper and immutable-base carriers, including reused files. A historical S's old
E is not a disk dependency. Any needed Bundle member retains its whole physical
carrier. Source/location binding precedes basename comparison. Selection reads
only bounded current metadata and Bundle indexes, not old candidate payloads or
full-image digests; Snapshot CPU/state bodies are not needed by this operation.
Any keep-plan or reader-close error prevents all deletions. Kernel/runtime basename
identities bind host-supplied boot files and are not checkpoint payload edges.

Bundle candidate locations are resolved only when the existing selector visits
that source: current Bundle first, then ordered refs, then remote. A current hit
does not require unused locations. Unavailable candidates may fall through;
malformed candidates and failures in a selected source remain errors. Explicit
current S/E bindings and a complete keep plan are still required before deletion.

The checkpoint directory must resolve to itself without symbolic links in any
path component, including its ancestors. Configure the sandbox base directory
using its physical canonical path; rejecting links inside the directory is a
separate protection and does not make a symlinked base path supported.

Only direct entries in the verified exclusive checkpoint directory are eligible:
64 lowercase hexadecimal digest/key names with `.snapshot`, `.sandbox`,
`.overlay`, `.image`, or `.bundle` must be regular files; capture partials must
exactly match `<producer-SandboxID>.<kind>.<uint32-decimal>.partial` (including
`bundle`, no leading zeros except `0`); fixed `<sid>.snapshot`/`<sid>.sandbox`
aliases and `.<sid>.<role>.<32-lowercase-hex>.tmp` must be symlinks with the
producer's basename target convention. SandboxID is not PathID or StableID.
Unknown names, other SIDs, prefix collisions, malformed lookalikes, directories,
and unexpected symlinks remain untouched. Incomplete partial contents need no
validation. A fixed alias is current only when it names the corresponding committed
S or E carrier. An E-only root retires its old S alias even when E retains the
same Bundle. Snapshot capture commits the S alias only; its E identity comes from
the stored pair, without requiring an E alias. Cleanup unlinks recognized aliases themselves and never follows
symlinks or recursively removes the directory; directory-fd operations keep
unlink confined if paths race.

A committed Pause remains successful if cleanup fails. The last durable RunDir
ownership marker remains until checkpoint cleanup and ordinary paused cleanup
succeed. The existing worker retries with a freshly loaded current source under
the SID lifecycle lock, including after restart. Active or detached exports keep
their read fence; cleanup never waits for an export while holding the lock it
needs. Resume/Wake/Exec obey the existing pending-cleanup admission contract.
Missing candidates count as finished; permission, I/O, and identity errors retain
retry ownership. Ordinary Kill and terminal BuildBaseDir deletion retain their
existing finalizers. Shared/external artifacts are never reclaimed by this step.

### 6.2 Freeze sequence and failure recovery

Predictable preflight work completes before guest freeze:

```text
T0 output/Manifest/local-crypto/ref/Bundle/merge/config/source preflight
   Close usage admission and break Gauge continuity before the resource barrier.
T1 enter MemoryController/Budget mutation barrier
T2 lock and lift/drain memory.high
T3 pause pinger and gate new host exec/forward; guest drains exec/forward and
   mem_report, freezes app, syncs, closes MUX; after quiesced+EOF the host
   joins residual exec/forward handlers
T4 pause CH, quiesce all block backends
T5 capture every data disk exactly once
T6 build C1 from immutable C0 + source binding + captured disk refs
T7 capture root once and emit Sandbox E
```

Export continues:

```text
T8 commit E last
T9 destroy, or --resume backend -> CH -> MUX/pinger/forward -> app -> barrier
```

Snapshot continues within the same T4 freeze:

```text
T8  CH /vm.snapshot -> config.json/state.json
T9  scan/capture memory self
T10 build S(snapshot.cfg.sandbox_ref=E, from_refs=memory parents)
T11 commit S last
T12 destroy, or --resume full recovery
```

Export never drops caches, invokes `/vm.snapshot`, or reads CH snapshot files or memfd. Snapshot reads disks once, rather than capturing them once for E and a second time through an old disk path.

BlockCOW SnapshotView is stable only while the backend is quiesced. V1 keeps the VM paused throughout sink reads; the VM cannot resume while the sink continues reading that view.

Failure semantics:

- Data artifacts/E can remain as content-addressed orphans. Reclamation belongs to the owning storage/retention policy; sandboxer does not promise automatic graph-aware collection of local output or a shared location.
- The root alias is committed only after E or S succeeds.
- `--resume` recovery after success or failure follows backend → CH → MUX → pinger/forward → app → resource barrier.
- With an ambiguous attach ACK, the host retries the idempotent operation once. If it still cannot establish MUX, capture becomes a terminal failure. While retaining memory/`memory.high` guards, the host requests VMM shutdown and uses bounded SIGTERM/SIGKILL fallback. It does not leave a VM that accepts new requests while the guest remains frozen.
- Default destruction also waits for CH exit. Failed or timed-out `/vmm.shutdown` falls back to bounded SIGTERM/SIGKILL before lifecycle guards are released.
- C0, active diffs, and the live lower graph remain unchanged.
- Local capture uses same-directory temporary files and cleans failed partial output. Named-location publication has its separate ownership-checked cleanup rules in [Local tarstream and crypto](sandbox.md#artifact-publication).

### 6.3 `ctl.sock` protocol

The socket is always `RunRoot/PathID/ctl.sock`; omitted PathID defaults to SandboxID. PathID is a host-side locator and does not enter the wire request.

Snapshot and export use distinct request types:

```text
snapshot_request -> snapshot_done | error
export_request   -> export_done   | error
exec_request     -> exec_ack      | error
usage_request    -> usage_response | error
resource_stats_request -> resource_stats_response | error
```

The sole reaper ends live resource observations as soon as `waitid(WEXITED|WNOWAIT)` confirms VMM exit, before the existing final usage sample and `cmd.Wait`. The same exit notification applies when usage is disabled, without adding samples or changing stop/save behavior. During exit, only effective specification fields remain; an exited VMM receives no fresh host observation timestamp.

Export is not `snapshot_request{memory:false}`. Requests execute in the run process, reusing its lifecycle barrier, guest/MUX gate, CH API socket, and live vhost SnapshotView.

Usage reads the owner's existing `usage.Snapshot`/`usage.Record` or confirmed
file history. It has no sampling/save side effect and does not acquire the
capture or CH mutation barrier. Only its response has a separate 1 MiB JSON
bound; ordinary ctl framing and limits remain unchanged.

The reader rejects missing or invalid required specifications: CPU capacity and allocatable must be positive, allocatable cannot exceed capacity, and memory capacity/headroom must be positive with headroom no greater than capacity. These checks do not reject a valid zero host counter.

`resource_stats_request` is a separate narrow read. The shared cold/from/restore
runtime returns its effective capacity CPU, allocatable CPU, capacity memory
and allocatable memory headroom. These are the values used to configure that
run, including host overrides. Allocatable CPU selects relative `cpu.weight`,
not a fractional-core quota or unconditional performance guarantee. Headroom
is `resources.allocatable.memory`, not the current Budget, Guest free memory
or the node reservation. The node controller remains the authority for the
reservation; it is not guessed in this response.

For a live VMM with a configured cgroup, the same pinned cgroup descriptor is
used for `memory.current` and `cpu.stat.usage_usec`. The response's
`memory_used` and `cpu_usage_usec` retain unsigned integers as decimal strings;
the conductor HTTP adapter expresses CPU in seconds. The read excludes the
host ctl process and does not add Guest CPU, subtract inactive file/balloon
memory, or clip VMM memory to Guest capacity. CPU is the current source's
cumulative value and may reset when that source is recreated. Lifecycle
cumulative accounting remains native usage.

The response includes `sandbox_id` for exact identity verification by
[`ctl.ReadResourceStats`](../pkg/ctl/resource_stats.go). Each host counter is
independently optional: missing is omitted, valid zero is present. The ctl reader
requires a timestamp exactly when at least one host counter is present.
An actual host read supplies `timestamp_unix`; specification-only reads, no cgroup and
no live VMM do not invent a host observation or timestamp. Malformed or
unreadable configured counters fail explicitly. A present `cpu.stat` without
`usage_usec` is malformed, including an empty file; it is not a missing counter.
No sampler, history, Guest
request, CH resize or controller mutation is involved. Static and dynamic
control modes use this same read path, with usage and telemetry independently
disabled. The existing ctl connection context/deadline bounds client waiting.

The real memory-budget E2E compares repeated ctl reads with the frozen VMM's
actual cgroup files, checks that the ctl process is outside that cgroup, and
verifies usage remains off and the reads do not modify resource controls.
Freezing belongs only to the test; the production reader does not freeze VMs.

Responses do not echo secret values. Remote Manifest uploads may take a long time. For local snapshot/live export, CLI `--timeout=0` leaves the ctl connection without a deadline; a positive value bounds that client's wait. Neither value sets a server-side deadline field in the wire request. Server lifecycle contexts still govern cancellable I/O, and callers must not interpret a timed-out CLI as proof that no artifact was committed. Image-to-Sandbox-E assembly instead applies a positive timeout to its own operation context (§2.4).

### 6.4 Image-to-Sandbox-E assembly

Input must be a flattened `EROFS + ZIP(config.json)`, not an already wrapped `.sandbox`:

```text
open carrier -> validate flattened image -> retain config.json bytes
load explicit config -> apply image defaults -> build direct-EROFS C1
rebuild EROFS + ZIP(config.json,sandbox.runtime.cfg)
emit local/Manifest/Bundle E -> commit E root
```

This creates no VM and needs no freeze, but still validates portable identities, limits, carrier admission, and output atomicity. The CLI is a thin wrapper around package APIs:

```text
OpenFlattenedImage
  -> PrepareSandboxEConfig
  -> AssembleSandboxE (logical sparse.Source)
  -> caller-selected direct publisher
```

`AssembleSandboxE` does not create a complete intermediate `.sandbox`. It borrows the caller-owned `FlattenedImage`, preserves the payload's Hole/Zero/Data map and byte-for-byte `config.json`, and appends canonical `sandbox.runtime.cfg`. Context cancellation can interrupt opening, configuration-ref canonicalization, image validation, and later sink consumption. Malformed image, JSON, or portable configuration fails closed before a root ref is published. Embedded multitask processes must not switch tenants by changing global `MANIFEST_KEY`; they should pass a task-scoped resolver through `NewProcessStorageWithCustomerKey`. `ProcessStorage` evaluates it at most once and fixes the same customer key for its fetch, ingest, and local-codec operations.

<a id="artifact-publication"></a>

### 6.5 Local tarstream and crypto

Local immutable artifacts support `crypto.local=off|auto|required`:

- `off`: plaintext tarstream, with `digest` identity.
- `auto`: recognize plaintext or KDXTS-encrypted tarstreams; configured codec governs new output.
- `required`: reject plaintext and identities not bound to the key; use `hmac` identity.

The omitted policy defaults to `off`. With `auto` or `required`, storage construction resolves the customer key even for a file-only operation. No Manifest config means no local codec or lazy Manifest client; file-only operation does not by itself imply that configured crypto can omit its key. See [storage.go](../pkg/artifact/storage.go).

Reusing an existing file requires revalidation of its role, logical size, content identity, and complete stream. Local output and named ref-location commit strategies are deliberately separate. Artifact capture/publication defines logical completion, not stable-storage durability. Local output relies on complete writes, checked `Close()`, content-addressed no-replace rename, and atomic alias rename. Named locations rely on exclusive creation, checked `Close()`, final-path reopening/full verification, and path-identity checks. Neither artifact-publication path explicitly flushes files/directories; physical writeback depends on the filesystem/storage implementation. This differs from the fsynced run-directory C0 write in §3.6.

<a id="local-output"></a>

#### 6.5.1 Local output

For `snapshot/export --output`, `FileSink` writes a unique same-directory temporary file, completes all writes and checks `Close` errors, then uses `renameat2(RENAME_NOREPLACE)` for an O(1) final commit. `BundleSink` likewise commits a complete Bundle with an atomic no-replace rename. Neither path rereads and copies the entire artifact merely to commit it. After root success, a semantic alias is updated with a random temporary symlink and atomic rename. Alias targets and existing entries use `NOFOLLOW`/`lstat` checks that fail closed, refusing to replace regular files or directories with symlinks. Complete writes, checked `Close()`, and atomic rename define commit; there is no explicit artifact file/directory fsync. Local output therefore requires a node-local filesystem with these atomic rename and symlink semantics.



<a id="named-ref-location"></a>

#### 6.5.2 Named ref-location

`publish/upload-snapshot --to-ref-location` does not reuse `FileSink` or `BundleSink`. A tarstream carrier's marker records payload boundaries and a payload commitment. Reading the complete carrier verifies that declaration against payload bytes; opening the carrier already exposes its declared identity. When only E/S's dense metadata tail changes, the carrier combines the old payload commitment with the new tail to derive a new identity in O(tail) work. It does not read GiB-scale payloads just to derive this value or first encode to `io.Discard`. After obtaining the carrier's scheme/digest, the location target creates `<digest>.image|overlay|sandbox|snapshot` directly with `O_CREATE|O_EXCL`. Canonical encoding is the only complete write into the shared target. `.image` carries an immutable root image; `.overlay` carries only a separate writable-disk dependency. The Sandbox E payload is not published again as `.overlay`. Plaintext output uses `@digest`; codec-backed output uses `@hmac`. The target directory holds no complete staging copy for this tarstream path and creates no `<sid>.sandbox`, `<sid>.snapshot`, or other semantic alias.

Fresh finals have mode `0644`. During the sole write, `tarstream.WriteTo` checks source reads and destination writes, reproducing the carrier-provided scheme/digest. While the owned write fd is still open, `lstat` and `SameFile` confirm the canonical path still identifies the inode created by this invocation's `O_EXCL`. A metadata-only guard fd pins that inode across checked `Close`. The publisher then reopens with `O_RDONLY|O_NOFOLLOW` (also nonblocking so unexpected FIFOs cannot stall validation) and fully verifies regular-file type, role/payload name, logical size, canonical tarstream, marker, codec, crypto policy, digest scheme/digest, and the complete sequential stream. Path identity is checked again after validation. Existing finals undergo the same complete content validation; successful reuse does not change inode or bytes.

Manifest Bundles do not enter the tarstream E/S rebuilding path. Their root Manifest key supplies `@manifest` identity. The location target first forces verification of the selected Manifest closure, recorded admission, physical keys, and crypto domain. It then performs ordered exact-byte copying of same-directory dependency Bundles and publishes the root `<key>.bundle` last. Each shared final is written once, reopened, validated as a canonical Bundle/root, and compared byte-for-byte with its source; the selected closure is strictly reverified through the target fd. This creates no `.snapshot`/`.sandbox` substitute and does not rewrite the Bundle's `snapshot.cfg`.

The final path is briefly visible before writing finishes. Normal consumers must use only a root ref returned successfully by the publisher, which continues to publish dependencies first and root last. A concurrent publisher encountering a partial final reopens and validates it with bounded, context-aware exponential backoff. If the writer completes within that window, the final is reused. A still-incomplete or invalid final after bounded retries fails closed with an explicit cleanup/repair requirement; the publisher does not delete another owner's path. Symlinks, directories, FIFOs, and other nonregular finals are also rejected and preserved. Cleanup removes only a failed write for which this publisher successfully acquired `O_EXCL` and whose path still identifies the recorded inode. Abandoned finals of unknown ownership need explicit cleanup or an owning retention/GC policy.



<a id="single-root-imagesandbox-manifest-bundle"></a>

#### 6.5.3 Single-root image/Sandbox Manifest Bundle

`NewSingleRootBundlePublisher` is a typed named-location sink for an already assembled `RoleImage` or `RoleSandbox` `sparse.Source`:

```text
logical source
  -> Manifest ingest with fixed write admission/customer key
  -> deterministic identity pass to a streaming hash sink
  -> replay the fixed source with the same key, admission and encoding options
  -> exclusive-create content-addressed <root>.bundle
  -> reopen/strict validate
  -> file://<root>.bundle@manifest:<root>#<location>
```

The source supports repeated reads. The first pass retains the root Manifest key, a physical Bundle digest and format metadata while discarding encoded Chunk bytes. The second pass writes the final `<root>.bundle` directly and checks the generated root against the planned identity. The customer key is resolved once for the publication; both passes use the same admission and encoding settings.

The only created file is the final Bundle, containing this logical root's Manifest and Chunks. Publication uses bounded Chunk buffers and preserves payload sparsity. The typed call site supplies the logical role; image and Sandbox readers retain their schema checks. Reuse requires matching admission, root, authenticated content and complete physical identity. Concurrent writers converge through bounded waiting, exclusive creation and full verification. Cancellation, changed source, write errors and close errors fail publication and remove only the publisher-owned incomplete final. A replaced pathname retains the replacement inode and returns an ownership error.

Tarstream and Bundle are physical carriers, not logical roles. Tarstream contains a single role-specific payload and sparse envelope, with `@digest`/`@hmac` identity. Bundle contains Manifest/chunk records, write admission, and optional local encryption, with `@manifest` root identity. Single-root Bundle publication must not first build a tarstream; conversely, a Bundle root cannot be inferred just from an `.image`/`.sandbox` extension.

Named locations therefore require directory creation, exclusive file creation, writes/reads, stat/fstat/lstat, permission setting, seek/pread, close, and removal of incomplete files owned by this process. They do not rely on rename/renameat2, symlinks, hardlinks, reflinks, preservation of filesystem holes, advisory locks, or lock files, and perform no explicit file/directory sync. Successful publication is defined by exclusive writes, checked `Close()`, final-path reopening/full verification, and path-identity checks; the filesystem or underlying storage defines physical writeback.

Active encrypted `.overlay.diff` files remain in KDXTS format. Export reads only the decrypted BlockCOW SnapshotView and creates a new immutable logical artifact. It never appends ZIP data to an active diff.

<a id="manifest-upload"></a>

### 6.6 Manifest upload

An already assembled image or top-level Sandbox E can be ingested directly with `NewManifestPublisher(...).PublishSource(ctx, RoleImage|RoleSandbox, source)`. The caller retains source ownership. This path creates no local tarstream: chunks and deduplicated objects are written first, the root Manifest last, returning `manifest://<root>`.

Local tarstream graphs are ingested bottom-up:

```text
data/lower -> E -> S
```

Already portable Manifest/located dependencies retain their refs rather than being materialized and rewritten. Bundle roots use the exact-upload fast path: force verification of the selected Manifest closure, recorded admission, and physical objects; upload Chunk/Manifest objects unchanged; commit the root Manifest last. The root key and `snapshot.cfg` remain unchanged. Existing located selectors inside a Bundle therefore still require consumers to configure the corresponding ref-location; they are not silently rewritten into Manifest refs. Customer key, chunk/Manifest crypto, content verification, and store-generation admission follow Manifest configuration. E is the export root; S is the snapshot root.

<a id="manifest-bundle"></a>

### 6.7 Manifest Bundle

Before pause, a Bundle completes:

- Write admission.
- Retention of portable dependencies and rewriting of located Bundle selectors.
- The plan to materialize unlocated local dependencies.
- The dependency set for the current operation.
- Exact Manifest copying from an unlocated parent Bundle or remote fallback; located parents retain canonical selectors.
- Ingestion of local tarstream dependencies.
- All ref replacements.

The writer then emits the metadata prefix once, writes Manifest/Chunk data, and finalizes with E's or S's root key. `FullVerify` uses the complete expected Manifest set. V1 does not infer E/S from outer ZIP magic.

<a id="publish-graph"></a>

### 6.8 Publish graph

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

The current S defines the ordered memory list and selects the current E disk graph. A memory parent contributes its memory payload. Ordinary publication can retain portable dependencies and use the exact Bundle-copy/upload path. A requested reference rewrite constructs new logical S/E artifacts through the selected target; Bundle-scoped dependencies acquire valid output bindings.

<a id="artifact-commit"></a>

### 6.9 Atomicity and determinism

Portable YAML and E/S ZIP use canonical order, fixed metadata, and bounded bytes. Local `FileSink`/`BundleSink` retain same-directory temporaries, complete writes/checked `Close()`, and atomic no-replace rename; final commit is O(1). The alias updates only after root commit. Artifact capture/publication defines logical completion, not stable-storage durability. Neither local artifact path explicitly fsyncs files/directories; the filesystem/storage implementation governs physical writeback. Named ref-locations use a separate exclusive-create, checked-write/copy-once, reopen-and-full-verify protocol. Tarstream carriers directly supply identity; Bundle copying preserves exact bytes. Neither uses the local sink's capture/commit path.

Multidisk ordering is fixed: data disks first, root E last. Snapshot then writes memory S last. This makes the S/E root an auditable graph commit point.

## 7. Memory restore data flow

`run --restore S` handles only genuine memory Snapshots:

```text
T0 parse host config with presence
T1 open carrier and strict-parse S
T2 parse canonical snapshot.cfg
T3 open S.sandbox_ref and strict-parse E
T4 apply restore host-only rules to E config -> immutable lifecycle C0
T5 preflight kernel binding, compare runtime identity, and check disk sources
T6 atomically write run-dir C0
T7 bind self and reconstruct root/data disks from E
T8 create memfd and layer S.memory over S.from_refs
T9 create UFFD/va_report endpoints and CH restore argv
T10 start CH from config.json/state.json
T11 transfer UFFD descriptor(s) and complete restore handshake
T12 poll GET /vm.info until the restored VM is Paused
T13 /vm.resume
T14 establish restore MUX and resume original process
T15 settle balloon/resource observation
T16 start pinger, forwarder, sensor, and steady lifecycle
```

Cloud Hypervisor v51.1 binds its API socket before its CLI submits `VmRestore`,
so socket connectivity is not restore readiness. The `vm.info` Paused check is
the correctness barrier before the existing external `/vm.resume`; only the
version's structured VM-not-created response and a not-yet-listening socket are
pending conditions. `timeouts.api_ready` bounds that entire barrier, including
a connected but stalled response. `timeouts.ch_api` independently bounds the
subsequent resume response, and `timeouts.restore` bounds the guest restore ACK
and MUX establishment. This completes the restore-readiness correctness part of
#72; eliminating the resume round trip remains a future optimization and would
require a compatible Cloud Hypervisor version.

Restore does not:

- Read a disk graph from `snapshot.cfg`.
- Cold-start on zero memory.
- Rerun init/files/launch/plugin.
- Treat E's `config.json` as a new process-launch request.
- Update memory parents or C0 as a re-snapshot side effect.

Allowed host-only fields include the network provider/current identity, cgroup/controller, allocatable CPU/memory and resource enforcement, actual kernel/runtime paths, active diff/template, restore prefetch, and timeouts. E owns the immutable disk graph, capacity identity, `deflate_on_oom`, and network topology. The kernel re-hash exception and runtime-footer comparison are described in [PortableSandboxConfig](sandbox.md#portable-config).

Runtime selection is a restore-only preflight. First inspect the supplied
`boot.runtime` file using the existing bundle-envelope and **basename + declared
footer digest** comparison against E. Success returns immediately without a
second lookup. If that file is missing, unreadable, malformed or mismatched, try
exactly one sibling: E's validated runtime basename in the supplied path's parent
directory. Do not follow the default file's symlink target to select a different
directory, scan directories, or retry an identical candidate. Both candidates use
the same check; if neither passes, report both causes. Invalid configuration,
cancellation, and later disk/network/VM errors do not trigger runtime fallback.
The existing local-E-relative default applies when no runtime binding is supplied.
No new directory field is required; image cold start and `--from` are unchanged.

Whichever candidate passes, use its absolute local path both for this invocation's
runtime binding and `pmem[0].file` in the generated `snap-state/config.json`. The
captured pmem must be the existing single runtime device with a nonempty file
field. Only that file field changes; device ID, optional size and other attributes,
`state.json`, original S/E and node defaults remain unchanged. The old absolute
path need not exist. Versioned runtime files must remain immutable and available
under distinct names in the same directory; no full EROFS rehash or runtime copy
is added to restore. The existing `e2e_sandbox_restore.sh` verifies relocated-v1
fallback from a v2-named default, unavailable captured path, selected pmem path,
unchanged source artifacts/host inputs, guest readiness and continued execution.

After restoring S0:

```text
C0 = E0 portable config
memory parent = S0

next snapshot:
  E1 = Export(C0,current disks)
  S1 = memory self + from_refs=[S0,...] + sandbox_ref=E1
```

After an explicit cold start or cold start from E, the memory-parent list is empty. `snapshot --resume` does not silently make S1 the current parent.

### 7.1 Memory prefetch

`restore.prefetch: memory` is a host-only optimization. It warms the current S self's file page cache or Manifest chunks, excluding parent memory layers and disk streams. The call receives the opened root stream, so its scope may also include S's bounded ZIP metadata tail; it is not a memory-payload-only section. It changes neither sparse truth, fault ordering, C0, S, nor memory parents. The default is `off`. An invalid configured mode fails validation before side effects; lack of prefetch capability or a prefetch I/O failure is best-effort and does not fail restore. It is logged and restore continues on demand. The asynchronous task is canceled and joined before its streams close. See [prefetch.go](../pkg/restore/prefetch.go) and its call in [restore.go](../pkg/restore/restore.go).

<a id="usage-generations"></a>

### 7.2 Usage across execution generations

Cold and restored instances use the current [host usage policy](#usage-policy) and the same Host owner. The [persistence contract](#usage-persistence) accumulates by logical SandboxID: restoring an old S does not roll usage back, and a clone with a new SandboxID does not inherit the original file. New runs retain cumulative endpoints while rebuilding clocks and Gauge baselines; they do not subtract monotonic positions across generations or subtract restored initial memory/filesystem occupancy.

Sampling starts with the Host processes; Guest rounds start only after launch/restore readiness, not merely ctl.sock existence. Before capture, Host closes usage admission and breaks Gauge continuity. Restore/attach and failed-capture recovery use the [Guest raw-usage admission lifecycle](sandbox-init.md#usage-observations), including generation fencing, blocked source slots, ACK/thaw rollback and epoch ownership. Restored balloon seeds are not fresh actual observations.

## 8. UFFD handler

Memory restore uses one shared memfd and a host handler that adopts CH-side userfaultfd descriptors. CH can report multiple regions of the same memory zone, for example when x86 RAM is split around the PCI hole; those descriptors share the handler's epoll set and address map. S self and `from_refs` are opened as a layered sparse source: resident top-layer data overrides lower layers, a top Hole falls through to its parent, and Zero remains explicit zero.

### 8.1 Single UFFD contract

The single-UFFD design means faults are handled on CH's mapping rather than registering a second UFFD on sandbox-ctl's backend mapping. It does not require exactly one CH descriptor for every layout. CH registers missing-page events for its restored memory regions; the host receives the descriptors through the restore handshake and validates region bounds and nonoverlap against the shared memfd address map. The first report starts the handler; later valid regions use `AddUffd`. Out-of-range faults, duplicate/invalid region registration, and truncated sources fail closed. See [serveandwait.go](../pkg/sandbox/serveandwait.go), [handler.go](../pkg/uffd/handler.go), and [addrmap.go](../pkg/uffd/addrmap.go).

### 8.2 Read path

UFFD uses two-stage population: one fault page followed by a best-effort serial tail. The fault worker first completes the fault page. It may submit the tail only after that page is actually committed as `Loaded`. Each handler retains at most one tail reservation; concurrent faults handle only their fault page when the reservation is busy. The fault page does not wait for a tail worker or tail ioctl. Concurrent source I/O, shared buffers, and guest population remain bounded. To reuse one physical decode result, `ChunkRun` completes the buffered source read described below before the urgent copy.

Population bounds depend on the final visible Run type:

| Run type | Fault stage | Tail stage | Total maximum per fault |
|---|---|---|---:|
| `Hole`, `Zero`, `Released` | One page via `UFFDIO_ZEROPAGE` | Up to 15 pages | 16 pages / 64 KiB |
| Ordinary `Data` | Read and `UFFDIO_COPY` one page | Deferred read/population of up to 15 pages | 16 pages / 64 KiB |
| Manifest `ChunkRun` | Read the final visible chunk window containing the fault once; populate the fault page separately first | Populate the following suffix, then the preceding prefix only after success | One current visible window, at most 1 MiB / 256 pages |
| Reclaimed `Loaded` | Restore only the current page | None | One page |

`ChunkRun` means the final serving leaf is a physical Manifest chunk. A chunk is the unit of decryption/decompression and, when enabled, ordinary content verification. The handler therefore avoids repeatedly reading it through ordinary Data's fixed window. `SnapshotReader` still returns only a forward anchor starting at the fault offset. Through a package-local optional capability, `StreamSnapshotSource` calls `fetch.ResolveChunkWindow` and uses metadata from the final composed Stream to expand that anchor to the largest contiguous visible window containing the fault within the same physical chunk. Resolution reads no payload. Upper Holes are transparent; upper Data/Zero, the memory section's end, and discontinuous visibility of the same lower chunk all truncate the window. Physical chunk bounds must not bypass the overlay.

The tail reservation is acquired before window resolution or payload reads. If busy, only the current 4 KiB fault page is read/populated from the original anchor. With the reservation, the handler reads a window no larger than 1 MiB into its existing buffer synchronously; a required source read retries the complete window on temporary failure. For an entirely visible chunk within that bound, this is an exact whole-chunk read. If an overlay truncates visibility, the source still performs the necessary chunk-level decryption, decompression, and configured verification internally; the handler receives only the contiguous window that is safe to populate. The fault worker takes the current page from the middle of the buffer for urgent `UFFDIO_COPY`. The tail worker first fills the adjacent suffix with one batch; only if that batch succeeds completely does it fill the adjacent prefix with one batch. Before executing a tail, it rechecks states outward from the fault's adjacent page and truncates at the first mismatch. Any state conflict, partial completion, or ioctl error stops more distant population and later stages without retry. One `ChunkRun` therefore issues at most one urgent copy and two tail copies.

Before workers start, an ordinary Data handler allocates a 64 KiB shared buffer; a source with chunk-window capability uses 1 MiB. A cold `ZeroSource` allocates neither buffer. Fault/tail paths do not grow these buffers or allocate temporary payload buffers; each fault worker holds a fixed 4 KiB urgent buffer. This is a constraint on the handler's additional payload allocation, not on source internals: Run objects, cache leases, and partial-chunk decode allocations remain the `SnapshotReader` implementation's responsibility. On the healthy path the handler calls `SnapshotReader.RunAt` once for a nonzero source; the optional window resolver performs further metadata `Stream.RunAt` calls only within accelerator. Ordinary Data and zero-like runs remain limited by the 64 KiB state boundary. Bidirectional `ChunkRun` candidates contain complete pages only. One scan under the state read lock clips them to contiguous `PageState` on both sides, RAM bounds, and the current CH UFFD region. These guest-population boundaries do not shorten an already admitted whole-chunk source read.

Default CDC chunks have a 1 MiB maximum, but Manifest configuration can admit larger chunks within its decoded-format limit. The UFFD buffer/window cap remains 1 MiB regardless. An unavailable, invalid, or over-limit expanded window retains the original safe forward anchor; if a buffered window cannot fit, the handler makes urgent-page progress without allocating a larger buffer. No oversized-chunk-specific population path is introduced. See [fault.go](../pkg/uffd/fault.go) and accelerator's [chunker configuration](https://github.com/kuasar-sandbox/accelerator/blob/main/pkg/manifest/chunker/chunker.go).

A tail issues a single multipage `UFFDIO_COPY` or `UFFDIO_ZEROPAGE` for its contiguous valid range; `ChunkRun` suffix and prefix each issue at most one. The kernel processes pages individually and may return a completed page-aligned prefix. The handler conditionally commits only that prefix as `Loaded`, abandoning the remaining tail without retry after the first conflict/error. Batching therefore retains the successful kernel-completed prefix while reducing the ordinary 16-page policy's tail to one ioctl and a fully visible 256-page window's 255-page tail to at most two.

`EVENT_REMOVE` makes discarded ranges missing again. Conditional tail-state updates must not overwrite concurrently produced `Released` state. Context cancellation stops workers and closes the owned descriptors/streams.

UFFD directly retries required `Run.ReadAt` operations. The urgent page and any Chunk window containing it are required. A pure speculative tail makes one best-effort attempt. The existing `ChunkRun` capability, buffers, tail reservation and ioctl partial-progress/EEXIST/EAGAIN convergence remain in use. After a delayed source read, urgent installation rechecks page state; an observed REMOVE invalidates the old data and uses the existing zero-page convergence without scheduling a tail from that obsolete plan.

The shared [source read recovery policy](#read-recovery) retains the operation context and first terminal cause.

## 9. Cgroups and balloon

### 9.1 Memory enforcement

Static/dynamic cgroup modes lift a potentially competing `memory.high` before CH pause and wait for existing high events to drain. Snapshot/export recovery restores the previous value. The VMM cgroup contains CH, not sandbox-ctl itself; sandbox-ctl is not charged to that workload Budget.

### 9.2 CPU

`capacity.cpu` determines the vCPU count. `allocatable.cpu` must be finite, positive, and no greater than capacity CPU; cgroup mode maps it to a clamped `cpu.weight`. This weight is relative contention policy, not a hard fractional-core quota; the VMM's `cpu.max` is based on capacity. Controllers may adjust grants during a lifecycle but do not rewrite C0 or the artifact.

### 9.3 Balloon

Balloon mutations serialize with snapshot/export through the same barrier. The freeze window cannot overlap asynchronous balloon resize. Restore takes CH state as the actual current value, then enters a new observation epoch; host YAML does not reconstruct the restored balloon's instantaneous state.

The sandbox-local `MemoryController` combines one accepted Guest ABI `mem_report` with CH `vm.info`. Guest reports start immediately after the cold-launch barrier and repeat every five seconds by default; [Guest ABI](sandbox-init.md) owns their epoch/sequence, retry and quiesce protocol. `Capacity` comes from the host-owned CH memory configuration, never Guest `MemTotal`. CH `config.balloon.size` is `AcceptedTarget`; `memory_actual_size` is `CurrentBudget`, so `BalloonCurrent = Capacity - CurrentBudget` and `TargetBudget = Capacity - AcceptedTarget`. `DesiredTarget` is separate local intent. The safe observed Budget is `B = max(TargetBudget, CurrentBudget)`. Exact equality is exposed only as the `TargetReached` diagnostic; it is not a convergence or settlement gate. A successful `vm.resize` accepts a target and does not prove current progress.

The local calculation is:

```text
DemandMemory = max(CurrentBudget - MemAvailable, 0)
RawRequested = min(Capacity, saturating_add(DemandMemory, AllocatableMemory))
DesiredTarget = align_down(Capacity - RawRequested, 64 MiB)
RequestedBudget = Capacity - DesiredTarget
```

Diagnostic Guest fields and host `memory.current` do not determine Budget. Host `memory.current` is only a lower bound when choosing `memory.high`.

Growth reserves Budget first, raises the required `memory.high` allowance, then lowers the balloon target through `PUT /api/v1/vm.resize` and confirms acceptance with `vm.info`. Partial node grants accumulate until a representable Budget fits within the reservation; rounding must never create unreserved memory. Safety growth can proceed after the lifecycle barrier even while target/current are unstable. Reservation, high or ambiguous-resize failures retain the forward growth objective for retry; a later smaller report does not implicitly roll it back.

Shrink requires a fresh, non-superseded report and known CH target/current. Each report permits at most one 64 MiB inflation step, retaining one step as a deadband. A new target must exceed the accepted target and is bounded by the desired target, capacity, `AcceptedTarget + 64 MiB`, and `BalloonCurrent + 64 MiB`, using checked or saturating arithmetic. Immediately before resize, CH and those bounds are checked again under the mutation gate; reversed actual movement or a changed accepted target invalidates the candidate. Once the target operation is confirmed, `B < Reservation` lowers `memory.high` for B and submits B as the absolute reservation baseline; `B == Reservation` does not resubmit, while `B > Reservation` neither releases nor treats actual movement as a node grant and instead leaves growth to the existing demand/pressure path. A partial observed advance can therefore settle the round without target equality; later reports handle later progress. A newer admitted report, unavailable CH state, or failed high/reservation operation prevents premature release. An unfinished grow retains its reservation, and growth after a partial commit requests the difference from the latest baseline. If a shrink response is lost, the submitted smaller baseline remains the conservative recovery value whether or not the node committed it. The mutation gate covers CH and `memory.high` changes, not the subsequent node-only commit RPC. Guest emergency deflation does not bypass these conditions.

See [memory.go](../pkg/resctl/memory.go), [memory_controller.go](../pkg/resctl/memory_controller.go) and [balloon.go](../pkg/resctl/balloon.go). Sparse `PUNCH_HOLE`/`MADV_DONTNEED` and hole-only skipping remain in [VMM patch 0004](cloud-hypervisor.md#34-0004--skip-hole-only-runs-during-balloon-release).

## 10. Resource protocol

### 10.1 Lifecycle states

```text
prepare -> admit -> start -> ready -> steady
                  \-> freeze -> capture -> resume|destroy
restore -> resume -> settle -> steady
```

Controller leases, heartbeats, and recovery inventory manage only host resource ownership and do not enter Portable configuration.

### 10.2 Capture barrier

Before snapshot/export, new Budget mutations and balloon transitions are blocked. Already running mutations must complete before freeze. Recovery releases the barrier only when the guest/app can continue; otherwise a liveness bug would permanently prevent resource updates after resume.

### 10.3 Pressure sensor

Static/dynamic modes can use PSI or `memory.events.local` polling. Host configuration determines the PSI trigger and debounce; they are not written to E. The sensor stops initiating growth during the capture gate and establishes a new observation epoch after restore.

## 11. vhost-user-blk backend

### 11.1 Payload boundary

The vhost backend receives the `fetch.Stream` Payload section, not FullStream. Including a `.sandbox` ZIP tail in the block device's logical size is a bug covered by format/unit tests.

### 11.2 Layered reads

Read order is active diff → captured top → `base_from_refs` → root image where applicable. Hole falls through; Zero/Data stop traversal. The immutable layers composed within one writable block device must have matching logical sizes. In the EROFS-plus-ext4 topology, the read-only EROFS base is a separate vhost device; it is not the final fall-through layer of the writable ext4 BlockCOW device. See [disks.go](../pkg/sandbox/disks.go) and [serveandwait.go](../pkg/sandbox/serveandwait.go).

`StreamReader` retries a complete read synchronously. Runtime queue contexts replace preparation contexts for that call, so stopping an old master session cancels its pending reads before its workers are joined and its memory table is replaced. The COW adapter passes that context into base materialization while retaining block locks, the dirty bitmap and partial-write algorithm. It never retries an entire `WriteAt`, `FLUSH`, Store `Put` or snapshot operation.

Both `processChain` and `processQueue` recognize stopped/terminal required reads. They leave the status byte, used ring and queue base uncompleted. This also applies to the implicit base read inside a COW write. Ordinary unsupported requests and independent writable-diff errors keep their existing protocol behavior.

The shared [source read recovery policy](#read-recovery) retains the operation context and first terminal cause.

### 11.3 SnapshotView

After CH pause and frontend quiesce, `SnapshotView` copies the logical bitmap and
serves upper-only plaintext, reading dirty/writeback cache pages before the file.
It does not force Drain/fsync or copy the whole cache. Background writeback may
continue moving identical logical contents. The last cached copy becomes
evictable only after successful file completion. Keep frontend requests quiesced
and the COW open until capture finishes. Background fatal errors during capture
must fail publication and reach the runtime owner, including when no guest request
follows the failure. Export does not rotate the active diff or change its base.

<a id="cow-backend"></a>

### 11.4 BlockCOW state and I/O

Active writable diffs request O_DIRECT through one common positioned-I/O API,
including on tmpfs. Runtime policy does not identify the backing filesystem.
One bounded plaintext page cache is owned by `sandbox-ctl` for each sandbox; root and data disks share it, including mixed tmpfs/disk storage. This
implements [issue #230](https://github.com/kuasar-sandbox/sandboxer/issues/230), with
tmpfs compatibility restored by [issue #238](https://github.com/kuasar-sandbox/sandboxer/issues/238).
It changes active data access, configuration and lifecycle; immutable base images,
templates, historical overlays, manifest decryption caches, artifact outputs,
tarstream representation, cgroups, balloon and guest resource budgets retain
their existing behavior.

#### 11.4.1 Logical state and accounting

The stack is `BlockCOW -> plaintext cache -> diffFile encoding -> owned I/O
workspace -> active file`. Pages are 4096 bytes. Base reads do not populate or
materialize the upper cache. Each cached page is indexed by active file and block number.

| State or transition | Total used | Dirty used |
| --- | --- | --- |
| Clean, dirty, writeback, read loading | One page each | Dirty/writeback only |
| New write construction/reservation | One page | One page |
| Clean becomes dirty | Unchanged | Add one page |
| Dirty overwrite or dirty becomes writeback | Unchanged | Unchanged |
| Successful writeback becomes clean | Unchanged | Remove one page |
| Evict clean | Remove one page | Unchanged |

At all times `dirty_used <= used <= cache_size` and
`dirty_used <= max_dirty_size`. Submitting I/O does not release reservations.
The page pool, index, clean LRU and dirty FIFO are bounded by capacity. Plaintext
is cleared on release. Separate bounded overhead consists of at most 256
writeback page pointers per sandbox, two independent 1 MiB MAP_SHARED
I/O workspaces per active file (each with less than 1 MiB alignment padding),
and page metadata/index/Go allocator overhead proportional to page capacity.
`cache_size` is not a cap on process RSS, guest memfd mappings, immutable-source
caches, tmpfs file storage or snapshot output memory.

Tmpfs file pages are filesystem storage in memory (and potentially swap), separate
from this bounded process cache. Successful writeback releases dirty quota but
does not release stored tmpfs data; clean-cache eviction does not remove it either.
`cache_size` does not bound those bytes or promise zero tmpfs page residency.
Existing tmpfs mount size/inode limits and host memory controls govern that
storage. Sandboxer does not remount tmpfs, change cgroups, inflate cache budgets
or add persistence guarantees. See the [Linux tmpfs documentation](https://www.kernel.org/doc/html/v6.1/filesystems/tmpfs.html).

The bitmap means **logical upper-present**, independent of clean/dirty state.
A complete new page is published to the cache and bitmap before acknowledging
the write. Successful writeback or eviction never clears that bit. Reopening
reconstructs upper presence from sparse file extents using the existing format.
The cache keeps latest plaintext ahead of physical file state.



#### 11.4.2 Reads, writes and backpressure

Reads copy from clean, dirty or frozen writeback pages first. Only a cache miss
may read/decrypt the active file. Read loading reserves total capacity; if no
clean slot is available, a bounded foreground workspace can service the read
without insertion. It never bypasses a newer cached page.

Live and snapshot reads share bounded upper-run batching. Only contiguous cold
pages of the requested upper range enter a physical read (at most 1 MiB); cache
hits and base/hole boundaries stop the run. Sorted stripe-range locks prevent
concurrent writes/discard, while reserved Loading pages prevent eviction. With
less cache capacity than the run, remaining cold pages bypass caching under the
same stripes. Read/decrypt stays in the owned MAP_SHARED I/O workspace.
Loading pages are filled from that workspace outside the cache mutex, then
published as clean under the mutex before copying out to mutable caller or guest memory.
Contiguous base blocks within the requested bounded range are passed together
to `BlockReader`, which retains its own lower read boundaries. No request-sized
payload allocation or base prefetch is added.

Writes acquire total and dirty capacity together before constructing a new page.
Clean promotion needs only dirty quota; rewriting an unselected dirty page needs
no extra quota or queue node. Full-page overwrites need no old-data read. First
partial writes build the full page from base (or zeros), preserving the existing
short-base/error behavior. Later partial writes preserve every untouched byte.
The cache copies guest bytes before completion and never retains descriptor
buffers. Requests larger than the cache advance page by page.

When full, evict least-recently-used clean pages; otherwise wait for writeback.
Quota waits are cancellable and recheck state after waking. Waiters hold no page
reservation needed by the worker. One-page capacity and all-pages-in-flight must
make progress. No request creates a background goroutine or an unbounded payload
queue. Block stripes serialize foreground same-block operations; the worker does
not acquire these locks.



#### 11.4.3 Writeback and FLUSH

One sandbox worker anchors each batch on the oldest dirty page. Hot overwrites
do not move a page to the tail. It may batch adjacent dirty pages of the same
file up to 1 MiB, without filling holes or preallocating gaps. A fixed 1 ms
aggregation window allows low-rate traffic to progress; quota pressure and
internal Drain wake it immediately. Selected pages become frozen writeback:
reads remain possible and same-page writes wait. The worker copies frozen pages
directly into the active diff's existing aligned MAP_SHARED write workspace,
encrypts that copy, and performs I/O without cache or global locks. Cache page
payloads stay plaintext and never become syscall buffers. Foreground reads have
a separate workspace.

The bounded page index supplies contiguous dirty neighbours from the same file,
regardless of arrival order; unselected FIFO age is unchanged. Ordinary write
notifications neither end nor restart the fixed aggregation deadline. Pressure
and Drain interrupt it. An already aggregated backlog proceeds without a new
1 ms wait per batch, including batches that contain only one page.

A healthy, valid guest **FLUSH is a no-op**. It neither initiates writeback,
waits for dirty pages nor invokes Drain, fsync or fdatasync. Request ordering and
fatal-state checks remain. Existing wire features are retained for Cloud
Hypervisor snapshot compatibility; CONFIG_WCE and wire discard are not added.

Write completion promises only acceptance of complete latest bytes into bounded
logical COW state. Neither writes nor FLUSH guarantee survival of a
`sandbox-ctl` crash, host power loss or storage failure. This deliberately differs
from the stable-storage FLUSH requirement in
[Virtio 1.2 §5.2.6.2](https://docs.oasis-open.org/virtio/virtio/v1.2/virtio-v1.2.html).
It is the project's non-durable COW contract, not standard durable FLUSH behavior.
Direct I/O alone is not a durability barrier either.



#### 11.4.4 Active-file I/O contract

Every active body attempts O_DIRECT on its opened descriptor and always uses the
same bounded aligned workspaces. If that F_SETFL request returns EINVAL or
EOPNOTSUPP/ENOTSUP, the descriptor keeps ordinary positioned I/O; the application
buffer and cache are unchanged. This initialization-only capability decision
does not inspect the filesystem and is not a retry after failed data I/O. Fresh targets complete this setup **before seeding**; existing
active validation and runtime/snapshot reads use the same path. Runtime code does
not inspect filesystem types, names, mount policies or filesystem-specific inode
flags. Header and format probing are bounded before concurrent body I/O;
templates and base remain buffered/read-only. Plaintext and encrypted file
formats, the fixed 4 KiB encrypted header, local encryption off/auto/required
policy and 512-byte XTS data-unit numbering do not change.

`STATX_DIOALIGN` on the open fd supplies separate address and offset/length
constraints. Positive reported constraints must fit the workspace bound and
4 KiB COW geometry; offset alignment must divide 4096 and fit body boundary and
size. A missing mask, query-unavailable ENOSYS/EINVAL/EOPNOTSUPP, or both-zero
alignment fields selects conservative 4096-byte alignment. This is an alignment
choice, not proof of cache bypass. One-zero or
other invalid constraints, genuine statx/descriptor errors, other flag-setup errors and
actual I/O failures remain errors. No application-side buffered retry hides EIO,
ENOSPC, alignment errors or short I/O. See [statx(2)](https://man7.org/linux/man-pages/man2/statx.2.html).

The kernel and backing implementation determine how an O_DIRECT request is
fulfilled. Successful flag setup and aligned I/O establish usable operations,
not universal physical disk-cache bypass. Tmpfs file pages remain memory/swap
storage outside `cache_size`. There is no filesystem allowlist, special tmpfs
backend, new configuration mode or fallback after failed data I/O. See
[open(2)](https://man7.org/linux/man-pages/man2/open.2.html).

I/O uses bounded, aligned anonymous `MAP_SHARED` workspaces with lifetime through
syscall completion, avoiding the private-heap/fork hazard. Arbitrary caller slices
and subpage reads are copied through these buffers; large operations are chunked.
Full-page cache writeback avoids read/modify/write. Short writes fail rather than
retrying an unaligned remainder; see [write(2)](https://man7.org/linux/man-pages/man2/write.2.html).

### 11.5 Quiesce / Resume

Quiesce waits for in-flight block requests to leave and prevents new ones. All data/root views are read within the same quiesce window. Recovery makes backends serviceable before resuming CH and guest connections, avoiding permanent blocking of block requests after VM resume.

Low-level Discard serializes with writes and in-flight writeback before punching
complete blocks and clearing upper presence. Partial edges retain contents.
Discard exposes base bytes again; it is not an explicit zero masking the base.
The wire profile still rejects DISCARD and WRITE_ZEROES. Writing zeros still
materializes upper data and cannot be treated as a hole.

Internal `Drain` waits for accepted writes to finish without fsync. Normal Close
stops new frontend work, cancels quota waits, drains accepted healthy writes,
joins I/O, clears plaintext and closes the file. Frontend cancellation does not
cancel healthy writeback. Close is idempotent and drain/close errors propagate
through run/restore. Destruction must also wait for actual in-flight syscalls
before freeing their buffers.

Writeback and short-write failures are sticky: failed pages remain dirty-accounted,
new writes fail, all waiters wake, and the runtime receives a non-blocking fatal
notification without self-joining. A partial physical write may already have changed
the file. Cleanup of newly materialized blocks cannot punch previously existing
upper pages in the same batch. There is no transactional-write or crash-recovery
promise and no silent buffered retry.

The original error is latched and the owner notified before cleanup I/O. While rollback runs, active-I/O ownership, frozen pages and their quotas remain held so Close cannot release the file or buffers early. Cleanup errors are joined afterward without masking the original failure.

## 12. Validation, errors, and security

### 12.1 Cold validation

Ordinary cold configuration validates at least:

- Capacity/allocatable consistency with resource mode.
- Absolute, unlocated file bindings for kernel/runtime.
- A unique network provider, and a provider for any identity fields.
- Valid root/data topology, disk names/order, and a 1:1 disk-to-mount relationship.
- Active diff/template/size combinations capable of yielding a mountable filesystem.
- Consistent local/Manifest/Bundle refs and crypto policy.
- Launch/files/init/plugin/metadata limits.

### 12.2 `run --from` and restore validation

Both modes strictly parse the logical artifact and canonical configuration before checking host ownership. `--from` permits persistent-workload overrides; restore rejects all cold-only fields. Kernel binding preflight, runtime identity comparison, network topology/provider, disk count/names/topology, and active-diff binding checks precede external lifecycle side effects. This does not add a kernel-digest re-hash to these two modes; see [PortableSandboxConfig](sandbox.md#portable-config).

### 12.3 CLI mutual exclusion

- `run --from` and `--restore` are mutually exclusive.
- `run --replace-boot` is allowed only with `--from`, requires host configuration, and rejects `--restore`.
- Export/snapshot require exactly one of `--output` and `--upload`.
- Explicit `--mode` and `--upload` are mutually exclusive.
- Live export rejects `--config`; image-to-Sandbox-E assembly requires `--from` plus `--config` or `SANDBOX_CONFIG`, and rejects `--resume`.
- Snapshot has no memory toggle.
- The exec command must follow `--`; local and proxy target rules are mutually exclusive.

### 12.4 Failure contract

- Errors provide field/entry/ref/disk-index context without printing inline file/env values, customer keys, or plaintext digests.
- Local crypto errors retain protected presentation.
- ZIP/path-traversal/symlink/regular-file checks fail closed.
- Remote I/O receives operation contexts; cancellation follows the owning I/O implementation and is not proof of rollback of already committed objects.
- Streams, fetchers, and Bundle readers share explicit close ownership; failure cleanup must not leak fds, mappings, or goroutines.
- Any post-freeze failure must recover app, CH/backends/MUX/pinger/forwarding and release resource locks. If MUX recovery can no longer be established, CH must be terminated while locks remain held and the capture gate becomes terminal.
- Successful operation-root publication is the commit point, followed by the semantic alias for local output. Named locations have no alias. Orphan dependencies do not constitute success.

<a id="cow-validation"></a>

### 12.5 COW validation

Use deterministic worker gates and injected failures for quota transitions,
FIFO/LRU, one-page capacity, cancellation, multi-disk contention, frozen-page
reads/rewrites, FLUSH, snapshots, Discard and Close. Real direct-I/O checks cover
alignment, sparse boundaries, templates, plaintext/encrypted reopen and mincore
page residency on a disk-backed fixture. Generic alignment tests cover positive,
missing, both-zero and malformed constraints, unavailable queries and genuine
errors. Dedicated real-tmpfs tests verify the fixture descriptor type and the
same O_DIRECT request, plaintext/encrypted I/O, bounded staging and copyout,
template seeding,
sparse holes, shared mixed-filesystem quotas/backpressure, dirty/writeback capture,
Drain/Close and reopen. `scripts/test-vhost-tmpfs-enospc.sh` explicitly runs the
plain and encrypted real-ENOSPC cases as root in a new mount namespace. The
30-second test deadline is retained, with an outer bounded timeout for abnormal
hangs. The runner requires both subcases to actually pass; failure, omission or
Skip cannot satisfy required CI. The test
uses a 32 KiB private tmpfs and never exhausts or remounts a shared mount; ordinary
`go test` runs skip only this capability-requiring case rather than attempting
implicit privilege escalation.
DIO/mincore benchmarks retain disk-backed fixtures. Tmpfs functional tests are
not evidence of disk cache bypass. Run targeted tests, race, vet, build, broader
tests and real CH/KVM tmpfs guest cold-start/read-write
and pause/export/restore; report skips and infrastructure failures explicitly.

Performance comparisons need datasets much larger than the cache and separate
frontend admission latency from total elapsed time including Drain. Cover
sequential/random 4 KiB, 512-byte updates, hot overwrites, encryption and multiple
disks. Report tail latency, logical/physical write volume, stable quota usage and
file-cache/dirty/writeback or mincore evidence. Compare buffered baseline and
candidate on the same storage. Do not manufacture low cache residency with
fsync/DONTNEED/drop_caches; exclude unchanged template input, artifact output and
shared guest mappings from active-diff memory attribution. Measurements are
observations, not unmeasured throughput targets or claims that skipped E2E passed.

#### 12.5.1 Repeatable COW measurement method

The historical runs remain in their original Git commits and [COW work](https://github.com/kuasar-sandbox/sandboxer/issues/230); measured revision tables are not a maintained specification. Repeat a comparison with recorded revisions, kernel/filesystem/storage, cache configuration, encryption, workload and prerequisites. A documented method is not evidence that it ran on the current revision.

Use the existing `blk_cow_workingset_bench_test.go` working-set driver and a matching adapter for each revision. A buffered baseline without a sandbox cache uses no-op cache drain/close and no cache statistics; do not imply that its request completion is media durability. Keep candidate admission and final Drain separate:

```bash
go test -tags no_rocksdb ./pkg/vhost -run '^$' \
  -bench '^BenchmarkCOWWorkingSet$' -benchtime=1x -count=1 -timeout 15m
```

Use a 256 MiB logical dataset, eight times a 32 MiB cache: sequential 1 MiB writes; deterministic random 4 KiB and 512-byte updates across 65,536 pages; hot overwrites with 90% in 4 MiB and 10% across the remainder; and prefilled reads. The 512-byte workload transfers 32 MiB while materializing 256 MiB of pages. Prefill hot/read workloads outside timing. Alternate 1 MiB requests between two 128 MiB disks to check a shared sandbox budget. Cover plaintext and XTS. This is a single-foreground backend driver, not a guest measurement.

Exclude fixture/setup work; measure request percentiles including quota waits, logical throughput and total elapsed including Drain. Buffered baseline completion means kernel acceptance; buffered prefilled reads can be warm in the kernel cache. No fsync, DONTNEED or drop_caches may manufacture a cache-residency result. Count active-file body read/write calls and bytes; `/proc/self/io` describes filesystem accounting, not durable media writes or an automatic write-amplification claim. Use mincore without touching mapped pages for active-file residency, and report logical cache/dirty peaks separately from heap/RSS.

The existing `cow_direct_workingset_bench_test.go` provides an uncached direct-I/O reference with the same active-I/O and XTS rules, not a selectable production mode:

```bash
go test -tags no_rocksdb ./pkg/vhost -run '^$' \
  -bench '^BenchmarkCOWDirectWorkingSet$' -benchtime=1x -count=1 -timeout 15m
```

Direct-I/O prefill distinguishes cached reads from warm kernel-file-cache effects. Hot-path checks cover nil/background/cancelable contexts and signaled waiters. Deterministic tests verify fixed batching deadlines, pressure/Drain bypass, no per-batch delay with backlog, oldest-dirty progress, bounded adjacent reads with small caches, mixed cache/base/hole reads, overlap/cancellation, sub-page and short-read boundaries, and copyout isolation from guest-mutated output buffers.

For actual guest validation, use the existing owner E2E cases for diff templates, encrypted diffs, snapshot/restore and data disks, with `REQUIRE_KVM`, private `TMPDIR` and matching `BIN`/native/runtime inputs. Record each unavailable prerequisite as missing validation. A guest probe can write a 64 MiB span in 1 MiB requests, then warm and overwrite the first 4 MiB with 4,096 aligned 4 KiB operations using mmap-backed buffers and O_DIRECT. Verify byte counts and payloads. Guest descriptor segmentation does not measure 1 MiB host batches, and admission-only timing without Drain/fsync is not full writeback timing. Keep raw run results in the originating PR/Issue or immutable Git history.

<a id="usage-validation"></a>

### 12.6 Usage performance and validation

Storage grows by cumulative checkpoints, not per-second samples. Record size
depends on vCPU/disk count, source strings and varint magnitudes; 1 KiB is not
a measured constant. CPU/Gauge aggregation and persistence tests are in
[`pkg/usage`](../pkg/usage); protocol and blocking-source tests live with
[`guestlink`](../pkg/guestlink) and [`sandbox-init`](../cmd/sandbox-init).
The component-owned [usage E2E](../test/e2e/e2e_usage.sh) is discovered by
[`run_all.sh`](../test/e2e/run_all.sh), requires real KVM, and verifies the
running Guest init hash against the supplied freshly rebuilt runtime bundle.
Its short-save cases exercise integration. Restore/clone checks use a one-minute
sample interval and require fresh memory/filesystem observations before the
first periodic tick, so retries cannot conceal a missed immediate ACK round.
A separate `defaults` case waits
for the actual default five-minute save. Neither is a production-density
performance measurement.

The OOM case pre-establishes an exec/MUX whose test probe waits for one stdin
byte before allocating pressure memory. A bounded test-only LaunchPort relay
forwards the ordinary hello/MUX stream and report/ACK frames unchanged. A new
epoch/sequence with a successfully forwarded ACK supplies a report-delivery
boundary, not proof that the Host control transaction has completed. The test
requires a real converged CH baseline and a fresh boundary before starting
pressure. Only actual growth before the first target change, with no accepted
or ambiguous resize in that interval, qualifies as autonomous deflation.
Duplicate reports, error ACKs, expired boundaries and later deflation after
a control resize do not qualify; the normal control loop remains running.
Recorded start-byte write endpoints use the Host monotonic clock; they are
not Guest allocation timestamps or a measurement of Guest scheduling latency.

The [storage-fault E2E](../test/e2e/e2e_usage_faults.sh) uses a private bounded
tmpfs for real ENOSPC and path-restricted `strace` injection for usage write
EIO and delayed writes. It also exercises SIGKILL and a sub-second Guest
run. Injection never targets the business writable disk's sync operations;
these tests do not simulate physical power loss. The Host-kill case pins and
verifies its CH child before the crash, then confirms that child's bounded
termination through a pidfd; it does not leave cleanup to the runner.
Linux/Python pidfd support, `strace`, mount privileges
and the ordinary KVM E2E prerequisites are required.

The [slow-source E2E](../test/e2e/e2e_usage_sources.sh) selects one registered
data-filesystem handle by device identity and checks CLOEXEC before delaying
only that `fstatfs` return inside the disposable Guest. It requires a first
timeout, subsequent busy responses from the same occupied slot, fresh memory
and healthy-filesystem coverage, and exactly one traced filesystem call.
Both data disks receive real writes. Bytes written after the delayed read
distinguish a new observation from a replayed old buffer after release.
A single long-lived test exec records all Guest FDs, identities, threads and
RSS; it does not create an exec per observation or label threads as goroutines.
The filesystem case's default fault lasts 30 seconds; `--seconds 60` extends it.
It is a syscall-return delay, not a physical-device failure or power-loss test.
The installed native `strace`, its `ldd`-resolved loader and libraries are
copied into the disposable application image only, never the product runtime.
Its `ch-info` and `ch-resize` cases place a test-only Unix HTTP relay before
the real CH executable. The relay forwards unchanged response bodies and
holds one actual reply for 12 seconds. The resize is driven by a real Guest
allocation, not by a test-issued resize. Selection requires a target lower
than the preceding real target; the held PUT reply or its immediate confirming
GET exercises the existing mutation-gate/API-lock transaction. Once the old observation expires, Guest
memory must be missing while filesystem/RSS coverage advances, without more
CH requests piling up. Recovery requires a new successful `vm.info` read.
These bounded functional faults do not measure uninstrumented API latency or
production-density overhead, nor identify which caller owns every info read.
The `vsock` case relays the real dedicated usage frames and preserves ordinary
management/MUX traffic. It exercises eight disconnects, one late reply, a
captured old response and slow fragments beyond the original round deadline,
while one managed filesystem syscall remains occupied. Raw Guest responses
must continue to report the same busy slot across new connections. Failed
rounds break Gauge coverage rather than filling zero; native CPU counters
remain a separate source. One observer exec streams resource diagnostics
across all eleven faults. Its first complete output establishes MUX readiness
before injection. Additional health/stop execs run outside the all-FD observation
window while the filesystem is still blocked; no FD samples are discarded.
The relay observes peer EOF while a late or fragmented reply is still held,
so the read-deadline witness is Host closure, not the next request's start.
One intervening tick may correctly remain missing while the original Host
read or Guest admission slot exits. New-connection recovery is checked separately.
Live-query evidence
retains changes to every metric's request ID and compares each metric with
its own raw request; it does not assume atomic publication of a whole round.
The uninjected root/second filesystem must independently return healthy raw
data and advance its own request identity and covered time; agreement between
two failed statuses or retention of an old successful value cannot pass.
The `restore` case keeps one occupied filesystem slot across same-process
quiesce/thaw and then lazy memory restore with the same SandboxID/usage file.
A bounded, test-only tracer owner joins the disposable Guest's root cgroup
before spawning `strace`; PID 1, the application and exec-join processes are
not moved. The Guest reaper owns the adopted helper, which alone waits for
its tracer. This avoids ordinary exec's intentional termination at quiesce.
The test-only CH wrapper relocates the vsock socket in private restore run
state, not in the immutable business snapshot. On the first restored response,
the relay replaces only the epoch with the real previous run's identity,
retaining the new request ID and values. This is explicit identity corruption,
not an unmodified historical frame. Host closure and a new request on another
connection must demonstrate rejection. Raw replies must still report the
original busy slot, while memory and healthy filesystems remain available.
Writes after the original read and after restore distinguish fresh recovery
from a late old buffer; the first new value cannot integrate the missing gap.
Both original and restored tracer owners must detach, and failure cleanup
still stops the owned VMs and reaps Host-side test subprocesses.
Before successful capture, the same case exhausts a test-owned snapshot-output
tmpfs after the immutable dependency preflight. The test requires a data-disk
capture ENOSPC after Guest quiesce/CH pause, MUX reattachment, a real health
exec and CH `Running` state. The occupied slot and its known usage must survive
this failed-capture rollback while healthy sources advance. The output limit
does not affect the usage-file directory; an earlier dependency error cannot
pass as rollback evidence. The temporary mount is unmounted on both paths.
Together with the repeated-operation and lifecycle regressions, these cases
check source isolation, bounded ownership and failure cleanup. Report their
actual observation windows; they are not physical power-loss experiments.

Run the [off/on harness](../test/e2e/usage_perf.py) without concurrent test
loads, supplying the assembled `BIN` and root privileges:

```bash
python3 test/e2e/usage_perf.py --densities 1,4 --seconds 30 --repeat 3
```

The baseline measures native process CPU, RssAnon/RssFile, FD/thread and
non-dead Go G counts, actual record bytes, concentrated-stop duration and
end-to-end exec p95/p99. Go G includes runtime system goroutines; it is not
`runtime.NumGoroutine`. Exact-binary DWARF/symbol inspection requires `gdb`
and Go tools. Diagnostic artifacts retain native proc stat text, individual
utime/stime/guest_time endpoints, read windows, boot identity and tick scale.
`proc-observations.json` preserves completed reads even when sampling fails;
incomplete pairs/windows are explicitly marked and missing endpoints are not
invented. These test artifacts are separate from the compact usage file.

Optional diagnostics can help investigate the baseline; they are not a separate
performance acceptance target or a prerequisite for merging this feature:

```bash
python3 test/e2e/usage_perf.py --densities 1,4 --seconds 30 --repeat 3 --trace
```

This optional Linux amd64 tracing run requires a `bpftrace` build
with instruction-offset support, tracefs access and the initial PID namespace
(`BPFTRACE_BIN` can select an installed tool). A preflight rejects differing
kernel and `/proc` PID identities before attaching target probes. It verifies
the real sched exec event of its own short-lived child, not only bpftrace's
version-dependent bare `pid` builtin. Empty or
failed tracing is not zero overhead. It adds wakeups, mallocgc requests/requested bytes, usage
framing traffic, CH info requests, contended API mutex wait and save-worker
duration. It verifies ordinary instruction probes against the exact binary;
it does not insert Go return trampolines. Tracing perturbs timing, so its
latencies cannot replace the untraced baseline. The harness checks available
memory before admitting density and does not change host resource limits.

Measure off/on using the same source set, runtime/kernel/CH, configuration,
density and load. Record those inputs and report Guest/Host CPU, per-process
RssAnon/RssFile, FD/goroutines, actual record bytes, concentrated-stop duration
and business p95/p99 across idle, busy CPU, memory pressure, multiple disks
and saves. This is descriptive feature validation, with no added SLO or
production-density certification.

Wakeups, allocations, management traffic, CH API counts/lock timing and
per-save duration are optional diagnostics. Unavailable measurements stay
explicitly unmeasured, not zero or passed; obtaining a privileged profiling
environment is not a merge prerequisite. Functional correctness, bounded
source/writer ownership, lifecycle and fault regressions, and the repository's
normal review and exact-integration CI remain required. Unit models and
ordinary-process experiments cannot replace CH/KVM evidence. Keep exact
measurement revisions, environments and limitations in the PR and evidence
artifacts; no benchmark number here is inferred from the design.

<a id="read-recovery-validation"></a>

### 12.7 Source read recovery validation

Regression coverage includes ring/completion invariants, COW materialization, EOF/EAGAIN wrappers, backend cancellation, success-only initialization, queue/pause shutdown and real kernel REMOVE/COPY page contents. Owner E2E additionally checks CH/KVM behavior with matching component binaries, runtime image and native dependencies. An unavailable prerequisite is missing validation, not a passed test.

## 13. Reliability, performance, and compatibility boundaries

### 13.1 Streaming and memory use

- Sparse tarstream does not spool the logical stream to disk.
- Manifest ingest reads resident extents rather than materializing holes.
- Large E/S payloads are not staged in the per-run `/run` directory; CH's bounded config/state staging files go there. Caller-selected output directories remain separate and consume their selected filesystem's capacity. Named-location Bundle publication computes identity in a streaming pass and writes only its final content-addressed file.
- Bundle dependency planning performs remote I/O and admission before pause.
- V1 `--resume` may keep the VM paused until sink writes finish, preserving SnapshotView stability.

### 13.2 Performance observations

Key metrics:

- Preflight latency.
- Guest freeze-ACK latency.
- CH pause latency.
- Disk/E emission time.
- CH memory-snapshot and resident-memory emission time.
- Resume/reattach latency.
- Manifest stored/deduplicated chunks.
- UFFD fault latency and restore-ready latency.

Benchmarks separately cover local tarstream/Bundle creation, Bundle reads, sparse merging, and UFFD faults. Performance optimization must not alter Hole/Zero/Data semantics, commit order, identity-verification policy, or freeze safety.

<a id="read-recovery"></a>

### 13.3 Synchronous source read recovery

`sandboxer` owns synchronous retry above `fetch.Stream` and `sparse.Run`. The same policy applies to file, Manifest Bundle, cache and Store sources. `accelerator` performs one business read attempt and preserves the error chain; a subsequent invocation can recover using the same selected source.

A required runtime read keeps its original request, buffer and inflight ownership while it waits. A temporary failure does not complete a Guest disk request or populate a missing memory page. When a required read cannot recover, the VM owner terminates the whole CH process and sandbox, preserving the first read cause.

The behavior applies to cold start, including `run --from manifest://...`, memory restore and subsequent disk/memory reads. Read-only artifact opening and metadata inspection use the same helper. Invalid command arguments return an error to their caller. A management input error or optional prefetch failure is not a runtime fatal notification.

There is no additional command, retry-count argument or recovery mode. An operation can be ended by its existing context or sandbox shutdown. A source outage may delay readiness, exec, or snapshot drain because those operations can need the unavailable data.

Backoff is internal: start at 10 ms, double after each failed attempt, cap at 1 s, and sample each actual wait uniformly from 50% through 100% of that step. Waits observe the operation context. There is no attempt-count or cumulative-time exhaustion that turns a pending read into Guest IOErr.

Backend socket/RPC deadlines still bound individual attempts. An internal backend cancellation or timeout does not end an otherwise live operation. Existing configured health policy remains independent; read recovery does not suppress health checks, restart services or select another endpoint. Customer keys remain fixed for each `ProcessStorage`, including the first key-resolution error.

`internal/readretry` returns terminal causes; it does not kill processes. Unknown access errors are conservatively retried. `accelerator/pkg/readerr` exposes a minimal `Retryable() bool` marker and `Unwrap`. Explicit permanent reasons include validated geometry, malformed complete frames, immutable object absence at a required lookup, and confirmed content/format/authentication failures. Custom decryptors and transport failures retain their actual cause rather than being classified solely by an operation name.

Each attempt may overwrite the same buffer, but later attempts read the full requested range again. Partial responses are not spliced. Parallel backend reads join all workers before returning; a derived cancellation cannot hide an original or later permanent cause. A legal complete read accompanied by ordinary EOF retains its normal contract. Terminal wrappers around EOF or EAGAIN are checked before compatibility or zero-fill branches.

Read-only coverage includes location-source verification, EROFS build-prefix probing and merge-parent sparse metadata. They retry individual source reads, without replaying publication or capture writes. Merge/seeker adapters and Ext4 validation preserve a terminal error even if its attempt filled the requested buffer. ZIP metadata parsing retains the source's first terminal error for that parse, because library helpers can otherwise discard a full read's error. Artifact format detection stops on that cause; missing source data is not treated as an absent optional image configuration. Ordinary format detection and optional configuration absence keep their existing behavior.

Lazy process Fetcher initialization, referenced Bundle resolution and Bundle Chunk-index preparation cache only successful results. Failed or canceled initialization leaves later calls able to try again. Closing an owner prevents initialization or resource publication from resurrecting it. Once a Manifest source is selected, a failed data read stays with that source.

The healthy retry helper allocates no timer and starts no goroutine. A failed synchronous read owns one reusable timer and its existing request/buffer. Backoff is bounded per wait, while the operation may remain pending indefinitely. Connection capacity includes idle, borrowed and dialing connections; bounded maintenance workers cannot grow with outage duration. Validation covers healthy-path allocation/latency comparisons and outage resource checks; these do not constitute a separate performance certification.

The backend marker contract remains in [Accelerator read errors](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/accelerator-read-recovery.md); see also the original [proposal and acceptance checklist](https://github.com/kuasar-sandbox/sandboxer/issues/225).

#### 13.3.1 Fatal ownership and capture

The worker reports a required read fatal before releasing inflight ownership; a later queue stop cannot suppress a permanent cause already returned by the backend. `ServeAndWait` records the first cause, cancels related waits and PostSpawn/handshake work, and directly kills CH. The existing sole `cmd.Wait` owns reaping and output draining. A worker never synchronously waits for its own cleanup. Ready commitment and fatal recording share one lock, so a recorded fatal prevents any new Ready transition. Delivery of an already committed Ready event runs outside that lock; a blocked notifier cannot delay fatal cancellation or CH termination.

Snapshot/export retain their freeze, drain and consistency conditions. Retries may extend the drain; inflight accounting is not reduced to pass it. Capture checks operation termination before committing and before returning success. If capture conditions cannot be met, the operation fails. Queue stop cancels waiting reads and the snapshot gate wait before join; old workers cannot write into a replacement master's memory table. UFFD queue submission also observes cancellation. Shutdown preserves reader completion before the remove flusher's final drain and then releases resources.

<a id="artifact-compatibility"></a>

### 13.4 Artifact compatibility boundaries

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


S/E suffix geometry, deterministic ZIP encoding and sparse prefix/append views are implemented by `accelerator/pkg/tailzip`. This package supplies ordered role entries and size limits; the Sandbox/Snapshot modules retain their portable config, JSON/state and device-topology validation. Logical reference locations use `manifest.RefLocations`.

## 14. See Also

- [sandbox-init.md](sandbox-init.md) — Guest PID 1 and launch/quiesce/MUX protocols.
- [cloud-hypervisor.md](cloud-hypervisor.md) — CH build, API, and restore boundaries.
- [Connector TAPFD protocol](https://github.com/kuasar-sandbox/connector/blob/main/docs/tapfd.md) — TAP descriptor handoff and network namespaces, owned by connector.
- [timeouts-production.yaml](../examples/timeouts-production.yaml) — Production host-timeout example.
- [restore-prefetch-memory.yaml](../examples/restore-prefetch-memory.yaml) — Explicit memory-prefetch example.
- [README.md](../README.md) — Build, release, and repository entry points.
