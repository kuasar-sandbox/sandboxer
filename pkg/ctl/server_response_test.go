package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSnapshotAfterWriteRunsAfterFramedResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctl.sock")
	afterWriteStarted := make(chan struct{})
	releaseAfterWrite := make(chan struct{})
	server := &Server{
		Path: path,
		SnapshotHandler: func(Request) (Response, error) {
			return Response{
				MemorySize: 123,
				AfterWrite: func() {
					close(afterWriteStarted)
					<-releaseAfterWrite
				},
			}, nil
		},
	}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		close(releaseAfterWrite)
		cancel()
		server.Stop()
		<-done
	})

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := WriteMessage(conn, Request{Type: TypeSnapshotRequest}); err != nil {
		t.Fatal(err)
	}

	var response Response
	readDone := make(chan error, 1)
	go func() { readDone <- ReadMessage(conn, &response) }()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot response was not readable while post-write lifecycle continuation was blocked")
	}
	if response.Type != TypeSnapshotDone || response.MemorySize != 123 {
		t.Fatalf("snapshot response = %+v", response)
	}
	select {
	case <-afterWriteStarted:
	case <-time.After(time.Second):
		t.Fatal("post-write lifecycle continuation was not invoked")
	}
}

// Force the body write to remain in flight until the client consumes it.
// The continuation must not run after only the length prefix was delivered.
func TestCaptureContinuationWaitsForCompleteFrame(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	called := make(chan struct{})
	done := make(chan error, 1)
	response := Response{Type: TypeSnapshotDone, Msg: "complete capture", AfterWrite: func() { close(called) }}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	go func() { done <- writeCaptureResponse(server, response) }()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	prefix := make([]byte, 4)
	if _, err := io.ReadFull(client, prefix); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
		t.Fatal("destroy after prefix, before response body")
	default:
	}
	got := make([]byte, len(body))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) || bytes.Contains(got, []byte("AfterWrite")) {
		t.Fatalf("wire body=%s", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("response write did not finish")
	}
	select {
	case <-called:
	default:
		t.Fatal("destroy was not continued")
	}
}

type responseDeadlineConn struct {
	net.Conn
	deadline      time.Time
	expireNow     bool
	deadlineError error
}

func (c *responseDeadlineConn) SetWriteDeadline(d time.Time) error {
	c.deadline = d
	if c.deadlineError != nil {
		return c.deadlineError
	}
	if c.expireNow {
		return c.Conn.SetWriteDeadline(time.Now())
	}
	return c.Conn.SetWriteDeadline(d)
}

func TestCaptureContinuationRunsAfterWriteFailure(t *testing.T) {
	for _, failure := range []string{"disconnected", "unread", "oversized", "deadline-error"} {
		t.Run(failure, func(t *testing.T) {
			s, c := net.Pipe()
			defer s.Close()
			defer c.Close()
			conn := &responseDeadlineConn{Conn: s}
			response := Response{Type: TypeExportDone}
			switch failure {
			case "disconnected":
				_ = c.Close()
			case "unread":
				conn.expireNow = true
			case "oversized":
				response.Msg = strings.Repeat("x", MaxMessageBytes)
			case "deadline-error":
				conn.deadlineError = errors.New("deadline unavailable")
			}
			var calls atomic.Int32
			response.AfterWrite = func() { calls.Add(1) }
			before := time.Now()
			err := writeCaptureResponse(conn, response)
			if err == nil {
				t.Fatal("failed transport reported success")
			}
			if failure == "unread" && !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("unread error=%v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("continuation calls=%d", calls.Load())
			}
			if conn.deadline.Before(before.Add(captureResponseWriteTimeout)) || conn.deadline.After(time.Now().Add(captureResponseWriteTimeout)) {
				t.Fatalf("invalid response-only deadline %v", conn.deadline)
			}
		})
	}
}

func TestCaptureWithoutDestroyHasNoNewWriteDeadline(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	conn := &responseDeadlineConn{Conn: s, deadlineError: errors.New("must not set deadline")}
	done := make(chan error, 1)
	go func() { done <- writeCaptureResponse(conn, Response{Type: TypeSnapshotDone}) }()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	var got Response
	if err := ReadMessage(c, &got); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !conn.deadline.IsZero() {
		t.Fatal("resume response got a new deadline")
	}
}

func TestCaptureServerContinuesExportAfterDisconnect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctl.sock")
	entered, release, continued := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := func(Request) (Response, error) {
		close(entered)
		<-release
		return Response{AfterWrite: func() { close(continued) }}, nil
	}
	server := &Server{Path: path, SnapshotHandler: handler, ExportHandler: handler}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := WriteMessage(conn, Request{Type: TypeExportRequest}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("export handler not entered")
	}
	_ = conn.Close()
	close(release)
	select {
	case <-continued:
	case <-time.After(time.Second):
		t.Fatal("disconnected client retained completed export")
	}
	cancel()
	server.Stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCaptureServerPreservesErrorAndTerminalContinuation(t *testing.T) {
	for _, kind := range []string{TypeSnapshotRequest, TypeExportRequest} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ctl.sock")
			continued := make(chan struct{})
			handler := func(Request) (Response, error) {
				return Response{AfterWrite: func() { close(continued) }}, errors.New("post-commit cleanup failed")
			}
			server := &Server{Path: path, SnapshotHandler: handler, ExportHandler: handler}
			if err := server.Listen(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx) }()
			t.Cleanup(func() { cancel(); server.Stop(); <-done })
			conn, err := net.Dial("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if err := WriteMessage(conn, Request{Type: kind}); err != nil {
				t.Fatal(err)
			}
			var response Response
			if err := ReadMessage(conn, &response); err != nil {
				t.Fatal(err)
			}
			if response.Type != TypeError || response.Msg != "post-commit cleanup failed" {
				t.Fatalf("lost handler error: %+v", response)
			}
			select {
			case <-continued:
			case <-time.After(time.Second):
				t.Fatal("error response lost terminal teardown")
			}
		})
	}
}
