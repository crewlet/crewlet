package routing_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/routing"
)

// THE THREE EMPTY DELIVERIES ARE THREE ANSWERS, and this is the whole of the
// rule.
//
// A record's `notified` flag does NOT mean somebody was woken — its applier
// says so itself, because it deliberately does not hold the roster the routing
// would need. So an announced change with no recipient rows is ambiguous
// between a routing that resolved to nobody, a set retention took, and not
// knowing which. Each sends a reader somewhere different: at who has left, at
// a retention horizon, and at neither.
//
// The first draft of Worklet's routing reader collapsed all three into one
// boolean called `swept`, which is the failure the whole engine's three-valued
// answers exist to prevent, arriving at the one surface built to end it.
//
// OVER VALUES, because a rule exercised only through a database is a rule
// nobody re-reads, and every one of these six cases would need a
// differently-aged company to reach through one.
func TestTheThreeEmptyDeliveriesAreToldApart(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	announced := routing.Evidence{Held: true, Announced: true, At: at}
	withFloor := func(e routing.Evidence, floor time.Time) routing.Evidence {
		e.RetainedFrom = floor
		return e
	}

	for _, c := range []struct {
		name     string
		evidence routing.Evidence
		want     routing.Delivery
	}{
		{"rows survive", routing.Evidence{
			Held: true, Announced: true, Recipients: 1,
		}, routing.DeliveryReached},
		// A commit that announced nothing has nothing missing, and so
		// does a record id naming no change at all.
		{"announced nothing", routing.Evidence{Held: true}, routing.DeliveryQuiet},
		{"no such change", routing.Evidence{Announced: true}, routing.DeliveryQuiet},
		// INSIDE the retained window: this node holds notices older
		// than the change and none of them is for it, so the routing
		// resolved to nobody.
		{"inside the window", withFloor(announced, at.Add(-time.Hour)),
			routing.DeliveryNobody},
		// OLDER than anything retained: if there were rows, the sweep
		// has them.
		{"older than the floor", withFloor(announced, at.Add(time.Hour)),
			routing.DeliverySwept},
		// No floor at all — an empty table dates nothing, and telling
		// an operator a change reached nobody on the strength of one
		// is the guess this value exists to refuse.
		{"no floor", announced, routing.DeliveryUnknown},
	} {
		got := routing.DeliveryOf(c.evidence)
		if got != c.want {
			t.Errorf("%s: delivery is %q, want %q", c.name, got, c.want)
		}
		if !got.Valid() {
			t.Errorf("%s: %q is not in the closed set", c.name, got)
		}
	}
}

// AND AN UNKNOWN VALUE OFF THE WIRE IS A VALUE, which is what the named type
// buys over two booleans: a peer on a newer build can add a sixth state and
// this one decodes it rather than panicking.
func TestADeliveryThisBuildDoesNotKnowIsAValue(t *testing.T) {
	t.Parallel()
	if (routing.Delivery("recalled")).Valid() {
		t.Fatal("an invented delivery reports itself as one this build knows")
	}
	if len(routing.Deliveries) != 5 {
		t.Fatalf("the closed set has %d members — a state added without a "+
			"place in the list is one no caller can enumerate", len(routing.Deliveries))
	}
}
