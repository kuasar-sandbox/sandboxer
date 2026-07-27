package sandbox

import (
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"strings"
	"testing"
)

func hasCmdlineToken(cmdline, want string) bool {
	for _, token := range strings.Fields(cmdline) {
		if token == want {
			return true
		}
	}
	return false
}

func makeMinimalCfg() *config.SandboxConfig {
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 2, Memory: "4GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1.5, Memory: "2GiB"},
		},
		Network: config.NetworkConfig{TAP: "tap0"},
		Boot: config.BootConfig{
			Kernel:  "file:///vmlinux",
			Runtime: "file:///sandbox-runtime.erofs",
			Cmdline: "console=hvc0",
			Root: config.RootConfig{
				Base: "file:///c.erofs",
				Overlay: &config.OverlayConfig{
					Diff:     "file:///d.ext4",
					DiffSize: "1GiB",
				},
			},
		},
		Launch: config.LaunchConfig{
			Exec:    "/usr/bin/echo",
			Args:    []string{"hello", "world"},
			Workdir: "/",
			Restart: "never",
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func TestCHCommand_HasExpectedFlags(t *testing.T) {
	cfg := makeMinimalCfg()
	args, err := CHCommand(cfg,
		[]DiskArg{{Sock: "/run/sb/blk0.sock", ReadOnly: true}, {Sock: "/run/sb/blk1.sock"}}, "/run/sb/ch.sock", "/run/sb/vsock.sock",
		"/vmlinux", "/sandbox-runtime.erofs", "/run/sb/uffd.sock", "tty", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--api-socket /run/sb/ch.sock",
		"--kernel /vmlinux",
		"file=/sandbox-runtime.erofs,discard_writes=on",
		"size=4096M,shared=on,fd=3,uffd_socket=/run/sb/uffd.sock",
		// cap = 4 GiB, alloc = 2 GiB → balloon pre-inflated to 2 GiB
		// (= 2147483648 bytes) so guest sees exactly `alloc` from boot.
		"--balloon size=2147483648",
		"boot=2",
		"--disk vhost_user=on,socket=/run/sb/blk0.sock,readonly=on vhost_user=on,socket=/run/sb/blk1.sock",
		"tap=tap0",
		"cid=3,socket=/run/sb/vsock.sock", // vsock device
		"--console tty",
		"--serial off",
		"console=hvc0",
		"init=/sbin/init",
		"root=/dev/pmem0",
		"rootfstype=erofs",
		"rootflags=dax=always",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("CH cmdline missing %q\n  got: %s", want, joined)
		}
	}
	// Ensure launch params are NOT in the cmdline (they go via vsock).
	for _, banned := range []string{"sandbox.app", "sandbox.args", "sandbox.env", "sandbox.workdir"} {
		if strings.Contains(joined, banned) {
			t.Errorf("CH cmdline must not contain %q (launch goes via vsock now)", banned)
		}
	}
}

func TestCHCommand_InitialAllocatableOverridesBalloon(t *testing.T) {
	cfg := makeMinimalCfg()
	args, err := CHCommandWithInitialAllocatable(cfg, 3<<30,
		[]DiskArg{{Sock: "/run/sb/blk0.sock", ReadOnly: true}, {Sock: "/run/sb/blk1.sock"}},
		"/run/sb/ch.sock", "/run/sb/vsock.sock", "/vmlinux", "/sandbox-runtime.erofs", "/run/sb/uffd.sock", "tty", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--balloon size=1073741824") {
		t.Fatalf("initial allocatable 3GiB under 4GiB capacity should boot with 1GiB balloon, got: %s", joined)
	}
}

