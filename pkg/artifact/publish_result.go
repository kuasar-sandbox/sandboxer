package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
)

// PublishReport is the public S/E publication contract. RemovedRefs describes
// references leaving this operation's known topology; it never authorizes deletion.
type PublishReport struct {
	SnapshotRef string   `json:"snapshotRef,omitempty"`
	SandboxRef  string   `json:"sandboxRef"`
	RemovedRefs []string `json:"removedRefs"`
}

// PublicRef projects a local reference into the caller's checkpoint namespace.
// Identity qualifiers are preserved; projection performs no filesystem I/O.
func PublicRef(raw string) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", err
	}
	if ref.Scheme == manifest.RefSchemeFile && ref.Location == "" {
		ref.Path = filepath.Base(ref.Path)
	}
	if ref.Scheme == manifest.RefSchemeFile && (ref.Path == "." || ref.Path == ".." || strings.ContainsAny(ref.Path, "/\\") || strings.IndexFunc(ref.Path, unicode.IsControl) >= 0) {
		return "", errors.New("publication reference must have a safe basename")
	}
	return ref.String(), nil
}

func (r PublishResult) Report() (PublishReport, error) {
	out := PublishReport{RemovedRefs: make([]string, 0, len(r.RemovedRefs))}
	var err error
	switch r.Role {
	case RoleSandbox:
		out.SandboxRef, err = PublicRef(r.Ref)
	case RoleSnapshot:
		out.SnapshotRef, err = PublicRef(r.Ref)
		if err == nil {
			out.SandboxRef, err = PublicRef(r.SandboxRef)
		}
	default:
		return out, fmt.Errorf("publication report: unsupported role %q", r.Role)
	}
	if err != nil {
		return PublishReport{}, err
	}
	seen := make(map[string]bool)
	for _, raw := range r.RemovedRefs {
		ref, err := PublicRef(raw)
		if err != nil {
			return PublishReport{}, err
		}
		if !seen[ref] {
			out.RemovedRefs = append(out.RemovedRefs, ref)
			seen[ref] = true
		}
	}
	sort.Strings(out.RemovedRefs)
	return out, nil
}

// Validate checks the exact expected role, portable roots and public ref safety.
// It intentionally does not open roots or verify payloads a second time.
func (r PublishReport) Validate(role LogicalRole) error {
	if r.RemovedRefs == nil {
		return errors.New("publication report: removedRefs must be an array")
	}
	if role != RoleSandbox && role != RoleSnapshot {
		return errors.New("publication report: invalid role")
	}
	if (role == RoleSnapshot) != (r.SnapshotRef != "") {
		return errors.New("publication report: root role mismatch")
	}
	checkRoot := func(raw string, role LogicalRole) error {
		ref, err := manifest.ParseRef(raw)
		if err != nil || !ref.Portable() {
			return errors.New("publication report: missing or nonportable root")
		}
		if ref.Scheme == manifest.RefSchemeFile && !strings.HasSuffix(ref.Path, ".bundle") && !strings.HasSuffix(ref.Path, "."+string(role)) {
			return errors.New("publication report: file root role mismatch")
		}
		safe, err := PublicRef(raw)
		if err != nil || safe != raw {
			return errors.New("publication report: unsafe root")
		}
		return nil
	}
	if err := checkRoot(r.SandboxRef, RoleSandbox); err != nil {
		return err
	}
	if role == RoleSnapshot {
		if err := checkRoot(r.SnapshotRef, RoleSnapshot); err != nil {
			return err
		}
	}
	for i, raw := range r.RemovedRefs {
		if raw == r.SandboxRef || raw == r.SnapshotRef {
			return errors.New("publication report: removed reference is a final root")
		}
		safe, err := PublicRef(raw)
		if err != nil || safe != raw {
			return errors.New("publication report: unsafe removed reference")
		}
		if i > 0 && r.RemovedRefs[i-1] >= raw {
			return errors.New("publication report: removedRefs must be sorted and unique")
		}
	}
	return nil
}

