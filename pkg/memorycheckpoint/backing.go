package memorycheckpoint

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"golang.org/x/sys/unix"
)

type OpenOptions struct {
	// SkipDirectoryPin leaves file lifetime protection to the caller. It must
	// hold an existing directory GC pin or retain the original file FDs until
	// this backing and all its in-flight reads and switching have closed.
	SkipDirectoryPin bool
	Directory        string
	OpenFile         func(context.Context, string) (fetch.Stream, error)
}

type TopBacking struct {
	Map      *memory.CaptureView
	Reader   io.ReaderAt
	reader   *switchReader
	conn     *net.UnixConn
	lease    holderMessage
	record   CaptureRecord
	options  OpenOptions
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
	closeErr error
	pin      *os.File
}

type switchReader struct {
	mu     sync.RWMutex
	ram    *os.File
	ramPin *os.File
	file   fetch.Stream
	closed bool
}

func (s *switchReader) ReadAt(p []byte, off int64) (int, error) {
	return s.ReadAtContext(context.Background(), p, off)
}
func (s *switchReader) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("memory checkpoint: negative memory offset")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, os.ErrClosed
	}
	if s.file != nil {
		return s.file.ReadAt(ctx, p, uint64(off))
	}
	if s.ram == nil {
		return 0, errors.New("memory checkpoint: missing memory backing")
	}
	return s.ram.ReadAt(p, off)
}
func (s *switchReader) replace(file fetch.Stream) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return file.Close()
	}
	ram, pin := s.ram, s.ramPin
	s.ram = nil
	s.ramPin = nil
	s.file = file
	s.mu.Unlock()
	return closeRAMPair(ram, pin)
}
func (s *switchReader) close() error {
	s.mu.Lock()
	s.closed = true
	ram, pin, file := s.ram, s.ramPin, s.file
	s.ram = nil
	s.ramPin = nil
	s.file = nil
	s.mu.Unlock()
	err := closeRAMPair(ram, pin)
	if file != nil {
		err = errors.Join(err, file.Close())
	}
	return err
}

func closeRAMPair(ram, pin *os.File) error {
	var err error
	if ram != nil {
		err = ram.Close()
	}
	if pin != nil {
		err = errors.Join(err, pin.Close())
	}
	return err
}

// PinDirectory protects live readers from checkpoint cleanup. The stable lock
// inode is never replaced; an exclusive cleanup must fail while a reader lives.
func PinDirectory(directory string) (*os.File, error) {
	return lockDirectory(directory, unix.LOCK_SH, true)
}

func ExclusiveDirectory(directory string) (*os.File, error) {
	return lockDirectory(directory, unix.LOCK_EX, true)
}

// PinExistingDirectory never creates a protection protocol for an external
// artifact. Only directories that already use the stable GC lock can be pooled.
func PinExistingDirectory(directory string) (*os.File, error) {
	return lockDirectory(directory, unix.LOCK_SH, false)
}

