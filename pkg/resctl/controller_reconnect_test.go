package resctl

import (
	"context"
	"fmt"
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
	syncAlloc uint64
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
	synced   chan resource.Message
	reject   bool
	done     chan struct{}
}

func startSettledController(t *testing.T, reject ...bool) *settledController {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "controller.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	c := &settledController{
		t: t, socket: socket, listener: listener,
		settled: make(chan resource.Message, 1), synced: make(chan resource.Message, 1), done: make(chan struct{}),
	}
	if len(reject) > 0 {
		c.reject = reject[0]
	}
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
		if !c.reject {
			_ = resource.WriteMessage(conn, &resource.Message{Type: resource.TypeAck})
			for {
				req, err := resource.ReadMessage(conn)
				if err != nil {
					return
				}
				switch req.Type {
				case resource.TypeHeartbeat:
					_ = resource.WriteMessage(conn, &resource.Message{
						Type: resource.TypeAck, NewAllocatable: admit.StartupBudgetMemory,
					})
				case resource.TypeRelease:
					_ = resource.WriteMessage(conn, &resource.Message{Type: resource.TypeAck})
					return
				default:
					_ = resource.WriteMessage(conn, &resource.Message{Type: resource.TypeAck})
				}
			}
		}
		_ = resource.WriteMessage(conn, &resource.Message{Type: resource.TypeError, Msg: "invalid session token"})
		_ = conn.Close()

		reconnected, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer reconnected.Close()
		syncReq, err := resource.ReadMessage(reconnected)
		if err != nil || syncReq.Type != resource.TypeStateSync {
			t.Errorf("state sync request = %+v err=%v", syncReq, err)
			return
		}
		_ = resource.WriteMessage(reconnected, &resource.Message{
			Type: resource.TypeAck, Token: "settled-sync-token", NewAllocatable: syncReq.AppliedAllocatableMemory,
		})
		c.synced <- *syncReq
		release, err := resource.ReadMessage(reconnected)
		if err == nil && release.Type == resource.TypeRelease {
			_ = resource.WriteMessage(reconnected, &resource.Message{Type: resource.TypeAck})
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

type sessionErrorController struct {
	t          *testing.T
	socket     string
	rejectType string
	listener   *net.UnixListener
	synced     chan resource.Message
	done       chan struct{}
}

func startSessionErrorController(t *testing.T, rejectType string) *sessionErrorController {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "controller.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	c := &sessionErrorController{
		t: t, socket: socket, rejectType: rejectType, listener: listener,
		synced: make(chan resource.Message, 1), done: make(chan struct{}),
	}
	go func() {
		defer close(c.done)
		first, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		admit, err := resource.ReadMessage(first)
		if err != nil || admit.Type != resource.TypeAdmit {
			t.Errorf("admit request = %+v err=%v", admit, err)
			_ = first.Close()
			return
		}
		_ = resource.WriteMessage(first, &resource.Message{
			Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted,
			Token: "rejected-session-token", GrantedInitialAlloc: admit.StartupBudgetMemory,
		})
		rejected, err := resource.ReadMessage(first)
		if err != nil || rejected.Type != rejectType {
			t.Errorf("rejected request = %+v err=%v, want %s", rejected, err, rejectType)
			_ = first.Close()
			return
		}
		_ = resource.WriteMessage(first, &resource.Message{Type: resource.TypeError, Msg: "no reservation"})
		_ = first.Close()

		second, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer second.Close()
		syncReq, err := resource.ReadMessage(second)
		if err != nil || syncReq.Type != resource.TypeStateSync {
			t.Errorf("state sync request = %+v err=%v", syncReq, err)
			return
		}
		_ = resource.WriteMessage(second, &resource.Message{
			Type: resource.TypeAck, Token: "recovered-session-token",
			NewAllocatable: syncReq.AppliedAllocatableMemory,
		})
		c.synced <- *syncReq
		for {
			req, err := resource.ReadMessage(second)
			if err != nil {
				return
			}
			if req.Type == resource.TypeRelease {
				_ = resource.WriteMessage(second, &resource.Message{Type: resource.TypeAck})
				return
			}
			_ = resource.WriteMessage(second, &resource.Message{Type: resource.TypeAck})
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			t.Error("session error controller did not stop")
		}
	})
	return c
}

