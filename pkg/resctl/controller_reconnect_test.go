package resctl

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

type reservationServer struct {
	sock     string
	listener net.Listener
	requests chan resource.Message
	handler  func(resource.Message) (*resource.Message, bool)
	done     chan struct{}
}

func startReservationServer(t *testing.T, handler func(resource.Message) (*resource.Message, bool)) *reservationServer {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "controller.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := &reservationServer{
		sock: sock, listener: listener, requests: make(chan resource.Message, 64),
		handler: handler, done: make(chan struct{}),
	}
	go func() {
		defer close(s.done)
		var wg sync.WaitGroup
		defer wg.Wait()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				for {
					req, err := resource.ReadMessage(conn)
					if err != nil {
						return
					}
					select {
					case s.requests <- *req:
					default:
					}
					resp, closeWithoutReply := s.handler(*req)
					if closeWithoutReply {
						return
					}
					if err := resource.WriteMessage(conn, resp); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-s.done
	})
	return s
}

func testControllerConfig(t *testing.T, socket string) *config.SandboxConfig {
	t.Helper()
	cgroup := t.TempDir()
	if err := os.WriteFile(filepath.Join(cgroup, "memory.current"), []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 1, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "512MiB"},
			Startup:     &config.StartupConfig{Memory: "128MiB"},
			Control: config.ControlConfig{
				Controller: socket, CgroupPath: cgroup,
			},
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func newAdmittedHooks(t *testing.T, server *reservationServer, cfg *config.SandboxConfig, snapshotBudget uint64) *ControllerHooks {
	t.Helper()
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: server.sock, SandboxID: "sandbox-1", CgroupPath: cfg.Resources.Control.CgroupPath,
		Context: context.Background(), Logf: func(string, ...any) {},
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hooks.Release("test") })
	if _, err := hooks.Admit("sandbox-1", snapshotBudget); err != nil {
		t.Fatal(err)
	}
	return hooks
}

func TestControllerHooksAdmitUsesExactColdAndRestoreBudgets(t *testing.T) {
	for _, tc := range []struct {
		name            string
		snapshotBudget  uint64
		expectedInitial uint64
	}{
		{name: "cold", expectedInitial: 128 << 20},
		{name: "restore", snapshotBudget: 320 << 20, expectedInitial: 320 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var admit resource.Message
			server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
				switch req.Type {
				case resource.TypeAdmit:
					mu.Lock()
					admit = req
					mu.Unlock()
					return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "token", GrantedInitialAlloc: tc.expectedInitial}, false
				case resource.TypeRelease:
					return &resource.Message{Type: resource.TypeAck}, false
				default:
					return &resource.Message{Type: resource.TypeAck, NewAllocatable: tc.expectedInitial}, false
				}
			})
			cfg := testControllerConfig(t, server.sock)
			hooks := newAdmittedHooks(t, server, cfg, tc.snapshotBudget)
			if got := hooks.ReservationMemory(); got != tc.expectedInitial {
				t.Fatalf("reservation = %d, want %d", got, tc.expectedInitial)
			}
			mu.Lock()
			got := admit
			mu.Unlock()
			if got.FloorMemoryBytes != 512<<20 || got.StartupBudgetMemory != 128<<20 ||
				got.AllocatableAtSnapshot != tc.snapshotBudget {
				t.Fatalf("admit request = %+v", got)
			}
		})
	}
}

func TestControllerHooksRejectsPartialInitialAdmission(t *testing.T) {
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		if req.Type == resource.TypeAdmit {
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "token", GrantedInitialAlloc: 64 << 20}, false
		}
		return &resource.Message{Type: resource.TypeAck}, false
	})
	cfg := testControllerConfig(t, server.sock)
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: server.sock, SandboxID: "sandbox-1", Context: context.Background(),
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	if _, err := hooks.Admit("sandbox-1", 0); err == nil {
		t.Fatal("partial cold admission was accepted")
	}
}

func TestControllerHooksSettledDoesNotChangeReservation(t *testing.T) {
	var settled resource.Message
	var mu sync.Mutex
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "token", GrantedInitialAlloc: 128 << 20}, false
		case resource.TypeSettled:
			mu.Lock()
			settled = req
			mu.Unlock()
			return &resource.Message{Type: resource.TypeAck}, false
		case resource.TypeRelease:
			return &resource.Message{Type: resource.TypeAck}, false
		default:
			return &resource.Message{Type: resource.TypeAck, NewAllocatable: 128 << 20}, false
		}
	})
	cfg := testControllerConfig(t, server.sock)
	hooks := newAdmittedHooks(t, server, cfg, 0)
	if err := hooks.Settled(); err != nil {
		t.Fatal(err)
	}
	if got := hooks.ReservationMemory(); got != 128<<20 {
		t.Fatalf("Settled changed reservation to %d", got)
	}
	mu.Lock()
	got := settled
	mu.Unlock()
	if got.CurrentRSS != 12345 {
		t.Fatalf("Settled host charge = %d, want diagnostic 12345", got.CurrentRSS)
	}
}

