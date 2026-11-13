// Package fwd preserves the sandboxer import path for the shared Guest ABI.
//
// Deprecated: use github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/fwd.
package fwd

import (
	"io"
	"net"

	abi "github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/fwd"
)

const (
	FrameHeaderLen  = abi.FrameHeaderLen
	MaxFramePayload = abi.MaxFramePayload
	DataChunk       = abi.DataChunk
	FrameData       = abi.FrameData
	FrameEOF        = abi.FrameEOF
	FrameRST        = abi.FrameRST
)

type (
	Frame = abi.Frame
	Relay = abi.Relay
)

var (
	ErrFrameTooLarge = abi.ErrFrameTooLarge
)

func WriteFrame(w io.Writer, f Frame) error { return abi.WriteFrame(w, f) }

func ReadFrame(r io.Reader) (Frame, error) { return abi.ReadFrame(r) }

func NewRelay(framed io.ReadWriteCloser, plain net.Conn) *Relay {
	return abi.NewRelay(framed, plain)
}
