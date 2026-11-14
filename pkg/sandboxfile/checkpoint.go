package sandboxfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

const CheckpointName = "checkpoint.json"

type CheckpointDisk struct {
	DiffID string `json:"diff_id"`
}
type CheckpointNamedDisk struct {
	Name   string `json:"name"`
	DiffID string `json:"diff_id"`
}

// Checkpoint is the sole disk-version reference table in a local Sandbox.
// No absolute paths or credentials are embedded in this document.
type Checkpoint struct {
	Version      int                   `json:"version"`
	CheckpointID string                `json:"checkpoint_id"`
	Root         CheckpointDisk        `json:"root"`
	Disks        []CheckpointNamedDisk `json:"disks"`
}

func ValidateCheckpointID(id string) error {
	if len(id) == 0 || len(id) > 128 || !utf8.ValidString(id) || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return errors.New("checkpoint ID must be 1–128 UTF-8 bytes without control characters")
	}
	return nil
}

func validDiffID(id string) bool {
	if len(id) < 1 || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (c *Checkpoint) Validate(p *config.PortableSandboxConfig) error {
	if c == nil || (c.Version != 1 && c.Version != 2) {
		return errors.New("sandbox Checkpoint version is unsupported")
	}
	if err := ValidateCheckpointID(c.CheckpointID); err != nil {
		return err
	}
	if p == nil || !p.IsCheckpoint() {
		return errors.New("sandbox Checkpoint requires zero-self configuration")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if !validDiffID(c.Root.DiffID) {
		return errors.New("sandbox Checkpoint root diff_id is invalid")
	}
	if len(c.Disks) != len(p.Boot.Disks) {
		return errors.New("sandbox Checkpoint disk topology mismatch")
	}
	seen := map[string]bool{c.Root.DiffID: true}
	empty := map[string]bool{}
	for _, d := range p.Boot.Disks {
		if d.Slot != nil && c.Version < 2 {
			return errors.New("checkpoint slot requires version 2")
		}
		empty[d.Name] = d.Unbound()
	}
	names := make(map[string]bool, len(c.Disks))
	for _, d := range c.Disks {
		if empty[d.Name] && !names[d.Name] && d.DiffID == "" {
			names[d.Name] = true
			continue
		}
		if empty[d.Name] || !validDiffID(d.DiffID) || seen[d.DiffID] || names[d.Name] {
			return errors.New("sandbox Checkpoint duplicate or invalid disk identity")
		}
		seen[d.DiffID], names[d.Name] = true, true
	}
	for _, d := range p.Boot.Disks {
		if !names[d.Name] {
			return fmt.Errorf("sandbox Checkpoint missing disk %q", d.Name)
		}
	}
	return nil
}

func ParseCheckpoint(raw []byte, p *config.PortableSandboxConfig) (*Checkpoint, error) {
	if len(raw) == 0 || len(raw) > MaxImageConfigBytes {
		return nil, errors.New("sandbox Checkpoint document outside size limit")
	}
	// Reject duplicate JSON keys, including nested disk objects. DisallowUnknownFields
	// alone does not prevent an attacker replacing an earlier identity.
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueJSON(d); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("sandbox Checkpoint trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var c Checkpoint
	if err := d.Decode(&c); err != nil {
		return nil, err
	}
	if err := c.Validate(p); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(&c)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, canonical) {
		return nil, errors.New("sandbox Checkpoint is not canonical JSON")
	}
	return &c, nil
}

func uniqueJSON(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := k.(string)
			if !ok || seen[name] {
				return errors.New("sandbox Checkpoint duplicate JSON field")
			}
			seen[name] = true
			if err := uniqueJSON(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSON(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("sandbox Checkpoint unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

// BuildCheckpointSource creates the canonical ZIP-only local layout. A
// Checkpoint source deliberately has no disk payload before the ZIP.
func BuildCheckpointSource(ctx context.Context, p *config.PortableSandboxConfig, c *Checkpoint) (sparse.Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.Validate(p); err != nil {
		return nil, err
	}
	runtime, err := config.MarshalPortableSandboxConfig(p)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	tail, err := BuildZIP(map[string][]byte{CheckpointName: raw, config.SandboxRuntimeConfigName: runtime})
	if err != nil {
		return nil, err
	}
	return sparse.NewSource(bytes.NewReader(tail), uint64(len(tail)), nil)
}
