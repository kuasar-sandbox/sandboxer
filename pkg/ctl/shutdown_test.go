package ctl

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func shutdownTestPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ctl-stop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "ctl.sock")
}

func startShutdownTestServer(t *testing.T, s *Server) {
	t.Helper()
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		s.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("Server cleanup blocked")
		}
	})
}

func TestShutdownDetachesBeforeFastOwnerCleanup(t *testing.T) {
	target := ShutdownTarget{SandboxID: "sandbox", RunID: "run"}
	deadline, err := MonotonicDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Path: shutdownTestPath(t)}
	s.ShutdownHandler = func(req ShutdownRequest) (ShutdownOperation, error) {
		if req.Target != target {
			return ShutdownOperation{}, ErrShutdownTargetMismatch
		}
		return ShutdownOperation{Begin: func() (int64, error) {
			s.Stop()         // would close this response unless detachment happened first
			s.connsWG.Wait() // would deadlock if the response retained cleanup ownership
			return deadline, nil
		}, Wait: func() error { return nil }}, nil
	}
	startShutdownTestServer(t, s)
	if err := Shutdown(context.Background(), s.Path, target); err != nil {
		t.Fatalf("lost final response during fast retirement: %v", err)
	}
	select {
	case <-s.ResponsesDone():
	case <-time.After(time.Second):
		t.Fatal("response write not joined")
	}
}

func TestShutdownClientUsesOwnerDeadlineWithoutCancelingOwner(t *testing.T) {
	target := ShutdownTarget{SandboxID: "sandbox", RunID: "run"}
	release := make(chan struct{})
	defer close(release)
	var began, waited atomic.Bool
	s := &Server{Path: shutdownTestPath(t), ShutdownHandler: func(req ShutdownRequest) (ShutdownOperation, error) {
		return ShutdownOperation{Begin: func() (int64, error) {
			began.Store(true)
			return MonotonicDeadline(time.Now().Add(50 * time.Millisecond))
		}, Wait: func() error { <-release; waited.Store(true); return nil }}, nil
	}}
	startShutdownTestServer(t, s)
	start := time.Now()
	err := Shutdown(context.Background(), s.Path, target)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > time.Second || !began.Load() || waited.Load() {
		t.Fatal("owner deadline or independent wait was lost")
	}
}

func TestShutdownClientBoundsSilentOwner(t *testing.T) {
	socket := shutdownTestPath(t)
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	release := make(chan struct{})
	defer close(release)
	go func() {
		conn, e := l.Accept()
		if e == nil {
			defer conn.Close()
			<-release
		}
	}()
	start := time.Now()
	err = Shutdown(context.Background(), socket, ShutdownTarget{SandboxID: "s", RunID: "r"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("silent owner err=%v", err)
	}
	elapsed := time.Since(start)
	if elapsed < shutdownAcceptTimeout-100*time.Millisecond || elapsed > shutdownAcceptTimeout+time.Second {
		t.Fatalf("accept deadline=%s", elapsed)
	}
}

func TestShutdownClientRejectsUnsupportedOwnerAndWrongIdentity(t *testing.T) {
	target := ShutdownTarget{SandboxID: "sandbox", RunID: "run"}
	for _, tc := range []struct {
		name     string
		response Response
		want     error
	}{
		{"old-owner", Response{Type: TypeError, Msg: "unknown type: " + TypeShutdownRequest}, ErrShutdownUnsupported},
		{"target-mismatch", Response{Type: TypeError, Code: ShutdownTargetMismatchCode, Msg: "stale run"}, ErrShutdownTargetMismatch},
		{"no-accepted", Response{Type: TypeShutdownDone, Shutdown: &target}, nil},
		{"wrong-run", Response{Type: TypeShutdownAccepted, Shutdown: &ShutdownTarget{SandboxID: "sandbox", RunID: "other"}, ShutdownDeadlineMonotonicNS: 123}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := shutdownTestPath(t)
			l, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			go func() {
				conn, e := l.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				var req Request
				if ReadMessage(conn, &req) == nil {
					_ = WriteMessage(conn, tc.response)
				}
			}()
			err = Shutdown(context.Background(), socket, target)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
		})
	}
}

func TestShutdownDeadlineAndTargetValidation(t *testing.T) {
	target := ShutdownTarget{SandboxID: "sandbox", RunID: "run"}
	for _, bad := range []ShutdownTarget{{SandboxID: "s"}, {RunID: "r"}, {SandboxID: "../s", RunID: "r"}, {SandboxID: "s", RunID: "."}} {
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	clock, err := monotonicNow()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (ShutdownRequest{Target: target, DeadlineMonotonicNS: clock - 1}).Deadline(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired accepted: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	raw, err := MonotonicDeadline(deadline)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := (ShutdownRequest{Target: target, DeadlineMonotonicNS: raw}).Deadline()
	if err != nil {
		t.Fatal(err)
	}
	if restored.After(deadline) || deadline.Sub(restored) > 50*time.Millisecond {
		t.Fatalf("deadline changed: %v -> %v", deadline, restored)
	}
}
