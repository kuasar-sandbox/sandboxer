package directio

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// BenchmarkIOPriorityDirectRead is opt-in and read-only. The selected file must
// already contain at least 4 KiB on a filesystem supporting O_DIRECT. It never
// creates a test file, changes a scheduler or drops the host's page cache.
func BenchmarkIOPriorityDirectRead(b *testing.B) {
	path := os.Getenv("SANDBOXER_DIO_BENCH_FILE")
	if path == "" {
		b.Skip("set SANDBOXER_DIO_BENCH_FILE to an isolated existing data file")
	}
	for _, mode := range []struct {
		name     string
		priority bool
		pinned   bool
		already  bool
	}{
		{"plain_pinned", false, true, false},
		{"priority_pinned", true, true, false},
		{"priority_unpinned", true, false, false},
		{"already_BE0_pinned", true, true, true},
	} {
		b.Run(mode.name, func(b *testing.B) {
			if mode.pinned {
				defer callerAtDefault(b)()
			}
			if mode.already {
				if err := setThreadPriority(ioPriorityBE0); err != nil {
					b.Fatal(err)
				}
			}
			fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECT, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer unix.Close(fd)
			var info unix.Stat_t
			if err := unix.Fstat(fd, &info); err != nil || info.Size < 4096 {
				b.Fatalf("benchmark file must contain at least 4096 bytes: size=%d err=%v", info.Size, err)
			}
			buffer, err := New(4096, 4096, 4096)
			if err != nil {
				b.Fatal(err)
			}
			defer buffer.Close()
			var sequence uint64 = 1
			b.ReportAllocs()
			b.SetBytes(4096)
			for b.Loop() {
				sequence = sequence*6364136223846793005 + 1
				offset := int64(sequence%uint64(info.Size/4096)) * 4096
				var n int
				if mode.priority {
					n, err = RetryInterruptedIO(unix.Pread, fd, buffer.Bytes, offset)
				} else {
					for {
						n, err = unix.Pread(fd, buffer.Bytes, offset)
						if n > 0 || err != unix.EINTR {
							break
						}
					}
				}
				if err != nil || n != len(buffer.Bytes) {
					b.Fatalf("read: %d of %d bytes: %v", n, len(buffer.Bytes), err)
				}
			}
		})
	}
}
