package artifact

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func newMemoryRefLimitFixture(t *testing.T, count int, working, opaque bool) (memoryCheckpointFixture, []string) {
	t.Helper()
	directory := t.TempDir()
	storage, err := NewProcessStorage(plaintextRefConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	sink := snapshot.NewFileSink(directory, "sid", nil, false, nil)
	t.Cleanup(func() { _ = sink.Close() })
	_, ecfg := publishPortable(t, "")
	eRef := rewriteSaveE(t, sink, ecfg, rewriteSparse(t, make([]byte, localMemorySize)))
	suffix := make([]string, count)
	for i := range suffix {
		if opaque {
			ref, err := manifest.ParseRef(fmt.Sprintf("file://unavailable-%d.snapshot@digest:%064x", i, i+1))
			if err != nil {
				t.Fatal(err)
			}
			ref.Location = "unavailable"
			suffix[i] = ref.String()
		} else {
			suffix[i] = rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: eRef},
				rewriteSparse(t, checkpointPageBytes(string(bytes.Repeat([]byte{byte('a' + i)}, 6)))))
		}
	}
	// History has dirty page 0 and released page 2, plus clean copies that
	// must not become candidates. Current W also includes clean pages 3/5.
	view := &memory.CaptureView{Size: localMemorySize, Materialized: []byte{0x13}, Dirty: []byte{1}, Released: []byte{4}}
	history, bits, err := snapshot.CaptureMemory(bytes.NewReader(checkpointPageBytes("bxxxxy")), view, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: eRef, FromRefs: suffix})
	if err != nil {
		t.Fatal(err)
	}
	source, err := snapshotfile.BuildSourceWithDirty(history, []byte("{}"), []byte("{}"), cfg, bits)
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := sink.AbsorbSnapshot(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	ref, path, descriptor := writeLocalMemory(t, directory, "limit", eRef, []string{parent}, suffix, "bcxxxx", 0x2b, 2, 0x10, working)
	return memoryCheckpointFixture{directory: directory, path: path, ref: ref, eRef: eRef, descriptor: descriptor, storage: storage}, suffix
}

func memoryRefLimitOptions(t *testing.T, fixture memoryCheckpointFixture, publisher *Publisher, mode, input string) RewriteOptions {
	t.Helper()
	switch mode {
	case "publish":
		return RewriteOptions{}
	case "rewrite":
		replacement, err := manifest.ParseRef(fixture.eRef)
		if err != nil {
			t.Fatal(err)
		}
		replacement.Path = filepath.Join(fixture.directory, replacement.Path)
		return RewriteOptions{Replacements: []RefReplacement{{Old: fixture.eRef, New: replacement.String()}}}
	case "reduce-any":
		return RewriteOptions{ReduceAny: true}
	case "reduce-root":
		raw, _, err := publisher.normalizeRoot(input)
		if err != nil {
			t.Fatal(err)
		}
		return RewriteOptions{Reductions: []RefReduction{{Top: raw}}}
	default:
		t.Fatalf("unknown mode %q", mode)
		return RewriteOptions{}
	}
}

