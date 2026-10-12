package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A CLIENT WAITS PAST THE NODE'S OWN BOUND ON A GESTURE, by the part of the
// request that bound does not cover: the round trip around the node's answer.
// A client that gave up at [GateAnswerBudget] itself would report a gesture
// the node answered a moment later as unanswered, which is the failure every
// client's own wait was written to end — and the answer it would miss is the
// one that says, per log, what to do about the logs the gesture did not
// finish.
//
// Mutation: make GateClientWait the answer budget alone and this fails.
func TestAClientWaitsPastTheGateAnswerBudget(t *testing.T) {
	t.Parallel()
	if slack := GateClientWait() - GateAnswerBudget; slack < statelog.DefaultResolveBudget {
		t.Errorf("a client waits %s for a gesture the node answers within %s — %s "+
			"past it, less than one resolve budget of round trip around the "+
			"node's answer", GateClientWait(), GateAnswerBudget, slack)
	}
}
