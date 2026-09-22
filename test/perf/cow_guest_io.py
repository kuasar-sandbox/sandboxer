#!/usr/bin/env python3
"""Guest-only diagnostic workload for a disposable sandbox with two writable disks.

Run through sandbox-ctl exec using the same root/data mount configuration for
both revisions. Files must not already exist; this script deliberately leaves
its 64 MiB-per-disk fixtures in the disposable sandbox. O_DIRECT bypasses the
Guest page cache. Completed writes retain the project's non-durable semantics;
this harness neither calls host Drain nor claims persistence.

Buffers, latency samples, and Python overhead belong to the Guest benchmark,
not the production COW cache. Compare the same harness and operation counts.
Use --mode baseline (the default) for the ten-workload run, or --mode sg for
the separate four-workload comparison. Start a fresh sandbox for each run.
Write verification and its bounded expected-data metadata are outside timing;
request stamps are prepared before each measured syscall.
Full-range read checksums are prepared from the fixture oracle before timing.
Read checksum validation is excluded from elapsed time and latency, but included
in the explicitly labeled Guest CPU total. Initial pages have distinct headers
so reordering middle SG vectors is observable.
"""

import argparse
import json
import mmap
import os
import resource
import struct
import time
import zlib

size = 64 << 20
page = 4096
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--mode', choices=('baseline', 'sg'), default='baseline')
parser.add_argument('paths', nargs=2, metavar='FILE')
args = parser.parse_args()
paths = args.paths
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
works = [('seq-read', 1 << 20), ('seq-write', 1 << 20),
         ('random-read', page), ('random-write', page),
         ('partial-512', 512), ('hot-overwrite', page),
         ('cache-hot', page), ('root-seq-read', 1 << 20),
         ('root-seq-write', 1 << 20), ('multi-disk', 1 << 20)]
if args.mode == 'sg':
    works = [('seq-read', 1 << 20), ('sg-seq-read', 1 << 20),
             ('seq-write', 1 << 20), ('sg-seq-write', 1 << 20)]

# One byte per 512 B sector plus the last request headers, bounded by the fixed
# fixture size. This oracle describes expected contents without another payload
# copy of either file. It is updated and fully checked outside timed writes.
markers = [bytearray([0x6d]) * (size // 512) for _ in fds]
headers = {}


def target(work, request, i):
    disk, off = 1, i * request
    if work.startswith('root-'):
        disk = 0
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
        disk, off = i % 2, (i // 2) * request
    return disk, off


def stamp(vector, disk, off, i):
    struct.pack_into('<QQ', vector, 0, disk, off | (i << 32))


for disk, fd in enumerate(fds):
    for off in range(0, size, len(buffer)):
        for block in range(0, len(buffer), page):
            stamp(view[block:], disk, off + block, 0)
            headers[disk, off + block] = bytes(view[block:block + 16])
        assert os.pwritev(fd, [view], off) == len(buffer)


def expected_checksum(disk, off, length):
    # Reuse the existing contiguous workspace before the read overwrites it.
    for relative in range(0, length, 512):
        sector = off + relative
        view[relative:relative + 512] = bytes([markers[disk][sector // 512]]) * 512
        header = headers.get((disk, sector))
        if header is not None:
            view[relative:relative + 16] = header
    return zlib.crc32(view[:length])


for generation, (work, request) in enumerate(works):
    reads = work.endswith('read') or work == 'cache-hot'
    operations = size // (request if request > page else page)
    if work == 'multi-disk':
        operations *= 2
    if work == 'cache-hot':
        for off in range(0, 4 << 20, len(buffer)):
            expected = expected_checksum(1, off, len(buffer))
            assert os.preadv(fds[1], [view], off) == len(buffer)
            assert zlib.crc32(view) == expected, ('warm-up verification', work, off)
    checksums = {}
    if reads:
        for i in range(operations):
            disk, off = target(work, request, i)
            if (disk, off) not in checksums:
                checksums[disk, off] = expected_checksum(disk, off, request)
    marker = 0x80 + generation
    if not reads:
        buffer[:] = bytes([marker]) * len(buffer)
        sg_buffer[:] = bytes([marker]) * len(sg_buffer)
    vectors = sg_vectors if work.startswith('sg-') else [view[:request]]
    latencies = []
    verification_ns = 0
    before_cpu = resource.getrusage(resource.RUSAGE_SELF)
    start = time.perf_counter_ns()
    for i in range(operations):
        disk, off = target(work, request, i)
        fd = fds[disk]
        if not reads:
            stamp(vectors[0], disk, off, i)
        before = time.perf_counter_ns()
        if reads:
            n = os.preadv(fd, vectors, off)
        else:
            n = os.pwritev(fd, vectors, off)
        latencies.append(time.perf_counter_ns() - before)
        assert n == request, (work, i, n, request)
        if reads:
            verify_start = time.perf_counter_ns()
            checksum = 0
            for vector in vectors:
                checksum = zlib.crc32(vector, checksum)
            assert checksum == checksums[disk, off], ('read verification', work, disk, off)
            del vector
            verification_ns += time.perf_counter_ns() - verify_start
    elapsed = (time.perf_counter_ns() - start - verification_ns) / 1e9
    after_cpu = resource.getrusage(resource.RUSAGE_SELF)
    verified = 0
    if not reads:
        # Within each workload ranges are disjoint or exact overwrites. Check
        # the latest contents of every written range, including both disks.
        latest = {target(work, request, i): i for i in range(operations)}
        buffer[:] = bytes([marker]) * len(buffer)
        for (disk, off), i in latest.items():
            stamp(view, disk, off, i)
            assert os.preadv(fds[disk], [sg_view[:request]], off) == request
            assert sg_view[:request] == view[:request], ('write verification', work, disk, off)
            for sector in range(off // 512, (off + request) // 512):
                markers[disk][sector] = marker
                headers.pop((disk, sector * 512), None)
            headers[disk, off] = bytes(view[:16])
        verified = len(latest)
    latencies.sort()
    print(json.dumps({'mode': args.mode, 'work': work, 'operations': operations,
                      'verified_write_ranges': verified,
                      'logical_bytes': operations * request,
                      'request_bytes': request, 'elapsed_s': elapsed,
                      'read_verification_s': verification_ns / 1e9,
                      'guest_cpu_includes_read_verification': reads,
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
