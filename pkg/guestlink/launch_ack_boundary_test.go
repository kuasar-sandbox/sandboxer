package guestlink

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// Deliver the ACK bytes to the peer, but pause the host writer before Write
// returns. A one-shot app_started can then reach admission at the exact
// publication boundary. A write error must still keep that barrier closed.
func TestLaunchServerAppStartedAtACKPublication(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		name := "successful-write"
		if failWrite {
			name = "failed-write"
		}
		t.Run(name, func(t *testing.T) {
			spec := &proto.LaunchSpec{Exec: "/bin/true", Restart: "never"}
			var started atomic.Int32
			srv := &LaunchServer{Spec: spec, DeferLaunch: true, Logf: t.Logf,
				stopped: make(chan struct{}), runtimeReady: make(chan struct{}),
				helloDone: make(chan struct{}), launchReady: make(chan struct{}),
				launchAckDone: make(chan struct{}), appStartedDone: make(chan struct{}), muxReady: make(chan struct{}),
				OnAppStarted: func(int) { started.Add(1) },
			}
			close(srv.launchReady)
			request := append(encodeLaunchMessage(t, &proto.Message{Type: proto.TypeHello, Phase: "runtime_ready", InitialLaunchStateV1: true}), encodeLaunchMessage(t, &proto.Message{Type: proto.TypeLaunchAck})...)
			expected := append(encodeLaunchMessage(t, &proto.Message{Type: proto.TypeLaunch, Launch: spec}), encodeLaunchMessage(t, &proto.Message{Type: proto.TypeAck})...)
			wireACK, releaseWrite, helloDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(releaseWrite) }) }
			defer release()
			hello := &scriptedLaunchConn{read: bytes.NewReader(request)}
			hello.write = func(p []byte) (int, error) {
				n, err := hello.written.Write(p)
				if bytes.Equal(hello.written.Bytes(), expected) {
					close(wireACK)
					<-releaseWrite
					if failWrite {
						return n, io.ErrClosedPipe
					}
				}
				return n, err
			}
			go func() { srv.handleConn(hello); close(helloDone) }()
			select {
			case <-wireACK:
			case <-time.After(time.Second):
				t.Fatal("host did not write ACK")
			}
			app := &scriptedLaunchConn{read: bytes.NewReader(encodeLaunchMessage(t, &proto.Message{Type: proto.TypeAppStarted, PID: 42}))}
			appDone := make(chan struct{})
			go func() { srv.handleConn(app); close(appDone) }()
			select {
			case <-appDone:
				t.Error("app_started admission escaped an in-flight ACK publication")
			case <-time.After(25 * time.Millisecond):
			}
			release()
			select {
			case <-helloDone:
			case <-time.After(time.Second):
				t.Fatal("hello did not finish")
			}
			select {
			case <-appDone:
			case <-time.After(time.Second):
				t.Fatal("app_started did not finish after ACK outcome")
			}
			response, err := proto.ReadMessage(bytes.NewReader(app.written.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			want, wantStarted := proto.TypeAck, int32(1)
			if failWrite {
				want, wantStarted = proto.TypeError, 0
			}
			if response.Type != want || started.Load() != wantStarted {
				t.Fatalf("app_started response=%s (%q), callbacks=%d; want %s/%d", response.Type, response.Msg, started.Load(), want, wantStarted)
			}
			select {
			case <-srv.launchAckDone:
				if failWrite {
					t.Fatal("failed ACK write opened launch barrier")
				}
			default:
				if !failWrite {
					t.Fatal("successful ACK write left launch barrier closed")
				}
			}
		})
	}
}

