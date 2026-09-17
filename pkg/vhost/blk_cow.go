package vhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

// BlockCOW provides a COW (copy-on-write) read-write block view on top
// of an optional read-only base layer plus a local sparse diff file.
//
// The bitmap tracks logical upper presence, published with a complete cached
// plaintext page. Reads prefer latest cached data; only misses reach the diff.
// First partial writes materialize from base/zeros. A shared bounded cache
// asynchronously writes frozen full pages. Block stripes serialize frontend
// access; the worker never acquires them. Reopen rebuilds presence from extents.
//
// The vhost profile does not advertise DISCARD or WRITE_ZEROES and rejects
// those wire requests. The low-level Discard helper has hole semantics below.
type BlockCOW struct {
	opMu      sync.RWMutex // lifetime of frontend operations; never taken by writeback
	lifeCtx   context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
	cache     *COWCache
	ownCache  bool
	base      BlockReader // optional; may be nil for no base layer
	diff      *diffFile
	size      int64
	bitmapMu  sync.RWMutex
	bitmap    []uint64 // each bit = one 4K block
	blockMu   []sync.RWMutex
	blockSize int64
}

const (
	cowBlockSize   = 4096
	cowLockStripes = 256
)

// OpenBlockCOW opens or atomically creates the active diff described by init,
// rebuilds logical upper presence from its body, and pairs it with the optional base.
//
// If base is nil, reads to clean blocks return zeros.
func OpenBlockCOW(diffPath string, base BlockReader, init DiffInit, rawOptions ...BlockCOWOption) (*BlockCOW, error) {
	options, err := parseBlockCOWOptions(rawOptions)
	if err != nil {
		return nil, err
	}
	diff, err := openBlockCOWDiff(diffPath, init, options)
	if err != nil {
		return nil, err
	}
	size := diff.logicalSize
	if base != nil && base.Size() > size {
		_ = diff.Close()
		return nil, fmt.Errorf("vhost: base size %d > diff size %d", base.Size(), size)
	}
	bitmap, err := diff.scanDirtyBlocks()
	if err != nil {
		_ = diff.Close()
		return nil, fmt.Errorf("vhost: rebuild bitmap: %w", err)
	}

	cow := &BlockCOW{
		base:      base,
		diff:      diff,
		size:      size,
		bitmap:    bitmap,
		blockMu:   make([]sync.RWMutex, cowLockStripes),
		blockSize: cowBlockSize,
	}
	cow.lifeCtx, cow.cancel = context.WithCancel(context.Background())
	cow.cache = options.cache
	if cow.cache == nil {
		cow.cache, err = NewCOWCache(DefaultCOWCacheSize, DefaultCOWMaxDirtySize)
		if err != nil {
			cow.cancel()
			return nil, errors.Join(err, diff.Close())
		}
		cow.ownCache = true
	}
	if err := cow.cache.attach(cow); err != nil {
		cow.cancel()
		if cow.ownCache {
			_ = cow.cache.Close()
		}
		return nil, errors.Join(err, diff.Close())
	}
	return cow, nil
}

// dirtyCount returns the number of blocks marked dirty (test helper).
func (c *BlockCOW) DirtyCount() int {
	c.bitmapMu.RLock()
	defer c.bitmapMu.RUnlock()
	count := 0
	for _, w := range c.bitmap {
		count += popcount64(w)
	}
	return count
}

