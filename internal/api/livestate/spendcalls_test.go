package livestate_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/tokens"
)

// THE LIVE WINDOW COUNTS A TURN'S CALLS AS THE STORE DOES: each launch's own
// turns as its harness reported them, and none for the executor's own record — the
// run is its harness's, and the record's one round is the engine's request, not a
// provider call. Counted any other way, a turn read off the live window and
// the same turn read off the store would disagree on every screen that says
// "N calls". And the launch is the executor's spend: one execute row, holding
// what the run spent, whatever launches the round took.
//
// Mutation: read the launch's record without its `cli_turns`, or count the
// executor's round, and the calls go wrong; file the launch under no phase,
// and the execute row loses the run's tokens.
func TestTheLiveWindowCountsALaunchsTurnsAndNoneForItsExecutor(t *testing.T) {
	t.Parallel()
	s := seededState(t)
	s.Apply(env("agent_phase_completed", map[string]any{
		"agent_id": "Lead", "role": "Lead", "phase": "execute", "turn_id": "tn-1",
		"rounds": []any{map[string]any{"round": 1}}, "rounds_used": 1,
	}, id("exec-1"), at("2026-06-14T11:50:00Z")))
	s.Apply(env("executor_collected", map[string]any{
		"agent_id": "Lead", "role": "Lead", "turn_id": "tn-1", "iteration": 1,
		"launch_id": "job-1",
		"cli_turns": 12, "input_tokens": 900, "output_tokens": 100, "total_tokens": 1000,
	}, id("run-1"), at("2026-06-14T11:50:00Z")))

	rollup := tokens.Aggregate(s.SpendRecords(), tokens.Options{})
	if rollup.Totals.Calls != 12 {
		t.Errorf("the live window counted %d calls, want the run's 12 turns and "+
			"none for the executor's own record", rollup.Totals.Calls)
	}
	if len(rollup.ByPhase) != 1 || rollup.ByPhase[0].Phase != "execute" ||
		rollup.ByPhase[0].TotalTokens != 1000 {
		t.Errorf("phases = %+v, want one execute row holding the run's 1000 tokens", rollup.ByPhase)
	}
}
