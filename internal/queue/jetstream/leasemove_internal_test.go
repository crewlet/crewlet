package jetstream

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// startMoving starts a lease link's probe on timing with firstHop and move,
// proven now; stopped with the case, if not before.
func startMoving(t *testing.T, timing dataPathTiming, firstHop, move func() error) *dataPath {
	t.Helper()
	d := startLeaseLink(timing, firstHop, move, slog.New(slog.DiscardHandler), time.Now())
	t.Cleanup(d.stop)
	return d
}

// A LEASE LINK THAT STOPS ANSWERING IS MOVED: a probe unanswered past its bound
// refuses the link, wrapping ErrLeaseLinkUnproven, and the link is dropped so
// it dials the next member, no sooner than the bound after the probe went out;
// a member answering "not available" past the budget is left the same way.
// Once the next member answers, the link reads proven and stays where it is.
//
// Mutant: relink never called — move is never asked. Mutant: the verdict's
// age restarted by a re-ask — the silent link is never refused, so never moved.
func TestALeaseLinkThatStopsAnsweringIsMoved(t *testing.T) {
	t.Parallel()
	const bound, every = 100 * time.Millisecond, 10 * time.Millisecond
	t.Run("silent", func(t *testing.T) {
		t.Parallel()
		// In a bubble, the ask and the move read one clock, so the age is exact.
		synctest.Test(t, func(t *testing.T) {
			var moved atomic.Bool
			start := time.Now()
			movedAt := make(chan time.Time, 1)
			d := startMoving(t, dataPathTiming{every: every, bound: bound, notAvailable: time.Hour,
				cap: bound, probe: func(ctx context.Context) error {
					if moved.Load() {
						return nil
					}
					<-ctx.Done()
					return ctx.Err()
				}}, func() error { return nil }, func() error {
				if !moved.Swap(true) {
					movedAt <- time.Now()
				}
				return nil
			})
			at := signalled(t, movedAt, "the silent lease link to be moved")
			if age := at.Sub(start); age < bound {
				t.Errorf("the lease link was moved %v after its probe went out, under its %v bound", age, bound)
			}
			synctest.Wait()
			time.Sleep(3 * bound)
			synctest.Wait()
			if err := d.verdict(time.Now()); err != nil {
				t.Errorf("the moved lease link, answering, reads %v", err)
			}
		})
	})
	t.Run("not available", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			const budget = time.Second
			notAvailable := &jetstream.APIError{Code: 503, ErrorCode: 10008,
				Description: "JetStream system temporarily unavailable"}
			var moves atomic.Int32
			start := time.Now()
			movedAt := make(chan time.Time, 1)
			d := startMoving(t, dataPathTiming{every: every, bound: bound, notAvailable: budget,
				cap: bound, probe: func(context.Context) error {
					if moves.Load() > 0 {
						return nil
					}
					return notAvailable
				}}, func() error { return nil }, func() error {
				if moves.Add(1) == 1 {
					movedAt <- time.Now()
				}
				return nil
			})
			synctest.Wait()
			err := d.verdict(start.Add(budget))
			if !errors.Is(err, ErrLeaseLinkUnproven) || !strings.Contains(err.Error(), "not available") {
				t.Errorf("answered \"not available\" for its whole budget, the lease link reads %v", err)
			}
			at := signalled(t, movedAt, "the lease link on a member answering \"not available\" to be moved")
			if age := at.Sub(start); age < budget {
				t.Errorf("the lease link was moved %v after its last success, inside its %v budget", age, budget)
			}
			time.Sleep(3 * bound)
			synctest.Wait()
			if n := moves.Load(); n != 1 {
				t.Errorf("the lease link was moved %d times, want once: the next member answered", n)
			}
		})
	})
}

// A LEASE LINK WHOSE FIRST HOP IS DOWN IS NOT MOVED: the link is already
// redialling, and dropping it again would only restart that; it reads down,
// naming its first hop, not unproven.
//
// Mutant: relink on any refusal — move is asked while the first hop is down.
func TestALeaseLinkWhoseFirstHopIsDownIsNotMoved(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const bound = 100 * time.Millisecond
		hopDown := errors.New("no lease link")
		var moves atomic.Int32
		d := startMoving(t, dataPathTiming{every: 10 * time.Millisecond, bound: bound,
			notAvailable: time.Hour, cap: bound, probe: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			}}, func() error { return hopDown }, func() error {
			moves.Add(1)
			return nil
		})
		time.Sleep(10 * bound)
		synctest.Wait()
		if n := moves.Load(); n != 0 {
			t.Errorf("a lease link whose first hop is down was moved %d times", n)
		}
		if err := d.verdict(time.Now()); !errors.Is(err, hopDown) {
			t.Errorf("a lease link whose first hop is down reads %v, want its first hop", err)
		}
	})
}

// linkID is the id of a leaf's one link in account, zero while it has none.
func linkID(e *embeddedServer, account string) uint64 {
	leafz, err := e.ns.Leafz(&server.LeafzOptions{Account: account})
	if err != nil || len(leafz.Leafs) != 1 {
		return 0
	}
	return leafz.Leafs[0].ID
}

// RELINK MOVES THE LEASE LINK ALONE: the leaf's link in the lease account is
// dropped and dials again, the data link left as it was, and the lease
// connection reads proven once the new link answers.
//
// Mutant: relink drops every leaf link — the data link's id changes.
// Mutant: relink drops nothing — the lease link's id never changes.
func TestRelinkMovesTheLeaseLinkAlone(t *testing.T) {
	t.Parallel()
	f := newMeterFleet(t, linktest.Lagging)
	q := clientOf(t, f.leaf)
	leasesOf(t, q)
	data, lease := linkID(f.leaf.embedded, server.DEFAULT_GLOBAL_ACCOUNT), linkID(f.leaf.embedded, LeaseAccount)
	if data == 0 || lease == 0 {
		t.Fatalf("the leaf holds data link %d and lease link %d, want one of each", data, lease)
	}
	if err := f.leaf.embedded.relink(LeaseAccount); err != nil {
		t.Fatalf("relink: %v", err)
	}
	until(t, "the dropped lease link dialled again", func() bool {
		id := linkID(f.leaf.embedded, LeaseAccount)
		return id != 0 && id != lease
	})
	if got := linkID(f.leaf.embedded, server.DEFAULT_GLOBAL_ACCOUNT); got != data {
		t.Errorf("moving the lease link moved the data link too, from %d to %d", data, got)
	}
	until(t, "the moved lease link read up", func() bool { return q.LeaseLink() == nil })
}
