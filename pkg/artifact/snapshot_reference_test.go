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
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
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

func TestSnapshotReferenceOnlyReadsRoot(t *testing.T) {
	ctx := context.Background()
	rootRef := "manifest://" + publishTestSHA
	eRef := "manifest://" + manifest.HexKey(store.ContentKey{42})
	// A very large sparse payload makes any accidental materialization visible.
	memory := rewriteKindSource{kind: sparse.Hole, size: 1 << 40, err: errors.New("memory payload was read")}
	cfg, _ := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: eRef, FromRefs: []string{"manifest://" + manifest.HexKey(store.ContentKey{43})}})
	source, err := snapshotfile.BuildSource(memory, []byte("{}"), []byte("{}"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := manifest.ParseKeyRef(rootRef)
	spy := &reportSpyFetcher{source: source, key: key}
	storage := &ProcessStorage{fetcher: &onDemandManifestFetcher{inner: spy}}
	got, err := storage.SnapshotSandboxRef(ctx, rootRef, nil)
	if err != nil || got != eRef {
		t.Fatalf("E=%q err=%v", got, err)
	}
	if spy.opens != 1 || spy.closes != 1 || spy.reads > 16 {
		t.Fatalf("metadata access: %+v", spy)
	}
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
