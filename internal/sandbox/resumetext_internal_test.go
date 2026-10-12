package sandbox

import (
	"strings"
	"testing"
)

// A RUN THAT DID NOT SUCCEED SAYS WHY, even when it also wrote a report. The
// resumed executor acts on this text alone — what to tell the requester, what
// to retry — and it used to be handed the report under "did NOT fully
// succeed" with the reason dropped whenever a report existed.
func TestTheResumedExecutorIsToldWhyARunDidNotSucceed(t *testing.T) {
	t.Parallel()
	failed := resumeText(Result{
		Text:  "Outcome: partial — the migration is written, the tests were not run",
		Error: "the coding agent exited with status 137",
	})
	if !strings.Contains(failed, "did NOT fully succeed") ||
		!strings.Contains(failed, "Outcome: partial") ||
		!strings.Contains(failed, "Why it did not succeed: the coding agent exited with status 137") {
		t.Errorf("a failed run with a report reads:\n%s", failed)
	}

	// Said once: a reason that IS the report is not repeated as its own why.
	same := resumeText(Result{Text: "ran out of turns", Error: "ran out of turns"})
	if strings.Count(same, "ran out of turns") != 1 {
		t.Errorf("a reason equal to the report was repeated:\n%s", same)
	}

	// A success carries no why, whatever an earlier stage left in Error.
	ok := resumeText(Result{Success: true, Text: "Outcome: succeeded", Error: "a warning"})
	if strings.Contains(ok, "Why it did not succeed") {
		t.Errorf("a successful run was given a reason it failed:\n%s", ok)
	}

	// And a run with no report says its error, as it always did.
	bare := resumeText(Result{Error: "the coding agent produced no output"})
	if !strings.Contains(bare, "Error: the coding agent produced no output") {
		t.Errorf("a run with only an error reads:\n%s", bare)
	}
}
