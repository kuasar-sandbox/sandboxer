package uffd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// PageTraceAdoptionObserver returns nil unless the one-page diagnostic is
// enabled. Metadata I/O runs before CH's ACK, outside all handler/server locks.
// It never reads the page or changes the result of the successful adoption.
func (h *Handler) PageTraceAdoptionObserver() func(AdoptionDiagnostic) {
	if h.pageTrace == nil {
		return nil
	}
	return func(d AdoptionDiagnostic) {
		if !h.pageTrace.covers(d.MemfdOffset, d.Size) {
			return
		}
		h.withPageTraceUser(func() {
			h.traceRegistration(d)
			h.traceBacking("before-ack")
		})
	}
}

func (h *Handler) withPageTraceUser(observe func()) {
	if h.pageTrace == nil {
		return
	}
	h.uffdsMu.Lock()
	if h.closing.Load() {
		h.uffdsMu.Unlock()
		return
	}
	h.users.Add(1)
	h.uffdsMu.Unlock()
	defer h.users.Done()
	observe()
}

func pageTraceErrno(err error) int {
	if err == nil {
		return 0
	}
	var errno unix.Errno
	if errors.As(err, &errno) {
		return int(errno)
	}
	return -1
}

func (h *Handler) traceRegistration(d AdoptionDiagnostic) {
	var stat unix.Stat_t
	statErr := unix.Fstat(h.cfg.MemfdFD, &stat)
	var uname unix.Utsname
	unameErr := unix.Uname(&uname)
	release := ""
	if unameErr == nil {
		for _, b := range uname.Release {
			if b == 0 || len(release) == 64 {
				break
			}
			release += string(byte(b))
		}
	}
	var mapping pageTraceMapping
	mapsErr := d.PeerErr
	if mapsErr == nil && d.PeerPID <= 0 {
		mapsErr = unix.ESRCH
	}
	if mapsErr == nil {
		mapsErr = statErr
	}
	if mapsErr == nil {
		var file *os.File
		file, mapsErr = os.Open(fmt.Sprintf("/proc/%d/maps", d.PeerPID))
		if mapsErr == nil {
			mapping, mapsErr = readPageTraceMapping(file, d.VAStart+(h.pageTrace.page-d.MemfdOffset))
			_ = file.Close()
		}
	}
	backendVA := uint64(h.cfg.BackendVA)
	dev, ino, size := uint64(stat.Dev), stat.Ino, uint64(0)
	if statErr == nil && stat.Size >= 0 {
		size = uint64(stat.Size)
	}
	peerErrno, statErrno := pageTraceErrno(d.PeerErr), pageTraceErrno(statErr)
	kernelErrno, mapsErrno := pageTraceErrno(unameErr), pageTraceErrno(mapsErr)
	h.pageTrace.record("register", d.MemfdOffset, d.Size, h.state.Get(h.pageTrace.page/PageSize), func(r *pageTraceRecord) {
		r.UffdFD, r.BackendVA, r.MemfdFD = &d.UffdFD, &backendVA, &h.cfg.MemfdFD
		r.MemfdDev, r.MemfdIno, r.MemfdSize = &dev, &ino, &size
		r.KernelRelease, r.CHVA, r.CHPID = &release, &d.VAStart, &d.PeerPID
		r.PeerErrno, r.MemfdErrno, r.KernelErrno, r.MapsErrno = &peerErrno, &statErrno, &kernelErrno, &mapsErrno
		if mapsErr == nil {
			targetVA := d.VAStart + (h.pageTrace.page - d.MemfdOffset)
			within := targetVA - mapping.va
			match := mapping.dev == dev && mapping.ino == ino && within <= ^uint64(0)-mapping.offset && mapping.offset+within == h.pageTrace.page
			r.MapsVA, r.MapsSize, r.MapsOffset = &mapping.va, &mapping.size, &mapping.offset
			r.MapsDev, r.MapsIno, r.MapsMatch = &mapping.dev, &mapping.ino, &match
		}
	})
}

type pageTraceMapping struct {
	va, size, offset, dev, ino uint64
}

