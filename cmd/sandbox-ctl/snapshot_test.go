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
	}{
		{name: "defaults", wantDropCache: true, wantMergeRef: true},
		{
			name:          "explicit false",
			extra:         []string{"--drop-caches=false", "--merge-ref=false"},
			wantDropCache: false,
			wantMergeRef:  false,
		},
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
		})
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
