package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
)

func TestCaptureJSONUsesOnlyActualRuntimePair(t *testing.T) {
	for _, command := range []string{"snapshot", "export"} {
		for _, mode := range []string{"local", "bundle", "upload"} {
			for _, resume := range []bool{false, true} {
				for _, jsonOutput := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/resume=%t/json=%t", command, mode, resume, jsonOutput), func(t *testing.T) {
						dir, err := os.MkdirTemp("", "capture-json-")
						if err != nil {
							t.Fatal(err)
						}
						defer os.RemoveAll(dir)
						run := filepath.Join(dir, "sid")
						if err := os.Mkdir(run, 0700); err != nil {
							t.Fatal(err)
						}
						response := ctl.Response{Type: ctl.TypeSnapshotDone,
							SnapshotRef:  "file:///unavailable/private/unrelated.snapshot@digest:" + strings.Repeat("1", 64),
							SandboxRef:   "file:///unavailable/another/actual.sandbox@hmac:" + strings.Repeat("2", 64),
							SnapshotPath: "/unavailable/sid.snapshot", SandboxPath: "/unavailable/sid.sandbox"}
						if mode == "bundle" {
							response.SnapshotRef = "file:///unavailable/shared.bundle@manifest:" + strings.Repeat("3", 64)
							response.SandboxRef = "file:///unavailable/shared.bundle@manifest:" + strings.Repeat("4", 64)
						} else if mode == "upload" {
							response.SnapshotManifestKey = strings.Repeat("5", 64)
							response.SandboxManifestKey = strings.Repeat("6", 64)
							response.SnapshotRef = "manifest://" + response.SnapshotManifestKey
							response.SandboxRef = "manifest://" + response.SandboxManifestKey
						}
						role := artifact.RoleSnapshot
						if command == "export" {
							response.Type = ctl.TypeExportDone
							role = artifact.RoleSandbox
						}
						listener, err := net.Listen("unix", filepath.Join(run, "ctl.sock"))
						if err != nil {
							t.Fatal(err)
						}
						defer listener.Close()
						received := make(chan ctl.Request, 1)
						done := make(chan error, 1)
						go func() {
							c, e := listener.Accept()
							if e != nil {
								done <- e
								return
							}
							defer c.Close()
							var req ctl.Request
							if e = ctl.ReadMessage(c, &req); e == nil {
								received <- req
								e = ctl.WriteMessage(c, &response)
							}
							done <- e
						}()
						args := []string{"--sandbox-id", "sid", "--run-root", dir, fmt.Sprintf("--resume=%t", resume)}
						if jsonOutput {
							args = append(args, "--json")
						}
						if mode == "upload" {
							args = append(args, "--upload")
						} else {
							args = append(args, "--output", filepath.Join(dir, "out"), "--mode", mode)
						}
						var code int
						out := capturePairStdout(t, func() {
							if command == "snapshot" {
								code = snapshotCmd(args)
							} else {
								code = exportCmd(args)
							}
						})
						if err := <-done; err != nil {
							t.Fatal(err)
						}
						if code != 0 {
							t.Fatalf("exit=%d stdout=%s", code, out)
						}
						req := <-received
						if req.ResumeAfter != resume || req.Upload != (mode == "upload") {
							t.Fatalf("request=%+v", req)
						}
						if !jsonOutput {
							var want string
							if command == "snapshot" {
								want = "snapshot done: memory_size=0 resident=0 pause_ms=0 dump_ms=0\n"
								if mode == "local" {
									want += "  Snapshot S: " + response.SnapshotPath + "\n  Sandbox E:  " + response.SandboxPath + "\n"
								}
								if mode == "upload" {
									want = response.SnapshotManifestKey + "\n"
								}
							} else {
								want = "export done: pause_ms=0 dump_ms=0\n"
								if mode == "local" {
									want += "  Sandbox E: " + response.SandboxPath + "\n  ref:       " + response.SandboxRef + "\n"
								}
								if mode == "bundle" {
									want += "  ref:       manifest://" + strings.Repeat("4", 64) + "\n"
								}
								if mode == "upload" {
									want = response.SandboxManifestKey + "\n"
								}
							}
							if string(out) != want {
								t.Fatalf("default stdout=%q want=%q", out, want)
							}
							return
						}
						result, err := artifact.DecodeCaptureReport(out, role)
						if err != nil {
							t.Fatal(err)
						}
						wantE, _ := artifact.PublicRef(response.SandboxRef)
						wantS := ""
						if command == "snapshot" {
							wantS, _ = artifact.PublicRef(response.SnapshotRef)
						}
						if result.SandboxRef != wantE || result.SnapshotRef != wantS || result.RemovedRefs == nil || len(result.RemovedRefs) != 0 {
							t.Fatalf("result=%+v", result)
						}
						if bytes.Contains(out, []byte("unavailable")) {
							t.Fatalf("directory leak: %s", out)
						}
					})
				}
			}
		}
	}

}

func TestCaptureJSONRejectsTornOrWrongResponse(t *testing.T) {
	good := ctl.Response{Type: ctl.TypeSnapshotDone, SnapshotRef: "manifest://" + strings.Repeat("a", 64), SandboxRef: "manifest://" + strings.Repeat("b", 64)}
	for _, response := range []ctl.Response{
		{Type: good.Type, SnapshotRef: good.SnapshotRef},
		{Type: good.Type, SandboxRef: good.SandboxRef},
		{Type: ctl.TypeExportDone, SnapshotRef: good.SnapshotRef, SandboxRef: good.SandboxRef},
		{Type: good.Type, SnapshotRef: good.SnapshotRef, SandboxRef: "file://unidentified.sandbox"},
	} {
		code := 0
		out := capturePairStdout(t, func() { code = printCaptureJSON(response, artifact.RoleSnapshot) })
		if code == 0 || len(out) != 0 {
			t.Fatalf("torn capture returned success: code=%d out=%s", code, out)
		}
	}
}

func capturePairStdout(t *testing.T, call func()) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	call()
	w.Close()
	out, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
