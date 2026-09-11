import copy
import errno
import http.client
import http.server
import io
import json
import os
from pathlib import Path
import subprocess
import socket
import socketserver
import struct
import tempfile
import threading
import unittest
from unittest.mock import patch

from usage import Sandbox, autonomous_balloon_prefix
import usage
import usage_perf
import usage_trace
import usage_faults
import usage_sources
from usage_ch_relay import CHRelay
from usage_vsock_relay import UsageRelay, exact, frame, line


class SourceFaultTests(unittest.TestCase):
    def baseline(self):
        return {**self.samples()[0]["live"]["gauges"][0], "status": "ok", "last_request_id": "0"}

    def samples(self):
        healthy = ("guest.memory", "filesystem.root", "filesystem.disk-1", "ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file")
        values = []
        for index in range(11):
            gauges = [{"name": "filesystem.disk-0", "status": "timeout" if index == 0 else "busy",
                       "continuous": False, "covered_total_ns": "10", "integral_total_byte_ns": "100",
                       "last_value_bytes": "10", "span_total_ns": str(index+10), "last_request_id": str(index+1)}]
            gauges += [{"name": name, "status": "ok", "covered_total_ns": str(index*1_000_000_000),
                        "last_request_id": str(index+1)} for name in healthy]
            values.append({"live": {"gauges": gauges}})
        return values

    def test_complete_partial_responses(self):
        self.assertEqual(len(usage_sources.validate_blocked(self.samples(), self.baseline())), 11)

    def test_reject_missing_first_timeout_replacement_and_fill(self):
        for field, value in (("status", "busy"), ("continuous", True)):
            with self.subTest(field=field):
                samples = self.samples()
                samples[0]["live"]["gauges"][0][field] = value
                with self.assertRaises(AssertionError):
                    usage_sources.validate_blocked(samples, self.baseline())
        for field, value in (("status", "timeout"), ("status", "ok"), ("covered_total_ns", "11"),
                             ("integral_total_byte_ns", "101"), ("last_value_bytes", "0")):
            with self.subTest(field=field):
                samples = self.samples()
                samples[5]["live"]["gauges"][0][field] = value
                with self.assertRaises(AssertionError):
                    usage_sources.validate_blocked(samples, self.baseline())

    def test_reject_unobserved_fault_round(self):
        samples = self.samples()
        del samples[4]
        with self.assertRaises(AssertionError):
            usage_sources.validate_blocked(samples, self.baseline())

    def test_first_timeout_cannot_clear_known_usage(self):
        rows = self.samples()
        for row in rows:
            row["live"]["gauges"][0].update(covered_total_ns="0", integral_total_byte_ns="0", last_value_bytes="0")
        with self.assertRaises(AssertionError):
            usage_sources.validate_blocked(rows, self.baseline())

    def test_recovery_requires_unbroken_observation_and_new_value(self):
        baseline = self.samples()[0]["live"]["gauges"][0]
        rows = self.samples()[:2]
        for row in rows:
            row["live"]["gauges"][0].update(status="ok", last_value_bytes="20")
        usage_sources.validate_recovery(rows, 0, baseline, 20)
        for field, value in (("last_request_id", "2"), ("last_value_bytes", "10"),
                             ("covered_total_ns", "11"), ("integral_total_byte_ns", "101")):
            original = rows[0]["live"]["gauges"][0][field]
            rows[0]["live"]["gauges"][0][field] = value
            with self.assertRaises(AssertionError):
                usage_sources.validate_recovery(rows, 0, baseline, 20)
            rows[0]["live"]["gauges"][0][field] = original

    def test_reject_other_source_failure_or_frozen_coverage(self):
        for failed in range(1, 8):
            samples = self.samples()
            samples[4]["live"]["gauges"][failed]["status"] = "missing"
            with self.assertRaises(AssertionError):
                usage_sources.validate_blocked(samples, self.baseline())
            samples = self.samples()
            samples[-1]["live"]["gauges"][failed]["covered_total_ns"] = "0"
            with self.assertRaises(AssertionError):
                usage_sources.validate_blocked(samples, self.baseline())

    def test_tracer_dependencies_include_loader(self):
        self.assertEqual(usage_sources.tracer_dependencies("libc.so.6 => /lib/libc.so.6 (0x123)\n/lib64/ld.so (0x456)\n"),
                         ["/lib/libc.so.6", "/lib64/ld.so"])
        self.assertEqual(usage_sources.tracer_dependencies("statically linked"), [])
        for invalid in ("libc => not found", "unexpected ldd output"):
            with self.assertRaises(ValueError):
                usage_sources.tracer_dependencies(invalid)

    def test_slow_ch_does_not_mask_other_sources_or_extend_old_memory(self):
        rows = self.samples()
        rows.append(self.samples()[-1])
        for row in rows:
            memory = row["live"]["gauges"][1]
            memory.update(status="missing", covered_total_ns="10", integral_total_byte_ns="100", last_value_bytes="20")
        baseline = {**rows[0]["live"]["gauges"][1], "status": "ok"}
        usage_sources.validate_ch_missing(rows, baseline)
        rows[7]["live"]["gauges"][1]["last_value_bytes"] = "0"
        with self.assertRaises(AssertionError):
            usage_sources.validate_ch_missing(rows, baseline)
        for row in rows:
            row["live"]["gauges"][1].update(covered_total_ns="0", integral_total_byte_ns="0", last_value_bytes="0")
        with self.assertRaises(AssertionError):
            usage_sources.validate_ch_missing(rows, baseline)

    def test_wire_recovery_cannot_integrate_gap_or_freeze_rss(self):
        rows = self.samples()[:3]
        baseline = copy.deepcopy(rows[0])
        base_memory = baseline["live"]["gauges"][1]
        base_memory.update(integral_total_byte_ns="100", last_value_bytes="20")
        for index, row in enumerate(rows):
            row["live"]["gauges"][1].update(covered_total_ns="0", integral_total_byte_ns="100", last_value_bytes="20", status="missing" if index == 0 else "ok")
        for gauge in rows[-1]["live"]["gauges"][4:]:
            gauge.update(covered_total_ns="40000000000", last_request_id="41")
        usage_sources.validate_wire_progress(baseline, rows)
        changed = copy.deepcopy(rows)
        changed[1]["live"]["gauges"][1]["covered_total_ns"] = "1"
        with self.assertRaises(AssertionError):
            usage_sources.validate_wire_progress(baseline, changed)
        for index in range(4, 8):
            changed = copy.deepcopy(rows)
            changed[-1]["live"]["gauges"][index]["covered_total_ns"] = "0"
            with self.assertRaises(AssertionError):
                usage_sources.validate_wire_progress(baseline, changed)
