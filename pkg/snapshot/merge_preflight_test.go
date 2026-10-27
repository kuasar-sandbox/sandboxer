package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

type lateMergeRunSource struct {
	sparse.Source
	run sparse.Run
	err error
}

func (s lateMergeRunSource) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= 4096 {
		return s.run, s.err
	}
	return s.Source.RunAt(offset, limit)
}

func TestMergePreflightValidatesLaterRuns(t *testing.T) {
	const size = 8192
	src, err := sparse.NewSource(bytes.NewReader(nil), size, []sparse.Extent{{Size: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		run  sparse.Run
		err  error
	}{
		{name: "nil"},
		{name: "wrong-offset", run: seekerRun{offset: 4097, end: size, kind: sparse.Data}},
		{name: "no-progress", run: seekerRun{offset: 4096, end: 4096, kind: sparse.Data}},
		{name: "past-end", run: seekerRun{offset: 4096, end: size + 1, kind: sparse.Data}},
		{name: "early-eof", err: io.EOF},
		{name: "truncated", err: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &boundaryStream{Source: lateMergeRunSource{Source: src, run: tc.run, err: tc.err}}
			err := ValidateMergeBaseWithOpener(context.Background(), "file://base.overlay", size, nil, false,
				func(context.Context, string) (fetch.Stream, error) { return s, nil })
			if tc.err != nil {
				if !errors.Is(err, tc.err) || !readretry.IsTerminal(err) {
					t.Fatalf("later run error=%v want terminal %v", err, tc.err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "invalid run") {
				t.Fatalf("later run error=%v want invalid run", err)
			}
			if s.calls != 2 || s.closes != 1 {
				t.Fatalf("calls=%d closes=%d want 2 and 1", s.calls, s.closes)
			}
		})
	}
}

func TestMergePreflightMetadataRetryAndCancellation(t *testing.T) {
	src, err := sparse.NewSource(bytes.NewReader(nil), 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("retry", func(t *testing.T) {
		s := &boundaryStream{Source: src, metadataFailures: 2}
		err := ValidateMergeBaseWithOpener(context.Background(), "file://base.overlay", 16, nil, false,
			func(context.Context, string) (fetch.Stream, error) { return s, nil })
		if err != nil || s.calls != 3 || s.closes != 1 {
			t.Fatalf("preflight: err=%v calls=%d closes=%d", err, s.calls, s.closes)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		cause := errors.New("preflight stopped")
		s := &boundaryStream{Source: src, metadataFailures: 1, cancel: func() { cancel(cause) }}
		err := ValidateMergeBaseWithOpener(ctx, "file://base.overlay", 16, nil, false,
			func(context.Context, string) (fetch.Stream, error) { return s, nil })
		if !readretry.IsTerminal(err) || !errors.Is(err, cause) || s.calls != 1 || s.closes != 1 {
			t.Fatalf("canceled preflight: err=%v calls=%d closes=%d", err, s.calls, s.closes)
		}
	})
}

type mergeCloseErrorStream struct {
	fetch.Stream
	err error
}

func (s mergeCloseErrorStream) Close() error {
	_ = s.Stream.Close()
	return s.err
}

func TestMergePreflightCloseAndSizeErrors(t *testing.T) {
	src, err := sparse.NewSource(bytes.NewReader(nil), 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("close failed")
	for _, size := range []int64{-1, 16, 17} {
		s := &boundaryStream{Source: src}
		err := ValidateMergeBaseWithOpener(context.Background(), "file://base.overlay", size, nil, false,
			func(context.Context, string) (fetch.Stream, error) {
				return mergeCloseErrorStream{Stream: s, err: closeErr}, nil
			})
		if size == 16 {
			if !errors.Is(err, closeErr) || s.calls != 1 {
				t.Fatalf("successful validation: err=%v calls=%d", err, s.calls)
			}
		} else if err == nil || errors.Is(err, closeErr) || !strings.Contains(err.Error(), "expected layer size") || s.calls != 0 {
			t.Fatalf("invalid size %d: err=%v calls=%d", size, err, s.calls)
		}
		if s.closes != 1 {
			t.Fatalf("size=%d closes=%d want 1", size, s.closes)
		}
	}
}
