#!/usr/bin/env python3
"""Faults affect only disposable test VMs and their usage files.

ENOSPC is real bounded tmpfs exhaustion. Sync failure and blocked writes are
explicit syscall injection, not hardware failure or a power-loss experiment.
"""
import argparse
import atexit
import errno
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time

from usage import BIN, Sandbox, digest, ext4, image_ref, kill_host_and_stop_ch, metric, run, write_json


def inject(sb, syscall, action):
    path = sb.baseroot / "instance" / f"{sb.name}.usage"
    log = (sb.dir / "injection.log").open("w")
    tracer = None
    try:
        tracer = subprocess.Popen(["strace", "-f", "-yy", "-ttt", "-T", "-p", str(sb.process.pid),
                                   "-e", f"trace={syscall}", "-e", f"inject={syscall}:{action}",
                                   "-P", str(path)], stdout=log, stderr=subprocess.STDOUT)
        time.sleep(.5)
        assert tracer.poll() is None, f"cannot attach injection; see {sb.dir / 'injection.log'}"
    except BaseException:
        if tracer is None:
            log.close()
        else:
            try:
                detach((tracer, log))
            except BaseException as error:
                print(f"usage injection cleanup: {error}", file=sys.stderr)
        raise
    return tracer, log


def detach(injection):
    tracer, log = injection
    if log.closed:
        return
    try:
        if tracer.poll() is None:
            tracer.send_signal(signal.SIGINT)
            try:
                tracer.wait(timeout=10)
            except subprocess.TimeoutExpired:
                tracer.kill()
                tracer.wait(timeout=5)
                raise AssertionError("strace did not stop cleanly")
    finally:
        log.close()
    assert tracer.returncode in (0, -signal.SIGINT), f"strace exit={tracer.returncode}"


