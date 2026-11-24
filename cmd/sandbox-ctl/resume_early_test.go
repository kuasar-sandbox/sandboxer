package main

import (
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeEarlyCLISelectsNormalLocalOrRemoteSinkAndAlwaysResumes(t *testing.T) {
	for _, flag := range []string{"", "--resume", "--resume-early"} {
		for _, remote := range []bool{false, true} {
			t.Run(map[bool]string{false: "local", true: "remote"}[remote]+flag, func(t *testing.T) {
				root := t.TempDir()
				dir := filepath.Join(root, "sid")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				l, err := net.Listen("unix", filepath.Join(dir, "ctl.sock"))
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				got := make(chan ctl.Request, 1)
				go func() {
					c, err := l.Accept()
					if err != nil {
						return
					}
					defer c.Close()
					var r ctl.Request
					if ctl.ReadMessage(c, &r) == nil {
						got <- r
					}
					_ = ctl.WriteMessage(c, ctl.Response{Type: ctl.TypeSnapshotDone, SnapshotRef: "manifest://" + strings.Repeat("1", 64), SandboxRef: "manifest://" + strings.Repeat("2", 64), SnapshotManifestKey: strings.Repeat("1", 64)})
				}()
				args := []string{"--sandbox-id", "sid", "--run-root", root}
				if flag != "" {
					args = append(args, flag)
				}
				if remote {
					args = append(args, "--upload")
				} else {
					args = append(args, "--output", t.TempDir())
				}
				if code := snapshotCmd(args); code != 0 {
					t.Fatal(code)
				}
				r := <-got
				if r.Type != ctl.TypeSnapshotRequest || r.ResumeAfter != (flag != "") || r.ResumeEarly != (flag == "--resume-early") || r.Upload != remote || r.CheckpointID != "" || r.DropCachesEnabled() {
					t.Fatalf("early-resume contract: %+v", r)
				}
			})
		}
	}
}

func TestResumeEarlyCaptureValidation(t *testing.T) {
	valid := ctl.Response{Type: ctl.TypeSnapshotDone, SnapshotRef: "manifest://" + strings.Repeat("1", 64), SandboxRef: "manifest://" + strings.Repeat("2", 64)}
	for _, test := range []struct {
		name     string
		response ctl.Response
		ok       bool
	}{
		{"valid", valid, true},
		{"empty", ctl.Response{Type: ctl.TypeSnapshotDone}, false},
		{"wrong-type", ctl.Response{Type: ctl.TypeExportDone, SnapshotRef: valid.SnapshotRef, SandboxRef: valid.SandboxRef}, false},
		{"missing-sandbox", ctl.Response{Type: ctl.TypeSnapshotDone, SnapshotRef: valid.SnapshotRef}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := captureReport(test.response, artifact.RoleSnapshot)
			if (err == nil) != test.ok {
				t.Fatal(err)
			}
		})
	}
}
