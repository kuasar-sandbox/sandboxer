package main

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/usage"
	"golang.org/x/sys/unix"
)

func TestUsageRunEpochMatchesHost(t *testing.T) {
	for _, epoch := range []string{strings.Repeat("x", 128), strings.Repeat("x", 129), strings.Repeat("x", 256), strings.Repeat("😀", 32), strings.Repeat("😀", 33)} {
		t.Run(epoch, func(t *testing.T) {
			m, openErr := usage.Open(t.TempDir(), "test", epoch, time.Now(), time.Second, time.Minute)
			if m != nil {
				defer func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					m.Close(ctx, time.Now())
				}()
			}
			s := newUsageService()
			defer s.close()
			var reads atomic.Int32
			s.sources[0].read = func() usageRawResult {
				reads.Add(1)
				return usageRawResult{memory: proto.UsageMemory{UsageReadState: proto.UsageReadState{Status: proto.UsageOK}}}
			}
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			client := os.NewFile(uintptr(pair[1]), "usage-client")
			defer client.Close()
			if err := unix.SetsockoptTimeval(pair[1], unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 2}); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.serve(&vsockConn{fd: pair[0]}, &proto.Message{Type: proto.TypeUsageRequest, UsageRequest: &proto.UsageRequest{RunEpoch: epoch, RequestID: 1, ReadBudgetNS: int64(100 * time.Millisecond)}})
			}()
			msg, guestErr := proto.ReadMessage(client)
			client.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Guest retained the usage connection")
			}
			accepted := guestErr == nil && msg.Type == proto.TypeUsageResponse && msg.UsageResponse != nil && msg.UsageResponse.RunEpoch == epoch
			want := len(epoch) <= 128
			if (openErr == nil) != want || accepted != want || (reads.Load() == 1) != want {
				t.Fatalf("epoch bytes=%d: Open=%v, Guest=%v, accepted=%t, reads=%d", len(epoch), openErr, guestErr, accepted, reads.Load())
			}
		})
	}
}