def collect_and_unmount(mount, sb):
    try:
        if sb is not None:
            path = mount / "instance" / f"{sb.name}.usage"
            if path.exists():
                shutil.copy2(path, sb.dir / "final.usage")
    finally:
        run("umount", mount)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--cases", default="enospc,sync,writer,kill,short")
    args = parser.parse_args()
    assert os.geteuid() == 0
    cases = args.cases.split(",")
    assert set(cases) <= {"enospc", "sync", "writer", "kill", "short"}
    work = Path(tempfile.mkdtemp(prefix="e2e-usage-faults-"))
    print(f"usage fault evidence: {work}", flush=True)
    if os.environ.get("KUASAR_CI_DIR"):
        def collect():
            evidence = Path(os.environ["KUASAR_CI_DIR"]) / "usage-faults"
            for path in work.rglob("*"):
                if path.is_file() and path.suffix in (".json", ".log", ".usage"):
                    dest = evidence / path.relative_to(work)
                    dest.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copy2(path, dest)
        atexit.register(collect)
    metadata = {"host_kernel": run("uname", "-a"), "build": run("go", "version", "-m", BIN / "sandbox-ctl"),
                "ch": run(BIN / "cloud-hypervisor", "--version"), "cases": cases,
                "artifacts": {n: digest(BIN / n) for n in ("sandbox-ctl", "sandbox-init", "sandbox-runtime.bundle", "vmlinux", "cloud-hypervisor")}}
    write_json(work / "source-set.json", metadata)
    root = work / "rootfs"
    root.mkdir()
    for name in ("tmp", "proc", "sys", "dev"):
        (root / name).mkdir()
    run("go", "build", "-trimpath", "-o", root / "probe", Path(__file__).parent / "usageprobe/main.go",
        env={**os.environ, "CGO_ENABLED": "0", "GOWORK": "off"})
    image = work / "root.img"
    run(BIN / "flatten-ctl", "export", "--output", image, "--no-progress", root)
    ref, results = image_ref(image), []
    for name in cases:
        diff = work / f"{name}.ext4"
        ext4(diff)
        config = {"resources": {"capacity": {"cpu": 1, "memory": "512MiB"},
                                "allocatable": {"cpu": 1, "memory": "512MiB", "deflate_on_oom": False}},
                  "boot": {"kernel": f"file://{BIN / 'vmlinux'}", "runtime": f"file://{BIN / 'sandbox-runtime.bundle'}",
                           "root": {"base": ref, "overlay": {"diff": f"file://{diff}"}}},
                  "launch": {"exec": "/probe", "args": ["cpu-ms", "200"] if name == "short" else ["wait"],
                             "restart": "never", "pid_namespace": "shared"},
                  "usage": {"enabled": True, "sample_interval": "1s", "flush_interval": "5s"}}
        mount = None
        if name == "enospc":
            mount = work / "enospc-base"
            mount.mkdir()
            run("mount", "-t", "tmpfs", "-o", "size=256k", "tmpfs", mount)
        sb, injection = None, None
        try:
            sb = Sandbox(work, name, config, base_root=mount)
            if name == "short":
                sb.process.wait(timeout=30)
                assert sb.process.returncode == 0
                sb.log.close()
                view = sb.view()
                write_json(sb.dir / "short.json", view)
                cpu = metric(view["saved"]["snapshot"], "counters", "guest.cpu")
                assert int(cpu["known_total_ns"]) >= 100_000_000, cpu
                results.append({"case": name, "passed": True})
                print(f"PASS usage-faults/{name}", flush=True)
                continue
            sb.ready()
            guest = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
            assert guest["sandbox_init_sha256"] == metadata["artifacts"]["sandbox-init"]
            write_json(sb.dir / "guest.json", guest)
            time.sleep(5.5)
            before = sb.view()
            assert before.get("saved")
            write_json(sb.dir / "before.json", before)
            if name == "enospc":
                # Leave the saved frame's partially used page intact: a later
                # append can perform a real partial write before ENOSPC.
                with (mount / "filler").open("wb", buffering=0) as filler:
                    while True:
                        try:
                            filler.write(bytes(4096))
                        except OSError as error:
                            assert error.errno == errno.ENOSPC
                            break
            elif name == "sync":
                injection = inject(sb, "fsync", "error=EIO")
            elif name == "writer":
                injection = inject(sb, "pwrite64", "delay_enter=15000000:when=1")
            elif name == "kill":
                sb.cli("exec", "--", "/probe", "cpu", "1")
                live = sb.view()
                write_json(sb.dir / "crash-cleanup.json", kill_host_and_stop_ch(sb))
                sb.log.close()
                saved = sb.view()
                write_json(sb.dir / "killed-live.json", live)
                write_json(sb.dir / "killed-saved.json", saved)
                assert not saved["saved"]["snapshot"]["closed"]
                assert int(metric(saved["saved"]["snapshot"], "counters", "guest.cpu")["known_total_ns"]) <= int(metric(live["live"], "counters", "guest.cpu")["known_total_ns"])
                child = Sandbox(work, "kill-restarted", config, sandbox_id=name, base_root=sb.baseroot)
                try:
                    child.ready()
                    time.sleep(2)
                    view = child.view()
                    write_json(child.dir / "restarted.json", view)
                    assert not metric(view["live"], "counters", "guest.cpu")["complete"]
                    assert metric(view["live"], "gauges", "guest.memory")["status"] == "ok"
                finally:
                    child.stop()
                results.append({"case": name, "passed": True})
                print(f"PASS usage-faults/{name}", flush=True)
                continue
            resources = []
            for _ in range(12):
                time.sleep(1)
                view = sb.view()
                resources.append({"fd_count": len(list(Path(f"/proc/{sb.process.pid}/fd").iterdir())),
                                  "thread_count": len(list(Path(f"/proc/{sb.process.pid}/task").iterdir())),
                                  "saving": view["saving"], "unknown_tail": view["unknown_tail"],
                                  "saved_sequence": view["saved"]["sequence"],
                                  "save_error": view.get("save_error", "")})
            write_json(sb.dir / "fault-resources.json", resources)
            after = sb.view()
            write_json(sb.dir / "during.json", after)
            if name == "enospc":
                # A complete frame may fit the saved file's already allocated
                # final page even though the filesystem has no free pages.
                faults = [r for r in resources if r["save_error"]]
                assert faults and after["saved"]["sequence"] == faults[0]["saved_sequence"], "failed append advanced saved baseline"
            else:
                assert after["saved"]["sequence"] == before["saved"]["sequence"], "fault advanced saved baseline"
            if name == "writer":
                assert after["saving"], "writer delay not active"
            else:
                assert after.get("save_error"), after
            for field in ("guest.memory", "filesystem.root", "ch.rss_anon", "sandbox_ctl.rss_anon"):
                a = metric(after["live"], "gauges", field)
                b = metric(before["live"], "gauges", field)
                assert a["status"] == "ok" and int(a["covered_total_ns"]) > int(b["covered_total_ns"]), a
            assert max(r["fd_count"] for r in resources)-min(r["fd_count"] for r in resources) <= 4
            # Actual data-plane Sync still succeeds: injection/exhaustion is
            # restricted to usage storage, never the writable disk backend.
            sb.cli("exec", "--", "/probe", "write", "/tmp/during-fault", "1")
            if injection is not None:
                detach(injection)
                injection = None
            if mount is not None:
                (mount / "filler").unlink()
            time.sleep(6)
            recovered = sb.view()
            write_json(sb.dir / "recovered.json", recovered)
            assert not recovered.get("save_error") and not recovered["unknown_tail"], recovered
            assert int(recovered["saved"]["sequence"]) > int(before["saved"]["sequence"])
            results.append({"case": name, "passed": True})
            print(f"PASS usage-faults/{name}", flush=True)
        finally:
            try:
                if injection is not None:
                    detach(injection)
            finally:
                try:
                    if sb is not None and sb.process.poll() is None:
                        sb.stop()
                finally:
                    if mount is not None:
                        collect_and_unmount(mount, sb)
        write_json(work / "results.json", results)
    write_json(work / "results.json", results)


if __name__ == "__main__":
    main()
