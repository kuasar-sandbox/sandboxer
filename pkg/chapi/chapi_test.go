package chapi

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWaitReadyReadySocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not available")
	}
	sock := filepath.Join(t.TempDir(), "ch.sock")
	done, ready := listenUnixOnce(t, sock, 0)
	if err := <-ready; err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}

	if err := WaitReady(context.Background(), sock, time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	waitDone(t, done)
}

func TestWaitReadySocketAppearsWithinShortDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not available")
	}
	sock := filepath.Join(t.TempDir(), "ch.sock")
	done, ready := listenUnixOnce(t, sock, 5*time.Millisecond)

	if err := WaitReady(context.Background(), sock, 45*time.Millisecond); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if err := <-ready; err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	waitDone(t, done)
}

func TestWaitReadyContextCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	sock := filepath.Join(t.TempDir(), "missing.sock")
	go func() {
		errCh <- WaitReady(ctx, sock, 0)
	}()

	time.Sleep(5 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitReady error = %v, want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("WaitReady did not return after context cancellation")
	}
}

func listenUnixOnce(t *testing.T, sock string, delay time.Duration) (<-chan struct{}, <-chan error) {
	t.Helper()
	done := make(chan struct{})
	ready := make(chan error, 1)
	go func() {
		defer close(done)
		time.Sleep(delay)
		ln, err := net.Listen("unix", sock)
		ready <- err
		if err != nil {
			return
		}
		defer ln.Close()
		if unixLn, ok := ln.(*net.UnixListener); ok {
			_ = unixLn.SetDeadline(time.Now().Add(time.Second))
		}
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()
	return done, ready
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("listener did not accept WaitReady connection")
	}
}
