package artifact

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestDirtyPublishAndRewrite(t *testing.T) {
	for _, mode := range []string{"publish", "verified", "skip-verify", "reduce"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			input := t.TempDir()
			storage, err := NewProcessStorage(plaintextRefConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			sink := snapshot.NewFileSink(input, "dirty", nil, false, nil)
			publisher := rewriteTarget(t, storage, t.TempDir())
			_, ecfg := publishPortable(t, "")
			eRef := rewriteSaveE(t, sink, ecfg, rewriteSparse(t, make([]byte, 8192)))
			parentBytes := bytes.Repeat([]byte{0x37}, 8192)
			currentBytes := append([]byte(nil), parentBytes...)
			copy(currentBytes[4096:], bytes.Repeat([]byte{0x59}, 4096))
			parent := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: eRef}, rewriteSparse(t, parentBytes))
			cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: eRef, FromRefs: []string{parent}})
			if err != nil {
				t.Fatal(err)
			}
			dirty, err := snapshotfile.EncodeDirty(8192, []byte{2})
			if err != nil {
				t.Fatal(err)
			}
			source, err := snapshotfile.BuildSourceWithDirty(rewriteSparse(t, currentBytes), []byte("{}"), []byte("{}"), cfg, dirty)
			if err != nil {
				t.Fatal(err)
			}
			_, path, err := sink.AbsorbSnapshot(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			var opts RewriteOptions
			switch mode {
			case "verified", "skip-verify":
				ref, err := manifest.ParseRef(parent)
				if err != nil {
					t.Fatal(err)
				}
				replacement := ref
				replacement.Path = filepath.Join(input, ref.Path)
				opts, err = ParseRewriteOptions([]string{parent + "=" + replacement.String()}, nil, mode == "skip-verify")
				if err != nil {
					t.Fatal(err)
				}
			case "reduce":
				opts.ReduceAny = true
			}
			result, err := publisher.PublishWithOptions(ctx, path, opts)
			if err != nil {
				t.Fatal(err)
			}
			root, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if mode == "publish" || mode == "verified" {
				if !bytes.Equal(root.MemoryDirty, dirty) {
					t.Fatal("equivalent rebuild changed dirty entry")
				}
			} else if root.MemoryDirty != nil {
				t.Fatal("changed/unproved baseline retained clean annotations")
			}
			body, err := readPublishSource(ctx, root.Memory)
			if err != nil || !bytes.Equal(body, currentBytes) {
				t.Fatalf("memory changed: %v", err)
			}
		})
	}
}
