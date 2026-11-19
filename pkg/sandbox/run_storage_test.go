package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/emptyext4"
)

type runVerifiedTemplateFetcher struct {
	failingManifestFetcher
	verified int
	closes   int
}

func (f *runVerifiedTemplateFetcher) OpenVerifiedManifest(context.Context, store.ContentKey) (fetch.Stream, error) {
	f.verified++
	return emptyext4.New(1 << 20)
}

func (f *runVerifiedTemplateFetcher) Close() error { f.closes++; return nil }

func TestRunStoragePreservesTemplateStoreWithScopedBase(t *testing.T) {
	storeFetcher := &runVerifiedTemplateFetcher{}
	owner, err := artifact.NewProcessStorageWithLocalAccess(nil, nil, nil, false, storeFetcher)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	baseFetcher := &failingManifestFetcher{}
	view, err := borrowRunStorage(RunOptions{Storage: owner, Fetcher: baseFetcher})
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if view.Fetcher() != baseFetcher || owner.Fetcher() != storeFetcher {
		t.Fatal("run changed the base source or the original Store")
	}
	source, err := view.OpenDiffTemplate(context.Background(), "manifest://"+strings.Repeat("a", 64), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.(diffstore.TemplateVerifier).Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if baseFetcher.calls != 0 || storeFetcher.verified != 1 || storeFetcher.calls != 0 || storeFetcher.closes != 0 {
		t.Fatalf("source isolation/lifetime: base=%d verified=%d ordinary=%d closes=%d", baseFetcher.calls, storeFetcher.verified, storeFetcher.calls, storeFetcher.closes)
	}
	defaultView, err := borrowRunStorage(RunOptions{Storage: owner})
	if err != nil {
		t.Fatal(err)
	}
	defer defaultView.Close()
	if defaultView.Fetcher() != storeFetcher {
		t.Fatal("no override lost the owner's base Fetcher")
	}
}

func TestRunStorageDoesNotElevateOpaqueFetcher(t *testing.T) {
	baseFetcher := &failingManifestFetcher{}
	view, err := borrowRunStorage(RunOptions{Fetcher: baseFetcher})
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if source, err := view.OpenDiffTemplate(context.Background(), "manifest://"+strings.Repeat("a", 64), "", nil); err == nil {
		source.Close()
		t.Fatal("opaque Fetcher was accepted as a verified template Store")
	}
	if baseFetcher.calls != 0 {
		t.Fatal("unverified Manifest source was opened")
	}
}
