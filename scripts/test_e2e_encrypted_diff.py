#!/usr/bin/env python3
"""Exercise encrypted-diff publication admission without KVM or host flushes."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[1]
SOURCE = (ROOT / 'test/e2e/cases/sandbox.encrypted-diff.sh').read_text()
WAIT = SOURCE[SOURCE.index('wait_active_diffs() {'):SOURCE.index('\nwait_active_diffs 120')]
ASSERT = SOURCE[SOURCE.index('assert_diff() {'):SOURCE.index('\nassert_diff "$ROOT_DIFF"')]


class EncryptedDiffPublicationTest(unittest.TestCase):
    def run_case(self, setup, budget=1, assertions=''):
        with tempfile.TemporaryDirectory(prefix='encrypted-diff-') as directory:
            script = '\n'.join([
                'set -euo pipefail', 'e2e_fail() { echo "FAIL: $*" >&2; exit 1; }',
                'RUN_PID=$$; LOG="$WORK/sandbox.log"; echo diagnostic-marker > "$LOG"',
                'ROOT_DIFF="$WORK/root"; SCRATCH_DIFF="$WORK/scratch"; DATASET_DIFF="$WORK/dataset"',
                WAIT, ASSERT, setup,
                f'wait_active_diffs {budget} "$ROOT_DIFF" "$SCRATCH_DIFF" "$DATASET_DIFF"',
                assertions, 'echo completed',
            ])
            env = {k: v for k, v in os.environ.items() if k not in ('BASH_ENV', 'ENV')}
            start = time.monotonic()
            result = subprocess.run(['bash', '-c', script], env={**env, 'WORK': directory},
                                    capture_output=True, text=True, timeout=6)
            return result, time.monotonic() - start

    def test_delayed_publication_waits_for_every_destination(self):
        result, elapsed = self.run_case('''
( sleep 0.1; touch "$ROOT_DIFF"; sleep 0.1; touch "$SCRATCH_DIFF";
  sleep 0.1; touch "$DATASET_DIFF" ) &
''', budget=3)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertGreaterEqual(elapsed, 0.3)

    def test_never_published_reports_missing_paths_with_one_budget(self):
        result, elapsed = self.run_case('touch "$ROOT_DIFF"')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('diagnostic-marker', result.stderr)
        self.assertIn('unpublished active diff:', result.stderr)
        self.assertIn('/scratch', result.stderr)
        self.assertIn('/dataset', result.stderr)
        self.assertIn('within 1s', result.stderr)
        self.assertLess(elapsed, 2.5, 'all paths must share the same budget')

    def test_dead_process_fails_even_when_files_exist(self):
        result, _ = self.run_case('''
touch "$ROOT_DIFF" "$SCRATCH_DIFF" "$DATASET_DIFF"
( exit 0 ) & RUN_PID=$!; wait "$RUN_PID"
''')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('exited before active diffs', result.stderr)
        self.assertIn('diagnostic-marker', result.stderr)

    def test_nonregular_destinations_fail_immediately(self):
        for setup in ['mkdir "$ROOT_DIFF"', 'mkfifo "$ROOT_DIFF"',
                      'ln -s missing "$ROOT_DIFF"',
                      'touch "$WORK/target"; ln -s "$WORK/target" "$ROOT_DIFF"']:
            with self.subTest(setup=setup):
                result, elapsed = self.run_case(setup)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('not a regular file', result.stderr)
                self.assertLess(elapsed, 0.8)

    def test_publication_does_not_bypass_header_validation(self):
        result, _ = self.run_case('touch "$ROOT_DIFF" "$SCRATCH_DIFF" "$DATASET_DIFF"',
                                  assertions='assert_diff "$ROOT_DIFF" 8192')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('KDXTS1 header', result.stderr)

    def test_valid_sparse_header_geometry_still_passes(self):
        setup = '''
for path in "$ROOT_DIFF" "$SCRATCH_DIFF" "$DATASET_DIFF"; do
    printf '\\211KDXTS1\\n' > "$path"
    truncate -s 12288 "$path"
done
'''
        result, _ = self.run_case(setup, assertions='''
assert_diff "$ROOT_DIFF" 8192
assert_diff "$SCRATCH_DIFF" 8192
assert_diff "$DATASET_DIFF" 8192
''')
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_final_assertion_block_is_unchanged(self):
        suffix = SOURCE[SOURCE.index('assert_diff() {'):]
        self.assertEqual(hashlib.sha256(suffix.encode()).hexdigest(),
                         '95ae89125d09aed81cdfa6f46a021b9785c9dc1dc3632298b3ada80b5cb47629')

    def test_production_budget_and_assertions_remain_wired(self):
        self.assertIn('wait_active_diffs 120 "$ROOT_DIFF" "$SCRATCH_DIFF" "$DATASET_DIFF"', SOURCE)
        for token in ['assert_diff "$ROOT_DIFF"', 'assert_diff "$SCRATCH_DIFF"',
                      'assert_diff "$DATASET_DIFF"', 'ROOT_TEMPLATE_SHA',
                      'SCRATCH_TEMPLATE_SHA', 'DATASET_TEMPLATE_SHA',
                      '[ "$HEADERS" -eq 3 ]', 'guest plaintext marker appears']:
            self.assertIn(token, SOURCE)
        self.assertIn('scripts/test_e2e_encrypted_diff.py', (ROOT / 'Makefile').read_text())


if __name__ == '__main__':
    unittest.main()
