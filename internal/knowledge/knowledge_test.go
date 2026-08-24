package knowledge_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/knowledge"
)

func TestScopeNormalisesAndKeepsTheOperatorsOrder(t *testing.T) {
	got := knowledge.Scope([]string{" eng ", "ENG", "ops", "", "   ", "Docs"})
	if !slices.Equal(got, []string{"ENG", "OPS", "DOCS"}) {
		t.Fatalf("Scope = %v", got)
	}
	// Nothing and nothing-but-blanks are ONE answer, so a caller cannot
	// accidentally treat a list of empty strings as a scope.
	for _, in := range [][]string{nil, {}, {"", "  "}} {
		if got := knowledge.Scope(in); got != nil {
			t.Fatalf("Scope(%v) = %#v, want nil", in, got)
		}
	}
}

// An empty scope is not "search everything": it is "search everything THIS
// SEAT's own account can read", which is only meaningful when the seat has an
// account. A seat on the shared engine credential reads nothing.
func TestUnscopedIsNotUnbounded(t *testing.T) {
	cases := []struct {
		name             string
		scope            []string
		selfAuth         bool
		allowed, unscope bool
	}{
		{"scoped, own account", []string{"ENG"}, true, true, false},
		{"scoped, shared credential", []string{"ENG"}, false, true, false},
		{"unscoped, own account", nil, true, true, true},
		{"unscoped, shared credential", nil, false, false, false},
	}
	for _, c := range cases {
		allowed, unscoped := knowledge.Permitted(c.scope, c.selfAuth)
		if allowed != c.allowed || unscoped != c.unscope {
			t.Errorf("%s: Permitted = %v/%v, want %v/%v",
				c.name, allowed, unscoped, c.allowed, c.unscope)
		}
	}
}

// A scope narrows; it never widens. A seat on a shared credential with an
// explicit scope searches that scope — the operator named it.
func TestAScopeAdmitsASeatWithNoAccountOfItsOwn(t *testing.T) {
	if allowed, unscoped := knowledge.Permitted([]string{"ENG"}, false); !allowed || unscoped {
		t.Fatalf("Permitted = %v/%v, want a scoped search", allowed, unscoped)
	}
}

func TestTheDefaultExclusionHidesDrafts(t *testing.T) {
	// Nil means "the caller expressed none", which takes the safe default.
	if got := (knowledge.Query{}).Excluded(); !slices.Equal(got, []string{knowledge.AutoDraftedParent}) {
		t.Fatalf("the default exclusion is %v", got)
	}
	// An EMPTY non-nil slice disables it — a caller who deliberately wants
	// to search drafts means it.
	if got := (knowledge.Query{ExcludeAncestors: []string{}}).Excluded(); len(got) != 0 {
		t.Fatalf("an explicit empty exclusion became %v", got)
	}
	if got := (knowledge.Query{ExcludeAncestors: []string{"Archive"}}).Excluded(); !slices.Equal(got, []string{"Archive"}) {
		t.Fatalf("an explicit exclusion became %v", got)
	}
}

func TestExcludesDropsHitsUnderAnExcludedParent(t *testing.T) {
	drafts := []string{knowledge.AutoDraftedParent}
	under := knowledge.Hit{Title: "Deploy runbook", Ancestors: []string{"Engineering", "Auto-Drafted Skills"}}
	if !knowledge.Excludes(under, drafts) {
		t.Fatal("a page under the draft parent was not excluded")
	}
	// Case-insensitive: a backend hands back whatever somebody typed, and
	// an exclusion missing on capitalisation leaks exactly what it hides.
	loud := knowledge.Hit{Title: "Deploy runbook", Ancestors: []string{"AUTO-DRAFTED SKILLS"}}
	if !knowledge.Excludes(loud, drafts) {
		t.Fatal("the exclusion is case-sensitive")
	}
	published := knowledge.Hit{Title: "Deploy runbook", Ancestors: []string{"Engineering"}}
	if knowledge.Excludes(published, drafts) {
		t.Fatal("a published page was excluded")
	}
	if knowledge.Excludes(under, nil) {
		t.Fatal("an empty exclusion list excluded something")
	}
}

// The FAIL-CLOSED BACKSTOP: a backend whose ancestor lookup came back empty
// must hide drafts rather than leak them.
func TestTheTitlePrefixCatchesADraftWithNoAncestorChain(t *testing.T) {
	orphan := knowledge.Hit{Title: knowledge.AutoDraftTitlePrefix + "Deploy runbook"}
	if !knowledge.Excludes(orphan, []string{knowledge.AutoDraftedParent}) {
		t.Fatal("a draft with no ancestor chain was not excluded")
	}
	// But only while drafts are actually being hidden — a caller who asked
	// to see them means it, and a different exclusion is a different ask.
	if knowledge.Excludes(orphan, []string{"Archive"}) {
		t.Fatal("an unrelated exclusion hid a draft the caller asked for")
	}
	if knowledge.Excludes(orphan, nil) {
		t.Fatal("no exclusion at all still hid a draft")
	}
	// And a lead who moved a draft out WITHOUT renaming it has published
	// it: moving is the gesture that means reviewed, renaming is optional.
	// The prefix must not outrank a chain that came back clean, or every
	// published draft stays invisible until somebody notices the title.
	moved := knowledge.Hit{
		Title:     knowledge.AutoDraftTitlePrefix + "Deploy runbook",
		Ancestors: []string{"Engineering"},
	}
	if knowledge.Excludes(moved, []string{knowledge.AutoDraftedParent}) {
		t.Fatal("a draft moved out of the parent is still hidden by its title")
	}
}

func TestSnippetCutsAtASentence(t *testing.T) {
	if got := knowledge.Snippet("  Deploy   the   thing.  "); got != "Deploy the thing." {
		t.Fatalf("Snippet collapsed to %q", got)
	}
	if got := knowledge.Snippet(""); got != "" {
		t.Fatalf("Snippet(\"\") = %q", got)
	}

	long := strings.Repeat("word ", 30) + "End of it. " + strings.Repeat("more ", 40)
	got := knowledge.Snippet(long)
	if len(got) > knowledge.SnippetLimit {
		t.Fatalf("Snippet is %d bytes, over the %d budget", len(got), knowledge.SnippetLimit)
	}
	if !strings.HasSuffix(got, "End of it.") {
		t.Fatalf("Snippet did not cut at the sentence: %q", got)
	}

	// With no sentence boundary in budget it cuts at a word and says so,
	// because a snippet ending mid-word reads as a rendering fault.
	noStop := strings.Repeat("word ", 100)
	got = knowledge.Snippet(noStop)
	if len(got) > knowledge.SnippetLimit+3 {
		t.Fatalf("Snippet is %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a mid-text cut did not mark itself: %q", got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") {
		t.Fatalf("the ellipsis follows a space: %q", got)
	}
}

func TestQueryDefaults(t *testing.T) {
	if got := (knowledge.Query{}).Hits(); got != knowledge.DefaultLimit {
		t.Fatalf("Hits = %d, want the default %d", got, knowledge.DefaultLimit)
	}
	for _, n := range []int{0, -1} {
		if got := (knowledge.Query{Limit: n}).Hits(); got != knowledge.DefaultLimit {
			t.Fatalf("Hits(%d) = %d", n, got)
		}
	}
	if got := (knowledge.Query{Limit: 3}).Hits(); got != 3 {
		t.Fatalf("Hits = %d", got)
	}
}