func startReconnectController(t *testing.T, legacy bool, syncAlloc ...uint64) *reconnectController {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "controller.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	c := &reconnectController{t: t, socket: socket, legacy: legacy, listener: listener, done: make(chan struct{}), synced: make(chan resource.Message, 1)}
	if len(syncAlloc) > 0 {
		c.syncAlloc = syncAlloc[0]
	}
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
		newAlloc := syncReq.AppliedAllocatableMemory
		if c.syncAlloc > 0 {
			newAlloc = c.syncAlloc
		}
		_ = resource.WriteMessage(second, &resource.Message{Type: resource.TypeAck, Token: "synced-token", NewAllocatable: newAlloc})
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
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, "intermediate"), 0o755); err != nil {
		t.Fatal(err)
	}
	cgroup := filepath.Join(dir, "target")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	unclean := filepath.Join(dir, "intermediate", "..", "target") + string(filepath.Separator)
	socket := filepath.Join("intermediate", "..", "controller.sock")
	wantSocket := filepath.Join(dir, "controller.sock")
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
	if lease.ControllerSocket != wantSocket || hooks.opts.SocketPath != wantSocket || hooks.client.SocketPath != wantSocket {
		t.Fatalf("canonical controller sockets: lease=%q hooks=%q client=%q want=%q",
			lease.ControllerSocket, hooks.opts.SocketPath, hooks.client.SocketPath, wantSocket)
	}
}

