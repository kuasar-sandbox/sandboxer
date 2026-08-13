package resource

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func startResourceProtocolServer(t *testing.T, handler func(*Message) (*Message, error)) *Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "controller.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		for {
			req, err := ReadMessage(conn)
			if err != nil {
				done <- nil
				return
			}
			resp, err := handler(req)
			if err != nil {
				done <- err
				return
			}
			if err := WriteMessage(conn, resp); err != nil {
				done <- err
				return
			}
		}
	}()
	client := &Client{SocketPath: path}
	if err := client.Connect(); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = listener.Close()
		if err := <-done; err != nil {
			t.Errorf("resource protocol server: %v", err)
		}
	})
	return client
}

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

func TestAdminListAggregatesPaginatedResponses(t *testing.T) {
	var requests atomic.Int32
	client := startResourceProtocolServer(t, func(req *Message) (*Message, error) {
		requests.Add(1)
		if req.Type != TypeAdminList || req.ListLimit != DefaultAdminListPageSize {
			return nil, fmt.Errorf("admin_list request = %+v", req)
		}
		switch req.ListAfter {
		case "":
			return &Message{Type: TypeAck, Reservations: []ReservationView{
				{SandboxID: "a"}, {SandboxID: "b"},
			}, ListNext: "b"}, nil
		case "b":
			return &Message{Type: TypeAck, Reservations: []ReservationView{
				{SandboxID: "c"}, {SandboxID: "d"},
			}, ListNext: "d"}, nil
		case "d":
			return &Message{Type: TypeAck, Reservations: []ReservationView{{SandboxID: "e"}}}, nil
		default:
			return nil, fmt.Errorf("unexpected cursor %q", req.ListAfter)
		}
	})
	got, err := client.AdminList()
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 || len(got) != 5 {
		t.Fatalf("AdminList requests=%d reservations=%+v", requests.Load(), got)
	}
	for n, want := range []string{"a", "b", "c", "d", "e"} {
		if got[n].SandboxID != want {
			t.Fatalf("reservation %d SID = %q, want %q", n, got[n].SandboxID, want)
		}
	}
}

func TestAdminListAcceptsLegacyUnpaginatedResponse(t *testing.T) {
	var requests atomic.Int32
	client := startResourceProtocolServer(t, func(req *Message) (*Message, error) {
		requests.Add(1)
		return &Message{Type: TypeAck, Reservations: []ReservationView{
			{SandboxID: "b"}, {SandboxID: "a"},
		}}, nil
	})
	got, err := client.AdminList()
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || len(got) != 2 {
		t.Fatalf("legacy AdminList requests=%d reservations=%+v", requests.Load(), got)
	}
	if got[0].SandboxID != "b" || got[1].SandboxID != "a" {
		t.Fatalf("legacy AdminList reordered reservations: %+v", got)
	}
}

func TestAdminListRejectsNonAdvancingPagination(t *testing.T) {
	client := startResourceProtocolServer(t, func(req *Message) (*Message, error) {
		if req.ListAfter == "" {
			return &Message{
				Type: TypeAck, Reservations: []ReservationView{{SandboxID: "a"}}, ListNext: "a",
			}, nil
		}
		return &Message{Type: TypeAck, ListNext: "a"}, nil
	})
	if _, err := client.AdminList(); err == nil ||
		err.Error() != "client: admin_list returned an empty page with a continuation cursor" {
		t.Fatalf("AdminList error = %v", err)
	}
}
