#!/usr/bin/env python3
"""Run one cold-boot vhost-blk comparison on Linux/KVM. Use a new output
directory per sample. Inputs are root.ref, root.template, data.template and
fio-runtime.tar (the fio 3.39 runtime with its private run-fio loader).
Compile fio_reverse_pfn.c as a shared library for that Guest's libc/ISA.
"""
import argparse
import collections
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import select
import signal
import struct
import subprocess
import time
import zlib

parser = argparse.ArgumentParser(description=__doc__)
for option in ("inputs", "output", "ctl", "ch", "kernel", "runtime", "pfn-lib"):
    parser.add_argument("--" + option, required=True, type=Path)
parser.add_argument("--crypto", required=True, choices=("off", "auto"))
parser.add_argument("--adjacent-only", action="store_true", help="supplemental natural writes, readback and optional large request probe")
parser.add_argument("--large-probe", type=Path, help="Guest binary built from vhost_large_read.c")
parser.add_argument("--trace", action="store_true", help="append a separate traced diagnostic")
args = parser.parse_args()
# Use the same local binaries for fingerprinting and every process launch.
args.ctl = args.ctl.resolve()
args.ch = args.ch.resolve()
inputs, out = args.inputs.resolve(), args.output.resolve()
out.mkdir(exist_ok=False, parents=True)
disk = out / "data.diff"
env = {**os.environ, "MANIFEST_KEY": secrets.token_hex(32)}
env.pop("MANIFEST_CONFIG", None)
sid = "fio"
runroot = out / "r"


def save(name, data):
    (out / name).write_text(json.dumps(data, indent=2) + "\n")


def guest(*cmd, stdin=None, timeout=600):
    invocation = [str(args.ctl), "exec", "--sandbox-id", sid, "--run-root", str(runroot)]
    if stdin is not None:
        invocation += ["--stdin-from", str(stdin)]
    result = subprocess.run(invocation + ["--", *cmd], capture_output=True,
                            env=env, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"{cmd}: exit {result.returncode}\n{result.stderr}\n{result.stdout[-2000:]}")
    return result


def proc_io():
    try:
        lines = Path(f"/proc/{vm.pid}/io").read_text().splitlines()
        return {k: int(v) for k, v in (line.split(":") for line in lines)}
    except FileNotFoundError:  # CONFIG_TASK_IO_ACCOUNTING can be disabled in WSL
        return {}


def check_disks():
    checks = []
    for path in (out / "root.diff", disk):
        with path.open("rb") as stream:
            header = stream.read(4096)
        version, prefix, flags = struct.unpack_from(">HHI", header, 8)
        if (header[:8] != b"\x89KDXTS1\n" or version != 3 or prefix != 16
                or flags != int(args.crypto == "auto")
                or zlib.crc32(header[:-4]) != struct.unpack_from("<I", header, 4092)[0]):
            raise RuntimeError(f"unexpected DIFF header {path}")
        direct = []
        for fd in Path(f"/proc/{vm.pid}/fd").iterdir():
            try:
                if os.readlink(fd) != str(path):
                    continue
                info = Path(f"/proc/{vm.pid}/fdinfo/{fd.name}").read_text()
                if int(re.search(r"flags:\s+(\d+)", info)[1], 8) & os.O_DIRECT:
                    direct.append(int(fd.name))
            except FileNotFoundError:
                continue
        if not direct:
            raise RuntimeError(f"missing direct fd for {path}")
        checks.append(dict(file=path.name, version=version, encrypted=bool(flags),
                           header_sha256=hashlib.sha256(header).hexdigest(), direct_fds=direct))
    save("diff-checks.json", checks)


def data_stat():
    # This fixture has root base, root overlay and one data disk.
    return list(map(int, guest("/bin/cat", "/sys/block/vdc/stat").stdout.split()))


results = []


