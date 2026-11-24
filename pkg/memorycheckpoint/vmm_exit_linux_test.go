//go:build linux

package memorycheckpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeferredVMMRecoveryWaitsForOriginalPhysicalOwner(t *testing.T) {
	p := startRecoveryProcess(t, "retired")
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", p.command.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	var raw *os.File
	for _, e := range entries {
		name := fmt.Sprintf("/proc/%d/fd/%s", p.command.Process.Pid, e.Name())
		link, err := os.Readlink(name)
		if err == nil && strings.Contains(link, "memfd:recovery-process") {
			raw, err = os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if raw == nil {
		t.Fatal("missing raw backing")
	}
	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = []*os.File{raw}
	if err := cmd.Start(); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	cg := t.TempDir()
	if err = os.WriteFile(filepath.Join(cg, "cgroup.procs"), []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := IdentifyVMM(cmd.Process.Pid, cg)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := v.CoversCgroup(cg); err != nil || !ok {
		t.Fatalf("cgroup proof=%t %v", ok, err)
	}
	p.record.Version = DeferredVMMRecordVersion
	p.record.VMM = &v
	data, err := json.Marshal(p.record)
	if err != nil {
		t.Fatal(err)
	}
	// Model the committed v3 record produced before the retirement ACK.
	if err = os.WriteFile(p.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p.kill(t)
	got, err := RecoverMemoryRelease(context.Background(), p.path, p.record)
	if err != nil || !got.ProducerExited || got.Record.RAMReleased {
		t.Fatalf("premature release %+v %v", got, err)
	}
	if _, err = DiscardCapture(context.Background(), p.dir, p.record.Producer, p.record.CheckpointID); err == nil {
		t.Fatal("discard bypassed live VMM")
	}
	live, err := os.Open(fmt.Sprintf("/proc/%d/fd/3", cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	var buf [1]byte
	if _, err = live.ReadAt(buf[:], 0); err != nil || buf[0] != 0x35 {
		t.Fatalf("backing lost %+v %v", buf, err)
	}
	live.Close()
	// A different live member at the old pathname must never be hidden.
	if err = os.WriteFile(filepath.Join(cg, "cgroup.procs"), []byte(fmt.Sprintf("%d\n%d\n", cmd.Process.Pid, os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	if ok, err := v.CoversCgroup(cg); err != nil || ok {
		t.Fatalf("foreign member was hidden: %t %v", ok, err)
	}
	stale := v
	stale.StartTime++
	if gone, err := stale.Exited(); err != nil || !gone {
		t.Fatalf("PID reuse %t %v", gone, err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if gone, err := v.Exited(); err != nil || !gone {
		t.Fatalf("child still live: %t %v", gone, err)
	}
	got, err = RecoverMemoryRelease(context.Background(), p.path, p.record)
	if err != nil || !got.Record.RAMReleased {
		t.Fatalf("release after VMM exit %+v %v", got, err)
	}
	// Legacy version may not carry an ignored extra proof.
	bad := p.record
	bad.Version = RecordVersion
	if err := bad.Validate(); err == nil {
		t.Fatal("legacy version accepted deferred proof")
	}
	bad = p.record
	bad.VMM = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("missing deferred proof accepted")
	}
}

func TestVMMExitUnknownIsNotProof(t *testing.T) {
	if gone, err := (VMMIdentity{}).Exited(); gone || err == nil {
		t.Fatalf("empty proof %t %v", gone, err)
	}
	// A known PID and exact directory are necessary at preparation.
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "cgroup.procs"), []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := IdentifyVMM(os.Getpid(), d); err == nil {
		t.Fatal("unrelated cgroup accepted")
	}
	if _, err := IdentifyVMM(-1, d); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid pid accepted")
	}
}