// BackendStats reports cow-specific metrics. Implements StatsReporter.
// The dirty-block count includes blocks already present in the diff
// file when the server started (rebuilt from SEEK_DATA), so it tracks
// the logical upper-layer footprint, including accepted unflushed writes.
func (c *BlockCOW) BackendStats() map[string]any {
	dirty := c.DirtyCount()
	total := c.size / c.blockSize
	return map[string]any{
		"diff_dirty_blocks":  dirty,
		"diff_cow_cache":     c.cache.Stats(),
		"diff_total_blocks":  total,
		"diff_block_bytes":   c.blockSize,
		"diff_dirty_percent": fmt.Sprintf("%.2f", float64(dirty)*100/float64(total)),
	}
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
func (c *BlockCOW) blockDirty(blk int64) bool {
	c.bitmapMu.RLock()
	defer c.bitmapMu.RUnlock()
	return c.bitmap[blk/64]&(1<<(uint64(blk)%64)) != 0
}

// markDirty sets the bit for blk.
func (c *BlockCOW) markDirty(blk int64) {
	c.bitmapMu.Lock()
	defer c.bitmapMu.Unlock()
	c.bitmap[blk/64] |= 1 << (uint64(blk) % 64)
}

func (c *BlockCOW) blockLock(blk int64) *sync.RWMutex {
	return &c.blockMu[uint64(blk)%uint64(len(c.blockMu))]
}

// ReadAt reads len(buf) bytes starting at offset, routing per-block
// reads to either the diff file or the base reader.
func (c *BlockCOW) ReadAt(buf []byte, offset int64) (int, error) {
	return c.readAt(nil, buf, offset)
}

func (c *BlockCOW) readAt(ctx context.Context, buf []byte, offset int64) (int, error) {
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
func (c *BlockCOW) readRange(ctx context.Context, buf []byte, offset int64, bitmap []uint64) (int, error) {
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
			return done, err
		}
	}
	return done, nil
}

