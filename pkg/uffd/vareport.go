package uffd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// VAReportServer accepts CH-side va_report messages plus the uffd fd
// attached via SCM_RIGHTS. CH calls into this socket once per memory
// region; on x86_64 a zone larger than 3 GiB is split across the PCI
// hole into two regions, producing two reports. The first report
// triggers OnReady (which constructs the uffd handler); subsequent
// reports trigger OnRegister (which adds the new uffd to the existing
// handler). Each registration appends a vma to the AddressMap with
// the appropriate memfdOffset (validated to start at 0 and accumulate
// in arrival order — CH iterates regions by GPA, which equals memfd
// offset order on x86 because the inode is contiguous and the GPA
// PCI hole only affects how regions are mapped into guest, not the
// underlying memfd layout).
type VAReportServer struct {
	Path    string
	AddrMap *AddressMap
	Logf    func(string, ...any)

	// HandshakeDeadline bounds the read of one va_report message (CH→host
	// uffd-fd handoff). 0 = no deadline: the host waits as long as CH needs
	// to issue the handoff. Set positive to fail fast.
	HandshakeDeadline time.Duration

	// OnReady is invoked synchronously on the FIRST va_report. The
	// handler should adopt the uffd fd (the server passes ownership)
	// and start its goroutines. Returning an error causes the
	// va_report to be NAK'd and the fd closed.
	OnReady func(uffdFD int, vaStart, size, memfdOffset uint64) error

	// OnRegister is invoked synchronously on each SUBSEQUENT va_report.
	// The handler should adopt the new uffd fd (also passed by
	// ownership) and add it to its epoll set. Returning an error NAKs.
	// May be left nil; in that case any second va_report is rejected
	// (single-region mode).
	OnRegister func(uffdFD int, vaStart, size, memfdOffset uint64) error

	// OnAdopted is an optional, read-only diagnostic after successful adoption
	// and before ACK. It runs without server locks and cannot reject adoption.
	// A nil callback performs no peer-credential lookup.
	OnAdopted func(AdoptionDiagnostic)

	connMu            sync.Mutex
	activeConn        *net.UnixConn
	listener          *net.UnixListener
	mu                sync.Mutex
	regionsRegistered int    // count of va_report messages handled successfully
	nextMemfdOffset   uint64 // accumulator: each new region's memfd offset
	stopOnce          sync.Once
	stopped           chan struct{}
}

// AdoptionDiagnostic identifies the accepted CH registration. PeerErr is
// diagnostic only; the received uffd has already been adopted successfully.
type AdoptionDiagnostic struct {
	UffdFD                     int
	VAStart, Size, MemfdOffset uint64
	PeerPID                    int
	PeerErr                    error
}

// Listen binds the UDS socket. Must be called before CH spawns; CH's
// patched create_ram_region will Dial this socket synchronously.
func (s *VAReportServer) Listen() error {
	if s.AddrMap == nil {
		return errors.New("vareport: AddrMap is nil")
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	s.stopped = make(chan struct{})
	_ = os.Remove(s.Path)
	addr, err := net.ResolveUnixAddr("unix", s.Path)
	if err != nil {
		return fmt.Errorf("vareport: resolve %s: %w", s.Path, err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("vareport: listen %s: %w", s.Path, err)
	}
	s.listener = l
	return nil
}

// Serve runs the accept loop until ctx is cancelled or Stop is called.
// Returns nil on clean shutdown.
func (s *VAReportServer) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("vareport: Listen not called")
	}
	go func() {
		select {
		case <-ctx.Done():
			s.Stop()
		case <-s.stopped:
		}
	}()
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.stopped:
				return nil
			default:
				return fmt.Errorf("vareport: accept: %w", err)
			}
		}
		s.connMu.Lock()
		select {
		case <-s.stopped:
			s.connMu.Unlock()
			_ = conn.Close()
			return nil
		default:
		}
		s.activeConn = conn
		s.connMu.Unlock()
		s.handle(conn)
		s.connMu.Lock()
		s.activeConn = nil
		s.connMu.Unlock()
	}
}

// Stop closes the listener and unlinks the socket.
func (s *VAReportServer) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		s.connMu.Lock()
		if s.activeConn != nil {
			_ = s.activeConn.Close()
		}
		s.connMu.Unlock()
		if s.listener != nil {
			_ = s.listener.Close()
		}
		_ = os.Remove(s.Path)
	})
}

type vaReportMsg struct {
	Type       string  `json:"type"`
	ZoneID     string  `json:"zone_id,omitempty"`
	VAStart    uint64  `json:"va_start"`
	Size       uint64  `json:"size"`
	FileOffset *uint64 `json:"file_offset,omitempty"`
}

type ackMsg struct {
	Type string `json:"type"`
	Msg  string `json:"msg,omitempty"`
}

