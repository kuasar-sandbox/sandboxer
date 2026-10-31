package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

type CheckpointDiskBinding struct{ DiffID, BaseID, Path string }
type CheckpointDiskPlan struct {
	Directory string
	Disks     []CheckpointDiskBinding
}

// PrepareCheckpointDiskPlan fixes the paths before capacity and ext4 preflight.
// bases may be bound after cold C0 projection, without changing any paths.
// reopen is populated only after pinning and checking every source version.
func PrepareCheckpointDiskPlan(cfg *config.SandboxConfig, bases *config.PortableSandboxConfig, directory string, reopen []string) (*CheckpointDiskPlan, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	roots := []*config.RootConfig{&cfg.Boot.Root}
	for i := range cfg.Boot.Disks {
		roots = append(roots, &cfg.Boot.Disks[i].RootConfig)
	}
	if len(reopen) != 0 && len(reopen) != len(roots) {
		return nil, errors.New("checkpoint reopen topology mismatch")
	}
	plan := &CheckpointDiskPlan{Directory: directory, Disks: make([]CheckpointDiskBinding, len(roots))}
	for i, root := range roots {
		target := &root.Diff
		if root.Overlay != nil {
			target = &root.Overlay.Diff
		}
		if err := validateCheckpointDiffTarget(*target, directory, len(reopen) != 0); err != nil {
			return nil, err
		}
		id := ""
		if len(reopen) > 0 {
			id = reopen[i]
		} else {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				return nil, err
			}
			id = hex.EncodeToString(b[:])
		}
		if id == "" || strings.ContainsAny(id, "/\\.") {
			return nil, errors.New("invalid checkpoint DiffID")
		}
		path := filepath.Join(directory, "diffs", id+".diff")
		plan.Disks[i] = CheckpointDiskBinding{DiffID: id, Path: path}
		*target = "file://" + path
	}
	if bases != nil {
		if err := plan.Bind(bases); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// validateCheckpointDiffTarget is shared by the read-only shape projection and
// the writer plan. A caller-specified path constrains the target directory; it
// does not authorize taking over an existing DIFF without a checked reopen set.
func validateCheckpointDiffTarget(raw, directory string, reopening bool) error {
	if raw == "" {
		return nil
	}
	scheme, path, ok := config.SchemeAndPath(raw)
	if !ok || scheme != "file" {
		return errors.New("checkpoint requires a local writable DIFF")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if filepath.Dir(abs) != filepath.Join(directory, "diffs") {
		return fmt.Errorf("checkpoint writable DIFF %q is outside its derived directory", path)
	}
	if !reopening {
		if _, err := os.Lstat(abs); err == nil {
			return errors.New("checkpoint refuses implicit writable DIFF takeover; import it as a template")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (p *CheckpointDiskPlan) Bind(bases *config.PortableSandboxConfig) error {
	if bases == nil || !bases.IsCheckpoint() || len(p.Disks) != len(bases.Boot.Disks)+1 {
		return errors.New("checkpoint base graph mismatch")
	}
	p.Disks[0].BaseID = artifact.DiffCheckpointBaseID(bases.Boot.Root)
	for i, d := range bases.Boot.Disks {
		p.Disks[i+1].BaseID = artifact.DiffCheckpointBaseID(d.PortableRootConfig)
	}
	return nil
}

func prepareNamedCapture(req ctl.Request, opts RunOptions, disks []SnapDiskRef) (ctl.Request, error) {
	if req.CheckpointID == "" {
		if req.Type == ctl.TypeCheckpointSnapshotRequest || req.Type == ctl.TypeCheckpointExportRequest {
			return req, errors.New("checkpoint capture requires a nonempty ID")
		}
		return req, nil
	}
	if !opts.EnableCheckpoint {
		return req, ctl.ErrCheckpointUnsupported
	}
	if err := sandboxfile.ValidateCheckpointID(req.CheckpointID); err != nil {
		return req, err
	}
	if req.Upload {
		return req, errors.New("checkpoint cannot be combined with upload")
	}
	for _, d := range disks {
		if d.DiffID == "" || d.Checkpoint == nil {
			return req, ctl.ErrCheckpointUnsupported
		}
	}
	want, err := filepath.Abs(opts.checkpointDir)
	if err != nil {
		return req, err
	}
	if req.OutDir == "" {
		req.OutDir = want
		return req, nil
	}
	got, err := filepath.Abs(req.OutDir)
	if err != nil {
		return req, err
	}
	if got != want {
		return req, errors.New("checkpoint output must be the owner's derived CheckpointDir")
	}
	req.OutDir = want
	return req, nil
}

// startCheckpointMaintenance is called only after the final active disk set is
// bound. A parked pool instance does not own or inspect a business directory.
func (h *SnapshotHandler) startCheckpointMaintenance(ctx context.Context, owner *diffstore.DirectoryOwner) error {
	if h.checkpointMaintenance != nil {
		return errors.New("checkpoint maintenance already initialized")
	}
	directory, err := filepath.Abs(h.CheckpointDir)
	if err != nil {
		return err
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	if owner == nil || owner.Directory != directory {
		return errors.New("checkpoint maintenance requires the target DirectoryOwner")
	}
	active := make([]string, 0, len(h.Disks))
	for _, d := range h.Disks {
		if d.DiffID == "" {
			return errors.New("checkpoint maintenance requires all active DiffIDs")
		}
		active = append(active, d.DiffID)
	}
	m, err := diffstore.NewDirectoryMaintenance(owner, active)
	if err != nil {
		return err
	}
	if err = m.Start(ctx, func(result diffstore.DirectoryGCResult) {
		if result.Error != "" && h.Logf != nil {
			h.Logf("checkpoint maintenance diff=%s: %s", result.DiffID, result.Error)
		}
	}); err != nil {
		return errors.Join(err, m.Close())
	}
	h.checkpointMaintenance = m
	return nil
}

func (h *SnapshotHandler) listDiffCheckpoints(ctx context.Context) ([]artifact.DiffCheckpointRef, error) {
	if !h.EnableCheckpoint {
		return nil, ctl.ErrCheckpointUnsupported
	}
	if h.checkpointMaintenance == nil {
		return nil, errors.New("checkpoint directory maintenance unavailable")
	}
	var refs []artifact.DiffCheckpointRef
	appendRefs := func(id string, ids []string) error {
		for _, cp := range ids {
			refs = append(refs, artifact.DiffCheckpointRef{DiffID: id, ID: cp})
		}
		return nil
	}
	for _, d := range h.Disks {
		if d.ListCheckpoints == nil {
			return nil, ctl.ErrCheckpointUnsupported
		}
		ids, err := d.ListCheckpoints(ctx)
		if err != nil {
			return nil, err
		}
		appendRefs(d.DiffID, ids)
	}
	if err := h.checkpointMaintenance.ListCheckpoints(ctx, appendRefs); err != nil {
		return nil, err
	}
	return refs, nil
}

func (h *SnapshotHandler) deleteDiffCheckpoints(ctx context.Context, refs []ctl.DiffCheckpointRef) error {
	if !h.EnableCheckpoint {
		return ctl.ErrCheckpointUnsupported
	}
	if err := h.capture.begin(); err != nil {
		return err
	}
	defer h.capture.finish(false)
	groups := map[string][]string{}
	for _, r := range refs {
		if r.DiffID == "" || strings.ContainsAny(r.DiffID, "/\\.") {
			return errors.New("invalid DiffID")
		}
		if err := sandboxfile.ValidateCheckpointID(r.ID); err != nil {
			return err
		}
		groups[r.DiffID] = append(groups[r.DiffID], r.ID)
	}
	for id, ids := range groups {
		live := false
		for _, d := range h.Disks {
			if d.DiffID == id {
				if d.DeleteCheckpoints == nil {
					return ctl.ErrCheckpointUnsupported
				}
				if err := d.DeleteCheckpoints(ctx, ids); err != nil {
					return err
				}
				live = true
				break
			}
		}
		if live {
			continue
		}
		if h.checkpointMaintenance == nil {
			return errors.New("checkpoint directory maintenance unavailable")
		}
		if err := h.checkpointMaintenance.DeleteCheckpoints(ctx, id, ids); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) ListCheckpoints(ctx context.Context) ([]artifact.DiffCheckpointRef, error) {
	if err := r.beginOperation(); err != nil {
		return nil, err
	}
	defer r.opsWG.Done()
	r.mu.Lock()
	fn := r.listCheckpoints
	r.mu.Unlock()
	if fn == nil {
		return nil, ctl.ErrCheckpointUnsupported
	}
	return fn(ctx)
}
func (r *Runtime) DeleteCheckpoint(ctx context.Context, ref artifact.DiffCheckpointRef) error {
	if err := r.beginOperation(); err != nil {
		return err
	}
	defer r.opsWG.Done()
	r.mu.Lock()
	fn := r.deleteCheckpoint
	r.mu.Unlock()
	if fn == nil {
		return ctl.ErrCheckpointUnsupported
	}
	return fn(ctx, ref)
}

func validateNamedCaptureGraph(id string, p *config.PortableSandboxConfig, parent string, merged []bool) error {
	if id != "" {
		return p.Validate()
	}
	return snapshot.ValidateExportGraph(p, parent, merged)
}
