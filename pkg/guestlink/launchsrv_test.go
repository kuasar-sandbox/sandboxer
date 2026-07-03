package guestlink

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
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

func TestLaunchServer_NilSpecRejected(t *testing.T) {
	srv := &LaunchServer{Path: "/tmp/never.sock"}
	if err := srv.Listen(); err == nil {
		t.Fatal("expected error when Spec is nil")
	}
}
