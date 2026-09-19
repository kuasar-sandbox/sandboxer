package artifact

import (
	"bytes"
	"context"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteCollapsedLowerReferencesFailBeforeWrites(t *testing.T) {
	ctx := context.Background()
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := rewriteTarget(t, storage, output)
	refs := make([]string, 3)
	for i, name := range []string{"old-a", "old-b", "new-c"} {
		_, cfg := publishPortable(t, "")
		cfg.Metadata = map[string]string{"fixture": name}
		refs[i] = rewriteSaveE(t, sink, cfg, rewriteSparse(t, bytes.Repeat([]byte{0x39}, 4096)))
	}
	_, rootCfg := publishPortable(t, "")
	rootCfg.Boot.Root.BaseFromRefs = refs[:2]
	root := rewriteSaveE(t, sink, rootCfg, rewriteSparse(t, bytes.Repeat([]byte{0x41}, 4096)))
	parsed, err := manifest.ParseRef(root)
	if err != nil {
		t.Fatal(err)
	}
	options := RewriteOptions{Replacements: []RefReplacement{
		{Old: refs[0], New: refs[2]}, {Old: refs[1], New: refs[2]},
	}}
	result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, parsed.Path), options)
	if err == nil || result.Ref != "" {
		t.Fatalf("invalid duplicate refs accepted: %+v %v", result, err)
	}
	entries, readErr := os.ReadDir(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid reference plan wrote %d final artifacts before rejection: %v", len(entries), err)
	}
}
