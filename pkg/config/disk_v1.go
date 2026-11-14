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

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
)

// DiskV1 describes a complete disk, including each immutable dependency in
// newest-to-oldest order. Size, when supplied, asserts logical capacity.
type DiskV1 struct {
	Kind         string   `json:"kind" yaml:"kind"`
	Version      int      `json:"version" yaml:"version"`
	Size         *int64   `json:"size,omitempty" yaml:"size,omitempty"`
	Base         string   `json:"base,omitempty" yaml:"base,omitempty"`
	BaseFromRefs []string `json:"base_from_refs,omitempty" yaml:"base_from_refs,omitempty"`
	DiffTemplate string   `json:"diff_template,omitempty" yaml:"diff_template,omitempty"`
}

func (d DiskV1) Validate() error {
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
	check := func(raw string, template bool) error {
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
		if template && (r.Scheme != manifest.RefSchemeFile || r.DigestScheme != "") {
			return errors.New("disk: diff_template requires a file path, optionally with a location")
		}
		return nil
	}
	if d.Base != "" {
		if err := check(d.Base, false); err != nil {
			return fmt.Errorf("base: %w", err)
		}
	}
	for i, raw := range d.BaseFromRefs {
		if err := check(raw, false); err != nil {
			return fmt.Errorf("base_from_refs[%d]: %w", i, err)
		}
	}
	if d.DiffTemplate != "" {
		if err := check(d.DiffTemplate, true); err != nil {
			return fmt.Errorf("diff_template: %w", err)
		}
	}
	return nil
}

// Identity fingerprints the source of a validated request independently of its
// optional capacity assertion. Qualified immutable refs survive relocation.
func (d DiskV1) Identity() string {
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
	graph := []string{d.DiffTemplate, layerID(d.Base)}
	for _, r := range d.BaseFromRefs {
		graph = append(graph, layerID(r))
	}
	b, _ := json.Marshal(graph)
	id := sha256.Sum256(b)
	return hex.EncodeToString(id[:])
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
	if err = out.Validate(); err != nil {
		return err
	}
	*d = out
	return nil
}