func (s *VAReportServer) handle(conn *net.UnixConn) {
	defer conn.Close()
	if s.HandshakeDeadline > 0 {
		_ = conn.SetDeadline(time.Now().Add(s.HandshakeDeadline))
	}

	// Read u32 length prefix + JSON body, plus SCM_RIGHTS ancillary
	// containing the uffd fd in the same recvmsg call.
	body, fds, err := readLPJSONWithFDs(conn)
	if err != nil {
		s.Logf("vareport: read: %v", err)
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: err.Error()})
		closeAll(fds)
		return
	}
	defer closeAll(fds)
	var req vaReportMsg
	if err := json.Unmarshal(body, &req); err != nil {
		s.Logf("vareport: unmarshal: %v", err)
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: err.Error()})
		return
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	if _, present := fields["file_offset"]; present && req.FileOffset == nil {
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: "null file_offset"})
		return
	}
	if req.Type != "va_report" {
		s.Logf("vareport: bad type %q", req.Type)
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: "expected va_report"})
		return
	}
	if len(fds) != 1 {
		s.Logf("vareport: expected 1 fd via SCM_RIGHTS, got %d", len(fds))
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: "expected exactly one fd via SCM_RIGHTS"})
		return
	}

	s.mu.Lock()
	regionIdx := s.regionsRegistered
	memfdOffset := s.nextMemfdOffset
	if req.FileOffset != nil {
		if *req.FileOffset != memfdOffset {
			s.mu.Unlock()
			_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: "noncontiguous file_offset"})
			return
		}
		memfdOffset = *req.FileOffset
	}
	if regionIdx > 0 && s.OnRegister == nil {
		s.mu.Unlock()
		s.Logf("vareport: extra report (zone=%s, region #%d) but OnRegister not set — single-region mode",
			req.ZoneID, regionIdx)
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: "multi-region not supported by handler"})
		return
	}
	uffdFD := fds[0]
	var adoptErr error
	if regionIdx == 0 {
		if s.OnReady == nil {
			adoptErr = errors.New("vareport: missing OnReady callback")
		} else {
			adoptErr = s.AddrMap.RegisterVMA(ProcessCH, req.VAStart, req.Size, memfdOffset)
			if adoptErr == nil {
				adoptErr = s.OnReady(uffdFD, req.VAStart, req.Size, memfdOffset)
				if adoptErr != nil {
					s.AddrMap.removeStaged(req.VAStart, req.Size, memfdOffset)
				}
			}
		}
	} else {
		// Handler stages the interval and atomically commits fd adoption with
		// installation visibility. A failed epoll add leaves neither published.
		adoptErr = s.OnRegister(uffdFD, req.VAStart, req.Size, memfdOffset)
	}
	if adoptErr != nil {
		s.mu.Unlock()
		s.Logf("vareport: adoption failed: %v", adoptErr)
		_ = writeLPJSON(conn, ackMsg{Type: "error", Msg: adoptErr.Error()})
		return
	}
	// Callback took ownership of uffdFD; remove from fds slice so
	// the deferred closeAll doesn't double-close.
	fds[0] = -1
	s.regionsRegistered++
	s.nextMemfdOffset += req.Size
	s.mu.Unlock()
	if s.OnAdopted != nil {
		pid, peerErr := vaReportPeerPID(conn)
		s.OnAdopted(AdoptionDiagnostic{UffdFD: uffdFD, VAStart: req.VAStart, Size: req.Size, MemfdOffset: memfdOffset, PeerPID: pid, PeerErr: peerErr})
	}

	s.Logf("vareport: accepted region #%d zone=%s va=0x%x size=%d memfd_off=0x%x uffd_fd=%d",
		regionIdx, req.ZoneID, req.VAStart, req.Size, memfdOffset, uffdFD)
	if err := writeLPJSON(conn, ackMsg{Type: "ack"}); err != nil {
		s.Logf("vareport: write ack: %v", err)
	}
}

func vaReportPeerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		cred, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if socketErr != nil {
		return 0, socketErr
	}
	return int(cred.Pid), nil
}

// readLPJSONWithFDs reads u32 LE length + JSON body, possibly with
// SCM_RIGHTS-attached fds on the same recvmsg.
func readLPJSONWithFDs(c *net.UnixConn) ([]byte, []int, error) {
	var fds []int
	oob := make([]byte, syscall.CmsgSpace(4*16))
	readFull := func(dst []byte) error {
		for len(dst) != 0 {
			n, oobN, flags, _, err := c.ReadMsgUnix(dst, oob)
			// Preserve ownership even on a short/truncated frame, so the caller
			// closes every received descriptor on rejection.
			fds = append(fds, parseFDsFromOOB(oob[:oobN])...)
			if flags&(syscall.MSG_CTRUNC|syscall.MSG_TRUNC) != 0 || len(fds) > 16 {
				return fmt.Errorf("truncated/oversized ancillary data")
			}
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrUnexpectedEOF
			}
			dst = dst[n:]
		}
		return nil
	}
	var lenBuf [4]byte
	if err := readFull(lenBuf[:]); err != nil {
		return nil, fds, fmt.Errorf("read length+oob: %w", err)
	}
	bodyLen := binary.LittleEndian.Uint32(lenBuf[:])
	if bodyLen == 0 || bodyLen > 64*1024 {
		return nil, fds, fmt.Errorf("invalid message length: %d", bodyLen)
	}
	body := make([]byte, bodyLen)
	if err := readFull(body); err != nil {
		return nil, fds, fmt.Errorf("read body+oob: %w", err)
	}
	return body, fds, nil
}

func parseFDsFromOOB(oob []byte) []int {
	if len(oob) == 0 {
		return nil
	}
	cmsgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var out []int
	for _, c := range cmsgs {
		if c.Header.Level == syscall.SOL_SOCKET && c.Header.Type == syscall.SCM_RIGHTS {
			fds, err := syscall.ParseUnixRights(&c)
			if err == nil {
				out = append(out, fds...)
			}
		}
	}
	// Set CLOEXEC on each so a stray exec doesn't leak the uffd.
	for _, fd := range out {
		_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	}
	return out
}

func closeAll(fds []int) {
	for _, fd := range fds {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}
}

func writeLPJSON(w net.Conn, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
