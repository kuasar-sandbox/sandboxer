package diffstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

type templateRegion struct {
	end  uint64
	kind sparse.RunKind
}

type regionTemplate struct {
	data    []byte
	regions []templateRegion
}

func (s *regionTemplate) Size() uint64 { return uint64(len(s.data)) }
func (s *regionTemplate) Close() error { return nil }
func (s *regionTemplate) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.Size() {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, fmt.Errorf("zero limit")
	}
	for _, r := range s.regions {
		if offset < r.end {
			return regionTemplateRun{s, offset, min(r.end, offset+min(limit, s.Size()-offset)), r.kind}, nil
		}
	}
	return nil, fmt.Errorf("incomplete test source")
}
func (s *regionTemplate) ReadAt(ctx context.Context, p []byte, offset uint64) (int, error) {
	if offset >= s.Size() {
		return 0, io.EOF
	}
	n := min(uint64(len(p)), s.Size()-offset)
	done := 0
	for uint64(done) < n {
		r, err := s.RunAt(offset+uint64(done), n-uint64(done))
		if err != nil {
			return done, err
		}
		count, err := r.ReadAt(ctx, p[done:done+int(r.End()-r.Offset())], 0)
		done += count
		if err != nil {
			return done, err
		}
	}
	if done < len(p) {
		return done, io.EOF
	}
	return done, nil
}

type regionTemplateRun struct {
	source      *regionTemplate
	offset, end uint64
	kind        sparse.RunKind
}

func (r regionTemplateRun) Offset() uint64       { return r.offset }
func (r regionTemplateRun) End() uint64          { return r.end }
func (r regionTemplateRun) Kind() sparse.RunKind { return r.kind }
func (r regionTemplateRun) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off > r.end-r.offset || uint64(len(p)) > r.end-r.offset-off {
		return 0, fmt.Errorf("read outside run")
	}
	if r.kind == sparse.Data {
		copy(p, r.source.data[r.offset+off:])
	} else {
		clear(p)
	}
	return len(p), nil
}

type fullRunGatedSource struct {
	TemplateSource
	gate *workerGate
}

func (s *fullRunGatedSource) RunAt(off, limit uint64) (sparse.Run, error) {
	if off == 0 && limit == s.Size() {
		s.gate.wait()
	}
	return s.TemplateSource.RunAt(off, limit)
}

func TestTemplateSubpageCompositionMatchesBeforeAndAfterMaterialization(t *testing.T) {
	const size = 3 * cowBlockSize
	for _, checkpoint := range []bool{false, true} {
		for _, encrypted := range []bool{false, true} {
			for _, withBase := range []bool{false, true} {
				t.Run(fmt.Sprintf("checkpoint=%v/encrypted=%v/base=%v", checkpoint, encrypted, withBase), func(t *testing.T) {
					source := &regionTemplate{data: bytes.Repeat([]byte{0x51}, size), regions: []templateRegion{
						{127, sparse.Hole}, {251, sparse.Data}, {401, sparse.Zero}, {4095, sparse.Hole},
						{4103, sparse.Data}, {8190, sparse.Hole}, {8192, sparse.Zero}, {size, sparse.Hole},
					}}
					want := make([]byte, size)
					var base BlockReader
					if withBase {
						baseBytes := bytes.Repeat([]byte{0xa7}, size-257)
						base = &fakeReader{data: baseBytes}
						copy(want, baseBytes)
					}
					start := uint64(0)
					for _, r := range source.regions {
						switch r.kind {
						case sparse.Data:
							copy(want[start:r.end], source.data[start:r.end])
						case sparse.Zero:
							clear(want[start:r.end])
						}
						start = r.end
					}
					gate := newWorkerGate()
					defer gate.open()
					options := templateOptions(checkpoint)
					if encrypted {
						options = append(options, WithDiffEncryption(testDiffKey(44), true))
					}
					path := filepath.Join(t.TempDir(), "active")
					disk, err := OpenDisk(path, base, DiffInit{Template: func() (TemplateSource, error) { return &fullRunGatedSource{source, gate}, nil }}, options...)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { gate.open(); _ = disk.Close() }()
					awaitSignal(t, gate.entered)
					got := make([]byte, size)
					// Short requests can read the source while the materializer's
					// initial whole-source metadata query is held by the test.
					for off := 0; off < size; {
						end := min(off+257, size)
						if _, err := disk.ReadAt(got[off:end], int64(off)); err != nil {
							t.Fatal(err)
						}
						off = end
					}
					if !bytes.Equal(got, want) {
						t.Fatal("lazy source/base composition mismatch")
					}
					gate.open()
					if err := disk.WaitMaterialized(context.Background()); err != nil {
						t.Fatal(err)
					}
					clear(got)
					if _, err := disk.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
						t.Fatal("materialization lost subpage Hole/Zero/Data", err)
					}
					if err := disk.Close(); err != nil {
						t.Fatal(err)
					}
					reopened, err := OpenDisk(path, base, DiffInit{Existing: true}, options...)
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.Close()
					clear(got)
					if _, err := reopened.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
						t.Fatal("reopened composition changed", err)
					}
				})
			}
		}
	}
}

