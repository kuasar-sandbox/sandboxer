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

// Start is called only after the normal runtime has stopped all writers.
// The callback writes this top alone using an independently owned local sink.
func (h *Holder) Start(dump func(context.Context, io.ReaderAt) (string, error), release func(context.Context) error) error {
	return h.StartWithCleanup(dump, nil, release)
}

// StartWithCleanup additionally owns a retired, inaccessible backend mapping.
// Its cleanup runs off the retirement critical path, after save or discard and
// before the RAM pin is dropped and RAMReleased can be published. It may retry;
// neither a failed cleanup nor a failed dump can manufacture a release.
func (h *Holder) StartWithCleanup(dump func(context.Context, io.ReaderAt) (string, error), cleanup func() error, release func(context.Context) error) error {
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
	go h.save(dump, cleanup, release)
	return nil
}

type dumpResult struct {
	ref string
	err error
}

// cancellationProofLocked drops only our pin reference. The writer and shared
// directory pin continue to protect the local memfd and its parent graph. An
// independent exclusive OFD proves that no accepted/queued RAM reader remains.
// On a conflict we restore our own protection without unlocking anyone else's
// shared OFD. Other holders' directory pins do not prevent this operation.
func (h *Holder) cancellationProofLocked() *os.File {
	r := h.record.Read()
	if !r.Deleted || !r.RuntimeRetired || r.FileReady || r.RAMReleased {
		return nil
	}
	if h.ramPin != nil {
		if err := h.ramPin.Close(); err != nil {
			h.logf("checkpoint delete pin close: %v", err)
		}
		h.ramPin = nil
	}
	proof, err := openRAMPin(h.record.Path(), r.Holder.RAMLock, unix.LOCK_EX)
	if err == nil {
		return proof
	}
	if !errors.Is(err, unix.EWOULDBLOCK) {
		h.logf("checkpoint delete RAM proof retained: %v", err)
	}
	h.ramPin, err = openRAMPin(h.record.Path(), r.Holder.RAMLock, unix.LOCK_SH)
	if err != nil {
		// Do not invent release on a missing/replaced proof. The unique writer
		// retains responsibility and retries, including after a finished dump.
		h.logf("checkpoint delete pin protection retry: %v", err)
	}
	return nil
}

func (h *Holder) save(dump func(context.Context, io.ReaderAt) (string, error), cleanup func() error, release func(context.Context) error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var result <-chan dumpResult
	var cancel context.CancelFunc
	var retryAt time.Time
	var proof *os.File
	discarded := false
	for {
		h.mu.Lock()
		proof = h.cancellationProofLocked()
		h.mu.Unlock()
		if proof != nil {
			// Publication happens only in this coordinator. Holding the proof
			// until the entire callback exits fences a late successful result.
			if cancel != nil {
				cancel()
				<-result
			}
			discarded = true
			break
		}
		if result == nil && !time.Now().Before(retryAt) {
			ctx, stop := context.WithCancel(context.Background())
			cancel = stop
			ch := make(chan dumpResult, 1)
			result = ch
			file := h.file // not closed until this complete task has exited
			go func() { ref, err := dump(ctx, file); ch <- dumpResult{ref, err} }()
		}
		select {
		case out := <-result:
			cancel()
			cancel, result = nil, nil
			h.mu.Lock()
			// Delete and FileReady share this lock. With surviving RAM readers,
			// a deleted version may still publish and switch those readers.
			if out.err == nil {
				out.err = h.record.Update(func(r *CaptureRecord) { r.PayloadRef = out.ref; r.FileReady = true; r.SaveError = "" })
			}
			if out.err == nil {
				close(h.ready)
			}
			h.mu.Unlock()
			if out.err == nil {
				goto published
			}
			h.logf("checkpoint memory save retained for retry: %v", out.err)
			_ = h.record.Update(func(r *CaptureRecord) { r.SaveError = out.err.Error() })
			retryAt = time.Now().Add(time.Second)
		case <-h.changed:
		case <-ticker.C:
		}
	}
published:
	// On delete the exclusive proof is already held; on publication our shared
	// RAM pin remains held. In both cases the producer's retained charge must
	// survive until even its inaccessible old mapping has gone away.
	for {
		r := h.record.Read()
		if r.VMM == nil {
			break
		}
		gone, err := r.VMM.Exited()
		if err == nil && gone {
			break
		}
		if err != nil {
			h.logf("checkpoint VMM exit proof retained for retry: %v", err)
		}
		select {
		case <-h.changed:
		case <-ticker.C:
		}
	}
	for cleanup != nil {
		if err := cleanup(); err == nil {
			break
		} else {
			h.logf("checkpoint retired mapping cleanup retained for retry: %v", err)
		}
		time.Sleep(time.Second)
	}
	h.mu.Lock()
	closeErr := h.closeRAMLocked()
	h.mu.Unlock()
	if closeErr != nil {
		h.logf("checkpoint RAM descriptor close: %v", closeErr)
	}
	for proof == nil {
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
		case <-ticker.C:
		}
	}
	defer proof.Close()
	for {
		err := h.record.Update(func(r *CaptureRecord) {
			r.RAMReleased = true
			if discarded {
				r.Discarded = true
				r.SaveError = ""
			}
		})
		if err == nil {
			break
		}
		h.logf("checkpoint release record: %v", err)
		time.Sleep(time.Second)
	}
	if release != nil {
		for {
			if err := release(context.Background()); err == nil {
				break
			} else {
				h.logf("checkpoint resource release retained for retry: %v", err)
			}
			time.Sleep(time.Second)
		}
	}
	h.finish(nil)
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
	if req.Type == "delete" {
		if err := h.markDeleted(); err != nil {
			_ = writeHolderMessage(c, holderMessage{Type: "error", Error: err.Error()})
		} else {
			_ = writeHolderMessage(c, holderMessage{Type: "deleted"})
		}
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
	if r.Deleted {
		h.mu.Unlock()
		_ = writeHolderMessage(c, holderMessage{Type: "deleted"})
		return
	}
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
