package buildversion

import (
	"bytes"
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

func TestQueries(t *testing.T) {
	const revision = "4f2a1b0123456789abcdef0123456789abcdef01"
	info := &debug.BuildInfo{GoVersion: "go1.26.4", Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: revision}, {Key: "vcs.time", Value: "2026-11-02T08:00:00Z"},
		{Key: "vcs.modified", Value: "false"}, {Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "amd64"},
		{Key: "-ldflags", Value: "not part of the identity JSON"},
	}}
	old := describe
	t.Cleanup(func() { describe = old })
	describe = "v1.2.0-3-g4f2a1b012345"
	read := func() (*debug.BuildInfo, bool) { return info, true }
	for _, arg := range []string{"version", "--version"} {
		var out, errs bytes.Buffer
		handled, code := run([]string{"/tmp/sandbox-init", arg}, &out, &errs, read)
		if !handled || code != 0 || out.String() != "sandbox-init "+describe+"\n" || errs.Len() != 0 {
			t.Fatalf("query: %v %d %q %q", handled, code, &out, &errs)
		}
	}
	var out, errs bytes.Buffer
	_, code := run([]string{"probe", "version", "--json"}, &out, &errs, read)
	var values map[string]string
	if err := json.Unmarshal(out.Bytes(), &values); err != nil {
		t.Fatal(err)
	}
	if code != 0 || len(values) != 6 || values["GitCommit"] != revision || values["GitModified"] != "false" || values["GitCommitTime"] != "2026-11-02T08:00:00Z" {
		t.Fatalf("JSON: %d %v", code, values)
	}
	out.Reset()
	errs.Reset()
	handled, code := run([]string{"probe", "version", "extra"}, &out, &errs, read)
	if !handled || code != 2 || out.Len() != 0 || errs.Len() == 0 {
		t.Fatal("invalid version query not rejected")
	}
	for _, args := range [][]string{{"probe"}, {"probe", "exec-child", "--version"}, {"probe", "run", "version"}} {
		handled, _ := run(args, &out, &errs, func() (*debug.BuildInfo, bool) { t.Fatal("business arguments inspected build info"); return nil, false })
		if handled {
			t.Fatalf("intercepted business arguments: %v", args)
		}
	}
	describe = "v1.2.0-0-gdeadbeef0000"
	out.Reset()
	errs.Reset()
	_, code = run([]string{"probe", "--version"}, &out, &errs, read)
	if code != 1 || out.Len() != 0 || !strings.Contains(errs.String(), "does not match") {
		t.Fatal("stale description accepted")
	}
	describe = ""
	out.Reset()
	errs.Reset()
	_, code = run([]string{"probe", "--version"}, &out, &errs, read)
	if code != 0 || out.String() != "probe "+revision+"\n" {
		t.Fatal("direct-build fallback lost embedded commit")
	}
	info.Settings[2].Value = "true"
	out.Reset()
	run([]string{"probe", "version"}, &out, &errs, read)
	if out.String() != "probe "+revision+"-dirty\n" {
		t.Fatal("dirty fallback not marked")
	}
}

func TestMissingBuildInfo(t *testing.T) {
	old := describe
	describe = ""
	t.Cleanup(func() { describe = old })
	read := func() (*debug.BuildInfo, bool) { return nil, false }
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"probe", "version"}, "probe unknown\n"},
		{[]string{"probe", "version", "--json"}, "null\n"},
	} {
		var out, errs bytes.Buffer
		handled, code := run(tc.args, &out, &errs, read)
		if !handled || code != 0 || out.String() != tc.want || errs.Len() != 0 {
			t.Fatalf("missing build info: %d %q", code, &out)
		}
	}
}