func TestMemoryCheckpointRootRefLimitKeepsWorkingSetContract(t *testing.T) {
	for _, working := range []bool{false, true} {
		for _, count := range []int{snapshot.MaxMemoryFromRefs - 1, snapshot.MaxMemoryFromRefs} {
			for _, mode := range []string{"publish", "rewrite", "reduce-any", "reduce-root"} {
				t.Run(fmt.Sprintf("working-%t/refs-%d/%s", working, count, mode), func(t *testing.T) {
					fixture, suffix := newMemoryRefLimitFixture(t, count, working, false)
					publisher := rewriteTarget(t, fixture.storage, t.TempDir())
					result, err := publisher.PublishWithOptions(context.Background(), fixture.path,
						memoryRefLimitOptions(t, fixture, publisher, mode, fixture.path))
					reduced := strings.HasPrefix(mode, "reduce-")
					if working && count == snapshot.MaxMemoryFromRefs && !reduced {
						// A historical object cannot keep W, H and 64 opaque
						// lowers in 64 slots. Ordinary publication must not
						// silently merge them or relabel W to conceal this.
						if err == nil || !strings.Contains(err.Error(), "from_refs exceeds") || result.Ref != "" {
							t.Fatalf("capacity error lost: ref=%q err=%v", result.Ref, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					root, err := snapshotfile.Open(context.Background(), rewriteOpen(t, publisher, result.Ref))
					if err != nil {
						t.Fatal(err)
					}
					defer root.Close()
					cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
					if err != nil {
						t.Fatal(err)
					}
					if reduced {
						want := "bc\x00a\x00a"
						if working {
							want = "bc\x00x\x00x"
						}
						assertMemoryPages(t, root.Memory, "DDZDZD", want)
						if len(cfg.FromRefs) != 0 || root.MemoryDirty != nil {
							t.Fatalf("reduction retained refs or clean labels: %+v dirty=%x", cfg, root.MemoryDirty)
						}
						return
					}
					if working {
						assertMemoryPages(t, root.Memory, "DDHDZD", "bc\x00x\x00x")
						if len(cfg.FromRefs) != count+1 || len(root.MemoryDirty) != snapshotfile.DirtyHeaderSize+1 || root.MemoryDirty[snapshotfile.DirtyHeaderSize] != 0x12 {
							t.Fatalf("W/H boundary changed: %+v dirty=%x", cfg, root.MemoryDirty)
						}
						history, err := snapshotfile.Open(context.Background(), rewriteOpen(t, publisher, cfg.FromRefs[0]))
						if err != nil {
							t.Fatal(err)
						}
						defer history.Close()
						assertMemoryPages(t, history.Memory, "DHZHHH", "b\x00\x00\x00\x00\x00")
						if history.MemoryDirty != nil {
							t.Fatal("historical H is no longer all dirty")
						}
						cfg.FromRefs = cfg.FromRefs[1:]
					} else {
						assertMemoryPages(t, root.Memory, "DDZHZH", "bc\x00\x00\x00\x00")
						if root.MemoryDirty != nil {
							t.Fatal("dirty root acquired clean labels")
						}
					}
					if !SnapshotParentViewEqual(cfg.FromRefs, suffix) {
						t.Fatal("external parent identities/order changed")
					}
				})
			}
		}
	}
}

func TestMemoryCheckpointLowerUsesOneOfSixtyFourRefSlots(t *testing.T) {
	for _, working := range []bool{false, true} {
		for _, mode := range []string{"publish", "rewrite", "reduce-any", "reduce-root"} {
			t.Run(fmt.Sprintf("working-%t/%s", working, mode), func(t *testing.T) {
				testMemoryCheckpointLowerRefSlots(t, working, false, mode)
			})
		}
	}
	t.Run("opaque-external-lowers", func(t *testing.T) {
		testMemoryCheckpointLowerRefSlots(t, true, true, "publish")
	})
}

func testMemoryCheckpointLowerRefSlots(t *testing.T, working, opaque bool, mode string) {
	t.Helper()
	ctx := context.Background()
	fixture, suffix := newMemoryRefLimitFixture(t, snapshot.MaxMemoryFromRefs-1, working, opaque)
	sink := snapshot.NewFileSink(fixture.directory, "sid", nil, false, nil)
	defer sink.Close()
	view := &memory.CaptureView{Size: localMemorySize, Materialized: []byte{0x20}, Dirty: []byte{0x20}, Released: []byte{0}}
	top, _, err := snapshot.CaptureMemory(bytes.NewReader(checkpointPageBytes("xxxxxq")), view, false)
	if err != nil {
		t.Fatal(err)
	}
	outer := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: fixture.eRef,
		FromRefs: append([]string{fixture.ref}, suffix...)}, top)
	parsed, err := manifest.ParseRef(outer)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(fixture.directory, parsed.Path)
	output := t.TempDir()
	publisher := rewriteTarget(t, fixture.storage, output)
	result, err := publisher.PublishWithOptions(ctx, input, memoryRefLimitOptions(t, fixture, publisher, mode, input))
	if err != nil {
		t.Fatal(err)
	}
	root, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(mode, "reduce-") {
		want := "bc\x00a\x00q"
		if working {
			want = "bc\x00x\x00q"
		}
		assertMemoryPages(t, root.Memory, "DDZDZD", want)
		if len(cfg.FromRefs) != 0 || root.MemoryDirty != nil {
			t.Fatal("outer reduction retained a parent or clean labels")
		}
		return
	}
	assertMemoryPages(t, root.Memory, "HHHHHD", "\x00\x00\x00\x00\x00q")
	if len(cfg.FromRefs) != snapshot.MaxMemoryFromRefs || !SnapshotParentViewEqual(cfg.FromRefs[1:], suffix) {
		t.Fatalf("one lower expanded or changed external suffix: %+v", cfg)
	}
	if opaque {
		for i, raw := range suffix {
			if cfg.FromRefs[i+1] != raw {
				t.Fatal("opaque external ref was rewritten")
			}
		}
	}
	lower, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, cfg.FromRefs[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer lower.Close()
	if working {
		assertMemoryPages(t, lower.Memory, "DDZDZD", "bc\x00x\x00x")
		if len(lower.MemoryDirty) != snapshotfile.DirtyHeaderSize+1 || lower.MemoryDirty[snapshotfile.DirtyHeaderSize] != 0x17 {
			t.Fatalf("lower lost dirty union or promoted clean working pages: %x", lower.MemoryDirty)
		}
	} else {
		assertMemoryPages(t, lower.Memory, "DDZHZH", "bc\x00\x00\x00\x00")
		if lower.MemoryDirty != nil {
			t.Fatal("all-dirty lower acquired clean labels")
		}
	}
	dirty, err := SnapshotDirtyMemory(lower)
	if err != nil {
		t.Fatal(err)
	}
	assertMemoryPages(t, dirty, "DDZHZH", "bc\x00\x00\x00\x00")
	if opaque || !working {
		return
	}
	// A real later dirty capture must trim the former working-set-only pages
	// from the portable lower while retaining old dirty/Zero history.
	parent, err := manifest.ParseRef(cfg.FromRefs[0])
	if err != nil {
		t.Fatal(err)
	}
	parent.Location = ""
	refs := append([]string(nil), cfg.FromRefs[1:]...)
	for i, raw := range refs {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			t.Fatal(err)
		}
		ref.Location = ""
		refs[i] = ref.String()
	}
	fixture.directory = output
	fixture.ref, fixture.path, fixture.descriptor = writeLocalMemory(t, output, "next", fixture.eRef,
		[]string{parent.String()}, refs, "bxdxxe", 0x25, 0x24, 0, false)
	next := openLocalFixture(t, fixture)
	defer next.Close()
	assertMemoryPages(t, next.Memory, "DDDHZD", "bcd\x00\x00e")
}
