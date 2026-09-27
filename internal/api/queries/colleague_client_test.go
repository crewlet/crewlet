package queries_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD SENDS NO NAME THE ENGINE WOULD REFUSE.
//
// The command palette asks `colleague{q}` with whatever is in its box, and
// the engine refuses a `q` past [queries.ColleagueQueryMax] as `bad_params`.
// A refusal there is silent — the Agents list simply loses the engine's name
// tiers — so the palette withholds a longer term and lets the chart's own
// matching answer it. That only works while the two agree: a dashboard cap
// above the engine's sends terms that are refused, and one below it withholds
// names the engine would have resolved.
func TestTheDashboardSendsNoNameTheEngineWouldRefuse(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "COLLEAGUE_QUERY_MAX")
	if err != nil {
		t.Fatal(err)
	}
	client, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("COLLEAGUE_QUERY_MAX is %q, which is not an integer: %v", raw, err)
	}
	if client != queries.ColleagueQueryMax {
		t.Errorf("the dashboard sends names up to %d bytes and the engine resolves up to %d — "+
			"change COLLEAGUE_QUERY_MAX in contract/wire.ts to the engine's number",
			client, queries.ColleagueQueryMax)
	}
}
