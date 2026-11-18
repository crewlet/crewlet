package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// AN OWED END IS FITTED TO WHAT THE EXECUTOR LOG HOLDS BEFORE IT RIDES THE ROW.
// Its prose (completed work, a failure's text) is unbounded model output, and
// the row's budget counts an owed end at the log's bounds, so each prose field
// is cut to the log's bound as the log would cut it, with the cut said; an
// engine word past its bound, which the log refuses rather than cuts, is
// refused.
//
// Mutation: owe the end as handed over, and a reviewer that pastes its diff
// into its completed work puts a megabyte on the row; skip the bound check
// after the fit, and a stop's actor of a mebibyte is owed.
func TestAnOwedEndIsFittedToWhatTheExecutorLogHolds(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	owed, err := rig.coordinator.OweEnd(t.Context(), "t1", executorkeep.Ending{
		Outcome: executorkeep.OutcomeCompleted, CompletedWork: strings.Repeat("<", 240_000),
		Failure: strings.Repeat("<", 40_000)})
	if err != nil || !owed {
		t.Fatalf("OweEnd = %v, %v; want the end owed", owed, err)
	}
	row := rig.get("t1")
	var ending executorkeep.Ending
	if err := json.Unmarshal(row.OwedEnd, &ending); err != nil {
		t.Fatalf("the owed end does not read back: %v", err)
	}
	if len(ending.CompletedWork) > executorkeep.CompletedWorkMaxBytes ||
		len(ending.Failure) > executorkeep.FailureMaxBytes || !strings.HasPrefix(ending.CompletedWork, "<") {
		t.Errorf("the row owes completed work of %d bytes and a failure of %d, want each its opening within "+
			"the log's %d and %d", len(ending.CompletedWork), len(ending.Failure),
			executorkeep.CompletedWorkMaxBytes, executorkeep.FailureMaxBytes)
	}
	if !ending.Within() {
		t.Errorf("the row owes an end outside the executor log's bounds, which its budget counts it at: %+v",
			ending)
	}

	// AND AN END THE LOG WOULD REFUSE IS NEVER OWED: it is refused here, before
	// the row carries what no record of the log can.
	if _, err := rig.coordinator.OweEnd(t.Context(), "t1", executorkeep.Ending{
		Outcome: executorkeep.OutcomeStopped, StoppedBy: strings.Repeat("<", 1<<20)}); err == nil {
		t.Error("an end whose actor is a mebibyte was owed")
	}
}
