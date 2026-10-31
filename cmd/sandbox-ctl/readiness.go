package main

import (
	"fmt"
	"os"

	"github.com/kuasar-sandbox/sandboxer/pkg/readiness"
	"golang.org/x/sys/unix"
)

// newReadinessFDWriter adopts the inherited readiness descriptor. Event framing
// and closure after notification are shared with SDK integrations.
func newReadinessFDWriter(fd int, logf func(string, ...any)) (*readiness.Writer, error) {
	if fd < 3 {
		return nil, fmt.Errorf("--ready-fd must be at least 3, got %d", fd)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return nil, fmt.Errorf("--ready-fd=%d is invalid: %w", fd, err)
	}
	// Keep the descriptor out of Cloud Hypervisor and every other later exec.
	// node-ctl clears CLOEXEC only for its own exec into sandbox-ctl; ownership
	// begins here and the bit is restored immediately.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return nil, fmt.Errorf("set FD_CLOEXEC on --ready-fd=%d: %w", fd, err)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("ready-fd-%d", fd))
	if f == nil {
		return nil, fmt.Errorf("wrap --ready-fd=%d", fd)
	}
	return readiness.NewWriter(f, logf), nil
}
