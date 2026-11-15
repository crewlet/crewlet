package node_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A DRAIN WAITS FOR THE SEATS IT CUT TO BE GIVEN BACK, within its own bound.
//
// The drain cuts a seat still being taken rather than waiting on the seat's
// lock for a hook with no total, and the cut seat leaves the held set at once,
// so the drain's release of the seats it holds never reaches it: the claim that
// was taking it gives it back once its hook returns. A drain that returned
// there would report done while that give-back — the seat's teardown and its
// lease — still ran, beside everything the stop tears down after the drain.
//
// The hook here honours its cut and is then held unwinding until the drain has
// returned, so the give-back cannot finish inside the drain: the drain must
// spend its whole bound waiting for it, and return only once that bound has
// ended. The bound is the caller's to set, as it is for the wait for turns, and
// its length decides nothing here: a drain that does not wait returns long
// before it, however long it is.
//
// Mutation: drop the drain's wait for the seats it cut ([seat.Host.AwaitCut]),
// and the drain returns with its bound still running.
func TestADrainWaitsForTheSeatsItCutWithinItsBound(t *testing.T) {
	t.Parallel()
	const ttl = 3 * time.Second
	broker := qmem.NewBroker()
	client := func() queue.EventQueue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("queue.Start: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		return q
	}
	nodeQueue, publisher := client(), client()
	if _, err := publisher.EnsureSubscription(t.Context(), inbox("ceo"), inboxGroup("ceo")); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}

	var (
		company  atomic.Bool // the seat appears only once the node has started
		entered  = make(chan struct{})
		unwind   = make(chan struct{})
		cutOnce  sync.Once
		wasCut   atomic.Bool
		doneOnce sync.Once
		gaveBack = make(chan struct{})
	)
	n, err := node.New(node.Config{
		Queue: nodeQueue, Coord: &coordmem.Backend{},
		NodeID: "node-a", Owner: "node-a:1",
		Seats: func() []placement.Seat {
			if !company.Load() {
				return nil
			}
			return seats("ceo")
		},
		LeaseTTL: ttl, HeartbeatInterval: ttl / 4, SweepInterval: ttl / 8,
		Turn: func(context.Context, string, []*events.Event) queue.Result {
			return queue.Ack()
		},
		// A HOOK THAT HONOURS ITS CUT, then takes until unwind to return.
		SeatReady: func(ctx context.Context, _ placement.Seat, _ coord.Lease) error {
			cutOnce.Do(func() { close(entered) })
			<-ctx.Done()
			wasCut.Store(true)
			<-unwind
			return ctx.Err()
		},
		SeatDone: func(context.Context, placement.Seat) {
			doneOnce.Do(func() { close(gaveBack) })
		},
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if err := n.Start(t.Context()); err != nil {
		t.Fatalf("node.Start: %v", err)
	}
	t.Cleanup(func() { n.Stop(context.WithoutCancel(t.Context())) })
	// BEFORE THE STOP, so a case that fails below lets the hook return rather
	// than leaving the stop waiting on the claim that runs it.
	var unwound sync.Once
	t.Cleanup(func() { unwound.Do(func() { close(unwind) }) })

	company.Store(true)
	n.Host().Resweep()
	select {
	case <-entered:
	case <-time.After(3 * ttl): //fixedwait:liveness
		t.Fatal("the premise: the node never began taking the seat")
	}

	bound, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	n.Drain(bound)

	if bound.Err() == nil {
		t.Error("the drain returned with its bound still running while the seat it cut " +
			"was still being given back: it reported done beside a give-back in flight")
	}
	if !wasCut.Load() {
		t.Error("the drain did not cut the seat that was being taken")
	}
	select {
	case <-gaveBack:
		t.Error("the premise: the seat was given back inside the drain, so the case " +
			"proves nothing about a give-back the drain has to wait for")
	default:
	}

	unwound.Do(func() { close(unwind) })
	select {
	case <-gaveBack:
	case <-time.After(3 * ttl): //fixedwait:liveness
		t.Fatal("the seat the drain cut was never given back once its hook returned")
	}
	if held := n.Host().Held(); len(held) != 0 {
		t.Errorf("the node still holds %v after the cut seat was given back", held)
	}
}