func TestLaunchServerAppStartedBeforeLaunchStillRejected(t *testing.T) {
	srv := &LaunchServer{DeferLaunch: true, Logf: t.Logf, launchAckDone: make(chan struct{})}
	conn := &scriptedLaunchConn{read: bytes.NewReader(encodeLaunchMessage(t, &proto.Message{Type: proto.TypeAppStarted, PID: 42}))}
	srv.handleConn(conn)
	response, err := proto.ReadMessage(bytes.NewReader(conn.written.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if response.Type != proto.TypeError || response.Msg != "workload has not launched" {
		t.Fatalf("early notification accepted: %+v", response)
	}
}

func TestLaunchServerExitFactPrecedesACKAndRejectsContradiction(t *testing.T) {
	for _, state := range []string{proto.InitialLaunchExited, proto.InitialLaunchFailed, proto.InitialLaunchStarted, proto.InitialLaunchPending, "unknown"} {
		t.Run(state, func(t *testing.T) {
			var observed *proto.InitialLaunchState
			srv := &LaunchServer{Logf: t.Logf, OnInitialLaunchState: func(s proto.InitialLaunchState) { observed = &s }}
			conn := &scriptedLaunchConn{read: bytes.NewReader(encodeLaunchMessage(t, &proto.Message{Type: proto.TypeAppExited,
				InitialLaunchState: &proto.InitialLaunchState{Token: "same-token", State: state, PID: 42}}))}
			conn.write = func(b []byte) (int, error) {
				if observed == nil {
					t.Error("exit ACK preceded terminal publication")
				}
				return conn.written.Write(b)
			}
			srv.handleConn(conn)
			want := state
			if state != proto.InitialLaunchExited && state != proto.InitialLaunchFailed {
				want = "invalid_app_exited"
			}
			if observed == nil || observed.Token != "same-token" || observed.State != want {
				t.Fatalf("fact=%+v want=%s", observed, want)
			}
		})
	}
}

// The ACK may be readable by the Guest before the host's Write returns.
// A matching standalone terminal report must not ACK or commit through
// that publication boundary, including when the original write fails.
func TestLaunchServerCompletedExitAtACKPublication(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		name := "successful-write"
		if failWrite {
			name = "failed-write"
		}
		t.Run(name, func(t *testing.T) {
			spec := &proto.LaunchSpec{Exec: "/bin/true", InitialLaunchToken: "same-token"}
			facts := make(chan proto.InitialLaunchState, 2)
			srv := &LaunchServer{Spec: spec, DeferLaunch: true, GateCompletedExit: true, Logf: t.Logf,
				stopped: make(chan struct{}), runtimeReady: make(chan struct{}),
				helloDone: make(chan struct{}), launchReady: make(chan struct{}),
				launchAckDone: make(chan struct{}), appStartedDone: make(chan struct{}), muxReady: make(chan struct{}),
				OnInitialLaunchState: func(f proto.InitialLaunchState) { facts <- f },
			}
			close(srv.launchReady)
			request := append(encodeLaunchMessage(t, &proto.Message{Type: proto.TypeHello, Phase: "runtime_ready", InitialLaunchStateV1: true}), encodeLaunchMessage(t, &proto.Message{Type: proto.TypeLaunchAck})...)
			expected := append(encodeLaunchMessage(t, &proto.Message{Type: proto.TypeLaunch, Launch: spec}), encodeLaunchMessage(t, &proto.Message{Type: proto.TypeAck})...)
			wireACK, releaseWrite, helloDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(releaseWrite) }) }
			defer release()
			hello := &scriptedLaunchConn{read: bytes.NewReader(request)}
			hello.write = func(p []byte) (int, error) {
				n, err := hello.written.Write(p)
				if bytes.Equal(hello.written.Bytes(), expected) {
					close(wireACK)
					<-releaseWrite
					if failWrite {
						return n, io.ErrClosedPipe
					}
				}
				return n, err
			}
			go func() { srv.handleConn(hello); close(helloDone) }()
			select {
			case <-wireACK:
			case <-time.After(time.Second):
				t.Fatal("host did not write ACK")
			}
			fact := proto.InitialLaunchState{Token: "same-token", State: proto.InitialLaunchExited, PID: 42}
			exit := &scriptedLaunchConn{read: bytes.NewReader(encodeLaunchMessage(t, &proto.Message{Type: proto.TypeAppExited, InitialLaunchState: &fact}))}
			exitDone := make(chan struct{})
			go func() { srv.handleConn(exit); close(exitDone) }()
			select {
			case got := <-facts:
				if got != fact {
					t.Fatalf("first fact=%+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("terminal fact not published")
			}
			select {
			case <-exitDone:
				t.Fatal("terminal reply escaped ACK barrier")
			case <-time.After(20 * time.Millisecond):
			}
			release()
			select {
			case <-helloDone:
			case <-time.After(time.Second):
				t.Fatal("hello did not finish")
			}
			select {
			case <-exitDone:
			case <-time.After(time.Second):
				t.Fatal("terminal request did not finish")
			}
			response, err := proto.ReadMessage(bytes.NewReader(exit.written.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			want, state := proto.TypeAck, proto.InitialLaunchExited
			if failWrite {
				want, state = proto.TypeError, "invalid_app_exited"
			}
			if response.Type != want {
				t.Fatalf("terminal reply=%s, want %s", response.Type, want)
			}
			select {
			case got := <-facts:
				if got.Token != fact.Token || got.State != state {
					t.Fatalf("second fact=%+v, want state=%s", got, state)
				}
			default:
				t.Fatal("terminal observation did not resolve")
			}
		})
	}
}
