package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"path/filepath"
	"reflect"
)

type rewriteUse uint8

const (
	rewriteDisk rewriteUse = iota
	rewriteImage
	rewriteMemory
)

type rewriteValue struct {
	raw         string
	scope       publishScope
	use         rewriteUse
	payload     sparse.Source
	source      sparse.Source
	role        LogicalRole
	imageConfig []byte
	forceWrite  bool
	published   string
}
type rewriteChain struct {
	values    []*rewriteValue
	target    *rewriteValue
	source    sparse.Source
	base      *string
	lowers    *[]string
	embedded  bool
	reduced   bool
	automatic bool
	role      LogicalRole
}
type sandboxRewrite struct {
	root    *sandboxfile.Root
	cfg     *config.PortableSandboxConfig
	payload sparse.Source
	chains  []*rewriteChain
}
type publicationRewrite struct {
	p      *Publisher
	rules  *rewriteSession
	closes []func() error
}

// PublishWithOptions verifies a complete plan before emitting new objects.
func (p *Publisher) PublishWithOptions(ctx context.Context, input string, opts RewriteOptions) (result PublishResult, retErr error) {
	replacements := make([]string, len(opts.Replacements))
	for i, r := range opts.Replacements {
		replacements[i] = r.Old + "=" + r.New
	}
	reductions := make([]string, 0, len(opts.Reductions)+1)
	for _, r := range opts.Reductions {
		v := r.Top
		if r.Target != "" {
			v += "=" + r.Target
		}
		reductions = append(reductions, v)
	}
	if opts.ReduceAny {
		reductions = append(reductions, "any")
	}
	checked, err := ParseRewriteOptions(replacements, reductions, opts.SkipVerify)
	if err != nil {
		return result, err
	}
	if checked.empty() {
		return p.Publish(ctx, input)
	}
	if p == nil || p.storage == nil || p.target == nil {
		return result, errors.New("publish: publisher is not initialized")
	}
	// A reference rewrite always uses authenticated Manifest/Chunk reads,
	// independently of the optional equivalence proof.
	storageConfig := p.storage.cfg
	if storageConfig != nil {
		copyConfig := *storageConfig
		verify := true
		copyConfig.Manifest.VerifyContent = &verify
		storageConfig = &copyConfig
	}
	verifiedStorage, err := NewProcessStorageWithCustomerKey(storageConfig, p.storage.CustomerKeyFunc())
	if err != nil {
		return result, err
	}
	operation := *p
	operation.storage = verifiedStorage
	plan := &publicationRewrite{p: &operation, rules: newRewriteSession(checked), closes: []func() error{verifiedStorage.Close}}
	defer func() {
		for i := len(plan.closes) - 1; i >= 0; i-- {
			retErr = errors.Join(retErr, plan.closes[i]())
		}
		if retErr != nil {
			result = PublishResult{}
		}
	}()
	raw, scope, err := plan.p.normalizeRoot(input)
	if err != nil {
		return result, err
	}
	stream, child, err := plan.open(ctx, raw, scope)
	if err != nil {
		return result, err
	}
	eroot, eerr := sandboxfile.Open(ctx, stream)
	if eerr == nil {
		plan.closes = append(plan.closes, eroot.FullStream.Close)
		e, err := plan.sandbox(ctx, raw, child, eroot, true)
		if err != nil {
			return result, err
		}
		if err = plan.rules.unmatched(); err != nil {
			return result, err
		}
		ref, err := plan.emitSandbox(ctx, e)
		return PublishResult{Role: RoleSandbox, Ref: ref}, err
	}
	if readretry.IsTerminal(eerr) || readerr.IsPermanent(eerr) {
		return result, eerr
	}
	stream, child, err = plan.open(ctx, raw, scope)
	if err != nil {
		return result, errors.Join(eerr, err)
	}
	sroot, err := snapshotfile.Open(ctx, stream)
	if err != nil {
		return result, errors.Join(eerr, err)
	}
	plan.closes = append(plan.closes, sroot.FullStream.Close)
	scfg, err := readRewriteSnapshot(sroot)
	if err != nil {
		return result, err
	}
	oldE := scfg.SandboxRef
	newE := plan.rules.replace(oldE)
	e, err := plan.openSandbox(ctx, newE, child, newE == oldE)
	if err != nil {
		return result, fmt.Errorf("snapshot sandbox_ref: %w", err)
	}
	if newE != oldE && !checked.SkipVerify {
		original, err := plan.openSandbox(ctx, oldE, child, false)
		if err != nil {
			return result, err
		}
		if err = compareSandboxRewrite(ctx, original, e); err != nil {
			return result, fmt.Errorf("sandbox_ref equivalence: %w", err)
		}
	}
	top := &rewriteValue{raw: raw, scope: child, use: rewriteMemory, payload: sroot.Memory, source: sroot.FullStream, role: RoleSnapshot}
	memory, err := plan.chain(ctx, raw, nil, &scfg.FromRefs, child, top, rewriteMemory, true)
	if err != nil {
		return result, err
	}
	if _, err = snapshot.MarshalConfig(scfg); err != nil {
		return result, err
	}
	if err = plan.rules.unmatched(); err != nil {
		return result, err
	}
	if err = plan.emitChain(ctx, memory); err != nil {
		return result, err
	}
	scfg.SandboxRef, err = plan.emitSandbox(ctx, e)
	if err != nil {
		return result, err
	}
	tail, err := snapshot.MarshalConfig(scfg)
	if err != nil {
		return result, err
	}
	source, err := snapshotfile.BuildSource(memory.topSource(), sroot.ConfigJSON, sroot.StateJSON, tail)
	if err != nil {
		return result, err
	}
	ref, err := p.target.Put(ctx, RoleSnapshot, source)
	return PublishResult{Role: RoleSnapshot, Ref: ref}, err
}

