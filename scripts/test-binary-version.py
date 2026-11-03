#!/usr/bin/env python3
"""Exercise real Go VCS stamping and Git describe in an isolated repository."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

SOURCE = Path(__file__).resolve().parent.parent
with tempfile.TemporaryDirectory(prefix="sandboxer-version-") as temporary:
    root = Path(temporary)
    repo = root / "repo"
    repo.mkdir()
    env = dict(os.environ, GOWORK="off", GOTOOLCHAIN="local", GOPROXY="off",
               GOSUMDB="off", GOFLAGS="", CGO_ENABLED="0", GOOS="linux",
               GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_NOSYSTEM="1",
               GIT_AUTHOR_NAME="Version Test", GIT_COMMITTER_NAME="Version Test",
               GIT_AUTHOR_EMAIL="test@example.invalid", GIT_COMMITTER_EMAIL="test@example.invalid")

    def run(*args, expected=0):
        result = subprocess.run(args, cwd=repo, env=env, text=True, capture_output=True)
        assert result.returncode == expected, (args, result.returncode, result.stdout, result.stderr)
        return result.stdout.strip()

    (repo / "internal/buildversion").mkdir(parents=True)
    (repo / "scripts").mkdir()
    shutil.copyfile(SOURCE / "internal/buildversion/version.go", repo / "internal/buildversion/version.go")
    shutil.copyfile(SOURCE / "scripts/build-go.sh", repo / "scripts/build-go.sh")
    (repo / "go.mod").write_text("module github.com/kuasar-sandbox/sandboxer\n\ngo 1.26.1\n")
    (repo / "main.go").write_text('package main\nimport("fmt";"github.com/kuasar-sandbox/sandboxer/internal/buildversion")\nfunc main(){buildversion.ExitIfRequested();fmt.Println("business")}\n')
    run("git", "init", "--initial-branch=main")
    run("git", "add", ".")
    run("git", "-c", "commit.gpgsign=false", "commit", "-m", "initial")
    binary = root / "probe"

    def check(label):
        run("bash", "scripts/build-go.sh", "-trimpath", "-o", str(binary), ".")
        expected = run("git", "describe", "--tags", "--long", "--always", "--abbrev=12", "--dirty", "--match", "v[0-9]*")
        assert run(str(binary), "version") == "probe " + expected
        assert run(str(binary), "--version") == "probe " + expected
        values = json.loads(run(str(binary), "version", "--json"))
        assert set(values) == {"GoVersion", "GitCommit", "GitCommitTime", "GitModified", "GOOS", "GOARCH"}, values
        assert values["GitCommit"] == run("git", "rev-parse", "HEAD")
        assert values["GitModified"] == str(bool(run("git", "status", "--porcelain"))).lower()
        assert values["GOOS"] == "linux"
        assert run(str(binary), "run", "--version") == "business"
        run(str(binary), "version", "bad", expected=2)
        print(label + ": " + expected, flush=True)

    check("no tag, clean")
    run("git", "tag", "v1.2.0")
    check("exact lightweight tag, clean")
    source = (repo / "main.go").read_text()
    (repo / "main.go").write_text(source + "// dirty\n")
    check("exact tag, dirty")
    run("git", "add", ".")
    run("git", "-c", "commit.gpgsign=false", "commit", "-m", "next")
    check("post tag, clean")
    (repo / "main.go").write_text(source + "// dirty again\n")
    check("post tag, dirty")
    run("git", "restore", "main.go")
    (repo / "untracked").write_text("untracked files affect Go VCS state\n")
    check("untracked file: Go modified, Git describe clean")
    (repo / "untracked").unlink()
    run("git", "tag", "v1.3.0-rc.1")
    check("prerelease tag")
    run("go", "build", "-buildvcs=true", "-o", str(binary), ".")
    assert run(str(binary), "version") == "probe " + run("git", "rev-parse", "HEAD")
    run("go", "build", "-buildvcs=false", "-o", str(binary), ".")
    assert run(str(binary), "version") == "probe unknown"
    values = json.loads(run(str(binary), "version", "--json"))
    assert "GitCommit" not in values and "GitCommitTime" not in values
    print("direct builds with and without VCS metadata: passed")
