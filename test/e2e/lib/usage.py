#!/usr/bin/env python3
"""Real usage integration; keep complete per-case evidence, never synthesize usage."""
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
import time

from usage_report_relay import ReportRelay

BIN = Path(os.environ["BIN"]).resolve()


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
    assert source is None, "product E2E cannot compile a custom probe"
    prepared = os.environ.get("USAGE_PROBE_BIN", "")
    assert prepared, "product E2E requires USAGE_PROBE_BIN"
    probe = Path(prepared)
    assert probe.is_file() and os.access(probe, os.X_OK), "missing prepared usage probe"
    shutil.copyfile(probe, output)
    Path(output).chmod(0o755)
    return ""


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


def runtime_init_digest(standalone):
    if os.environ.get("KUASAR_ARTIFACT_E2E") == "1":
        expected = os.environ.get("KUASAR_EXPECTED_RUNTIME_INIT_SHA256", "")
        assert re.fullmatch(r"[0-9a-f]{64}", expected), "missing validated runtime init identity"
        return expected
    return standalone


def ext4(path, root=None):
    with path.open("wb") as f:
        f.truncate(256 * 1024 * 1024)
    args = ["mkfs.ext4", "-q", "-F", "-O", "^has_journal"]
    if root is not None:
        args += ["-d", str(root)]
    run(*args, path)


class Sandbox:
    def __init__(self, work, name, config, restore=None, sandbox_id=None, base_root=None, ch_binary=None):
        self.dir = work / name
        self.dir.mkdir()
        self.name = sandbox_id or name
        self.runroot = work / {"restore-original": "ro", "restore-child": "rc"}.get(name, name) / "r"
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
