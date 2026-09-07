// Package mux implements the stdio MUX sub-protocol that runs over the
// vsock connection born from a launch / restore / attach / exec management
// operation (docs/sandbox-init.md §4.5 / §4.6).
//
// One MUX connection carries the user application's stdin/stdout/stderr
// (pipe mode) or a single pty (tty mode), each as a logical stream,
// plus control frames for winsize changes and the graceful-close handshake.
// WINDOW_UPDATE carries the relevant data stream ID.
//
// Uses the standard library plus internal/wireio; imported by sandbox-init.
package mux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/kuasar-sandbox/sandboxer/internal/wireio"
)

// Wire format of one frame:
//
//	┌────────┬────────┬───────────────┬────────────────────────┐
//	│ stream │  type  │  len (u16 BE) │   payload (len bytes)  │
//	│  (u8)  │  (u8)  │               │                        │
//	└────────┴────────┴───────────────┴────────────────────────┘
const (
	FrameHeaderLen = 4
	// MaxFramePayload is the absolute wire cap (u16 length field).
	MaxFramePayload = 0xFFFF
	// DataChunk caps a single DATA frame so the muxer stays fair across
	// streams; larger writes are split.
	DataChunk = 32 * 1024
	// DefaultWindow is the per-stream receive window (and the implicit
	// initial send credit each side assumes for streams the peer sends).
	DefaultWindow = 64 * 1024
)

// Stream IDs.
const (
	StreamControl uint8 = 0
	StreamStdin   uint8 = 1 // host → guest (app input)
	StreamStdout  uint8 = 2 // guest → host
	StreamStderr  uint8 = 3 // guest → host
	StreamPTY     uint8 = 4 // bidirectional (pty master ↔ host terminal)
)

// Frame types.
const (
	FrameData         uint8 = 0 // payload = bytes; on a data stream
	FrameEOF          uint8 = 1 // no payload; sender half-closes the write dir of `Stream`
	FrameReset        uint8 = 2 // no payload; aborts `Stream`
	FrameWindowUpdate uint8 = 3 // `Stream` = the data stream gaining credit; payload = uint32 BE delta
	FrameSetWinsize   uint8 = 4 // `Stream` = StreamControl; payload = [cols:u16 BE][rows:u16 BE]
	FrameMuxClose     uint8 = 5 // `Stream` = StreamControl; no payload
	FrameMuxCloseAck  uint8 = 6 // `Stream` = StreamControl; no payload
	FrameExitStatus   uint8 = 7 // `Stream` = StreamControl; payload = uint32 BE exit code (128+sig if signalled)
)

// Frame is one decoded wire frame. Payload is owned by the frame (a
// fresh slice on read).
type Frame struct {
	Stream  uint8
	Type    uint8
	Payload []byte
}

// ErrFrameTooLarge is returned by WriteFrame when the payload exceeds
// the u16 wire cap.
var ErrFrameTooLarge = errors.New("mux: frame payload exceeds 65535 bytes")

// WriteFrame writes one frame to w.
func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxFramePayload {
		return ErrFrameTooLarge
	}
	var hdr [FrameHeaderLen]byte
	hdr[0] = f.Stream
	hdr[1] = f.Type
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(f.Payload)))
	if err := wireio.WriteAll(w, hdr[:]); err != nil {
		return fmt.Errorf("mux: write header: %w", err)
	}
	if len(f.Payload) > 0 {
		if err := wireio.WriteAll(w, f.Payload); err != nil {
			return fmt.Errorf("mux: write payload: %w", err)
		}
	}
	return nil
}

// ReadFrame reads one frame from r. The returned Frame.Payload is a
// freshly allocated slice (nil when len==0).
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [FrameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint16(hdr[2:4])
	f := Frame{Stream: hdr[0], Type: hdr[1]}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return Frame{}, fmt.Errorf("mux: read payload: %w", err)
		}
	}
	return f, nil
}

// --- small payload encoders/decoders --------------------------------

func encodeWinsize(cols, rows uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], cols)
	binary.BigEndian.PutUint16(b[2:4], rows)
	return b
}

func decodeWinsize(p []byte) (cols, rows uint16, ok bool) {
	if len(p) != 4 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(p[0:2]), binary.BigEndian.Uint16(p[2:4]), true
}

func encodeUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func decodeUint32(p []byte) (uint32, bool) {
	if len(p) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(p), true
}

// isDataStream reports whether s is one of the app data streams.
func isDataStream(s uint8) bool {
	return s == StreamStdin || s == StreamStdout || s == StreamStderr || s == StreamPTY
}
