package guestlink

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestLaunchServerExt4DiscardCompatibility(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, supported := range []bool{false, true} {
			for _, mode := range []string{"", proto.DiscardAuto, proto.DiscardManual} {
				t.Run(fmt.Sprintf("deferred=%t/supported=%t/mode=%s", deferred, supported, mode), func(t *testing.T) {
					var failure error
					spec := &proto.LaunchSpec{Exec: "/bin/true", Ext4Discard: mode}
					srv := &LaunchServer{
						Spec: spec, DeferLaunch: deferred, Logf: t.Logf,
						stopped: make(chan struct{}), runtimeReady: make(chan struct{}),
						helloDone: make(chan struct{}), launchReady: make(chan struct{}),
						launchAckDone: make(chan struct{}), muxReady: make(chan struct{}),
						OnLaunchFailure: func(err error) { failure = err },
					}
					close(srv.launchReady)
					request := append(encodeLaunchMessage(t, &proto.Message{
						Type: proto.TypeHello, Phase: "runtime_ready", InitialLaunchStateV1: true, Ext4DiscardV1: supported,
					}), encodeLaunchMessage(t, &proto.Message{Type: proto.TypeLaunchAck})...)
					conn := &scriptedLaunchConn{read: bytes.NewReader(request)}
					srv.handleConn(conn)
					if srv.Ext4DiscardSupported() != supported {
						t.Fatal("accepted hello lost discard capability")
					}
					reader := bytes.NewReader(conn.written.Bytes())
					response, err := proto.ReadMessage(reader)
					if err != nil {
						t.Fatal(err)
					}
					if mode == proto.DiscardManual && !supported {
						if response.Type != proto.TypeError || !strings.Contains(response.Msg, "ext4_discard_v1") || failure == nil {
							t.Fatalf("unsupported manual silently accepted: %+v, failure=%v", response, failure)
						}
						select {
						case <-srv.HelloDone():
							t.Fatal("unsupported manual was published")
						default:
						}
						select {
						case <-srv.MUXReady():
							t.Fatal("unsupported manual became ready")
						default:
						}
						return
					}
					if response.Type != proto.TypeLaunch || response.Launch == nil || response.Launch.Ext4Discard != mode || failure != nil {
						t.Fatalf("launch policy changed: %+v, failure=%v", response, failure)
					}
					ack, err := proto.ReadMessage(reader)
					if err != nil || ack.Type != proto.TypeAck {
						t.Fatalf("launch failed: %+v, %v", ack, err)
					}
					// A rejected duplicate hello cannot change the capability
					// observed by the Runtime after its ready barrier.
					duplicate := &scriptedLaunchConn{read: bytes.NewReader(encodeLaunchMessage(t, &proto.Message{
						Type: proto.TypeHello, Phase: "runtime_ready", InitialLaunchStateV1: true, Ext4DiscardV1: !supported,
					}))}
					srv.handleConn(duplicate)
					if srv.Ext4DiscardSupported() != supported {
						t.Fatal("duplicate hello changed accepted discard capability")
					}
				})
			}
		}
	}
}