func readRewriteSnapshot(root *snapshotfile.Root) (*snapshot.Config, error) {
	cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
	if err != nil {
		return nil, err
	}
	canonical, err := snapshot.MarshalConfig(cfg)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, root.SnapshotConfig) {
		return nil, errors.New("snapshot.cfg is not canonical")
	}
	return cfg, nil
}
func (p *publicationRewrite) openSandbox(ctx context.Context, raw string, scope publishScope, apply bool) (*sandboxRewrite, error) {
	stream, child, err := p.open(ctx, raw, scope)
	if err != nil {
		return nil, err
	}
	root, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		return nil, err
	}
	p.closes = append(p.closes, root.FullStream.Close)
	return p.sandbox(ctx, raw, child, root, apply)
}
func (p *publicationRewrite) sandbox(ctx context.Context, raw string, scope publishScope, root *sandboxfile.Root, apply bool) (*sandboxRewrite, error) {
	cfg, err := root.Portable.Clone()
	if err != nil {
		return nil, err
	}
	planned := &sandboxRewrite{root: root, cfg: cfg, payload: root.Payload}
	add := func(base *string, lowers *[]string, use rewriteUse) error {
		if *base == "" {
			return nil
		}
		var embedded *rewriteValue
		selector := *base
		if *base == "self" {
			selector = raw
			embedded = &rewriteValue{raw: raw, scope: scope, use: use, payload: root.Payload, source: root.FullStream, role: RoleSandbox, imageConfig: root.ImageConfig}
		}
		chain, err := p.chain(ctx, selector, base, lowers, scope, embedded, use, apply)
		if err != nil {
			return err
		}
		planned.chains = append(planned.chains, chain)
		if embedded != nil {
			planned.payload = chain.topSource()
		}
		return nil
	}
	addDisk := func(root *config.PortableRootConfig) error {
		use := rewriteDisk
		if root.Overlay != nil {
			use = rewriteImage
		}
		if err := add(&root.Base, &root.BaseFromRefs, use); err != nil {
			return err
		}
		if root.Overlay != nil {
			return add(&root.Overlay.Base, &root.Overlay.BaseFromRefs, rewriteDisk)
		}
		return nil
	}
	if err = addDisk(&cfg.Boot.Root); err != nil {
		return nil, fmt.Errorf("root disk: %w", err)
	}
	for i := range cfg.Boot.Disks {
		if err = addDisk(&cfg.Boot.Disks[i].PortableRootConfig); err != nil {
			return nil, fmt.Errorf("disk %s: %w", cfg.Boot.Disks[i].Name, err)
		}
	}
	if err = cfg.Validate(); err != nil {
		return nil, err
	}
	return planned, nil
}
func (p *publicationRewrite) openValue(ctx context.Context, raw string, scope publishScope, use rewriteUse) (*rewriteValue, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return nil, err
	}
	force := !ref.Portable()
	if ref.Scheme == manifest.RefSchemeManifest {
		if local, ok := scope.fetcher.(*manifestbundle.ManifestFetcher); ok {
			key, err := manifest.ParseKeyRef(raw)
			if err != nil {
				return nil, err
			}
			selected, err := local.SelectManifest(ctx, key)
			if err != nil {
				return nil, err
			}
			force = selected.Reader != nil
		}
	}
	stream, child, err := p.open(ctx, raw, scope)
	if err != nil {
		return nil, err
	}
	out := &rewriteValue{raw: raw, scope: child, use: use, forceWrite: force}
	if use == rewriteMemory {
		root, err := snapshotfile.Open(ctx, stream)
		if err != nil {
			return nil, err
		}
		p.closes = append(p.closes, root.FullStream.Close)
		if _, err = readRewriteSnapshot(root); err != nil {
			return nil, err
		}
		out.payload, out.source, out.role = root.Memory, root.FullStream, RoleSnapshot
		return out, nil
	}
	required := ref.Scheme == manifest.RefSchemeFile && filepath.Ext(ref.Path) == ".sandbox"
	payload, _, err := sandboxfile.PayloadIfSandbox(ctx, stream, required)
	if err != nil {
		return nil, err
	}
	if use == rewriteImage {
		image, err := sandboxfile.OpenEROFSArtifact(ctx, payload)
		if err != nil {
			return nil, err
		}
		p.closes = append(p.closes, image.FullStream.Close)
		out.payload, out.source, out.role, out.imageConfig = image.Payload, image.FullStream, RoleImage, image.ImageConfig
	} else {
		p.closes = append(p.closes, payload.Close)
		out.payload, out.source, out.role = payload, payload, RoleOverlay
	}
	return out, nil
}
func (p *publicationRewrite) changedValue(ctx context.Context, raw string, scope publishScope, use rewriteUse, apply bool) (*rewriteValue, error) {
	mapped := raw
	if apply {
		mapped = p.rules.replace(raw)
	}
	value, err := p.openValue(ctx, mapped, scope, use)
	if err != nil {
		return nil, err
	}
	if mapped != raw && !p.rules.options.SkipVerify {
		old, err := p.openValue(ctx, raw, scope, use)
		if err != nil {
			return nil, fmt.Errorf("replace %s: %w", raw, err)
		}
		if err = compareRewriteValues(ctx, old, value); err != nil {
			return nil, fmt.Errorf("replace %s: %w", raw, err)
		}
	}
	return value, nil
}

