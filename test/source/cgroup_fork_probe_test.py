#!/usr/bin/env python3
"""Native ABI/entry smoke; the real cgroup race remains in sandbox.cgroup.sh."""
import os
from pathlib import Path
import platform
import select
import struct
import subprocess
import tempfile
import unittest
import uuid

ROOT = Path(__file__).resolve().parents[2]


class InitCgroupAssertionTests(unittest.TestCase):
    def test_init_shares_visible_pid1_group_without_runtime_topology_assumptions(self):
        source = (ROOT / 'test/e2e/cases/sandbox.cgroup.sh').read_text()
        # The unquoted YAML heredoc removes these dollar escapes before the
        # guest shell sees the command. Execute the shipped command itself.
        command = next(line.strip() for line in source.splitlines()
                       if line.strip().startswith('init_cgroup=')).replace('\\$', '$')
        with tempfile.TemporaryDirectory() as directory:
            cat = Path(directory, 'cat')
            cat.write_text('#!/bin/sh\n'
                           'printf "CG-INIT console-noise\\n" >&2\n'
                           'case "$1" in\n'
                           '  /proc/self/cgroup) printf "%s" "$TEST_CGROUP"; exit "$TEST_READ_STATUS" ;;\n'
                           '  /proc/1/cgroup) printf "%s" "$TEST_PID1_CGROUP"; exit "$TEST_PID1_READ_STATUS" ;;\n'
                           '  *) exit 99 ;;\n'
                           'esac\n')
            cat.chmod(0o755)
            cases = [
                ('false', '0::/\n', '0::/\n', 0, 0, True),
                ('true', '0::/\n', '0::/\n', 0, 0, True),
                ('true', '0::/init\n', '0::/init\n', 0, 0, True),
                ('false', '0::/init\n', '0::/init\n', 0, 0, False),
                ('true', '0::/init\n', '0::/\n', 0, 0, False),
                ('true', '0::/\n', '0::/init\n', 0, 0, False),
                ('true', '0::/unexpected\n', '0::/unexpected\n', 0, 0, False),
                ('true', '', '', 0, 0, False),
                ('true', '0::/\n1::/other\n', '0::/\n1::/other\n', 0, 0, False),
                ('true', '0::/\n', '0::/\n', 7, 0, False),
                ('true', '0::/\n', '0::/\n', 0, 7, False),
            ]
            for control, value, owner, status, owner_status, valid in cases:
                with self.subTest(control=control, value=value, owner=owner,
                                  read_status=status, owner_read_status=owner_status):
                    result = subprocess.run(['sh', '-c', command], capture_output=True,
                        env={**os.environ, 'PATH': directory + ':' + os.environ['PATH'],
                             'control': control, 'TEST_CGROUP': value,
                             'TEST_READ_STATUS': str(status), 'TEST_PID1_CGROUP': owner,
                             'TEST_PID1_READ_STATUS': str(owner_status)}, timeout=5)
                    self.assertEqual(result.returncode == 0, valid, result.stderr)


class ProbeCompletionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        source = (ROOT / 'test/e2e/cases/sandbox.cgroup.sh').read_text()
        # Execute the shipped helper and GEN-2 wait calls, without requiring a
        # VM. Removing either completion wait must break the handshake below.
        cls.wait_marker = 'wait_marker() {' + source.split('wait_marker() {', 1)[1].split('\n}', 1)[0] + '\n}'
        cls.restart_waits = '\n'.join(line for line in source.splitlines()
                                     if line.strip().startswith('wait_marker ') and '-GEN-2-' in line)

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='cgroup-completion-')
        self.addCleanup(temporary.cleanup)
        self.log = Path(temporary.name, 'probe.log')
        self.producer = self.start_process(['bash', '-c', r'''
set -euo pipefail
log=$1
printf '%s\n' 'CG-primary-GEN-2-SELF /init' 'CG-primary-GEN-2-FORK-OK 128' \
    'CG-primary-GEN-2-PID-private-OK' 'CG-plugin-GEN-2-SELF /init' \
    'CG-plugin-GEN-2-FORK-OK 128' >"$log"
printf 'written\n'
while IFS= read -r marker; do
    printf '%s\n' "$marker" >>"$log"
    printf 'written\n'
done
''', 'producer', str(self.log)])
        self.expect_line(self.producer, 'written\n')

    def start_process(self, command):
        process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True, bufsize=1)
        self.addCleanup(self.stop_process, process)
        return process

    @staticmethod
    def stop_process(process):
        if process.poll() is None:
            process.terminate()
        try:
            process.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.communicate(timeout=5)

    def expect_line(self, process, expected):
        self.assertTrue(select.select([process.stdout], [], [], 5)[0],
                        'shell did not reach the expected handshake')
        self.assertEqual(process.stdout.readline(), expected)

    def write_markers(self, *markers):
        for marker in markers:
            self.producer.stdin.write(marker + '\n')
            self.producer.stdin.flush()
            self.expect_line(self.producer, 'written\n')

    def start_waiter(self):
        # Gate each retry before its real 50ms sleep, so the test controls log
        # arrival without racing arbitrary delays against the polling loop.
        script = r'''
set -euo pipefail
log=$1 run_pid=$2 expected=/init pid_mode=private name=test
e2e_fail() { printf '%s\n' "$*" >&2; exit 1; }
sleep() { printf 'poll\n'; IFS= read -r advance; command sleep "$@"; }
'''
        return self.start_process(['bash', '-c', script + self.wait_marker + '\n' + self.restart_waits,
                                   'waiter', str(self.log), str(self.producer.pid)])

    @staticmethod
    def advance(waiter):
        waiter.stdin.write('\n')
        waiter.stdin.flush()

    def finish(self, waiter, expected):
        # Keep stdin open until exit: a missing marker must not appear to fail
        # merely because the test closed the retry handshake's input.
        waiter.wait(timeout=5)
        stdout, stderr = waiter.communicate(timeout=5)
        self.assertEqual(waiter.returncode, expected, stdout + stderr)
        return stderr

    def assert_completion_wait(self, pending_role, completed_role):
        self.write_markers(f'CG-{completed_role}-GEN-2-CONTROLLERS-OK',
                           f'CG-{completed_role}-GEN-2-MEMBERSHIP-OK')
        waiter = self.start_waiter()
        self.expect_line(waiter, 'poll\n')
        self.write_markers(f'CG-{pending_role}-GEN-2-CONTROLLERS-OK',
                           f'CG-{pending_role}-GEN-2-MEMBERSHIP-OK')
        self.advance(waiter)
        self.finish(waiter, 0)

    def test_waits_for_primary_generation_two_completion_after_pid(self):
        self.assert_completion_wait('primary', 'plugin')

    def test_waits_for_plugin_generation_two_completion_after_pid(self):
        self.assert_completion_wait('plugin', 'primary')

    def test_failure_wins_over_later_completion_markers(self):
        waiter = self.start_waiter()
        self.expect_line(waiter, 'poll\n')
        self.write_markers('CG-FAIL primary root-members', 'CG-primary-GEN-2-MEMBERSHIP-OK',
                           'CG-plugin-GEN-2-MEMBERSHIP-OK')
        self.advance(waiter)
        self.assertIn('CG-FAIL primary root-members', self.finish(waiter, 1))

    def test_producer_exit_before_completion_fails(self):
        waiter = self.start_waiter()
        self.expect_line(waiter, 'poll\n')
        self.stop_process(self.producer)
        self.advance(waiter)
        self.finish(waiter, 1)

    def test_other_generations_do_not_satisfy_completion(self):
        self.write_markers('CG-primary-GEN-1-MEMBERSHIP-OK', 'CG-plugin-GEN-1-MEMBERSHIP-OK',
                           'CG-primary-GEN-3-MEMBERSHIP-OK', 'CG-plugin-GEN-3-MEMBERSHIP-OK')
        waiter = self.start_waiter()
        self.expect_line(waiter, 'poll\n')
        self.stop_process(self.producer)
        self.advance(waiter)
        self.finish(waiter, 1)


class NativeForkProbeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix='fork-probe-build-')
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.root = Path(cls.temporary.name)
        cls.binary = cls.root / 'cgroup-fork-probe'
        result = subprocess.run(['make', '-C', str(ROOT), 'e2e-cgroup-fork-probe',
                                 'TARGET_ARCH=' + platform.machine(),
                                 'E2E_FIXTURE_DIR=' + str(cls.root)],
                                text=True, capture_output=True, timeout=30)
        if result.returncode:
            raise AssertionError(result.stdout + result.stderr)

    def test_static_target_elf_has_no_runtime_loader(self):
        data = self.binary.read_bytes()
        self.assertEqual(data[:7], b'\x7fELF\x02\x01\x01')
        self.assertEqual(struct.unpack_from('<H', data, 18)[0],
                         {'x86_64': 62, 'aarch64': 183}[platform.machine()])
        offset = struct.unpack_from('<Q', data, 32)[0]
        size, count = struct.unpack_from('<HH', data, 54)
        kinds = [struct.unpack_from('<I', data, offset + i * size)[0] for i in range(count)]
        self.assertNotIn(2, kinds, 'probe must not depend on PT_DYNAMIC')
        self.assertNotIn(3, kinds, 'probe must not depend on PT_INTERP')

    def invoke(self, program):
        role = 'probe-' + uuid.uuid4().hex
        result_file = Path('/tmp') / ('cg-' + role + '.fast-paths')
        self.addCleanup(lambda: result_file.unlink(missing_ok=True))
        env = dict(os.environ, PROBE_EXPECTED_ROLE=role, PROBE_TEST_ENV='preserved')
        result = subprocess.run([str(self.binary), str(program), role, '/', 'shared', 'false', '0'],
                                env=env, capture_output=True, text=True, timeout=20)
        return result, result_file

    def test_immediate_children_then_exec_preserve_args_and_environment(self):
        program = self.root / ('entry-' + uuid.uuid4().hex + '.sh')
        program.write_text('#!/bin/sh\n'
                           'test "$1" = "$PROBE_EXPECTED_ROLE" || exit 31\n'
                           'test "$PROBE_TEST_ENV" = preserved || exit 32\n'
                           'test "$2:$3:$4:$5" = /:shared:false:0 || exit 33\n'
                           'exit 0\n')
        program.chmod(0o755)
        result, output = self.invoke(program)
        self.assertEqual(result.returncode, 0, result.stderr)
        records = output.read_text().splitlines()
        self.assertEqual(len(records), 128)
        expected = next(line for line in Path('/proc/self/cgroup').read_text().splitlines()
                        if line.startswith('0::'))
        self.assertEqual(set(records), {expected})

    def test_bad_arguments_fail_without_fork(self):
        result = subprocess.run([str(self.binary)], timeout=5)
        self.assertEqual(result.returncode, 125)

    def test_exec_failure_is_reported_after_children_are_reaped(self):
        result, output = self.invoke(self.root / 'missing-program')
        self.assertEqual(result.returncode, 127)
        self.assertEqual(len(output.read_text().splitlines()), 128)


if __name__ == '__main__':
    unittest.main()
