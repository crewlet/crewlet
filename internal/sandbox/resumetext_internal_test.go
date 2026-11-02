package sandbox

import (
	"strings"
	"testing"
)

// A RUN THAT DID NOT SUCCEED SAYS WHY, even when it also wrote a report. The
// reviewer judges the run on this text — whether to send it back, what the
// requester still needs — and it used to be handed the report under "did NOT
// fully succeed" with the reason dropped whenever a report existed.
func TestARunsReportSaysWhyItDidNotSucceed(t *testing.T) {
	t.Parallel()
	failed := runReport(PendingRun{}, Result{
		Text:  "Outcome: partial — the migration is written, the tests were not run",
		Error: "the coding agent exited with status 137",
	})
	if !strings.Contains(failed, "did NOT fully succeed") ||
		!strings.Contains(failed, "Outcome: partial") ||
		!strings.Contains(failed, "Why it did not succeed: the coding agent exited with status 137") {
		t.Errorf("a failed run with a report reads:\n%s", failed)
	}

	// Said once: a reason that IS the report is not repeated as its own why.
	same := runReport(PendingRun{}, Result{Text: "ran out of turns", Error: "ran out of turns"})
	if strings.Count(same, "ran out of turns") != 1 {
		t.Errorf("a reason equal to the report was repeated:\n%s", same)
	}

	// A success carries no why, whatever an earlier stage left in Error.
	ok := runReport(PendingRun{}, Result{Success: true, Text: "Outcome: succeeded", Error: "a warning"})
	if strings.Contains(ok, "Why it did not succeed") {
		t.Errorf("a successful run was given a reason it failed:\n%s", ok)
	}

	// And a run with no report says its error, as it always did.
	bare := runReport(PendingRun{}, Result{Error: "the coding agent produced no output"})
	if !strings.Contains(bare, "Error: the coding agent produced no output") {
		t.Errorf("a run with only an error reads:\n%s", bare)
	}
}

// A RUN'S REPORT IS ITS OWN ACCOUNT, addressed to nobody: the run was the
// executor, so an instruction to "continue the task" would be read by the
// reviewer as the executor's own claim and filed on the work item as what the
// turn did.
func TestARunsReportInstructsNobody(t *testing.T) {
	t.Parallel()
	got := runReport(PendingRun{}, Result{Success: true, Text: "Outcome: succeeded",
		DeliveredRefs: []string{"pr/42"}})
	if !strings.HasPrefix(got, "The executor's run succeeded.") ||
		!strings.Contains(got, "Delivered: pr/42") || !strings.Contains(got, "Outcome: succeeded") {
		t.Errorf("the report reads:\n%s", got)
	}
	for _, instruction := range []string{"Continue the task", "do NOT redo", "report the outcome"} {
		if strings.Contains(got, instruction) {
			t.Errorf("the report instructs its reader (%q):\n%s", instruction, got)
		}
	}
}
