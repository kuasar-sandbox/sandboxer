#!/usr/bin/env python3
"""Exercise real Cargo freshness with the CH recipe and an offline tiny crate."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "native-deps/deps/build-cloud-hypervisor.sh"


class CargoRetryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="ch-cargo-retry-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.source = self.root / "source"
        (self.source / "src").mkdir(parents=True)
        (self.source / "Cargo.toml").write_text(
            '[package]\nname = "cloud-hypervisor"\nversion = "0.0.0"\nedition = "2021"\n')
        (self.source / "Cargo.lock").write_text(
            'version = 4\n\n[[package]]\nname = "cloud-hypervisor"\nversion = "0.0.0"\n')
        (self.source / "src/main.rs").write_text('fn main() { println!("offline CH recipe fixture"); }\n')
        tools = self.root / "tools"
        tools.mkdir()
        wrapper = tools / "cp"
        wrapper.write_text('''#!/usr/bin/env python3
import os, sys
if os.environ.get("CH_FIXTURE_COPY_FAILURE") == "1":
    print("injected CH publish-copy failure", file=sys.stderr)
    sys.exit(23)
os.execv(os.environ["CH_FIXTURE_REAL_CP"], [os.environ["CH_FIXTURE_REAL_CP"], *sys.argv[1:]])
''')
        wrapper.chmod(0o755)
        self.env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ["PATH"],
                        STAGE="build", CH_SRC=str(self.source), CH_BUILD_OUT=str(self.root / "target"),
                        BINDIR=str(self.root / "bin"), RUST_TARGET="", CROSS_PREFIX="",
                        CARGO_NET_OFFLINE="true", CH_FIXTURE_REAL_CP=shutil.which("cp"))
        self.binary = self.root / "bin/cloud-hypervisor"
        self.report = self.root / "target/build-report.jsonl"
        self.link_map = self.root / "target/link.map"
        self.env.update(CH_BUILD_REPORT=str(self.report), CH_LINK_MAP=str(self.link_map))

    def build(self, fail=False):
        return subprocess.run(["bash", str(SCRIPT)], env={**self.env,
                              "CH_FIXTURE_COPY_FAILURE": "1" if fail else "0"},
                              text=True, capture_output=True, timeout=120)

    def assert_materials(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(self.binary.is_file())
        self.assertTrue(self.link_map.stat().st_size)
        self.assertFalse((self.root / "target/link.map.pending").exists())
        report = [json.loads(line) for line in self.report.read_text().splitlines()]
        self.assertTrue(any(row.get("reason") == "build-finished" and row.get("success") for row in report))
        self.assertTrue(any(row.get("reason") == "compiler-artifact"
                            and row.get("target", {}).get("name") == "cloud-hypervisor"
                            for row in report))

    def test_real_cargo_relinks_after_successful_compile_failed_to_publish(self):
        failed = self.build(fail=True)
        self.assertEqual(failed.returncode, 23, failed.stdout + failed.stderr)
        self.assertIn("injected CH publish-copy failure", failed.stderr)
        self.assertEqual(self.report.stat().st_size, 0)
        self.assertFalse((self.root / "target/link.map.pending").exists())
        self.assert_materials(self.build())
        before = [hashlib.sha256(path.read_bytes()).hexdigest()
                  for path in (self.binary, self.report, self.link_map)]
        reused = self.build()
        self.assertEqual(reused.returncode, 0, reused.stdout + reused.stderr)
        self.assertIn("build materials retained", reused.stdout)
        self.assertEqual(before, [hashlib.sha256(path.read_bytes()).hexdigest()
                                 for path in (self.binary, self.report, self.link_map)])

    def test_missing_local_map_does_not_reuse_an_incomplete_output_set(self):
        self.assert_materials(self.build())
        self.link_map.unlink()
        self.assert_materials(self.build())


if __name__ == "__main__":
    unittest.main()
