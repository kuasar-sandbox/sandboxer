package chapi

import (
	"context"
	"net"
	"syscall"
	"testing"
)

func TestSocketPendingIncludesDialTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"missing", syscall.ENOENT, true},
		{"refused", syscall.ECONNREFUSED, true},
		{"dial timeout", &net.OpError{Op: "dial", Net: "unix", Err: context.DeadlineExceeded}, true},
		{"permission denied", syscall.EACCES, false},
		{"cancelled", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorsIsSocketPending(tc.err); got != tc.want {
				t.Fatalf("errorsIsSocketPending(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
