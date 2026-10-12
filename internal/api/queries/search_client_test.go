package queries_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/knowledge"
)

// THE DASHBOARD SENDS NO SEARCH THE ENGINE WOULD REFUSE.
//
// The palette, the work search and the knowledge screen ask `work_search`,
// `knowledge` and `answer_knowledge` with what a person typed, and the engine
// refuses a phrase past [knowledge.MaxQueryBytes] as `bad_params`. So each
// withholds a longer phrase and says why, which only works while the two
// numbers agree: a dashboard bound above the engine's sends phrases that are
// refused — a list that silently loses its results — and one below it tells a
// person their phrase is too long when the engine would have searched it.
func TestTheDashboardSendsNoSearchTheEngineWouldRefuse(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "SEARCH_QUERY_MAX")
	if err != nil {
		t.Fatal(err)
	}
	client, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("SEARCH_QUERY_MAX is %q, which is not an integer: %v", raw, err)
	}
	if client != knowledge.MaxQueryBytes {
		t.Errorf("the dashboard searches phrases up to %d bytes and the engine takes up to %d — "+
			"change SEARCH_QUERY_MAX in contract/wire.ts to the engine's number",
			client, knowledge.MaxQueryBytes)
	}
}
