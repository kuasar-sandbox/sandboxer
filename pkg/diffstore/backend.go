package diffstore

import (
	"context"
	"io"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/versioned"
)

// A Data pageWrite borrows a complete plaintext page; an explicit Zero needs
// only its block number and has no Data payload. Before starting I/O the single
// layer backend updates Fresh from effective payload existence for failure
// cleanup: logical presence alone cannot distinguish Data from Zero.
type pageWrite struct {
	Block uint64
	Data  []byte
	Fresh bool
	Zero  bool // explicit logical Zero; ordinary all-zero Data remains Data
}
type batchPolicy struct {
	materialPages int
}

// diffBackend owns physical storage only. It never owns/cache-mutates borrowed
// pages. A write returns only after all I/O/commit work has stopped using them.
// Failure cleanup runs after the cache publishes its fatal state and before it
// releases frozen pages or dirty charges. Sync is distinct from guest FLUSH.
type diffBackend interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
	initialized() <-chan struct{}
	waitInitialized(context.Context) error
	name() string
	policy() batchPolicy
	withDirectRead(int, int64, func([]byte) error) error
	writePages(context.Context, []pageWrite) error
	cleanupWriteFailure([]pageWrite) error
	scanDirtyBlocks() ([]uint64, error)
	payloadPresence(context.Context, uint64, []bool) error
	discard(context.Context, int64, int64) error
	markModified(context.Context) error
	err() error
	Sync() error
	Close() error
}

func (d *diffFile) Size() int64                               { return d.logicalSize }
func (d *diffFile) initialized() <-chan struct{}              { return nil }
func (d *diffFile) waitInitialized(ctx context.Context) error { return ctx.Err() }
func (d *checkpointDiff) initialized() <-chan struct{}        { return versioned.Initialized(d.store) }
func (d *checkpointDiff) waitInitialized(ctx context.Context) error {
	return versioned.WaitInitialized(d.store, ctx)
}

// Called under the cache lock before freezing any page. A failed initializer
// is reported by the coordinator after publishing materialDone, not by a
// worker that could invoke a completion-waiting fatal handler too early.
func backendWritable(d diffBackend) bool {
	if done := d.initialized(); done != nil {
		select {
		case <-done:
		default:
			return false
		}
	}
	return d.waitInitialized(context.Background()) == nil
}

// Only the v4 cache write API may defer a safely rolled-back reservation
// failure. Other backend failures retain the ordinary sticky-fatal behavior.
func isWritebackSpacePressure(cow *Disk, err error) bool {
	return cow.versions != nil && versioned.IsSpacePressure(err)
}
func (d *diffFile) policy() batchPolicy {
	// Reuse writeback batching for template materialization. Cap each staging
	// and stripe-lock window at 256 KiB instead of publishing P per 4 KiB page.
	return batchPolicy{materialPages: (256 << 10) / cowBlockSize}
}
func (d *diffFile) markModified(ctx context.Context) error { return ctx.Err() }
func (d *diffFile) err() error                             { return nil }
func (d *checkpointDiff) Size() int64                      { return d.logicalSize }
func (d *checkpointDiff) payloadPresence(ctx context.Context, start uint64, dst []bool) error {
	return d.store.PayloadPresence(ctx, start, dst)
}
func (d *diffFile) payloadPresence(ctx context.Context, start uint64, dst []bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if start > uint64(d.logicalSize/cowBlockSize) || uint64(len(dst)) > uint64(d.logicalSize/cowBlockSize)-start {
		return io.EOF
	}
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	for i := range dst {
		dst[i] = bitmapBlockDirty(d.payload, int64(start)+int64(i))
	}
	return nil
}
func (d *checkpointDiff) name() string { return d.path }
func (d *checkpointDiff) policy() batchPolicy {
	return batchPolicy{materialPages: maxWritebackPages}
}
func (d *checkpointDiff) cleanupWriteFailure([]pageWrite) error { return nil }
func (d *checkpointDiff) discard(ctx context.Context, off, n int64) error {
	return d.store.Discard(ctx, off, n)
}
func (d *checkpointDiff) scanDirtyBlocks() ([]uint64, error) {
	bitmap := make([]uint64, (d.logicalSize/cowBlockSize+63)/64)
	p, err := d.store.PresenceBitmap(context.Background())
	if err != nil {
		return nil, err
	}
	for i, v := range p {
		bitmap[i/8] |= uint64(v) << (8 * (i % 8))
	}
	return bitmap, nil
}

var _ diffBackend = (*diffFile)(nil)
var _ diffBackend = (*checkpointDiff)(nil)
