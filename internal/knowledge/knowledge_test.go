package knowledge_test

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

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

// ON A BACKEND WHOSE SEATS CAN AUTHENTICATE AS THEMSELVES — Confluence — an
// empty scope is not "search everything": it is "search everything THIS SEAT's
// own account can read", which is only meaningful when the seat has an
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

// THE TITLE PREFIX JUDGES ONLY A CHAIN THE BACKEND COULD NOT READ.
//
// Fail closed where the chain is not known — a lookup that did not come back,
// a chain that ran into a parent the backend no longer holds — because an
// outage must hide drafts rather than leak them. And never where it IS known,
// an empty one included: a lead who moved a draft out WITHOUT renaming it has
// published it, whether it landed under another page or at the top of its
// container, because moving is the gesture that means reviewed and renaming
// is optional. A prefix that outranked a known chain would leave every
// published draft invisible until somebody noticed the title.
func TestTheTitlePrefixJudgesOnlyAChainTheBackendCouldNotRead(t *testing.T) {
	const title = knowledge.AutoDraftTitlePrefix + "Deploy runbook"
	drafts := []string{knowledge.AutoDraftedParent}
	for _, tc := range []struct {
		name     string
		hit      knowledge.Hit
		excluded []string
		want     bool
	}{
		{"no chain came back", knowledge.Hit{Title: title}, drafts, true},
		{"a chain cut short by a parent the backend no longer holds",
			knowledge.Hit{Title: title, Ancestors: []string{"Engineering"}}, drafts, true},
		{"moved to the top of its container: the known chain is empty",
			knowledge.Hit{Title: title, AncestorsKnown: true}, drafts, false},
		{"moved under another page",
			knowledge.Hit{Title: title, Ancestors: []string{"Engineering"}, AncestorsKnown: true},
			drafts, false},
		// THE CHAIN STILL DECIDES FIRST: a known chain that names the
		// draft parent is excluded whatever the title says.
		{"a known chain under the draft parent",
			knowledge.Hit{Title: "Deploy runbook", Ancestors: []string{knowledge.AutoDraftedParent},
				AncestorsKnown: true}, drafts, true},
		// ONLY WHILE DRAFTS ARE BEING HIDDEN: a caller who asked to see
		// them means it, and a different exclusion is a different ask.
		{"an unrelated exclusion", knowledge.Hit{Title: title}, []string{"Archive"}, false},
		{"no exclusion at all", knowledge.Hit{Title: title}, nil, false},
		// AND ONLY ON THE PREFIX: an unknown chain on an ordinary title is
		// not a reason to hide a page, or every outage empties the answer.
		{"an ordinary title with no chain", knowledge.Hit{Title: "Deploy runbook"}, drafts, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := knowledge.Excludes(tc.hit, tc.excluded); got != tc.want {
				t.Errorf("Excludes(%+v, %v) = %v, want %v", tc.hit, tc.excluded, got, tc.want)
			}
		})
	}
}

