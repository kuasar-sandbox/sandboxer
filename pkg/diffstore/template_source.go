package diffstore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

// TemplateSource is one immutable logical provisioning image. It strengthens
// sparse.Source: RunAt, ReadAt and returned Runs must support arbitrary,
// repeated and concurrent reads. A sequential sparse.Source is not sufficient.
// Disk owns a successfully supplied source and closes it exactly once after
// all admitted reads and required verification have ended, including on error.
type TemplateSource interface {
	sparse.Source
	io.Closer
}

// TemplateFactory opens a new, independently owned source only after the fresh
// destination claim is acquired and its absence is checked again. Existing
// diffs never call it. On error the factory releases its own partial resources
// and returns a nil source; success transfers ownership to Disk. The source's
// lifetime must not be tied to an earlier preparation request context.
type TemplateFactory func() (TemplateSource, error)

// TemplateVerifier is an optional completion requirement of an identified
// template. Verify must validate the entire selected source, including content
// overwritten by foreground writes, and may run concurrently with frontend
// source reads. It must honor ctx and use the same immutable carrier and identity
// as the supplied source. WaitMaterialReady cannot succeed before Verify does.
// Raw images and deterministic generators need not implement this interface.
type TemplateVerifier interface {
	Verify(context.Context) error
}

func validateTemplateRun(run sparse.Run, offset, end uint64) error {
	if run == nil || run.Offset() != offset || run.End() <= offset || run.End() > end {
		return fmt.Errorf("diffstore: invalid template run boundary at %d", offset)
	}
	if run.Kind() != sparse.Hole && run.Kind() != sparse.Zero && run.Kind() != sparse.Data {
		return fmt.Errorf("diffstore: invalid template run kind at %d", offset)
	}
	return nil
}

// readTemplateBase reads the effective sparse view without taking ownership.
// Sub-page Holes inherit base; Zero masks it. Use this for both lazy reads and
// complete boundary pages written by materialization, so expanding a run to a
// page never replaces its adjacent Hole bytes with zeros from Source.ReadAt.
func readTemplateBase(ctx context.Context, source sparse.Source, base BlockReader, p []byte, offset uint64) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	done := 0
	for done < len(p) {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		pos := offset + uint64(done)
		length := uint64(len(p) - done)
		kind := sparse.Hole
		if source != nil && pos < source.Size() {
			length = min(length, source.Size()-pos)
			run, err := source.RunAt(pos, length)
			if err != nil {
				return done, err
			}
			if err := validateTemplateRun(run, pos, pos+length); err != nil {
				return done, err
			}
			kind, length = run.Kind(), run.End()-pos
		}
		chunk := p[done : done+int(length)]
		switch kind {
		case sparse.Data:
			n, err := readretry.ReadAt(ctx, len(chunk), func() (int, error) {
				return source.ReadAt(ctx, chunk, pos)
			})
			if err := templateReadResult(n, len(chunk), err); err != nil {
				return done + n, err
			}
		case sparse.Zero:
			clear(chunk)
		case sparse.Hole:
			if base != nil && base.Size() > 0 && pos < uint64(base.Size()) {
				readLen := min(length, uint64(base.Size())-pos)
				n, err := readBlock(ctx, base, chunk[:readLen], int64(pos))
				if err := templateReadResult(n, int(readLen), err); err != nil {
					return done + n, err
				}
				clear(chunk[n:])
			} else {
				clear(chunk)
			}
		}
		done += len(chunk)
	}
	return done, nil
}

func templateReadResult(n, want int, err error) error {
	if readretry.IsTerminal(err) || readerr.IsPermanent(err) || err != nil && !(errors.Is(err, io.EOF) && n == want) {
		return err
	}
	if n != want {
		return io.ErrUnexpectedEOF
	}
	return nil
}
