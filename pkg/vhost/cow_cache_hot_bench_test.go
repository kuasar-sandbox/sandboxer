package vhost

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// Isolate cache-hit and first-publication overhead from disk latency.
func BenchmarkCOWCacheHot(b *testing.B) {
	for _, explicit := range []string{"nil", "background", "cancelable"} {
		b.Run(fmt.Sprintf("context=%s", explicit), func(b *testing.B) {
			cache, err := NewCOWCache(DefaultCOWCacheSize, DefaultCOWMaxDirtySize)
			if err != nil {
				b.Fatal(err)
			}
			defer cache.Close()
			cow, err := OpenBlockCOW(filepath.Join(b.TempDir(), "diff"), nil, DiffInit{CreateSize: 1 << 20}, WithCOWCache(cache))
			if err != nil {
				b.Fatal(err)
			}
			defer cow.Close()
			buf := make([]byte, cowBlockSize)
			if _, err := cow.WriteAt(buf, 0); err != nil {
				b.Fatal(err)
			}
			if err := cow.Drain(context.Background()); err != nil {
				b.Fatal(err)
			}
			var ctx context.Context
			if explicit == "background" {
				ctx = context.Background()
			}
			if explicit == "cancelable" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(context.Background())
				defer cancel()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := cow.readAt(ctx, buf, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("signal-without-waiters", func(b *testing.B) {
		c := &COWCache{changed: make(chan struct{})}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.mu.Lock()
			c.signalLocked()
			c.mu.Unlock()
		}
	})
}
