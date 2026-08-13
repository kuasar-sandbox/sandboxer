package resctl

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

type reconnectController struct {
	t         *testing.T
	socket    string
	legacy    bool
	listener  *net.UnixListener
	done      chan struct{}
	synced    chan resource.Message
	closeOnce sync.Once
}

type settledController struct {
	t        *testing.T
	socket   string
	listener *net.UnixListener
	settled  chan resource.Message
	done     chan struct{}
}

func startSettledController(t *testing.T) *settledController {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "controller.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	c := &settledController{t: t, socket: socket, listener: listener, settled: make(chan resource.Message, 1), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		admit, err := resource.ReadMessage(conn)
		if err != nil || admit.Type != resource.TypeAdmit {
			t.Errorf("admit request = %+v err=%v", admit, err)
			return
		}
		_ = resource.WriteMessage(conn, &resource.Message{
			Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted,
			Token: "settle-token", GrantedInitialAlloc: admit.StartupBudgetMemory,
		})
		settled, err := resource.ReadMessage(conn)
		if err != nil || settled.Type != resource.TypeSettled {
			t.Errorf("settled request = %+v err=%v", settled, err)
			return
		}
		c.settled <- *settled
		_ = resource.WriteMessage(conn, &resource.Message{Type: resource.TypeAck})
		release, err := resource.ReadMessage(conn)
		if err == nil && release.Type == resource.TypeRelease {
			_ = resource.WriteMessage(conn, &resource.Message{Type: resource.TypeAck})
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			t.Error("settled controller did not stop")
		}
	})
	return c
}

func startReconnectController(t *testing.T, legacy bool) *reconnectController {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "controller.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	c := &reconnectController{t: t, socket: socket, legacy: legacy, listener: listener, done: make(chan struct{}), synced: make(chan resource.Message, 1)}
	go c.run()
	t.Cleanup(c.close)
	return c
}

func (c *reconnectController) close() {
	c.closeOnce.Do(func() {
		_ = c.listener.Close()
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			c.t.Error("fake controller did not stop")
		}
	})
}

func (c *reconnectController) run() {
	defer close(c.done)
	first, err := c.listener.AcceptUnix()
	if err != nil {
		return
	}
	req, err := resource.ReadMessage(first)
	if err != nil || req.Type != resource.TypeAdmit {
		c.t.Errorf("first request = %+v err=%v", req, err)
		_ = first.Close()
		return
	}
	_ = resource.WriteMessage(first, &resource.Message{
		Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted,
		Token: "initial-token", GrantedInitialAlloc: req.StartupBudgetMemory,
	})
	_ = first.Close()

	second, err := c.listener.AcceptUnix()
	if err != nil {
		return
	}
	defer second.Close()
	syncReq, err := resource.ReadMessage(second)
	if err != nil || syncReq.Type != resource.TypeStateSync {
		c.t.Errorf("sync request = %+v err=%v", syncReq, err)
		return
	}
	if c.legacy {
		_ = resource.WriteMessage(second, &resource.Message{Type: resource.TypeError, Msg: "unknown type: state_sync"})
		reattach, err := resource.ReadMessage(second)
		if err != nil || reattach.Type != resource.TypeReattach || reattach.Token != "initial-token" {
			c.t.Errorf("reattach request = %+v err=%v", reattach, err)
			return
		}
		_ = resource.WriteMessage(second, &resource.Message{Type: resource.TypeAck, Token: reattach.Token, NewAllocatable: syncReq.AppliedAllocatableMemory})
	} else {
		_ = resource.WriteMessage(second, &resource.Message{Type: resource.TypeAck, Token: "synced-token", NewAllocatable: syncReq.AppliedAllocatableMemory})
	}
	c.synced <- *syncReq
	for {
		req, err := resource.ReadMessage(second)
		if err != nil {
			return
		}
		switch req.Type {
		case resource.TypeHeartbeat:
			_ = resource.WriteMessage(second, &resource.Message{Type: resource.TypeAck, NewAllocatable: syncReq.AppliedAllocatableMemory})
		case resource.TypeRequestBudget:
			_ = resource.WriteMessage(second, &resource.Message{
				Type: resource.TypeBudgetResponse, GrantedDelta: req.RequestedDelta,
				NewAllocatable: req.CurrentAlloc + req.RequestedDelta,
			})
		case resource.TypeRelease:
			_ = resource.WriteMessage(second, &resource.Message{Type: resource.TypeAck})
			return
		default:
			_ = resource.WriteMessage(second, &resource.Message{Type: resource.TypeAck})
		}
	}
}

func reconnectConfig(t *testing.T, socket, cgroup string) *config.SandboxConfig {
	t.Helper()
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity = config.CapacityConfig{CPU: 1, Memory: "1GiB"}
	cfg.Resources.Allocatable = config.AllocatableConfig{CPU: 0.5, Memory: "512MiB"}
	cfg.Resources.Startup = &config.StartupConfig{Memory: "768MiB"}
	cfg.Resources.Control.Controller = socket
	cfg.Resources.Control.CgroupPath = cgroup
	cfg.ApplyDefaults()
	return cfg
}

