package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/kv"
)

// EVERY COORDINATION STORE ON A BROKER CONNECTION IS OPENED KNOWING WHETHER IT
// CROSSES A LINK, BY ONE READING: a member's JetStream is in process, a leaf's
// and an external cluster's client's across a link. The store cannot tell from
// its connection, and one told "in process" over a link holds a large write to
// the request term alone and fails as an outage. So the fleet, lease and epochs
// stores all take it from coordinationStoresOf, the reading the object store's
// window gets too.
//
// Mutation: open any of the stores as though every connection were a
// member's, or as though every one crossed a link, and a row goes red.
func TestEveryCoordinationStoreIsOpenedKnowingWhetherItsConnectionCrossesALink(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		b    *config.Bootstrap
		want kv.Reach
	}{
		{"a member", &config.Bootstrap{}, kv.InProcess},
		{"a leaf", &config.Bootstrap{Stream: config.Stream{
			Leaf: config.StreamLeaf{URLs: []string{"nats-leaf://member-a.internal:7422"}}}}, kv.AcrossLink},
		{"a client of an external cluster", &config.Bootstrap{Stream: config.Stream{
			Type: config.StreamNATS}}, kv.AcrossLink},
	} {
		stores := coordinationStoresOf(c.b)
		for store, got := range map[string]kv.Reach{
			"fleet store":    stores.fleet.Reach,
			"epochs counter": stores.epochs.Reach,
			"lease store":    stores.leases.Reach,
			"one reading":    coordinationReach(c.b),
		} {
			if got != c.want {
				t.Errorf("%s's %s is opened as reaching its buckets %q, want %q",
					c.name, store, got, c.want)
			}
		}
		if got := acrossLink(c.b); got != (c.want == kv.AcrossLink) {
			t.Errorf("%s's object store is told its connection crosses a link: %t, want %t",
				c.name, got, c.want == kv.AcrossLink)
		}
	}
}
