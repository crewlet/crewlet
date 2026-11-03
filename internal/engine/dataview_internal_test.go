package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/seat"
)

// A DATA NODE THAT JOINS IS ROUTED TO AT ONCE, NOT A HEARTBEAT LATER — and one
// that leaves cleanly is gone as fast.
//
// The view the router asks is the one the engine builds ([newDataView]), at the
// default lease's cadence: it lists once a presence heartbeat, fifteen seconds.
// A view that only listed took that long to name a node that joined, so a fleet
// booted at once — every node's first listing taken before any presence lease
// existed — routed as though each node were alone: a data node committed every
// push it received itself, and a node without `data` had nobody to ask and
// refused every push, fetch and read. Here the node joins after the view's
// first listing, and must be in the router's placement well inside one
// heartbeat.
func TestADataNodeThatJoinsIsRoutedToAtOnce(t *testing.T) {
	t.Parallel()
	backend := coordmem.New()
	view, err := newDataView(&config.Bootstrap{}, backend)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := seat.SeatLeaseTTL / seat.HeartbeatRatio
	e := &Engine{dataView: view}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = view.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, 5*time.Second, "the view's first listing", func() bool {
		return !view.ListedAt().IsZero()
	})
	routed := func() ([]string, error) { return e.estatePlacement().Holders() }
	if nodes, err := routed(); err != nil || len(nodes) != 0 {
		t.Fatalf("before anybody claimed, the router asks %v (%v), want nobody", nodes, err)
	}

	// WELL INSIDE A HEARTBEAT: a third of one, which a listing on the
	// cadence cannot meet and a watch meets in about a second.
	bound := heartbeat / 3
	claimPresences(t, backend, map[string][]string{"data-a": {"data"}})
	waitUntil(t, bound, "the joining data node to be routed to", func() bool {
		nodes, err := routed()
		return err == nil && slices.Equal(nodes, []string{"data-a"})
	})

	lease, err := backend.Get(t.Context(), coord.NodeResource("data-a"))
	if err != nil || lease == nil {
		t.Fatalf("read the joined node's presence: %v %v", lease, err)
	}
	if ok, err := backend.Release(t.Context(), lease.Resource, lease.Owner, lease.Epoch); err != nil || !ok {
		t.Fatalf("release the joined node's presence: %v %v", ok, err)
	}
	waitUntil(t, bound, "the departed data node to be routed to no more", func() bool {
		nodes, err := routed()
		return err == nil && len(nodes) == 0
	})
}
