package vhost

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"golang.org/x/sys/unix"
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
// DISCARD requests are accepted but currently no-op (v1 limitation).
type BlockCOW struct {
	base      BlockReader // optional; may be nil for no base layer
	diff      *os.File
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

// OpenBlockCOW opens diff for read+write (creating it sparse if absent),
// builds the dirty bitmap, and pairs it with the optional base reader.
//
// If base is nil, reads to clean blocks return zeros.
//
// createSize sizes the diff ONLY when it is freshly created (absent/empty):
// the device size then equals createSize. An existing non-empty diff is used
// at its current size and is NEVER truncated — shrinking would corrupt the
// filesystem inside it, and growing the block device would not grow that
// filesystem anyway, so the diff is provisioned at its final size up front
// (see docs/sandbox.md §3.1). The caller provisions a pre-formatted /
// template- / base-backed diff for cold boot.
func OpenBlockCOW(diffPath string, base BlockReader, createSize int64) (*BlockCOW, error) {
	if createSize <= 0 {
		return nil, errors.New("vhost: createSize must be > 0")
	}
	if createSize%cowBlockSize != 0 {
		return nil, fmt.Errorf("vhost: createSize %d not aligned to %d", createSize, cowBlockSize)
	}

	f, err := os.OpenFile(diffPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("vhost: open diff %s: %w", diffPath, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("vhost: stat diff %s: %w", diffPath, err)
	}
	size := st.Size()
	if size == 0 {
		// Freshly created (or empty): size it once, here. This truncate is
		// creation-only — it never runs against a diff that already has data.
		size = createSize
		if err := f.Truncate(size); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("vhost: size fresh diff to %d: %w", size, err)
		}
	}
	if size%cowBlockSize != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("vhost: existing diff size %d not aligned to %d", size, cowBlockSize)
	}
	if base != nil && base.Size() > size {
		_ = f.Close()
		return nil, fmt.Errorf("vhost: base size %d > diff size %d", base.Size(), size)
	}

	numBlocks := size / cowBlockSize
	bitmap := make([]uint64, (numBlocks+63)/64)

	cow := &BlockCOW{
		base:      base,
		diff:      f,
		size:      size,
		bitmap:    bitmap,
		blockMu:   make([]sync.RWMutex, cowLockStripes),
		blockSize: cowBlockSize,
	}
	if err := cow.rebuildBitmap(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("vhost: rebuild bitmap: %w", err)
	}
	return cow, nil
}

// rebuildBitmap walks the diff file with SEEK_DATA/SEEK_HOLE and marks
// each 4K block that contains any data as dirty.
func (c *BlockCOW) rebuildBitmap() error {
	fd := int(c.diff.Fd())
	var off int64 = 0
	for off < c.size {
		dataOff, err := syscall.Seek(fd, off, 3 /* SEEK_DATA */)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				return nil // no more data; rest of file is hole
			}
			return fmt.Errorf("SEEK_DATA at %d: %w", off, err)
		}
		holeOff, err := syscall.Seek(fd, dataOff, 4 /* SEEK_HOLE */)
		if err != nil {
			return fmt.Errorf("SEEK_HOLE at %d: %w", dataOff, err)
		}
		if holeOff > c.size {
			holeOff = c.size
		}
		// Mark every block in [dataOff, holeOff) as dirty.
		startBlk := dataOff / c.blockSize
		endBlk := (holeOff + c.blockSize - 1) / c.blockSize
		for blk := startBlk; blk < endBlk; blk++ {
			c.bitmap[blk/64] |= 1 << (uint64(blk) % 64)
		}
		off = holeOff
	}
	return nil
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
	if offset < 0 || offset >= c.size {
		return 0, io.EOF
	}
	end := offset + int64(len(buf))
	if end > c.size {
		buf = buf[:c.size-offset]
		end = c.size
	}

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
	_ = unix.Fallocate(int(c.diff.Fd()),
		unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE,
		offset, c.blockSize)
	return fmt.Errorf("vhost: materialize diff block at %d: %w", offset, err)
}

// Flush is called on virtio-blk FLUSH; sync diff file to disk.
func (c *BlockCOW) Flush() error {
	return c.diff.Sync()
}

// Discard handles virtio-blk DISCARD/WRITE_ZEROES. Punches a hole in the
// diff file for the requested range and clears the corresponding dirty
// bits, so subsequent reads see zeros via the no-base-layer path.
//
// Correctness depends on base layer being nil — the case we hit in cold
// start and P2 snapshot. With a base layer (P3 restore), bitmap=clean
// would route reads to base.ReadAt instead of zero, returning stale
// content the guest considered freed. The P3-era extension switches to
// a 3-state stateMap (clean/dirty/discard); see sandbox.md §12.4.
func (c *BlockCOW) Discard(offset, length int64) error {
	if offset < 0 || length < 0 || offset+length > c.size {
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
	if err := unix.Fallocate(int(c.diff.Fd()),
		unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE,
		punchOff, punchLen); err != nil {
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
		blkEnd := (blk + 1) * r.cow.blockSize
		chunkLen := n - written
		if remaining := blkEnd - pos; int64(chunkLen) > remaining {
			chunkLen = int(remaining)
		}
		chunk := buf[written : written+chunkLen]

		if bitmapBlockDirty(r.bitmap, blk) {
			lock := r.cow.blockLock(blk)
			lock.RLock()
			read, err := r.cow.diff.ReadAt(chunk, pos)
			lock.RUnlock()
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
