package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

// RewriteOptions applies once to the original reference positions of a
// publication. The publisher first plans and verifies, then writes new objects.
type RewriteOptions struct {
	Replacements []RefReplacement
	Reductions   []RefReduction
	ReduceAny    bool
	SkipVerify   bool
}

func compareSparseStreams(ctx context.Context, left, right sparse.Source) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if left.Size() != right.Size() {
		return fmt.Errorf("logical size differs: %d != %d", left.Size(), right.Size())
	}
	if a, ok := left.(tarstream.IdentityProvider); ok {
		if b, ok := right.(tarstream.IdentityProvider); ok {
			as, ad, av := a.PayloadCommitment()
			bs, bd, bv := b.PayloadCommitment()
			if av && bv && as == left.Size() && bs == right.Size() && ad == bd {
				return nil
			}
		}
	}
	const budget = 64 << 10
	lbuf, rbuf := make([]byte, budget), make([]byte, budget)
	defer clear(lbuf)
	defer clear(rbuf)
	for offset := uint64(0); offset < left.Size(); {
		if err := ctx.Err(); err != nil {
			return err
		}
		lrun, err := left.RunAt(offset, left.Size()-offset)
		if err != nil {
			return err
		}
		rrun, err := right.RunAt(offset, right.Size()-offset)
		if err != nil {
			return err
		}
		for _, r := range []sparse.Run{lrun, rrun} {
			if r == nil || r.Offset() != offset || r.End() <= offset || r.End() > left.Size() {
				return errors.New("invalid sparse run")
			}
			if r.Kind() != sparse.Hole && r.Kind() != sparse.Zero && r.Kind() != sparse.Data {
				return errors.New("invalid sparse run kind")
			}
		}
		if (lrun.Kind() == sparse.Hole) != (rrun.Kind() == sparse.Hole) {
			return fmt.Errorf("Hole/Present layout differs at %d", offset)
		}
		end := min(lrun.End(), rrun.End())
		if lrun.Kind() != sparse.Hole {
			for at := offset; at < end; {
				n := int(min(uint64(budget), end-at))
				for i, r := range []sparse.Run{lrun, rrun} {
					dst := lbuf[:n]
					if i == 1 {
						dst = rbuf[:n]
					}
					if r.Kind() == sparse.Zero {
						clear(dst)
						continue
					}
					got, e := readretry.ReadAt(ctx, n, func() (int, error) { return r.ReadAt(ctx, dst, at-offset) })
					if e != nil && !(e == io.EOF && got == n) {
						return e
					}
					if got != n {
						return io.ErrUnexpectedEOF
					}
				}
				if !bytes.Equal(lbuf[:n], rbuf[:n]) {
					return fmt.Errorf("present bytes differ at %d", at)
				}
				at += uint64(n)
			}
		}
		offset = end
	}
	return nil
}

type RefReplacement struct{ Old, New string }
type RefReduction struct {
	Top    string
	Target string // empty asks the publisher to synthesize the flattened target
}