// Read at most 1 MiB of maps and retain no paths. A split VMA is acceptable if
// the complete target page is contained and its file offset/inode match.
func readPageTraceMapping(reader io.Reader, target uint64) (pageTraceMapping, error) {
	const limit = 1 << 20
	bounded := &io.LimitedReader{R: reader, N: limit + 1}
	scanner := bufio.NewScanner(bounded)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			return pageTraceMapping{}, unix.EINVAL
		}
		address := strings.Split(fields[0], "-")
		device := strings.Split(fields[3], ":")
		if len(address) != 2 || len(device) != 2 {
			return pageTraceMapping{}, unix.EINVAL
		}
		start, e1 := strconv.ParseUint(address[0], 16, 64)
		end, e2 := strconv.ParseUint(address[1], 16, 64)
		if e1 != nil || e2 != nil || start >= end {
			return pageTraceMapping{}, unix.EINVAL
		}
		if target < start || target >= end || PageSize > end-target {
			continue
		}
		offset, e3 := strconv.ParseUint(fields[2], 16, 64)
		major, e4 := strconv.ParseUint(device[0], 16, 32)
		minor, e5 := strconv.ParseUint(device[1], 16, 32)
		ino, e6 := strconv.ParseUint(fields[4], 10, 64)
		if e3 != nil || e4 != nil || e5 != nil || e6 != nil || bounded.N <= 0 {
			return pageTraceMapping{}, unix.EINVAL
		}
		return pageTraceMapping{va: start, size: end - start, offset: offset, dev: unix.Mkdev(uint32(major), uint32(minor)), ino: ino}, nil
	}
	if err := scanner.Err(); err != nil {
		return pageTraceMapping{}, err
	}
	if bounded.N <= 0 {
		return pageTraceMapping{}, unix.E2BIG
	}
	return pageTraceMapping{}, unix.ENOENT
}

// traceBacking examines only file/VMA metadata. SEEK_DATA includes allocated
// swap entries; mincore reports residency, not bytes or the validity of a page.
// This is intentionally not atomic with kernel REMOVE/UNMAP or guest activity.
func (h *Handler) traceBacking(phase string) {
	page := h.pageTrace.page
	data, hole, dataErr, holeErr := pageTraceExtents(h.cfg.MemfdFD, page)
	resident, mincoreErr := pageTraceResident(h.cfg.BackendVA, page)
	dataErrno, holeErrno, mincoreErrno := pageTraceErrno(dataErr), pageTraceErrno(holeErr), pageTraceErrno(mincoreErr)
	h.pageTrace.record("observe", page, PageSize, h.state.Get(page/PageSize), func(r *pageTraceRecord) {
		r.Phase = phase
		r.SeekDataOffset, r.SeekHoleOffset = &data, &hole
		r.SeekDataErrno, r.SeekHoleErrno, r.MincoreErrno = &dataErrno, &holeErrno, &mincoreErrno
		if mincoreErr == nil {
			r.Resident = &resident
		}
		if holeErr == nil && (dataErr == nil || errors.Is(dataErr, unix.ENXIO)) {
			extent := dataErr == nil && data == int64(page) && hole >= int64(page+PageSize)
			r.ExtentData = &extent
		}
	})
}

func pageTraceExtents(fd int, page uint64) (data, hole int64, dataErr, holeErr error) {
	// Reopen, rather than dup: this must have an independent file description
	// so diagnostic lseek never changes seed/capture's shared file position.
	probe, err := unix.Open(fmt.Sprintf("/proc/self/fd/%d", fd), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, -1, err, err
	}
	defer unix.Close(probe)
	data, dataErr = unix.Seek(probe, int64(page), unix.SEEK_DATA)
	hole, holeErr = unix.Seek(probe, int64(page), unix.SEEK_HOLE)
	if dataErr != nil {
		data = -1
	}
	if holeErr != nil {
		hole = -1
	}
	return
}

func pageTraceResident(base uintptr, offset uint64) (bool, error) {
	if base == 0 || offset > uint64(^uintptr(0))-uint64(base) {
		return false, unix.EINVAL
	}
	// mincore needs native-page alignment (64 KiB on some ARM hosts). A
	// one-byte length asks about exactly one native page and never touches it.
	addr := (base + uintptr(offset)) &^ uintptr(os.Getpagesize()-1)
	var vector byte
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, addr, 1, uintptr(unsafe.Pointer(&vector)))
	if errno != 0 {
		return false, errno
	}
	return vector&1 != 0, nil
}
