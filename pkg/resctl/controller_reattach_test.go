package resctl

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestReleasePreparedReservationRetriesControllerDial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.sock")
	received := make(chan *resource.Message, 1)
	serverErr := make(chan error, 1)
	go func() {
		time.Sleep(25 * time.Millisecond)
		ln, err := net.Listen("unix", path)
		if err != nil {
			serverErr <- err
			return
		}
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		req, err := resource.ReadMessage(conn)
		if err != nil {
			serverErr <- err
			return
		}
		received <- req
		serverErr <- resource.WriteMessage(conn, &resource.Message{Type: resource.TypeAck})
	}()

	if err := ReleasePreparedReservation(path, "reservation-token", "setup_failed"); err != nil {
		t.Fatal(err)
	}
	req := <-received
	if req.Type != resource.TypeRelease || req.Token != "reservation-token" || req.Reason != "setup_failed" {
		t.Fatalf("request = %+v, want retried prepared-token release", req)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestPreparedReservationRequiresController(t *testing.T) {
	_, err := NewControllerHooks(ControllerHookOptions{ReservationToken: "reservation-token"}, &config.SandboxConfig{})
	if err == nil {
		t.Fatal("prepared reservation was accepted without a resource controller")
	}
}

func TestPreparedReservationIsReleasedBeforeReattach(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	received := make(chan *resource.Message, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		req, err := resource.ReadMessage(conn)
		if err != nil {
			serverErr <- err
			return
		}
		received <- req
		serverErr <- resource.WriteMessage(conn, &resource.Message{Type: resource.TypeAck})
	}()

	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: path, ReservationToken: "reservation-token",
	}, &config.SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	hooks.Release("setup_failed")
	req := <-received
	if req.Type != resource.TypeRelease || req.Token != "reservation-token" || req.Reason != "setup_failed" {
		t.Fatalf("request = %+v, want prepared-token release", req)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestReattachRejectsMissingAllocatableGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		if _, err := resource.ReadMessage(conn); err != nil {
			serverErr <- err
			return
		}
		serverErr <- resource.WriteMessage(conn, &resource.Message{Type: resource.TypeAck})
	}()

	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: path, ReservationToken: "reservation-token",
	}, &config.SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.client.Close()
	if _, err := hooks.Admit("sandbox-1", 0); err == nil {
		t.Fatalf("missing grant error = %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestAdmitReattachesPreassignedReservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	received := make(chan *resource.Message, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		req, err := resource.ReadMessage(conn)
		if err != nil {
			serverErr <- err
			return
		}
		received <- req
		serverErr <- resource.WriteMessage(conn, &resource.Message{
			Type:           resource.TypeAck,
			Token:          req.Token,
			NewAllocatable: 512 << 20,
		})
	}()

	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath:       path,
		ReservationToken: "reservation-token",
	}, &config.SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.client.Close()

	granted, err := hooks.Admit("sandbox-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if granted != 512<<20 {
		t.Fatalf("granted = %d, want %d", granted, uint64(512<<20))
	}
	req := <-received
	if req.Type != resource.TypeReattach || req.Token != "reservation-token" {
		t.Fatalf("request = %+v, want reattach with preassigned token", req)
	}
	if req.SandboxID != "" {
		t.Fatalf("reattach unexpectedly submitted sandbox admission: %+v", req)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}
