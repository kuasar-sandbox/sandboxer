package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"syscall"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

type boundaryInspectStream struct {
	sparse.Source
	cause error
	calls int
}

func (*boundaryInspectStream) Close() error { return nil }
func (s *boundaryInspectStream) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	s.calls++
	n, err := s.Source.ReadAt(ctx, p, off)
	if s.cause != nil {
		return n, readerr.Mark(s.cause, false)
	}
	return n, err
}

type locationRetrySource struct {
	sparse.Source
	metadataErrors, readErrors []error
	metadataCalls, readCalls   int
	cancel                     func()
}

func (s *locationRetrySource) RunAt(offset, limit uint64) (sparse.Run, error) {
	s.metadataCalls++
	if s.metadataCalls <= len(s.metadataErrors) {
		if s.cancel != nil {
			s.cancel()
		}
		return nil, s.metadataErrors[s.metadataCalls-1]
	}
	return s.Source.RunAt(offset, limit)
}

func (s *locationRetrySource) ReadAt(ctx context.Context, buf []byte, offset uint64) (int, error) {
	s.readCalls++
	n, err := s.Source.ReadAt(ctx, buf, offset)
	if s.readCalls <= len(s.readErrors) {
		if s.cancel != nil {
			s.cancel()
		}
		// An unsuccessful attempt may have overwritten the whole buffer.
		return n, s.readErrors[s.readCalls-1]
	}
	return n, err
}

func TestLocationValidationRetriesOnlySourceReads(t *testing.T) {
	source, err := sparse.NewSource(bytes.NewReader(make([]byte, 4096)), 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	sequence := []error{errors.New("temporary access"), context.DeadlineExceeded, context.Canceled}
	s := &locationRetrySource{Source: source, metadataErrors: sequence, readErrors: sequence}
	if err := consumeLocationSource(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.metadataCalls != 4 || s.readCalls != 4 {
		t.Fatalf("metadata=%d payload=%d", s.metadataCalls, s.readCalls)
	}
}

func TestLocationValidationPreservesTerminalAndOrdinaryEOF(t *testing.T) {
	for _, boundary := range []string{"metadata", "payload"} {
		for _, cause := range []error{io.EOF, syscall.EAGAIN} {
			t.Run(boundary+"/"+cause.Error(), func(t *testing.T) {
				source, err := sparse.NewSource(bytes.NewReader(make([]byte, 4096)), 4096, nil)
				if err != nil {
					t.Fatal(err)
				}
				s := &locationRetrySource{Source: source}
				if boundary == "metadata" {
					s.metadataErrors = []error{readerr.Mark(cause, false)}
				} else {
					s.readErrors = []error{readerr.Mark(cause, false)}
				}
				err = consumeLocationSource(context.Background(), s)
				if !readretry.IsTerminal(err) || !readerr.IsPermanent(err) || !errors.Is(err, cause) {
					t.Fatalf("lost terminal: %v", err)
				}
				if s.metadataCalls != 1 || s.readCalls > 1 {
					t.Fatalf("retried terminal: %+v", s)
				}
			})
		}
		t.Run(boundary+"/cancel", func(t *testing.T) {
			source, err := sparse.NewSource(bytes.NewReader(make([]byte, 4096)), 4096, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			cause := errors.New("validation stopped")
			s := &locationRetrySource{Source: source, cancel: func() { cancel(cause) }}
			if boundary == "metadata" {
				s.metadataErrors = []error{context.DeadlineExceeded}
			} else {
				s.readErrors = []error{context.DeadlineExceeded}
			}
			if err := consumeLocationSource(ctx, s); !readretry.IsTerminal(err) || !errors.Is(err, cause) {
				t.Fatalf("lost operation cause: %v", err)
			}
		})
	}
	source, err := sparse.NewSource(bytes.NewReader(make([]byte, 4096)), 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumeLocationSource(context.Background(), &locationRetrySource{Source: source, readErrors: []error{io.EOF}}); err != nil {
		t.Fatalf("full ordinary EOF: %v", err)
	}
}

type boundaryInspectFetcher struct {
	source sparse.Source
	opens  int
}

func (f *boundaryInspectFetcher) OpenManifest(context.Context, store.ContentKey) (fetch.Stream, error) {
	f.opens++
	s := &boundaryInspectStream{Source: f.source}
	if f.opens == 1 {
		s.cause = io.EOF
	}
	return s, nil
}
func TestReadBoundaryInspectCannotFallbackAfterTerminal(t *testing.T) {
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: snapshot.SnapshotConfigVersion, SandboxRef: "manifest://" + publishTestSHA})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := sparse.NewSource(bytes.NewReader(make([]byte, 4096)), 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := snapshotfile.BuildSource(payload, []byte("{}"), []byte("{}"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := &boundaryInspectFetcher{source: source}
	s := &ProcessStorage{fetcher: &onDemandManifestFetcher{inner: f}}
	info, err := s.Inspect(context.Background(), "manifest://"+publishTestSHA, nil)
	if info != nil || !readretry.IsTerminal(err) || !readerr.IsPermanent(err) || !errors.Is(err, io.EOF) {
		t.Fatalf("Inspect accepted fallback after terminal: info=%v err=%v opens=%d", info, err, f.opens)
	}
	if f.opens != 1 {
		t.Fatalf("reopened after terminal: %d", f.opens)
	}
}
