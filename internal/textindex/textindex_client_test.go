package textindex_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/textindex"
)

// THE DASHBOARD'S SHORTEST SEARCH WORD IS THE INDEX'S.
//
// The chat search screen sends a query only when it holds a word the index
// keeps (`SEARCH_TERM_MIN`, contract/wire.ts), and says how long one must be
// otherwise. A query of shorter words analyzes to no terms here, so the engine
// answers it with nothing; a screen that sent it drew that as "nothing
// matched" and advised fewer words. Below the index's floor, the screen would
// send what can match nothing; above it, it would refuse a word that matches.
//
// Mutation: change SEARCH_TERM_MIN in contract/wire.ts, or MinTermLength, and
// this goes red.
func TestTheDashboardsShortestSearchWordIsTheIndexs(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "SEARCH_TERM_MIN")
	if err != nil {
		t.Fatal(err)
	}
	client, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("SEARCH_TERM_MIN is %q, which is not an integer: %v", raw, err)
	}
	if client != textindex.MinTermLength {
		t.Errorf("the dashboard searches for words of %d bytes or more and the index keeps "+
			"words of %d (textindex.MinTermLength); the engine owns the value, so "+
			"contract/wire.ts is what changes", client, textindex.MinTermLength)
	}
}
