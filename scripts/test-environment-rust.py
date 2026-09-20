#!/usr/bin/env python3
"""The selected Cargo/rustc, not an unrelated rustup, own target support."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "native-deps/deps/build-cloud-hypervisor.sh"


class RustEnvironment(unittest.TestCase):
    def run_build(self, rustup=False, cargo_failure=False):
        with tempfile.TemporaryDirectory(prefix="rust-environment-") as directory:
            root = Path(directory)
            tools = root / "tools"
            source = root / "source"
            tools.mkdir()
            source.mkdir()
            (source / "Cargo.toml").write_text('[package]\nname="fixture"\nversion="0.0.0"\n')
            for name in ("bash", "dirname", "mkdir", "awk", "tr", "mktemp", "env", "tee",
                         "cp", "chmod", "mv", "du", "cut", "head", "rm", "python3"):
                (tools / name).symlink_to(shutil.which(name))
            (tools / "file").write_text("#!/bin/sh\necho fixture-output\n")
            (tools / "rustc").write_text('#!/bin/sh\necho "host: x86_64-unknown-linux-gnu"\n')
            (tools / "aarch64-linux-gnu-gcc").write_text('#!/bin/sh\nexit 0\n')
            (tools / "cargo").write_text('''#!/usr/bin/env python3
import os, sys
from pathlib import Path
if os.environ.get('FAIL_CARGO') == '1':
    print('selected compiler rejected target', file=sys.stderr)
    sys.exit(42)
args = sys.argv[1:]
assert '--target' in args and args[args.index('--target') + 1] == 'aarch64-unknown-linux-gnu'
assert os.environ['CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER'] == 'aarch64-linux-gnu-gcc'
link = next(arg for arg in args if arg.startswith('link-arg=-Wl,-Map,')).split(',', 2)[2]
Path(link).write_text('fixture link map\\n')
binary = Path(os.environ['CARGO_TARGET_DIR']) / 'aarch64-unknown-linux-gnu/release/cloud-hypervisor'
binary.parent.mkdir(parents=True)
binary.write_text('#!/bin/sh\\nexit 0\\n')
print('{"reason":"compiler-artifact"}')
''')
            if rustup:
                (tools / "rustup").write_text('#!/bin/sh\necho called > "$RUSTUP_PROBE"\nexit 89\n')
            for path in tools.iterdir():
                if not path.is_symlink():
                    path.chmod(0o755)
            env = dict(os.environ, PATH=str(tools), STAGE="build", CH_SRC=str(source),
                       CH_BUILD_OUT=str(root / "build"), BINDIR=str(root / "output"),
                       RUST_TARGET="aarch64-unknown-linux-gnu", CROSS_PREFIX="aarch64-linux-gnu-",
                       FAIL_CARGO="1" if cargo_failure else "0", RUSTUP_PROBE=str(root / "rustup-called"))
            result = subprocess.run([shutil.which("bash"), str(SCRIPT)], env=env,
                                    text=True, capture_output=True, timeout=10)
            self.assertFalse((root / "rustup-called").exists(), result.stdout + result.stderr)
            if cargo_failure:
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("selected compiler rejected target", result.stderr)
                self.assertFalse((root / "output/cloud-hypervisor").exists())
            else:
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertTrue((root / "output/cloud-hypervisor").is_file())

    def test_cross_build_without_rustup(self):
        self.run_build()

    def test_unrelated_rustup_does_not_veto_environment_compiler(self):
        self.run_build(rustup=True)

    def test_actual_target_failure_is_not_hidden(self):
        self.run_build(rustup=True, cargo_failure=True)


if __name__ == "__main__":
    unittest.main()
