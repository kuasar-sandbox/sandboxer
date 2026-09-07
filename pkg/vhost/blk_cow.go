package vhost

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// BlockCOW provides a COW (copy-on-write) read-write block view on top
// of an optional read-only base layer plus a local sparse diff file.
//
// Read path: each 4K block is checked against the in-memory dirty bitmap.
// If dirty (i.e. ever written since this backend started), pread from
// diff. Otherwise, pread from base; if base is nil, return zeros.
//
// Write path: a first partial write materializes the complete 4K block from
// base (or zeros), merges the update, writes the complete block, then marks it
// dirty. Reads and writes to the same block share a striped RWMutex.
//
// At backend startup, we rebuild the bitmap from diff file by walking
// SEEK_DATA / SEEK_HOLE — any byte range marked as data in the diff is
// considered dirty (i.e. came from a previous write to this diff).
//
// The vhost profile does not advertise DISCARD or WRITE_ZEROES and rejects
// those wire requests. The low-level Discard helper has hole semantics below.
type BlockCOW struct {
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
// builds the dirty bitmap from its body, and pairs it with the optional base.
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
// the persisted upper-layer footprint, not just this-run writes.
func (c *BlockCOW) BackendStats() map[string]any {
	dirty := c.DirtyCount()
	total := c.size / c.blockSize
	return map[string]any{
		"diff_dirty_blocks":  dirty,
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
	end := offset + int64(len(buf))

	pos := int64(0)
	for offset+pos < end {
		blk := (offset + pos) / c.blockSize
		// How much of this block remains.
		blkEnd := (blk + 1) * c.blockSize
		if blkEnd > end {
			blkEnd = end
		}
		chunk := buf[pos : pos+(blkEnd-(offset+pos))]
		lock := c.blockLock(blk)
		lock.RLock()
		if c.blockDirty(blk) {
			if _, err := c.diff.ReadAt(chunk, offset+pos); err != nil && !errors.Is(err, io.EOF) {
				lock.RUnlock()
				return int(pos), err
			}
		} else if c.base != nil && offset+pos < c.base.Size() {
			n, err := c.base.ReadAt(chunk, offset+pos)
			if err != nil && !errors.Is(err, io.EOF) {
				// Zero the rest of the chunk if base reader returned partial.
				zeroSlice(chunk[n:])
				lock.RUnlock()
				return int(pos) + n, err
			}
			zeroSlice(chunk[n:])
		} else {
			zeroSlice(chunk)
		}
		lock.RUnlock()
		pos += int64(len(chunk))
	}
	return int(pos), nil
}

// WriteAt writes buf at offset to the diff file. The first partial write to a
// clean block materializes the complete block from base (or zeros), merges the
// caller's bytes, writes the complete block, and only then marks it dirty.
func (c *BlockCOW) WriteAt(buf []byte, offset int64) (int, error) {
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
		n, err := c.writeBlockLocked(buf[written:written+chunkLen], pos, blk)
		lock.Unlock()
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// writeBlockLocked writes a range wholly contained in blk. The caller holds
// that block's stripe lock exclusively.
func (c *BlockCOW) writeBlockLocked(buf []byte, offset, blk int64) (int, error) {
	if c.blockDirty(blk) {
		return c.diff.WriteAt(buf, offset)
	}

	blockStart := blk * c.blockSize
	if offset == blockStart && int64(len(buf)) == c.blockSize {
		if err := c.writeFreshBlock(buf, blockStart); err != nil {
			return 0, err
		}
		c.markDirty(blk)
		return len(buf), nil
	}

	block := make([]byte, c.blockSize)
	if c.base != nil && blockStart < c.base.Size() {
		readLen := c.blockSize
		if remaining := c.base.Size() - blockStart; remaining < readLen {
			readLen = remaining
		}
		n, err := c.base.ReadAt(block[:readLen], blockStart)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("vhost: materialize block %d from base: %w", blk, err)
		}
		if int64(n) != readLen {
			return 0, fmt.Errorf("vhost: materialize block %d from base: %w", blk, io.ErrUnexpectedEOF)
		}
	}
	copy(block[offset-blockStart:], buf)
	if err := c.writeFreshBlock(block, blockStart); err != nil {
		return 0, err
	}
	c.markDirty(blk)
	return len(buf), nil
}

func (c *BlockCOW) writeFreshBlock(block []byte, offset int64) error {
	n, err := c.diff.WriteAt(block, offset)
	if err == nil && n != len(block) {
		err = io.ErrShortWrite
	}
	if err == nil {
		return nil
	}
	// A clean block must remain a hole after a failed materialization so a
	// future reopen cannot mistake a partial write for a complete dirty block.
	_ = c.diff.punchHole(offset, c.blockSize)
	return fmt.Errorf("vhost: materialize diff block at %d: %w", offset, err)
}

// Flush is called on virtio-blk FLUSH; sync diff file to disk.
func (c *BlockCOW) Flush() error {
	return c.diff.Sync()
}

// Discard punches complete blocks in the diff and clears their dirty bits.
// Subsequent reads fall through to the base, or return zeros without a base;
// this helper does not create an explicit zero that masks lower-layer data.
// Partial edge blocks retain their contents. The current vhost dispatcher
// does not call this helper: DISCARD and WRITE_ZEROES requests are unsupported.
func (c *BlockCOW) Discard(offset, length int64) error {
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

	punchOff := startBlk * blockSize
	punchLen := (endBlk - startBlk) * blockSize
	if err := c.diff.punchHole(punchOff, punchLen); err != nil {
		return fmt.Errorf("vhost: punch_hole [%d,%d): %w",
			punchOff, punchOff+punchLen, err)
	}

	c.bitmapMu.Lock()
	for blk := startBlk; blk < endBlk; blk++ {
		c.bitmap[blk/64] &^= 1 << (uint64(blk) % 64)
	}
	c.bitmapMu.Unlock()

	return nil
}

// SnapshotView returns a read-only, upper-only view of the diff at the current
// dirty-bitmap state. Dirty blocks expose their complete plaintext diff bytes;
// clean blocks are holes and defensively read as zeros rather than falling
// through to base. The caller must keep the BlockCOW open while using the view.
// Snapshot orchestration calls this after quiescing every vhost backend, so the
// dirty block contents stay stable for the view's lifetime.
func (c *BlockCOW) SnapshotView() (io.ReadSeeker, []sparse.Extent, error) {
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

	written := 0
	for written < n {
		pos := offset + int64(written)
		blk := pos / r.cow.blockSize
		dirty := bitmapBlockDirty(r.bitmap, blk)
		runEnd := min((blk+1)*r.cow.blockSize, offset+int64(n))
		for runEnd < offset+int64(n) {
			nextBlk := runEnd / r.cow.blockSize
			if bitmapBlockDirty(r.bitmap, nextBlk) != dirty {
				break
			}
			runEnd = min((nextBlk+1)*r.cow.blockSize, offset+int64(n))
		}
		chunkLen := int(runEnd - pos)
		chunk := buf[written : written+chunkLen]

		if dirty {
			lastBlk := (runEnd - 1) / r.cow.blockSize
			r.cow.rlockBlockRange(blk, lastBlk)
			read, err := r.cow.diff.ReadAt(chunk, pos)
			r.cow.runlockBlockRange(blk, lastBlk)
			written += read
			if err != nil && !(errors.Is(err, io.EOF) && read == len(chunk)) {
				return written, err
			}
			if read != len(chunk) {
				return written, io.ErrUnexpectedEOF
			}
			continue
		}

		clear(chunk)
		written += len(chunk)
	}
	return written, eof
}

// rlockBlockRange locks each stripe touched by the inclusive block range once,
// in numeric stripe order. Snapshot reads can then issue one pread for a
// contiguous dirty run without weakening same-block exclusion or recursively
// taking an RWMutex when a range spans more than one full stripe cycle.
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

// Close releases the diff file. Base is closed by its owner.
func (c *BlockCOW) Close() error {
	return c.diff.Close()
}

func zeroSlice(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
