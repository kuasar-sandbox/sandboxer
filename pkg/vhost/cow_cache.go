package vhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	DefaultCOWCacheSize    = 32 << 20
	DefaultCOWMaxDirtySize = 16 << 20
	maxWritebackPages      = (1 << 20) / cowBlockSize
)

type cacheState uint8

const (
	cacheLoading cacheState = iota
	cacheConstructing
	cacheClean
	cacheDirty
	cacheWriteback
	cacheDiscarding
)

type cacheKey struct {
	cow   *BlockCOW
	block int64
}
type cachePage struct {
	key                 cacheKey
	data                [cowBlockSize]byte
	state               cacheState
	chargedDirty, fresh bool
	prev, next          *cachePage
}

// Intrusive lists reuse page metadata across arbitrarily many overwrites.
type pageList struct{ first, last *cachePage }

func (q *pageList) push(p *cachePage) {
	p.prev, p.next = q.last, nil
	if q.last != nil {
		q.last.next = p
	} else {
		q.first = p
	}
	q.last = p
}
func (q *pageList) remove(p *cachePage) {
	if p.prev != nil {
		p.prev.next = p.next
	} else {
		q.first = p.next
	}
	if p.next != nil {
		p.next.prev = p.prev
	} else {
		q.last = p.prev
	}
	p.prev, p.next = nil, nil
}

// COWCache owns one sandbox's plaintext pages and one writeback worker. All
// active root/data diffs must share this handle. Close the BlockCOWs before it.
// Page payload and metadata are bounded by capacity. Writeback stages frozen
// pages directly in each diff's existing bounded, owned DIO workspace.
type COWCache struct {
	mu                            sync.Mutex
	capacity, maxDirty, dirtyUsed int
	pages                         map[cacheKey]*cachePage
	free                          []*cachePage
	clients                       map[*BlockCOW]int // dirty reservations, including construction and I/O
	clean, dirty                  pageList
	changed                       chan struct{}
	kick                          chan struct{}
	done                          chan struct{}
	closed                        bool
	urgent                        bool
	active                        *BlockCOW
	err                           error
	onFatal                       func(error)
	hooks                         cacheHooks
	writeBytes, writeBatches      uint64
	peakUsed, peakDirty           int
}

// Hooks are installed before starting the worker, only by deterministic tests.
type cacheHooks struct {
	afterReadCopy func()
	waiting       func()
	beforeSelect  func()
	newTimer      func() (<-chan time.Time, func())
	aggregating   func()
	beforeWrite   func([]*cachePage) error
	beforeCleanup func() error // deterministic failure-path I/O gate for tests
}

func NewCOWCache(cacheSize, maxDirtySize uint64) (*COWCache, error) {
	return newCOWCache(cacheSize, maxDirtySize, cacheHooks{})
}
func newCOWCache(size, dirty uint64, hooks cacheHooks) (*COWCache, error) {
	if size == 0 || dirty == 0 || dirty > size || size%cowBlockSize != 0 || dirty%cowBlockSize != 0 || size > uint64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("vhost: COW cache sizes must be positive 4 KiB multiples with max_dirty_size <= cache_size")
	}
	c := &COWCache{capacity: int(size / cowBlockSize), maxDirty: int(dirty / cowBlockSize), pages: make(map[cacheKey]*cachePage), clients: make(map[*BlockCOW]int), kick: make(chan struct{}, 1), done: make(chan struct{}), hooks: hooks}
	go c.run()
	return c, nil
}

type cacheBlockCOWOption struct{ cache *COWCache }

func WithCOWCache(c *COWCache) BlockCOWOption { return cacheBlockCOWOption{c} }
func (o cacheBlockCOWOption) applyBlockCOW(options *blockCOWOptions) error {
	if o.cache == nil || options.cache != nil {
		return fmt.Errorf("vhost: nil or duplicate WithCOWCache")
	}
	options.cache = o.cache
	return nil
}
func (c *COWCache) attach(cow *BlockCOW) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if c.closed {
		return os.ErrClosed
	}
	c.clients[cow] = 0
	return nil
}

