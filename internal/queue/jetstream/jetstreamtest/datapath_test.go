package jetstreamtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
