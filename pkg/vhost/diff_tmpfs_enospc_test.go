package vhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// The source E2E root runner enters a new mount namespace before enabling this
// test. Ordinary unit-test invocations do not unexpectedly elevate privileges.
// Nothing mounts, remounts or fills a shared /dev/shm. The file store is 32 KiB.
func validateTmpfsENOSPCLaunch(enabled, parentNS, currentNS string, euid int) (bool, error) {
	if enabled == "" {
		return false, nil
	}
	if enabled != "1" {
		return true, errors.New("invalid privileged tmpfs test activation")
	}
	if euid != 0 {
		return true, errors.New("privileged tmpfs test requires root")
	}
	if parentNS == "" {
		return true, errors.New("privileged tmpfs test requires the parent mount namespace")
	}
	if currentNS == "" || currentNS == parentNS {
		return true, errors.New("refusing to mount without a separate mount namespace")
	}
	return true, nil
}

func TestTmpfsENOSPCLaunchContract(t *testing.T) {
	for _, tc := range []struct {
		name, enabled, parent, current string
		euid                           int
		wantRun, wantErr               bool
	}{
		{name: "ordinary unit test", current: "mnt:[2]"},
		{name: "invalid activation", enabled: "0", parent: "mnt:[1]", current: "mnt:[2]", wantRun: true, wantErr: true},
		{name: "missing current", enabled: "1", parent: "mnt:[1]", wantRun: true, wantErr: true},
		{name: "non-root", enabled: "1", parent: "mnt:[1]", current: "mnt:[2]", euid: 1000, wantRun: true, wantErr: true},
		{name: "missing parent", enabled: "1", current: "mnt:[2]", wantRun: true, wantErr: true},
		{name: "shared namespace", enabled: "1", parent: "mnt:[1]", current: "mnt:[1]", wantRun: true, wantErr: true},
		{name: "isolated root", enabled: "1", parent: "mnt:[1]", current: "mnt:[2]", wantRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, err := validateTmpfsENOSPCLaunch(tc.enabled, tc.parent, tc.current, tc.euid)
			if run != tc.wantRun || (err != nil) != tc.wantErr {
				t.Fatalf("launch validation = %t, %v; want run=%t error=%t", run, err, tc.wantRun, tc.wantErr)
			}
		})
	}
}

func TestDiffTmpfsENOSPC(t *testing.T) {
	const runEnv = "SANDBOXER_TMPFS_ENOSPC_ROOT"
	const namespaceEnv = "SANDBOXER_TMPFS_TEST_PARENT_MNT"
	currentNS, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	run, err := validateTmpfsENOSPCLaunch(os.Getenv(runEnv), os.Getenv(namespaceEnv), currentNS, os.Geteuid())
	if !run {
		t.Skip("requires the explicit privileged tmpfs test runner")
	}
	if err != nil {
		t.Fatalf("%s/%s launch contract: %v", runEnv, namespaceEnv, err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			dir := t.TempDir()
			if err := unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "size=32k,nr_inodes=16,mode=0700"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := unix.Unmount(dir, 0); err != nil {
					t.Error(err)
				}
			}()
			cleanup := newWorkerGate()
			defer cleanup.open()
			waiting := make(chan struct{}, 8)
			cache := testCache(t, 2, 1, cacheHooks{beforeCleanup: func() error { cleanup.wait(); return nil }, waiting: func() {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}})
			options := []BlockCOWOption{WithCOWCache(cache)}
			if encrypted {
				options = append(options, WithDiffEncryption(testDiffKey(21), true))
			}
			cow, err := OpenBlockCOW(filepath.Join(dir, "active.diff"), nil, DiffInit{CreateSize: 16 * cowBlockSize}, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { cleanup.open(); _ = cow.Close() }()
			checkTmpfsActiveIO(t, cow.diff)
			notified := make(chan error, 1)
			cow.SetFatalHandler(func(err error) { notified <- err })
			failed := false
			for block := 0; block < 16; block++ {
				writePage(t, cow, block, byte(block+1))
				err := cow.Drain(context.Background())
				if errors.Is(err, unix.ENOSPC) {
					failed = true
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if !failed {
				t.Fatal("bounded tmpfs never returned ENOSPC")
			}
			awaitSignal(t, cleanup.entered)
			if err := awaitError(t, notified); !errors.Is(err, unix.ENOSPC) {
				t.Fatalf("owner failure: %v", err)
			}
			if _, err := cow.WriteAt([]byte{1}, 15*cowBlockSize); !errors.Is(err, unix.ENOSPC) {
				t.Fatalf("write after fatal: %v", err)
			}
			if err := cow.Flush(); !errors.Is(err, unix.ENOSPC) {
				t.Fatalf("flush after fatal: %v", err)
			}
			s := cache.Stats()
			if s.DirtyUsed != cowBlockSize || s.Writeback != cowBlockSize || s.Used > s.Capacity || s.DirtyUsed > s.MaxDirty {
				t.Fatalf("failure quota %+v", s)
			}
			cache.mu.Lock()
			active := cache.active
			cache.mu.Unlock()
			if active != cow {
				t.Fatal("I/O owner released before failed-write cleanup")
			}
			// Drain's earlier quota notifications are irrelevant to Close.
			for len(waiting) > 0 {
				<-waiting
			}
			closed := make(chan error, 1)
			go func() { closed <- cow.Close() }()
			awaitSignal(t, waiting)
			select {
			case err := <-closed:
				t.Fatalf("Close finished during cleanup: %v", err)
			default:
			}
			if _, err := cow.diff.f.Stat(); err != nil {
				t.Fatalf("fd closed during cleanup: %v", err)
			}
			cleanup.open()
			if err := awaitError(t, closed); !errors.Is(err, unix.ENOSPC) {
				t.Fatalf("Close failure: %v", err)
			}
			if err := cache.Close(); !errors.Is(err, unix.ENOSPC) {
				t.Fatalf("cache failure: %v", err)
			}
			checkBudget(t, cache, 0, 0, 0)
		})
	}
}
