package diffstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

// Disk provides a COW (copy-on-write) read-write block view on top
// of an optional read-only base layer plus a local sparse diff file.
//
// The bitmap tracks logical upper presence, published with a complete cached
// plaintext page. Reads prefer latest cached data; only misses reach the diff.
// First partial writes materialize from base/zeros. A shared bounded cache
// asynchronously writes frozen full pages. Block stripes serialize frontend
// access; the worker never acquires them. Reopen loads committed presence;
// fresh owners establish an independent empty bitmap before accepting writes.
type Disk struct {
	opMu      sync.RWMutex // lifetime of frontend operations; never taken by writeback
	lifeCtx   context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
	cache     *Cache
	ownCache  bool
	base      BlockReader // optional; may be nil for no base layer
	diff      diffBackend
	versions  *checkpointDiff
	size      int64
	bitmapMu  sync.RWMutex
	bitmap    []uint64 // each bit = logical presence of one 4 KiB block
	blockMu   []sync.RWMutex
	blockSize int64

	materialMu        sync.Mutex
	materialReaders   sync.WaitGroup
	materialReady     chan struct{}
	materialReadyOnce sync.Once
	materialDone      chan struct{}
	materialErr       error
	materialTemplate  diffTemplateSource
	materialFresh     *freshDiffInit
	materialCancel    context.CancelFunc
	materialDiscard   bool
	discardOnClose    bool
}

const (
	cowBlockSize   = 4096
	cowLockStripes = 256
)

// OpenDisk opens or atomically creates the active diff described by init,
// pairs it with the optional base, and establishes logical upper presence.
// Fresh checkpoint format initialization and publication run asynchronously;
// WaitMaterialized is required before handing off or reopening the final path.
//
// If base is nil, reads to clean blocks return zeros. A supplied base is borrowed:
// the caller keeps it open through Disk.Close and remains responsible for closing it.
func OpenDisk(diffPath string, base BlockReader, init DiffInit, rawOptions ...DiskOption) (*Disk, error) {
	options, err := parseDiskOptions(rawOptions)
	if err != nil {
		return nil, err
	}

	var diff diffBackend
	var fresh *freshDiffInit
	var bitmap []uint64
	if options.checkpoint != nil {
		diff, fresh, err = openCheckpointDiff(diffPath, init, options)
		if err != nil {
			return nil, err
		}
		if init.Existing {
			bitmap, err = diff.scanDirtyBlocks()
			if err != nil {
				return nil, errors.Join(err, diff.Close())
			}
		} else {
			// Fresh stores have no upper blocks. Keep frontend presence separate
			// from committed backend presence without copying known-zero bytes.
			// Templates determine capacity and start materializing below.
			bitmap = make([]uint64, (diff.Size()/cowBlockSize+63)/64)
		}
	} else if !init.Existing && init.TemplatePath != "" {
		fresh, err = createFreshDiffFile(diffPath, init, options.encryption)
		if err != nil {
			return nil, err
		}
		diff = fresh.diff
		numBlocks := diff.Size() / cowBlockSize
		bitmap = make([]uint64, int((numBlocks+63)/64))
	} else {
		diff, err = openSingleDiff(diffPath, init, options)
		if err != nil {
			return nil, err
		}
		bitmap, err = diff.scanDirtyBlocks()
		if err != nil {
			_ = diff.Close()
			return nil, fmt.Errorf("diffstore: rebuild bitmap: %w", err)
		}
	}

	return attachDisk(diff, fresh, bitmap, base, options)
}

