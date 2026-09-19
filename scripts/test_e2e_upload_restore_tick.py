#!/usr/bin/env python3
"""Exercise the upload/restore fixture's real snapshot shell sequences, without KVM."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

SHELL_ENV = {name: value for name, value in os.environ.items()
             if name not in ("BASH_ENV", "ENV")}

FIXTURE = Path(os.environ.get(
    "UPLOAD_RESTORE_FIXTURE",
    Path(__file__).resolve().parents[1] / "test/e2e/e2e_sandbox_upload_restore.sh",
))


class SnapshotTickTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = FIXTURE.read_text()
        match = re.search(r"(?ms)^snapshot_tick\(\) \{.*?^\}", cls.source)
        cls.helper = match.group(0) if match else ""

    def test_snapshot_phases_read_drained_output(self):
        phases = (
            ("PRE_SNAP_TICK=", "# ---- restore from manifest://", "WANT_TICK"),
            ("SNAP2_LOG=", "# ---- phase 4:", "WANT_TICK2"),
            ("SNAP3_LOG=", "DIFF_RESTORE3=", "WANT_TICK3"),
        )
        for start, end, target in phases:
            with self.subTest(target=target), tempfile.TemporaryDirectory() as work:
                work = Path(work)
                binary = work / "sandbox-ctl"
                binary.write_text(
                    "#!/usr/bin/env bash\n"
                    "printf 'TICK 17 DISK blk0=TICK00000000\\n' >> \"$TEST_LOG\"\n"
                    "printf '%064d\\n' 1\n"
                )
                binary.chmod(0o755)
                begin = self.source.index(start)
                sequence = self.source[begin:self.source.index(end, begin)]
                target_line = next(line for line in self.source.splitlines()
                                   if line.startswith(target + "="))
                setup = r'''
WORK=$TEST_WORK
BIN=$WORK
BLK0_OK='DISK blk0=TICK00000000'
LOG1=$WORK/source.log; LOG2=$LOG1; LOG3=$LOG1
export TEST_LOG=$LOG1
SID1=source; SID2=source; SID3=source
RESTORE_MS=1; RESTORE2_MS=1
printf 'TICK 10 %s\n' "$BLK0_OK" > "$LOG1"
assert_cgroup_init() { printf 'TICK 14 %s\n' "$BLK0_OK" >> "$LOG1"; }
uffd_performance_gate() { :; }
mkfifo "$WORK/drain"
# Final output arrives only when the parent enters wait; no timing sleeps.
( read -r _ < "$WORK/drain"; printf 'TICK 22 %s\n' "$BLK0_OK" >> "$LOG1" ) &
source_pid=$!
trap 'kill "$source_pid" 2>/dev/null || :; builtin wait "$source_pid" 2>/dev/null || :' EXIT
SBPID1=$source_pid; SBPID2=$source_pid; SBPID3=$source_pid
source_shell=$BASHPID
wait() {
    # A command-substitution subshell cannot wait for the parent's child.
    [[ $BASHPID == "$source_shell" ]] || return 1
    printf 'drain\n' > "$WORK/drain"
    builtin wait "$@"
}
'''
                checks = f'''
{target_line}
printf 'target=%s\\n' "${target}"
[[ ${target} == 25 ]]
'''
                result = subprocess.run(
                    ["bash", "-euo", "pipefail", "-c",
                     setup + self.helper + "\n" + sequence + checks],
                    env={**SHELL_ENV, "TEST_WORK": str(work)},
                    capture_output=True, text=True, timeout=5,
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_missing_or_invalid_final_record_fails(self):
        self.assertTrue(self.helper, "fixture has no final-record validator")
        cases = (
            "",
            "TICK nope DISK blk0=TICK00000000\n",
            "TICK 22 DISK blk0=CORRUPTED\n",
            "TICK 21 DISK blk0=TICK00000000\nTICK 22 DISK blk0=CORRUPTED\n",
            "TICK 22\nDISK blk0=TICK00000000\n",
        )
        for content in cases:
            with self.subTest(content=content), tempfile.TemporaryDirectory() as work:
                log = Path(work) / "source.log"
                log.write_text(content)
                result = subprocess.run(
                    ["bash", "-euo", "pipefail", "-c",
                     "BLK0_OK='DISK blk0=TICK00000000'\n" + self.helper +
                     '\nsnapshot_tick "$1"', "test", str(log)],
                    env=SHELL_ENV, capture_output=True, text=True, timeout=5,
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("missing or invalid final TICK", result.stderr)


if __name__ == "__main__":
    unittest.main()