// An explicitly supplied merged memory target can be a bare logical stream.
func (p *publicationRewrite) memoryTarget(ctx context.Context, raw string, scope publishScope, size uint64) (*rewriteValue, error) {
	stream, child, err := p.open(ctx, raw, scope)
	if err != nil {
		return nil, err
	}
	if stream.Size() == size {
		p.closes = append(p.closes, stream.Close)
		return &rewriteValue{raw: raw, scope: child, use: rewriteMemory, payload: stream, source: stream, role: RoleSnapshot}, nil
	}
	if err = stream.Close(); err != nil {
		return nil, err
	}
	return p.openValue(ctx, raw, scope, rewriteMemory)
}

func (p *publicationRewrite) chain(ctx context.Context, selector string, base *string, lowers *[]string, scope publishScope, embedded *rewriteValue, use rewriteUse, apply bool) (*rewriteChain, error) {
	chain := &rewriteChain{base: base, lowers: lowers, embedded: embedded != nil, role: RoleOverlay}
	if use == rewriteImage {
		chain.role = RoleImage
	}
	if use == rewriteMemory {
		chain.role = RoleSnapshot
	}
	var target string
	if apply {
		target, chain.reduced = p.rules.reduction(selector, len(*lowers) > 0)
	}
	// An explicitly supplied replacement chain can repair unavailable old
	// refs when the caller owns the equivalence assertion. Embedded top size
	// remains authoritative and is checked even in this mode.
	if chain.reduced && target != "" && p.rules.options.SkipVerify {
		var value *rewriteValue
		var err error
		if use == rewriteMemory && embedded != nil {
			value, err = p.memoryTarget(ctx, target, scope, embedded.payload.Size())
		} else {
			value, err = p.openValue(ctx, target, scope, use)
		}
		if err != nil {
			return nil, err
		}
		if embedded != nil && value.payload.Size() != embedded.payload.Size() {
			return nil, errors.New("reduction target capacity differs")
		}
		chain.target = value
		chain.values = []*rewriteValue{value}
		chain.source = value.payload
		*lowers = nil
		return chain, nil
	}
	top := embedded
	var err error
	if top == nil {
		top, err = p.changedValue(ctx, *base, scope, use, apply)
		if err != nil {
			return nil, err
		}
	}
	chain.values = append(chain.values, top)
	for _, raw := range *lowers {
		value, err := p.changedValue(ctx, raw, scope, use, apply)
		if err != nil {
			return nil, err
		}
		if value.payload.Size() != top.payload.Size() {
			return nil, fmt.Errorf("layer capacity differs for %s: %d != %d", raw, value.payload.Size(), top.payload.Size())
		}
		chain.values = append(chain.values, value)
	}
	chain.source = layeredRewriteValues(chain.values)
	if chain.reduced && target != "" {
		if use == rewriteMemory && embedded != nil {
			chain.target, err = p.memoryTarget(ctx, target, scope, top.payload.Size())
		} else {
			chain.target, err = p.openValue(ctx, target, scope, use)
		}
		if err != nil {
			return nil, err
		}
		if chain.target.payload.Size() != chain.source.Size() {
			return nil, errors.New("reduction target capacity differs")
		}
		if !p.rules.options.SkipVerify {
			if err = compareSparseStreams(ctx, chain.source, chain.target.payload); err != nil {
				return nil, fmt.Errorf("reduce %s=%s: %w", selector, target, err)
			}
			if use == rewriteImage && !equivalentJSON(top.imageConfig, chain.target.imageConfig) {
				return nil, errors.New("reduction image configuration differs")
			}
		}
		chain.source = chain.target.payload
	} else if chain.reduced && len(chain.values) > 1 {
		chain.automatic = true
	}
	// Keep the original valid reference spellings until emission. Values bind
	// replacements to their source scope; final portable refs are installed only
	// after their dependency objects have been published.
	if chain.reduced {
		*lowers = nil
	}

	return chain, nil
}

