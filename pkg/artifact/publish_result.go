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

// PublishReport is the public S/E publication contract. Refs distinguishes a
// complete graph from a root-only result. RemovedRefs is an operation delta,
// never permission to delete a shared artifact. A nonnil empty delta is encoded
// as []; an unknown graph omits it. Keep this type free of MarshalJSON methods:
// platform responses embed it alongside their own result field.
type PublishReport struct {
	Refs         bool     `json:"refs"`
	SnapshotRef  string   `json:"snapshotRef,omitempty"`
	SnapshotRefs []string `json:"snapshotRefs,omitempty"`
	SandboxRef   string   `json:"sandboxRef"`
	SandboxRefs  []string `json:"sandboxRefs,omitempty"`
	RemovedRefs  []string `json:"removedRefs,omitzero"`
}

// CaptureReport preserves the existing local export and snapshot wire format.
// Those commands do not adopt the publication reference-report contract.
type CaptureReport struct {
	SnapshotRef string   `json:"snapshotRef,omitempty"`
	SandboxRef  string   `json:"sandboxRef"`
	RemovedRefs []string `json:"removedRefs"`
}

func (r CaptureReport) ValidateCapture(role LogicalRole) error {
	return (PublishReport{SnapshotRef: r.SnapshotRef, SandboxRef: r.SandboxRef, RemovedRefs: r.RemovedRefs}).ValidateCapture(role)
}

func (r PublishResult) withSnapshotRuntime(runtime manifest.RuntimeIdentity, original string) (PublishResult, error) {
	annotate := func(raw string) (string, error) {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return "", err
		}
		ref.Runtime = runtime
		return ref.String(), nil
	}
	base := r.Ref
	var err error
	r.Ref, err = annotate(r.Ref)
	if err != nil {
		return PublishResult{}, err
	}
	for i, raw := range r.SnapshotRefs {
		if raw == base {
			r.SnapshotRefs[i] = r.Ref
		}
	}
	for i, raw := range r.RemovedRefs {
		if raw == original {
			r.RemovedRefs[i], err = annotate(raw)
			if err != nil {
				return PublishResult{}, err
			}
		}
	}
	sort.Strings(r.SnapshotRefs)
	sort.Strings(r.RemovedRefs)
	return r, nil
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

