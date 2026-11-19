package uffd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"golang.org/x/sys/unix"
)

const backendMemoryGUPChildEnv = "SANDBOX_BACKEND_MEMORY_GUP_CHILD"

type backendMemoryGUPReady struct {
	PID int
	VA  uint64
}
type backendMemoryGUPResult struct {
	Value       byte
	GuardClosed bool
}

// The parent reads only its own child and a fresh, owned 16 KiB memfd. The CH
// alias is independently MISSING-registered. No external process is inspected.
func TestBackendMemoryGuardNativeRemoteGUP(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprintf("guarded_%t", guarded), func(t *testing.T) {
			m, err := memory.Create("backend-gup-owned", 4*PageSize)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			chFD, err := createBackendGuardUFFD(createUffd)
			if err != nil {
				backendMemoryNativeError(t, err)
			}
			defer unix.Close(chFD)
			if _, err = ioctlUffdAPI(chFD, uffdFeatureMissingShmem); err != nil {
				backendMemoryNativeError(t, err)
			}
			if err = ioctlUffdRegister(chFD, uint64(m.Addr()), uint64(m.Size())); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			mode := "plain"
			if guarded {
				mode = "guarded"
			}
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackendMemoryGuardRemoteGUPChild$")
			cmd.Env = append(os.Environ(), backendMemoryGUPChildEnv+"="+mode)
			cmd.ExtraFiles = []*os.File{m.File()}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); _ = stdin.Close(); _ = cmd.Wait() }()
			decoder := json.NewDecoder(stdout)
			var ready backendMemoryGUPReady
			if err = decoder.Decode(&ready); err != nil {
				t.Fatalf("child readiness: %v", err)
			}
			if ready.PID != cmd.Process.Pid || ready.VA == 0 {
				t.Fatalf("unexpected child identity %+v", ready)
			}
			if off, err := unix.Seek(m.FD(), PageSize, unix.SEEK_DATA); off != -1 || !errors.Is(err, unix.ENXIO) {
				t.Fatalf("initial target not a hole: off=%d err=%v", off, err)
			}
			value := [1]byte{0xa5}
			n, readErr := unix.ProcessVMReadv(cmd.Process.Pid, []unix.Iovec{{Base: &value[0], Len: 1}}, []unix.RemoteIovec{{Base: uintptr(ready.VA) + PageSize, Len: 1}}, 0)
			off, seekErr := unix.Seek(m.FD(), PageSize, unix.SEEK_DATA)
			if guarded {
				if n > 0 || !errors.Is(readErr, unix.EFAULT) || value[0] != 0xa5 || off != -1 || !errors.Is(seekErr, unix.ENXIO) {
					t.Fatalf("guard did not preserve hole: read=%d/%v value=%x seek=%d/%v", n, readErr, value[0], off, seekErr)
				}
			} else if n != 1 || readErr != nil || value[0] != 0 || off != PageSize || seekErr != nil {
				t.Fatalf("unguarded regression not reproduced: read=%d/%v value=%x seek=%d/%v", n, readErr, value[0], off, seekErr)
			}
			source := bytes.Repeat([]byte{0xff}, PageSize)
			copied, copyErr := realUffdOps.copy(chFD, uint64(m.Addr())+PageSize, source)
			if guarded {
				if copied != PageSize || copyErr != nil {
					t.Fatalf("guarded CH COPY=%d/%v", copied, copyErr)
				}
			} else if copied != 0 || !errors.Is(copyErr, unix.EEXIST) {
				t.Fatalf("unguarded CH COPY=%d/%v", copied, copyErr)
			}
			if _, err = fmt.Fprintln(stdin, "R"); err != nil {
				t.Fatal(err)
			}
			var result backendMemoryGUPResult
			if err = decoder.Decode(&result); err != nil {
				t.Fatalf("child result: %v", err)
			}
			want := byte(0)
			if guarded {
				want = 0xff
			}
			if result.Value != want || !result.GuardClosed {
				t.Fatalf("bad backend observation/cleanup %+v", result)
			}
			if err = cmd.Wait(); err != nil {
				t.Fatalf("child exit: %v stderr=%s", err, stderr.String())
			}
			t.Logf("guarded=%t remote_read=%d/%v seek_data=%d/%v CH_COPY=%d/%v backend=%02x", guarded, n, readErr, off, seekErr, copied, copyErr, result.Value)
		})
	}
}

func TestBackendMemoryGuardRemoteGUPChild(t *testing.T) {
	mode := os.Getenv(backendMemoryGUPChildEnv)
	if mode == "" {
		return
	}
	if mode != "plain" && mode != "guarded" {
		t.Fatal("invalid owned-child mode")
	}
	const size = 4 * PageSize
	fd := 3 // The parent passes only its own memfd through ExtraFiles.
	backend, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	var guard *BackendMemoryGuard
	defer func() {
		if backend != nil {
			_ = unix.Munmap(backend)
		}
		_ = guard.Close()
	}()
	if err = unix.Madvise(backend, unix.MADV_NOHUGEPAGE); err != nil {
		t.Fatal(err)
	}
	va := uintptr(unsafe.Pointer(&backend[0]))
	if mode == "guarded" {
		guard, err = NewBackendMemoryGuard(fd, va, size)
		if err != nil {
			t.Fatal(err)
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	if err = encoder.Encode(backendMemoryGUPReady{PID: os.Getpid(), VA: uint64(va)}); err != nil {
		t.Fatal(err)
	}
	command, err := bufio.NewReader(os.Stdin).ReadByte()
	if err != nil || command != 'R' {
		t.Fatalf("child command %q err=%v", command, err)
	}
	result := backendMemoryGUPResult{Value: backend[PageSize], GuardClosed: true}
	if err = unix.Munmap(backend); err != nil {
		t.Fatal(err)
	}
	backend = nil
	if guard != nil {
		guardFD := guard.fd
		if err = guard.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = unix.FcntlInt(uintptr(guardFD), unix.F_GETFD, 0)
		result.GuardClosed = errors.Is(err, unix.EBADF)
	}
	if err = encoder.Encode(result); err != nil {
		t.Fatal(err)
	}
}
