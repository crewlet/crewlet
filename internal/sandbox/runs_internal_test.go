package sandbox

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A WAIT'S ANSWER IS TAKEN ONLY FOR THE RUN IT WAITS ON. An answer by turn
// saying how a run left its seat is recorded only on a run whose row waits on
// that very run, in words this build knows, its report redacted; any other
// answer carries no such account.
//
// Mutation: take the account whatever the row waits on (wait-any-run), and an
// answer could tell a run a wait ended that it never made.
func TestAWaitsAnswerIsTakenOnlyForTheRunItWaitsOn(t *testing.T) {
	t.Parallel()
	waiting := PendingRun{TurnID: "t1", WaitsFor: &RunWait{Turn: "t2", Seat: "swe"}}
	left := &types.RunWaited{Turn: "t2", Seat: "swe", Left: types.RunLeftEnded, Outcome: "completed",
		Pushed: types.RunPushedNone, Report: "deployed with password=hunter2-staging"}
	got, err := waitedOf(waiting, left)
	if err != nil || got == nil || got.Turn != "t2" || strings.Contains(got.Report, "hunter2") {
		t.Errorf("the wait's own answer is taken as %+v, %v, want it with its report redacted", got, err)
	}
	for name, tc := range map[string]struct {
		run    PendingRun
		waited *types.RunWaited
	}{
		"a run waiting on none": {PendingRun{TurnID: "t1"}, left},
		"another run":           {PendingRun{TurnID: "t1", WaitsFor: &RunWait{Turn: "t3"}}, left},
		"an unknown leaving": {waiting, &types.RunWaited{Turn: "t2", Left: "vanished",
			Pushed: types.RunPushedNone}},
	} {
		if _, err := waitedOf(tc.run, tc.waited); err == nil {
			t.Errorf("%s: the account was taken", name)
		}
	}
	if got, err := waitedOf(waiting, nil); got != nil || err != nil {
		t.Errorf("an answer saying nothing of a run is taken as %+v, %v", got, err)
	}
}

// A REPLY NEVER ANSWERS A RUN WAITING ON ANOTHER RUN. Its requester's message
// in its conversation is put to the run when it parked on a question, and is
// the answer; parked on a run, it is put to nobody, and the message waits aside
// behind it (ADR-0176).
//
// Mutation: put a wait to its requester as a question is, and its requester's
// next message resumes it.
func TestAReplyNeverAnswersARunWaitingOnAnotherRun(t *testing.T) {
	t.Parallel()
	asked := PendingRun{TurnID: "t1", AgentHandle: "swe", Requester: "ana"}
	reply := Reply{Writer: "ana"}
	if !reply.putTo(asked) {
		t.Fatal("the premise: a question is put to the run's requester")
	}
	waiting := asked
	waiting.WaitsFor = &RunWait{Turn: "t2", Seat: "swe", Reason: "its branch first"}
	if reply.putTo(waiting) {
		t.Error("the requester's reply was taken as the answer to a wait on another run")
	}
}
