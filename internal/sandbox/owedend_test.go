package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executordomain"
)

// AN OWED END IS FITTED TO WHAT THE EXECUTOR LOG HOLDS BEFORE IT RIDES THE ROW. A
// turn's end is owed on its run's row until the run's ending records it on the
// executor log, and its prose is a model's and a guard's — a reviewer's
// completed work, a failure's text — which nothing bounds before the row: the
// row's budget counts an owed end at the log's largest record, so the end is
// fitted where it is owed, each prose field to the log's bound with the cut
// said, as the log fits it when it records it.
//
// Mutation: owe the end as it was handed over, and a reviewer that pastes its
// diff into its completed work puts a megabyte of it on the row, past what the
// row's arithmetic gave it.
func TestAnOwedEndIsFittedToWhatTheExecutorLogHolds(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	owed, err := rig.coordinator.OweEnd(t.Context(), "t1", executordomain.Ending{
		Outcome: executordomain.OutcomeCompleted, CompletedWork: strings.Repeat("<", 240_000),
		Failure: strings.Repeat("<", 40_000)})
	if err != nil || !owed {
		t.Fatalf("OweEnd = %v, %v; want the end owed", owed, err)
	}
	row := rig.get("t1")
	var ending executordomain.Ending
	if err := json.Unmarshal(row.OwedEnd, &ending); err != nil {
		t.Fatalf("the owed end does not read back: %v", err)
	}
	if len(ending.CompletedWork) > executordomain.CompletedWorkMaxBytes ||
		len(ending.Failure) > executordomain.FailureMaxBytes || !strings.HasPrefix(ending.CompletedWork, "<") {
		t.Errorf("the row owes completed work of %d bytes and a failure of %d, want each its opening within "+
			"the log's %d and %d", len(ending.CompletedWork), len(ending.Failure),
			executordomain.CompletedWorkMaxBytes, executordomain.FailureMaxBytes)
	}
	if len(row.OwedEnd) > executordomain.MaxRecordBytes {
		t.Errorf("the owed end weighs %d on the row, past the %d its budget counts", len(row.OwedEnd),
			executordomain.MaxRecordBytes)
	}
}
