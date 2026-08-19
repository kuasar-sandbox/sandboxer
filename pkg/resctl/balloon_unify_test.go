package resctl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"golang.org/x/sys/unix"
)

// fakeCHResize spins a tiny HTTP/1.1-over-UDS server that records every
// /api/v1/vm.resize PUT and replies 204. Mirrors enough of CH to exercise
// BalloonController.callResize end-to-end.
type fakeCHResize struct {
	sock     string
	listen   net.Listener
	wg       sync.WaitGroup
	mu       sync.Mutex
	calls    []uint64 // desired_balloon values seen, in order
	current  uint64
	failures int
	dropAcks int
	infoFail int
	stopped  atomic.Bool
}

func newFakeCHResize(t *testing.T) *fakeCHResize {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "ch.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeCHResize{sock: sock, listen: l}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		fail := s.infoFail > 0
		if fail {
			s.infoFail--
		}
		current := s.current
		s.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"config":{"balloon":{"size":%d}}}`, current)
	})
	mux.HandleFunc("/api/v1/vm.resize", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DesiredBalloon uint64 `json:"desired_balloon"`
		}
		// minimal JSON parse; CH's body is one short line.
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		_ = body
		var v uint64
		// Locate "desired_balloon": <num> manually to avoid an extra dep.
		raw := string(buf[:n])
		for i := 0; i < len(raw); i++ {
			if raw[i] >= '0' && raw[i] <= '9' {
				for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
					v = v*10 + uint64(raw[i]-'0')
					i++
				}
				break
			}
		}
		s.mu.Lock()
		s.calls = append(s.calls, v)
		fail := s.failures > 0
		if fail {
			s.failures--
		}
		dropAck := s.dropAcks > 0
		if dropAck {
			s.dropAcks--
		}
		if !fail {
			s.current = v
		}
		s.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if dropAck {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("response writer cannot drop resize acknowledgement")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack resize response: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = srv.Serve(l)
	}()
	t.Cleanup(func() {
		s.stopped.Store(true)
		_ = srv.Shutdown(context.Background())
		s.wg.Wait()
	})
	return s
}

func (s *fakeCHResize) failNext(count int) {
	s.mu.Lock()
	s.failures = count
	s.mu.Unlock()
}

func (s *fakeCHResize) setCurrent(size uint64) {
	s.mu.Lock()
	s.current = size
	s.mu.Unlock()
}

func (s *fakeCHResize) dropAckNext(count int) {
	s.mu.Lock()
	s.dropAcks = count
	s.mu.Unlock()
}

func (s *fakeCHResize) failInfoNext(count int) {
	s.mu.Lock()
	s.infoFail = count
	s.mu.Unlock()
}

func (s *fakeCHResize) currentSize() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *fakeCHResize) seen() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint64, len(s.calls))
	copy(out, s.calls)
	return out
}

// TestBalloon_SetAllocatableComputesTarget: SetAllocatable(alloc) should
// store balloon = Capacity - alloc; clamp to 0 when alloc >= Capacity.
func TestBalloon_SetAllocatableComputesTarget(t *testing.T) {
	b := NewBalloonController("/dev/null", 8<<30, nil)
	b.SetAllocatable(256 << 20)
	if got := b.CurrentTarget(); got != (8<<30)-(256<<20) {
		t.Errorf("target = %d, want %d", got, (8<<30)-(256<<20))
	}
	b.SetAllocatable(8 << 30)
	if got := b.CurrentTarget(); got != 0 {
		t.Errorf("alloc == cap → target should be 0, got %d", got)
	}
	b.SetAllocatable(16 << 30) // > cap
	if got := b.CurrentTarget(); got != 0 {
		t.Errorf("alloc > cap → target should be 0, got %d", got)
	}
}

func TestBalloon_SeedAppliedAllocatableSkipsInitialResize(t *testing.T) {
	srv := newFakeCHResize(t)
	b := NewBalloonController(srv.sock, 8<<30, nil)
	b.Interval = 100 * time.Millisecond
	b.SeedAppliedAllocatable(256 << 20)
	wantInitial := uint64((8 << 30) - (256 << 20))
	if got := b.CurrentTarget(); got != wantInitial {
		t.Fatalf("target = %d, want %d", got, wantInitial)
	}
	if got := b.CurrentActual(); got != wantInitial {
		t.Fatalf("actual = %d, want %d", got, wantInitial)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	if calls := srv.seen(); len(calls) != 0 {
		t.Fatalf("seeded Start issued resize calls: %v", calls)
	}

	time.Sleep(b.Interval + 20*time.Millisecond)
	b.SetAllocatable(512 << 20)
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && len(srv.seen()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	calls := srv.seen()
	if len(calls) != 1 || calls[0] != (8<<30)-(512<<20) {
		t.Fatalf("later resize calls = %v", calls)
	}
}

func TestBalloon_SeedRestoredStateReconcilesInFlightInflation(t *testing.T) {
	srv := newFakeCHResize(t)
	b := NewBalloonController(srv.sock, 1<<30, nil)
	// Snapshot effective allocation used current=100 MiB, but CH restores the
	// still-in-flight num_pages target of 200 MiB.
	b.SeedRestoredState((1<<30)-(100<<20), 200<<20)
	if got := b.CurrentTarget(); got != 100<<20 {
		t.Fatalf("desired restored target = %d, want %d", got, uint64(100<<20))
	}
	if got := b.CurrentActual(); got != 200<<20 {
		t.Fatalf("CH restored target = %d, want %d", got, uint64(200<<20))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if calls := srv.seen(); len(calls) != 1 || calls[0] != 100<<20 {
		t.Fatalf("in-flight restore resize calls = %v, want [%d]", calls, uint64(100<<20))
	}
}

func TestBalloon_StartRetriesAfterInitialReconcileFailure(t *testing.T) {
	srv := newFakeCHResize(t)
	srv.failNext(1)
	b := NewBalloonController(srv.sock, 1<<30, nil)
	b.Interval = 10 * time.Millisecond
	b.SeedRestoredState((1<<30)-(100<<20), 200<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err == nil {
		t.Fatal("Start hid the injected initial reconcile failure")
	}
	defer b.Stop()
	deadline := time.Now().Add(time.Second)
	for b.CurrentActual() != 100<<20 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := b.CurrentActual(); got != 100<<20 {
		t.Fatalf("background retry left actual target at %d, want %d; calls=%v", got, uint64(100<<20), srv.seen())
	}
	if calls := srv.seen(); len(calls) < 2 || calls[0] != 100<<20 || calls[1] != 100<<20 {
		t.Fatalf("initial failure/retry calls = %v, want repeated %d", calls, uint64(100<<20))
	}
}

// TestBalloon_KickFiresImmediatelyWhenElapsed: when the previous
// reconcile is older than Interval, a SetAllocatable should trigger an
// immediate /vm.resize call (not wait for the ticker).
func TestBalloon_KickFiresImmediatelyWhenElapsed(t *testing.T) {
	srv := newFakeCHResize(t)
	b := NewBalloonController(srv.sock, 8<<30, nil)
	b.Interval = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.SetAllocatable(256 << 20) // target = (8G - 256M)
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// Initial reconcile during Start. Wait long enough for the next-allowed
	// kick window to open (just past Interval), then send a new target.
	time.Sleep(b.Interval + 50*time.Millisecond)

	b.SetAllocatable(512 << 20) // target = (8G - 512M)
	// Kick should fire within a few ms; allow generous wait for CI jitter.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(srv.seen()) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	calls := srv.seen()
	if len(calls) < 2 {
		t.Fatalf("expected ≥ 2 resize calls (initial + kick), got %d: %v", len(calls), calls)
	}
	if calls[0] != (8<<30)-(256<<20) {
		t.Errorf("call[0] = %d, want %d (initial)", calls[0], (8<<30)-(256<<20))
	}
	if calls[1] != (8<<30)-(512<<20) {
		t.Errorf("call[1] = %d, want %d (kicked)", calls[1], (8<<30)-(512<<20))
	}
}

// TestBalloon_KickRateLimited: two rapid SetAllocatable calls within
// MinInterval should produce at most ONE extra Reconcile beyond the
// initial Start-time Reconcile (the second is rate-limited and falls
// through to the next ticker tick, which lands on the latest target —
// so we observe at most 2 distinct on-wire values).
func TestBalloon_KickRateLimited(t *testing.T) {
	srv := newFakeCHResize(t)
	b := NewBalloonController(srv.sock, 8<<30, nil)
	b.Interval = 500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.SetAllocatable(256 << 20) // target 0
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// Two kicks back-to-back, well within MinInterval of the Start
	// reconcile (which just happened):
	time.Sleep(10 * time.Millisecond)
	b.SetAllocatable(300 << 20)
	time.Sleep(5 * time.Millisecond)
	b.SetAllocatable(400 << 20)

	// Wait less than MinInterval — no extra reconcile yet.
	time.Sleep(b.Interval / 4)
	calls := srv.seen()
	if len(calls) != 1 {
		t.Fatalf("within Interval/4 expected just 1 call (the initial), got %d: %v",
			len(calls), calls)
	}

	// Now wait past MinInterval — the pending target (400 MiB alloc) is
	// applied by the next ticker tick.
	time.Sleep(b.Interval)
	calls = srv.seen()
	if len(calls) < 2 {
		t.Fatalf("after one full Interval expected ≥ 2 calls, got %d: %v",
			len(calls), calls)
	}
	last := calls[len(calls)-1]
	wantLast := uint64((8 << 30) - (400 << 20))
	if last != wantLast {
		t.Errorf("last call = %d, want %d (latest target, dropped intermediates)",
			last, wantLast)
	}
}

// TestHooks_OnAllocatableChangedTouchesBalloon: dynamic-mode runtime
// adjustment must call balloonCtl.SetAllocatable (and not open its own
// HTTP path).
func TestHooks_OnAllocatableChangedTouchesBalloon(t *testing.T) {
	srv := newFakeCHResize(t)
	b := NewBalloonController(srv.sock, 8<<30, nil)
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()
	h := &ControllerHooks{
		opts: ControllerHookOptions{Balloon: b, Logf: func(string, ...any) {}},
		cfg:  cfg,
	}
	if err := h.OnAllocatableChanged(2 << 30); err != nil {
		t.Fatalf("OnAllocatableChanged: %v", err)
	}
	want := uint64(8<<30) - uint64(2<<30)
	if got := b.CurrentTarget(); got != want {
		t.Errorf("balloon target = %d, want %d", got, want)
	}
	if got := h.AllocatableNowMem(); got != 2<<30 {
		t.Errorf("allocatableNowMem = %d, want %d", got, 2<<30)
	}
}

func TestHooks_OnAllocatableChangedRollsBackMemoryHighWhenBalloonFails(t *testing.T) {
	const (
		capacity      = uint64(8 << 30)
		previousAlloc = uint64(2 << 30)
		newAlloc      = uint64(3 << 30)
	)
	cgroup := t.TempDir()
	memoryHigh := filepath.Join(cgroup, "memory.high")
	previousHigh := []byte("1879048192\n")
	if err := os.WriteFile(memoryHigh, previousHigh, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newFakeCHResize(t)
	srv.setCurrent(capacity - previousAlloc)
	srv.failNext(1)
	b := NewBalloonController(srv.sock, capacity, nil)
	b.SeedAppliedAllocatable(previousAlloc)
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()
	h := &ControllerHooks{
		opts: ControllerHookOptions{
			SocketPath: "/run/node-resource-controller.sock",
			CgroupPath: cgroup,
			Balloon:    b,
			Logf:       func(string, ...any) {},
		},
		cfg:               cfg,
		allocatableNowMem: previousAlloc,
		desiredAllocMem:   previousAlloc,
	}

	if err := h.OnAllocatableChanged(newAlloc); err == nil {
		t.Fatal("OnAllocatableChanged succeeded despite failed balloon enforcement")
	}
	gotHigh, err := os.ReadFile(memoryHigh)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotHigh) != string(previousHigh) {
		t.Fatalf("memory.high = %q after failed balloon, want rollback to %q", gotHigh, previousHigh)
	}
	if got := h.AllocatableNowMem(); got != previousAlloc {
		t.Fatalf("applied allocation advanced to %d, want %d", got, previousAlloc)
	}
	wantBalloon := capacity - previousAlloc
	if got := b.CurrentTarget(); got != wantBalloon {
		t.Fatalf("balloon target = %d after failed apply, want rollback to %d", got, wantBalloon)
	}
	if got := b.CurrentActual(); got != wantBalloon {
		t.Fatalf("balloon actual = %d after failed apply, want %d", got, wantBalloon)
	}
}

func TestHooks_OnAllocatableChangedConfirmsLostBalloonAcknowledgement(t *testing.T) {
	const (
		capacity      = uint64(8 << 30)
		previousAlloc = uint64(2 << 30)
		newAlloc      = uint64(3 << 30)
	)
	cgroup := t.TempDir()
	memoryHigh := filepath.Join(cgroup, "memory.high")
	if err := os.WriteFile(memoryHigh, []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newFakeCHResize(t)
	srv.setCurrent(capacity - previousAlloc)
	srv.dropAckNext(1)
	b := NewBalloonController(srv.sock, capacity, nil)
	b.SeedAppliedAllocatable(previousAlloc)
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()
	h := &ControllerHooks{
		opts: ControllerHookOptions{
			SocketPath: "/run/node-resource-controller.sock",
			CgroupPath: cgroup,
			Balloon:    b,
			Logf:       func(string, ...any) {},
		},
		cfg:               cfg,
		allocatableNowMem: previousAlloc,
		desiredAllocMem:   previousAlloc,
	}

	if err := h.OnAllocatableChanged(newAlloc); err != nil {
		t.Fatalf("confirmed lost acknowledgement was not committed: %v", err)
	}
	wantHigh := fmt.Sprint(uint64(float64(newAlloc) * 0.875))
	gotHigh, err := os.ReadFile(memoryHigh)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotHigh) != wantHigh {
		t.Fatalf("memory.high = %q, want %q", gotHigh, wantHigh)
	}
	if got := h.AllocatableNowMem(); got != newAlloc {
		t.Fatalf("applied allocation = %d, want %d", got, newAlloc)
	}
	wantBalloon := capacity - newAlloc
	if got := b.CurrentTarget(); got != wantBalloon {
		t.Fatalf("balloon target = %d, want %d", got, wantBalloon)
	}
	if got := b.CurrentActual(); got != wantBalloon {
		t.Fatalf("balloon actual = %d, want confirmed %d", got, wantBalloon)
	}
}

func TestHooks_OnAllocatableChangedCompensatesWhenLostAckCannotBeConfirmed(t *testing.T) {
	const (
		capacity      = uint64(8 << 30)
		previousAlloc = uint64(2 << 30)
		newAlloc      = uint64(3 << 30)
	)
	cgroup := t.TempDir()
	memoryHigh := filepath.Join(cgroup, "memory.high")
	previousHigh := []byte("1879048192\n")
	if err := os.WriteFile(memoryHigh, previousHigh, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newFakeCHResize(t)
	previousBalloon := capacity - previousAlloc
	srv.setCurrent(previousBalloon)
	srv.dropAckNext(1)
	srv.failInfoNext(1)
	b := NewBalloonController(srv.sock, capacity, nil)
	b.SeedAppliedAllocatable(previousAlloc)
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()
	h := &ControllerHooks{
		opts: ControllerHookOptions{
			SocketPath: "/run/node-resource-controller.sock",
			CgroupPath: cgroup,
			Balloon:    b,
			Logf:       func(string, ...any) {},
		},
		cfg:               cfg,
		allocatableNowMem: previousAlloc,
		desiredAllocMem:   previousAlloc,
	}

	if err := h.OnAllocatableChanged(newAlloc); err == nil {
		t.Fatal("compensated ambiguous resize did not return the original failure")
	}
	gotHigh, err := os.ReadFile(memoryHigh)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotHigh) != string(previousHigh) {
		t.Fatalf("memory.high = %q after compensation, want %q", gotHigh, previousHigh)
	}
	if got := h.AllocatableNowMem(); got != previousAlloc {
		t.Fatalf("applied allocation = %d after compensation, want %d", got, previousAlloc)
	}
	if got := b.CurrentTarget(); got != previousBalloon {
		t.Fatalf("balloon target = %d after compensation, want %d", got, previousBalloon)
	}
	if got := b.CurrentActual(); got != previousBalloon {
		t.Fatalf("balloon actual = %d after compensation, want %d", got, previousBalloon)
	}
	if got := srv.currentSize(); got != previousBalloon {
		t.Fatalf("Cloud Hypervisor target = %d after compensation, want %d", got, previousBalloon)
	}
	if calls := srv.seen(); len(calls) != 2 || calls[0] != capacity-newAlloc || calls[1] != previousBalloon {
		t.Fatalf("resize calls = %v, want attempted %d then compensation %d", calls, capacity-newAlloc, previousBalloon)
	}
}

func TestHooks_AmbiguousBalloonDoesNotReturnBeforeResolution(t *testing.T) {
	const (
		capacity      = uint64(8 << 30)
		previousAlloc = uint64(2 << 30)
		newAlloc      = uint64(3 << 30)
	)
	cgroup := t.TempDir()
	memoryHigh := filepath.Join(cgroup, "memory.high")
	previousHigh := []byte("1879048192\n")
	if err := os.WriteFile(memoryHigh, previousHigh, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newFakeCHResize(t)
	srv.setCurrent(capacity - previousAlloc)
	srv.dropAckNext(100)
	srv.failInfoNext(100)
	b := NewBalloonController(srv.sock, capacity, nil)
	b.SeedAppliedAllocatable(previousAlloc)
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	h := &ControllerHooks{
		opts: ControllerHookOptions{
			SocketPath: "/run/node-resource-controller.sock",
			CgroupPath: cgroup,
			Balloon:    b,
			Logf:       func(string, ...any) {},
		},
		cfg:               cfg,
		lifetimeCtx:       ctx,
		allocatableNowMem: previousAlloc,
		desiredAllocMem:   previousAlloc,
	}

	started := time.Now()
	err := h.OnAllocatableChanged(newAlloc)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ambiguous resize error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("ambiguous resize returned before fail-closed resolution window: %v", elapsed)
	}
	if calls := srv.seen(); len(calls) < 2 {
		t.Fatalf("ambiguous resize did not attempt explicit compensation: %v", calls)
	}
	gotHigh, readErr := os.ReadFile(memoryHigh)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(gotHigh) != string(previousHigh) {
		t.Fatalf("memory.high = %q after cancelled resolution, want %q", gotHigh, previousHigh)
	}
	if got := h.AllocatableNowMem(); got != previousAlloc {
		t.Fatalf("ambiguous allocation was published as %d, want retained %d", got, previousAlloc)
	}
}

func TestHooks_AmbiguousBalloonReleasesLifecycleLockAndSerializesRollback(t *testing.T) {
	const (
		capacity      = uint64(8 << 30)
		previousAlloc = uint64(2 << 30)
		newAlloc      = uint64(3 << 30)
	)
	cgroup := t.TempDir()
	memoryHigh := filepath.Join(cgroup, "memory.high")
	previousHigh := []byte("1879048192\n")
	if err := os.WriteFile(memoryHigh, previousHigh, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newFakeCHResize(t)
	srv.setCurrent(capacity - previousAlloc)
	srv.dropAckNext(100)
	srv.failInfoNext(100)
	b := NewBalloonController(srv.sock, capacity, nil)
	b.SeedAppliedAllocatable(previousAlloc)
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &ControllerHooks{
		opts: ControllerHookOptions{
			SocketPath: "/run/node-resource-controller.sock",
			CgroupPath: cgroup,
			Balloon:    b,
			Logf:       func(string, ...any) {},
		},
		cfg:               cfg,
		lifetimeCtx:       ctx,
		allocatableNowMem: previousAlloc,
		desiredAllocMem:   previousAlloc,
	}

	applyDone := make(chan error, 1)
	go func() { applyDone <- h.OnAllocatableChanged(newAlloc) }()
	deadline := time.Now().Add(time.Second)
	for len(srv.seen()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls := srv.seen(); len(calls) < 2 {
		cancel()
		t.Fatalf("ambiguous resize did not enter compensation loop: %v", calls)
	}

	var lifecycleLock *os.File
	for time.Now().Before(deadline) {
		candidate, err := os.Open(cgroup)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		err = unix.Flock(int(candidate.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			lifecycleLock = candidate
			break
		}
		_ = candidate.Close()
		if !errors.Is(err, unix.EWOULDBLOCK) {
			cancel()
			t.Fatalf("acquire lifecycle lock: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if lifecycleLock == nil {
		cancel()
		t.Fatal("ambiguous balloon recovery retained the lifecycle lock")
	}

	cancel()
	select {
	case err := <-applyDone:
		_ = lifecycleLock.Close()
		t.Fatalf("allocation returned before rollback acquired lifecycle lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := lifecycleLock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-applyDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ambiguous resize error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("allocation did not complete after lifecycle lock release")
	}
	gotHigh, err := os.ReadFile(memoryHigh)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotHigh) != string(previousHigh) {
		t.Fatalf("memory.high = %q after serialized rollback, want %q", gotHigh, previousHigh)
	}
}

func TestHooks_StaticFailedBalloonKeepsMemoryHighForQueuedRetry(t *testing.T) {
	const (
		capacity      = uint64(8 << 30)
		previousAlloc = uint64(2 << 30)
		newAlloc      = uint64(3 << 30)
	)
	cgroup := t.TempDir()
	memoryHigh := filepath.Join(cgroup, "memory.high")
	if err := os.WriteFile(memoryHigh, []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := NewBalloonController(filepath.Join(t.TempDir(), "missing-ch.sock"), capacity, nil)
	b.SeedAppliedAllocatable(previousAlloc)
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()
	h := &ControllerHooks{
		opts: ControllerHookOptions{CgroupPath: cgroup, Balloon: b, Logf: func(string, ...any) {}},
		cfg:  cfg,
	}

	if err := h.OnAllocatableChanged(newAlloc); err == nil {
		t.Fatal("OnAllocatableChanged succeeded despite failed balloon enforcement")
	}
	gotHigh, err := os.ReadFile(memoryHigh)
	if err != nil {
		t.Fatal(err)
	}
	wantHigh := fmt.Sprint(uint64(float64(newAlloc) * 0.875))
	if string(gotHigh) != wantHigh {
		t.Fatalf("memory.high = %q after queued static apply, want %q", gotHigh, wantHigh)
	}
	if got := b.CurrentTarget(); got != capacity-newAlloc {
		t.Fatalf("queued balloon target = %d, want %d", got, capacity-newAlloc)
	}
	if got := b.CurrentActual(); got != capacity-previousAlloc {
		t.Fatalf("failed balloon actual = %d, want %d", got, capacity-previousAlloc)
	}
}

// TestHooks_SetRestoreAppliedAllocatableDoesNotTouchBalloon covers the
// restore pre-resume path: record observed state without external side effects.
func TestHooks_SetRestoreAppliedAllocatableDoesNotTouchBalloon(t *testing.T) {
	b := NewBalloonController("/dev/null", 8<<30, nil)
	h := &ControllerHooks{opts: ControllerHookOptions{Balloon: b, Logf: func(string, ...any) {}}}
	// Seed balloon target to a known value so we can detect spurious writes.
	b.SetAllocatable(4 << 30)
	wantTarget := b.CurrentTarget()
	h.SetRestoreAppliedAllocatable(1 << 30)
	if got := h.AllocatableNowMem(); got != 1<<30 {
		t.Errorf("allocatableNowMem = %d, want %d", got, 1<<30)
	}
	if got := b.CurrentTarget(); got != wantTarget {
		t.Errorf("balloon target changed under SetAllocatableNow: %d → %d", wantTarget, got)
	}
}

// TestHooks_SettledRestoreBalloonCorrectionOnlyOnMismatch: SettledRestore
// should issue a balloon correction only when the locally-decided
// allocatable differs from allocAtSnap.
func TestHooks_SettledRestoreBalloonCorrectionOnlyOnMismatch(t *testing.T) {
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()

	// Case 1: match → no correction.
	b1 := NewBalloonController("/dev/null", 8<<30, nil)
	b1.SeedAppliedAllocatable(2 << 30)
	initial := b1.CurrentTarget()
	h1 := &ControllerHooks{opts: ControllerHookOptions{Balloon: b1, Logf: func(string, ...any) {}}, cfg: cfg}
	h1.SetRestoreAppliedAllocatable(2 << 30) // == allocAtSnap
	if err := h1.SettledRestore(2<<30, 2<<30); err != nil {
		t.Fatalf("SettledRestore: %v", err)
	}
	if got := b1.CurrentTarget(); got != initial {
		t.Errorf("matching case: balloon target moved %d → %d (should be no-op)", initial, got)
	}

	// Case 2: mismatch → correction applied to balloon.
	srv := newFakeCHResize(t)
	b2 := NewBalloonController(srv.sock, 8<<30, nil)
	b2.SetAllocatable(2 << 30) // snapshot value
	h2 := &ControllerHooks{opts: ControllerHookOptions{Balloon: b2, Logf: func(string, ...any) {}}, cfg: cfg}
	h2.SetRestoreAppliedAllocatable(2 << 30)
	if err := h2.SettledRestore(2<<30, 3<<30); err != nil {
		t.Fatalf("SettledRestore: %v", err)
	}
	want := uint64(8<<30) - uint64(3<<30)
	if got := b2.CurrentTarget(); got != want {
		t.Errorf("mismatch case: balloon target = %d, want %d", got, want)
	}
}

func TestHooks_SettledRestoreFailedCorrectionKeepsObservedAllocation(t *testing.T) {
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()
	b := NewBalloonController(filepath.Join(t.TempDir(), "missing-ch.sock"), 8<<30, nil)
	b.SeedAppliedAllocatable(2 << 30)
	desiredTarget := uint64(8<<30) - uint64(3<<30)
	h := &ControllerHooks{opts: ControllerHookOptions{Balloon: b, Logf: func(string, ...any) {}}, cfg: cfg}
	h.SetRestoreAppliedAllocatable(2 << 30)

	if err := h.SettledRestore(2<<30, 3<<30); err == nil {
		t.Fatal("SettledRestore succeeded despite failed balloon correction")
	}
	if !h.localState().settled {
		t.Fatal("failed post-restore correction lost the crossed settle barrier")
	}
	if got := h.AllocatableNowMem(); got != 2<<30 {
		t.Fatalf("applied allocation advanced after failed correction: got %d want %d", got, uint64(2<<30))
	}
	if got := b.CurrentTarget(); got != desiredTarget {
		t.Fatalf("static failed correction was not queued: target=%d want=%d", got, desiredTarget)
	}
	if err := b.Reconcile(context.Background()); err == nil {
		t.Fatal("queued static correction did not retry failed resize")
	}
}

func TestReconcileReadsTargetAfterSerialization(t *testing.T) {
	srv := newFakeCHResize(t)
	b := NewBalloonController(srv.sock, 8<<30, nil)
	b.defaults()
	b.SeedAppliedAllocatable(1 << 30)
	b.SetAllocatable(2 << 30)

	b.reconcileMu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- b.Reconcile(context.Background())
	}()
	<-started
	// Let Reconcile reach the held serialization boundary. The target update
	// must still be observed after the lock is acquired.
	time.Sleep(20 * time.Millisecond)
	b.SetAllocatable(3 << 30)
	b.reconcileMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := uint64(5 << 30)
	if got := b.CurrentActual(); got != want {
		t.Fatalf("reconciled stale target = %d, want latest %d", got, want)
	}
}

func TestApplyAllocatableOverridesCompetingHintTarget(t *testing.T) {
	srv := newFakeCHResize(t)
	b := NewBalloonController(srv.sock, 8<<30, nil)
	b.SeedAppliedAllocatable(1 << 30)
	b.TargetFreeBuffer = 64 << 20
	b.Slack = 1
	b.MaxStep = 256 << 20

	// Publish a competing hint target before the explicit apply. The explicit
	// controller allocation must still be the resize committed by this call.
	b.Hint(256<<20, 7<<30)
	if err := b.ApplyAllocatable(context.Background(), 2<<30); err != nil {
		t.Fatal(err)
	}
	want := uint64(6 << 30)
	if got := b.CurrentActual(); got != want {
		t.Fatalf("explicit apply actual target = %d, want %d", got, want)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.calls) == 0 || srv.calls[len(srv.calls)-1] != want {
		t.Fatalf("resize calls = %v, want final %d", srv.calls, want)
	}
}

// silence unused-import linter when build tags strip http use elsewhere.
var _ = fmt.Sprintf
