package main

import (
	"strings"
	"testing"
)

func TestRestoreFilter(t *testing.T) {
	in := `resources:
  capacity: { cpu: 2, memory: 8GiB }
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
init:
  - { exec: /bin/true }
restore:
  prefetch: memory
`
	out, err := restoreFilter([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, dropped := range []string{"launch:", "mounts:", "files:", "init:", "kernel:", "cmdline:", "/snap.ext4"} {
		if strings.Contains(got, dropped) {
			t.Errorf("restore filter should have dropped %q; output:\n%s", dropped, got)
		}
	}
	for _, kept := range []string{"runtime:", "tap0", "diff_template", "capacity", "prefetch: memory"} {
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
  runtime: file:///opt/sandbox/sandbox-runtime.bundle
  root:
    base: file:///opt/sandbox/app.erofs
    overlay: {}
`)
	for mode, doc := range map[string][]byte{"cold": cold, "restore": restore} {
		t.Run(mode, func(t *testing.T) {
			if rc := strictCheckBytes(doc, mode); rc != 0 {
				t.Fatalf("%s strict check returned %d for no-network config", mode, rc)
			}
		})
	}
}
