package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// snapshotCmd implements `sandbox-ctl snapshot`. --output and --upload
// are strictly mutually exclusive (one required); --resume defaults to
// false (post-snapshot the sandbox is destroyed via /vm.shutdown).
//
// See docs/runtime.md#snapshot.
func snapshotCmd(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	resumeEarly := fs.Bool("resume-early", false, "resume after freezing independent inputs, before persistence completes (implies --resume)")
	checkpoint := fs.String("checkpoint", "", "create a named local disk checkpoint")
	jsonOutput := fs.Bool("json", false, "emit the completed Snapshot S and Sandbox E references as JSON")
	sandboxID := fs.String("sandbox-id", "", "target sandbox id (path fallback when --path-id is omitted)")
	pathID := fs.String("path-id", "", "run-root directory leaf (takes precedence over --sandbox-id)")
	outDir := fs.String("output", "", "local output dir; produces <sid>.snapshot + scheme-qualified content-addressed artifacts")
	upload := fs.Bool("upload", false, "ingest Snapshot S, Sandbox E, and dependencies into the manifest store; stdout = S manifest key")
	mode := fs.String("mode", ctl.SnapshotModeLocal, "local snapshot format: local|bundle (default local)")
	resume := fs.Bool("resume", false, "keep sandbox running after snapshot (default: destroy via /vm.shutdown)")
	dropCaches := fs.Bool("drop-caches", false, "drop guest page, inode, and dentry caches before snapshot (default: preserve guest caches)")
	mergeRef := fs.Bool("merge-ref", true, "merge a local parent memory ref into the new memory self layer")
	noWait := fs.Bool("no-wait-for-previous-memory-save", false, "legacy compatibility option; current runtimes allow earlier checkpoint saves to remain pending")
	runRoot := fs.String("run-root", "", "tmpfs run root (overrides SANDBOX_RUN_ROOT env; default /run/sandbox)")
	timeoutS := fs.Int("timeout", 0, "seconds to wait for snapshot_done (0 = wait indefinitely; upload can take minutes)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *resumeEarly {
		if *checkpoint != "" {
			fmt.Fprintln(os.Stderr, "snapshot: named local checkpoints are not used by early-resume snapshots")
			return 2
		}
		*resume = true
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "snapshot: unexpected positional arguments")
		return 2
	}
	if *sandboxID == "" && *pathID == "" {
		fmt.Fprintln(os.Stderr, "snapshot: --sandbox-id or --path-id is required")
		return 2
	}
	targetPathID, err := resolveTargetPathID(*sandboxID, *pathID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: target path: %v\n", err)
		return 2
	}
	if *timeoutS < 0 {
		fmt.Fprintln(os.Stderr, "snapshot: --timeout must be >= 0")
		return 2
	}
	modeSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "mode" {
			modeSet = true
		}
	})
	if *mode != ctl.SnapshotModeLocal && *mode != ctl.SnapshotModeBundle {
		fmt.Fprintf(os.Stderr, "snapshot: --mode %q invalid (want local|bundle)\n", *mode)
		return 2
	}
	if *upload && modeSet {
		fmt.Fprintln(os.Stderr, "snapshot: --upload and explicit --mode are mutually exclusive")
		return 2
	}

	if *checkpoint != "" && *upload {
		fmt.Fprintln(os.Stderr, "snapshot: --checkpoint and --upload are mutually exclusive")
		return 2
	}
	// Strict mutual exclusion (docs/runtime.md#cli-validation).
	if *upload && *outDir != "" {
		fmt.Fprintln(os.Stderr, "snapshot: --output and --upload are mutually exclusive")
		return 2
	}
	if !*upload && *outDir == "" && *checkpoint == "" {
		fmt.Fprintln(os.Stderr, "snapshot: --output and --upload are mutually exclusive; one is required")
		return 2
	}

	if *outDir != "" {
		abs, err := prepareArtifactOutputDir(*outDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot: output directory: %v\n", err)
			return 1
		}
		*outDir = abs
	}

	rd := *runRoot
	if rd == "" {
		rd = os.Getenv("SANDBOX_RUN_ROOT")
	}
	if rd == "" {
		rd = "/run/sandbox"
	}

	ctlSock := filepath.Join(rd, targetPathID, "ctl.sock")
	c, err := net.DialTimeout("unix", ctlSock, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: dial %s: %v\n", ctlSock, err)
		return 1
	}
	defer c.Close()
	// 0 = no deadline: a real snapshot upload (multi-GiB memory image
	// through chunk/encrypt/store) routinely takes minutes; the server
	// only replies once it finishes. A fixed client deadline would
	// abandon a perfectly healthy in-progress upload. --timeout N is
	// available to opt back into a bound.
	if *timeoutS > 0 {
		_ = c.SetDeadline(time.Now().Add(time.Duration(*timeoutS) * time.Second))
	}

	req := ctl.Request{
		NoWaitForPreviousMemorySave: *noWait,
		Type:                        ctl.TypeSnapshotRequest,
		OutDir:                      *outDir,
		Upload:                      *upload,
		ResumeAfter:                 *resume,
		DropCaches:                  dropCaches,
		MergeRef:                    mergeRef,
	}
	if *resumeEarly {
		req.ResumeEarly = true
	}
	req.HostRetirement, err = ctl.ParseHostRetirement(os.Getenv(ctl.HostRetirementEnvironment))
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: retirement permit: %v\n", err)
		return 79
	}
	if *checkpoint != "" {
		req.Type = ctl.TypeCheckpointSnapshotRequest
		req.CheckpointID = *checkpoint
	}
	if modeSet {
		req.Mode = *mode
	}
	if err := ctl.WriteMessage(c, &req); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: send request: %v\n", err)
		return 1
	}
	var resp ctl.Response
	if err := ctl.ReadMessage(c, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot: recv response: %v\n", err)
		return 1
	}
	if resp.Type == ctl.TypeError {
		fmt.Fprintf(os.Stderr, "snapshot: error from sandbox: %s\n", resp.Msg)
		if errors.Is(ctl.CaptureError(resp), ctl.ErrCheckpointUnsupported) {
			return 78
		}
		if resp.Code == ctl.CaptureRejectedCode {
			return 79
		}
		return 1
	}
	if warning := snapshotDropCachesWarning(*dropCaches, resp.DropCachesResult); warning != "" {
		fmt.Fprintf(os.Stderr, "snapshot: warning: %s\n", warning)
	}
	if *resumeEarly {
		if _, err := captureReport(resp, artifact.RoleSnapshot); err != nil {
			fmt.Fprintf(os.Stderr, "snapshot: invalid runtime result: %v\n", err)
			return 1
		}
		if resp.CleanupWarning != "" {
			fmt.Fprintf(os.Stderr, "snapshot: snapshot saved; cleanup warning: %s\n", resp.CleanupWarning)
		}
	}
	if *jsonOutput {
		if *resumeEarly {
			// These legacy fields measure the pause RPC and sink work, not the
			// application pause. Early resume exposes its explicitly defined timings.
			resp.WallclockPauseMs, resp.WallclockDumpMs = 0, 0
			if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			return 0
		}
		return printCaptureJSON(resp, artifact.RoleSnapshot)
	}
	if *resumeEarly {
		fmt.Fprintf(os.Stderr, "snapshot saved: application_pause_ms=%.3f save_complete_ms=%.3f\n", resp.ApplicationPauseMs, resp.SaveCompleteMs)
		if *upload {
			fmt.Println(resp.SnapshotManifestKey)
		} else {
			fmt.Println(resp.SnapshotRef)
		}
		return 0
	}
	if *upload {
		// Match `manifest-ctl store --put-manifest`: stdout = manifest key,
		// human-readable details to stderr. Lets `MK=$(sandbox-ctl snapshot
		// --upload ...)` work in shell pipelines.
		fmt.Fprintf(os.Stderr, "snapshot upload done: memory_size=%d resident=%d pause_ms=%d dump_ms=%d\n",
			resp.MemorySize, resp.MemoryResident, resp.WallclockPauseMs, resp.WallclockDumpMs)
		if resp.SandboxManifestKey != "" {
			fmt.Fprintf(os.Stderr, "  Sandbox E manifest key: %s\n", resp.SandboxManifestKey)
		}
		if resp.Msg != "" {
			fmt.Fprintf(os.Stderr, "  %s\n", resp.Msg)
		}
		fmt.Println(resp.SnapshotManifestKey)
		return 0
	}

	resp = captureHumanResponse(resp)
	fmt.Printf("snapshot done: memory_size=%d resident=%d pause_ms=%d dump_ms=%d\n",
		resp.MemorySize, resp.MemoryResident, resp.WallclockPauseMs, resp.WallclockDumpMs)
	if resp.SnapshotPath != "" {
		fmt.Printf("  Snapshot S: %s\n", resp.SnapshotPath)
	}
	if resp.SandboxPath != "" {
		fmt.Printf("  Sandbox E:  %s\n", resp.SandboxPath)
	}
	return 0
}

func snapshotDropCachesWarning(dropCaches bool, result proto.DropCachesResult) string {
	switch {
	case !dropCaches && result == proto.DropCachesUnknown:
		return "guest did not report drop_caches capability; it may have dropped caches despite --drop-caches=false"
	case !dropCaches && result != proto.DropCachesSkipped:
		return fmt.Sprintf("guest reported drop_caches=%s despite --drop-caches=false", result)
	case dropCaches && result == proto.DropCachesFailed:
		return "guest failed to write drop_caches; snapshot continued because cache dropping is best-effort"
	case dropCaches && result == proto.DropCachesSkipped:
		return "guest unexpectedly skipped drop_caches although the snapshot requested it"
	default:
		return ""
	}
}