def fio(name, rw, bs, qd, controlled=False, working_set="512m", amount=512 << 20,
        verify=False):
    wrapper = "run-fio-controlled" if controlled else "run-fio"
    cmd = ["/opt/fio-runtime/" + wrapper, "--filename=/data/sg.dat", "--name=" + name,
           "--thread=1", "--ioengine=libaio", "--direct=1", "--numjobs=1",
           "--fallocate=none", "--refill_buffers=1", "--output-format=json",
           "--rw=" + rw, "--bs=" + bs, "--iodepth=" + str(qd),
           "--size=" + working_set, "--io_size=" + str(amount)]
    if rw == "read":
        cmd.append("--allow_file_create=0")
    if verify:
        cmd += ["--verify=crc32c", "--verify_fatal=1", "--verify_dump=0"]
    io_before, block_before = proc_io(), data_stat()
    result = guest(*cmd)
    block_after, io_after = data_stat(), proc_io()
    (out / (name + ".stderr")).write_text(result.stderr)
    (out / (name + ".fio.json")).write_text(result.stdout)
    jobs = json.loads(result.stdout)["jobs"]
    if len(jobs) != 1 or jobs[0]["error"] or jobs[0][rw]["io_bytes"] != amount:
        raise RuntimeError(f"{name}: incomplete transfer")
    if verify and jobs[0]["read"]["io_bytes"] != amount:
        raise RuntimeError(f"{name}: incomplete CRC32C readback")
    if controlled and not re.search(r"FIO_GEOMETRY_VERIFIED .*adjacent_forward_pairs=0 ", result.stderr):
        raise RuntimeError(f"{name}: missing verified PFN layout")
    metric = jobs[0][rw]
    record = dict(name=name, cmd=cmd, MiBs=metric["bw_bytes"] / (1 << 20),
                  total_mean_us=(metric["slat_ns"]["mean"] + metric["clat_ns"]["mean"]) / 1000,
                  bytes=metric["io_bytes"], block_writes=block_after[4]-block_before[4],
                  write_sectors=block_after[6]-block_before[6], block_reads=block_after[0]-block_before[0],
                  read_sectors=block_after[2]-block_before[2],
                  process_read_bytes=(io_after["read_bytes"]-io_before["read_bytes"]
                                      if "read_bytes" in io_before else None))
    results.append(record)
    save("results.json", results)
    print(f"{name} {record['MiBs']:.2f} MiB/s", flush=True)


def diagnostic():
    # -y labels even concurrent and resumed preads with the actual file path.
    path = out / "pread.trace"
    with (out / "trace.stderr").open("w") as log:
        tracer = subprocess.Popen(["strace", "-f", "-y", "-s", "0", "-e", "trace=pread64",
                                   "-p", str(vm.pid), "-o", str(path)], stderr=log)
        try:
            deadline = time.monotonic() + 10
            while not re.search(r"TracerPid:\s+[1-9]", Path(f"/proc/{vm.pid}/status").read_text()):
                if tracer.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError("strace attach failed")
                time.sleep(.05)
            fio("trace-frag", "read", "1m", 1, True, working_set="64m", amount=64 << 20)
        finally:
            tracer.send_signal(signal.SIGINT)
            tracer.wait(timeout=15)
    save("pread-sizes.json", pread_sizes(path, disk))


def pread_sizes(trace, data_diff):
    pending = {}
    sizes = collections.Counter()
    pattern = re.compile(r'pread64\(\d+<' + re.escape(str(data_diff)) +
                         r'>, .*?, (\d+), \d+\)\s+= ([-\d]+)')
    for line in trace.read_text().splitlines():
        pid, _, call = line.strip().partition(" ")
        call = call.lstrip()
        if call.startswith("<... pread64 resumed>"):
            call = pending.pop(pid) + call.removeprefix("<... pread64 resumed>")
        if call.startswith("pread64(") and call.endswith("<unfinished ...>"):
            pending[pid] = call.removesuffix("<unfinished ...>")
        elif match := pattern.fullmatch(call):
            requested, returned = map(int, match.groups())
            if requested != returned:
                raise RuntimeError(f"incomplete DIFF pread: {call}")
            sizes[returned] += 1
        elif call.startswith("pread64(") and "<" + str(data_diff) + ">" in call:
            raise RuntimeError(f"unparsed DIFF pread: {call}")
    if not sizes or any(str(data_diff) in call for call in pending.values()):
        raise RuntimeError("incomplete data DIFF trace")
    return sizes


