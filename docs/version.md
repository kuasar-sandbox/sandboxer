# Binary version queries

Every sandboxer Go executable, including sandbox-init, release-archive-validator,
and the E2E fixture and performance programs, supports these top-level queries:

```sh
sandbox-ctl --version
sandbox-ctl version
sandbox-ctl version --json
```

Text is just the executable name and Git's description, for example:

```text
sandbox-ctl v1.2.0-3-g4f2a1b012345
```

Makefile builds use `git describe --tags --long --always --abbrev=12 --dirty`,
restricted to sandboxer `v[0-9]*` tags. Exact tags retain the zero distance and
commit suffix; no-tag builds use the commit abbreviation. A tracked modification
adds `-dirty`. No Go or Git installation, checkout, configuration, service, root
privileges, or PID 1 is needed to query a copied binary.

JSON is flat and contains only available Go build information:

```json
{
  "GitCommit": "4f2a1b0123456789abcdef0123456789abcdef01",
  "GitCommitTime": "2026-11-02T08:00:00Z",
  "GitModified": "false",
  "GoVersion": "go1.26.4",
  "GOOS": "linux",
  "GOARCH": "amd64"
}
```

These are illustrative values. `GitCommit`, `GitCommitTime`, and `GitModified`
come from Go's `vcs.revision`, `vcs.time`, and `vcs.modified`, respectively.
Commit time is not compilation time; no build timestamp is injected. Git's dirty
check can ignore untracked files that Go's modified check includes, so the text
suffix and JSON modified value may differ. JSON values remain strings.

Direct `go build` without the Makefile still exposes the embedded full commit as
the text fallback. Missing settings are omitted from JSON; when build information
is unavailable JSON is `null`. Text with no usable description or revision is
`<name> unknown`. A description that disagrees with the embedded commit is rejected.
Version queries return 0 on success, 2 for invalid query arguments, and 1 for an
output error or inconsistent description. Other arguments retain their normal
meaning, including internal guest bootstrap commands.

To inspect the reported source in the sandboxer checkout:

```sh
commit="$(sandbox-ctl version --json | jq -r .GitCommit)"
git checkout --detach "$commit"
```

Dirty builds additionally require their uncommitted changes to reproduce the
source tree. Querying version information does not change any runtime state.

Validate the Git/tag/dirty/fallback behavior with:

```sh
python3 scripts/test-binary-version.py
go test ./internal/buildversion
```

The release archive validator deliberately remains a single standard-library-only
Go file, compiled with GO111MODULE=off by publication workflows. It supports the
same queries using available native metadata, but that build mode has no source
revision: text is `release-archive-validator unknown` and absent VCS JSON fields
are omitted. No commit is manufactured to fill that gap. Product and fixture
binaries are built as module packages so Go can embed their revisions.
