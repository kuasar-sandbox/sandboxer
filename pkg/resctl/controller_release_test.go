package resctl

import (
	"context"
	"errors"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestControllerHooksReleaseAfterCancellation(t *testing.T) {
	for _, mode := range []string{"idle-cancel-control", "signal-context-during-budget", "release-during-budget"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var releases atomic.Int32
			var charged atomic.Bool
			entered := make(chan struct{})
			allowReply := make(chan struct{})
			defer close(allowReply)
			server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
				switch req.Type {
				case resource.TypeAdmit:
					charged.Store(true)
					return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "owned-session", GrantedInitialAlloc: 128 << 20}, false
				case resource.TypeRequestBudget:
					close(entered)
					<-allowReply
					return &resource.Message{Type: resource.TypeBudgetResponse, NewAllocatable: 128 << 20}, false
				case resource.TypeRelease:
					if req.Token != "owned-session" {
						t.Errorf("wrong cleanup token: %q", req.Token)
					}
					releases.Add(1)
					charged.Store(false)
					return &resource.Message{Type: resource.TypeAck}, false
				default:
					return &resource.Message{Type: resource.TypeAck, NewAllocatable: 128 << 20}, false
				}
			})
			cfg := testControllerConfig(t, server.sock)
			h, err := NewControllerHooks(ControllerHookOptions{SocketPath: server.sock, SandboxID: "sandbox-1", CgroupPath: cfg.Resources.Control.CgroupPath, Context: ctx, Logf: t.Logf}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Release("test-cleanup")
			if _, err = h.Admit("sandbox-1", 0); err != nil {
				t.Fatal(err)
			}
			if mode == "idle-cancel-control" {
				cancel()
				h.Release("normal")
				if releases.Load() != 1 || charged.Load() {
					t.Fatalf("idle cancellation failed to release: count=%d charged=%v", releases.Load(), charged.Load())
				}
				t.Log("CONTROL: canceled context with no in-flight RPC sends exactly one Release and removes charge")
				return
			}
			result := make(chan error, 1)
			go func() { _, _, _, e := h.RequestBudget(128<<20, 0, resource.UrgencyNormal, "diagnostic"); result <- e }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("RPC not received")
			}
			releaseDone := make(chan struct{})
			if mode == "signal-context-during-budget" {
				cancel()
				close(releaseDone)
			} else {
				go func() { h.Release("normal"); close(releaseDone) }()
			}
			select {
			case err = <-result:
			case <-time.After(time.Second):
				t.Fatal("cancel did not interrupt budget RPC")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want context.Canceled, got %v", err)
			}
			h.Release("normal")
			<-releaseDone
			if releases.Load() != 1 || charged.Load() {
				t.Fatalf("cleanup lost: releases=%d charge=%v", releases.Load(), charged.Load())
			}
			if h.client.Connected() {
				t.Fatal("transport still connected")
			}
			t.Log("cancellation closes the in-flight RPC, but bounded cleanup releases the exact original token")
		})
	}
}

