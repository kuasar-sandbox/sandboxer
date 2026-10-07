package resource

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func releaseTestClient(t *testing.T, handler func(*Message) (*Message, bool)) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "cleanup.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	var conns sync.Map
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conns.Store(conn, struct{}{})
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				defer conns.Delete(conn)
				for {
					req, err := ReadMessage(conn)
					if err != nil {
						return
					}
					reply, drop := handler(req)
					if drop {
						return
					}
					if reply != nil {
						if err := WriteMessage(conn, reply); err != nil {
							return
						}
					}
				}
			}()
		}
	}()
	client := &Client{SocketPath: sock, token: "original-token"}
	t.Cleanup(func() {
		client.Close()
		listener.Close()
		<-done
		conns.Range(func(key, value any) bool { key.(net.Conn).Close(); return true })
		workers.Wait()
	})
	return client
}

func TestClientReleaseReconnectUsesOnlyOriginalToken(t *testing.T) {
	var count atomic.Int32
	client := releaseTestClient(t, func(req *Message) (*Message, bool) {
		count.Add(1)
		if req.Type != TypeRelease || req.Token != "original-token" {
			t.Errorf("unexpected cleanup request: %+v", req)
		}
		return &Message{Type: TypeAck}, false
	})
	if err := client.Release("normal"); err != nil {
		t.Fatal(err)
	}
	if err := client.Release("normal"); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 || client.Token() != "" {
		t.Fatalf("requests=%d token=%q", count.Load(), client.Token())
	}
}

func TestClientReleaseLostAckRetriesIdempotentOriginalToken(t *testing.T) {
	var requests, removals atomic.Int32
	var charged atomic.Bool
	charged.Store(true)
	client := releaseTestClient(t, func(req *Message) (*Message, bool) {
		if req.Type != TypeRelease || req.Token != "original-token" {
			t.Errorf("unexpected cleanup request: %+v", req)
		}
		if charged.Swap(false) {
			removals.Add(1)
		}
		if requests.Add(1) == 1 {
			return nil, true
		}
		return &Message{Type: TypeAck}, false
	})
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := client.Release("normal"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || removals.Load() != 1 || client.Token() != "" {
		t.Fatalf("requests=%d removals=%d token=%q", requests.Load(), removals.Load(), client.Token())
	}
}

func TestClientReleaseDoesNotRetryControllerRejection(t *testing.T) {
	var requests atomic.Int32
	client := releaseTestClient(t, func(req *Message) (*Message, bool) {
		requests.Add(1)
		return &Message{Type: TypeError, Msg: "checked release rejected"}, false
	})
	var rejection *ControllerError
	if err := client.Release("normal"); !errors.As(err, &rejection) {
		t.Fatalf("want controller error, got %v", err)
	}
	if requests.Load() != 1 || client.Token() != "original-token" {
		t.Fatalf("requests=%d token=%q", requests.Load(), client.Token())
	}
}

func TestClientReleaseDeadlineIncludesReconnect(t *testing.T) {
	var requests atomic.Int32
	client := releaseTestClient(t, func(req *Message) (*Message, bool) { requests.Add(1); return nil, false })
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := client.ReleaseContext(ctx, "normal")
	if err == nil {
		t.Fatal("silent controller unexpectedly acknowledged")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cleanup exceeded shared context bound")
	}
	if requests.Load() != 1 || client.Token() != "original-token" {
		t.Fatalf("requests=%d token=%q", requests.Load(), client.Token())
	}
}

func TestClientReleaseUnavailableControllerKeepsToken(t *testing.T) {
	client := &Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock"), token: "original-token"}
	start := time.Now()
	if err := client.Release("normal"); err == nil {
		t.Fatal("missing controller unexpectedly acknowledged")
	}
	if time.Since(start) > time.Second || client.Token() != "original-token" {
		t.Fatal("cleanup was unbounded or discarded token")
	}
}

func TestClientReleaseWithoutTokenDoesNotDial(t *testing.T) {
	client := &Client{SocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	if err := client.Release("normal"); err != nil {
		t.Fatal(err)
	}
}
