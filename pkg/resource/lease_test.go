package resource

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLeaseHelperProcess(t *testing.T) {
	if os.Getenv("RESOURCE_LEASE_HELPER") != "1" {
		return
	}
	lease := Lease{
		Version: LeaseVersion, SandboxID: os.Getenv("RESOURCE_LEASE_SID"), PID: os.Getpid(),
		ControllerSocket: os.Getenv("RESOURCE_LEASE_SOCKET"), CgroupPath: os.Getenv("RESOURCE_LEASE_CGROUP"),
		CapacityMemory: 1024, CapacityCPUMilli: 1000,
		FloorMemory: 256, FloorCPUMilli: 500, StartupMemory: 512,
		ClientFeatures: []string{FeatureStateSyncV1},
	}
	handle, err := CreateLease(lease)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	_, _ = os.Stdout.WriteString("ready\n")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestLeaseLockProbeHelperProcess(t *testing.T) {
	if os.Getenv("RESOURCE_LEASE_PROBE_HELPER") != "1" {
		return
	}
	owner, locked, err := LeaseLockOwner(os.Getenv("RESOURCE_LEASE_PROBE_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("lease is not locked")
	}
	_, _ = fmt.Fprintln(os.Stdout, owner)
}

func TestLeaseStartupBudgetMayBeBelowSettledHeadroom(t *testing.T) {
	lease := Lease{
		Version: LeaseVersion, SandboxID: "startup-below-headroom", PID: os.Getpid(),
		ControllerSocket: "/run/controller.sock", CgroupPath: "/sys/fs/cgroup/sandbox",
		CapacityMemory: 1024, CapacityCPUMilli: 1000,
		FloorMemory: 512, FloorCPUMilli: 500, StartupMemory: 256,
	}
	if err := lease.Validate(); err != nil {
		t.Fatalf("independent startup headroom rejected: %v", err)
	}
	lease.StartupMemory = 0
	if err := lease.Validate(); err == nil {
		t.Fatal("zero startup headroom accepted")
	}
}

func assertExternalLeaseOwner(t *testing.T, path string, want int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseLockProbeHelperProcess$")
	cmd.Env = append(os.Environ(),
		"RESOURCE_LEASE_PROBE_HELPER=1", "RESOURCE_LEASE_PROBE_PATH="+path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("external lock probe: %v: %s", err, out)
	}
	var got int
	if _, err := fmt.Fscan(strings.NewReader(string(out)), &got); err != nil {
		t.Fatalf("parse external lock owner from %q: %v", out, err)
	}
	if got != want {
		t.Fatalf("external lock owner = %d, want %d", got, want)
	}
}

func TestLeaseLifecycleAndHashedPath(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "controller.sock")
	sid := "../../tenant/sandbox"
	cgroup := filepath.Join(dir, "cgroup")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHelperProcess$")
	cmd.Env = append(os.Environ(),
		"RESOURCE_LEASE_HELPER=1", "RESOURCE_LEASE_SOCKET="+socket,
		"RESOURCE_LEASE_SID="+sid, "RESOURCE_LEASE_CGROUP="+cgroup)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper ready = %q, %v", line, err)
	}
	path := LeasePath(socket, sid)
	if filepath.Dir(path) != LeaseDir(socket) || strings.Contains(filepath.Base(path), sid) {
		t.Fatalf("unsafe lease path %q", path)
	}
	owner, locked, err := LeaseLockOwner(path)
	if err != nil || !locked || owner != cmd.Process.Pid {
		t.Fatalf("lock owner = %d locked=%v err=%v, want %d", owner, locked, err, cmd.Process.Pid)
	}
	lease, err := ReadLease(path)
	if err != nil {
		t.Fatal(err)
	}
	if lease.SandboxID != sid || lease.PID != cmd.Process.Pid || !lease.Supports(FeatureStateSyncV1) {
		t.Fatalf("lease = %+v", lease)
	}
	inspected, inspectOwner, inspectLocked, err := InspectLease(path)
	if err != nil || !inspectLocked || inspectOwner != cmd.Process.Pid || inspected.SandboxID != sid {
		t.Fatalf("InspectLease = %+v owner=%d locked=%v err=%v", inspected, inspectOwner, inspectLocked, err)
	}
	if removed, err := RemoveUnlockedLease(path); err != nil || removed {
		t.Fatalf("live lease stale cleanup = removed %v, err %v", removed, err)
	}
	_, _ = stdin.Write([]byte("stop\n"))
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("normal exit retained lease: %v", err)
	}
}

