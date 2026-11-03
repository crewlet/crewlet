package executorkeep

import (
	"strings"
	"testing"
)

// AN ENDING'S PROSE IS CUT TO ITS BOUND, MARKED, AND NEVER INSIDE A RUNE: a
// reviewer's account past its cap is recorded rather than refusing the turn's
// end, and the cut is visible to the turn that reads it.
//
// Mutation: cut with textcut.Bytes, which marks nothing, and the reader takes
// a severed account for the whole of it.
func TestAnEndingsProseIsCutToItsBoundAndMarked(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", CompletedWorkMaxBytes)
	fitted := Ending{Outcome: OutcomeCompleted, CompletedWork: long}.Fit()
	if len(fitted.CompletedWork) > CompletedWorkMaxBytes {
		t.Fatalf("the fitted account is %d bytes past its %d-byte cap",
			len(fitted.CompletedWork), CompletedWorkMaxBytes)
	}
	if !strings.HasSuffix(fitted.CompletedWork, "…") {
		t.Error("a cut account carries no marker")
	}
	if err := (Ending{Outcome: OutcomeCompleted,
		Decision: strings.Repeat("d", WordMaxBytes+1)}).Validate(); err == nil {
		t.Error("an engine word past its bound was accepted rather than refused")
	}
	if err := (Ending{Outcome: "abandoned"}).Validate(); err == nil {
		t.Error("an outcome this build does not know was accepted")
	}
	if err := (Ending{Outcome: OutcomeCompleted, Unkept: &Unkept{Reason: "refused"}}).Validate(); err == nil {
		t.Error("an unkept capture placed nowhere was accepted")
	}
	cut := Ending{Outcome: OutcomeCompleted,
		Unkept: &Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: long}}.Fit()
	if len(cut.Unkept.Reason) > UnkeptReasonMaxBytes || !cut.Within() {
		t.Errorf("the unkept capture's reason is %d bytes past its %d-byte cap",
			len(cut.Unkept.Reason), UnkeptReasonMaxBytes)
	}
}
