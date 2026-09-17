package vhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Only the child mounts tmpfs, after entering new user and mount namespaces.
// Nothing mounts, remounts or fills a shared /dev/shm. The file store is 32 KiB.
func TestDiffTmpfsENOSPC(t *testing.T) {
	const namespaceEnv = "SANDBOXER_TMPFS_TEST_PARENT_MNT"
	parentNS := os.Getenv(namespaceEnv)
	if parentNS == "" {
		ns, err := os.Readlink("/proc/self/ns/mnt")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDiffTmpfsENOSPC$", "-test.v")
		cmd.Env = append(os.Environ(), namespaceEnv+"="+ns)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNS,
			UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
			GidMappingsEnableSetgroups: false,
		}
		out, err := cmd.CombinedOutput()
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
			t.Skipf("isolated user/mount namespaces unavailable: %v", err)
		}
		if err != nil {
			t.Fatalf("isolated tmpfs child: %v\n%s", err, out)
		}
		t.Logf("isolated tmpfs child:\n%s", out)
		return
	}
	ns, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	if ns == parentNS {
		t.Fatal("refusing to mount without a separate mount namespace")
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
