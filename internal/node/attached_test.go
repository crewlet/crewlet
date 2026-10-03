package node_test

import (
	"context"
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

// WHAT A NODE CONSUMES IS KEPT BY THE SEAT'S ID, and a release under the handle
// the seat answers to NOW forgets it.
//
// The release names a seat by whatever handle it has by then, which a rename
// moves while the seat stays attached. Keyed on the handle it was attached
// under, the release deleted nothing: the node went on listing a seat it no
// longer consumed, and a reader asking whether this node's copy of a seat's
// memory is current was told yes by a node that had let the seat go.
//
// Mutation: key the attached set on the handle, and this fails.
func TestASeatRenamedWhileAttachedIsForgottenOnRelease(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	broker := qmem.NewBroker()
	q := broker.Client()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	if _, err := q.EnsureSubscription(ctx, inbox("ceo"), inboxGroup("ceo")); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	n, err := node.New(node.Config{
		Queue: q, Coord: &coordmem.Backend{}, NodeID: "node-a", Owner: "node-a:1",
		Seats: func() []placement.Seat { return nil },
		Turn: func(context.Context, string, []*events.Event) queue.Result {
			return queue.Ack()
		},
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	lease := coord.Lease{Owner: "node-a:1", Epoch: 1}
	if err := n.OnAcquire(ctx, seat("ceo"), lease); err != nil {
		t.Fatalf("OnAcquire: %v", err)
	}
	if got := n.AttachedSeats(); len(got) != 1 || got[0] != seatID("ceo") {
		t.Fatalf("attached %v after the acquire, want the one seat", got)
	}

	renamed := placement.Seat{ID: seatID("ceo"), Handle: "chief"}
	if err := n.OnRelease(ctx, renamed, lease, seathost.ReasonDrain); err != nil {
		t.Fatalf("OnRelease: %v", err)
	}
	if got := n.AttachedSeats(); len(got) != 0 {
		t.Errorf("a seat released under its new handle is still listed as consumed: %v", got)
	}
	if got := n.Attached(); len(got) != 0 {
		t.Errorf("Attached = %v after the release, want nothing", got)
	}
}
