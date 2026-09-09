package guestlink

import (
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestUsageReusableConnectionAndDeadlineMargin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vsock")
	var conns atomic.Int32
	p := newFakeCHProxy(t, path, func(c net.Conn) {
		conns.Add(1)
		for {
			msg, err := proto.ReadMessage(c)
			if err != nil {
				return
			}
			req := msg.UsageRequest
			if req.ReadBudgetNS <= 0 || req.ReadBudgetNS >= int64(time.Second) {
				t.Error("Guest budget consumed Host deadline")
			}
			if err := proto.WriteMessage(c, &proto.Message{Type: proto.TypeUsageResponse, UsageResponse: &proto.UsageResponse{RunEpoch: req.RunEpoch, RequestID: req.RequestID, Memory: proto.UsageMemory{UsageReadState: proto.UsageReadState{Status: proto.UsageOK}}}}); err != nil {
				return
			}
		}
	})
	defer p.close()
	u := &UsageClient{Host: &HostClient{BasePath: path}}
	defer u.Close()
	for i := 1; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		w, err := u.Read(ctx, "epoch", uint64(i))
		cancel()
		if err != nil || w.Response.RequestID != uint64(i) || w.Finished.Before(w.Started) {
			t.Fatalf("%+v %v", w, err)
		}
	}
	if conns.Load() != 1 {
		t.Fatal("connection was not reused")
	}
	u.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := u.Read(ctx, "epoch", 20); err != nil {
		t.Fatal(err)
	}
	if conns.Load() != 2 {
		t.Fatal("reconnect failed")
	}
}

func TestUsageStaleResponseRejected(t *testing.T) {
	for _, oldEpoch := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "vsock")
		p := newFakeCHProxy(t, path, func(c net.Conn) {
			msg, err := proto.ReadMessage(c)
			if err != nil {
				return
			}
			r := msg.UsageRequest
			if oldEpoch {
				r.RunEpoch = "old"
			} else {
				r.RequestID--
			}
			_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeUsageResponse, UsageResponse: &proto.UsageResponse{RunEpoch: r.RunEpoch, RequestID: r.RequestID}})
		})
		u := &UsageClient{Host: &HostClient{BasePath: path}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if _, err := u.Read(ctx, "new", 2); err == nil {
			t.Error("stale response accepted")
		}
		cancel()
		u.Close()
		p.close()
	}
}
