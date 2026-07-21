package sandbox

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeSignaler records signals sent to it; never blocks. Goroutine-safe:
// waitForCHWithSignalEscalation calls Signal from the test goroutine while
// watcher goroutines poll the recorded list.
type fakeSignaler struct {
	mu   sync.Mutex
	sent []os.Signal
}

func (f *fakeSignaler) Signal(sig os.Signal) error {
	f.mu.Lock()
	f.sent = append(f.sent, sig)
	f.mu.Unlock()
	return nil
}

func (f *fakeSignaler) sentCount(sig os.Signal) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sent {
		if s == sig {
			n++
		}
	}
	return n
}

func discardLogf(string, ...any) {}

// TestWaitForCH_NoSignals: clean-exit path — doneCh fires before any
// signal, helper returns immediately with the wait error.
func TestWaitForCH_NoSignals(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}

	doneCh <- nil
	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", 0, time.Second, discardLogf, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(proc.sent) != 0 {
		t.Fatalf("expected no signals sent, got %v", proc.sent)
	}
}

// TestWaitForCH_SIGTERM_GracefulExit: SIGTERM arrives, CH exits within
// grace — only SIGTERM forwarded, no SIGKILL.
func TestWaitForCH_SIGTERM_GracefulExit(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}

	go func() {
		sigCh <- syscall.SIGTERM
		time.Sleep(50 * time.Millisecond)
		doneCh <- &exitErrStub{code: 0}
	}()

	shutdownStarted := make(chan struct{})
	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", 0, 5*time.Second, discardLogf, func() {
		close(shutdownStarted)
	})
	if err == nil {
		t.Fatal("expected non-nil exit err stub")
	}
	if proc.sentCount(syscall.SIGTERM) != 1 {
		t.Fatalf("expected 1 SIGTERM, got %v", proc.sent)
	}
	if proc.sentCount(syscall.SIGKILL) != 0 {
		t.Fatalf("expected no SIGKILL, got %v", proc.sent)
	}
	select {
	case <-shutdownStarted:
	default:
		t.Fatal("shutdown callback was not invoked before graceful exit")
	}
}

// TestWaitForCH_SIGTERM_EscalatesToSIGKILL: CH ignores SIGTERM, helper
// must escalate to SIGKILL after grace expires. This is the regression
// test for the >50min hang observed in cold-target.
func TestWaitForCH_SIGTERM_EscalatesToSIGKILL(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}
	grace := 100 * time.Millisecond

	// Simulate stuck CH: doneCh never fires until we manually signal it.
	// SIGKILL handling: once helper sends SIGKILL we treat as "process
	// died" and unblock doneCh.
	var killSeen atomic.Bool
	go func() {
		for {
			if killSeen.Load() {
				doneCh <- &exitErrStub{code: -1}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	// Watch for SIGKILL appearance (via the goroutine-safe accessor).
	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			if proc.sentCount(syscall.SIGKILL) > 0 {
				killSeen.Store(true)
				return
			}
		}
	}()

	t0 := time.Now()
	sigCh <- syscall.SIGTERM
	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", 0, grace, discardLogf, nil)
	elapsed := time.Since(t0)
	if err == nil {
		t.Fatal("expected exit err stub")
	}

	if proc.sentCount(syscall.SIGTERM) != 1 {
		t.Fatalf("expected 1 SIGTERM, got %v", proc.sent)
	}
	if proc.sentCount(syscall.SIGKILL) != 1 {
		t.Fatalf("expected 1 SIGKILL escalation, got sent=%v", proc.sent)
	}
	// Must have waited at least the grace period before SIGKILL.
	if elapsed < grace {
		t.Fatalf("escalation fired too early: %v < %v", elapsed, grace)
	}
	// And not waited far longer (would indicate hang).
	if elapsed > grace+500*time.Millisecond {
		t.Fatalf("escalation took too long: %v (grace=%v)", elapsed, grace)
	}
}

// TestWaitForCH_DoubleSIGTERM_EscalatesImmediately: second SIGTERM
// during shutdown grace must skip the timer and SIGKILL right away.
func TestWaitForCH_DoubleSIGTERM_EscalatesImmediately(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 2)
	proc := &fakeSignaler{}
	grace := 10 * time.Second // long — would mask immediate escalation if buggy

	var killSeen atomic.Bool
	go func() {
		for !killSeen.Load() {
			time.Sleep(5 * time.Millisecond)
		}
		doneCh <- &exitErrStub{code: -1}
	}()
	go func() {
		for {
			time.Sleep(2 * time.Millisecond)
			if proc.sentCount(syscall.SIGKILL) > 0 {
				killSeen.Store(true)
				return
			}
		}
	}()

	t0 := time.Now()
	sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond) // first SIGTERM arms timer
	sigCh <- syscall.SIGINT           // second signal escalates
	_ = waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", 0, grace, discardLogf, nil)
	elapsed := time.Since(t0)

	if proc.sentCount(syscall.SIGKILL) != 1 {
		t.Fatalf("expected 1 SIGKILL via immediate-escalate path, got sent=%v", proc.sent)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("immediate escalation took too long: %v", elapsed)
	}
}

// exitErrStub matches the *exec.ExitError shape just enough for callers
// that check via type assertion; ours doesn't, but we use it to be
// explicit about "process exited unsuccessfully".
type exitErrStub struct{ code int }

func (e *exitErrStub) Error() string { return fmt.Sprintf("exit %d", e.code) }

func init() {
	// Silence "imported and not used" if the file is included in builds
	// where errors is not referenced elsewhere.
	_ = errors.New
}
