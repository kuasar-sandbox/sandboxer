package resource

import (
	"bufio"
	"encoding/json"
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