// The backend is already owned and, for fresh checkpoint files, its initializer
// already runs. Every failure below joins that owner directly, even before a
// cache or coordinator exists.
func attachDisk(diff diffBackend, fresh *freshDiffInit, bitmap []uint64, base BlockReader, options diskOptions) (*Disk, error) {
	size := diff.Size()
	if base != nil && base.Size() > size {
		err := fmt.Errorf("diffstore: base size %d > diff size %d", base.Size(), size)
		if fresh != nil {
			return nil, errors.Join(err, fresh.abort())
		}
		return nil, errors.Join(err, diff.Close())
	}

	cow := &Disk{
		base:           base,
		diff:           diff,
		size:           size,
		bitmap:         bitmap,
		blockMu:        make([]sync.RWMutex, cowLockStripes),
		blockSize:      cowBlockSize,
		discardOnClose: options.discardOnClose,
	}
	cow.versions, _ = diff.(*checkpointDiff)
	cow.lifeCtx, cow.cancel = context.WithCancel(context.Background())
	cow.cache = options.cache
	if cow.cache == nil {
		var err error
		cow.cache, err = NewCache(DefaultCacheSize, DefaultMaxDirtySize)
		if err != nil {
			cow.cancel()
			if fresh != nil {
				return nil, errors.Join(err, fresh.abort())
			}
			return nil, errors.Join(err, diff.Close())
		}
		cow.ownCache = true
	}
	if err := cow.cache.attach(cow); err != nil {
		cow.cancel()
		if cow.ownCache {
			_ = cow.cache.Close()
		}
		if fresh != nil {
			return nil, errors.Join(err, fresh.abort())
		}
		return nil, errors.Join(err, diff.Close())
	}
	if fresh != nil {
		cow.materialReady = make(chan struct{})
		cow.materialDone = make(chan struct{})
		cow.materialTemplate = fresh.template
		cow.materialFresh = fresh
		// Materialization has its own lifetime. Closing frontend admission must
		// not cancel retained-diff publication; ephemeral teardown explicitly
		// cancels this context through materialCancel.
		ctx, cancel := context.WithCancel(context.Background())
		cow.materialCancel = cancel
		go cow.runMaterializer(ctx)
	}
	return cow, nil
}

// DirtyCount returns the exact number of upper-present blocks, including Zero
// and accepted unflushed writes.
func (c *Disk) DirtyCount() int {
	dirty, _ := c.presenceCounts()
	return dirty
}

func (c *Disk) presenceCounts() (dirty, known int) {
	c.bitmapMu.RLock()
	defer c.bitmapMu.RUnlock()
	for _, w := range c.bitmap {
		dirty += popcount64(w)
	}
	known = int(c.size / c.blockSize)
	return dirty, known
}

// BackendStats reports cow-specific metrics. Implements StatsReporter.
// Counts include Zero and accepted unflushed writes; presence is complete at open.
func (c *Disk) BackendStats() map[string]any {
	dirty, known := c.presenceCounts()
	total := c.size / c.blockSize
	stats := map[string]any{
		"diff_dirty_blocks":          dirty,
		"diff_cow_cache":             c.cache.Stats(),
		"diff_total_blocks":          total,
		"diff_block_bytes":           c.blockSize,
		"diff_dirty_percent":         fmt.Sprintf("%.2f", float64(dirty)*100/float64(total)),
		"diff_presence_known_blocks": known,
		"diff_presence_complete":     int64(known) == total,
	}
	if d, err := c.checkpointBackend(); err == nil {
		stats["diff_checkpoint_store"] = d.store.Stats()
	}
	return stats
}

func popcount64(x uint64) int {
	// Hamming weight; tiny helper to avoid importing math/bits here.
	count := 0
	for x != 0 {
		x &= x - 1
		count++
	}
	return count
}

// blockDirty checks whether a given 4K block index is dirty.
func (c *Disk) blockDirty(blk int64) bool {
	c.bitmapMu.RLock()
	defer c.bitmapMu.RUnlock()
	return c.bitmap[blk/64]&(1<<(uint64(blk)%64)) != 0
}

// markDirty sets the bit for blk.
func (c *Disk) markDirty(blk int64) {
	c.bitmapMu.Lock()
	defer c.bitmapMu.Unlock()
	c.bitmap[blk/64] |= 1 << (uint64(blk) % 64)
}

