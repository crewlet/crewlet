package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A RUN PARKED ON ITS OWN CALL IS TOLD THE CALL RETURNED, NEVER AN ANSWER. A
// run whose call was answered still running before its harness's ceiling
// ended its turn to wait for it; the engine answers that park once the call
// has returned, and the run is carried on, in its own session or on a fresh
// machine, told which call it waited for, that it ran once, and what it
// returned, here where the round's log keeps it no longer: never as a
// person's answer to a question.
//
// Mutation: tell the park as an answered question (drop the CallsReturned
// branch), and the run reads a person's answer nobody gave.
func TestARunParkedOnItsOwnCallIsToldTheCallReturnedNeverAnAnswer(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	asked := runner.Asked{Question: "Waiting for its call to return: delegate", BoxKept: true,
		Answer: "Your call to delegate returned.", Called: []types.CallReturned{{Tool: "delegate", Seq: 4}},
		CallsReturned: true}
	for name, said := range map[string]string{"continued": asked.Continuation(b), "reseeded": asked.Reseed(b)} {
		for _, want := range []string{"called `delegate`", "still running", "do not make it again",
			"no longer on the run's record"} {
			if !strings.Contains(said, want) {
				t.Errorf("%s: the run is not told %q:\n%s", name, want, said)
			}
		}
		if strings.Contains(said, "answered") {
			t.Errorf("%s: the call is told as an answered question:\n%s", name, said)
		}
	}
}

// EVERY CALL PARKED UNDER THE PROMISE REACHES THE SESSION, WHATEVER ANSWERED
// IT. A run answered "you will be resumed with its result" for a call, then for
// a second, or for one after it asked a person, is carried on by whichever
// answer comes: the engine's that its calls returned, or the person's. Either
// way each call is told, with what it returned, once.
//
// Mutation: tell only the first parked call (Called[:1]), or none beside a
// person's answer (calledToo answering ""), and a call's result promised to
// the run never reaches it.
func TestEveryCallParkedUnderThePromiseReachesTheSession(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	two := []types.CallReturned{{Tool: "delegate", Seq: 3}, {Tool: "post_report", Seq: 5}}
	for name, asked := range map[string]runner.Asked{
		"its calls returned": {Question: "Waiting for its call to return: delegate", BoxKept: true,
			Answer: "Your calls to `delegate` and `post_report` returned.", Called: two, CallsReturned: true},
		"a person answered": {Question: "Which branch?", Answer: "Use main.", BoxKept: true, Called: two},
	} {
		for kind, said := range map[string]string{"continued": asked.Continuation(b), "reseeded": asked.Reseed(b)} {
			for _, tool := range []string{"called `delegate`", "called `post_report`"} {
				if strings.Count(said, tool) != 1 {
					t.Errorf("%s, %s: the run is told %q %d times, want once:\n%s", name, kind, tool,
						strings.Count(said, tool), said)
				}
			}
			if asked.CallsReturned == strings.Contains(said, "Use main.") {
				t.Errorf("%s, %s: the person's answer is told %v:\n%s", name, kind, !asked.CallsReturned, said)
			}
		}
	}
}
