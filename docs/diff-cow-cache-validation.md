[English](diff-cow-cache-validation.md) | [简体中文](diff-cow-cache-validation_zh.md)

# Issue #230 validation observations

The [cache/direct I/O contract](diff-cow-cache.md) bounds active-diff memory but
changes I/O completion and performance. The original d601fa0 measurements below show lower file
page-cache residency and substantial throughput regressions against the buffered
baseline on this storage. They are retained as historical observations, not
performance targets. Revision measurements and independently supplied d601
CH/KVM evidence are recorded separately below.

## Environment and method

Measured on 2026-09-17, Linux/amd64, Intel Xeon Gold 6266C (88 logical CPUs visible),
Go 1.26.5, kernel `6.6.0-159.4.6.157.20260713.a4e2472763b2.oe2403sp4.x86_64`.
Sources were isolated under `/var/tmp/diff-cow-230-lb6t8B`; the storage was ext4
on `/dev/sdb2` with 4 KiB filesystem blocks. `/tmp` is tmpfs. Go tests used a
short, canonical, task-owned disk directory `/var/q` for TMPDIR because some
existing Unix-socket fixtures exceed the path limit with a longer TMPDIR and
other existing tests compare canonical paths. No test was weakened for this.

B is exact main `b1db083dee2cacac141d4033bbf41b518cf80c9e`, with only the common
benchmark harness and a baseline adapter added. Historical C is `d601fa0`, this issue's initial implementation
with the default shared 32 MiB total / 16 MiB dirty subset. The
[common harness](../pkg/vhost/blk_cow_workingset_bench_test.go) runs unchanged on
both, using [the candidate adapter](../pkg/vhost/cow_cache_bench_adapter_test.go)
or this baseline replacement:

```go
package vhost

func newWorkingSetCache() (workingSetCache, error) {
    return workingSetCache{
        drain: func() error { return nil },
        close: func() error { return nil },
        stats: func() map[string]float64 { return nil },
    }, nil
}
```

Run the following separately, baseline then candidate, on the same disk with a
disk-backed TMPDIR. Each case is one complete workload; these are single samples,
not confidence intervals. An initial run without the additional `/proc/self/io`
counters also completed; the tables report the subsequent complete run with those
counters, without selecting the faster sample.

```sh
go test -tags no_rocksdb ./pkg/vhost -run '^$'   -bench '^BenchmarkCOWWorkingSet$' -benchtime=1x -count=1 -timeout 15m
```

Every workload spans 256 MiB, eight times total cache capacity. Sequential requests
are 1 MiB. Random 4 KiB and 512-byte requests use a deterministic permutation of
65,536 pages; 512-byte requests transfer 32 MiB but materialize a 256 MiB footprint.
Hot overwrites prefill the whole dataset, then send 90% of 4 KiB updates to a
4 MiB hot region and 10% across the dataset. Read cases also prefill the dataset.
Two-disk cases alternate 1 MiB requests over two 128 MiB diffs sharing one budget.
Encryption uses the existing local XTS format. There is one foreground requester;
these are backend measurements, not guest/application benchmarks.

Setup is outside timing and I/O deltas. Latencies include quota waits; admission
ends after the last frontend request. C total includes Drain, and reported
throughput uses that total. B has synchronous buffered syscalls and no userspace
writeback queue, so its adapter's Drain is empty: **B total measures buffered
completion, not storage-media completion**. Neither comparison forces fsync,
DONTNEED or drop_caches. Read samples intentionally retain B's naturally warm
file cache. Fresh-file metadata initialization keeps its existing sync behavior
outside measurement; guest FLUSH, writeback and Drain add no sync.

## Historical d601 elapsed time, throughput and request latency

B admission and total are equal at the displayed precision. Throughput is logical
MiB divided by total elapsed; p50/p99 describe individual requests, not Drain.

