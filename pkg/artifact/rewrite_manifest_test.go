package artifact

import (
	"bytes"
	"context"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"path/filepath"
	"testing"
)

func TestRewriteManifestIdentityDomainsCompareContent(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	payload := bytes.Repeat([]byte{0x35}, 8192)
	refs := make([]string, 2)
	for i, salt := range []string{"old-domain", "new-domain"} {
		ing, err := cfg.NewIngester(storage.CustomerKeyFunc(), func() ([]byte, error) { return []byte(salt), nil })
		if err != nil {
			t.Fatal(err)
		}
		result, err := ing.Ingest(ctx, publishSource(t, payload), ingest.IngestOption{})
		closeErr := ing.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("ingest %v %v", err, closeErr)
		}
		refs[i] = "manifest://" + manifest.HexKey(result.ManifestKey)
	}
	if refs[0] == refs[1] {
		t.Fatal("test domains did not change object identities")
	}
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", storage.LocalCodec(), storage.LocalRequired(), nil)
	_, runtime := publishPortable(t, refs[0])
	source := rewriteSaveE(t, sink, runtime, rewriteSparse(t, payload))
	parsed, _ := manifest.ParseRef(source)
	publisher := rewriteTarget(t, storage, output)
	result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, parsed.Path), RewriteOptions{Replacements: []RefReplacement{{Old: refs[0], New: refs[1]}}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if root.Portable.Boot.Root.BaseFromRefs[0] != refs[1] {
		t.Fatal("replacement Manifest ref lost")
	}
}