func publicReferences(refs []string) ([]string, error) {
	out := make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, raw := range refs {
		ref, err := PublicRef(raw)
		if err != nil {
			return nil, err
		}
		if !seen[ref] {
			out = append(out, ref)
			seen[ref] = true
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r PublishResult) Report() (PublishReport, error) {
	out := PublishReport{Refs: r.Refs}
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
	if !r.Refs {
		return out, nil
	}
	if r.Role == RoleSnapshot {
		out.SnapshotRefs, err = publicReferences(r.SnapshotRefs)
		if err != nil {
			return PublishReport{}, err
		}
	}
	out.SandboxRefs, err = publicReferences(r.SandboxRefs)
	if err != nil {
		return PublishReport{}, err
	}
	out.RemovedRefs, err = publicReferences(r.RemovedRefs)
	if err != nil {
		return PublishReport{}, err
	}
	if err := out.validate(r.Role, true); err != nil {
		return PublishReport{}, err
	}
	return out, nil
}

func (r PublishResult) CaptureReport() (CaptureReport, error) {
	roots, err := (PublishResult{Role: r.Role, Ref: r.Ref, SandboxRef: r.SandboxRef}).Report()
	if err != nil {
		return CaptureReport{}, err
	}
	return CaptureReport{SnapshotRef: roots.SnapshotRef, SandboxRef: roots.SandboxRef, RemovedRefs: []string{}}, nil
}

// Validate checks role, complete-set membership and public reference safety.
// It does not open roots or verify payloads a second time.
func (r PublishReport) Validate(role LogicalRole) error { return r.validate(role, false) }

// ValidateCapture validates the unchanged roots-only capture contract.
func (r PublishReport) ValidateCapture(role LogicalRole) error {
	if r.Refs || r.SnapshotRefs != nil || r.SandboxRefs != nil {
		return errors.New("capture report: unexpected publication reference groups")
	}
	if len(r.RemovedRefs) != 0 {
		return errors.New("capture report: unexpected removed references")
	}
	return r.validateLegacy(role, true)
}

func (r PublishReport) validateRoots(role LogicalRole, local bool) error {
	if role != RoleSandbox && role != RoleSnapshot {
		return errors.New("publication report: invalid role")
	}
	if (role == RoleSnapshot) != (r.SnapshotRef != "") {
		return errors.New("publication report: root role mismatch")
	}
	checkRoot := func(raw string, role LogicalRole) error {
		ref, err := manifest.ParseRef(raw)
		if err != nil || (!ref.Portable() && !(local && ref.Scheme == manifest.RefSchemeFile && ref.Digest != "")) {
			return errors.New("publication report: missing or nonportable root")
		}
		if ref.Scheme == manifest.RefSchemeFile && !strings.HasSuffix(ref.Path, ".bundle") && !strings.HasSuffix(ref.Path, "."+string(role)) {
			return errors.New("publication report: file root role mismatch")
		}
		if role != RoleSnapshot && !ref.Runtime.IsZero() {
			return errors.New("publication report: only a snapshot root may declare a runtime")
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
		return checkRoot(r.SnapshotRef, RoleSnapshot)
	}
	return nil
}

func validateReferenceSet(name string, refs []string, required bool) error {
	if refs == nil {
		return fmt.Errorf("publication report: %s must be a nonnull array", name)
	}
	if required && len(refs) == 0 {
		return fmt.Errorf("publication report: %s must be nonempty", name)
	}
	for i, raw := range refs {
		safe, err := PublicRef(raw)
		if err != nil || safe != raw {
			return fmt.Errorf("publication report: unsafe %s reference", name)
		}
		if i > 0 && refs[i-1] >= raw {
			return fmt.Errorf("publication report: %s must be sorted and unique", name)
		}
	}
	return nil
}

func containsReference(refs []string, raw string) bool {
	i := sort.SearchStrings(refs, raw)
	return i < len(refs) && refs[i] == raw
}

func (r PublishReport) validate(role LogicalRole, local bool) error {
	if err := r.validateRoots(role, local); err != nil {
		return err
	}
	if !r.Refs {
		if r.SnapshotRefs != nil || r.SandboxRefs != nil || r.RemovedRefs != nil {
			return errors.New("publication report: refs:false must omit reference sets and removedRefs")
		}
		return nil
	}
	if role == RoleSandbox && r.SnapshotRefs != nil {
		return errors.New("publication report: snapshotRefs on Sandbox report")
	}
	if role == RoleSnapshot {
		if err := validateReferenceSet("snapshotRefs", r.SnapshotRefs, true); err != nil {
			return err
		}
		if !containsReference(r.SnapshotRefs, r.SnapshotRef) {
			return errors.New("publication report: snapshotRefs must contain snapshotRef")
		}
	}
	if err := validateReferenceSet("sandboxRefs", r.SandboxRefs, true); err != nil {
		return err
	}
	if !containsReference(r.SandboxRefs, r.SandboxRef) {
		return errors.New("publication report: sandboxRefs must contain sandboxRef")
	}
	if err := validateReferenceSet("removedRefs", r.RemovedRefs, false); err != nil {
		return err
	}
	for _, raw := range r.RemovedRefs {
		if containsReference(r.SnapshotRefs, raw) || containsReference(r.SandboxRefs, raw) {
			return errors.New("publication report: removed reference is still in the final graph")
		}
	}
	return nil
}

func (r PublishReport) validateLegacy(role LogicalRole, local bool) error {
	if err := r.validateRoots(role, local); err != nil {
		return err
	}
	if err := validateReferenceSet("removedRefs", r.RemovedRefs, false); err != nil {
		return err
	}
	for _, raw := range r.RemovedRefs {
		if raw == r.SandboxRef || raw == r.SnapshotRef {
			return errors.New("publication report: removed reference is a final root")
		}
	}
	return nil
}

// DecodePublishReport accepts an exact new report or the exact legacy S/E
// shape. Legacy roots are retained, but its incomplete delta is discarded.
func DecodePublishReport(data []byte, role LogicalRole) (PublishReport, error) {
	fields, err := decodeReportFields(data)
	if err != nil {
		return PublishReport{}, err
	}
	if _, exists := fields["refs"]; !exists {
		out, err := decodeLegacyReport(fields, role, false)
		out.RemovedRefs = nil
		return out, err
	}
	var out PublishReport
	if bytes.Equal(bytes.TrimSpace(fields["refs"]), []byte("null")) {
		return out, errors.New("publication report: refs must be a boolean")
	}
	if err := json.Unmarshal(fields["refs"], &out.Refs); err != nil {
		return out, err
	}
	allowed := []string{"refs", "sandboxRef"}
	if role == RoleSnapshot {
		allowed = append(allowed, "snapshotRef")
	}
	if out.Refs {
		allowed = append(allowed, "sandboxRefs", "removedRefs")
		if role == RoleSnapshot {
			allowed = append(allowed, "snapshotRefs")
		}
	}
	if err := exactReportFields(fields, allowed); err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	if err := out.Validate(role); err != nil {
		return PublishReport{}, err
	}
	return out, nil
}

// DecodeCaptureReport strictly decodes the unchanged local/capture format.
func DecodeCaptureReport(data []byte, role LogicalRole) (PublishReport, error) {
	fields, err := decodeReportFields(data)
	if err != nil {
		return PublishReport{}, err
	}
	out, err := decodeLegacyReport(fields, role, true)
	if err == nil {
		err = out.ValidateCapture(role)
	}
	if err != nil {
		return PublishReport{}, err
	}
	return out, nil
}

func decodeReportFields(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("publication report: expected JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("publication report: invalid field")
		}
		if _, exists := fields[name]; exists {
			return nil, errors.New("publication report: duplicate field")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("publication report: trailing output")
	}
	return fields, nil
}

func exactReportFields(fields map[string]json.RawMessage, allowed []string) error {
	if len(fields) != len(allowed) {
		return errors.New("publication report: invalid fields for role and refs state")
	}
	for _, name := range allowed {
		if fields[name] == nil {
			return fmt.Errorf("publication report: missing %s", name)
		}
	}
	return nil
}

func decodeLegacyReport(fields map[string]json.RawMessage, role LogicalRole, local bool) (PublishReport, error) {
	var out PublishReport
	allowed := []string{"sandboxRef", "removedRefs"}
	if role == RoleSnapshot {
		allowed = append(allowed, "snapshotRef")
	}
	if err := exactReportFields(fields, allowed); err != nil {
		return out, err
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
	if err := out.validateLegacy(role, local); err != nil {
		return PublishReport{}, err
	}
	return out, nil
}

type publicationRef struct {
	raw   string
	scope publishScope
}
type publicationReport struct {
	before                    []publicationRef
	snapshotRefs, sandboxRefs []string
	retainedSandbox           bool
	checkpoints               []checkpointPublication
}

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
	if ref, err := manifest.ParseRef(raw); err == nil && p.transientLocations[ref.Location] {
		return
	}
	if raw != "" && raw != "self" {
		p.report.before = append(p.report.before, publicationRef{raw, scope})
	}
}
func scopedRef(raw string, scope publishScope) string {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return raw
	}
	ref = ref.Base()
	raw = ref.String()
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

// knownScopedRef records only an established old carrier. A skipped alias may
// have referred to a Bundle fallback that the operation never selected; it is
// not evidence of a Store entry. Do not probe old sources just for the delta.
func knownScopedRef(raw string, scope publishScope) (string, bool) {
	ref, err := manifest.ParseRef(raw)
	if err != nil || ref.Scheme != manifest.RefSchemeManifest {
		return scopedRef(raw, scope), true
	}
	raw = ref.Base().String()
	if scope.unbound[raw] {
		return "", false
	}
	if bound, ok := scope.bindings[raw]; ok {
		return bound, true
	}
	if scope.current != nil {
		key, err := manifest.ParseKeyRef(ref.Path)
		if err == nil && scope.current.HasManifest(key) {
			carrier, err := manifest.ParseRef(scope.carrier)
			if err != nil || carrier.Scheme != manifest.RefSchemeFile {
				return "", false
			}
			return scopedRef(bundleMemberRef(scope.carrier, key), scope), true
		}
	}
	if _, bundled := scope.fetcher.(*manifestbundle.ManifestFetcher); bundled {
		return "", false
	}
	return raw, true
}

// rememberSelection is called only when publication already selects a source.
func (scope publishScope) rememberSelection(raw string, selected manifestbundle.ManifestSource) {
	if scope.bindings == nil {
		return
	}
	bound := raw
	delete(scope.unbound, raw)
	if selected.Reader != nil {
		carrier := selectedBundleCarrier(scope.carrier, selected.Ref)
		if ref, err := manifest.ParseRef(carrier); err == nil {
			key, _ := manifest.ParseRef(raw)
			ref.DigestScheme, ref.Digest = "manifest", key.Path
			bound = scopedRef(ref.String(), scope)
		} else if scope.unbound != nil {
			// A lookup alias in a borrowed private Bundle does not prove that
			// the corresponding Manifest exists in the Store.
			scope.unbound[raw] = true
		}
	}
	scope.bindings[raw] = bound
}

func (p *Publisher) result(role LogicalRole, ref, sandboxRef string) (PublishResult, error) {
	if role == RoleSandbox {
		sandboxRef = ref
	}
	sandboxRefs, err := canonicalReferences(append(append([]string{}, p.report.sandboxRefs...), sandboxRef))
	if err != nil {
		return PublishResult{}, err
	}
	var snapshotRefs []string
	if role == RoleSnapshot {
		snapshotRefs, err = canonicalReferences(append(append([]string{}, p.report.snapshotRefs...), ref))
		if err != nil {
			return PublishResult{}, err
		}
	}
	final := append(append([]string{}, sandboxRefs...), snapshotRefs...)
	before := make([]string, 0, len(p.report.before))
	for _, item := range p.report.before {
		if ref, known := knownScopedRef(item.raw, item.scope); known {
			before = append(before, ref)
		}
	}
	removed, err := referenceDifference(before, final)
	if err != nil {
		return PublishResult{}, err
	}
	return PublishResult{Role: role, Ref: ref, SandboxRef: sandboxRef, Refs: true,
		SnapshotRefs: snapshotRefs, SandboxRefs: sandboxRefs, RemovedRefs: removed,
		checkpointSources: append([]checkpointPublication(nil), p.report.checkpoints...), completion: p.completion}, nil
}
