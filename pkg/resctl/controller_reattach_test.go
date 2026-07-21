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
	}, preparedReservationTestConfig(path, "3GiB"))
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
	}, preparedReservationTestConfig(path, "3GiB"))
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
			NewAllocatable: 3 << 30,
		})
	}()

	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath:       path,
		ReservationToken: "reservation-token",
	}, preparedReservationTestConfig(path, "3GiB"))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.client.Close()

	granted, err := hooks.Admit("sandbox-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if granted != 3<<30 {
		t.Fatalf("granted = %d, want %d", granted, uint64(3<<30))
	}
	req := <-received
	if req.Type != resource.TypeReattach || req.Token != "reservation-token" {
		t.Fatalf("request = %+v, want reattach with preassigned token", req)
	}
	if req.SandboxID != "sandbox-1" {
		t.Fatalf("reattach sandbox identity = %q, want sandbox-1", req.SandboxID)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestReattachRejectsGrantBelowStartupBudget(t *testing.T) {
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
		req, err := resource.ReadMessage(conn)
		if err != nil {
			serverErr <- err
			return
		}
		serverErr <- resource.WriteMessage(conn, &resource.Message{
			Type: resource.TypeAck, Token: req.Token, NewAllocatable: 2 << 30,
		})
	}()

	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: path, ReservationToken: "reservation-token",
	}, preparedReservationTestConfig(path, "3GiB"))
	if err != nil {
		t.Fatal(err)
	}
	defer hooks.client.Close()
	if _, err := hooks.Admit("sandbox-1", 0); err == nil {
		t.Fatal("reattach accepted a grant below the startup budget")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestReleaseReconnectsAfterReattachConnectionFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	released := make(chan *resource.Message, 1)
	serverErr := make(chan error, 1)
	go func() {
		first, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		if _, err := resource.ReadMessage(first); err != nil {
			_ = first.Close()
			serverErr <- err
			return
		}
		_ = first.Close()

		second, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer second.Close()
		req, err := resource.ReadMessage(second)
		if err != nil {
			serverErr <- err
			return
		}
		released <- req
		serverErr <- resource.WriteMessage(second, &resource.Message{Type: resource.TypeAck})
	}()

	hooks, err := NewControllerHooks(ControllerHookOptions{
		SocketPath: path, ReservationToken: "reservation-token",
	}, preparedReservationTestConfig(path, "3GiB"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hooks.Admit("sandbox-1", 0); err == nil {
		t.Fatal("reattach unexpectedly survived a broken controller connection")
	}
	hooks.Release("reattach_failed")
	req := <-released
	if req.Type != resource.TypeRelease || req.Token != "reservation-token" || req.Reason != "reattach_failed" {
		t.Fatalf("fallback release = %+v", req)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func preparedReservationTestConfig(path, startup string) *config.SandboxConfig {
	cfg := makeMinimalCfg()
	cfg.Resources.Control.Controller = path
	cfg.Resources.Startup = &config.StartupConfig{Memory: startup}
	return cfg
}
