package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDefaultPreservesColdOverlayBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.yaml")
	body := `resources:
  capacity: { cpu: 2, memory: 2GiB }
  allocatable: { cpu: 2, memory: 1GiB }
  startup: { memory: 2GiB }
boot:
  kernel: file:///opt/vmlinux
  runtime: file:///opt/runtime.bundle
  cmdline: quiet
  root:
    base: file:///opt/root.erofs
    overlay:
      base: file:///opt/root-upper.ext4
launch: { exec: /bin/app }
files:
  - { path: /etc/value, content: persistent }
metadata: { owner: test }
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, stdout, stderr := captureInfoOutput(t, func() int {
		return configCmd([]string{"--config", path, "--mode", "default", "--check", "strict"})
	})
	if rc != 0 || stderr != "" {
		t.Fatalf("config default rc=%d stderr=%q", rc, stderr)
	}
	for _, kept := range []string{
		"cmdline: quiet", "base: file:///opt/root-upper.ext4", "startup:",
		"launch:", "files:", "metadata:",
	} {
		if !strings.Contains(stdout, kept) {
			t.Errorf("config default omitted %q; output:\n%s", kept, stdout)
		}
	}
}

func TestRestoreFilter(t *testing.T) {
	in := `resources:
  capacity: { cpu: 2, memory: 8GiB }
  startup: { memory: 1GiB }
network: { tap: tap0 }
boot:
  kernel: file:///opt/vmlinux
  cmdline: "quiet"
  runtime: file:///opt/rt.erofs
  root:
    base: file:///opt/app.erofs
    overlay:
      base: file:///snap.ext4
      diff_template: file:///opt/t.ext4
launch: { exec: /bin/app }
mounts:
  - { target: /tmp, type: tmpfs }
files:
  - { path: /etc/x }
ephemeral_files:
  - { path: /run/x }
init:
  - { exec: /bin/true }
metadata: { owner: test }
restore:
  prefetch: memory
`
	out, err := restoreFilter([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, dropped := range []string{"launch:", "mounts:", "files:", "ephemeral_files:", "init:", "metadata:", "cmdline:", "base:", "/snap.ext4"} {
		if strings.Contains(got, dropped) {
			t.Errorf("restore filter should have dropped %q; output:\n%s", dropped, got)
		}
	}
	for _, kept := range []string{"kernel:", "runtime:", "tap0", "diff_template", "capacity", "startup:", "memory: 1GiB", "prefetch: memory"} {
		if !strings.Contains(got, kept) {
			t.Errorf("restore filter should have kept %q; output:\n%s", kept, got)
		}
	}
}

func TestRestoreSkeletonKeepsPrefetchDisabled(t *testing.T) {
	if rc := strictCheckBytes([]byte(skeletonRestore), "restore"); rc != 0 {
		t.Fatalf("restore skeleton strict check returned %d", rc)
	}
	if !strings.Contains(skeletonRestore, "prefetch: off") {
		t.Fatalf("restore skeleton must expose disabled-by-default prefetch policy")
	}
}

func TestStrictCheckAcceptsNoNetwork(t *testing.T) {
	cold := []byte(`resources:
  capacity: { cpu: 1, memory: 1GiB }
  allocatable: { cpu: 1, memory: 1GiB }
boot:
  kernel: file:///opt/sandbox/vmlinux
  runtime: file:///opt/sandbox/sandbox-runtime.bundle
  root:
    diff_template: file:///opt/sandbox/root.ext4
launch: { exec: /bin/true }
`)
	restore := []byte(`resources:
  capacity: { cpu: 1, memory: 1GiB }
boot:
  kernel: file:///opt/sandbox/vmlinux
  runtime: file:///opt/sandbox/sandbox-runtime.bundle
  root:
    overlay:
      diff_template: file:///opt/sandbox/root.ext4
`)
	for mode, doc := range map[string][]byte{"cold": cold, "restore": restore} {
		t.Run(mode, func(t *testing.T) {
			if rc := strictCheckBytes(doc, mode); rc != 0 {
				t.Fatalf("%s strict check returned %d for no-network config", mode, rc)
			}
		})
	}
}
