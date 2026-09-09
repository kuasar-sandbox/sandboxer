#!/usr/bin/env python3
"""Same-binary off/on CH/KVM measurements; no synthetic resource totals.

Run separately from functional E2E to avoid concurrent workloads. Measurements
include the common probe/inspection overhead. This is descriptive evidence,
not an overhead threshold or a production-density claim. Traced diagnostic
runs must not replace this untraced latency baseline.
"""
import argparse
import concurrent.futures
import json
import math
import os
from pathlib import Path
import re
import struct
import subprocess
import sys
import tempfile
import time

from usage import BIN, REPO, Sandbox, digest, ext4, image_ref, run, write_json


def percentile(values, q):
    return sorted(values)[max(0, math.ceil(len(values)*q)-1)] if values else None


def proc(pid):
    root = Path(f"/proc/{pid}")
    raw = (root / "stat").read_text()
    fields = raw[raw.rindex(")")+2:].split()
    status = dict(line.split(":", 1) for line in (root / "status").read_text().splitlines())
    switches = 0
    for task in (root / "task").iterdir():
        try:
            s = dict(line.split(":", 1) for line in (task / "status").read_text().splitlines())
            switches += int(s["voluntary_ctxt_switches"]) + int(s["nonvoluntary_ctxt_switches"])
        except FileNotFoundError:
            pass  # Exited threads' unknown switch tails are not invented.
    return {"pid": pid, "start_ticks": int(fields[19]), "cpu_ticks": int(fields[11])+int(fields[12]),
            "guest_ticks": int(fields[40]), "thread_switches_known": switches,
            "rss_anon_bytes": int(status["RssAnon"].split()[0])*1024,
            "rss_file_bytes": int(status["RssFile"].split()[0])*1024,
            "rss_bytes": int(status["VmRSS"].split()[0])*1024,
            "fds": len(list((root / "fd").iterdir())), "threads": int(status["Threads"])}


def goroutine_reader():
    # Runtime-private inspection belongs only to this test, not pkg/usage.
    # Resolve fields from the exact normal, unstripped sandbox-ctl binary.
    with (BIN / "sandbox-ctl").open("rb") as f:
        ident = f.read(20)
    assert ident[:6] == b"\x7fELF\x02\x01" and struct.unpack_from("<H", ident, 16)[0] == 2, "requires ELF64 LE ET_EXEC"
    symbols = {}
    for line in run("go", "tool", "nm", BIN / "sandbox-ctl").splitlines():
        fields = line.split()
        if len(fields) == 3:
            symbols[fields[2]] = int(fields[0], 16)
    layout = run("gdb", "-q", "-nx", "-batch", "-ex", "set auto-load off", "-ex", "set language c",
                 "-ex", "p/x (unsigned long)&(('runtime.g' *)0)->atomicstatus", BIN / "sandbox-ctl")
    offset = int(re.search(r"\$1 = (0x[0-9a-f]+)", layout)[1], 16)
    assert 0 < offset < 4096

    def read(pid):
        fd = os.open(f"/proc/{pid}/mem", os.O_RDONLY | os.O_CLOEXEC)
        try:
            for _ in range(3):
                header = os.pread(fd, 24, symbols["runtime.allgs"])
                ptr, length, capacity = struct.unpack("<QQQ", header)
                assert length <= capacity <= 65536, "unexpected runtime.g bound"
                pointers = struct.unpack(f"<{length}Q", os.pread(fd, length*8, ptr))
                live = sum((struct.unpack("<I", os.pread(fd, 4, p+offset))[0] & ~0x1000) != 6 for p in pointers)
                if header == os.pread(fd, 24, symbols["runtime.allgs"]):
                    # Includes runtime system goroutines; not runtime.NumGoroutine.
                    return live
            return None  # Concurrent array replacement is unavailable, not zero.
        finally:
            os.close(fd)
    return read, {"allgs_address": symbols["runtime.allgs"], "g_status_offset": offset,
                  "definition": "non-dead Go G, including runtime system goroutines; bounded non-atomic diagnostic"}


def record_sizes(path):
    data = path.read_bytes()
    sizes, pos = [], 0
    while pos < len(data):
        assert data[pos:pos+8] == b"KUUSAGE1"
        size = struct.unpack_from("<I", data, pos+12)[0]
        assert 32 <= size <= 512*1024 and pos+size <= len(data)
        sizes.append(size)
        pos += size
    return sizes


def cleanup(trace, sandboxes):
    # One failed teardown must not abandon the remaining owned processes or
    # hide the workload failure that brought us here.
    original = sys.exc_info()[1]
    errors = []
    if trace is not None:
        try:
            trace.close()
        except BaseException as error:
            errors.append(error)
    for sb in sandboxes:
        try:
            sb.stop()
        except BaseException as error:
            errors.append(error)
    if errors:
        if original is None:
            raise errors[0]
        for error in errors:
            print(f"usage performance cleanup: {error}", file=sys.stderr)


