package diffstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/directio"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/sectorcrypt"
	"golang.org/x/sys/unix"
)

type alignedMapping = directio.Buffer

func newAlignedMapping(size, alignment int) (*alignedMapping, error) {
	return directio.New(size, alignment, maxDiffScratchSize)
}

// directWorkspace stages the common O_DIRECT API requests. The kernel decides
// how those requests are fulfilled; this is not proof of physical cache bypass.
type directWorkspace struct {
	readMu, writeMu          sync.Mutex
	read, write              *alignedMapping
	memoryAlign, offsetAlign int
}

// directBody returns short transfers to its caller. os.File.WriteAt retries a
// short write using the remaining slice, which need not satisfy DIO alignment.
type directBody struct{ f *os.File }

func (b directBody) ReadAt(p []byte, off int64) (int, error) {
	n, err := directio.RetryInterruptedIO(unix.Pread, int(b.f.Fd()), p, off)
	if n < 0 {
		n = 0
	}
	if err == nil && n != len(p) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
func (b directBody) WriteAt(p []byte, off int64) (int, error) {
	n, err := directio.RetryInterruptedIO(unix.Pwrite, int(b.f.Fd()), p, off)
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
		return fmt.Errorf("diffstore: direct I/O %s: %w", d.f.Name(), err)
	}
	r, err := newAlignedMapping(maxDiffScratchSize, memory)
	if err != nil {
		return err
	}
	w, err := newAlignedMapping(maxDiffScratchSize, memory)
	if err != nil {
		return errors.Join(err, r.Close())
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
		return errors.Join(fmt.Errorf("diffstore: enable O_DIRECT %s: %w", d.f.Name(), err), r.Close(), w.Close())
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
	if d.direct.read.Bytes == nil {
		return os.ErrClosed
	}
	start := offset / cowBlockSize * cowBlockSize
	end := alignUp(offset+int64(length), cowBlockSize)
	if length <= 0 || end-start > maxDiffScratchSize {
		return fmt.Errorf("diffstore: invalid bounded direct read length %d", length)
	}
	scratch := d.direct.read.Bytes[:int(end-start)]
	defer clear(scratch)
	if err := readFullAt(d.bodyIO, scratch, d.bodyOffset+start); err != nil {
		return err
	}
	d.decodePayload(scratch, start)
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
	if d.direct.write.Bytes == nil {
		return 0, os.ErrClosed
	}
	used := 0
	defer func() { clear(d.direct.write.Bytes[:used]) }()
	done := 0
	for done < len(buf) {
		pos := offset + int64(done)
		start := pos / cowBlockSize * cowBlockSize
		length := min(len(buf)-done, maxDiffScratchSize-int(pos-start))
		end := alignUp(pos+int64(length), cowBlockSize)
		scratch := d.direct.write.Bytes[:int(end-start)]
		used = max(used, len(scratch))
		if pos != start || pos+int64(length) != end {
			if err := readFullAt(d.bodyIO, scratch, d.bodyOffset+start); err != nil {
				return done, err
			}
			d.decodePayload(scratch, start)
		}
		pages := make([]pageWrite, (end-start)/cowBlockSize)
		for i := range pages {
			pages[i].Block = uint64(start/cowBlockSize) + uint64(i)
		}
		d.prepareWrites(pages)
		copy(scratch[pos-start:pos-start+int64(length)], buf[done:done+length])
		d.cryptUnits(scratch, start, true)
		if err := writeFullAt(d.bodyIO, scratch, d.bodyOffset+start); err != nil {
			return done, errors.Join(err, d.cleanupWritesLocked(pages))
		}
		if err := d.finishWrites(pages); err != nil {
			return done, errors.Join(err, d.cleanupWritesLocked(pages))
		}
		done += length
	}
	return done, nil
}

// writePages borrows plaintext pages until synchronous I/O completes. Split
// disjoint template batches into contiguous writes without copying page payloads
// to the Go heap. Cache pages are never encrypted in place.
func (d *diffFile) writePages(ctx context.Context, pages []pageWrite) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(pages) == 0 || len(pages) > maxWritebackPages {
		return fmt.Errorf("diffstore: invalid single-layer batch")
	}
	for _, p := range pages {
		if p.Zero || len(p.Data) != cowBlockSize || p.Block >= uint64(d.logicalSize/cowBlockSize) {
			return fmt.Errorf("diffstore: invalid single-layer page")
		}
	}
	d.direct.writeMu.Lock()
	defer d.direct.writeMu.Unlock()
	if d.direct.write.Bytes == nil {
		return os.ErrClosed
	}
	d.prepareWrites(pages)
	for first := 0; first < len(pages); {
		end := first + 1
		for end < len(pages) && pages[end].Block == pages[end-1].Block+1 {
			end++
		}
		scratch := d.direct.write.Bytes[:(end-first)*cowBlockSize]
		for i, p := range pages[first:end] {
			copy(scratch[i*cowBlockSize:], p.Data)
		}
		offset := int64(pages[first].Block) * cowBlockSize
		d.cryptUnits(scratch, offset, true)
		err := writeFullAt(d.bodyIO, scratch, d.bodyOffset+offset)
		clear(scratch)
		if err != nil {
			return err
		}
		first = end
	}
	return d.finishWrites(pages)
}

func (d *diffFile) cryptUnits(buf []byte, start int64, encrypt bool) {
	sectorcrypt.Transform(d.xts, buf, uint64(start/diffDataUnitSize), encrypt)
}
