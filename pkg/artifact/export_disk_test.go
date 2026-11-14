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

func TestPublishDiskFreezesSparseSeedAndParents(t *testing.T) {
	const page = 4096
	for _, destination := range []string{"bundle", "store"} {
		t.Run(destination, func(t *testing.T) {
			ctx := context.Background()
			var storage *ProcessStorage
			dest := DiskDestination{Upload: destination == "store"}
			if dest.Upload {
				_, storage = manifestPublisherFixture(t)
			} else {
				cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
				var err error
				storage, err = NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
				requireDiskIO(t, err)
				defer storage.Close()
				dest.OutputDir = t.TempDir()
			}
			src := t.TempDir()
			sink := snapshot.NewFileSink(src, "disk", nil, false, nil)
			_, file, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{'B'}, 2*page)), nil)
			requireDiskIO(t, err)
			requireDiskIO(t, sink.Close())
			seed := filepath.Join(src, "seed.raw")
			f, err := os.Create(seed)
			requireDiskIO(t, err)
			requireDiskIO(t, f.Truncate(3*page))
			_, err = f.WriteAt(make([]byte, page), page)
			requireDiskIO(t, err)
			_, err = f.WriteAt(bytes.Repeat([]byte{'T'}, page), 2*page)
			requireDiskIO(t, err)
			requireDiskIO(t, f.Close())
			disk := DiskV1{Kind: "disk", Version: 1, Base: "file://" + file, DiffTemplate: "file://" + seed}
			out, err := storage.PublishDisk(ctx, disk, nil, dest)
			requireDiskIO(t, err)
			bad := int64(page)
			disk.Size = &bad
			if _, err = storage.PublishDisk(ctx, disk, nil, dest); err == nil {
				t.Fatal("accepted size mismatch")
			}
			requireDiskIO(t, os.RemoveAll(src))
			got := readExportedDisk(t, storage, out)
			want := append(bytes.Repeat([]byte{'B'}, page), make([]byte, page)...)
			want = append(want, bytes.Repeat([]byte{'T'}, page)...)
			if out.DiffTemplate != "" || *out.Size != 3*page || !bytes.Equal(got, want) {
				t.Fatal("workspace seed or lower lost during publication")
			}
		})
	}
}