func TestControllerHooksRequestBudgetUsesAbsoluteBaseline(t *testing.T) {
	var mu sync.Mutex
	reservation := uint64(128 << 20)
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "token", GrantedInitialAlloc: reservation}, false
		case resource.TypeRequestBudget:
			reservation = req.CurrentAlloc + req.RequestedDelta
			return &resource.Message{Type: resource.TypeBudgetResponse, GrantedDelta: req.RequestedDelta, NewAllocatable: reservation}, false
		case resource.TypeHeartbeat:
			return &resource.Message{Type: resource.TypeAck, NewAllocatable: reservation}, false
		case resource.TypeRelease:
			return &resource.Message{Type: resource.TypeAck}, false
		default:
			return &resource.Message{Type: resource.TypeAck, NewAllocatable: reservation}, false
		}
	})
	cfg := testControllerConfig(t, server.sock)
	hooks := newAdmittedHooks(t, server, cfg, 0)
	granted, next, _, err := hooks.RequestBudget(128<<20, 64<<20, resource.UrgencyNormal, "grow")
	if err != nil || granted != 64<<20 || next != 192<<20 {
		t.Fatalf("grow = (%d, %d, %v)", granted, next, err)
	}
	granted, next, _, err = hooks.RequestBudget(96<<20, 0, resource.UrgencyLow, "shrink_commit")
	if err != nil || granted != 0 || next != 96<<20 {
		t.Fatalf("shrink = (%d, %d, %v)", granted, next, err)
	}
	if got := hooks.ReservationMemory(); got != 96<<20 {
		t.Fatalf("reservation = %d, want %d", got, uint64(96<<20))
	}
}

func TestControllerHooksLostGrowResponseStateSyncsOldSafeBaseline(t *testing.T) {
	var mu sync.Mutex
	reservation := uint64(128 << 20)
	dropGrow := true
	syncSeen := make(chan resource.Message, 1)
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "token-1", GrantedInitialAlloc: reservation}, false
		case resource.TypeRequestBudget:
			reservation = req.CurrentAlloc + req.RequestedDelta
			if dropGrow {
				dropGrow = false
				return nil, true
			}
			return &resource.Message{Type: resource.TypeBudgetResponse, GrantedDelta: req.RequestedDelta, NewAllocatable: reservation}, false
		case resource.TypeStateSync:
			reservation = req.AppliedAllocatableMemory
			syncSeen <- req
			return &resource.Message{Type: resource.TypeAck, Token: "token-2", NewAllocatable: reservation}, false
		case resource.TypeRelease:
			return &resource.Message{Type: resource.TypeAck}, false
		default:
			return &resource.Message{Type: resource.TypeAck, NewAllocatable: reservation}, false
		}
	})
	cfg := testControllerConfig(t, server.sock)
	hooks := newAdmittedHooks(t, server, cfg, 0)
	if _, _, _, err := hooks.RequestBudget(128<<20, 64<<20, resource.UrgencyNormal, "grow"); err == nil {
		t.Fatal("lost grow response was reported as success")
	}
	select {
	case syncReq := <-syncSeen:
		if syncReq.AppliedAllocatableMemory != 128<<20 {
			t.Fatalf("StateSync baseline = %d, want pre-response %d", syncReq.AppliedAllocatableMemory, uint64(128<<20))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StateSync did not recover lost response")
	}
	waitHooksConnected(t, hooks)
}