func TestControllerHooksRoundsPositiveCPUFloorUp(t *testing.T) {
	dir := t.TempDir()
	cgroup := filepath.Join(dir, "cgroup")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "controller.sock")
	cfg := reconnectConfig(t, socket, cgroup)
	cfg.Resources.Allocatable.CPU = 0.0005
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: socket, SandboxID: "sub-millicore", Context: context.Background(), Logf: t.Logf,
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	lease, err := resource.ReadLease(hooks.lease.Path())
	if err != nil {
		t.Fatal(err)
	}
	if lease.FloorCPUMilli != 1 {
		t.Fatalf("floor CPU = %d millicores, want 1", lease.FloorCPUMilli)
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

func TestControllerHooksAppliesStateSyncAllocation(t *testing.T) {
	const syncAlloc = uint64(640 << 20)
	controller := startReconnectController(t, false, syncAlloc)
	cgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"memory.high": "max", "memory.current": "123"} {
		if err := os.WriteFile(filepath.Join(cgroup, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: controller.socket, CgroupPath: cgroup, SandboxID: "state-sync-allocation",
		Context: context.Background(), Logf: t.Logf,
	}, reconnectConfig(t, controller.socket, cgroup))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	if _, err := hooks.Admit("state-sync-allocation", 0); err != nil {
		t.Fatal(err)
	}
	if err := hooks.Settled(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.synced:
	case <-time.After(5 * time.Second):
		t.Fatal("StateSync timeout")
	}
	waitHooksConnected(t, hooks)
	if got := hooks.AllocatableNowMem(); got != syncAlloc {
		t.Fatalf("applied allocation = %d, want %d", got, syncAlloc)
	}
	wantHigh := uint64(float64(syncAlloc) * 0.875)
	data, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != fmt.Sprint(wantHigh) {
		t.Fatalf("memory.high = %q, want %d", data, wantHigh)
	}
}

func TestControllerHooksDefersPreSettledStateSyncAllocation(t *testing.T) {
	const (
		startupAlloc = uint64(768 << 20)
		syncAlloc    = uint64(640 << 20)
	)
	controller := startReconnectController(t, false, syncAlloc)
	cgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"memory.high": "max", "memory.current": "123"} {
		if err := os.WriteFile(filepath.Join(cgroup, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: controller.socket, CgroupPath: cgroup, SandboxID: "pre-settle-sync",
		Context: context.Background(), Logf: t.Logf,
	}, reconnectConfig(t, controller.socket, cgroup))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	if _, err := hooks.Admit("pre-settle-sync", 0); err != nil {
		t.Fatal(err)
	}

	// Detect the server-side close before crossing hello/restore_ack.
	hooks.heartbeatOnce(context.Background())
	select {
	case syncReq := <-controller.synced:
		if syncReq.Settled || syncReq.AppliedAllocatableMemory != startupAlloc {
			t.Fatalf("pre-settle StateSync = %+v", syncReq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pre-settle StateSync timeout")
	}
	waitHooksConnected(t, hooks)
	state := hooks.localState()
	if state.applied != startupAlloc || state.desired != syncAlloc || !state.enforcementPending {
		t.Fatalf("deferred state = %+v", state)
	}
	data, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "max" {
		t.Fatalf("pre-settle StateSync wrote memory.high=%q", data)
	}

	if err := hooks.Settled(); err != nil {
		t.Fatal(err)
	}
	state = hooks.localState()
	if state.applied != syncAlloc || state.desired != syncAlloc || state.enforcementPending {
		t.Fatalf("settled state = %+v", state)
	}
	wantHigh := fmt.Sprint(uint64(float64(syncAlloc) * 0.875))
	data, err = os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != wantHigh {
		t.Fatalf("settled memory.high = %q, want %q", data, wantHigh)
	}
}

func TestControllerHooksDefersPreRestoreAckStateSyncAllocation(t *testing.T) {
	const (
		allocAtSnapshot = uint64(704 << 20)
		admitAlloc      = uint64(768 << 20)
		syncAlloc       = uint64(640 << 20)
	)
	controller := startReconnectController(t, false, syncAlloc)
	cgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"memory.high": "max", "memory.current": "123"} {
		if err := os.WriteFile(filepath.Join(cgroup, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: controller.socket, CgroupPath: cgroup, SandboxID: "pre-restore-ack-sync",
		Context: context.Background(), Logf: t.Logf,
	}, reconnectConfig(t, controller.socket, cgroup))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	hooks.SetRestoreAppliedAllocatable(allocAtSnapshot)
	grant, err := hooks.Admit("pre-restore-ack-sync", allocAtSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if grant != admitAlloc {
		t.Fatalf("admit grant = %d, want %d", grant, admitAlloc)
	}

	// Detect the server-side close before restore_ack, then ensure the
	// recovered target remains deferred and replaces the stale Admit grant.
	hooks.heartbeatOnce(context.Background())
	select {
	case syncReq := <-controller.synced:
		if syncReq.Settled || syncReq.AppliedAllocatableMemory != allocAtSnapshot {
			t.Fatalf("pre-restore_ack StateSync = %+v", syncReq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pre-restore_ack StateSync timeout")
	}
	waitHooksConnected(t, hooks)
	if state := hooks.localState(); state.applied != allocAtSnapshot || state.desired != syncAlloc || !state.enforcementPending {
		t.Fatalf("deferred restore state = %+v", state)
	}
	data, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "max" {
		t.Fatalf("pre-restore_ack StateSync wrote memory.high=%q", data)
	}

	if err := hooks.SettledRestore(allocAtSnapshot, grant); err != nil {
		t.Fatal(err)
	}
	state := hooks.localState()
	if state.applied != syncAlloc || state.desired != syncAlloc || state.enforcementPending {
		t.Fatalf("settled restore state = %+v", state)
	}
	wantHigh := fmt.Sprint(uint64(float64(syncAlloc) * 0.875))
	data, err = os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != wantHigh {
		t.Fatalf("settled restore memory.high = %q, want %q", data, wantHigh)
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

func TestHeartbeatRetriesPendingInitialEnforcement(t *testing.T) {
	controller := startSettledController(t)
	cgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroup, "memory.current"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: controller.socket, CgroupPath: cgroup, SandboxID: "retry-initial-enforcement",
		Context: context.Background(), Logf: t.Logf,
	}, reconnectConfig(t, controller.socket, cgroup))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	if _, err := hooks.Admit("retry-initial-enforcement", 0); err != nil {
		t.Fatal(err)
	}
	if err := hooks.Settled(); err == nil {
		t.Fatal("Settled hid missing memory.high")
	}
	if state := hooks.localState(); !state.settled || !state.enforcementPending {
		t.Fatalf("failed settle state = %+v", state)
	}
	if err := os.WriteFile(filepath.Join(cgroup, "memory.high"), []byte("max"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooks.heartbeatOnce(context.Background())
	state := hooks.localState()
	if state.enforcementPending || state.applied != 768<<20 {
		t.Fatalf("heartbeat retry state = %+v", state)
	}
	wantHigh := fmt.Sprint(uint64(float64(uint64(768<<20)) * 0.875))
	data, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != wantHigh {
		t.Fatalf("retried memory.high = %q, want %q", data, wantHigh)
	}
}

func TestSettledExplicitErrorReconnectsWithSettledState(t *testing.T) {
	controller := startSettledController(t, true)
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
		SocketPath: controller.socket, CgroupPath: cgroup, SandboxID: "settled-reconnect",
		Context: context.Background(), Logf: t.Logf,
	}, reconnectConfig(t, controller.socket, cgroup))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.Release("test")
	if _, err := hooks.Admit("settled-reconnect", 0); err != nil {
		t.Fatal(err)
	}
	if err := hooks.Settled(); err == nil {
		t.Fatal("Settled hid the controller rejection")
	}
	select {
	case syncReq := <-controller.synced:
		if syncReq.SandboxID != "settled-reconnect" || !syncReq.Settled ||
			syncReq.AppliedAllocatableMemory != 768<<20 || syncReq.PreviousToken != "settle-token" {
			t.Fatalf("StateSync after Settled rejection = %+v", syncReq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StateSync after Settled rejection timed out")
	}
	waitHooksConnected(t, hooks)
	if got := hooks.localState(); !got.connected || !got.settled || got.token != "settled-sync-token" {
		t.Fatalf("recovered state = %+v", got)
	}
}

func TestSessionErrorsReconnectThroughStateSync(t *testing.T) {
	for _, rpcType := range []string{resource.TypeHeartbeat, resource.TypeRequestBudget} {
		t.Run(rpcType, func(t *testing.T) {
			controller := startSessionErrorController(t, rpcType)
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
				SocketPath: controller.socket, CgroupPath: cgroup, SandboxID: "session-error-" + rpcType,
				Context: context.Background(), Logf: t.Logf,
			}, reconnectConfig(t, controller.socket, cgroup))
			if err != nil {
				t.Fatal(err)
			}
			defer hooks.Release("test")
			if _, err := hooks.Admit("session-error-"+rpcType, 0); err != nil {
				t.Fatal(err)
			}
			switch rpcType {
			case resource.TypeHeartbeat:
				hooks.heartbeatOnce(context.Background())
			case resource.TypeRequestBudget:
				if _, _, _, err := hooks.RequestBudget(64<<20, resource.UrgencyNormal, "test"); err == nil {
					t.Fatal("RequestBudget hid the rejected session")
				}
			}
			select {
			case syncReq := <-controller.synced:
				if syncReq.SandboxID != "session-error-"+rpcType || syncReq.PreviousToken != "rejected-session-token" {
					t.Fatalf("StateSync after %s rejection = %+v", rpcType, syncReq)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("StateSync after %s rejection timed out", rpcType)
			}
			waitHooksConnected(t, hooks)
			if state := hooks.localState(); !state.connected || state.token != "recovered-session-token" {
				t.Fatalf("recovered %s state = %+v", rpcType, state)
			}
		})
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

func TestControllerHooksReleaseCancelsLifetimeBeforeWaiting(t *testing.T) {
	lifetimeCtx, cancelLifetime := context.WithCancel(context.Background())
	hooks := &ControllerHooks{
		opts:           ControllerHookOptions{Logf: t.Logf},
		lifetimeCtx:    lifetimeCtx,
		cancelLifetime: cancelLifetime,
	}
	hooks.bgWG.Add(1)
	go func() {
		defer hooks.bgWG.Done()
		<-lifetimeCtx.Done()
	}()
	done := make(chan struct{})
	go func() {
		hooks.Release("test")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Release waited for background work before cancelling its lifetime context")
	}
}

func TestControllerHooksLifetimeFollowsRunContext(t *testing.T) {
	runCtx, cancelRun := context.WithCancel(context.Background())
	dir := t.TempDir()
	cgroup := filepath.Join(dir, "cgroup")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "controller.sock")
	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: socket, SandboxID: "run-context", Context: runCtx, Logf: t.Logf,
	}, reconnectConfig(t, socket, cgroup))
	if err != nil {
		t.Fatal(err)
	}
	cancelRun()
	select {
	case <-hooks.lifetimeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("run context cancellation did not cancel controller lifetime")
	}
	hooks.Release("test")
}
