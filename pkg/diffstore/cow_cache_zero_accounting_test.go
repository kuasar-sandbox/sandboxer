package diffstore

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// Count logical reservations independently of list membership: a frozen Zero
// remains charged until successful completion or owner detach releases it.
func checkChargedZeros(t *testing.T, c *Cache) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	want := make(map[*Disk]int)
	for _, p := range c.pages {
		if p.chargedDirty && p.zero {
			want[p.key.cow]++
		}
	}
	for cow := range c.clients {
		if got := c.chargedZeros[cow]; got != want[cow] {
			t.Fatalf("owner %p charged Zeros=%d, want %d", cow, got, want[cow])
		}
		delete(want, cow)
	}
	if len(want) != 0 {
		t.Fatalf("charged Zero pages belong to detached owners: %v", want)
	}
	for cow := range c.chargedZeros {
		if _, attached := c.clients[cow]; !attached {
			t.Fatalf("charged Zero counter retained detached owner %p", cow)
		}
	}
}

func TestCOWCacheChargedZeroAccounting(t *testing.T) {
	// No worker or backend: explicitly drive selection/completion so every
	// accounting boundary is observable. Existing gated I/O tests also run the
	// independent oracle through checkBudget, including repair and Close.
	c := &Cache{
		capacity: 4, maxCapacity: 4, maxDirty: 4, maxDirtyLimit: 4,
		pages:        make(map[cacheKey]*cachePage),
		clients:      make(map[*Disk]int),
		chargedZeros: make(map[*Disk]int),
		kick:         make(chan struct{}, 1),
		started:      time.Now(),
	}
	ctx := context.Background()
	a := &Disk{bitmap: make([]uint64, 1)}
	b := &Disk{bitmap: make([]uint64, 1)}
	for _, cow := range []*Disk{a, b} {
		if err := c.attach(cow); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.stageZero(ctx, a, 0); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, c, 1, 1, 0)
	if err := c.stageZero(ctx, a, 0); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, c, 1, 1, 0) // repeated Zero must not charge twice
	if n, err := c.write(ctx, a, []byte{0xb2}, 19, 0, true, func([]byte) error {
		t.Fatal("Zero-to-Data write initialized from backend")
		return nil
	}); n != 1 || err != nil {
		t.Fatalf("partial write=%d, %v", n, err)
	}
	p := c.pages[cacheKey{a, 0}]
	want := make([]byte, cowBlockSize)
	want[19] = 0xb2
	if p.data == nil || !bytes.Equal(p.data[:], want) {
		t.Fatal("Zero-to-Data write did not preserve zeros outside its range")
	}
	checkBudget(t, c, 1, 1, 0) // Data keeps its dirty charge, not a Zero charge
	if err := c.stageZero(ctx, a, 0); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, c, 1, 1, 0)

	c.mu.Lock()
	c.dirty.remove(p)
	p.state = cacheWriteback
	c.mu.Unlock()
	checkBudget(t, c, 1, 1, 1)
	if _, ok := c.stageZero(ctx, a, 0).(*cacheRetry); !ok {
		t.Fatal("Zero writeback did not retain its immutable reservation")
	}
	checkBudget(t, c, 1, 1, 1)
	c.mu.Lock()
	c.completeWritebackLocked([]*cachePage{p})
	c.mu.Unlock()
	checkBudget(t, c, 0, 0, 0)

	// Clean Data promotion acquires both a dirty charge and a Zero charge.
	if n, err := c.write(ctx, a, []byte{0xc3}, cowBlockSize+19, 1, true, func([]byte) error { return nil }); n != 1 || err != nil {
		t.Fatalf("Data write=%d, %v", n, err)
	}
	c.mu.Lock()
	p = c.pages[cacheKey{a, 1}]
	c.dirty.remove(p)
	p.state = cacheWriteback
	c.completeWritebackLocked([]*cachePage{p})
	c.mu.Unlock()
	checkBudget(t, c, 1, 0, 0)
	if err := c.stageZero(ctx, a, 1); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, c, 1, 1, 0)
	c.mu.Lock()
	c.dirty.remove(p)
	p.state = cacheDiscarding
	c.mu.Unlock()
	checkBudget(t, c, 1, 1, 0)
	c.mu.Lock()
	c.releaseLocked(p)
	c.mu.Unlock()
	checkBudget(t, c, 0, 0, 0)

	if err := c.stageZero(ctx, a, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.stageZero(ctx, b, 0); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, c, 2, 2, 0)
	fatalErr := errors.New("writeback failed")
	c.mu.Lock()
	p = c.pages[cacheKey{a, 2}]
	c.dirty.remove(p)
	p.state = cacheWriteback
	c.failLocked(fatalErr)
	c.mu.Unlock()
	checkBudget(t, c, 2, 2, 1) // fatal retains frozen and unselected reservations
	if err := c.stageZero(ctx, a, 3); !errors.Is(err, fatalErr) {
		t.Fatalf("stageZero after fatal=%v, want %v", err, fatalErr)
	}
	checkBudget(t, c, 2, 2, 1)
	c.detach(a)
	checkBudget(t, c, 1, 1, 0)
	c.detach(b)
	checkBudget(t, c, 0, 0, 0)
}
