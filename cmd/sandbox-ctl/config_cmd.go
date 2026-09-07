package main

import (
	"flag"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// configCmd implements `sandbox-ctl config` — produce a sandbox.yaml on
// stdout from a chosen source, with an optional mode filter and validation.
//
//	--config a.yaml[:b.yaml...]   read + deep-merge front-to-back, re-emit
//	  (or SANDBOX_CONFIG)          (later files override; nested maps merge)
//	--template                    emit a commented skeleton instead
//	--mode default|restore        restore drops fields a restore ignores
//	--check skip|strict           strict = validate, error if invalid
//	-o <file>                     write to file (default stdout)
//
// --config (merge) emits a normalized config (comments not preserved — it is
// parsed and re-serialized); --template is the comment-rich authoring path.
func configCmd(args []string) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	configPath := fs.String("config", "", "input sandbox.yaml path(s), ':'-separated, merged front-to-back (overrides SANDBOX_CONFIG)")
	template := fs.Bool("template", false, "emit a commented skeleton config instead of reading --config")
	mode := fs.String("mode", "default", "default | restore (restore drops fields a snapshot-restore ignores)")
	check := fs.String("check", "skip", "skip (emit as-is) | strict (validate; error + non-zero exit if invalid)")
	out := fs.String("o", "", "write output to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *mode != "default" && *mode != "restore" {
		fmt.Fprintln(os.Stderr, "sandbox-ctl config: --mode must be 'default' or 'restore'")
		return 2
	}
	if *check != "skip" && *check != "strict" {
		fmt.Fprintln(os.Stderr, "sandbox-ctl config: --check must be 'skip' or 'strict'")
		return 2
	}

	src := *configPath
	if src == "" {
		src = os.Getenv("SANDBOX_CONFIG")
	}
	if *template && src != "" {
		fmt.Fprintln(os.Stderr, "sandbox-ctl config: --template and --config are mutually exclusive")
		return 2
	}

	var output []byte
	switch {
	case *template:
		tmpl := skeletonCold
		if *mode == "restore" {
			tmpl = skeletonRestore
		}
		if *check == "strict" {
			if rc := strictCheckBytes([]byte(tmpl), *mode); rc != 0 {
				return rc
			}
		}
		output = []byte(tmpl)

	case src != "":
		cfg, err := config.LoadMerged(strings.Split(src, ":"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if *check == "strict" {
			if err := validateForMode(cfg, *mode); err != nil {
				fmt.Fprintf(os.Stderr, "sandbox-ctl config: invalid (%s mode): %v\n", *mode, err)
				return 1
			}
		}
		b, err := yaml.Marshal(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if *mode == "restore" {
			b, err = restoreFilter(b)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		output = b

	default:
		fmt.Fprintln(os.Stderr, "sandbox-ctl config: provide --config (or SANDBOX_CONFIG), or --template")
		return 2
	}

	if *out != "" {
		if err := os.WriteFile(*out, output, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	os.Stdout.Write(output)
	return 0
}

// validateForMode runs the validator appropriate to the output mode.
func validateForMode(cfg *config.SandboxConfig, mode string) error {
	if mode == "restore" {
		return cfg.ValidateRestoreHostConfig()
	}
	return cfg.ValidateCold()
}

// strictCheckBytes loads YAML bytes and validates them for mode (used to
// strict-check the built-in skeletons).
func strictCheckBytes(b []byte, mode string) int {
	tmp, err := os.CreateTemp("", "sandbox-skel-*.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tmp.Close()
	cfg, err := config.Load(tmp.Name())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := validateForMode(cfg, mode); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-ctl config: skeleton invalid (%s mode): %v\n", mode, err)
		return 1
	}
	return 0
}

// restoreFilter turns a cold authoring config into host-only restore input.
// Snapshot S points to Sandbox E, so workload state and every immutable disk
// ref are removed instead of being silently ignored. Kernel/runtime paths,
// active diff bindings, network provider/identity, and host policy remain.
func restoreFilter(in []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return in, nil
	}
	root := doc.Content[0]
	for _, k := range []string{"launch", "mounts", "files", "ephemeral_files", "init", "metadata"} {
		mapDelete(root, k)
	}
	stripImmutableDisk := func(disk *yaml.Node) {
		if disk == nil {
			return
		}
		mapDelete(disk, "base")
		mapDelete(disk, "base_from_refs")
		if overlay := mapGet(disk, "overlay"); overlay != nil {
			mapDelete(overlay, "base")
			mapDelete(overlay, "base_from_refs")
		}
	}
	if boot := mapGet(root, "boot"); boot != nil {
		mapDelete(boot, "cmdline")
		stripImmutableDisk(mapGet(boot, "root"))
		if disks := mapGet(boot, "disks"); disks != nil && disks.Kind == yaml.SequenceNode {
			for _, disk := range disks.Content {
				stripImmutableDisk(disk)
			}
		}
	}
	return yaml.Marshal(&doc)
}

// mapGet returns the value node for key in a mapping node, or nil.
func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mapDelete removes the key+value pair for key from a mapping node.
func mapDelete(m *yaml.Node, key string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// skeletonCold is the cold-start authoring template (must pass --check=strict).
const skeletonCold = `# sandbox.yaml — sandbox-ctl run --config <this>
# Fill the placeholder paths/values. Schema reference: docs/sandbox.md §3.
resources:
  capacity:    { cpu: 2, memory: 8GiB }   # vCPU / memory the guest sees
  allocatable: { cpu: 2, memory: 8GiB }   # <= capacity (no cgroup ⇒ cpu == capacity.cpu)
network:
  # Optional: omit this block or use "network: {}" to start without a NIC.
  # Otherwise set at most one source: tap (pre-existing host TAP) or tapfd (handoff helper).
  tap: tap0
  # tapfd: { exec: ["connector-ctl", "vswitch", "open-port", "sw0", "--port=1"] }
  # tapfd: { socket: /run/kuasar/connector/sw0/tapfd.sock, request: "VSWITCH=sw0 PORT=1" }
  ip: 169.254.1.1/31                       # guest CIDR; "" → no IP
  hostname: my-sandbox
boot:
  kernel:  file:///opt/sandbox/vmlinux
  runtime: file:///opt/sandbox/sandbox-runtime.bundle
  root:
    base: file:///opt/sandbox/app.erofs    # flattened image (erofs, ro lower)
    overlay:
      # diff omitted → auto-default to /var/lib/sandbox/<sid>/<sid>.overlay.diff
      diff_template: file:///opt/sandbox/overlay-templates/basic-1G.ext4
    # Single-disk mode: omit the overlay block above and give the root disk
    # write capability directly (writable ext4, no overlayfs, no second disk):
    #   diff_template: file:///opt/sandbox/root-templates/app-2G.ext4
    # (base optional; launch.exec required — single-disk has no image config.)
launch:
  exec: /usr/bin/app                       # or omit to use the image's Entrypoint/Cmd (overlay mode only)
  args: []
  env:                                     # portable: re-applied by run --from
    APP_MODE: production
  # ephemeral_env:                        # instance-only: omitted from Sandbox E
  #   REQUEST_TOKEN: current-run-only
  restart: never                           # never | on-failure | always (in-place restart + backoff)
  cgroup_control: false                    # false (default): app sees cgroup /;
                                           # true: delegate empty root, managed processes see /init
  # placeholder: true                      # no exec: empty anchor app (drive the sandbox via exec sessions);
                                           #   mutually exclusive with exec; always restarts (kill ⇒ restart, not reboot)
  # pid_namespace: private                 # private (default) | shared (reuse sandbox-init's reaper)
  # user: "0:0"                            # uid:gid or name:group
  # stop_signal: SIGTERM
  # Companion processes (sidecars), same rootfs/cgroup/network; exit never reboots the sandbox:
  # plugin:
  #   - { exec: /usr/bin/sidecar, restart: always }   # never|on-failure|always (default always)
# Optional guest environment (applied before the app forks):
# mounts:
#   - { target: /tmp,     type: tmpfs, options: "nosuid,nodev,mode=1777" }
#   - { target: /var/log, type: empty }
# files:
#   - { path: /etc/resolv.conf, mode: "0644", content: "nameserver 169.254.169.253\n" }
# ephemeral_files:                         # instance-only; same path overrides files[]
#   - { path: /run/instance-token, mode: "0600", content: "current-run-only" }
# init:  # one-shot, run-to-completion before the app (use plugin[] for long-running)
#   - { exec: /bin/sh, args: ["-c", "echo provisioning"], timeout: 30s }
# ch:                                      # privileged host escape hatch: raw cloud-hypervisor
#   extra_args: ["-vv"]                    # argv additions (cold AND restore). ONE list item =
#                                          # ONE argv token (no splitting/quoting), appended at
#                                          # the very END of the CH command line; "-vv" = CH
#                                          # Debug logging. Flags sandboxer emits itself are
#                                          # rejected.
`

// skeletonRestore is the host-only restore template. Workload state and the
// immutable disk graph come from Snapshot S -> Sandbox E and are not repeated.
const skeletonRestore = `# restore host yaml — sandbox-ctl run --restore <ref> --config <this>
# Cold-only fields (launch, mounts, files, init, plugin, metadata) are rejected.
restore:
  prefetch: off                            # off (default) | memory (current memory self)
resources:
  capacity: { cpu: 2, memory: 8GiB }       # if present, must equal referenced Sandbox E
network:
  # Provider presence must match E's portable network topology. Identity is fresh.
  tap: tap0
  ip: 169.254.4.1/31                       # clone takes a fresh identity
  hostname: clone-1
boot:
  kernel:  file:///opt/sandbox/vmlinux                 # host binding; identity verified against E
  runtime: file:///opt/sandbox/sandbox-runtime.bundle  # host binding; identity verified against E
  root:
    overlay:
      diff_template: file:///opt/sandbox/overlay-templates/basic-1G.ext4
      # Immutable base/base_from_refs are forbidden here; E owns them.
# ch:
#   extra_args: ["-vv"]                    # host-only CH argv additions (one item = one argv
#                                          # token), appended after --restore; logging flags
#                                          # (-v, --log-file) are valid here, vm-config flags
#                                          # are rejected by CH
`
