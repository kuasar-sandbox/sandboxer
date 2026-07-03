package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
)

// snapshotCmd implements `sandbox-ctl snapshot`. --output and --upload
// are strictly mutually exclusive (one required); --resume defaults to
// false (post-snapshot the sandbox is destroyed via /vm.shutdown).
//
// See docs/sandbox.md §2.3.
func snapshotCmd(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	sandboxID := fs.String("sandbox-id", "", "target sandbox id (required)")
	outDir := fs.String("output", "", "local output dir; produces <sid>.snapshot + <sha256>.overlay")
	upload := fs.Bool("upload", false, "ingest snapshot bundle + overlay into manifest store; stdout = snapshot manifest key")
	resume := fs.Bool("resume", false, "keep sandbox running after snapshot (default: destroy via /vm.shutdown)")
	runRoot := fs.String("run-root", "", "tmpfs run root (overrides SANDBOX_RUN_ROOT env; default /run/sandbox)")
	timeoutS := fs.Int("timeout", 0, "seconds to wait for snapshot_done (0 = wait indefinitely; upload can take minutes)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sandboxID == "" {
		fmt.Fprintln(os.Stderr, "snapshot: --sandbox-id required")
		return 2
	}

	// Strict mutual exclusion (docs/sandbox.md §13.3).
	if *upload && *outDir != "" {
		fmt.Fprintln(os.Stderr, "snapshot: --output and --upload are mutually exclusive")
		return 2
	}
	if !*upload && *outDir == "" {
		fmt.Fprintln(os.Stderr, "snapshot: --output and --upload are mutually exclusive; one is required")
		return 2
	}

	if *outDir != "" {
		abs, err := filepath.Abs(*outDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		*outDir = abs
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	rd := *runRoot
	if rd == "" {
		rd = os.Getenv("SANDBOX_RUN_ROOT")
	}
	if rd == "" {
		rd = "/run/sandbox"
	}

	ctlSock := filepath.Join(rd, *sandboxID, "ctl.sock")
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
		Type:        ctl.TypeSnapshotRequest,
		OutDir:      *outDir,
		Upload:      *upload,
		ResumeAfter: *resume,
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
		return 1
	}
	if *upload {
		// Match `manifest-ctl store --put-manifest`: stdout = manifest key,
		// human-readable details to stderr. Lets `MK=$(sandbox-ctl snapshot
		// --upload ...)` work in shell pipelines.
		fmt.Fprintf(os.Stderr, "snapshot upload done: memory_size=%d resident=%d pause_ms=%d dump_ms=%d\n",
			resp.MemorySize, resp.MemoryResident, resp.WallclockPauseMs, resp.WallclockDumpMs)
		if resp.OverlayManifestKey != "" {
			fmt.Fprintf(os.Stderr, "  overlay manifest key: %s\n", resp.OverlayManifestKey)
		}
		if resp.Msg != "" {
			fmt.Fprintf(os.Stderr, "  %s\n", resp.Msg)
		}
		fmt.Println(resp.SnapshotManifestKey)
		return 0
	}

	fmt.Printf("snapshot done: memory_size=%d resident=%d pause_ms=%d dump_ms=%d\n",
		resp.MemorySize, resp.MemoryResident, resp.WallclockPauseMs, resp.WallclockDumpMs)
	if resp.SnapshotPath != "" {
		fmt.Printf("  snapshot bundle: %s\n", resp.SnapshotPath)
	}
	if resp.OverlayPath != "" {
		fmt.Printf("  overlay file:    %s\n", resp.OverlayPath)
	}
	return 0
}
