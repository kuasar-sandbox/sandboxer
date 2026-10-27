// Package presence owns the bounded, page-aligned logical presence bitmap used
// by both writable DIFF formats. Payload locations remain backend-specific.
package presence

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/bits"
)

const (
	PageSize    = 4096
	MaxSize     = int64(1 << 40)
	HeaderSize  = 32
	PayloadSize = PageSize - HeaderSize - 4
)

var magic = [8]byte{'K', 'D', 'P', 'R', 'E', 'S', '1', '\n'}
var le = binary.LittleEndian

func ValidateSize(size int64) error {
	if size <= 0 || size%PageSize != 0 || size > MaxSize {
		return fmt.Errorf("diffstore: writable capacity must be a positive 4 KiB multiple no larger than 1 TiB")
	}
	return nil
}

// Bytes and Pages require a validated capacity.
func Bytes(size int64) int                 { return int((size/PageSize + 7) / 8) }
func Pages(size int64) int                 { return (Bytes(size) + PayloadSize - 1) / PayloadSize }
func Has(bitmap []byte, block uint64) bool { return bitmap[block/8]&(1<<(block%8)) != 0 }
func Set(bitmap []byte, block uint64) {
	bitmap[block/8] |= byte(1 << (block % 8))
}

// End returns the exclusive end of the logical-presence run starting at block.
// It does not describe whether the run has physical payload.
func End(p []byte, page, pages uint64) uint64 {
	set := Has(p, page)
	page++
	for page < pages {
		start := (page / 64) * 8
		if start+8 <= uint64(len(p)) {
			w := binary.LittleEndian.Uint64(p[start : start+8])
			if set {
				w = ^w
			}
			shift := page % 64
			w >>= shift
			if w != 0 {
				return min(pages, page+uint64(bits.TrailingZeros64(w)))
			}
			page += 64 - shift
		} else {
			w := p[page/8]
			if set {
				w = ^w
			}
			shift := page % 8
			w >>= shift
			if w != 0 {
				return min(pages, page+uint64(bits.TrailingZeros8(w)))
			}
			page += 8 - shift
		}
	}
	return pages
}

// Encode copies one bounded bitmap page. The full logical length, ordinal and
// page count are repeated so a page cannot silently move within a bitmap.
func Encode(bitmap []byte, size int64, index int) []byte {
	b := make([]byte, PageSize)
	EncodeInto(b, bitmap, size, index)
	return b
}

// EncodeInto replaces one complete page, including padding, in caller-owned
// scratch. The destination must not overlap bitmap.
func EncodeInto(b, bitmap []byte, size int64, index int) {
	if len(b) != PageSize || len(bitmap) != Bytes(size) || index < 0 || index >= Pages(size) {
		panic("diffstore: invalid presence page")
	}
	clear(b)
	copy(b, magic[:])
	le.PutUint32(b[8:], uint32(index))
	le.PutUint32(b[12:], uint32(Pages(size)))
	le.PutUint64(b[16:], uint64(size))
	start := index * PayloadSize
	copy(b[HeaderSize:PageSize-4], bitmap[start:min(start+PayloadSize, len(bitmap))])
	le.PutUint32(b[PageSize-4:], crc32.ChecksumIEEE(b[:PageSize-4]))
}

// Decode validates before returning the useful bytes. The caller copies them
// into its complete bitmap; no partially validated bitmap may be published.
func Decode(b []byte, size int64, index int) ([]byte, error) {
	if err := ValidateSize(size); err != nil {
		return nil, err
	}
	if len(b) != PageSize || index < 0 || index >= Pages(size) ||
		!bytes.Equal(b[:8], magic[:]) || le.Uint32(b[8:]) != uint32(index) ||
		le.Uint32(b[12:]) != uint32(Pages(size)) || le.Uint64(b[16:]) != uint64(size) ||
		le.Uint64(b[24:]) != 0 || le.Uint32(b[PageSize-4:]) != crc32.ChecksumIEEE(b[:PageSize-4]) {
		return nil, fmt.Errorf("diffstore: invalid presence bitmap page")
	}
	n := min(PayloadSize, Bytes(size)-index*PayloadSize)
	for _, v := range b[HeaderSize+n : PageSize-4] {
		if v != 0 {
			return nil, fmt.Errorf("diffstore: nonzero presence bitmap padding")
		}
	}
	p := b[HeaderSize : HeaderSize+n]
	if index == Pages(size)-1 && size/PageSize%8 != 0 && p[n-1]>>uint(size/PageSize%8) != 0 {
		return nil, fmt.Errorf("diffstore: presence bitmap extends beyond logical capacity")
	}
	return p, nil
}