func TestTemplateExt4PreflightBorrowsAndComposesSplitMagic(t *testing.T) {
	for _, first := range []sparse.RunKind{sparse.Hole, sparse.Data, sparse.Zero} {
		t.Run(fmt.Sprint(first), func(t *testing.T) {
			data := make([]byte, cowBlockSize)
			data[ext4MagicOffset] = 0x53
			data[ext4MagicOffset+1] = 0xef
			base := &fakeReader{data: append([]byte(nil), data...)}
			base.data[ext4MagicOffset+1] = 0
			source := &observedTemplate{TemplateSource: &regionTemplate{data: data, regions: []templateRegion{{uint64(ext4MagicOffset + 1), first}, {cowBlockSize, sparse.Data}}}}
			err := ValidateDiffTemplateExt4(context.Background(), source, base)
			if (err == nil) != (first != sparse.Zero) {
				t.Fatalf("split magic validation: %v", err)
			}
			if source.closes.Load() != 0 || source.verifies.Load() != 0 {
				t.Fatal("preflight consumed source ownership or full verification")
			}
		})
	}
}

type failedReadTemplate struct {
	TemplateSource
	err   error
	short bool
}

func (s failedReadTemplate) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	n, _ := s.TemplateSource.ReadAt(ctx, p, off)
	if s.short {
		n--
	}
	return n, s.err
}

func TestTemplateReadNeverAcceptsFullPermanentEOFOrShortRead(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		short bool
	}{
		{"permanent-eof", readerr.Mark(io.EOF, false), false},
		{"short", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := failedReadTemplate{TemplateSource: newMemoryTemplate(t, cowBlockSize), err: test.cause, short: test.short}
			p := make([]byte, 8)
			_, err := readTemplateBase(context.Background(), source, nil, p, 0)
			if err == nil {
				t.Fatal("failed read accepted")
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("read error lost: %v", err)
			}
		})
	}
}

func TestTemplatePermanentReadFailureIsFatalAndUnpublished(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			cause := readerr.Mark(io.EOF, false)
			source := &observedTemplate{TemplateSource: failedReadTemplate{TemplateSource: newMemoryTemplate(t, cowBlockSize), err: cause}}
			dir := t.TempDir()
			disk, err := OpenDisk(filepath.Join(dir, "active"), nil, DiffInit{Template: func() (TemplateSource, error) { return source, nil }}, templateOptions(checkpoint)...)
			if err != nil {
				t.Fatal(err)
			}
			if err := disk.WaitMaterialized(context.Background()); !errors.Is(err, cause) {
				t.Fatalf("materialization accepted permanent EOF: %v", err)
			}
			_ = disk.Close()
			if source.closes.Load() != 1 {
				t.Fatalf("source closes=%d", source.closes.Load())
			}
			assertNoFreshResources(t, dir)
		})
	}
}

type retryReadTemplate struct {
	TemplateSource
	attempts   atomic.Int32
	started    chan struct{}
	alwaysFail bool
}

func (s *retryReadTemplate) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	attempt := s.attempts.Add(1)
	if attempt == 1 && s.started != nil {
		close(s.started)
	}
	if s.alwaysFail || attempt == 1 {
		// A later successful attempt must replace the whole range, including
		// bytes left by a partially completed failed attempt.
		n := len(p) / 2
		for i := range p[:n] {
			p[i] = 0xa5
		}
		return n, readerr.Mark(errors.New("temporary template read failure"), true)
	}
	return s.TemplateSource.ReadAt(ctx, p, off)
}

