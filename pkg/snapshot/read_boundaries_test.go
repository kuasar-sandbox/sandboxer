package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

type boundaryStream struct {
	sparse.Source
	cause            error
	short            bool
	plainEOF         bool
	cancel           func()
	calls            int
	metadataFailures int
	closes           int
}

func (s *boundaryStream) Close() error { s.closes++; return nil }
func (s *boundaryStream) RunAt(offset, limit uint64) (sparse.Run, error) {
	s.calls++
	if s.calls <= s.metadataFailures {
		if s.cancel != nil {
			s.cancel()
		}
		return nil, context.DeadlineExceeded
	}
	return s.Source.RunAt(offset, limit)
}
func (s *boundaryStream) ReadAt(ctx context.Context, buf []byte, off uint64) (int, error) {
	n, err := s.Source.ReadAt(ctx, buf, off)
	if s.cause != nil {
		if s.short {
			n /= 2
		}
		return n, readerr.Mark(s.cause, false)
	}
	if s.plainEOF {
		return n, io.EOF
	}
	return n, err
}
func TestReadBoundaryMergeMetadataRetry(t *testing.T) {
	src, err := sparse.NewSource(bytes.NewReader(make([]byte, 16)), 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &boundaryStream{Source: src, metadataFailures: 2}
	layer, _, err := prepareMergeBase(context.Background(), s, 16)
	if err != nil {
		t.Fatalf("required metadata abandoned after %d attempts: %v", s.calls, err)
	}
	defer layer.Close()
	if s.calls != 3 {
		t.Fatalf("calls=%d", s.calls)
	}
}
func TestReadBoundaryMergeFullTerminal(t *testing.T) {
	for _, short := range []bool{false, true} {
		for _, cause := range []error{io.EOF, syscall.EAGAIN} {
			for _, stage := range []string{"reader", "merged", "source", "run"} {
				t.Run(fmt.Sprintf("short=%v/%v/%s", short, cause, stage), func(t *testing.T) {
					src, err := sparse.NewSource(bytes.NewReader(make([]byte, 16)), 16, nil)
					if err != nil {
						t.Fatal(err)
					}
					s := &boundaryStream{Source: src, cause: cause, short: short}
					var _ fetch.Stream = s
					base, holes, err := prepareMergeBase(context.Background(), s, 16)
					if err != nil {
						t.Fatal(err)
					}
					defer base.Close()
					merged, mergedHoles := mergeSparse(bytes.NewReader(make([]byte, 16)), []sparse.Extent{{Offset: 0, Size: 16}}, base, holes, 16)
					view := &seekerSource{rs: merged, size: 16, holes: mergedHoles}
					buf := make([]byte, 16)
					var n int
					switch stage {
					case "reader":
						n, err = base.Read(buf)
					case "merged":
						n, err = merged.Read(buf)
					case "source":
						n, err = view.ReadAt(context.Background(), buf, 0)
					case "run":
						var run sparse.Run
						run, err = view.RunAt(0, 16)
						if err == nil {
							n, err = run.ReadAt(context.Background(), buf, 0)
						}
					}
					if !readretry.IsTerminal(err) || !readerr.IsPermanent(err) || !errors.Is(err, cause) {
						t.Fatalf("%s swallowed terminal: n=%d err=%v", stage, n, err)
					}
				})
			}
		}
	}
}

func TestMergeMetadataCancellationClosesLayer(t *testing.T) {
	src, err := sparse.NewSource(bytes.NewReader(make([]byte, 16)), 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("capture stopped")
	s := &boundaryStream{Source: src, metadataFailures: 1, cancel: func() { cancel(cause) }}
	layer, _, err := prepareMergeBase(ctx, s, 16)
	if layer != nil || !readretry.IsTerminal(err) || !errors.Is(err, cause) || s.closes != 1 || s.calls != 1 {
		t.Fatalf("canceled preparation: layer=%v err=%v closes=%d calls=%d", layer, err, s.closes, s.calls)
	}
}

func TestMergeFullOrdinaryEOFRemainsValid(t *testing.T) {
	want := bytes.Repeat([]byte{0x41}, 16)
	src, err := sparse.NewSource(bytes.NewReader(want), 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, holes, err := prepareMergeBase(context.Background(), &boundaryStream{Source: src, plainEOF: true}, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	merged, mergedHoles := mergeSparse(bytes.NewReader(make([]byte, 16)), []sparse.Extent{{Size: 16}}, base, holes, 16)
	source := &seekerSource{rs: merged, size: 16, holes: mergedHoles}
	run, err := source.RunAt(0, 16)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := run.ReadAt(context.Background(), buf, 0)
	if err != nil || n != len(buf) || !bytes.Equal(buf, want) {
		t.Fatalf("ordinary EOF: n=%d err=%v bytes=%x", n, err, buf)
	}
}
