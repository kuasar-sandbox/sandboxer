[English](diff-cow-cache-validation.md) | [简体中文](diff-cow-cache-validation_zh.md)

# Issue #230 validation observations

The [cache/direct I/O contract](diff-cow-cache.md) bounds active-diff memory but
changes I/O completion and performance. The measurements below show lower file
page-cache residency and substantial throughput regressions against the buffered
baseline on this storage. They are observations, not performance targets or a
claim that KVM E2E passed.

## Environment and method

Measured on 2026-09-17, Linux/amd64, Intel Xeon Gold 6266C (88 logical CPUs visible),
Go 1.26.5, kernel `6.6.0-159.4.6.157.20260713.a4e2472763b2.oe2403sp4.x86_64`.
Sources were isolated under `/var/tmp/diff-cow-230-lb6t8B`; the storage was ext4
on `/dev/sdb2` with 4 KiB filesystem blocks. `/tmp` is tmpfs. Go tests used a
short, canonical, task-owned disk directory `/var/q` for TMPDIR because some
existing Unix-socket fixtures exceed the path limit with a longer TMPDIR and
other existing tests compare canonical paths. No test was weakened for this.

B is exact main `b1db083dee2cacac141d4033bbf41b518cf80c9e`, with only the common
benchmark harness and a baseline adapter added. C is this issue's implementation
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

## Elapsed time, throughput and request latency

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

## Traffic and memory evidence

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
RocksDB/native variants and trusted exact-integration CI remain separate checks.

`REQUIRE_KVM=1` attempts of `diff_template`, `encrypted_diff`, `snapshot` and
`disks` each exited **1** at the missing task-local `bin/cloud-hypervisor`
prerequisite. `/dev/kvm` exists, but the task has no assembled CH/runtime/kernel
BIN; cargo/rustc are also absent from the BMS command PATH. Nothing was taken
from another running worktree or installed globally. This is a validation
blocker, not four passing or silently skipped E2Es. After provisioning a trusted
BIN matching the repository's patched VMM/runtime requirements, run:

```sh
export REQUIRE_KVM=1 TMPDIR=/absolute/disk-backed/task/tmp
export BIN=/absolute/task/assembled/bin
for test in diff_template encrypted_diff snapshot disks; do
  bash "test/e2e/e2e_sandbox_${test}.sh" || exit "$?"
done
```

Independent review and the normal exact-integration CI are required before
publication/merge; these local observations do not replace them.
