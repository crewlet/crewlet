package sandbox_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// EVERY CALL PARKED UNDER THE PROMISE IS ON ITS JOB'S ROW. A call answered
// still running before its harness's ceiling is told "you will be resumed with
// its result", so its park is never refused as a question already asked: the
// first park of a job that asked nothing is the job's ask, put to nobody and
// waiting on its own calls; every later one, and one after a person's question
// or a wait on a run, is added to the calls the job parked on, its ask left as
// it was, so whatever answers the job, its resume tells each call.
//
// Mutation: refuse a park on a job that asked already (AskAlreadyAsked), and a
// second call, or one after a question, is promised a resume that never tells
// it; replace the list in place of adding to it, and the first call is lost.
func TestEveryCallParkedUnderThePromiseIsOnItsJobsRow(t *testing.T) {
	t.Parallel()
	first, second := sandbox.CallWait{Tool: "delegate", Seq: 3}, sandbox.CallWait{Tool: "post_report", Seq: 5}
	question := sandboxtest.Question("Which branch?", sandbox.AskToRequester)
	wait := &sandbox.RunWait{Turn: "0190f3a8-0000-7000-8000-0000000000a1", Seat: "swe",
		Reason: "it pushes the branch first"}
	for name, c := range map[string]struct {
		before   *sandbox.Ask
		calls    []sandbox.CallWait
		question string
		ownCalls bool
	}{
		"two calls":        {calls: []sandbox.CallWait{first, second}, question: sandbox.CallWaitQuestion("delegate"), ownCalls: true},
		"after a question": {before: &question, calls: []sandbox.CallWait{first}, question: question.Question},
		"after a wait on a run": {before: &sandbox.Ask{Question: sandbox.RunWaitQuestion(wait.Reason), Wait: wait},
			calls: []sandbox.CallWait{first, second}, question: sandbox.RunWaitQuestion(wait.Reason)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store := sandbox.NewCoordStore(memory.NewFleet())
			job := launchedRun(t, store, "t1")
			if c.before != nil {
				if outcome, err := store.RecordAsk(ctx, "t1", job, *c.before); err != nil ||
					outcome != sandbox.AskRecorded {
					t.Fatalf("asking first: %v, %v", outcome, err)
				}
			}
			for _, call := range c.calls {
				if outcome, err := store.RecordAsk(ctx, "t1", job, sandbox.Ask{Call: &call}); err != nil ||
					outcome != sandbox.AskRecorded {
					t.Fatalf("parking on call %d: %v, %v; want recorded", call.Seq, outcome, err)
				}
			}
			row, found, err := store.Get(ctx, "t1")
			if err != nil || !found {
				t.Fatalf("Get: %v, found %v", err, found)
			}
			if !slices.EqualFunc(row.ParkedCalls, c.calls, func(a, b sandbox.CallWait) bool {
				return a.Tool == b.Tool && a.Seq == b.Seq
			}) {
				t.Errorf("the row holds parked calls %+v, want %+v", row.ParkedCalls, c.calls)
			}
			if row.Question != c.question || row.WaitsOnOwnCalls() != c.ownCalls || !row.AskedBy(job) {
				t.Errorf("the row asks %q, waiting on its own calls %v; want %q, %v", row.Question,
					row.WaitsOnOwnCalls(), c.question, c.ownCalls)
			}
			if c.before != nil && c.before.Wait != nil && (row.WaitsFor == nil || row.WaitsFor.Turn != wait.Turn) {
				t.Errorf("the wait on a run became %+v", row.WaitsFor)
			}
		})
	}
}

// A JOB THAT IS NOT RUNNING PARKS ON NO CALL. A park reaching a row that no
// longer holds its job running is refused as not running, so the bridge
// promises it no resume.
//
// Mutation: drop the running check for a park, and a call of an ended job is
// promised a resume nothing carries.
func TestAJobThatIsNotRunningParksOnNoCall(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(memory.NewFleet())
	launchedRun(t, store, "t1")
	call := sandbox.CallWait{Tool: "delegate", Seq: 1}
	if outcome, err := store.RecordAsk(ctx, "t1", "another-job", sandbox.Ask{Call: &call}); err != nil ||
		outcome != sandbox.AskNotRunning {
		t.Errorf("a park for a job the row does not hold answered %v, %v; want not running", outcome, err)
	}
}
