package main

import (
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"testing"
	"time"
)

func TestBaseRuntimeManagement(t *testing.T) {
	for _, kind := range []string{proto.TypePing, proto.TypeShutdown, proto.TypeExec, proto.TypeConnect, proto.TypeQuiesce, proto.TypeRestore, proto.TypeAttach} {
		t.Run(kind, func(t *testing.T) {
			guest, host := usageSocketPair(t)
			stopped := make(chan struct{}, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer guest.Close()
				handleReverseConnShutdown(guest, nil, nil, func() { stopped <- struct{}{} })
			}()
			if err := proto.WriteMessage(host, &proto.Message{Type: kind, ID: 17, TSendNs: 42}); err != nil {
				t.Fatal(err)
			}
			reply, err := proto.ReadMessage(host)
			if err != nil {
				t.Fatal(err)
			}
			want := proto.TypeError
			if kind == proto.TypePing {
				want = proto.TypePong
			}
			if kind == proto.TypeShutdown {
				want = proto.TypeAck
			}
			if reply.Type != want {
				t.Fatalf("reply=%+v want %s", reply, want)
			}
			if kind == proto.TypePing && (reply.ID != 17 || reply.TSendNs != 42) {
				t.Fatal("ping identity lost")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("base management stranded")
			}
			if kind == proto.TypeShutdown {
				select {
				case <-stopped:
				default:
					t.Fatal("shutdown not requested")
				}
			}
		})
	}
}
