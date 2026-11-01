package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// The CLI owns only argument/configuration adaptation. Disk selection,
// validation, multi-disk recovery and E construction are exclusively the SDK's.
func resetCmd(args []string) int {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	directory := fs.String("checkpoint-dir", "", "parent-owned current disk directory")
	sid := fs.String("sandbox-id", "", "parent sandbox identity")
	instance := fs.String("instance-id", "", "parent object instance")
	run := fs.String("run-id", "", "terminated run identity")
	operation := fs.String("operation-id", "", "durable Reset operation identity")
	manifestPath := fs.String("manifest-config", "", "storage configuration")
	jsonOutput := fs.Bool("json", false, "emit prepared Sandbox E reference")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	identity := artifact.CurrentRun{SandboxID: *sid, InstanceID: *instance, RunID: *run}
	if fs.NArg() != 0 || identity.Validate() != nil || *directory == "" || *operation == "" {
		fmt.Fprintln(os.Stderr, "reset requires --checkpoint-dir, --sandbox-id, --instance-id, --run-id and --operation-id")
		return 2
	}
	cfg, err := config.LoadManifestConfig(*manifestPath)
	if err != nil && !errors.Is(err, manifest.ErrConfigNotProvided) {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	storage, err := artifact.NewProcessStorage(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer storage.Close()
	ctx, stop := commandContext()
	defer stop()
	ref, err := storage.ResetCurrentDisks(ctx, *directory, identity, *operation)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if ctx.Err() != nil {
			// EX_TEMPFAIL is a resumable interruption, not a definitive disk
			// validation failure. The caller must reuse its durable operation ID.
			return 75
		}
		return 1
	}
	if *jsonOutput {
		err = json.NewEncoder(os.Stdout).Encode(struct {
			SandboxRef string `json:"sandbox_ref"`
		}{ref})
	} else {
		_, err = fmt.Fprintln(os.Stdout, ref)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 75 // E may already be durable; its response was not delivered.
	}
	return 0
}
