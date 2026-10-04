package kv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// holdSeats claims n fresh seats ungated, the way a fleet's seats come to be
// held without a gate being judged for each.
func holdSeats(tb testing.TB, s *Store, n int) {
	tb.Helper()
	for i := range n {
		lease, _, err := s.TryAcquire(context.Background(),
			coord.ClassSeat.Resource(fmt.Sprintf("held-%d", i)),
			coord.AcquireOptions{Owner: "node-b/1", TTL: coordtest.LongTTL, Ungated: true})
		if err != nil || lease == nil {
			tb.Fatalf("hold seat %d: (%v, %v)", i, lease, err)
		}
	}
}

// delivered is how many messages the broker has sent to its clients — which is
// what a listing of the lease buckets costs: one per record, per bucket read.
func delivered(ns *server.Server) int64 {
	v, err := ns.Varz(nil)
	if err != nil {
		panic(err)
	}
	return v.OutMsgs
}

// A GATED CLAIM COSTS THE SAME WITH TWO THOUSAND LEASES HELD AS WITH TEN.
//
// The gates judged every lease in the fleet by LISTING both buckets, on every
// gated claim twice, on every duty claim and in every FleetProtocolFloor —
// so a claim's cost grew with the leases already held, and a fleet claiming
// its seats from cold grew with their square (gate.go has the measurements).
// What a listing costs is its records, delivered to the claimant one by one,
// and the broker counts every message it delivers: so the case holds a number
// of seats, claims more gated, and compares what a claim was delivered at the
// two sizes. The view's watch is started, and its load of the buckets paid,
// by one claim BEFORE the ones counted — that load is the view's cost once
// per run, not a claim's.
func TestAGatedClaimCostsTheSameWhateverIsHeld(t *testing.T) {
	t.Parallel()
	const claims = 20
	perClaim := func(held int) float64 {
		ns, nc := embeddedServer(t)
		s := openStore(t, nc, coordtest.LongTTL)
		holdSeats(t, s, held)
		claim := func(name string) {
			lease, _, err := s.TryAcquire(t.Context(), coord.ClassSeat.Resource(name),
				coord.AcquireOptions{Owner: "node-a/1", TTL: coordtest.LongTTL})
			if err != nil || lease == nil {
				t.Fatalf("gated claim of %s with %d held: (%v, %v)", name, held, lease, err)
			}
		}
		claim("warm")
		before := delivered(ns)
		for i := range claims {
			claim(fmt.Sprintf("claimed-%d", i))
		}
		if _, _, err := s.FleetProtocolFloor(t.Context()); err != nil {
			t.Fatalf("floor: %v", err)
		}
		return float64(delivered(ns)-before) / claims
	}
	few, many := perClaim(10), perClaim(2000)
	// A claim is a handful of reads and writes and a view that is
	// delivered the claim's own writes: a few dozen messages, the same at
	// either size. One listing of two thousand seats is two thousand.
	if many > few+20 {
		t.Fatalf("a gated claim was delivered %.0f messages with 2,000 leases held "+
			"and %.0f with 10 — its cost grows with the fleet's leases", many, few)
	}
}

// A LEASE WRITTEN THE INSTANT BEFORE A GATE IS JUDGED IS IN THE GATE.
//
// The view is a watch, and a watch is behind the store by whatever it has not
// yet been delivered; the barrier is what makes a gate exact rather than
// recent. The view is started first, by a claim, so that what follows is not
// its initial load: an older build's lease lands, and the very next gated
// claim must be refused because of it — and the floor must name it.
//
// A HUNDRED TIMES, because the defect is a race: a gate that judged the view
// without waiting for it lost to the watch about once in fifteen attempts, so
// a hundred leave a missing barrier no realistic way through.
func TestALeaseWrittenJustBeforeAGateIsInIt(t *testing.T) {
	t.Parallel()
	for attempt := range 100 {
		s := openStore(t, embeddedNATS(t), coordtest.LongTTL)
		if lease, _, err := s.TryAcquire(t.Context(), coord.ClassSeat.Resource("first"),
			coord.AcquireOptions{Owner: "node-a/1", TTL: coordtest.LongTTL}); err != nil || lease == nil {
			t.Fatalf("start the view with a claim: (%v, %v)", lease, err)
		}
		if lease, _, err := s.TryAcquire(t.Context(), coord.ClassSeat.Resource("older"),
			coord.AcquireOptions{Owner: "old-node/1", TTL: coordtest.LongTTL,
				Protocol: coord.ProtocolVersion - 1, Ungated: true}); err != nil || lease == nil {
			t.Fatalf("an older build's lease: (%v, %v)", lease, err)
		}
		lease, _, err := s.TryAcquire(t.Context(), coord.ClassSeat.Resource("next"),
			coord.AcquireOptions{Owner: "node-a/1", TTL: coordtest.LongTTL})
		if err != nil {
			t.Fatalf("attempt %d: the gated claim: %v", attempt, err)
		}
		if lease != nil {
			t.Fatalf("attempt %d: a gated claim straight after an older build's lease "+
				"landed was granted — the gate judged a view that had not taken it in",
				attempt)
		}
		floor, found, err := s.FleetProtocolFloor(t.Context())
		if err != nil || !found || floor != coord.ProtocolVersion-1 {
			t.Fatalf("attempt %d: the floor is (%d, %v, %v), want the older build's %d",
				attempt, floor, found, err, coord.ProtocolVersion-1)
		}
	}
}

