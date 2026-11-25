package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func requireDiskIO(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func readExportedDisk(t *testing.T, st *ProcessStorage, d DiskV1, locations ...config.RefLocations) []byte {
	t.Helper()
	ctx := context.Background()
	requireDiskIO(t, d.Validate())
	var loc config.RefLocations
	if len(locations) > 0 {
		loc = locations[0]
	}
	var streams []fetch.Stream
	for _, raw := range append([]string{d.Base}, d.BaseFromRefs...) {

		stream, _, err := newPublisher(st, loc, nil, nil).open(ctx, raw, publishScope{})
		requireDiskIO(t, err)
		streams = append(streams, stream)
	}
	view := fetch.NewLayered(streams...)
	defer view.Close()
	b := make([]byte, *d.Size)
	n, err := view.ReadAt(ctx, b, 0)
	if n != len(b) || err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %d, %v", n, err)
	}
	return b
}

func TestExportDiskIndependentBundleEAndS(t *testing.T) {
	mc, key := sourceBundleConfig(manifestcrypto.LocalOff)
	st, err := NewProcessStorageWithCustomerKey(mc, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer st.Close()
	compareExportedDisks(t, st, false)
}

func TestExportDiskStoreEAndS(t *testing.T) {
	_, st := manifestPublisherFixture(t)
	compareExportedDisks(t, st, true)
}

func borrowedDiskBundle(t *testing.T, storage *ProcessStorage) (*ProcessStorage, string, string) {
	t.Helper()
	ctx := context.Background()
	cfg := storage.ManifestConfig()
	sourceDir := t.TempDir()
	sink, err := snapshot.NewBundleSink(ctx, sourceDir, "source", cfg, storage.CustomerKeyFunc(), nil)
	requireDiskIO(t, err)
	defer sink.Close()
	const page = 4096
	parent := bytes.Repeat([]byte{'P'}, 2*page)
	parentRef, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(parent), nil)
	requireDiskIO(t, err)
	top := bytes.Repeat([]byte{'T'}, 2*page)
	topRef, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(top), []sparse.Extent{{Offset: 0, Size: page}})
	requireDiskIO(t, err)
	_, portable := publishPortable(t, "")
	portable.Version = 2
	portable.Boot.Disks = []config.PortableDiskConfig{{
		Name: "work", PureBlock: true, PortableRootConfig: config.PortableRootConfig{Base: topRef, BaseFromRefs: []string{parentRef}},
	}}
	portable.Mounts = append(portable.Mounts, config.MountConfig{Type: "disk", Source: "work", Target: "/work"})
	body, err := config.MarshalPortableSandboxConfig(portable)
	requireDiskIO(t, err)
	source, err := sandboxfile.BuildSource(publishSource(t, make([]byte, page)), nil, body)
	requireDiskIO(t, err)
	rootRef, _, err := sink.AbsorbSandbox(ctx, source)
	requireDiskIO(t, err)
	requireDiskIO(t, sink.CommitSandbox(ctx, rootRef, ""))
	requireDiskIO(t, sink.Close())

	rootKey, err := manifest.ParseKeyRef(rootRef)
	requireDiskIO(t, err)
	path := filepath.Join(sourceDir, manifest.HexKey(rootKey)+".bundle")
	bundle, err := storage.OpenFile(ctx, path, manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path})
	requireDiskIO(t, err)
	t.Cleanup(func() { requireDiskIO(t, bundle.Close()) })
	reader := storage.BorrowWithFetcher(bundle.ScopedFetcher())
	t.Cleanup(func() { requireDiskIO(t, reader.Close()) })
	return reader, rootRef, sourceDir
}

func TestExportDiskBorrowedLayersRequireUpload(t *testing.T) {
	ctx := context.Background()
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer storage.Close()
	reader, rootRef, _ := borrowedDiskBundle(t, storage)
	publisher := newPublisher(reader, nil, nil, nil)
	err = publisher.withDiskTable(ctx, ExportDiskRequest{SourceRef: rootRef}, func(cfg *config.PortableSandboxConfig, cp *sandboxfile.Checkpoint, scope publishScope) error {
		layers, err := publisher.openSavedDisk(ctx, cfg, cp, scope, "work")
		requireDiskIO(t, err)
		defer closeDiskLayers(layers)
		if len(layers) != 2 || layers[0].portable != "" || layers[1].portable != "" {
			t.Fatal("source Bundle layer was marked present in destination Store")
		}
		return nil
	})
	requireDiskIO(t, err)
}

