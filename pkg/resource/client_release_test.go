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

// Release must not wait behind a concurrent, silent RPC beyond its own deadline.
func TestClientReleaseDeadlineWhileStateSyncHoldsRPCMutex(t *testing.T) {
	entered := make(chan struct{})
	unblock := make(chan struct{})
	defer close(unblock)
	client := releaseTestClient(t, func(req *Message) (*Message, bool) {
		if req.Type != TypeStateSync {
			t.Errorf("unexpected concurrent request: %s", req.Type)
		}
		close(entered)
		<-unblock
		return &Message{Type: TypeAck}, false
	})
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	rpcDone := make(chan error, 1)
	go func() {
		_, err := client.roundTripContext(context.Background(), &Message{Type: TypeStateSync}, time.Second)
		rpcDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("StateSync did not reach controller")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := client.ReleaseContext(ctx, "normal")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release should time out on client lock, got %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("release waited beyond context deadline")
	}
	unblock <- struct{}{}
	select {
	case err := <-rpcDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("StateSync did not finish")
	}
	if got := client.Token(); got != "original-token" {
		t.Fatalf("timed-out release must retain token, got %q", got)
	}
}

func TestClientStateSyncWriteBarrierPrecedesACK(t *testing.T) {
	received := make(chan struct{})
	allowACK := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(allowACK) }) }
	defer unblock()
	client := releaseTestClient(t, func(req *Message) (*Message, bool) {
		if req.Type != TypeStateSync {
			t.Errorf("unexpected request: %s", req.Type)
		}
		close(received)
		<-allowACK
		return &Message{Type: TypeAck, Token: "rotated-token"}, false
	})
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	guard, cancelGuard := context.WithCancel(context.Background())
	defer cancelGuard()
	written := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := client.StateSyncContextOnWrite(context.Background(),
			StateSyncParams{SandboxID: "sandbox-1", PreviousToken: "original-token"},
			func() { close(written) }, guard)
		finished <- err
	}()
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("write barrier not signaled")
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("no StateSync")
	}
	select {
	case <-finished:
		t.Fatal("finished before ACK")
	default:
	}
	cancelGuard() // A request already written must still drain its ACK.
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("no StateSync reply")
	}
	if client.Token() != "rotated-token" {
		t.Fatal("token not rotated")
	}
}

// A parent cancellation between the outer reconnect guard and the serialized
// wire write must not create a new StateSync or rotate the reservation token.
func TestClientStateSyncStartGuardCanceledBeforeWrite(t *testing.T) {
	var received atomic.Int32
	client := releaseTestClient(t, func(req *Message) (*Message, bool) {
		if req.Type == TypeStateSync {
			received.Add(1)
		}
		return &Message{Type: TypeAck, Token: "unexpected-rotation"}, false
	})
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	startCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.mu.Lock() // Stall between reconnect's guard and the actual wire write.
	ready := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(ready)
		_, err := client.StateSyncContextOnWrite(context.Background(),
			StateSyncParams{SandboxID: "sandbox-1", PreviousToken: "original-token"},
			nil, startCtx)
		finished <- err
	}()
	<-ready
	cancel()
	client.mu.Unlock()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want canceled before wire write, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled StateSync did not return")
	}
	if received.Load() != 0 || client.Token() != "original-token" {
		t.Fatalf("unexpected StateSync after cancellation: received=%d token=%q", received.Load(), client.Token())
	}
}
