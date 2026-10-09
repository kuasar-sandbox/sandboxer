#!/usr/bin/env python3
"""The Workbench split retains the existing source-check commands and failures."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class SourceCheckModes(unittest.TestCase):
    def invoke(self, *arguments, failure=""):
        with tempfile.TemporaryDirectory(prefix="source-check-modes-") as directory:
            root = Path(directory)
            (root / "scripts").mkdir()
            (root / "tools").mkdir()
            shutil.copyfile(Path(__file__).with_name("ci-source-checks.sh"), root / "scripts/ci-source-checks.sh")
            for name, command in (("tools/make", "make"), ("tools/go", "go"),
                                  ("scripts/test-vhost-tmpfs-runner.sh", "offline-launch-check"),
                                  ("scripts/test-vhost-tmpfs-enospc.sh", "privileged-enospc")):
                script = root / name
                script.write_text('#!/bin/sh\n'
                                  f'printf "%s\\n" "{command}:$*:CGO=${{CGO_ENABLED:-}}" >> "$TRACE"\n'
                                  f'[ "$FAIL" != "{command}" ] || exit 17\n')
                script.chmod(0o755)
            trace = root / "trace"
            result = subprocess.run(["bash", str(root / "scripts/ci-source-checks.sh"), *arguments],
                                    env={**os.environ, "PATH": str(root / "tools") + os.pathsep + os.environ["PATH"],
                                         "CGO_ENABLED": "", "TRACE": str(trace), "FAIL": failure},
                                    text=True, capture_output=True, timeout=10)
            return result, trace.read_text().splitlines() if trace.exists() else []

    def test_default_preserves_all_steps_and_order(self):
        result, commands = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(commands, ["make:test-e2e-scripts:CGO=", "go:test -count=1 ./...:CGO=0",
                                    "offline-launch-check::CGO=", "privileged-enospc::CGO=",
                                    "go:test -race -count=1 ./...:CGO=1", "go:vet ./...:CGO=0"])

    def test_modes_partition_the_existing_commands_without_skips(self):
        ordinary, ordinary_commands = self.invoke("--ordinary")
        privileged, privileged_commands = self.invoke("--privileged")
        self.assertEqual(ordinary.returncode, 0, ordinary.stderr)
        self.assertEqual(privileged.returncode, 0, privileged.stderr)
        _, all_commands = self.invoke()
        self.assertEqual(privileged_commands, ["privileged-enospc::CGO="])
        self.assertEqual(ordinary_commands, [command for command in all_commands if command not in privileged_commands])

    def test_failures_propagate(self):
        for mode, command in (("--ordinary", "go"), ("--privileged", "privileged-enospc")):
            with self.subTest(mode=mode):
                result, commands = self.invoke(mode, failure=command)
                self.assertEqual(result.returncode, 17, result.stderr)
                self.assertTrue(commands[-1].startswith(command + ":"))

    def test_unknown_or_multiple_modes_do_not_run_checks(self):
        for arguments in (("--skip",), ("--ordinary", "--privileged")):
            with self.subTest(arguments=arguments):
                result, commands = self.invoke(*arguments)
                self.assertEqual(result.returncode, 2)
                self.assertFalse(commands)


if __name__ == "__main__":
    unittest.main()
