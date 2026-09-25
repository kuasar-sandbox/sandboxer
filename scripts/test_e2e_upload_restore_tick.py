#!/usr/bin/env python3
"""Exercise the rewritten snapshot sequences, including parent-shell output drain."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
CASES = ROOT / "test/e2e/cases"
ENV = {k: v for k, v in os.environ.items() if k not in ("BASH_ENV", "ENV")}


def function(source, name):
    match = re.search(r"(?ms)^" + name + r"\(\) \{.*?^\}", source)
    assert match, name
    return match.group()


class SnapshotTickTests(unittest.TestCase):
    def test_snapshot_phases_read_drained_output(self):
        phases = [("snapshot.remote-roundtrip.sh", 1, "SNAP1_TICK")]
        phases += [("snapshot.remote-chain.sh", n, f"TICK{n}") for n in (1, 2, 3)]
        for name, ordinal, counter in phases:
            source = (CASES / name).read_text()
            begin = source.index(f"SNAP{ordinal}=$(")
            end = re.search(r"(?m)^" + counter + r'=\$\(last_tick[^\n]+', source[begin:])
            self.assertIsNotNone(end)
            sequence = source[begin:begin + end.end()]
            helpers = function(source, "last_tick")
            if "snapshot_upload" in sequence:
                helpers += "\n" + function(source, "snapshot_upload")
                use = next(line for line in source.splitlines() if "wait_tick " in line and f"{counter} + 3" in line)
            else:
                use = next(line for line in source.splitlines() if line.startswith("WANT=$((SNAP1_TICK"))
                use += '\n[[ "$WANT" == 25 ]]'
            with self.subTest(case=name, snapshot=ordinal), tempfile.TemporaryDirectory() as directory:
                work = Path(directory)
                binary = work / "sandbox-ctl"
                binary.write_text('#!/bin/bash\nprintf "TICK 17 DISK blk0=TICK00000000\\n" >> "$TEST_LOG"\nprintf "%064d\\n" 1\n')
                binary.chmod(0o755)
                setup = r'''
WORK=$TEST_WORK; BIN=$WORK; OUT=$WORK; RUN_ROOT=$WORK/run
LOG1=$WORK/source.log; LOG2=$LOG1; LOG3=$LOG1; LOG4=$LOG1
export TEST_LOG=$LOG1
SID1=source; SID2=source; SID3=source
printf 'TICK 10 DISK blk0=TICK00000000\n' > "$LOG1"
mkfifo "$WORK/drain"
( read -r _ < "$WORK/drain"; printf 'TICK 22 DISK blk0=TICK00000000\n' >> "$LOG1" ) &
source_pid=$!
STORE_PID=$source_pid
PID1=$source_pid; PID2=$source_pid; PID3=$source_pid; PID4=$source_pid
PIDS=("$source_pid")
trap 'kill "$source_pid" 2>/dev/null || :; builtin wait "$source_pid" 2>/dev/null || :' EXIT
source_shell=$BASHPID
wait() {
    [[ $BASHPID == "$source_shell" ]] || return 1
    printf 'drain\n' > "$WORK/drain"
    builtin wait "$@"
}
e2e_fail() { echo "$*" >&2; exit 1; }
wait_tick() { [[ "$2" == 25 ]]; }
'''
                result = subprocess.run(["bash", "-euo", "pipefail", "-c", setup + helpers + "\n" + sequence + "\n" + use],
                                        env={**ENV, "TEST_WORK": directory}, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_missing_or_invalid_final_record_fails(self):
        invalid = ("", "TICK nope DISK blk0=TICK00000000\n", "TICK 22 DISK blk0=CORRUPTED\n",
                   "TICK 21 DISK blk0=TICK00000000\nTICK 22 DISK blk0=CORRUPTED\n",
                   "TICK 22\nDISK blk0=TICK00000000\n")
        for name in ("snapshot.remote-roundtrip.sh", "snapshot.remote-chain.sh"):
            helper = function((CASES / name).read_text(), "last_tick")
            for content in invalid:
                with self.subTest(case=name, content=content), tempfile.TemporaryDirectory() as directory:
                    path = Path(directory) / "source.log"
                    path.write_text(content)
                    result = subprocess.run(["bash", "-euo", "pipefail", "-c", helper + '\nlast_tick "$1"', "test", str(path)],
                                            env=ENV, capture_output=True, text=True, timeout=5)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("missing or invalid final TICK", result.stderr)


if __name__ == "__main__":
    unittest.main()
