package presence

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"math/rand"
	"testing"
)

func TestBitmapBoundariesAndCorruption(t *testing.T) {
	for _, size := range []int64{PageSize, 7 * PageSize, 8 * PageSize, 9 * PageSize, int64(PayloadSize) * 8 * PageSize, 32 << 30, MaxSize} {
		bitmap := make([]byte, Bytes(size))
		Set(bitmap, 0)
		Set(bitmap, uint64(size/PageSize-1))
		reopened := make([]byte, len(bitmap))
		for i := 0; i < Pages(size); i++ {
			b := Encode(bitmap, size, i)
			part, err := Decode(b, size, i)
			if err != nil {
				t.Fatal(err)
			}
			copy(reopened[i*PayloadSize:], part)
			b[HeaderSize] ^= 1
			if _, err = Decode(b, size, i); err == nil {
				t.Fatal("accepted corrupt bitmap")
			}
		}
		if !Has(reopened, 0) || !Has(reopened, uint64(size/PageSize-1)) {
			t.Fatal("lost boundary bit")
		}
	}
	for _, size := range []int64{-PageSize, 0, 1, MaxSize + PageSize} {
		if ValidateSize(size) == nil {
			t.Fatalf("accepted invalid capacity %d", size)
		}
	}
	b := Encode([]byte{1}, PageSize, 0)
	b[HeaderSize] = 3
	binary.LittleEndian.PutUint32(b[PageSize-4:], crc32.ChecksumIEEE(b[:PageSize-4]))
	if _, err := Decode(b, PageSize, 0); err == nil {
		t.Fatal("accepted out-of-capacity presence bit with valid checksum")
	}
	if Bytes(MaxSize) != 32<<20 || Pages(MaxSize) != 8265 {
		t.Fatal("maximum bitmap layout changed")
	}
}

func TestEndMatchesScalar(t *testing.T) {
	rng := rand.New(rand.NewSource(37))
	for _, pages := range []uint64{1, 7, 8, 9, 63, 64, 65, 127, 128, 129, 513} {
		for pattern := 0; pattern < 5; pattern++ {
			bitmap := make([]byte, (pages+7)/8)
			for i := range bitmap {
				switch pattern {
				case 1:
					bitmap[i] = 255
				case 2:
					bitmap[i] = 0x55
				case 3:
					bitmap[i] = 0xaa
				case 4:
					bitmap[i] = byte(rng.Intn(256))
				}
			}
			for start := uint64(0); start < pages; start++ {
				for limit := start + 1; limit <= pages; limit++ {
					want := start + 1
					for want < limit && Has(bitmap, want) == Has(bitmap, start) {
						want++
					}
					if got := End(bitmap, start, limit); got != want {
						t.Fatalf("pattern=%d start=%d limit=%d got=%d want=%d", pattern, start, limit, got, want)
					}
				}
			}
		}
	}
}

func TestEncodeIntoCanonicalReuse(t *testing.T) {
	for _, size := range []int64{PageSize, 9 * PageSize, int64(PayloadSize)*8*PageSize + PageSize} {
		bitmap := make([]byte, Bytes(size))
		Set(bitmap, 0)
		Set(bitmap, uint64(size/PageSize-1))
		scratch := bytes.Repeat([]byte{0xff}, PageSize)
		for i := 0; i < Pages(size); i++ {
			// Independent scalar layout oracle, including reserved and trailing zeros.
			want := make([]byte, PageSize)
			copy(want, []byte("KDPRES1\n"))
			binary.LittleEndian.PutUint32(want[8:], uint32(i))
			binary.LittleEndian.PutUint32(want[12:], uint32(Pages(size)))
			binary.LittleEndian.PutUint64(want[16:], uint64(size))
			for j := 0; j < PayloadSize && i*PayloadSize+j < len(bitmap); j++ {
				want[HeaderSize+j] = bitmap[i*PayloadSize+j]
			}
			binary.LittleEndian.PutUint32(want[PageSize-4:], crc32.ChecksumIEEE(want[:PageSize-4]))
			EncodeInto(scratch, bitmap, size, i)
			if !bytes.Equal(scratch, want) {
				t.Fatalf("size=%d page=%d encoding changed", size, i)
			}
		}
	}
}
