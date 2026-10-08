package node_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A SEAT THAT STARTS ADMITTING TURNS SAYS SO, at both edges into admission: the
// acquisition established, with its mailbox attached and MayStart saying yes —
// which no renew reports — and ownership proven again after a stretch it could
// not be. Whatever the node refused for want of a seat that admits turns waits
// on exactly these; the engine's is a recorded answer its seat's preparation
// inherited and could not resume while the seat was still establishing.
func TestASeatThatStartsAdmittingTurnsSaysSo(t *testing.T) {
	q := qmem.NewBroker().Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })

	var (
		mu       sync.Mutex
		admitted []string
		n        *node.Node
	)
	n, err := node.New(node.Config{
		Queue: q, Coord: &coordmem.Backend{},
		NodeID: "node-a", Owner: "node-a:1",
		Seats:    func() []placement.Seat { return []placement.Seat{{Handle: "ceo"}} },
		LeaseTTL: fleetTTL, HeartbeatInterval: fleetTTL / 4, SweepInterval: fleetTTL / 8,
		Turn: func(context.Context, string, []*events.Event) queue.Result { return queue.Ack() },
		SeatAdmitted: func(_ context.Context, handle string) {
			_, admits := n.Host().MayStart(handle)
			mu.Lock()
			defer mu.Unlock()
			entry := handle
			if admits {
				entry += ":admitting"
			}
			admitted = append(admitted, entry)
		},
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if err := n.Start(t.Context()); err != nil {
		t.Fatalf("node.Start: %v", err)
	}
	t.Cleanup(func() { n.Stop(context.WithoutCancel(t.Context())) })
	got := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(admitted)
	}

	within(t, "the seat to say it is established", func() bool { return len(got()) > 0 })
	if want := []string{"ceo:admitting"}; !slices.Equal(got(), want) {
		t.Fatalf("admitted = %v, want %v: once, by a seat that already admits turns", got(), want)
	}

	// OWNERSHIP PROVEN AGAIN after a blip is the same edge.
	if err := n.OnAdmission(t.Context(), "ceo", false); err != nil {
		t.Fatalf("OnAdmission(false): %v", err)
	}
	if err := n.OnAdmission(t.Context(), "ceo", true); err != nil {
		t.Fatalf("OnAdmission(true): %v", err)
	}
	if want := []string{"ceo:admitting", "ceo:admitting"}; !slices.Equal(got(), want) {
		t.Fatalf("admitted = %v, want %v: losing admission says nothing, regaining it says "+
			"the seat admits again", got(), want)
	}
}