func TestTemplateTemporaryReadFailureRecovers(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			reads := &retryReadTemplate{TemplateSource: newMemoryTemplate(t, cowBlockSize)}
			source := &observedTemplate{TemplateSource: reads}
			options := append(templateOptions(checkpoint), WithDiscardOnClose())
			disk, err := OpenDisk(filepath.Join(t.TempDir(), "active"), nil, DiffInit{Template: func() (TemplateSource, error) { return source, nil }}, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer disk.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := disk.WaitMaterialized(ctx); err != nil {
				t.Fatalf("temporary source failure prevented publication: %v", err)
			}
			got := make([]byte, cowBlockSize)
			if n, err := disk.ReadAt(got, 0); err != nil || n != len(got) {
				t.Fatalf("read materialized image: n=%d err=%v", n, err)
			}
			if !bytes.Equal(got, bytes.Repeat([]byte{0x53}, len(got))) {
				t.Fatal("materialized image retained bytes from failed attempt")
			}
			if reads.attempts.Load() != 2 || source.verifies.Load() != 1 || source.closes.Load() != 1 {
				t.Fatalf("attempts=%d verifies=%d closes=%d", reads.attempts.Load(), source.verifies.Load(), source.closes.Load())
			}
		})
	}
}

func TestTemplateReadRetryCancellation(t *testing.T) {
	source := &retryReadTemplate{TemplateSource: newMemoryTemplate(t, cowBlockSize), alwaysFail: true, started: make(chan struct{})}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("template read request stopped")
	done := make(chan error, 1)
	go func() {
		_, err := readTemplateBase(ctx, source, nil, make([]byte, cowBlockSize), 0)
		done <- err
	}()
	awaitSignal(t, source.started)
	cancel(cause)
	select {
	case err := <-done:
		if !readretry.IsTerminal(err) || !errors.Is(err, cause) {
			t.Fatalf("template retry lost request cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("template retry did not stop on cancellation")
	}
}

func TestTemplateReadRetryDisposableClose(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			reads := &retryReadTemplate{TemplateSource: newMemoryTemplate(t, cowBlockSize), alwaysFail: true, started: make(chan struct{})}
			source := &observedTemplate{TemplateSource: reads}
			dir := t.TempDir()
			options := append(templateOptions(checkpoint), WithDiscardOnClose())
			disk, err := OpenDisk(filepath.Join(dir, "active"), nil, DiffInit{Template: func() (TemplateSource, error) { return source, nil }}, options...)
			if err != nil {
				t.Fatal(err)
			}
			awaitSignal(t, reads.started)
			closed := make(chan error, 1)
			go func() { closed <- disk.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("disposable close failed to cancel source retry: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("disposable Close did not join retrying materializer")
			}
			if source.closes.Load() != 1 || source.verifies.Load() != 0 {
				t.Fatalf("closes=%d verifies=%d", source.closes.Load(), source.verifies.Load())
			}
			assertNoFreshResources(t, dir)
		})
	}
}

func TestTemplateRetainedReadRetryExplicitDiscardClose(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			reads := &retryReadTemplate{TemplateSource: newMemoryTemplate(t, cowBlockSize), alwaysFail: true, started: make(chan struct{})}
			source := &observedTemplate{TemplateSource: reads}
			dir := t.TempDir()
			// This writer would normally finish materialization on Close. Its
			// owner can still abandon a transaction before publishing the bind.
			disk, err := OpenDisk(filepath.Join(dir, "active"), nil, DiffInit{Template: func() (TemplateSource, error) { return source, nil }}, templateOptions(checkpoint)...)
			if err != nil {
				t.Fatal(err)
			}
			awaitSignal(t, reads.started)
			closed := make(chan error, 1)
			go func() { closed <- disk.CloseDiscarding() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("explicit discard failed: %v", err)
				}
			case <-time.After(5 * time.Second):
				// Let a broken retained-close implementation release its worker
				// before failing this regression.
				disk.materialCancel()
				<-closed
				t.Fatal("explicit discard waited for the abandoned template to recover")
			}
			if err := disk.Close(); err != nil {
				t.Fatalf("ordinary Close did not reuse completed shutdown: %v", err)
			}
			if source.closes.Load() != 1 || source.verifies.Load() != 0 {
				t.Fatalf("closes=%d verifies=%d", source.closes.Load(), source.verifies.Load())
			}
			assertNoFreshResources(t, dir)
		})
	}
}
