package snapshot

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSparseCopyShortPwrite(t *testing.T) {
	// The limit and SIGXFSZ disposition belong to the child process alone.
	const child = "SANDBOXER_TEST_SPARSE_SHORT_PWRITE"
	if os.Getenv(child) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSparseCopyShortPwrite$")
		cmd.Env = append(os.Environ(), child+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("short pwrite: %v\n%s", err, out)
		}
		return
	}

	fd, err := unix.MemfdCreate("sparse-copy-source", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	src := os.NewFile(uintptr(fd), "sparse-copy-source")
	defer src.Close()
	const size = 4096
	if _, err := src.Write(bytes.Repeat([]byte{'x'}, size)); err != nil {
		t.Fatal(err)
	}
	dst, err := os.CreateTemp(t.TempDir(), "dst")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	// SparseCopy's initial truncate can succeed, but the first fallback
	// pwrite can only copy half the data. The next write returns EFBIG.
	if err := dst.Truncate(size); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(unix.SIGXFSZ)
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	limit.Cur = size / 2
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	n, err := SparseCopy(dst, int(src.Fd()), size)
	if n != size/2 || !errors.Is(err, unix.EFBIG) {
		t.Fatalf("SparseCopy = (%d, %v), want (%d, EFBIG)", n, err, size/2)
	}
}
