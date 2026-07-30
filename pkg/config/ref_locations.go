package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
)

// RefLocations maps logical location names from portable file refs to trusted
// host directories supplied by the sandbox-ctl caller.
type RefLocations map[string]string

// Set parses one repeated flag value in the form name=file:///absolute/path.
func (l RefLocations) Set(spec string) error {
	name, rawURI, ok := strings.Cut(spec, "=")
	if !ok || name == "" || rawURI == "" {
		return fmt.Errorf("ref location %q: want name=file:///absolute/path", spec)
	}
	probe := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: "probe", Location: name}
	if err := probe.Validate(); err != nil {
		return fmt.Errorf("ref location %q: %w", spec, err)
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return fmt.Errorf("ref location %q: %w", spec, err)
	}
	if u.Scheme != "file" || u.Host != "" || u.Path == "" || !filepath.IsAbs(u.Path) || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("ref location %q: URI must be file:///absolute/path without host, query, or fragment", spec)
	}
	if _, exists := l[name]; exists {
		return fmt.Errorf("ref location %q: duplicate name %q", spec, name)
	}
	l[name] = filepath.Clean(u.Path)
	return nil
}

func (l RefLocations) String() string {
	names := make([]string, 0, len(l))
	for name := range l {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"=file://"+l[name])
	}
	return strings.Join(parts, ",")
}

// ResolveFile resolves a parsed file ref. Located refs fail closed when their
// trusted mapping is absent. Unlocated relative refs use relativeDir when it is
// non-empty; absolute local refs pass through unchanged.
func (l RefLocations) ResolveFile(ref manifest.Ref, relativeDir string) (string, error) {
	if ref.Scheme != manifest.RefSchemeFile {
		return "", fmt.Errorf("resolve file ref: scheme %q", ref.Scheme)
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	if ref.Location != "" {
		root, ok := l[ref.Location]
		if !ok {
			return "", fmt.Errorf("file ref location %q is not configured", ref.Location)
		}
		return filepath.Join(root, ref.Path), nil
	}
	if filepath.IsAbs(ref.Path) || relativeDir == "" {
		return ref.Path, nil
	}
	return filepath.Join(relativeDir, ref.Path), nil
}
