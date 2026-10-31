package main

import (
	"bytes"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

func TestRunFromAndRestoreAreMutuallyExclusive(t *testing.T) {
	if code := runCmd([]string{"--from", "a.sandbox", "--restore", "b.snapshot"}); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestReplaceBootCLIValidation(t *testing.T) {
	t.Setenv("SANDBOX_CONFIG", "")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "without from", args: []string{"--replace-boot"}},
		{name: "with restore", args: []string{"--replace-boot", "--restore", "source.snapshot"}},
		{name: "without config", args: []string{"--replace-boot", "--from", "source.sandbox"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := runCmd(tc.args); code != 2 {
				t.Fatalf("runCmd(%v) exit = %d, want 2", tc.args, code)
			}
		})
	}
}

func testRunFromPortable(t *testing.T) *config.PortableSandboxConfig {
	t.Helper()
	const a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cfg := &config.PortableSandboxConfig{
		Version: config.PortableSandboxConfigVersion,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 1, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "1GiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel: "file://vmlinux@digest:" + a, Runtime: "file://runtime.bundle@digest:" + b,
			Root: config.PortableRootConfig{Base: "file://base.erofs@digest:" + a, Overlay: &config.PortableOverlayConfig{Base: "self"}},
		},
		Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runFromDirectPortable(t *testing.T) *config.PortableSandboxConfig {
	t.Helper()
	cfg := testRunFromPortable(t)
	cfg.Boot.Root = config.PortableRootConfig{Base: "self"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runFromLogical(t *testing.T, portable *config.PortableSandboxConfig) sparse.Source {
	t.Helper()
	runtimeBytes, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := sparse.NewSource(bytes.NewReader(bytes.Repeat([]byte{0x6d}, 4096)), 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := sandboxfile.BuildSource(payload, nil, runtimeBytes)
	if err != nil {
		t.Fatal(err)
	}
	return logical
}
