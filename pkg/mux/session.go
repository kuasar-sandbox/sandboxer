package mux

import (
	"errors"
	"io"
	"sync"
)

// StreamSet is the set of active data streams on a MUX connection,
// derived from the negotiated StdioSpec:
//
//	tty mode  → {StreamPTY}
//	pipe mode → subset of {StreamStdin, StreamStdout, StreamStderr}
type StreamSet map[uint8]bool

// PipeStreams builds the pipe-mode stream set from per-channel flags.
func PipeStreams(stdin, stdout, stderr bool) StreamSet {
	s := StreamSet{}
	if stdin {
		s[StreamStdin] = true
	}
	if stdout {
		s[StreamStdout] = true
	}
	if stderr {
		s[StreamStderr] = true
	}
	return s
}

// PTYStreams is the tty-mode stream set.
func PTYStreams() StreamSet { return StreamSet{StreamPTY: true} }

// Options configures a Session.
type Options struct {
	// OnSetWinsize, if non-nil, is called from the read loop whenever a
	// SET_WINSIZE control frame arrives (guest side: do TIOCSWINSZ).
	OnSetWinsize func(cols, rows uint16)
	// Window is the per-stream receive window; 0 → DefaultWindow.
	Window int
}

// Sentinel errors.
var (
	ErrStreamReset = errors.New("mux: stream reset by peer")
	ErrClosed      = errors.New("mux: session closed")
	ErrProtocol    = errors.New("mux: protocol violation")
)

// Session multiplexes the negotiated data streams over one connection.
// It is symmetric — host and guest both create a Session over their end
// of the same conn; which streams a side actually reads vs writes is up
// to the caller (host writes StreamStdin and reads StreamStdout; guest
// the reverse; both read and write StreamPTY).
type Session struct {
	conn   io.ReadWriteCloser
	window int

	wmu sync.Mutex // serializes frame writes to conn

	mu      sync.Mutex
	streams map[uint8]*Stream
	// MUX_CLOSE handshake state (guarded by mu).
	closeInitiated bool          // we sent MUX_CLOSE
	closeAckedCh   chan struct{} // closed when MUX_CLOSE_ACK received
	peerClosedCh   chan struct{} // closed when MUX_CLOSE received from peer
	closeAcked     bool
	peerClosed     bool
	respClosed     bool // responder closed conn after MUX_CLOSE_ACK (read err expected)

	onSetWinsize func(cols, rows uint16)

	// Exec exit-status relay (exec sessions only; the launch/restore/
	// attach app path never sends FrameExitStatus). exitCh closes once
	// the peer's exit code has been received.
	exitCode int32
	exitRecv bool
	exitCh   chan struct{}

	doneCh  chan struct{}
	errOnce sync.Once
	err     error
}

// NewSession wraps conn and starts the read loop. streams is the
// negotiated set; only frames for those data streams (plus the control
// stream) are accepted — anything else is a protocol violation that
// tears the session down.
func NewSession(conn io.ReadWriteCloser, streams StreamSet, opt Options) *Session {
	w := opt.Window
	if w <= 0 {
		w = DefaultWindow
	}
	s := &Session{
		conn:         conn,
		window:       w,
		streams:      make(map[uint8]*Stream),
		closeAckedCh: make(chan struct{}),
		peerClosedCh: make(chan struct{}),
		onSetWinsize: opt.OnSetWinsize,
		exitCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
	for id := range streams {
		s.streams[id] = newStream(s, id, w)
	}
	go s.readLoop()
	return s
}

// Stream returns the handle for data stream id, or nil if id is not in
// the negotiated set.
func (s *Session) Stream(id uint8) *Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

// SetWinsize sends a SET_WINSIZE control frame (host side, on SIGWINCH).
func (s *Session) SetWinsize(cols, rows uint16) error {
	return s.writeFrame(Frame{Stream: StreamControl, Type: FrameSetWinsize, Payload: encodeWinsize(cols, rows)})
}

// InitMuxClose performs the guest-initiated graceful close handshake
// (docs/sandbox-init.md §4.6): send MUX_CLOSE, then block until the
// peer's MUX_CLOSE_ACK arrives (or the session ends). After this call
// returns, data Writes are refused; the caller should then Close().
// Frames arriving in the meantime are still processed normally.
func (s *Session) InitMuxClose() error {
	s.mu.Lock()
	if s.closeInitiated {
		ackCh := s.closeAckedCh
		s.mu.Unlock()
		<-ackCh
		return nil
	}
	s.closeInitiated = true
	s.mu.Unlock()
	if err := s.writeFrame(Frame{Stream: StreamControl, Type: FrameMuxClose}); err != nil {
		return err
	}
	select {
	case <-s.closeAckedCh:
		return nil
	case <-s.doneCh:
		return s.Err()
	}
}

// SendExitStatus sends the exec child's exit code as a control frame
// (exec sessions only). The receiver reads it via ExitStatus after the
// session ends. Sent before the MUX_CLOSE handshake so it is in flight
// before the conn is torn down.
func (s *Session) SendExitStatus(code int) error {
	return s.writeFrame(Frame{Stream: StreamControl, Type: FrameExitStatus, Payload: encodeUint32(uint32(int32(code)))})
}

// ExitStatus returns the exec child's exit code if a FrameExitStatus
// was received (ok=false otherwise — e.g. the app stdio path, which
// never sends one, or a session that died before the peer reported).
func (s *Session) ExitStatus() (code int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.exitCode), s.exitRecv
}

