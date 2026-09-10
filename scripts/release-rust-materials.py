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
import shutil
import subprocess
import sys
import tarfile
import tempfile
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
                 "CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER", "GOEXPERIMENT",
                 "RUSTFLAGS", "CARGO_ENCODED_RUSTFLAGS"):
        require(not original.get(name), "release native build does not accept " + name)
    allowed = {"PATH", "LANG", "LC_ALL", "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR",
               "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
               "http_proxy", "https_proxy", "all_proxy", "no_proxy", "CARGO_BUILD_JOBS",
               "CARGO_NET_GIT_FETCH_WITH_CLI", "GOPROXY", "GOSUMDB", "GOTOOLCHAIN"}
    environment = {name: value for name, value in original.items() if name in allowed}
    environment.setdefault("CARGO_NET_GIT_FETCH_WITH_CLI", "true")
    require(environment["CARGO_NET_GIT_FETCH_WITH_CLI"] in ("true", "false"),
            "invalid Cargo Git transport setting")
    for value in environment.get("GOPROXY", "").replace("|", ",").split(","):
        if value in ("", "direct", "off"):
            continue
        parsed = urlsplit(value)
        require(parsed.scheme == "https" and parsed.hostname and parsed.username is None
                and parsed.password is None and not parsed.query and not parsed.fragment,
                "release Go proxy routing must use credential-free HTTPS")
    sumdb = environment.setdefault("GOSUMDB", "sum.golang.org")
    require("\n" not in sumdb and "\r" not in sumdb,
            "release checksum database routing must be a single line")
    fields = sumdb.split()
    require(1 <= len(fields) <= 2 and re.fullmatch(r"[A-Za-z0-9._+/:=-]+", fields[0]),
            "invalid release checksum database identity")
    if len(fields) == 2:
        parsed = urlsplit(fields[1])
        require(parsed.scheme == "https" and parsed.hostname and parsed.username is None
                and parsed.password is None and not parsed.query and not parsed.fragment,
                "release checksum database routing must use credential-free HTTPS")
    toolchain = environment.setdefault("GOTOOLCHAIN", "local")
    require(re.fullmatch(r"(?:local|auto|path|go[0-9]+\.[0-9]+(?:\.[0-9]+|beta[0-9]+|rc[0-9]+)?"
                         r"(?:\+(?:auto|path))?)", toolchain),
            "invalid release Go toolchain selection")
    for name, value in environment.items():
        if name.lower() in ("http_proxy", "https_proxy", "all_proxy"):
            parsed = urlsplit(value)
            require(parsed.username is None and parsed.password is None,
                    "release native build does not pass authenticated proxy URLs")
    home.mkdir(parents=True, exist_ok=True)
    home.chmod(0o700)
    environment.update(HOME=str(home), CARGO_HOME=str(cargo_home), RUSTC=str(rustc),
                       PATH=str(rustc.parent) + os.pathsep + environment.get("PATH", os.defpath),
                       GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
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


def rust_standard_library_inventory(stage, sysroot, link_map):
    target_directories = set()
    for line in link_map.read_text().splitlines():
        fields = line.split()
        if len(fields) != 2 or fields[0] != "LOAD" or not fields[1].endswith(".rlib"):
            continue
        path = Path(fields[1])
        require(path.is_absolute(), "unresolved relative Rust link input")
        # rustc may remove its own intermediate rlibs immediately after linking.
        # Only sysroot inputs must still exist for standard-library collection.
        path = path.resolve()
        if not path.is_relative_to(sysroot):
            continue  # Fresh Cargo outputs are covered by the observed package records.
        relative = path.relative_to(sysroot).as_posix()
        require(re.fullmatch(r"lib/rustlib/[A-Za-z0-9._+-]+/lib/lib[A-Za-z0-9._+-]+\.rlib", relative),
                "unexpected Rust standard-library input path")
        require(path.is_file(), "linked Rust sysroot archive is missing")
        target_directories.add(path.parent)
    require(len(target_directories) == 1, "link map omits or mixes Rust target sysroots")
    # Fat LTO consumes std/core bitcode before invoking the system linker, so
    # its map may retain only compiler_builtins. Inventory the selected target's
    # complete .rlib input set, not a falsely precise post-LTO linked subset.
    libraries = {}
    for path in sorted(next(iter(target_directories)).glob("*.rlib")):
        require(path.is_file() and not path.is_symlink(), "invalid Rust target standard-library archive")
        relative = path.relative_to(sysroot).as_posix()
        require(re.fullmatch(r"lib/rustlib/[A-Za-z0-9._+-]+/lib/lib[A-Za-z0-9._+-]+\.rlib", relative),
                "unexpected Rust standard-library input path")
        with path.open("rb") as contents:
            libraries[relative] = hashlib.file_digest(contents, "sha256").hexdigest()
    require(any(PurePosixPath(path).name.startswith("libstd-") for path in libraries),
            "selected Rust target sysroot omits its standard library")
    inventory = "sysroot_path\tsha256\n" + "".join(path + "\t" + digest + "\n"
        for path, digest in sorted(libraries.items()))
    destination = stage / "share/sources/sandboxer/RUST-STDLIB.tsv"
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(inventory)
    destination.chmod(0o644)
    return hashlib.sha256(inventory.encode()).hexdigest()


def rust_installed_license_bytes(path, package_format, expected_source=None):
    # Distribution copyright files can be package-owned symlinks. Follow the
    # link only to a regular file whose own installed bytes/source are verified.
    resolved = path.resolve(strict=True)
    require(resolved.is_file(), "Rust package license is not a regular file")
    contents = resolved.read_bytes()
    require(contents, "Rust package license is empty")
    if package_format == "deb":
        ownership = subprocess.check_output(["dpkg-query", "-S", str(resolved)], text=True)
        owners = []
        for line in ownership.splitlines():
            require(line.endswith(": " + str(resolved)), "ambiguous Rust license package owner")
            owners.extend(line.removesuffix(": " + str(resolved)).split(", "))
        require(owners, "Rust license has no package owner")
        for owner in owners:
            require(re.fullmatch(r"[a-z0-9][a-z0-9+.-]*(:[a-z0-9]+)?", owner), "invalid Rust license package owner")
            records = subprocess.check_output(["dpkg-query", "--control-show", owner, "md5sums"], text=True)
            digests = [fields[0] for line in records.splitlines()
                       if len(fields := line.split(maxsplit=1)) == 2 and fields[1] == str(resolved).lstrip("/")]
            require(len(digests) == 1 and re.fullmatch(r"[0-9a-f]{32}", digests[0]), "missing or ambiguous Rust license package digest")
            require(hashlib.md5(contents).hexdigest() == digests[0], "Rust license bytes differ from installed package metadata")
            if expected_source is not None:
                identity = subprocess.check_output(["dpkg-query", "-W", "-f=${source:Package}\t${source:Version}\n", owner], text=True).strip().split("\t")
                require(len(identity) == 2 and "deb-source:" + "@".join(identity) == expected_source,
                        "Rust license belongs to a different source package")
    elif package_format == "rpm":
        records = subprocess.check_output(["rpm", "-qf", "--dump", str(resolved)], text=True)
        digests = [fields[3] for line in records.splitlines()
                   if len(fields := line.split()) >= 4 and fields[0] == str(resolved)]
        require(len(digests) == 1 and re.fullmatch(r"[0-9a-f]+", digests[0]), "missing or ambiguous Rust license package digest")
        algorithm = {32: "md5", 40: "sha1", 64: "sha256", 96: "sha384", 128: "sha512"}.get(len(digests[0]))
        require(algorithm is not None, "unsupported Rust license package digest")
        require(hashlib.new(algorithm, contents).hexdigest() == digests[0], "Rust license bytes differ from installed package metadata")
        if expected_source is not None:
            identity = subprocess.check_output(["rpm", "-qf", "--qf", "%{SOURCERPM}\n", str(resolved)], text=True).strip()
            require(identity and identity != "(none)" and "rpm-source:" + identity == expected_source,
                    "Rust license belongs to a different source package")
    else:
        raise ValueError("unsupported Rust license package format")
    return contents


def rustup_toolchain_notices(sysroot, fields, destination, stage):
    """Read notices from Rust's HTTPS manifest-bound compiler distribution.

    The rustc component, not rust-docs, installs share/doc/rust's generated
    copyright and REUSE license files. Local editable notice bytes are unused.
    """
    version, commit, host = fields["release"], fields["commit-hash"], fields.get("host", "")
    require(re.fullmatch(r"[A-Za-z0-9_.+-]{1,100}", host), "invalid Rust distribution host")
    if re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        selector = "channel-rust-" + version + ".toml"
    else:
        match = re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+-(nightly|beta(?:\.[0-9]+)?)", version)
        require(match, "Rust notices require a released or dated official toolchain")
        installed = tomllib.loads((sysroot / "lib/rustlib/multirust-channel-manifest.toml").read_text())
        date = installed.get("date", "")
        require(isinstance(date, str) and re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", date),
                "invalid installed Rust channel date")
        # The local date only locates a fixed official manifest. Its full commit
        # and exact release string must still match; no moving channel is used.
        selector = date + "/channel-rust-" + ("nightly" if match[1] == "nightly" else "beta") + ".toml"
    manifest_url = "https://static.rust-lang.org/dist/" + selector
    with urlopen(manifest_url, timeout=30) as response:
        manifest_bytes = response.read(8 * 1024 * 1024 + 1)
    require(len(manifest_bytes) <= 8 * 1024 * 1024, "Rust distribution manifest is too large")
    manifest = tomllib.loads(manifest_bytes.decode())
    require(manifest.get("manifest-version") == "2", "unsupported Rust distribution manifest")
    package = manifest["pkg"]["rustc"]
    require(package.get("git_commit_hash") == commit and package.get("version", "").split(" ", 1)[0] == version,
            "Rust distribution differs from selected compiler commit or release")
    target = package["target"][host]
    require(target.get("available") is True, "selected Rust compiler distribution is unavailable")
    archive_url, digest = target["xz_url"], target["xz_hash"]
    parsed = urlsplit(archive_url)
    require(parsed.scheme == "https" and parsed.netloc == "static.rust-lang.org"
            and not parsed.query and not parsed.fragment
            and re.fullmatch(r"/dist/[0-9]{4}-[0-9]{2}-[0-9]{2}/rustc-[A-Za-z0-9_.+-]+\.tar\.xz", parsed.path)
            and parsed.path.endswith("-" + host + ".tar.xz"), "invalid Rust compiler distribution URL")
    require(re.fullmatch(r"[0-9a-f]{64}", digest), "invalid Rust compiler distribution digest")
    prefix = PurePosixPath(parsed.path).name.removesuffix(".tar.xz") + "/rustc/share/doc/rust/"
    notices = {}
    with tempfile.TemporaryFile() as downloaded:
        total = 0
        actual = hashlib.sha256()
        with urlopen(archive_url, timeout=30) as response:
            while block := response.read(1024 * 1024):
                total += len(block)
                require(total <= 512 * 1024 * 1024, "Rust compiler distribution is too large")
                actual.update(block)
                downloaded.write(block)
        require(actual.hexdigest() == digest, "Rust compiler distribution checksum mismatch")
        downloaded.seek(0)
        notice_size = 0
        with tarfile.open(fileobj=downloaded, mode="r|xz") as archive:
            for member in archive:
                if not member.name.startswith(prefix):
                    continue
                relative = member.name[len(prefix):]
                if member.isdir() or not (relative == "COPYRIGHT-library.html" or relative.startswith("licenses/")):
                    continue
                require(safe_relative(relative) and member.isfile() and relative not in notices,
                        "invalid or duplicate Rust distribution notice")
                notice_size += member.size
                require(0 < member.size <= 16 * 1024 * 1024 and notice_size <= 32 * 1024 * 1024,
                        "Rust distribution notices exceed size limit")
                notices[relative] = archive.extractfile(member).read()
    require("COPYRIGHT-library.html" in notices and any(name.startswith("licenses/") for name in notices),
            "Rust compiler distribution omits standard-library copyright or license texts")
    for relative, contents in sorted(notices.items()):
        put_material(destination, relative, contents)
    receipt = stage / "share/sources/sandboxer/RUST-NOTICES.tsv"
    receipt.parent.mkdir(parents=True, exist_ok=True)
    receipt.write_text("input\tsource\tsha256\nmanifest\t" + manifest_url + "\t"
                       + hashlib.sha256(manifest_bytes).hexdigest() + "\ncompiler-archive\t" + archive_url + "\t" + digest + "\n")
    receipt.chmod(0o644)
    return digest


def rust_toolchain_materials(stage, rustc, link_map):
    info = subprocess.check_output([str(rustc), "-vV"], text=True)
    fields = dict(line.split(": ", 1) for line in info.splitlines() if ": " in line)
    version, commit = fields.get("release", ""), fields.get("commit-hash", "")
    require(re.fullmatch(r"[0-9a-f]{40}", commit), "Rust toolchain has no source commit")
    require(re.fullmatch(r"[0-9A-Za-z.+-]+", version), "invalid Rust toolchain version")
    sysroot = Path(subprocess.check_output([str(rustc), "--print", "sysroot"], text=True).strip()).resolve()
    stdlib_digest = rust_standard_library_inventory(stage, sysroot, link_map)
    docs = sysroot / "share" / "doc" / "rust"
    destination = stage / "share" / "licenses" / "sandboxer" / "rust-toolchain" / version
    source = "https://github.com/rust-lang/rust/tree/" + commit
    paths = []
    package_format = None
    common_paths = set()
    notices_digest = ""
    if (docs / "COPYRIGHT-library.html").is_file() and (docs / "licenses").is_dir():
        notices_digest = rustup_toolchain_notices(sysroot, fields, destination, stage)
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
        package_format = "deb"
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
                    common_paths.add(common_path)
    elif shutil.which("rpm"):
        # Distribution Rust installs keep notices in RPM subpackages, not the
        # rustup documentation layout. Select only siblings of the same SRPM.
        query = subprocess.check_output(["rpm", "-qf", "--qf", "%{SOURCERPM}", str(sysroot / "bin" / "rustc")],
                                        text=True).strip()
        require(query and query != "(none)", "Rust RPM has no source identity")
        source = "rpm-source:" + query
        package_format = "rpm"
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
    require(paths or notices_digest, "Rust toolchain copyright/license material is missing; install the selected toolchain's "
                   "documentation/license package before release packaging")
    for path, relative in paths:
        contents = rust_installed_license_bytes(path, package_format, None if path in common_paths else source)
        put_material(destination, relative, contents)
    return ["bin/cloud-hypervisor", "Rust toolchain", version, source,
            "git:" + commit + ";compiler-sha256:" + hashlib.sha256(rustc.read_bytes()).hexdigest()
            + (";notices-archive-sha256:" + notices_digest if notices_digest else "")
            + ";target-stdlib-sha256:" + stdlib_digest,
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
    if len(sys.argv) == 4 and sys.argv[1] == "configure-cargo-home":
        configure_cargo_home(Path(sys.argv[2]), Path(sys.argv[3]))
        return
    if len(sys.argv) >= 6 and sys.argv[1] == "run-native":
        environment = native_build_environment(os.environ, *map(Path, sys.argv[2:5]))
        subprocess.run(sys.argv[5:], env=environment, check=True)
        return
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("metadata", "build-report", "lock", "cargo-home", "source-root", "stage", "rustc", "link-map"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    rows = collect(json.loads(args.metadata.read_text()), args.build_report.read_text(),
                   tomllib.loads(args.lock.read_text()), args.cargo_home, args.source_root, args.stage)
    rows.append(rust_toolchain_materials(args.stage, args.rustc, args.link_map))
    for row in rows:
        require(all(value and "\n" not in value and "\t" not in value for value in row),
                "invalid Rust source material field")
        print("\t".join(row))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, KeyError, subprocess.CalledProcessError) as error:
        raise SystemExit("release Rust materials: " + str(error))
