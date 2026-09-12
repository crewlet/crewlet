package jetstreamtest

import (
	"net"
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

// THE MAJORITY SURVIVES, AFTER IT ELECTS.
//
// This is the point of the cut and it is why the harness exists: a fleet of
// three that loses one member has a quorum and goes on working, and a harness
// that took the whole cluster down with the partitioned member would prove the
// opposite.
//
// # Why the publish is retried and the first attempt is expected to fail
//
// A publish is answered by the STREAM LEADER, and the partitioned member may
// have been it. The remaining two cannot answer until they elect a new one —
// measured here at several seconds, which is NATS's own election timeout and
// not something this engine sets. So "the majority survives" is a claim about
// the interval AFTER the election, and a test that published once immediately
// would be asserting that a leaderless raft group answers, which no design
// promises.
//
// What is asserted is that the election happens at all and that it is bounded:
// a cluster that never elects answers nothing for ever, which is the failure
// this case is really about.
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

	// Two of three replicas is a quorum, so this must become durable once
	// the surviving pair has a leader.
	started := time.Now()
	deadline := started.Add(60 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		last = q.Publish(t.Context(), topic, events.New(probe{N: 2}, events.TraceContext{}))
		if last == nil {
			t.Logf("the majority accepted a publish %v after the partition",
				time.Since(started).Round(100*time.Millisecond))
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the surviving majority never accepted a publish: %v — two of three "+
		"replicas is a quorum, so a cluster that cannot commit here has lost more "+
		"than the member that was cut", last)
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

// THE PORT PROBE IS WHAT TURNS A TWO-MINUTE TIMEOUT INTO A RETRY.
//
// A clustered member whose route port has been taken does not fail fast: it
// starts, serves clients, never forms a route, and is reported only when its
// own readiness budget expires — which was measured, in a full run of this
// repository's suite, as a hundred and twenty seconds of a harness waiting for
// peers that could never arrive.
func TestThePortProbeSeesAHeldPort(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	held, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("take a port: %v", err)
	}
	port := held.Addr().(*net.TCPAddr).Port

	if portFree(port) {
		t.Fatalf("the probe reports port %d free while this test holds it — a "+
			"probe that cannot see a held port is a probe that never fires",
			port)
	}
	if err := held.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	if !portFree(port) {
		t.Fatalf("the probe reports port %d taken after it was released — a "+
			"probe that never passes would restart every cluster four times",
			port)
	}
}
