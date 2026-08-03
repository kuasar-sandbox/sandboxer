package guestlink

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// TestLaunchServer_HelloLaunchHandshake spawns the launch server on a
// UDS and acts as a fake guest sandbox-init: connect, send hello, read
// launch spec, send launch_ack, read ack — all on a single connection.
// Mirrors what sandbox-init does for the cold-start handshake.
func TestLaunchServer_HelloLaunchHandshake(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "vsock.sock_5000")

	spec := &proto.LaunchSpec{
		Exec:    "/usr/bin/echo",
		Args:    []string{"hello"},
		Env:     map[string]string{"PATH": "/usr/bin"},
		Workdir: "/",
		Restart: "never",
	}
	launchAckSeen := make(chan struct{})
	srv := &LaunchServer{
		Path:        sockPath,
		Spec:        spec,
		Logf:        func(string, ...any) {},
		OnLaunchAck: func() { close(launchAckSeen) },
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ctx) }()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := proto.WriteMessage(conn, &proto.Message{
		Type:  proto.TypeHello,
		Phase: "ready",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := proto.ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != proto.TypeLaunch || got.Launch == nil {
		t.Fatalf("expected launch message, got %+v", got)
	}
	if !reflect.DeepEqual(got.Launch, spec) {
		t.Errorf("launch spec mismatch:\n got=%+v\nwant=%+v", got.Launch, spec)
	}

	// HelloDone fires before the guest replies with launch_ack.
	select {
	case <-srv.HelloDone():
	case <-time.After(time.Second):
		t.Errorf("HelloDone not closed after launch sent")
	}

	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeLaunchAck}); err != nil {
		t.Fatal(err)
	}
	ack, err := proto.ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Type != proto.TypeAck {
		t.Errorf("expected ack, got %+v", ack)
	}

	// Server should close the connection after ack.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := proto.ReadMessage(conn); err == nil || (!errors.Is(err, io.EOF) && err != io.ErrUnexpectedEOF) {
		t.Errorf("expected EOF after ack (server should close), got err=%v", err)
	}
	conn.Close()

	select {
	case <-launchAckSeen:
	case <-time.After(time.Second):
		t.Errorf("OnLaunchAck not fired")
	}
	select {
	case <-srv.LaunchAckDone():
	case <-time.After(time.Second):
		t.Errorf("LaunchAckDone not closed")
	}

	srv.Stop()
	if err := <-srvErr; err != nil {
		t.Errorf("Serve returned: %v", err)
	}
}

// TestLaunchServer_AppLifecycleNotifications verifies the server
// dispatches app_started / app_exited on **fresh short-conn dials**
// after the cold-start hello/launch handshake.
func TestLaunchServer_AppLifecycleNotifications(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "vsock.sock_5000")

	var startedPID, exitedCode int
	var startedSeen, exitedSeen = make(chan struct{}), make(chan struct{})
	srv := &LaunchServer{
		Path: sockPath,
		Spec: &proto.LaunchSpec{Exec: "/bin/true"},
		Logf: func(string, ...any) {},
		OnAppStarted: func(pid int) {
			startedPID = pid
			close(startedSeen)
		},
		OnAppExited: func(code int) {
			exitedCode = code
			close(exitedSeen)
		},
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ctx) }()

	roundTrip := func(req *proto.Message) *proto.Message {
		c, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := proto.WriteMessage(c, req); err != nil {
			t.Fatal(err)
		}
		resp, err := proto.ReadMessage(c)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// app_started
	resp := roundTrip(&proto.Message{Type: proto.TypeAppStarted, PID: 4711})
	if resp.Type != proto.TypeAck {
		t.Errorf("app_started: expected ack, got %+v", resp)
	}
	<-startedSeen
	if startedPID != 4711 {
		t.Errorf("app_started pid=%d", startedPID)
	}

	// app_exited
	resp = roundTrip(&proto.Message{Type: proto.TypeAppExited, Code: 137})
	if resp.Type != proto.TypeAck {
		t.Errorf("app_exited: expected ack, got %+v", resp)
	}
	<-exitedSeen
	if exitedCode != 137 {
		t.Errorf("app_exited code=%d", exitedCode)
	}

	srv.Stop()
	if err := <-srvErr; err != nil {
		t.Errorf("Serve returned: %v", err)
	}
}