// DecodePublishReport accepts exactly one JSON document with the public fields.
func DecodePublishReport(data []byte, role LogicalRole) (PublishReport, error) {
	var out PublishReport
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return out, errors.New("publication report: expected JSON object")
	}
	fields := make(map[string]json.RawMessage, 3)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return out, err
		}
		name, ok := token.(string)
		if !ok {
			return out, errors.New("publication report: invalid field")
		}
		if _, exists := fields[name]; exists {
			return out, errors.New("publication report: duplicate field")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return out, err
		}
		fields[name] = value
	}
	if _, err := dec.Token(); err != nil {
		return out, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return out, errors.New("publication report: trailing output")
	}
	expected := 2
	if role == RoleSnapshot {
		expected = 3
	}
	if len(fields) != expected || fields["sandboxRef"] == nil || fields["removedRefs"] == nil || (role == RoleSnapshot && fields["snapshotRef"] == nil) {
		return out, errors.New("publication report: invalid fields for role")
	}
	if err := json.Unmarshal(fields["sandboxRef"], &out.SandboxRef); err != nil {
		return out, err
	}
	if role == RoleSnapshot {
		if err := json.Unmarshal(fields["snapshotRef"], &out.SnapshotRef); err != nil {
			return out, err
		}
	}
	if err := json.Unmarshal(fields["removedRefs"], &out.RemovedRefs); err != nil {
		return out, err
	}
	if err := out.Validate(role); err != nil {
		return PublishReport{}, err
	}
	return out, nil
}

type publicationRef struct {
	raw   string
	scope publishScope
}
type publicationReport struct{ before, after []publicationRef }

func (p *Publisher) operation() *Publisher {
	op := *p
	// Publication caches retain their established lifetime. The report and cycle
	// detector belong only to this call, including hits in those caches.
	if op.memoMu == nil {
		op.memoMu = &sync.Mutex{}
	}
	if op.diskMemo == nil {
		op.diskMemo = make(map[string]string)
	}
	if op.memoryMemo == nil {
		op.memoryMemo = make(map[string]string)
	}
	op.visiting = make(map[string]struct{})
	op.report = &publicationReport{}
	return &op
}
func (p *Publisher) original(raw string, scope publishScope) {
	if raw != "" && raw != "self" {
		p.report.before = append(p.report.before, publicationRef{raw, scope})
	}
}
func (p *Publisher) retained(raw string, scope publishScope) {
	if raw != "" && raw != "self" {
		p.report.after = append(p.report.after, publicationRef{raw, scope})
	}
}

func scopedRef(raw string, scope publishScope) string {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return raw
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		if bound, ok := scope.bindings[raw]; ok {
			return bound
		}
		// Membership of the already-open current carrier is metadata, not a scan.
		// In skip mode this also binds an old leaf without opening that leaf.
		if scope.current != nil {
			key, err := manifest.ParseKeyRef(ref.Path)
			if err == nil && scope.current.HasManifest(key) {
				return scopedRef(bundleMemberRef(scope.carrier, key), scope)
			}
		}
		return raw
	}
	if ref.Location == "" {
		if !filepath.IsAbs(ref.Path) {
			ref.Path = filepath.Join(scope.relativeDir, ref.Path)
		}
		if path, err := filepath.Abs(ref.Path); err == nil {
			ref.Path = filepath.Clean(path)
		}
	}
	return ref.String()
}

// rememberSelection is called only when publication already selects a source.
func (scope publishScope) rememberSelection(raw string, selected manifestbundle.ManifestSource) {
	if scope.bindings == nil {
		return
	}
	bound := raw
	if selected.Reader != nil {
		carrier := selectedBundleCarrier(scope.carrier, selected.Ref)
		if ref, err := manifest.ParseRef(carrier); err == nil {
			key, _ := manifest.ParseRef(raw)
			ref.DigestScheme, ref.Digest = "manifest", key.Path
			bound = scopedRef(ref.String(), scope)
		}
	}
	scope.bindings[raw] = bound
}

func (p *Publisher) result(role LogicalRole, ref, sandboxRef string) PublishResult {
	p.retained(ref, publishScope{})
	final := make(map[string]bool, len(p.report.after))
	for _, item := range p.report.after {
		final[scopedRef(item.raw, item.scope)] = true
	}
	removed := make(map[string]bool)
	for _, item := range p.report.before {
		normalized := scopedRef(item.raw, item.scope)
		if !final[normalized] {
			removed[normalized] = true
		}
	}
	refs := make([]string, 0, len(removed))
	for raw := range removed {
		refs = append(refs, raw)
	}
	sort.Strings(refs)
	if role == RoleSandbox {
		sandboxRef = ref
	}
	return PublishResult{Role: role, Ref: ref, SandboxRef: sandboxRef, RemovedRefs: refs}
}
