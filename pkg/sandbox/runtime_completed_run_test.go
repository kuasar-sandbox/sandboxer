package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestStandaloneCompletedRunFactFence(t *testing.T) {
	for _, name := range []string{"success", "public-sdk", "systemd", "no-started-pid", "different-pid", "nonzero", "error", "wrong-token", "cancelled", "life-cancelled", "prior-failure", "no-ack-mux", "closing"} {
		t.Run(name, func(t *testing.T) {
			r := initialRuntimeForTest(t)
			r.spec.acceptCompletedRun = true
			f := proto.InitialLaunchState{Token: "current", State: proto.InitialLaunchExited, PID: 42}
			switch name {
			case "public-sdk":
				r.spec.acceptCompletedRun = false
			case "systemd":
				r.environmentActive = true
			case "no-started-pid":
				f.PID = 0
			case "different-pid":
				r.initialLaunchState.PID = 43
			case "nonzero":
				f.Code = 7
			case "error":
				f.Error = "exec failed"
			case "wrong-token":
				f.Token = "stale"
			case "cancelled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r.launchContext = ctx
			case "life-cancelled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r.lifeCtx = ctx
			case "prior-failure":
				r.launchFailure = errors.New("prior failure")
			case "no-ack-mux":
				r.launch = &guestlink.LaunchServer{}
			case "closing":
				r.state = runtimeClosing
			}
			r.observeInitialLaunch(f)
			if r.completedRun != (name == "success") {
				t.Fatalf("completed=%t, failure=%v", r.completedRun, r.launchFailure)
			}
			if r.commitLaunch() {
				t.Fatal("terminal command published running state")
			}
			if name == "success" {
				if r.state != runtimeClosing || r.launchFailure != nil || r.initialLaunchState != f {
					t.Fatalf("invalid terminal state: %v / %v", r.state, r.launchFailure)
				}
				select {
				case <-r.launchDone:
				default:
					t.Fatal("terminal run not signaled")
				}
				r.observeInitialLaunch(proto.InitialLaunchState{Token: "current", State: proto.InitialLaunchStarted, PID: 42})
				if r.commitLaunch() {
					t.Fatal("late start published readiness")
				}
			}
		})
	}
}

func TestStandaloneCompletedRunJoinsRealChildWithoutReady(t *testing.T) {
	for _, tc := range []struct {
		name, outcome  string
		allow, success bool
		exitCode       int
	}{
		{"standalone", "success", true, true, 0},
		{"public-sdk", "success", false, false, 0},
		{"nonzero", "nonzero", true, false, 0},
		{"not-started", "not-started", true, false, 0},
		{"physical-failure", "vmm-error", true, true, 19},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SDK_TEST_SHORT_EXIT", tc.outcome)
			shape, launch := sdkFixture(t)
			shape.acceptCompletedRun = tc.allow
			ready := make(chan struct{}, 1)
			shape.NotifyReadiness = func(s ReadinessEvent) {
				if s == ReadinessReady {
					ready <- struct{}{}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
			if (err == nil) != tc.success {
				t.Fatalf("start=%v want success=%t", err, tc.success)
			}
			if r != nil {
				defer r.Close()
				got, err := r.Wait(ctx)
				if err != nil || got.Err != nil || got.Code != tc.exitCode {
					t.Fatalf("exit=%+v / %v", got, err)
				}
				if r.State() != "closed" || r.launchSucceeded || !r.completedRun {
					t.Fatal("not a completed standalone command")
				}
			}
			select {
			case <-ready:
				t.Fatal("reported readiness for an already completed workload")
			default:
			}
		})
	}
}

func TestStandaloneRunAdapterPreservesCompletionWithoutReady(t *testing.T) {
	for _, managed := range []bool{false, true} {
		name := "standalone"
		if managed {
			name = "managed"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("SDK_TEST_SHORT_EXIT", "success")
			shape, launch, fetcher := coldPreparationFixture(t)
			cfg, err := launch.config(shape.config())
			if err != nil {
				t.Fatal(err)
			}
			opts := RunOptions{Cfg: cfg, Fetcher: fetcher, SandboxID: shape.SandboxID,
				CHBinary: shape.CHBinary, RuntimeRoot: shape.RuntimeRoot, BaseRoot: shape.BaseRoot}
			if managed {
				opts.NotifyReadiness = func(e ReadinessEvent) {
					if e == ReadinessReady {
						t.Error("terminal Guest was reported ready")
					}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			code, err := Run(ctx, opts)
			if managed {
				if err == nil {
					t.Fatal("managed run accepted terminal before readiness")
				}
			} else if code != 0 || err != nil {
				t.Fatalf("standalone run: code=%d err=%v", code, err)
			}
		})
	}
}
