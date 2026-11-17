package resctl

import (
	"context"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestCacheReleaseRetainedOnDisconnectedSession(t *testing.T) {
	const miB = 1 << 20
	for _, requested := range []uint64{0, 8 * miB} {
		h := &ControllerHooks{
			opts:           ControllerHookOptions{SocketPath: "/tmp/cache-budget-disconnected.sock"},
			lifetimeCtx:    context.Background(),
			cacheSupported: true, admitted: true, connected: false,
			cacheMaximum: 64 * miB, cacheReservation: 16 * miB,
			reconnectWake: make(chan struct{}, 1),
		}
		_, reservation, _, _, err := h.RequestCacheBudget(8*miB, requested)
		if !resource.IsTransportError(err) {
			t.Fatalf("requested %d: expected transport error, got %v", requested, err)
		}
		if reservation != 8*miB || h.CacheReservation() != 8*miB {
			t.Fatalf("requested %d: StateSync would resurrect already reclaimed cache: returned %d, local %d",
				requested, reservation, h.CacheReservation())
		}
	}
}
