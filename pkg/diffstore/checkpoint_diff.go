package diffstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/versioned"
)

type checkpointDiskOption struct{ diffID, baseID string }

// WithCheckpoint selects the persistent v4 DIFF format and binds it to its
// stable disk identity and immutable base. Reopening never adopts older formats.
func WithCheckpoint(diffID, baseID string) DiskOption {
	return checkpointDiskOption{diffID, baseID}
}

func (o checkpointDiskOption) applyDisk(options *diskOptions) error {
	if o.diffID == "" || options.checkpoint != nil {
		return fmt.Errorf("diffstore: empty or duplicate checkpoint disk identity")
	}
	options.checkpoint = &o
	return nil
}

type checkpointDiff struct {
	path        string
	logicalSize int64
	store       *versioned.Store
	readMu      sync.Mutex
	read        []byte
}

func (d *checkpointDiff) ReadAt(p []byte, off int64) (int, error)  { return d.store.ReadAt(p, off) }
func (d *checkpointDiff) WriteAt(p []byte, off int64) (int, error) { return d.store.WriteAt(p, off) }
func (d *checkpointDiff) markModified(ctx context.Context) error   { return d.store.MarkModified(ctx) }
func (d *checkpointDiff) Sync() error                              { return d.store.Sync() }
func (d *checkpointDiff) err() error                               { return d.store.Err() }
func (d *checkpointDiff) Close() error {
	d.readMu.Lock()
	defer d.readMu.Unlock()
	clear(d.read)
	d.read = nil
	return d.store.Close()
}

func (d *checkpointDiff) withDirectRead(length int, offset int64, consume func([]byte) error) error {
	if length <= 0 || length > maxDiffScratchSize {
		return fmt.Errorf("diffstore: invalid bounded checkpoint read")
	}
	d.readMu.Lock()
	defer d.readMu.Unlock()
	if d.read == nil {
		return os.ErrClosed
	}
	p := d.read[:length]
	defer clear(p)
	if err := readFullAt(d.store, p, offset); err != nil {
		return err
	}
	return consume(p)
}

func (d *checkpointDiff) writePages(ctx context.Context, pages []pageWrite) error {
	if len(pages) == 0 || len(pages) > maxWritebackPages {
		return fmt.Errorf("diffstore: invalid checkpoint batch")
	}
	var batch [maxWritebackPages]versioned.Page
	for i, p := range pages {
		batch[i] = versioned.Page{Block: p.Block, Data: p.Data}
	}
	return d.store.WritePages(ctx, batch[:len(pages)])
}

func checkpointStoreOptions(options diskOptions, size int64, path string) versioned.Options {
	return versioned.Options{DiffID: options.checkpoint.diffID, BaseID: options.checkpoint.baseID,
		LogicalSize: size, Key: options.checkpointKey, RequireEncryption: options.required, LockPath: path + ".lock"}
}

func wrapCheckpointDiff(store *versioned.Store, path string) *checkpointDiff {
	return &checkpointDiff{store: store, path: path, logicalSize: store.Size(), read: make([]byte, maxDiffScratchSize)}
}

func openCheckpointDiff(path string, init DiffInit, options diskOptions) (diffBackend, *freshDiffInit, error) {
	if path == "" {
		return nil, nil, fmt.Errorf("diffstore: empty checkpoint diff path")
	}
	if init.Existing {
		store, err := versioned.Open(path, checkpointStoreOptions(options, init.CreateSize, path))
		if err != nil {
			return nil, nil, err
		}
		info := store.Info()
		// Info resolves Current and records metadata failures in store health.
		// Lazy presence no longer runs Scan/healthy before cache attachment.
		if err := store.Err(); err != nil {
			return nil, nil, errors.Join(fmt.Errorf("diffstore: inspect checkpoint diff: %w", err), store.Close())
		}
		if info.DiffID != options.checkpoint.diffID || info.BaseID != options.checkpoint.baseID {
			return nil, nil, errors.Join(fmt.Errorf("diffstore: checkpoint identity/base mismatch"), store.Close())
		}
		return wrapCheckpointDiff(store, path), nil, nil
	}
	fresh, err := createFreshDiff(path, init, options)
	if err != nil {
		return nil, nil, err
	}
	return fresh.diff, fresh, nil
}

func (d *diffFile) name() string {
	if d.path != "" {
		return d.path
	}
	return d.f.Name()
}
func (d *diffFile) cleanupWriteFailure(pages []pageWrite) error {
	d.direct.writeMu.Lock()
	defer d.direct.writeMu.Unlock()
	return d.cleanupWritesLocked(pages)
}

func (c *Disk) markModified(ctx context.Context) error {
	if err := c.diff.markModified(ctx); err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return err
		}
		if initializationFailure(c.diff) {
			// The initializer's coordinator publishes completion and cache
			// failure together. Do not race it with an early fatal callback.
			return err
		}
		c.cache.mu.Lock()
		report := c.cache.failLocked(fmt.Errorf("diffstore: invalidate checkpoint continuation: %w", err))
		fatal := c.cache.err
		c.cache.mu.Unlock()
		if report != nil {
			report(fatal)
		}
		return fatal
	}
	return nil
}

