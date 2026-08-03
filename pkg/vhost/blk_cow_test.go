package vhost

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeReader is a BlockReader returning a fixed pattern.
type fakeReader struct {
	data []byte
}

func (r *fakeReader) ReadAt(buf []byte, offset int64) (int, error) {
	if offset >= int64(len(r.data)) {
		return 0, nil
	}
	end := offset + int64(len(buf))
	if end > int64(len(r.data)) {
		end = int64(len(r.data))
	}
	copy(buf, r.data[offset:end])
	return int(end - offset), nil
}
func (r *fakeReader) Size() int64  { return int64(len(r.data)) }
func (r *fakeReader) Close() error { return nil }

func TestBlockCOW_BareDiff_NoBaseRead(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "diff.ext4")
	cow, err := OpenBlockCOW(diff, nil, 4*4096)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	// Read should return zeros (no base, no dirty data).
	buf := make([]byte, 4096)
	n, err := cow.ReadAt(buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4096 {
		t.Fatalf("ReadAt n=%d want 4096", n)
	}
	if !bytes.Equal(buf, make([]byte, 4096)) {
		t.Fatalf("expected zeros, got %x...", buf[:8])
	}
	if cow.DirtyCount() != 0 {
		t.Errorf("expected 0 dirty blocks, got %d", cow.DirtyCount())
	}
}

func TestBlockCOW_WriteThenRead(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "diff.ext4")
	base := &fakeReader{data: bytes.Repeat([]byte("BASE"), 4096)} // 16 KiB
	cow, err := OpenBlockCOW(diff, base, 4*4096)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	// Write 4 KiB of 'X' at offset 4096 (block 1).
	payload := bytes.Repeat([]byte{'X'}, 4096)
	n, err := cow.WriteAt(payload, 4096)
	if err != nil || n != 4096 {
		t.Fatalf("WriteAt: n=%d err=%v", n, err)
	}
	if !cow.blockDirty(1) {
		t.Errorf("block 1 should be dirty after write")
	}
	if cow.blockDirty(0) || cow.blockDirty(2) {
		t.Errorf("only block 1 should be dirty")
	}

	// Read block 1 back: should get our X's.
	buf := make([]byte, 4096)
	if _, err := cow.ReadAt(buf, 4096); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, payload) {
		t.Fatalf("dirty block read mismatch")
	}

	// Read block 0: should get base data.
	if _, err := cow.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf, []byte("BASEBASEBASE")) {
		t.Fatalf("clean block should fall through to base, got %q", buf[:12])
	}
}

func TestBlockCOW_BitmapRebuildFromExistingDiff(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "diff.ext4")
	const size = 8 * 4096

	// Pre-create a sparse diff with data at block 2 only.
	f, err := os.Create(diff)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{'A'}, 4096)
	if _, err := f.WriteAt(payload, 2*4096); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cow, err := OpenBlockCOW(diff, nil, size)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	if !cow.blockDirty(2) {
		t.Errorf("rebuilt bitmap should mark block 2 dirty")
	}
	if cow.blockDirty(0) || cow.blockDirty(1) || cow.blockDirty(3) {
		t.Errorf("rebuilt bitmap marked extra blocks dirty")
	}
	// Verify read returns the data we pre-wrote.
	buf := make([]byte, 4096)
	if _, err := cow.ReadAt(buf, 2*4096); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, payload) {
		t.Fatalf("data mismatch after rebuild")
	}
}

func TestBlockCOW_DeclaredSizeAlignment(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "diff.ext4")
	_, err := OpenBlockCOW(diff, nil, 4097) // not 4K aligned
	if err == nil {
		t.Fatal("expected alignment error")
	}
}

func TestBlockCOW_ReadAcrossBlocks(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "diff.ext4")
	const size = 4 * 4096
	cow, err := OpenBlockCOW(diff, nil, size)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	// Write 8 KiB of 'P' starting at offset 0 (covers block 0 and 1).
	if _, err := cow.WriteAt(bytes.Repeat([]byte{'P'}, 8192), 0); err != nil {
		t.Fatal(err)
	}
	// Read 16 KiB starting at 0: should be 8 KiB of P then 8 KiB of zeros.
	buf := make([]byte, 16*1024)
	if _, err := cow.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	expected := append(bytes.Repeat([]byte{'P'}, 8192), make([]byte, 8192)...)
	if !bytes.Equal(buf, expected) {
		t.Fatalf("multi-block read mismatch")
	}
}

func TestBlockCOW_FirstPartialWriteMaterializesBase(t *testing.T) {
	tests := []struct {
		name   string
		offset int64
		length int
	}{
		{name: "sector", offset: 512, length: 512},
		{name: "partial-sector", offset: 123, length: 257},
		{name: "cross-sector", offset: 400, length: 300},
		{name: "cross-block", offset: cowBlockSize - 100, length: 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const size = 3 * cowBlockSize
			baseData := patternedBytes(size)
			base := &fakeReader{data: baseData}
			cow, err := OpenBlockCOW(filepath.Join(t.TempDir(), "diff.ext4"), base, size)
			if err != nil {
				t.Fatal(err)
			}
			defer cow.Close()

			payload := bytes.Repeat([]byte{0xa5}, tt.length)
			if n, err := cow.WriteAt(payload, tt.offset); err != nil || n != len(payload) {
				t.Fatalf("WriteAt: n=%d err=%v", n, err)
			}

			got := make([]byte, size)
			if n, err := cow.ReadAt(got, 0); err != nil || n != len(got) {
				t.Fatalf("ReadAt: n=%d err=%v", n, err)
			}
			want := append([]byte(nil), baseData...)
			copy(want[tt.offset:], payload)
			if !bytes.Equal(got, want) {
				t.Fatal("partial write did not preserve untouched base bytes")
			}
			wantDirty := 1
			if tt.offset < cowBlockSize && tt.offset+int64(tt.length) > cowBlockSize {
				wantDirty = 2
			}
			if got := cow.DirtyCount(); got != wantDirty {
				t.Fatalf("DirtyCount=%d want %d", got, wantDirty)
			}
		})
	}
}

