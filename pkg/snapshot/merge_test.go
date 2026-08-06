package snapshot

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

func TestOpenMergeBaseFollowsSymlink(t *testing.T) {
	payload := bytes.Repeat([]byte{0x5A}, 4096)
	targetDir := t.TempDir()
	tmp, err := os.CreateTemp(targetDir, "merge-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	scheme, digest, err := tarstream.WriteTo(context.Background(), tmp, "overlay", sparse.Dense(bytes.NewReader(payload), uint64(len(payload))))
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	name := digest + ".overlay"
	target := filepath.Join(targetDir, name)
	if err := os.Rename(tmp.Name(), target); err != nil {
		t.Fatal(err)
	}
	layer, _, err := openMergeBase(mergeTestRef(t, target, scheme, digest), int64(len(payload)), nil, false)
	if err != nil {
		t.Fatalf("open merge base: %v", err)
	}
	if err := layer.Close(); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(t.TempDir(), name)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	linkedLayer, _, err := openMergeBase(mergeTestRef(t, link, scheme, digest), int64(len(payload)), nil, false)
	if err != nil {
		t.Fatalf("open merge base symlink: %v", err)
	}
	if err := linkedLayer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMergeBaseCryptoPolicy(t *testing.T) {
	payload := bytes.Repeat([]byte{0x6c}, 3*4096)
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x51})
	wrong, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x52})
	dir := t.TempDir()
	type artifact struct {
		path, scheme, digest string
	}
	write := func(encrypted bool) artifact {
		tmp, err := os.CreateTemp(dir, "merge-*.tmp")
		if err != nil {
			t.Fatal(err)
		}
		var options []tarstream.WriteOption
		if encrypted {
			options = append(options, tarstream.WithCodec(codec, true))
		}
		scheme, digest, err := tarstream.WriteTo(context.Background(), tmp, "overlay", sparse.Dense(bytes.NewReader(payload), uint64(len(payload))), options...)
		if err != nil {
			_ = tmp.Close()
			t.Fatal(err)
		}
		if err := tmp.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, digest+".overlay")
		if err := os.Rename(tmp.Name(), path); err != nil {
			t.Fatal(err)
		}
		return artifact{path: path, scheme: scheme, digest: digest}
	}
	plainPath := write(false)
	encryptedPath := write(true)

	layer, holes, err := openMergeBase(mergeTestRef(t, encryptedPath.path, encryptedPath.scheme, encryptedPath.digest), int64(len(payload)), codec, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(holes) != 0 {
		t.Fatalf("encrypted merge holes=%v", holes)
	}
	got, err := io.ReadAll(layer)
	if closeErr := layer.Close(); err == nil {
		err = closeErr
	}
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("encrypted merge read err=%v equal=%v", err, bytes.Equal(got, payload))
	}
	if _, _, err := openMergeBase(mergeTestRef(t, encryptedPath.path, encryptedPath.scheme, encryptedPath.digest), int64(len(payload)), wrong, true); err == nil {
		t.Fatal("merge accepted the wrong customer key")
	}
	legacy, _, err := openMergeBase(mergeTestRef(t, plainPath.path, plainPath.scheme, plainPath.digest), int64(len(payload)), codec, false)
	if err != nil {
		t.Fatalf("auto legacy merge: %v", err)
	}
	_ = legacy.Close()
	if _, _, err := openMergeBase(mergeTestRef(t, plainPath.path, plainPath.scheme, plainPath.digest), int64(len(payload)), codec, true); err == nil {
		t.Fatal("required merge accepted plaintext")
	}

	logical := filepath.Join(dir, "logical-parent.overlay")
	if err := os.Rename(encryptedPath.path, logical); err != nil {
		t.Fatal(err)
	}
	logicalLayer, _, err := openMergeBase(mergeTestRef(t, logical, encryptedPath.scheme, encryptedPath.digest), int64(len(payload)), codec, true)
	if err != nil {
		t.Fatalf("scheme-qualified logical merge basename: %v", err)
	}
	_ = logicalLayer.Close()
}

func mergeTestRef(t *testing.T, path, scheme, digest string) string {
	t.Helper()
	ref := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path, DigestScheme: scheme, Digest: digest}
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
	return ref.String()
}

// oracle is the reference layering semantics (must match mergeSparse): top byte
// where top is resident, else base byte where base is resident, else 0 (merged
// hole). This is what fetch.Layered would resolve for a [top, base] chain.
func oracle(top, base []byte, topHoles, baseHoles []sparse.Extent, size int64) []byte {
	out := make([]byte, size)
	for off := int64(0); off < size; off++ {
		if th, _ := holeRun(off, topHoles, size); !th {
			out[off] = top[off]
		} else if bh, _ := holeRun(off, baseHoles, size); !bh {
			out[off] = base[off]
		} // else merged hole → 0
	}
	return out
}

func readAll(t *testing.T, rs io.ReadSeeker, size int64, holes []sparse.Extent) []byte {
	t.Helper()
	// Read exactly as the artifact packer does: zero-fill holes, copy the
	// data segments (= [0,size) minus holes, walked in order).
	out := make([]byte, size)
	cursor := uint64(0)
	read := func(from, to uint64) {
		if to <= from {
			return
		}
		if _, err := rs.Seek(int64(from), io.SeekStart); err != nil {
			t.Fatalf("seek %d: %v", from, err)
		}
		if _, err := io.ReadFull(rs, out[from:to]); err != nil {
			t.Fatalf("read seg [%d,%d): %v", from, to, err)
		}
	}
	for _, h := range holes {
		read(cursor, h.Offset)
		cursor = h.Offset + h.Size
	}
	read(cursor, uint64(size))
	return out
}

