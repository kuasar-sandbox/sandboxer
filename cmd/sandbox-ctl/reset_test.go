package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Exercise the real command and signal.NotifyContext in a separate process.
// A FIFO holds metadata I/O after commandContext is installed. Releasing it
// after the signal models an interrupted SDK call returning to the CLI.
func TestResetCommandSignalIsResumable(t *testing.T) {
	if directory := os.Getenv("RESET_SIGNAL_CHILD"); directory != "" {
		os.Exit(resetCmd([]string{"--checkpoint-dir", directory, "--sandbox-id", "sid",
			"--instance-id", "instance", "--run-id", "run", "--operation-id", "operation"}))
	}
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "current-disks.json")
			if err := unix.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResetCommandSignalIsResumable$")
			cmd.Env = append(os.Environ(), "RESET_SIGNAL_CHILD="+directory)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill() }()
			// Nonblocking writer open succeeds only after Reset is reading metadata.
			var writer *os.File
			for writer == nil {
				fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0)
				if err == nil {
					writer = os.NewFile(uintptr(fd), path)
					break
				}
				if !errors.Is(err, unix.ENXIO) || ctx.Err() != nil {
					t.Fatalf("wait for metadata reader: %v / %v", err, ctx.Err())
				}
				time.Sleep(time.Millisecond)
			}
			defer writer.Close()
			if err := cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			// Allow Go's asynchronous signal delivery while the SDK stays blocked.
			time.Sleep(100 * time.Millisecond)
			if _, err := writer.WriteString("{}"); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			var exited *exec.ExitError
			if err := cmd.Wait(); !errors.As(err, &exited) || exited.ExitCode() != 75 {
				t.Fatalf("interrupted Reset = %v; want EX_TEMPFAIL 75", err)
			}
		})
	}
}
