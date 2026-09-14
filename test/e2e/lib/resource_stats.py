#!/usr/bin/env python3
"""Check native resource reads against the actual stopped VMM's cgroup."""
import json
import os
from pathlib import Path
import signal
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
    members = (cgroup / "cgroup.procs").read_text().split()
    # cgroup.procs prints task_pid_vnr: tasks without a PID in the reader's
    # namespace can appear as 0. Preserve that evidence, and verify the visible
    # VMM and owner through their actual /proc cgroup bindings.
    vmm_pids = sorted(set(pid for pid in members if int(pid) > 0))
    assert len(vmm_pids) == 1 and str(owner_pid) not in vmm_pids, members
    assert Path(f"/proc/{vmm_pids[0]}/exe").resolve().name == "cloud-hypervisor"
    def process_cgroup(pid):
        rows = Path(f"/proc/{pid}/cgroup").read_text().splitlines()
        unified = [row[3:] for row in rows if row.startswith("0::")]
        assert len(unified) == 1, rows
        return Path("/sys/fs/cgroup") / unified[0].lstrip("/")
    assert process_cgroup(vmm_pids[0]).samefile(cgroup)
    assert not process_cgroup(owner_pid).samefile(cgroup), "ctl is charged to VMM scope"
    controls = {name: (cgroup / name).read_text() for name in ("memory.high", "memory.max", "cpu.max", "cpu.weight")}
    # Stop the VMM's userspace thread group, independently of any kernel task
    # charged to its cgroup. Verify every VMM thread really stopped; a freezer
    # request alone is not evidence that the VMM cannot answer guest/CH calls.
    vmm_pid = int(vmm_pids[0])
    os.kill(vmm_pid, signal.SIGSTOP)
    try:
        deadline = time.monotonic() + 3
        while True:
            states = {task.name: next(line for line in (task / "status").read_text().splitlines()
                                     if line.startswith("State:"))
                      for task in Path(f"/proc/{vmm_pid}/task").iterdir()}
            if states and all(value.split()[1] == "T" for value in states.values()):
                break
            assert time.monotonic() < deadline, ("VMM did not stop", states)
            time.sleep(.01)
        def counters():
            memory = int((cgroup / "memory.current").read_text())
            cpu = int(dict(line.split() for line in (cgroup / "cpu.stat").read_text().splitlines())["usage_usec"])
            return memory, cpu
        begin = int(time.time())
        rows = []
        for _ in range(8):
            before = counters()
            response = request(path, "resource_stats_request")
            after = counters()
            assert response["type"] == "resource_stats_response", response
            stats = response["resource_stats"]
            assert stats["sandbox_id"] == sid
            assert stats["cpu_capacity"] == 1 and stats["cpu_allocatable"] == 1
            assert int(stats["memory_capacity"]) == 8 * 1024**3
            assert int(stats["memory_headroom"]) == int(headroom)
            # Kernel accounting/charge release is not an atomic two-file read,
            # even when userspace is stopped. Bracket each actual observation.
            for index, name in enumerate(("memory_used", "cpu_usage_usec")):
                assert min(before[index], after[index]) <= int(stats[name]) <= max(before[index], after[index]), (stats, before, after)
            assert begin <= stats["timestamp_unix"] <= int(time.time())
            rows.append(stats)
        usage = request(path, "usage_request")
        assert usage["type"] == "usage_response" and not usage["usage"]["enabled"], usage
        assert "live" not in usage["usage"], "resource stats started usage"
        assert controls == {name: (cgroup / name).read_text() for name in controls}, "read changed resource policy"
        print(json.dumps({"vmm_pid": vmm_pid, "owner_pid": int(owner_pid), "cgroup_procs": members,
                          "stopped_vmm_threads": states, "resource_reads": rows}))
    finally:
        os.kill(vmm_pid, signal.SIGCONT)


if __name__ == "__main__":
    check(*sys.argv[1:])
