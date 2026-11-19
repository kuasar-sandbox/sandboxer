package ctl

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCaptureStateWaitCancellationAndStop(t *testing.T) {
	for _, stopServer := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel-client", true: "stop-server"}[stopServer], func(t *testing.T) {
			entered, exited := make(chan struct{}), make(chan struct{})
			s := &Server{Path: filepath.Join(t.TempDir(), "ctl.sock"), CaptureStateHandler: func(ctx context.Context, q CaptureStateQuery) (CaptureState, error) {
				close(entered)
				<-ctx.Done()
				close(exited)
				return CaptureState{}, ctx.Err()
			}}
			if err := s.Listen(); err != nil {
				t.Fatal(err)
			}
			life, stop := context.WithCancel(context.Background())
			defer stop()
			defer s.Stop()
			served := make(chan error, 1)
			go func() { served <- s.Serve(life) }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := ReadCaptureState(ctx, s.Path, CaptureStateQuery{SandboxID: "sid"}); result <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("wait did not start")
			}
			if stopServer {
				s.Stop()
			} else {
				cancel()
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("cancelled wait leaked")
			}
			select {
			case err := <-result:
				if err == nil || !stopServer && !errors.Is(err, context.Canceled) {
					t.Fatalf("result=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("client leaked")
			}
			s.Stop()
			select {
			case err := <-served:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Serve blocked on connsWG")
			}
		})
	}
}

func TestFrozenResponseDetachesBeforeRuntimeCleanup(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	s := &Server{Path: filepath.Join(t.TempDir(), "ctl.sock")}
	s.SnapshotHandler = func(Request) (Response, error) {
		return Response{SnapshotRef: "fixed", BeforeWrite: func() error {
			cancel()
			select {
			case err := <-served:
				return err
			case <-time.After(time.Second):
				return errors.New("capture connection blocked runtime cleanup")
			}
		}}, nil
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	go func() { served <- s.Serve(life) }()
	c, err := net.Dial("unix", s.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if err = WriteMessage(c, Request{Type: TypeSnapshotRequest}); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err = ReadMessage(c, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != TypeSnapshotDone || resp.SnapshotRef != "fixed" {
		t.Fatalf("response before retirement: %+v", resp)
	}
}

func TestCaptureStateNoWaitAndIdentity(t *testing.T) {
	s := &Server{Path: filepath.Join(t.TempDir(), "ctl.sock"), CaptureStateHandler: func(_ context.Context, q CaptureStateQuery) (CaptureState, error) {
		if !q.NoWaitForPreviousMemorySave {
			t.Error("lost NoWait")
		}
		return CaptureState{SandboxID: q.SandboxID, InstanceID: q.InstanceID, RunID: q.RunID, Pending: true, MemoryID: "memory", Epoch: 1}, nil
	}}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()
	go s.Serve(ctx)
	state, err := ReadCaptureState(ctx, s.Path, CaptureStateQuery{SandboxID: "sid", InstanceID: "instance", RunID: "run", NoWaitForPreviousMemorySave: true})
	if err != nil || !state.Pending || state.MemoryID != "memory" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestSnapshotDisconnectCancelsOnlyAdmission(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	var captures atomic.Int64
	s := &Server{Path: filepath.Join(t.TempDir(), "ctl.sock"),
		SnapshotAdmissionHandler: func(ctx context.Context, _ Request) error {
			close(entered)
			<-ctx.Done()
			close(exited)
			return ctx.Err()
		},
		SnapshotHandler: func(Request) (Response, error) {
			captures.Add(1)
			return Response{}, nil
		},
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	c, err := net.Dial("unix", s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = WriteMessage(c, Request{Type: TypeCheckpointSnapshotRequest, CheckpointID: "next"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("admission did not start")
	}
	_ = c.Close()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("disconnected admission continued waiting")
	}
	s.Stop()
	select {
	case err = <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server retained disconnected admission")
	}
	if captures.Load() != 0 {
		t.Fatal("cancelled admission started capture")
	}
}
