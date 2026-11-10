package queries

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/usage"
)

// THE TURNS THAT CHANGED NOTHING, PER AGENT AND WAKE: every node's started turns
// summed by what woke them, a held wake never counted as a turn, the spend of
// those that committed nothing apart, and each agent named by the chart's handle.
func TestTurnOutcomesSumStartedTurnsByAgentAndWake(t *testing.T) {
	t.Parallel()
	rows := []usage.TurnLedgerRow{
		{AgentID: "a-1", Handle: "old", Wake: "schedule:nightly", Disposition: "started", Outcome: "nothing",
			Turns: 3, Input: 100, Output: 20},
		{AgentID: "a-1", Wake: "schedule:nightly", Disposition: "started", Outcome: "nothing",
			Turns: 2, Input: 50, CacheRead: 30},
		{AgentID: "a-1", Wake: "schedule:nightly", Disposition: "started", Outcome: "moved",
			Turns: 1, Input: 40, AuxOutput: 5},
		{AgentID: "a-1", Wake: "schedule:nightly", Disposition: "held", Outcome: "", Turns: 9},
		{AgentID: "a-2", Wake: "work_item:pickup", Disposition: "started", Outcome: "words", Turns: 4, Output: 8},
	}
	chart := map[string]chartSeat{"a-1": {handle: "ops", agentID: "a-1"}}
	got := foldTurnOutcomes(rows, chart, "")
	want := []TurnOutcomeRow{
		{Seat: "a-2", AgentID: "a-2", Wake: "work_item:pickup", Turns: 4, Tokens: 8},
		{Seat: "ops", AgentID: "a-1", Wake: "schedule:nightly", Turns: 6, Tokens: 245,
			Nothing: 5, NothingTokens: 200},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("folded\n%+v\nwant\n%+v", got, want)
	}
	if only := foldTurnOutcomes(rows, chart, "a-2"); len(only) != 1 || only[0].AgentID != "a-2" {
		t.Errorf("a window narrowed to a-2 gave %+v", only)
	}
}
