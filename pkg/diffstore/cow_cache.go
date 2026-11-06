package diffstore

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
	DefaultCacheSize      = 32 << 20
	DefaultMaxDirtySize   = 16 << 20
	maxWritebackPages     = (1 << 20) / cowBlockSize
	maxWritebackMutations = (4 << 20) / cowBlockSize
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
	cow   *Disk
	block int64
}
type cachePage struct {
	key                 cacheKey
	data                *[cowBlockSize]byte // nil for explicit Zero; Data owns a buffer
	state               cacheState
	chargedDirty, fresh bool
	zero                bool // a DISCARD mutation, independent of cache state
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

// Cache owns one sandbox's plaintext pages and one writeback worker. All
// active root/data diffs must share this handle. Close the Disks before it.
// Page payload and metadata are bounded by capacity. Writeback stages frozen
// pages directly in each diff's existing bounded, owned DIO workspace.
type Cache struct {
	mu                            sync.Mutex
	capacity, maxDirty, dirtyUsed int
	pages                         map[cacheKey]*cachePage
	free                          []*cachePage          // metadata only; all data pointers are nil
	freeData                      []*[cowBlockSize]byte // cleared, exclusively owned payloads
	clients                       map[*Disk]int         // dirty reservations, including construction and I/O
	clean, dirty                  pageList
	changed                       chan struct{}
	kick                          chan struct{}
	done                          chan struct{}
	closed                        bool
	urgent                        bool
	active                        *Disk
	err                           error
	onFatal                       func(error)
	hooks                         cacheHooks
	writeBytes, writeBatches      uint64
	peakUsed, peakDirty           int
}

// Hooks are installed before starting the worker, only by deterministic tests.
type cacheHooks struct {
	idle          func() // observes the worker entering its event wait, under mu
	afterReadCopy func()
	waiting       func()
	beforeSelect  func()
	newTimer      func() (<-chan time.Time, func())
	aggregating   func()
	beforeWrite   func([]*cachePage) error
	beforeCleanup func() error // deterministic failure-path I/O gate for tests
}

func NewCache(cacheSize, maxDirtySize uint64) (*Cache, error) {
	return newCache(cacheSize, maxDirtySize, cacheHooks{})
}
func newCache(size, dirty uint64, hooks cacheHooks) (*Cache, error) {
	if size == 0 || dirty == 0 || dirty > size || size%cowBlockSize != 0 || dirty%cowBlockSize != 0 || size > uint64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("diffstore: COW cache sizes must be positive 4 KiB multiples with max_dirty_size <= cache_size")
	}
	c := &Cache{capacity: int(size / cowBlockSize), maxDirty: int(dirty / cowBlockSize), pages: make(map[cacheKey]*cachePage), clients: make(map[*Disk]int), kick: make(chan struct{}, 1), done: make(chan struct{}), hooks: hooks}
	go c.run()
	return c, nil
}

type cacheDiskOption struct{ cache *Cache }

