#!/usr/bin/env python3
"""Run inside a disposable Guest: ROOT_HOT_FILE DATA_SCAN_FILE.

Both files must be absent. O_DIRECT bypasses the Guest page cache. The 4 MiB
hot set and 64 MiB scan exercise the host's shared root/data COW budget. The
same-page overwrite comparison uses real storage latency, without injecting a
delay or claiming durability. Latency samples and buffers belong to this Guest
test process, not to the sandboxer COW implementation.
"""

import json
import mmap
import os
import resource
import sys
import threading
import time

if len(sys.argv) != 3:
    raise SystemExit('usage: cow_guest_pressure.py ROOT_HOT_FILE DATA_SCAN_FILE')

fds = [os.open(p, os.O_CREAT | os.O_EXCL | os.O_RDWR | os.O_DIRECT, 0o600)
       for p in sys.argv[1:]]
buffer = mmap.mmap(-1, 1 << 20)
buffer[:] = b'\x6d' * len(buffer)
view = memoryview(buffer)
for fd, size in zip(fds, (4 << 20, 64 << 20)):
    for off in range(0, size, len(buffer)):
        assert os.pwritev(fd, [view], off) == len(buffer)

for work in ('hot-alone', 'hot-with-scan', 'spread-write', 'same-page-write'):
    for _ in range(2):
        for off in range(0, 4 << 20, len(buffer)):
            assert os.preadv(fds[0], [view], off) == len(buffer)
    latencies = []
    cpu_start = resource.getrusage(resource.RUSAGE_SELF)
    start = time.perf_counter_ns()
    operations = 4096
    for i in range(operations):
        if work == 'hot-with-scan' and i % 16 == 0:
            off = ((i // 16) % 64) * len(buffer)
            assert os.preadv(fds[1], [view], off) == len(buffer)
        off = 0 if work == 'same-page-write' else (i % 1024) * 4096
        before = time.perf_counter_ns()
        if work.endswith('write'):
            n = os.pwritev(fds[0], [view[:4096]], off)
        else:
            n = os.preadv(fds[0], [view[:4096]], off)
        latencies.append(time.perf_counter_ns() - before)
        assert n == 4096 and view[0] == 0x6d and view[4095] == 0x6d
    elapsed = (time.perf_counter_ns() - start) / 1e9
    cpu_end = resource.getrusage(resource.RUSAGE_SELF)
    latencies.sort()
    print(json.dumps({
        'work': work, 'operations': operations, 'hot_logical_bytes': operations * 4096,
        'scan_logical_bytes': (256 << 20) if work == 'hot-with-scan' else 0,
        'elapsed_s': elapsed, 'hot_MiB/s': operations * 4096 / (1 << 20) / (sum(latencies) / 1e9),
        'p50_us': latencies[len(latencies) // 2] / 1000,
        'p99_us': latencies[len(latencies) * 99 // 100] / 1000,
        'guest_cpu_s': cpu_end.ru_utime + cpu_end.ru_stime - cpu_start.ru_utime - cpu_start.ru_stime,
    }), flush=True)

# One fixed scanner and one hot caller use different disks/virtqueues while
# sharing the same sandbox cache. Keep only the latest 1024 hot latency samples.
for off in range(0, 4 << 20, len(buffer)):
    assert os.preadv(fds[0], [view], off) == len(buffer)
hot_buffer = mmap.mmap(-1, 4096)
hot_view = memoryview(hot_buffer)
done = threading.Event()
errors = []


def scan():
    try:
        for i in range(256):
            assert os.preadv(fds[1], [view], (i % 64) * len(buffer)) == len(buffer)
    except BaseException as err:
        errors.append(err)
    finally:
        done.set()


samples = [0] * 1024
operations = 0
cpu_start = resource.getrusage(resource.RUSAGE_SELF)
start = time.perf_counter_ns()
scanner = threading.Thread(target=scan)
scanner.start()
try:
    while not done.is_set():
        before = time.perf_counter_ns()
        assert os.preadv(fds[0], [hot_view], (operations % 1024) * 4096) == 4096
        samples[operations % len(samples)] = time.perf_counter_ns() - before
        operations += 1
finally:
    scanner.join()
if errors:
    raise errors[0]
elapsed = (time.perf_counter_ns() - start) / 1e9
cpu_end = resource.getrusage(resource.RUSAGE_SELF)
samples = sorted(samples[:min(operations, len(samples))])
assert samples and hot_view[0] == 0x6d and hot_view[-1] == 0x6d
print(json.dumps({
    'work': 'hot-concurrent-scan', 'operations': operations,
    'scan_logical_bytes': 256 << 20, 'elapsed_s': elapsed,
    'hot_MiB/s': operations * 4096 / (1 << 20) / elapsed,
    'scan_MiB/s': 256 / elapsed,
    'recent_p50_us': samples[len(samples) // 2] / 1000,
    'recent_p99_us': samples[len(samples) * 99 // 100] / 1000,
    'guest_cpu_s': cpu_end.ru_utime + cpu_end.ru_stime - cpu_start.ru_utime - cpu_start.ru_stime,
}), flush=True)
hot_view.release()
hot_buffer.close()

for fd in fds:
    os.close(fd)
view.release()
buffer.close()