// Allocate a broadcast generation only when someone actually waits. Hot page
// publication need not allocate a channel for a nonexistent waiter.
func (c *COWCache) changeLocked() <-chan struct{} {
	if c.changed == nil {
		c.changed = make(chan struct{})
	}
	return c.changed
}
func (c *COWCache) signalLocked() {
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
}
func (c *COWCache) kickLocked(urgent bool) {
	c.urgent = c.urgent || urgent
	select {
	case c.kick <- struct{}{}:
	default:
	}
}
func (c *COWCache) checkLocked(ctx context.Context) error {
	if c.err != nil {
		return c.err
	}
	if c.closed {
		return os.ErrClosed
	}
	return ctx.Err()
}
func (c *COWCache) waitLocked(ctx context.Context) error {
	ch := c.changeLocked()
	if c.hooks.waiting != nil {
		c.hooks.waiting()
	}
	c.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *COWCache) reserveLocked(key cacheKey, dirty bool) *cachePage {
	if len(c.pages) == c.capacity {
		if c.clean.first == nil {
			return nil
		}
		c.releaseLocked(c.clean.first)
	}
	var p *cachePage
	if n := len(c.free); n > 0 {
		p = c.free[n-1]
		c.free = c.free[:n-1]
	} else {
		p = &cachePage{}
	}
	p.key, p.chargedDirty = key, dirty
	p.state = cacheLoading
	if dirty {
		p.state = cacheConstructing
		c.dirtyUsed++
		c.clients[key.cow]++
	}
	c.pages[key] = p
	c.peakUsed = max(c.peakUsed, len(c.pages))
	c.peakDirty = max(c.peakDirty, c.dirtyUsed)
	return p
}
func (c *COWCache) releaseLocked(p *cachePage) {
	switch p.state {
	case cacheClean:
		c.clean.remove(p)
	case cacheDirty:
		c.dirty.remove(p)
	}
	delete(c.pages, p.key)
	if p.chargedDirty {
		c.dirtyUsed--
		c.clients[p.key.cow]--
	}
	clear(p.data[:])
	p.key = cacheKey{}
	p.chargedDirty = false
	p.fresh = false
	p.prev = nil
	p.next = nil
	c.free = append(c.free, p)
}

// read is called with every foreground stripe for this upper-only range held.
// Hits copy under mu; cold full-page runs reserve bounded Loading entries, then
// use the existing DIO workspace outside mu. Stripes prevent writes/discard even
// when a full cache requires bypass; Loading entries cannot be evicted.
func (c *COWCache) read(ctx context.Context, cow *BlockCOW, buf []byte, offset int64) (int, error) {
	done := 0
	for done < len(buf) {
		pos := offset + int64(done)
		key := cacheKey{cow, pos / cowBlockSize}
		length := min(len(buf)-done, cowBlockSize-int(pos%cowBlockSize))
		c.mu.Lock()
		if err := c.checkLocked(ctx); err != nil {
			c.mu.Unlock()
			return done, err
		}
		if p := c.pages[key]; p != nil {
			if p.state == cacheLoading || p.state == cacheConstructing || p.state == cacheDiscarding {
				if err := c.waitLocked(ctx); err != nil {
					return done, err
				}
				continue
			}
			copy(buf[done:done+length], p.data[pos%cowBlockSize:])
			if p.state == cacheClean {
				c.clean.remove(p)
				c.clean.push(p)
			}
			c.mu.Unlock()
			done += length
			continue
		}
		if pos%cowBlockSize == 0 && length == cowBlockSize {
			n, err := c.readColdRunLocked(ctx, cow, buf[done:], pos)
			done += n
			if err != nil {
				return done, err
			}
			continue
		}
		// Partial edges retain full-page loading without allocating a request buffer.
		p := c.reserveLocked(key, false)
		c.mu.Unlock()
		if p == nil {
			n, err := cow.diff.ReadAt(buf[done:done+length], pos)
			done += n
			if err != nil {
				return done, err
			}
			continue
		}
		err := readFullAt(cow.diff, p.data[:], key.block*cowBlockSize)
		c.mu.Lock()
		if err == nil {
			err = c.checkLocked(ctx)
		}
		if err != nil {
			c.releaseLocked(p)
		} else {
			p.state = cacheClean
			c.clean.push(p)
			copy(buf[done:done+length], p.data[pos%cowBlockSize:])
		}
		c.signalLocked()
		c.mu.Unlock()
		if err != nil {
			return done, err
		}
		done += length
	}
	return done, nil
}

