package diffstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
)

type bitmapIO struct {
	data    []byte
	calls   int
	failAt  int
	short   bool
	sizes   []int
	offsets []int64
}

var errBitmapIO = errors.New("injected bitmap I/O failure")

func (m *bitmapIO) WriteAt(p []byte, off int64) (int, error) {
	m.calls++
	m.sizes = append(m.sizes, len(p))
	m.offsets = append(m.offsets, off)
	if m.calls == m.failAt {
		if m.short {
			return len(p) - 1, nil
		}
		return 0, errBitmapIO
	}
	return copy(m.data[off:], p), nil
}
func (m *bitmapIO) ReadAt(p []byte, off int64) (int, error) {
	m.calls++
	m.sizes = append(m.sizes, len(p))
	m.offsets = append(m.offsets, off)
	if m.calls == m.failAt {
		if m.short {
			return len(p) - 1, io.EOF
		}
		return 0, errBitmapIO
	}
	return copy(p, m.data[off:]), nil
}

func TestSingleBitmapBatches(t *testing.T) {
	for _, pages := range []int{1, 63, 64, 65, 129} {
		t.Run(fmt.Sprint(pages), func(t *testing.T) {
			// One byte in the last metadata page exercises buffer reuse and tail padding.
			size := int64((pages-1)*presence.PayloadSize*8+1) * cowBlockSize
			bitmap := make([]byte, presence.Bytes(size))
			for i := range bitmap {
				bitmap[i] = byte(i * 37)
			}
			bitmap[len(bitmap)-1] = 1
			m := &bitmapIO{data: make([]byte, singleBodyOffset(size))}
			if err := writeSingleBitmap(m, bitmap, size); err != nil {
				t.Fatal(err)
			}
			if want := (pages + singleBitmapBatchPages - 1) / singleBitmapBatchPages; m.calls != want {
				t.Fatalf("calls=%d want=%d", m.calls, want)
			}
			next := int64(diffHeaderRegionSize)
			for i, n := range m.sizes {
				if n > 256<<10 || n%cowBlockSize != 0 || m.offsets[i] != next {
					t.Fatal("unbounded or out-of-order batch")
				}
				next += int64(n)
			}
			for i := 0; i < pages; i++ {
				off := diffHeaderRegionSize + int64(i)*cowBlockSize
				if !bytes.Equal(m.data[off:off+cowBlockSize], presence.Encode(bitmap, size, i)) {
					t.Fatalf("page %d changed", i)
				}
			}
			got := make([]byte, len(bitmap))
			m.calls = 0
			m.sizes = nil
			m.offsets = nil
			if err := readSingleBitmap(m, got, size); err != nil {
				t.Fatal(err)
			}
			if want := (pages + singleBitmapBatchPages - 1) / singleBitmapBatchPages; m.calls != want {
				t.Fatalf("read calls=%d want=%d", m.calls, want)
			}
			next = int64(diffHeaderRegionSize)
			for i, n := range m.sizes {
				if n > 256<<10 || n%cowBlockSize != 0 || m.offsets[i] != next {
					t.Fatal("unbounded or out-of-order read")
				}
				next += int64(n)
			}
			if !bytes.Equal(got, bitmap) {
				t.Fatal("bitmap changed on reopen")
			}
		})
	}
}