class SourceCleanupTests(unittest.TestCase):
    def test_observer_ready_requires_first_complete_frame_before_deadline(self):
        observer, path = unittest.mock.Mock(), unittest.mock.Mock()
        observer.poll.return_value = None
        path.read_text.side_effect = ["", '{"sequence":0}', '{"sequence":0}\n']
        with patch.object(usage_sources.time, "sleep"), \
             patch.object(usage_sources.time, "monotonic", side_effect=[0, .1, .2, .3]):
            usage_sources.wait_observer_ready(observer, path)
        self.assertEqual(path.read_text.call_count, 3)
        with patch.object(usage_sources.time, "monotonic", side_effect=[0, 6]):
            with self.assertRaisesRegex(AssertionError, "did not become ready"):
                usage_sources.wait_observer_ready(observer, path)
        observer.poll.return_value = 1
        with self.assertRaisesRegex(AssertionError, "exited before ready"):
            usage_sources.wait_observer_ready(observer, path)

    def test_each_trace_entry_escalates_without_masking_sampling_failure(self):
        for case in ("filesystem", "vsock"):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                sb, trace, relay = unittest.mock.Mock(), unittest.mock.Mock(), unittest.mock.Mock()
                sb.dir, sb.name, sb.runroot = Path(directory), "case", Path(directory)
                original = RuntimeError("original sampling failure")
                cleanup_error = RuntimeError("Guest trace-stop failed")
                guest = {"sandbox_init_sha256": "hash", "balloon_proc_field": "false", "zoneinfo": "pagesets count:"}

                def cli(*args, **kwargs):
                    if args[-1] == "trace-stop":
                        raise cleanup_error
                    return json.dumps(guest)

                sb.cli.side_effect = cli
                sb.stop.side_effect = RuntimeError("sandbox stop also failed")
                trace.poll.return_value = None
                trace.wait.side_effect = [subprocess.TimeoutExpired("owned trace", 5), 0]
                relay.requests, relay.errors = [], []
                views = [{"live": {"gauges": [{"name": name, "last_value_bytes": str(value)}
                         for name in ("filesystem.disk-0", "filesystem.disk-1")]}}
                         for value in (0, 8*1024*1024)]
                sb.view.side_effect = views if case == "filesystem" else original
                with patch.object(usage_sources, "filesystem_config", return_value={}), \
                     patch.object(usage_sources, "Sandbox", return_value=sb), \
                     patch.object(usage_sources, "UsageRelay", return_value=relay), \
                     patch.object(usage_sources, "digest", return_value="hash"), \
                     patch.object(usage_sources, "write_json"), \
                     patch.object(usage_sources.subprocess, "Popen", return_value=trace), \
                     patch.object(usage_sources.time, "sleep"), \
                     patch.object(usage_sources.time, "monotonic", side_effect=original), \
                     patch.object(usage_sources.sys, "stderr", io.StringIO()) as errors:
                    with self.assertRaises(RuntimeError) as caught:
                        if case == "filesystem":
                            usage_sources.filesystem_case(Path(directory), "ref", 30)
                        else:
                            usage_sources.vsock_case(Path(directory), "ref")
                self.assertIs(caught.exception, original)
                trace.terminate.assert_called_once()
                trace.kill.assert_called_once()
                self.assertEqual(trace.wait.call_args_list, [unittest.mock.call(timeout=5)]*2)
                sb.stop.assert_called_once()
                if case == "vsock":
                    relay.stop.assert_called_once()
                self.assertIn("Guest trace-stop failed", errors.getvalue())
                self.assertIn("sandbox stop also failed", errors.getvalue())

    def test_cleanup_only_error_is_not_hidden_by_enclosing_except(self):
        sb = unittest.mock.Mock()
        original = RuntimeError("real cleanup failure")
        sb.stop.side_effect = original
        try:
            raise ValueError("unrelated already handled error")
        except ValueError:
            with self.assertRaises(RuntimeError) as caught:
                usage_sources.cleanup_sources(sb)
        self.assertIs(caught.exception, original)

    def test_failed_term_still_attempts_kill_wait_and_remaining_cleanup(self):
        sb, process, other, relay = (unittest.mock.Mock() for _ in range(4))
        sb.dir = Path("unused")
        relay.requests, relay.errors = [], []
        process.poll.return_value = None
        other.poll.return_value = None
        original = RuntimeError("TERM failed")
        process.terminate.side_effect = original
        process.wait.side_effect = [subprocess.TimeoutExpired("owned CLI", 5), 0]
        with patch.object(usage_sources, "write_json"):
            with self.assertRaises(RuntimeError) as caught:
                usage_sources.cleanup_sources(sb, processes=(process, other), relay=relay)
        self.assertIs(caught.exception, original)
        process.kill.assert_called_once()
        self.assertEqual(process.wait.call_args_list, [unittest.mock.call(timeout=5)]*2)
        other.terminate.assert_called_once()
        other.wait.assert_called_once_with(timeout=5)
        sb.stop.assert_called_once()
        relay.stop.assert_called_once()


