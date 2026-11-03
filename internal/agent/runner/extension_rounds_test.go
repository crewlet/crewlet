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

// THE ROUND IN FLIGHT IS ON THE SAME SCALE AS THE ROUNDS BEHIND IT.
//
// A consumer keys the ledger on the round number and the partial overwrites
// the block it lands on, so an unshifted partial from extension round 1 is
// written into the phase's committed round 1: that round's prose replaced by
// text the model is writing twenty rounds later, sitting directly
// above round 1's own tool calls, and no block at all for the round actually
// running. The abandoned attempts carry the same number and travel with it.
func TestTheRoundInFlightIsShiftedWithTheRoundsBehindIt(t *testing.T) {
	t.Parallel()
	live := toolloop.Result{
		RoundsUsed: 2,
		Narration:  []toolloop.Narration{{Round: 1, Content: "again"}},
		Partial: &toolloop.Partial{
			Round:     2,
			Content:   "still writing",
			Abandoned: []toolloop.Narration{{Round: 2, Content: "half a sentence"}},
		},
	}

	got := offsetRounds(live, 20)

	if got.Partial.Round != 22 {
		t.Errorf("partial round = %d, want 22", got.Partial.Round)
	}
	if got.Partial.Abandoned[0].Round != 22 {
		t.Errorf("abandoned round = %d, want 22", got.Partial.Abandoned[0].Round)
	}
	// The round being written cannot BE a round already committed, or the
	// ledger has one block claiming to be both.
	if got.Partial.Round <= got.Narration[len(got.Narration)-1].Round {
		t.Errorf("the in-flight round %d collides with committed round %d",
			got.Partial.Round, got.Narration[len(got.Narration)-1].Round)
	}
}

// AN EXTENSION'S HARNESS RUNS NAME THE PHASE'S ROUNDS. A run states the first and
// last round it made, and the loop numbers them per invocation like every
// other round, so an unshifted extension's first run claimed round 1 of a
// phase twenty rounds in — a run a timeline draws over the phase's opening,
// beside calls that are not its own. A run that made no request names none,
// and stays naming none.
//
// Mutation: leave the runs out of the shift, or shift a run that made no
// request, and this goes red.
func TestAnExtensionsRunsNameThePhasesRounds(t *testing.T) {
	t.Parallel()
	live := toolloop.Result{
		RoundsUsed: 3,
		Invocations: []toolloop.Invocation{
			{FirstRound: 1, LastRound: 2, Outcome: toolloop.OutcomeStopped},
			{Outcome: toolloop.OutcomeFailed},
			{FirstRound: 3, LastRound: 3, Outcome: toolloop.OutcomeEnd},
		},
	}

	got := offsetRounds(live, 20)

	if first := got.Invocations[0]; first.FirstRound != 21 || first.LastRound != 22 {
		t.Errorf("the first run names rounds %d..%d, want 21..22", first.FirstRound, first.LastRound)
	}
	if none := got.Invocations[1]; none.FirstRound != 0 || none.LastRound != 0 {
		t.Errorf("a run that made no request names rounds %d..%d, want none", none.FirstRound, none.LastRound)
	}
	if last := got.Invocations[2]; last.FirstRound != 23 || last.LastRound != 23 {
		t.Errorf("the last run names rounds %d..%d, want 23", last.FirstRound, last.LastRound)
	}
	if live.Invocations[0].FirstRound != 1 {
		t.Error("the loop's own run record was renumbered in place")
	}
}

// COPIES. The Result handed to OnProgress is the loop's live snapshot,
// published from another goroutine while the loop is still appending to those
// same slices; renumbering in place would corrupt them under the writer.
func TestTheShiftDoesNotMutateTheLoopsOwnSnapshot(t *testing.T) {
	t.Parallel()
	execs := []toolloop.Execution{{Round: 1, Name: "search"}}
	narr := []toolloop.Narration{{Round: 1, Content: "hello"}}
	partial := &toolloop.Partial{Round: 1, Abandoned: []toolloop.Narration{{Round: 1}}}
	live := toolloop.Result{RoundsUsed: 1, Executions: execs, Narration: narr, Partial: partial}

	_ = offsetRounds(live, 8)

	if execs[0].Round != 1 {
		t.Errorf("the loop's own execution was renumbered to %d", execs[0].Round)
	}
	if narr[0].Round != 1 {
		t.Errorf("the loop's own narration was renumbered to %d", narr[0].Round)
	}
	if partial.Round != 1 || partial.Abandoned[0].Round != 1 {
		t.Errorf("the loop's own partial was renumbered to %d (abandoned %d)",
			partial.Round, partial.Abandoned[0].Round)
	}
}

// An extended phase's record is the PHASE's, not the last invocation's. Three
// publishers used to build it separately and disagreed: the live frame carried
// the invocation alone, so an extension's first frame collapsed a twenty-round
// ledger to one; the completed record took the invocation's token counters, so
// every round before the extension was billed and then dropped from the
// report; and the failure record published the invocation raw, so a phase that
// died on extension round 2 reported two rounds numbered 1 and 2.
func TestAnInvocationIsFoldedOntoTheRoundsBehindIt(t *testing.T) {
	t.Parallel()
	done := phaseResult{
		Rounds: 2,
		Result: toolloop.Result{
			RoundsUsed:   2,
			Executions:   []toolloop.Execution{{Round: 1, Name: "search"}},
			Narration:    []toolloop.Narration{{Round: 1, Content: "first"}},
			InputTokens:  400,
			OutputTokens: 40,
			Model:        "test/model",
		},
	}
	done.Result.Invocations = []toolloop.Invocation{{FirstRound: 1, LastRound: 2}}
	live := toolloop.Result{
		RoundsUsed:   1,
		Executions:   []toolloop.Execution{{Round: 1, Name: "post"}},
		Narration:    []toolloop.Narration{{Round: 1, Content: "second"}},
		Invocations:  []toolloop.Invocation{{FirstRound: 1, LastRound: 1}},
		InputTokens:  100,
		OutputTokens: 10,
	}

	got := foldOnto(done, live)

	if got.RoundsUsed != 3 {
		t.Errorf("RoundsUsed = %d, want the phase total 3", got.RoundsUsed)
	}
	if len(got.Executions) != 2 || got.Executions[0].Name != "search" {
		t.Errorf("the rounds before the extension were dropped: %+v", got.Executions)
	}
	if got.Executions[1].Round != 3 || got.Narration[1].Round != 3 {
		t.Errorf("the folded round is numbered %d/%d, want 3",
			got.Executions[1].Round, got.Narration[1].Round)
	}
	// Every harness run of the phase, the extension's after the original's and
	// on the phase's scale.
	if len(got.Invocations) != 2 || got.Invocations[1].FirstRound != 3 {
		t.Errorf("runs = %+v, want the original's and the extension's, the second at round 3",
			got.Invocations)
	}
	if got.InputTokens != 500 || got.OutputTokens != 50 {
		t.Errorf("tokens = %d in / %d out, want the phase total 500/50",
			got.InputTokens, got.OutputTokens)
	}
	// An invocation that died before its first completion names no model,
	// and a record with no model reads as a phase that never reached one.
	if got.Model != "test/model" {
		t.Errorf("Model = %q, want the model the phase already had", got.Model)
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