| Workload | XTS | B admission/total (s) | C admission (s) | C total (s) | B / C (MiB/s) | B p50 / p99 (µs) | C p50 / p99 (µs) |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `seq-write` | false | 0.1329 | 1.192 | 1.255 | 1926 / 204 | 500.7 / 701 | 3795 / 7374 |
| `random-4k` | false | 0.1847 | 27.44 | 29.26 | 1386 / 8.749 | 2.628 / 4.373 | 388.9 / 1530 |
| `partial-512` | false | 0.3087 | 26.92 | 28.73 | 103.7 / 1.114 | 3.932 / 12.6 | 386.7 / 1501 |
| `hot-overwrite` | false | 0.0683 | 2.113 | 3.901 | 3748 / 65.62 | 0.806 / 2.473 | 0.87 / 436 |
| `seq-read` | false | 0.06802 | 13.92 | 13.92 | 3764 / 18.4 | 244.7 / 335.1 | 54004 / 63544 |
| `random-read` | false | 0.07078 | 16.1 | 16.1 | 3617 / 15.9 | 0.986 / 1.501 | 196.1 / 604.1 |
| `multi-disk` | false | 0.1299 | 0.9948 | 1.059 | 1971 / 241.8 | 496.4 / 605.5 | 3984 / 6361 |
| `seq-write` | true | 1.177 | 1.88 | 1.994 | 217.5 / 128.4 | 4592 / 4716 | 7398 / 10808 |
| `random-4k` | true | 1.24 | 28.38 | 30.22 | 206.5 / 8.472 | 18.62 / 21.28 | 406.2 / 1464 |
| `partial-512` | true | 1.366 | 27.92 | 29.75 | 23.43 / 1.076 | 19.94 / 29.53 | 403.4 / 1443 |
| `hot-overwrite` | true | 1.108 | 2.148 | 3.946 | 231 / 64.88 | 16.68 / 18.82 | 0.527 / 450.3 |
| `seq-read` | true | 1.137 | 14.66 | 14.66 | 225.1 / 17.46 | 4462 / 4481 | 57156 / 68747 |
| `random-read` | true | 1.155 | 17.46 | 17.46 | 221.6 / 14.66 | 17.49 / 19.24 | 218.1 / 614.5 |
| `multi-disk` | true | 1.182 | 1.939 | 2.069 | 216.5 / 123.7 | 4602 / 4796 | 7916 / 11218 |

## Historical d601 traffic and memory evidence

Body write counts are successful active-body syscall bytes, excluding setup.
`proc write` / `proc read` are deltas of Linux `/proc/self/io` `write_bytes` /
`read_bytes`; these are filesystem I/O accounting, not device durability or a
measurement of lower-storage amplification. In particular, buffered dirty-page
charges do not prove those bytes have reached media by the timing boundary.
`cancelled_write_bytes` was zero in every measured interval. Logical traffic in
read rows denotes reads.

| Workload | XTS | Logical MiB | B / C body write MiB | B / C proc write MiB | B / C proc read MiB |
| --- | --- | ---: | ---: | ---: | ---: |
| `seq-write` | false | 256 | 256 / 256 | 256 / 256 | 0 / 0 |
| `random-4k` | false | 256 | 256 / 256 | 256 / 256 | 0 / 0 |
| `partial-512` | false | 32 | 256 / 256 | 256 / 256 | 0 / 0 |
| `hot-overwrite` | false | 256 | 256 / 35.523 | 0 / 35.523 | 0 / 0 |
| `seq-read` | false | 256 | 0 / 0 | 0 / 0 | 0 / 256 |
| `random-read` | false | 256 | 0 / 0 | 0 / 0 | 0 / 253.91 |
| `multi-disk` | false | 256 | 256 / 256 | 256 / 256 | 0 / 0 |
| `seq-write` | true | 256 | 256 / 256 | 256 / 256 | 0 / 0 |
| `random-4k` | true | 256 | 256 / 256 | 256 / 256 | 0 / 0 |
| `partial-512` | true | 32 | 256 / 256 | 256 / 256 | 0 / 0 |
| `hot-overwrite` | true | 256 | 256 / 35.523 | 0 / 35.523 | 0 / 0 |
| `seq-read` | true | 256 | 0 / 0 | 0 / 0 | 0 / 256 |
| `random-read` | true | 256 | 0 / 0 | 0 / 0 | 0 / 253.91 |
| `multi-disk` | true | 256 | 256 / 256 | 256 / 256 | 0 / 0 |

For sequential reads, body read calls transferred 256 MiB in both versions.
For random reads, B transferred 256 MiB and C transferred 253.914 MiB; the small
remainder hit C's cache. Hot overwrites reduced C body writes from B's 256 MiB to
35.523 MiB, but C still took longer through Drain. First 512-byte writes retain
full-page materialization: 32 MiB logical traffic writes 256 MiB in both versions.

