#!/usr/bin/env python3
"""Guest-side FITRIM probe; runs from the prepared case without building tools."""
import fcntl
import json
import mmap
import os
from pathlib import Path
import struct
import subprocess
import sys

PAGE = 4096
PATH = Path('/scratch/trim-probe')
POSITION = Path('/trim-position.json')


def raw_page(position):
    # A direct block-device read avoids proving only a filesystem cache hit.
    fd = os.open(position['device'], os.O_RDONLY | os.O_DIRECT)
    try:
        with mmap.mmap(-1, PAGE) as buf:
            assert os.preadv(fd, [buf], position['offset']) == PAGE
            return bytes(buf)
    finally:
        os.close(fd)


def run():
    mode, = sys.argv[1:]
    if mode == 'seed':
        with PATH.open('wb') as f:
            for _ in range(1024):
                f.write(b'\x5c' * PAGE)
            f.flush()
            os.fsync(f.fileno())
            # FIEMAP header and one extent; FIEMAP_FLAG_SYNC.
            data = bytearray(struct.pack('QQIIII', 0, 4 << 20, 1, 0, 1, 0) + bytes(56))
            fcntl.ioctl(f.fileno(), 0xC020660B, data, True)
        _, _, _, count, _, _ = struct.unpack_from('QQIIII', data)
        assert count == 1
        logical, physical, length, _, _, flags, _, _, _ = struct.unpack_from('QQQQQIIII', data, 32)
        assert logical == 0 and physical % PAGE == 0 and length >= PAGE
        assert flags & (0x2 | 0x4 | 0x800) == 0, flags
        device = next(line.split()[0] for line in Path('/proc/mounts').read_text().splitlines()
                      if line.split()[1] == '/scratch')
        position = {'device': device, 'offset': physical, 'length': PAGE}
        POSITION.write_text(json.dumps(position))
        os.sync()
        assert raw_page(position) == b'\x5c' * PAGE
    else:
        position = json.loads(POSITION.read_text())
        if mode in ('trim', 'manual-trim'):
            assert raw_page(position) == b'\x5c' * PAGE, 'nonzero lower payload was not retained'
            PATH.unlink()
            os.sync()
            if mode == 'manual-trim':
                assert raw_page(position) == b'\x5c' * PAGE, 'manual mount discarded on unlink'
            subprocess.run(['fstrim', '-v', '/scratch'], check=True)
        else:
            assert mode == 'check'
        assert not PATH.exists()
        assert raw_page(position) == bytes(PAGE), 'discard exposed lower data'
    print(json.dumps(position, sort_keys=True))


if __name__ == '__main__':
    run()
