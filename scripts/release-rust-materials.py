#!/usr/bin/env python3
"""Collect materials for Cargo packages observed in the actual CH build.

Registry licenses are read from checksum-verified .crate archives, never from
an editable extracted Cargo cache. Git dependencies must match the lock commit.
The caller supplies the normal Cargo build report and matching source/cache.
"""
import argparse
import fnmatch
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tomllib
from urllib.parse import urlsplit
from urllib.request import urlopen


def require(condition, message):
    if not condition:
        raise ValueError(message)


def safe_relative(value):
    path = PurePosixPath(value)
    return bool(value and not path.is_absolute() and ".." not in path.parts
                and re.fullmatch(r"[A-Za-z0-9._+@/=-]+", value))


def material_name(name, declared=None):
    path = PurePosixPath(name)
    return name == declared or any(
        fnmatch.fnmatchcase(path.name.lower(), pattern)
        for pattern in ("license*", "copying*", "notice*", "copyright*", "authors*", "credits*", "patents*")
    ) or any(part.lower() in ("licenses", "license") for part in path.parts[:-1])


def put_material(destination, relative, contents):
    require(safe_relative(relative), "unsafe Rust license material path")
    require(contents, "empty Rust license material: " + relative)
    target = destination / relative
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(contents)
    target.chmod(0o644)


def vhost_workspace_materials(package, archive, destination):
    # The published vhost crate omits the workspace-root license files.
    # Its checksum-bound Cargo VCS record selects the exact upstream revision;
    # also compare the original manifest before collecting those root files.
    require(package["name"] == "vhost" and package.get("repository") == "https://github.com/rust-vmm/vhost",
            "crate contains no license/notice material: " + package["name"] + "@" + package["version"])
    prefix = package["name"] + "-" + package["version"] + "/"
    with tarfile.open(archive) as source:
        def read_member(name):
            member = source.getmember(prefix + name)
            require(member.isfile(), "invalid vhost source identity member")
            return source.extractfile(member).read()
        vcs = json.loads(read_member(".cargo_vcs_info.json"))
        original_manifest = read_member("Cargo.toml.orig")
    commit = vcs.get("git", {}).get("sha1", "")
    require(re.fullmatch(r"[0-9a-f]{40}", commit) and not vcs.get("git", {}).get("dirty", False)
            and vcs.get("path_in_vcs") == "vhost", "unverifiable vhost workspace source")
    base = "https://raw.githubusercontent.com/rust-vmm/vhost/" + commit + "/"
    def download(name):
        with urlopen(base + name, timeout=30) as response:
            return response.read()
    require(download("vhost/Cargo.toml") == original_manifest, "vhost workspace manifest differs from crate")
    for name in ("LICENSE", "LICENSE-BSD-3-Clause"):
        put_material(destination, "upstream/" + name, download(name))
    return ";license-source-git:" + commit


def registry_materials(package, locked, cargo_home, destination):
    name, version = package["name"], package["version"]
    checksum = locked.get("checksum", "")
    require(re.fullmatch(r"[0-9a-f]{64}", checksum), "registry crate has no lock checksum")
    archives = sorted((cargo_home / "registry" / "cache").glob(f"*/{name}-{version}.crate"))
    require(archives, f"missing downloaded crate: {name}@{version}")
    matching = [path for path in archives if hashlib.sha256(path.read_bytes()).hexdigest() == checksum]
    require(matching,
            f"crate archive checksum mismatch: {name}@{version}")
    archive = matching[0]
    declared = package.get("license_file")
    if declared:
        declared_path = Path(declared)
        if declared_path.is_absolute():
            declared_path = declared_path.relative_to(Path(package["manifest_path"]).parent)
        declared = declared_path.as_posix()
        require(safe_relative(declared), "unsafe declared Rust license path")
    count = 0
    with tarfile.open(archive) as source:
        for member in source:
            parts = PurePosixPath(member.name).parts
            require(parts and parts[0] == f"{name}-{version}", "unexpected crate archive root")
            relative = "/".join(parts[1:])
            if not relative or member.isdir():
                continue
            if not material_name(relative, declared):
                continue  # Not extracted or read; upstream test filenames may contain '!'.
            require(safe_relative(relative) and member.isfile(), "unsafe Rust license archive member")
            put_material(destination, relative, source.extractfile(member).read())
            count += 1
    supplemental = "" if count else vhost_workspace_materials(package, archive, destination)
    return f"https://static.crates.io/crates/{name}/{name}-{version}.crate", "sha256:" + checksum + supplemental