func TestLaunchServer_AppStartedCallbackRunsAfterACK(t *testing.T) {
	request := encodeLaunchMessage(t, &proto.Message{Type: proto.TypeAppStarted, PID: 4711})
	wantACK := encodeLaunchMessage(t, &proto.Message{Type: proto.TypeAck})
	conn := &scriptedLaunchConn{read: bytes.NewReader(request)}
	var ackComplete atomic.Bool
	conn.write = func(p []byte) (int, error) {
		n, err := conn.written.Write(p)
		if bytes.Equal(conn.written.Bytes(), wantACK) {
			ackComplete.Store(true)
		}
		return n, err
	}
	called := false
	srv := &LaunchServer{
		Logf: func(string, ...any) {},
		OnAppStarted: func(pid int) {
			called = true
			if pid != 4711 {
				t.Errorf("pid = %d, want 4711", pid)
			}
			if !ackComplete.Load() {
				t.Error("OnAppStarted ran before the ACK was fully written")
			}
		},
	}
	if keep := srv.handleConn(conn); keep {
		t.Fatal("app_started connection unexpectedly handed off")
	}
	if !called {
		t.Fatal("OnAppStarted was not called")
	}
	if !bytes.Equal(conn.written.Bytes(), wantACK) {
		t.Fatalf("ACK wire = %x, want %x", conn.written.Bytes(), wantACK)
	}
}

func TestLaunchServer_AppStartedACKFailureSkipsCallback(t *testing.T) {
	request := encodeLaunchMessage(t, &proto.Message{Type: proto.TypeAppStarted, PID: 4711})
	conn := &scriptedLaunchConn{read: bytes.NewReader(request), failAfter: 4}
	conn.write = conn.writeUntilFailure
	called := false
	srv := &LaunchServer{
		Logf:         func(string, ...any) {},
		OnAppStarted: func(int) { called = true },
	}
	if keep := srv.handleConn(conn); keep {
		t.Fatal("app_started connection unexpectedly handed off")
	}
	if called {
		t.Fatal("OnAppStarted ran after a failed ACK write")
	}
}

func encodeLaunchMessage(t *testing.T, msg *proto.Message) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := proto.WriteMessage(&buf, msg); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type scriptedLaunchConn struct {
	read      *bytes.Reader
	written   bytes.Buffer
	write     func([]byte) (int, error)
	failAfter int
}

func (c *scriptedLaunchConn) Read(p []byte) (int, error) { return c.read.Read(p) }
func (c *scriptedLaunchConn) Write(p []byte) (int, error) {
	if c.write != nil {
		return c.write(p)
	}
	return c.written.Write(p)
}
func (c *scriptedLaunchConn) writeUntilFailure(p []byte) (int, error) {
	remaining := c.failAfter - c.written.Len()
	if remaining <= 0 {
		return 0, io.ErrClosedPipe
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, _ := c.written.Write(p)
	return n, nil
}
func (*scriptedLaunchConn) Close() error                     { return nil }
func (*scriptedLaunchConn) LocalAddr() net.Addr              { return testLaunchAddr("local") }
func (*scriptedLaunchConn) RemoteAddr() net.Addr             { return testLaunchAddr("remote") }
func (*scriptedLaunchConn) SetDeadline(time.Time) error      { return nil }
func (*scriptedLaunchConn) SetReadDeadline(time.Time) error  { return nil }
func (*scriptedLaunchConn) SetWriteDeadline(time.Time) error { return nil }

type testLaunchAddr string

func (a testLaunchAddr) Network() string { return "test" }
func (a testLaunchAddr) String() string  { return string(a) }

func TestLaunchServer_NilSpecRejected(t *testing.T) {
	srv := &LaunchServer{Path: "/tmp/never.sock"}
	if err := srv.Listen(); err == nil {
		t.Fatal("expected error when Spec is nil")
	}
}
