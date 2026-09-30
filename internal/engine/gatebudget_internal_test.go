package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE GATE BUDGET IS COUNTED FROM THE REGISTER, AT THE MARGIN IT WAS SIZED AT.
//
// A gesture writes one gate record per identity-claiming log, each resolved
// through the publisher's own two waits. The budget was a literal minute, sized
// when two logs claimed identity — a margin of three over twenty seconds — and
// the register grew to four while the literal stayed, so the margin fell to one
// and a half with nobody deciding it. Counted here from the register itself,
// independently of the function under test, a domain that starts claiming
// identity moves the expectation and the budget together or fails.
//
// Mutation: put the literal minute back and the budget is half what four logs
// need at the margin; drop a log from the count and it is three quarters.
func TestTheGateBudgetIsCountedFromTheRegister(t *testing.T) {
	t.Parallel()
	logs := 0
	for _, domain := range registeredDomains() {
		if domain.ClaimsIdentity() {
			logs++
		}
	}
	if logs == 0 {
		t.Fatal("the premise: no registered domain claims identity, so no " +
			"gesture writes anything and this case certifies nothing")
	}
	waits := time.Duration(logs*2) * statelog.DefaultResolveBudget
	if want := waits * 3; GateBudget() != want {
		t.Errorf("the gate budget is %s for %d identity logs of two %s waits "+
			"each (%s), want %s — the margin of three it was sized at",
			GateBudget(), logs, statelog.DefaultResolveBudget, waits, want)
	}
}

// AND A CLIENT WAITS PAST IT, by the part of the request the budget does not
// cover: the judgement before the first record and the round trip around the
// gesture. A client that gave up at the budget itself would report a gesture
// the node answered a moment later as unanswered, which is the failure every
// client's own wait was written to end.
//
// Mutation: make GateClientWait the budget alone and this fails.
func TestAClientWaitsPastTheGateBudget(t *testing.T) {
	t.Parallel()
	if slack := GateClientWait() - GateBudget(); slack < statelog.DefaultResolveBudget {
		t.Errorf("a client waits %s for a gesture the node bounds at %s — %s past "+
			"it, less than the one resolve budget the judgement before the first "+
			"record may take", GateClientWait(), GateBudget(), slack)
	}
}