// Metadata corruption is sticky in the checkpoint backend. A foreground read
// can discover it before the writeback worker; notify the shared runtime owner
// immediately through the same fatal path, without poisoning ordinary cancels.
func (c *Disk) propagateBackendFailure(err error) error {
	if err == nil || c.diff.err() == nil {
		return err
	}
	if initializationFailure(c.diff) {
		return err
	}
	c.cache.mu.Lock()
	report := c.cache.failLocked(err)
	fatal := c.cache.err
	c.cache.mu.Unlock()
	if report != nil {
		report(fatal)
	}
	return fatal
}

// Only used after observing a backend error. A pending fresh backend cannot
// yet have a normal I/O failure: initialization owns its physical operations.
// Err can observe the atomic failure just before the completion event closes.
func initializationFailure(d diffBackend) bool {
	if done := d.initialized(); done != nil {
		select {
		case <-done:
			return d.waitInitialized(context.Background()) != nil
		default:
			return true
		}
	}
	return false
}

type CheckpointID string

const ActiveDiff CheckpointID = ""

var ErrCheckpointUnsupported = errors.New("checkpoint_unsupported")

type ExportView interface {
	sparse.Source
	io.Closer
}

func (c *Disk) checkpointBackend() (*checkpointDiff, error) {
	backend := c.versions
	if backend == nil {
		return nil, ErrCheckpointUnsupported
	}
	return backend, nil
}

// CheckpointInfo returns the immutable identity used by the shared disk plan.
func (c *Disk) CheckpointInfo() (diffID, baseID string, enabled bool) {
	d, err := c.checkpointBackend()
	if err != nil {
		return "", "", false
	}
	info := versioned.Identity(d.store)
	return info.DiffID, info.BaseID, true
}

func (c *Disk) CanResumeCheckpoint(id CheckpointID) bool {
	d, err := c.checkpointBackend()
	if err != nil {
		return false
	}
	_, end, err := c.begin(nil)
	if err != nil {
		return false
	}
	defer c.end(end)
	return d.store.CanResume(string(id))
}

// Checkpoint pauses frontend admission only after the template has reached
// Ready, so ongoing guest writes cannot starve entry to this boundary. The
// worker and materializer never take opMu, and can finish Drain/publication.
func (c *Disk) Checkpoint(ctx context.Context, id CheckpointID) error {
	d, err := c.checkpointBackend()
	if err != nil {
		return err
	}
	if id == ActiveDiff {
		return fmt.Errorf("diffstore: empty checkpoint ID")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.WaitMaterialReady(ctx); err != nil {
		return err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := c.Err(); err != nil {
		return err
	}
	if err := c.cache.drain(ctx, c); err != nil {
		return err
	}
	if err := c.WaitMaterialized(ctx); err != nil {
		return err
	}
	return d.store.Checkpoint(ctx, string(id))
}

func (c *Disk) ListCheckpoints(ctx context.Context) ([]CheckpointID, error) {
	d, err := c.checkpointBackend()
	if err != nil {
		return nil, err
	}
	ctx, end, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer c.end(end)
	ids, err := d.store.ListCheckpoints(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]CheckpointID, len(ids))
	for i, id := range ids {
		result[i] = CheckpointID(id)
	}
	return result, nil
}

func (c *Disk) DeleteCheckpoint(ctx context.Context, id CheckpointID) error {
	return c.DeleteCheckpoints(ctx, []CheckpointID{id})
}

// DeleteCheckpoints revokes a complete selection before permitting the store's
// background collector to transfer ownership between the selected versions.
func (c *Disk) DeleteCheckpoints(ctx context.Context, ids []CheckpointID) error {
	d, err := c.checkpointBackend()
	if err != nil {
		return err
	}
	selected := make([]string, len(ids))
	for i, id := range ids {
		if id == ActiveDiff {
			return fmt.Errorf("diffstore: empty checkpoint ID")
		}
		selected[i] = string(id)
	}
	ctx, end, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer c.end(end)
	return d.store.DeleteCheckpoints(ctx, selected)
}

type activeExportView struct {
	source sparse.Source
	size   uint64
	mu     sync.Mutex
}

func (v *activeExportView) Size() uint64 { return v.size }
func (v *activeExportView) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.source = nil
	return nil
}
func (v *activeExportView) RunAt(offset, limit uint64) (sparse.Run, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.source == nil {
		return nil, os.ErrClosed
	}
	run, err := v.source.RunAt(offset, limit)
	if err != nil {
		return nil, err
	}
	return activeExportRun{source: v, offset: run.Offset(), end: run.End(), kind: run.Kind()}, nil
}
func (v *activeExportView) ReadAt(ctx context.Context, p []byte, offset uint64) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.source == nil {
		return 0, os.ErrClosed
	}
	return v.source.ReadAt(ctx, p, offset)
}