// ExitReceived returns a channel closed when a FrameExitStatus has been
// received. exec uses this (not Done) to know the remote command
// finished: the guest sends FrameExitStatus after all stdout/stderr
// EOFs, so observing it means output is fully delivered — without
// depending on the underlying conn close propagating (CH's hybrid
// vsock proxy only surfaces a peer close on subsequent I/O).
func (s *Session) ExitReceived() <-chan struct{} { return s.exitCh }

// PeerClosed returns a channel closed when the peer sent MUX_CLOSE
// (i.e. this side should be the one ACKing — the Session does that
// automatically; this is just an observation hook for the host side).
func (s *Session) PeerClosed() <-chan struct{} { return s.peerClosedCh }

// Done returns a channel closed when the read loop has exited (conn
// closed or read error).
func (s *Session) Done() <-chan struct{} { return s.doneCh }

// Err returns the first read-loop error (nil on clean EOF / Close()).
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close closes the underlying connection, which unblocks the read loop
// and any blocked Stream Reads/Writes.
func (s *Session) Close() error { return s.conn.Close() }

// --- internal -------------------------------------------------------

func (s *Session) setErr(err error) {
	if err == nil {
		return
	}
	s.errOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
	})
}

func (s *Session) writeFrame(f Frame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return WriteFrame(s.conn, f)
}

// writeData sends b as DATA frames on stream st, each ≤ DataChunk and
// bounded by the available send credit (blocking until ≥1 credit). It
// never waits for an arbitrary credit threshold, so it makes progress
// even when the peer's window is smaller than DataChunk. Returns the
// number of bytes written before any error.
func (s *Session) writeData(st *Stream, b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		want := len(b)
		if want > DataChunk {
			want = DataChunk
		}
		n, err := st.acquireUpTo(want)
		if err != nil {
			return total, err
		}
		if err := s.writeFrame(Frame{Stream: st.id, Type: FrameData, Payload: b[:n]}); err != nil {
			return total, err
		}
		total += n
		b = b[n:]
	}
	return total, nil
}

func (s *Session) readLoop() {
	defer close(s.doneCh)
	defer func() {
		// Wake everyone blocked on a stream.
		s.mu.Lock()
		for _, st := range s.streams {
			st.shutdown()
		}
		// Wake InitMuxClose waiters if still pending.
		if !s.closeAcked {
			s.closeAcked = true
			close(s.closeAckedCh)
		}
		s.mu.Unlock()
	}()
	for {
		f, err := ReadFrame(s.conn)
		if err != nil {
			s.mu.Lock()
			respClosed := s.respClosed
			s.mu.Unlock()
			if err != io.EOF && !respClosed {
				s.setErr(err)
			}
			return
		}
		if err := s.dispatch(f); err != nil {
			s.setErr(err)
			_ = s.conn.Close()
			return
		}
	}
}

func (s *Session) dispatch(f Frame) error {
	switch f.Type {
	case FrameData, FrameEOF, FrameReset:
		if !isDataStream(f.Stream) {
			return ErrProtocol
		}
		st := s.Stream(f.Stream)
		if st == nil {
			return ErrProtocol // frame for a stream not in the negotiated set
		}
		switch f.Type {
		case FrameData:
			return st.deliver(f.Payload)
		case FrameEOF:
			st.deliverEOF()
		case FrameReset:
			st.deliverReset()
		}
	case FrameWindowUpdate:
		if !isDataStream(f.Stream) {
			return ErrProtocol
		}
		st := s.Stream(f.Stream)
		if st == nil {
			return ErrProtocol
		}
		delta, ok := decodeUint32(f.Payload)
		if !ok {
			return ErrProtocol
		}
		st.addCredit(int(delta))
	case FrameSetWinsize:
		if f.Stream != StreamControl {
			return ErrProtocol
		}
		cols, rows, ok := decodeWinsize(f.Payload)
		if !ok {
			return ErrProtocol
		}
		if s.onSetWinsize != nil {
			s.onSetWinsize(cols, rows)
		}
	case FrameMuxClose:
		if f.Stream != StreamControl {
			return ErrProtocol
		}
		s.mu.Lock()
		if !s.peerClosed {
			s.peerClosed = true
			close(s.peerClosedCh)
		}
		s.respClosed = true
		s.mu.Unlock()
		// We're the responder: reply ACK. (The doc lets the responder
		// flush pending first; in our flows the responder — always the
		// host — has nothing pending when MUX_CLOSE arrives.)
		if err := s.writeFrame(Frame{Stream: StreamControl, Type: FrameMuxCloseAck}); err != nil {
			return err
		}
		// Complete the teardown: close our conn so the initiator (guest)
		// receives the RST and its SO_LINGER close returns with the vsock
		// socket actually removed — not left in virtio-vsock's 8s deferred
		// window where a snapshot would capture it as a half-closed
		// remnant (which a later restore's reused muxer local port
		// collides with). The next readLoop ReadFrame fails on the closed
		// conn; respClosed marks that error expected (clean teardown, not
		// a session fault).
		_ = s.conn.Close()
	case FrameMuxCloseAck:
		if f.Stream != StreamControl {
			return ErrProtocol
		}
		s.mu.Lock()
		if !s.closeAcked {
			s.closeAcked = true
			close(s.closeAckedCh)
		}
		s.mu.Unlock()
	case FrameExitStatus:
		if f.Stream != StreamControl {
			return ErrProtocol
		}
		code, ok := decodeUint32(f.Payload)
		if !ok {
			return ErrProtocol
		}
		s.mu.Lock()
		if !s.exitRecv {
			s.exitCode = int32(code)
			s.exitRecv = true
			close(s.exitCh)
		}
		s.mu.Unlock()
	default:
		return ErrProtocol
	}
	return nil
}

func (s *Session) refuseWrites() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeInitiated
}