func TestOwnerSideLeaseInspectionPreservesPOSIXLock(t *testing.T) {
	dir := t.TempDir()
	lease := Lease{
		Version: LeaseVersion, SandboxID: "owner-inspection", PID: os.Getpid(),
		ControllerSocket: filepath.Join(dir, "controller.sock"), CgroupPath: filepath.Join(dir, "cgroup"),
		CapacityMemory: 1024, CapacityCPUMilli: 1000,
		FloorMemory: 256, FloorCPUMilli: 500, StartupMemory: 512,
	}
	handle, err := CreateLease(lease)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	path := handle.Path()
	assertExternalLeaseOwner(t, path, os.Getpid())

	owner, locked, err := LeaseLockOwner(path)
	if err != nil || !locked || owner != os.Getpid() {
		t.Fatalf("owner-side LockOwner = %d locked=%v err=%v", owner, locked, err)
	}
	assertExternalLeaseOwner(t, path, os.Getpid())

	read, err := ReadLease(path)
	if err != nil || read.SandboxID != lease.SandboxID {
		t.Fatalf("owner-side ReadLease = %+v, %v", read, err)
	}
	assertExternalLeaseOwner(t, path, os.Getpid())

	inspected, owner, locked, err := InspectLease(path)
	if err != nil || !locked || owner != os.Getpid() || inspected.SandboxID != lease.SandboxID {
		t.Fatalf("owner-side InspectLease = %+v owner=%d locked=%v err=%v", inspected, owner, locked, err)
	}
	assertExternalLeaseOwner(t, path, os.Getpid())

	if removed, err := RemoveUnlockedLease(path); err != nil || removed {
		t.Fatalf("owner-side stale cleanup = removed %v, err %v", removed, err)
	}
	assertExternalLeaseOwner(t, path, os.Getpid())

	if duplicate, err := CreateLease(lease); err == nil {
		_ = duplicate.Close()
		t.Fatal("same process created a second live lease for one SID")
	}
	assertExternalLeaseOwner(t, path, os.Getpid())
}

func TestLeaseHandleCloseDoesNotUnlinkReplacementPath(t *testing.T) {
	dir := t.TempDir()
	lease := Lease{
		Version: LeaseVersion, SandboxID: "close-replacement", PID: os.Getpid(),
		ControllerSocket: filepath.Join(dir, "controller.sock"), CgroupPath: filepath.Join(dir, "cgroup"),
		CapacityMemory: 1024, CapacityCPUMilli: 1000,
		FloorMemory: 256, FloorCPUMilli: 500, StartupMemory: 512,
	}
	handle, err := CreateLease(lease)
	if err != nil {
		t.Fatal(err)
	}
	path := handle.Path()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "replacement\n" {
		t.Fatalf("replacement content = %q", content)
	}
}

func TestReadLeaseRejectsTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.json")
	lease := Lease{
		Version: LeaseVersion, SandboxID: "trailing", PID: os.Getpid(),
		ControllerSocket: "/run/controller.sock", CgroupPath: "/sys/fs/cgroup/test",
		CapacityMemory: 1024, CapacityCPUMilli: 1000,
		FloorMemory: 256, FloorCPUMilli: 500, StartupMemory: 512,
	}
	payload, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(payload, []byte("\n{}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLease(path); err == nil {
		t.Fatal("lease with trailing JSON was accepted")
	}
}

func TestRemoveUnlockedLeaseRechecksAndRemovesStaleRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale.json")
	if err := os.WriteFile(path, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveUnlockedLease(path)
	if err != nil || !removed {
		t.Fatalf("RemoveUnlockedLease = removed %v, err %v", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale lease remains: %v", err)
	}
}

func TestRemoveUnlockedLeaseDoesNotUnlinkReplacementPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale.json")
	if err := os.WriteFile(path, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var hookErr error
	removed, err := removeUnlockedLease(path, func(path string, _ *os.File) {
		if hookErr = os.Remove(path); hookErr != nil {
			return
		}
		hookErr = os.WriteFile(path, []byte("replacement\n"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if removed {
		t.Fatal("cleanup reported removing an inode it did not open")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "replacement\n" {
		t.Fatalf("replacement pathname content = %q", got)
	}
}

func TestCreateLeaseAtomicallyPublishesLockedCompleteInodeAfterStaleRace(t *testing.T) {
	dir := t.TempDir()
	lease := Lease{
		Version: LeaseVersion, SandboxID: "open-lock-race", PID: os.Getpid(),
		ControllerSocket: filepath.Join(dir, "controller.sock"), CgroupPath: filepath.Join(dir, "cgroup"),
		CapacityMemory: 1024, CapacityCPUMilli: 1000,
		FloorMemory: 256, FloorCPUMilli: 500, StartupMemory: 512,
	}
	path := LeasePath(lease.ControllerSocket, lease.SandboxID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var temporary os.FileInfo
	var hookErr error
	handle, err := createLease(lease, func(path string, temporaryFile *os.File) {
		temporary, hookErr = temporaryFile.Stat()
		if hookErr != nil {
			return
		}
		visible, readErr := os.ReadFile(path)
		if readErr != nil {
			hookErr = readErr
			return
		}
		if string(visible) != "stale\n" {
			hookErr = fmt.Errorf("stable pathname exposed unpublished content %q", visible)
			return
		}
		if hookErr = os.Remove(path); hookErr != nil {
			return
		}
		hookErr = os.WriteFile(path, []byte("replacement\n"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	current, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	handleInfo, err := handle.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(handleInfo, current) {
		t.Fatal("lease handle does not own the inode reachable by pathname")
	}
	if !os.SameFile(temporary, current) {
		t.Fatal("published lease is not the completely written temporary inode")
	}
	got, err := ReadLease(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != lease.SandboxID || got.PID != lease.PID {
		t.Fatalf("lease = %+v", got)
	}
}
