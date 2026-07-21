package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestRunRequiresControllerSocketWithPreparedReservation(t *testing.T) {
	t.Setenv("KUASAR_RESOURCE_RESERVATION_TOKEN", "reservation-token")
	t.Setenv("KUASAR_RESOURCE_CONTROLLER_SOCKET", "")
	var stderr bytes.Buffer
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = oldStderr })

	code := runCmdContext(context.Background(), nil)
	_ = w.Close()
	_, _ = stderr.ReadFrom(r)
	_ = r.Close()
	if code != 1 {
		t.Fatalf("runCmdContext code = %d, want 1", code)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("KUASAR_RESOURCE_CONTROLLER_SOCKET is required")) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if os.Getenv("KUASAR_RESOURCE_RESERVATION_TOKEN") != "" {
		t.Fatal("prepared reservation token leaked into child environment")
	}
}

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

func TestRunReleasesPreparedReservationWhenSetupContextIsCanceled(t *testing.T) {
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if code := runCmdContext(ctx, nil); code != 1 {
		t.Fatalf("runCmdContext code = %d, want 1", code)
	}
	req := <-received
	if req.Type != resource.TypeRelease || req.Token != "reservation-token" || req.Reason != "run_setup_failed" {
		t.Fatalf("request = %+v, want canceled setup release", req)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}
