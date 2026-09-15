package sandboxfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

type boundaryZIPStream struct {
	sparse.Source
	calls, fail int
	cause       error
}

func (*boundaryZIPStream) Close() error { return nil }
func (s *boundaryZIPStream) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	s.calls++
	n, e := s.Source.ReadAt(ctx, p, off)
	if s.calls == s.fail {
		return n, s.cause
	}
	return n, e
}
func TestReadBoundaryFlattenedZIPFullTerminal(t *testing.T) {
	payload := fakeEROFS()
	body := append(append([]byte(nil), payload...), legacyConfigZIP(t, []byte(`{"review":true}`))...)
	baseline := &boundaryZIPStream{Source: dataSource(t, body)}
	_, _, err := parseFlattenedImageArchive(context.Background(), baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{readerr.Mark(io.EOF, false), readerr.Mark(syscall.EAGAIN, false), io.EOF} {
		for fail := 1; fail <= baseline.calls; fail++ {
			t.Run(fmt.Sprintf("%v/%d", cause, fail), func(t *testing.T) {
				s := &boundaryZIPStream{Source: dataSource(t, body), fail: fail, cause: cause}
				_, _, err := parseFlattenedImageArchive(context.Background(), s)
				if cause == io.EOF {
					if err != nil {
						t.Fatalf("full ordinary EOF: %v", err)
					}
					return
				}
				if !errors.Is(err, cause) || !readretry.IsTerminal(err) || !readerr.IsPermanent(err) {
					t.Fatalf("ZIP read #%d swallowed terminal: calls=%d err=%v", fail, s.calls, err)
				}
			})
		}
	}
}

type erofsRetrySource struct {
	sparse.Source
	metadataErrors, readErrors []error
	metadataCalls, readCalls   int
	cancel                     func()
}

func (s *erofsRetrySource) RunAt(offset, limit uint64) (sparse.Run, error) {
	s.metadataCalls++
	if s.metadataCalls <= len(s.metadataErrors) {
		if s.cancel != nil {
			s.cancel()
		}
		return nil, s.metadataErrors[s.metadataCalls-1]
	}
	return s.Source.RunAt(offset, limit)
}

func (s *erofsRetrySource) ReadAt(ctx context.Context, buf []byte, offset uint64) (int, error) {
	s.readCalls++
	n, err := s.Source.ReadAt(ctx, buf, offset)
	if s.readCalls <= len(s.readErrors) {
		if s.cancel != nil {
			s.cancel()
		}
		return n, s.readErrors[s.readCalls-1]
	}
	return n, err
}

func TestEROFSBuildPrefixReadRecovery(t *testing.T) {
	sequence := []error{errors.New("temporary access"), context.DeadlineExceeded, context.Canceled}
	source := &erofsRetrySource{Source: dataSource(t, fakeEROFS()), metadataErrors: sequence, readErrors: sequence}
	view, size, err := prepareEROFSBuildPayload(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if size != uint64(len(fakeEROFS())) || view.Size() != size || source.metadataCalls != 4 || source.readCalls != 4 {
		t.Fatalf("size=%d metadata=%d payload=%d", size, source.metadataCalls, source.readCalls)
	}
	// Cached prefix bytes belong to the single successful read, not a replay
	// of BuildSource or of a publisher/ingester write.
	buf := make([]byte, erofsBuildProbeBytes)
	if _, err := view.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if source.readCalls != 4 {
		t.Fatalf("prefix reread: %d", source.readCalls)
	}
}

func TestEROFSBuildPrefixTerminalAndCancellation(t *testing.T) {
	for _, boundary := range []string{"metadata", "payload"} {
		for _, cause := range []error{io.EOF, syscall.EAGAIN} {
			t.Run(boundary+"/"+cause.Error(), func(t *testing.T) {
				source := &erofsRetrySource{Source: dataSource(t, fakeEROFS())}
				if boundary == "metadata" {
					source.metadataErrors = []error{readerr.Mark(cause, false)}
				} else {
					source.readErrors = []error{readerr.Mark(cause, false)}
				}
				view, _, err := prepareEROFSBuildPayload(context.Background(), source)
				if view != nil || !readretry.IsTerminal(err) || !readerr.IsPermanent(err) || !errors.Is(err, cause) {
					t.Fatalf("accepted failed prefix: view=%v err=%v", view, err)
				}
			})
		}
		t.Run(boundary+"/cancel", func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			cause := errors.New("build stopped")
			source := &erofsRetrySource{Source: dataSource(t, fakeEROFS()), cancel: func() { cancel(cause) }}
			if boundary == "metadata" {
				source.metadataErrors = []error{context.DeadlineExceeded}
			} else {
				source.readErrors = []error{context.DeadlineExceeded}
			}
			view, _, err := prepareEROFSBuildPayload(ctx, source)
			if view != nil || !readretry.IsTerminal(err) || !errors.Is(err, cause) {
				t.Fatalf("lost operation cause: view=%v err=%v", view, err)
			}
		})
	}
	source := &erofsRetrySource{Source: dataSource(t, fakeEROFS()), readErrors: []error{io.EOF}}
	if _, _, err := prepareEROFSBuildPayload(context.Background(), source); err != nil {
		t.Fatalf("full ordinary EOF: %v", err)
	}
}
