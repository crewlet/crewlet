package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A CLIENT WAITS PAST THE NODE'S OWN BOUND ON A GESTURE, by at least one round
// trip around the node's answer: a client that gave up at [GateAnswerBudget]
// would report as unanswered a gesture the node answered a moment later, and
// miss the per-log account of what the gesture did not finish.
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