// Starts at a missing aligned page, with mu held; returns with it unlocked.
// Only the requested cold run is read. No dirty, writeback, loading, clean hit,
// base page or hole is included. The fixed pointer array adds no payload buffer.
func (c *COWCache) readColdRunLocked(ctx context.Context, cow *BlockCOW, buf []byte, offset int64) (int, error) {
	var reserved [maxDiffScratchSize / cowBlockSize]*cachePage
	count := 0
	for count < min(len(reserved), len(buf)/cowBlockSize) {
		key := cacheKey{cow, offset/cowBlockSize + int64(count)}
		if c.pages[key] != nil {
			break
		}
		reserved[count] = c.reserveLocked(key, false)
		count++
	}
	c.mu.Unlock()
	length := count * cowBlockSize
	err := cow.diff.withDirectRead(length, offset, func(plain []byte) error {
		// Loading entries are privately owned until publication: eviction,
		// writes and discard cannot release or change them, and the COW's
		// operation lifetime prevents detach. Fill from owned plaintext before
		// taking the global lock to publish the completed pages.
		for i, p := range reserved[:count] {
			if p != nil {
				copy(p.data[:], plain[i*cowBlockSize:(i+1)*cowBlockSize])
			}
		}
		c.mu.Lock()
		if err := c.checkLocked(ctx); err != nil {
			c.mu.Unlock()
			return err
		}
		for _, p := range reserved[:count] {
			if p == nil {
				continue
			}
			p.state = cacheClean
			c.clean.push(p)
		}
		c.signalLocked()
		c.mu.Unlock()
		// Publish from private plaintext before exposing any bytes to the caller.
		copy(buf[:length], plain)
		if c.hooks.afterReadCopy != nil {
			c.hooks.afterReadCopy()
		}
		return nil
	})
	if err != nil {
		c.mu.Lock()
		for _, p := range reserved[:count] {
			if p != nil {
				c.releaseLocked(p)
			}
		}
		c.signalLocked()
		c.mu.Unlock()
		return 0, err
	}
	return length, nil
}

// A foreground writer drops its block stripe before sleeping. The wake channel
// is captured under mu to avoid lost wakeups; every retry reacquires the stripe
// and reevaluates upper presence, cache state and both quotas.
type cacheRetry struct{ wake <-chan struct{} }

func (*cacheRetry) Error() string { return "vhost: retry cache admission" }
func (c *COWCache) retryLocked() error {
	retry := &cacheRetry{c.changeLocked()}
	if c.hooks.waiting != nil {
		c.hooks.waiting()
	}
	c.mu.Unlock()
	return retry
}

// write reserves both resources atomically. Initialization fills the reserved
// page directly, so a waiting guest never owns an extra queued payload copy.
func (c *COWCache) write(ctx context.Context, cow *BlockCOW, buf []byte, offset, block int64, upper bool, init func([]byte) error) (int, error) {
	key := cacheKey{cow, block}
	c.mu.Lock()
	if err := c.checkLocked(ctx); err != nil {
		c.mu.Unlock()
		return 0, err
	}
	p := c.pages[key]
	if p != nil && p.state != cacheClean && p.state != cacheDirty {
		c.kickLocked(true)
		return 0, c.retryLocked()
	}
	if p != nil && p.state == cacheDirty {
		copy(p.data[offset%cowBlockSize:], buf)
		c.mu.Unlock()
		return len(buf), nil
	}
	if c.dirtyUsed == c.maxDirty {
		c.kickLocked(true)
		return 0, c.retryLocked()
	}
	if p != nil {
		c.clean.remove(p)
		p.state = cacheDirty
		p.chargedDirty = true
		c.dirtyUsed++
		c.clients[cow]++
		c.peakDirty = max(c.peakDirty, c.dirtyUsed)
		copy(p.data[offset%cowBlockSize:], buf)
		c.dirty.push(p)
		c.kickLocked(false)
		c.mu.Unlock()
		return len(buf), nil
	}
	p = c.reserveLocked(key, true)
	if p == nil {
		c.kickLocked(true)
		return 0, c.retryLocked()
	}
	p.fresh = !upper
	c.mu.Unlock()
	err := init(p.data[:])
	c.mu.Lock()
	if err == nil {
		err = c.checkLocked(ctx)
	}
	if err != nil {
		c.releaseLocked(p)
		c.signalLocked()
		c.mu.Unlock()
		return 0, err
	}
	copy(p.data[offset%cowBlockSize:], buf)
	p.state = cacheDirty
	c.dirty.push(p)
	// Cache content and upper presence become visible before worker selection
	// and before the frontend drops the same-block stripe.
	cow.markDirty(block)
	c.signalLocked()
	c.kickLocked(false)
	c.mu.Unlock()
	return len(buf), nil
}

