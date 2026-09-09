#!/usr/bin/env python3
"""Private fixtures for native source-material validation; not MicroVM tests."""
import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("materials", Path(__file__).with_name("release-rust-materials.py"))
materials = importlib.util.module_from_spec(spec)
spec.loader.exec_module(materials)


class MaterialsTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="rust-material-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = self.root / "source"
        self.source.mkdir()
        self.home = self.root / "cargo"
        self.archive = self.home / "registry/cache/fixture/fixture-1.0.0.crate"
        self.archive.parent.mkdir(parents=True)
        with tarfile.open(self.archive, "w:gz") as archive:
            for name, value in (("Cargo.toml", b'[package]\nname="fixture"\nversion="1.0.0"\n'),
                                ("LICENSE", b"fixture license\n"), ("src/lib.rs", b"pub fn fixture() {}\n")):
                member = tarfile.TarInfo("fixture-1.0.0/" + name)
                member.size = len(value)
                archive.addfile(member, io.BytesIO(value))
        self.registry = "registry+https://github.com/rust-lang/crates.io-index"
        self.package = {"name": "fixture", "version": "1.0.0", "id": self.registry + "#fixture@1.0.0",
                        "manifest_path": str(self.home / "registry/src/fixture/fixture-1.0.0/Cargo.toml"),
                        "source": self.registry, "license_file": None}
        self.metadata = {"packages": [self.package]}
        self.lock = {"package": [{"name": "fixture", "version": "1.0.0", "source": self.registry,
                                  "checksum": hashlib.sha256(self.archive.read_bytes()).hexdigest()}]}
        self.messages = [{"reason": "compiler-artifact", "package_id": self.package["id"],
                          "target": {"name": "cloud-hypervisor"}, "executable": "/fixture/cloud-hypervisor"},
                         {"reason": "build-finished", "success": True}]

    def collect(self, metadata=None, messages=None, lock=None):
        return materials.collect(metadata or self.metadata, "\n".join(map(json.dumps, messages or self.messages)),
                                 lock or self.lock, self.home, self.source, self.root / "stage")

    def test_archive_license_and_namespace(self):
        rows = self.collect()
        self.assertEqual(rows[0][1], "rust-build-input:fixture")
        label = "source-" + hashlib.sha256(self.registry.encode()).hexdigest()
        path = self.root / "stage/share/licenses/sandboxer/rust/fixture@1.0.0" / label / "LICENSE"
        self.assertEqual(path.read_text(), "fixture license\n")
        self.assertEqual(path.stat().st_mode & 0o777, 0o644)

    def test_changed_archive_rejected(self):
        with self.archive.open("ab") as archive:
            archive.write(b"changed")
        with self.assertRaisesRegex(ValueError, "checksum"):
            self.collect()

    def test_extracted_cache_not_used(self):
        extracted = Path(self.package["manifest_path"]).parent
        extracted.mkdir(parents=True)
        (extracted / "LICENSE").write_text("changed extracted cache")
        self.test_archive_license_and_namespace()

    def test_relative_and_absolute_declared_license_paths(self):
        for declared in ("LICENSE", str(Path(self.package["manifest_path"]).parent / "LICENSE")):
            metadata = copy.deepcopy(self.metadata)
            metadata["packages"][0]["license_file"] = declared
            self.collect(metadata=metadata)
        metadata["packages"][0]["license_file"] = "../LICENSE"
        with self.assertRaisesRegex(ValueError, "unsafe"):
            self.collect(metadata=metadata)

    def test_other_lock_checksum_rejected(self):
        lock = copy.deepcopy(self.lock)
        lock["package"][0]["checksum"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "checksum"):
            self.collect(lock=lock)

    def test_unshipped_test_filename_is_not_a_license_path(self):
        with tarfile.open(self.archive, "w:gz") as archive:
            for name in ("LICENSE", "tests/!complex-expressions/example.rs"):
                contents = b"fixture\n"
                member = tarfile.TarInfo("fixture-1.0.0/" + name)
                member.size = len(contents)
                archive.addfile(member, io.BytesIO(contents))
        self.lock["package"][0]["checksum"] = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        rows = self.collect()
        self.assertFalse((self.root / "stage" / rows[0][5] / "tests").exists())

    def test_license_traversal_still_rejected(self):
        with tarfile.open(self.archive, "w:gz") as archive:
            member = tarfile.TarInfo("fixture-1.0.0/../LICENSE")
            member.size = 8
            archive.addfile(member, io.BytesIO(b"fixture\n"))
        self.lock["package"][0]["checksum"] = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        with self.assertRaisesRegex(ValueError, "unsafe"):
            self.collect()

    def test_vhost_workspace_licenses_match_the_published_manifest(self):
        archive = self.root / "vhost-0.14.0.crate"
        original = b'[package]\nname="vhost"\nversion="0.14.0"\n'
        vcs = json.dumps({"git": {"sha1": "1" * 40}, "path_in_vcs": "vhost"}).encode()
        with tarfile.open(archive, "w:gz") as output:
            for name, value in ((".cargo_vcs_info.json", vcs), ("Cargo.toml.orig", original)):
                member = tarfile.TarInfo("vhost-0.14.0/" + name)
                member.size = len(value)
                output.addfile(member, io.BytesIO(value))
        package = {"name": "vhost", "version": "0.14.0", "repository": "https://github.com/rust-vmm/vhost"}
        destination = self.root / "vhost-material"
        with patch.object(materials, "urlopen", side_effect=lambda url, timeout: io.BytesIO(
                original if url.endswith("vhost/Cargo.toml") else b"fixture upstream license\n")):
            identity = materials.vhost_workspace_materials(package, archive, destination)
        self.assertEqual(identity, ";license-source-git:" + "1" * 40)
        self.assertTrue((destination / "upstream/LICENSE-BSD-3-Clause").is_file())
        with patch.object(materials, "urlopen", return_value=io.BytesIO(b"different manifest")):
            with self.assertRaisesRegex(ValueError, "differs"):
                materials.vhost_workspace_materials(package, archive, destination)

    def test_failed_build_and_missing_package_rejected(self):
        messages = copy.deepcopy(self.messages)
        messages[-1]["success"] = False
        with self.assertRaisesRegex(ValueError, "successful"):
            self.collect(messages=messages)
        with self.assertRaisesRegex(ValueError, "omits"):
            self.collect(metadata={"packages": []})

    def test_local_dependency_escape_rejected(self):
        metadata = copy.deepcopy(self.metadata)
        metadata["packages"][0]["source"] = None
        with self.assertRaisesRegex(ValueError, "escaped"):
            self.collect(metadata=metadata)

    def test_git_commit_and_tracked_license(self):
        git_root = self.root / "git"
        git_root.mkdir()
        def git(*args):
            return subprocess.check_output(["git", "-C", str(git_root), *args], text=True).strip()
        git("init", "-q")
        git("config", "--local", "user.name", "Chen Xiaohui")
        git("config", "--local", "user.email", "graych@gmail.com")
        (git_root / "LICENSE").write_text("fixture Git license\n")
        (git_root / "nested").mkdir()
        (git_root / "nested/Cargo.toml").write_text('[package]\nname="fixture"\nversion="1.0.0"\n')
        git("add", "--", "LICENSE", "nested/Cargo.toml")
        git("commit", "-qm", "test: create Rust source fixture")
        commit = git("rev-parse", "HEAD")
        package = {**self.package, "manifest_path": str(git_root / "nested/Cargo.toml")}
        locked = {"source": "git+https://example.invalid/fixture?branch=main#" + commit}
        destination = self.root / "git-material"
        materials.git_materials(package, locked, destination)
        self.assertEqual((destination / "LICENSE").read_text(), "fixture Git license\n")
        (git_root / "LICENSE").write_text("changed")
        with self.assertRaisesRegex(ValueError, "dirty"):
            materials.git_materials(package, locked, destination)
        with self.assertRaisesRegex(ValueError, "differs"):
            materials.git_materials(package, {"source": "git+https://example.invalid/fixture#" + "0" * 40}, destination)

    def test_same_name_version_with_distinct_sources_keeps_both_licenses(self):
        git_root = self.root / "git-fork"
        git_root.mkdir()
        def git(*args):
            return subprocess.check_output(["git", "-C", str(git_root), *args], text=True).strip()
        git("init", "-q")
        git("config", "--local", "user.name", "Chen Xiaohui")
        git("config", "--local", "user.email", "graych@gmail.com")
        (git_root / "LICENSE").write_text("distinct Git fork license\n")
        (git_root / "Cargo.toml").write_text('[package]\nname="fixture"\nversion="1.0.0"\n')
        git("add", "--", "LICENSE", "Cargo.toml")
        git("commit", "-qm", "test: create same-name Cargo source fixture")
        source = "git+https://example.invalid/fixture#" + git("rev-parse", "HEAD")
        package = {**self.package, "id": source, "source": source,
                   "manifest_path": str(git_root / "Cargo.toml")}
        metadata = {"packages": [self.package, package]}
        lock = {"package": [*self.lock["package"], {"name": "fixture", "version": "1.0.0", "source": source}]}
        messages = [*self.messages, {"reason": "compiler-artifact", "package_id": source}]
        rows = self.collect(metadata, messages, lock)
        self.assertEqual(len({row[5] for row in rows}), 2)
        licenses = {(self.root / "stage" / row[5] / "LICENSE").read_text() for row in rows}
        self.assertEqual(licenses, {"fixture license\n", "distinct Git fork license\n"})

    def test_cargo_configuration_excludes_credentials(self):
        original, destination = self.root / "original-home", self.root / "private-home"
        original.mkdir()
        (original / "config.toml").write_text(
            '[source.crates-io]\nreplace-with="public-mirror"\n'
            '[source.public-mirror]\nregistry="sparse+https://mirror.example.invalid/index/"\n'
            '[registry]\ntoken="fixture credential must not be copied"\n'
            '[build]\nrustc-wrapper="/fixture/wrapper"\n')
        materials.configure_cargo_home(original, destination)
        copied = (destination / "config.toml").read_text()
        self.assertIn("public-mirror", copied)
        self.assertNotIn("token", copied)
        self.assertNotIn("wrapper", copied)
        self.assertEqual((destination / "config.toml").stat().st_mode & 0o777, 0o600)
        self.assertEqual(destination.stat().st_mode & 0o777, 0o700)

    def test_build_environment_does_not_inherit_credentials(self):
        environment = materials.native_build_environment(
            {"PATH": "/usr/bin", "CARGO_BUILD_JOBS": "2", "GH_TOKEN": "fixture",
             "CARGO_NET_GIT_FETCH_WITH_CLI": "false",
             "GOPROXY": "https://proxy.example.invalid,direct",
             "CARGO_REGISTRIES_CRATES_IO_TOKEN": "fixture", "AWS_SECRET_ACCESS_KEY": "fixture",
             "CARGO_REGISTRY_CREDENTIAL_PROVIDER": "fixture", "SSH_AUTH_SOCK": "/fixture"},
            self.root / "home", self.home, Path("/toolchain/bin/rustc"))
        self.assertEqual(environment["RUSTC"], "/toolchain/bin/rustc")
        self.assertEqual(environment["CARGO_BUILD_JOBS"], "2")
        self.assertEqual(environment["CARGO_NET_GIT_FETCH_WITH_CLI"], "false")
        self.assertEqual(environment["GOPROXY"], "https://proxy.example.invalid,direct")
        self.assertNotIn("fixture", environment.values())
        self.assertNotIn("SSH_AUTH_SOCK", environment)
        for name in ("RUSTC", "RUSTC_WRAPPER", "CARGO_BUILD_RUSTC"):
            with self.assertRaisesRegex(ValueError, "does not accept"):
                materials.native_build_environment({name: "fixture"}, self.root / "home", self.home,
                                                   Path("/toolchain/bin/rustc"))
        with self.assertRaisesRegex(ValueError, "Git transport"):
            materials.native_build_environment({"CARGO_NET_GIT_FETCH_WITH_CLI": "invalid"},
                                               self.root / "home", self.home, Path("/toolchain/bin/rustc"))
        with self.assertRaisesRegex(ValueError, "Go proxy"):
            materials.native_build_environment({"GOPROXY": "https://fixture:fixture@proxy.example.invalid"},
                                               self.root / "home", self.home, Path("/toolchain/bin/rustc"))

    def test_cargo_directory_and_authenticated_registry_rejected(self):
        original, destination = self.root / "original-home", self.root / "private-home"
        original.mkdir()
        for value in ('[source.cache]\ndirectory="/fixture/cache"\n',
                      '[source.cache]\nregistry="sparse+https://fixture:fixture@mirror.example.invalid/"\n'):
            (original / "config.toml").write_text(value)
            with self.assertRaises(ValueError):
                materials.configure_cargo_home(original, destination)

    def rust_fixture(self):
        sysroot = self.root / "rust"
        library = sysroot / "lib/rustlib/x86_64-unknown-linux-gnu/lib/libstd-fixture.rlib"
        library.parent.mkdir(parents=True)
        library.write_bytes(b"fixture linked Rust standard library")
        rustc = sysroot / "bin/rustc"
        rustc.parent.mkdir()
        rustc.write_bytes(b"fixture compiler")
        link_map = self.root / "rust.map"
        link_map.write_text("LOAD " + str(library) + "\n")
        return sysroot, rustc, library, link_map

    def test_linked_standard_library_digest_tracks_actual_bytes(self):
        sysroot, _, library, link_map = self.rust_fixture()
        stage = self.root / "stage"
        first = materials.rust_standard_library_inventory(stage, sysroot, link_map)
        with link_map.open("a") as output:
            output.write("LOAD " + str(self.root / "removed-rustc-temporary/intermediate.rlib") + "\n")
        self.assertEqual(first, materials.rust_standard_library_inventory(stage, sysroot, link_map))
        inventory = stage / "share/sources/sandboxer/RUST-STDLIB.tsv"
        self.assertIn(hashlib.sha256(library.read_bytes()).hexdigest(), inventory.read_text())
        self.assertNotIn(str(self.root), inventory.read_text())
        self.assertEqual(inventory.stat().st_mode & 0o777, 0o644)
        library.write_bytes(b"locally replaced standard library")
        self.assertNotEqual(first, materials.rust_standard_library_inventory(stage, sysroot, link_map))
        link_map.write_text("LOAD relative.rlib\n")
        with self.assertRaisesRegex(ValueError, "relative"):
            materials.rust_standard_library_inventory(stage, sysroot, link_map)
        link_map.write_text("LOAD " + str(self.archive) + "\n")
        with self.assertRaisesRegex(ValueError, "omits"):
            materials.rust_standard_library_inventory(stage, sysroot, link_map)

    def test_lto_inventory_keeps_the_target_stdlib_input_set(self):
        sysroot, _, library, link_map = self.rust_fixture()
        builtins = library.with_name("libcompiler_builtins-fixture.rlib")
        builtins.write_bytes(b"fixture compiler builtins")
        link_map.write_text("LOAD " + str(builtins) + "\n")
        stage = self.root / "stage"
        first = materials.rust_standard_library_inventory(stage, sysroot, link_map)
        inventory = (stage / "share/sources/sandboxer/RUST-STDLIB.tsv").read_text()
        self.assertIn(library.name, inventory)
        self.assertIn(builtins.name, inventory)
        library.write_bytes(b"changed bitcode used before the final linker")
        self.assertNotEqual(first, materials.rust_standard_library_inventory(stage, sysroot, link_map))
        other = sysroot / "lib/rustlib/aarch64-unknown-linux-gnu/lib/libstd-fixture.rlib"
        other.parent.mkdir(parents=True)
        other.write_bytes(b"other target")
        with link_map.open("a") as output:
            output.write("LOAD " + str(other) + "\n")
        with self.assertRaisesRegex(ValueError, "mixes"):
            materials.rust_standard_library_inventory(stage, sysroot, link_map)

    def test_debian_toolchain_uses_matching_source_package_not_rpm(self):
        sysroot, rustc, _, link_map = self.rust_fixture()
        copyright = self.root / "debian/copyright"
        copyright.parent.mkdir()
        copyright.write_text("fixture Debian Rust standard library copyright\n")
        def query(command, **_):
            if command == [str(rustc), "-vV"]:
                return "release: 1.89.0\ncommit-hash: " + "1" * 40 + "\n"
            if command == [str(rustc), "--print", "sysroot"]:
                return str(sysroot) + "\n"
            self.assertEqual(command[0], "dpkg-query")
            if command[1] == "-S":
                return "rustc: " + str(rustc) + "\n"
            if command[1:3] == ["-L", "libstd-rust:amd64"]:
                return str(copyright) + "\n"
            if command[1] == "-W" and command[-1] == "rustc":
                return "rustc\t1.89.0+fixture\n"
            if command[1] == "-W":
                return ("installed\tlibstd-rust:amd64\trustc\t1.89.0+fixture\n"
                        "installed\tother-rust\trustc\t0.0.0+unrelated\n")
            self.fail("unexpected package query: " + repr(command))
        with patch.object(materials.shutil, "which", side_effect=lambda name: "/usr/bin/dpkg-query"
                          if name == "dpkg-query" else None), \
                patch.object(materials.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)), \
                patch.object(materials.subprocess, "check_output", side_effect=query):
            row = materials.rust_toolchain_materials(self.root / "stage", rustc, link_map)
        self.assertEqual(row[3], "deb-source:rustc@1.89.0+fixture")
        self.assertIn(";target-stdlib-sha256:", row[4])
        copied = self.root / "stage" / row[5] / str(copyright).lstrip("/")
        self.assertEqual(copied.read_text(), copyright.read_text())

    def test_unknown_toolchain_layout_has_actionable_error(self):
        sysroot, rustc, _, link_map = self.rust_fixture()
        with patch.object(materials.shutil, "which", return_value=None), \
                patch.object(materials.subprocess, "check_output", side_effect=[
                    "release: 1.89.0\ncommit-hash: " + "1" * 40 + "\n", str(sysroot) + "\n"]):
            with self.assertRaisesRegex(ValueError, "install rust-docs"):
                materials.rust_toolchain_materials(self.root / "stage", rustc, link_map)


if __name__ == "__main__":
    unittest.main()
