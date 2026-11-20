package sandbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
)

// Exercise the actual socket admission adapter and handler against the same
// pending receipt used by direct SDK capture. The CH preflight is the first
// observable capture action; none of these cases needs a running VMM.
func TestCheckpointMemorySocketAdmission(t *testing.T) {
	directory := t.TempDir()
	memorySaveRecord(t, directory)
	current := &artifact.CurrentRun{SandboxID: "sid", InstanceID: "instance", RunID: "run"}
	permit := &ctl.HostRetirementPermit{Version: ctl.HostRetirementVersion, SandboxID: "sid", InstanceID: "instance", RunID: "run", CheckpointID: "next", OperationID: "operation"}
	wrongPermit := *permit
	wrongPermit.RunID = "other-run"

	// Without a positive retained-memory capability echo, dynamic controllers
	// must preserve synchronous capture. Construct the real reservation adapter
	// instead of replacing the eligibility predicate with a test hook.
	cfg := &config.SandboxConfig{Resources: config.ResourcesConfig{
		Capacity:    config.CapacityConfig{CPU: 1, Memory: "1GiB"},
		Allocatable: config.AllocatableConfig{CPU: 1, Memory: "512MiB"},
		Control:     config.ControlConfig{CgroupPath: t.TempDir()},
	}}
	cfg.ApplyDefaults()
	hooks, err := resctl.NewControllerHooks(resctl.ControllerHookOptions{
		SocketPath: filepath.Join(t.TempDir(), "old-controller.sock"), SandboxID: "sid",
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hooks.Release("test") })
	if !hooks.Enabled() || hooks.SupportsRetainedMemory() {
		t.Fatal("fixture must require a controller without retained-memory capability")
	}

	for _, mode := range []string{ctl.SnapshotModeLocal, ctl.SnapshotModeBundle} {
		for _, tc := range []struct {
			name          string
			current       *artifact.CurrentRun
			permit        *ctl.HostRetirementPermit
			hooks         *resctl.ControllerHooks
			code, message string
			captures      int64
			wait          bool
		}{
			{name: "standalone-retained", message: "process is not started", captures: 1},
			{name: "managed-retained", current: current, permit: permit, code: ctl.CaptureRejectedCode, message: "process is not started", captures: 1},
			{name: "managed-without-permit", current: current, message: "process is not started", captures: 1},
			{name: "managed-without-permit-default-wait", current: current, message: "process is not started", captures: 1, wait: true},
			{name: "unsupported-controller", current: current, permit: permit, hooks: hooks, code: ctl.CaptureRejectedCode, message: "process is not started", captures: 1},
			{name: "unsupported-controller-default-wait", current: current, permit: permit, hooks: hooks, code: ctl.CaptureRejectedCode, message: "process is not started", captures: 1, wait: true},
			{name: "wrong-permit", current: current, permit: &wrongPermit, code: ctl.CaptureRejectedCode, message: "identity mismatch"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var captures atomic.Int64
				h := &SnapshotHandler{EnableCheckpoint: true, CurrentRun: tc.current,
					CheckpointDir: directory, runtime: &Runtime{hooks: tc.hooks},
					CHProcess: func() processSignaler { captures.Add(1); return nil },
				}
				req := ctl.Request{Type: ctl.TypeCheckpointSnapshotRequest, CheckpointID: "next", Mode: mode,
					HostRetirement: tc.permit, NoWaitForPreviousMemorySave: !tc.wait}
				_, directErr := h.Handle(req)
				if directErr == nil || !strings.Contains(directErr.Error(), tc.message) || captures.Load() != tc.captures {
					t.Fatalf("direct: captures=%d err=%v", captures.Load(), directErr)
				}
				captures.Store(0)
				resp := snapshotAdmissionSocketRequest(t, h, req)
				if resp.Type != ctl.TypeError || resp.Code != tc.code || resp.Msg != directErr.Error() || captures.Load() != tc.captures {
					t.Fatalf("socket: captures=%d response=%+v; direct=%v", captures.Load(), resp, directErr)
				}
				if err := h.capture.begin(); err != nil {
					t.Fatalf("admission poisoned capture gate: %v", err)
				}
				h.capture.finish(false)
			})
		}
	}
}

func snapshotAdmissionSocketRequest(t *testing.T, h *SnapshotHandler, req ctl.Request) ctl.Response {
	t.Helper()
	// Keep AF_UNIX paths independent of the descriptive subtest names.
	directory, err := os.MkdirTemp("", "capture-admit-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &ctl.Server{Path: filepath.Join(directory, "ctl.sock"),
		SnapshotAdmissionHandler: h.snapshotAdmission(life), SnapshotHandler: h.Handle}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(life) }()
	defer func() {
		s.Stop()
		select {
		case err := <-served:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("ctl server did not stop")
		}
	}()
	c, err := net.Dial("unix", s.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ctl.WriteMessage(c, req); err != nil {
		t.Fatal(err)
	}
	var resp ctl.Response
	if err := ctl.ReadMessage(c, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}
