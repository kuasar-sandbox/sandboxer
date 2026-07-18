package resctl

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

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
