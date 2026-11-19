package diffstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// Test fixtures supply the same logical Source contract as production artifact
// openers. Parsing file references and choosing carriers belongs to artifact.
type testFileTemplate struct {
	sparse.Source
	f *os.File
}

func (s *testFileTemplate) Close() error { return s.f.Close() }

func openTestRawTemplate(path string) (TemplateSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (TemplateSource, error) { return nil, errors.Join(err, f.Close()) }
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return fail(fmt.Errorf("invalid raw fixture"))
	}
	holes, err := sparse.ProbeHoles(f)
	if err != nil {
		return fail(err)
	}
	source, err := sparse.NewSource(f, uint64(info.Size()), holes)
	if err != nil {
		return fail(err)
	}
	return &testFileTemplate{Source: source, f: f}, nil
}

func rawTemplateFactory(path string) TemplateFactory {
	return func() (TemplateSource, error) { return openTestRawTemplate(path) }
}

func openTestActiveTemplate(path string, encryption *diffEncryption) (TemplateSource, error) {
	diff, err := openExistingDiffFile(path, encryption, false, true)
	if err != nil {
		return nil, err
	}
	return &singleDiffSource{diff: diff}, nil
}

func validateTestRawTemplateExt4(ctx context.Context, path string, base BlockReader) error {
	source, err := openTestRawTemplate(path)
	if err != nil {
		return err
	}
	return errors.Join(ValidateDiffTemplateExt4(ctx, source, base), source.Close())
}

// initializeFreshDiffFile builds a synchronous storage fixture. Production
// creation uses OpenDisk and its owned source verification/materialization path.
func initializeFreshDiffFile(path string, logicalSize int64, encryption *diffEncryption, template TemplateSource) (*diffFile, error) {
	init := DiffInit{CreateSize: logicalSize}
	fresh, err := createFreshDiffFile(path, init, encryption)
	if err != nil {
		return nil, err
	}
	defer fresh.abort()
	if template != nil {
		if err := seedDiffTemplate(context.Background(), fresh.diff, template); err != nil {
			return nil, fmt.Errorf("diffstore: seed diff template: %w", err)
		}
	}
	if err := fresh.diff.Sync(); err != nil {
		return nil, fmt.Errorf("diffstore: sync fresh diff: %w", err)
	}
	if err := fresh.diff.Close(); err != nil {
		fresh.diff = nil
		return nil, fmt.Errorf("diffstore: close fresh diff: %w", err)
	}
	fresh.diff = nil
	if err := fresh.commit(); err != nil {
		return nil, err
	}
	return openExistingDiffFile(path, encryption, encryption != nil, false)
}

func seedDiffTemplate(ctx context.Context, target diffBackend, source TemplateSource) error {
	block := make([]byte, cowBlockSize)
	defer clear(block)
	var nextSeed uint64
	for offset := uint64(0); offset < source.Size(); {
		run, err := source.RunAt(offset, source.Size()-offset)
		if err != nil {
			return err
		}
		if err := validateTemplateRun(run, offset, source.Size()); err != nil {
			return err
		}
		kind, end := run.Kind(), run.End()
		if kind != sparse.Hole {
			start := uint64(int64(offset) / cowBlockSize * cowBlockSize)
			if start < nextSeed {
				start = nextSeed
			}
			seedEnd := uint64(alignUp(int64(end), cowBlockSize))
			for position := start; position < seedEnd; {
				if err := ctx.Err(); err != nil {
					return err
				}
				if kind == sparse.Zero && position >= offset && position+cowBlockSize <= end {
					zeroEnd := min(end/cowBlockSize*cowBlockSize, position+64*cowBlockSize)
					if err := target.discard(ctx, int64(position), int64(zeroEnd-position)); err != nil {
						return err
					}
					position = zeroEnd
					continue
				}
				read, err := source.ReadAt(ctx, block, position)
				if err := templateReadResult(read, len(block), err); err != nil {
					return err
				}
				written, err := target.WriteAt(block, int64(position))
				if err != nil {
					return err
				}
				if written != len(block) {
					return io.ErrShortWrite
				}
				position += cowBlockSize
			}
			nextSeed = seedEnd
		}
		offset = end
	}
	return nil
}
