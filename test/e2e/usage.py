#!/usr/bin/env python3
"""Real usage integration; keep complete per-case evidence, never synthesize usage."""
import argparse
import atexit
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import select
import shutil
import signal
import socket
import subprocess
import sys
import tarfile
import tempfile
import time

from usage_report_relay import ReportRelay

REPO = Path(__file__).resolve().parents[2]
BIN = Path(os.environ["BIN"]).resolve()
# sudo can replace PATH while preserving the caller's explicit Go distribution.
# Keep its driver and compiler paired; an invalid root must fail, not fall back.
GO = str(Path(os.environ["GOROOT"]) / "bin/go") if os.environ.get("GOROOT") else "go"


def run(*args, timeout=60, **kw):
    result = subprocess.run([str(a) for a in args], check=False, timeout=timeout,
                            text=True, capture_output=True, **kw)
    if result.returncode:
        if result.stdout:
            print(result.stdout, end="" if result.stdout.endswith("\n") else "\n", file=sys.stderr)
        if result.stderr:
            print(result.stderr, end="" if result.stderr.endswith("\n") else "\n", file=sys.stderr)
        result.check_returncode()
    return result.stdout


def build_probe(output, source=None):
    source = source or Path(__file__).parent / "usageprobe/main.go"
    return run(GO, "build", "-trimpath", "-o", output, source,
               env={**os.environ, "CGO_ENABLED": "0", "GOWORK": "off"})


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for b in iter(lambda: f.read(1024 * 1024), b""):
            h.update(b)
    return h.hexdigest()


def image_ref(path):
    with tarfile.open(path) as archive:
        names = [n for n in archive.getnames() if n.startswith(".kuasar.digest.")]
    assert len(names) == 1, names
    return f"file://{path}@digest:{names[0].removeprefix('.kuasar.digest.')}"


def ext4(path, root=None):
    with path.open("wb") as f:
        f.truncate(256 * 1024 * 1024)
    args = ["mkfs.ext4", "-q", "-F"]
    if root is not None:
        args += ["-d", str(root)]
    run(*args, path)


class Sandbox:
    def __init__(self, work, name, config, restore=None, sandbox_id=None, base_root=None, ch_binary=None):
        self.dir = work / name
        self.dir.mkdir()
        self.name = sandbox_id or name
        self.runroot = self.dir / "run"
        self.baseroot = base_root or self.dir / "base"
        self.config = self.dir / "config.yaml"
        write_json(self.config, config)  # JSON is a strict subset of YAML.
        self.log = (self.dir / "run.log").open("w")
        command = [
            str(BIN / "sandbox-ctl"), "run", "--config", str(self.config),
            "--sandbox-id", self.name, "--path-id", "instance", "--run-root", str(self.runroot),
            "--base-root", str(self.baseroot), "--ch-binary", str(ch_binary or BIN / "cloud-hypervisor")]
        if restore is not None:
            command += ["--restore", str(restore)]
        try:
            self.process = subprocess.Popen(command,
                stdout=self.log, stderr=subprocess.STDOUT, start_new_session=True,
                env={**os.environ, "USAGE_TEST_CH": str(BIN / "cloud-hypervisor")} if ch_binary else None)
        except BaseException:
            self.log.close()
            raise

    def cli(self, command, *args, timeout=15):
        return run(BIN / "sandbox-ctl", command, "--sandbox-id", self.name,
                   "--path-id", "instance", "--run-root", self.runroot, *args, timeout=timeout)

    def ready(self):
        end = time.monotonic() + 45
        while self.process.poll() is None and time.monotonic() < end:
            try:
                self.cli("exec", "--", "/probe", "true", timeout=2)
                return
            except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
                time.sleep(.1)
        raise AssertionError(f"guest not ready; complete log: {self.dir / 'run.log'}")

    def view(self):
        return json.loads(self.cli("usage", "--base-root", self.baseroot))

    def ch_info(self):
        connection = http.client.HTTPConnection("localhost", timeout=2)
        connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        connection.sock.settimeout(2)
        connection.sock.connect(str(self.runroot / "instance/ch.sock"))
        try:
            connection.request("GET", "/api/v1/vm.info")
            response = connection.getresponse()
            body = response.read()
            assert response.status == 200, body
            return json.loads(body)
        finally:
            connection.close()

    def stop(self):
        command_error = None
        try:
            if self.process.poll() is None:
                try:
                    self.cli("exec", "--", "/probe", "exit")
                except (subprocess.CalledProcessError, subprocess.TimeoutExpired, OSError) as error:
                    if isinstance(error, OSError):
                        command_error = error
                    try:
                        os.killpg(self.process.pid, signal.SIGTERM)
                    except ProcessLookupError:
                        pass  # The owned process may have exited meanwhile.
                try:
                    self.process.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    try:
                        if "CH started pid=" in (self.dir / "run.log").read_text():
                            write_json(self.dir / "crash-cleanup.json", kill_host_and_stop_ch(self))
                        else:
                            self.process.kill()
                            self.process.wait(timeout=5)
                    except BaseException:
                        # CH may already have been reaped while the Host is
                        # draining backends. A log/pin/cleanup failure must
                        # still terminate this owned Popen Host.
                        try:
                            if self.process.poll() is None:
                                self.process.kill()
                                self.process.wait(timeout=5)
                        except BaseException as error:
                            print(f"usage Host cleanup: {error}", file=sys.stderr)
                        raise
        finally:
            self.log.close()
        if command_error is not None:
            raise command_error
        assert self.process.returncode == 0, f"sandbox exit={self.process.returncode}; log={self.dir / 'run.log'}"