def git_materials(package, locked, destination):
    source = locked["source"]
    commit = source.rsplit("#", 1)[-1]
    require(re.fullmatch(r"[0-9a-f]{40}", commit), "Git crate is not locked to a full commit")
    directory = Path(package["manifest_path"]).parent.resolve()
    def git(*args):
        return subprocess.check_output(["git", "-C", str(directory), *args], text=True).strip()
    require(git("rev-parse", "HEAD") == commit, "Git crate checkout differs from Cargo.lock")
    require(not git("status", "--porcelain", "--untracked-files=no"), "Git crate checkout is dirty")
    root = Path(git("rev-parse", "--show-toplevel")).resolve()
    declared = package.get("license_file")
    if declared:
        require(isinstance(declared, str), "invalid declared Git license path")
        declared_path = Path(declared)
        if not declared_path.is_absolute():
            declared_path = directory / declared_path
        # Normalize lexically, without following a symlink into another file.
        # Workspace-root licenses may be above the crate but not above the repo.
        declared_path = Path(os.path.normpath(declared_path))
        require(declared_path.is_relative_to(root), "declared Git license escapes selected repository")
        declared = declared_path.relative_to(root).as_posix()
        require(safe_relative(declared), "unsafe declared Git license path")
    count = 0
    declared_found = not declared
    tree = subprocess.check_output(["git", "-C", str(root), "ls-tree", "-rz", "--full-tree", commit])
    for record in tree.split(b"\0"):
        if not record:
            continue
        metadata, name = record.split(b"\t", 1)
        relative = os.fsdecode(name)
        if not material_name(relative, declared):
            continue
        mode, kind, blob = metadata.split()
        require(kind == b"blob" and mode in (b"100644", b"100755"), "invalid Git crate material")
        contents = subprocess.check_output(["git", "-C", str(root), "cat-file", "blob", blob.decode("ascii")])
        put_material(destination, relative, contents)
        declared_found = declared_found or relative == declared
        count += 1
    require(declared_found, "declared Git license is not a tracked regular material")
    require(count, "Git crate has no tracked license/notice material")
    return source[4:].split("?", 1)[0].split("#", 1)[0], "git:" + commit


def rust_toolchain_materials(stage, rustc):
    info = subprocess.check_output([str(rustc), "-vV"], text=True)
    fields = dict(line.split(": ", 1) for line in info.splitlines() if ": " in line)
    version, commit = fields.get("release", ""), fields.get("commit-hash", "")
    require(re.fullmatch(r"[0-9A-Za-z.+-]+", version), "invalid Rust toolchain version")
    sysroot = Path(subprocess.check_output([str(rustc), "--print", "sysroot"], text=True).strip()).resolve()
    docs = sysroot / "share" / "doc" / "rust"
    destination = stage / "share" / "licenses" / "sandboxer" / "rust-toolchain" / version
    source = "https://github.com/rust-lang/rust"
    integrity = "-"
    if re.fullmatch(r"[0-9a-f]{40}", commit):
        source += "/tree/" + commit
        integrity = "git:" + commit
    paths = []
    if (docs / "COPYRIGHT-library.html").is_file() and (docs / "licenses").is_dir():
        # Collect the selected installed toolchain's notices without downloading
        # or authenticating a compiler distribution or standard-library input.
        paths.append((docs / "COPYRIGHT-library.html", "COPYRIGHT-library.html"))
        paths.extend((path, path.relative_to(docs).as_posix())
                     for path in sorted((docs / "licenses").rglob("*")) if path.is_file())
    elif shutil.which("dpkg-query") and subprocess.run(
            ["dpkg-query", "-S", str(rustc.resolve())], stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL).returncode == 0:
        ownership = subprocess.check_output(["dpkg-query", "-S", str(rustc.resolve())], text=True).strip()
        owner = ownership.rsplit(": ", 1)[0]
        require("\n" not in owner and "," not in owner, "ambiguous Rust Debian package owner")
        identity = subprocess.check_output(["dpkg-query", "-W", "-f=${source:Package}\t${source:Version}\n",
                                            owner], text=True).strip().split("\t")
        require(len(identity) == 2 and all(identity), "Rust Debian package has no source identity")
        source = "deb-source:" + identity[0] + "@" + identity[1]
        owners = subprocess.check_output(["dpkg-query", "-W",
            "-f=${db:Status-Status}\t${binary:Package}\t${source:Package}\t${source:Version}\n"], text=True)
        for record in owners.splitlines():
            fields = record.split("\t")
            if len(fields) != 4 or fields[0] != "installed" or fields[2:] != identity:
                continue
            for value in subprocess.check_output(["dpkg-query", "-L", fields[1]], text=True).splitlines():
                path = Path(value)
                if not path.is_file() or not material_name(path.name):
                    continue
                paths.append((path, value.lstrip("/")))
                for common in sorted(set(re.findall(r"/usr/share/common-licenses/[A-Za-z0-9.+-]+",
                                                    path.read_text(errors="replace")))):
                    common_path = Path(common)
                    if not common_path.exists() and common.endswith("."):
                        common_path = Path(common[:-1])
                    require(common_path.is_file(), "referenced Rust Debian license is missing")
                    paths.append((common_path, str(common_path).lstrip("/")))
    elif shutil.which("rpm"):
        # Distribution Rust installs keep notices in RPM subpackages, not the
        # rustup documentation layout. Select only siblings of the same SRPM.
        query = subprocess.check_output(["rpm", "-qf", "--qf", "%{SOURCERPM}", str(sysroot / "bin" / "rustc")],
                                        text=True).strip()
        require(query and query != "(none)", "Rust RPM has no source identity")
        source = "rpm-source:" + query
        owners = subprocess.check_output(["rpm", "-qa", "--qf", "%{NAME}.%{ARCH}\t%{SOURCERPM}\n"], text=True)
        for owner in owners.splitlines():
            name, source_rpm = owner.split("\t")
            if source_rpm != query:
                continue
            for value in subprocess.check_output(["rpm", "-ql", name], text=True).splitlines():
                path = Path(value)
                if path.is_file() and (material_name(path.name) or value.startswith("/usr/share/licenses/")):
                    paths.append((path, value.lstrip("/")))
    else:
        raise ValueError("Rust toolchain notices are unavailable: restore the selected rustup rustc "
                         "component, or install its matching Debian/RPM copyright and license packages")
    require(paths, "Rust toolchain copyright/license material is missing; install the selected toolchain's "
                   "documentation/license package before release packaging")
    for path, relative in paths:
        resolved = path.resolve(strict=True)
        require(resolved.is_file(), "Rust license is not a regular file")
        contents = resolved.read_bytes()
        require(contents, "Rust license is empty")
        put_material(destination, relative, contents)
    return ["bin/cloud-hypervisor", "Rust toolchain", version, source, integrity,
            "share/licenses/sandboxer/rust-toolchain/" + version]


