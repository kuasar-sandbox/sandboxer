package diffstore

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
	"golang.org/x/crypto/xts"
	"golang.org/x/sys/unix"
)

// Single-layer v3 has a 4 KiB header, a capacity-sized presence area and a
// logically addressed sparse data region. v4 is the checkpoint record format.
func singleBodyOffset(size int64) int64 {
	return diffHeaderRegionSize + int64(presence.Pages(size))*cowBlockSize
}

func createDiffFile(f *os.File, size int64, encryption *diffEncryption, random io.Reader) (*diffFile, error) {
	if err := validateDiffLogicalSize(size); err != nil {
		return nil, err
	}
	var header [diffHeaderPlainSize]byte
	defer clear(header[:])
	binary.BigEndian.PutUint64(header[0:], uint64(size))
	binary.BigEndian.PutUint32(header[8:], cowBlockSize)
	binary.BigEndian.PutUint32(header[12:], uint32(diffDataUnitSize))
	binary.BigEndian.PutUint64(header[16:], uint64(singleBodyOffset(size)))
	binary.BigEndian.PutUint64(header[24:], uint64(diffHeaderRegionSize))
	binary.BigEndian.PutUint64(header[32:], uint64(presence.Bytes(size)))
	binary.BigEndian.PutUint32(header[40:], uint32(presence.Pages(size)))
	var xc *xts.Cipher
	if encryption != nil {
		binary.BigEndian.PutUint32(header[44:], diffXTSKeySize)
		if _, err := io.ReadFull(random, header[48:112]); err != nil {
			return nil, fmt.Errorf("diffstore: generate XTS key: %w", err)
		}
		var err error
		xc, err = xts.NewCipher(aes.NewCipher, header[48:112])
		if err != nil {
			return nil, err
		}
	}
	region := make([]byte, diffHeaderRegionSize)
	defer clear(region)
	copy(region, diffMagic[:])
	binary.BigEndian.PutUint16(region[8:], diffVersion)
	binary.BigEndian.PutUint16(region[10:], diffPrefixSize)
	if encryption == nil {
		copy(region[diffPrefixSize:], header[:])
	} else {
		binary.BigEndian.PutUint32(region[12:], 1)
		nonce := region[diffPrefixSize : diffPrefixSize+diffHeaderNonceSize]
		if _, err := io.ReadFull(random, nonce); err != nil {
			return nil, err
		}
		start := diffPrefixSize + diffHeaderNonceSize
		encryption.header.Seal(region[start:start], nonce, header[:], diffHeaderAAD(region[:diffPrefixSize]))
	}
	binary.LittleEndian.PutUint32(region[len(region)-4:], crc32.ChecksumIEEE(region[:len(region)-4]))
	if err := f.Truncate(singleBodyOffset(size) + size); err != nil {
		return nil, err
	}
	if err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, singleBodyOffset(size)); err != nil {
		return nil, err
	}
	if err := writeFullAt(f, region, 0); err != nil {
		return nil, err
	}
	bitmap := make([]byte, presence.Bytes(size))
	if err := writeSingleBitmap(f, bitmap, size); err != nil {
		return nil, err
	}

	return &diffFile{f: f, bodyIO: f, logicalSize: size, bodyOffset: singleBodyOffset(size),
		encrypted: encryption != nil, xts: xc, bitmap: bitmap, payload: make([]uint64, (size/cowBlockSize+63)/64)}, nil
}