func TestBlockCOW_FirstPartialWriteMaterializesZerosWithoutBase(t *testing.T) {
	const size = 2 * cowBlockSize
	cow, err := OpenBlockCOW(filepath.Join(t.TempDir(), "diff.ext4"), nil, size)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	payload := bytes.Repeat([]byte{0x5a}, 513)
	if n, err := cow.WriteAt(payload, 255); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: n=%d err=%v", n, err)
	}
	got := make([]byte, cowBlockSize)
	if _, err := cow.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, cowBlockSize)
	copy(want[255:], payload)
	if !bytes.Equal(got, want) {
		t.Fatal("partial write did not preserve zero-filled clean bytes")
	}
}

func TestBlockCOW_PartialWriteSurvivesReopen(t *testing.T) {
	const size = 4 * cowBlockSize
	baseData := patternedBytes(size)
	base := &fakeReader{data: baseData}
	diff := filepath.Join(t.TempDir(), "diff.ext4")
	cow, err := OpenBlockCOW(diff, base, size)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0xd3}, 512)
	const offset = cowBlockSize + 512
	if _, err := cow.WriteAt(payload, offset); err != nil {
		t.Fatal(err)
	}
	if err := cow.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := cow.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenBlockCOW(diff, base, size)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.DirtyCount(); got != 1 {
		t.Fatalf("DirtyCount after reopen=%d want 1", got)
	}
	got := make([]byte, size)
	if _, err := reopened.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), baseData...)
	copy(want[offset:], payload)
	if !bytes.Equal(got, want) {
		t.Fatal("reopened diff did not preserve materialized base bytes")
	}
}

func TestBlockCOW_DescriptorSplitPreservesMaterializedBlock(t *testing.T) {
	const size = 2 * cowBlockSize
	baseData := patternedBytes(size)
	cow, err := OpenBlockCOW(filepath.Join(t.TempDir(), "diff.ext4"), &fakeReader{data: baseData}, size)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	segments := []struct {
		offset int64
		data   []byte
	}{
		{offset: 700, data: bytes.Repeat([]byte{'a'}, 113)},
		{offset: 813, data: bytes.Repeat([]byte{'b'}, 271)},
		{offset: 1084, data: bytes.Repeat([]byte{'c'}, 509)},
	}
	want := append([]byte(nil), baseData...)
	for _, seg := range segments {
		if n, err := cow.WriteAt(seg.data, seg.offset); err != nil || n != len(seg.data) {
			t.Fatalf("WriteAt(%d): n=%d err=%v", seg.offset, n, err)
		}
		copy(want[seg.offset:], seg.data)
	}
	got := make([]byte, cowBlockSize)
	if _, err := cow.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want[:cowBlockSize]) {
		t.Fatal("descriptor-split writes lost base or prior segment bytes")
	}
}

func TestBlockCOW_ConcurrentOverlappingFirstWrites(t *testing.T) {
	const blocks = 64
	const size = blocks * cowBlockSize
	baseData := patternedBytes(size)
	cow, err := OpenBlockCOW(filepath.Join(t.TempDir(), "diff.ext4"), &fakeReader{data: baseData}, size)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	start := make(chan struct{})
	var wg sync.WaitGroup
	for blk := 0; blk < blocks; blk++ {
		blockOffset := int64(blk * cowBlockSize)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := cow.WriteAt(bytes.Repeat([]byte{'x'}, 1024), blockOffset); err != nil {
				t.Errorf("first overlapping write: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := cow.WriteAt(bytes.Repeat([]byte{'y'}, 1024), blockOffset+512); err != nil {
				t.Errorf("second overlapping write: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	for blk := 0; blk < blocks; blk++ {
		blockOffset := blk * cowBlockSize
		got := make([]byte, cowBlockSize)
		if _, err := cow.ReadAt(got, int64(blockOffset)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[:512], bytes.Repeat([]byte{'x'}, 512)) {
			t.Fatalf("block %d lost first writer's non-overlap", blk)
		}
		overlap := got[512:1024]
		if !bytes.Equal(overlap, bytes.Repeat([]byte{'x'}, 512)) &&
			!bytes.Equal(overlap, bytes.Repeat([]byte{'y'}, 512)) {
			t.Fatalf("block %d has torn overlap", blk)
		}
		if !bytes.Equal(got[1024:1536], bytes.Repeat([]byte{'y'}, 512)) {
			t.Fatalf("block %d lost second writer's non-overlap", blk)
		}
		if !bytes.Equal(got[1536:], baseData[blockOffset+1536:blockOffset+cowBlockSize]) {
			t.Fatalf("block %d lost untouched base tail", blk)
		}
	}
}

func TestBlockCOW_FailedMaterializationDoesNotMarkDirty(t *testing.T) {
	const size = cowBlockSize
	cow, err := OpenBlockCOW(filepath.Join(t.TempDir(), "diff.ext4"), &fakeReader{data: patternedBytes(size)}, size)
	if err != nil {
		t.Fatal(err)
	}
	if err := cow.diff.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cow.WriteAt([]byte("partial"), 17); err == nil {
		t.Fatal("WriteAt succeeded with closed diff")
	}
	if cow.blockDirty(0) {
		t.Fatal("failed materialization marked block dirty")
	}
}

func patternedBytes(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte((i*31 + 7) % 251)
	}
	return b
}
