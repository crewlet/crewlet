package textindex_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/textindex"
)

// A NON-POSITIVE SNIPPET BUDGET IS EMPTY, NEVER THE WHOLE DOCUMENT.
//
// This pins the half of the contract a reader is most likely to carry over
// from somewhere else: knowledge.Snippet has the same name, does the same job
// in the same tree, and reads a limit of zero or less as UNBOUNDED. The two
// disagree on purpose and [textindex.Snippet]'s doc says why — this budget is
// the WIDTH OF A WINDOW, and a width of zero is zero bytes of it, the same
// reading textcut.Bytes gives a non-positive byte budget.
//
// The harmonisation this guards against is not hypothetical tidying. Reading
// it as unbounded would make an unset field return whole documents, times the
// hit count, times the phase's round cap, into a prompt somebody is billed
// for — a cap of zero that returns everything — and nothing downstream would
// report it, because a large snippet is indistinguishable from a large page.
func TestANonPositiveSnippetBudgetIsEmptyRatherThanUnbounded(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("the rollback procedure is documented here. ", 50)
	for _, limit := range []int{0, -1, -200} {
		got := textindex.Snippet(body, []string{"rollback"}, limit)
		if got != "" {
			t.Errorf("Snippet(_, _, %d) returned %d bytes; a budget of %d is "+
				"zero bytes of window, not the whole document",
				limit, len(got), limit)
		}
	}
}

// A BUDGET TOO SMALL TO HOLD CONTENT LANDS IN THE SAME PLACE, and the answer
// is the empty string rather than a marker alone: a snippet that is only an
// ellipsis spends prompt bytes to say nothing, while an absent one leaves the
// hit's title and id — which is what a pointer is for — standing on their own.
// Whatever it returns, it is never longer than the marker overrun the doc
// declares, and never invalid UTF-8.
func TestASnippetBudgetTooSmallForContentYieldsNothing(t *testing.T) {
	t.Parallel()
	// A CJK run: no spaces to trim to, and every rune three bytes, so a
	// one-byte window cannot hold even one of them.
	if got := textindex.Snippet("設定を確認してください", []string{"設定"}, 1); got != "" {
		t.Errorf("a one-byte window produced %q, want nothing", got)
	}
}

// THE MARKER IS NOT COUNTED AGAINST THE BUDGET, and the overrun is bounded at
// two markers.
//
// [textindex.Snippet] follows textcut.Ellipsis's rule — the budget bounds the
// CONTENT — which is right for a caller whose limit is a prompt-cost guide and
// wrong for one whose limit is a ceiling something refuses at. This pins the
// bound so the second kind of caller cannot arrive believing the first kind's
// promise: at most limit bytes of content plus a leading and a trailing "…".
func TestASnippetOverrunsItsBudgetByAtMostTwoMarkers(t *testing.T) {
	t.Parallel()
	const marker = "…"
	body := strings.Repeat("filler words here. ", 40) +
		"the rollback procedure. " + strings.Repeat("more filler. ", 40)
	for _, limit := range []int{20, 60, 120, 200} {
		got := textindex.Snippet(body, []string{"rollback"}, limit)
		if ceiling := limit + 2*len(marker); len(got) > ceiling {
			t.Errorf("Snippet(_, _, %d) is %d bytes, past the %d-byte ceiling of "+
				"the budget plus two markers: %q", limit, len(got), ceiling, got)
		}
	}
}
