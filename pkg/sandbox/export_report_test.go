package sandbox

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestExportReferencePlanRecordsOnlyRequestedActualSources(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := snapshotTestPortable(t)
	cfg.Boot.Root = config.PortableRootConfig{Base: "self"}
	encoded, err := config.MarshalPortableSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := sparse.NewSource(bytes.NewReader(make([]byte, 4096)), 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := sandboxfile.BuildSourceContext(ctx, payload, nil, encoded)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(dir, "parent", nil, false, nil)
	ref, path, err := sink.AbsorbSandbox(ctx, source)
	if err == nil {
		err = sink.CommitSandbox(ctx, ref, path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = filepath.Join(dir, filepath.Base(path))
	want := parsed.String()
	for _, report := range []bool{false, true} {
		plan, err := PrepareSandboxDependencyPlan(ctx, RunOptions{
			Cfg: &config.SandboxConfig{}, PortableConfig: cfg,
			SourceBinding: &RunSourceBinding{SandboxRef: ref, RuntimeRef: want, RelativeDir: dir},
		}, []bool{false}, "", store.WriteAdmission{}, false, report)
		if err != nil {
			t.Fatal(err)
		}
		got := plan.OriginalRefs()
		if report && !slices.Equal(got, []string{want}) || !report && got != nil {
			t.Fatalf("report=%t actual sources=%v, want %q", report, got, want)
		}
		if err := plan.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportReferenceSelectedCarrierScope(t *testing.T) {
	current, sibling := new(manifestbundle.Reader), new(manifestbundle.Reader)
	logical, err := manifest.ParseRef("manifest://" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	parent := &BundleSourceBinding{RootRef: "file://root.bundle@location:shared", RootPath: "/shared/root.bundle"}
	selector := "@manifest:" + logical.Path
	for _, test := range []struct {
		name, source, want string
		reader             *manifestbundle.Reader
		binding            *BundleSourceBinding
	}{
		{"current named", "", "file://root.bundle" + selector + "@location:shared", current, parent},
		{"relative sibling", "file://parent.bundle", "file://parent.bundle" + selector + "@location:shared", sibling, parent},
		{"other named sibling", "file://parent.bundle@location:other", "file://parent.bundle" + selector + "@location:other", sibling, parent},
		{"private sibling", "file://parent.bundle", "file:///private/parent.bundle" + selector, sibling, &BundleSourceBinding{RootRef: "file://root.bundle", RootPath: "/private/root.bundle"}},
		{"unknown borrowed current", "", "", current, nil},
		{"unknown borrowed relative", "file://parent.bundle", "", sibling, nil},
		{"known borrowed named sibling", "file://parent.bundle@location:other", "file://parent.bundle" + selector + "@location:other", sibling, nil},
		{"unknown alias", logical.String(), "", sibling, nil},
		{"remote", "", logical.String(), nil, parent},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectedDependencyRef(logical, manifestbundle.ManifestSource{Reader: test.reader, Ref: test.source}, bundleLookup{reader: current, binding: test.binding}, RunOptions{})
			if err != nil || got != test.want {
				t.Fatalf("selected carrier = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestExportReferencePureCompactionUsesSelectedCarrier(t *testing.T) {
	ctx := t.Context()
	directory := t.TempDir()
	keyFn := func() ([32]byte, error) { return [32]byte{0x51, 0x52}, nil }
	manifestCfg := &manifest.Config{
		Manifest: manifest.ManifestSubConfig{WriteGeneration: "G1"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	sink, err := snapshot.NewBundleSink(ctx, directory, "pure-parents", manifestCfg, keyFn, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]string, config.MaxPortableLayerRefs+1)
	for i := range refs {
		refs[i], _, err = sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{byte(i + 1)}, 4096)), nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	path, err := sink.CommitDisk(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := manifest.ParseRef(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	opened, err := artifact.OpenFile(ctx, path, manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path, DigestScheme: "manifest", Digest: root.Path}, manifestCfg, keyFn, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	p := testRunFromPortable(t)
	p.Version = 2
	source := config.DiskV1{Kind: "disk", Version: 1, Base: refs[0], BaseFromRefs: refs[1:]}
	p.Boot.Disks = []config.PortableDiskConfig{{Name: "work", PureBlock: true, PortableRootConfig: config.PortableRootConfig{Base: source.Base, BaseFromRefs: source.BaseFromRefs}}}
	p.Mounts = []config.MountConfig{{Type: "disk", Source: "work", Target: "/data"}}
	cfg := &config.SandboxConfig{Boot: config.BootConfig{Disks: []config.DiskConfig{{Name: "work", Disk: &source, PureBlock: true}}}}
	view := SnapDiskRef{Size: 8192, SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) { return nil, nil, nil }}
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "named", false: "borrowed"}[known], func(t *testing.T) {
			opts := RunOptions{Cfg: cfg, PortableConfig: p, ManifestCfg: manifestCfg, CustomerKeyFn: keyFn,
				Fetcher: opened.ManifestFetcher(), BundleReader: opened.BundleReader(), BundleFetcher: opened.ManifestFetcher()}
			if known {
				opts.SourceBinding = &RunSourceBinding{BundleSource: &BundleSourceBinding{
					RootRef: "file://" + filepath.Base(path) + "@location:shared", RootPath: path,
					Reader: opened.BundleReader(), Fetcher: opened.ManifestFetcher(),
				}}
			}
			var before []string
			_, diffs, merged, open, err := prepareCheckpointDisks(ctx, opts, []SnapDiskRef{view, view}, func(raw string) { before = append(before, raw) })
			if err != nil {
				t.Fatal(err)
			}
			if !merged[1] || len(before) != 0 {
				t.Fatalf("compaction must not infer source identity before reading: merged=%v refs=%v", merged, before)
			}
			stream, err := open(ctx, diffs[1].MergeBase)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			if !known {
				if len(before) != 0 {
					t.Fatalf("borrowed Bundle aliases became Store removals: %v", before)
				}
				return
			}
			want := make([]string, len(refs))
			for i, raw := range refs {
				logical, err := manifest.ParseRef(raw)
				if err != nil {
					t.Fatal(err)
				}
				want[i] = "file://" + filepath.Base(path) + "@manifest:" + logical.Path + "@location:shared"
			}
			if !slices.Equal(before, want) {
				t.Fatalf("compaction source report = %v, want %v", before, want)
			}
		})
	}

	// A complete output can materialize a borrowed dependency even though its
	// old physical carrier name was not supplied by the embedding caller.
	borrowed := RunOptions{Cfg: cfg, PortableConfig: p, ManifestCfg: manifestCfg, CustomerKeyFn: keyFn,
		Fetcher: opened.ManifestFetcher(), BundleReader: opened.BundleReader(), BundleFetcher: opened.ManifestFetcher()}
	borrowed.PortableConfig, err = p.Clone()
	if err != nil {
		t.Fatal(err)
	}
	borrowed.PortableConfig.Boot.Root = config.PortableRootConfig{Base: "self"}
	borrowed.PortableConfig.Boot.Disks[0].BaseFromRefs = nil
	plan, err := PrepareSandboxDependencyPlan(ctx, borrowed, []bool{false, false}, "", store.WriteAdmission{}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.OriginalRefs()) != 0 {
		t.Fatalf("borrowed materialization invented old identities: %v", plan.OriginalRefs())
	}
	if err := plan.Close(); err != nil {
		t.Fatal(err)
	}
}
