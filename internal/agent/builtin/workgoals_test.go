package builtin_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A GOAL UPDATE PAST THE CAP IS A REFUSAL THE CALLER READS, NOT A RECEIPT.
//
// A goal stores at most tracker.MaxGoalUpdates updates, and an update is the
// stored value rather than a preview of one — one dropped to make room would
// be readable nowhere. So the writer refuses the save that would add one past
// the cap, naming it, with nothing saved; and this tool is where the caller
// hears that. Answered as a receipt, the model would report an assessment
// filed that nobody can ever read.
//
// Mutation: answer write_work_goal's refusal with a receipt and the save reads
// as applied.
func TestAGoalUpdatePastTheCapIsARefusal(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.writeErr = fmt.Errorf("%w: goal g-1 holds %d updates, this save adds 1, "+
		"and a goal stores at most %d (tracker.MaxGoalUpdates) — nothing was "+
		"saved", tracker.ErrGoalUpdatesFull, tracker.MaxGoalUpdates,
		tracker.MaxGoalUpdates)
	reg := rosterRegistry(t, trk)

	got := callWork(t, reg, tracker.WriteWorkGoalTool, map[string]any{
		"id": "g-1", "name": "Ship it", "owners": []any{"alice"},
		"update": map[string]any{"health": "on_track", "text": "on schedule"},
	})
	if !got.Failed {
		t.Fatalf("a save the writer refused answered %q", got.Output)
	}
	for _, want := range []string{"tracker.MaxGoalUpdates", "NOT made"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal reads %q and does not say %q", got.Output, want)
		}
	}
	if strings.Contains(got.Output, `"outcome"`) {
		t.Errorf("the refusal carries an outcome, so it reads as a save: %q",
			got.Output)
	}
}