func (c *Disk) blockLock(blk int64) *sync.RWMutex {
	return &c.blockMu[uint64(blk)%uint64(len(c.blockMu))]
}

// ReadAt reads len(buf) bytes starting at offset, routing per-block
// reads to either the diff file or the base reader.
func (c *Disk) ReadAt(buf []byte, offset int64) (int, error) {
	return c.ReadAtContext(nil, buf, offset)
}

func (c *Disk) ReadAtContext(ctx context.Context, buf []byte, offset int64) (int, error) {
	ctx, endOp, err := c.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer c.end(endOp)
	if len(buf) == 0 {
		if offset < 0 || offset > c.size {
			return 0, io.EOF
		}
		return 0, nil
	}
	if offset < 0 || offset >= c.size {
		return 0, io.EOF
	}
	available := c.size - offset
	if int64(len(buf)) > available {
		buf = buf[:c.size-offset]
	}
	return c.readRange(ctx, buf, offset, nil)
}

// readRange is shared by live and snapshot upper reads. Lock bounded ranges in
// numeric stripe order once, before inspecting presence. No recursive RLock or
// cache/global lock spans I/O; the worker never needs a frontend stripe.
// A non-nil bitmap denotes the snapshot's upper-only view.
func (c *Disk) readRange(ctx context.Context, buf []byte, offset int64, bitmap []uint64) (int, error) {
	done := 0
	for done < len(buf) {
		pos := offset + int64(done)
		length := min(len(buf)-done, maxDiffScratchSize-int(pos%cowBlockSize))
		first, last := pos/cowBlockSize, (pos+int64(length)-1)/cowBlockSize
		c.rlockBlockRange(first, last)
		n, err := c.readRangeLocked(ctx, buf[done:done+length], pos, bitmap)
		c.runlockBlockRange(first, last)
		done += n
		if err != nil {
			return done, c.propagateBackendFailure(err)
		}
	}
	return done, nil
}

func (c *Disk) readRangeLocked(ctx context.Context, buf []byte, offset int64, bitmap []uint64) (int, error) {

	done := 0
	end := offset + int64(len(buf))
	for done < len(buf) {
		pos := offset + int64(done)
		block := pos / cowBlockSize
		runEnd := min((block+1)*cowBlockSize, end)
		bits := bitmap
		if bitmap == nil {
			c.bitmapMu.RLock()
			bits = c.bitmap
		}
		upper := bitmapBlockDirty(bits, block)
		// Both upper and base runs stay within the requested, stripe-locked
		// range. Let the immutable reader choose its own lower I/O boundaries.
		for runEnd < end && bitmapBlockDirty(bits, runEnd/cowBlockSize) == upper {
			runEnd = min(runEnd+cowBlockSize, end)
		}
		if bitmap == nil {
			c.bitmapMu.RUnlock()
		}
		chunk := buf[done : done+int(runEnd-pos)]
		if upper {
			n, err := c.cache.read(ctx, c, chunk, pos)
			done += n
			if err != nil {
				return done, err
			}
		} else {
			if err := ctx.Err(); err != nil {
				return done, err
			}
			if bitmap == nil {
				c.materialMu.Lock()
				materializing := c.materialTemplate != nil
				c.materialMu.Unlock()
				if materializing {
					n, err := c.readLower(ctx, chunk, pos)
					if err != nil {
						return done + n, err
					}
					zeroSlice(chunk[n:])
				} else if c.base != nil && pos < c.base.Size() {
					n, err := readBlock(ctx, c.base, chunk, pos)
					if readretry.IsTerminal(err) || (err != nil && !errors.Is(err, io.EOF)) {
						return done + n, err
					}
					zeroSlice(chunk[n:])
				} else {
					zeroSlice(chunk)
				}
			} else {
				zeroSlice(chunk)
			}
			done += len(chunk)
		}
	}
	return done, nil
}

