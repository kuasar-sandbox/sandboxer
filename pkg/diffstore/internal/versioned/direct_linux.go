package versioned

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/directio"
	"golang.org/x/sys/unix"
)

// Four bounded shared mappings are sufficient for overlapping owner and
// publisher I/O. MAP_SHARED is essential: private Go-heap DIO buffers are not
// safe while another goroutine forks a guest/helper process. No borrowed buffer
// escapes its callback; index caches hold ordinary copies. The pool lives for the
// process lifetime and never exceeds 32 MiB, irrespective of version count.
const directBufferSize = 8 << 20

var directPool struct {
	once    sync.Once
	buffers chan *directio.Buffer
	err     error
}

func withDirectBuffer(n int, fn func([]byte) error) error {
	if n < 0 || n > directBufferSize || n%PageSize != 0 {
		return fmt.Errorf("diffstore: invalid direct I/O size %d", n)
	}
	directPool.once.Do(func() {
		directPool.buffers = make(chan *directio.Buffer, 4)
		for i := 0; i < 4; i++ {
			b, err := directio.New(directBufferSize, PageSize, directBufferSize)
			if err != nil {
				directPool.err = err
				for len(directPool.buffers) != 0 {
					_ = (<-directPool.buffers).Close()
				}
				return
			}
			directPool.buffers <- b
		}
	})
	if directPool.err != nil {
		return directPool.err
	}
	b := <-directPool.buffers
	defer func() { clear(b.Bytes[:n]); directPool.buffers <- b }()
	return fn(b.Bytes[:n])
}
func directRead(f *os.File, n int, off int64) ([]byte, error) {
	if off < 0 || off%PageSize != 0 {
		return nil, fmt.Errorf("diffstore: unaligned direct read")
	}
	var result []byte
	err := withDirectBuffer(n, func(b []byte) error {
		m, err := directio.RetryInterruptedIO(unix.Pread, int(f.Fd()), b, off)
		if err != nil {
			return err
		}
		if m != len(b) {
			return io.ErrUnexpectedEOF
		}
		result = append([]byte(nil), b...)
		return nil
	})
	return result, err
}
func directWrite(f *os.File, p []byte, off int64) error {
	if off < 0 || off%PageSize != 0 {
		return fmt.Errorf("diffstore: unaligned direct write")
	}
	return withDirectBuffer(len(p), func(b []byte) error {
		copy(b, p)
		n, err := directio.RetryInterruptedIO(unix.Pwrite, int(f.Fd()), b, off)
		if err != nil {
			return err
		}
		if n != len(b) {
			return io.ErrShortWrite
		}
		return nil
	})
}

// readData consumes one bounded physical run while Store.mu protects its active
// mappings. Plaintext is copied directly from the shared DIO buffer and cleared
// before returning it to the pool; no payload copy is allocated on the Go heap.
func (s *Store) readData(p []byte, page, block uint64, pages, inner int) (done int, err error) {
	err = withDirectBuffer(pages*PageSize, func(b []byte) error {
		read := unix.Pread
		if s.preadData != nil {
			read = s.preadData
		}
		n, readErr := directio.RetryInterruptedIO(read, int(s.file.Fd()), b, int64(page)*PageSize)
		// Preserve the page-at-a-time contract on short I/O: only complete
		// data pages can be decrypted and delivered; a partial page is invalid.
		complete := max(0, n) / PageSize
		for i := 0; i < complete; i++ {
			s.crypt(b[i*PageSize:(i+1)*PageSize], block+uint64(i), false)
		}
		s.stats.DataRead += uint64(complete * PageSize)
		done = min(len(p), max(0, complete*PageSize-inner))
		if done != 0 {
			copy(p[:done], b[inner:inner+done])
		}
		if readErr != nil {
			return readErr
		}
		if n != len(b) {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	return done, err
}
