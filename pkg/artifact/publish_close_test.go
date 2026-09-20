package artifact

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

type reportSpyStream struct {
	sparse.Source
	reads, closes *int
	closeErr      error
}

func (s *reportSpyStream) ReadAt(ctx context.Context, b []byte, off uint64) (int, error) {
	*s.reads++
	return s.Source.ReadAt(ctx, b, off)
}
func (s *reportSpyStream) Close() error { *s.closes++; return s.closeErr }

type reportSpyFetcher struct {
	source               sparse.Source
	key                  store.ContentKey
	opens, reads, closes int
	closeErr             error
}

func (f *reportSpyFetcher) OpenManifest(_ context.Context, key store.ContentKey) (fetch.Stream, error) {
	if key != f.key {
		return nil, errors.New("unexpected dependency scan")
	}
	f.opens++
	return &reportSpyStream{Source: f.source, reads: &f.reads, closes: &f.closes, closeErr: f.closeErr}, nil
}

func TestReportCloseFailureDoesNotReturnRoot(t *testing.T) {
	ctx := context.Background()
	runtime, _ := publishPortable(t, "")
	source, err := sandboxfile.BuildSource(sparse.Dense(bytes.NewReader(make([]byte, 4096)), 4096), nil, runtime)
	if err != nil {
		t.Fatal(err)
	}
	rootRef := "manifest://" + publishTestSHA
	key, _ := manifest.ParseKeyRef(rootRef)
	failure := errors.New("source close failed")
	spy := &reportSpyFetcher{source: source, key: key, closeErr: failure}
	storage := &ProcessStorage{fetcher: &onDemandManifestFetcher{inner: spy}}
	p := newPublisher(storage, nil, &recordingPublishTarget{}, nil)
	result, err := p.Publish(ctx, rootRef)
	if !errors.Is(err, failure) || result.Ref != "" || spy.closes != 1 {
		t.Fatalf("close result=%+v err=%v closes=%d", result, err, spy.closes)
	}
}
