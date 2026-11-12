package directio

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestRetryInterruptedIOPreservesProgressAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		err  error
	}{
		{"full", 4096, nil},
		{"short", 512, nil},
		{"partial with error", 512, unix.EINTR},
		{"I/O error", -1, unix.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := make([]byte, 4096)
			const off = int64(8192)
			calls := 0
			n, err := RetryInterruptedIO(func(fd int, b []byte, offset int64) (int, error) {
				calls++
				if fd != 7 || len(b) != len(p) || &b[0] != &p[0] || offset != off {
					t.Fatal("syscall arguments changed on retry")
				}
				if calls == 1 {
					return -1, unix.EINTR
				}
				if calls > 2 {
					t.Fatal("retried after progress or a terminal result")
				}
				return tc.n, tc.err
			}, 7, p, off)
			if calls != 2 || n != tc.n || err != tc.err {
				t.Fatalf("calls=%d, got (%d, %v), want (%d, %v)", calls, n, err, tc.n, tc.err)
			}
		})
	}
}