type rewriteSourceStream struct{ sparse.Source }

func (rewriteSourceStream) Close() error { return nil }
func layeredRewriteValues(values []*rewriteValue) sparse.Source {
	if len(values) == 1 {
		return values[0].payload
	}
	streams := make([]fetch.Stream, len(values))
	for i, v := range values {
		streams[i] = rewriteSourceStream{v.payload}
	}
	return fetch.NewLayered(streams...)
}
func (p *publicationRewrite) emitValue(ctx context.Context, value *rewriteValue) (string, error) {
	if value.published != "" {
		return value.published, nil
	}
	if !value.forceWrite {
		return value.raw, nil
	}
	ref, err := p.p.target.Put(ctx, value.role, value.source)
	if err == nil {
		value.published = ref
	}
	return ref, err
}
func (p *publicationRewrite) emitChain(ctx context.Context, chain *rewriteChain) error {
	if chain.reduced {
		if chain.embedded {
			return nil
		}
		var ref string
		var err error
		switch {
		case chain.automatic:
			ref, err = p.p.target.Put(ctx, chain.role, chain.source)
		case chain.target != nil:
			ref, err = p.emitValue(ctx, chain.target)
		default:
			ref, err = p.emitValue(ctx, chain.values[0])
		}
		if err == nil {
			*chain.base = ref
		}
		return err
	}
	for i := len(chain.values) - 1; i >= 0; i-- {
		if i == 0 && chain.embedded {
			continue
		}
		ref, err := p.emitValue(ctx, chain.values[i])
		if err != nil {
			return err
		}
		if i == 0 {
			*chain.base = ref
		} else {
			(*chain.lowers)[i-1] = ref
		}
	}
	return nil
}
func (p *publicationRewrite) emitSandbox(ctx context.Context, e *sandboxRewrite) (string, error) {
	for _, chain := range e.chains {
		if err := p.emitChain(ctx, chain); err != nil {
			return "", err
		}
	}
	runtime, err := config.MarshalPortableSandboxConfig(e.cfg)
	if err != nil {
		return "", err
	}
	source, err := sandboxfile.BuildSourceContext(ctx, e.payload, e.root.ImageConfig, runtime)
	if err != nil {
		return "", err
	}
	return p.p.target.Put(ctx, RoleSandbox, source)
}

