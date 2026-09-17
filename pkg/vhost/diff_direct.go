package vhost

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// alignedMapping is shared anonymous memory, not a Go/private heap buffer.
// Linux forbids concurrent fork with in-flight O_DIRECT into private mappings.
// Both its size and its extra alignment padding have a fixed upper bound.
type alignedMapping struct{ mapping, bytes []byte }

func newAlignedMapping(size, alignment int) (*alignedMapping, error) {
	if alignment <= 0 || alignment > maxDiffScratchSize || size <= 0 || size > maxDiffScratchSize {
		return nil, fmt.Errorf("vhost: unsupported direct I/O workspace size/alignment %d/%d", size, alignment)
	}
	raw, err := unix.Mmap(-1, 0, size+alignment-1, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("vhost: mmap direct I/O workspace: %w", err)
	}
	address := uintptr(unsafe.Pointer(&raw[0]))
	pad := (uintptr(alignment) - address%uintptr(alignment)) % uintptr(alignment)
	return &alignedMapping{mapping: raw, bytes: raw[int(pad) : int(pad)+size]}, nil
}
func (m *alignedMapping) close() error {
	if m == nil || m.mapping == nil {
		return nil
	}
	clear(m.mapping)
	err := unix.Munmap(m.mapping)
	m.mapping, m.bytes = nil, nil
	return err
}

// directWorkspace stages the common O_DIRECT API requests. The kernel decides
// how those requests are fulfilled; this is not proof of physical cache bypass.
type directWorkspace struct {
	readMu, writeMu          sync.Mutex
	read, write              *alignedMapping
	memoryAlign, offsetAlign int
}

// directBody deliberately makes one syscall. os.File.WriteAt retries a short
// write using the remaining slice, which need not satisfy DIO alignment.
type directBody struct{ f *os.File }

