package resource

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestConnectFailureClearsClosedConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{SocketPath: path}
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(); err == nil {
		t.Fatal("Connect unexpectedly succeeded after listener closed")
	}
	if c.Connected() {
		t.Fatal("failed Connect retained the closed previous connection")
	}
}

func TestAdmitContextCancelsInFlightRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := ReadMessage(conn)
		if err != nil || req.Type != TypeAdmit {
			return
		}
		close(received)
		// Deliberately never reply. The cancelled client must interrupt its
		// read and close the stream instead of waiting DeadlineAdmit.
		_, _ = ReadMessage(conn)
	}()

	client := &Client{SocketPath: path}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.ConnectContext(ctx); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := client.AdmitContext(ctx, AdmitParams{SandboxID: "cancel-admit"})
		result <- err
	}()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("controller did not receive Admit")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("AdmitContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Admit waited for its protocol deadline")
	}
	if client.Connected() {
		t.Fatal("cancelled Admit retained its unusable connection")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled Admit did not close the server stream")
	}
}
