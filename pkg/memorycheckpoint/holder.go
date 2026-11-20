package memorycheckpoint

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Holder owns the frozen read-only FD, pins, service guard, writer and local job.
// It must outlive Runtime.Wait and cannot borrow that Runtime's storage graph.
type Holder struct {
	mu         sync.Mutex
	record     *RecordWriter
	file       *os.File
	ramPin     *os.File
	gcPin      *os.File
	directory  *holderDirectory
	size       uint64
	listener   *net.UnixListener
	readers    map[string]*readerLease
	ready      chan struct{}
	done       chan struct{}
	changed    chan struct{}
	started    bool
	closed     bool
	err        error
	logf       func(string, ...any)
	finishOnce sync.Once
	abortOnce  sync.Once
	abortErr   error
}
type readerLease struct {
	pid   int
	start uint64
	token string
	conn  *net.UnixConn
}
type holderMessage struct {
	Type         string `json:"type"`
	MemoryID     string `json:"memory_id,omitempty"`
	Epoch        uint64 `json:"epoch,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	Token        string `json:"token,omitempty"`
	Size         uint64 `json:"size,omitempty"`
	PayloadRef   string `json:"payload_ref,omitempty"`
	Error        string `json:"error,omitempty"`
}

func ProcessStartTime(pid int) (uint64, error) {
	start, _, err := processIdentityState(pid)
	return start, err
}

func processIdentityState(pid int) (uint64, byte, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, 0, errors.New("invalid process stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 || len(f[0]) != 1 {
		return 0, 0, errors.New("short process stat")
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	return start, f[0][0], err
}

func ReadOnlyMemfd(fd int) (*os.File, error) { return os.Open(fmt.Sprintf("/proc/self/fd/%d", fd)) }

func (h *Holder) Record() *RecordWriter { return h.record }
func (h *Holder) Done() <-chan struct{} { return h.done }
func (h *Holder) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.done:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.err
	}
}

// Start is called only after the normal runtime has released its mappings and
// all writers. The callback writes this top alone using an independently owned
// local sink. Failed writes retain RAM and retry; they never manufacture Ready.
func (h *Holder) Start(dump func(context.Context, io.ReaderAt) (string, error), release func(context.Context) error) error {
	if !h.record.Read().RuntimeRetired || dump == nil {
		return errors.New("memory checkpoint: start without retired runtime and dump")
	}
	h.mu.Lock()
	if h.started || h.closed {
		h.mu.Unlock()
		return errors.New("memory checkpoint: holder already started or closed")
	}
	h.started = true
	h.mu.Unlock()
	go func() {
		ctx := context.Background()
		for {
			ref, err := dump(ctx, h.file)
			if err == nil {
				err = h.record.Update(func(r *CaptureRecord) { r.PayloadRef = ref; r.FileReady = true; r.SaveError = "" })
			}
			if err == nil {
				break
			}
			h.logf("checkpoint memory save retained for retry: %v", err)
			_ = h.record.Update(func(r *CaptureRecord) { r.SaveError = err.Error() })
			time.Sleep(time.Second)
		}
		h.mu.Lock()
		close(h.ready)
		h.mu.Unlock()
		// File publication closes RAM admission. Drop our own backing before
		// testing an independent OFD; queued SCM_RIGHTS and accepted readers
		// keep the shared pin alive even if an ACK was lost or a peer exited.
		h.mu.Lock()
		closeErr := h.closeRAMLocked()
		h.mu.Unlock()
		if closeErr != nil {
			h.logf("checkpoint RAM descriptor close: %v", closeErr)
		}
		var proof *os.File
		for {
			r := h.record.Read()
			var err error
			proof, err = openRAMPin(h.record.Path(), r.Holder.RAMLock, unix.LOCK_EX)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EWOULDBLOCK) {
				h.logf("checkpoint RAM release proof retained for retry: %v", err)
			}
			select {
			case <-h.changed:
			case <-time.After(100 * time.Millisecond):
			}
		}
		defer proof.Close()
		// The record is the physical release fact. Resource ACK may be replayed
		// separately; a failed ACK cannot charge or free another generation.
		for {
			err := h.record.Update(func(r *CaptureRecord) { r.RAMReleased = true })
			if err == nil {
				break
			}
			h.logf("checkpoint release record: %v", err)
			time.Sleep(time.Second)
		}
		if release != nil {
			for {
				if err := release(ctx); err == nil {
					break
				} else {
					h.logf("checkpoint resource release retained for retry: %v", err)
				}
				time.Sleep(time.Second)
			}
		}
		h.finish(nil)
	}()
	return nil
}

func (h *Holder) Abort() error {
	return h.AbortWithCleanup(nil)
}

// AbortWithCleanup releases the provisional backing immediately. Metadata
// failure can retain only its record lock, GC pin and completion cleanup; it can
// never strand a duplicate RAM fd after a later capture has released its RAM.
func (h *Holder) AbortWithCleanup(complete func() error) error {
	h.abortOnce.Do(func() {
		h.mu.Lock()
		if h.started || h.record.Read().Committed {
			h.abortErr = errors.New("memory checkpoint: cannot abort committed holder")
			h.mu.Unlock()
			return
		}
		closeErr := h.closeBackingLocked()
		h.mu.Unlock()
		closeErr = errors.Join(closeErr, h.directory.close())
		h.abortErr = errors.Join(closeErr, AbortRecord(h.record, func() error {
			var err error
			if complete != nil {
				err = complete()
			}
			h.finish(errors.Join(closeErr, err))
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.err
		}, h.logf))
	})
	return h.abortErr
}

// closeBackingLocked closes only physical/service resources. Metadata and the
// done signal have a separate once-only completion after publication succeeds.
func (h *Holder) closeBackingLocked() error {
	if h.closed {
		return nil
	}
	h.closed = true
	var err error
	if h.listener != nil {
		err = h.listener.Close()
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
	}
	for _, l := range h.readers {
		_ = l.conn.Close()
	}
	clear(h.readers)
	return errors.Join(err, h.closeRAMLocked())
}

// Only Close releases a reference to the shared RAM-pin OFD. LOCK_UN would
// incorrectly unlock it for every queued/received copy of this descriptor.
func (h *Holder) closeRAMLocked() error {
	var err error
	if h.file != nil {
		err = h.file.Close()
		h.file = nil
	}
	if h.ramPin != nil {
		err = errors.Join(err, h.ramPin.Close())
		h.ramPin = nil
	}
	return err
}

func (h *Holder) finish(err error) {
	h.finishOnce.Do(func() {
		h.mu.Lock()
		err = errors.Join(err, h.closeBackingLocked())
		h.mu.Unlock()
		err = errors.Join(err, h.directory.close())
		if h.record != nil {
			err = errors.Join(err, h.record.Close())
		}
		if h.gcPin != nil {
			err = errors.Join(err, h.gcPin.Close())
			h.gcPin = nil
		}
		h.mu.Lock()
		h.err = err
		h.mu.Unlock()
		close(h.done)
	})
}

func (h *Holder) serve() {
	for {
		c, err := h.listener.AcceptUnix()
		if err != nil {
			return
		}
		go h.serveConn(c)
	}
}
func (h *Holder) serveConn(c *net.UnixConn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	var req holderMessage
	if err := readHolderMessage(c, &req); err != nil {
		return
	}
	r := h.record.Read()
	if req.MemoryID != r.MemoryID || req.Epoch != r.Epoch || req.CheckpointID != r.CheckpointID {
		_ = writeHolderMessage(c, holderMessage{Type: "error", Error: "holder identity mismatch"})
		return
	}
	pid, err := peerPID(c)
	if err != nil {
		return
	}
	start, err := ProcessStartTime(pid)
	if err != nil {
		return
	}
	if req.Type == "release" {
		h.release(req.Token, pid, start)
		_ = writeHolderMessage(c, holderMessage{Type: "released"})
		return
	}
	if req.Type != "open" || !r.Committed {
		_ = writeHolderMessage(c, holderMessage{Type: "error", Error: "capture is not committed"})
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	r = h.record.Read()
	if r.FileReady {
		h.mu.Unlock()
		_ = writeHolderMessage(c, holderMessage{Type: "file", PayloadRef: r.PayloadRef, Size: h.size})
		return
	}
	token := req.Token
	if !validID(token) || h.readers[token] != nil {
		h.mu.Unlock()
		_ = writeHolderMessage(c, holderMessage{Type: "error", Error: "invalid or duplicate reader token"})
		return
	}
	l := &readerLease{pid: pid, start: start, token: token, conn: c}
	h.readers[token] = l
	err = writeHolderMessage(c, holderMessage{Type: "ram", Token: token, Size: h.size})
	if err != nil {
		delete(h.readers, token)
		h.mu.Unlock()
		return
	} // No fd transfer was attempted.
	_, _, err = c.WriteMsgUnix([]byte{1}, unix.UnixRights(int(h.file.Fd()), int(h.ramPin.Fd())), nil)
	h.mu.Unlock()
	if err != nil {
		return
	} // The receiver might already own the fd.
	_ = c.SetDeadline(time.Time{})
	ack := make(chan holderMessage, 1)
	go func() { var m holderMessage; _ = readHolderMessage(c, &m); ack <- m }()
	select {
	case m := <-ack:
		if m.Type == "release" && m.Token == token {
			h.release(token, pid, start)
			_ = writeHolderMessage(c, holderMessage{Type: "released"})
		}
		return
	case <-h.ready:
	}
	r = h.record.Read()
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = writeHolderMessage(c, holderMessage{Type: "file", PayloadRef: r.PayloadRef, Size: h.size})
	if m := <-ack; m.Type == "release" && m.Token == token {
		h.release(token, pid, start)
		_ = writeHolderMessage(c, holderMessage{Type: "released"})
	}
}

func (h *Holder) release(token string, pid int, start uint64) {
	h.mu.Lock()
	if l := h.readers[token]; l != nil && l.pid == pid && l.start == start {
		delete(h.readers, token)
	}
	h.mu.Unlock()
	select {
	case h.changed <- struct{}{}:
	default:
	}
}

func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var inner error
	if err = raw.Control(func(fd uintptr) { cred, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return 0, err
	}
	if inner != nil {
		return 0, inner
	}
	return int(cred.Pid), nil
}
func writeHolderMessage(w io.Writer, m holderMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > 64*1024 {
		return errors.New("oversized holder message")
	}
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(b)))
	if err = writeAll(w, n[:]); err != nil {
		return err
	}
	return writeAll(w, b)
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) != 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return errors.New("invalid write count")
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
func readHolderMessage(r io.Reader, m *holderMessage) error {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return err
	}
	size := binary.LittleEndian.Uint32(n[:])
	if size == 0 || size > 64*1024 {
		return errors.New("invalid holder message length")
	}
	b := make([]byte, int(size))
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, m)
}
