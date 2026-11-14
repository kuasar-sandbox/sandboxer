package sandbox

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestDiskV1SourceShortParentAndNamedSeed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sink := snapshot.NewFileSink(dir, "test", nil, false, nil)
	defer sink.Close()
	const page = 4096
	topData := make([]byte, 4*page)
	copy(topData[page:], bytes.Repeat([]byte{'T'}, page))
	top, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(topData), []sparse.Extent{{Offset: 0, Size: page}, {Offset: 3 * page, Size: page}})
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{'B'}, page)), nil)
	if err != nil {
		t.Fatal(err)
	}
	abs := func(raw string) string {
		r, e := manifest.ParseRef(raw)
		if e != nil {
			t.Fatal(e)
		}
		r.Path = filepath.Join(dir, r.Path)
		return r.String()
	}
	spec := config.DiskV1{Kind: "disk", Version: 1, Base: abs(top), BaseFromRefs: []string{abs(parent)}}
	reader, size, template, err := OpenDiskV1Source(ctx, spec, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if size != 4*page || template != "" {
		t.Fatal(size, template)
	}
	b := make([]byte, size)
	n, err := reader.ReadAt(b, 0)
	if n != len(b) || err != nil && err != io.EOF {
		t.Fatal(n, err)
	}
	for i, v := range []byte{'B', 'T', 0, 0} {
		if !bytes.Equal(b[i*page:(i+1)*page], bytes.Repeat([]byte{v}, page)) {
			t.Fatalf("wrong block %d", i)
		}
	}
	seed := filepath.Join(dir, "seed")
	if err = os.WriteFile(seed, make([]byte, 8*page), 0600); err != nil {
		t.Fatal(err)
	}
	spec.DiffTemplate = "file://seed@location:seeds"
	loc := config.RefLocations{"seeds": dir}
	derived, capacity, tmpl, err := OpenDiskV1Source(ctx, spec, st, loc)
	if err != nil {
		t.Fatal(err)
	}
	defer derived.Close()
	if capacity != 8*page || tmpl != "file://"+seed {
		t.Fatal(capacity, tmpl)
	}
	wrong := int64(page)
	spec.Size = &wrong
	if _, _, _, e := OpenDiskV1Source(ctx, spec, st, loc); e == nil {
		t.Fatal("ignored capacity assertion")
	}
}

func TestDiskV1PinsUnqualifiedFileFromLiveStream(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, scheme, digest := writeDiskArtifact(t, dir, "seed", make([]byte, 8192), nil)
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	spec := config.DiskV1{Kind: "disk", Version: 1, Base: "file://" + path, BaseFromRefs: []string{"file://" + filepath.Base(path) + "@location:here"}}
	locations := config.RefLocations{"here": dir}
	var result config.DiskV1
	reader, _, _, err := openDiskV1Source(ctx, spec, storage, locations, &result)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, raw := range []string{result.Base, result.BaseFromRefs[0]} {
		ref, e := manifest.ParseRef(raw)
		if e != nil || ref.DigestScheme != scheme || ref.Digest != digest {
			t.Fatalf("unqualified file was not pinned: %s (%v)", raw, e)
		}
	}
	if result.Identity() == spec.Identity() || spec.BaseFromRefs[0] == result.BaseFromRefs[0] {
		t.Fatal("content resolution overwrote the idempotency selector")
	}
}

func TestColdDiskV1IdentityNeverProbesGuestZIPOrFilename(t *testing.T) {
	dir := t.TempDir()
	// An ordinary disk may use an E-like filename. A generic E probe would
	// fail this fixture even though the backing tarstream is valid.
	path, scheme, digest := writeDiskArtifact(t, dir, "overlay", make([]byte, 8192), nil)
	renamed := filepath.Join(dir, "user.sandbox")
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	base := "file://" + renamed
	cfg := &config.SandboxConfig{Boot: config.BootConfig{Disks: []config.DiskConfig{{Name: "data", Disk: &config.DiskV1{Kind: "disk", Version: 1, Base: base}}}}}
	if err := canonicalizeConfiguredTarRefs(cfg, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	resolved, err := manifest.ParseRef(cfg.Boot.Disks[0].Base)
	if err != nil || resolved.DigestScheme != scheme || resolved.Digest != digest || cfg.Boot.Disks[0].Disk.Base != resolved.String() {
		t.Fatalf("cold disk reference: %v, %v", cfg.Boot.Disks[0], err)
	}
}
