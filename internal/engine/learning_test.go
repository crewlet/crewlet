package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// reflecting reports whether anything on e's broker consumes completed turns
// for the reflect dispatcher, which is the whole of the learning write side.
func reflecting(t *testing.T, e *engine.Engine) bool {
	t.Helper()
	subs, err := e.Backends().Queue.ListSubscriptions(t.Context(),
		topics.Event(types.TurnCompleted{}.EventType()))
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	for _, sub := range subs {
		if sub.Group == learning.ReflectGroup {
			return true
		}
	}
	return false
}

// THE FIRST COMPANY ON AN UNCONFIGURED NODE REFLECTS.
//
// The dispatcher is attached once per process, and boot attaches it only for
// a company it already has. Every apply after that only swaps the workers
// behind it, and with no dispatcher to swap into, the swap returned early: a
// company created on a fresh node, from the dashboard or by the first
// PUT /config, wrote no diary row, no episode and no skill for any turn it
// ran until the process restarted, and nothing anywhere said so.
func TestTheFirstCompanyOnAnUnconfiguredNodeReflects(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	if reflecting(t, e) {
		t.Fatal("an unconfigured node reads completed turns before it has an " +
			"org to resolve their seats against")
	}
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, companyDoc)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !reflecting(t, e) {
		t.Fatal("the first company applied to a fresh node reflects on no turn " +
			"until the process restarts")
	}
}
