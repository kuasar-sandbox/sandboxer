package uffd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"golang.org/x/sys/unix"
)

const backendMemorySIGBUSChildEnv = "SANDBOX_BACKEND_MEMORY_SIGBUS_CHILD"
const backendMemorySIGBUSReadyPrefix = "backend-memory-sigbus-ready "

type backendMemorySIGBUSReady struct {
	PID    int
	Base   uint64
	Size   int
	Target uint64
}

func TestBackendMemoryGuardNativeUserReadSIGBUS(t *testing.T) {
	backendMemoryUserSIGBUS(t, "read", 0)
}

func TestBackendMemoryGuardNativeUserWriteSIGBUS(t *testing.T) {
	backendMemoryUserSIGBUS(t, "write", 3*PageSize)
}

// USER_MODE_ONLY alone can reject remote GUP. These independent user-access
// controls exercise the production SIGBUS feature: without it, the child would
// block waiting for a guard reader that intentionally does not exist. Use the
// first and last pages to cover both ends of the complete backend registration.
func backendMemoryUserSIGBUS(t *testing.T, mode string, offset uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackendMemoryGuardSIGBUSChild$")
	cmd.Env = append(os.Environ(), backendMemorySIGBUSChildEnv+"="+mode, "GOTRACEBACK=single")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("backend access blocked instead of raising SIGBUS: %s", output)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 77 {
		if os.Getenv("SANDBOX_UFFD_GUARD_TEST_REQUIRE") == "1" {
			t.Fatalf("native backend guard required but unavailable: %s", output)
		}
		t.Skipf("native backend guard unavailable: %s", output)
	}
	var ready backendMemorySIGBUSReady
	count := 0
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, backendMemorySIGBUSReadyPrefix) {
			count++
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, backendMemorySIGBUSReadyPrefix)), &ready); err != nil {
				t.Fatalf("invalid child readiness: %v", err)
			}
		}
	}
	if !errors.As(err, &exitErr) || count != 1 || ready.PID != cmd.Process.Pid || ready.Base == 0 ||
		ready.Size != 4*PageSize || ready.Target != ready.Base+offset {
		t.Fatalf("missing fatal production-guard control: err=%v ready=%+v output=%s", err, ready, output)
	}
	address := fmt.Sprintf("addr=0x%x", ready.Target)
	var signalLine string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "[signal SIGBUS:") {
			for _, field := range strings.Fields(line) {
				if strings.HasPrefix(field, "addr=") {
					faultVA, parseErr := strconv.ParseUint(strings.TrimPrefix(field, "addr="), 0, 64)
					if parseErr == nil && faultVA == ready.Target {
						signalLine = line
					}
				}
			}
		}
	}
	if signalLine == "" || !strings.Contains(string(output), "TestBackendMemoryGuardSIGBUSChild(") {
		t.Fatalf("fatal signal/address/stack did not identify the guarded user access: %s", output)
	}
	t.Log(signalLine)
	t.Logf("%s: production guard raised fatal SIGBUS at %s (offset=%d); child reaped", mode, address, offset)
}

func TestBackendMemoryGuardSIGBUSChild(t *testing.T) {
	mode := os.Getenv(backendMemorySIGBUSChildEnv)
	if mode == "" {
		return
	}
	if mode != "read" && mode != "write" {
		t.Fatal("invalid SIGBUS child mode")
	}
	// The intended fatal signal must not create a core file. All mappings and
	// descriptors are owned by this child and reclaimed when the parent reaps it.
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		t.Fatal(err)
	}
	m, err := memory.Create("backend-guard-user-access", 4*PageSize)
	if err != nil {
		t.Fatal(err)
	}
	var guard *BackendMemoryGuard
	defer func() { _ = m.Close(); _ = guard.Close() }()
	guard, err = NewBackendMemoryGuard(m.FD(), m.Addr(), m.Size())
	if err != nil {
		for _, unsupported := range []error{unix.EPERM, unix.EACCES, unix.ENOSYS, unix.EINVAL, unix.EOPNOTSUPP} {
			if errors.Is(err, unsupported) {
				fmt.Fprintf(os.Stderr, "native backend guard unavailable: %v\n", err)
				os.Exit(77)
			}
		}
		t.Fatal(err)
	}
	if flags, err := unix.FcntlInt(uintptr(guard.fd), unix.F_GETFD, 0); err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("guard must have CLOEXEC: flags=%d err=%v", flags, err)
	}
	if off, err := unix.Seek(m.FD(), 0, unix.SEEK_DATA); off != -1 || !errors.Is(err, unix.ENXIO) {
		t.Fatalf("guarded mapping is not entirely holes: off=%d err=%v", off, err)
	}
	offset := 0
	if mode == "write" {
		offset = 3 * PageSize
	}
	ready := backendMemorySIGBUSReady{os.Getpid(), uint64(m.Addr()), m.Size(), uint64(m.Addr()) + uint64(offset)}
	body, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(os.Stdout, "%s%s\n", backendMemorySIGBUSReadyPrefix, body)
	if mode == "read" {
		fmt.Fprintf(os.Stdout, "unexpected backend read: %d\n", m.Bytes()[offset])
	} else {
		m.Bytes()[offset] = 0xa5
	}
	t.Fatal("backend hole access did not raise SIGBUS")
}
