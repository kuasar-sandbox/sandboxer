#!/usr/bin/env python3
"""Read existing files backed by immutable COW layers inside a disposable Guest.

Create the files before capturing the disk layers, then boot with those layers
as COW bases. O_DIRECT bypasses the Guest page cache; immutable host caching
remains enabled. The host harness must verify this disk topology separately.
"""

import json
import mmap
import os
import sys
import time

if len(sys.argv) < 2:
    raise SystemExit("usage: cow_guest_base_read.py FILE [FILE ...]")

workspace = mmap.mmap(-1, 1 << 20)
view = memoryview(workspace)
try:
    for path in sys.argv[1:]:
        fd = os.open(path, os.O_RDONLY | os.O_DIRECT)
        try:
            size = min(os.fstat(fd).st_size, 64 << 20)
            if size < 1 << 20 or size % (1 << 20):
                raise ValueError("fixture must contain a whole number of MiB")
            for off in range(0, size, 1 << 20):
                if os.preadv(fd, [view], off) != len(view):
                    raise IOError("short warm-up read")
            for repeat in range(3):
                for block in (1 << 20, 4096):
                    samples = []
                    count = size // block
                    output = view[:block]
                    cpu = time.process_time_ns()
                    start = time.perf_counter_ns()
                    for index in range(count):
                        # Odd stride permutes the power-of-two 64 MiB fixture.
                        off = (index if block != 4096 else (index * 8191) % count) * block
                        before = time.perf_counter_ns()
                        n = os.preadv(fd, [output], off)
                        samples.append(time.perf_counter_ns() - before)
                        if n != block:
                            raise IOError("short measured read")
                    elapsed = time.perf_counter_ns() - start
                    cpu = time.process_time_ns() - cpu
                    samples.sort()
                    print(json.dumps({"path": path, "repeat": repeat, "block": block,
                                      "logical_bytes": size, "MiB_s": size / (1 << 20) * 1e9 / elapsed,
                                      "p50_us": samples[len(samples) // 2] / 1000,
                                      "p99_us": samples[min(len(samples) - 1, len(samples) * 99 // 100)] / 1000,
                                      "cpu_s": cpu / 1e9}), flush=True)
                    output.release()
        finally:
            os.close(fd)
finally:
    view.release()
    workspace.close()
