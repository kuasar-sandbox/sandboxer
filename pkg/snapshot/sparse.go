package snapshot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"golang.org/x/sys/unix"
)

const (
	seekData = 3 // SEEK_DATA
	seekHole = 4 // SEEK_HOLE
)

// WalkHoles returns the hole extents in [0, size) of fd — the raw-fd,
// size-bounded sibling of sparse.ProbeHoles (memfds and sections have
// no *os.File to hand over). The returned ranges are non-overlapping,
// sorted, and disjoint.
//
// Implementation walks SEEK_DATA / SEEK_HOLE alternately. A trailing
// hole (no data after offset) is included.
func WalkHoles(fd int, size int64) ([]sparse.Extent, error) {
	var holes []sparse.Extent
	var off int64
	for off < size {
		dataOff, err := syscall.Seek(fd, off, seekData)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				// No more data → rest is hole.
				if off < size {
					holes = append(holes, sparse.Extent{
						Offset: uint64(off),
						Size:   uint64(size - off),
					})
				}
				return holes, nil
			}
			return nil, fmt.Errorf("SEEK_DATA at %d: %w", off, err)
		}
		if dataOff > off {
			holes = append(holes, sparse.Extent{
				Offset: uint64(off),
				Size:   uint64(dataOff - off),
			})
		}
		holeOff, err := syscall.Seek(fd, dataOff, seekHole)
		if err != nil {
			return nil, fmt.Errorf("SEEK_HOLE at %d: %w", dataOff, err)
		}
		if holeOff > size {
			holeOff = size
		}
		off = holeOff
	}
	return holes, nil
}

// SparseCopy copies fd[0..size) into dst, preserving holes.
// SEEK_DATA / SEEK_HOLE drive the walk; only data extents are copied
// via copy_file_range (or pread/pwrite fallback). dst must support
// pwrite at offsets up to size.
//
// Returns the bytes physically copied (excluding holes).
func SparseCopy(dst *os.File, srcFD int, size int64) (int64, error) {
	// Pre-allocate logical size on dst (sparse).
	if err := dst.Truncate(size); err != nil {
		return 0, fmt.Errorf("truncate dst: %w", err)
	}
	dstFD := int(dst.Fd())

	var off int64
	var copied int64
	for off < size {
		dataOff, err := syscall.Seek(srcFD, off, seekData)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				return copied, nil
			}
			return copied, fmt.Errorf("SEEK_DATA at %d: %w", off, err)
		}
		holeOff, err := syscall.Seek(srcFD, dataOff, seekHole)
		if err != nil {
			return copied, fmt.Errorf("SEEK_HOLE at %d: %w", dataOff, err)
		}
		if holeOff > size {
			holeOff = size
		}
		extentLen := holeOff - dataOff
		if extentLen <= 0 {
			off = holeOff
			continue
		}

		// Try copy_file_range first; falls back to pread/pwrite on
		// EXDEV / EOPNOTSUPP / EINVAL (e.g. tmpfs ↔ ext4 cross-fs).
		srcCopyOff, dstCopyOff := dataOff, dataOff
		remaining := extentLen
		for remaining > 0 {
			n, err := unix.CopyFileRange(srcFD, &srcCopyOff,
				dstFD, &dstCopyOff, int(remaining), 0)
			if err != nil {
				if errors.Is(err, unix.EXDEV) ||
					errors.Is(err, unix.EOPNOTSUPP) ||
					errors.Is(err, unix.EINVAL) ||
					errors.Is(err, unix.ENOSYS) {
					// Fallback path
					if err := preadPwriteCopy(srcFD, dstFD,
						srcCopyOff, dstCopyOff, remaining); err != nil {
						return copied, err
					}
					copied += remaining
					srcCopyOff += remaining
					dstCopyOff += remaining
					remaining = 0
					break
				}
				return copied, fmt.Errorf("copy_file_range off=%d len=%d: %w",
					srcCopyOff, remaining, err)
			}
			if n == 0 {
				return copied, fmt.Errorf("copy_file_range short: 0 bytes")
			}
			copied += int64(n)
			remaining -= int64(n)
		}
		off = holeOff
	}
	return copied, nil
}

func preadPwriteCopy(srcFD, dstFD int, srcOff, dstOff, length int64) error {
	buf := make([]byte, 1<<20) // 1 MiB chunks
	for length > 0 {
		toRead := int64(len(buf))
		if toRead > length {
			toRead = length
		}
		n, err := unix.Pread(srcFD, buf[:toRead], srcOff)
		if err != nil {
			return fmt.Errorf("pread off=%d: %w", srcOff, err)
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		if _, err := unix.Pwrite(dstFD, buf[:n], dstOff); err != nil {
			return fmt.Errorf("pwrite off=%d: %w", dstOff, err)
		}
		srcOff += int64(n)
		dstOff += int64(n)
		length -= int64(n)
	}
	return nil
}