`mincore` queried only the active body after measurement, without touching the
mapped pages: B had **256 MiB resident in all 14 cases**, C had **zero in all 14**.
No immutable template/base input, exported artifact, or guest memfd is included.
C's total peak and final cache payload were **32 MiB in every case**, dirty peak
was **16 MiB**, and final dirty usage was **zero**, including both two-disk cases.
Peaks include prefill in read/hot cases. These counters demonstrate the shared
payload budget across an 8× working set, not a process RSS cap.

End-of-sample Go HeapInuse ranged **3.33–5.31 MiB (B)** and
**45.34–53.05 MiB (C)**. Process RSS ranged
**18.61–25.55 MiB (B)** and **71.34–94.94 MiB (C)**.
These are whole benchmark-process observations including Go allocator retention,
page/index metadata, fixed I/O mappings and harness buffers; they do not isolate
cache overhead or promise a maximum RSS. The buffered file cache is separate from
process RSS. No production throughput threshold was invented from these samples.

## Correctness checks and remaining validation

Targeted vhost/config/sandbox/restore/snapshot/CLI tests, the six-package race
suite, whole-repository vet/build and the broader Go suite were run with
`-tags no_rocksdb`. Deterministic gates cover new/clean/dirty/loading/writeback
quota accounting, one-page capacity, requests larger than cache, FIFO/LRU,
frozen-page reads, canceled waits, Discard, Close and sticky failures.
Real disk tests cover alignment, mmap buffers, tmpfs rejection, seeding,
plaintext/encrypted reopen and mincore. Artifact snapshot/export/readback tests
restore unflushed dirty/writeback data and reject a failure after the last disk
read. Those tests simulate CH's API; they are not actual CH/KVM E2E.

The broader suite retains one existing skip:
`TestSendU64Reply_RoundTrip` uses net.Pipe, not UnixConn, and is marked indirectly
covered by `TestParseSetMemTable_Layout`. Three packages contain no test files.
No new tests were skipped. The supplied disk environment exercised ext4, not a
real XFS mount; legacy unsupported-mode rejection has deterministic unit coverage.
Parent subsequently passed default-tag targeted and whole-repository tests,
vet, build and six-package races on immutable `d601fa0`, with `GOFLAGS` empty
and `CGO_ENABLED=1`. The previous native-check gap is closed for that revision;
trusted exact-integration CI remains a separate requirement.

The initial `REQUIRE_KVM=1` attempts of `diff_template`, `encrypted_diff`,
`snapshot` and `disks` each exited **1** at a missing task-local
`bin/cloud-hypervisor` prerequisite. Those failed attempts remain part of the
history, but **the asset blocker is superseded**: parent assembled the task
CH/runtime/kernel BIN and ran all four real CH/KVM cases on exact `d601fa0`;
each exited **0**. Evidence is in the task's `kvm-d601-results.log` and immutable
BMS `/var/tmp/diff-cow-230-kvm-lb6t8B/d601/evidence`. Default/native results are
in `native-d601-tests.log` and `native-d601-full.log` (both exit 0), with parent
also confirming six-package races. These are d601 results, not final-revision
KVM evidence. Parent will build an immutable final-head directory and rerun
after the scoped revision commit. The generic invocation remains:

```sh
export REQUIRE_KVM=1 TMPDIR=/absolute/disk-backed/task/tmp
export BIN=/absolute/task/assembled/bin
for test in diff_template encrypted_diff snapshot disks; do
  bash "test/e2e/e2e_sandbox_${test}.sh" || exit "$?"
done
```

Independent review and the normal exact-integration CI are required before
publication/merge; these local observations do not replace them.

## Revised batching measurements

The following is a new, complete sequential run on the same host, disk, 256 MiB
data pattern, request sizes and 32/16 MiB limits. B remains exact `b1db083`, d601
is exact `d601fa0`, C is the scoped revision accompanying this report, and D is a
**test-only uncached synchronous direct-I/O reference**. The common harness now
also counts actual body calls; B and d601 use isolated archived production
sources with that same harness. Every mode completed all 14 cases once. The old
measurements above remain intact; no faster sample was selected.

