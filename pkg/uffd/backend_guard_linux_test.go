package uffd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestBackendGuardPageSetting(t *testing.T) {
	const size = 8 << 20
	for _, value := range []string{"0", "0000", "4096", "4194304", "04194304", "0x400000", "0X400000", "8384512"} {
		page, ok := backendGuardPage(value, size)
		if !ok || page%PageSize != 0 {
			t.Errorf("rejected aligned page %q", value)
		}
	}
	for _, value := range []string{"", " ", " 4096", "4096\n", "+4096", "-0", "-4096", "0x", "0b1000000000000", "0o10000", "4_096", "0x100_000", "0xg000", "1", "4095", "8388608", "18446744073709551616", strings.Repeat("0", 21)} {
		if _, ok := backendGuardPage(value, size); ok {
			t.Errorf("accepted invalid setting %q", value)
		}
	}
	for _, size := range []int{-1, 0, PageSize - 1} {
		if _, ok := backendGuardPage("0", size); ok {
			t.Errorf("accepted incomplete mapping size=%d", size)
		}
	}
}

func TestBackendGuardDisabledAndRejected(t *testing.T) {
	var output strings.Builder
	logf := func(format string, args ...any) { fmt.Fprintf(&output, format+"\n", args...) }
	t.Setenv(backendGuardEnv, "")
	g := ArmBackendPageGuardFromEnv(-1, 0, -1, logf)
	if g != nil || output.Len() != 0 || g.Close() != nil {
		t.Fatal("default-off guard must do nothing, even with invalid memory arguments")
	}
	for _, tc := range []struct {
		name, setting, reason string
		base                  uintptr
		errno                 unix.Errno
	}{
		{"invalid", "private-value\n" + strings.Repeat("x", 8192), "target", 0x100000, unix.EINVAL},
		{"unaligned", "0", "address", 0x100001, unix.EINVAL},
		{"zero-address", "0", "address", 0, unix.EINVAL},
		{"overflow", "0", "address", ^uintptr(0) &^ uintptr(PageSize-1), unix.EINVAL},
		{"bad-fd", "0", "stat", 0x100000, unix.EBADF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(backendGuardEnv, tc.setting)
			output.Reset()
			if g := ArmBackendPageGuardFromEnv(-1, tc.base, PageSize, logf); g != nil {
				_ = g.Close()
				t.Fatal("failed diagnostic was armed")
			}
			records := backendGuardRecords(t, output.String())
			if len(records) != 2 || records[0].Event != "attempt" || records[1].Event != "failed" {
				t.Fatalf("invalid failure sequence: %+v", records)
			}
			wantReason, wantErrno := tc.reason, tc.errno
			if tc.reason != "target" && os.Getpagesize() != PageSize {
				wantReason, wantErrno = "page_size", unix.EOPNOTSUPP
			}
			if records[1].Reason != wantReason || records[1].Errno != int(wantErrno) || records[1].GuardFD != -1 {
				t.Fatalf("failure=%+v, want %s/%d", records[1], wantReason, wantErrno)
			}
			if strings.Contains(output.String(), "private-value") || output.Len() >= 4096 {
				t.Fatal("failure leaked the setting or exceeded its bounded records")
			}
		})
	}
}

