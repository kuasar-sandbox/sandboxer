package resctl

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestAbortRetainedRetryCompletesReleasedRuntime(t *testing.T) {
	var allowAbort atomic.Bool
	var aborts, releases atomic.Int32
	released := make(chan struct{}, 1)
	capture := resource.CaptureRetention{CapturePath: filepath.Join(t.TempDir(), "original.json"), MemoryID: "original", Epoch: 1, OperationID: "capture-1"}
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "original-token", GrantedInitialAlloc: 128 << 20, ClientFeatures: []string{resource.FeatureRetainedMemoryV2}}, false
		case resource.TypePrepareRetained:
			return &resource.Message{Type: resource.TypeAck, Retained: &resource.RetainedMemory{MemoryID: capture.MemoryID, Epoch: capture.Epoch}}, false
		case resource.TypeAbortRetained:
			aborts.Add(1)
			if req.Retention == nil || *req.Retention != capture || releases.Load() == 0 && req.Token != "original-token" {
				t.Errorf("wrong abort identity: %+v", req)
			}
			if !allowAbort.Load() {
				return nil, true
			}
			return &resource.Message{Type: resource.TypeAck}, false
		case resource.TypeRelease:
			if !allowAbort.Load() {
				t.Error("B released before retention abort")
			}
			if req.Token != "original-token" {
				t.Errorf("wrong Release token: %q", req.Token)
			}
			releases.Add(1)
			released <- struct{}{}
			return &resource.Message{Type: resource.TypeAck}, false
		default:
			return &resource.Message{Type: resource.TypeAck}, false
		}
	})
	h := newAdmittedHooks(t, server, testControllerConfig(t, server.sock), 0)
	t.Cleanup(func() { allowAbort.Store(true); _ = h.AbortRetained(context.Background(), capture); h.Release("test") })
	if _, err := h.PrepareRetained(context.Background(), capture); err != nil {
		t.Fatal(err)
	}
	if err := h.AbortRetained(context.Background(), capture); err == nil {
		t.Fatal("missing injected abort transport failure")
	}
	h.Release("runtime closed")
	if releases.Load() != 0 {
		t.Fatal("unknown abort prematurely released original B")
	}
	lease := resource.LeasePath(server.sock, "sandbox-1")
	if _, err := os.Stat(lease); err != nil {
		t.Fatalf("prepared capture lost lease: %v", err)
	}
	allowAbort.Store(true)
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("abort retry did not finish ordinary release after Runtime.Close")
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, err := os.Stat(lease)
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("aborted runtime retained lease: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if releases.Load() != 1 || aborts.Load() < 3 {
		t.Fatalf("aborts=%d releases=%d", aborts.Load(), releases.Load())
	}
	h.Release("duplicate close")
	if releases.Load() != 1 {
		t.Fatal("duplicate release")
	}
}

func TestLateAbortDoesNotReleaseSuccessorRetention(t *testing.T) {
	var releases atomic.Int32
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "token", GrantedInitialAlloc: 128 << 20, ClientFeatures: []string{resource.FeatureRetainedMemoryV2}}, false
		case resource.TypePrepareRetained:
			return &resource.Message{Type: resource.TypeAck, Retained: &resource.RetainedMemory{MemoryID: req.Retention.MemoryID, Epoch: req.Retention.Epoch}}, false
		case resource.TypeRelease:
			releases.Add(1)
		}
		return &resource.Message{Type: resource.TypeAck}, false
	})
	h := newAdmittedHooks(t, server, testControllerConfig(t, server.sock), 0)
	old := resource.CaptureRetention{CapturePath: filepath.Join(t.TempDir(), "old.json"), MemoryID: "old", Epoch: 1, OperationID: "one"}
	next := resource.CaptureRetention{CapturePath: filepath.Join(t.TempDir(), "next.json"), MemoryID: "next", Epoch: 2, OperationID: "two"}
	t.Cleanup(func() { _ = h.AbortRetained(context.Background(), next); h.Release("test") })
	if _, err := h.PrepareRetained(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PrepareRetained(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	h.Release("runtime closed with later capture")
	if err := h.AbortRetained(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	preserved := h.retention != nil && *h.retention == next && h.retentionPrepared
	h.mu.Unlock()
	if !preserved || releases.Load() != 0 {
		t.Fatal("late old Abort released successor capture")
	}
	if _, err := os.Stat(resource.LeasePath(server.sock, "sandbox-1")); err != nil {
		t.Fatal(err)
	}
	if err := h.AbortRetained(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 1 {
		t.Fatalf("successor Abort did not release B: %d", releases.Load())
	}
}
