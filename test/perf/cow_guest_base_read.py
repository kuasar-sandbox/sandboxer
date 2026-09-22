#!/usr/bin/env python3
"""Read existing files backed by immutable COW layers inside a disposable Guest.

Create the files before capturing the disk layers, then boot with those layers
as COW bases. O_DIRECT bypasses the Guest page cache; immutable host caching
remains enabled. The host harness must verify this disk topology separately.
Before capturing, run --prepare-checksums OUTPUT on the original fixtures.
Supply that trusted manifest with --checksums INPUT when measuring the restored
files in the same order. Checksums cover every byte of warm-up and measured
reads; never derive the expected checksums from the base path under test.
Checksum verification is excluded from elapsed time and syscall latency.
The reported CPU total includes checksum verification.
"""

import argparse
import json
import mmap
import os
from pathlib import Path
import time
import zlib

parser = argparse.ArgumentParser(description=__doc__)
mode = parser.add_mutually_exclusive_group(required=True)
mode.add_argument('--prepare-checksums', metavar='OUTPUT')
mode.add_argument('--checksums', metavar='INPUT')
parser.add_argument('paths', nargs='+', metavar='FILE')
args = parser.parse_args()
expected = json.loads(Path(args.checksums).read_text()) if args.checksums else []
if args.checksums and len(expected) != len(args.paths):
    raise ValueError('checksum manifest must match the fixture count and order')

workspace = mmap.mmap(-1, 1 << 20)
view = memoryview(workspace)
try:
    for file_index, path in enumerate(args.paths):
        fd = os.open(path, os.O_RDONLY | os.O_DIRECT)
        try:
            size = min(os.fstat(fd).st_size, 64 << 20)
            if size < 1 << 20 or size % (1 << 20):
                raise ValueError("fixture must contain a whole number of MiB")
            if args.prepare_checksums:
                entry = {'size': size, 'crc32_4k': [], 'crc32_1m': []}
                for off in range(0, size, len(view)):
                    if os.preadv(fd, [view], off) != len(view):
                        raise IOError('short fixture read')
                    entry['crc32_1m'].append(zlib.crc32(view))
                    entry['crc32_4k'].extend(zlib.crc32(view[i:i + 4096])
                                             for i in range(0, len(view), 4096))
                expected.append(entry)
                continue
            entry = expected[file_index]
            if (entry['size'] != size or len(entry['crc32_4k']) != size // 4096
                    or len(entry['crc32_1m']) != size // (1 << 20)):
                raise ValueError('checksum manifest does not match fixture geometry')
            for off in range(0, size, 1 << 20):
                if os.preadv(fd, [view], off) != len(view):
                    raise IOError("short warm-up read")
                if zlib.crc32(view) != entry['crc32_1m'][off // (1 << 20)]:
                    raise IOError('warm-up data verification failed')
            for repeat in range(3):
                for block in (1 << 20, 4096):
                    samples = []
                    count = size // block
                    output = view[:block]
                    checksums = entry['crc32_4k' if block == 4096 else 'crc32_1m']
                    verification_ns = 0
                    cpu = time.process_time_ns()
                    start = time.perf_counter_ns()
                    for index in range(count):
                        # Odd stride permutes the power-of-two 64 MiB fixture.
                        off = (index if block != 4096 else (index * 8191) % count) * block
                        before = time.perf_counter_ns()
                        n = os.preadv(fd, [output], off)
                        samples.append(time.perf_counter_ns() - before)
                        if n != block:
                            output.release()
                            raise IOError("short measured read")
                        verify_start = time.perf_counter_ns()
                        if zlib.crc32(output) != checksums[off // block]:
                            output.release()
                            raise IOError('measured data verification failed')
                        verification_ns += time.perf_counter_ns() - verify_start
                    elapsed = time.perf_counter_ns() - start - verification_ns
                    cpu = time.process_time_ns() - cpu
                    samples.sort()
                    print(json.dumps({"path": path, "repeat": repeat, "block": block,
                                      "logical_bytes": size, "MiB_s": size / (1 << 20) * 1e9 / elapsed,
                                      "p50_us": samples[len(samples) // 2] / 1000,
                                      "p99_us": samples[min(len(samples) - 1, len(samples) * 99 // 100)] / 1000,
                                      "verification_s": verification_ns / 1e9,
                                      "cpu_s_including_verification": cpu / 1e9}), flush=True)
                    output.release()
        finally:
            os.close(fd)
finally:
    view.release()
    workspace.close()
if args.prepare_checksums:
    Path(args.prepare_checksums).write_text(json.dumps(expected) + '\n')
