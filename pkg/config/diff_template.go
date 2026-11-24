package config

import (
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/emptyext4"
	"github.com/kuasar-sandbox/sandboxer/pkg/util"
)

// DiffTemplateKind selects one logical disk initializer without content sniffing.
type DiffTemplateKind string

const (
	DiffTemplateRaw       DiffTemplateKind = "raw"
	DiffTemplateTarstream DiffTemplateKind = "tarstream"
	DiffTemplateBundle    DiffTemplateKind = "bundle"
	DiffTemplateManifest  DiffTemplateKind = "manifest"
	DiffTemplateMkfs      DiffTemplateKind = "mkfs"
)

// DiffTemplateRef is an immutable logical disk source. Size is the requested
// generation capacity for mkfs; external carriers determine their logical size
// when opened. Parsing never opens a carrier or applies a target backend limit.
type DiffTemplateRef struct {
	Kind DiffTemplateKind
	Ref  manifest.Ref
	Size int64
}

func (r DiffTemplateRef) String() string {
	if r.Kind == DiffTemplateMkfs {
		return "mkfs://ext4?size=" + strconv.FormatInt(r.Size, 10)
	}
	return r.Ref.String()
}

// Identity excludes lookup hints for qualified content and fixes the generator
// profile as well as its capacity. Raw files retain their existing path identity.
func (r DiffTemplateRef) Identity() string {
	if r.Kind == DiffTemplateMkfs {
		return emptyext4.Profile + ":" + strconv.FormatInt(r.Size, 10)
	}
	if r.Ref.Scheme == manifest.RefSchemeManifest {
		return "manifest:" + r.Ref.Path
	}
	if r.Ref.DigestScheme != "" {
		return r.Ref.DigestScheme + ":" + r.Ref.Digest
	}
	return r.String()
}

// ParseDiffTemplate selects raw, canonical tarstream, Manifest Bundle, Store,
// or built-in ext4. Qualified files keep the shared Ref/location syntax.
// Creation alignment and generator/backend limits are deliberately checked only
// when mkfs is selected; an existing writable layer does not use its template.
func ParseDiffTemplate(raw string) (DiffTemplateRef, error) {
	if strings.HasPrefix(strings.ToLower(raw), "mkfs:") {
		return parseMkfsTemplate(raw)
	}
	r, err := manifest.ParseRef(raw)
	if err != nil {
		return DiffTemplateRef{}, err
	}
	if r.String() != raw {
		return DiffTemplateRef{}, fmt.Errorf("diff_template: noncanonical source reference")
	}
	if !r.Runtime.IsZero() {
		return DiffTemplateRef{}, fmt.Errorf("diff_template: snapshot runtime declaration is not applicable")
	}
	kind := DiffTemplateManifest
	if r.Scheme == manifest.RefSchemeFile {
		if r.Location == "" && !filepath.IsAbs(r.Path) {
			return DiffTemplateRef{}, fmt.Errorf("diff_template: unlocated file reference requires an absolute path")
		}
		switch r.DigestScheme {
		case "":
			kind = DiffTemplateRaw
		case "digest", "hmac":
			kind = DiffTemplateTarstream
		case "manifest":
			kind = DiffTemplateBundle
		default:
			return DiffTemplateRef{}, fmt.Errorf("diff_template: unsupported identity %q", r.DigestScheme)
		}
	}
	return DiffTemplateRef{Kind: kind, Ref: r}, nil
}

// NormalizeDiffTemplate preserves an omitted source and canonicalizes a present
// reference. It does not infer a format or turn a bare host path into a URI.
func NormalizeDiffTemplate(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	r, err := ParseDiffTemplate(raw)
	if err != nil {
		return "", err
	}
	return r.String(), nil
}

func parseMkfsTemplate(raw string) (DiffTemplateRef, error) {
	fail := func(message string) (DiffTemplateRef, error) {
		return DiffTemplateRef{}, fmt.Errorf("mkfs URI: %s", message)
	}
	if strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return fail("raw whitespace is not URI encoded")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return DiffTemplateRef{}, fmt.Errorf("mkfs URI: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "mkfs") || u.Opaque != "" || u.Host == "" {
		return fail("expected mkfs://ext4?size=<capacity>")
	}
	if u.User != nil || strings.Contains(u.Host, ":") {
		return fail("userinfo and port are unsupported")
	}
	if !strings.EqualFold(u.Host, "ext4") {
		return fail("unsupported filesystem; expected ext4")
	}
	if u.Path != "" || strings.Contains(raw, "#") {
		return fail("path and fragment are unsupported")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return DiffTemplateRef{}, fmt.Errorf("mkfs URI query: %w", err)
	}
	for key := range q {
		if key != "size" {
			return fail("unknown query key " + strconv.Quote(key))
		}
	}
	values := q["size"]
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return fail("exactly one nonempty size is required")
	}
	n, err := util.ParseSize(values[0])
	if err != nil {
		return DiffTemplateRef{}, fmt.Errorf("mkfs URI size: %w", err)
	}
	if n == 0 || n > math.MaxInt64 {
		return fail("size must be a positive int64 byte count")
	}
	return DiffTemplateRef{Kind: DiffTemplateMkfs, Size: int64(n)}, nil
}
