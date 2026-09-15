package vhost

import (
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

// runWorker handles virtq IO for one queue. It blocks reading the kick
// eventfd, scans the avail ring on each kick, processes new descriptor
// chains in order, writes completions to the used ring, and signals the
// call eventfd.
//
// The goroutine is pinned to one OS thread so syscall blocking does not
// migrate (helpful for fault attribution under uffd later in P3).
func (s *Server) runWorker(idx int, q *virtq) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(q.done)

	// CH passes the kick eventfd as non-blocking via SCM_RIGHTS; switch
	// it to blocking so our Read parks until a real kick arrives instead
	// of returning EAGAIN immediately and busy-spinning. We poll q.stop
	// periodically by setting a recv timeout via O_NONBLOCK + sleep is
	// possible but blocking + a separate stop-fd is simpler. v1: blocking
	// read; on Stop() we close the fd which unblocks Read with EBADF.
	if err := syscall.SetNonblock(q.kickFd, false); err != nil {
		s.logf("vhost: queue %d kickfd SetNonblock(false): %v", idx, err)
	}

	kickBuf := make([]byte, 8)
	for {
		select {
		case <-q.stop:
			return
		default:
		}

		// Block reading the kick eventfd. The Read returns when the master
		// has called eventfd_write(kickFd, 1) — i.e. published new descriptors.
		_, err := syscall.Read(q.kickFd, kickBuf)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			// EBADF means our Stop() closed the fd to wake us. EAGAIN
			// shouldn't fire after SetNonblock(false), but be lenient
			// in case fd flags didn't take.
			if errors.Is(err, syscall.EBADF) {
				return
			}
			if errors.Is(err, syscall.EAGAIN) {
				continue
			}
			s.logf("vhost: queue %d kickfd read: %v (worker exiting)", idx, err)
			return
		}

		// Quiesce gate (docs/sandbox.md §11.5): pauseMu is held by Quiesce(), so workers
		// block here while a snapshot is in progress. Once Resume()
		// releases it, this iteration runs and processQueue will scan
		// the avail ring including any KICKs that piled up during pause.
		if err := s.pauseMu.lock(q.readContext()); err != nil {
			return
		}
		if q.stopped() != nil {
			s.pauseMu.Unlock()
			return
		}
		s.inflight.Add(1)
		s.pauseMu.Unlock()

		err = s.processQueue(q)
		// Publish the runtime cause before Quiesce can pass this request.
		// The owner callback must never synchronously join this worker.
		if readretry.IsTerminal(err) && q.stopped() == nil && s.onReadFatal != nil {
			s.onReadFatal(err)
		}
		s.inflight.Done()
		if err != nil {
			if readretry.IsTerminal(err) {
				return
			}
			if errors.Is(err, syscall.EBADF) {
				select {
				case <-q.stop:
					return
				default:
				}
			}
			s.logf("vhost: queue %d process: %v", idx, err)
		}
	}
}

// processQueue scans the avail ring from baseIdx forward and processes
// every new descriptor chain.
func (s *Server) processQueue(q *virtq) error {
	availRing, err := s.readAvailRing(q)
	if err != nil {
		return err
	}
	headIdx := availRing.idx

	for q.baseIdx != headIdx {
		if err := q.stopped(); err != nil {
			return err
		}
		descIdx := availRing.ring[q.baseIdx%uint16(q.num)]
		written, err := s.processChain(q, descIdx)
		if readretry.IsTerminal(err) {
			return err
		}
		if stopped := q.stopped(); stopped != nil {
			return stopped
		}
		if err != nil {
			s.logf("vhost: chain %d: %v", descIdx, err)
		}
		if err := s.publishUsed(q, descIdx, uint32(written)); err != nil {
			return err
		}
		q.baseIdx++
	}

	select {
	case <-q.stop:
		return nil
	default:
	}
	return s.notifyCall(q)
}

// availSnapshot is a snapshot of the avail ring.
type availSnapshot struct {
	flags uint16
	idx   uint16
	ring  []uint16
}