func TestExportDiskUploadsLayersFromBorrowedBundle(t *testing.T) {
	_, storage := manifestPublisherFixture(t)
	reader, rootRef, sourceDir := borrowedDiskBundle(t, storage)
	disk, err := reader.ExportDisk(context.Background(), ExportDiskRequest{SourceRef: rootRef, DiskName: "work", Upload: true})
	requireDiskIO(t, err)
	requireDiskIO(t, os.RemoveAll(sourceDir))
	const page = 4096
	got := readExportedDisk(t, storage, disk)
	if !bytes.Equal(got[:page], bytes.Repeat([]byte{'P'}, page)) || !bytes.Equal(got[page:], bytes.Repeat([]byte{'T'}, page)) {
		t.Fatal("exported disk lost its inherited or written page")
	}
}

func TestExportDiskUploadsLayersToNamedLocation(t *testing.T) {
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer storage.Close()
	reader, rootRef, sourceDir := borrowedDiskBundle(t, storage)
	targetDir := filepath.Join(t.TempDir(), "published")
	locations := config.RefLocations{"input": sourceDir}
	disk, err := reader.ExportDisk(context.Background(), ExportDiskRequest{
		SourceRef: rootRef, DiskName: "work", Upload: true,
		ToRefLocation: config.RefLocations{"workspace": targetDir}, RefLocations: locations,
	})
	requireDiskIO(t, err)
	if !maps.Equal(locations, config.RefLocations{"input": sourceDir}) {
		t.Fatalf("export mutated input mappings: %v", locations)
	}
	var bundlePath string
	for _, raw := range append([]string{disk.Base}, disk.BaseFromRefs...) {
		ref, err := manifest.ParseRef(raw)
		requireDiskIO(t, err)
		if ref.Scheme != manifest.RefSchemeFile || ref.Location != "workspace" || ref.DigestScheme != "manifest" || filepath.Ext(ref.Path) != ".bundle" || filepath.Base(ref.Path) != ref.Path {
			t.Fatalf("named publication returned nonportable ref %q", raw)
		}
		if bundlePath != "" && ref.Path != bundlePath {
			t.Fatalf("selected layers were split across Bundles: %s != %s", ref.Path, bundlePath)
		}
		bundlePath = ref.Path
	}
	files, err := os.ReadDir(targetDir)
	requireDiskIO(t, err)
	if len(files) != 1 || files[0].Name() != bundlePath {
		t.Fatalf("named publication must commit exactly one Bundle, got %v", files)
	}
	requireDiskIO(t, os.RemoveAll(sourceDir))
	got := readExportedDisk(t, storage, disk, config.RefLocations{"workspace": targetDir})
	const page = 4096
	if !bytes.Equal(got[:page], bytes.Repeat([]byte{'P'}, page)) || !bytes.Equal(got[page:], bytes.Repeat([]byte{'T'}, page)) {
		t.Fatal("named export lost inherited or written data")
	}
}

