package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func exportCmd(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	sandboxID := fs.String("sandbox-id", "", "live sandbox id; optional output alias for offline export")
	from := fs.String("from", "", "offline flattened EROFS reference")
	configPath := fs.String("config", "", "offline sandbox.yaml path(s), ':'-separated (or SANDBOX_CONFIG)")
	outDir := fs.String("output", "", "local output directory")
	upload := fs.Bool("upload", false, "upload artifacts to the manifest store")
	mode := fs.String("mode", ctl.SnapshotModeLocal, "local carrier: local|bundle")
	resume := fs.Bool("resume", false, "live export: keep the sandbox running")
	runRoot := fs.String("run-root", "", "tmpfs run root (or SANDBOX_RUN_ROOT; default /run/sandbox)")
	manifestPath := fs.String("manifest-config", "", "storage config YAML (or MANIFEST_CONFIG)")
	timeoutS := fs.Int("timeout", 0, "seconds to wait for export_done (0 = indefinite)")
	refLocations := config.RefLocations{}
	fs.Var(refLocations, "ref-location", "trusted ref location name=file:///absolute/path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	modeSet := false
	fs.Visit(func(f *flag.Flag) { modeSet = modeSet || f.Name == "mode" })
	if *mode != ctl.SnapshotModeLocal && *mode != ctl.SnapshotModeBundle {
		fmt.Fprintf(os.Stderr, "export: --mode %q invalid (want local|bundle)\n", *mode)
		return 2
	}
	if *upload && modeSet {
		fmt.Fprintln(os.Stderr, "export: --upload and explicit --mode are mutually exclusive")
		return 2
	}
	if *upload == (*outDir != "") {
		fmt.Fprintln(os.Stderr, "export: exactly one of --output or --upload is required")
		return 2
	}
	if *from == "" && *sandboxID == "" {
		fmt.Fprintln(os.Stderr, "export: live mode requires --sandbox-id")
		return 2
	}
	if *from != "" && *resume {
		fmt.Fprintln(os.Stderr, "export: offline --from does not accept --resume")
		return 2
	}
	if *from == "" && *configPath != "" {
		fmt.Fprintln(os.Stderr, "export: live mode does not accept --config")
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
	if *from != "" {
		if *configPath == "" {
			*configPath = os.Getenv("SANDBOX_CONFIG")
		}
		if *configPath == "" {
			fmt.Fprintln(os.Stderr, "export: offline mode requires --config or SANDBOX_CONFIG")
			return 2
		}
		return offlineExport(*from, *configPath, *sandboxID, *outDir, *upload, *mode, *manifestPath, refLocations)
	}
	return liveExport(*sandboxID, *outDir, *upload, *mode, modeSet, *resume, *runRoot, *timeoutS)
}

func liveExport(sandboxID, outDir string, upload bool, mode string, modeSet, resume bool, runRoot string, timeoutS int) int {
	if runRoot == "" {
		runRoot = os.Getenv("SANDBOX_RUN_ROOT")
	}
	if runRoot == "" {
		runRoot = "/run/sandbox"
	}
	ctlSock := filepath.Join(runRoot, sandboxID, "ctl.sock")
	conn, err := net.DialTimeout("unix", ctlSock, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: dial %s: %v\n", ctlSock, err)
		return 1
	}
	defer conn.Close()
	if timeoutS > 0 {
		_ = conn.SetDeadline(time.Now().Add(time.Duration(timeoutS) * time.Second))
	}
	req := ctl.Request{Type: ctl.TypeExportRequest, OutDir: outDir, Upload: upload, ResumeAfter: resume}
	if modeSet {
		req.Mode = mode
	}
	if err := ctl.WriteMessage(conn, &req); err != nil {
		fmt.Fprintf(os.Stderr, "export: send request: %v\n", err)
		return 1
	}
	var resp ctl.Response
	if err := ctl.ReadMessage(conn, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "export: receive response: %v\n", err)
		return 1
	}
	if resp.Type == ctl.TypeError {
		fmt.Fprintf(os.Stderr, "export: error from sandbox: %s\n", resp.Msg)
		return 1
	}
	return printExportResult(upload, resp)
}

func offlineExport(raw, configPaths, sandboxID, outDir string, upload bool, mode, manifestPath string, locations config.RefLocations) int {
	ctx := context.Background()
	cfg, err := config.LoadMerged(strings.Split(configPaths, ":"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	manifestCfg, err := config.LoadManifestConfig(manifestPath)
	if err != nil {
		if !errors.Is(err, manifest.ErrConfigNotProvided) {
			fmt.Fprintf(os.Stderr, "export: manifest config: %v\n", err)
			return 1
		}
		manifestCfg = nil
	}
	storage, err := artifact.NewProcessStorage(manifestCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: local crypto: %v\n", err)
		return 1
	}
	defer storage.Close()
	image, err := openFlattenedExportSource(ctx, raw, storage, locations)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		return 1
	}
	defer image.Close()
	opener := sandbox.FileStreamOpener(func(ctx context.Context, path string, ref manifest.Ref) (fetch.Stream, error) {
		return storage.OpenFileWithLocations(ctx, path, ref, locations)
	})
	imageDefaults, err := sandbox.LoadImageConfigBytes(image.ImageConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: config.json: %v\n", err)
		return 1
	}
	if err := sandbox.MaterializeImageDefaults(cfg, imageDefaults); err != nil {
		fmt.Fprintf(os.Stderr, "export: image defaults: %v\n", err)
		return 1
	}
	portable, err := sandbox.PrepareOfflinePortableConfig(cfg, locations, storage.LocalCodec(), storage.LocalRequired(), opener)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: config: %v\n", err)
		return 1
	}
	if sandboxID == "" {
		sandboxID = "offline"
	}
	bundleTarget := !upload && mode == ctl.SnapshotModeBundle
	var admission store.WriteAdmission
	if bundleTarget {
		if manifestCfg == nil || storage.CustomerKeyFunc() == nil {
			fmt.Fprintln(os.Stderr, "export: Bundle mode requires manifest configuration and customer key")
			return 1
		}
		admission, err = manifestCfg.WriteAdmission(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "export: Bundle admission: %v\n", err)
			return 1
		}
	}
	dependencyPlan, err := sandbox.PrepareSandboxDependencyPlan(ctx, sandbox.RunOptions{
		Cfg: cfg, PortableConfig: portable, ManifestCfg: manifestCfg,
		Fetcher: storage.Fetcher(), RefLocations: locations,
		CustomerKeyFn: storage.CustomerKeyFunc(), LocalCodec: storage.LocalCodec(), LocalRequired: storage.LocalRequired(),
	}, make([]bool, 1+len(portable.Boot.Disks)), outDir, admission, bundleTarget)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: dependencies: %v\n", err)
		return 1
	}
	defer dependencyPlan.Close()
	sink, closer, err := newOfflineArtifactSink(ctx, outDir, sandboxID, upload, mode, manifestCfg, storage, admission, dependencyPlan.Refs())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if closer != nil {
		defer closer.Close()
	}
	replacements, err := dependencyPlan.Emit(ctx, sink, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: dependencies: %v\n", err)
		return 1
	}
	portable, err = portable.RewriteDiskArtifactRefs(replacements)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: dependency refs: %v\n", err)
		return 1
	}
	runtimeBytes, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logical, err := sandboxfile.BuildSource(image.Payload, image.ImageConfig, runtimeBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: build Sandbox E: %v\n", err)
		return 1
	}
	ref, path, err := sink.AbsorbSandbox(ctx, logical)
	if err == nil {
		err = sink.CommitSandbox(ctx, ref, path)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "export: write Sandbox E: %v\n", err)
		return 1
	}
	resp := ctl.Response{Type: ctl.TypeExportDone, SandboxRef: ref, SandboxPath: path}
	if parsed, err := manifest.ParseRef(ref); err == nil && parsed.Scheme == manifest.RefSchemeManifest {
		resp.SandboxManifestKey = parsed.Path
	}
	return printExportResult(upload, resp)
}

