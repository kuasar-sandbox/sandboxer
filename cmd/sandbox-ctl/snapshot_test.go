package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestSnapshotCmdBoolFlagMapping(t *testing.T) {
	tests := []struct {
		name          string
		extra         []string
		wantDropCache bool
		wantMergeRef  bool
		wantMode      string
	}{
		{name: "defaults", wantDropCache: false, wantMergeRef: true},
		{
			name:          "explicit false",
			extra:         []string{"--drop-caches=false", "--merge-ref=false"},
			wantDropCache: false,
			wantMergeRef:  false,
		},
		{name: "explicit true", extra: []string{"--drop-caches=true"}, wantDropCache: true, wantMergeRef: true},
		{name: "explicit bundle", extra: []string{"--mode", "bundle"}, wantDropCache: false, wantMergeRef: true, wantMode: ctl.SnapshotModeBundle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runRoot := t.TempDir()
			sandboxID := "test-sandbox"
			runDir := filepath.Join(runRoot, sandboxID)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(runDir, "ctl.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			requestCh := make(chan ctl.Request, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var req ctl.Request
				if ctl.ReadMessage(conn, &req) == nil {
					requestCh <- req
				}
				_ = ctl.WriteMessage(conn, &ctl.Response{Type: ctl.TypeSnapshotDone})
			}()

			args := []string{
				"--sandbox-id", sandboxID,
				"--run-root", runRoot,
				"--output", filepath.Join(t.TempDir(), "out"),
			}
			args = append(args, tt.extra...)
			if code := snapshotCmd(args); code != 0 {
				t.Fatalf("snapshotCmd exit=%d", code)
			}
			req := <-requestCh
			if req.DropCaches == nil || *req.DropCaches != tt.wantDropCache {
				t.Fatalf("drop_caches=%v, want %v", req.DropCaches, tt.wantDropCache)
			}
			if req.MergeRef == nil || *req.MergeRef != tt.wantMergeRef {
				t.Fatalf("merge_ref=%v, want %v", req.MergeRef, tt.wantMergeRef)
			}
			if req.Mode != tt.wantMode {
				t.Fatalf("mode=%q, want %q", req.Mode, tt.wantMode)
			}
		})
	}
}

func TestSnapshotCmdUsesPathIDWithoutSandboxIDAndWithPrecedence(t *testing.T) {
	for _, test := range []struct {
		name      string
		sandboxID string
	}{
		{name: "path id only"},
		{name: "path id takes precedence", sandboxID: "logical-sandbox"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runRoot, err := os.MkdirTemp("", "pathid-snapshot-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(runRoot) })
			pathID := "phase-c"
			runDir := filepath.Join(runRoot, pathID)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(runDir, "ctl.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			serverDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				var req ctl.Request
				if err := ctl.ReadMessage(conn, &req); err != nil {
					serverDone <- err
					return
				}
				serverDone <- ctl.WriteMessage(conn, &ctl.Response{Type: ctl.TypeSnapshotDone})
			}()

			args := []string{"--path-id", pathID, "--run-root", runRoot, "--output", filepath.Join(t.TempDir(), "out")}
			if test.sandboxID != "" {
				args = append(args, "--sandbox-id", test.sandboxID)
			}
			if code := snapshotCmd(args); code != 0 {
				t.Fatalf("snapshotCmd exit=%d", code)
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotCmdRejectsUploadWithExplicitMode(t *testing.T) {
	if code := snapshotCmd([]string{"--sandbox-id", "test", "--upload", "--mode", "local"}); code != 2 {
		t.Fatalf("snapshotCmd exit=%d, want 2", code)
	}
}

func TestSnapshotCmdRejectsNegativeTimeout(t *testing.T) {
	if code := snapshotCmd([]string{"--sandbox-id", "test", "--output", t.TempDir(), "--timeout", "-1"}); code != 2 {
		t.Fatalf("snapshotCmd exit=%d, want 2", code)
	}
}

func TestSnapshotCmdRejectsPositionalArguments(t *testing.T) {
	if code := snapshotCmd([]string{"--sandbox-id", "test", "--output", t.TempDir(), "extra"}); code != 2 {
		t.Fatalf("snapshotCmd exit=%d, want 2", code)
	}
}

func TestSnapshotCmdRejectsUnsafeSandboxID(t *testing.T) {
	if code := snapshotCmd([]string{"--sandbox-id", "../test", "--output", t.TempDir()}); code != 2 {
		t.Fatalf("snapshotCmd exit=%d, want 2", code)
	}
}

func TestSnapshotCmdRejectsUnsafePathID(t *testing.T) {
	if code := snapshotCmd([]string{"--path-id", "../test", "--output", t.TempDir()}); code != 2 {
		t.Fatalf("snapshotCmd exit=%d, want 2", code)
	}
}

func TestSnapshotDropCachesWarning(t *testing.T) {
	tests := []struct {
		name       string
		dropCaches bool
		result     proto.DropCachesResult
		want       bool
	}{
		{name: "skip confirmed", result: proto.DropCachesSkipped},
		{name: "old guest may have dropped", result: proto.DropCachesUnknown, want: true},
		{name: "drop succeeded", dropCaches: true, result: proto.DropCachesSucceeded},
		{name: "drop failed", dropCaches: true, result: proto.DropCachesFailed, want: true},
		{name: "unexpected skip", dropCaches: true, result: proto.DropCachesSkipped, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := snapshotDropCachesWarning(tt.dropCaches, tt.result)
			if (got != "") != tt.want {
				t.Fatalf("warning = %q, want present=%v", got, tt.want)
			}
		})
	}
}
