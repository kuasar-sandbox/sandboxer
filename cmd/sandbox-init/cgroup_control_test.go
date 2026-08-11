package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCgroupEnableAdvertisedControllersVerifiesReadback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.controllers"), []byte("cpu memory io"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cgroupEnableAdvertisedControllers(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "+cpu +memory +io" {
		t.Fatalf("controller write = %q", got)
	}
	if missing := missingControllers([]string{"cpu", "memory", "io"}, []string{"memory", "cpu"}); len(missing) != 1 || missing[0] != "io" {
		t.Fatalf("missingControllers = %v, want [io]", missing)
	}
}

func TestCgroupControllerEnableFailureIsFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.controllers"), []byte("cpu memory"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory at the write target injects a deterministic write failure,
	// proving strict delegation returns the error instead of degrading.
	if err := os.Mkdir(filepath.Join(dir, "cgroup.subtree_control"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cgroupEnableAdvertisedControllers(dir); err == nil || !strings.Contains(err.Error(), "cgroup.subtree_control") {
		t.Fatalf("controller failure = %v, want fatal subtree_control error", err)
	}

	readCount := 0
	err := cgroupEnableAdvertisedControllersWith("/injected", func(path string) ([]byte, error) {
		readCount++
		if readCount == 1 {
			return []byte("cpu memory io"), nil
		}
		return []byte("cpu memory"), nil
	}, func(path string, body []byte, mode os.FileMode) error {
		if path != "/injected/cgroup.subtree_control" || string(body) != "+cpu +memory +io" {
			t.Fatalf("controller write = %q %q", path, body)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "controllers not enabled: io") {
		t.Fatalf("controller readback failure = %v, want missing io", err)
	}
}

func TestCgroupConfigureCloneUsesRootThenFinalTarget(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "root")
	targetPath := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(targetPath, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := openCgroupDir(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	target, err := openCgroupDir(targetPath)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	nsFD, err := unix.Open("/proc/self/ns/cgroup", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = root.Close()
		_ = target.Close()
		t.Fatal(err)
	}
	ns := os.NewFile(uintptr(nsFD), "test-cgroup-namespace")

	applicationCgroups.Lock()
	oldControl := applicationCgroups.control
	oldRoot := applicationCgroups.root
	oldTarget := applicationCgroups.target
	oldNamespace := applicationCgroups.namespace
	applicationCgroups.control = true
	applicationCgroups.root = root
	applicationCgroups.target = target
	applicationCgroups.namespace = nil
	applicationCgroups.Unlock()
	t.Cleanup(func() {
		applicationCgroups.Lock()
		applicationCgroups.control = oldControl
		applicationCgroups.root = oldRoot
		applicationCgroups.target = oldTarget
		applicationCgroups.namespace = oldNamespace
		applicationCgroups.Unlock()
		_ = root.Close()
		_ = target.Close()
		_ = ns.Close()
	})

	first := &syscall.SysProcAttr{Unshareflags: unix.CLONE_FS}
	if err := cgroupConfigureClone(first, true); err != nil {
		t.Fatal(err)
	}
	if !first.UseCgroupFD || first.CgroupFD != int(root.Fd()) {
		t.Fatalf("first clone cgroup = enabled:%t fd:%d, want root fd %d", first.UseCgroupFD, first.CgroupFD, root.Fd())
	}
	if first.Unshareflags != unix.CLONE_FS|unix.CLONE_NEWCGROUP {
		t.Fatalf("first clone unshare flags = %#x, want CLONE_FS|CLONE_NEWCGROUP", first.Unshareflags)
	}

	if err := cgroupConfigureClone(&syscall.SysProcAttr{}, false); err == nil {
		t.Fatal("later clone without pinned namespace succeeded")
	}
	applicationCgroups.Lock()
	applicationCgroups.namespace = ns
	applicationCgroups.Unlock()
	later := &syscall.SysProcAttr{}
	if err := cgroupConfigureClone(later, false); err != nil {
		t.Fatal(err)
	}
	if !later.UseCgroupFD || later.CgroupFD != int(target.Fd()) {
		t.Fatalf("later clone cgroup = enabled:%t fd:%d, want target fd %d", later.UseCgroupFD, later.CgroupFD, target.Fd())
	}
	if later.Unshareflags != 0 {
		t.Fatalf("later clone unexpectedly unshares cgroup namespace: %#x", later.Unshareflags)
	}
}

func TestCgroupNamespaceMountAndMoveFailureInjection(t *testing.T) {
	injected := errors.New("injected")
	if err := cgroupJoinNamespaceWith(9, func(fd, nstype int) error {
		if fd != 9 || nstype != unix.CLONE_NEWCGROUP {
			t.Fatalf("setns args = (%d,%#x)", fd, nstype)
		}
		return injected
	}); !errors.Is(err, injected) {
		t.Fatalf("setns injection = %v", err)
	}

	mountCalled := false
	err := cgroupMountScopedWith(func(path string, flags int) error {
		return injected
	}, func(source, target, fstype string, flags uintptr, data string) error {
		mountCalled = true
		return nil
	})
	if !errors.Is(err, injected) || mountCalled {
		t.Fatalf("unmount injection err=%v mountCalled=%t", err, mountCalled)
	}
	err = cgroupMountScopedWith(func(string, int) error { return nil }, func(source, target, fstype string, flags uintptr, data string) error {
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("mount injection = %v", err)
	}

	err = cgroupMoveBootstrapToInitWith(func(path string, data []byte, mode os.FileMode) error {
		if path != scopedInitCgroupProcs || string(data) != "0" {
			t.Fatalf("self-move write = %q %q", path, data)
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("self-move injection = %v", err)
	}
}

func TestChildHandshakeFailureIsReportedBeforeExit(t *testing.T) {
	if os.Getenv("SANDBOX_INIT_HANDSHAKE_HELPER") == "fail" {
		sync, err := childSyncFromFD(childBootstrapSyncFD)
		if err != nil {
			os.Exit(90)
		}
		if err := sync.waitFor(childMsgStart); err != nil {
			os.Exit(91)
		}
		sync.fail(errors.New("injected child setup failure"))
	}

	parent, child, err := newChildSyncPair()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildHandshakeFailureIsReportedBeforeExit$")
	cmd.Env = append(os.Environ(), "SANDBOX_INIT_HANDSHAKE_HELPER=fail")
	cmd.ExtraFiles = []*os.File{child}
	failureMarked := false
	pid, err := coordinateChild(cmd, parent, child, func(int) error { return nil }, nil, func(int) {
		failureMarked = true
	}, childSyncExecEOF)
	if pid <= 0 {
		t.Fatalf("helper pid = %d", pid)
	}
	if err == nil || !strings.Contains(err.Error(), "injected child setup failure") {
		t.Fatalf("coordinateChild error = %v", err)
	}
	if !failureMarked {
		t.Fatal("parent did not mark the launch failure before acknowledging it")
	}
	if waitErr := cmd.Wait(); waitErr == nil {
		t.Fatal("failed helper exited successfully")
	}
}

func TestChildHandshakeParentFailureAbortsFinalExecGate(t *testing.T) {
	if os.Getenv("SANDBOX_INIT_HANDSHAKE_HELPER") == "ready" {
		sync, err := childSyncFromFD(childBootstrapSyncFD)
		if err != nil {
			os.Exit(90)
		}
		if err := sync.waitFor(childMsgStart); err != nil {
			os.Exit(91)
		}
		if err := sync.readyAndWait(); err == nil {
			// Receiving "go" would mean user code could run despite the
			// injected parent-side namespace/controller failure.
			os.Exit(92)
		}
		sync.close()
		os.Exit(0)
	}

	parent, child, err := newChildSyncPair()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildHandshakeParentFailureAbortsFinalExecGate$")
	cmd.Env = append(os.Environ(), "SANDBOX_INIT_HANDSHAKE_HELPER=ready")
	cmd.ExtraFiles = []*os.File{child}
	injected := errors.New("injected parent setup failure")
	failureMarked := false
	pid, err := coordinateChild(cmd, parent, child, func(int) error { return nil }, func(int) error {
		return injected
	}, func(int) {
		failureMarked = true
	}, childSyncExecEOF)
	if pid <= 0 {
		t.Fatalf("helper pid = %d", pid)
	}
	if !errors.Is(err, injected) {
		t.Fatalf("coordinateChild error = %v, want injected parent failure", err)
	}
	if !failureMarked {
		t.Fatal("parent failure was not marked before the child was aborted")
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		t.Fatalf("helper did not stop cleanly at the final-exec gate: %v", waitErr)
	}
}

func TestPluginHelperEnvironmentKeepsDefaultPATH(t *testing.T) {
	value := func(env []string, key string) string {
		prefix := key + "="
		for _, entry := range env {
			if strings.HasPrefix(entry, prefix) {
				return strings.TrimPrefix(entry, prefix)
			}
		}
		return ""
	}

	got := execEnv(map[string]string{"LOG": "info"})
	if value(got, "PATH") == "" {
		t.Fatalf("plugin helper environment lost default PATH: %v", got)
	}
	if value(got, "LOG") != "info" {
		t.Fatalf("plugin helper environment lost explicit variable: %v", got)
	}
	overridden := execEnv(map[string]string{"PATH": "/plugin/bin"})
	if value(overridden, "PATH") != "/plugin/bin" {
		t.Fatalf("explicit plugin PATH did not override default: %v", overridden)
	}
}
