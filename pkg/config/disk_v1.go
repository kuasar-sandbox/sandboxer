package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/emptyext4"
)

// DiskV1 describes a complete disk, including each immutable dependency in
// newest-to-oldest order. Size asserts logical capacity for an explicit source;
// with no source or parents it creates an empty ext4 disk of that capacity.
type DiskV1 struct {
	Kind         string   `json:"kind" yaml:"kind"`
	Version      int      `json:"version" yaml:"version"`
	Size         *int64   `json:"size,omitempty" yaml:"size,omitempty"`
	Base         string   `json:"base,omitempty" yaml:"base,omitempty"`
	BaseFromRefs []string `json:"base_from_refs,omitempty" yaml:"base_from_refs,omitempty"`
	DiffTemplate string   `json:"diff_template,omitempty" yaml:"diff_template,omitempty"`
}

func (d DiskV1) Validate() error {
	_, err := d.NormalizeCreate()
	return err
}

// NormalizeCreate expands a size-only creation request and canonicalizes mkfs
// aliases before validation or source identity comparison. It does not change
// captured/restored disk graphs, infer a slot's capacity, or repair a bad source.
func (d DiskV1) NormalizeCreate() (DiskV1, error) {
	if d.DiffTemplate != "" {
		normalized, err := NormalizeDiffTemplate(d.DiffTemplate)
		if err != nil {
			return DiskV1{}, fmt.Errorf("diff_template: %w", err)
		}
		d.DiffTemplate = normalized
	} else if d.Base == "" && len(d.BaseFromRefs) == 0 && d.Size != nil {
		if err := emptyext4.ValidateSize(*d.Size); err != nil {
			return DiskV1{}, fmt.Errorf("disk size-only creation: %w", err)
		}
		d.DiffTemplate = (DiffTemplateRef{Kind: DiffTemplateMkfs, Size: *d.Size}).String()
	}
	if err := d.validateNormalized(); err != nil {
		return DiskV1{}, err
	}
	return d, nil
}

func (d DiskV1) validateNormalized() error {
	if d.Kind != "disk" || d.Version != 1 {
		return errors.New("disk: expected kind disk and version 1")
	}
	if d.Size != nil && (*d.Size <= 0 || *d.Size%4096 != 0) {
		return errors.New("disk: size must be positive and 4096-byte aligned")
	}
	if d.Base == "" && (d.DiffTemplate == "" || len(d.BaseFromRefs) > 0) {
		return errors.New("disk: require a base or template and a base for any parents")
	}
	if len(d.BaseFromRefs) > MaxPortableLayerRefs {
		return fmt.Errorf("disk: at most %d parent layers", MaxPortableLayerRefs)
	}
	check := func(raw string) error {
		r, err := manifest.ParseRef(raw)
		if err != nil {
			return err
		}
		if r.String() != raw {
			return errors.New("disk: noncanonical source reference")
		}
		if r.Scheme == manifest.RefSchemeFile && r.Location == "" && !filepath.IsAbs(r.Path) {
			return errors.New("disk: unlocated file reference requires an absolute path")
		}
		return nil
	}
	if d.Base != "" {
		if err := check(d.Base); err != nil {
			return fmt.Errorf("base: %w", err)
		}
	}
	for i, raw := range d.BaseFromRefs {
		if err := check(raw); err != nil {
			return fmt.Errorf("base_from_refs[%d]: %w", i, err)
		}
	}
	if d.DiffTemplate != "" {
		if _, err := ParseDiffTemplate(d.DiffTemplate); err != nil {
			return fmt.Errorf("diff_template: %w", err)
		}
	}
	return nil
}

// Identity fingerprints the source of a validated request independently of its
// optional capacity assertion. Qualified immutable refs survive relocation.
func (d DiskV1) Identity() string {
	if normalized, err := d.NormalizeCreate(); err == nil {
		d = normalized
	}
	layerID := func(raw string) string {
		r, err := manifest.ParseRef(raw)
		if err == nil {
			if r.Scheme == manifest.RefSchemeManifest {
				return "manifest:" + r.Path
			}
			if r.DigestScheme != "" {
				return r.DigestScheme + ":" + r.Digest
			}
		}
		return raw
	}
	templateID := d.DiffTemplate
	if r, err := ParseDiffTemplate(d.DiffTemplate); err == nil {
		templateID = r.Identity()
	}
	graph := []string{templateID, layerID(d.Base)}
	for _, r := range d.BaseFromRefs {
		graph = append(graph, layerID(r))
	}
	b, _ := json.Marshal(graph)
	id := sha256.Sum256(b)
	return hex.EncodeToString(id[:])
}

// UnmarshalYAML enforces the same field and source contract on ordinary
// boot.disks[].disk as on the JSON bind request.
func (d *DiskV1) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("disk: expected a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		switch key := node.Content[i].Value; key {
		case "kind", "version", "size", "base", "base_from_refs", "diff_template":
			if node.Content[i+1].Tag == "!!null" {
				return fmt.Errorf("disk: %s may not be null", key)
			}
		default:
			return fmt.Errorf("disk: unknown field %q", key)
		}
	}
	type wire DiskV1
	var spec wire
	if err := node.Decode(&spec); err != nil {
		return err
	}
	normalized, err := DiskV1(spec).NormalizeCreate()
	if err != nil {
		return err
	}
	*d = normalized
	return nil
}

// UnmarshalJSON rejects ambiguous fields, including duplicates, nulls, and
// alternative capitalization in requests nested inside a control message.
func (d *DiskV1) UnmarshalJSON(raw []byte) error {
	if len(raw) > MaxPortableConfigBytes || !utf8.Valid(raw) {
		return errors.New("disk: invalid UTF-8 or excessive request size")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("disk: expected an object")
	}
	var out DiskV1
	fields := map[string]any{"kind": &out.Kind, "version": &out.Version, "size": &out.Size, "base": &out.Base, "base_from_refs": &out.BaseFromRefs, "diff_template": &out.DiffTemplate}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return err
		}
		name, _ := k.(string)
		dst, ok := fields[name]
		if !ok {
			return fmt.Errorf("disk: unknown or duplicate field %q", name)
		}
		delete(fields, name)
		var v json.RawMessage
		if err = dec.Decode(&v); err != nil {
			return err
		}
		if bytes.Equal(v, []byte("null")) {
			return fmt.Errorf("disk: %s may not be null", name)
		}
		if err = json.Unmarshal(v, dst); err != nil {
			return fmt.Errorf("disk %s: %w", name, err)
		}
	}
	if _, err = dec.Token(); err != nil {
		return err
	}
	if _, err = dec.Token(); err != io.EOF {
		return errors.New("disk: trailing JSON")
	}
	normalized, err := out.NormalizeCreate()
	if err != nil {
		return err
	}
	*d = normalized
	return nil
}
