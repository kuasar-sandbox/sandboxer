package guestlink

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestEnvironmentReportPersistsBeforeACKWithoutColdHello(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore", true: "cold"}[deferred], func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "env-report-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			var recorded, primary atomic.Bool
			srv := &LaunchServer{Path: filepath.Join(dir, "launch.sock"), DeferLaunch: deferred, Spec: &proto.LaunchSpec{}, OnAppExited: func(int) { primary.Store(true) }, OnEnvironmentExited: func(f proto.EnvironmentExit) error {
				recorded.Store(f.State == "failed" && f.Reason == "observer-expired")
				return nil
			}}
			if deferred {
				srv.Spec = nil
			}
			if err := srv.Listen(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer srv.Stop()
			go srv.Serve(ctx)
			conn, err := net.Dial("unix", srv.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			fact := &proto.EnvironmentExit{State: "failed", Reason: "observer-expired"}
			if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeEnvironmentExited, EnvironmentExit: fact}); err != nil {
				t.Fatal(err)
			}
			ack, err := proto.ReadMessage(conn)
			if err != nil || ack.Type != proto.TypeAck {
				t.Fatalf("ACK=%+v/%v", ack, err)
			}
			if !recorded.Load() || primary.Load() {
				t.Fatal("environment fact lost or substituted primary exit")
			}
			select {
			case <-srv.RuntimeReady():
				t.Fatal("terminal report manufactured cold Hello")
			default:
			}
		})
	}
}

func TestEnvironmentReportRejectsMalformedFact(t *testing.T) {
	for _, fact := range []*proto.EnvironmentExit{nil, {State: "unknown", Reason: "x"}, {State: "failed"}, {State: "failed", Code: -2, Reason: "x"}, {State: "stopped", TermSignal: 65, Reason: "x"}} {
		request := encodeLaunchMessage(t, &proto.Message{Type: proto.TypeEnvironmentExited, EnvironmentExit: fact})
		conn := &scriptedLaunchConn{read: bytes.NewReader(request)}
		srv := &LaunchServer{Logf: func(string, ...any) {}, OnEnvironmentExited: func(proto.EnvironmentExit) error { t.Error("invalid fact reached owner"); return nil }}
		if srv.handleConn(conn) {
			t.Fatal("terminal notification handed off connection")
		}
		reply, err := proto.ReadMessage(bytes.NewReader(conn.written.Bytes()))
		if err != nil || reply.Type != proto.TypeError {
			t.Fatalf("invalid fact accepted: %+v/%v", reply, err)
		}
	}
}