// ParseRewriteOptions parses the CLI spelling and performs all validation that
// does not require opening the input graph. In particular, it catches
// conflicting assignments before a Publisher can create an output object.
func ParseRewriteOptions(replacements, reductions []string, skipVerify bool) (RewriteOptions, error) {
	options := RewriteOptions{SkipVerify: skipVerify}
	replaceTargets := make(map[string]string)
	for _, rule := range replacements {
		old, replacement, ok := strings.Cut(rule, "=")
		if !ok || old == "" || replacement == "" {
			return RewriteOptions{}, fmt.Errorf("invalid --replace-ref %q: expected OLD=NEW", rule)
		}
		old, err := canonicalRewriteRef(old)
		if err != nil {
			return RewriteOptions{}, fmt.Errorf("invalid --replace-ref OLD %q: %w", old, err)
		}
		replacement, err = canonicalRewriteRef(replacement)
		if err != nil {
			return RewriteOptions{}, fmt.Errorf("invalid --replace-ref NEW %q: %w", replacement, err)
		}
		if previous, exists := replaceTargets[old]; exists {
			if previous != replacement {
				return RewriteOptions{}, fmt.Errorf("conflicting --replace-ref assignments for %q", old)
			}
			continue
		}
		replaceTargets[old] = replacement
		options.Replacements = append(options.Replacements, RefReplacement{Old: old, New: replacement})
	}
	reduceTargets := make(map[string]string)
	for _, rule := range reductions {
		if rule == "any" {
			options.ReduceAny = true
			continue
		}
		top, target, hasTarget := strings.Cut(rule, "=")
		if top == "" || (hasTarget && target == "") {
			return RewriteOptions{}, fmt.Errorf("invalid --reduce-ref %q: expected A, A=X, or any", rule)
		}
		var err error
		top, err = canonicalRewriteRef(top)
		if err != nil {
			return RewriteOptions{}, fmt.Errorf("invalid --reduce-ref selector %q: %w", top, err)
		}
		if hasTarget {
			target, err = canonicalRewriteRef(target)
			if err != nil {
				return RewriteOptions{}, fmt.Errorf("invalid --reduce-ref target %q: %w", target, err)
			}
		}
		if previous, exists := reduceTargets[top]; exists {
			if previous != target {
				return RewriteOptions{}, fmt.Errorf("conflicting --reduce-ref assignments for %q", top)
			}
			continue
		}
		reduceTargets[top] = target
		options.Reductions = append(options.Reductions, RefReduction{Top: top, Target: target})
	}
	for _, reduction := range options.Reductions {
		if replacement, exists := replaceTargets[reduction.Top]; exists && reduction.Target != "" && replacement != reduction.Target {
			return RewriteOptions{}, fmt.Errorf("conflicting replacement and reduction targets for %q", reduction.Top)
		}
	}
	if options.ReduceAny && len(options.Reductions) != 0 {
		return RewriteOptions{}, errors.New("--reduce-ref=any cannot be combined with explicit --reduce-ref selectors")
	}
	return options, nil
}

func canonicalRewriteRef(raw string) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", err
	}
	return ref.String(), nil
}

func (o RewriteOptions) empty() bool {
	return len(o.Replacements) == 0 && len(o.Reductions) == 0 && !o.ReduceAny
}

type rewriteSession struct {
	options            RewriteOptions
	matchedReplacement []bool
	matchedReduction   []bool
}

func newRewriteSession(options RewriteOptions) *rewriteSession {
	return &rewriteSession{options: options, matchedReplacement: make([]bool, len(options.Replacements)), matchedReduction: make([]bool, len(options.Reductions))}
}

func (s *rewriteSession) replace(raw string) string {
	if s == nil || raw == "" || raw == "self" {
		return raw
	}
	for i, rule := range s.options.Replacements {
		if raw == rule.Old {
			s.matchedReplacement[i] = true
			return rule.New
		}
	}
	return raw
}

func (s *rewriteSession) reduction(top string, hasLowers bool) (string, bool) {
	if s == nil {
		return "", false
	}
	if s.options.ReduceAny {
		return "", hasLowers
	}
	for i, rule := range s.options.Reductions {
		if top == rule.Top {
			s.matchedReduction[i] = true
			return rule.Target, true
		}
	}
	return "", false
}

func (s *rewriteSession) unmatched() error {
	if s == nil {
		return nil
	}
	for i, matched := range s.matchedReplacement {
		if !matched {
			return fmt.Errorf("unmatched --replace-ref selector %q", s.options.Replacements[i].Old)
		}
	}
	for i, matched := range s.matchedReduction {
		if !matched {
			return fmt.Errorf("unmatched --reduce-ref selector %q", s.options.Reductions[i].Top)
		}
	}
	return nil
}