func TestCHCommand_SingleDisk(t *testing.T) {
	cfg := makeMinimalCfg()
	// Single-disk: drop the overlay; the root disk is a writable ext4 CoW.
	cfg.Boot.Root = config.RootConfig{DiffTemplate: "file:///root.ext4"}
	if !cfg.SingleDisk() {
		t.Fatal("cfg should be single-disk after removing overlay")
	}
	args, err := CHCommand(cfg,
		[]DiskArg{{Sock: "/run/sb/blk0.sock"}}, "/run/sb/ch.sock", "/run/sb/vsock.sock",
		"/vmlinux", "/sandbox-runtime.erofs", "/run/sb/uffd.sock", "tty", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	// Exactly one writable disk (blk0, no readonly), no blk1, single-disk cmdline.
	if !strings.Contains(joined, "--disk vhost_user=on,socket=/run/sb/blk0.sock") {
		t.Errorf("single-disk should emit one writable blk0 disk, got: %s", joined)
	}
	if strings.Contains(joined, "readonly=on") {
		t.Errorf("single-disk blk0 must not be readonly, got: %s", joined)
	}
	if strings.Contains(joined, "blk1") {
		t.Errorf("single-disk must not emit blk1, got: %s", joined)
	}
	if !strings.Contains(joined, "sandbox.root.layout=single") {
		t.Errorf("single-disk cmdline must select single layout, got: %s", joined)
	}
}

func TestCHCommand_TapFDMode(t *testing.T) {
	cfg := makeMinimalCfg()
	cfg.Network = config.NetworkConfig{TapFD: &config.TapFDConfig{Exec: []string{"helper"}}}
	args, err := CHCommand(cfg,
		[]DiskArg{{Sock: "/run/sb/blk0.sock", ReadOnly: true}, {Sock: "/run/sb/blk1.sock"}}, "/run/sb/ch.sock", "/run/sb/vsock.sock",
		"/vmlinux", "/sandbox-runtime.erofs", "/run/sb/uffd.sock", "tty",
		4, "02:00:00:00:80:01") // CH fd 4 (memfd=3 + tap), effective mac from handoff
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--net fd=4,mac=02:00:00:00:80:01,id=_net0,iommu=off") {
		t.Errorf("fd-mode --net wrong\n  got: %s", joined)
	}
	if strings.Contains(joined, "tap=") {
		t.Errorf("fd-mode must not emit tap=, got: %s", joined)
	}
}

func TestCHCommand_NoNetwork(t *testing.T) {
	cfg := makeMinimalCfg()
	cfg.Network = config.NetworkConfig{}
	args, err := CHCommand(cfg,
		[]DiskArg{{Sock: "/run/sb/blk0.sock", ReadOnly: true}, {Sock: "/run/sb/blk1.sock"}},
		"/run/sb/ch.sock", "/run/sb/vsock.sock", "/vmlinux", "/sandbox-runtime.erofs",
		"/run/sb/uffd.sock", "tty", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--net") {
		t.Fatalf("no-network mode must omit --net, got: %s", joined)
	}
	if !strings.Contains(joined, "--vsock cid=3,socket=/run/sb/vsock.sock") {
		t.Fatalf("no-network mode must retain the vsock control plane, got: %s", joined)
	}
}

func TestCHCommand_TapNameModeMirrorsMAC(t *testing.T) {
	cfg := makeMinimalCfg() // Network.TAP = "tap0"
	args, err := CHCommand(cfg, []DiskArg{{Sock: "/0", ReadOnly: true}, {Sock: "/1"}}, "/c", "/v", "/k", "/r", "/u", "tty",
		0, "02:00:00:00:80:01") // no fd; effective mac mirrored
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "--net tap=tap0,mac=02:00:00:00:80:01,iommu=off") {
		t.Errorf("tap-mode --net should carry mac, got: %s", joined)
	}
}

func TestCHCommand_BalloonDeflateOnOOMDefault(t *testing.T) {
	cfg := makeMinimalCfg()
	args, err := CHCommand(cfg, []DiskArg{{Sock: "/0", ReadOnly: true}, {Sock: "/1"}}, "/c", "/v", "/k", "/r", "/u", "tty", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "deflate_on_oom=on") {
		t.Errorf("balloon should contain deflate_on_oom=on by default, got: %s", joined)
	}
}