// WriteAt accepts complete plaintext pages into the bounded cache. First
// partial writes preserve base/zero bytes before publishing upper presence.
func (c *Disk) WriteAt(buf []byte, offset int64) (int, error) {
	return c.WriteAtContext(nil, buf, offset)
}

func (c *Disk) WriteAtContext(ctx context.Context, buf []byte, offset int64) (int, error) {
	ctx, endOp, err := c.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer c.end(endOp)
	if offset < 0 || offset > c.size || int64(len(buf)) > c.size-offset {
		return 0, fmt.Errorf("diffstore: write out of bounds: offset=%d len=%d size=%d", offset, len(buf), c.size)
	}
	if len(buf) != 0 {
		if err := c.markModified(ctx); err != nil {
			return 0, err
		}
	}
	written := 0
	for written < len(buf) {
		pos := offset + int64(written)
		blk := pos / c.blockSize
		blkEnd := (blk + 1) * c.blockSize
		chunkLen := len(buf) - written
		if remaining := blkEnd - pos; int64(chunkLen) > remaining {
			chunkLen = int(remaining)
		}

		lock := c.blockLock(blk)
		lock.Lock()
		n, err := c.writeBlockLocked(ctx, buf[written:written+chunkLen], pos, blk)
		lock.Unlock()
		if retry, ok := err.(*cacheRetry); ok {
			select {
			case <-retry.wake:
				continue
			case <-ctx.Done():
				return written, ctx.Err()
			}
		}
		written += n
		if err != nil {
			return written, c.propagateBackendFailure(err)
		}
	}
	return written, nil
}

// writeBlockLocked fills a quota-reserved page, then publishes upper presence.
// The worker never takes the caller's block stripe, including while it waits.
func (c *Disk) writeBlockLocked(ctx context.Context, buf []byte, offset, blk int64) (int, error) {
	start := blk * c.blockSize

	upper := c.blockDirty(blk)
	return c.cache.write(ctx, c, buf, offset, blk, upper, func(page []byte) error {
		if offset == start && int64(len(buf)) == c.blockSize {
			return nil
		}
		if upper {
			return readFullAt(c.diff, page, start)
		}
		n, err := c.readLower(ctx, page, start)
		if err != nil {
			return fmt.Errorf("diffstore: materialize block %d from lower: %w", blk, err)
		}
		if n != len(page) {
			return fmt.Errorf("diffstore: materialize block %d from lower: %w", blk, io.ErrUnexpectedEOF)
		}
		return nil
	})
}

func (c *Disk) readLower(ctx context.Context, buf []byte, offset int64) (int, error) {
	done := 0
	c.materialMu.Lock()
	template := c.materialTemplate
	if template != nil {
		c.materialReaders.Add(1)
	}
	c.materialMu.Unlock()
	if template != nil {
		defer c.materialReaders.Done()
	}
	for done < len(buf) {
		pos := offset + int64(done)
		length := min(len(buf)-done, int(c.blockSize-pos%c.blockSize))
		chunk := buf[done : done+length]
		usedTemplate := false
		if template != nil && pos < int64(template.Size()) {
			run, err := template.RunAt(uint64(pos), uint64(length))
			if err != nil && !errors.Is(err, io.EOF) {
				return done, err
			}
			if err == nil {
				if run.End() <= uint64(pos) || run.End() > uint64(pos)+uint64(length) {
					return done, fmt.Errorf("diffstore: invalid template run boundary")
				}
				length = int(run.End() - uint64(pos))
				chunk = chunk[:length]
				if run.Kind() == sparse.Zero {
					clear(chunk)
					done += length
					continue
				}
			}
			if err == nil && run.Kind() != sparse.Hole {
				n, readErr := template.ReadAt(ctx, chunk, uint64(pos))
				done += n
				if readErr != nil && !(errors.Is(readErr, io.EOF) && n == len(chunk)) {
					return done, readErr
				}
				if n != len(chunk) {
					return done, io.ErrUnexpectedEOF
				}
				usedTemplate = true
			}
		}
		if usedTemplate {
			continue
		}
		if c.base != nil && pos < c.base.Size() {
			readLen := min(int64(length), c.base.Size()-pos)
			n, err := readBlock(ctx, c.base, chunk[:readLen], pos)
			if readretry.IsTerminal(err) || (err != nil && !errors.Is(err, io.EOF)) {
				return done + n, err
			}
			zeroSlice(chunk[n:])
		} else {
			zeroSlice(chunk)
		}
		done += len(chunk)
	}
	return done, nil
}