def kill_host_and_stop_ch(sb):
    # CH has its own process group. Killing the Host's group does not stop
    # that VMM. Pin and verify this test's child before the Host dies; never
    # signal a numeric CH PID parsed from an old log after it can be reused.
    matches = re.findall(r"CH started pid=(\d+)", (sb.dir / "run.log").read_text())
    assert len(matches) == 1, "expected one owned CH process"
    ch_pid = int(matches[0])
    ch_fd = os.pidfd_open(ch_pid)
    result = {"host_pid": sb.process.pid, "ch_pid": ch_pid, "signals": [], "ch_exited": False}
    try:
        status = dict(line.split(":", 1) for line in Path(f"/proc/{ch_pid}/status").read_text().splitlines())
        assert int(status["PPid"]) == sb.process.pid and sb.process.poll() is None, "CH is not this live Host's child"
        assert Path(f"/proc/{ch_pid}/exe").samefile(BIN / "cloud-hypervisor"), "CH executable identity mismatch"
        poller = select.poll()
        poller.register(ch_fd, select.POLLIN)

        def exited(timeout):
            events = poller.poll(timeout)
            assert all(fd == ch_fd and flags & select.POLLIN for fd, flags in events), "unexpected pidfd event"
            return bool(events)

        host_error = None
        try:
            sb.process.kill()  # SIGKILL the Host, not a graceful usage save.
            assert sb.process.wait(timeout=10) == -signal.SIGKILL, "Host was not killed by SIGKILL"
        except BaseException as error:
            host_error = error
            raise
        finally:
            try:
                result["ch_exited"] = exited(0)
                for sig, budget_ms in ((signal.SIGTERM, 2000), (signal.SIGKILL, 5000)):
                    if result["ch_exited"]:
                        break
                    try:
                        signal.pidfd_send_signal(ch_fd, sig)
                        result["signals"].append(int(sig))
                    except ProcessLookupError:
                        pass  # Exit raced the signal; still verify the pidfd.
                    result["ch_exited"] = exited(budget_ms)
                assert result["ch_exited"], "owned CH did not exit within cleanup budget"
            except BaseException as error:
                if host_error is None:
                    raise
                print(f"usage crash cleanup: {error}", file=sys.stderr)
    finally:
        os.close(ch_fd)
    return result


def metric(snapshot, group, name):
    return next(m for m in snapshot[group] if m["name"] == name)


def autonomous_balloon_prefix(capacity, target, actual, observations):
    """A converged baseline plus actual growth before the first target change."""
    if target <= 0 or actual != capacity-target:
        return False
    for observation in observations:
        if observation["target"] != target:
            return False
        if observation["actual"] > actual:
            return True
    return False


