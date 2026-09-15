package vhost

import (
	"context"
	"fmt"
	"io"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

// BlockReader is the abstract source for read-only block data behind a
// vhost-user-blk backend. blk0 (base image) and blk1's optional base layer
// both go through this interface.
//
// ReadAt must be safe for concurrent use; the backend serves multiple
// virtq requests in parallel.
type BlockReader interface {
	ReadAt(buf []byte, offset int64) (int, error)
	Size() int64
	Close() error
}

// StreamReader adapts a fetch.Stream (local file, manifest, or layered overlay)
// to a BlockReader. Holes and IsZero regions are zero-filled inline by
// Stream.ReadAt's POSIX semantics, so the guest sees a flat sparse device.
type StreamReader struct {
	stream fetch.Stream
	size   int64
	ctx    context.Context
}

// NewStreamReader wraps an already-opened fetch.Stream with the given total
// image size. The reader owns the stream's resources: Close releases them
// (closing a file stream's fd; a manifest stream's underlying cache/store
// client is owned by the Fetcher and closed separately).
func NewStreamReader(ctx context.Context, stream fetch.Stream, size int64) *StreamReader {
	return &StreamReader{stream: stream, size: size, ctx: ctx}
}

// ReadAt fills buf from the stream's virtual image at offset.
//
// Satisfies io.ReaderAt: short reads at end-of-image come back with
// (n, io.EOF), past-EOF reads with (0, io.EOF). archive/zip and other
// io.ReaderAt consumers can treat StreamReader the same as a file.
func (r *StreamReader) ReadAt(buf []byte, offset int64) (int, error) {
	return r.readAt(r.ctx, buf, offset)
}

// Runtime calls supply the queue context, replacing the preparation context
// for this call only. A reset can stop the old consumer without changing the
// immutable Stream or the context used by another operation.
func (r *StreamReader) readAt(ctx context.Context, buf []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, io.EOF
	}
	if offset >= r.size {
		return 0, io.EOF
	}
	want := min(int64(len(buf)), r.size-offset)
	var n int
	var result error
	err := readretry.Do(ctx, func() error {
		n, result = r.stream.ReadAt(ctx, buf, uint64(offset))
		if readretry.IsTerminal(result) || readerr.IsPermanent(result) {
			return result
		}
		if int64(n) == want && (result == nil || result == io.EOF) {
			return nil
		}
		if result == nil || result == io.EOF || result == io.ErrUnexpectedEOF {
			return readerr.Mark(fmt.Errorf("vhost: source read %d of %d bytes: %w", n, want, io.ErrUnexpectedEOF), false)
		}
		return result
	})
	if err != nil {
		return n, err
	}
	return n, result
}

func readBlock(ctx context.Context, r BlockReader, buf []byte, offset int64) (int, error) {
	if ctx != nil {
		if reader, ok := r.(interface {
			readAt(context.Context, []byte, int64) (int, error)
		}); ok {
			return reader.readAt(ctx, buf, offset)
		}
	}
	return r.ReadAt(buf, offset)
}

func (r *StreamReader) Size() int64 { return r.size }

func (r *StreamReader) Close() error { return r.stream.Close() }

// ImageConfigBytes exposes the separately validated config.json carried by a
// .sandbox EROFS root. It never changes the block-visible Size or ReadAt view.
func (r *StreamReader) ImageConfigBytes() []byte {
	if provider, ok := r.stream.(interface{ ImageConfigBytes() []byte }); ok {
		return provider.ImageConfigBytes()
	}
	return nil
}