func equivalentJSON(left, right []byte) bool {
	if bytes.Equal(left, right) {
		return true
	}
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}

func compareRewriteValues(ctx context.Context, left, right *rewriteValue) error {
	if left.use == rewriteImage && !equivalentJSON(left.imageConfig, right.imageConfig) {
		return errors.New("image configuration differs")
	}
	return compareSparseStreams(ctx, left.payload, right.payload)
}

// The clone used for semantic comparison omits reference spelling while
// retaining all device and non-reference configuration fields.
func omitReferenceSpelling(cfg *config.PortableSandboxConfig) {
	omit := func(root *config.PortableRootConfig) {
		root.Base = ""
		root.BaseFromRefs = nil
		if root.Overlay != nil {
			root.Overlay.Base = ""
			root.Overlay.BaseFromRefs = nil
		}
	}
	omit(&cfg.Boot.Root)
	for i := range cfg.Boot.Disks {
		omit(&cfg.Boot.Disks[i].PortableRootConfig)
	}
}

func compareSandboxRewrite(ctx context.Context, left, right *sandboxRewrite) error {
	a, err := left.cfg.Clone()
	if err != nil {
		return err
	}
	b, err := right.cfg.Clone()
	if err != nil {
		return err
	}
	omitReferenceSpelling(a)
	omitReferenceSpelling(b)
	if !reflect.DeepEqual(a, b) || !equivalentJSON(left.root.ImageConfig, right.root.ImageConfig) || len(left.chains) != len(right.chains) {
		return errors.New("Sandbox configuration or device topology differs")
	}
	for i := range left.chains {
		if err = compareSparseStreams(ctx, left.chains[i].source, right.chains[i].source); err != nil {
			return fmt.Errorf("Sandbox device %d: %w", i, err)
		}
		if left.chains[i].values[0].use == rewriteImage && !equivalentJSON(left.chains[i].values[0].imageConfig, right.chains[i].values[0].imageConfig) {
			return errors.New("Sandbox base image configuration differs")
		}
	}
	return nil
}

func (c *rewriteChain) topSource() sparse.Source {
	if c.reduced {
		return c.source
	}
	return c.values[0].payload
}