D uses the same active diff, direct alignment workspace and XTS encoding; first
512-byte writes still construct a complete 4 KiB page. It retains no plaintext
cache pages and performs writes synchronously. This reference is not an available
production cache mode. Its read prefill also uses DIO, so it avoids conflating
bounded-cache misses with B's naturally warm 256 MiB kernel cache. Setup remains
outside timing. B completion still means buffered acceptance, not media durability.
There is no fsync, DONTNEED or drop_caches in the measurement. All raw reported
metrics, including `/proc/self/io`, are retained in
[revision data](diff-cow-cache-revision-data.json).

The run used the task's `run-bms-check` helper, canonical disk TMPDIR `/var/q`,
`GOFLAGS` empty and `CGO_ENABLED=1`. Benchmarks use `-tags no_rocksdb` to match
the original comparison. Run the shared benchmark on each source as documented
above; for D use `-bench '^BenchmarkCOWDirectWorkingSet$'` with the same remaining
arguments. The reference implementation is in
[cow_direct_workingset_bench_test.go](../pkg/vhost/cow_direct_workingset_bench_test.go).

### Total elapsed and throughput

Each cell is **seconds through Drain / logical MiB/s**. D has no pending queue;
B and D admission and total are equal at this displayed precision.

| Workload | XTS | B | d601 | C | D |
| --- | --- | --- | --- | --- | --- |
| `seq-write` | false | 0.132 / 1939 | 1.135 / 225.5 | 1.303 / 196.5 | 1.193 / 214.5 |
| `random-4k` | false | 0.1841 / 1390 | 29.86 / 8.574 | 29.64 / 8.637 | 29.09 / 8.801 |
| `partial-512` | false | 0.2966 / 107.9 | 29.39 / 1.089 | 29.76 / 1.075 | 29.24 / 1.094 |
| `hot-overwrite` | false | 0.06408 / 3995 | 3.908 / 65.5 | 2.994 / 85.51 | 25.82 / 9.916 |
| `seq-read` | false | 0.0814 / 3145 | 13.89 / 18.43 | 1.345 / 190.3 | 1.199 / 213.6 |
| `random-read` | false | 0.1086 / 2358 | 15.86 / 16.14 | 16.26 / 15.74 | 16.2 / 15.8 |
| `multi-disk` | false | 0.1295 / 1976 | 1.052 / 243.4 | 1.057 / 242.2 | 1.005 / 254.7 |
| `seq-write` | true | 1.196 / 214 | 1.903 / 134.5 | 2.304 / 111.1 | 2.036 / 125.7 |
| `random-4k` | true | 1.228 / 208.4 | 30.94 / 8.273 | 30.65 / 8.353 | 30.5 / 8.392 |
| `partial-512` | true | 1.383 / 23.14 | 31.04 / 1.031 | 30.83 / 1.038 | 30.41 / 1.052 |
| `hot-overwrite` | true | 1.154 / 221.9 | 4.061 / 63.04 | 3.092 / 82.78 | 26.52 / 9.653 |
| `seq-read` | true | 1.176 / 217.7 | 14.75 / 17.36 | 2.241 / 114.2 | 2.109 / 121.4 |
| `random-read` | true | 1.142 / 224.2 | 16.93 / 15.12 | 17.09 / 14.98 | 17.6 / 14.55 |
| `multi-disk` | true | 1.182 / 216.7 | 2.061 / 124.2 | 2.064 / 124.1 | 2.007 / 127.5 |

### Admission and request latency

Each cell is **admission seconds / p50 µs / p99 µs**. Percentiles cover frontend
requests; total elapsed above includes the remaining Drain.