// Native controls run in an exec-self child, never in the test runner's mm.
// SIGBUS is deliberately fatal in the hole cases. Other cases describe this
// diagnostic's positive and negative controls, not an end-to-end VM restore.
// Set SANDBOX_UFFD_GUARD_TEST_REQUIRE=1 on a capable native host to make an
// unavailable UFFD fail the suite instead of skipping those controls.
func TestBackendGuardNative(t *testing.T) {
	for _, mode := range []string{"hole-read", "hole-write", "ch-copy", "alias-zero", "kernel-from-backend", "kernel-to-backend", "close", "register-failed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackendGuardNativeChild$")
			cmd.Env = append(os.Environ(), "SANDBOX_UFFD_GUARD_TEST_CASE="+mode, backendGuardEnv+"=4096", "GOTRACEBACK=single")
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("guard child timed out: %s", output)
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 77 {
				if os.Getenv("SANDBOX_UFFD_GUARD_TEST_REQUIRE") == "1" {
					t.Fatalf("native guard required but unavailable: %s", output)
				}
				t.Skipf("native guard unavailable: %s", output)
			}
			records := backendGuardRecords(t, string(output))
			if mode == "hole-read" || mode == "hole-write" {
				if err == nil || len(records) != 2 || records[1].Event != "armed" {
					t.Fatalf("missing expected fatal armed control: err=%v, output=%s", err, output)
				}
				address := fmt.Sprintf("addr=0x%x", records[1].TargetVA)
				if !bytes.Contains(output, []byte("SIGBUS")) || !bytes.Contains(output, []byte(address)) || !bytes.Contains(output, []byte("TestBackendGuardNativeChild")) {
					t.Fatalf("SIGBUS address and Go stack did not identify the guarded access: %s", output)
				}
				t.Logf("%s: fatal SIGBUS at guarded address with Go stack; no close record", mode)
				return
			}
			if err != nil {
				t.Fatalf("guard child failed: %v\n%s", err, output)
			}
			if mode == "register-failed" {
				if len(records) != 2 || records[1].Event != "failed" || records[1].Reason != "register" || records[1].Errno <= 0 {
					t.Fatalf("expected bounded register failure: %s", output)
				}
			} else if len(records) != 3 || records[1].Event != "armed" || records[2].Event != "closed" || records[2].Errno != 0 {
				t.Fatalf("expected successful arm/close: %s", output)
			}
			t.Logf("%s: %s", mode, strings.TrimSpace(string(output)))
		})
	}
}

func backendGuardRecords(t *testing.T, output string) []backendGuardRecord {
	t.Helper()
	var result []backendGuardRecord
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, backendGuardPrefix) {
			continue
		}
		if len(line) >= 2048 {
			t.Fatal("guard record exceeded its line limit")
		}
		var record backendGuardRecord
		decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(line, backendGuardPrefix)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record.Version != 1 || record.Sequence != len(result)+1 || record.PID <= 0 || record.Length != PageSize ||
			len(result) > 0 && record.Elapsed < result[len(result)-1].Elapsed {
			t.Fatalf("invalid guard identity or ordering: %+v", record)
		}
		result = append(result, record)
	}
	if len(result) > 3 {
		t.Fatalf("guard emitted %d records", len(result))
	}
	return result
}