func (c *BlockCOW) readRangeLocked(ctx context.Context, buf []byte, offset int64, bitmap []uint64) (int, error) {
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
		if upper {
			for runEnd < end && bitmapBlockDirty(bits, runEnd/cowBlockSize) {
				runEnd = min(runEnd+cowBlockSize, end)
			}
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
			if bitmap == nil && c.base != nil && pos < c.base.Size() {
				n, err := readBlock(ctx, c.base, chunk, pos)
				if readretry.IsTerminal(err) || (err != nil && !errors.Is(err, io.EOF)) {
					return done + n, err
				}
				zeroSlice(chunk[n:])
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
func (c *BlockCOW) WriteAt(buf []byte, offset int64) (int, error) {
	return c.writeAt(nil, buf, offset)
}

func (c *BlockCOW) writeAt(ctx context.Context, buf []byte, offset int64) (int, error) {
	ctx, endOp, err := c.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer c.end(endOp)
	if offset < 0 || offset > c.size || int64(len(buf)) > c.size-offset {
		return 0, fmt.Errorf("vhost: write out of bounds: offset=%d len=%d size=%d", offset, len(buf), c.size)
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
			return written, err
		}
	}
	return written, nil
}

// writeBlockLocked fills a quota-reserved page, then publishes upper presence.
// The worker never takes the caller's block stripe, including while it waits.
func (c *BlockCOW) writeBlockLocked(ctx context.Context, buf []byte, offset, blk int64) (int, error) {
	upper := c.blockDirty(blk)
	start := blk * c.blockSize
	return c.cache.write(ctx, c, buf, offset, blk, upper, func(page []byte) error {
		if offset == start && int64(len(buf)) == c.blockSize {
			return nil
		}
		if upper {
			return readFullAt(c.diff, page, start)
		}
		if c.base == nil || start >= c.base.Size() {
			return nil
		}
		readLen := min(c.blockSize, c.base.Size()-start)
		n, err := readBlock(ctx, c.base, page[:readLen], start)
		if readretry.IsTerminal(err) || (err != nil && !errors.Is(err, io.EOF)) {
			return fmt.Errorf("vhost: materialize block %d from base: %w", blk, err)
		}
		if int64(n) != readLen {
			return fmt.Errorf("vhost: materialize block %d from base: %w", blk, io.ErrUnexpectedEOF)
		}
		return nil
	})
}

// Flush is the project's non-durable guest FLUSH: health check only, with no
// writeback wakeup, Drain or filesystem sync. Wire features remain unchanged.
func (c *BlockCOW) Flush() error { return c.Err() }
func (c *BlockCOW) Err() error {
	if err := c.cache.Err(); err != nil {
		return err
	}
	return c.lifeCtx.Err()
}
func (c *BlockCOW) SetFatalHandler(report func(error)) { c.cache.SetFatalHandler(report) }

// Drain is an internal completion boundary, not a guest durability operation.
func (c *BlockCOW) Drain(ctx context.Context) error { return c.cache.drain(ctx, c) }

// Discard punches complete blocks in the diff and clears their dirty bits.
// Subsequent reads fall through to the base, or return zeros without a base;
// this helper does not create an explicit zero that masks lower-layer data.
// Partial edge blocks retain their contents. The current vhost dispatcher
// does not call this helper: DISCARD and WRITE_ZEROES requests are unsupported.
func (c *BlockCOW) Discard(offset, length int64) error {
	ctx, endOp, err := c.begin(nil)
	if err != nil {
		return err
	}
	defer c.end(endOp)
	if offset < 0 || offset > c.size || length < 0 || length > c.size-offset {
		return fmt.Errorf("vhost: discard out of bounds: off=%d len=%d size=%d",
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

	for blk := startBlk; blk < endBlk; blk++ {
		lock := c.blockLock(blk)
		lock.Lock()
		err := c.cache.discard(ctx, c, blk)
		lock.Unlock()
		if err != nil {
			return err
		}
	}

	return nil
}

// SnapshotView returns a read-only, upper-only view at the current logical
// upper-present bitmap. Present blocks expose complete latest plaintext, including
// unflushed cache pages. Absent blocks are holes and defensively read as zeros
// rather than falling through to base. Keep the BlockCOW open while using the view.
// Snapshot orchestration calls this after quiescing every vhost backend, so the
// dirty block contents stay stable for the view's lifetime.
func (c *BlockCOW) SnapshotView() (io.ReadSeeker, []sparse.Extent, error) {
	_, endOp, err := c.begin(nil)
	if err != nil {
		return nil, nil, err
	}
	defer c.end(endOp)
	c.bitmapMu.RLock()
	bitmap := append([]uint64(nil), c.bitmap...)
	c.bitmapMu.RUnlock()

	view := &cowSnapshotReaderAt{cow: c, bitmap: bitmap}
	return io.NewSectionReader(view, 0, c.size), snapshotHoles(bitmap, c.size), nil
}

type cowSnapshotReaderAt struct {
	cow    *BlockCOW
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
func (c *BlockCOW) rlockBlockRange(first, last int64) {
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

func (c *BlockCOW) runlockBlockRange(first, last int64) {
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

func snapshotHoles(bitmap []uint64, size int64) []sparse.Extent {
	numBlocks := size / cowBlockSize
	var holes []sparse.Extent
	for blk := int64(0); blk < numBlocks; {
		if bitmapBlockDirty(bitmap, blk) {
			blk++
			continue
		}
		start := blk
		for blk < numBlocks && !bitmapBlockDirty(bitmap, blk) {
			blk++
		}
		holes = append(holes, sparse.Extent{
			Offset: uint64(start * cowBlockSize),
			Size:   uint64((blk - start) * cowBlockSize),
		})
	}
	return holes
}

// Size returns the visible block-device size in bytes.
func (c *BlockCOW) Size() int64 { return c.size }

// begin joins frontend lifetime and cancellation without a payload goroutine.
// AfterFunc only runs on Close; runtime queue cancellation remains independent
// from the cache worker, which must drain healthy accepted writes.
func (c *BlockCOW) begin(ctx context.Context) (context.Context, func(), error) {
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

func (c *BlockCOW) end(cancel func()) {
	if cancel != nil {
		cancel()
	}
	c.opMu.RUnlock()
}

// Close stops admission, drains accepted writes without fsync, and waits for
// real I/O before releasing plaintext and the FD. Base is closed by its owner.
func (c *BlockCOW) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		c.opMu.Lock()
		c.opMu.Unlock() // canceled admission makes this a lifetime barrier only
		c.closeErr = c.cache.drain(context.Background(), c)
		c.cache.detach(c)
		if c.ownCache {
			c.closeErr = errors.Join(c.closeErr, c.cache.Close())
		}
		c.closeErr = errors.Join(c.closeErr, c.diff.Close())
	})
	return c.closeErr
}

func zeroSlice(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