func TestCHCommand_BalloonDeflateOnOOMDisabled(t *testing.T) {
	cfg := makeMinimalCfg()
	off := false
	cfg.Resources.Allocatable.DeflateOnOOM = &off
	args, err := CHCommand(cfg, []DiskArg{{Sock: "/0", ReadOnly: true}, {Sock: "/1"}}, "/c", "/v", "/k", "/r", "/u", "tty", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "deflate_on_oom=on") {
		t.Errorf("balloon should NOT contain deflate_on_oom=on when explicitly disabled, got: %s", joined)
	}
	// free_page_reporting is now intentionally OFF (its mmu_notifier
	// traffic deadlocks the guest vsock kthread; replaced by the
	// host-side resctl.BalloonController + sandbox-init mem_report).
	if strings.Contains(joined, "free_page_reporting") {
		t.Errorf("balloon must not advertise free_page_reporting (replaced by mem_report-driven vm.resize), got: %s", joined)
	}
}

func TestCHCommand_NoBalloonWhenAllocEqualsCapacity(t *testing.T) {
	cfg := makeMinimalCfg()
	cfg.Resources.Allocatable.Memory = cfg.Resources.Capacity.Memory
	args, err := CHCommand(cfg,
		[]DiskArg{{Sock: "/run/sb/blk0.sock", ReadOnly: true}, {Sock: "/run/sb/blk1.sock"}}, "/run/sb/ch.sock", "/run/sb/vsock.sock",
		"/vmlinux", "/sandbox-runtime.erofs", "/run/sb/uffd.sock", "tty", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range args {
		if strings.Contains(a, "balloon") {
			t.Errorf("balloon should not appear when allocatable == capacity, got %q", a)
		}
	}
}

func TestBuildCmdline_ContainsAutoInjectedAndUserExtras(t *testing.T) {
	cfg := makeMinimalCfg()
	cfg.Boot.Cmdline = "console=hvc0 ip=169.254.1.1::169.254.1.0:255.255.255.254:sb1:eth0:off"

	cl := buildCmdline(cfg)
	for _, want := range []string{
		"init=/sbin/init",
		"root=/dev/pmem0",
		"rootfstype=erofs",
		"rootflags=dax=always",
		"console=hvc0",
		"ip=169.254.1.1",
	} {
		if !strings.Contains(cl, want) {
			t.Errorf("cmdline missing %q\n  got: %s", want, cl)
		}
	}
	if !hasCmdlineToken(cl, "rootflags=dax=always") {
		t.Errorf("cmdline must pass EROFS DAX through rootflags: %s", cl)
	}
	if hasCmdlineToken(cl, "dax=always") {
		t.Errorf("cmdline must not contain bare EROFS mount option: %s", cl)
	}
	for _, banned := range []string{"sandbox.app", "sandbox.args"} {
		if strings.Contains(cl, banned) {
			t.Errorf("cmdline must not contain %q (vsock-only now)", banned)
		}
	}
}

func TestCHCommand_MemoryStringsHonored(t *testing.T) {
	cfg := makeMinimalCfg()
	cfg.Resources.Capacity.Memory = "512MiB"
	cfg.Resources.Allocatable.Memory = "256MiB"
	args, err := CHCommand(cfg, []DiskArg{{Sock: "/0", ReadOnly: true}, {Sock: "/1"}}, "/c", "/v", "/k", "/r", "/u", "tty", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "size=512M,shared=on") {
		t.Errorf("expected 512M memory, got: %s", joined)
	}
	// Balloon is pre-inflated to (cap-alloc) at boot so guest sees
	// exactly `alloc` from kernel init — no post-Settled inflate
	// transition. cap=512M alloc=256M → balloon=256M=268435456.
	if !strings.Contains(joined, "--balloon size=268435456") {
		t.Errorf("expected --balloon size=268435456 (cap-alloc pre-inflated), got: %s", joined)
	}
}
