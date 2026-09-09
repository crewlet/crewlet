package jetstreamtest

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// probe is a registered payload, so a publish here takes the same typed path
// a seat's mail does.
type probe struct {
	N int `json:"n"`
}

func (probe) EventType() string { return "test.probe" }

func init() { events.Register[probe]() }

// A partition harness that does not partition is worse than none: every
// fleet-failure case written on top of it passes having staged nothing. So
// this suite asserts the cut itself, in the two halves that can be wrong
// independently — the routes go away, and they STAY away while NATS re-dials.
func TestPartitionHarnessActuallyPartitions(t *testing.T) {
	t.Parallel()
	c := StartPartitionableCluster(t, 3, js.Config{})
	awaitRoutes(t, c, 0, 2)
	awaitRoutes(t, c, 1, 2)
	awaitRoutes(t, c, 2, 2)

	c.Partition(t, 2)

	// The mutation this case exists for: stop the listeners and leave the
	// live connections open. NATS holds an established route indefinitely,
	// so member 2 keeps both of its routes and this wait times out.
	awaitRoutes(t, c, 2, 0)
	awaitRoutes(t, c, 0, 1)
	awaitRoutes(t, c, 1, 1)

	// And it must stay cut. A route is re-dialled about once a second, so
	// this window covers several attempts through relays that must all
	// refuse — including the gossiped address, which is the path that
	// bypasses the relays if it is ever reachable.
	holdRoutes(t, c, 2, 0, 5*time.Second)

	c.Heal(t, 2)
	awaitRoutes(t, c, 2, 2)
	awaitRoutes(t, c, 0, 2)
	awaitRoutes(t, c, 1, 2)
}

// The majority is the point of the cut: a fleet of three that loses one
// member has quorum and goes on working, and a harness that took the whole
// cluster down with the partitioned member would prove the opposite.
func TestAMajoritySurvivesAPartition(t *testing.T) {
	t.Parallel()
	c := StartPartitionableCluster(t, 3, js.Config{})
	for i := range c.Servers {
		awaitRoutes(t, c, i, 2)
	}
	q := c.Client(t, 0)
	topic := topics.AgentInbox("alice")
	if err := q.Publish(t.Context(), topic, events.New(probe{N: 1}, events.TraceContext{})); err != nil {
		t.Fatalf("publish before the partition: %v", err)
	}

	c.Partition(t, 2)
	awaitRoutes(t, c, 2, 0)

	// Two of three replicas is a quorum, so this must still be durable.
	if err := q.Publish(t.Context(), topic, events.New(probe{N: 2}, events.TraceContext{})); err != nil {
		t.Fatalf("publish with one of three members partitioned: %v", err)
	}
}

// awaitRoutes waits for member i to hold routes to exactly want peers.
func awaitRoutes(t *testing.T, c *Cluster, i, want int) {
	t.Helper()
	// Generous because a route is re-dialled on a ~1s schedule and a
	// cluster forming under the full race suite competes for the CPU. It
	// bounds a wait that succeeds in well under a second when it succeeds
	// at all.
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := c.Servers[i].RoutePeers()
		if len(got) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("member %d is routed to %v, want %d peers", i, got, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// holdRoutes asserts member i's peer count stays at want for d.
func holdRoutes(t *testing.T, c *Cluster, i, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := c.Servers[i].RoutePeers(); len(got) != want {
			t.Fatalf("member %d rose to %v, want %d peers held", i, got, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
