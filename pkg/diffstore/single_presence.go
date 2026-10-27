package diffstore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
	"golang.org/x/sys/unix"
)

func sealSinglePresence(b []byte) {
	binary.LittleEndian.PutUint32(b[len(b)-4:], crc32.ChecksumIEEE(b[:len(b)-4]))
}

// prepareWrites records effective payload existence, not just P: a Zero page's
// first payload must be removed on failure while preserving its zero mask.
func (d *diffFile) prepareWrites(pages []pageWrite) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	for i := range pages {
		b := pages[i].Block
		pages[i].Fresh = !presence.Has(d.bitmap, b) || !bitmapBlockDirty(d.payload, int64(b))
	}
}

func (d *diffFile) finishWrites(pages []pageWrite) error {
	blocks := make([]uint64, len(pages))
	for i, p := range pages {
		blocks[i] = p.Block
	}
	if err := d.publishPresence(blocks, false); err != nil {
		return err
	}
	d.stateMu.Lock()
	for _, b := range blocks {
		d.payload[b/64] |= uint64(1) << (b % 64)
	}
	d.stateMu.Unlock()
	return nil
}

func (d *diffFile) cleanupWritesLocked(pages []pageWrite) error {
	var result error
	var blocks []uint64
	for _, p := range pages {
		if !p.Fresh {
			continue
		}
		err := unix.Fallocate(int(d.f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, d.bodyOffset+int64(p.Block)*cowBlockSize, cowBlockSize)
		result = errors.Join(result, err)
		if err == nil {
			blocks = append(blocks, p.Block)
		}
	}
	// A bitmap write may have failed after an earlier page was persisted. Restore
	// exactly the old committed bits after removing fresh payloads, including
	// P=1 for Zero. Do not copy any accepted-but-unflushed frontend state.
	result = errors.Join(result, d.publishPresence(blocks, true))
	return result
}

func (d *diffFile) scanDirtyBlocks() ([]uint64, error) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	bitmap := make([]uint64, (d.logicalSize/cowBlockSize+63)/64)
	full := len(d.bitmap) / 8
	for i := 0; i < full; i++ {
		bitmap[i] = binary.LittleEndian.Uint64(d.bitmap[i*8 : i*8+8])
	}
	for i := full * 8; i < len(d.bitmap); i++ {
		bitmap[i/8] |= uint64(d.bitmap[i]) << (8 * (i % 8))
	}
	return bitmap, nil
}

// direct.writeMu serializes payload changes with persisted bitmap updates.
// stateMu is held only while copying/publishing in-memory state, never for I/O.
// Normal publication only sets P. restore is exclusively failed-write cleanup:
// encode the old committed bitmap verbatim without changing in-memory bits.
func (d *diffFile) publishPresence(blocks []uint64, restore bool) error {
	pages := make(map[int][]byte)
	d.stateMu.RLock()
	for _, block := range blocks {
		if !restore && presence.Has(d.bitmap, block) {
			continue
		}
		i := int(block/8) / presence.PayloadSize
		if pages[i] == nil {
			pages[i] = presence.Encode(d.bitmap, d.logicalSize, i)
		}
		if !restore {
			presence.Set(pages[i][presence.HeaderSize:cowBlockSize-4], block-uint64(i*presence.PayloadSize)*8)
		}
	}
	d.stateMu.RUnlock()
	indices := make([]int, 0, len(pages))
	for i := range pages {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		b := pages[i]
		sealSinglePresence(b)
		if d.beforePresenceWrite != nil {
			if err := d.beforePresenceWrite(i); err != nil {
				return err
			}
		}
		if d.direct == nil {
			return fmt.Errorf("diffstore: bitmap write without owned direct workspace")
		}
		buf := d.direct.write.Bytes[:cowBlockSize]
		copy(buf, b)
		err := writeFullAt(directBody{d.f}, buf, diffHeaderRegionSize+int64(i)*cowBlockSize)
		clear(buf)
		if err != nil {
			return err
		}
	}
	if !restore {
		d.stateMu.Lock()
		for _, block := range blocks {
			presence.Set(d.bitmap, block)
		}
		d.stateMu.Unlock()
	}
	return nil
}

func (d *diffFile) classify(offset, limit uint64) (sparse.RunKind, uint64) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	block := offset / cowBlockSize
	end := min(uint64(d.logicalSize), offset+min(limit, uint64(d.logicalSize)-offset))
	if !presence.Has(d.bitmap, block) {
		return sparse.Hole, min(end, presence.End(d.bitmap, block, (end+cowBlockSize-1)/cowBlockSize)*cowBlockSize)
	}
	data := bitmapBlockDirty(d.payload, int64(block))
	for next := block + 1; next*cowBlockSize < end; next++ {
		if !presence.Has(d.bitmap, next) || bitmapBlockDirty(d.payload, int64(next)) != data {
			end = next * cowBlockSize
			break
		}
	}
	if data {
		return sparse.Data, end
	}
	return sparse.Zero, end
}

// decodePayload never decrypts a sparse file hole. P=0 also hides any stale
// bytes left by a failed or interrupted write before its presence publication.
func (d *diffFile) decodePayload(buf []byte, start int64) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	for i := 0; i < len(buf); i += cowBlockSize {
		block := (start + int64(i)) / cowBlockSize
		part := buf[i : i+cowBlockSize]
		if !presence.Has(d.bitmap, uint64(block)) || !bitmapBlockDirty(d.payload, block) {
			clear(part)
		} else {
			d.cryptUnits(part, block*cowBlockSize, false)
		}
	}
}

func (d *diffFile) discard(ctx context.Context, offset, length int64) error {
	if offset < 0 || length < 0 || offset%cowBlockSize != 0 || length%cowBlockSize != 0 || offset > d.logicalSize || length > d.logicalSize-offset {
		return fmt.Errorf("diffstore: invalid single-layer discard")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if length == 0 {
		return nil
	}
	d.direct.writeMu.Lock()
	defer d.direct.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Fallocate(int(d.f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, d.bodyOffset+offset, length); err != nil {
		return err
	}
	blocks := make([]uint64, length/cowBlockSize)
	for i := range blocks {
		blocks[i] = uint64(offset/cowBlockSize) + uint64(i)
	}
	if err := d.publishPresence(blocks, false); err != nil {
		return err
	}
	d.stateMu.Lock()
	for _, block := range blocks {
		d.payload[block/64] &^= uint64(1) << (block % 64)
	}
	d.stateMu.Unlock()
	return nil
}
