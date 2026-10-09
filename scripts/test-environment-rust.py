#!/usr/bin/env python3
"""The selected Cargo/rustc, not an unrelated rustup, own target support."""
import os
import io
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "native-deps/deps/build-cloud-hypervisor.sh"


class RustEnvironment(unittest.TestCase):
    def run_build(self, rustup=False, cargo_failure=False, jobs=None):
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
if os.environ.get('EXPECT_CARGO_JOBS'):
    assert os.environ['CARGO_BUILD_JOBS'] == os.environ['EXPECT_CARGO_JOBS']
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
            if jobs is not None:
                env.update(KUASAR_BUILD_JOBS=jobs, CARGO_BUILD_JOBS="160", EXPECT_CARGO_JOBS=jobs)
            result = subprocess.run([shutil.which("bash"), str(SCRIPT)], env=env,
                                    text=True, capture_output=True, timeout=10)
            self.assertFalse((root / "rustup-called").exists(), result.stdout + result.stderr)
            if jobs is not None and (not jobs.isdecimal() or jobs == "0"):
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("KUASAR_BUILD_JOBS must be a positive integer", result.stderr)
                self.assertFalse((root / "output/cloud-hypervisor").exists())
            elif cargo_failure:
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

    def test_task_budget_reaches_the_actual_cargo_invocation(self):
        self.run_build(jobs="2")

    def test_invalid_task_budget_does_not_build(self):
        for jobs in ("0", "-1", "auto"):
            with self.subTest(jobs=jobs):
                self.run_build(jobs=jobs)


class SyntheticSourceIdentity(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="rust-source-identity-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.archive = self.root / "source.tar.gz"
        content = b'[package]\nname = "fixture"\nversion = "0.0.0"\n'
        with tarfile.open(self.archive, "w:gz") as archive:
            entry = tarfile.TarInfo("fixture/Cargo.toml")
            entry.size = len(content)
            entry.mode = 0o644
            archive.addfile(entry, io.BytesIO(content))

    def environment(self, name, **overrides):
        root = self.root / name
        env = {key: value for key, value in os.environ.items()
               if key not in ("GIT_AUTHOR_DATE", "GIT_COMMITTER_DATE", "SOURCE_DATE_EPOCH")}
        env.update(STAGE="fetch", CLOUD_HYPERVISOR_TARBALL=str(self.archive),
                   BUILD_DIR=str(root / "build"), CH_SRC=str(root / "source"),
                   CH_BUILD_OUT=str(root / "output"), PATCHES_DIR=str(root / "patches"),
                   GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
        env.update(overrides)
        return env

    def run_stage(self, env):
        result = subprocess.run(["bash", str(SCRIPT)], env=env, text=True,
                                capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return subprocess.check_output(["git", "-C", env["CH_SRC"], "show", "-s",
                                        "--format=%H%n%at%n%ct%n%s", "HEAD"],
                                       env=env, text=True).splitlines()

    def test_fresh_source_imports_ignore_task_paths_and_wall_clock(self):
        first = self.run_stage(self.environment("first-task"))
        second = self.run_stage(self.environment("second-task"))
        self.assertEqual(first, second)
        self.assertEqual(first[1:], ["0", "0", "import source.tar.gz"])

    def test_source_date_epoch_and_explicit_dates_remain_inputs(self):
        epoch = self.run_stage(self.environment("epoch", SOURCE_DATE_EPOCH="1700000000"))
        self.assertEqual(epoch[1:3], ["1700000000", "1700000000"])
        explicit = self.run_stage(self.environment("explicit", SOURCE_DATE_EPOCH="1700000000",
                                  GIT_AUTHOR_DATE="@1700000010 +0000",
                                  GIT_COMMITTER_DATE="@1700000020 +0000"))
        self.assertEqual(explicit[1:3], ["1700000010", "1700000020"])
        self.assertNotEqual(epoch[0], explicit[0])

    def test_patch_committer_is_stable_and_original_author_date_is_retained(self):
        commits = []
        for name in ("first-task", "second-task"):
            env = self.environment(name)
            self.run_stage(env)
            patches = Path(env["PATCHES_DIR"])
            patches.mkdir()
            (patches / "0001-fixture.patch").write_text('''From 1111111111111111111111111111111111111111 Mon Sep 17 00:00:00 2001
From: Fixture Author <fixture@example.invalid>
Date: Tue, 14 Nov 2023 22:13:20 +0000
Subject: [PATCH] fixture update

---
 Cargo.toml | 2 +-
 1 file changed, 1 insertion(+), 1 deletion(-)

diff --git a/Cargo.toml b/Cargo.toml
--- a/Cargo.toml
+++ b/Cargo.toml
@@ -1,3 +1,3 @@
 [package]
 name = "fixture"
-version = "0.0.0"
+version = "0.0.1"
''')
            env["STAGE"] = "patches-apply"
            commits.append(self.run_stage(env))
        self.assertEqual(commits[0], commits[1])
        self.assertEqual(commits[0][1:3], ["1700000000", "0"])


if __name__ == "__main__":
    unittest.main()