class CHRelayTests(unittest.TestCase):
    def test_real_body_is_held_then_forwarded_without_modification(self):
        body = b'{"memory_actual_size":503316480}'

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "ch.sock"
            native = socketserver.UnixStreamServer(str(path)+".real", Handler)
            native_thread = threading.Thread(target=native.serve_forever, kwargs={"poll_interval": .05})
            native_thread.start()
            relay, connection = CHRelay(path), http.client.HTTPConnection("localhost", timeout=2)
            connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            connection.sock.settimeout(.1)
            connection.sock.connect(str(path))
            try:
                relay.arm("/api/v1/vm.info")
                connection.request("GET", "/api/v1/vm.info")
                self.assertTrue(relay.held.wait(timeout=2))
                with self.assertRaises(TimeoutError):
                    connection.sock.recv(1, socket.MSG_PEEK)
                relay.release.set()
                connection.sock.settimeout(2)
                response = connection.getresponse()
                self.assertEqual(response.status, 200)
                self.assertEqual(response.read(), body)
            finally:
                connection.close()
                relay.stop()
                native.shutdown()
                native.server_close()
                native_thread.join(timeout=2)
            self.assertFalse(relay.errors)
            self.assertEqual(relay.requests[0]["response_body"], body.decode())


class UsageRelayTests(unittest.TestCase):
    def wire(self, kind, number):
        body = json.dumps({"type": kind, kind: {"request_id": str(number), "run_epoch": "run"}}, separators=(",", ":")).encode()
        return struct.pack("<I", len(body)) + body

    def test_length_and_preface_are_bounded(self):
        left, right = socket.socketpair()
        try:
            left.sendall(struct.pack("<I", (1 << 20) + 1))
            with self.assertRaises(AssertionError):
                frame(right)
            left.sendall(b"a"*128)
            with self.assertRaises(ValueError):
                line(right)
        finally:
            left.close()
            right.close()

    def test_only_usage_is_replayed_and_new_connection_reads_new_frame(self):
        owner = self

        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                try:
                    self.request.settimeout(2)
                    owner.assertEqual(line(self.request), b"CONNECT 5000\n")
                    self.request.sendall(b"OK 1\n")
                    while True:
                        data, message = frame(self.request)
                        if message["type"] != "usage_request":
                            self.request.sendall(data)
                            return
                        if message["usage_request"]["request_id"] == "7":
                            return  # Guest EOF must not be attributed to Host.
                        self.request.sendall(owner.wire("usage_response", int(message["usage_request"]["request_id"])))
                except EOFError:
                    pass

        class Native(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
            daemon_threads = True

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "vsock.sock"
            native = Native(str(path)+".real", Handler)
            thread = threading.Thread(target=native.serve_forever, kwargs={"poll_interval": .05})
            thread.start()
            relay = UsageRelay(path)

            def connect():
                connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                connection.settimeout(2)
                connection.connect(str(path))
                connection.sendall(b"CONNECT 5000\n")
                self.assertEqual(line(connection), b"OK 1\n")
                return connection

            client = ordinary = replacement = None
            try:
                client = connect()
                client.sendall(self.wire("usage_request", 1))
                self.assertEqual(frame(client)[0], self.wire("usage_response", 1))
                relay.arm("repeat")
                client.sendall(self.wire("usage_request", 2))
                self.assertEqual(frame(client)[0], self.wire("usage_response", 1))
                # The relay cannot create a later EOF which disguises a
                # client wrongly accepting the old frame as a timeout.
                client.sendall(self.wire("usage_request", 3))
                self.assertEqual(frame(client)[0], self.wire("usage_response", 3))
                client.close()
                ordinary = connect()
                ordinary.sendall(self.wire("ping", 9))
                self.assertEqual(frame(ordinary)[0], self.wire("ping", 9))
                replacement = connect()
                replacement.sendall(self.wire("usage_request", 4))
                self.assertEqual(frame(replacement)[0], self.wire("usage_response", 4))
                relay.arm("drop")
                replacement.sendall(self.wire("usage_request", 5))
                self.assertEqual(replacement.recv(1), b"")
                replacement.close()
                replacement = connect()
                replacement.sendall(self.wire("usage_request", 6))
                self.assertEqual(frame(replacement)[0], self.wire("usage_response", 6))
                relay.arm("repeat")
                replacement.sendall(self.wire("usage_request", 7))
                self.assertEqual(replacement.recv(1), b"")
            finally:
                for connection in (client, ordinary, replacement):
                    if connection:
                        connection.close()
                relay.stop()
                native.shutdown()
                native.server_close()
                thread.join(timeout=2)
            self.assertFalse(relay.errors)
            self.assertEqual([row["request"]["request_id"] for row in relay.requests], ["1", "2", "3", "4", "5", "6", "7"])
            self.assertEqual(relay.requests[1]["response"]["usage_response"]["request_id"], "2")
            self.assertEqual(relay.requests[1]["forwarded"]["usage_response"]["request_id"], "1")
            self.assertIn("guest_closed_ns", relay.requests[-1])
            self.assertNotIn("client_closed_ns", relay.requests[-1])


class BalloonWitnessTests(unittest.TestCase):
    def test_unchanged_target_growth(self):
        self.assertTrue(autonomous_balloon_prefix(512, 128, 384, [
            {"target": 128, "actual": 384}, {"target": 128, "actual": 388}]))

    def test_control_grow_then_delayed_shrink_does_not_prove_oom(self):
        self.assertFalse(autonomous_balloon_prefix(512, 128, 384, [
            {"target": 128, "actual": 384}, {"target": 0, "actual": 512},
            {"target": 128, "actual": 512}]))

    def test_target_changes_before_actual_do_not_prove_oom(self):
        self.assertFalse(autonomous_balloon_prefix(512, 128, 384, [
            {"target": 256, "actual": 384}]))

    def test_unconverged_or_no_balloon_baseline(self):
        self.assertFalse(autonomous_balloon_prefix(512, 128, 400, [
            {"target": 128, "actual": 404}]))
        self.assertFalse(autonomous_balloon_prefix(512, 0, 512, [
            {"target": 0, "actual": 513}]))


class CleanupTests(unittest.TestCase):
    def test_crash_cleanup_terminates_a_real_pinned_process(self):
        # Exercise real Linux pidfd readiness/signal/close, without KVM or
        # killing the test process standing in for the already-dead Host.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            sleeper = usage_faults.shutil.which("sleep")
            self.assertIsNotNone(sleeper)
            (root / "cloud-hypervisor").symlink_to(sleeper)
            child = subprocess.Popen([sleeper, "30"], start_new_session=True)
            try:
                (root / "run.log").write_text(f"CH started pid={child.pid}\n")
                sb = unittest.mock.Mock()
                sb.dir, sb.process.pid = root, os.getpid()
                sb.process.poll.return_value = None
                sb.process.wait.return_value = -usage_faults.signal.SIGKILL
                with patch.object(usage, "BIN", root):
                    result = usage_faults.kill_host_and_stop_ch(sb)
                self.assertTrue(result["ch_exited"])
                self.assertEqual(result["ch_pid"], child.pid)
                self.assertIn(child.wait(timeout=2), (-usage_faults.signal.SIGTERM, -usage_faults.signal.SIGKILL))
                sb.process.kill.assert_called_once()
            finally:
                if child.poll() is None:
                    child.kill()
                child.wait(timeout=5)

    def test_stop_cli_spawn_failure_still_reaps_owned_vm(self):
        sb = Sandbox.__new__(Sandbox)
        sb.process, sb.log = unittest.mock.Mock(), io.StringIO()
        sb.process.pid, sb.process.returncode = 123, 0
        sb.process.poll.return_value = None
        original = OSError(errno.EMFILE, "cannot start stop CLI")
        sb.cli = unittest.mock.Mock(side_effect=original)
        with patch("usage.os.killpg") as kill:
            with self.assertRaises(OSError) as caught:
                sb.stop()
        self.assertIs(caught.exception, original)
        kill.assert_called_once_with(123, usage_faults.signal.SIGTERM)
        sb.process.wait.assert_called_once_with(timeout=20)
        self.assertTrue(sb.log.closed)

    def test_all_vms_stopped_without_masking_readiness_failure(self):
        instances, stopped = [], []

        class Process:
            def poll(self):
                return None

        class Sandbox:
            def __init__(self, work, name, config):
                self.index = len(instances)
                self.dir, self.process = work / name, Process()
                instances.append(self)

            def ready(self):
                if self.index == 1:
                    raise RuntimeError("second VM readiness failure")

            def cli(self, *args, **kwargs):
                return '{"sandbox_init_sha256":"hash"}'

            def stop(self):
                stopped.append(self.index)
                if self.index == 0:
                    raise RuntimeError("first VM stop failure")

        with tempfile.TemporaryDirectory() as directory:
            with patch.object(usage_perf, "Sandbox", Sandbox), patch.object(usage_perf, "ext4"), \
                 patch.object(usage_perf, "digest", return_value="hash"), patch.object(usage_perf, "write_json"), \
                 patch.object(usage_perf.sys, "stderr", io.StringIO()) as errors:
                with self.assertRaisesRegex(RuntimeError, "second VM readiness failure"):
                    usage_perf.measure(Path(directory), "case", 2, "idle", False, 15, None, "ref", None, False)
        self.assertEqual(stopped, [0, 1])
        self.assertIn("first VM stop failure", errors.getvalue())

    def test_cleanup_failure_is_not_silently_successful(self):
        trace, first, second = unittest.mock.Mock(), unittest.mock.Mock(), unittest.mock.Mock()
        trace.close.side_effect = RuntimeError("trace failed")
        first.stop.side_effect = RuntimeError("stop failed")
        with self.assertRaisesRegex(RuntimeError, "trace failed"):
            usage_perf.cleanup(trace, [first, second])
        first.stop.assert_called_once()
        second.stop.assert_called_once()

    def test_trace_constructor_reaps_after_metadata_or_version_failure(self):
        for failure in ("metadata", "version"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                processes = []

                class Process:
                    def __init__(self, args, stdout=None, **kwargs):
                        self.returncode, self.signals, self.log = None, [], stdout
                        stdout.write("TRACE_READY\n")
                        stdout.flush()
                        processes.append(self)

                    def poll(self):
                        return self.returncode

                    def send_signal(self, sig):
                        self.signals.append(sig)

                    def wait(self, timeout=None):
                        self.returncode = 0
                        return 0

                def run(*args, **kwargs):
                    if args[0] == "gdb":
                        return "$1 = 0x40\n"
                    if args[0] == "go":
                        return "  source.go:1 0x1000 00 TESTQ AX, AX\n  source.go:2 0x1001 c3 RET\n"
                    if failure == "version":
                        raise RuntimeError("version failure")
                    return "bpftrace fake\n"

                with patch.object(usage_trace, "run", run), \
                     patch.object(usage_trace, "check_pid_namespace", return_value={}), \
                     patch.object(usage_trace.subprocess, "Popen", Process), \
                     patch.object(usage_trace.platform, "machine", return_value="x86_64"), \
                     patch.object(usage_trace.Path, "iterdir", return_value=[]), \
                     patch.object(usage_trace.time, "sleep"), \
                     patch.object(usage_trace.sys, "stderr", io.StringIO()), \
                     patch.object(usage_trace, "write_json", side_effect=OSError(errno.ENOSPC, "metadata full")):
                    with self.assertRaises(OSError if failure == "metadata" else RuntimeError):
                        usage_trace.Trace(Path(directory), [123], [456])
                self.assertEqual(len(processes), 1)
                self.assertEqual(processes[0].returncode, 0)
                self.assertTrue(processes[0].signals)
                self.assertTrue(processes[0].log.closed)

    def test_trace_timeout_closes_log_and_does_not_repeat_kill(self):
        trace = usage_trace.Trace.__new__(usage_trace.Trace)
        trace.log = io.StringIO()
        trace.process = unittest.mock.Mock()
        trace.process.poll.return_value = None
        trace.process.wait.side_effect = [subprocess.TimeoutExpired("tracer", 10), 0]
        with self.assertRaisesRegex(AssertionError, "did not stop cleanly"):
            trace.close()
        self.assertTrue(trace.log.closed)
        trace.process.kill.assert_called_once()
        self.assertEqual(trace.process.wait.call_count, 2)
        trace.close()
        trace.process.kill.assert_called_once()

    def test_injection_timeout_reaps_and_closes_log(self):
        tracer, log = unittest.mock.Mock(), io.StringIO()
        tracer.poll.return_value = None
        tracer.wait.side_effect = [subprocess.TimeoutExpired("strace", 10), 0]
        with self.assertRaisesRegex(AssertionError, "strace did not stop cleanly"):
            usage_faults.detach((tracer, log))
        self.assertTrue(log.closed)
        tracer.kill.assert_called_once()
        self.assertEqual(tracer.wait.call_count, 2)
        usage_faults.detach((tracer, log))
        tracer.kill.assert_called_once()

    def test_injection_start_failure_closes_log(self):
        sb, log = unittest.mock.Mock(), io.StringIO()
        sb.baseroot, sb.dir, sb.name = Path("base"), Path("dir"), "test"
        with patch.object(usage_faults.Path, "open", return_value=log), \
             patch.object(usage_faults.subprocess, "Popen", side_effect=OSError("spawn failed")):
            with self.assertRaisesRegex(OSError, "spawn failed"):
                usage_faults.inject(sb, "fsync", "error=EIO")
        self.assertTrue(log.closed)

    def test_evidence_copy_failure_does_not_skip_unmount(self):
        sb = unittest.mock.Mock()
        sb.name, sb.dir = "test", Path("destination")
        with patch.object(usage_faults.Path, "exists", return_value=True), \
             patch.object(usage_faults.shutil, "copy2", side_effect=OSError(errno.ENOSPC, "evidence full")), \
             patch.object(usage_faults, "run") as run:
            with self.assertRaises(OSError):
                usage_faults.collect_and_unmount(Path("mounted"), sb)
        run.assert_called_once_with("umount", Path("mounted"))


class CrashCleanupTests(unittest.TestCase):
    def setUp(self):
        self.sb = unittest.mock.Mock()
        self.sb.dir, self.sb.process.pid = Path("owned"), 123
        self.sb.process.poll.return_value = None
        self.sb.process.wait.return_value = -usage_faults.signal.SIGKILL

        def start(target, *args, **kwargs):
            p = patch.object(target, *args, **kwargs)
            value = p.start()
            self.addCleanup(p.stop)
            return value

        self.read = start(usage_faults.Path, "read_text", side_effect=["CH started pid=456\n", "PPid:\t123\n"])
        self.identity = start(usage_faults.Path, "samefile", return_value=True)
        self.open = start(usage_faults.os, "pidfd_open", return_value=42)
        self.close = start(usage_faults.os, "close")
        self.send = start(usage_faults.signal, "pidfd_send_signal")
        self.poller = start(usage.select, "poll").return_value
        self.poller.poll.side_effect = [[], [(42, usage.select.POLLIN)]]

    def test_host_crashes_before_ch_cleanup(self):
        self.send.side_effect = lambda *_: self.sb.process.wait.assert_called_once_with(timeout=10)
        result = usage_faults.kill_host_and_stop_ch(self.sb)
        self.assertEqual(result, {"host_pid": 123, "ch_pid": 456, "signals": [15], "ch_exited": True})
        self.open.assert_called_once_with(456)
        self.send.assert_called_once_with(42, usage_faults.signal.SIGTERM)
        self.close.assert_called_once_with(42)
        self.sb.process.kill.assert_called_once()

    def test_term_timeout_escalates_only_the_pinned_child(self):
        self.poller.poll.side_effect = [[], [], [(42, usage.select.POLLIN)]]
        result = usage_faults.kill_host_and_stop_ch(self.sb)
        self.assertEqual(result["signals"], [15, 9])
        self.assertEqual(self.poller.poll.call_args_list, [unittest.mock.call(0), unittest.mock.call(2000), unittest.mock.call(5000)])
        self.assertEqual(self.send.call_args_list, [unittest.mock.call(42, 15), unittest.mock.call(42, 9)])
        self.close.assert_called_once_with(42)

    def test_already_exited_child_is_not_signalled(self):
        self.poller.poll.side_effect = [[(42, usage.select.POLLIN)]]
        self.assertTrue(usage_faults.kill_host_and_stop_ch(self.sb)["ch_exited"])
        self.send.assert_not_called()
        self.close.assert_called_once_with(42)

    def test_pid_reuse_or_wrong_binary_fails_before_host_kill(self):
        for parent, binary in ((999, True), (123, False)):
            with self.subTest(parent=parent, binary=binary):
                self.read.side_effect = ["CH started pid=456\n", f"PPid:\t{parent}\n"]
                self.identity.return_value = binary
                with self.assertRaises(AssertionError):
                    usage_faults.kill_host_and_stop_ch(self.sb)
        self.sb.process.kill.assert_not_called()
        self.send.assert_not_called()
        self.assertEqual(self.close.call_count, 2)

    def test_host_wait_failure_still_stops_ch(self):
        original = subprocess.TimeoutExpired("host", 10)
        self.sb.process.wait.side_effect = original
        with self.assertRaises(subprocess.TimeoutExpired) as caught:
            usage_faults.kill_host_and_stop_ch(self.sb)
        self.assertIs(caught.exception, original)
        self.send.assert_called_once_with(42, usage_faults.signal.SIGTERM)
        self.close.assert_called_once_with(42)

    def test_ch_timeout_is_not_success(self):
        self.poller.poll.side_effect = [[], [], []]
        with self.assertRaisesRegex(AssertionError, "cleanup budget"):
            usage_faults.kill_host_and_stop_ch(self.sb)
        self.close.assert_called_once_with(42)

    def test_enclosing_handled_timeout_does_not_hide_cleanup_failure(self):
        self.poller.poll.side_effect = [[], [], []]
        try:
            raise subprocess.TimeoutExpired("normal stop", 20)
        except subprocess.TimeoutExpired:
            with self.assertRaisesRegex(AssertionError, "cleanup budget"):
                usage.kill_host_and_stop_ch(self.sb)
        self.close.assert_called_once_with(42)

    def test_stop_timeout_uses_pinned_ch_cleanup(self):
        self.sb.log = io.StringIO()
        self.sb.process.wait.side_effect = [subprocess.TimeoutExpired("host", 20), -9]
        self.sb.process.returncode = -9
        self.read.side_effect = ["CH started pid=456\n", "CH started pid=456\n", "PPid:\t123\n"]
        with patch.object(usage, "write_json") as write:
            with self.assertRaisesRegex(AssertionError, "sandbox exit=-9"):
                Sandbox.stop(self.sb)
        self.sb.process.kill.assert_called_once()
        self.send.assert_called_once_with(42, usage.signal.SIGTERM)
        self.assertTrue(self.sb.log.closed)
        self.assertTrue(write.call_args.args[1]["ch_exited"])
        self.close.assert_called_once_with(42)

    def test_stop_timeout_before_ch_start_still_kills_host(self):
        self.sb.log = io.StringIO()
        self.sb.process.wait.side_effect = [subprocess.TimeoutExpired("host", 20), -9]
        self.sb.process.returncode = -9
        self.read.side_effect = ["setup not complete\n"]
        with self.assertRaisesRegex(AssertionError, "sandbox exit=-9"):
            Sandbox.stop(self.sb)
        self.sb.process.kill.assert_called_once()
        self.open.assert_not_called()
        self.send.assert_not_called()
        self.assertTrue(self.sb.log.closed)

    def test_stop_timeout_after_ch_reaped_still_kills_host(self):
        self.sb.log = io.StringIO()
        self.sb.process.wait.side_effect = [subprocess.TimeoutExpired("host", 20), -9]
        self.read.side_effect = ["CH started pid=456\n", "CH started pid=456\n"]
        original = ProcessLookupError("CH already reaped")
        self.open.side_effect = original
        with self.assertRaises(ProcessLookupError) as caught:
            Sandbox.stop(self.sb)
        self.assertIs(caught.exception, original)
        self.sb.process.kill.assert_called_once()
        self.assertEqual(self.sb.process.wait.call_args_list, [unittest.mock.call(timeout=20), unittest.mock.call(timeout=5)])
        self.send.assert_not_called()
        self.close.assert_not_called()
        self.assertTrue(self.sb.log.closed)

    def test_stop_identity_failure_never_signals_unverified_child(self):
        self.sb.log = io.StringIO()
        self.sb.process.wait.side_effect = [subprocess.TimeoutExpired("host", 20), -9]
        self.read.side_effect = ["CH started pid=456\n", "CH started pid=456\n", "PPid:\t999\n"]
        with self.assertRaisesRegex(AssertionError, "not this live Host"):
            Sandbox.stop(self.sb)
        self.sb.process.kill.assert_called_once()
        self.send.assert_not_called()
        self.close.assert_called_once_with(42)
        self.assertTrue(self.sb.log.closed)

    def test_stop_host_cleanup_error_preserves_ch_pin_failure(self):
        self.sb.log = io.StringIO()
        self.sb.process.wait.side_effect = subprocess.TimeoutExpired("host", 20)
        self.sb.process.kill.side_effect = PermissionError("Host signal failed")
        self.read.side_effect = ["CH started pid=456\n", "CH started pid=456\n"]
        original = ProcessLookupError("CH already reaped")
        self.open.side_effect = original
        with patch.object(usage.sys, "stderr", io.StringIO()) as errors:
            with self.assertRaises(ProcessLookupError) as caught:
                Sandbox.stop(self.sb)
        self.assertIs(caught.exception, original)
        self.assertIn("Host signal failed", errors.getvalue())
        self.send.assert_not_called()
        self.assertTrue(self.sb.log.closed)

    def test_stop_log_read_failure_still_kills_owned_host(self):
        self.sb.log = io.StringIO()
        self.sb.process.wait.side_effect = [subprocess.TimeoutExpired("host", 20), -9]
        original = OSError("log read failed")
        self.read.side_effect = original
        with self.assertRaises(OSError) as caught:
            Sandbox.stop(self.sb)
        self.assertIs(caught.exception, original)
        self.sb.process.kill.assert_called_once()
        self.open.assert_not_called()
        self.send.assert_not_called()
        self.assertTrue(self.sb.log.closed)

    def test_double_failure_preserves_host_error(self):
        original = OSError("host kill failed")
        self.sb.process.kill.side_effect = original
        self.send.side_effect = PermissionError("CH signal failed")
        with patch.object(usage_faults.sys, "stderr", io.StringIO()) as errors:
            with self.assertRaises(OSError) as caught:
                usage_faults.kill_host_and_stop_ch(self.sb)
        self.assertIs(caught.exception, original)
        self.assertIn("CH signal failed", errors.getvalue())
        self.close.assert_called_once_with(42)

    def test_bad_pidfd_event_is_not_exit(self):
        self.poller.poll.side_effect = [[(42, usage.select.POLLNVAL)]]
        with self.assertRaisesRegex(AssertionError, "unexpected pidfd event"):
            usage_faults.kill_host_and_stop_ch(self.sb)
        self.close.assert_called_once_with(42)


class TraceIdentityTests(unittest.TestCase):
    def test_initial_pid_identity(self):
        process = unittest.mock.Mock(pid=123, returncode=0)
        process.communicate.return_value = ('{"type":"printf","data":"USAGE_TRACE_PID 123\\n"}\n'
                                           '{"type":"printf","data":"USAGE_TRACE_EXEC 124 124 124\\n"}\n', None)
        with patch.object(usage_trace.subprocess, "Popen", return_value=process) as popen:
            self.assertEqual(usage_trace.check_pid_namespace("bpftrace"), {
                "proc_pid": 123, "bpf_pid": 123, "child_proc_pid": 124,
                "child_bpf_pid": 124, "child_sched_pid": 124})
        command = popen.call_args.args[0]
        self.assertEqual(command[command.index("-c")+1], "/bin/true")
        self.assertIn("args->pid", command[-1])

    def test_namespaced_bare_pid_cannot_hide_sched_identity_mismatch(self):
        # bpftrace >= 0.23: BEGIN now agrees with /proc even in a nested ns.
        # The kernel sched argument for its own child is still different.
        for witness in ("124 124 456", "124 456 456", "0 0 0", ""):
            with self.subTest(witness=witness):
                process = unittest.mock.Mock(pid=123, returncode=0)
                process.communicate.return_value = (
                    f"USAGE_TRACE_PID 123\nUSAGE_TRACE_EXEC {witness}\n", None)
                with patch.object(usage_trace.subprocess, "Popen", return_value=process):
                    with self.assertRaisesRegex(AssertionError, "initial PID namespace"):
                        usage_trace.check_pid_namespace("bpftrace")

    def test_duplicate_exec_witness_is_rejected(self):
        process = unittest.mock.Mock(pid=123, returncode=0)
        process.communicate.return_value = ("USAGE_TRACE_PID 123\n" + "USAGE_TRACE_EXEC 124 124 124\n"*2, None)
        with patch.object(usage_trace.subprocess, "Popen", return_value=process):
            with self.assertRaisesRegex(AssertionError, "initial PID namespace"):
                usage_trace.check_pid_namespace("bpftrace")

    def test_nested_pid_namespace_and_missing_witness_are_rejected(self):
        for output in ('{"data":"USAGE_TRACE_PID 456\\n"}', "", "USAGE_TRACE_PID 123\nUSAGE_TRACE_PID 123"):
            with self.subTest(output=output):
                process = unittest.mock.Mock(pid=123, returncode=0)
                process.communicate.return_value = (output, None)
                with patch.object(usage_trace.subprocess, "Popen", return_value=process):
                    with self.assertRaisesRegex(AssertionError, "initial PID namespace"):
                        usage_trace.check_pid_namespace("bpftrace")

    def test_preflight_timeout_reaps_own_process(self):
        process = unittest.mock.Mock()
        process.communicate.side_effect = [subprocess.TimeoutExpired("preflight", 15), ("", None)]
        with patch.object(usage_trace.subprocess, "Popen", return_value=process):
            with self.assertRaises(subprocess.TimeoutExpired):
                usage_trace.check_pid_namespace("bpftrace")
        process.kill.assert_called_once()
        self.assertEqual(process.communicate.call_count, 2)

    def test_preflight_second_timeout_closes_pipe_and_preserves_first_error(self):
        process = unittest.mock.Mock()
        original = subprocess.TimeoutExpired("preflight", 15)
        process.communicate.side_effect = [original, subprocess.TimeoutExpired("reap", 5)]
        with patch.object(usage_trace.subprocess, "Popen", return_value=process), \
             patch.object(usage_trace.sys, "stderr", io.StringIO()):
            with self.assertRaises(subprocess.TimeoutExpired) as caught:
                usage_trace.check_pid_namespace("bpftrace")
        self.assertIs(caught.exception, original)
        process.kill.assert_called_once()
        process.stdout.close.assert_called_once()


class ProcEvidenceTests(unittest.TestCase):
    def stat(self, pid=123, comm="CH (vcpu) ) worker"):
        fields = ["S"] + ["0"]*49
        for index, value in ((11, 90), (12, 3), (19, 500), (40, 173)):
            fields[index] = str(value)
        return f"{pid} ({comm}) {' '.join(fields)}\n"

    def test_raw_and_separate_ticks_preserve_native_anomaly(self):
        raw = self.stat()
        values = usage_perf.proc_stat(raw, 123)
        self.assertEqual(values, {"pid": 123, "start_ticks": 500, "utime_ticks": 90,
                                 "stime_ticks": 3, "cpu_ticks": 93, "guest_ticks": 173,
                                 "stat_raw": raw})
        # Preserve observed values; no clamping or alternate CPU definition.
        self.assertGreater(values["guest_ticks"], values["cpu_ticks"])

    def test_invalid_stat_fails_closed(self):
        for raw in (self.stat(pid=456), "123 (bad) S", self.stat().replace(" 90 ", " -1 "),
                    self.stat().replace(" 173 ", " bogus ")):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                usage_perf.proc_stat(raw, 123)

    def test_read_window_is_retained(self):
        raw = self.stat()
        status = "voluntary_ctxt_switches: 1\nnonvoluntary_ctxt_switches: 2\nRssAnon: 4 kB\nRssFile: 5 kB\nVmRSS: 9 kB\nThreads: 1\n"
        with patch.object(usage_perf.Path, "read_text", side_effect=[raw, status]), \
             patch.object(usage_perf.Path, "iterdir", return_value=[]), \
             patch.object(usage_perf.time, "monotonic_ns", side_effect=[100, 120]):
            values = usage_perf.proc(123)
        self.assertEqual((values["stat_read_started_ns"], values["stat_read_finished_ns"]), (100, 120))
        self.assertEqual(values["stat_raw"], raw)

    def test_partial_sampling_and_evidence_failure_do_not_abandon_vm(self):
        for storage_fails in (False, True):
            with self.subTest(storage_fails=storage_fails), tempfile.TemporaryDirectory() as directory:
                sb = unittest.mock.Mock()
                sb.dir, sb.process.pid = Path(directory), 123
                sb.cli.return_value = '{"sandbox_init_sha256":"hash"}'
                endpoint = usage_perf.proc_stat(self.stat(), 123)
                original = OSError("CH proc read failed")
                saved = []

                def write(path, value):
                    if path.name == "proc-observations.json":
                        saved.append(value)
                        if storage_fails:
                            raise OSError(errno.ENOSPC, "evidence full")

                with patch.object(usage_perf, "Sandbox", return_value=sb), \
                     patch.object(usage_perf, "ext4"), \
                     patch.object(usage_perf, "digest", return_value="hash"), \
                     patch.object(usage_perf, "write_json", side_effect=write), \
                     patch.object(usage_perf.Path, "read_text", return_value="CH started pid=456"), \
                     patch.object(usage_perf, "proc", side_effect=[endpoint, original]), \
                     patch.object(usage_perf.time, "sleep"), \
                     patch.object(usage_perf.sys, "stderr", io.StringIO()):
                    with self.assertRaises(OSError) as caught:
                        usage_perf.measure(Path(directory), "case", 1, "idle", False, 10, None, "ref", None, False)
                self.assertIs(caught.exception, original)
                sb.stop.assert_called_once()
                self.assertEqual(len(saved), 1)
                self.assertEqual(saved[0]["before"], [endpoint])
                self.assertEqual(saved[0]["after"], [])
                self.assertFalse(saved[0]["measurement_complete"])
                self.assertEqual(saved[0]["error"], str(original))

    def test_incomplete_pair_and_business_failure_preserve_completed_reads(self):
        for phase in ("ch-read", "business"):
            with self.subTest(phase=phase), tempfile.TemporaryDirectory() as directory:
                sb = unittest.mock.Mock()
                sb.dir, sb.process.pid = Path(directory), 123
                original = RuntimeError(f"{phase} failed")
                sb.cli.side_effect = ['{"sandbox_init_sha256":"hash"}', original]
                host = usage_perf.proc_stat(self.stat(), 123)
                ch = usage_perf.proc_stat(self.stat(pid=456), 456)
                reads = [host, ch, dict(host), original if phase == "ch-read" else dict(ch)]
                saved = []

                def write(path, value):
                    if path.name == "proc-observations.json":
                        saved.append(value)

                with patch.object(usage_perf, "Sandbox", return_value=sb), \
                     patch.object(usage_perf, "ext4"), \
                     patch.object(usage_perf, "digest", return_value="hash"), \
                     patch.object(usage_perf, "write_json", side_effect=write), \
                     patch.object(usage_perf.Path, "read_text", return_value="CH started pid=456"), \
                     patch.object(usage_perf, "proc", side_effect=reads), \
                     patch.object(usage_perf.time, "sleep"):
                    with self.assertRaises(RuntimeError) as caught:
                        usage_perf.measure(Path(directory), "case", 1, "idle", False, 10, None, "ref", lambda pid: 5, False)
                self.assertIs(caught.exception, original)
                sb.stop.assert_called_once()
                self.assertEqual(len(saved), 1)
                self.assertEqual(saved[0]["before"], [host, ch])
                self.assertEqual(saved[0]["after"], [])
                self.assertFalse(saved[0]["measurement_complete"])
                row, = saved[0]["measurements"]
                self.assertEqual(row["host"]["stat_raw"], host["stat_raw"])
                self.assertEqual(row["complete"], phase == "business")
                self.assertEqual("ch" in row, phase == "business")


if __name__ == "__main__":
    unittest.main()
