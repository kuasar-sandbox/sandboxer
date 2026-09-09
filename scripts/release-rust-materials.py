#!/usr/bin/env python3
"""Collect materials for Cargo packages observed in the actual CH build.

Registry licenses are read from checksum-verified .crate archives, never from
an editable extracted Cargo cache. Git dependencies must match the lock commit.
The caller builds in a fresh private Cargo home and supplies its build report.
"""
import argparse
import fnmatch
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tarfile
import tomllib
from urllib.parse import urlsplit
from urllib.request import urlopen


def require(condition, message):
    if not condition:
        raise ValueError(message)


def configure_cargo_home(original, destination):
    """Carry public registry routing only, never credentials or writable caches."""
    configs = [p for p in (original / "config", original / "config.toml") if p.is_file()]
    require(len(configs) <= 1, "ambiguous Cargo source configuration")
    if not configs:
        return
    sources = tomllib.loads(configs[0].read_text()).get("source", {})
    require(isinstance(sources, dict), "invalid Cargo source configuration")
    rendered = []
    for name, value in sorted(sources.items()):
        require(re.fullmatch(r"[A-Za-z0-9_-]+", name), "invalid Cargo source name")
        require(isinstance(value, dict) and set(value) <= {"replace-with", "registry"},
                "release Cargo sources must be public registries, not directory/git overrides")
        rendered.append("[source." + name + "]")
        for key, item in sorted(value.items()):
            require(isinstance(item, str), "invalid Cargo source value")
            if key == "registry":
                parsed = urlsplit(item.removeprefix("sparse+"))
                require(parsed.scheme == "https" and parsed.hostname and
                        parsed.username is None and parsed.password is None and
                        not parsed.query and not parsed.fragment,
                        "release Cargo registry must use credential-free HTTPS")
            else:
                require(re.fullmatch(r"[A-Za-z0-9_-]+", item), "invalid Cargo source replacement")
            rendered.append(key + " = " + json.dumps(item))
        rendered.append("")
    destination.mkdir(parents=True, exist_ok=True)
    destination.chmod(0o700)
    target = destination / "config.toml"
    target.write_text("\n".join(rendered))
    target.chmod(0o600)


def native_build_environment(original, home, cargo_home, rustc):
    """Pass build settings, not the caller's authentication environment."""
    for name in ("RUSTC", "RUSTC_WRAPPER", "RUSTC_WORKSPACE_WRAPPER",
                 "CARGO_BUILD_RUSTC", "CARGO_BUILD_RUSTC_WRAPPER",
                 "CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER"):
        require(not original.get(name), "release native build does not accept " + name)
    allowed = {"PATH", "LANG", "LC_ALL", "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR",
               "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
               "http_proxy", "https_proxy", "all_proxy", "no_proxy", "CARGO_BUILD_JOBS"}
    environment = {name: value for name, value in original.items() if name in allowed}
    for name, value in environment.items():
        if name.lower() in ("http_proxy", "https_proxy", "all_proxy"):
            parsed = urlsplit(value)
            require(parsed.username is None and parsed.password is None,
                    "release native build does not pass authenticated proxy URLs")
    home.mkdir(parents=True, exist_ok=True)
    home.chmod(0o700)
    environment.update(HOME=str(home), CARGO_HOME=str(cargo_home), RUSTC=str(rustc),
                       PATH=str(rustc.parent) + os.pathsep + environment.get("PATH", os.defpath),
                       GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull,
                       CARGO_NET_GIT_FETCH_WITH_CLI="true")
    return environment


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
    archives = list((cargo_home / "registry" / "cache").glob(f"*/{name}-{version}.crate"))
    require(len(archives) == 1, f"missing or ambiguous downloaded crate: {name}@{version}")
    archive = archives[0]
    require(hashlib.sha256(archive.read_bytes()).hexdigest() == checksum,
            f"crate archive checksum mismatch: {name}@{version}")
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
    root = Path(git("rev-parse", "--show-toplevel"))
    count = 0
    for relative in subprocess.check_output(["git", "-C", str(root), "ls-files"], text=True).splitlines():
        if not material_name(relative):
            continue
        path = root / relative
        require(path.is_file() and not path.is_symlink(), "invalid Git crate material")
        put_material(destination, str(path.relative_to(root)), path.read_bytes())
        count += 1
    require(count, "Git crate has no tracked license/notice material")
    return source[4:].split("?", 1)[0].split("#", 1)[0], "git:" + commit


def rust_toolchain_materials(stage, rustc):
    info = subprocess.check_output([str(rustc), "-vV"], text=True)
    fields = dict(line.split(": ", 1) for line in info.splitlines() if ": " in line)
    version, commit = fields.get("release", ""), fields.get("commit-hash", "")
    require(re.fullmatch(r"[0-9a-f]{40}", commit), "Rust toolchain has no source commit")
    require(re.fullmatch(r"[0-9A-Za-z.+-]+", version), "invalid Rust toolchain version")
    sysroot = Path(subprocess.check_output([str(rustc), "--print", "sysroot"], text=True).strip())
    docs = sysroot / "share" / "doc" / "rust"
    destination = stage / "share" / "licenses" / "sandboxer" / "rust-toolchain" / version
    source = "https://github.com/rust-lang/rust/tree/" + commit
    paths = []
    if (docs / "COPYRIGHT-library.html").is_file() and (docs / "licenses").is_dir():
        paths = [docs / "COPYRIGHT-library.html", *sorted((docs / "licenses").rglob("*"))]
        paths = [(p, str(p.relative_to(docs))) for p in paths if p.is_file()]
    else:
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
    require(paths, "Rust toolchain copyright/license material is missing")
    for path, relative in paths:
        require(not path.is_symlink(), "Rust toolchain license material is a symbolic link")
        put_material(destination, relative, path.read_bytes())
    return ["bin/cloud-hypervisor", "Rust toolchain", version, source,
            "git:" + commit + ";compiler-sha256:" + hashlib.sha256(rustc.read_bytes()).hexdigest(),
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
        label = f"rust/{package['name']}@{package['version']}"
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
    if len(sys.argv) == 4 and sys.argv[1] == "configure-cargo-home":
        configure_cargo_home(Path(sys.argv[2]), Path(sys.argv[3]))
        return
    if len(sys.argv) >= 6 and sys.argv[1] == "run-native":
        environment = native_build_environment(os.environ, *map(Path, sys.argv[2:5]))
        subprocess.run(sys.argv[5:], env=environment, check=True)
        return
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