func readSingleHeader(f *os.File, physical int64, encryption *diffEncryption, required bool) (*diffFile, error) {
	region := make([]byte, diffHeaderRegionSize)
	defer clear(region)
	if err := readFullAt(f, region, 0); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	if !bytes.Equal(region[:8], diffMagic[:]) || binary.BigEndian.Uint16(region[8:]) != diffVersion {
		return nil, ErrFormat
	}
	flags := binary.BigEndian.Uint32(region[12:])
	if binary.BigEndian.Uint16(region[10:]) != diffPrefixSize || flags > 1 ||
		binary.LittleEndian.Uint32(region[len(region)-4:]) != crc32.ChecksumIEEE(region[:len(region)-4]) {
		return nil, fmt.Errorf("diffstore: invalid v3 header")
	}
	var plain []byte
	var xc *xts.Cipher
	end := diffPrefixSize + diffHeaderPlainSize
	if flags == 0 {
		if required {
			return nil, ErrDiffPlaintextForbidden
		}
		plain = region[diffPrefixSize:end]
	} else {
		if encryption == nil {
			return nil, ErrDiffEncryptionRequired
		}
		end = diffPrefixSize + diffHeaderSealedSize
		start := diffPrefixSize + diffHeaderNonceSize
		var err error
		plain, err = encryption.header.Open(nil, region[diffPrefixSize:start], region[start:end], diffHeaderAAD(region[:diffPrefixSize]))
		if err != nil {
			return nil, ErrDiffAuthentication
		}
		defer clear(plain)
	}
	if !allZero(region[end:len(region)-4]) || len(plain) != diffHeaderPlainSize {
		return nil, fmt.Errorf("diffstore: invalid v3 header padding")
	}
	size := int64(binary.BigEndian.Uint64(plain))
	if err := validateDiffLogicalSize(size); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(plain[8:]) != cowBlockSize || binary.BigEndian.Uint32(plain[12:]) != uint32(diffDataUnitSize) ||
		binary.BigEndian.Uint64(plain[16:]) != uint64(singleBodyOffset(size)) || binary.BigEndian.Uint64(plain[24:]) != uint64(diffHeaderRegionSize) ||
		binary.BigEndian.Uint64(plain[32:]) != uint64(presence.Bytes(size)) || binary.BigEndian.Uint32(plain[40:]) != uint32(presence.Pages(size)) ||
		physical != singleBodyOffset(size)+size || !allZero(plain[112:]) {
		return nil, fmt.Errorf("diffstore: inconsistent v3 layout")
	}
	if flags == 0 {
		if !allZero(plain[44:112]) {
			return nil, fmt.Errorf("diffstore: key in plaintext v3 header")
		}
	} else {
		if binary.BigEndian.Uint32(plain[44:]) != diffXTSKeySize {
			return nil, fmt.Errorf("diffstore: invalid v3 key length")
		}
		var err error
		xc, err = xts.NewCipher(aes.NewCipher, plain[48:112])
		if err != nil {
			return nil, err
		}
	}
	d := &diffFile{f: f, bodyIO: f, encrypted: flags == 1, logicalSize: size, bodyOffset: singleBodyOffset(size), xts: xc, bitmap: make([]byte, presence.Bytes(size))}
	if err := readSingleBitmap(f, d.bitmap, size); err != nil {
		return nil, err
	}

	var err error
	d.payload, err = d.scanPayload()
	if err != nil {
		return nil, err
	}
	return d, nil
}

// Creation and reopen use buffered metadata I/O before the direct workspace is
// attached. Keep per-page encoding/validation, but bound scratch and syscalls.
const singleBitmapBatchPages = (256 << 10) / cowBlockSize

func writeSingleBitmap(w io.WriterAt, bitmap []byte, size int64) error {
	pages := presence.Pages(size)
	buf := make([]byte, min(singleBitmapBatchPages, pages)*cowBlockSize)
	for i := 0; i < pages; {
		n := min(singleBitmapBatchPages, pages-i)
		for j := 0; j < n; j++ {
			presence.EncodeInto(buf[j*cowBlockSize:(j+1)*cowBlockSize], bitmap, size, i+j)
		}
		if err := writeFullAt(w, buf[:n*cowBlockSize], diffHeaderRegionSize+int64(i)*cowBlockSize); err != nil {
			return err
		}
		i += n
	}
	return nil
}

// The caller owns bitmap privately until every page and payload scan succeeds.
func readSingleBitmap(r io.ReaderAt, bitmap []byte, size int64) error {
	pages := presence.Pages(size)
	buf := make([]byte, min(singleBitmapBatchPages, pages)*cowBlockSize)
	for i := 0; i < pages; {
		n := min(singleBitmapBatchPages, pages-i)
		if err := readFullAt(r, buf[:n*cowBlockSize], diffHeaderRegionSize+int64(i)*cowBlockSize); err != nil {
			return err
		}
		for j := 0; j < n; j++ {
			part, err := presence.Decode(buf[j*cowBlockSize:(j+1)*cowBlockSize], size, i+j)
			if err != nil {
				return err
			}
			copy(bitmap[(i+j)*presence.PayloadSize:], part)
		}
		i += n
	}
	return nil
}
