#!/usr/bin/env python3
"""Run current base/working-snapshot sparse-disk preparation without a VM."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

CASES = Path(__file__).resolve().parents[1] / "test/e2e/cases"


class RestoreDiskPreparationTest(unittest.TestCase):
    def check_restore(self, case, root_name, data_name, end):
        source = (CASES / case).read_text()
        start = source.index(f'truncate -s 512M "$WORK/{root_name}"')
        block = source[start:source.index(end, start)]
        with tempfile.TemporaryDirectory(prefix="restore-disks-") as directory:
            work = Path(directory)
            script = 'set -euo pipefail\nmkfs.ext4() { echo invoked >> "$WORK/formats"; }\n' + block
            env = {k: v for k, v in os.environ.items() if k not in ("BASH_ENV", "ENV")}
            result = subprocess.run(["bash", "-c", script], env={**env, "WORK": directory}, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertFalse((work / "formats").exists(), "restore must not format captured filesystem state")
            for name, size in ((root_name, 512 << 20), (data_name, 256 << 20)):
                path = work / name
                self.assertEqual(path.stat().st_size, size)
                self.assertEqual(path.stat().st_blocks, 0)
                with path.open("rb") as stream:
                    self.assertEqual(stream.read(4096), bytes(4096))

    def test_base_restore_inherits_filesystem(self):
        self.check_restore("snapshot.disks.sh", "root-restore.ext4", "dataset-restore.ext4", 'cat >"$WORK/restore.yaml"')

    def test_working_parent_restore_inherits_filesystem(self):
        self.check_restore("snapshot.memory-residency.sh", "r1-root.ext4", "r1-data.ext4", 'cat >"$WORK/restore1.yaml"')

    def test_working_set_restore_inherits_filesystem(self):
        self.check_restore("snapshot.memory-residency.sh", "r2-root.ext4", "r2-data.ext4", 'cat >"$WORK/restore2.yaml"')


if __name__ == "__main__":
    unittest.main()
