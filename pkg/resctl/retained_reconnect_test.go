package resctl

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestPrepareRetainedRestoresActiveSession(t *testing.T) {
	for _, scenario := range []string{"closed-stream", "prepare-response-lost"} {
		t.Run(scenario, func(t *testing.T) {
			capture := resource.CaptureRetention{CapturePath: filepath.Join(t.TempDir(), "capture.json"), MemoryID: "memory", Epoch: 1, OperationID: "operation"}
			var prepares, syncs atomic.Int32
			server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
				switch req.Type {
				case resource.TypeAdmit:
					return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "original-token", GrantedInitialAlloc: 128 << 20, ClientFeatures: []string{resource.FeatureRetainedMemoryV2}}, false
				case resource.TypeStateSync:
					syncs.Add(1)
					if req.AppliedAllocatableMemory != 128<<20 || req.PreviousToken != "original-token" {
						t.Errorf("StateSync changed the admitted B baseline: %+v", req)
					}
					return &resource.Message{Type: resource.TypeAck, Token: "restored-token", NewAllocatable: 128 << 20, ClientFeatures: []string{resource.FeatureRetainedMemoryV2}}, false
				case resource.TypePrepareRetained:
					attempt := prepares.Add(1)
					if req.Retention == nil || *req.Retention != capture {
						t.Errorf("replay changed the capture identity: %+v", req.Retention)
					}
					if scenario == "prepare-response-lost" && attempt == 1 {
						return nil, true
					}
					if syncs.Load() != 1 || req.Token != "restored-token" {
						return &resource.Message{Type: resource.TypeError, Msg: "checkpoint memory: retention requires the current admitted session"}, false
					}
					return &resource.Message{Type: resource.TypeAck, Retained: &resource.RetainedMemory{MemoryID: capture.MemoryID, Epoch: capture.Epoch, Bytes: 128 << 20}}, false
				default:
					return &resource.Message{Type: resource.TypeAck}, false
				}
			})
			h := newAdmittedHooks(t, server, testControllerConfig(t, server.sock), 0)
			t.Cleanup(func() { _ = h.AbortRetained(context.Background(), capture); h.Release("test") })
			if scenario == "closed-stream" {
				_ = h.client.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			retained, err := h.PrepareRetained(ctx, capture)
			if err != nil {
				t.Fatalf("Prepare after controller disconnect failed: %v (StateSync=%d, Prepare=%d)", err, syncs.Load(), prepares.Load())
			}
			if retained.Bytes != 128<<20 || syncs.Load() != 1 || h.ReservationMemory() != 128<<20 {
				t.Fatalf("wrong recovered responsibility: retained=%+v syncs=%d B=%d", retained, syncs.Load(), h.ReservationMemory())
			}
		})
	}
}

func TestRetainedTerminalReplayDoesNotRestoreActiveSession(t *testing.T) {
	for _, operation := range []string{resource.TypeCommitRetained, resource.TypeAbortRetained, resource.TypeReleaseRetained} {
		t.Run(operation, func(t *testing.T) {
			capture := resource.CaptureRetention{CapturePath: filepath.Join(t.TempDir(), "capture.json"), MemoryID: "memory", Epoch: 1, OperationID: "operation"}
			var dropped atomic.Bool
			var syncs atomic.Int32
			server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
				if req.Type == operation && dropped.CompareAndSwap(false, true) {
					return nil, true
				}
				switch req.Type {
				case resource.TypeAdmit:
					return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "original-token", GrantedInitialAlloc: 128 << 20, ClientFeatures: []string{resource.FeatureRetainedMemoryV2}}, false
				case resource.TypeStateSync:
					syncs.Add(1)
					return &resource.Message{Type: resource.TypeError, Msg: "terminal replay must not restore active B"}, false
				default:
					return &resource.Message{Type: resource.TypeAck, Retained: &resource.RetainedMemory{MemoryID: capture.MemoryID, Epoch: capture.Epoch, Bytes: 128 << 20}}, false
				}
			})
			h := newAdmittedHooks(t, server, testControllerConfig(t, server.sock), 0)
			if _, err := h.PrepareRetained(context.Background(), capture); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case resource.TypeAbortRetained:
				err = h.AbortRetained(context.Background(), capture)
			case resource.TypeCommitRetained:
				err = h.CommitRetained(context.Background(), capture)
			case resource.TypeReleaseRetained:
				if err = h.CommitRetained(context.Background(), capture); err == nil {
					err = h.ReleaseRetained(context.Background(), capture)
				}
			}
			if err != nil || !dropped.Load() || syncs.Load() != 0 {
				t.Fatalf("terminal replay: err=%v dropped=%v StateSync=%d", err, dropped.Load(), syncs.Load())
			}
			_ = h.CloseRetained()
		})
	}
}

