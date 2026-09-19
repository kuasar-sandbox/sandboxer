package artifact

import (
	"strings"
	"testing"
)

func rewriteTestRef(digit byte) string {
	return "manifest://" + strings.Repeat(string(digit), 64)
}

func TestParseRewriteOptionsPreservesOnePassRules(t *testing.T) {
	a, b, c := rewriteTestRef('a'), rewriteTestRef('b'), rewriteTestRef('c')
	options, err := ParseRewriteOptions([]string{a + "=" + b, b + "=" + c}, []string{c + "=" + a}, true)
	if err != nil {
		t.Fatal(err)
	}
	session := newRewriteSession(options)
	if got := session.replace(a); got != b {
		t.Fatalf("A replacement = %q, want B", got)
	}
	if got := session.replace(b); got != c {
		t.Fatalf("B replacement = %q, want C", got)
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