func (s *Server) readAvailRing(q *virtq) (availSnapshot, error) {
	// Ring base addresses come from SET_VRING_ADDR, which delivers them
	// as master-process user virtual addresses (per vhost-user spec) —
	// translate via UserspaceAddr, not GuestPhysAddr.
	hdr, err := s.memTable.TranslateUVA(q.availAddr, 4+uint64(q.num)*2)
	if err != nil {
		return availSnapshot{}, err
	}
	flags := binary.LittleEndian.Uint16(hdr[0:2])
	idx := binary.LittleEndian.Uint16(hdr[2:4])
	ring := make([]uint16, q.num)
	for i := uint32(0); i < q.num; i++ {
		ring[i] = binary.LittleEndian.Uint16(hdr[4+i*2 : 4+i*2+2])
	}
	return availSnapshot{flags: flags, idx: idx, ring: ring}, nil
}

// publishUsed appends one entry to the used ring and bumps used.idx.
func (s *Server) publishUsed(q *virtq, descIdx uint16, length uint32) error {
	usedHeader, err := s.memTable.TranslateUVA(q.usedAddr, 4)
	if err != nil {
		return err
	}
	usedIdx := binary.LittleEndian.Uint16(usedHeader[2:4])

	entryAddr := q.usedAddr + 4 + uint64(usedIdx%uint16(q.num))*8
	entry, err := s.memTable.TranslateUVA(entryAddr, 8)
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(entry[0:4], uint32(descIdx))
	binary.LittleEndian.PutUint32(entry[4:8], length)

	binary.LittleEndian.PutUint16(usedHeader[2:4], usedIdx+1)
	return nil
}

// notifyCall writes 1 to the call eventfd to interrupt the guest.
func (s *Server) notifyCall(q *virtq) error {
	val := uint64(1)
	_, err := syscall.Write(q.callFd, (*[8]byte)(unsafe.Pointer(&val))[:])
	return err
}

// vringDesc is a 16-byte virtio descriptor.
type vringDesc struct {
	Addr  uint64
	Len   uint32
	Flags uint16
	Next  uint16
}

const (
	descFlagNext     uint16 = 1
	descFlagWrite    uint16 = 2
	descFlagIndirect uint16 = 4
)

// readDesc reads descriptor at the given index from the desc table.
// q.descAddr is master UVA (from SET_VRING_ADDR); the descriptor's own
// .Addr field is GPA per virtio spec (translated separately in appendSeg).
func (s *Server) readDesc(q *virtq, idx uint16) (vringDesc, error) {
	addr := q.descAddr + uint64(idx)*16
	b, err := s.memTable.TranslateUVA(addr, 16)
	if err != nil {
		return vringDesc{}, err
	}
	return vringDesc{
		Addr:  binary.LittleEndian.Uint64(b[0:8]),
		Len:   binary.LittleEndian.Uint32(b[8:12]),
		Flags: binary.LittleEndian.Uint16(b[12:14]),
		Next:  binary.LittleEndian.Uint16(b[14:16]),
	}, nil
}

// chain holds the descriptors of a single virtio request chain, classified
// by direction.
type chain struct {
	readSegs  [][]byte // device-readable (guest → device)
	writeSegs [][]byte // device-writable (device → guest)
}

// walkChain follows NEXT pointers from headIdx and resolves each descriptor
// to a host slice via memTable. Indirect descriptors are flattened.
func (s *Server) walkChain(q *virtq, headIdx uint16) (*chain, error) {
	c := &chain{}
	idx := headIdx
	for {
		d, err := s.readDesc(q, idx)
		if err != nil {
			return nil, err
		}
		if d.Flags&descFlagIndirect != 0 {
			// Indirect: d.Addr points to a small array of vringDesc.
			n := int(d.Len) / 16
			arr, err := s.memTable.TranslateGPA(d.Addr, uint64(n*16))
			if err != nil {
				return nil, err
			}
			for i := 0; i < n; i++ {
				inner := vringDesc{
					Addr:  binary.LittleEndian.Uint64(arr[i*16 : i*16+8]),
					Len:   binary.LittleEndian.Uint32(arr[i*16+8 : i*16+12]),
					Flags: binary.LittleEndian.Uint16(arr[i*16+12 : i*16+14]),
				}
				if err := s.appendSeg(c, inner); err != nil {
					return nil, err
				}
			}
		} else {
			if err := s.appendSeg(c, d); err != nil {
				return nil, err
			}
		}
		if d.Flags&descFlagNext == 0 {
			break
		}
		idx = d.Next
	}
	return c, nil
}

func (s *Server) appendSeg(c *chain, d vringDesc) error {
	if d.Len == 0 {
		return nil
	}
	hva, err := s.memTable.TranslateGPA(d.Addr, uint64(d.Len))
	if err != nil {
		return err
	}
	if d.Flags&descFlagWrite != 0 {
		c.writeSegs = append(c.writeSegs, hva)
	} else {
		c.readSegs = append(c.readSegs, hva)
	}
	return nil
}

