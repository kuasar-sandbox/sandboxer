package main

import (
	"encoding/json"
	"fmt"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"os"
	"strings"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
)

func captureReport(resp ctl.Response, role artifact.LogicalRole) (artifact.PublishReport, error) {
	want := ctl.TypeExportDone
	root := resp.SandboxRef
	if role == artifact.RoleSnapshot {
		want, root = ctl.TypeSnapshotDone, resp.SnapshotRef
	}
	if resp.Type != want {
		return artifact.PublishReport{}, fmt.Errorf("unexpected runtime response")
	}
	report, err := (artifact.PublishResult{Role: role, Ref: root, SandboxRef: resp.SandboxRef}).Report()
	if err == nil {
		err = report.ValidateCapture(role)
	}
	return report, err
}

func printCaptureJSON(resp ctl.Response, role artifact.LogicalRole) int {
	report, err := captureReport(resp, role)
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(report)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "capture: invalid runtime result: %v\n", err)
		return 1
	}
	return 0
}

// Keep the established human Bundle output, whose refs were Manifest keys and
// whose path fields were empty. JSON above exposes the actual local binding.
func captureHumanResponse(resp ctl.Response) ctl.Response {
	if ref, err := manifest.ParseRef(resp.SnapshotRef); err == nil && ref.Scheme == manifest.RefSchemeFile && strings.HasSuffix(ref.Path, ".bundle") && ref.DigestScheme == "manifest" {
		resp.SnapshotPath = ""
	}
	if ref, err := manifest.ParseRef(resp.SandboxRef); err == nil && ref.Scheme == manifest.RefSchemeFile && strings.HasSuffix(ref.Path, ".bundle") && ref.DigestScheme == "manifest" {
		resp.SandboxRef, resp.SandboxPath = "manifest://"+ref.Digest, ""
	}
	return resp
}
