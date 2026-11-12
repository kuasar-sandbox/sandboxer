package wireio

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type limitedWriter struct {
	bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.Buffer.Write(p)
}

func TestWriteAllHandlesShortWrites(t *testing.T) {
	w := &limitedWriter{max: 3}
	want := bytes.Repeat([]byte("payload"), 1024)
	if err := WriteAll(w, want); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("wrote %d bytes, want %d", w.Len(), len(want))
	}
}

func TestWriteAllRejectsNoProgress(t *testing.T) {
	err := WriteAll(writerFunc(func([]byte) (int, error) { return 0, nil }), []byte("payload"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WriteAll error = %v, want io.ErrShortWrite", err)
	}
}

func TestWriteAllPreservesWriterError(t *testing.T) {
	wantErr := errors.New("writer failed")
	err := WriteAll(writerFunc(func([]byte) (int, error) { return -1, wantErr }), []byte("payload"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("WriteAll error = %v, want %v", err, wantErr)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestWriteFullReportsProgress(t *testing.T) {
	wantErr := errors.New("write failed")
	for _, tt := range []struct {
		name    string
		n       int
		err     error
		written int
		wantErr error
	}{
		{"progress and error", 1, wantErr, 3, wantErr},
		{"syscall error", -1, wantErr, 2, wantErr},
		{"no progress", 0, nil, 2, io.ErrShortWrite},
		{"negative count", -1, nil, 2, io.ErrShortWrite},
		{"oversized count", 3, nil, 2, io.ErrShortWrite},
	} {
		t.Run(tt.name, func(t *testing.T) {
			write := func(p []byte) (int, error) {
				switch string(p) {
				case "abcd":
					return 2, nil
				case "cd":
					return tt.n, tt.err
				default:
					t.Fatalf("unexpected remaining data %q", p)
					return 0, nil
				}
			}
			n, err := WriteFull(write, []byte("abcd"))
			if n != tt.written || !errors.Is(err, tt.wantErr) {
				t.Fatalf("WriteFull = (%d, %v), want (%d, %v)", n, err, tt.written, tt.wantErr)
			}
		})
	}
}
