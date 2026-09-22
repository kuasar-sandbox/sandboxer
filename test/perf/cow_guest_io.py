#!/usr/bin/env python3
"""Guest-only diagnostic workload for a disposable sandbox with two writable disks.

Run through sandbox-ctl exec using the same root/data mount configuration for
both revisions. Files must not already exist; this script deliberately leaves
its 64 MiB-per-disk fixtures in the disposable sandbox. O_DIRECT bypasses the
Guest page cache. Completed writes retain the project's non-durable semantics;
this harness neither calls host Drain nor claims persistence.

Buffers, latency samples, and Python overhead belong to the Guest benchmark,
not the production COW cache. Compare the same harness and operation counts.
"""

import json
import mmap
import os
import resource
import sys
import time

size = 64 << 20
page = 4096
if len(sys.argv) != 3:
    raise SystemExit('usage: python3 cow_guest_io.py ROOT_FILE DATA_FILE')
paths = sys.argv[1:]
fds = [os.open(p, os.O_CREAT | os.O_EXCL | os.O_RDWR | os.O_DIRECT, 0o600) for p in paths]
buffer = mmap.mmap(-1, 1 << 20)
buffer[:] = b'\x6d' * len(buffer)
view = memoryview(buffer)
sg_buffer = mmap.mmap(-1, 2 << 20)
sg_buffer[:] = b'\x6d' * len(sg_buffer)
sg_view = memoryview(sg_buffer)
# Touch intervening pages too, then use every other page. This makes SG an
# explicit Guest workload instead of assuming contiguous virtual memory is SG.
sg_vectors = [sg_view[i * 8192:i * 8192 + page] for i in range(256)]
for fd in fds:
    for off in range(0, size, len(buffer)):
        assert os.pwritev(fd, [view], off) == len(buffer)

works = [('seq-read', 1 << 20), ('sg-seq-read', 1 << 20),
         ('seq-write', 1 << 20), ('sg-seq-write', 1 << 20),
         ('random-read', page), ('random-write', page),
         ('partial-512', 512), ('hot-overwrite', page),
         ('cache-hot', page), ('root-seq-read', 1 << 20),
         ('root-seq-write', 1 << 20), ('multi-disk', 1 << 20)]
for work, request in works:
    operations = size // (request if request > page else page)
    if work == 'multi-disk':
        operations *= 2
    if work == 'cache-hot':
        for off in range(0, 4 << 20, len(buffer)):
            assert os.preadv(fds[1], [view], off) == len(buffer)
    latencies = []
    before_cpu = resource.getrusage(resource.RUSAGE_SELF)
    start = time.perf_counter_ns()
    for i in range(operations):
        fd = fds[1]
        off = i * request
        if work.startswith('root-'):
            fd = fds[0]
        if work in ('random-read', 'random-write', 'partial-512', 'hot-overwrite'):
            block = (i * 40503) % (size // page)
            if work == 'hot-overwrite' and i % 10:
                block %= 1024
            off = block * page
        if work == 'partial-512':
            off += 512
        if work == 'cache-hot':
            off = (i % 1024) * page
        if work == 'multi-disk':
            fd = fds[i % 2]
            off = (i // 2) * request
        before = time.perf_counter_ns()
        vectors = sg_vectors if work.startswith('sg-') else [view[:request]]
        if work.endswith('read') or work == 'cache-hot':
            n = os.preadv(fd, vectors, off)
        else:
            n = os.pwritev(fd, vectors, off)
        latencies.append(time.perf_counter_ns() - before)
        assert n == request, (work, i, n, request)
        assert vectors[0][0] == 0x6d and vectors[-1][-1] == 0x6d
    elapsed = (time.perf_counter_ns() - start) / 1e9
    after_cpu = resource.getrusage(resource.RUSAGE_SELF)
    latencies.sort()
    print(json.dumps({'work': work, 'operations': operations,
                      'logical_bytes': operations * request,
                      'request_bytes': request, 'elapsed_s': elapsed,
                      'MiB/s': operations * request / (1 << 20) / elapsed,
                      'p50_us': latencies[len(latencies)//2] / 1000,
                      'p99_us': latencies[len(latencies)*99//100] / 1000,
                      'guest_cpu_s': after_cpu.ru_utime + after_cpu.ru_stime
                          - before_cpu.ru_utime - before_cpu.ru_stime}), flush=True)
for fd in fds:
    os.close(fd)
del vectors
view.release()
buffer.close()
for vector in sg_vectors:
    vector.release()
sg_view.release()
sg_buffer.close()
