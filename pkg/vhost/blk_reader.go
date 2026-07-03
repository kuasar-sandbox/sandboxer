package vhost

import (
	"context"
	"io"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
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
	if offset < 0 {
		return 0, io.EOF
	}
	return r.stream.ReadAt(r.ctx, buf, uint64(offset))
}

func (r *StreamReader) Size() int64 { return r.size }

func (r *StreamReader) Close() error { return r.stream.Close() }
