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