func lockDirectory(directory string, mode int, create bool) (*os.File, error) {
	flags := os.O_RDWR | unix.O_NOFOLLOW
	if create {
		flags |= os.O_CREATE
	}
	path := filepath.Join(directory, ".memory-readers.lock")
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	id, err := fileIdentity(f)
	var named unix.Stat_t
	if err == nil {
		err = unix.Lstat(path, &named)
	}
	if err == nil && (id.Device != uint64(named.Dev) || id.Inode != named.Ino || named.Mode&unix.S_IFMT != unix.S_IFREG) {
		err = errors.New("memory checkpoint: directory pin identity changed")
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func OpenTop(ctx context.Context, d Descriptor, opts OpenOptions) (top *TopBacking, retErr error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if opts.OpenFile == nil {
		return nil, errors.New("memory checkpoint: missing local carrier opener")
	}
	var pin *os.File
	var err error
	if !opts.SkipDirectoryPin {
		pin, err = PinDirectory(opts.Directory)
		if err != nil {
			return nil, err
		}
	}
	t := &TopBacking{reader: &switchReader{}, options: opts, done: make(chan struct{}), pin: pin}
	t.Reader = t.reader
	defer func() {
		if retErr != nil {
			_ = t.reader.close()
			if !t.releaseLease() {
				retryRelease(t.options.Directory, t.record, t.lease)
			}
			if t.conn != nil {
				_ = t.conn.Close()
			}
			if pin != nil {
				_ = pin.Close()
			}
		}
	}()
	m, err := opts.OpenFile(ctx, d.MapRef)
	if err != nil {
		return nil, err
	}
	n, err := mapBytes(d.Size)
	if err != nil {
		m.Close()
		return nil, err
	}
	if m.Size() != uint64(mapHeader+3*n) {
		m.Close()
		return nil, errors.New("memory checkpoint: map carrier size mismatch")
	}
	b := make([]byte, mapHeader+3*n)
	_, err = m.ReadAt(ctx, b, 0)
	err = errors.Join(err, m.Close())
	if err != nil {
		return nil, err
	}
	t.Map, err = DecodeMap(b, d.Size)
	if err != nil {
		return nil, err
	}
	r, err := ReadRecord(opts.Directory, d.CheckpointID, d.MemoryID)
	if err != nil {
		return nil, err
	}
	if r.Deleted {
		return nil, ErrDeleted
	}
	if r.MemoryID != d.MemoryID || !r.Committed || r.Aborted || r.Discarded {
		return nil, errors.New("memory checkpoint: uncommitted or mismatched backing")
	}
	t.record = *r
	if r.Lost() {
		return nil, fmt.Errorf("%w: checkpoint %q", ErrMemoryLost, r.CheckpointID)
	}
	openPayload := func(ref string) error {
		file, err := opts.OpenFile(ctx, ref)
		if err != nil {
			return err
		}
		if file.Size() != d.Size {
			file.Close()
			return errors.New("memory checkpoint: payload geometry mismatch")
		}
		// A deleted version rejects a newly opened file view too. The directory
		// pin already protects this view if deletion linearizes after this read.
		latest, err := ReadRecord(opts.Directory, d.CheckpointID, d.MemoryID)
		if err != nil {
			return errors.Join(err, file.Close())
		}
		if latest.Deleted {
			return errors.Join(ErrDeleted, file.Close())
		}
		t.reader.file = file
		close(t.done)
		return nil
	}
	if r.FileReady {
		return t, openPayload(r.PayloadRef)
	}
	c, err := dialHolder(ctx, r.Holder)
	if err != nil {
		// Publication and socket deletion can race the initial record read.
		path, pe := RecordPath(opts.Directory, r.CheckpointID, r.MemoryID)
		if pe != nil {
			return nil, pe
		}
		latest, re := RecoverMemoryRelease(ctx, path, *r)
		if re != nil {
			return nil, errors.Join(err, re)
		}
		if latest.Record.Deleted {
			return nil, ErrDeleted
		}
		if latest.Record.FileReady {
			return t, openPayload(latest.Record.PayloadRef)
		}
		if latest.Record.Lost() {
			return nil, fmt.Errorf("%w: checkpoint %q", ErrMemoryLost, r.CheckpointID)
		}
		if latest.ProducerExited {
			return nil, fmt.Errorf("%w: checkpoint %q", ErrMemoryUnavailable, r.CheckpointID)
		}
		return nil, err
	}
	t.conn = c
	stopOpen := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stopOpen()
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return nil, err
	}
	t.lease = holderMessage{Type: "release", MemoryID: r.MemoryID, Epoch: r.Epoch, CheckpointID: r.CheckpointID, Token: hex.EncodeToString(token[:])}
	request := t.lease
	request.Type = "open"
	if err = writeHolderMessage(c, request); err != nil {
		return nil, err
	}
	var reply holderMessage
	if err = readHolderMessage(c, &reply); err != nil {
		return nil, err
	}
	if reply.Type == "deleted" {
		return nil, ErrDeleted
	}
	if reply.Size != d.Size {
		return nil, errors.New("memory checkpoint: holder geometry mismatch")
	}
	if reply.Type == "file" {
		c.Close()
		t.conn = nil
		t.lease.Token = ""
		return t, openPayload(reply.PayloadRef)
	}
	if reply.Type != "ram" || reply.Token != t.lease.Token {
		return nil, fmt.Errorf("memory checkpoint: holder refused: %s", reply.Error)
	}
	t.reader.ram, t.reader.ramPin, err = receiveRAMPair(c)
	if err != nil {
		return nil, err
	}
	if err := validateRAMPin(t.reader.ramPin, r.Holder.RAMLock); err != nil {
		return nil, err
	}
	fdFlags, err := unix.FcntlInt(t.reader.ram.Fd(), unix.F_GETFL, 0)
	if err != nil || fdFlags&unix.O_ACCMODE != unix.O_RDONLY {
		return nil, errors.New("memory checkpoint: writable transferred fd")
	}
	stat, err := t.reader.ram.Stat()
	if err != nil || uint64(stat.Size()) != d.Size {
		return nil, errors.New("memory checkpoint: transferred fd geometry mismatch")
	}
	_ = c.SetDeadline(time.Time{})
	watchCtx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	go t.watch(watchCtx, d.Size)
	return t, nil
}

func receiveRAMPair(c *net.UnixConn) (*os.File, *os.File, error) {
	var one [1]byte
	oob := make([]byte, unix.CmsgSpace(4*4))
	_, oobn, flags, _, recvErr := c.ReadMsgUnix(one[:], oob)
	msgs, parseErr := unix.ParseSocketControlMessage(oob[:oobn])
	var fds []int
	for _, msg := range msgs {
		rights, err := unix.ParseUnixRights(&msg)
		parseErr = errors.Join(parseErr, err)
		fds = append(fds, rights...)
	}
	err := errors.Join(recvErr, parseErr)
	if flags&unix.MSG_CTRUNC != 0 {
		err = errors.Join(err, errors.New("memory checkpoint: truncated descriptor transfer"))
	}
	if len(fds) != 2 {
		err = errors.Join(err, errors.New("memory checkpoint: expected memory and RAM-pin descriptors"))
	}
	if err != nil {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		return nil, nil, err
	}
	// Go's Linux ReadMsgUnix uses MSG_CMSG_CLOEXEC. Keep the contract explicit
	// for both descriptors and never accept the former single-fd protocol.
	for _, fd := range fds {
		unix.CloseOnExec(fd)
	}
	return os.NewFile(uintptr(fds[0]), "checkpoint-memory"), os.NewFile(uintptr(fds[1]), "checkpoint-ram-pin"), nil
}

func (t *TopBacking) watch(ctx context.Context, size uint64) {
	defer close(t.done)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		r, err := ReadRecord(t.options.Directory, t.record.CheckpointID, t.record.MemoryID)
		if err != nil || !sameCaptureIdentity(*r, t.record) || !r.FileReady {
			continue
		}
		file, err := t.options.OpenFile(ctx, r.PayloadRef)
		if err != nil {
			continue
		}
		if file.Size() != size {
			_ = file.Close()
			continue
		}
		if err = t.reader.replace(file); err != nil {
			continue
		}
		for !t.releaseLease() {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
		return
	}
}

// releaseLease follows RAM/pin closure. A positive ACK or exact producer death
// ends this connection obligation; only the independent RAM proof can publish
// physical release. EOF and a successful write prove neither fact.
func (t *TopBacking) releaseLease() bool {
	if t.lease.Token == "" {
		return true
	}
	if leaseReleaseRecorded(t.options.Directory, t.record) {
		if t.conn != nil {
			_ = t.conn.Close()
			t.conn = nil
		}
		t.lease.Token = ""
		return true
	}
	path, err := RecordPath(t.options.Directory, t.record.CheckpointID, t.record.MemoryID)
	if err == nil {
		recovery, re := RecoverMemoryRelease(context.Background(), path, t.record)
		if re == nil && (recovery.Record.RAMReleased || recovery.ProducerExited) {
			if t.conn != nil {
				_ = t.conn.Close()
				t.conn = nil
			}
			t.lease.Token = ""
			return true
		}
	}
	if t.conn != nil {
		c := t.conn
		t.conn = nil
		_ = c.SetDeadline(time.Now().Add(time.Second))
		ok := releaseOnConnection(c, t.lease)
		_ = c.Close()
		if ok {
			t.lease.Token = ""
			return true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := dialHolder(ctx, t.record.Holder)
	if err != nil {
		return false
	}
	defer c.Close()
	if releaseOnConnection(c, t.lease) {
		t.lease.Token = ""
		return true
	}
	return false
}

func releaseOnConnection(c *net.UnixConn, lease holderMessage) bool {
	if err := writeHolderMessage(c, lease); err != nil {
		return false
	}
	for i := 0; i < 3; i++ {
		var reply holderMessage
		if readHolderMessage(c, &reply) != nil {
			return false
		}
		if reply.Type == "released" {
			return true
		}
		if reply.Type != "file" {
			return false
		}
	}
	return false
}

func leaseReleaseRecorded(directory string, record CaptureRecord) bool {
	r, err := ReadRecord(directory, record.CheckpointID, record.MemoryID)
	return err == nil && sameCaptureIdentity(*r, record) && r.RAMReleased
}

// A source Close must not retain borrowed storage while retrying an ACK. This
// continuation owns only lease metadata and a directory pin, never RAM or a
// payload reader. The pin preserves a release fact if the ACK was lost just
// before an otherwise idle SDK holder removed its socket and remained alive.
func retryRelease(directory string, record CaptureRecord, lease holderMessage) {
	if lease.Token == "" {
		return
	}
	pin, _ := PinDirectory(directory)
	go func() {
		if pin != nil {
			defer pin.Close()
		}
		for {
			if leaseReleaseRecorded(directory, record) {
				return
			}
			path, err := RecordPath(directory, record.CheckpointID, record.MemoryID)
			if err == nil {
				recovery, re := RecoverMemoryRelease(context.Background(), path, record)
				if re == nil && (recovery.Record.RAMReleased || recovery.ProducerExited) {
					return
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			c, err := dialHolder(ctx, record.Holder)
			ok := false
			if err == nil {
				ok = releaseOnConnection(c, lease)
				_ = c.Close()
			}
			cancel()
			if ok {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

func (t *TopBacking) Close() error {
	if t == nil {
		return nil
	}
	t.once.Do(func() {
		if t.cancel != nil {
			t.cancel()
		}
		<-t.done
		t.closeErr = t.reader.close()
		if !t.releaseLease() {
			retryRelease(t.options.Directory, t.record, t.lease)
		}
		if t.conn != nil {
			_ = t.conn.Close()
		}
		if t.pin != nil {
			t.closeErr = errors.Join(t.closeErr, t.pin.Close())
		}
	})
	return t.closeErr
}

func dialHolder(ctx context.Context, id HolderIdentity) (*net.UnixConn, error) {
	dir, err := openDirectory(id.Directory.Path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if err := verifyDirectory(dir, id.Directory); err != nil {
		return nil, err
	}
	marker, err := readHolderMarker(dir)
	if err != nil || marker.Holder != id {
		return nil, errors.Join(errors.New("memory checkpoint: holder marker identity mismatch"), err)
	}
	address := fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(id.SocketPath))
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", address)
	if err != nil {
		return nil, err
	}
	c := conn.(*net.UnixConn)
	pid, err := peerPID(c)
	if err != nil || pid != id.PID {
		c.Close()
		return nil, errors.New("memory checkpoint: holder PID mismatch")
	}
	start, err := ProcessStartTime(pid)
	if err != nil || start != id.StartTime {
		c.Close()
		return nil, errors.New("memory checkpoint: holder generation mismatch")
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)
	return c, nil
}