func TestControllerHooksLostShrinkResponseStateSyncsCompletedBaseline(t *testing.T) {
	var mu sync.Mutex
	reservation := uint64(128 << 20)
	dropShrink := false
	syncSeen := make(chan resource.Message, 1)
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "token-1", GrantedInitialAlloc: reservation}, false
		case resource.TypeRequestBudget:
			reservation = req.CurrentAlloc + req.RequestedDelta
			if dropShrink {
				dropShrink = false
				return nil, true
			}
			return &resource.Message{Type: resource.TypeBudgetResponse, GrantedDelta: req.RequestedDelta, NewAllocatable: reservation}, false
		case resource.TypeStateSync:
			reservation = req.AppliedAllocatableMemory
			syncSeen <- req
			return &resource.Message{Type: resource.TypeAck, Token: "token-2", NewAllocatable: reservation}, false
		case resource.TypeRelease:
			return &resource.Message{Type: resource.TypeAck}, false
		default:
			return &resource.Message{Type: resource.TypeAck, NewAllocatable: reservation}, false
		}
	})
	cfg := testControllerConfig(t, server.sock)
	hooks := newAdmittedHooks(t, server, cfg, 0)
	granted, next, _, err := hooks.RequestBudget(128<<20, 64<<20, resource.UrgencyNormal, "grow_before_shrink")
	if err != nil {
		t.Fatalf("grow before lost shrink: %v", err)
	}
	if granted != 64<<20 || next != 192<<20 {
		t.Fatalf("grow result = (%d, %d), want (%d, %d)", granted, next, uint64(64<<20), uint64(192<<20))
	}
	mu.Lock()
	dropShrink = true
	mu.Unlock()
	if _, _, _, err := hooks.RequestBudget(128<<20, 0, resource.UrgencyLow, "shrink_commit"); err == nil {
		t.Fatal("lost shrink response was reported as success")
	}
	if got := hooks.ReservationMemory(); got != 128<<20 {
		t.Fatalf("completed shrink baseline = %d, want %d", got, uint64(128<<20))
	}
	select {
	case syncReq := <-syncSeen:
		if syncReq.AppliedAllocatableMemory != 128<<20 {
			t.Fatalf("StateSync baseline = %d, want completed shrink %d", syncReq.AppliedAllocatableMemory, uint64(128<<20))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StateSync did not recover lost shrink response")
	}
	waitHooksConnected(t, hooks)
}

func TestControllerHooksHeartbeatMismatchTriggersStateSyncWithoutLocalCommand(t *testing.T) {
	const baseline = uint64(128 << 20)
	mismatch := true
	var mu sync.Mutex
	syncSeen := make(chan resource.Message, 1)
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "token-1", GrantedInitialAlloc: baseline}, false
		case resource.TypeHeartbeat:
			value := baseline
			if mismatch {
				value += 64 << 20
				mismatch = false
			}
			return &resource.Message{Type: resource.TypeAck, NewAllocatable: value}, false
		case resource.TypeStateSync:
			syncSeen <- req
			return &resource.Message{Type: resource.TypeAck, Token: "token-2", NewAllocatable: req.AppliedAllocatableMemory}, false
		case resource.TypeRelease:
			return &resource.Message{Type: resource.TypeAck}, false
		default:
			return &resource.Message{Type: resource.TypeAck, NewAllocatable: baseline}, false
		}
	})
	cfg := testControllerConfig(t, server.sock)
	hooks := newAdmittedHooks(t, server, cfg, 0)
	hooks.heartbeatOnce()
	if got := hooks.ReservationMemory(); got != baseline {
		t.Fatalf("heartbeat response changed local reservation to %d", got)
	}
	select {
	case req := <-syncSeen:
		if req.AppliedAllocatableMemory != baseline {
			t.Fatalf("StateSync baseline = %d", req.AppliedAllocatableMemory)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat mismatch did not trigger StateSync")
	}
}

func TestControllerHooksRejectsMalformedBudgetResponse(t *testing.T) {
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		switch req.Type {
		case resource.TypeAdmit:
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "token", GrantedInitialAlloc: 128 << 20}, false
		case resource.TypeRequestBudget:
			return &resource.Message{Type: resource.TypeBudgetResponse, GrantedDelta: 64 << 20, NewAllocatable: 512 << 20}, false
		case resource.TypeStateSync:
			return &resource.Message{Type: resource.TypeAck, Token: "token-2", NewAllocatable: req.AppliedAllocatableMemory}, false
		case resource.TypeRelease:
			return &resource.Message{Type: resource.TypeAck}, false
		default:
			return &resource.Message{Type: resource.TypeAck, NewAllocatable: 128 << 20}, false
		}
	})
	cfg := testControllerConfig(t, server.sock)
	hooks := newAdmittedHooks(t, server, cfg, 0)
	if _, _, _, err := hooks.RequestBudget(128<<20, 64<<20, resource.UrgencyNormal, "grow"); err == nil {
		t.Fatal("malformed NewAllocatable was accepted")
	}
}

func waitHooksConnected(t *testing.T, hooks *ControllerHooks) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !hooks.Connected() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hooks.Connected() {
		t.Fatal("controller hooks did not reconnect")
	}
}

func TestControllerHooksReleaseCancelsReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hooks := &ControllerHooks{
		opts:        ControllerHookOptions{Context: ctx, Logf: func(string, ...any) {}},
		lifetimeCtx: ctx, cancelLifetime: cancel, reconnectWake: make(chan struct{}, 1),
	}
	hooks.reconnectWG.Add(1)
	go hooks.reconnectLoop()
	hooks.Release("test")
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("context = %v, want canceled", ctx.Err())
	}
}
