package codeindex

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD SEARCHES CODE AT THE TRIGRAM'S WIDTH: the palette's Code scope
// asks for a term only once it is as long as a trigram ([trigramBytes]),
// because the plan refuses `bad_params` for a term with no literal that long —
// a refusal per keystroke a reader would read as a broken search. A width
// above the engine's leaves a search unasked that it would answer; one below
// asks for refusals.
//
// Mutation: move either figure and this fails.
func TestTheDashboardSearchesCodeAtTheTrigramsWidth(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "CODE_SEARCH_TRIGRAM_BYTES")
	if err != nil {
		t.Fatal(err)
	}
	if got != strconv.Itoa(trigramBytes) {
		t.Errorf("the dashboard's CODE_SEARCH_TRIGRAM_BYTES is %s and a trigram is %d bytes: "+
			"change it in contract/objectids.ts", got, trigramBytes)
	}
}
