// Package fwd implements the per-connection frame sub-protocol that
// carries `sandbox-ctl run --connect` port-forward traffic over a
// reverse-channel vsock connection (docs/sandbox-init.md §3.7 / §4).
//
// Unlike the stdio MUX (pkg/mux), which multiplexes several
// fixed streams over one connection and therefore needs per-stream
// windows, a forward connection is 1:1 with its vsock connection — one
// logical byte stream per conn — so no stream IDs and no application
// window are needed: the vsock connection's own kernel-buffer backpressure
// is the flow control. What the frames DO add over a raw byte relay is
// faithful TCP half-close: the close signal rides in-band as an EOF/RST
// byte in the data plane, so it survives CH's hybrid vsock proxy (which
// does not translate transport-level shutdown(SHUT_WR) across the
// UDS↔vsock boundary — the same reason the stdio MUX uses FrameEOF rather
// than relying on the conn closing).
//
// stdlib-only: the guest sandbox-init binary imports this package.
package fwd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Wire format of one frame:
//
//	┌────────┬───────────────┬────────────────────────┐
//	│  type  │  len (u16 BE) │   payload (len bytes)  │
//	│  (u8)  │               │                        │
//	└────────┴───────────────┴────────────────────────┘
const (
	FrameHeaderLen = 3
	// MaxFramePayload is the absolute wire cap (u16 length field).
	MaxFramePayload = 0xFFFF
	// DataChunk caps a single DATA frame; larger reads are split. Matches
	// the stdio MUX's chunk so the read buffer sizing is consistent.
	DataChunk = 32 * 1024
)

// Frame types.
const (
	// FrameData carries payload bytes in one direction.
	FrameData uint8 = 0
	// FrameEOF half-closes the sender's write direction: the peer's read
	// side sees EOF (it does CloseWrite on the spliced conn) but may keep
	// sending on the other direction. No payload.
	FrameEOF uint8 = 1
	// FrameRST aborts the forward in both directions (the spliced conn
	// errored or was reset). No payload.
	FrameRST uint8 = 2
)

// Frame is one decoded wire frame. Payload is owned by the frame (a fresh
// slice on read).
type Frame struct {
	Type    uint8
	Payload []byte
}

// ErrFrameTooLarge is returned by WriteFrame when the payload exceeds the
// u16 wire cap.
var ErrFrameTooLarge = errors.New("fwd: frame payload exceeds 65535 bytes")

// WriteFrame writes one frame to w.
func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxFramePayload {
		return ErrFrameTooLarge
	}
	var hdr [FrameHeaderLen]byte
	hdr[0] = f.Type
	binary.BigEndian.PutUint16(hdr[1:3], uint16(len(f.Payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("fwd: write header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("fwd: write payload: %w", err)
		}
	}
	return nil
}

// ReadFrame reads one frame from r. The returned Frame.Payload is a freshly
// allocated slice (nil when len==0).
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [FrameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint16(hdr[1:3])
	f := Frame{Type: hdr[0]}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return Frame{}, fmt.Errorf("fwd: read payload: %w", err)
		}
	}
	return f, nil
}
