package jetstreamtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// THE DATA PATH OF A LEAF ON A CUT-OFF MEMBER IS REFUSED, though its first hop
// stands: a leaf naming member 0 alone keeps its links to it while member 0's
// routes are cut, and once the probe has heard "not available" with no success
// for the fast path's thirty seconds, DataPath refuses, saying so. Its link
// reads up throughout, so only an answer from beyond the first hop can tell.
// Production's timing, no seam: the case waits about forty seconds.
//
// Mutant: DataPath reads only the first hop — it never refuses.
// Mutant: the fast path disabled — the member's explicit answers never refuse.
func TestTheDataPathOfALeafOnACutOffMemberIsRefused(t *testing.T) {
	t.Parallel()
	c := withFreshPorts(t.Context(), t, "partitionable cluster with leaf listeners",
		func(ctx context.Context, name string) (*Cluster, error) {
			return startPartitionable(ctx, t, 3, js.Config{}, name, true)
		})
	const cutOff = 0
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf",
		LeafURLs: c.LeafURLs()[cutOff : cutOff+1], Replicas: 3, FleetAuth: fleetAuth()})
	if err != nil {
		t.Fatalf("start a leaf of member %d: %v", cutOff, err)
	}
	t.Cleanup(leaf.Shutdown)
	q, err := leaf.Client(t.Context())
	if err != nil {
		t.Fatalf("a client of the leaf: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	path := q.ProveDataPath()
	if err := path(); err != nil {
		t.Fatalf("the data path of a leaf on a whole fleet reads %v", err)
	}

	c.Partition(t, cutOff)
	// LIVENESS: the fast path refuses thirty seconds after the last success
	// and a probe period on; twice that only bounds the wait.
	bound := time.NewTimer(90 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	poll := time.NewTicker(100 * time.Millisecond) //fixedwait:liveness
	defer poll.Stop()
	for {
		err := path()
		if err != nil && strings.Contains(err.Error(), "not available") {
			if !errors.Is(err, js.ErrDataPathUnproven) {
				t.Errorf("the refusal %v does not wrap ErrDataPathUnproven", err)
			}
			break
		}
		select {
		case <-bound.C:
			t.Fatalf("the data path of a leaf on a member cut off from its peers still reads %v", err)
		case <-poll.C:
		}
	}
	if err := q.Link(); err != nil {
		t.Errorf("the leaf's first hop reads %v, so the case never needed the probe", err)
	}
}

// A LEASE LINK ON A CUT-OFF MEMBER IS MOVED, the data link left where it is: a
// leaf naming members 0 and 1 in that order (js.Config.LeafURLsInOrder, so both
// links land on member 0 rather than where a shuffle puts them) has member 0's
// routes cut. Its lease connection hears member 0 answer "not available", and
// once the lease link's probe has for its budget the link is dropped and
// dials member 1, across which the fleet's JetStream answers; the data
// connection still hears member 0. Production's timing: about forty seconds.
//
// Mutant: relink never called — the lease connection hears member 0 to the
// bound. Mutant: the leaf's URLs shuffled — in about half the runs the lease
// link lands on member 1 and never hears "not available".
func TestALeaseLinkOnACutOffMemberIsMoved(t *testing.T) {
	t.Parallel()
	c := withFreshPorts(t.Context(), t, "partitionable cluster with leaf listeners",
		func(ctx context.Context, name string) (*Cluster, error) {
			return startPartitionable(ctx, t, 3, js.Config{}, name, true)
		})
	const cutOff = 0
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf",
		LeafURLs: c.LeafURLs()[cutOff : cutOff+2], LeafURLsInOrder: true, Replicas: 3,
		FleetAuth: fleetAuth()})
	if err != nil {
		t.Fatalf("start a leaf of members %d and %d: %v", cutOff, cutOff+1, err)
	}
	t.Cleanup(leaf.Shutdown)
	q, err := leaf.Client(t.Context())
	if err != nil {
		t.Fatalf("a client of the leaf: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	leases, err := q.DialLeases(t.Context())
	if err != nil {
		t.Fatalf("dial the lease connection: %v", err)
	}
	t.Cleanup(leases.Close)
	onLeases, err := jsapi.Embedded().Client(leases)
	if err != nil {
		t.Fatalf("a JetStream client of the lease connection: %v", err)
	}
	onData, err := jsapi.Embedded().Client(q.Conn())
	if err != nil {
		t.Fatalf("a JetStream client of the data connection: %v", err)
	}
	// ask is what the fleet's JetStream answers on a connection, within a
	// read's term.
	ask := func(on jetstream.JetStream) error {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err := on.AccountInfo(ctx)
		return err
	}
	answering := func(err error) bool {
		var api *jetstream.APIError
		return errors.As(err, &api)
	}
	if err := ask(onLeases); err != nil {
		t.Fatalf("the lease connection on a whole fleet hears %v", err)
	}

	c.Partition(t, cutOff)
	// LIVENESS: the probe moves the link thirty seconds after its last
	// success and a probe period on; three times that only bounds the wait.
	eventually(t, 2*time.Minute, "the lease connection to hear member 0 cut off", func() bool {
		return answering(ask(onLeases))
	})
	eventually(t, 2*time.Minute, "the lease link to move to a member the fleet answers on", func() bool {
		return ask(onLeases) == nil
	})
	if err := ask(onData); !answering(err) {
		t.Errorf("the data connection hears %v, want member 0's own \"not available\": its data "+
			"link moved too, or never landed there", err)
	}
	eventually(t, time.Minute, "the moved lease link to read proven", func() bool {
		return q.LeaseLink() == nil
	})
}
