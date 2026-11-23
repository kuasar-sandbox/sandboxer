package artifact

import (
	"strings"
	"testing"
)

func rewriteTestRef(digit byte) string {
	return "manifest://" + strings.Repeat(string(digit), 64)
}

func TestRewriteRulesUseBaseContentIdentities(t *testing.T) {
	a, b, c, d := rewriteTestRef('a'), rewriteTestRef('b'), rewriteTestRef('c'), rewriteTestRef('d')
	annotation := "@runtime:runtime.bundle:digest:" + strings.Repeat("c", 64)
	options, err := ParseRewriteOptions([]string{a + annotation + "=" + b + annotation}, []string{c + annotation + "=" + d}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Replacements) != 1 || options.Replacements[0].Old != a || options.Replacements[0].New != b || options.Reductions[0].Top != c {
		t.Fatalf("rewrite rules retained scheduling metadata: %+v", options)
	}
}

func TestParseRewriteOptionsPreservesIndependentRules(t *testing.T) {
	a, b, c, d := rewriteTestRef('a'), rewriteTestRef('b'), rewriteTestRef('c'), rewriteTestRef('d')
	e, f := rewriteTestRef('e'), rewriteTestRef('f')
	options, err := ParseRewriteOptions([]string{a + "=" + b, c + "=" + d}, []string{e + "=" + f}, true)
	if err != nil {
		t.Fatal(err)
	}
	session := newRewriteSession(options)
	if got := session.replace(a); got != b {
		t.Fatalf("A replacement = %q, want B", got)
	}
	if got := session.replace(c); got != d {
		t.Fatalf("C replacement = %q, want D", got)
	}
	if !options.SkipVerify {
		t.Fatal("skip verification was lost")
	}
}

func TestParseRewriteOptionsRejectsConflictsBeforePublish(t *testing.T) {
	a, b, c := rewriteTestRef('a'), rewriteTestRef('b'), rewriteTestRef('c')
	for _, test := range []struct{ replacements, reductions []string }{
		{replacements: []string{a + "=" + b, a + "=" + c}},
		{reductions: []string{a + "=" + b, a + "=" + c}},
		{reductions: []string{"any", a}},
	} {
		if _, err := ParseRewriteOptions(test.replacements, test.reductions, false); err == nil {
			t.Fatalf("accepted conflicting rules %+v", test)
		}
	}
}

func TestRewriteSessionReportsUnmatchedExplicitSelector(t *testing.T) {
	a, b := rewriteTestRef('a'), rewriteTestRef('b')
	options, err := ParseRewriteOptions([]string{a + "=" + b}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := newRewriteSession(options).unmatched(); err == nil || !strings.Contains(err.Error(), a) {
		t.Fatalf("unmatched error = %v", err)
	}
}