func (c *COWCache) run() {
	defer close(c.done)
	batch := make([]*cachePage, 0, maxWritebackPages)
	aggregated := false
	for {
		c.mu.Lock()
		if c.err != nil || c.closed {
			c.mu.Unlock()
			return
		}
		if c.dirty.first == nil {
			aggregated = false
			c.mu.Unlock()
			<-c.kick
			continue
		}
		wait := !aggregated && !c.urgent
		c.mu.Unlock()
		if wait {
			c.aggregate()
		}
		// A burst gets one fixed window. Drain an existing backlog immediately,
		// including noncontiguous singletons, instead of sleeping per batch.
		aggregated = true
		if c.hooks.beforeSelect != nil {
			c.hooks.beforeSelect()
		}
		c.mu.Lock()
		if c.err != nil || c.closed {
			c.mu.Unlock()
			return
		}
		c.urgent = false
		batch = batch[:0]
		if anchor := c.dirty.first; anchor != nil {
			// Always include the oldest page, then find contiguous dirty neighbours
			// in the bounded index regardless of arrival order. Unselected FIFO age
			// is untouched. Scan at most one batch backwards and forwards.
			first := anchor
			for lenBack := 1; lenBack < maxWritebackPages && first.key.block > 0; lenBack++ {
				p := c.pages[cacheKey{anchor.key.cow, first.key.block - 1}]
				if p == nil || p.state != cacheDirty {
					break
				}
				first = p
			}
			for block := first.key.block; len(batch) < maxWritebackPages; block++ {
				p := c.pages[cacheKey{anchor.key.cow, block}]
				if p == nil || p.state != cacheDirty {
					break
				}
				c.dirty.remove(p)
				p.state = cacheWriteback
				batch = append(batch, p)
			}
		}
		if len(batch) == 0 {
			c.mu.Unlock()
			continue
		}
		cow := batch[0].key.cow
		off := batch[0].key.block * cowBlockSize
		c.active = cow
		c.signalLocked()
		c.mu.Unlock()
		// Frozen pages remain readable; no writer may change or release them.
		var err error
		if c.hooks.beforeWrite != nil {
			err = c.hooks.beforeWrite(batch)
		}
		if err == nil {
			err = cow.diff.writeCachePages(batch, off)
		}
		if err != nil {
			// A known failure must be visible before any potentially blocking
			// rollback. Keep active and the frozen pages until that I/O ends.
			c.mu.Lock()
			report := c.failLocked(fmt.Errorf("vhost: diff writeback %s at %d: %w", cow.diff.f.Name(), off, err))
			fatal := c.err
			c.mu.Unlock()
			if report != nil {
				report(fatal)
			}
		}
		var cleanupErr error
		if err != nil {
			if c.hooks.beforeCleanup != nil {
				cleanupErr = c.hooks.beforeCleanup()
			}
			// Only genuinely new upper blocks can be punched after a partial batch.
			// Existing upper blocks may already be partially changed; never hole them.
			if cleanupErr == nil {
				for _, p := range batch {
					if p.fresh {
						cleanupErr = errors.Join(cleanupErr, cow.diff.punchHole(p.key.block*cowBlockSize, cowBlockSize))
					}
				}
			}
		}
		c.mu.Lock()
		c.active = nil
		c.writeBatches++
		if err == nil && c.err == nil {
			c.writeBytes += uint64(len(batch) * cowBlockSize)
			for _, p := range batch {
				p.state = cacheClean
				p.fresh = false
				p.chargedDirty = false
				c.dirtyUsed--
				c.clients[p.key.cow]--
				c.clean.push(p)
			}
		}
		if cleanupErr != nil {
			c.err = errors.Join(c.err, fmt.Errorf("vhost: diff cleanup %s at %d: %w", cow.diff.f.Name(), off, cleanupErr))
		}
		c.signalLocked()
		c.mu.Unlock()
		clear(batch)
	}
}

// Ordinary kicks do not shorten or restart the aggregation deadline. Pressure,
// Drain, failure and Close interrupt it. Tests supply a manually fired timer.
func (c *COWCache) aggregate() {
	var deadline <-chan time.Time
	var stop func()
	if c.hooks.newTimer != nil {
		deadline, stop = c.hooks.newTimer()
	} else {
		timer := time.NewTimer(time.Millisecond)
		deadline, stop = timer.C, func() { timer.Stop() }
	}
	defer stop()
	for {
		c.mu.Lock()
		urgent := c.urgent || c.err != nil || c.closed
		c.mu.Unlock()
		if urgent {
			return
		}
		if c.hooks.aggregating != nil {
			c.hooks.aggregating()
		}
		select {
		case <-deadline:
			return
		case <-c.kick:
		}
	}
}