func openFlattenedExportSource(ctx context.Context, raw string, storage *artifact.ProcessStorage, locations config.RefLocations) (*sandboxfile.FlattenedImage, error) {
	ref, path, err := resolveRunSourceRef(raw, locations)
	if err != nil {
		return nil, err
	}
	var stream fetch.Stream
	if ref.Scheme == manifest.RefSchemeManifest {
		if storage.Fetcher() == nil {
			return nil, errors.New("manifest:// flattened EROFS requires manifest configuration")
		}
		key, err := manifest.ParseKeyRef(ref.Path)
		if err != nil {
			return nil, err
		}
		stream, err = storage.Fetcher().OpenManifest(ctx, key)
		if err != nil {
			return nil, err
		}
	} else {
		stream, err = storage.OpenFileWithLocations(ctx, path, ref, locations)
		if err != nil {
			return nil, err
		}
	}
	return sandboxfile.OpenFlattenedEROFS(ctx, stream)
}

func newOfflineArtifactSink(ctx context.Context, outDir, sandboxID string, upload bool, mode string, manifestCfg *config.ManifestConfig, storage *artifact.ProcessStorage, admission store.WriteAdmission, refs []string) (snapshot.ArtifactSink, io.Closer, error) {
	if upload {
		if manifestCfg == nil || manifestCfg.Store.Endpoint == "" || storage.CustomerKeyFunc() == nil {
			return nil, nil, errors.New("export upload requires manifest store configuration")
		}
		ing, err := manifestCfg.NewIngester(storage.CustomerKeyFunc(), nil)
		if err != nil {
			return nil, nil, err
		}
		return snapshot.NewIngestSink(ing, nil), ing, nil
	}
	if mode == ctl.SnapshotModeBundle {
		bundle, err := snapshot.NewPlannedBundleSink(outDir, sandboxID, manifestCfg, storage.CustomerKeyFunc(), admission, refs, nil)
		return bundle, bundle, err
	}
	return snapshot.NewFileSink(outDir, sandboxID, storage.LocalCodec(), storage.LocalRequired(), nil), nil, nil
}

func printExportResult(upload bool, resp ctl.Response) int {
	if upload {
		fmt.Fprintf(os.Stderr, "export done: pause_ms=%d dump_ms=%d\n", resp.WallclockPauseMs, resp.WallclockDumpMs)
		fmt.Println(resp.SandboxManifestKey)
		return 0
	}
	fmt.Printf("export done: pause_ms=%d dump_ms=%d\n", resp.WallclockPauseMs, resp.WallclockDumpMs)
	if resp.SandboxPath != "" {
		fmt.Printf("  Sandbox E: %s\n", resp.SandboxPath)
	}
	if resp.SandboxRef != "" {
		fmt.Printf("  ref:       %s\n", resp.SandboxRef)
	}
	return 0
}
