package uffd

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNormalizeUffdCompletion(t *testing.T) {
	for _, tt := range []struct {
		name          string
		completed     int64
		errno         unix.Errno
		wantCompleted int64
		wantErr       error
	}{
		{name: "complete", completed: PageSize, wantCompleted: PageSize},
		{name: "partial", completed: PageSize, errno: unix.EAGAIN, wantCompleted: PageSize, wantErr: unix.EAGAIN},
		{name: "negative field and errno", completed: -int64(unix.EEXIST), errno: unix.EEXIST, wantErr: unix.EEXIST},
		{name: "negative field only", completed: -int64(unix.ENOENT), wantErr: unix.ENOENT},
		{name: "syscall errno is authoritative", completed: -int64(unix.EEXIST), errno: unix.EAGAIN, wantErr: unix.EAGAIN},
	} {
		t.Run(tt.name, func(t *testing.T) {
			completed, err := normalizeUffdCompletion(tt.completed, tt.errno)
			if completed != tt.wantCompleted {
				t.Fatalf("completed = %d, want %d", completed, tt.wantCompleted)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
