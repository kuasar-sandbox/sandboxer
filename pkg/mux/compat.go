// Package mux preserves the sandboxer import path for the shared Guest ABI.
//
// Deprecated: use github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/mux.
package mux

import (
	"io"

	abi "github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/mux"
)

const (
	FrameHeaderLen    = abi.FrameHeaderLen
	MaxFramePayload   = abi.MaxFramePayload
	DataChunk         = abi.DataChunk
	DefaultWindow     = abi.DefaultWindow
	StreamControl     = abi.StreamControl
	StreamStdin       = abi.StreamStdin
	StreamStdout      = abi.StreamStdout
	StreamStderr      = abi.StreamStderr
	StreamPTY         = abi.StreamPTY
	FrameData         = abi.FrameData
	FrameEOF          = abi.FrameEOF
	FrameReset        = abi.FrameReset
	FrameWindowUpdate = abi.FrameWindowUpdate
	FrameSetWinsize   = abi.FrameSetWinsize
	FrameMuxClose     = abi.FrameMuxClose
	FrameMuxCloseAck  = abi.FrameMuxCloseAck
	FrameExitStatus   = abi.FrameExitStatus
)

type (
	Frame     = abi.Frame
	StreamSet = abi.StreamSet
	Options   = abi.Options
	Session   = abi.Session
	Stream    = abi.Stream
)

var (
	ErrFrameTooLarge = abi.ErrFrameTooLarge
	ErrStreamReset   = abi.ErrStreamReset
	ErrClosed        = abi.ErrClosed
	ErrProtocol      = abi.ErrProtocol
)

func WriteFrame(w io.Writer, f Frame) error { return abi.WriteFrame(w, f) }

func ReadFrame(r io.Reader) (Frame, error) { return abi.ReadFrame(r) }

func PipeStreams(stdin, stdout, stderr bool) StreamSet {
	return abi.PipeStreams(stdin, stdout, stderr)
}

func PTYStreams() StreamSet { return abi.PTYStreams() }

func NewSession(conn io.ReadWriteCloser, streams StreamSet, opt Options) *Session {
	return abi.NewSession(conn, streams, opt)
}
