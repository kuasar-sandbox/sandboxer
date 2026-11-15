package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
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
func readExportedDisk(t *testing.T, st *ProcessStorage, d DiskV1) []byte {
	t.Helper()
	ctx := context.Background()
	requireDiskIO(t, d.Validate())
	var streams []fetch.Stream
	for _, raw := range append([]string{d.Base}, d.BaseFromRefs...) {

		stream, _, err := newPublisher(st, nil, nil, nil).open(ctx, raw, publishScope{})
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
	portable.Boot.Disks = []config.PortableDiskConfig{{
		Name: "work", PortableRootConfig: config.PortableRootConfig{Base: topRef, BaseFromRefs: []string{parentRef}},
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
	out, err := st.ExportDisk(ctx, ExportDiskRequest{SourceRef: path, DiskName: "data-b", OutputDir: t.TempDir()})
	requireDiskIO(t, err)
	b := readExportedDisk(t, st, out)
	if !bytes.Equal(b[:4096], bytes.Repeat([]byte{3}, 4096)) {
		t.Fatal("wrong checkpoint disk")
	}
	requireDiskIO(t, os.Rename(unselected+".away", unselected))
	pins, err := st.PinDiffCheckpoints(ctx, f.dir, f.cp)
	requireDiskIO(t, err)
	requireDiskIO(t, pins.Close())
}
