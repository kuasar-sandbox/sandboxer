package artifact

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestRewriteRejectsReplacementOverlapWithEveryReducedLayer(t *testing.T) {
	for _, memory := range []bool{false, true} {
		for _, skip := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				for _, targetOverlap := range []bool{false, true} {
					name := fmt.Sprintf("memory=%t/skip=%t/explicit=%t/target-overlap=%t", memory, skip, explicit, targetOverlap)
					t.Run(name, func(t *testing.T) {
						ctx := context.Background()
						input, output := t.TempDir(), t.TempDir()
						sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
						storage, err := NewProcessStorage(nil)
						if err != nil {
							t.Fatal(err)
						}
						defer storage.Close()
						publisher := rewriteTarget(t, storage, output)
						body := bytes.Repeat([]byte{0x53}, 4096)
						_, cfg := publishPortable(t, "")
						cfg.Metadata = map[string]string{"fixture": "lower"}
						lower := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
						cfg.Metadata = map[string]string{"fixture": "replacement"}
						replacement := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
						cfg.Metadata = nil
						merged, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(body), nil)
						if err != nil {
							t.Fatal(err)
						}
						var root string
						if memory {
							e := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
							lower = rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: e}, rewriteSparse(t, body))
							replacement = rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: "file://history.sandbox@digest:" + publishTestSHA}, rewriteSparse(t, body))
							root = rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: e, FromRefs: []string{lower}}, rewriteSparse(t, body))
						} else {
							cfg.Boot.Root.BaseFromRefs = []string{lower}
							root = rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
						}
						ref, err := manifest.ParseRef(root)
						if err != nil {
							t.Fatal(err)
						}
						ref.Path = filepath.Join(input, ref.Path)
						reduction := RefReduction{Top: ref.String()}
						if explicit {
							reduction.Target = merged
						}
						replace := RefReplacement{Old: lower, New: replacement}
						if targetOverlap {
							replace = RefReplacement{Old: replacement, New: lower}
						}
						opts := RewriteOptions{Replacements: []RefReplacement{replace}, Reductions: []RefReduction{reduction}, SkipVerify: skip}
						result, err := publisher.PublishWithOptions(ctx, ref.String(), opts)
						if err == nil || !strings.Contains(err.Error(), "overlap") || result.Ref != "" {
							t.Fatalf("want pre-output chain-overlap rejection, got %+v, %v", result, err)
						}
						files, err := os.ReadDir(output)
						if err != nil || len(files) != 0 {
							t.Fatalf("overlap produced files: %v %v", files, err)
						}
					})
				}
			}
		}
	}
}

func TestRewriteIndependentReplacementAndReduction(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip=%t", skip), func(t *testing.T) {
			ctx := context.Background()
			input, output := t.TempDir(), t.TempDir()
			sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
			storage, err := NewProcessStorage(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			publisher := rewriteTarget(t, storage, output)
			body := bytes.Repeat([]byte{0x35}, 4096)
			_, cfg := publishPortable(t, "")
			cfg.Metadata = map[string]string{"fixture": "old"}
			old := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
			cfg.Metadata = map[string]string{"fixture": "new"}
			replacement := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
			cfg.Metadata = nil
			top, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{0x46}, 4096)), nil)
			if err != nil {
				t.Fatal(err)
			}
			bottom, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{0x57}, 4096)), nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Boot.Root.BaseFromRefs = []string{old}
			cfg.Boot.Disks = []config.PortableDiskConfig{{Name: "data", PortableRootConfig: config.PortableRootConfig{Base: top, BaseFromRefs: []string{bottom}}}}
			cfg.Mounts = []config.MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
			root := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
			ref, _ := manifest.ParseRef(root)
			result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, ref.Path), RewriteOptions{Replacements: []RefReplacement{{Old: old, New: replacement}}, Reductions: []RefReduction{{Top: top}}, SkipVerify: skip})
			if err != nil || result.Ref == "" {
				t.Fatalf("independent rules rejected: %+v %v", result, err)
			}
		})
	}
}
