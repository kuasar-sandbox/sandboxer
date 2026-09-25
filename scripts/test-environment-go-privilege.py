#!/usr/bin/env python3
"""The source-build stage selects Go; prepared product cases never select a compiler."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class PreparedProbeBoundary(unittest.TestCase):
    def check_source_build(self, explicit):
        with tempfile.TemporaryDirectory(prefix="probe-driver-") as directory:
            work = Path(directory)
            driver = work / "go"
            driver.write_text('''#!/usr/bin/python3
import json,os,pathlib,sys
pathlib.Path(os.environ["DRIVER_LOG"]).write_text(json.dumps({"args":sys.argv[1:],"env":{k:os.environ.get(k) for k in ("GOROOT","GOTOOLCHAIN","GOOS","GOARCH","GOWORK","CGO_ENABLED")}}))
out=pathlib.Path(sys.argv[sys.argv.index("-o")+1]);out.write_bytes(b"source fixture output");out.chmod(0o755)
''')
            driver.chmod(0o755)
            env = {**os.environ, "PATH": str(work) + os.pathsep + os.environ["PATH"],
                   "GOROOT": str(work / "environment-root"), "GOTOOLCHAIN": "go1.99.1+path", "DRIVER_LOG": str(work / "driver.json")}
            command = ["make", "--no-print-directory", "e2e-usage-probe", "TARGET_ARCH=x86_64", f"E2E_FIXTURE_DIR={work / 'out'}"]
            if explicit:
                command.append(f"GO={driver}")
            result = subprocess.run(command, cwd=ROOT, env=env, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            record = json.loads((work / "driver.json").read_text())
            self.assertEqual(record["env"], {"GOROOT": env["GOROOT"], "GOTOOLCHAIN": env["GOTOOLCHAIN"],
                                            "GOOS": "linux", "GOARCH": "amd64", "GOWORK": "off", "CGO_ENABLED": "0"})
            self.assertEqual(record["args"][-1], "test/fixtures/usageprobe/main.go")
            self.assertTrue((work / "out/usage-probe").is_file())

    def test_environment_path_driver_is_preserved_in_source_build(self):
        self.check_source_build(False)

    def test_explicit_environment_driver_is_preserved_in_source_build(self):
        self.check_source_build(True)

    def test_compiler_and_legacy_entrypoints_are_not_product_inputs(self):
        for path in sorted((ROOT / "test/e2e/cases").glob("*.sh")):
            text = path.read_text()
            with self.subTest(case=path.name):
                self.assertNotRegex(text, r"\b(?:go|cargo)\s+(?:build|run|test)\b")
                self.assertNotIn("run_all.sh", text)
                self.assertNotIn("e2e_usage.sh", text)
        self.assertFalse((ROOT / "test/e2e/run_all.sh").exists())
        self.assertFalse(list((ROOT / "test/e2e").glob("e2e_*.sh")))


if __name__ == "__main__":
    unittest.main()