def collect(metadata, build_report, lock, cargo_home, source_root, stage):
    packages = {p["id"]: p for p in metadata["packages"]}
    observed = set()
    completed = False
    executable = False
    for line in build_report.splitlines():
        message = json.loads(line)
        if message.get("reason") == "compiler-artifact":
            observed.add(message["package_id"])
            if message.get("target", {}).get("name") == "cloud-hypervisor" and message.get("executable"):
                executable = True
        if message.get("reason") == "build-finished":
            completed = message.get("success") is True
    require(completed and executable, "missing successful Cloud Hypervisor build report")
    require(observed <= packages.keys(), "Cargo metadata omits an observed build package")
    locked = {(p["name"], p["version"], p.get("source")): p for p in lock["package"]}
    rows = []
    for identifier in sorted(observed):
        package = packages[identifier]
        source = package.get("source")
        if source is None:
            require(Path(package["manifest_path"]).resolve().is_relative_to(source_root.resolve()),
                    "local Cargo dependency escaped the selected Cloud Hypervisor source")
            continue  # Covered by the freshly extracted/patched CH source material.
        key = package["name"], package["version"], source
        require(key in locked, "observed Cargo package is absent from Cargo.lock")
        # Cargo permits a registry package and a Git fork with the same name
        # and version in one graph. Keep their notices in distinct namespaces.
        source_digest = hashlib.sha256(source.encode()).hexdigest()
        label = f"rust/{package['name']}@{package['version']}/source-{source_digest}"
        require(safe_relative(label), "unsafe Cargo package identity")
        directory = stage / "share" / "licenses" / "sandboxer" / label
        if source == "registry+https://github.com/rust-lang/crates.io-index":
            url, integrity = registry_materials(package, locked[key], cargo_home, directory)
        elif source.startswith("git+https://"):
            url, integrity = git_materials(package, locked[key], directory)
        else:
            raise ValueError("unsupported Rust release source: " + source)
        rows.append(["bin/cloud-hypervisor", "rust-build-input:" + package["name"],
                     package["version"], url, integrity, "share/licenses/sandboxer/" + label])
    require(rows, "Cloud Hypervisor build report contains no external crates")
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("metadata", "build-report", "lock", "cargo-home", "source-root", "stage", "rustc"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    rows = collect(json.loads(args.metadata.read_text()), args.build_report.read_text(),
                   tomllib.loads(args.lock.read_text()), args.cargo_home, args.source_root, args.stage)
    rows.append(rust_toolchain_materials(args.stage, args.rustc))
    for row in rows:
        require(all(value and "\n" not in value and "\t" not in value for value in row),
                "invalid Rust source material field")
        print("\t".join(row))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, KeyError, subprocess.CalledProcessError) as error:
        raise SystemExit("release Rust materials: " + str(error))