func TestControllerHooksCanonicalizesLeaseAndAdmitCgroupPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "intermediate"), 0o755); err != nil {
		t.Fatal(err)
	}
	cgroup := filepath.Join(dir, "target")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	unclean := filepath.Join(dir, "intermediate", "..", "target") + string(filepath.Separator)
	socket := filepath.Join(dir, "controller.sock")
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: socket, SandboxID: "canonical-path", Context: context.Background(), Logf: t.Logf,
	}, reconnectConfig(t, socket, unclean))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	lease, err := resource.ReadLease(hooks.lease.Path())
	if err != nil {
		t.Fatal(err)
	}
	if lease.CgroupPath != cgroup || hooks.controllerCgroupPath != cgroup {
		t.Fatalf("canonical paths: lease=%q Admit=%q want=%q", lease.CgroupPath, hooks.controllerCgroupPath, cgroup)
	}
}

func TestControllerHooksReconnectStateSync(t *testing.T) {
	controller := startReconnectController(t, false)
	cgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"memory.high": "max", "memory.current": "123"} {
		if err := os.WriteFile(filepath.Join(cgroup, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: controller.socket, CgroupPath: cgroup, SandboxID: "sandbox-new",
		Context: ctx, Logf: t.Logf,
	}, reconnectConfig(t, controller.socket, cgroup))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	if granted, err := hooks.Admit("sandbox-new", 0); err != nil || granted != 768<<20 {
		t.Fatalf("Admit = %d, %v", granted, err)
	}
	if err := hooks.Settled(); err != nil {
		t.Fatal(err)
	}
	select {
	case syncReq := <-controller.synced:
		if syncReq.SandboxID != "sandbox-new" || !syncReq.Settled ||
			syncReq.AppliedAllocatableMemory != 768<<20 || syncReq.PreviousToken != "initial-token" {
			t.Fatalf("StateSync = %+v", syncReq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StateSync timeout")
	}
	waitHooksConnected(t, hooks)
	if !hooks.Connected() || hooks.AllocatableNowMem() != 768<<20 {
		t.Fatalf("reconnected=%v applied=%d", hooks.Connected(), hooks.AllocatableNowMem())
	}
	if err := os.RemoveAll(cgroup); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := hooks.RequestBudget(64<<20, resource.UrgencyNormal, "test"); err == nil {
		t.Fatal("budget application unexpectedly succeeded without cgroup")
	}
	if got := hooks.AllocatableNowMem(); got != 768<<20 {
		t.Fatalf("failed apply advanced allocatable to %d", got)
	}
}

func TestSettledNotificationSurvivesLocalEnforcementFailure(t *testing.T) {
	controller := startSettledController(t)
	missingCgroup := filepath.Join(t.TempDir(), "missing-cgroup")
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: controller.socket, CgroupPath: missingCgroup, SandboxID: "settle-on-failure",
		Context: context.Background(), Logf: t.Logf,
	}, reconnectConfig(t, controller.socket, missingCgroup))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	if _, err := hooks.Admit("settle-on-failure", 0); err != nil {
		t.Fatal(err)
	}
	if err := hooks.Settled(); err == nil {
		t.Fatal("Settled hid the local enforcement failure")
	}
	select {
	case req := <-controller.settled:
		if req.Type != resource.TypeSettled {
			t.Fatalf("settled request = %+v", req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local enforcement failure suppressed Settled RPC")
	}
	if !hooks.localState().settled {
		t.Fatal("local settle barrier was not retained")
	}
}

func TestControllerHooksFallsBackToLegacyReattach(t *testing.T) {
	controller := startReconnectController(t, true)
	cgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"memory.high": "max", "memory.current": "1"} {
		if err := os.WriteFile(filepath.Join(cgroup, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: controller.socket, CgroupPath: cgroup, SandboxID: "sandbox-legacy",
		Context: context.Background(), Logf: t.Logf,
	}, reconnectConfig(t, controller.socket, cgroup))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	if _, err := hooks.Admit("sandbox-legacy", 0); err != nil {
		t.Fatal(err)
	}
	if err := hooks.Settled(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.synced:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy Reattach timeout")
	}
	waitHooksConnected(t, hooks)
	if !hooks.Connected() || hooks.AllocatableNowMem() != 768<<20 {
		t.Fatalf("legacy connected=%v alloc=%d", hooks.Connected(), hooks.AllocatableNowMem())
	}
}

func waitHooksConnected(t *testing.T, hooks *ControllerHooks) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !hooks.Connected() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hooks.Connected() {
		t.Fatal("hooks did not become connected")
	}
}
