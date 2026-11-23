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
// Mutation: tell the park as an answered question (drop Called's branch), and
// the run reads a person's answer nobody gave.
func TestARunParkedOnItsOwnCallIsToldTheCallReturnedNeverAnAnswer(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	asked := runner.Asked{Question: "Waiting for its call to return: delegate", BoxKept: true,
		Answer: "Your call to delegate returned.", Called: &types.CallReturned{Tool: "delegate", Seq: 4}}
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
