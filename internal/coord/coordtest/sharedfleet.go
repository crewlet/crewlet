package coordtest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/period"
)

// RunSharedFleet certifies what ONE fleet store answers through SEVERAL
// HANDLES on it — every node a client of its own, writing the same records.
//
// [RunFleet] hands each case one handle, which is one node however many
// goroutines a case runs through it. A backend that coalesces a node's writes
// to one key (coord/kv) makes a handle's contention on a key ONE contender,
// whatever the case puts through it, so a store certified through one handle
// has never met two nodes racing one record — which is the only contention a
// fleet's hottest key, the org's token counter, ever has once its node's
// charges are one write.
//
// open returns the handles, every one on the same EMPTY store, and at least
// two of them; a backend with a single process returns the same handle more
// than once. Run it with the handles on different connections — through
// different leaves, where that is the topology — or it certifies one node
// twice.
func RunSharedFleet(t *testing.T, open func(t *testing.T) []coord.Fleet) {
	t.Helper()
	for _, c := range sharedFleetCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			handles := open(t)
			if len(handles) < 2 {
				t.Fatalf("RunSharedFleet: open returned %d handles, want at least two", len(handles))
			}
			// NOT t.Context(), for [RunFleet]'s reason: it is cancelled
			// before cleanup runs, and the cases join goroutines that are
			// still mid-call at that point.
			ctx, cancel := context.WithTimeout(context.Background(), stallBudget)
			t.Cleanup(cancel)
			c.fn(ctx, t, handles)
		})
	}
}

var sharedFleetCases = []struct {
	name string
	fn   func(ctx context.Context, t *testing.T, handles []coord.Fleet)
}{
	{"every_nodes_charges_add_up", testEveryNodesChargesAddUp},
	{"every_nodes_charges_never_admit_past_a_ceiling", testEveryNodesChargesNeverAdmitPastACeiling},
}

// chargeFromEveryHandle puts [ContendedCharges] charges of tokens on one org
// counter through EVERY handle at once, released together, and answers how
// many were admitted. Any charge that fails fails the case: every one of
// them reaches a healthy store, so an error is a healthy round reported as
// an outage.
func chargeFromEveryHandle(ctx context.Context, t *testing.T, handles []coord.Fleet,
	tokens int, orgCaps coord.Caps) int {

	t.Helper()
	w := coord.WindowsAt(time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC), time.UTC)
	var (
		wg       sync.WaitGroup
		admitted atomic.Int64
		mu       sync.Mutex
		failures []string
	)
	start := make(chan struct{})
	for i, f := range handles {
		for k := range ContendedCharges {
			// A seat of its own per charge: the case is about the ORG's
			// counter, which every charge writes, and a shared seat would
			// make each handle's seat a second hot key that tells the
			// case nothing the org's does not.
			seat := coord.AgentScope(fmt.Sprintf("shared-%d-%d", i, k))
			wg.Go(func() {
				<-start
				got, err := f.Charge(ctx, coord.ChargeRequest{
					Seat: seat, Tokens: tokens, Windows: w, OrgCaps: orgCaps,
				})
				switch {
				case err != nil:
					mu.Lock()
					if len(failures) < 5 {
						failures = append(failures, fmt.Sprintf("handle %d: %v", i, err))
					}
					mu.Unlock()
				case got.OK:
					admitted.Add(1)
				}
			})
		}
	}
	close(start)
	wg.Wait()
	for _, f := range failures {
		t.Errorf("a charge to a healthy store failed: %s", f)
	}
	if len(failures) > 0 {
		t.FailNow()
	}
	return int(admitted.Load())
}

// orgSpentThrough reads the org's day through one handle at the case's
// windows.
func orgSpentThrough(ctx context.Context, t *testing.T, f coord.Fleet) int {
	t.Helper()
	w := coord.WindowsAt(time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC), time.UTC)
	u, err := f.Used(ctx, coord.OrgScope, w)
	if err != nil {
		t.Fatalf("Used(org): %v", err)
	}
	return u.In(period.Day).Used
}

// EVERY NODE'S CHARGES ADD UP: more charges through each handle than any
// attempt bound, from several handles at once, and every one of them is
// counted — none lost to a read-modify-write that two nodes raced, and none
// failed closed by a compare-and-swap that lost to another node every time.
func testEveryNodesChargesAddUp(ctx context.Context, t *testing.T, handles []coord.Fleet) {
	chargeFromEveryHandle(ctx, t, handles, 10, nil)
	want := len(handles) * ContendedCharges * 10
	for i, f := range handles {
		if got := orgSpentThrough(ctx, t, f); got != want {
			t.Fatalf("the org's day reads %d through handle %d after %d charges of 10 "+
				"from each of %d handles, want %d", got, i, ContendedCharges, len(handles), want)
		}
	}
}

// EVERY NODE'S CHARGES NEVER ADMIT PAST A CEILING: the gate's half of the same
// property across nodes. What is admitted is what the ceiling holds, exactly,
// however the nodes' writes interleave; what is counted is every round.
func testEveryNodesChargesNeverAdmitPastACeiling(ctx context.Context, t *testing.T, handles []coord.Fleet) {
	const ceiling, tokens = 1000, 10
	admitted := chargeFromEveryHandle(ctx, t, handles, tokens, coord.Caps{period.Day: ceiling})
	if admitted != ceiling/tokens {
		t.Fatalf("%d of %d racing charges of %d were admitted against a ceiling of %d, "+
			"want exactly %d", admitted, len(handles)*ContendedCharges, tokens, ceiling, ceiling/tokens)
	}
	if got, want := orgSpentThrough(ctx, t, handles[0]), len(handles)*ContendedCharges*tokens; got != want {
		t.Fatalf("org spend = %d after every racing charge, want all %d: a refused round "+
			"is spent like an admitted one", got, want)
	}
}
