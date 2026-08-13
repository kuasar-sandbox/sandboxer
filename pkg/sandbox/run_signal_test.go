package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRunSignalContextRetainsSignalBeforeServeAndWait(t *testing.T) {
	source := make(chan os.Signal, 2)
	var stopped atomic.Bool
	ctx, stop := newRunSignalContext(context.Background(), source, func() { stopped.Store(true) })
	t.Cleanup(stop)
	controllerCtx := ControllerWorkContext(ctx)
	vmCtx := VMLifecycleContext(ctx)

	// Model SIGTERM after Admit but before ServeAndWait obtains its shutdown
	// channel. Cancellation and later CH delivery must both survive that gap.
	source <- syscall.SIGTERM
	select {
	case <-controllerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("controller work context was not cancelled")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("pre-spawn context was not cancelled")
	}
	if err := vmCtx.Err(); err != nil {
		t.Fatalf("VM backend context cancelled before graceful CH shutdown: %v", err)
	}
	signals := runSignalsFromContext(ctx)
	select {
	case sig := <-signals:
		if sig != syscall.SIGTERM {
			t.Fatalf("retained signal = %v", sig)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-ServeAndWait signal was lost")
	}

	// Later signals remain distinct so waitForCHWithSignalEscalation preserves
	// its existing immediate escalation on the user's second signal.
	source <- syscall.SIGINT
	select {
	case sig := <-signals:
		if sig != syscall.SIGINT {
			t.Fatalf("second signal = %v", sig)
		}
	case <-time.After(time.Second):
		t.Fatal("second signal was lost")
	}
	stop()
	if !stopped.Load() {
		t.Fatal("signal source was not stopped")
	}
}

func TestServeAndWaitRejectsCancelledPreSpawnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	buildCalled := false
	_, err := ServeAndWait(VMParams{
		Ctx: ctx,
		BuildCmd: func(CmdEnv) (*exec.Cmd, func(), error) {
			buildCalled = true
			return nil, func() {}, nil
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ServeAndWait error = %v, want context.Canceled", err)
	}
	if buildCalled {
		t.Fatal("BuildCmd called after pre-spawn cancellation")
	}
}
