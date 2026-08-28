package config

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
)

// BindPortableDiskGraph replaces the single reserved self value with the
// current Sandbox source and binds unlocated basename file refs relative to
// the trusted source directory. It mutates only the host runtime config; C0
// remains portable and byte-identical for the whole run.
func BindPortableDiskGraph(runtime *SandboxConfig, selfRef, relativeDir string) error {
	if runtime == nil {
		return errors.New("portable binding: nil runtime config")
	}
	if selfRef == "" {
		return errors.New("portable binding: current Sandbox self ref is required")
	}
	self, err := manifest.ParseRef(selfRef)
	if err != nil {
		return fmt.Errorf("portable binding self: %w", err)
	}
	if self.Scheme == manifest.RefSchemeFile && self.Location == "" && !filepath.IsAbs(self.Path) {
		return errors.New("portable binding self file ref must be absolute or located")
	}

	selfCount := 0
	bind := func(field string, raw *string) error {
		if raw == nil || *raw == "" {
			return nil
		}
		if *raw == "self" {
			*raw = self.String()
			selfCount++
			return nil
		}
		bound, err := bindPortableFileRef(*raw, relativeDir)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		*raw = bound
		return nil
	}
	bindList := func(field string, refs []string) error {
		for i := range refs {
			if err := bind(fmt.Sprintf("%s[%d]", field, i), &refs[i]); err != nil {
				return err
			}
		}
		return nil
	}
	bindRoot := func(field string, root *RootConfig) error {
		if err := bind(field+".base", &root.Base); err != nil {
			return err
		}
		if err := bindList(field+".base_from_refs", root.BaseFromRefs); err != nil {
			return err
		}
		if root.Overlay == nil {
			return nil
		}
		if err := bind(field+".overlay.base", &root.Overlay.Base); err != nil {
			return err
		}
		return bindList(field+".overlay.base_from_refs", root.Overlay.BaseFromRefs)
	}
	if err := bindRoot("boot.root", &runtime.Boot.Root); err != nil {
		return err
	}
	for i := range runtime.Boot.Disks {
		if err := bindRoot(fmt.Sprintf("boot.disks[%d]", i), &runtime.Boot.Disks[i].RootConfig); err != nil {
			return err
		}
	}
	if selfCount != 1 {
		return fmt.Errorf("portable binding: expected self exactly once, got %d", selfCount)
	}
	return nil
}

func bindPortableFileRef(raw, relativeDir string) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", err
	}
	if ref.Scheme != manifest.RefSchemeFile || ref.Location != "" || filepath.IsAbs(ref.Path) {
		return ref.String(), nil
	}
	if relativeDir == "" {
		return "", fmt.Errorf("unlocated local ref %q has no trusted source directory", raw)
	}
	ref.Path = filepath.Join(relativeDir, ref.Path)
	return ref.String(), nil
}
