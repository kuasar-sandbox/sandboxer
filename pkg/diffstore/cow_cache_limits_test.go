package diffstore

import (
	"bytes"
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testCacheWithLimits(t *testing.T, capacity, global, perDisk int, hooks cacheHooks) *Cache {
	t.Helper()
	c, err := newCache(uint64(capacity*cowBlockSize), uint64(global*cowBlockSize), hooks, WithPerDiskDirtyLimit(uint64(perDisk*cowBlockSize)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func checkDirtyCharges(t *testing.T, c *Cache) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	want := make(map[*Disk]int)
	zero := make(map[*Disk]int)
	queued := make(map[*cachePage]bool)
	for cow, worker := range c.workers {
		var previous *cachePage
		for p := worker.dirty.first; p != nil; p = p.next {
			if queued[p] || p.prev != previous || p.key.cow != cow || p.state != cacheDirty {
				t.Fatalf("invalid owner dirty queue: owner=%p page=%+v", cow, p)
			}
			queued[p] = true
			previous = p
		}
		if previous != worker.dirty.last {
			t.Fatalf("owner %p dirty tail does not match traversal", cow)
		}
	}
	for _, p := range c.pages {
		if queued[p] != (p.state == cacheDirty) {
			t.Fatalf("dirty queue membership disagrees with page state: %+v", p)
		}
		delete(queued, p)
		if p.chargedDirty {
			want[p.key.cow]++
			if p.zero {
				zero[p.key.cow]++
			}
		}
	}
	if len(queued) != 0 {
		t.Fatal("dirty queue retained pages outside the cache")
	}
	total := 0
	for cow, n := range c.clients {
		if n != want[cow] || c.chargedZeros[cow] != zero[cow] {
			t.Fatalf("dirty ledger mismatch for %p: owner=%d want=%d", cow, n, want[cow])
		}
		if c.maxDirtyPerDisk > 0 && n > c.maxDirtyPerDisk {
			t.Fatalf("owner exceeded limit: %d", n)
		}
		total += n
		delete(want, cow)
		delete(zero, cow)
	}
	if len(want) != 0 || len(zero) != 0 {
		t.Fatal("dirty reservations belong to detached owners")
	}
	if total != c.dirtyUsed || total > c.maxDirty || len(c.pages) > c.capacity {
		t.Fatalf("global ledger: sum=%d dirty=%d limit=%d pages=%d capacity=%d", total, c.dirtyUsed, c.maxDirty, len(c.pages), c.capacity)
	}
}

func TestCOWCachePerDiskDirtyLimitOptions(t *testing.T) {
	constructors := map[string]func(...CacheOption) (*Cache, error){
		"fixed": func(options ...CacheOption) (*Cache, error) {
			return NewCache(4*cowBlockSize, 2*cowBlockSize, options...)
		},
		"elastic": func(options ...CacheOption) (*Cache, error) {
			return NewElasticCache(4*cowBlockSize, 2*cowBlockSize, cowBlockSize, options...)
		},
	}
	for name, construct := range constructors {
		t.Run(name, func(t *testing.T) {
			for _, test := range []struct {
				name    string
				options []CacheOption
				pages   int
				invalid bool
			}{
				{name: "default"},
				{name: "zero", options: []CacheOption{WithPerDiskDirtyLimit(0)}},
				{name: "one-page", options: []CacheOption{WithPerDiskDirtyLimit(cowBlockSize)}, pages: 1},
				{name: "above-shared-budget", options: []CacheOption{WithPerDiskDirtyLimit(8 * cowBlockSize)}, pages: 8},
				{name: "unaligned", options: []CacheOption{WithPerDiskDirtyLimit(1)}, invalid: true},
				{name: "address-space", options: []CacheOption{WithPerDiskDirtyLimit(math.MaxUint64 &^ (cowBlockSize - 1))}, invalid: true},
				{name: "nil", options: []CacheOption{nil}, invalid: true},
				{name: "duplicate", options: []CacheOption{WithPerDiskDirtyLimit(0), WithPerDiskDirtyLimit(cowBlockSize)}, invalid: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					c, err := construct(test.options...)
					if test.invalid {
						if err == nil {
							_ = c.Close()
							t.Fatal("invalid option accepted")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					defer c.Close()
					if c.maxDirtyPerDisk != test.pages || c.maxDirty != min(2, c.capacity) {
						t.Fatalf("limits: per-disk=%d shared=%d capacity=%d", c.maxDirtyPerDisk, c.maxDirty, c.capacity)
					}
					if ok, err := c.TryResizeCapacity(4 * cowBlockSize); err != nil || !ok {
						t.Fatalf("grow: %v %v", ok, err)
					}
					if c.maxDirtyPerDisk != test.pages || c.maxDirty != 2 {
						t.Fatal("resize lost a configured dirty limit")
					}
				})
			}
		})
	}
}

func TestCOWCacheDisabledDirtyLimitsAndElasticResize(t *testing.T) {
	c, err := NewCache(4*cowBlockSize, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.maxDirtyLimit != 0 || c.maxDirty != 4 || c.maxDirtyPerDisk != 0 {
		t.Fatal("zero dirty limit did not retain capacity bound")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := NewElasticCache(8*cowBlockSize, 0, 2*cowBlockSize)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, pages := range []int{2, 6, 1, 8} {
		ok, err := e.TryResizeCapacity(uint64(pages * cowBlockSize))
		if err != nil || !ok {
			t.Fatalf("resize %d: %v %v", pages, ok, err)
		}
		if e.maxDirty != pages || e.maxDirtyLimit != 0 {
			t.Fatalf("resize %d left effective dirty=%d configured=%d", pages, e.maxDirty, e.maxDirtyLimit)
		}
	}
	disk, err := OpenDisk(filepath.Join(t.TempDir(), "private.diff"), nil, DiffInit{CreateSize: cowBlockSize})
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if !disk.ownCache || disk.cache.maxDirtyLimit != 0 || disk.cache.maxDirty != disk.cache.capacity {
		t.Fatal("private fallback retained an independent dirty ceiling")
	}
	t.Log("NewCache/elastic/private fallback accept zero limits; effective dirty follows capacity through grow/shrink")
}

func TestCOWCacheOwnerLimitOverwriteZeroAndOtherOwner(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	waiting := make(chan struct{}, 16)
	c := testCacheWithLimits(t, 4, 0, 1, cacheHooks{beforeSelect: gate.wait, waiting: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	a := cachedCOW(t, c, 4, nil, WithCheckpoint("qos-owner-a", ""))
	b := cachedCOW(t, c, 4, nil, WithCheckpoint("qos-owner-b", ""))
	for _, d := range []*Disk{a, b} {
		if err := d.WaitMaterialized(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	writePage(t, a, 0, 1)
	awaitSignal(t, gate.entered)
	for range 8 {
		if _, err := a.WriteAt([]byte{2}, 17); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Discard(0, cowBlockSize); err != nil {
		t.Fatal(err)
	}
	if _, err := a.WriteAt([]byte{3}, 17); err != nil {
		t.Fatal(err)
	}
	checkDirtyCharges(t, c)
	// The only dirty reservation is Data; a new Zero at the owner ceiling
	// must retain the existing synchronous space-release escape route.
	if err := a.Discard(cowBlockSize, cowBlockSize); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := make(chan error, 1)
	go func() {
		_, err := a.WriteAtContext(ctx, bytes.Repeat([]byte{4}, cowBlockSize), 2*cowBlockSize)
		blocked <- err
	}()
	awaitSignal(t, waiting)
	writePage(t, b, 0, 5)
	c.mu.Lock()
	ac, bc, total := c.clients[a], c.clients[b], c.dirtyUsed
	c.mu.Unlock()
	if ac != 1 || bc != 1 || total != 2 {
		t.Fatalf("owner quota blocked unrelated owner or double-charged overwrite: %d %d %d", ac, bc, total)
	}
	cancel()
	if err := awaitError(t, blocked); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	checkDirtyCharges(t, c)
	gate.open()
	if err := c.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkDirtyCharges(t, c)
	t.Log("full owner: overwrite/Data-Zero transitions remain singly charged; new Data waits; synchronous discard and other owner progress")
}

func TestCOWCacheGlobalAndCapacityCeilings(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		capacity, global, owner int
	}{{"global", 4, 2, 2}, {"capacity_only", 2, 0, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newWorkerGate()
			defer gate.open()
			waiting := make(chan struct{}, 8)
			c := testCacheWithLimits(t, tc.capacity, tc.global, tc.owner, cacheHooks{beforeSelect: gate.wait, waiting: func() {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}})
			a := cachedCOW(t, c, 4, nil)
			b := cachedCOW(t, c, 4, nil)
			writePage(t, a, 0, 1)
			awaitSignal(t, gate.entered)
			writePage(t, b, 0, 2)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			blocked := make(chan error, 1)
			go func() {
				_, err := a.WriteAtContext(ctx, bytes.Repeat([]byte{3}, cowBlockSize), cowBlockSize)
				blocked <- err
			}()
			awaitSignal(t, waiting)
			checkDirtyCharges(t, c)
			select {
			case err := <-blocked:
				t.Fatalf("exceeded %s bound: %v", tc.name, err)
			default:
			}
			cancel()
			if err := awaitError(t, blocked); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			gate.open()
			if err := c.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCOWCacheOnePageOwnerLimitLargeRequest(t *testing.T) {
	c := testCacheWithLimits(t, 4, 0, 1, cacheHooks{})
	d := cachedCOW(t, c, 16, nil)
	payload := bytes.Repeat([]byte{0x67}, 8*cowBlockSize)
	n, err := d.WriteAt(payload, 0)
	if err != nil || n != len(payload) {
		t.Fatalf("request larger than owner ceiling: %d %v", n, err)
	}
	if err := d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkRead(t, d, 0, payload)
	checkDirtyCharges(t, c)
	t.Log("8-page request completed with a 1-page per-owner dirty ceiling")
}

func TestCOWCacheSlowOwnerQuotaAndFastDrain(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	var slow atomic.Pointer[Disk]
	waiting := make(chan struct{}, 16)
	c := testCacheWithLimits(t, 4, 0, 1, cacheHooks{beforeWrite: func(p []*cachePage) error {
		if p[0].key.cow == slow.Load() {
			gate.wait()
		}
		return nil
	}, waiting: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	a := cachedCOW(t, c, 4, nil)
	slow.Store(a)
	b := cachedCOW(t, c, 4, nil)
	writePage(t, a, 0, 1)
	awaitSignal(t, gate.entered)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := make(chan error, 1)
	go func() {
		_, err := a.WriteAtContext(ctx, bytes.Repeat([]byte{2}, cowBlockSize), cowBlockSize)
		blocked <- err
	}()
	awaitSignal(t, waiting)
	writePage(t, b, 0, 3)
	fast := make(chan error, 1)
	go func() { fast <- b.Drain(context.Background()) }()
	select {
	case err := <-fast:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow owner stalled another owner's writeback")
	}
	checkDirtyCharges(t, c)
	c.mu.Lock()
	ac, bc := c.clients[a], c.clients[b]
	c.mu.Unlock()
	if ac != 1 || bc != 0 {
		t.Fatalf("inflight quota prematurely released: %d %d", ac, bc)
	}
	cancel()
	if err := awaitError(t, blocked); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	gate.open()
	if err := a.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Log("A retains its inflight charge and blocks new A dirty pages while B completes its drain")
}

func TestCOWCacheMultiOwnerFatalJoinsEachInflightWorker(t *testing.T) {
	ag, bg := newWorkerGate(), newWorkerGate()
	defer ag.open()
	defer bg.open()
	var first atomic.Pointer[Disk]
	fatal := errors.New("validation-owner-a-write-failure")
	notified := make(chan error, 1)
	c := testCacheWithLimits(t, 6, 0, 2, cacheHooks{beforeWrite: func(p []*cachePage) error {
		if p[0].key.cow == first.Load() {
			ag.wait()
			return fatal
		}
		bg.wait()
		return nil
	}})
	c.SetFatalHandler(func(err error) { notified <- err })
	a := cachedCOW(t, c, 4, nil)
	first.Store(a)
	b := cachedCOW(t, c, 4, nil)
	writePage(t, a, 0, 1)
	awaitSignal(t, ag.entered)
	writePage(t, b, 0, 2)
	awaitSignal(t, bg.entered)
	ag.open()
	if err := awaitError(t, notified); !errors.Is(err, fatal) {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("B closed while its worker still owns buffers: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	bg.open()
	if err := awaitError(t, closed); !errors.Is(err, fatal) {
		t.Fatal(err)
	}
	if err := a.Close(); !errors.Is(err, fatal) {
		t.Fatal(err)
	}
	if err := c.Close(); !errors.Is(err, fatal) {
		t.Fatal(err)
	}
	c.mu.Lock()
	left := len(c.pages) + len(c.workers) + c.workersRunning
	c.mu.Unlock()
	if left != 0 {
		t.Fatalf("worker/pages retained after join: %d", left)
	}
	t.Log("global fatal wakes admission; each detach still joins only its own pending I/O before buffer release")
}
