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
// Page payload and metadata are bounded by capacity; the worker has one extra
// fixed 1 MiB batch copy. Each diff has independent bounded DIO workspaces.
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
	waiting      func()
	beforeSelect func()
	beforeWrite  func([]*cachePage) error
}

func NewCOWCache(cacheSize, maxDirtySize uint64) (*COWCache, error) {
	return newCOWCache(cacheSize, maxDirtySize, cacheHooks{})
}
func newCOWCache(size, dirty uint64, hooks cacheHooks) (*COWCache, error) {
	if size == 0 || dirty == 0 || dirty > size || size%cowBlockSize != 0 || dirty%cowBlockSize != 0 || size > uint64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("vhost: COW cache sizes must be positive 4 KiB multiples with max_dirty_size <= cache_size")
	}
	c := &COWCache{capacity: int(size / cowBlockSize), maxDirty: int(dirty / cowBlockSize), pages: make(map[cacheKey]*cachePage), clients: make(map[*BlockCOW]int), changed: make(chan struct{}), kick: make(chan struct{}, 1), done: make(chan struct{}), hooks: hooks}
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
func (c *COWCache) signalLocked() { close(c.changed); c.changed = make(chan struct{}) }
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
	ch := c.changed
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

// read is called with the foreground block stripe held. Copies on hits occur
// under mu; slow misses and bypass reads perform disk I/O after dropping it.
func (c *COWCache) read(ctx context.Context, cow *BlockCOW, buf []byte, offset int64) (int, error) {
	key := cacheKey{cow, offset / cowBlockSize}
	for {
		c.mu.Lock()
		if err := c.checkLocked(ctx); err != nil {
			c.mu.Unlock()
			return 0, err
		}
		if p := c.pages[key]; p != nil {
			if p.state == cacheLoading || p.state == cacheConstructing || p.state == cacheDiscarding {
				if err := c.waitLocked(ctx); err != nil {
					return 0, err
				}
				continue
			}
			copy(buf, p.data[offset%cowBlockSize:])
			if p.state == cacheClean {
				c.clean.remove(p)
				c.clean.push(p)
			}
			c.mu.Unlock()
			return len(buf), nil
		}
		p := c.reserveLocked(key, false)
		c.mu.Unlock()
		if p == nil {
			return cow.diff.ReadAt(buf, offset)
		}
		err := readFullAt(cow.diff, p.data[:], key.block*cowBlockSize)
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
		p.state = cacheClean
		c.clean.push(p)
		copy(buf, p.data[offset%cowBlockSize:])
		c.signalLocked()
		c.mu.Unlock()
		return len(buf), nil
	}
}

// A foreground writer drops its block stripe before sleeping. The wake channel
// is captured under mu to avoid lost wakeups; every retry reacquires the stripe
// and reevaluates upper presence, cache state and both quotas.
type cacheRetry struct{ wake <-chan struct{} }

func (*cacheRetry) Error() string { return "vhost: retry cache admission" }
func (c *COWCache) retryLocked() error {
	retry := &cacheRetry{c.changed}
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
	buffer := make([]byte, maxWritebackPages*cowBlockSize)
	defer clear(buffer)
	batch := make([]*cachePage, 0, maxWritebackPages)
	for {
		c.mu.Lock()
		if c.err != nil || c.closed {
			c.mu.Unlock()
			return
		}
		if c.dirty.first == nil {
			c.mu.Unlock()
			<-c.kick
			continue
		}
		urgent := c.urgent
		c.urgent = false
		c.mu.Unlock()
		if !urgent {
			timer := time.NewTimer(time.Millisecond)
			select {
			case <-timer.C:
			case <-c.kick:
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if c.hooks.beforeSelect != nil {
			c.hooks.beforeSelect()
		}
		c.mu.Lock()
		if c.err != nil || c.closed {
			c.mu.Unlock()
			return
		}
		batch = batch[:0]
		for p := c.dirty.first; p != nil && len(batch) < maxWritebackPages; p = c.dirty.first {
			if len(batch) > 0 {
				last := batch[len(batch)-1]
				if p.key.cow != last.key.cow || p.key.block != last.key.block+1 {
					break
				}
			}
			c.dirty.remove(p)
			p.state = cacheWriteback
			batch = append(batch, p)
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
		for i, p := range batch {
			copy(buffer[i*cowBlockSize:], p.data[:])
		}
		var err error
		if c.hooks.beforeWrite != nil {
			err = c.hooks.beforeWrite(batch)
		}
		if err == nil {
			err = writeFullAt(cow.diff, buffer[:len(batch)*cowBlockSize], off)
		}
		clear(buffer[:len(batch)*cowBlockSize])
		if err != nil {
			// Only genuinely new upper blocks can be punched after a partial batch.
			// Existing upper blocks may already be partially changed; never hole them.
			for _, p := range batch {
				if p.fresh {
					err = errors.Join(err, cow.diff.punchHole(p.key.block*cowBlockSize, cowBlockSize))
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
		var report func(error)
		if err != nil {
			report = c.failLocked(fmt.Errorf("vhost: diff writeback %s at %d: %w", cow.diff.f.Name(), off, err))
		}
		fatal := c.err
		c.signalLocked()
		c.mu.Unlock()
		clear(batch)
		if report != nil {
			report(fatal)
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
