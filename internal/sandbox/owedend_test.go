package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// AN OWED END IS FITTED TO WHAT THE EXECUTOR LOG HOLDS BEFORE IT RIDES THE ROW. A
// turn's end is owed on its run's row until the run's ending records it on the
// executor log, and its prose is a model's and a guard's — a reviewer's
// completed work, a failure's text — which nothing bounds before the row: the
// row's budget counts an owed end with every field at the log's bound, so the
// end is fitted where it is owed, each prose field to the log's bound with the
// cut said, as the log fits it when it records it — and an end with an engine
// word past its bound, which the log refuses rather than cuts, is refused.
//
// Mutation: owe the end as it was handed over, and a reviewer that pastes its
// diff into its completed work puts a megabyte of it on the row, past what the
// row's arithmetic gave it; skip the bound check after the fit, and a stop's
// actor of a mebibyte is owed.
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

	// AND AN END THE LOG WOULD REFUSE IS NEVER OWED: an engine word past its
	// bound is refused rather than cut, so it is refused here, before the row
	// carries what no record of the log can.
	if _, err := rig.coordinator.OweEnd(t.Context(), "t1", executorkeep.Ending{
		Outcome: executorkeep.OutcomeStopped, StoppedBy: strings.Repeat("<", 1<<20)}); err == nil {
		t.Error("an end whose actor is a mebibyte was owed")
	}
}