func (c *Disk) signalMaterialReady() {
	c.materialReadyOnce.Do(func() {
		if c.materialReady != nil {
			close(c.materialReady)
		}
	})
}

func (c *Disk) WaitMaterialReady(ctx context.Context) error {
	c.materialMu.Lock()
	ready := c.materialReady
	err := c.materialErr
	c.materialMu.Unlock()
	if ready == nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ready:
		c.materialMu.Lock()
		defer c.materialMu.Unlock()
		return c.materialErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Disk) runMaterializer(ctx context.Context) {
	err := c.materializeTemplate(ctx)
	c.materialMu.Lock()
	c.materialErr = err
	done := c.materialDone
	c.materialMu.Unlock()
	// On failure keep lower readers and private resources until Close has
	// stopped admission and joined callers that already passed the health check.
	// Keep the cache gate through completion: late handler installation cannot
	// replay this failure before a handler can safely WaitMaterialized.
	var report func(error)
	c.cache.mu.Lock()
	if err != nil && !errors.Is(err, errMaterialDiscard) {
		report = c.cache.failLocked(fmt.Errorf("diffstore: initialize/materialize diff: %w", err))
	}
	c.signalMaterialReady()
	if done != nil {
		close(done)
	}
	c.cache.mu.Unlock()
	if report != nil {
		report(err)
	}
}

var errMaterialDiscard = fmt.Errorf("diffstore: disposable materialization stopped: %w", context.Canceled)

func (c *Disk) materializeTemplate(ctx context.Context) error {
	c.materialMu.Lock()
	template := c.materialTemplate
	fresh := c.materialFresh
	c.materialMu.Unlock()
	if fresh == nil {
		return nil
	}
	// Never hold frontend stripes while waiting. Initialization belongs to the
	// backend and cannot be canceled by ordinary teardown or a request timeout.
	if err := c.diff.waitInitialized(context.Background()); err != nil {
		return err
	}
	c.cache.mu.Lock()
	c.cache.signalLocked()
	c.cache.kickLocked(true)
	c.cache.mu.Unlock()
	if ctx.Err() != nil {
		return errMaterialDiscard
	}
	if template != nil {
		if err := c.materializeContents(ctx, template); err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return errMaterialDiscard
			}
			return err
		}
		// Detach only after every template page is owned by committed or
		// cached upper state. Existing readers retain their borrowed source.
		c.materialMu.Lock()
		c.materialTemplate = nil
		c.materialMu.Unlock()
		c.materialReaders.Wait()
		if err := template.Close(); err != nil {
			return fmt.Errorf("close materialized diff template: %w", err)
		}
		fresh.template = nil
	}
	c.signalMaterialReady()
	if err := c.cache.drain(ctx, c); err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return errMaterialDiscard
		}
		return err
	}
	// Publication is admitted under the teardown gate. Once admitted, preserve
	// its real sync/rename/dirsync result even if Close requests disposal.
	c.materialMu.Lock()
	discard := c.materialDiscard
	c.materialMu.Unlock()
	if discard {
		return errMaterialDiscard
	}
	return fresh.publish()
}