// processChain resolves the chain, parses the virtio-blk header, performs
// the IO, and writes the status byte. Returns the number of bytes written
// to device-writable buffers (i.e. to put in the used ring entry).
func (s *Server) processChain(q *virtq, headIdx uint16) (int, error) {
	if err := q.stopped(); err != nil {
		return 0, err
	}
	c, err := s.walkChain(q, headIdx)
	if err != nil {
		return 0, err
	}
	if len(c.readSegs) == 0 || len(c.writeSegs) == 0 {
		return 0, errors.New("vhost: chain missing header or status segment")
	}
	hdr, err := ParseBlkReqHeader(c.readSegs[0])
	if err != nil {
		return 0, err
	}

	var startNs int64
	if s.stats != nil {
		startNs = nowNs()
	}

	// Status byte is the last byte of the last writable segment.
	statusSeg := c.writeSegs[len(c.writeSegs)-1]
	status := &statusSeg[len(statusSeg)-1]
	statusValue := byte(BlkStatusOK)

	bytesIO := 0
	switch hdr.Type {
	case BlkTypeIn:
		// device-writable segments (excluding status byte) are the data buffer.
		offset := int64(hdr.Sector) * SectorSize
		for i, seg := range c.writeSegs {
			if i == len(c.writeSegs)-1 {
				// last segment may contain status byte at end; if it's only
				// 1 byte, skip data here. Otherwise, use all but last byte.
				if len(seg) > 1 {
					n, err := s.readAt(q.readContext(), seg[:len(seg)-1], offset)
					if readretry.IsTerminal(err) {
						return 0, err
					}
					if stopped := q.stopped(); stopped != nil {
						return 0, stopped
					}
					bytesIO += n
					offset += int64(n)
					if err != nil && !errors.Is(err, syscall.EAGAIN) && !(errors.Is(err, io.EOF) && n == len(seg)-1) {
						statusValue = BlkStatusIOErr
						break
					}
				}
				continue
			}
			n, err := s.readAt(q.readContext(), seg, offset)
			if readretry.IsTerminal(err) {
				return 0, err
			}
			if stopped := q.stopped(); stopped != nil {
				return 0, stopped
			}
			bytesIO += n
			offset += int64(n)
			if err != nil && !errors.Is(err, syscall.EAGAIN) && !(errors.Is(err, io.EOF) && n == len(seg)) {
				statusValue = BlkStatusIOErr
				break
			}
		}

	case BlkTypeOut:
		offset := int64(hdr.Sector) * SectorSize
		// readable segments after the header are the data buffer.
		for i, seg := range c.readSegs {
			if i == 0 {
				continue // header
			}
			n, err := s.writeAt(q.readContext(), seg, offset)
			if readretry.IsTerminal(err) {
				return 0, err
			}
			if stopped := q.stopped(); stopped != nil {
				return 0, stopped
			}
			bytesIO += n
			offset += int64(n)
			if err != nil {
				if errors.Is(err, ErrReadOnly) {
					statusValue = BlkStatusUnsupp
				} else {
					statusValue = BlkStatusIOErr
				}
				break
			}
		}

	case BlkTypeFlush:
		if err := s.backend.Flush(); err != nil {
			statusValue = BlkStatusIOErr
		}

	default:
		// Optional commands, including DISCARD and WRITE_ZEROES, are not
		// advertised by our minimal profile. Never report false success.
		statusValue = BlkStatusUnsupp
	}

	if err := q.stopped(); err != nil {
		return 0, err
	}
	*status = statusValue

	// Number of bytes written to device-writable buffers = data bytes for
	// IN + 1 status byte. Other commands write only the status byte.
	ret := 1
	if hdr.Type == BlkTypeIn {
		ret = bytesIO + 1
	}

	if s.stats != nil {
		latNs := uint64(nowNs() - startNs)
		s.stats.Record(hdr.Type, uint64(bytesIO), latNs, *status == BlkStatusOK)
		if bytesIO > 0 {
			off := int64(hdr.Sector) * SectorSize
			switch hdr.Type {
			case BlkTypeIn:
				s.stats.MarkRead(off, bytesIO)
			case BlkTypeOut:
				s.stats.MarkWrite(off, bytesIO)
			}
		}
	}

	return ret, nil
}
