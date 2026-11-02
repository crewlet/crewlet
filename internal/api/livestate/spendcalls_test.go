package livestate_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/tokens"
)

// THE LIVE WINDOW COUNTS A TURN'S CALLS AS THE STORE DOES: a coding run's own
// turns as its CLI reported them, and none for the executor's own record — the
// run is its CLI's, and its one round is the engine's request, not a provider
// call. Counted any other way, a turn read off the live window and the same
// turn read off the store would disagree on every screen that says "N calls".
//
// Mutation: read the record without its phase or its `cli_turns`, and this
// goes red.
func TestTheLiveWindowCountsARunsTurnsAndNoneForItsExecutor(t *testing.T) {
	t.Parallel()
	s := seededState(t)
	s.Apply(env("agent_phase_completed", map[string]any{
		"agent_id": "Lead", "role": "Lead", "phase": "execute", "turn_id": "tn-1",
		"rounds": []any{map[string]any{"round": 1}}, "rounds_used": 1,
	}, id("exec-1"), at("2026-06-14T11:50:00Z")))
	s.Apply(env("agent_phase_completed", map[string]any{
		"agent_id": "Lead", "role": "Lead", "phase": "sandbox", "turn_id": "tn-1",
		"cli_turns": 12, "input_tokens": 900, "output_tokens": 100, "total_tokens": 1000,
	}, id("run-1"), at("2026-06-14T11:50:00Z")))

	rollup := tokens.Aggregate(s.SpendRecords(), tokens.Options{})
	if rollup.Totals.Calls != 12 {
		t.Errorf("the live window counted %d calls, want the run's 12 turns and "+
			"none for the executor's own record", rollup.Totals.Calls)
	}
}