func TestControllerHooksReleaseDrainsStateSyncTokenRotation(t *testing.T) {
	for _, externalCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "signal"}[externalCancel], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			syncEntered := make(chan struct{})
			syncGate := make(chan struct{})
			defer close(syncGate)
			var released atomic.Int32
			server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
				switch req.Type {
				case resource.TypeAdmit:
					return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "first-token", GrantedInitialAlloc: 128 << 20}, false
				case resource.TypeRequestBudget:
					return nil, true // genuine broken transport before reconnect
				case resource.TypeStateSync:
					close(syncEntered)
					<-syncGate
					return &resource.Message{Type: resource.TypeAck, Token: "rotated-token", NewAllocatable: 128 << 20}, false
				case resource.TypeRelease:
					if req.Token != "rotated-token" {
						t.Errorf("cleanup must use acknowledged rotated token, got %q", req.Token)
					}
					released.Add(1)
					return &resource.Message{Type: resource.TypeAck}, false
				default:
					return &resource.Message{Type: resource.TypeAck}, false
				}
			})
			cfg := testControllerConfig(t, server.sock)
			h, err := NewControllerHooks(ControllerHookOptions{SocketPath: server.sock, SandboxID: "sandbox-1", CgroupPath: cfg.Resources.Control.CgroupPath, Context: ctx, Logf: t.Logf}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Release("test")
			if _, err = h.Admit("sandbox-1", 0); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err = h.RequestBudget(128<<20, 0, resource.UrgencyNormal, "test"); err == nil {
				t.Fatal("missing injected transport error")
			}
			select {
			case <-syncEntered:
			case <-time.After(2 * time.Second):
				t.Fatal("StateSync did not start")
			}
			if externalCancel {
				cancel()
			}
			done := make(chan struct{})
			go func() { h.Release("normal"); close(done) }()
			deadline := time.Now().Add(time.Second)
			for !h.localState().released && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !h.localState().released {
				t.Fatal("Release did not start")
			}
			select {
			case <-done:
				t.Fatal("Release completed before the in-flight token was known")
			default:
			}
			// The lease must remain published throughout bounded cleanup.
			if _, err := os.Stat(resource.LeasePath(server.sock, "sandbox-1")); err != nil {
				t.Fatal(err)
			}
			syncGate <- struct{}{}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("cleanup did not finish")
			}
			if released.Load() != 1 {
				t.Fatalf("Release count=%d", released.Load())
			}
			if _, err := os.Stat(resource.LeasePath(server.sock, "sandbox-1")); !os.IsNotExist(err) {
				t.Fatalf("lease retained after cleanup: %v", err)
			}
		})
	}
}

// The writer holds this gate from its startup check through the successful
// WriteMessage callback. Parent cancellation must not bypass that ownership.
func TestControllerHooksParentCancellationWaitsForStateSyncWriteGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		return &resource.Message{Type: resource.TypeAck}, false
	})
	cfg := testControllerConfig(t, server.sock)
	h, err := NewControllerHooks(ControllerHookOptions{SocketPath: server.sock, SandboxID: "sandbox-1", Context: ctx}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release("test")

	h.syncStartMu.Lock()
	before := h.lifetimeCtx.Err()
	cancel() // exactly between the startup check and the wire-write callback
	duringWrite := h.lifetimeCtx.Err()
	h.syncStartMu.Unlock()
	if before != nil {
		t.Fatalf("lifetime already canceled: %v", before)
	}
	if duringWrite != nil {
		t.Fatalf("parent cancellation bypassed StateSync write gate: %v", duringWrite)
	}
	select {
	case <-h.lifetimeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not propagate after the write gate opened")
	}
}

func TestControllerHooksCanceledParentCannotStartQueuedStateSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var syncs, releases atomic.Int32
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "original-token", GrantedInitialAlloc: 128 << 20}, false
		case resource.TypeRequestBudget:
			return nil, true
		case resource.TypeStateSync:
			syncs.Add(1)
			return &resource.Message{Type: resource.TypeAck, Token: "unexpected-token", NewAllocatable: 128 << 20}, false
		case resource.TypeRelease:
			if req.Token != "original-token" {
				t.Errorf("cleanup changed token after parent cancellation: %q", req.Token)
			}
			releases.Add(1)
			return &resource.Message{Type: resource.TypeAck}, false
		default:
			return &resource.Message{Type: resource.TypeAck}, false
		}
	})
	cfg := testControllerConfig(t, server.sock)
	h, err := NewControllerHooks(ControllerHookOptions{SocketPath: server.sock, SandboxID: "sandbox-1", Context: ctx}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release("test")
	if _, err := h.Admit("sandbox-1", 0); err != nil {
		t.Fatal(err)
	}

	h.syncStartMu.Lock()
	_, _, _, budgetErr := h.RequestBudget(128<<20, 0, resource.UrgencyNormal, "test")
	cancel()
	h.syncStartMu.Unlock()
	if budgetErr == nil {
		t.Fatal("missing injected transport failure")
	}
	done := make(chan struct{})
	go func() { h.Release("normal"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown waited on a StateSync that had not started")
	}
	if syncs.Load() != 0 || releases.Load() != 1 {
		t.Fatalf("StateSync=%d Release=%d, want 0 and 1", syncs.Load(), releases.Load())
	}
}
