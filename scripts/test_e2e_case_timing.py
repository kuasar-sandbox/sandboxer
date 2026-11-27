#!/usr/bin/env python3
"""Exercise proof-phase timing and exit handling without starting a VM."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

HELPER = Path(__file__).resolve().parents[1] / "test/e2e/lib/case_timing.sh"
ENV = {key: value for key, value in os.environ.items() if key not in ("BASH_ENV", "ENV")}


class CaseTimingTests(unittest.TestCase):
    def exercise(self, body):
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run(["bash", "-euo", "pipefail", "-c", 'source "$1"\n' + body,
                                     "test", str(HELPER)], env=ENV | {"OUT": directory},
                                    capture_output=True, text=True, timeout=5)
            path = Path(directory) / "phases.jsonl"
            rows = [json.loads(line) for line in path.read_text().splitlines()] if path.is_file() else []
            return result, rows

    def test_begin_closes_the_previous_phase_without_losing_the_last_one(self):
        result, rows = self.exercise('case_phase_begin prepare\ncase_phase_begin trim-v4-xts\ncase_phase_end 0\ncase_phase_end 0\n')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual([row["phase"] for row in rows], ["prepare", "trim-v4-xts"])
        self.assertTrue(all(row["exit_code"] == 0 and row["wall_seconds"] > 0 for row in rows))

    def test_cleanup_records_the_failed_phase_and_preserves_failure_status(self):
        result, rows = self.exercise('''cleanup() {
    local status=$?
    set +e
    case_phase_end "$status"
    exit "$status"
}
trap cleanup EXIT
case_phase_begin branch-and-delete
exit 23
''')
        self.assertEqual(result.returncode, 23)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["phase"], "branch-and-delete")
        self.assertEqual(rows[0]["exit_code"], 23)

    def test_record_write_failure_propagates_and_does_not_start_another_phase(self):
        result, rows = self.exercise('case_phase_begin pending-memory\nmkdir "$OUT/phases.jsonl"\ncase_phase_begin portable\n')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(rows, [])


if __name__ == "__main__":
    unittest.main()
