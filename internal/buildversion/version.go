// Package buildversion exposes the identity embedded in a Go executable.
package buildversion

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// describe is supplied by scripts/build-go.sh, never evaluated at runtime.
var describe string

// ExitIfRequested handles only an explicit top-level version query. Call it
// before argument parsing, service startup, or guest PID 1 checks.
func ExitIfRequested() {
	if handled, code := run(os.Args, os.Stdout, os.Stderr, debug.ReadBuildInfo); handled {
		os.Exit(code)
	}
}

func run(args []string, out, errOut io.Writer, read func() (*debug.BuildInfo, bool)) (bool, int) {
	if len(args) < 2 || (args[1] != "version" && args[1] != "--version") {
		return false, 0
	}
	asJSON := len(args) == 3 && args[2] == "--json"
	if len(args) != 2 && !asJSON {
		fmt.Fprintln(errOut, "usage: "+filepath.Base(args[0])+" version [--json]")
		return true, 2
	}
	info, ok := read()
	values := fields(info, ok)
	var err error
	if asJSON {
		err = json.NewEncoder(out).Encode(values)
	} else {
		version := describe
		commit := values["GitCommit"]
		if version != "" {
			suffix := strings.TrimSuffix(version, "-dirty")
			if i := strings.LastIndex(suffix, "-g"); i >= 0 {
				suffix = suffix[i+2:]
			}
			if commit == "" || !strings.HasPrefix(commit, suffix) {
				fmt.Fprintln(errOut, "version description does not match embedded VCS revision")
				return true, 1
			}
		} else if commit != "" {
			version = commit
			if values["GitModified"] == "true" {
				version += "-dirty"
			}
		} else {
			version = "unknown"
		}
		_, err = fmt.Fprintln(out, filepath.Base(args[0])+" "+version)
	}
	if err != nil {
		fmt.Fprintln(errOut, err)
		return true, 1
	}
	return true, 0
}

func fields(info *debug.BuildInfo, ok bool) map[string]string {
	if !ok || info == nil {
		return nil
	}
	values := map[string]string{"GoVersion": info.GoVersion}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			values["GitCommit"] = s.Value
		case "vcs.time":
			values["GitCommitTime"] = s.Value
		case "vcs.modified":
			values["GitModified"] = s.Value
		case "GOOS", "GOARCH":
			values[s.Key] = s.Value
		}
	}
	return values
}
