package sandbox

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A job whose provider refused the request its session makes is recorded as
// that, with the provider's message, and its session is retired and not kept:
// every later launch of it would be refused the same way.
//
// Mutation: drop requestRefused from sessionSpent, at.refused or the record's
// kind, and it goes red.
func TestAJobItsProviderRefusedIsRecordedAndRetiresItsSession(t *testing.T) {
	t.Parallel()
	const said = "API Error: 400 tools.88.custom.name: String should match pattern '^[a-zA-Z0-9_-]{1,128}$'"
	inBubble(t, func(t *testing.T) {
		rig, writes, keep := keepingRig(t)
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		rig.runner.Finish(Result{Error: said, RequestRefused: true, SessionID: "s-1", Captured: &session.Archive})
		collectHeld(t, rig, "t1")

		run := rig.get("t1")
		if want := []string{run.SessionRan()}; want[0] == "" || !slices.Equal(run.ExhaustedSessions, want) {
			t.Fatalf("the row retired %q, want the session the job ran, %q", run.ExhaustedSessions, want)
		}
		if run.Capture == nil || run.Capture.State != CaptureRefused || run.Kept != nil ||
			len(keep.uploads()) != 0 || len(writes.captured()) != 0 {
			t.Fatalf("the row holds capture %+v, kept %+v, with %d uploads and %d records: want nothing "+
				"kept of a session its provider refuses", run.Capture, run.Kept, len(keep.uploads()),
				len(writes.captured()))
		}
		recs := rig.collected()
		if len(recs) != 1 || !recs[0].Failed || recs[0].ErrorKind != types.ExecutorErrorRequestRefused ||
			recs[0].Error != said || !strings.Contains(recs[0].SessionNotKept, "refused the request") {
			t.Fatalf("the launch's record is %+v, want request_refused with the provider's message and "+
				"why its session was not kept", recs)
		}
	})
}

// Only a job that FAILED on the refusal is one: a success, a job stopped, lost
// or parked on a question is not, and one out of context is named for that.
func TestOnlyAFailedJobIsRefusedByItsProvider(t *testing.T) {
	t.Parallel()
	refused := Result{RequestRefused: true}
	if !requestRefused(PendingRun{}, refused) {
		t.Fatal("a failed job whose harness names the refusal is not judged refused")
	}
	for name, tc := range map[string]struct {
		run    PendingRun
		result Result
	}{
		"a job that succeeded":       {PendingRun{}, Result{Success: true, RequestRefused: true}},
		"a job out of context":       {PendingRun{}, Result{RequestRefused: true, ContextExhausted: true}},
		"a job a person stopped":     {PendingRun{Stop: &RunStop{By: "ana"}}, refused},
		"a job lost with its host":   {PendingRun{Lost: &JobLoss{}}, refused},
		"a job parked on a question": {PendingRun{Question: "which branch?"}, refused},
	} {
		if requestRefused(tc.run, tc.result) {
			t.Errorf("%s is judged refused by its provider", name)
		}
	}
}