func (b directBody) ReadAt(p []byte, off int64) (int, error) {
	n, err := unix.Pread(int(b.f.Fd()), p, off)
	if n < 0 {
		n = 0
	}
	if err == nil && n != len(p) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
func (b directBody) WriteAt(p []byte, off int64) (int, error) {
	n, err := unix.Pwrite(int(b.f.Fd()), p, off)
	if n < 0 {
		n = 0
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func directAlignment(fd int, bodyOffset, size int64) (int, int, error) {
	var st unix.Statx_t
	err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_DIOALIGN, &st)
	return directAlignmentFromStatx(st, err, bodyOffset, size)
}

func directAlignmentFromStatx(st unix.Statx_t, queryErr error, bodyOffset, size int64) (int, int, error) {
	if queryErr != nil && !errors.Is(queryErr, unix.ENOSYS) && !errors.Is(queryErr, unix.EINVAL) && !errors.Is(queryErr, unix.EOPNOTSUPP) {
		return 0, 0, fmt.Errorf("statx DIOALIGN: %w", queryErr)
	}
	// Missing constraints (including a both-zero report) do not identify the
	// backing filesystem. Use the format's conservative alignment and let
	// O_DIRECT setup and actual I/O determine whether operations are supported.
	memory, offset := 4096, 4096
	if queryErr == nil && st.Mask&unix.STATX_DIOALIGN != 0 && (st.Dio_mem_align != 0 || st.Dio_offset_align != 0) {
		memory, offset = int(st.Dio_mem_align), int(st.Dio_offset_align)
	}
	if err := validateDirectAlignment(memory, offset, bodyOffset, size); err != nil {
		return 0, 0, err
	}
	return memory, offset, nil
}

func validateDirectAlignment(memory, offset int, bodyOffset, size int64) error {
	if memory <= 0 || memory > maxDiffScratchSize || offset <= 0 || cowBlockSize%offset != 0 || bodyOffset%int64(offset) != 0 || size%int64(offset) != 0 {
		return fmt.Errorf("unsupported direct I/O alignment: address=%d offset/length=%d body=%d size=%d", memory, offset, bodyOffset, size)
	}
	return nil
}

// enableDirect runs after bounded header probing/creation and before *any*
// active body access, including provisioning from a read-only template.
func (d *diffFile) enableDirect() error {
	return d.enableDirectWithFcntl(unix.FcntlInt)
}

// The callback is a per-call test seam, not a filesystem policy or runtime mode.
func (d *diffFile) enableDirectWithFcntl(fcntl func(uintptr, int, int) (int, error)) error {
	if d.direct != nil {
		return nil
	}
	memory, offset, err := directAlignment(int(d.f.Fd()), d.bodyOffset, d.logicalSize)
	if err != nil {
		return fmt.Errorf("vhost: direct I/O %s: %w", d.f.Name(), err)
	}
	r, err := newAlignedMapping(maxDiffScratchSize, memory)
	if err != nil {
		return err
	}
	w, err := newAlignedMapping(maxDiffScratchSize, memory)
	if err != nil {
		return errors.Join(err, r.close())
	}
	flags, err := fcntl(d.f.Fd(), unix.F_GETFL, 0)
	if err == nil {
		_, err = fcntl(d.f.Fd(), unix.F_SETFL, flags|unix.O_DIRECT)
		// O_DIRECT is optional; unsupported setup keeps the same owned buffers.
		// This does not retry any failed data I/O or ignore other setup errors.
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			err = nil
		}
	}
	if err != nil {
		return errors.Join(fmt.Errorf("vhost: enable O_DIRECT %s: %w", d.f.Name(), err), r.close(), w.close())
	}
	d.direct = &directWorkspace{read: r, write: w, memoryAlign: memory, offsetAlign: offset}
	d.bodyIO = directBody{d.f}
	return nil
}

// withDirectRead keeps plaintext in the fixed owned mapping until consume has
// published any cache pages and copied out. Caller/guest memory is never a
// trusted source for clean cache contents. consume must not retain the slice.
func (d *diffFile) withDirectRead(length int, offset int64, consume func([]byte) error) error {
	d.direct.readMu.Lock()
	defer d.direct.readMu.Unlock()
	if d.direct.read.bytes == nil {
		return os.ErrClosed
	}
	start := offset / cowBlockSize * cowBlockSize
	end := alignUp(offset+int64(length), cowBlockSize)
	if length <= 0 || end-start > maxDiffScratchSize {
		return fmt.Errorf("vhost: invalid bounded direct read length %d", length)
	}
	scratch := d.direct.read.bytes[:int(end-start)]
	defer clear(scratch)
	if err := readFullAt(d.bodyIO, scratch, d.bodyOffset+start); err != nil {
		return err
	}
	d.cryptUnits(scratch, start, false)
	return consume(scratch[offset-start : offset-start+int64(length)])
}

func (d *diffFile) directReadAt(buf []byte, offset int64) (int, error) {
	done := 0
	for done < len(buf) {
		pos := offset + int64(done)
		length := min(len(buf)-done, maxDiffScratchSize-int(pos%cowBlockSize))
		err := d.withDirectRead(length, pos, func(plain []byte) error {
			copy(buf[done:done+length], plain)
			return nil
		})
		if err != nil {
			return done, err
		}
		done += length
	}
	return done, nil
}
func (d *diffFile) directWriteAt(buf []byte, offset int64) (int, error) {
	d.direct.writeMu.Lock()
	defer d.direct.writeMu.Unlock()
	if d.direct.write.bytes == nil {
		return 0, os.ErrClosed
	}
	used := 0
	defer func() { clear(d.direct.write.bytes[:used]) }()
	done := 0
	for done < len(buf) {
		pos := offset + int64(done)
		start := pos / cowBlockSize * cowBlockSize
		length := min(len(buf)-done, maxDiffScratchSize-int(pos-start))
		end := alignUp(pos+int64(length), cowBlockSize)
		scratch := d.direct.write.bytes[:int(end-start)]
		used = max(used, len(scratch))
		if pos != start || pos+int64(length) != end {
			if err := readFullAt(d.bodyIO, scratch, d.bodyOffset+start); err != nil {
				return done, err
			}
			d.cryptUnits(scratch, start, false)
		}
		copy(scratch[pos-start:pos-start+int64(length)], buf[done:done+length])
		d.cryptUnits(scratch, start, true)
		if err := writeFullAt(d.bodyIO, scratch, d.bodyOffset+start); err != nil {
			return done, err
		}
		done += length
	}
	return done, nil
}
func (d *diffFile) cryptUnits(buf []byte, start int64, encrypt bool) {
	if !d.encrypted {
		return
	}
	for off := 0; off < len(buf); off += int(diffDataUnitSize) {
		sector := buf[off : off+int(diffDataUnitSize)]
		number := uint64((start + int64(off)) / diffDataUnitSize)
		if encrypt {
			d.xts.Encrypt(sector, sector, number)
		} else {
			d.xts.Decrypt(sector, sector, number)
		}
	}
}