func TestSingleBitmapBatchErrors(t *testing.T) {
	size := int64(128*presence.PayloadSize*8+1) * cowBlockSize
	bitmap := make([]byte, presence.Bytes(size))
	good := &bitmapIO{data: make([]byte, singleBodyOffset(size))}
	if err := writeSingleBitmap(good, bitmap, size); err != nil {
		t.Fatal(err)
	}
	for _, call := range []int{1, 2, 3} {
		for _, short := range []bool{false, true} {
			m := &bitmapIO{data: append([]byte(nil), good.data...), failAt: call, short: short}
			err := writeSingleBitmap(m, bitmap, size)
			want := errBitmapIO
			if short {
				want = io.ErrShortWrite
			}
			if !errors.Is(err, want) || m.calls != call {
				t.Fatalf("write call=%d short=%v: %v (%d calls)", call, short, err, m.calls)
			}
			m.calls = 0
			err = readSingleBitmap(m, make([]byte, len(bitmap)), size)
			want = errBitmapIO
			if short {
				want = io.ErrUnexpectedEOF
			}
			if !errors.Is(err, want) || m.calls != call {
				t.Fatalf("read call=%d short=%v: %v (%d calls)", call, short, err, m.calls)
			}
		}
	}
	for _, page := range []int{0, 63, 64, 127, 128} {
		m := &bitmapIO{data: append([]byte(nil), good.data...)}
		m.data[diffHeaderRegionSize+int64(page)*cowBlockSize+presence.HeaderSize] ^= 1
		if err := readSingleBitmap(m, make([]byte, len(bitmap)), size); err == nil {
			t.Fatalf("accepted corrupt page %d", page)
		}
	}
	m := &bitmapIO{data: append([]byte(nil), good.data...)}
	a := diffHeaderRegionSize + 63*cowBlockSize
	b := a + cowBlockSize
	copy(m.data[a:a+cowBlockSize], good.data[b:b+cowBlockSize])
	copy(m.data[b:b+cowBlockSize], good.data[a:a+cowBlockSize])
	if err := readSingleBitmap(m, make([]byte, len(bitmap)), size); err == nil {
		t.Fatal("accepted reordered pages")
	}
}

func TestBitmapConversionsMatchScalar(t *testing.T) {
	rng := rand.New(rand.NewSource(91))
	for pages := 1; pages <= 257; pages++ {
		raw := make([]byte, (pages+7)/8)
		rng.Read(raw)
		raw[len(raw)-1] &= byte(255 >> ((8 - pages%8) % 8))
		d := &diffFile{logicalSize: int64(pages) * cowBlockSize, bitmap: raw}
		got, err := d.scanDirtyBlocks()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(got)*64; i++ {
			want := i < pages && raw[i/8]&(1<<uint(i%8)) != 0
			if bitmapBlockDirty(got, int64(i)) != want {
				t.Fatalf("conversion pages=%d bit=%d", pages, i)
			}
		}
		raw[0] ^= 1
		if bitmapBlockDirty(got, 0) == (raw[0]&1 != 0) {
			t.Fatal("conversion aliases source")
		}
		for start := 0; start <= pages; start++ {
			end := start + rng.Intn(pages-start+1)
			words := make([]uint64, (pages+63)/64)
			for i := range words {
				words[i] = rng.Uint64()
			}
			want := append([]uint64(nil), words...)
			for bit := start; bit < end; bit++ {
				want[bit/64] |= uint64(1) << uint(bit%64)
			}
			setBitmapRange(words, uint64(start), uint64(end))
			for i := range words {
				if words[i] != want[i] {
					t.Fatalf("range [%d,%d) word=%d", start, end, i)
				}
			}
		}
	}
}

func TestSingleBitmapBatchSparseReopen(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			const size = int64(65*presence.PayloadSize*8+1) * cowBlockSize
			path := filepath.Join(t.TempDir(), "disk")
			var opts []DiskOption
			if encrypted {
				opts = append(opts, WithDiffEncryption([32]byte{7, 9}, true))
			}
			disk, err := OpenDisk(path, nil, DiffInit{CreateSize: size}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if disk != nil {
					disk.Close()
				}
			}()
			if err := disk.WaitMaterialized(context.Background()); err != nil {
				t.Fatal(err)
			}
			offsets := []int64{0, 63*presence.PayloadSize*8*cowBlockSize - cowBlockSize, 64 * presence.PayloadSize * 8 * cowBlockSize, size - cowBlockSize}
			payload := bytes.Repeat([]byte{0x9d}, cowBlockSize)
			for _, off := range offsets {
				if _, err := disk.WriteAt(payload, off); err != nil {
					t.Fatal(err)
				}
			}
			zero := int64(64*presence.PayloadSize*8*cowBlockSize + cowBlockSize)
			if err := disk.Discard(zero, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			if err := disk.Close(); err != nil {
				t.Fatal(err)
			}
			disk = nil
			disk, err = OpenDisk(path, nil, DiffInit{Existing: true}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, cowBlockSize)
			for _, off := range offsets {
				if n, err := disk.ReadAt(got, off); err != nil || n != len(got) || !bytes.Equal(got, payload) {
					t.Fatalf("read at %d: %d %v", off, n, err)
				}
			}
			if _, err := disk.ReadAt(got, zero); err != nil || !bytes.Equal(got, make([]byte, cowBlockSize)) {
				t.Fatalf("zero read: %v", err)
			}
		})
	}
}