func (c *Disk) materializeContents(ctx context.Context, template diffTemplateSource) error {
	staging := make([]byte, c.diff.policy().materialPages*cowBlockSize)
	defer clear(staging)
	for offset := uint64(0); offset < template.Size(); {
		if err := ctx.Err(); err != nil {
			return err
		}
		run, err := template.RunAt(offset, template.Size()-offset)
		if err != nil {
			return err
		}
		end := run.End()
		if end <= offset || end > template.Size() {
			return fmt.Errorf("invalid diff template sparse run")
		}
		if run.Kind() != sparse.Hole {
			start := uint64(int64(offset) / cowBlockSize * cowBlockSize)
			seedEnd := uint64(alignUp(int64(end), cowBlockSize))
			if run.Kind() == sparse.Zero && offset%cowBlockSize == 0 && end%cowBlockSize == 0 {
				err = c.materializeZero(ctx, start, seedEnd)
			} else {
				err = c.materializeRange(ctx, template, start, seedEnd, staging)
			}
			if err != nil {
				return err
			}
		}
		offset = end
	}

	return nil
}

func (c *Disk) WaitMaterialized(ctx context.Context) error {
	c.materialMu.Lock()
	done := c.materialDone
	err := c.materialErr
	c.materialMu.Unlock()
	if done == nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
		c.materialMu.Lock()
		defer c.materialMu.Unlock()
		return c.materialErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Flush is the project's non-durable guest FLUSH: health check only, with no
// writeback wakeup, Drain or filesystem sync. Wire features remain unchanged.
func (c *Disk) Flush() error { return c.Err() }
func (c *Disk) Err() error {
	if err := c.cache.Err(); err != nil {
		return err
	}
	if err := c.diff.err(); err != nil {
		return err
	}
	return c.lifeCtx.Err()
}
func (c *Disk) SetFatalHandler(report func(error)) { c.cache.SetFatalHandler(report) }

// Drain is an internal completion boundary, not a guest durability operation.
func (c *Disk) Drain(ctx context.Context) error { return c.cache.drain(ctx, c) }

// Discard releases active payload for fully covered pages and publishes Zero,
// masking the lower layer. Retained versions keep their protected payload.
// Partial edge pages retain their original contents.
func (c *Disk) Discard(offset, length int64) error {
	ctx, endOp, err := c.begin(nil)
	if err != nil {
		return err
	}
	defer c.end(endOp)
	if offset < 0 || offset > c.size || length < 0 || length > c.size-offset {
		return fmt.Errorf("diffstore: discard out of bounds: off=%d len=%d size=%d",
			offset, length, c.size)
	}
	if length == 0 {
		return nil
	}

	// Punch only full blocks. Partial blocks at the edges retain their
	// existing contents — discarding them would corrupt data the guest
	// hasn't asked to drop.
	blockSize := c.blockSize
	startBlk := (offset + blockSize - 1) / blockSize
	endBlk := (offset + length) / blockSize
	if startBlk >= endBlk {
		return nil
	}
	if err := c.diff.waitInitialized(ctx); err != nil {
		return err
	}

	if err := c.markModified(ctx); err != nil {
		return err
	}
	for first := startBlk; first < endBlk; first += maxDiscardPages {
		last := min(first+maxDiscardPages, endBlk) - 1
		c.lockBlockRange(first, last)
		err := c.cache.discard(ctx, c, first, last)
		c.unlockBlockRange(first, last)
		if err != nil {
			return err
		}
	}

	return nil
}

const maxDiscardPages = 64

// Discard uses at most 64 pages, but can wrap around the stripe array. Lock
// each touched stripe once in the same numeric order as multi-page readers.
func (c *Disk) lockBlockRange(first, last int64) {
	for stripe := range c.blockMu {
		if delta := (int64(stripe) - first%int64(len(c.blockMu)) + int64(len(c.blockMu))) % int64(len(c.blockMu)); delta <= last-first {
			c.blockMu[stripe].Lock()
		}
	}
}

func (c *Disk) unlockBlockRange(first, last int64) {
	for stripe := len(c.blockMu) - 1; stripe >= 0; stripe-- {
		if delta := (int64(stripe) - first%int64(len(c.blockMu)) + int64(len(c.blockMu))) % int64(len(c.blockMu)); delta <= last-first {
			c.blockMu[stripe].Unlock()
		}
	}
}

// SnapshotView returns a read-only, upper-only view at the current logical
// upper-present bitmap. Present blocks expose complete latest plaintext, including
// unflushed cache pages. Absent blocks are holes and defensively read as zeros
// rather than falling through to base. Keep the Disk open while using the view.
// Snapshot orchestration calls this after quiescing every vhost backend, so the
// dirty block contents stay stable for the view's lifetime.
func (c *Disk) SnapshotView() (io.ReadSeeker, []sparse.Extent, error) {
	view, err := c.activeView(context.Background())
	if err != nil {
		return nil, nil, err
	}
	return io.NewSectionReader(view, 0, c.size), snapshotHoles(view.bitmap, c.size), nil
}

func (c *Disk) activeView(ctx context.Context) (*cowSnapshotReaderAt, error) {
	if err := c.WaitMaterialized(ctx); err != nil {
		return nil, err
	}
	ctx, endOp, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer c.end(endOp)

	c.bitmapMu.RLock()
	bitmap := append([]uint64(nil), c.bitmap...)
	c.bitmapMu.RUnlock()

	return &cowSnapshotReaderAt{cow: c, bitmap: bitmap}, nil
}

type cowSnapshotReaderAt struct {
	cow    *Disk
	bitmap []uint64
}

func (r *cowSnapshotReaderAt) ReadAt(buf []byte, offset int64) (int, error) {
	ctx, endOp, err := r.cow.begin(nil)
	if err != nil {
		return 0, err
	}
	defer r.cow.end(endOp)
	if len(buf) == 0 {
		if offset < 0 || offset > r.cow.size {
			return 0, io.EOF
		}
		return 0, nil
	}
	if offset < 0 || offset >= r.cow.size {
		return 0, io.EOF
	}
	n := len(buf)
	var eof error
	if int64(n) > r.cow.size-offset {
		n = int(r.cow.size - offset)
		eof = io.EOF
	}

	written, err := r.cow.readRange(ctx, buf[:n], offset, r.bitmap)
	if err != nil {
		return written, err
	}
	if err := r.cow.Err(); err != nil {
		return written, err
	}
	return written, eof
}

// rlockBlockRange locks each stripe touched by the inclusive block range once,
// in numeric stripe order, without recursively taking an RWMutex when a range
// spans more than one full stripe cycle.
func (c *Disk) rlockBlockRange(first, last int64) {
	stripeCount := int64(len(c.blockMu))
	blockCount := last - first + 1
	firstStripe := first % stripeCount
	if blockCount >= stripeCount {
		for stripe := int64(0); stripe < stripeCount; stripe++ {
			c.blockMu[stripe].RLock()
		}
		return
	}
	end := firstStripe + blockCount
	if end <= stripeCount {
		for stripe := firstStripe; stripe < end; stripe++ {
			c.blockMu[stripe].RLock()
		}
		return
	}
	for stripe := int64(0); stripe < end-stripeCount; stripe++ {
		c.blockMu[stripe].RLock()
	}
	for stripe := firstStripe; stripe < stripeCount; stripe++ {
		c.blockMu[stripe].RLock()
	}
}

func (c *Disk) runlockBlockRange(first, last int64) {
	stripeCount := int64(len(c.blockMu))
	blockCount := last - first + 1
	firstStripe := first % stripeCount
	if blockCount >= stripeCount {
		for stripe := stripeCount - 1; stripe >= 0; stripe-- {
			c.blockMu[stripe].RUnlock()
		}
		return
	}
	end := firstStripe + blockCount
	if end <= stripeCount {
		for stripe := end - 1; stripe >= firstStripe; stripe-- {
			c.blockMu[stripe].RUnlock()
		}
		return
	}
	for stripe := stripeCount - 1; stripe >= firstStripe; stripe-- {
		c.blockMu[stripe].RUnlock()
	}
	for stripe := end - stripeCount - 1; stripe >= 0; stripe-- {
		c.blockMu[stripe].RUnlock()
	}
}

func bitmapBlockDirty(bitmap []uint64, blk int64) bool {
	return bitmap[blk/64]&(1<<(uint64(blk)%64)) != 0
}

// bitmapRunEnd finds a change of presence in [block, limit). The caller supplies
// a nonempty range within the bitmap's logical capacity; padding bits are ignored.
func bitmapRunEnd(bitmap []uint64, block, limit uint64) uint64 {
	var invert uint64
	if bitmapBlockDirty(bitmap, int64(block)) {
		invert = ^uint64(0)
	}
	for block < limit {
		base := block &^ 63
		changed := (bitmap[block/64] ^ invert) & (^uint64(0) << (block % 64))
		if changed != 0 {
			return min(limit, base+uint64(bits.TrailingZeros64(changed)))
		}
		block = base + 64
	}
	return limit
}

func snapshotHoles(bitmap []uint64, size int64) []sparse.Extent {
	numBlocks := uint64(size / cowBlockSize)
	var holes []sparse.Extent
	for block := uint64(0); block < numBlocks; {
		end := bitmapRunEnd(bitmap, block, numBlocks)
		if !bitmapBlockDirty(bitmap, int64(block)) {
			holes = append(holes, sparse.Extent{
				Offset: block * cowBlockSize,
				Size:   (end - block) * cowBlockSize,
			})
		}
		block = end
	}
	return holes
}

// Size returns the visible block-device size in bytes.
func (c *Disk) Size() int64 { return c.size }

// begin joins frontend lifetime and cancellation without a payload goroutine.
// AfterFunc only runs on Close; runtime queue cancellation remains independent
// from the cache worker, which must drain healthy accepted writes.
func (c *Disk) begin(ctx context.Context) (context.Context, func(), error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	c.opMu.RLock()
	if err := c.Err(); err != nil {
		c.opMu.RUnlock()
		return nil, nil, err
	}
	if ctx == nil || ctx == context.Background() || ctx == context.TODO() {
		return c.lifeCtx, nil, nil
	}
	merged, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.lifeCtx, cancel)
	return merged, func() { stop(); cancel() }, nil
}