type activeExportRun struct {
	source      *activeExportView
	offset, end uint64
	kind        sparse.RunKind
}

func (r activeExportRun) Offset() uint64       { return r.offset }
func (r activeExportRun) End() uint64          { return r.end }
func (r activeExportRun) Kind() sparse.RunKind { return r.kind }
func (r activeExportRun) ReadAt(ctx context.Context, p []byte, inner uint64) (int, error) {
	if inner > r.end-r.offset || uint64(len(p)) > r.end-r.offset-inner {
		return 0, fmt.Errorf("diffstore: export run read out of bounds")
	}
	return r.source.ReadAt(ctx, p, r.offset+inner)
}

// Export(ActiveDiff) shares the private active view with SnapshotView; its
// caller keeps the backend quiescent. Named views own independent reader pins
// and remain usable after the active Disk closes or resumes writing.
func (c *Disk) Export(ctx context.Context, id CheckpointID) (ExportView, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if id == ActiveDiff {
		reader, err := c.activeView(ctx)
		if err != nil {
			return nil, err
		}
		source := &activeSource{reader: reader}
		return &activeExportView{source: source, size: uint64(c.size)}, nil
	}
	d, err := c.checkpointBackend()
	if err != nil {
		return nil, err
	}
	ctx, end, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer c.end(end)
	return d.store.Export(ctx, string(id))
}

// openCheckpointPreflight authenticates metadata and reads a quiescent active
// view without acquiring write ownership or replaying metadata in place.
func openCheckpointPreflight(ctx context.Context, path string, options diskOptions) (diffTemplateSource, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	info, err := versioned.Probe(path)
	if err != nil {
		return nil, err
	}
	if info.DiffID != options.checkpoint.diffID || info.BaseID != options.checkpoint.baseID {
		return nil, fmt.Errorf("diffstore: checkpoint identity/base mismatch")
	}
	if options.required && !info.Encrypted {
		return nil, ErrDiffPlaintextForbidden
	}
	return versioned.OpenExport(ctx, path, "", options.checkpointKey)
}

// Template writes use bounded metadata batches too. Read template bytes before
// taking stripes, then recheck presence under the same locks as frontend writes.
// The cache worker does not take stripes, so metadata completion can progress.
func (c *Disk) materializeRange(ctx context.Context, template diffTemplateSource, start, end uint64, staging []byte) error {
	var records [maxWritebackPages]pageWrite
	for offset := start; offset < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := min(end, offset+uint64(len(staging)))
		length := int(next - offset)
		n, err := template.ReadAt(ctx, staging[:length], offset)
		if err != nil && !(errors.Is(err, io.EOF) && n == length) {
			return err
		}
		if n != length {
			return io.ErrUnexpectedEOF
		}
		first, last := int64(offset)/cowBlockSize, int64(next-1)/cowBlockSize
		// Numeric order prevents inversion with bounded frontend read ranges.
		var locked [cowLockStripes]bool
		for block := first; block <= last; block++ {
			locked[block%cowLockStripes] = true
		}
		for stripe := range locked {
			if locked[stripe] {
				c.blockMu[stripe].Lock()
			}
		}
		pages := records[:0]
		for pos := offset; pos < next; pos += cowBlockSize {
			block := int64(pos) / cowBlockSize
			if !c.blockDirty(block) {
				pages = append(pages, pageWrite{Block: uint64(block), Data: staging[pos-offset : pos-offset+cowBlockSize]})
			}
		}
		if len(pages) > 0 {
			err = c.diff.writePages(ctx, pages)
		}
		if err == nil {
			for _, page := range pages {
				c.markDirty(int64(page.Block))
			}
		}
		for stripe := len(locked) - 1; stripe >= 0; stripe-- {
			if locked[stripe] {
				c.blockMu[stripe].Unlock()
			}
		}
		if err != nil {
			return err
		}
		offset = next
	}
	return nil
}

// A full template Zero is a logical mask and needs no payload allocation.
// Frontend changes and earlier discards win under the same stripe locks.
func (c *Disk) materializeZero(ctx context.Context, start, end uint64) error {
	for first := int64(start) / cowBlockSize; first < int64(end)/cowBlockSize; first += maxDiscardPages {
		last := min(first+maxDiscardPages, int64(end)/cowBlockSize) - 1
		c.lockBlockRange(first, last)
		var err error
		for block := first; block <= last; {
			if c.blockDirty(block) {
				block++
				continue
			}
			next := block + 1
			for next <= last && !c.blockDirty(next) {
				next++
			}
			err = c.cache.discard(ctx, c, block, next-1)
			if err != nil {
				break
			}
			block = next
		}
		c.unlockBlockRange(first, last)
		if err != nil {
			return err
		}
	}
	return nil
}
