#!/usr/bin/env python3
"""Private fixtures for native source-material validation; not MicroVM tests."""
import copy
from contextlib import contextmanager, redirect_stderr
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import unittest
from unittest.mock import patch
from urllib.error import HTTPError, URLError

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


    def test_duplicate_registry_cache_namespaces_use_locked_archive(self):
        duplicate = self.home / "registry/cache/another-index" / self.archive.name
        duplicate.parent.mkdir(parents=True)
        duplicate.write_bytes(self.archive.read_bytes())
        self.test_archive_license_and_namespace()
        duplicate.write_bytes(b"unrelated cached archive")
        self.test_archive_license_and_namespace()

    def test_packager_resolves_bare_rustc_using_path(self):
        tools = self.root / "tools"
        tools.mkdir()
        compiler = tools / "selected-rustc"
        compiler.write_text("#!/bin/sh\nexit 0\n")
        compiler.chmod(0o755)
        source = Path(__file__).with_name("release.sh").read_text()
        assignment = next(line for line in source.splitlines() if line.strip().startswith("rustc_path="))
        for selected in ("selected-rustc", str(compiler)):
            env = dict(os.environ, RUSTC=selected, PATH=str(tools) + os.pathsep + os.environ["PATH"])
            result = subprocess.run(["bash", "-c", 'set -e; ' + assignment +
                                     '\nprintf "%s\n" "$rustc_path"'],
                                    env=env, text=True, capture_output=True, check=True)
            self.assertEqual(result.stdout.strip(), str(compiler))

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

    def vhost_fixture(self, commit="1" * 40):
        archive = self.root / "vhost-0.14.0.crate"
        original = b'[package]\nname="vhost"\nversion="0.14.0"\n'
        vcs = json.dumps({"git": {"sha1": commit}, "path_in_vcs": "vhost"}).encode()
        with tarfile.open(archive, "w:gz") as output:
            for name, value in ((".cargo_vcs_info.json", vcs), ("Cargo.toml.orig", original)):
                member = tarfile.TarInfo("vhost-0.14.0/" + name)
                member.size = len(value)
                output.addfile(member, io.BytesIO(value))
        package = {"name": "vhost", "version": "0.14.0", "repository": "https://github.com/rust-vmm/vhost"}
        destination = self.root / "vhost-material"
        return package, archive, destination, original

    @staticmethod
    def vhost_response(url, original):
        return original if url.endswith("vhost/Cargo.toml") else b"fixture upstream license\n"

    def test_vhost_workspace_licenses_match_the_published_manifest(self):
        package, archive, destination, original = self.vhost_fixture()
        with patch.object(materials, "_request_vhost_material", side_effect=lambda url, timeout: self.vhost_response(url, original)):
            identity = materials.vhost_workspace_materials(package, archive, destination, self.home)
        self.assertEqual(identity, ";license-source-git:" + "1" * 40)
        self.assertTrue((destination / "upstream/LICENSE-BSD-3-Clause").is_file())
        with patch.object(materials, "_request_vhost_material", side_effect=lambda url, timeout: self.vhost_response(url, b"different manifest")):
            with self.assertRaisesRegex(ValueError, "differs"):
                materials.vhost_workspace_materials(package, archive, destination, self.root / "fresh-cargo")

    def test_vhost_warm_cache_works_offline_in_a_fresh_material_directory(self):
        package, archive, destination, original = self.vhost_fixture()
        with patch.object(materials, "_request_vhost_material", side_effect=lambda url, timeout: self.vhost_response(url, original)) as request:
            identity = materials.vhost_workspace_materials(package, archive, destination, self.home)
        self.assertEqual(request.call_count, 3)
        fresh = self.root / "fresh-job"
        fresh.mkdir()
        restored_home = fresh / "cargo"
        shutil.copytree(self.home, restored_home)
        restored_archive = fresh / archive.name
        shutil.copyfile(archive, restored_archive)
        archive.unlink()
        with patch.object(materials, "_request_vhost_material", side_effect=AssertionError("offline: network forbidden")):
            restored = fresh / "materials"
            self.assertEqual(materials.vhost_workspace_materials(package, restored_archive, restored, restored_home), identity)
        for name in ("LICENSE", "LICENSE-BSD-3-Clause"):
            self.assertEqual((restored / "upstream" / name).read_bytes(), (destination / "upstream" / name).read_bytes())

    def test_vhost_corrupt_or_wrong_source_cache_is_rejected_without_network(self):
        package, archive, destination, original = self.vhost_fixture()
        with patch.object(materials, "_request_vhost_material", side_effect=lambda url, timeout: self.vhost_response(url, original)):
            materials.vhost_workspace_materials(package, archive, destination, self.home)
        cache, = (self.home / "release-materials/vhost").glob("*/*.json")
        valid = json.loads(cache.read_bytes())
        for failure in ("json", "repository", "commit", "crate_sha256", "url", "checksum", "manifest", "partial"):
            with self.subTest(failure=failure):
                record = copy.deepcopy(valid)
                if failure in ("repository", "commit", "crate_sha256"):
                    record["identity"][failure] = "wrong-source"
                elif failure == "url":
                    record["files"]["LICENSE"]["url"] = "https://example.invalid/LICENSE"
                elif failure == "checksum":
                    record["files"]["LICENSE"]["sha256"] = "0" * 64
                elif failure == "manifest":
                    value = b"different manifest"
                    record["files"]["vhost/Cargo.toml"]["contents"] = materials.base64.b64encode(value).decode("ascii")
                    record["files"]["vhost/Cargo.toml"]["sha256"] = hashlib.sha256(value).hexdigest()
                elif failure == "partial":
                    del record["files"]["LICENSE-BSD-3-Clause"]
                cache.write_text("{" if failure == "json" else json.dumps(record))
                before = cache.read_bytes()
                with patch.object(materials, "_request_vhost_material", side_effect=AssertionError("invalid cache must fail closed")):
                    with self.assertRaisesRegex(ValueError, "invalid vhost material cache"):
                        materials.vhost_workspace_materials(package, archive, self.root / "rejected", self.home)
                self.assertEqual(cache.read_bytes(), before)
                self.assertFalse((self.root / "rejected").exists())

    def test_vhost_wrong_repository_or_non_exact_commit_never_downloads(self):
        package, archive, destination, _ = self.vhost_fixture(commit="main")
        with patch.object(materials, "_request_vhost_material", side_effect=AssertionError("no download for unbound source")):
            with self.assertRaisesRegex(ValueError, "unverifiable"):
                materials.vhost_workspace_materials(package, archive, destination, self.home)
            with self.assertRaisesRegex(ValueError, "no license"):
                materials.vhost_workspace_materials({**package, "repository": "https://example.invalid/vhost"},
                                                     archive, destination, self.home)

    def test_vhost_partial_download_does_not_publish_cache_or_licenses(self):
        package, archive, destination, original = self.vhost_fixture()
        def request(url, timeout):
            if url.endswith("LICENSE-BSD-3-Clause"):
                raise HTTPError(url, 404, "not found", {}, None)
            return self.vhost_response(url, original)
        with patch.object(materials, "_request_vhost_material", side_effect=request) as request_mock:
            with self.assertRaisesRegex(ValueError, "LICENSE-BSD-3-Clause.*404"):
                materials.vhost_workspace_materials(package, archive, destination, self.home)
        self.assertEqual(request_mock.call_count, 3)  # A permanent 404 is not retried.
        self.assertFalse(destination.exists())
        self.assertFalse(list((self.home / "release-materials").rglob("*")))

    def test_vhost_transient_failure_and_finite_attempts_have_request_diagnostics(self):
        url = "https://raw.githubusercontent.com/rust-vmm/vhost/" + "1" * 40 + "/LICENSE"
        log = io.StringIO()
        with patch.object(materials, "_request_vhost_material", side_effect=[URLError("temporary DNS failure"), self.vhost_response(url, b"")]) as request, \
                patch.object(materials.time, "sleep"), redirect_stderr(log):
            self.assertEqual(materials.download_vhost_material(url, time.monotonic() + 10), b"fixture upstream license\n")
        self.assertEqual(request.call_count, 2)
        self.assertIn(url, log.getvalue())
        self.assertIn("attempt=1", log.getvalue())
        self.assertIn("temporary DNS failure", log.getvalue())
        with patch.object(materials, "_request_vhost_material", side_effect=URLError("still unavailable")) as request, \
                patch.object(materials.time, "sleep"):
            with self.assertRaisesRegex(ValueError, "after 3 attempt"):
                materials.download_vhost_material(url, time.monotonic() + 10)
        self.assertEqual(request.call_count, 3)

    def test_vhost_redirect_is_not_relabelled_as_the_selected_source(self):
        attempts = []
        def respond(handler):
            attempts.append(handler.path)
            if handler.path == "/LICENSE":
                handler.send_response(302)
                handler.send_header("Location", "/different-source")
                handler.end_headers()
            else:
                handler.send_response(200)
                handler.end_headers()
                handler.wfile.write(b"wrong-source license")
        with self.vhost_http_server(respond) as url:
            with self.assertRaisesRegex(ValueError, "redirected from exact source"):
                materials.download_vhost_material(url, time.monotonic() + 2)
        self.assertEqual(attempts, ["/LICENSE", "/different-source"])

    @contextmanager
    def vhost_http_server(self, responder):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                responder(self)

            def log_message(self, *_args):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        worker = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
        worker.start()
        try:
            yield "http://127.0.0.1:" + str(server.server_port) + "/LICENSE"
        finally:
            server.shutdown()
            server.server_close()
            worker.join(timeout=2)
            self.assertFalse(worker.is_alive())

    def test_vhost_real_http_transient_error_recovers(self):
        attempts = []
        def respond(handler):
            attempts.append(handler.path)
            handler.send_response(503 if len(attempts) == 1 else 200)
            handler.end_headers()
            handler.wfile.write(b"real HTTP fixture license")
        with self.vhost_http_server(respond) as url, patch.object(materials.time, "sleep"):
            self.assertEqual(materials.download_vhost_material(url, time.monotonic() + 2), b"real HTTP fixture license")
        self.assertEqual(attempts, ["/LICENSE", "/LICENSE"])

    def test_vhost_real_slow_body_cannot_exceed_total_budget(self):
        # Bytes arrive within the socket inactivity timeout, but the complete
        # request must still stop at the remaining material-download budget.
        requested = threading.Event()
        def respond(handler):
            requested.set()
            handler.send_response(200)
            handler.send_header("Content-Length", "100")
            handler.end_headers()
            try:
                for _ in range(100):
                    handler.wfile.write(b"x")
                    handler.wfile.flush()
                    threading.Event().wait(0.02)
            except (BrokenPipeError, ConnectionResetError):
                pass
        with self.vhost_http_server(respond) as url, patch.object(materials, "VHOST_REQUEST_TIMEOUT", 1):
            started = time.monotonic()
            with self.assertRaisesRegex(ValueError, "budget exhausted"):
                materials.download_vhost_material(url, started + 0.4)
            self.assertLess(time.monotonic() - started, 1.25)
            self.assertTrue(requested.is_set(), "request must enter the slow HTTP body fixture")

    def test_vhost_stuck_resolver_process_is_killed_and_reaped_at_budget(self):
        # Patch only the child's resolver. The real worker still enters urlopen,
        # while the real parent must kill and wait for this owned OS process.
        marker = self.root / "resolver-pid"
        real_popen = subprocess.Popen
        requests = []
        block_resolver = """import os,signal,socket,sys,time
def blocked_resolver(*args, **kwargs):
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    with open(sys.argv[4], "w") as output:
        output.write(str(os.getpid()))
    while True:
        time.sleep(60)
socket.getaddrinfo = blocked_resolver
"""
        def spawn(command, **kwargs):
            self.assertEqual(command[:4], [sys.executable, "-I", "-B", "-c"])
            self.assertNotIn("GH_TOKEN", kwargs["env"])
            self.assertNotIn("GITHUB_TOKEN", kwargs["env"])
            child = real_popen([*command[:4], block_resolver + command[4], *command[5:], str(marker)], **kwargs)
            requests.append(child)
            return child
        def cleanup():
            for child in requests:
                if child.poll() is None:
                    child.kill()
                child.wait()
        self.addCleanup(cleanup)
        url = "https://raw.githubusercontent.com/rust-vmm/vhost/" + "1" * 40 + "/LICENSE"
        log = io.StringIO()
        started = time.monotonic()
        with patch.object(materials.subprocess, "Popen", side_effect=spawn), \
                patch.dict(os.environ, {"GH_TOKEN": "fixture-do-not-forward", "GITHUB_TOKEN": "fixture-do-not-forward"}), \
                redirect_stderr(log):
            with self.assertRaisesRegex(ValueError, "budget exhausted.*request process reaped"):
                materials.download_vhost_material(url, started + 0.6)
        self.assertLess(time.monotonic() - started, 2)
        self.assertEqual(len(requests), 1)
        self.assertEqual(int(marker.read_text()), requests[0].pid)
        self.assertEqual(requests[0].returncode, -signal.SIGKILL)
        with self.assertRaises(ChildProcessError):
            os.waitpid(requests[0].pid, os.WNOHANG)
        self.assertIn(url, log.getvalue())
        self.assertIn("attempt=1", log.getvalue())
        self.assertIn("remaining=0.000s", log.getvalue())

    def test_vhost_real_partial_http_body_is_rejected(self):
        attempts = []
        def respond(handler):
            attempts.append(handler.path)
            handler.send_response(200)
            handler.send_header("Content-Length", "100")
            handler.end_headers()
            handler.wfile.write(b"partial")
        with self.vhost_http_server(respond) as url, patch.object(materials.time, "sleep"):
            with self.assertRaisesRegex(ValueError, "after 3 attempt.*bytes read"):
                materials.download_vhost_material(url, time.monotonic() + 2)
        self.assertEqual(attempts, ["/LICENSE"] * 3)

    def test_vhost_material_requests_share_one_total_budget(self):
        package, archive, destination, original = self.vhost_fixture()
        deadlines = []
        def download(url, deadline):
            deadlines.append(deadline)
            if len(deadlines) == 3:
                raise ValueError("vhost material download budget exhausted: " + url)
            return original if url.endswith("vhost/Cargo.toml") else b"fixture license"
        with patch.object(materials, "download_vhost_material", side_effect=download):
            with self.assertRaisesRegex(ValueError, "budget exhausted.*LICENSE-BSD-3-Clause"):
                materials.vhost_workspace_materials(package, archive, destination, self.home)
        self.assertEqual(len(deadlines), 3)
        self.assertEqual(len(set(deadlines)), 1)
        self.assertFalse(destination.exists())

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

    def test_git_declared_notices_come_from_the_locked_tree(self):
        git_root = self.root / "git-declared"
        crate = git_root / "nested"
        (crate / "legal").mkdir(parents=True)
        def git(*args):
            return subprocess.check_output(["git", "-C", str(git_root), *args], text=True).strip()
        git("init", "-q")
        git("config", "--local", "user.name", "Chen Xiaohui")
        git("config", "--local", "user.email", "graych@gmail.com")
        (git_root / "LICENSE").write_text("fixture root notice\n")
        (git_root / "root-terms.txt").write_text("fixture workspace terms\n")
        (crate / "legal/MIT.txt").write_text("fixture declared license\n")
        (crate / "Cargo.toml").write_text('[package]\nname="fixture"\nversion="1.0.0"\nlicense-file="legal/MIT.txt"\n')
        (crate / "legal/link.txt").symlink_to("MIT.txt")
        git("add", "--", "LICENSE", "root-terms.txt", "nested")
        git("commit", "-qm", "test: declare nonstandard Git crate notices")
        locked = {"source": "git+https://example.invalid/fixture#" + git("rev-parse", "HEAD")}
        package = {**self.package, "manifest_path": str(crate / "Cargo.toml")}
        for declared, relative, contents in (
                ("legal/MIT.txt", "nested/legal/MIT.txt", "fixture declared license\n"),
                (str(crate / "legal/MIT.txt"), "nested/legal/MIT.txt", "fixture declared license\n"),
                ("../root-terms.txt", "root-terms.txt", "fixture workspace terms\n")):
            with self.subTest(declared=declared):
                destination = self.root / "declared-material"
                materials.git_materials({**package, "license_file": declared}, locked, destination)
                self.assertEqual((destination / relative).read_text(), contents)
        for declared, error in (("legal/missing.txt", "not a tracked"),
                                ("legal/link.txt", "invalid Git crate material"),
                                ("../../outside.txt", "escapes"),
                                (str(self.root / "outside.txt"), "escapes")):
            with self.subTest(declared=declared), self.assertRaisesRegex(ValueError, error):
                materials.git_materials({**package, "license_file": declared}, locked, self.root / "rejected")
        git("update-index", "--assume-unchanged", "nested/legal/MIT.txt")
        (crate / "legal/MIT.txt").write_text("modified cache must not become license authority")
        destination = self.root / "locked-notice"
        materials.git_materials({**package, "license_file": "legal/MIT.txt"}, locked, destination)
        self.assertEqual((destination / "nested/legal/MIT.txt").read_text(), "fixture declared license\n")

    def rust_fixture(self):
        sysroot = self.root / "rust"
        sysroot.mkdir()
        rustc = sysroot / "bin/rustc"
        rustc.parent.mkdir(parents=True)
        rustc.write_bytes(b"fixture compiler")
        return sysroot, rustc

    def test_debian_toolchain_uses_matching_source_package_not_rpm(self):
        sysroot, rustc = self.rust_fixture()
        copyright = self.root / "debian/copyright"
        copyright.parent.mkdir()
        copyright.write_text("fixture Debian Rust standard library copyright\n")
        owned_target = copyright.with_name("COPYING")
        copyright.rename(owned_target)
        copyright.symlink_to(owned_target)
        def query(command, **_):
            if command == [str(rustc), "-vV"]:
                return "release: 1.89.0\ncommit-hash: " + "1" * 40 + "\n"
            if command == [str(rustc), "--print", "sysroot"]:
                return str(sysroot) + "\n"
            self.assertEqual(command[0], "dpkg-query")
            if command[1] == "-S":
                if command[-1] == str(owned_target):
                    return "libstd-rust:amd64: " + str(owned_target) + "\n"
                return "rustc: " + str(rustc) + "\n"
            if command[1:3] == ["-L", "libstd-rust:amd64"]:
                return str(copyright) + "\n"
            if command[1] == "-W" and command[-1] in ("rustc", "libstd-rust:amd64"):
                return "rustc\t1.89.0+fixture\n"
            if command[1] == "-W":
                return ("installed\tlibstd-rust:amd64\trustc\t1.89.0+fixture\n"
                        "installed\tother-rust\trustc\t0.0.0+unrelated\n")
            self.fail("unexpected package query: " + repr(command))
        with patch.object(materials.shutil, "which", side_effect=lambda name: "/usr/bin/dpkg-query"
                          if name == "dpkg-query" else None), \
                patch.object(materials.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)), \
                patch.object(materials.subprocess, "check_output", side_effect=query):
            row = materials.rust_toolchain_materials(self.root / "stage", rustc)
        self.assertEqual(row[3], "deb-source:rustc@1.89.0+fixture")
        self.assertEqual(row[4], "git:" + "1" * 40)
        copied = self.root / "stage" / row[5] / str(copyright).lstrip("/")
        self.assertEqual(copied.read_text(), copyright.read_text())
        self.assertFalse(copied.is_symlink())
    def test_rustup_notices_use_selected_installed_materials_without_downloads(self):
        sysroot, rustc = self.rust_fixture()
        docs = sysroot / "share/doc/rust"
        (docs / "licenses").mkdir(parents=True)
        notices = {"COPYRIGHT-library.html": b"installed library copyright",
                   "licenses/Apache-2.0.txt": b"installed license text"}
        for relative, contents in notices.items():
            (docs / relative).write_bytes(contents)
        with patch.object(materials.subprocess, "check_output", side_effect=[
                "release: 1.89.0\ncommit-hash: " + "1" * 40 + "\n", str(sysroot) + "\n"]):
            row = materials.rust_toolchain_materials(self.root / "stage", rustc)
        for relative, contents in notices.items():
            self.assertEqual((self.root / "stage" / row[5] / relative).read_bytes(), contents)
        self.assertEqual(row[4], "git:" + "1" * 40)
        self.assertFalse((self.root / "stage/share/sources/sandboxer/RUST-NOTICES.tsv").exists())
        self.assertFalse((self.root / "stage/share/sources/sandboxer/RUST-STDLIB.tsv").exists())

    def test_unknown_toolchain_layout_has_actionable_error(self):
        sysroot, rustc = self.rust_fixture()
        with patch.object(materials.shutil, "which", return_value=None), \
                patch.object(materials.subprocess, "check_output", side_effect=[
                    "release: 1.89.0\ncommit-hash: " + "1" * 40 + "\n", str(sysroot) + "\n"]):
            with self.assertRaisesRegex(ValueError, "restore the selected rustup rustc component"):
                materials.rust_toolchain_materials(self.root / "stage", rustc)


if __name__ == "__main__":
    unittest.main()
