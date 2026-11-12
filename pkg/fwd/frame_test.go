package fwd

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []Frame{
		{Type: FrameData, Payload: []byte("hello world")},
		{Type: FrameData, Payload: bytes.Repeat([]byte{0xab}, MaxFramePayload)},
		{Type: FrameEOF},
		{Type: FrameRST},
		{Type: FrameData, Payload: nil}, // empty data frame
	}
	var buf bytes.Buffer
	for _, in := range cases {
		if err := WriteFrame(&buf, in); err != nil {
			t.Fatalf("WriteFrame(%d): %v", in.Type, err)
		}
	}
	for i, want := range cases {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame #%d: %v", i, err)
		}
		if got.Type != want.Type {
			t.Errorf("#%d type: got %d want %d", i, got.Type, want.Type)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("#%d payload: got %d bytes want %d bytes", i, len(got.Payload), len(want.Payload))
		}
	}
	if _, err := ReadFrame(&buf); err != io.EOF {
		t.Errorf("trailing ReadFrame: got %v want EOF", err)
	}
}

func TestWriteFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	err := WriteFrame(&buf, Frame{Type: FrameData, Payload: make([]byte, MaxFramePayload+1)})
	if err != ErrFrameTooLarge {
		t.Fatalf("got %v want ErrFrameTooLarge", err)
	}
}

// shortReader returns one byte per Read so ReadFrame must reassemble a
// header/payload split across reads (io.ReadFull semantics).
type shortReader struct{ r io.Reader }

func (s shortReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return s.r.Read(p)
}

func TestReadFrameReassembles(t *testing.T) {
	var buf bytes.Buffer
	want := Frame{Type: FrameData, Payload: []byte("split across reads")}
	if err := WriteFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(shortReader{r: &buf})
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

type frameLimitedWriter struct {
	io.Writer
	max int
}

func (w frameLimitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.Writer.Write(p)
}

func TestWriteFrameHandlesShortWrites(t *testing.T) {
	frames := []Frame{
		{Type: FrameData, Payload: bytes.Repeat([]byte("payload"), DataChunk/7+1)},
		{Type: FrameData, Payload: []byte("next frame")},
		{Type: FrameEOF},
		{Type: FrameRST},
	}
	for _, tc := range []struct {
		name string
		max  int
	}{
		{"header_and_payload", 1},
		{"payload", FrameHeaderLen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := frameLimitedWriter{Writer: &buf, max: tc.max}
			for i, frame := range frames {
				if err := WriteFrame(w, frame); err != nil {
					t.Fatalf("WriteFrame #%d: %v", i, err)
				}
			}
			for i, want := range frames {
				got, err := ReadFrame(&buf)
				if err != nil {
					t.Fatalf("ReadFrame #%d: %v", i, err)
				}
				if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
					t.Fatalf("frame #%d corrupted: type=%d payload=%d bytes", i, got.Type, len(got.Payload))
				}
			}
			if _, err := ReadFrame(&buf); err != io.EOF {
				t.Fatalf("trailing ReadFrame: got %v want EOF", err)
			}
		})
	}
}

type frameWriterFunc func([]byte) (int, error)

func (f frameWriterFunc) Write(p []byte) (int, error) { return f(p) }

func TestWriteFrameWriteErrors(t *testing.T) {
	writeErr := errors.New("injected write failure")
	for _, tc := range []struct {
		name     string
		writeErr error
		wantErr  error
	}{
		{"no_progress", nil, io.ErrShortWrite},
		{"writer_error", writeErr, writeErr},
	} {
		for i, stage := range []string{"header", "payload"} {
			t.Run(tc.name+"/"+stage, func(t *testing.T) {
				calls := 0
				w := frameWriterFunc(func(p []byte) (int, error) {
					calls++
					if calls == i+1 {
						return 0, tc.writeErr
					}
					return len(p), nil
				})
				err := WriteFrame(w, Frame{Type: FrameData, Payload: []byte("payload")})
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v want %v", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), "write "+stage+":") {
					t.Fatalf("missing %s error context: %v", stage, err)
				}
			})
		}
	}
}