func TestExportDiskRejectsInvalidNamedDestinationBeforeSourceIO(t *testing.T) {
	storage, err := NewProcessStorage(nil)
	requireDiskIO(t, err)
	defer storage.Close()
	target := filepath.Join(t.TempDir(), "unused")
	for _, tc := range []struct {
		name, want string
		change     func(*ExportDiskRequest)
	}{
		{"without-upload", "with upload", func(r *ExportDiskRequest) { r.Upload, r.OutputDir = false, target }},
		{"with-output", "exactly one destination", func(r *ExportDiskRequest) { r.OutputDir = target }},
		{"multiple", "one target", func(r *ExportDiskRequest) { r.ToRefLocation["other"] = target }},
		{"empty-name", "name is required", func(r *ExportDiskRequest) { r.ToRefLocation = config.RefLocations{"": target} }},
		{"invalid-name", "location", func(r *ExportDiskRequest) { r.ToRefLocation = config.RefLocations{"../bad": target} }},
		{"relative-directory", "must be absolute", func(r *ExportDiskRequest) { r.ToRefLocation["out"] = "relative" }},
		{"conflicting-input", "conflicts with input", func(r *ExportDiskRequest) { r.RefLocations = config.RefLocations{"out": target + "-other"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := ExportDiskRequest{SourceRef: "/missing-source.sandbox", DiskName: "work", Upload: true, ToRefLocation: config.RefLocations{"out": target}}
			tc.change(&req)
			_, err := storage.ExportDisk(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q before opening source", err, tc.want)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("invalid request created target: %v", err)
			}
		})
	}
}

// A disk exporter must accept sequential checkpoint sources without an
// identity prepass that consumes the payload before the destination write.
type singlePassDiskStream struct {
	sparse.Source
	next   uint64
	closed bool
}

func (s *singlePassDiskStream) ReadAt(ctx context.Context, b []byte, off uint64) (int, error) {
	if off < s.next {
		return 0, errors.New("disk source payload was read twice")
	}
	n, err := s.Source.ReadAt(ctx, b, off)
	s.next = off + uint64(n)
	return n, err
}

func (s *singlePassDiskStream) RunAt(off, limit uint64) (sparse.Run, error) {
	run, err := s.Source.RunAt(off, limit)
	if err != nil {
		return nil, err
	}
	return singlePassDiskRun{Run: run, source: s}, nil
}

func (s *singlePassDiskStream) Close() error { s.closed = true; return nil }

type singlePassDiskRun struct {
	sparse.Run
	source *singlePassDiskStream
}

func (r singlePassDiskRun) ReadAt(ctx context.Context, b []byte, off uint64) (int, error) {
	return r.source.ReadAt(ctx, b, r.Offset()+off)
}

func TestExportDiskNamedDestinationConsumesPayloadOnce(t *testing.T) {
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer storage.Close()
	want := bytes.Repeat([]byte{0x73}, 3*4096)
	stream := &singlePassDiskStream{Source: publishSource(t, want)}
	target := config.RefLocations{"out": t.TempDir()}
	disk, err := storage.publishSavedDisk(context.Background(), ExportDiskRequest{Upload: true, ToRefLocation: target}, []savedDiskLayer{{Stream: stream, size: uint64(len(want))}})
	requireDiskIO(t, err)
	if !stream.closed || stream.next != uint64(len(want)) {
		t.Fatalf("source not fully consumed and closed: %+v", stream)
	}
	if !bytes.Equal(readExportedDisk(t, storage, disk, target), want) {
		t.Fatal("single-pass publication changed payload")
	}
}

func compareExportedDisks(t *testing.T, st *ProcessStorage, upload bool) {
	t.Helper()
	ctx := context.Background()
	var err error
	src, dest := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(src, "source", nil, false, nil)
	defer sink.Close()
	const page = 4096
	refs := make([]string, 2)
	for i, v := range []byte{'A', 'B'} {
		refs[i], _, err = sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{v}, 3*page)), nil)
		requireDiskIO(t, err)
	}
	data := make([]byte, 3*page)
	copy(data[page:], bytes.Repeat([]byte{'T'}, page))
	top, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(data), []sparse.Extent{{Offset: 0, Size: page}})
	requireDiskIO(t, err)
	var disks []DiskV1
	for i, base := range refs {
		_, c := publishPortable(t, "")
		c.Boot.Disks = []config.PortableDiskConfig{{Name: "other", PortableRootConfig: config.PortableRootConfig{Base: "file://absent.overlay@digest:" + publishTestSHA}}, {Name: "work", PortableRootConfig: config.PortableRootConfig{Base: top, BaseFromRefs: []string{base}}}}
		for _, d := range c.Boot.Disks {
			c.Mounts = append(c.Mounts, config.MountConfig{Type: "disk", Source: d.Name, Target: "/" + d.Name})
		}
		b, e := config.MarshalPortableSandboxConfig(c)
		requireDiskIO(t, e)
		eSrc, e := sandboxfile.BuildSource(publishSource(t, make([]byte, page)), nil, b)
		requireDiskIO(t, e)
		eRef, ePath, e := sink.AbsorbSandbox(ctx, eSrc)
		requireDiskIO(t, e)
		requireDiskIO(t, sink.CommitSandbox(ctx, eRef, ePath))
		ref := eRef
		if i == 1 {
			cfg, e := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: eRef})
			requireDiskIO(t, e)
			sm, e := snapshotfile.BuildSource(publishSource(t, make([]byte, page)), []byte("{}"), []byte("{}"), cfg)
			requireDiskIO(t, e)
			sr, sp, e := sink.AbsorbSnapshot(ctx, sm)
			requireDiskIO(t, e)
			requireDiskIO(t, sink.CommitSnapshot(ctx, sr, sp))
			ref = sr
		}
		req := ExportDiskRequest{SourceRef: ref, SourceDir: src, DiskName: "work", OutputDir: dest}
		if upload {
			req.OutputDir = ""
			req.Upload = true
		}
		out, e := st.ExportDisk(ctx, req)
		requireDiskIO(t, e)
		disks = append(disks, out)
		same, e := st.ExportDisk(ctx, req)
		requireDiskIO(t, e)
		if same.Base != out.Base || !slices.Equal(same.BaseFromRefs, out.BaseFromRefs) {
			t.Fatal("second export changed reference")
		}
	}
	requireDiskIO(t, os.RemoveAll(src))
	if !upload && disks[0].Base == disks[1].Base {
		t.Fatal("same top but distinct parent reused wrong Bundle")
	}
	for i, d := range disks {
		b := readExportedDisk(t, st, d)
		if !bytes.Equal(b[:page], bytes.Repeat([]byte{'A' + byte(i)}, page)) || !bytes.Equal(b[page:2*page], data[page:2*page]) || !bytes.Equal(b[2*page:], make([]byte, page)) {
			t.Fatal("inherited, modified or explicit zero block changed")
		}
	}
}