// THE VIEW STOPS WHEN NOTHING JUDGES A GATE, AND STARTS AGAIN WHEN SOMETHING
// DOES.
//
// Its standing cost is every lease write in the fleet delivered to it, so a
// node that has stopped claiming must stop paying it; and a view that stopped
// learned of no write while stopped, so it must not answer from what it held.
func TestTheViewStopsWhenIdleAndStartsAgainWhenAsked(t *testing.T) {
	t.Parallel()
	s := openStore(t, embeddedNATS(t), coordtest.LongTTL)
	claim := func(name string, protocol int, ungated bool) {
		t.Helper()
		lease, _, err := s.TryAcquire(t.Context(), coord.ClassSeat.Resource(name),
			coord.AcquireOptions{Owner: "node-a/" + name, TTL: coordtest.LongTTL,
				Protocol: protocol, Ungated: ungated})
		if err != nil || lease == nil {
			t.Fatalf("claim %s: (%v, %v)", name, lease, err)
		}
	}
	claim("first", 0, false)
	s.gate.mu.Lock()
	started := s.gate.run
	s.gate.lastUse = time.Now().Add(-2 * gateViewIdle)
	s.gate.mu.Unlock()
	if started == nil {
		t.Fatal("a gated claim judged no view")
	}

	s.gate.stopIfIdle()
	select {
	case <-started.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("an idle view's watch did not stop")
	}
	s.gate.mu.Lock()
	stopped := s.gate.run == nil
	s.gate.mu.Unlock()
	if !stopped {
		t.Fatal("an idle view is still running")
	}

	// WHILE IT IS STOPPED an older build's lease lands; the next gate
	// starts a new view, which must see it.
	claim("older", coord.ProtocolVersion-1, true)
	floor, found, err := s.FleetProtocolFloor(t.Context())
	if err != nil || !found || floor != coord.ProtocolVersion-1 {
		t.Fatalf("after a restart the floor is (%d, %v, %v), want %d", floor, found, err,
			coord.ProtocolVersion-1)
	}
	s.gate.mu.Lock()
	restarted := s.gate.run
	s.gate.mu.Unlock()
	if restarted == nil || restarted == started {
		t.Fatal("a gate judged after the view stopped did not start a new one")
	}

	// AND CLOSE WAITS FOR IT.
	s.Close()
	select {
	case <-restarted.exited:
	default:
		t.Fatal("Close returned with the view's watch still running")
	}
}

// BenchmarkAGatedClaim is what a gated seat claim costs with ten leases held
// and with ten thousand — the fleet the gate used to list on every claim.
func BenchmarkAGatedClaim(b *testing.B) {
	for _, held := range []int{10, 10_000} {
		b.Run(fmt.Sprintf("held=%d", held), func(b *testing.B) {
			_, nc := embeddedServer(b)
			s, err := Open(context.Background(), jsOf(nc), Config{
				TTL: coordtest.LongTTL, BucketPrefix: fmt.Sprintf("b%d", bucketSeq.Add(1)),
			})
			if err != nil {
				b.Fatalf("Open: %v", err)
			}
			b.Cleanup(s.Close)
			holdSeats(b, s, held)
			// One claim first: the view's load of the buckets is paid once
			// per run of it, not per claim.
			if lease, _, err := s.TryAcquire(context.Background(), coord.ClassSeat.Resource("warm"),
				coord.AcquireOptions{Owner: "node-a/1", TTL: coordtest.LongTTL}); err != nil || lease == nil {
				b.Fatalf("warm the view: (%v, %v)", lease, err)
			}
			b.ResetTimer()
			for i := range b.N {
				lease, _, err := s.TryAcquire(context.Background(),
					coord.ClassSeat.Resource(fmt.Sprintf("bench-%d", i)),
					coord.AcquireOptions{Owner: "node-a/1", TTL: coordtest.LongTTL})
				if err != nil || lease == nil {
					b.Fatalf("gated claim: (%v, %v)", lease, err)
				}
			}
		})
	}
}
