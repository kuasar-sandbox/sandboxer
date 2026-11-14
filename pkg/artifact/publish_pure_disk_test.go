package artifact

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestPublishPureDiskPreservesSandboxZIPBytesAndShortParent(t *testing.T) {
	ctx := context.Background()
	in := t.TempDir()
	sink := snapshot.NewFileSink(in, "disk", nil, false, nil)
	defer sink.Close()
	raw, cfg := publishPortable(t, "")
	const size = 16384
	// Seed a user disk with bytes which form a valid E at its logical EOF.
	empty, err := sandboxfile.BuildSource(rewriteSparse(t, []byte{0}), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{0x5a}, size-int(empty.Size())+1)
	clear(body[:4096])
	tricky, err := sandboxfile.BuildSource(rewriteSparse(t, body, sparse.Extent{Offset: 0, Size: 4096}), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	if tricky.Size() != size {
		t.Fatalf("aligned E-like payload size = %d", tricky.Size())
	}
	top, _, err := sink.AbsorbOverlaySource(ctx, tricky)
	if err != nil {
		t.Fatal(err)
	}
	parentBytes := bytes.Repeat([]byte{0x53}, 4096)
	parent, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(parentBytes), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Version = 2
	cfg.Boot.Disks = []config.PortableDiskConfig{{Name: "data", PureBlock: true, PortableRootConfig: config.PortableRootConfig{Base: top, BaseFromRefs: []string{parent}}}}
	cfg.Mounts = []config.MountConfig{{Source: "data", Type: "disk", Target: "/data"}}
	saved := rewriteSaveE(t, sink, cfg, rewriteSparse(t, make([]byte, 4096)))
	ref, _ := manifest.ParseRef(saved)
	expected := make([]byte, size)
	if _, err = tricky.ReadAt(ctx, expected, 0); err != nil {
		t.Fatal(err)
	}
	storage, err := NewProcessStorage(plaintextRefConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	for _, tc := range []struct {
		name    string
		options RewriteOptions
	}{{"publish", RewriteOptions{}}, {"reduce", RewriteOptions{ReduceAny: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			pub := rewriteTarget(t, storage, t.TempDir())
			result, err := pub.PublishWithOptions(ctx, filepath.Join(in, ref.Path), tc.options)
			if err != nil {
				t.Fatal(err)
			}
			root, err := sandboxfile.Open(ctx, rewriteOpen(t, pub, result.Ref))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			graph := root.Portable.Boot.Disks[0]
			opened := rewriteOpen(t, pub, graph.Base)
			defer opened.Close()
			if !graph.PureBlock || opened.Size() != size {
				t.Fatalf("published graph: pure=%v top bytes=%d", graph.PureBlock, opened.Size())
			}
			data := make([]byte, size)
			if _, err = opened.ReadAt(ctx, data, 0); err != nil {
				t.Fatal(err)
			}
			want := bytes.Clone(expected)
			if tc.options.ReduceAny {
				copy(want, parentBytes)
				if len(graph.BaseFromRefs) != 0 {
					t.Fatal("parent still present after reduction")
				}
			} else if len(graph.BaseFromRefs) != 1 {
				t.Fatal("missing short parent")
			}
			if !bytes.Equal(data, want) {
				t.Fatal("published disk bytes changed")
			}
		})
	}
}
