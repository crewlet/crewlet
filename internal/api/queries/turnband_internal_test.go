package queries

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// row is one stored row of a turn, failed or not as the store reads it back.
func row(typ string, failed bool) store.EventRecord {
	return store.EventRecord{ID: typ, Type: typ, Failed: failed}
}

// failedEnd is the turn's own completion record, failed — what the engine
// closes a broken turn with.
var failedEnd = row("agent_turn_completed", true)

// THE PROBLEM COUNT IS THE ROWS THAT WENT WRONG, WITH THE TURN'S OWN STOP
// COUNTED ONCE. Each case is a way the Turn header's badge once disagreed with
// the panel under it: a stop the engine wrote two records about read as two
// problems, a failure with no record of its own read as none, and a dedupe
// written as a plain `||` swallowed a second, independent problem.
func TestATurnsProblemsCountItsStopOnce(t *testing.T) {
	t.Parallel()
	cases := []struct {
		why  string
		rows []store.EventRecord
		want int
	}{
		{"a guard breach beside the failed completion is one stop",
			[]store.EventRecord{row("turn.guard_breach", true), failedEnd}, 1},
		{"a budget refusal beside the failed completion is one stop",
			[]store.EventRecord{row("budget_exhausted", true), failedEnd}, 1},
		{"a chain with nothing left beside the failed completion is one stop",
			[]store.EventRecord{row("llm_unavailable", true), failedEnd}, 1},
		{"a turn the reviewer failed carries the flag and no record of its own",
			[]store.EventRecord{failedEnd}, 1},
		{"a recovered fallback is a second, independent problem",
			[]store.EventRecord{row("provider_fallback", false), failedEnd}, 2},
		{"a failed coding run does not describe the turn's own stop",
			[]store.EventRecord{row("sandbox_run_failed", true), failedEnd}, 2},
		{"the dedupe fires when the stop is among several rows",
			[]store.EventRecord{row("provider_fallback", false),
				row("turn.guard_breach", true), failedEnd}, 2},
		{"a fallback on a turn that ended well is still a problem",
			[]store.EventRecord{row("provider_fallback", false),
				row("agent_turn_completed", false)}, 1},
		{"a clean turn has nothing to count",
			[]store.EventRecord{row("agent_phase_completed", false),
				row("agent_turn_completed", false)}, 0},
		{"a record that no turn ran is not this turn's problem",
			[]store.EventRecord{row("notification_skipped", false)}, 0},
		// THE NEWEST COMPLETION DECIDES: a turn that parked on a coding run
		// completes once per segment, and its first segment's record — not
		// failed, only suspended — says nothing about how the turn ended.
		{"a parked turn that failed after it resumed failed",
			[]store.EventRecord{row("agent_turn_completed", false), failedEnd}, 1},
	}
	for _, c := range cases {
		if _, _, got := bandTurn(c.rows); got != c.want {
			t.Errorf("%s: problems = %d, want %d", c.why, got, c.want)
		}
	}
}

// EVERY ROW CARRIES THE BAND THE ONE RULE GIVES IT, and the absorbed ones are
// inventoried by type, counted across the whole turn rather than run by run,
// each with the destination [types.AbsorbedInto] names.
func TestATurnsRowsAreFiledAndItsAbsorbedRowsInventoried(t *testing.T) {
	t.Parallel()
	turn := []store.EventRecord{
		row("agent_phase_started", false),
		row("agent_phase_completed", false),
		row("provider_fallback", false),
		row("agent_phase_started", false),
		row("agent_phase_completed", true),
		row("knowledge_read", false),
		row("agent_turn_completed", false),
	}
	rows, absorbed, _ := bandTurn(turn)
	if len(rows) != len(turn) {
		t.Fatalf("%d rows back for %d read — a row the query returned and the "+
			"answer dropped is indistinguishable from one the store never held",
			len(rows), len(turn))
	}
	for i, r := range rows {
		if want := types.BandOf(turn[i].Type, turn[i].Failed); r.Band != want ||
			r.ID != turn[i].ID {
			t.Errorf("row %d (%s) = %q, want %q in read order", i, r.Type, r.Band, want)
		}
	}
	want := []AbsorbedGroup{
		{"agent_phase_started", types.AbsorbedInto("agent_phase_started"), 2},
		{"agent_phase_completed", types.AbsorbedInto("agent_phase_completed"), 2},
		{"agent_turn_completed", types.AbsorbedInto("agent_turn_completed"), 1},
	}
	if len(absorbed) != len(want) {
		t.Fatalf("absorbed = %+v, want %+v", absorbed, want)
	}
	for i := range want {
		if absorbed[i] != want[i] {
			t.Errorf("absorbed[%d] = %+v, want %+v", i, absorbed[i], want[i])
		}
	}
	if _, absorbed, _ := bandTurn(nil); absorbed == nil {
		t.Error("an empty turn answers a nil inventory, which is `null` on the " +
			"wire rather than the empty list a reader can range over")
	}
}
