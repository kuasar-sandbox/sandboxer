package artifact

import (
	"context"
	"fmt"
	"sort"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// NewSandboxPublishResult builds a complete E report from the final small
// configuration. Callers supply already-bound references from the same export;
// this helper never opens artifacts or traverses Bundle fallback candidates.
func NewSandboxPublishResult(ref string, cfg *config.PortableSandboxConfig, beforeRefs []string) (PublishResult, error) {
	if cfg == nil {
		return PublishResult{}, fmt.Errorf("sandbox publication: final configuration is required")
	}
	refs := []string{ref}
	for _, dep := range portableDiskRefs(cfg) {
		refs = append(refs, dep.raw)
	}
	refs, err := canonicalReferences(refs)
	if err != nil {
		return PublishResult{}, err
	}
	removed, err := referenceDifference(beforeRefs, refs)
	if err != nil {
		return PublishResult{}, err
	}
	return PublishResult{Role: RoleSandbox, Ref: ref, SandboxRef: ref, Refs: true, SandboxRefs: refs, RemovedRefs: removed}, nil
}

func canonicalReferences(refs []string) ([]string, error) {
	seen := make(map[string]bool, len(refs))
	out := make([]string, 0, len(refs))
	for _, raw := range refs {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return nil, err
		}
		raw = ref.String()
		if !seen[raw] {
			seen[raw] = true
			out = append(out, raw)
		}
	}
	sort.Strings(out)
	return out, nil
}

func referenceDifference(before, after []string) ([]string, error) {
	final := make(map[string]bool, len(after))
	for _, raw := range after {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return nil, err
		}
		final[ref.Base().String()] = true
	}
	removed := make([]string, 0)
	for _, raw := range before {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return nil, err
		}
		if !final[ref.Base().String()] {
			removed = append(removed, raw)
		}
	}
	return canonicalReferences(removed)
}

func (p *Publisher) sandboxGroup(cfg *config.PortableSandboxConfig) {
	p.report.sandboxRefs = p.report.sandboxRefs[:0]
	for _, dep := range portableDiskRefs(cfg) {
		p.report.sandboxRefs = append(p.report.sandboxRefs, dep.raw)
	}
}

// Retaining an E also retains its complete explicit disk graph. A located
// file is already independently addressable and is never opened for reporting.
// For manifest aliases, select only metadata through the existing source view.
func (p *Publisher) retainedSandboxGroup(ctx context.Context, cfg *config.PortableSandboxConfig, scope publishScope) error {
	p.report.sandboxRefs = p.report.sandboxRefs[:0]
	for _, dep := range portableDiskRefs(cfg) {
		ref, err := p.boundReportReference(ctx, dep.raw, scope)
		if err != nil {
			return err
		}
		p.report.sandboxRefs = append(p.report.sandboxRefs, ref)
	}
	return nil
}

func (p *Publisher) boundReportReference(ctx context.Context, raw string, scope publishScope) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", err
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		if scope.unbound[raw] {
			return "", fmt.Errorf("publication report: final manifest alias %q has no known source carrier", raw)
		}
		if _, known := scope.bindings[raw]; !known {
			if local, ok := scope.fetcher.(*manifestbundle.ManifestFetcher); ok {
				key, err := manifest.ParseKeyRef(ref.Path)
				if err != nil {
					return "", err
				}
				selected, err := local.SelectManifest(ctx, key)
				if err != nil {
					return "", err
				}
				if selected.Reader != nil {
					carrier := selectedBundleCarrier(scope.carrier, selected.Ref)
					parsed, err := manifest.ParseRef(carrier)
					if err != nil || parsed.Scheme != manifest.RefSchemeFile {
						return "", fmt.Errorf("publication report: final manifest alias %q has no known source carrier", raw)
					}
					return scopedRef(bundleMemberRef(carrier, key), scope), nil
				}
			}
		}
	}
	return scopedRef(raw, scope), nil
}