| Workload | XTS | B | d601 | C | D |
| --- | --- | --- | --- | --- | --- |
| `seq-write` | false | 0.132 / 499.8 / 679.8 | 1.083 / 3229 / 6952 | 1.23 / 4089 / 6676 | 1.193 / 3791 / 6061 |
| `random-4k` | false | 0.1841 / 2.595 / 4.918 | 28.03 / 395.2 / 1586 | 27.79 / 392.6 / 1526 | 29.09 / 389 / 1714 |
| `partial-512` | false | 0.2966 / 3.725 / 12.64 | 27.54 / 390.4 / 1545 | 27.89 / 393.5 / 1675 | 29.24 / 392.2 / 1641 |
| `hot-overwrite` | false | 0.06408 / 0.826 / 1.747 | 2.145 / 1.103 / 441.9 | 1.575 / 0.794 / 432.2 | 25.82 / 351.2 / 1092 |
| `seq-read` | false | 0.0814 / 301.2 / 420.6 | 13.89 / 5.332e+04 / 6.724e+04 | 1.345 / 4019 / 5646 | 1.199 / 3018 / 4378 |
| `random-read` | false | 0.1086 / 1.566 / 2.264 | 15.86 / 193.7 / 576.6 | 16.26 / 197.9 / 645.8 | 16.2 / 195.6 / 645.7 |
| `multi-disk` | false | 0.1295 / 496.8 / 585.7 | 0.994 / 3792 / 6981 | 0.9919 / 3982 / 5933 | 1.005 / 3805 / 6917 |
| `seq-write` | true | 1.196 / 4645 / 4942 | 1.788 / 7246 / 1.226e+04 | 2.164 / 7982 / 1.014e+04 | 2.036 / 7716 / 1.199e+04 |
| `random-4k` | true | 1.228 / 18.43 / 22.66 | 28.94 / 409.8 / 1693 | 28.72 / 410 / 1652 | 30.5 / 406.8 / 1736 |
| `partial-512` | true | 1.383 / 20.25 / 29.16 | 29.1 / 412.1 / 1788 | 28.94 / 409.9 / 1809 | 30.41 / 406.7 / 1737 |
| `hot-overwrite` | true | 1.154 / 16.84 / 27.2 | 2.191 / 0.979 / 460.8 | 1.626 / 0.74 / 440 | 26.52 / 361.7 / 1155 |
| `seq-read` | true | 1.176 / 4473 / 5663 | 14.75 / 5.683e+04 / 6.944e+04 | 2.241 / 8724 / 1.095e+04 | 2.109 / 8379 / 1.025e+04 |
| `random-read` | true | 1.142 / 17.26 / 20.98 | 16.93 / 211 / 590 | 17.09 / 211.3 / 618.1 | 17.6 / 217 / 641.1 |
| `multi-disk` | true | 1.182 / 4604 / 4789 | 1.933 / 7956 / 1.027e+04 | 1.937 / 7979 / 1.011e+04 | 2.007 / 7755 / 1.104e+04 |

### Physical body calls and bytes

Each cell is **read calls/read MiB; write calls/write MiB**, excluding setup.
These are active-body syscalls, not device durability or lower-layer amplification.

| Workload | XTS | B | d601 | C | D |
| --- | --- | --- | --- | --- | --- |
| `seq-write` | false | 0/0; 65536/256 | 0/0; 257/256 | 0/0; 256/256 | 0/0; 256/256 |
| `random-4k` | false | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 |
| `partial-512` | false | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 |
| `hot-overwrite` | false | 0/0; 65536/256 | 0/0; 9103/35.56 | 0/0; 6493/41.41 | 0/0; 65536/256 |
| `seq-read` | false | 65536/256; 0/0 | 65536/256; 0/0 | 256/256; 0/0 | 256/256; 0/0 |
| `random-read` | false | 65536/256; 0/0 | 65002/253.9; 0/0 | 65002/253.9; 0/0 | 65536/256; 0/0 |
| `multi-disk` | false | 0/0; 65536/256 | 0/0; 257/256 | 0/0; 256/256 | 0/0; 256/256 |
| `seq-write` | true | 0/0; 65536/256 | 0/0; 257/256 | 0/0; 256/256 | 0/0; 256/256 |
| `random-4k` | true | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 |
| `partial-512` | true | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 | 0/0; 65536/256 |
| `hot-overwrite` | true | 0/0; 65536/256 | 0/0; 9094/35.52 | 0/0; 6533/42.02 | 0/0; 65536/256 |
| `seq-read` | true | 65536/256; 0/0 | 65536/256; 0/0 | 256/256; 0/0 | 256/256; 0/0 |
| `random-read` | true | 65536/256; 0/0 | 65002/253.9; 0/0 | 65002/253.9; 0/0 | 65536/256; 0/0 |
| `multi-disk` | true | 0/0; 65536/256 | 0/0; 257/256 | 0/0; 256/256 | 0/0; 256/256 |

### Memory and allocation evidence

Ranges span all 14 cases. Peaks include prefill. Cache usage is a shared payload
budget, not RSS; process heap/RSS also includes metadata, fixed mappings, harness
buffers and allocator retention. D retains the normal diff/owner scaffolding but
never admits a plaintext page. `proc` columns are filesystem-accounting ranges,
not device-level completion; the JSON preserves each case. Every measured
`cancelled_write_bytes` delta was zero. No base/template, export or guest-memory
residency is included in mincore.

