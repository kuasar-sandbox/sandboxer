package vhost

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return 0, 0, fmt.Errorf("statfs: %w", err)
	}
	// These are the verified local sparse/DIO filesystems. In particular tmpfs
	// accepts O_DIRECT on some kernels while continuing to use its page cache.
	if fs.Type != unix.EXT4_SUPER_MAGIC && fs.Type != unix.XFS_SUPER_MAGIC {
		return 0, 0, fmt.Errorf("unsupported active diff filesystem 0x%x (requires disk-backed ext4/XFS)", fs.Type)
	}
	if fs.Bsize <= 0 || fs.Bsize > cowBlockSize || cowBlockSize%fs.Bsize != 0 {
		return 0, 0, fmt.Errorf("unsupported active diff filesystem block size %d", fs.Bsize)
	}
	var st unix.Statx_t
	err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_DIOALIGN, &st)
	memory, offset := 4096, 4096
	if err == nil && st.Mask&unix.STATX_DIOALIGN != 0 {
		// Zero is an explicit unsupported result, not permission to assume 4 KiB.
		memory, offset = int(st.Dio_mem_align), int(st.Dio_offset_align)
	} else {
		if err != nil && !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EOPNOTSUPP) {
			return 0, 0, fmt.Errorf("statx DIOALIGN: %w", err)
		}
		// Older ext4 can silently buffer O_DIRECT for data=journal, fscrypt,
		// verity or inline data. Verify both inode and mount before using its
		// conservative 4 KiB path. Unknown XFS needs STATX_DIOALIGN: realtime
		// allocation units can exceed the format's page size.
		if fs.Type != unix.EXT4_SUPER_MAGIC {
			return 0, 0, fmt.Errorf("STATX_DIOALIGN unavailable; no verified legacy DIO path for filesystem 0x%x", fs.Type)
		}
		flags, flagErr := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
		if flagErr != nil {
			return 0, 0, fmt.Errorf("legacy ext4 DIO inode flags: %w", flagErr)
		}
		mode, modeErr := ext4DataMode(fd)
		if modeErr != nil {
			return 0, 0, modeErr
		}
		if err := validateLegacyExt4(flags, mode); err != nil {
			return 0, 0, err
		}
	}
	if err := validateDirectAlignment(memory, offset, bodyOffset, size); err != nil {
		return 0, 0, err
	}
	return memory, offset, nil
}

// Values are Linux UAPI FS_*_FL, not on-disk encrypted-diff policy flags.
func validateLegacyExt4(flags int, mode string) error {
	const unsupported = 0x4 | 0x800 | 0x4000 | 0x100000 | 0x10000000 // compression, fscrypt, journal-data, verity, inline
	if flags&unsupported != 0 || (mode != "ordered" && mode != "writeback") {
		return fmt.Errorf("STATX_DIOALIGN unavailable; unsupported legacy ext4 DIO flags=0x%x data=%s", flags, mode)
	}
	return nil
}
func ext4DataMode(fd int) (string, error) {
	// Bind mount policy to the opened descriptor's mount ID, not a path which
	// could have been renamed or overmounted. Bound reads of proc metadata.
	info, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return "", fmt.Errorf("legacy ext4 DIO fdinfo: %w", err)
	}
	id := ""
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "mnt_id:" {
			id = fields[1]
		}
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", fmt.Errorf("legacy ext4 DIO missing mount ID")
	}
	mounts, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer mounts.Close()
	scan := bufio.NewScanner(io.LimitReader(mounts, 4<<20))
	scan.Buffer(make([]byte, 4096), 64<<10)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 10 || fields[0] != id {
			continue
		}
		for i, field := range fields {
			if field == "-" && i+3 < len(fields) && fields[i+1] == "ext4" {
				for _, option := range strings.Split(fields[i+3], ",") {
					if strings.HasPrefix(option, "data=") {
						return strings.TrimPrefix(option, "data="), nil
					}
				}
			}
		}
	}
	return "", errors.Join(fmt.Errorf("legacy ext4 DIO could not verify mount data policy"), scan.Err())
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
	flags, err := unix.FcntlInt(d.f.Fd(), unix.F_GETFL, 0)
	if err == nil {
		_, err = unix.FcntlInt(d.f.Fd(), unix.F_SETFL, flags|unix.O_DIRECT)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("vhost: enable O_DIRECT %s: %w", d.f.Name(), err), r.close(), w.close())
	}
	d.direct = &directWorkspace{read: r, write: w, memoryAlign: memory, offsetAlign: offset}
	d.bodyIO = directBody{d.f}
	return nil
}

func (d *diffFile) directReadAt(buf []byte, offset int64) (int, error) {
	d.direct.readMu.Lock()
	defer d.direct.readMu.Unlock()
	if d.direct.read.bytes == nil {
		return 0, os.ErrClosed
	}
	used := 0
	defer func() { clear(d.direct.read.bytes[:used]) }()
	done := 0
	for done < len(buf) {
		pos := offset + int64(done)
		start := pos / cowBlockSize * cowBlockSize
		length := min(len(buf)-done, maxDiffScratchSize-int(pos-start))
		end := alignUp(pos+int64(length), cowBlockSize)
		scratch := d.direct.read.bytes[:int(end-start)]
		used = max(used, len(scratch))
		if err := readFullAt(d.bodyIO, scratch, d.bodyOffset+start); err != nil {
			return done, err
		}
		d.cryptUnits(scratch, start, false)
		copy(buf[done:done+length], scratch[pos-start:pos-start+int64(length)])
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