def measure(work, name, density, workload, enabled, seconds, root, ref, read_g, trace_enabled):
    group = work / name
    group.mkdir()
    sandboxes, tasks, rows, latency = [], [], [], []
    trace = None
    with concurrent.futures.ThreadPoolExecutor(max_workers=density) as pool:
        try:
            for ordinal in range(density):
                diff = group / f"root-{ordinal}.ext4"
                ext4(diff)
                config = {"resources": {"capacity": {"cpu": 1, "memory": "512MiB"},
                                        "allocatable": {"cpu": 1, "memory": "256MiB", "deflate_on_oom": True}},
                          "boot": {"kernel": f"file://{BIN / 'vmlinux'}", "runtime": f"file://{BIN / 'sandbox-runtime.bundle'}",
                                   "root": {"base": ref, "overlay": {"diff": f"file://{diff}"}}},
                          "launch": {"exec": "/probe", "args": ["wait"], "restart": "never", "pid_namespace": "shared"},
                          "usage": {"enabled": enabled, "sample_interval": "1s", "flush_interval": "5s" if workload == "save" else "5m"}}
                if workload == "multidisk":
                    config["boot"]["disks"], config["mounts"] = [], []
                    for disk in range(2):
                        path = group / f"data-{ordinal}-{disk}.ext4"
                        ext4(path)
                        config["boot"]["disks"].append({"name": f"d{disk}", "diff": f"file://{path}"})
                        config["mounts"].append({"type": "disk", "source": f"d{disk}", "target": f"/data{disk}"})
                sb = Sandbox(group, f"s{ordinal}", config)
                sandboxes.append(sb)
                sb.ready()
                guest = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
                assert guest["sandbox_init_sha256"] == digest(BIN / "sandbox-init")
                write_json(sb.dir / "guest.json", guest)
            time.sleep(3)
            hosts = [s.process.pid for s in sandboxes]
            chs = []
            for sb in sandboxes:
                matches = re.findall(r"CH started pid=(\d+)", (sb.dir / "run.log").read_text())
                assert len(matches) == 1
                chs.append(int(matches[0]))
            if trace_enabled:
                from usage_trace import Trace
                trace = Trace(group, hosts, chs)
            if workload in ("cpu", "memory", "multidisk"):
                for sb in sandboxes:
                    args = {"cpu": ["cpu", str(seconds+2)], "memory": ["memory", "320", str(seconds+2)],
                            "multidisk": ["write", "/data0/data", "32"]}[workload]
                    tasks.append(pool.submit(sb.cli, "exec", "--", "/probe", *args, timeout=seconds+30))
            start = time.monotonic_ns()
            before = [proc(p) for p in hosts+chs]
            while (time.monotonic_ns()-start)/1e9 < seconds:
                for hp, cp in zip(hosts, chs):
                    h, c = proc(hp), proc(cp)
                    h["live_go_g"] = read_g(hp)
                    rows.append({"at_ns": time.monotonic_ns()-start, "host": h, "ch": c})
                # CLI latency is real end-to-end host CLI + Guest exec, not an
                # in-process RPC approximation. The same probe runs off/on.
                for sb in sandboxes:
                    at = time.monotonic_ns()
                    sb.cli("exec", "--", "/probe", "true")
                    latency.append(time.monotonic_ns()-at)
                time.sleep(.2)
            after = [proc(p) for p in hosts+chs]
            elapsed = time.monotonic_ns()-start
            for task in tasks:
                task.result()
            for sb in sandboxes:
                write_json(sb.dir / "live.json", sb.view())
            stop_at = time.monotonic_ns()
            list(pool.map(lambda sb: sb.stop(), sandboxes))
            stop_ns = time.monotonic_ns()-stop_at
            if trace is not None:
                trace.close()
                trace = None
                trace_output = (group / "trace.log").read_text()
                assert '"@mallocgc_calls"' in trace_output and '"@wakeups"' in trace_output, "required trace measurements absent"
                if enabled:
                    assert '"@usage_requests"' in trace_output and '"@saves"' in trace_output, "usage trace did not observe requests and final save"
                else:
                    assert '"@usage_requests"' not in trace_output and '"@saves"' not in trace_output, "disabled usage performed periodic work"
            hz = os.sysconf("SC_CLK_TCK")  # Harness only; product uses AT_CLKTCK.
            deltas = []
            for b, a in zip(before, after):
                assert b["pid"] == a["pid"] and b["start_ticks"] == a["start_ticks"]
                deltas.append({"pid": a["pid"], "cpu_ns": (a["cpu_ticks"]-b["cpu_ticks"])*1_000_000_000//hz,
                               "guest_ns": (a["guest_ticks"]-b["guest_ticks"])*1_000_000_000//hz})
            sizes = []
            for sb in sandboxes:
                if enabled:
                    view = sb.view()  # Offline CRC/identity validation, after close.
                    write_json(sb.dir / "saved.json", view)
                    assert view["saved"]["snapshot"]["closed"]
                    sizes += record_sizes(sb.baseroot / "instance" / f"{sb.name}.usage")
            result = {"case": name, "density": density, "workload": workload, "enabled": enabled,
                      "elapsed_ns": elapsed, "cpu": deltas, "host_pids": hosts, "ch_pids": chs,
                      "business_samples": len(latency), "business_p95_ns": percentile(latency, .95),
                      "business_p99_ns": percentile(latency, .99), "concentrated_stop_ns": stop_ns,
                      "record_bytes": sizes, "measurements": rows, "business_latency_ns": latency}
            write_json(group / "result.json", result)
            return {k: v for k, v in result.items() if k not in ("measurements", "business_latency_ns")}
        finally:
            cleanup(trace, sandboxes)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--densities", default="1,4")
    parser.add_argument("--workloads", default="idle,cpu,memory,multidisk,save,stop")
    parser.add_argument("--seconds", type=int, default=30)
    parser.add_argument("--repeat", type=int, default=3)
    parser.add_argument("--trace", action="store_true", help="separate perturbed bpftrace diagnostics, not baseline latency")
    args = parser.parse_args()
    assert os.geteuid() == 0 and args.seconds >= 10 and args.repeat >= 1
    densities = [int(x) for x in args.densities.split(",")]
    assert all(1 <= d <= 16 for d in densities)
    workloads = args.workloads.split(",")
    assert set(workloads) <= {"idle", "cpu", "memory", "multidisk", "save", "stop"}
    # Do not risk host OOM for a benchmark. Increase density only in a suitably
    # provisioned, explicitly selected environment; no host limit changes.
    available = int(re.search(r"MemAvailable:\s+(\d+)", Path("/proc/meminfo").read_text())[1])*1024
    assert max(densities)*512*1024*1024 < available//2, "insufficient safe memory for selected density"
    work = Path(tempfile.mkdtemp(prefix="perf-usage-"))
    print(f"usage performance evidence: {work}", flush=True)
    read_g, layout = goroutine_reader()
    metadata = {"completed": False, "arguments": vars(args), "host_kernel": run("uname", "-a"), "lscpu": run("lscpu"),
                "go_runtime_layout": layout, "ch": run(BIN / "cloud-hypervisor", "--version"),
                "build": run("go", "version", "-m", BIN / "sandbox-ctl"),
                "artifacts": {n: digest(BIN / n) for n in ("sandbox-ctl", "sandbox-init", "sandbox-runtime.bundle", "vmlinux", "cloud-hypervisor")},
                "measurement": "descriptive local CH/KVM; common .2s resource/exec probes; trace flag identifies perturbed diagnostics; no production-density inference",
                "not_measured_here": [] if args.trace else ["wakeups", "allocations", "management traffic", "CH API calls/lock wait", "per-save duration"]}
    metadata["harness_sha256"] = {p.name: digest(p) for p in Path(__file__).parent.glob("usage*.py")}
    metadata["harness_sha256"]["usageprobe/main.go"] = digest(Path(__file__).parent / "usageprobe/main.go")
    if (REPO / ".git").exists():
        metadata["source_revisions"] = {name: run("git", "rev-parse", "HEAD", cwd=REPO.parent / name).strip()
                                        for name in ("sandboxer", "accelerator", "connector", "guest-runtime")
                                        if (REPO.parent / name / ".git").exists()}
    write_json(work / "source-set.json", metadata)
    root = work / "rootfs"
    root.mkdir()
    for path in ("tmp", "proc", "sys", "dev", "data0", "data1"):
        (root / path).mkdir()
    run("go", "build", "-trimpath", "-o", root / "probe", Path(__file__).parent / "usageprobe/main.go",
        env={**os.environ, "CGO_ENABLED": "0", "GOWORK": "off"})
    image = work / "root.img"
    run(BIN / "flatten-ctl", "export", "--output", image, "--no-progress", root)
    ref, results = image_ref(image), []
    for repeat in range(args.repeat):
        for density in densities:
            for workload in workloads:
                # Alternate order between repeats to expose warm/order bias.
                for enabled in ((False, True) if repeat % 2 == 0 else (True, False)):
                    name = f"r{repeat}-n{density}-{workload}-{'on' if enabled else 'off'}"
                    results.append(measure(work, name, density, workload, enabled, args.seconds, root, ref, read_g, args.trace))
                    write_json(work / "results.json", results)
                    print(f"PASS measured {name}", flush=True)
    metadata["completed"] = True
    write_json(work / "source-set.json", metadata)


if __name__ == "__main__":
    main()
