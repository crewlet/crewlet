package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// TestAStaleRewireCannotWriteTheCurrentRegistry holds that a rewire resolved
// from a replaced revision is never handed the live registry.
//
// A seat's third-party identity is resolved from ONE revision's credentials and
// written into the LIVE registry, by refreshParties on an apply and by the
// reconcile loop's identity retry, which holds the company current when it
// began. Nothing orders the two: a retry finishing after an apply would put the
// previous account back, and the next webhook naming the new one reaches
// nobody, silently. internal/e2e's TestARotatedCredentialIsReresolved shows the
// symptom through a whole engine; this pins the rule, so a refactor that hides
// the symptom by accident still fails here.
func TestAStaleRewireCannotWriteTheCurrentRegistry(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	previous := &Company{Org: &org.Organization{Name: "acme"}}
	current := &Company{Org: &org.Organization{Name: "acme"}}

	e.notify.registry = notify.NewRegistry(previous.Org, nil)
	e.notify.registryFor = previous
	if got := e.registryOf(previous); got == nil {
		t.Fatal("the pass that built the live registry cannot write it")
	}

	// AN APPLY PUBLISHES A NEW PAIR: the retry's work is stale from here.
	fresh := notify.NewRegistry(current.Org, nil)
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
	// A NIL COMPANY IS NOT A MATCH: a surface arrives with one before any
	// revision has been applied.
	if got := e.registryOf(nil); got != nil {
		t.Fatal("a nil company matched the live registry")
	}
}
