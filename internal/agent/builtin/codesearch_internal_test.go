package builtin

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// shortestLines is a search's answer of n of the shortest lines that carry
// text (a one-byte repository and path, and one byte of text, fenced, which
// matched) of a search that stopped at them, each with context one-byte
// lines on each side.
func shortestLines(n, context int) codesearch.Answer {
	a := codesearch.Answer{Tips: map[string]string{"a": strings.Repeat("c", 40)},
		Coverage: codesearch.Coverage{Searched: 1, Truncated: true}}
	for i := range n {
		h := gitlet.SearchHit{Path: "a", Line: (2*context+1)*i + context + 1, Text: "x",
			Blob: strings.Repeat("b", 40), Matches: [][2]int{{0, 1}}}
		for range context {
			h.Before, h.After = append(h.Before, "y"), append(h.After, "y")
		}
		a.Hits = append(a.Hits, codesearch.Hit{Repo: "a", SearchHit: h})
	}
	return a
}

// A PAGE OF code_search ASKS FOR WHAT ONE PAGE HOLDS (searchPageHits, and
// searchContextPageHits for a page asking three lines of context): one tool
// answer, as sent, holds at least that many of the shortest lines that carry
// text, each with its match, and fewer than a twentieth more, so the search
// is never asked for lines no page of them can carry, nor cut into pages
// shorter than an answer holds; and a page that holds every line its search
// answered, the search having stopped at them, names the page after it:
// lines may follow.
//
// Mutations: ask for gitlet.MaxSearchHits — one answer holds far fewer, red;
// name the next page only where the page was cut (searchPage's Truncated) —
// the full page of a search that stopped names none, and paging ends with
// lines unread, red.
func TestCodeSearchAsksForWhatOnePageHolds(t *testing.T) {
	t.Parallel()
	search := codeindex.Query{Text: "x", Mode: codeindex.ModeLiteral}
	for _, tc := range []struct {
		hits, context int
	}{{searchPageHits, 0}, {searchContextPageHits, gitlet.MaxSearchContext}} {
		over := tc.hits + tc.hits/20
		held, _, err := fitLabelled(over, searchPage(nil, "", search, 0, shortestLines(over, tc.context)))
		if err != nil {
			t.Fatal(err)
		}
		if held < tc.hits || held >= over {
			t.Errorf("one answer holds %d of the shortest lines with %d of context, and a page asks for %d: "+
				"want what one holds, within a twentieth", held, tc.context, tc.hits)
		}
		k, rendered, err := fitLabelled(tc.hits, searchPage(nil, "", search, 0, shortestLines(tc.hits, tc.context)))
		if err != nil {
			t.Fatal(err)
		}
		if k != tc.hits || !strings.Contains(string(rendered), `"next":`) {
			t.Errorf("a page of all %d lines its stopped search answered held %d and named no next page: %t",
				tc.hits, k, !strings.Contains(string(rendered), `"next":`))
		}
	}
	if searchContextPageHits > gitlet.MaxSearchContextHits {
		t.Errorf("a page asking context asks %d hits, past the %d a search answering context answers",
			searchContextPageHits, gitlet.MaxSearchContextHits)
	}
}