def suite():
    guest("/bin/sh", "-ec", "mkdir -p /opt/fio-runtime; tar xf - -C /opt/fio-runtime; "
          "if test -f /opt/fio-runtime/fio-libaio.so; then mkdir -p /usr/lib64/fio; "
          "ln -sf /opt/fio-runtime/fio-libaio.so /usr/lib64/fio/fio-libaio.so; fi",
          stdin=inputs / "fio-runtime.tar")
    guest("/bin/sh", "-ec", "cat > /opt/fio-runtime/libgeometry.so", stdin=args.pfn_lib)
    wrapper = guest("/bin/cat", "/opt/fio-runtime/run-fio").stdout
    if wrapper.count('--library-path "$D"') != 1 or wrapper.count('"$@"') != 1:
        raise RuntimeError("unknown fio runtime wrapper")
    wrapper = wrapper.replace('--library-path "$D"', '--preload "$D/libgeometry.so" --library-path "$D"')
    script = out / "wrapper.sh"
    script.write_text(wrapper.replace('"$@"', '"$@" --iomem=mmap:/tmp/fio-geometry-buffer'))
    guest("/bin/sh", "-ec", "cat > /opt/fio-runtime/run-fio-controlled; chmod +x /opt/fio-runtime/run-fio-controlled",
          stdin=script)
    state = guest("/bin/sh", "-ec", "uname -a; /opt/fio-runtime/run-fio --version; "
                  "cat /sys/block/vdc/queue/{max_segments,max_sectors_kb} 2>/dev/null || "
                  "cat /sys/block/vdc/queue/max_segments /sys/block/vdc/queue/max_sectors_kb; "
                  "echo probe > /data/probe; sync")
    (out / "guest.txt").write_text(state.stdout)
    check_disks()
    fio("prepare", "write", "1m", 32)
    fio("precondition", "read", "1m", 32)
    if not args.adjacent_only:
        for controlled, layout in [(False, "natural"), (True, "frag")]:
            for qd in (1, 32):
                fio(f"{layout}-read-{qd}", "read", "1m", qd, controlled)
        for qd in (1, 32):
            fio(f"small-{qd}", "read", "4k", qd)
        fio("large", "read", "2m", 1)
        fio("warm", "read", "1m", 1, working_set="8m", amount=8 << 20)
        fio("hot", "read", "1m", 1, working_set="8m")
    for qd in (1, 32):
        fio(f"natural-write-{qd}", "write", "1m", qd)
    for qd in (1, 32):
        fio(f"write-{qd}", "write", "1m", qd, controlled=True)
    fio("verify", "write", "1m", 1, controlled=True, verify=True)
    if args.trace:
        diagnostic()
    if args.large_probe:
        guest("/bin/sh", "-ec", "cat > /tmp/large-read; chmod +x /tmp/large-read",
              stdin=args.large_probe)
        report = guest("/bin/sh", "-c", "/tmp/large-read; rc=$?; echo exit=$rc; exit 0")
        (out / "large-probe.txt").write_text(report.stdout + report.stderr)
        if "exit=77" in report.stdout:
            print("SKIP large-chain proof: Guest split the request", flush=True)
        elif "exit=0" not in report.stdout:
            raise RuntimeError("large request was split or verification failed; see large-probe.txt")


save("config.json", dict(resources=dict(capacity=dict(cpu=2, memory="2GiB"),
     allocatable=dict(cpu=2, memory="2GiB"), diff_cow=dict(cache_size="32MiB", max_dirty_size="16MiB")),
     boot=dict(kernel=args.kernel.resolve().as_uri(), runtime=args.runtime.resolve().as_uri(),
               cmdline="console=hvc0", root=dict(base=(inputs/"root.ref").read_text().strip(),
               overlay=dict(diff=(out/"root.diff").as_uri(), diff_template=(inputs/"root.template").as_uri())),
               disks=[dict(name="data", diff=disk.as_uri(), diff_template=(inputs/"data.template").as_uri())]),
     mounts=[dict(target="/data", type="disk", source="data")], launch=dict(placeholder=True)))
save("manifest.json", {"crypto": {"local": args.crypto}})
save("binaries.json", {k: hashlib.sha256(path.read_bytes()).hexdigest() for k, path in
                      vars(args).items() if k in ("ctl", "ch", "kernel", "runtime", "pfn_lib")})
rd, wr = os.pipe()
cmd = [str(args.ctl), "run", "--config", str(out/"config.json"), "--manifest-config",
       str(out/"manifest.json"), "--sandbox-id", sid, "--ch-binary", str(args.ch),
       "--run-root", str(runroot), "--base-root", str(out/"base"), "--stats-json",
       str(out/"backend-stats.json"), "--ready-fd", str(wr)]
with (out/"sandbox.log").open("w") as log:
    vm = subprocess.Popen(cmd, env=env, pass_fds=(wr,), stdout=log, stderr=log)
os.close(wr)
try:
    ready = b""
    deadline = time.monotonic() + 90
    while b"\nready\n" not in b"\n" + ready:
        if not select.select([rd], [], [], max(0, deadline-time.monotonic()))[0]:
            raise RuntimeError("Guest readiness timeout")
        chunk = os.read(rd, 4096)
        if not chunk:
            raise RuntimeError("early Guest exit; inspect sandbox.log")
        ready += chunk
    suite()
finally:
    os.close(rd)
    vm.terminate()
    try:
        vm.wait(timeout=45)
    except subprocess.TimeoutExpired:
        vm.kill()
        vm.wait()
if vm.returncode:
    raise RuntimeError(f"sandbox exit {vm.returncode}")
