package restore

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"golang.org/x/sys/unix"
)

// A sparse aligned envelope is enough: restore deliberately inspects only the
// declared footer identity, not the EROFS payload.
func writeRestoreRuntime(t *testing.T, path, digest string) {
	t.Helper()
	var footer bytes.Buffer
	zw := zip.NewWriter(&footer)
	if _, err := zw.CreateHeader(&zip.FileHeader{Name: tarstream.DigestMarkerPrefix + digest, Method: zip.Store}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const size = 2 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(footer.Bytes(), int64(size-footer.Len())); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRestoreRuntimeCandidates(t *testing.T) {
	good, bad := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, kind := range []string{"missing", "unreadable", "malformed", "digest mismatch", "basename mismatch"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			primary, fallback := filepath.Join(dir, "runtime-v2.bundle"), filepath.Join(dir, "runtime-v1.bundle")
			writeRestoreRuntime(t, fallback, good)
			switch kind {
			case "unreadable":
				if os.Geteuid() == 0 {
					t.Skip("root can read mode-000 files")
				}
				writeRestoreRuntime(t, primary, good)
				if err := os.Chmod(primary, 0); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(primary, []byte("not a bundle"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "digest mismatch":
				writeRestoreRuntime(t, primary, bad)
			case "basename mismatch":
				writeRestoreRuntime(t, primary, good)
			}
			got, err := resolveRestoreRuntime(context.Background(), "file://"+primary, "file://runtime-v1.bundle@digest:"+good)
			if err != nil || got != fallback {
				t.Fatalf("selected %q, %v; want %q", got, err, fallback)
			}
		})
	}
}

func TestResolveRestoreRuntimeSingleOpen(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "primary success", false: "same failed path"}[valid], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime-v1.bundle")
			digest := strings.Repeat("a", 64)
			writeRestoreRuntime(t, path, digest)
			fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			// Include close events so consecutive open events cannot coalesce.
			if _, err := unix.InotifyAddWatch(fd, path, unix.IN_OPEN|unix.IN_CLOSE_NOWRITE); err != nil {
				t.Fatal(err)
			}
			if !valid {
				digest = strings.Repeat("b", 64)
			}
			got, err := resolveRestoreRuntime(context.Background(), "file://"+path, "file://runtime-v1.bundle@digest:"+digest)
			if valid && (err != nil || got != path) {
				t.Fatalf("primary selection = %q, %v", got, err)
			}
			if !valid && (err == nil || !strings.Contains(err.Error(), "identity mismatch") || strings.Contains(err.Error(), "fallback")) {
				t.Fatalf("same-path error = %v", err)
			}
			var events [4096]byte
			n, err := unix.Read(fd, events[:])
			if err != nil {
				t.Fatal(err)
			}
			opens := 0
			for offset := 0; offset+unix.SizeofInotifyEvent <= n; {
				mask := binary.NativeEndian.Uint32(events[offset+4:])
				if mask&unix.IN_OPEN != 0 {
					opens++
				}
				offset += unix.SizeofInotifyEvent + int(binary.NativeEndian.Uint32(events[offset+12:]))
			}
			if opens != 1 {
				t.Fatalf("opened primary %d times, want exactly once", opens)
			}
		})
	}
}

func TestResolveRestoreRuntimeRetainsBothErrors(t *testing.T) {
	for _, fallbackKind := range []string{"missing", "malformed", "mismatch"} {
		t.Run(fallbackKind, func(t *testing.T) {
			dir := t.TempDir()
			primary, fallback := filepath.Join(dir, "runtime-v2.bundle"), filepath.Join(dir, "runtime-v1.bundle")
			good, bad := strings.Repeat("a", 64), strings.Repeat("b", 64)
			writeRestoreRuntime(t, primary, bad)
			switch fallbackKind {
			case "malformed":
				if err := os.WriteFile(fallback, []byte("bad"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "mismatch":
				writeRestoreRuntime(t, fallback, bad)
			}
			_, err := resolveRestoreRuntime(context.Background(), "file://"+primary, "file://runtime-v1.bundle@digest:"+good)
			if err == nil || !strings.Contains(err.Error(), "primary "+primary) || !strings.Contains(err.Error(), "fallback "+fallback) || !strings.Contains(err.Error(), "identity mismatch") {
				t.Fatalf("lost candidate failures: %v", err)
			}
			if fallbackKind == "missing" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lost wrapped missing-file error: %v", err)
			}
		})
	}
}

func TestResolveRestoreRuntimeRelativeAndSymlinkDirectory(t *testing.T) {
	dir, targetDir := t.TempDir(), t.TempDir()
	good := strings.Repeat("a", 64)
	writeRestoreRuntime(t, filepath.Join(targetDir, "runtime-v2.bundle"), good)
	primary := filepath.Join(dir, "runtime-v2.bundle")
	if err := os.Symlink(filepath.Join(targetDir, "runtime-v2.bundle"), primary); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(dir, "runtime-v1.bundle")
	writeRestoreRuntime(t, fallback, good)
	// No runtime-v1 exists beside the symlink target. The caller's directory
	// is the authority even when the configured default is a symlink.
	t.Chdir(dir)
	got, err := resolveRestoreRuntime(context.Background(), "file://./runtime-v2.bundle", "file://runtime-v1.bundle@digest:"+good)
	if err != nil || got != fallback || !filepath.IsAbs(got) {
		t.Fatalf("symlink/relative selection = %q, %v; want %q", got, err, fallback)
	}
}

func TestResolveRestoreRuntimeRejectsInvalidInputsAndCancellation(t *testing.T) {
	digest := strings.Repeat("a", 64)
	path := filepath.Join(t.TempDir(), "runtime-v1.bundle")
	want := "file://runtime-v1.bundle@digest:" + digest
	for _, tc := range []struct{ host, required string }{
		{"file://", want},
		{"manifest://" + digest, want},
		{"file://" + path + "@digest:" + digest, want},
		{"file://runtime-v1.bundle@location:runtime", want},
		{"file://" + path, "file://../runtime-v1.bundle@digest:" + digest},
		{"file://" + path, "file://runtime-v1.bundle"},
		{"file://" + path, "file://runtime-v1.bundle@digest:invalid"},
		{"file://" + path, "file://runtime-v1.bundle@digest:" + digest + "@location:runtime"},
	} {
		_, err := resolveRestoreRuntime(context.Background(), tc.host, tc.required)
		if err == nil || strings.Contains(err.Error(), "primary") || strings.Contains(err.Error(), "fallback") {
			t.Fatalf("invalid input entered candidate lookup: %q, %q: %v", tc.host, tc.required, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := resolveRestoreRuntime(ctx, "file://"+path, want)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "primary") {
		t.Fatalf("canceled lookup = %v", err)
	}
}
