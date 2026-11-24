package sandbox

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func shutdownRuntimeFixture(send func(context.Context) error) *Runtime {
	life, cancel := context.WithCancelCause(context.Background())
	return &Runtime{state: runtimeRunning, environmentActive: true, sandboxID: "sandbox", spec: RuntimeSpec{RunID: "run"}, lifeCtx: life, cancelLife: cancel, exitDone: make(chan struct{}), shutdownGuest: send, shutdownGrace: time.Second}
}

func TestRuntimeShutdownACKDoesNotCompleteAndCallerCancelDoesNotUndo(t *testing.T) {
	acked := make(chan struct{})
	var sends atomic.Int32
	r := shutdownRuntimeFixture(func(context.Context) error { sends.Add(1); close(acked); return nil })
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Shutdown(ctx) }()
	select {
	case <-acked:
	case <-time.After(time.Second):
		t.Fatal("shutdown not sent")
	}
	first := r.shutdownFinalDeadline()
	select {
	case err := <-done:
		t.Fatalf("ACK reported final completion: %v", err)
	default:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// A duplicate cannot extend or shrink the original owner deadline.
	if err := r.beginShutdown(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r.shutdownFinalDeadline() != first || sends.Load() != 1 {
		t.Fatal("duplicate reset deadline or resent shutdown")
	}
	if r.State() != "closing" {
		t.Fatal("caller cancellation changed owner state")
	}
	r.mu.Lock()
	r.environmentExit = &proto.EnvironmentExit{State: proto.EnvironmentStopped}
	r.mu.Unlock()
	r.finishExit(ExitResult{})
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeShutdownDeadlineIndependentOfBlockedSend(t *testing.T) {
	started := make(chan struct{})
	sendDone := make(chan struct{})
	r := shutdownRuntimeFixture(func(ctx context.Context) error { close(started); <-ctx.Done(); close(sendDone); return ctx.Err() })
	deadline := time.Now().Add(50 * time.Millisecond)
	if err := r.beginShutdown(deadline); err != nil {
		t.Fatal(err)
	}
	<-started
	select {
	case <-r.exitDone:
	case <-time.After(time.Second):
		t.Fatal("blocked control request suppressed owner deadline")
	}
	select {
	case <-sendDone:
	case <-time.After(time.Second):
		t.Fatal("control call was not canceled")
	}
	if !r.shutdown.escalated.Load() || r.shutdownFinalDeadline() != deadline {
		t.Fatal("deadline did not cause bounded retirement")
	}
	if err := r.waitShutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("forced retirement returned success: %v", err)
	}
}

func TestRuntimeShutdownDoesNotHoldOperationOrWorkLocksWhileWaiting(t *testing.T) {
	acked := make(chan struct{})
	r := shutdownRuntimeFixture(func(context.Context) error { close(acked); return nil })
	r.workMu.Lock()
	r.opsWG.Add(1)
	done := make(chan error, 1)
	go func() { done <- r.Shutdown(context.Background()) }()
	select {
	case <-acked:
	case <-time.After(time.Second):
		t.Fatal("shutdown needed workMu")
	}
	retiring := make(chan struct{})
	go func() {
		r.mu.Lock()
		r.environmentExit = &proto.EnvironmentExit{State: proto.EnvironmentStopped}
		r.mu.Unlock()
		r.finishExit(ExitResult{})
		close(retiring)
	}()
	r.opsWG.Done()
	r.workMu.Unlock()
	select {
	case <-retiring:
	case <-time.After(time.Second):
		t.Fatal("shutdown waiter held an operation slot")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not see real retirement")
	}
}

func TestRuntimeShutdownRejectsWrongRunCaptureAndExpiredRequest(t *testing.T) {
	r := shutdownRuntimeFixture(func(context.Context) error { t.Error("rejected shutdown sent guest request"); return nil })
	defer r.Close()
	wrong := ctl.ShutdownRequest{Target: ctl.ShutdownTarget{SandboxID: "sandbox", RunID: "old-run"}}
	if _, err := r.prepareShutdown(wrong); !errors.Is(err, ctl.ErrShutdownTargetMismatch) {
		t.Fatalf("target err=%v", err)
	}
	if err := r.beginShutdown(time.Now().Add(-time.Second)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired err=%v", err)
	}
	for _, terminal := range []bool{false, true} {
		r.diskSnapshot = &SnapshotHandler{}
		r.diskSnapshot.capture.active = !terminal
		r.diskSnapshot.capture.terminal = terminal
		if err := r.beginShutdown(time.Time{}); !errors.Is(err, ctl.ErrShutdownUnavailable) {
			t.Fatalf("capture err=%v", err)
		}
	}
	if r.shutdown != nil || r.State() != "running" {
		t.Fatal("rejected request consumed shutdown admission")
	}
}

func TestRuntimeShutdownPreparedRequestLatchesOnlyAtDetachedBegin(t *testing.T) {
	r := shutdownRuntimeFixture(func(context.Context) error { return nil })
	defer r.Close()
	target := ctl.ShutdownTarget{SandboxID: "sandbox", RunID: "run"}
	raw, err := ctl.MonotonicDeadline(time.Now().Add(30 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	op, err := r.prepareShutdown(ctl.ShutdownRequest{Target: target, DeadlineMonotonicNS: raw})
	if err != nil {
		t.Fatal(err)
	}
	if r.shutdown != nil || r.State() != "running" {
		t.Fatal("parse started owner before detach")
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := op.Begin(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired before latch accepted: %v", err)
	}
	if r.shutdown != nil {
		t.Fatal("expired request fixed an owner deadline")
	}
}

func TestRuntimeShutdownRetriesReportSameAbsoluteDeadline(t *testing.T) {
	r := shutdownRuntimeFixture(func(context.Context) error { return nil })
	defer r.Close()
	req := ctl.ShutdownRequest{Target: ctl.ShutdownTarget{SandboxID: "sandbox", RunID: "run"}}
	first, err := r.prepareShutdown(req)
	if err != nil {
		t.Fatal(err)
	}
	d1, err := first.Begin()
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.prepareShutdown(req)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := second.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if d1 <= 0 || d1 != d2 {
		t.Fatalf("retry deadline changed: %d -> %d", d1, d2)
	}
}

func TestRuntimeShutdownBoundsWaitWithoutPretendingReaperCompletion(t *testing.T) {
	r := shutdownRuntimeFixture(func(context.Context) error { return nil })
	r.engineStarted = true
	r.signals = make(chan os.Signal, 1)
	deadline := time.Now().Add(40 * time.Millisecond)
	if err := r.beginShutdown(deadline); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := r.waitShutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing reaper result=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("SDK ignored fixed owner deadline")
	}
	select {
	case <-r.exitDone:
		t.Fatal("deadline faked resource retirement")
	default:
	}
	select {
	case sig := <-r.signals:
		if sig != syscall.SIGTERM {
			t.Fatal(sig)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded caller return canceled owner fence")
	}
	r.finishExit(ExitResult{}) // the independent real reaper finally completes
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeShutdownReapedGuestDoesNotEscalateDuringCleanup(t *testing.T) {
	r := shutdownRuntimeFixture(func(context.Context) error { return nil })
	reaped := make(chan struct{})
	r.reaped = reaped
	r.engineStarted = true
	r.signals = make(chan os.Signal, 1)
	if err := r.beginShutdown(time.Now().Add(40 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	close(reaped)
	if err := r.waitShutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished cleanup result=%v", err)
	}
	if r.shutdown.escalated.Load() {
		t.Fatal("cleanup timeout misreported as Guest shutdown escalation")
	}
	select {
	case sig := <-r.signals:
		t.Fatalf("signaled already-reaped VMM: %v", sig)
	default:
	}
	r.finishExit(ExitResult{})
}

// Direct mode preserves the preexisting StopUnit/Close path rather than
// beginning a normal Guest/systemd shutdown transaction.
func TestDirectRuntimeShutdownDoesNotChangeLifecycle(t *testing.T) {
	r := shutdownRuntimeFixture(func(context.Context) error {
		t.Fatal("direct run incorrectly received Guest shutdown")
		return nil
	})
	r.environmentActive = false
	if err := r.Shutdown(context.Background()); !errors.Is(err, ctl.ErrShutdownUnsupported) {
		t.Fatalf("direct Shutdown = %v, want unsupported", err)
	}
	if r.shutdown != nil || r.State() != "running" {
		t.Fatalf("direct shutdown changed lifecycle: state=%s shutdown=%+v", r.State(), r.shutdown)
	}
	r.finishExit(ExitResult{})
}
