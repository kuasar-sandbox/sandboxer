package resctl

import (
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

// A full but aged cache must not reacquire a node budget while no I/O occurs.
func TestElasticCacheExpiredCleanCannotTriggerIdleRegrowth(t *testing.T) {
	const MiB = uint64(1 << 20)
	s := diffstore.BudgetSnapshot{Capacity: 8 * MiB, Maximum: 64 * MiB, Used: 8 * MiB, Expired: 8 * MiB}
	if cacheHasGrowthDemand(s, 4*MiB) {
		t.Fatal("entirely expired clean data triggered idle regrowth")
	}
	s.Expired = 7 * MiB
	if cacheHasGrowthDemand(s, 4*MiB) {
		t.Fatal("one MiB of revived cache incorrectly reached the four MiB watermark")
	}
	// Hits move expired pages to the active LRU, so new demand can grow.
	s.Expired = 4 * MiB
	if !cacheHasGrowthDemand(s, 4*MiB) {
		t.Fatal("newly revived working set should trigger growth")
	}
	s.Expired = 0
	if !cacheHasGrowthDemand(s, 4*MiB) {
		t.Fatal("full active cache lost growth demand")
	}
}

func TestCacheGrowthMarginIsBoundedByOneGrant(t *testing.T) {
	const miB = uint64(1 << 20)
	for _, tc := range []struct {
		capacity, previousGrant, margin uint64
	}{
		{4, 4, 3},
		{12, 8, 9},
		{28, 16, 21},
		{60, 32, 32},
		{512, 32, 32},
		{512, 4, 8},
	} {
		if got := cacheGrowthMargin(tc.capacity*miB, tc.previousGrant*miB); got != tc.margin*miB {
			t.Errorf("capacity %d MiB, previous grant %d MiB: margin %d, want %d",
				tc.capacity, tc.previousGrant, got, tc.margin*miB)
		}
	}
}
