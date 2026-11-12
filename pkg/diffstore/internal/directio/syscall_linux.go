package directio

import "golang.org/x/sys/unix"

// RetryInterruptedIO repeats a positional I/O syscall at the original offset
// only if a signal interrupted it before any bytes transferred. A short
// transfer must reach the caller intact: its remainder may be unaligned.
func RetryInterruptedIO(op func(int, []byte, int64) (int, error), fd int, p []byte, off int64) (int, error) {
	for {
		n, err := op(fd, p, off)
		if n > 0 || err != unix.EINTR {
			return n, err
		}
	}
}

// Reserve allocates the range without changing the file size.
func Reserve(fd int, off, length int64) error {
	return fallocate(fd, unix.FALLOC_FL_KEEP_SIZE, off, length)
}

// PunchHole releases the range without changing the file size.
func PunchHole(fd int, off, length int64) error {
	return fallocate(fd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, length)
}

// Both operations are idempotent over the same byte range.
func fallocate(fd int, mode uint32, off, length int64) error {
	for {
		err := unix.Fallocate(fd, mode, off, length)
		if err != unix.EINTR {
			return err
		}
	}
}
