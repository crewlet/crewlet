package node_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
	seathost "github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A SEAT'S TENURE IS TOLD AT BOTH ENDS, with its lease: once the seat is
// established here, and once its release's teardown is proven, with why, so
// the time no node held it can be read back. Mutation: drop the release's
// call, and a drained seat reads as held.
func TestASeatsTenureIsToldAtBothEnds(t *testing.T) {
	q := qmem.NewBroker().Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })

	var (
		mu   sync.Mutex
		told []string
	)
	note := func(entry string) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, entry)
	}
	n, err := node.New(node.Config{
		Queue: q, Coord: &coordmem.Backend{},
		NodeID: "node-a", Owner: "node-a:1",
		Seats:    func() []placement.Seat { return seats("ceo") },
		LeaseTTL: fleetTTL, HeartbeatInterval: fleetTTL / 4, SweepInterval: fleetTTL / 8,
		Turn: func(context.Context, string, []*events.Event) queue.Result { return queue.Ack() },
		SeatPlaced: func(_ context.Context, s placement.Seat, lease coord.Lease) {
			if lease.Epoch > 0 && !lease.AcquiredAt.IsZero() {
				note("placed:" + s.Handle)
			}
		},
		SeatReleased: func(_ context.Context, s placement.Seat, lease coord.Lease, reason seathost.ReleaseReason) {
			if lease.Epoch > 0 {
				note("released:" + s.Handle + ":" + reason.String())
			}
		},
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if err := n.Start(t.Context()); err != nil {
		t.Fatalf("node.Start: %v", err)
	}
	got := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(told)
	}
	within(t, "the seat to be placed", func() bool { return len(got()) > 0 })
	n.Stop(context.WithoutCancel(t.Context()))
	if want := []string{"placed:ceo", "released:ceo:drain"}; !slices.Equal(got(), want) {
		t.Fatalf("told %v, want %v: its tenure's start, then its end on the drain", got(), want)
	}
}
