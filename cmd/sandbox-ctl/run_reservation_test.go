package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestRunReleasesPreparedReservationOnFlagParseFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan *resource.Message, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
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
	t.Setenv("KUASAR_RESOURCE_RESERVATION_TOKEN", "reservation-token")
	t.Setenv("KUASAR_RESOURCE_CONTROLLER_SOCKET", path)

	if code := runCmd([]string{"--not-a-real-flag"}); code != 2 {
		t.Fatalf("runCmd code = %d, want 2", code)
	}
	req := <-received
	if req.Type != resource.TypeRelease || req.Token != "reservation-token" || req.Reason != "run_setup_failed" {
		t.Fatalf("request = %+v, want entry-owned prepared-token release", req)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if os.Getenv("KUASAR_RESOURCE_RESERVATION_TOKEN") != "" || os.Getenv("KUASAR_RESOURCE_CONTROLLER_SOCKET") != "" {
		t.Fatal("prepared reservation handoff leaked into child environment")
	}
}
