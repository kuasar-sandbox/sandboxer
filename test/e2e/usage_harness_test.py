import errno
import io
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from usage import Sandbox, autonomous_balloon_prefix
import usage_perf
import usage_trace
import usage_faults


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


class TraceIdentityTests(unittest.TestCase):
    def test_initial_pid_identity(self):
        process = unittest.mock.Mock(pid=123, returncode=0)
        process.communicate.return_value = ('{"type":"printf","data":"USAGE_TRACE_PID 123\\n"}\n', None)
        with patch.object(usage_trace.subprocess, "Popen", return_value=process):
            self.assertEqual(usage_trace.check_pid_namespace("bpftrace"), {"proc_pid": 123, "kernel_pid": 123})

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


if __name__ == "__main__":
    unittest.main()