func TestHoleRunAlternatingBlocks(t *testing.T) {
	const (
		blockSize = int64(4096)
		blocks    = 1 << 16
		size      = blocks * blockSize
	)
	holes := make([]sparse.Extent, 0, blocks/2)
	for block := int64(1); block < blocks; block += 2 {
		holes = append(holes, sparse.Extent{
			Offset: uint64(block * blockSize),
			Size:   uint64(blockSize),
		})
	}
	for block := int64(0); block < blocks; block++ {
		isHole, end := holeRun(block*blockSize, holes, size)
		if want := block%2 == 1; isHole != want {
			t.Fatalf("block %d hole=%v want %v", block, isHole, want)
		}
		if want := (block + 1) * blockSize; end != want {
			t.Fatalf("block %d end=%d want %d", block, end, want)
		}
	}
	if isHole, end := holeRun(-1, holes, size); isHole || end != 0 {
		t.Fatalf("negative position = (%v, %d), want (false, 0)", isHole, end)
	}
	if isHole, end := holeRun(size, holes, size); isHole || end != size {
		t.Fatalf("end position = (%v, %d), want (false, %d)", isHole, end, size)
	}
}

func BenchmarkSeekerSourceAlternatingBlocks(b *testing.B) {
	const (
		blockSize = uint64(4096)
		blocks    = 1 << 20
		size      = blocks * blockSize
	)
	holes := make([]sparse.Extent, 0, blocks/2)
	for block := uint64(1); block < blocks; block += 2 {
		holes = append(holes, sparse.Extent{Offset: block * blockSize, Size: blockSize})
	}
	src := &seekerSource{size: size, holes: holes}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		offset := uint64(i&(blocks-1)) * blockSize
		if _, err := src.RunAt(offset, blockSize); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMergeSparse_Equivalence(t *testing.T) {
	const size = 16
	cases := []struct {
		name                string
		topHoles, baseHoles []sparse.Extent
		wantMergedHoles     []sparse.Extent
	}{
		{
			name:      "no overlap → no merged hole",
			topHoles:  []sparse.Extent{{Offset: 4, Size: 4}, {Offset: 12, Size: 4}},
			baseHoles: []sparse.Extent{{Offset: 0, Size: 4}},
		},
		{
			name:            "both-hole region → merged hole",
			topHoles:        []sparse.Extent{{Offset: 4, Size: 8}}, // [4,12)
			baseHoles:       []sparse.Extent{{Offset: 8, Size: 8}}, // [8,16)
			wantMergedHoles: []sparse.Extent{{Offset: 8, Size: 4}}, // [8,12)
		},
		{
			name:            "base fully holed → merged == top holes",
			topHoles:        []sparse.Extent{{Offset: 8, Size: 8}},  // [8,16)
			baseHoles:       []sparse.Extent{{Offset: 0, Size: 16}}, // all
			wantMergedHoles: []sparse.Extent{{Offset: 8, Size: 8}},
		},
		{
			name:     "top fully resident → base never shows, no merged hole",
			topHoles: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			top := make([]byte, size)
			base := make([]byte, size)
			for i := range top {
				top[i] = 0xA0 | byte(i) // distinct from base
				base[i] = 0xB0 | byte(i)
			}
			merged, mergedHoles := mergeSparse(bytes.NewReader(top), tc.topHoles, bytes.NewReader(base), tc.baseHoles, size)

			if !equalHoles(mergedHoles, tc.wantMergedHoles) {
				t.Errorf("mergedHoles = %v, want %v", mergedHoles, tc.wantMergedHoles)
			}
			got := readAll(t, merged, size, mergedHoles)
			want := oracle(top, base, tc.topHoles, tc.baseHoles, size)
			if !bytes.Equal(got, want) {
				t.Errorf("merged bytes mismatch\n got=%v\nwant=%v", got, want)
			}
		})
	}
}

func TestHoleIntersection(t *testing.T) {
	cases := []struct {
		a, b, want []sparse.Extent
		size       int64
	}{
		{size: 8},
		{a: hx(0, 8), b: hx(0, 8), want: hx(0, 8), size: 8},
		{a: hx(0, 4), b: hx(4, 4), want: nil, size: 8}, // disjoint
		{a: hx(2, 6), b: hx(0, 4), want: hx(2, 2), size: 8},
		{a: []sparse.Extent{{Offset: 0, Size: 2}, {Offset: 6, Size: 2}}, b: hx(0, 8), want: []sparse.Extent{{Offset: 0, Size: 2}, {Offset: 6, Size: 2}}, size: 8},
	}
	for i, tc := range cases {
		got := holeIntersection(tc.a, tc.b, tc.size)
		if !equalHoles(got, tc.want) {
			t.Errorf("case %d: holeIntersection = %v, want %v", i, got, tc.want)
		}
	}
}

func hx(offset, size uint64) []sparse.Extent {
	return []sparse.Extent{{Offset: offset, Size: size}}
}

func equalHoles(a, b []sparse.Extent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
