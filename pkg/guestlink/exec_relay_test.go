package guestlink

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestPipeConnsKeepsReverseDirectionAfterHalfClose(t *testing.T) {
	client, relayClient := unixConnPair(t, "client")
	defer client.Close()
	defer relayClient.Close()
	relayGuest, guest := unixConnPair(t, "guest")
	defer relayGuest.Close()
	defer guest.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		pipeConns(ctx, relayClient, relayGuest)
		close(done)
	}()

	if err := client.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite: %v", err)
	}
	if _, err := guest.Write([]byte("exit-status")); err != nil {
		t.Fatalf("guest write after client half-close: %v", err)
	}
	if err := guest.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatalf("guest CloseWrite: %v", err)
	}
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("client SetReadDeadline: %v", err)
	}
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(got) != "exit-status" {
		t.Fatalf("client got %q, want exit-status", got)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeConns did not return after both half-closes")
	}
}

func TestPipeConnsFullLocalCloseForcesGuestConnectionClosed(t *testing.T) {
	client, relayClient := unixConnPair(t, "client-full-close")
	defer client.Close()
	relayGuest, guest := unixConnPair(t, "guest-full-close")
	defer guest.Close()

	done := make(chan struct{})
	go func() {
		pipeConns(context.Background(), relayClient, relayGuest)
		close(done)
	}()

	// Abort/shutdown closes the admitted ctl-side connection itself. That is
	// distinct from a CLI CloseWrite: the guest reverse session must be fully
	// closed so its silent read side cannot keep the exec handler in the drain.
	if err := relayClient.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		_ = guest.Close()
		<-done
		t.Fatal("pipeConns remained blocked on the guest after a full local close")
	}
}

func TestPipeConnsCaptureCancelAfterLocalHalfCloseClosesGuest(t *testing.T) {
	client, relayClient := unixConnPair(t, "client-half-then-capture")
	defer client.Close()
	relayGuest, guest := unixConnPair(t, "guest-half-then-capture")
	defer guest.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		pipeConns(ctx, relayClient, relayGuest)
		close(done)
	}()
	if err := client.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := guest.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := guest.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("guest read after local half-close = %d, %v; want EOF", n, err)
	}
	if err := guest.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	// Abort/shutdown first cancels the admitted exec context, then closes its ctl
	// connection. The explicit cancellation must close the guest side even though
	// the ctl-to-guest copy already returned cleanly on EOF.
	cancel()
	_ = relayClient.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = guest.Close()
		<-done
		t.Fatal("pipeConns remained blocked after capture cancellation")
	}
}

func unixConnPair(t *testing.T, name string) (net.Conn, net.Conn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()

	acceptCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		acceptCh <- c
	}()

	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	select {
	case server := <-acceptCh:
		return client, server
	case err := <-errCh:
		client.Close()
		t.Fatalf("accept unix: %v", err)
	case <-time.After(2 * time.Second):
		client.Close()
		t.Fatal("accept unix timed out")
	}
	panic("unreachable")
}
