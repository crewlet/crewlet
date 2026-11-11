package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/kv"
)

// THE STORES ON A BROKER CONNECTION ARE TOLD WHETHER IT CROSSES A LINK, BY ONE
// READING.
//
// A member reaches its broker's JetStream in its own process; a leaf reaches
// the members' across its link, and a client of an external cluster across
// the network. The coordination store cannot see which from the connection it
// is handed, and a store told "in process" over a link holds a large write —
// an MCP listing every node with seats writes — to the request term alone, so
// it fails as an outage while its bytes cross. So the engine tells it, from
// the same reading it hands the object store's window.
//
// Mutation: open the coordination store as though every connection were a
// member's, or as though every one crossed a link, and a row goes red.
func TestTheStoresOnABrokerConnectionAreToldWhetherItCrossesALink(t *testing.T) {
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
		if got := coordinationReach(c.b); got != c.want {
			t.Errorf("%s's coordination store is opened as reaching its buckets %q, want %q",
				c.name, got, c.want)
		}
		if got := acrossLink(c.b); got != (c.want == kv.AcrossLink) {
			t.Errorf("%s's object store is told its connection crosses a link: %t, want %t",
				c.name, got, c.want == kv.AcrossLink)
		}
	}
}
