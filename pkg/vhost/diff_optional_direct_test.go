package vhost

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func optionalDirectFixture(t *testing.T, encrypted bool) *diffFile {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "active"))
	if err != nil {
		t.Fatal(err)
	}
	d := &diffFile{f: f, bodyIO: f, logicalSize: 3 * cowBlockSize}
	if encrypted {
		codec, e := newDiffEncryption(testDiffKey(27))
		if e != nil {
			_ = f.Close()
			t.Fatal(e)
		}
		d, err = createEncryptedDiffFile(f, 3*cowBlockSize, codec, rand.Reader)
	} else {
		err = f.Truncate(d.logicalSize)
	}
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

type optionalDirectFailingBody struct {
	err           error
	reads, writes int
}

func (b *optionalDirectFailingBody) ReadAt([]byte, int64) (int, error)  { b.reads++; return 0, b.err }
func (b *optionalDirectFailingBody) WriteAt([]byte, int64) (int, error) { b.writes++; return 0, b.err }

func TestDiffOptionalDirectDoesNotRetryDataErrors(t *testing.T) {
	for _, failure := range []error{unix.EIO, unix.ENOSPC, io.ErrShortWrite} {
		t.Run(failure.Error(), func(t *testing.T) {
			d := optionalDirectFixture(t, false)
			setCalls := 0
			err := d.enableDirectWithFcntl(func(fd uintptr, cmd, arg int) (int, error) {
				if cmd == unix.F_SETFL {
					setCalls++
					return 0, unix.EOPNOTSUPP
				}
				return unix.FcntlInt(fd, cmd, arg)
			})
			if err != nil {
				t.Fatal(err)
			}
			body := &optionalDirectFailingBody{err: failure}
			d.bodyIO = body
			if _, err := d.WriteAt(make([]byte, cowBlockSize), 0); !errors.Is(err, failure) {
				t.Fatalf("write failure hidden: %v", err)
			}
			if _, err := d.ReadAt(make([]byte, cowBlockSize), 0); !errors.Is(err, failure) {
				t.Fatalf("read failure hidden: %v", err)
			}
			if body.reads != 1 || body.writes != 1 || setCalls != 1 {
				t.Fatalf("data error retried: %+v sets=%d", body, setCalls)
			}
		})
	}
}
