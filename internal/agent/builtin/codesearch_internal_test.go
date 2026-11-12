package builtin

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// shortestLines is a search's answer of n of the shortest lines that carry
// text — a one-byte repository and path, and one byte of text, fenced — of a
// search that stopped at them.
func shortestLines(n int) codesearch.Answer {
	a := codesearch.Answer{Tips: map[string]string{"a": strings.Repeat("c", 40)},
		Coverage: codesearch.Coverage{Searched: 1, Truncated: true}}
	for i := range n {
		a.Hits = append(a.Hits, codesearch.Hit{Repo: "a", SearchHit: gitlet.SearchHit{Path: "a", Line: i + 1,
			Text: "x", Blob: strings.Repeat("b", 40)}})
	}
	return a
}

// A PAGE OF code_search ASKS FOR WHAT ONE PAGE HOLDS (searchPageHits): one
// tool answer, as sent, holds at least searchPageHits of the shortest lines
// that carry text and fewer than a twentieth more — so the search is never
// asked for lines no page of them can carry, nor cut into pages shorter than
// an answer holds — and a page that holds every line its search answered,
// the search having stopped at them, names the page after it: lines may
// follow.
//
// Mutations: ask for gitlet.MaxSearchHits — one answer holds far fewer, red;
// name the next page only where the page was cut (searchPage's Truncated) —
// the full page of a search that stopped names none, and paging ends with
// lines unread, red.
func TestCodeSearchAsksForWhatOnePageHolds(t *testing.T) {
	t.Parallel()
	search := codeindex.Query{Text: "x", Mode: codeindex.ModeLiteral}
	over := searchPageHits + searchPageHits/20
	held, _, err := fitLabelled(over, searchPage(nil, "", search, shortestLines(over)))
	if err != nil {
		t.Fatal(err)
	}
	if held < searchPageHits || held >= over {
		t.Errorf("one answer holds %d of the shortest lines, and a page asks for %d: want what one holds, "+
			"within a twentieth", held, searchPageHits)
	}
	k, rendered, err := fitLabelled(searchPageHits, searchPage(nil, "", search, shortestLines(searchPageHits)))
	if err != nil {
		t.Fatal(err)
	}
	if k != searchPageHits || !strings.Contains(string(rendered), `"next":`) {
		t.Errorf("a page of all %d lines its stopped search answered held %d and named no next page: %t", searchPageHits,
			k, !strings.Contains(string(rendered), `"next":`))
	}
}
