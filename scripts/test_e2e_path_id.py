#!/usr/bin/env python3
"""Exercise the shipped PathID checks with controlled asynchronous publication."""
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
CASE = ROOT / "test/e2e/cases/sandbox.path-id.sh"


class PathIDPublicationTest(unittest.TestCase):
    def run_checks(self, phase, mode, source=None):
        source = source or CASE.read_text()
        helper = ""
        if "wait_diff_paths() {" in source:
            helper = source[source.index("wait_diff_paths() {"):source.index('\nSID_A=')]
        if phase == "cold":
            checks = source[source.index('[ -S "$RUN_ROOT/a/ctl.sock" ]'):
                            source.index('\n"$BIN/sandbox-ctl" exec --path-id a')]
            path_id, sandbox_id = "a", "logical-phase-a-123"
        else:
            marker = "-- /bin/sh -c 'grep -q PATH-ID-OK /scratch/path-id-marker'\n"
            checks = source[source.index(marker) + len(marker):
                            source.index('\nRESTORE_SNAPSHOT_OUT=')]
            path_id, sandbox_id = "c", "logical-phase-c-123"

        with tempfile.TemporaryDirectory(prefix="path-id-publication-") as directory:
            root = Path(directory)
            run = root / "run" / path_id
            base = root / "base" / path_id
            run.mkdir(parents=True)
            base.mkdir(parents=True)
            for suffix in ("overlay", "disk0"):
                (base / f"{sandbox_id}.{suffix}.diff.partial").write_text("prepared")
            if mode == "ready":
                for path in base.glob("*.partial"):
                    path.rename(path.with_suffix(""))
            if mode == "wrong-identity":
                (base / f"{path_id}.overlay.diff").touch()
                (base / f"{path_id}.disk0.diff").touch()
            if mode == "directory":
                (base / f"{sandbox_id}.overlay.diff").mkdir()
                (base / f"{sandbox_id}.disk0.diff").mkdir()
            with socket.socket(socket.AF_UNIX) as ctl:
                ctl.bind(str(run / "ctl.sock"))
                prelude = r"""
set -euo pipefail
RUN_PID=$$
if [ "$MODE" = dead ]; then RUN_PID=999999999; fi
SID_A=logical-phase-a-123
SID_C=logical-phase-c-123
e2e_fail() { echo "$*" >&2; exit 1; }
# No wall-clock race: the first exec-ready checks run before publication,
# then root and data publish at different polling boundaries.
polls=0
sleep() {
    polls=$((polls + 1))
    echo "$polls" > "$POLL_COUNT"
    if [ "$polls" = 2 ] && { [ "$MODE" = delayed ] || [ "$MODE" = missing-data ]; }; then
        mv "$EXPECTED.overlay.diff.partial" "$EXPECTED.overlay.diff"
    fi
    if [ "$polls" = 4 ] && { [ "$MODE" = delayed ] || [ "$MODE" = missing-root ]; }; then
        mv "$EXPECTED.disk0.diff.partial" "$EXPECTED.disk0.diff"
    fi
}
"""
                env = {k: v for k, v in os.environ.items() if k not in ("BASH_ENV", "ENV")}
                result = subprocess.run(
                    ["bash", "-c", prelude + helper + checks],
                    env={**env, "RUN_ROOT": str(root / "run"), "BASE_ROOT": str(root / "base"),
                         "EXPECTED": str(base / sandbox_id), "MODE": mode,
                         "POLL_COUNT": str(root / "polls")},
                    capture_output=True, text=True, timeout=5)
            polls = int((root / "polls").read_text()) if (root / "polls").exists() else 0
            return result, polls

    def test_delayed_root_and_data_publication(self):
        for phase in ("cold", "restore"):
            with self.subTest(phase=phase):
                result, polls = self.run_checks(phase, "delayed")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(polls, 4, "both exact final files must be published")

    def test_already_published_diffs_need_no_wait(self):
        for phase in ("cold", "restore"):
            result, polls = self.run_checks(phase, "ready")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(polls, 0)

    def test_partial_missing_wrong_identity_and_directory_still_fail(self):
        for phase in ("cold", "restore"):
            for mode in ("partial", "missing-root", "missing-data", "wrong-identity", "directory"):
                with self.subTest(phase=phase, mode=mode):
                    result, polls = self.run_checks(phase, mode)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(polls, 90, "publication failure must remain bounded")

    def test_exited_sandbox_fails_without_polling(self):
        for phase in ("cold", "restore"):
            result, polls = self.run_checks(phase, "dead")
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(polls, 0)


if __name__ == "__main__":
    unittest.main()
