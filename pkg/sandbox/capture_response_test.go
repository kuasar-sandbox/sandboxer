package sandbox

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
)

// Uses the real capture handler and ctl server, and deliberately holds handler
// return after a committed capture. No new production hook is referenced: the
// same regression builds against the published baseline and the correction.
func TestCaptureResponsePrecedesDestroy(t *testing.T) {
	for _, kind := range []string{ctl.TypeSnapshotRequest, ctl.TypeExportRequest} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			runDir := filepath.Join(dir, "run")
			if err := os.MkdirAll(runDir, 0700); err != nil {
				t.Fatal(err)
			}
			guest := filepath.Join(dir, "guest.sock")
			guestDone := serveOneQuiesce(t, guest, nil)
			shutdown := make(chan struct{})
			var stopped sync.Once
			chSock, _ := serveLifecycleCH(t, dir, func(path string) int {
				if path == "/api/v1/vm.snapshot" {
					for name, body := range map[string]string{"config.json": `{"memory":{"size":4096}}`, "state.json": `{}`} {
						if err := os.WriteFile(filepath.Join(runDir, "snap-stage", name), []byte(body), 0600); err != nil {
							t.Errorf("write CH fixture: %v", err)
							return http.StatusInternalServerError
						}
					}
				}
				if path == "/api/v1/vmm.shutdown" {
					stopped.Do(func() { close(shutdown) })
				}
				return http.StatusNoContent
			})
			mfd, err := memory.Create("capture-response-order", 4096)
			if err != nil {
				t.Fatal(err)
			}
			defer mfd.Close()
			handler := &SnapshotHandler{
				Context: context.Background(),
				Cfg:     &config.SandboxConfig{}, PortableConfig: snapshotTestLivePortable(t), SandboxID: "test",
				Memfd: mfd,
				Disks: []SnapDiskRef{{Size: 4096, SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
					return bytes.NewReader(make([]byte, 4096)), nil, nil
				}}},
				CHSock: chSock, RunDir: runDir,
				CHProcess: func() processSignaler { return &channelSignaler{sent: make(chan os.Signal, 2)} },
				Pinger:    &guestlink.Pinger{Client: &guestlink.HostClient{BasePath: guest}},
				Logf:      discardLogf,
			}
			captured := make(chan error, 1)
			release := make(chan struct{})
			var released sync.Once
			unblock := func() { released.Do(func() { close(release) }) }
			defer unblock()
			holdReturn := func(capture func(ctl.Request) (ctl.Response, error)) func(ctl.Request) (ctl.Response, error) {
				return func(req ctl.Request) (ctl.Response, error) {
					resp, err := capture(req)
					captured <- err
					<-release
					return resp, err
				}
			}
			server := &ctl.Server{Path: filepath.Join(dir, "ctl.sock"),
				SnapshotHandler: holdReturn(func(req ctl.Request) (ctl.Response, error) { return handler.handle(req, "", shutdown) }),
				ExportHandler:   holdReturn(func(req ctl.Request) (ctl.Response, error) { return handler.handleExport(req, "", shutdown) })}
			if err := server.Listen(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { _ = server.Serve(ctx); close(done) }()
			defer func() { cancel(); server.Stop(); <-done }()
			conn, err := net.Dial("unix", server.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if err := ctl.WriteMessage(conn, ctl.Request{Type: kind, OutDir: filepath.Join(dir, "out")}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-captured:
				if err != nil {
					t.Fatalf("capture failed before reply-order check: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("capture did not complete")
			}
			if err := waitLifecycleResult(t, guestDone); err != nil {
				t.Fatal(err)
			}
			select {
			case <-shutdown:
				t.Fatal("VMM shutdown started before capture handler returned or ctl reply was written")
			case <-time.After(500 * time.Millisecond):
			}
			unblock()
			var response ctl.Response
			if err := ctl.ReadMessage(conn, &response); err != nil {
				t.Fatal(err)
			}
			expected := ctl.TypeSnapshotDone
			if kind == ctl.TypeExportRequest {
				expected = ctl.TypeExportDone
			}
			if response.Type != expected {
				t.Fatalf("reply type=%q want %q", response.Type, expected)
			}
			select {
			case <-shutdown:
			case <-time.After(time.Second):
				t.Fatal("completed reply did not release destroy")
			}
		})
	}
}
