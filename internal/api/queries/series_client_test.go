package queries_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/tokens"
)

// THE SPEND SCREEN ASKS FOR NO MORE BANDS THAN THE ENGINE NAMES: By unit asks
// a series for as many groups as one answer names ([tokens.MaxSeriesGroups]).
// A figure above it is an ask the engine does not answer as asked; one below
// folds units into "other" that the engine would have named.
//
// Mutation: move either figure and this fails.
func TestTheSpendScreenAsksForNoMoreBandsThanTheEngineNames(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "SERIES_MAX_GROUPS")
	if err != nil {
		t.Fatal(err)
	}
	if got != strconv.Itoa(tokens.MaxSeriesGroups) {
		t.Errorf("the dashboard's SERIES_MAX_GROUPS is %s and the engine names at most %d: "+
			"change it in contract/spend.ts", got, tokens.MaxSeriesGroups)
	}
}
