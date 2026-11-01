package codeindex_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/codeindex"
)

// THE DASHBOARD OFFERS EVERY MODE OF A SEARCH, in the engine's order, and no
// other.
//
// Code › Search offers `CODE_SEARCH_MODES` (`contract/code.ts`) as the `mode`
// it asks `code_search` with: a mode the engine grew that the list lacks is
// one nobody can choose, and a mode the list keeps that the engine dropped is
// a search every press of it is refused `bad_params`. Read from the client's
// own source, both ways, in order.
//
// Mutation: drop a mode from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardOffersEveryModeOfASearch(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "CODE_SEARCH_MODES")
	if err != nil {
		t.Fatal(err)
	}
	got := clientsource.Strings(body)
	if len(got) == 0 {
		t.Fatal("CODE_SEARCH_MODES reads as empty, so this gate certifies nothing")
	}
	want := make([]string, 0, len(codeindex.Modes))
	for _, m := range codeindex.Modes {
		want = append(want, string(m))
	}
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's CODE_SEARCH_MODES is %v, want the engine's, in order: %v", got, want)
	}
}
