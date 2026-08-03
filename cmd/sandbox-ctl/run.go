package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/restore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
	"github.com/kuasar-sandbox/sandboxer/pkg/util"
)

// runCmd implements `sandbox-ctl run`. With --restore=<ref> it switches
// to restore mode (snapshot bundle); without it goes cold-start.
//
// stdio model: see docs/sandbox.md §2.2. The app's stdin/stdout/stderr
// (pipe mode) or a single pty (--tty) travel over the vsock stdio MUX.
// --tty defaults to auto-detect (on iff stdin and stdout are both
// terminals); pipe-mode --stdin/--stdout/--stderr and their -from/-to
// variants force pipe mode. The guest kernel dmesg is a separate channel
// — --console off|default|file=PATH (default: our stderr).
//
// --ch-binary defaults to a precedence chain: SANDBOX_CH_PATH env →
// directory of running sandbox-ctl executable → exec.LookPath.
//
// --run-root defaults to SANDBOX_RUN_ROOT env or "/run/sandbox" (tmpfs:
// sockets + snap staging). --base-root defaults to SANDBOX_BASE_ROOT env or
// "/var/lib/sandbox" (on-disk: the overlay diff).
func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)

	configPath := fs.String("config", "", "sandbox.yaml path(s), ':'-separated, merged front-to-back (or SANDBOX_CONFIG env)")
	manifestPath := fs.String("manifest-config", "", "path to manifest config YAML (overrides MANIFEST_CONFIG env; required for manifest:// resources)")
	sandboxID := fs.String("sandbox-id", "", "sandbox id (overrides sandbox.yaml)")
	chBinary := fs.String("ch-binary", "", "path to cloud-hypervisor binary (default: SANDBOX_CH_PATH env, exe-dir, or PATH)")
	runRoot := fs.String("run-root", "", "tmpfs run root: sockets + snap staging (overrides SANDBOX_RUN_ROOT env; default /run/sandbox)")
	baseRoot := fs.String("base-root", "", "on-disk base root: overlay diff (overrides SANDBOX_BASE_ROOT env; default /var/lib/sandbox)")
	refLocations := config.RefLocations{}
	fs.Var(refLocations, "ref-location", "trusted ref location name=file:///absolute/path (repeatable)")

	cgroupPath := fs.String("cgroup-path", "", "absolute cgroup v2 directory to join (must already exist); empty = no cgroup")
	cgroupAdopt := fs.Bool("cgroup-adopt", false, "adopt the cgroup sandbox-ctl is already in (its systemd unit's cgroup): write limits there, do NOT move CH; resolves the cgroup path from /proc/self/cgroup")
	statsJSON := fs.String("stats-json", "", "if set, write per-backend + uffd stats as JSON to this path on shutdown")
	readyFD := fs.Int("ready-fd", -1, "write control_ready and ready events to an inherited fd")

	restoreRef := fs.String("restore", "", "snapshot reference (file path or manifest://<hex>) — switches to restore mode")

	// stdio flags. The bool flags (--stdin/--stdout/--stderr/--tty) are
	// tri-state — "not set" must be distinguishable from "set to false"
	// — so we read fs.Visit after Parse to wrap them in *bool.
	stdinFlag := fs.Bool("stdin", false, "pipe mode: connect app stdin to sandbox-ctl's stdin (default off → /dev/null)")
	stdoutFlag := fs.Bool("stdout", true, "pipe mode: app stdout → sandbox-ctl stdout (default on; --stdout=false discards)")
	stderrFlag := fs.Bool("stderr", true, "pipe mode: app stderr → sandbox-ctl stderr (default on; --stderr=false discards)")
	stdinFrom := fs.String("stdin-from", "", "pipe mode: app stdin reads from FILE (implies --stdin)")
	stdoutTo := fs.String("stdout-to", "", "pipe mode: app stdout → FILE or journald=TAG (implies --stdout)")
	stderrTo := fs.String("stderr-to", "", "pipe mode: app stderr → FILE or journald=TAG (implies --stderr)")
	ttyFlag := fs.Bool("tty", false, "give the app a pty + put our terminal in raw mode (default: auto = on iff stdin&stdout are terminals; mutually exclusive with --stdin/--stdout/--stderr/--*-from/--*-to)")
	console := fs.String("console", "default", "guest kernel dmesg sink: off | default (our stderr) | file=PATH | journald=TAG")

	// Reliability backstop: after N consecutive failed pings, sandbox-ctl
	// SIGTERMs CH so cmd.Wait returns rather than hanging on a wedged-
	// but-alive guest. 0 = disabled (default — wait for outer signal).
	// With the default 1 s ping interval, 30 ≈ 30 s of unreachability.
	pingFatal := fs.Int("ping-fatal-threshold", 0,
		"consecutive ping failures before SIGTERMing CH (overrides SANDBOX_PING_FATAL_THRESHOLD env; 0 disables)")

	// Periodic lazy-load stats: every interval, log uffd page-in + vhost read
	// rates, in-flight/queue gauges, and fetch latency so a slow remote/cache
	// is visible in real time. Idle intervals are skipped (quiet once warm).
	statsInterval := fs.Duration("stats-interval", 30*time.Second,
		"periodic lazy-load stats log interval (overrides SANDBOX_STATS_INTERVAL env; 0 disables)")

	// --connect LOCAL:TARGET (guest dials) or LOCAL::TARGET (guest accepts)
	// — port-forward a host-local endpoint to a guest-side endpoint
	// (repeatable). LOCAL is a UDS path or fd=N (an inherited, already-
	// listening socket); TARGET is host:port (tcp) or an absolute/abstract
	// path (unix, when it starts with '/' or '@').
	var forwards forwardFlags
	fs.Var(&forwards, "connect",
		"port-forward LOCAL:TARGET (guest dials) or LOCAL::TARGET (guest accepts); "+
			"LOCAL = UDS path or fd=N; TARGET = host:port or /path|@abstract (repeatable)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	readyFDSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "ready-fd" {
			readyFDSet = true
		}
	})
	var readiness *readinessFDWriter
	if readyFDSet {
		var err error
		readiness, err = newReadinessFDWriter(*readyFD, func(format string, args ...any) {
			log.Printf("[sandbox-ctl] "+format, args...)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-ctl run: %v\n", err)
			return 2
		}
		// Every subsequent return closes the descriptor, so startup failure is
		// represented by EOF (possibly after control_ready).
		defer readiness.Close()
	}
	var notifyReadiness sandbox.ReadinessNotify
	if readiness != nil {
		notifyReadiness = readiness.Notify
	}
	// --ping-fatal-threshold precedence: flag > env > 0 (disabled).
	pingFatalSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "ping-fatal-threshold" {
			pingFatalSet = true
		}
	})
	if !pingFatalSet {
		if s := os.Getenv("SANDBOX_PING_FATAL_THRESHOLD"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				fmt.Fprintf(os.Stderr, "sandbox-ctl run: bad SANDBOX_PING_FATAL_THRESHOLD=%q (want non-negative int)\n", s)
				return 2
			}
			*pingFatal = n
		}
	}

	// --stats-interval precedence: flag > SANDBOX_STATS_INTERVAL env > 30s default.
	statsIntervalSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "stats-interval" {
			statsIntervalSet = true
		}
	})
	if !statsIntervalSet {
		if s := os.Getenv("SANDBOX_STATS_INTERVAL"); s != "" {
			d, err := time.ParseDuration(s)
			if err != nil || d < 0 {
				fmt.Fprintf(os.Stderr, "sandbox-ctl run: bad SANDBOX_STATS_INTERVAL=%q (want non-negative duration)\n", s)
				return 2
			}
			*statsInterval = d
		}
	}

	// Detect which tri-state bool flags were explicitly set.
	var stdinSet, stdoutSet, stderrSet, ttySet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "stdin":
			stdinSet = true
		case "stdout":
			stdoutSet = true
		case "stderr":
			stderrSet = true
		case "tty":
			ttySet = true
		}
	})
	tri := func(set bool, v *bool) *bool {
		if !set {
			return nil
		}
		x := *v
		return &x
	}
	stdioMode, err := stdio.FromFlags(
		tri(stdinSet, stdinFlag), tri(stdoutSet, stdoutFlag), tri(stderrSet, stderrFlag),
		*stdinFrom, *stdoutTo, *stderrTo, tri(ttySet, ttyFlag), *console)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-ctl run: %v\n", err)
		return 2
	}

	// Resolve --run-root / --base-root (flag > env > default) first — the
	// config-socket pidfile lives under run-root.
	rd := *runRoot
	if rd == "" {
		rd = os.Getenv("SANDBOX_RUN_ROOT")
	}
	if rd == "" {
		rd = "/run/sandbox"
	}
	br := *baseRoot
	if br == "" {
		br = os.Getenv("SANDBOX_BASE_ROOT")
	}
	if br == "" {
		br = "/var/lib/sandbox"
	}

	// Resolve --ch-binary precedence: explicit flag > $SANDBOX_CH_PATH >
	// directory of running sandbox-ctl executable > exec.LookPath.
	chBin := *chBinary
	if chBin == "" {
		chBin, err = locateCH()
		if err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-ctl run: cloud-hypervisor not found: %v\n", err)
			return 1
		}
	}

	// Load the sandbox + manifest config from --config / --manifest-config (files
	// + env). The orchestrator delivers per-sandbox config by writing these files;
	// the secret manifest key rides in the MANIFEST_KEY env (resolved by
	// pkg/manifest). orchestrator-ctl run-task sets both up before exec'ing here.
	if *configPath == "" {
		*configPath = os.Getenv("SANDBOX_CONFIG")
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "sandbox-ctl run: --config or SANDBOX_CONFIG required")
		return 2
	}
	// --config accepts ':'-separated paths, deep-merged front-to-back.
	cfg, err := config.LoadMerged(strings.Split(*configPath, ":"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	manifestCfg, err := config.LoadManifestConfig(*manifestPath)
	if err != nil {
		if !errors.Is(err, manifest.ErrConfigNotProvided) {
			fmt.Fprintf(os.Stderr, "[sandbox-ctl] manifest config: %v\n", err)
			return 1
		}
		manifestCfg = nil
	}

	// cgroup overrides.
	if *cgroupPath != "" {
		cfg.Resources.Control.CgroupPath = *cgroupPath
	}
	if *cgroupAdopt {
		p, perr := resctl.SelfCgroupV2Path()
		if perr != nil {
			fmt.Fprintf(os.Stderr, "[sandbox-ctl] --cgroup-adopt: %v\n", perr)
			return 1
		}
		cfg.Resources.Control.CgroupPath = p
		cfg.Resources.Control.Adopt = true
	}

	restoreR := *restoreRef

	// Signal handling lives in pkg/sandbox (lifecycle.go /
	// restore.go) — they own the CH process and forward SIGTERM/SIGINT
	// to it with SIGKILL escalation. So this layer just passes a plain
	// context.
	ctx := context.Background()

	// Restore mode dispatch.
	if restoreR != "" {
		return runRestore(ctx, cfg, manifestCfg, restoreR,
			*sandboxID, chBin, rd, br, *statsJSON, stdioMode, *pingFatal, *statsInterval, forwards, refLocations, notifyReadiness)
	}

	exit, err := sandbox.Run(ctx, sandbox.RunOptions{
		Cfg:                cfg,
		ManifestCfg:        manifestCfg,
		RefLocations:       refLocations,
		SandboxID:          *sandboxID,
		CHBinary:           chBin,
		RuntimeRoot:        rd,
		BaseRoot:           br,
		StatsJSONPath:      *statsJSON,
		StatsInterval:      *statsInterval,
		StdioMode:          stdioMode,
		PingFatalThreshold: *pingFatal,
		Forwards:           forwards,
		NotifyReadiness:    notifyReadiness,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return exit
}

// runRestore parses the snapshot reference and dispatches to restore.Run.
func runRestore(ctx context.Context, cfg *config.SandboxConfig, manifestCfg *config.ManifestConfig,
	ref string, sandboxID, chBin, runDir, baseRoot, statsJSON string, stdioMode stdio.Mode, pingFatal int,
	statsInterval time.Duration, forwards []sandbox.ForwardSpec, refLocations config.RefLocations,
	notifyReadiness sandbox.ReadinessNotify,
) int {
	// Validate host-only restore policy before inspecting the remote reference or
	// constructing a Fetcher. restore.Run repeats this at its public boundary,
	// but the CLI owns NewFetcher and must not dial for an invalid config.
	if _, err := config.ParsePrefetchMode(cfg.Restore.Prefetch); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var (
		snapshotPath string
		snapshotKey  string
		snapshotRef  string
	)
	if strings.HasPrefix(ref, "manifest://") || strings.HasPrefix(ref, "file://") {
		parsed, err := manifest.ParseRef(ref)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		switch parsed.Scheme {
		case manifest.RefSchemeManifest:
			snapshotRef = parsed.String()
			snapshotKey = parsed.Path
			if manifestCfg == nil {
				fmt.Fprintln(os.Stderr, "sandbox-ctl run --restore=manifest://: requires --manifest-config or MANIFEST_CONFIG")
				return 2
			}
		case manifest.RefSchemeFile:
			snapshotPath, err = refLocations.ResolveFile(parsed, "")
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			if parsed.Location != "" || parsed.Digest != "" {
				snapshotRef = parsed.String()
			}
		}
	} else {
		snapshotPath = ref
	}

	var (
		fetcher manifest.FetcherCloser
		err     error
	)
	if manifestCfg != nil {
		fetcher, err = manifestCfg.NewFetcher()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer fetcher.Close()
	}

	exit, err := restore.Run(ctx, restore.Options{
		SnapshotPath:        snapshotPath,
		SnapshotManifestKey: snapshotKey,
		SnapshotRef:         snapshotRef,
		HostCfg:             cfg,
		ManifestCfg:         manifestCfg,
		Fetcher:             fetcher,
		RefLocations:        refLocations,
		SandboxID:           sandboxID,
		CHBinary:            chBin,
		RuntimeRoot:         runDir,
		BaseRoot:            baseRoot,
		StatsJSONPath:       statsJSON,
		StatsInterval:       statsInterval,
		StdioMode:           stdioMode,
		PingFatalThreshold:  pingFatal,
		Forwards:            forwards,
		NotifyReadiness:     notifyReadiness,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return exit
}

// forwardFlags collects repeated `--connect LOCAL:TARGET` / `LOCAL::TARGET`
// directives, parsing each into a sandbox.ForwardSpec as it is seen.
type forwardFlags []sandbox.ForwardSpec

func (f *forwardFlags) String() string {
	parts := make([]string, len(*f))
	for i, s := range *f {
		parts[i] = s.Raw
	}
	return strings.Join(parts, ",")
}

func (f *forwardFlags) Set(v string) error {
	spec, err := sandbox.ParseForwardSpec(v)
	if err != nil {
		return err
	}
	*f = append(*f, spec)
	return nil
}

// locateCH resolves the cloud-hypervisor binary with a fixed
// precedence: $SANDBOX_CH_PATH > directory of running sandbox-ctl
// executable > $PATH. The exe-dir fallback covers the release-bundle
// layout where helper binaries ship alongside the tool under
// bin/<arch>/; $PATH covers distro-installed equivalents.
func locateCH() (string, error) {
	if p := os.Getenv("SANDBOX_CH_PATH"); p != "" {
		return p, nil
	}
	p, err := util.LocateBinary("cloud-hypervisor")
	if err != nil {
		return "", fmt.Errorf("%w; or set $SANDBOX_CH_PATH", err)
	}
	return p, nil
}