func TestBackendGuardNativeChild(t *testing.T) {
	mode := os.Getenv("SANDBOX_UFFD_GUARD_TEST_CASE")
	if mode == "" {
		return
	}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		t.Fatal(err)
	}
	const size, page = 3 * PageSize, PageSize
	fd, err := unix.MemfdCreate("backend-guard-test", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, size); err != nil {
		t.Fatal(err)
	}
	backend, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(backend)
	if err := unix.Madvise(backend, unix.MADV_NOHUGEPAGE); err != nil {
		t.Fatal(err)
	}
	base := uintptr(unsafe.Pointer(&backend[0]))
	if mode == "register-failed" {
		// Below mmap_min_addr and deliberately unmapped; no address is read.
		base = PageSize
	}
	var records []backendGuardRecord
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		fmt.Fprintln(os.Stdout, line)
		var record backendGuardRecord
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, backendGuardPrefix)), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	g := ArmBackendPageGuardFromEnv(fd, base, size, logf)
	if g == nil {
		if len(records) != 2 {
			t.Fatalf("missing arm failure: %+v", records)
		}
		failed := records[1]
		if mode == "register-failed" && failed.Reason == "register" {
			if _, err := unix.FcntlInt(uintptr(failed.GuardFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
				t.Fatalf("failed registration leaked fd=%d: %v", failed.GuardFD, err)
			}
			return
		}
		if failed.Reason == "page_size" || failed.Reason == "create" || failed.Reason == "api" || failed.Reason == "features" {
			os.Exit(77)
		}
		t.Fatalf("unexpected arm failure: %+v", failed)
	}
	defer g.Close()
	flags, err := unix.FcntlInt(uintptr(g.fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("guard must have CLOEXEC: flags=%d, err=%v", flags, err)
	}
	if _, err := unix.Seek(fd, page, unix.SEEK_DATA); !errors.Is(err, unix.ENXIO) {
		t.Fatalf("arming touched the hole: SEEK_DATA err=%v", err)
	}
	switch mode {
	case "hole-read":
		fmt.Fprintf(os.Stdout, "unexpected backend read: %d\n", backend[page])
		t.Fatal("backend hole read did not raise SIGBUS")
	case "hole-write":
		backend[page] = 0xa5
		t.Fatal("backend hole write did not raise SIGBUS")
	case "ch-copy", "alias-zero":
		alias, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Munmap(alias)
		if err := unix.Madvise(alias, unix.MADV_NOHUGEPAGE); err != nil {
			t.Fatal(err)
		}
		if mode == "ch-copy" {
			// An independent UFFD on the CH-style alias installs the same
			// shared backing page. There is no guard reader or CH process.
			chFD, err := createUffd(unix.O_CLOEXEC | unix.O_NONBLOCK | backendGuardUserModeOnly)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(chFD)
			if _, err := ioctlUffdAPI(chFD, uffdFeatureMissingShmem); err != nil {
				t.Fatal(err)
			}
			chVA := uint64(uintptr(unsafe.Pointer(&alias[page])))
			if err := ioctlUffdRegister(chFD, chVA, PageSize); err != nil {
				t.Fatal(err)
			}
			source, err := unix.Mmap(-1, 0, PageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Munmap(source)
			for i := range source {
				source[i] = 0xff
			}
			if n, err := ioctlUffdCopy(chFD, chVA, uint64(uintptr(unsafe.Pointer(&source[0]))), PageSize); err != nil || n != PageSize {
				t.Fatalf("independent CH copy=%d, err=%v", n, err)
			}
			if !bytes.Equal(backend[page:page+PageSize], source) {
				t.Fatal("backend did not observe the installed FF page")
			}
		} else {
			alias[page] = 0
			if backend[page] != 0 {
				t.Fatal("backend did not observe the alias-created zero page")
			}
		}
	case "kernel-from-backend", "kernel-to-backend":
		var pipe [2]int
		if err := unix.Pipe2(pipe[:], unix.O_CLOEXEC); err != nil {
			t.Fatal(err)
		}
		defer unix.Close(pipe[0])
		defer unix.Close(pipe[1])
		var n int
		if mode == "kernel-from-backend" {
			n, err = unix.Write(pipe[1], backend[page:page+1])
		} else {
			if _, err := unix.Write(pipe[1], []byte{0xff}); err != nil {
				t.Fatal(err)
			}
			n, err = unix.Read(pipe[0], backend[page:page+1])
		}
		if !errors.Is(err, unix.EFAULT) || n > 0 {
			t.Fatalf("kernel uaccess must fail without Go SIGBUS: n=%d err=%v", n, err)
		}
		if _, err := unix.Seek(fd, page, unix.SEEK_DATA); !errors.Is(err, unix.ENXIO) {
			t.Fatalf("kernel uaccess unexpectedly populated the guarded page: %v", err)
		}
		fmt.Fprintf(os.Stdout, "kernel-uaccess errno=%d\n", unix.EFAULT)
	case "close":
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if err := g.Close(); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if _, err := unix.FcntlInt(uintptr(g.fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatalf("Close left its descriptor open: %v", err)
		}
		if backend[page] != 0 {
			t.Fatal("Close did not restore ordinary missing-page behavior")
		}
	default:
		t.Fatalf("unknown native control %q", mode)
	}
}
