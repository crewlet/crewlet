package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// TestAStaleRewireCannotWriteTheCurrentRegistry is the race that made a
// rotated credential route to nobody.
//
// A seat's third-party identity is resolved from ONE revision's credentials
// and then written into the LIVE registry — by refreshParties on the apply,
// and by the reconcile loop's identity retry on its own tick, which holds
// whatever company was current when it began. Nothing orders the two. A retry
// that started before an apply can finish after it and put the PREVIOUS
// revision's account back over the one the apply just installed, and the next
// webhook naming the new account then reaches nobody, with no error anywhere.
//
// It was found through internal/e2e's TestARotatedCredentialIsReresolved,
// which went red the moment this branch's native runtime made an apply take
// seconds instead of microseconds — the window was always there, and was
// simply too narrow to lose before. That case proves the SYMPTOM through a
// whole engine; this one pins the rule directly, so a refactor that keeps the
// symptom away by accident still fails here.
func TestAStaleRewireCannotWriteTheCurrentRegistry(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	previous := &Company{Org: &org.Organization{Name: "acme"}}
	current := &Company{Org: &org.Organization{Name: "acme"}}

	e.notify.registry = notify.NewRegistry(previous.Org)
	e.notify.registryFor = previous
	if got := e.registryOf(previous); got == nil {
		t.Fatal("the pass that built the live registry cannot write it")
	}

	// AN APPLY PUBLISHES A NEW PAIR, which is the instant the retry's
	// work goes stale.
	fresh := notify.NewRegistry(current.Org)
	e.notify.registry, e.notify.registryFor = fresh, current

	if got := e.registryOf(previous); got != nil {
		t.Fatal("a retry holding the replaced revision was handed the live " +
			"registry — it would write that revision's account over the one " +
			"the apply just installed, and the rotated credential would " +
			"route to nobody")
	}
	// AND THE CURRENT ONE STILL WRITES, or the guard would have turned a
	// race into a retry that never lands.
	if got := e.registryOf(current); got != fresh {
		t.Fatal("the pass holding the current revision was refused")
	}
	// A NIL COMPANY IS NOT A MATCH, which is the shape a surface reaches
	// here with before any revision has been applied.
	if got := e.registryOf(nil); got != nil {
		t.Fatal("a nil company matched the live registry")
	}
}
