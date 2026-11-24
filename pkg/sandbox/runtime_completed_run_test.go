package sandbox

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
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

// Reproduce the exact publication ordering without scheduling luck: the
// guest has read the final launch ACK, but the host's MUX callback has not
// returned. A terminal notification (or Pong) must be monotonic and must
// neither fail nor publish readiness before the actual handoff.
func TestStandaloneCompletionAtMUXPublication(t *testing.T) {
	for _, scenario := range []string{"success", "cancel", "contradictory"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &Runtime{state: runtimeLaunching, lifeCtx: context.Background(), launchContext: ctx,
				initialLaunchToken: "current", initialLaunchChanged: make(chan struct{}, 1), launchDone: make(chan struct{}),
				initialLaunchState: proto.InitialLaunchState{Token: "current", State: proto.InitialLaunchPending}}
			r.spec.acceptCompletedRun = true
			muxEntered, releaseMUX, observed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 2)
			var once sync.Once
			release := func() { once.Do(func() { close(releaseMUX) }) }
			srv := &guestlink.LaunchServer{
				Path: filepath.Join(t.TempDir(), "launch.sock"), DeferLaunch: true, GateCompletedExit: true,
				Spec: &proto.LaunchSpec{Exec: "/bin/true", InitialLaunchToken: "current"},
				OnMUXReady: func(c net.Conn, _ proto.StdioSpec) {
					defer c.Close()
					close(muxEntered)
					<-releaseMUX
				},
				OnInitialLaunchState: func(f proto.InitialLaunchState) {
					r.observeInitialLaunch(f)
					observed <- struct{}{}
				},
			}
			r.launch = srv
			if err := srv.Listen(); err != nil {
				t.Fatal(err)
			}
			serveCtx, stop := context.WithCancel(context.Background())
			served := make(chan struct{})
			go func() { _ = srv.Serve(serveCtx); close(served) }()
			defer func() { release(); stop(); srv.Stop(); <-served }()
			hello, err := net.Dial("unix", srv.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer hello.Close()
			_ = hello.SetDeadline(time.Now().Add(2 * time.Second))
			if err = proto.WriteMessage(hello, &proto.Message{Type: proto.TypeHello, Phase: "runtime_ready", InitialLaunchStateV1: true}); err != nil {
				t.Fatal(err)
			}
			if _, err = proto.ReadMessage(hello); err != nil {
				t.Fatal(err)
			}
			if err = proto.WriteMessage(hello, &proto.Message{Type: proto.TypeLaunchAck}); err != nil {
				t.Fatal(err)
			}
			if m, err := proto.ReadMessage(hello); err != nil || m.Type != proto.TypeAck {
				t.Fatalf("launch ack: %+v %v", m, err)
			}
			select {
			case <-muxEntered:
			case <-time.After(time.Second):
				t.Fatal("MUX callback not entered")
			}

			terminal := proto.InitialLaunchState{Token: "current", State: proto.InitialLaunchExited, PID: 42}
			// A verified Pong may also arrive at this boundary. It must keep
			// the terminal fact until the host publishes MUX readiness.
			r.observeInitialLaunch(terminal)
			exitConn, err := net.Dial("unix", srv.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer exitConn.Close()
			_ = exitConn.SetDeadline(time.Now().Add(2 * time.Second))
			if err = proto.WriteMessage(exitConn, &proto.Message{Type: proto.TypeAppExited, InitialLaunchState: &terminal}); err != nil {
				t.Fatal(err)
			}
			type reply struct {
				msg *proto.Message
				err error
			}
			replied := make(chan reply, 1)
			go func() { m, err := proto.ReadMessage(exitConn); replied <- reply{m, err} }()
			select {
			case <-observed:
			case <-time.After(time.Second):
				t.Fatal("terminal fact not observed")
			}
			r.mu.Lock()
			complete, failure, fact := r.commitCompletedRunLocked(), r.launchFailure, r.initialLaunchState
			r.mu.Unlock()
			if complete || failure != nil || fact != terminal {
				t.Fatalf("premature outcome: complete=%t failure=%v fact=%+v", complete, failure, fact)
			}
			r.observeInitialLaunch(proto.InitialLaunchState{Token: "current", State: proto.InitialLaunchStarted, PID: 42})
			if r.commitLaunch() {
				t.Fatal("late started fact erased terminal fence")
			}
			select {
			case got := <-replied:
				t.Fatalf("terminal ACK escaped MUX gate: %+v", got)
			case <-time.After(20 * time.Millisecond):
			}
			if scenario == "cancel" {
				cancel()
			}
			if scenario == "contradictory" {
				r.observeInitialLaunch(proto.InitialLaunchState{Token: "current", State: proto.InitialLaunchStarted, PID: 43})
			}
			release()
			select {
			case got := <-replied:
				if got.err != nil || got.msg.Type != proto.TypeAck {
					t.Fatalf("terminal response: %+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("terminal ACK did not follow MUX publication")
			}
			r.mu.Lock()
			complete, failure = r.completedRun, r.launchFailure
			state := r.state
			r.mu.Unlock()
			if complete != (scenario == "success") {
				t.Fatalf("completed=%t failure=%v state=%v", complete, failure, state)
			}
			if scenario == "success" && (failure != nil || state != runtimeClosing) {
				t.Fatalf("invalid completed outcome: %v %v", failure, state)
			}
			if r.commitLaunch() {
				t.Fatal("terminal command published running state")
			}
		})
	}
}
