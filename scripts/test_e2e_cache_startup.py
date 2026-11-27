#!/usr/bin/env python3
"""Run the actual local-merge port/readiness functions with real owned children."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SOURCE = (ROOT / 'test/e2e/cases/snapshot.local-merge.sh').read_text()


def function(name):
    start = SOURCE.index(name + '() {\n')
    return SOURCE[start:SOURCE.index('\n}\n', start) + 3]


class CacheStartupTests(unittest.TestCase):
    def test_ports_are_distinct_while_both_sockets_are_owned(self):
        body = function('free_cache_ports').split("<<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
        # Wrap actual sockets: the second bind must occur before the first closes.
        probe = '''import socket
real_socket = socket.socket
opened = []
class CheckedSocket:
    def __init__(self):
        self.sock = real_socket()
        opened.append(self.sock)
    def __enter__(self): return self
    def __exit__(self, *args): self.sock.close()
    def bind(self, address):
        if self.sock is opened[1]:
            assert opened[0].fileno() >= 0
            assert opened[0].getsockname()[1] != 0
        self.sock.bind(address)
    def getsockname(self): return self.sock.getsockname()
socket.socket = CheckedSocket
'''
        result = subprocess.run(['python3', '-c', probe + body + '\nassert all(s.fileno() == -1 for s in opened)\n'],
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        ports = [int(value) for value in result.stdout.split()]
        self.assertEqual(len(ports), 2)
        self.assertNotEqual(*ports)

    def test_allocation_failure_does_not_start_configuration(self):
        assignment = SOURCE[SOURCE.index('CACHE_PORTS='):SOURCE.index('cat >"$WORK/cache.yaml"')]
        result = subprocess.run(['bash', '-ceu', function('free_cache_ports') + '\n'
                                 'python3() { return 57; }\n' + assignment + '\necho unexpected\n'],
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 57, result.stderr)
        self.assertNotIn('unexpected', result.stdout)

    def run_wait(self, mode):
        with tempfile.TemporaryDirectory(prefix='cache-startup-test-') as directory:
            root = Path(directory)
            binary = root / 'cache-ctl'
            binary.write_text('''#!/usr/bin/env bash
set -euo pipefail
echo probe >>"$OUT/calls"
echo "probe diagnostic: $MODE" >&2
case "$MODE" in
    serving) echo SERVING ;;
    not-serving) echo NOT_SERVING ;;
    exit-during-probe|serving-after-exit)
        touch "$OUT/exit-child"
        for _ in $(seq 1 100); do
            kill -0 "$CACHE_PID" 2>/dev/null || break
            command sleep 0.01
        done
        if [ "$MODE" = serving-after-exit ]; then echo SERVING; else exit 1; fi
        ;;
    *) exit 1 ;;
esac
''')
            binary.chmod(0o755)
            child = root / 'child.py'
            child.write_text('''import os, pathlib, time
root = pathlib.Path(os.environ['OUT'])
mode = os.environ['MODE']
if mode.startswith('early-exit-'):
    (root / 'cache.log').write_text('owned daemon startup failure\\n')
    raise SystemExit(int(mode.rsplit('-', 1)[1]))
while not (root / 'exit-child').exists():
    time.sleep(0.005)
(root / 'cache.log').write_text('owned daemon failed during probe\\n')
raise SystemExit(37)
''')
            body = '\n'.join(function(name) for name in (
                'cache_startup_diagnostics', 'require_cache_alive', 'wait_cache_ready'))
            # Zero-cost sleep spy preserves all 100 iterations and exact requested delays.
            script = ('set -euo pipefail\n' + body + '''
e2e_fail() { echo "FAIL: $*" >&2; exit 1; }
sleep() { printf '%s\\n' "$*" >>"$OUT/sleeps"; }
python3 "$OUT/child.py" &
CACHE_PID=$!
export CACHE_PID
trap 'kill -TERM "$CACHE_PID" 2>/dev/null || true; wait "$CACHE_PID" 2>/dev/null || true' EXIT
if [[ "$MODE" == early-exit-* ]]; then
    wait "$CACHE_PID" || true
fi
wait_cache_ready
''')
            env = dict(os.environ, OUT=str(root), BIN=str(root), MODE=mode, CACHE_HEALTH_PORT='1')
            result = subprocess.run(['bash', '-c', script], env=env, capture_output=True,
                                    text=True, timeout=8)
            calls = (root / 'calls').read_text().splitlines() if (root / 'calls').exists() else []
            sleeps = (root / 'sleeps').read_text().splitlines() if (root / 'sleeps').exists() else []
            errors = (root / 'cache-probe.log').read_text() if (root / 'cache-probe.log').exists() else ''
            return result, calls, sleeps, errors

    def test_serving_requires_live_owned_daemon_and_final_probe(self):
        result, calls, sleeps, errors = self.run_wait('serving')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 2)
        self.assertEqual(sleeps, [])
        self.assertIn('probe diagnostic: serving', errors)

    def test_non_serving_preserves_all_original_retries_and_never_passes(self):
        result, calls, sleeps, errors = self.run_wait('not-serving')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('cache did not become ready', result.stderr)
        self.assertEqual(len(calls), 101)
        self.assertEqual(sleeps, ['0.05'] * 100)
        self.assertIn('probe diagnostic: not-serving', errors)

    def test_early_child_exit_reports_real_status_and_cache_log(self):
        for code in (0, 23):
            with self.subTest(exit_code=code):
                result, calls, sleeps, _ = self.run_wait(f'early-exit-{code}')
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(f'cache exited before ready (exit={code})', result.stderr)
                self.assertIn('owned daemon startup failure', result.stderr)
                self.assertEqual(calls, [])
                self.assertEqual(sleeps, [])

    def test_exit_during_probe_cannot_wait_full_budget_or_fake_ready(self):
        for mode in ('exit-during-probe', 'serving-after-exit'):
            with self.subTest(mode=mode):
                result, calls, sleeps, errors = self.run_wait(mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('cache exited before ready (exit=37)', result.stderr)
                self.assertIn('owned daemon failed during probe', result.stderr)
                self.assertIn('probe diagnostic: ' + mode, result.stderr)
                self.assertIn('probe diagnostic: ' + mode, errors)
                self.assertEqual(len(calls), 1)
                self.assertEqual(sleeps, [])


if __name__ == '__main__':
    unittest.main(verbosity=2)
