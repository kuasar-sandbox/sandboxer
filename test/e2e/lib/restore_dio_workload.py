"""Snapshot integrity workload: real O_DIRECT subpage DMA with cold/warm controls."""
import ctypes
import json
import mmap
import os
import time

libc = ctypes.CDLL(None, use_errno=True)
libc.pread.argtypes = [ctypes.c_int, ctypes.c_void_p, ctypes.c_size_t, ctypes.c_longlong]
libc.pread.restype = ctypes.c_ssize_t
# Keep the file on the writable root overlay, not the guest's /tmp tmpfs.
path = os.path.join(os.path.expanduser('~'), 'dio-source.bin')
with open(path, 'wb', buffering=0) as output:
    output.write(b'Z' * 4096)
    os.fsync(output.fileno())
fd = os.open(path, os.O_RDONLY | os.O_DIRECT)
# Spacing keeps unrelated interpreter, control and ring traffic away from the
# target pages. A valid guest O_DIRECT pread must preserve the remaining bytes.
regions = []
for _ in range(32):
    region = mmap.mmap(-1, 1 << 20, flags=mmap.MAP_PRIVATE | mmap.MAP_ANONYMOUS)
    region[:] = b'\xa5' * len(region)
    address = ctypes.addressof(ctypes.c_char.from_buffer(region)) + (512 << 10)
    regions.append((region, address))

def direct_read(address):
    result = libc.pread(fd, ctypes.c_void_p(address + 512), 512, 0)
    if result != 512:
        raise RuntimeError('O_DIRECT positive control unsupported: result=%d errno=%d' % (result, ctypes.get_errno()))
    page = ctypes.string_at(address, 4096)
    expected = b'\xa5' * 512 + b'Z' * 512 + b'\xa5' * 3072
    return sum(a != b for a, b in zip(page, expected))

def state(value):
    with open('/tmp/dio-state.json.new', 'w') as output:
        json.dump(value, output)
    os.replace('/tmp/dio-state.json.new', '/tmp/dio-state.json')

cold = direct_read(regions[0][1])
if cold:
    raise RuntimeError('cold direct read corrupted %d bytes' % cold)
for region, address in regions:
    ctypes.memset(address, 0xa5, 4096)
state({'phase': 'armed', 'cold_control_mismatches': cold, 'regions': len(regions)})
print('DIO-ARMED', flush=True)
i = 0
while not os.path.exists('/tmp/dio-trigger'):
    print('TICK', i, flush=True)
    i += 1
    time.sleep(.25)
# The first sixteen pages receive no userspace touch before DMA. The other
# sixteen are explicitly CPU-warmed after restore as a positive control.
# Guest pinning/prefetch can warm a candidate; deterministic backend-first
# attribution belongs to the real MISSING_SHMEM used-ring regression.
results = [direct_read(address) for region, address in regions[:16]]
for region, address in regions[16:]:
    if ctypes.string_at(address, 4096) != b'\xa5' * 4096:
        raise RuntimeError('restored CPU-warmed control lost saved contents')
warm = [direct_read(address) for region, address in regions[16:]]
state({'phase': 'complete', 'cold_control_mismatches': cold,
       'mismatches': results, 'warm_mismatches': warm,
       'corrupt_regions': sum(bool(v) for v in results + warm)})
if any(results + warm):
    raise RuntimeError('O_DIRECT restore corrupted untouched bytes: %r / %r' % (results, warm))
print('DIO-COMPLETE', flush=True)
while True:
    print('TICK', i, flush=True)
    i += 1
    time.sleep(.25)