| Mode | mincore MiB | Peak cache / dirty MiB | End dirty MiB | HeapInuse MiB | RSS MiB | proc read / write MiB |
| --- | --- | --- | --- | --- | --- | --- |
| B | 256–256 | — | — | 3.336–5.945 | 18.65–25.33 | 0–0 / 0–256 |
| d601 | 0–0 | 32–32 / 16–16 | 0–0 | 45.67–66.08 | 71.45–94.88 | 0–256 / 0–256 |
| C | 0–0 | 32–32 / 16–16 | 0–0 | 43.9–51.88 | 65.71–80.21 | 0–256 / 0–256 |
| D | 0–0 | — | — | 4.602–8.617 | 32.29–38.02 | 0–256 / 0–256 |

The focused `BenchmarkCOWCacheHot` ran three samples per path on both sources.
The table uses median ns/op and the identical allocation counts from all three.
Idle notification now allocates a broadcast channel only when a waiter registers;
ordinary/background-context hits avoid the completion closure and unnecessary
context merge. Cancelable requests still merge cancellation with Close through
AfterFunc: they retain five allocations and were slightly slower in this sample.
No cancellation guarantee was traded for the faster uncancelable path.

| Path | d601 ns/op; B/op; allocs/op | C ns/op; B/op; allocs/op |
| --- | --- | --- |
| `context=nil` | 216.2; 16; 1 | 199.2; 0; 0 |
| `context=background` | 724.7; 256; 5 | 203.4; 0; 0 |
| `context=cancelable` | 830.3; 256; 5 | 852.3; 248; 5 |
| `signal-without-waiters` | 100.9; 112; 1 | 15.68; 0; 0 |

### Interpretation and verification

Cold sequential reads now use 256 body syscalls for 256 MiB, versus 65,536 on
d601. C measured 190.3 MiB/s plaintext and 114.2 MiB/s encrypted; D measured
213.6 and 121.4 respectively. Remaining cache/index/copy costs and storage
variation are visible. C is still substantially slower than warm buffered B.
Some sequential-write timings also worsened against this d601 sample (plaintext
1.303 versus 1.135 seconds). The randomized permutation leaves few simultaneously
dirty adjacent pages: both d601 and C still issued 65,536 writes. C random-write
and partial-write totals remained close to D's physical-I/O reference, rather
than approaching buffered B. Hot-overwrite write calls dropped from 9,103 to
6,493/6,533 (plain/XTS), with total time falling to 2.994/3.092 seconds. These are
single samples, not confidence intervals or hardware-independent parity claims.

`processChain` still dispatches each guest descriptor segment separately. A
1 MiB host ReadAt can use the new batching; a series of 4 KiB guest segments can
still require separate cold reads. This benchmark does not measure guest
sequential throughput, and this revision adds no virtqueue gather/scatter,
readahead, MQ or io_uring changes.

The revision passed default-tag `go test -json -count=1 -timeout 180s ./...`
(29 tested packages), the six-package `-race` suite, `go vet ./...` and
`go build ./...`, through the helper with `GOFLAGS` empty and `CGO_ENABLED=1`.
The cache race subset also passed ten repetitions. New deterministic tests cover
a manual aggregation deadline, nonurgent notifications, pressure/Drain progress,
no per-singleton backlog delay, out-of-order spatial merging with oldest-page
fairness, bounded syscall counts, smaller-than-run capacity, cached/dirty/
writeback/loading mixtures, overlapping readers, cancellation, base/hole
boundaries, unaligned requests and short-read reservation cleanup. Plain/XTS
adversarial-output tests verify that clean cache contents come from the owned DIO
mapping before copyout, never from mutable guest/caller output. Guest FLUSH,
quota, Close, fatal and unflushed snapshot tests remain passing. Only the existing
`TestSendU64Reply_RoundTrip` skip remains; three packages have no test files.

The four actual CH/KVM passes and parent native passes described above apply to
d601. Final-head CH/KVM and trusted exact-integration CI remain separate evidence;
parent will rerun KVM from an immutable directory after this commit. PR #231 is
Draft at d601; this revision is a local commit only, with no push or merge.
