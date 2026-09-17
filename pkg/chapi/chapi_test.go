package chapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const notCreatedBody = `["Error from API","The VM info is not available","VM is not created"]`

func TestWaitReadyWaitsForPausedBeforeResume(t *testing.T) {
	sock := unixSocket(t)
	var paused atomic.Bool
	var infoCalls, resumeCalls atomic.Int32
	stop := serveUnixHTTP(t, sock, func(conn net.Conn, req *http.Request) {
		switch req.URL.Path {
		case "/api/v1/vm.info":
			infoCalls.Add(1)
			if !paused.Load() {
				writeResponse(conn, 500, notCreatedBody)
				return
			}
			// Deliberately split the framed response across writes.
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 18\r\nConnection: close\r\n\r\n{\"state\":")
			time.Sleep(time.Millisecond)
			_, _ = io.WriteString(conn, `"Paused"}`)
		case "/api/v1/vm.resume":
			resumeCalls.Add(1)
			writeResponse(conn, 204, "")
		default:
			writeResponse(conn, 404, "")
		}
	})
	defer stop()

	ready := make(chan error, 1)
	go func() { ready <- WaitReady(context.Background(), sock, time.Second) }()
	waitFor(t, func() bool { return infoCalls.Load() > 0 })
	select {
	case err := <-ready:
		t.Fatalf("WaitReady returned before restore: %v", err)
	default:
	}
	if got := resumeCalls.Load(); got != 0 {
		t.Fatalf("resume calls before Paused = %d", got)
	}

	paused.Store(true)
	if err := <-ready; err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if err := (Client{Sock: sock, RespDeadline: time.Second}).ResumeContext(context.Background()); err != nil {
		t.Fatalf("ResumeContext: %v", err)
	}
	if got := resumeCalls.Load(); got != 1 {
		t.Fatalf("resume calls = %d, want 1", got)
	}
}

func TestWaitReadyFailures(t *testing.T) {
	tests := []struct {
		name, response, want string
	}{
		{"malformed HTTP", "not http", "malformed HTTP status"},
		{"truncated body", "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n{}", "unexpected EOF"},
		{"malformed JSON", response(200, `{`), "vm.info JSON"},
		{"unexpected state", response(200, `{"state":"Running"}`), `unexpected state "Running"`},
		{"missing state", response(200, `{}`), `unexpected state ""`},
		{"unrelated server error", response(500, `["Internal Server Error"]`), "HTTP 500"},
		{"lookalike not-created", response(500, `["VM is not created"]`), "HTTP 500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sock := unixSocket(t)
			stop := serveUnixHTTP(t, sock, func(conn net.Conn, _ *http.Request) { _, _ = io.WriteString(conn, tt.response) })
			defer stop()
			err := WaitReady(context.Background(), sock, time.Second)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("WaitReady error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestWaitReadyDeadlineCoversBlockedResponse(t *testing.T) {
	sock := unixSocket(t)
	stop := serveUnixHTTP(t, sock, func(_ net.Conn, _ *http.Request) { time.Sleep(time.Second) })
	defer stop()
	start := time.Now()
	err := WaitReady(context.Background(), sock, 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("WaitReady error = %v, want deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("deadline took %v", elapsed)
	}
}

func TestWaitReadyCancelBlockedResponse(t *testing.T) {
	sock := unixSocket(t)
	requestRead := make(chan struct{})
	stop := serveUnixHTTP(t, sock, func(_ net.Conn, _ *http.Request) { close(requestRead); time.Sleep(time.Second) })
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- WaitReady(ctx, sock, 0) }()
	<-requestRead
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked response did not cancel")
	}
}

func TestWaitReadyZeroDeadlineCancelsMissingSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitReady(ctx, filepath.Join(t.TempDir(), "missing.sock"), 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
}

func TestResumeContextCancelsStalledResponse(t *testing.T) {
	sock := unixSocket(t)
	requestRead := make(chan struct{})
	stop := serveUnixHTTP(t, sock, func(_ net.Conn, _ *http.Request) { close(requestRead); time.Sleep(time.Second) })
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- (Client{Sock: sock}).ResumeContext(ctx) }()
	<-requestRead
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
}

func unixSocket(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not available")
	}
	return filepath.Join(t.TempDir(), "ch.sock")
}

func serveUnixHTTP(t *testing.T, sock string, handler func(net.Conn, *http.Request)) func() {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err == nil {
					handler(conn, req)
				}
			}()
		}
	}()
	return func() { _ = ln.Close(); <-done }
}

func response(status int, body string) string {
	return fmt.Sprintf("HTTP/1.1 %d Test\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, len(body), body)
}

func writeResponse(w io.Writer, status int, body string) {
	_, _ = io.WriteString(w, response(status, body))
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	end := time.Now().Add(time.Second)
	for !f() && time.Now().Before(end) {
		time.Sleep(time.Millisecond)
	}
	if !f() {
		t.Fatal("condition not reached")
	}
}