func WithCache(c *Cache) DiskOption { return cacheDiskOption{c} }
func (o cacheDiskOption) applyDisk(options *diskOptions) error {
	if o.cache == nil || options.cache != nil {
		return fmt.Errorf("diffstore: nil or duplicate WithCache")
	}
	options.cache = o.cache
	return nil
}
func (c *Cache) attach(cow *Disk) error {
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
func (c *Cache) changeLocked() <-chan struct{} {
	if c.changed == nil {
		c.changed = make(chan struct{})
	}
	return c.changed
}
func (c *Cache) signalLocked() {
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
}
func (c *Cache) kickLocked(urgent bool) {
	c.urgent = c.urgent || urgent
	select {
	case c.kick <- struct{}{}:
	default:
	}
}
func (c *Cache) checkLocked(ctx context.Context) error {
	if c.err != nil {
		return c.err
	}
	if c.closed {
		return os.ErrClosed
	}
	return ctx.Err()
}
func (c *Cache) waitLocked(ctx context.Context) error {
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
func (c *Cache) reserveLocked(key cacheKey, dirty bool) *cachePage {
	if len(c.pages) == c.capacity {
		if c.clean.first == nil {
			return nil
		}
		c.releaseLocked(c.clean.first)
	}
	var p *cachePage
	if n := len(c.free); n > 0 {
		p = c.free[n-1]
		c.free[n-1] = nil
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

// Payloads are separate from page metadata so accepting Zero never has to
// allocate a 4 KiB array. Both pools are bounded by the existing page capacity:
// a new payload is needed only when an admitted page lacks one and this pool is
// empty. Every buffer returned by this helper is zeroed, including reuse after
// Data became Zero. The caller holds mu and owns the page's mutable state.
func (c *Cache) acquireDataLocked(p *cachePage) {
	if p.data != nil {
		return
	}
	if n := len(c.freeData); n > 0 {
		p.data = c.freeData[n-1]
		c.freeData[n-1] = nil
		c.freeData = c.freeData[:n-1]
	} else {
		p.data = &[cowBlockSize]byte{}
	}
}

// A frozen page cannot reach here until its I/O has finished. Clear the old
// plaintext before retaining its buffer for a later Data page; Zero metadata
// itself has no payload to clear or retain.
func (c *Cache) releaseDataLocked(p *cachePage) {
	if p.data != nil {
		clear(p.data[:])
		c.freeData = append(c.freeData, p.data)
		p.data = nil
	}
}

func (c *Cache) releaseLocked(p *cachePage) {
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
	c.releaseDataLocked(p)
	p.key = cacheKey{}
	p.chargedDirty = false
	p.fresh = false
	p.zero = false
	p.prev = nil
	p.next = nil
	c.free = append(c.free, p)
}

// read is called with every foreground stripe for this upper-only range held.
// Hits copy under mu; cold full-page runs reserve bounded Loading entries, then
// use the existing DIO workspace outside mu. Stripes prevent writes/discard even
// when a full cache requires bypass; Loading entries cannot be evicted.
func (c *Cache) read(ctx context.Context, cow *Disk, buf []byte, offset int64) (int, error) {
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
			if p.zero {
				clear(buf[done : done+length])
			} else {
				copy(buf[done:done+length], p.data[pos%cowBlockSize:])
			}
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
		if p != nil {
			c.acquireDataLocked(p)
		}
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
func (c *Cache) readColdRunLocked(ctx context.Context, cow *Disk, buf []byte, offset int64) (int, error) {
	var reserved [maxDiffScratchSize / cowBlockSize]*cachePage
	count := 0
	for count < min(len(reserved), len(buf)/cowBlockSize) {
		key := cacheKey{cow, offset/cowBlockSize + int64(count)}
		if c.pages[key] != nil {
			break
		}
		reserved[count] = c.reserveLocked(key, false)
		if reserved[count] != nil {
			c.acquireDataLocked(reserved[count])
		}
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

func (*cacheRetry) Error() string { return "diffstore: retry cache admission" }
func (c *Cache) retryLocked() error {
	retry := &cacheRetry{c.changeLocked()}
	if c.hooks.waiting != nil {
		c.hooks.waiting()
	}
	c.mu.Unlock()
	return retry
}

// write reserves both resources atomically. Initialization fills the reserved
// page directly, so a waiting guest never owns an extra queued payload copy.
func (c *Cache) write(ctx context.Context, cow *Disk, buf []byte, offset, block int64, upper bool, init func([]byte) error) (int, error) {
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
		if p.zero {
			// A partial write must preserve logical zero outside its range,
			// never stale bytes from the backend or a recycled Data buffer.
			c.acquireDataLocked(p)
		}
		p.zero = false
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
		p.zero = false
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
	c.acquireDataLocked(p)
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
	p.zero = false
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

// stageZero accepts one explicit v4 DISCARD page under its foreground stripe.
// Zero shares the ordinary cache and dirty budgets. If either quota blocks a
// cold/clean page and no same-owner Zero can release quota, use the synchronous
// single-page path instead of preventing a reclaiming discard from entering.
// Other admission waits drop the stripe exactly like WriteAt. A selected writeback
// remains immutable until completion, so an older Data write cannot resurrect
// after Zero is accepted. Zero has no payload; reads synthesize zeros, and a
// later Data write acquires a zeroed buffer without consulting the old backend,
// template or base. The original logical total/dirty charges remain unchanged.
func (c *Cache) stageZero(ctx context.Context, cow *Disk, block int64) error {
	c.mu.Lock()
	if err := c.checkLocked(ctx); err != nil {
		c.mu.Unlock()
		return err
	}
	p := c.pages[cacheKey{cow, block}]
	if p != nil && p.state != cacheClean && p.state != cacheDirty {
		c.kickLocked(true)
		return c.retryLocked()
	}
	alreadyDirty := p != nil && p.state == cacheDirty
	if !alreadyDirty {
		blockedTotal := p == nil && len(c.pages) == c.capacity && c.clean.first == nil
		if c.dirtyUsed == c.maxDirty || blockedTotal {
			pendingZero := false
			for _, q := range c.pages {
				if q.key.cow == cow && q.chargedDirty && q.zero {
					pendingZero = true
					break
				}
			}
			if !pendingZero {
				// Still holding the foreground stripe. Do not kick a queued
				// Data allocation before this space-releasing mutation; the
				// old synchronous path rechecks page state and owns completion.
				c.mu.Unlock()
				return c.discard(ctx, cow, block, block)
			}
			c.kickLocked(true)
			return c.retryLocked()
		}
		if p == nil {
			p = c.reserveLocked(cacheKey{cow, block}, true)
			if p == nil {
				c.kickLocked(true)
				return c.retryLocked()
			}
		} else {
			c.clean.remove(p)
			p.chargedDirty = true
			c.dirtyUsed++
			c.clients[cow]++
			c.peakDirty = max(c.peakDirty, c.dirtyUsed)
		}
	}
	c.releaseDataLocked(p)
	p.zero, p.fresh, p.state = true, false, cacheDirty
	if !alreadyDirty {
		c.dirty.push(p)
	}
	// Logical ownership and readable Zero precede both worker selection and ACK.
	cow.markDirty(block)
	c.signalLocked()
	c.kickLocked(false)
	c.mu.Unlock()
	return nil
}

func (c *Cache) run() {
	defer close(c.done)
	batch := make([]*cachePage, 0, maxWritebackMutations)
	var records [maxWritebackMutations]pageWrite
	aggregated := false
	for {
		c.mu.Lock()
		if c.err != nil || c.closed {
			c.mu.Unlock()
			return
		}
		if c.oldestEligibleLocked() == nil {
			aggregated = false
			if c.hooks.idle != nil {
				c.hooks.idle()
			}
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
		if anchor := c.oldestEligibleLocked(); anchor != nil {
			// Keep the oldest anchor, then fill its same-DIFF, same-kind batch
			// with neighboring runs before unrelated logical pages. The
			// bounded candidate list preserves batching for dispersed
			// writes while retaining physical coalescing for mapped pages.
			var candidates [maxWritebackMutations]*cachePage
			count := 0
			limit := maxWritebackPages
			if anchor.zero {
				limit = maxWritebackMutations
			}
			for p := anchor; p != nil && count < limit; p = p.next {
				if p.key.cow == anchor.key.cow && p.zero == anchor.zero {
					candidates[count] = p
					count++
				}
			}
			// Data retains its existing 256-page workspace and transaction path.
			// Zero uses only metadata and can fill a larger bounded batch.
			for _, candidate := range candidates[:count] {
				if len(batch) == limit {
					break
				}
				if candidate.state != cacheDirty {
					continue
				}
				first := candidate
				for n := 1; n < limit-len(batch) && first.key.block > 0; n++ {
					p := c.pages[cacheKey{anchor.key.cow, first.key.block - 1}]
					if p == nil || p.state != cacheDirty || p.zero != anchor.zero {
						break
					}
					first = p
				}
				for block := first.key.block; len(batch) < limit; block++ {
					p := c.pages[cacheKey{anchor.key.cow, block}]
					if p == nil || p.state != cacheDirty || p.zero != anchor.zero {
						break
					}
					c.dirty.remove(p)
					p.state = cacheWriteback
					batch = append(batch, p)
				}
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
		for i, p := range batch {
			records[i] = pageWrite{Block: uint64(p.key.block), Fresh: p.fresh, Zero: p.zero}
			if !p.zero {
				records[i].Data = p.data[:]
			}
		}
		pages := records[:len(batch)]
		err := c.writeFrozen(cow, batch, pages)
		if !batch[0].zero && isWritebackSpacePressure(cow, err) {
			// The backend has joined/rolled back the failed reservation and
			// remains healthy. Keep this Data batch frozen while completing
			// one finite cohort of already accepted same-owner Zero mutations.
			if err = c.repairSpacePressure(cow); err == nil {
				// Successful prefix transactions may be replayed using these
				// immutable Data buffers. A second pressure failure is fatal.
				err = c.writeFrozen(cow, batch, pages)
			}
		}
		if err != nil {
			// A known failure must be visible before any potentially blocking
			// rollback. Keep active and the frozen pages until that I/O ends.
			c.mu.Lock()
			report := c.failLocked(fmt.Errorf("diffstore: diff writeback %s at %d: %w", cow.diff.name(), off, err))
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
				cleanupErr = cow.diff.cleanupWriteFailure(pages)
			}
		}
		c.mu.Lock()
		c.active = nil
		c.writeBatches++
		if err == nil && c.err == nil {
			c.completeWritebackLocked(batch)
		}
		if cleanupErr != nil {
			c.err = errors.Join(c.err, fmt.Errorf("diffstore: diff cleanup %s at %d: %w", cow.diff.name(), off, cleanupErr))
		}
		c.signalLocked()
		c.mu.Unlock()
		clear(pages)
		clear(batch)
	}
}

func (c *Cache) writeFrozen(cow *Disk, batch []*cachePage, pages []pageWrite) error {
	if err := c.Err(); err != nil {
		return err
	}
	if c.hooks.beforeWrite != nil {
		if err := c.hooks.beforeWrite(batch); err != nil {
			return err
		}
		// A foreground failure may have occurred while the gate was held.
		if err := c.Err(); err != nil {
			return err
		}
	}
	return cow.diff.writePages(context.Background(), pages)
}

func (c *Cache) completeWritebackLocked(batch []*cachePage) {
	for _, p := range batch {
		if p.zero {
			// Committed Zero is readable from the backend. Return its quota
			// and metadata instead of crowding out Data on the clean LRU.
			c.releaseLocked(p)
			continue
		}
		c.writeBytes += cowBlockSize
		p.state = cacheClean
		p.fresh = false
		p.chargedDirty = false
		c.dirtyUsed--
		c.clients[p.key.cow]--
		c.clean.push(p)
	}
}

// Called only after v4 safely defers a reservation failure. Capture exactly the
// current same-owner dirty Zero pages; new arrivals cannot extend this repair or
// starve the frozen Data retry. The pointer cohort is bounded by dirty quota and
// borrows existing cache metadata. active remains the original owner throughout.
func (c *Cache) repairSpacePressure(cow *Disk) error {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	count := 0
	for p := c.dirty.first; p != nil; p = p.next {
		if p.key.cow == cow && p.zero {
			count++
		}
	}
	cohort := make([]*cachePage, 0, count)
	for p := c.dirty.first; p != nil; {
		next := p.next
		if p.key.cow == cow && p.zero {
			c.dirty.remove(p)
			p.state = cacheWriteback
			cohort = append(cohort, p)
		}
		p = next
	}
	c.signalLocked()
	c.mu.Unlock()
	defer clear(cohort)
	var records [maxWritebackMutations]pageWrite
	for start := 0; start < len(cohort); start += maxWritebackMutations {
		batch := cohort[start:min(start+maxWritebackMutations, len(cohort))]
		for i, p := range batch {
			records[i] = pageWrite{Block: uint64(p.key.block), Zero: true}
		}
		pages := records[:len(batch)]
		err := c.writeFrozen(cow, batch, pages)
		clear(pages)
		c.mu.Lock()
		c.writeBatches++
		if err == nil && c.err == nil {
			c.completeWritebackLocked(batch)
		} else if err == nil {
			err = c.err
		}
		c.signalLocked()
		c.mu.Unlock()
		if err != nil {
			// Uncompleted cohort pages stay frozen/charged until the normal
			// fatal shutdown joins this worker and detaches the disk.
			return err
		}
	}
	return nil
}

// Pending owners remain dirty (and rewritable). Search the existing bounded
// queue without a batch-size search limit: a long pending prefix must not
// conceal another disk's runnable pages. Completion kicks this same worker.
func (c *Cache) oldestEligibleLocked() *cachePage {
	for p := c.dirty.first; p != nil; p = p.next {
		if backendWritable(p.key.cow.diff) {
			return p
		}
	}
	return nil
}

// Ordinary kicks do not shorten or restart the aggregation deadline. Pressure,
// Drain, failure and Close interrupt it. Tests supply a manually fired timer.
func (c *Cache) aggregate() {
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

func (c *Cache) failLocked(err error) func(error) {
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
func (c *Cache) SetFatalHandler(report func(error)) {
	c.mu.Lock()
	c.onFatal = report
	err := c.err
	c.mu.Unlock()
	if err != nil && report != nil {
		report(err)
	}
}
func (c *Cache) Err() error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

// Drain waits for accepted dirty reservations and writeback without fsync.
// Callers stop frontend writes first when they need a stable boundary.
func (c *Cache) Drain(ctx context.Context) error { return c.drain(ctx, nil) }
func (c *Cache) drain(ctx context.Context, cow *Disk) error {
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
func (c *Cache) detach(cow *Disk) {
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
func (c *Cache) Close() error {
	c.mu.Lock()
	if len(c.clients) != 0 {
		c.mu.Unlock()
		return fmt.Errorf("diffstore: close COWs before their shared cache")
	}
	c.closed = true
	c.signalLocked()
	c.kickLocked(true)
	c.mu.Unlock()
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, data := range c.freeData {
		clear(data[:])
	}
	c.freeData = nil
	c.free = nil
	return c.err
}

func (c *Cache) discard(ctx context.Context, cow *Disk, first, last int64) error {
	for {
		c.mu.Lock()
		if err := c.checkLocked(ctx); err != nil {
			c.mu.Unlock()
			return err
		}
		var pages [maxDiscardPages]*cachePage
		count, waiting := 0, false
		for block := first; block <= last; block++ {
			p := c.pages[cacheKey{cow, block}]
			if p == nil {
				continue // no reservation for an uncached page
			}
			if p.state != cacheClean && p.state != cacheDirty {
				waiting = true
				break
			}
			pages[count] = p
			count++
		}
		if waiting {
			if err := c.waitLocked(ctx); err != nil {
				return err
			}
			continue
		}
		// Freeze only after checking the entire batch under this same lock.
		// Waiting with a partially frozen batch could prevent writeback progress.
		for _, p := range pages[:count] {
			if p.state == cacheClean {
				c.clean.remove(p)
			} else {
				c.dirty.remove(p)
			}
			p.state = cacheDiscarding
		}
		c.mu.Unlock()
		// Once frozen, completion cannot be abandoned on cancellation: publish
		// the complete batch, or the existing sticky failure, and wake waiters.
		err := cow.diff.discard(context.WithoutCancel(ctx), first*cowBlockSize, (last-first+1)*cowBlockSize)
		c.mu.Lock()
		var report func(error)
		if err == nil {
			for _, p := range pages[:count] {
				c.releaseLocked(p)
			}
			for block := first; block <= last; block++ {
				cow.markDirty(block)
			}
		} else {
			report = c.failLocked(fmt.Errorf("diffstore: discard blocks %d..%d: %w", first, last, err))
			for _, p := range pages[:count] {
				c.releaseLocked(p)
			}
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

// CacheStats reports shared sandbox totals in bytes, not per-disk budgets.
// Occupancy is charged in logical 4 KiB pages, including payload-free Zero;
// these fields do not measure heap allocation or retained buffer-pool memory.
type CacheStats struct {
	Used, DirtyUsed, Clean, Loading, Writeback uint64
	Capacity, MaxDirty, PeakUsed, PeakDirty    uint64
	WrittenBytes, WriteBatches                 uint64
}

func (c *Cache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := CacheStats{Used: uint64(len(c.pages)) * cowBlockSize, DirtyUsed: uint64(c.dirtyUsed) * cowBlockSize, Capacity: uint64(c.capacity) * cowBlockSize, MaxDirty: uint64(c.maxDirty) * cowBlockSize, PeakUsed: uint64(c.peakUsed) * cowBlockSize, PeakDirty: uint64(c.peakDirty) * cowBlockSize, WrittenBytes: c.writeBytes, WriteBatches: c.writeBatches}
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

var _ io.Closer = (*Cache)(nil)
