package vhost

import (
	"errors"
	"fmt"
	"io"
	"math"
	"syscall"

	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

const (
	maxReadWindow = 1 << 20
	// A 256 KiB direct read already matches the coalesced 64 x 4 KiB
	// workload. Preserve such spans without making a small trailing I/O.
	minDirectSpan = 256 << 10
)

// readPayload walks device-writable slices in descriptor order. They may
// be discontiguous or run backwards in guest memory.
func (q *virtq) readPayload(backend Backend, sector uint64, segs [][]byte) (int, error) {
	// used.len includes the status byte. Check the entire range before
	// delivering any window so a later offset or count cannot wrap.
	var total int
	for _, seg := range segs {
		if len(seg) > min(math.MaxUint32-1, math.MaxInt-1)-total {
			return 0, errors.New("vhost: read exceeds used length")
		}
		total += len(seg)
	}
	if sector > math.MaxInt64/SectorSize {
		return 0, errors.New("vhost: sector offset overflow")
	}
	offset := int64(sector) * SectorSize
	// GET_CONFIG publishes capacity in complete sectors, including for RO sources.
	if size := backend.Size(); size < 0 || int64(total) > size/SectorSize*SectorSize-offset {
		return 0, errors.New("vhost: read exceeds disk capacity")
	}
	done := 0
	for done < total {
		if err := q.stopped(); err != nil {
			return done, err
		}
		length := min(total-done, maxReadWindow)
		n, err := q.readWindow(backend, offset+int64(done), segs, length)
		done += n
		if err != nil {
			return done, err
		}
		// A window can end in the middle of a descriptor.
		for n > 0 {
			if n < len(segs[0]) {
				segs[0] = segs[0][n:]
				break
			}
			n -= len(segs[0])
			segs = segs[1:]
		}
	}
	return done, nil
}

// Each slice has already been translated, loaded and marked writable. If
// the whole window matches consecutive addresses within the first mapping,
// it is also a single ReaderAt destination. Never extend past that mapping.
func contiguousWindow(segs [][]byte, length int) []byte {
	first := segs[0]
	if len(first) >= length {
		return first[:length:length]
	}
	if cap(first) < length {
		return nil
	}
	buf := first[:length:length]
	pos := len(first)
	for _, seg := range segs[1:] {
		if &buf[pos] != &seg[0] {
			return nil
		}
		if len(seg) >= length-pos {
			return buf
		}
		pos += len(seg)
	}
	return nil
}

func (q *virtq) readWindow(backend Backend, offset int64, segs [][]byte, length int) (int, error) {
	buf := contiguousWindow(segs, length)
	if buf == nil && len(segs[0]) >= minDirectSpan && length-len(segs[0]) >= minDirectSpan {
		length = len(segs[0])
		buf = segs[0][:length:length]
	}
	gather := buf == nil
	if gather {
		if q.readScratch == nil {
			q.readScratch = make([]byte, maxReadWindow)
		}
		buf = q.readScratch[:length:length]
		defer clear(buf)
	}
	n, err := readBackendAt(q.readContext(), backend, buf, offset)
	if readretry.IsTerminal(err) {
		return 0, err
	}
	if stopped := q.stopped(); stopped != nil {
		return 0, stopped
	}
	if n < 0 || n > length {
		return 0, fmt.Errorf("vhost: ReadAt returned %d for a %d-byte buffer", n, length)
	}
	if gather {
		prefix := buf[:n]
		for _, seg := range segs {
			if len(prefix) == 0 {
				break
			}
			if stopped := q.stopped(); stopped != nil {
				return 0, stopped
			}
			prefix = prefix[copy(seg, prefix):]
		}
	}
	if err != nil && n == length && completeReadCompatible(err) {
		err = nil
	}
	if n != length && err == nil {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// A joined error can contain an ordinary failure alongside EOF or EAGAIN.
func completeReadCompatible(err error) bool {
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range wrapped.Unwrap() {
			if !completeReadCompatible(cause) {
				return false
			}
		}
		return len(wrapped.Unwrap()) > 0
	case interface{ Unwrap() error }:
		return completeReadCompatible(wrapped.Unwrap())
	default:
		return errors.Is(err, io.EOF) || errors.Is(err, syscall.EAGAIN)
	}
}
