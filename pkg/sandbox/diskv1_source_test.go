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