func TestSnippetCutsAtASentence(t *testing.T) {
	if got := knowledge.Snippet("  Deploy   the   thing.  ", knowledge.SnippetLimit); got != "Deploy the thing." {
		t.Fatalf("Snippet collapsed to %q", got)
	}
	if got := knowledge.Snippet("", knowledge.SnippetLimit); got != "" {
		t.Fatalf("Snippet(\"\") = %q", got)
	}

	// LENGTH ONLY. A cut at the first newline or ". " would decapitate a page
	// to its opening sentence whatever the budget — and at limit 0, where the
	// caller asked for no cut at all.
	multi := "First line. Second sentence follows.\nAnd a third."
	if got := knowledge.Snippet(multi, 0); got != "First line. Second sentence follows. And a third." {
		t.Fatalf("an unbounded snippet was still cut: %q", got)
	}

	long := strings.Repeat("word ", 30) + "End of it. " + strings.Repeat("more ", 40)
	got := knowledge.Snippet(long, knowledge.SnippetLimit)
	if len(got) > knowledge.SnippetLimit+len(" …") {
		t.Fatalf("Snippet is %d bytes, over the %d budget", len(got), knowledge.SnippetLimit)
	}
	// A sentence boundary is preferred, and the cut is STILL MARKED: the
	// page continues past it either way, and an unmarked cut is
	// indistinguishable from a page that really does end there.
	if !strings.HasSuffix(got, "End of it. …") {
		t.Fatalf("Snippet did not cut at the sentence, marked: %q", got)
	}

	// With no sentence boundary in budget it cuts at a word and says so,
	// because a snippet ending mid-word reads as a rendering fault.
	noStop := strings.Repeat("word ", 100)
	got = knowledge.Snippet(noStop, knowledge.SnippetLimit)
	if len(got) > knowledge.SnippetLimit+3 {
		t.Fatalf("Snippet is %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a mid-text cut did not mark itself: %q", got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") {
		t.Fatalf("the ellipsis follows a space: %q", got)
	}

	// NEVER THROUGH A RUNE, including the no-space fallback a URL or a CJK
	// run reaches. Invalid UTF-8 reaches a model as a replacement
	// character, which only shows up once a page stops being ASCII.
	cjk := strings.Repeat("日本語", 200)
	if got := knowledge.Snippet(cjk, knowledge.SnippetLimit); !utf8.ValidString(got) {
		t.Fatalf("a snippet cut through a rune: %q", got)
	}
	if got := knowledge.Snippet(cjk, knowledge.SnippetLimit); !strings.HasSuffix(got, "…") {
		t.Fatalf("a no-space cut did not mark itself: %q", got)
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

// THE STATES A SEARCH THAT CANNOT RUN IS IN ARE A CLOSED SET, so a caller
// branching on one reads an unknown value as a value rather than a match.
func TestUnsearchableIsAClosedSet(t *testing.T) {
	t.Parallel()
	for _, state := range []knowledge.Unsearchable{
		knowledge.NoBackend, knowledge.NotServed, knowledge.NoScope,
	} {
		if !state.Valid() {
			t.Errorf("%q is a state this build names and reports itself invalid", state)
		}
	}
	for _, state := range []knowledge.Unsearchable{"", "no_company", "NotServed"} {
		if state.Valid() {
			t.Errorf("%q reports itself a state", state)
		}
	}
}

// A REFUSAL NAMES ONE STATE, and each state sends a reader somewhere else — a
// backend to choose, a connection to repair, a scope to declare — so no two
// may share a sentence, and a refuser's own sentence outranks the state's.
func TestARefusalSaysWhichStateItIsIn(t *testing.T) {
	t.Parallel()
	if (knowledge.Refusal{}).Refused() {
		t.Error("the zero refusal refuses a search")
	}
	seen := map[string]knowledge.Unsearchable{}
	for _, state := range []knowledge.Unsearchable{
		knowledge.NoBackend, knowledge.NotServed, knowledge.NoScope,
	} {
		r := knowledge.Refusal{State: state}
		if !r.Refused() {
			t.Errorf("a refusal in state %q does not refuse", state)
		}
		reason := r.Reason()
		if reason == "" {
			t.Errorf("state %q has no sentence for a person", state)
		}
		if other, dup := seen[reason]; dup {
			t.Errorf("%q and %q share the sentence %q", other, state, reason)
		}
		seen[reason] = state
	}
	const detail = "the company's knowledge base is Confluence, and its connection did not build"
	if got := (knowledge.Refusal{State: knowledge.NotServed, Detail: detail}).Reason(); got != detail {
		t.Errorf("a refuser's own sentence became %q", got)
	}
	// A BLANK DETAIL IS NO DETAIL: the state's own sentence stands.
	blank := knowledge.Refusal{State: knowledge.NoScope, Detail: "  "}
	if got := blank.Reason(); got != (knowledge.Refusal{State: knowledge.NoScope}).Reason() {
		t.Errorf("a blank detail answered %q", got)
	}
}
