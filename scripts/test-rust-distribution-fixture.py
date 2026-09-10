"""Synthetic TLS response bytes for tests; never imported by production code."""
import hashlib
import io
import json
import os
import tarfile


def rust_distribution_fixture(version="1.0.0", commit="3" * 40, host="x86_64-unknown-linux-gnu"):
    fields = {"release": version, "commit-hash": commit, "host": host}
    name = "rustc-" + version + "-" + host
    base = os.environ.get("RUSTUP_DIST_SERVER", "https://static.rust-lang.org").rstrip("/") + "/dist/"
    manifest_url = base + "channel-rust-" + version + ".toml"
    archive_url = base + "2025-08-07/" + name + ".tar.xz"
    notices = {"COPYRIGHT-library.html": b"fixture Rust standard library copyright\n",
               "licenses/Apache-2.0.txt": b"fixture Rust toolchain license\n"}
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:xz") as archive:
        for relative, contents in notices.items():
            member = tarfile.TarInfo(name + "/rustc/share/doc/rust/" + relative)
            member.size = len(contents)
            archive.addfile(member, io.BytesIO(contents))
    archive_bytes = output.getvalue()
    manifest = ('manifest-version="2"\ndate="2025-08-07"\n[pkg.rustc]\nversion='
                + json.dumps(version + " (" + commit[:9] + " 2025-08-04)") + '\ngit_commit_hash='
                + json.dumps(commit) + '\n[pkg.rustc.target.' + host + ']\navailable=true\nxz_url='
                + json.dumps(archive_url) + '\nxz_hash="' + hashlib.sha256(archive_bytes).hexdigest() + '"\n').encode()
    return fields, notices, {manifest_url: manifest, archive_url: archive_bytes}


def fixture_urlopen(url, timeout):
    _, _, responses = rust_distribution_fixture()
    return io.BytesIO(responses[url])