func (c *COWCache) failLocked(err error) func(error) {
	if c.err != nil {
		return nil
	}
	c.err = err
	c.signalLocked()
	c.kickLocked(true)
	return c.onFatal
}

// SetFatalHandler installs the runtime's non-blocking termination reporter.
// It must not synchronously join the cache worker. A prior failure is replayed.
func (c *COWCache) SetFatalHandler(report func(error)) {
	c.mu.Lock()
	c.onFatal = report
	err := c.err
	c.mu.Unlock()
	if err != nil && report != nil {
		report(err)
	}
}
func (c *COWCache) Err() error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

// Drain waits for accepted dirty reservations and writeback without fsync.
// Callers stop frontend writes first when they need a stable boundary.
func (c *COWCache) Drain(ctx context.Context) error { return c.drain(ctx, nil) }
func (c *COWCache) drain(ctx context.Context, cow *BlockCOW) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		c.mu.Lock()
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return err
		}
		n := c.dirtyUsed
		if cow != nil {
			n = c.clients[cow]
		}
		if n == 0 {
			c.mu.Unlock()
			return nil
		}
		c.kickLocked(true)
		if err := c.waitLocked(ctx); err != nil {
			return err
		}
	}
}
func (c *COWCache) detach(cow *BlockCOW) {
	c.mu.Lock()
	// Even after fatal, a real syscall owns its buffers until it returns.
	for c.active == cow {
		_ = c.waitLocked(context.Background())
		c.mu.Lock()
	}
	for key, p := range c.pages {
		if key.cow == cow {
			c.releaseLocked(p)
		}
	}
	delete(c.clients, cow)
	c.signalLocked()
	c.mu.Unlock()
}
func (c *COWCache) Close() error {
	c.mu.Lock()
	if len(c.clients) != 0 {
		c.mu.Unlock()
		return fmt.Errorf("vhost: close COWs before their shared cache")
	}
	c.closed = true
	c.signalLocked()
	c.kickLocked(true)
	c.mu.Unlock()
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.free {
		clear(p.data[:])
	}
	c.free = nil
	return c.err
}

func (c *COWCache) discard(ctx context.Context, cow *BlockCOW, block int64) error {
	key := cacheKey{cow, block}
	for {
		c.mu.Lock()
		if err := c.checkLocked(ctx); err != nil {
			c.mu.Unlock()
			return err
		}
		p := c.pages[key]
		if p != nil && p.state != cacheClean && p.state != cacheDirty {
			if err := c.waitLocked(ctx); err != nil {
				return err
			}
			continue
		}
		if p != nil {
			if p.state == cacheClean {
				c.clean.remove(p)
			} else {
				c.dirty.remove(p)
			}
			p.state = cacheDiscarding
		}
		c.mu.Unlock()
		err := cow.diff.punchHole(block*cowBlockSize, cowBlockSize)
		c.mu.Lock()
		var report func(error)
		if err == nil {
			if p != nil {
				c.releaseLocked(p)
			}
			cow.bitmapMu.Lock()
			cow.bitmap[block/64] &^= 1 << (uint64(block) % 64)
			cow.bitmapMu.Unlock()
		} else {
			report = c.failLocked(fmt.Errorf("vhost: discard block %d: %w", block, err))
		}
		fatal := c.err
		c.signalLocked()
		c.mu.Unlock()
		if report != nil {
			report(fatal)
		}
		return err
	}
}

// COWCacheStats reports shared sandbox totals in bytes, not per-disk budgets.
type COWCacheStats struct {
	Used, DirtyUsed, Clean, Loading, Writeback uint64
	Capacity, MaxDirty, PeakUsed, PeakDirty    uint64
	WrittenBytes, WriteBatches                 uint64
}

func (c *COWCache) Stats() COWCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := COWCacheStats{Used: uint64(len(c.pages)) * cowBlockSize, DirtyUsed: uint64(c.dirtyUsed) * cowBlockSize, Capacity: uint64(c.capacity) * cowBlockSize, MaxDirty: uint64(c.maxDirty) * cowBlockSize, PeakUsed: uint64(c.peakUsed) * cowBlockSize, PeakDirty: uint64(c.peakDirty) * cowBlockSize, WrittenBytes: c.writeBytes, WriteBatches: c.writeBatches}
	for _, p := range c.pages {
		switch p.state {
		case cacheClean:
			s.Clean += cowBlockSize
		case cacheLoading, cacheConstructing:
			s.Loading += cowBlockSize
		case cacheWriteback:
			s.Writeback += cowBlockSize
		}
	}
	return s
}

var _ io.Closer = (*COWCache)(nil)
