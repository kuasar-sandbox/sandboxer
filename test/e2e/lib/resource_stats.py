#!/usr/bin/env python3
"""Check native resource reads against the actual frozen VMM cgroup."""
import json
from pathlib import Path
import socket
import struct
import sys
import time


def request(path, kind):
    body = json.dumps({"type": kind}).encode()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(2)
        connection.connect(path)
        connection.sendall(struct.pack("<I", len(body)) + body)
        def read_exact(size):
            result = bytearray()
            while len(result) < size:
                chunk = connection.recv(size - len(result))
                assert chunk, "truncated native response"
                result.extend(chunk)
            return result
        size, = struct.unpack("<I", read_exact(4))
        assert 0 < size <= 65536, size
        return json.loads(read_exact(size))


def check(path, cgroup, sid, owner_pid, headroom):
    cgroup = Path(cgroup)
    vmm_pids = (cgroup / "cgroup.procs").read_text().split()
    assert len(vmm_pids) == 1 and str(owner_pid) not in vmm_pids, vmm_pids
    assert Path(f"/proc/{vmm_pids[0]}/exe").resolve().name == "cloud-hypervisor"
    controls = {name: (cgroup / name).read_text() for name in ("memory.high", "memory.max", "cpu.max", "cpu.weight")}
    # Freezing is owned by this test. Successful reads while the VMM cannot
    # answer also prove that resource stats never requires a guest/CH request.
    (cgroup / "cgroup.freeze").write_text("1")
    try:
        deadline = time.monotonic() + 3
        while "frozen 1" not in (cgroup / "cgroup.events").read_text():
            assert time.monotonic() < deadline, "VMM did not freeze"
            time.sleep(.01)
        memory = int((cgroup / "memory.current").read_text())
        cpu = int(dict(line.split() for line in (cgroup / "cpu.stat").read_text().splitlines())["usage_usec"])
        begin = int(time.time())
        rows = []
        for _ in range(8):
            response = request(path, "resource_stats_request")
            assert response["type"] == "resource_stats_response", response
            stats = response["resource_stats"]
            assert stats["sandbox_id"] == sid
            assert stats["cpu_capacity"] == 1 and stats["cpu_allocatable"] == 1
            assert int(stats["memory_capacity"]) == 8 * 1024**3
            assert int(stats["memory_headroom"]) == int(headroom)
            assert int(stats["memory_used"]) == memory, (stats, memory)
            assert int(stats["cpu_usage_usec"]) == cpu, (stats, cpu)
            assert begin <= stats["timestamp_unix"] <= int(time.time())
            rows.append(stats)
        usage = request(path, "usage_request")
        assert usage["type"] == "usage_response" and not usage["usage"]["enabled"], usage
        assert "live" not in usage["usage"], "resource stats started usage"
        assert controls == {name: (cgroup / name).read_text() for name in controls}, "read changed resource policy"
        print(json.dumps({"vmm_pid": int(vmm_pids[0]), "owner_pid": int(owner_pid), "resource_reads": rows}))
    finally:
        (cgroup / "cgroup.freeze").write_text("0")


if __name__ == "__main__":
    check(*sys.argv[1:])
