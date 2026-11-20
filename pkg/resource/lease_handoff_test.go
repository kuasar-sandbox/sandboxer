package resource

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLeaseCandidatePublicationAndLostACK(t *testing.T) {
	dir := t.TempDir()
	socket, sid := filepath.Join(dir, "ctl.sock"), "handoff-sid"
	old := exec.Command(os.Args[0], "-test.run=^TestLeaseHelperProcess$")
	old.Env = append(os.Environ(), "RESOURCE_LEASE_HELPER=1", "RESOURCE_LEASE_SOCKET="+socket,
		"RESOURCE_LEASE_SID="+sid, "RESOURCE_LEASE_CGROUP="+filepath.Join(dir, "old"), "GORACE=atexit_sleep_ms=0")
	in, err := old.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := old.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if old.ProcessState == nil {
			_ = old.Process.Kill()
			_ = old.Wait()
		}
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("old owner ready: %q %v", line, err)
	}
	l := Lease{Version: LeaseVersion, SandboxID: sid, PID: os.Getpid(), ControllerSocket: socket,
		CgroupPath: filepath.Join(dir, "new"), CapacityMemory: 1024, CapacityCPUMilli: 1000, FloorMemory: 256,
		FloorCPUMilli: 500, StartupMemory: 512, ClientFeatures: []string{FeatureStateSyncV1, FeatureRetainedMemoryV1}}
	h, err := CreateLeaseCandidate(l)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	candidate := h.Path()
	assertExternalLeaseOwner(t, LeasePath(socket, sid), old.Process.Pid)
	_, owner, id, err := InspectLeaseCandidate(socket, sid, candidate)
	if err != nil || owner != os.Getpid() {
		t.Fatalf("candidate inspection: owner=%d err=%v", owner, err)
	}
	if err := PublishLeaseCandidate(socket, sid, candidate, id, owner); err != nil {
		t.Fatal(err)
	}
	// Model the server having committed but its ACK not reaching the owner.
	assertExternalLeaseOwner(t, LeasePath(socket, sid), os.Getpid())
	// Inventory can run before the client receives its ACK or confirms the
	// canonical path. An embedded reader must not release the process lock.
	if got, err := ReadLease(LeasePath(socket, sid)); err != nil || got.CgroupPath != l.CgroupPath {
		t.Fatalf("published lease before ACK: %+v %v", got, err)
	}
	assertExternalLeaseOwner(t, LeasePath(socket, sid), os.Getpid())
	if published, err := h.ConfirmPublished(); err != nil || !published {
		t.Fatalf("ACK-loss confirmation: %v %v", published, err)
	}
	if _, err := ReadLease(h.Path()); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Write([]byte("close\n")); err != nil {
		t.Fatal(err)
	}
	if err := old.Wait(); err != nil {
		t.Fatal(err)
	}
	assertExternalLeaseOwner(t, h.Path(), os.Getpid())
	if err := PublishLeaseCandidate(socket, sid, candidate, id, owner); err != nil {
		t.Fatalf("replay: %v", err)
	}
	wrong := id
	wrong.Inode++
	if err := PublishLeaseCandidate(socket, sid, candidate, wrong, owner); err == nil {
		t.Fatal("different inode replay accepted")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(LeasePath(socket, sid)); !os.IsNotExist(err) {
		t.Fatalf("new owner's final close did not remove its lease: %v", err)
	}
}

func TestLeaseCandidateEmbeddedOwnerReplacesLocalCanonical(t *testing.T) {
	dir := t.TempDir()
	socket, sid := filepath.Join(dir, "ctl.sock"), "embedded-sid"
	l := Lease{Version: LeaseVersion, SandboxID: sid, PID: os.Getpid(), ControllerSocket: socket,
		CgroupPath: filepath.Join(dir, "old"), CapacityMemory: 1024, CapacityCPUMilli: 1000, FloorMemory: 256,
		FloorCPUMilli: 500, StartupMemory: 512, ClientFeatures: []string{FeatureStateSyncV1, FeatureRetainedMemoryV1}}
	old, err := CreateLease(l)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	l.CgroupPath = filepath.Join(dir, "new")
	next, err := CreateLeaseCandidate(l)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	candidate := next.Path()
	_, pid, file, err := InspectLeaseCandidate(socket, sid, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := PublishLeaseCandidate(socket, sid, candidate, file, pid); err != nil {
		t.Fatal(err)
	}
	got, _, gotFile, err := InspectLeaseIdentity(LeasePath(socket, sid))
	if err != nil || got.CgroupPath != l.CgroupPath || gotFile != file {
		t.Fatalf("canonical read used replaced local handle: %+v %+v %v", got, gotFile, err)
	}
	assertExternalLeaseOwner(t, LeasePath(socket, sid), os.Getpid())
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	assertExternalLeaseOwner(t, LeasePath(socket, sid), os.Getpid())
	if got, err := ReadLease(LeasePath(socket, sid)); err != nil || got.CgroupPath != l.CgroupPath {
		t.Fatalf("old Close removed successor registration: %+v %v", got, err)
	}
	assertExternalLeaseOwner(t, LeasePath(socket, sid), os.Getpid())
}
