package runner

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
)

// An extended phase runs the tool loop MORE THAN ONCE, and the loop numbers
// its rounds per invocation from 1. Without a shift, the second invocation
// restarts at 1 and the phase reads as running backwards: the live
// projection's stale-round guard (`roundNum < cur.RoundNum`) drops every
// extension round, so the dashboard freezes for the rest of the phase, and
// the ledger merges extension round 1 into original round 1.
func TestAnExtensionContinuesTheRoundCountInsteadOfRestartingIt(t *testing.T) {
	t.Parallel()
	second := toolloop.Result{
		RoundsUsed: 3,
		Executions: []toolloop.Execution{{Round: 1, Name: "search"}, {Round: 3, Name: "post"}},
		Narration:  []toolloop.Narration{{Round: 1, Content: "again"}, {Round: 3, Content: "done"}},
	}

	got := offsetRounds(second, 20)

	if got.RoundsUsed != 23 {
		t.Errorf("RoundsUsed = %d, want the phase total 23", got.RoundsUsed)
	}
	if got.Executions[0].Round != 21 || got.Executions[1].Round != 23 {
		t.Errorf("execution rounds = %d,%d — want 21,23",
			got.Executions[0].Round, got.Executions[1].Round)
	}
	if got.Narration[0].Round != 21 || got.Narration[1].Round != 23 {
		t.Errorf("narration rounds = %d,%d — want 21,23",
			got.Narration[0].Round, got.Narration[1].Round)
	}
	// A round's narration and the calls it asked for must still carry the
	// SAME number after the shift, or the ledger cannot interleave them.
	if got.Executions[0].Round != got.Narration[0].Round {
		t.Error("the shift desynchronised a round's narration from its own tool calls")
	}
}

// COPIES. The Result handed to OnProgress is the loop's live snapshot,
// published from another goroutine while the loop is still appending to those
// same slices; renumbering in place would corrupt them under the writer.
func TestTheShiftDoesNotMutateTheLoopsOwnSnapshot(t *testing.T) {
	t.Parallel()
	execs := []toolloop.Execution{{Round: 1, Name: "search"}}
	narr := []toolloop.Narration{{Round: 1, Content: "hello"}}
	live := toolloop.Result{RoundsUsed: 1, Executions: execs, Narration: narr}

	_ = offsetRounds(live, 8)

	if execs[0].Round != 1 {
		t.Errorf("the loop's own execution was renumbered to %d", execs[0].Round)
	}
	if narr[0].Round != 1 {
		t.Errorf("the loop's own narration was renumbered to %d", narr[0].Round)
	}
}

// The first invocation must pass through untouched — there is nothing before
// it to continue from, and a needless copy of every round on the common path
// is waste.
func TestTheFirstInvocationIsNotShifted(t *testing.T) {
	t.Parallel()
	in := toolloop.Result{RoundsUsed: 2, Executions: []toolloop.Execution{{Round: 1}}}
	got := offsetRounds(in, 0)
	if got.RoundsUsed != 2 || got.Executions[0].Round != 1 {
		t.Errorf("an unextended phase was renumbered: %+v", got)
	}
}
