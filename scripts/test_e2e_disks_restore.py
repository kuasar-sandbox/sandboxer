#!/usr/bin/env python3
"""Execute multi-disk restore fixture preparation without starting a VM."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[1] / "test/e2e/e2e_sandbox_disks.sh"


class RestoreDiskPreparationTest(unittest.TestCase):
    def check_restore(self, start_marker, end_marker, suffix):
        source = SOURCE.read_text()
        start = source.index(start_marker)
        preparation = source[start:source.index(end_marker, start)]
        with tempfile.TemporaryDirectory(prefix="restore-disks-") as directory:
            work = Path(directory)
            # Record formatter invocation without requiring e2fsprogs. The
            # real preparation must create sparse uppers inheriting the image.
            script = "set -euo pipefail\n"
            script += 'mkfs.ext4() { printf "%s\\n" "$*" >> "$WORK/formats"; }\n'
            script += preparation
            result = subprocess.run(["bash", "-c", script],
                                    env=dict(os.environ, WORK=directory),
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertFalse((work / "formats").exists(),
                             "restore preparation must not format captured disks")
            for disk, size in (("root", 512 << 20), ("dataset", 256 << 20)):
                path = work / f"{disk}-{suffix}.ext4"
                metadata = path.stat()
                self.assertEqual(metadata.st_size, size)
                self.assertEqual(metadata.st_blocks, 0)
                with path.open("rb") as stream:
                    self.assertEqual(stream.read(4096), bytes(4096))

    def test_base_restore_inherits_filesystem(self):
        self.check_restore('echo "==> [6] restore + verify persistence"',
                           'cat > "$WORK/restore.yaml"', "r")

    def test_working_set_restore_inherits_filesystem(self):
        self.check_restore("# Restore W with fresh writable uppers.",
                           'cat > "$WORK/restore-w.yaml"', "w")


if __name__ == "__main__":
    unittest.main()