func TestExportDiskCheckpointPinsOnlyNamedVersion(t *testing.T) {
	ctx := context.Background()
	f := newDiffCheckpointFixture(t)
	// Neither the E disk order nor the presence of an unselected local DIFF
	// may influence publication of data-b.
	slices.Reverse(f.cp.Disks)
	src, err := sandboxfile.BuildCheckpointSource(ctx, f.cfg, f.cp)
	requireDiskIO(t, err)
	sink := snapshot.NewFileSink(f.dir, "reordered", nil, false, nil)
	ref, path, err := sink.AbsorbSandbox(ctx, src)
	requireDiskIO(t, err)
	requireDiskIO(t, sink.CommitSandbox(ctx, ref, path))
	requireDiskIO(t, sink.Close())
	unselected := filepath.Join(f.dir, "diffs", "data-1.diff")
	requireDiskIO(t, os.Rename(unselected, unselected+".away"))
	mc, key := sourceBundleConfig(manifestcrypto.LocalOff)
	st, err := NewProcessStorageWithCustomerKey(mc, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer st.Close()
	// A later active write cannot change the selected committed version.
	_, err = f.stores[2].WriteAt(bytes.Repeat([]byte{9}, 4096), 0)
	requireDiskIO(t, err)
	for _, named := range []bool{false, true} {
		target := t.TempDir()
		req := ExportDiskRequest{SourceRef: path, DiskName: "data-b", OutputDir: target}
		var locations config.RefLocations
		if named {
			locations = config.RefLocations{"out": target}
			req.OutputDir, req.Upload, req.ToRefLocation = "", true, locations
		}
		out, err := st.ExportDisk(ctx, req)
		requireDiskIO(t, err)
		b := readExportedDisk(t, st, out, locations)
		if !bytes.Equal(b[:4096], bytes.Repeat([]byte{3}, 4096)) {
			t.Fatalf("wrong checkpoint disk or version (named=%t)", named)
		}
	}
	requireDiskIO(t, os.Rename(unselected+".away", unselected))
	pins, err := st.PinDiffCheckpoints(ctx, f.dir, f.cp)
	requireDiskIO(t, err)
	requireDiskIO(t, pins.Close())
}
