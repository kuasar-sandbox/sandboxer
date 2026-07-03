package vhost

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// memBackend is a tiny Backend stub for tests.
type memBackend struct {
	data []byte
}

func (m *memBackend) ReadAt(buf []byte, offset int64) (int, error) {
	if offset >= int64(len(m.data)) {
		return 0, errors.New("EOF")
	}
	return copy(buf, m.data[offset:]), nil
}
func (m *memBackend) WriteAt(buf []byte, offset int64) (int, error) {
	if offset+int64(len(buf)) > int64(len(m.data)) {
		return 0, errors.New("OOB")
	}
	return copy(m.data[offset:], buf), nil
}
func (m *memBackend) Flush() error                    { return nil }
func (m *memBackend) Discard(off, length int64) error { return nil }
func (m *memBackend) Size() int64                     { return int64(len(m.data)) }
func (m *memBackend) ReadOnly() bool                  { return false }
func (m *memBackend) BackendStats() map[string]any    { return nil }

// TestServer_AcceptsAfterMasterDisconnect verifies the regression fix
// for Issue 1/2: after a master closes the connection, the server must
// continue to accept new master connections (not exit Serve).
//
// Previously Serve() returned after a single AcceptUnix() — meaning any
// CH reset/reboot path that tried to reconnect vhost-user backends got
// "Connection refused" and CH wedged.
func TestServer_AcceptsAfterMasterDisconnect(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "vhost.sock")

	srv := NewServer(sockPath, &memBackend{data: make([]byte, 4096)}, nil)
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	// First connect+disconnect cycle.
	c1, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	// Wait briefly for server to log the connection.
	time.Sleep(50 * time.Millisecond)
	c1.Close()

	// After disconnect, server must accept again. Try several times to
	// avoid a flake from the server still in resetConnectionState.
	var connected2 bool
	for i := 0; i < 20; i++ {
		time.Sleep(25 * time.Millisecond)
		c2, err := net.Dial("unix", sockPath)
		if err == nil {
			c2.Close()
			connected2 = true
			break
		}
	}
	if !connected2 {
		t.Fatal("vhost server did not accept second master connection after first disconnect — single-shot regression")
	}

	// Third cycle for good measure.
	c3, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("third dial: %v", err)
	}
	c3.Close()

	srv.Stop()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Stop")
	}

	// Socket file should be cleaned up.
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket %s not cleaned up: %v", sockPath, err)
	}
}

// TestServer_StopUnblocksReadMidConnection verifies Stop() closes the
// active master connection so a serveOneMaster blocked in ReadMessage
// returns immediately. There is no read deadline (vhost-user has no
// keepalive — see serveOneMaster), so Stop's conn.Close() is the
// *only* mechanism that unblocks an idle master read.
//
// This is the regression test for the e2e_sandbox_cold timeout caused
// by an earlier vhost re-acceptable fix: cancelBackends called Stop,
// but serveOneMaster was blocked in Read and only noticed after the
// (now removed) deadline fired, leaving sandbox-ctl pinned in
// backendWG.Wait().
func TestServer_StopUnblocksReadMidConnection(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "vhost.sock")
	srv := NewServer(sockPath, &memBackend{data: make([]byte, 4096)}, nil)
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	// Connect master, send no messages — server is now blocked in Read.
	c, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	time.Sleep(50 * time.Millisecond) // let server enter Read

	t0 := time.Now()
	srv.Stop()
	select {
	case err := <-serveErr:
		elapsed := time.Since(t0)
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
		if elapsed > 1*time.Second {
			t.Fatalf("Stop took too long to unblock serve: %v "+
				"(Stop must close activeConn to unblock the otherwise-deadline-less Read)",
				elapsed)
		}
		t.Logf("Stop unblocked Serve in %v", elapsed)
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return within 3s of Stop — activeConn close not unblocking Read")
	}
}

// TestServer_ResetClearsState verifies resetConnectionState wipes
// memtable / features / queues so a second master starts clean.
func TestServer_ResetClearsState(t *testing.T) {
	srv := NewServer("/tmp/test.sock-unused", &memBackend{data: make([]byte, 4096)}, nil)

	srv.mu.Lock()
	srv.features = 0xDEADBEEF
	srv.protocolFeatures = 0xCAFEBABE
	// Stub queue with stop/done already closed (so reset doesn't block).
	stop := make(chan struct{})
	done := make(chan struct{})
	close(done)
	srv.queues[0] = &virtq{stop: stop, done: done}
	srv.mu.Unlock()

	// Mark reset done via a goroutine.
	doneFlag := atomic.Bool{}
	go func() {
		srv.resetConnectionState()
		doneFlag.Store(true)
	}()

	// Wait briefly for reset.
	for i := 0; i < 20; i++ {
		if doneFlag.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !doneFlag.Load() {
		t.Fatal("resetConnectionState blocked unexpectedly")
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.features != 0 {
		t.Errorf("features not reset: %x", srv.features)
	}
	if srv.protocolFeatures != 0 {
		t.Errorf("protocolFeatures not reset: %x", srv.protocolFeatures)
	}
	if srv.queues[0] != nil {
		t.Errorf("queue[0] not cleared")
	}
}