def oom_baseline(sb, capacity):
    # Control settlement logs are not periodic report boundaries. Require a
    # real converged CH baseline; never mutate or pause the controller to get
    # one. The pressure witness must still precede the first target change.
    end, readings = time.monotonic() + 7, []
    try:
        while time.monotonic() < end:
            info = sb.ch_info()
            readings.append({"at_monotonic_ns": time.monotonic_ns(), "info": info})
            balloon = info["config"].get("balloon")
            assert balloon and balloon["deflate_on_oom"], "OOM case requires an enabled deflate-on-OOM device"
            target, actual = balloon["size"], info["memory_actual_size"]
            if 0 < target < capacity and actual == capacity-target:
                return info
            time.sleep(.02)
        raise AssertionError("no converged CH OOM baseline within budget")
    finally:
        write_json(sb.dir / "ch-oom-baseline-probes.json", readings)


def oom_pressure(sb, relay):
    observations, pressure, failure = [], None, None
    try:
        with (sb.dir / "pressure.log").open("w") as pressure_log:
            pressure = subprocess.Popen([
                str(BIN / "sandbox-ctl"), "exec", "--sandbox-id", sb.name,
                "--path-id", "instance", "--run-root", str(sb.runroot), "--stdin",
                "--", "/probe", "memory-wait", "440", "4"],
                stdin=subprocess.PIPE, stdout=pressure_log, stderr=subprocess.STDOUT)
            end = time.monotonic() + 5
            while time.monotonic() < end:
                assert pressure.poll() is None, "pressure probe exited before arming"
                if "MEMORY-ARMED\n" in (sb.dir / "pressure.log").read_text():
                    break
                time.sleep(.01)
            else:
                raise AssertionError("pressure probe did not arm")
            # Observe delivery of a new real report, not a periodic log or
            # assumed completion of its asynchronous Host control transaction.
            boundary = relay.next_report()
            log_offset = (sb.dir / "run.log").stat().st_size
            initial = oom_baseline(sb, 512*1024*1024)
            write_json(sb.dir / "ch-oom-baseline.json", initial)
            initial_target, initial_actual = initial["config"]["balloon"]["size"], initial["memory_actual_size"]
            started = time.monotonic_ns()
            assert 0 <= started - boundary["ack_forwarded_ns"] < 1_000_000_000, "report delivery window expired before pressure"
            pressure.stdin.write(b"G")
            pressure.stdin.flush()
            # These are Host pipe-write endpoints, not a Guest allocation
            # timestamp or a measurement of Guest scheduling latency.
            write_json(sb.dir / "oom-start.json", {"report": boundary, "start_write_begin_ns": started,
                                                   "start_write_end_ns": time.monotonic_ns()})
            autonomous, end = False, time.monotonic() + 8
            while time.monotonic() < end:
                info = sb.ch_info()
                observations.append({"at_monotonic_ns": time.monotonic_ns(),
                                     "target": info["config"]["balloon"]["size"], "actual": info["memory_actual_size"]})
                if not autonomous and autonomous_balloon_prefix(512*1024*1024, initial_target, initial_actual, observations):
                    with (sb.dir / "run.log").open("rb") as log:
                        log.seek(log_offset)
                        segment = log.read().decode(errors="replace")
                    (sb.dir / "autonomous-window.log").write_text(segment)
                    # A later unchanged-target deflate after a control resize
                    # cannot qualify: only the first target prefix is tested.
                    assert "balloon: CH accepted target=" not in segment and "after lost/ambiguous resize" not in segment, segment
                    autonomous = True
                time.sleep(.02)
            pressure.wait(timeout=10)
            assert pressure.returncode == 0, "memory workload failed; see pressure.log"
            assert "MEMORY-READY 461373440" in (sb.dir / "pressure.log").read_text()
            assert autonomous, "no autonomous deflation before the first target change"
            assert not relay.errors, relay.errors
    except BaseException as error:
        failure = error
        raise
    finally:
        errors = []
        if pressure is not None:
            try:
                pressure.stdin.close()
            except BaseException as error:
                errors.append(error)
            try:
                if pressure.poll() is None:
                    try:
                        pressure.terminate()
                    finally:
                        try:
                            pressure.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            try:
                                pressure.kill()
                            finally:
                                pressure.wait(timeout=5)
            except BaseException as error:
                errors.append(error)
        write_json(sb.dir / "ch-pressure.json", observations)
        if errors:
            if failure is None:
                raise errors[0]
            for error in errors:
                print(f"usage OOM cleanup: {error}", file=sys.stderr)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--cases", default="off,overlay,single,balloon,balloon-no-oom,oom,multidisk,restore")
    parser.add_argument("--seconds", type=int, default=12)
    # The production 5m default is covered by config/scheduler unit tests.
    # Keep --cases defaults as an explicit soak/debug check, never a CI wall-clock wait.
    args = parser.parse_args()
    assert args.seconds >= 6
    work = Path(tempfile.mkdtemp(prefix="e2e-usage-"))
    print(f"usage evidence: {work}", flush=True)
    # Only small evidence files enter the CI artifact, not disk/runtime images.
    # The packaged suite has no Git checkout/go.mod; compile the stdlib-only
    # probe by filename and record binary build identities in both layouts.
    if os.environ.get("KUASAR_CI_DIR"):
        evidence = Path(os.environ["KUASAR_CI_DIR"]) / "usage"
        def collect_evidence():
            evidence.mkdir(parents=True, exist_ok=True)
            for path in work.rglob("*"):
                if path.is_file() and path.suffix in (".json", ".log", ".usage"):
                    dest = evidence / path.relative_to(work)
                    dest.parent.mkdir(parents=True, exist_ok=True)
                    # The suite runs as root; do not preserve private source modes in CI evidence.
                    shutil.copyfile(path, dest)
        atexit.register(collect_evidence)
    metadata = {"sandbox_ctl_build": run(GO, "version", "-m", BIN / "sandbox-ctl"),
                "sandbox_init_build": run(GO, "version", "-m", BIN / "sandbox-init"),
                "host_kernel": run("uname", "-a").strip(),
                "ch_version": run(BIN / "cloud-hypervisor", "--version").strip(),
                "artifacts": {n: digest(BIN / n) for n in ("sandbox-ctl", "sandbox-init", "sandbox-runtime.bundle", "vmlinux", "cloud-hypervisor")},
                "seconds": args.seconds, "cases": args.cases.split(",")}
    if (REPO / ".git").exists():
        metadata["sandboxer_head"] = run("git", "rev-parse", "HEAD", cwd=REPO).strip()
        metadata["sandboxer_diff_sha256"] = hashlib.sha256(run("git", "diff", "HEAD", cwd=REPO).encode()).hexdigest()
    write_json(work / "source-set.json", metadata)
    root = work / "rootfs"
    root.mkdir()
    for name in ("tmp", "proc", "sys", "dev", "data", "cache", "other"):
        (root / name).mkdir()
    build_probe(root / "probe")
    image = work / "root.img"
    run(BIN / "flatten-ctl", "export", "--output", image, "--no-progress", root)
    ref = image_ref(image)
    results = []
    for name in metadata["cases"]:
        assert name in ("off", "overlay", "single", "balloon", "balloon-no-oom", "oom", "multidisk", "restore", "defaults"), name
        balloon = name in ("balloon", "balloon-no-oom", "oom")
        diff = work / f"{name}.ext4"
        ext4(diff, root if name == "single" else None)
        config = {"resources": {"capacity": {"cpu": 1, "memory": "512MiB"},
                                "allocatable": {"cpu": 1, "memory": "256MiB" if balloon else "512MiB",
                                                "deflate_on_oom": name in ("balloon", "oom")}},
                  "boot": {"kernel": f"file://{BIN / 'vmlinux'}", "runtime": f"file://{BIN / 'sandbox-runtime.bundle'}",
                           "cmdline": "console=hvc0 printk.time=1",
                           "root": {"diff": f"file://{diff}"} if name == "single" else
                           {"base": ref, "overlay": {"diff": f"file://{diff}"}}},
                  # Shared PID namespace makes /proc/1/exe the real init for
                  # artifact verification; the default private namespace's PID 1 is the app.
                  "launch": {"exec": "/probe", "args": ["wait"], "restart": "never", "pid_namespace": "shared"},
                  "usage": {"enabled": name != "off", "sample_interval": "1s", "flush_interval": "5s"}}
        if name == "defaults":
            config["usage"] = {"enabled": True}
        if name == "multidisk":
            config["boot"]["disks"] = []
            config["mounts"] = []
            for ordinal, target in enumerate(("/data", "/other")):
                disk = work / f"data-{ordinal}.ext4"
                ext4(disk)
                config["boot"]["disks"].append({"name": f"data{ordinal}", "diff": f"file://{disk}"})
                config["mounts"].append({"type": "disk", "source": f"data{ordinal}", "target": target})
            config["mounts"].append({"type": "empty", "target": "/data/cache"})
        sb = Sandbox(work, name, config, ch_binary=Path(__file__).with_name("usage_oom_wrapper.py") if name == "oom" else None)
        relay, failure = None, None
        try:
            if name == "oom":
                relay = ReportRelay(sb.runroot / "instance/vsock.sock.oom_5000")
            sb.ready()
            inspect = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
            write_json(sb.dir / "guest.json", inspect)
            assert inspect["sandbox_init_sha256"] == metadata["artifacts"]["sandbox-init"], "runtime bundle does not contain the selected sandbox-init"
            assert inspect["balloon_proc_field"] == "false", "this case requires the unpatched Balloon proc ABI"
            assert "pagesets" in inspect["zoneinfo"] and "count:" in inspect["zoneinfo"], "PCP source missing"
            time.sleep(2.2)
            before = sb.view()
            write_json(sb.dir / "before.json", before)
            started = time.monotonic()
            sb.cli("exec", "--", "/probe", "cpu", "2")
            sb.cli("exec", "--", "/probe", "write", "/tmp/usage-data", "16")
            if name == "multidisk":
                sb.cli("exec", "--", "/probe", "write", "/data/cache/usage-data", "16")
            seconds = max(303, args.seconds) if name == "defaults" else args.seconds
            time.sleep(max(0, seconds - (time.monotonic() - started)))
            after = sb.view()
            write_json(sb.dir / "after.json", after)
            if name == "off":
                assert not after["enabled"] and "live" not in after
                assert not list(sb.baseroot.rglob("*.usage"))
            else:
                b, a = before["live"], after["live"]
                cpu = int(metric(a, "counters", "guest.cpu")["known_total_ns"]) - int(metric(b, "counters", "guest.cpu")["known_total_ns"])
                assert cpu > 1_000_000_000, ("guest CPU did not observe busy workload", cpu)
                for field in ("guest.memory", "filesystem.root", "ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file"):
                    gauge = metric(a, "gauges", field)
                    assert gauge["status"] == "ok" and int(gauge["covered_total_ns"]) > 0, gauge
                fs_before = int(metric(b, "gauges", "filesystem.root")["last_value_bytes"])
                fs_after = int(metric(a, "gauges", "filesystem.root")["last_value_bytes"])
                assert fs_after - fs_before >= 16 * 1024 * 1024, (fs_before, fs_after)
                count = sum(g["name"].startswith("filesystem.") for g in a["gauges"])
                assert count == (3 if name == "multidisk" else 1), count
                assert after.get("saved") and int(after["saved_end"]) > 0, after
                if name == "defaults":
                    assert not before.get("saved") and after["saved"]["sequence"] == "1", "default cadence is not five minutes"
                assert a["sandbox_id"] == name and (sb.baseroot / "instance" / f"{name}.usage").exists()
                write_json(sb.dir / "history.json", json.loads(sb.cli("usage", "--history", "--limit", "10")))
                if balloon:
                    initial = sb.ch_info()
                    write_json(sb.dir / "ch-before-pressure.json", initial)
                    assert initial["config"]["balloon"]["size"] > 0 and initial["memory_actual_size"] < 512*1024*1024, "no actual balloon inflation observed"
                    assert initial["config"]["balloon"]["deflate_on_oom"] == (name != "balloon-no-oom")
                if name == "oom":
                    oom_pressure(sb, relay)
                    time.sleep(2)
                    pressure_view = sb.view()
                    write_json(sb.dir / "after-pressure.json", pressure_view)
                    ram = metric(pressure_view["live"], "gauges", "guest.memory")
                    assert ram["status"] == "ok" and int(ram["covered_total_ns"]) > int(metric(a, "gauges", "guest.memory")["covered_total_ns"]), ram
                if name == "restore":
                    output = sb.dir / "snapshot"
                    output.mkdir()
                    capture = sb.cli("snapshot", "--output", output, "--resume", "--drop-caches=false", "--merge-ref=false", timeout=60)
                    (sb.dir / "snapshot.log").write_text(capture)
                    assert (output / f"{name}.snapshot").is_file()
                    sb.cli("exec", "--", "/probe", "cpu", "2")
                    time.sleep(2)
                    write_json(sb.dir / "resumed.json", sb.view())
            results.append({"case": name, "passed": True})
        except BaseException as error:
            failure = error
            raise
        finally:
            try:
                try:
                    sb.stop()
                except BaseException as error:
                    if failure is None:
                        failure = error
                        raise
                    print(f"usage sandbox cleanup: {error}", file=sys.stderr)
            finally:
                if relay is not None:
                    try:
                        try:
                            relay.stop()
                        finally:
                            write_json(sb.dir / "reports.json", {"requests": relay.requests, "errors": relay.errors})
                    except BaseException as error:
                        if failure is None:
                            raise
                        print(f"usage report relay cleanup: {error}", file=sys.stderr)
        if name != "off":
            saved = sb.view()
            write_json(sb.dir / "stopped.json", saved)
            assert saved["saved"]["snapshot"]["closed"], saved
            if name == "restore":
                parent = saved["saved"]["snapshot"]
                parent_cpu = int(metric(parent, "counters", "guest.cpu")["known_total_ns"])
                parent_fs = int(metric(parent, "gauges", "filesystem.root")["last_value_bytes"])
                host = {"resources": config["resources"],
                        "boot": {"kernel": config["boot"]["kernel"], "runtime": config["boot"]["runtime"]},
                        "restore": {"prefetch": "off"},
                        # A periodic retry must not conceal a missed immediate
                        # post-ACK observation in a short restored run.
                        "usage": {"enabled": True, "sample_interval": "1m", "flush_interval": "5m"},
                        "timeouts": {"restore": "30s"}}
                for same_id in (False, True):
                    role = "same-restore" if same_id else "clone-restore"
                    restore_started = time.monotonic()
                    child = Sandbox(work, role, host, restore=output / f"{name}.snapshot",
                                    sandbox_id=name if same_id else role,
                                    base_root=sb.baseroot if same_id else None)
                    try:
                        child.ready()
                        time.sleep(3)
                        v = child.view()
                        elapsed = time.monotonic() - restore_started
                        assert elapsed < 60, ("restore test crossed its first periodic tick", elapsed)
                        write_json(child.dir / "first-round.json", {
                            "elapsed_seconds": elapsed, "sample_interval_seconds": 60,
                            "before_first_periodic_tick": True})
                        write_json(child.dir / "restored.json", v)
                        live = v["live"]
                        cpu = int(metric(live, "counters", "guest.cpu")["known_total_ns"])
                        assert (cpu >= parent_cpu) if same_id else (cpu < parent_cpu), (same_id, cpu, parent_cpu)
                        fs = metric(live, "gauges", "filesystem.root")
                        assert fs["status"] == "ok" and int(fs["last_value_bytes"]) >= parent_fs, fs
                        memory = metric(live, "gauges", "guest.memory")
                        assert memory["status"] == "ok", memory
                        assert int(fs["window"]["samples"]) > 0 and int(memory["window"]["samples"]) > 0, v
                        assert live["run_epoch"] != parent["run_epoch"]
                    finally:
                        child.stop()
        print(f"PASS usage/{name}", flush=True)
    write_json(work / "results.json", results)


if __name__ == "__main__":
    main()