func (c *Disk) end(cancel func()) {
	if cancel != nil {
		cancel()
	}
	c.opMu.RUnlock()
}

// Close stops admission, joins initialization and required fresh publication,
// and drains accepted writes before releasing plaintext and the FD. Already
// published disks get no additional fsync. Base is closed by its owner.
func (c *Disk) Close() error {
	c.closeOnce.Do(func() {
		// Stop frontend admission first. The materializer does not enter through
		// begin(), so a retained diff can still finish drain/sync/publication
		// after all accepted frontend operations have crossed this barrier.
		if c.materialCancel != nil && c.discardOnClose {
			c.materialMu.Lock()
			c.materialDiscard = true
			c.materialCancel()
			c.materialMu.Unlock()
		}
		c.cancel()
		c.opMu.Lock()
		c.opMu.Unlock()

		if c.materialCancel != nil {
			if err := c.WaitMaterialized(context.Background()); !errors.Is(err, errMaterialDiscard) {
				c.closeErr = errors.Join(c.closeErr, err)
			}
			c.materialCancel()
		}
		c.closeErr = errors.Join(c.closeErr, c.cache.drain(context.Background(), c))
		c.cache.detach(c)
		if c.ownCache {
			c.closeErr = errors.Join(c.closeErr, c.cache.Close())
		}
		c.closeErr = errors.Join(c.closeErr, c.diff.Close())
		c.materialReaders.Wait()
		if c.materialFresh != nil {
			c.closeErr = errors.Join(c.closeErr, c.materialFresh.failMaterialization())
		}
	})
	return c.closeErr
}

func zeroSlice(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// CacheStats reports the shared cache budget and occupancy.
func (c *Disk) CacheStats() CacheStats { return c.cache.Stats() }
