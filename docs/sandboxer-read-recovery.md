[English](sandboxer-read-recovery.md) | [简体中文](sandboxer-read-recovery_zh.md)

# Synchronous source read recovery

## 1. Overview

`sandboxer` owns synchronous retry above `fetch.Stream` and `sparse.Run`. The same policy applies to file, Manifest Bundle, cache and Store sources. `accelerator` performs one business read attempt and preserves the error chain; a subsequent invocation can recover using the same selected source.

A required runtime read keeps its original request, buffer and inflight ownership while it waits. A temporary failure does not complete a Guest disk request or populate a missing memory page. When a required read cannot recover, the VM owner terminates the whole CH process and sandbox, preserving the first read cause.

## 2. CLI

The behavior applies to cold start, including `run --from manifest://...`, memory restore and subsequent disk/memory reads. Read-only artifact opening and metadata inspection use the same helper. Invalid command arguments return an error to their caller. A management input error or optional prefetch failure is not a runtime fatal notification.

There is no additional command, retry-count argument or recovery mode. An operation can be ended by its existing context or sandbox shutdown. A source outage may delay readiness, exec, or snapshot drain because those operations can need the unavailable data.

## 3. Configuration

Backoff is internal: start at 10 ms, double after each failed attempt, cap at 1 s, and sample each actual wait uniformly from 50% through 100% of that step. Waits observe the operation context. There is no attempt-count or cumulative-time exhaustion that turns a pending read into Guest IOErr.

Backend socket/RPC deadlines still bound individual attempts. An internal backend cancellation or timeout does not end an otherwise live operation. Existing configured health policy remains independent; read recovery does not suppress health checks, restart services or select another endpoint. Customer keys remain fixed for each `ProcessStorage`, including the first key-resolution error.

## 4. Reading and ownership

`internal/readretry` returns terminal causes; it does not kill processes. Unknown access errors are conservatively retried. `accelerator/pkg/readerr` exposes a minimal `Retryable() bool` marker and `Unwrap`. Explicit permanent reasons include validated geometry, malformed complete frames, immutable object absence at a required lookup, and confirmed content/format/authentication failures. Custom decryptors and transport failures retain their actual cause rather than being classified solely by an operation name.

`StreamReader` retries a complete read synchronously. Runtime queue contexts replace preparation contexts for that call, so stopping an old master session cancels its pending reads before its workers are joined and its memory table is replaced. The COW adapter passes that context into base materialization while retaining block locks, the dirty bitmap and partial-write algorithm. It never retries an entire `WriteAt`, `FLUSH`, Store `Put` or snapshot operation.

UFFD directly retries required `Run.ReadAt` operations. The urgent page and any Chunk window containing it are required. A pure speculative tail makes one best-effort attempt. The existing `ChunkRun` capability, buffers, tail reservation and ioctl partial-progress/EEXIST/EAGAIN convergence remain in use. After a delayed source read, urgent installation rechecks page state; an observed REMOVE invalidates the old data and uses the existing zero-page convergence without scheduling a tail from that obsolete plan.

Each attempt may overwrite the same buffer, but later attempts read the full requested range again. Partial responses are not spliced. Parallel backend reads join all workers before returning; a derived cancellation cannot hide an original or later permanent cause. A legal complete read accompanied by ordinary EOF retains its normal contract. Terminal wrappers around EOF or EAGAIN are checked before compatibility or zero-fill branches.

Lazy process Fetcher initialization, referenced Bundle resolution and Bundle Chunk-index preparation cache only successful results. Failed or canceled initialization leaves later calls able to try again. Closing an owner prevents initialization or resource publication from resurrecting it. Once a Manifest source is selected, a failed data read stays with that source.

## 5. Reliability and capture

Both `processChain` and `processQueue` recognize stopped/terminal required reads. They leave the status byte, used ring and queue base uncompleted. This also applies to the implicit base read inside a COW write. Ordinary unsupported requests and independent writable-diff errors keep their existing protocol behavior.

The worker reports a required read fatal before releasing inflight ownership; a later queue stop cannot suppress a permanent cause already returned by the backend. `ServeAndWait` records the first cause, cancels related waits and PostSpawn/handshake work, and directly kills CH. The existing sole `cmd.Wait` owns reaping and output draining. A worker never synchronously waits for its own cleanup. Ready commitment and fatal recording share one lock, so a recorded fatal prevents any new Ready transition. Delivery of an already committed Ready event runs outside that lock; a blocked notifier cannot delay fatal cancellation or CH termination.

Snapshot/export retain their freeze, drain and consistency conditions. Retries may extend the drain; inflight accounting is not reduced to pass it. Capture checks operation termination before committing and before returning success. If capture conditions cannot be met, the operation fails. Queue stop cancels waiting reads and the snapshot gate wait before join; old workers cannot write into a replacement master's memory table. UFFD queue submission also observes cancellation. Shutdown preserves reader completion before the remove flusher's final drain and then releases resources.

Regression coverage includes ring/completion invariants, COW materialization, EOF/EAGAIN wrappers, backend cancellation, success-only initialization, queue/pause shutdown and real kernel REMOVE/COPY page contents. Owner E2E additionally checks CH/KVM behavior with matching component binaries, runtime image and native dependencies. An unavailable prerequisite is missing validation, not a passed test.

## 6. Performance

The healthy retry helper allocates no timer and starts no goroutine. A failed synchronous read owns one reusable timer and its existing request/buffer. Backoff is bounded per wait, while the operation may remain pending indefinitely. Connection capacity includes idle, borrowed and dialing connections; bounded maintenance workers cannot grow with outage duration. Healthy-path allocation/latency comparison and outage resource checks accompany this change; they are not a separate performance certification.

## 7. See Also

- [Sandbox lifecycle](sandbox.md)
- [Artifact contracts](sandbox-artifacts.md)
- [Accelerator read errors and recovery](https://github.com/kuasar-sandbox/accelerator/blob/main/docs/accelerator-read-recovery.md)
- [Proposal and acceptance checklist](https://github.com/kuasar-sandbox/sandboxer/issues/225)