func TestPrepareRetainedDoesNotRestoreReleasedSession(t *testing.T) {
	var syncs, prepares atomic.Int32
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "original-token", GrantedInitialAlloc: 128 << 20, ClientFeatures: []string{resource.FeatureRetainedMemoryV2}}, false
		case resource.TypeStateSync:
			syncs.Add(1)
		case resource.TypePrepareRetained:
			prepares.Add(1)
		}
		return &resource.Message{Type: resource.TypeAck}, false
	})
	h := newAdmittedHooks(t, server, testControllerConfig(t, server.sock), 0)
	h.Release("runtime already closed")
	capture := resource.CaptureRetention{CapturePath: filepath.Join(t.TempDir(), "capture.json"), MemoryID: "memory", Epoch: 1, OperationID: "operation"}
	defer h.AbortRetained(context.Background(), capture)
	if _, err := h.PrepareRetained(context.Background(), capture); err == nil {
		t.Fatal("released runtime prepared a new capture")
	}
	if syncs.Load() != 0 || prepares.Load() != 0 {
		t.Fatalf("released runtime restored responsibility: StateSync=%d Prepare=%d", syncs.Load(), prepares.Load())
	}
}

func TestPrepareRetainedRecoveryDrainsStateSyncBeforeRelease(t *testing.T) {
	capture := resource.CaptureRetention{CapturePath: filepath.Join(t.TempDir(), "capture.json"), MemoryID: "memory", Epoch: 1, OperationID: "operation"}
	syncStarted, allowSync := make(chan struct{}), make(chan struct{})
	var unblocked atomic.Bool
	unblockSync := func() {
		if unblocked.CompareAndSwap(false, true) {
			close(allowSync)
		}
	}
	var prepares, releases atomic.Int32
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "original-token", GrantedInitialAlloc: 128 << 20, ClientFeatures: []string{resource.FeatureRetainedMemoryV2}}, false
		case resource.TypeStateSync:
			close(syncStarted)
			<-allowSync
			return &resource.Message{Type: resource.TypeAck, Token: "restored-token", NewAllocatable: 128 << 20, ClientFeatures: []string{resource.FeatureRetainedMemoryV2}}, false
		case resource.TypePrepareRetained:
			prepares.Add(1)
		case resource.TypeRelease:
			if req.Token != "restored-token" {
				t.Errorf("Release lost the drained StateSync token: %q", req.Token)
			}
			releases.Add(1)
		}
		return &resource.Message{Type: resource.TypeAck}, false
	})
	h := newAdmittedHooks(t, server, testControllerConfig(t, server.sock), 0)
	t.Cleanup(func() { unblockSync(); _ = h.AbortRetained(context.Background(), capture); h.Release("test") })
	_ = h.client.Close()
	prepared := make(chan error, 1)
	go func() { _, err := h.PrepareRetained(context.Background(), capture); prepared <- err }()
	select {
	case <-syncStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Prepare did not restore the active session")
	}
	released := make(chan struct{})
	go func() { h.Release("runtime closed during Prepare recovery"); close(released) }()
	select {
	case <-h.lifetimeCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Release did not cancel the active runtime")
	}
	select {
	case <-released:
		t.Fatal("Release overtook the StateSync token ACK")
	default:
	}
	unblockSync()
	select {
	case err := <-prepared:
		if err == nil {
			t.Fatal("Prepare continued after runtime cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prepare recovery did not drain")
	}
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("Release did not join Prepare recovery")
	}
	if prepares.Load() != 0 || releases.Load() != 0 {
		t.Fatalf("canceled capture sent Prepare or released an unresolved rollback: Prepare=%d Release=%d", prepares.Load(), releases.Load())
	}
	if err := h.AbortRetained(context.Background(), capture); err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 1 {
		t.Fatalf("abort did not release exactly the restored B: %d", releases.Load())
	}
}
