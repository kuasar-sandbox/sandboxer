package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The ACK precedes the guest's process/cgroup/namespace bootstrap. A valid
// app_started event may arrive later than one launch-port message deadline.
func TestSDKAppStartWaitIsNotSocketDeadline(t *testing.T) {
	for _, socketDeadline := range []string{"", "200ms"} {
		t.Run("app_notify="+socketDeadline, func(t *testing.T) {
			t.Setenv("SDK_TEST_APP_STARTED_DELAY", "600ms")
			shape, launch := sdkFixture(t)
			shape.Timeouts.AppNotify = socketDeadline
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := StartRuntime(ctx, shape)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if err := r.Launch(ctx, launch); err != nil {
				t.Fatalf("valid post-ACK process bootstrap rejected: %v", err)
			}
			if r.State() != "running" {
				t.Fatalf("state=%s", r.State())
			}
		})
	}
}

func TestSDKAppStartDeadlineAndCancellationCloseRuntime(t *testing.T) {
	for _, cancelOperation := range []bool{false, true} {
		name := "startup_deadline"
		if cancelOperation {
			name = "operation_deadline"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("SDK_TEST_APP_STARTED_DELAY", "10s")
			shape, launch := sdkFixture(t)
			shape.Timeouts.AppNotify = "1h"
			shape.Timeouts.AppStart = "150ms"
			if cancelOperation {
				shape.Timeouts.AppStart = "5s"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := StartRuntime(ctx, shape)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if cancelOperation {
				// The operation must win even when the startup budget is longer.
				var cancelLaunch context.CancelFunc
				ctx, cancelLaunch = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancelLaunch()
			}
			started := time.Now()
			err = r.Launch(ctx, launch)
			if cancelOperation {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cancellation: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "app_started notification timed out after 150ms") || ctx.Err() != nil {
				t.Fatalf("startup budget: %v, context=%v", err, ctx.Err())
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("failed launch cleanup exceeded bound")
			}
			if r.State() != "closed" {
				t.Fatalf("state=%s", r.State())
			}
			select {
			case <-r.exitDone:
			default:
				t.Fatal("failed launch retained the child process")
			}
		})
	}
}

func TestSDKAppStartDoesNotTimeoutCommittedReadinessCallback(t *testing.T) {
	for _, startupBudget := range []string{"", "100ms"} {
		t.Run("app_start="+startupBudget, func(t *testing.T) {
			shape, launch := sdkFixture(t)
			shape.Timeouts.AppNotify = "5s"
			shape.Timeouts.AppStart = startupBudget
			entered, release := make(chan struct{}), make(chan struct{})
			shape.NotifyReadiness = func(event ReadinessEvent) {
				if event == ReadinessReady {
					close(entered)
					<-release
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			r, err := StartRuntime(ctx, shape)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			// Release the callback before joining runtime cleanup, including on
			// a failed assertion. Launch must succeed while it is still blocked.
			defer close(release)
			result := make(chan error, 1)
			go func() { result <- r.Launch(ctx, launch) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("readiness callback was not entered")
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("committed launch rejected for a blocked callback: %v", err)
				}
			case <-time.After(4 * time.Second):
				t.Fatalf("committed launch waited for callback: state=%s", r.State())
			}
			if r.State() != "running" {
				t.Fatalf("committed launch state=%s", r.State())
			}
		})
	}
}
