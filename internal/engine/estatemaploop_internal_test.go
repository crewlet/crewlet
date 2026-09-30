package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/estate/partmap"
)

// leaseScript is a listing of the estate leases per poll, scripted: each call
// answers the next entry, and the last one from then on.
type leaseScript struct {
	listings [][]partmap.Presence
	errs     []error
	calls    int
}

func (s *leaseScript) live(context.Context) ([]partmap.Presence, error) {
	i := min(s.calls, len(s.listings)-1)
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	return s.listings[i], nil
}

// leasesAt is one node's lease naming the map epoch it acted on.
func leasesAt(epoch uint64) []partmap.Presence {
	layout := 1
	healthy := true
	return []partmap.Presence{{Node: "data-00", Meta: partmap.Meta{Weight: 1, Layout: &layout,
		Healthy: &healthy, MapEpoch: epoch, Partitions: map[string]partmap.PartitionState{}}}}
}

// aLoopOn is an estate map loop on a fake clock whose sleep moves it, listing
// leases from script and counting its convergences at the fake instant each
// ran.
func aLoopOn(script *leaseScript) (*estateMapLoop, *[]time.Duration) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	now := start
	var converged []time.Duration
	l := &estateMapLoop{
		live:  script.live,
		now:   func() time.Time { return now },
		sleep: func(_ context.Context, d time.Duration) { now = now.Add(d) },
		poll:  estateLeasePoll,
		converge: func(context.Context) (bool, error) {
			converged = append(converged, now.Sub(start))
			return true, nil
		},
	}
	return l, &converged
}

// THE ESTATE MAP CONVERGES BETWEEN TICKS WHEN A LEASE CHANGES AND HOLDS STILL.
//
// A node's word reaches the map on its lease's renewal, and waited for the
// next tick as well — up to fifteen seconds of a joiner that serves going
// unrouted. So the duty's holder lists the leases every second between ticks
// and converges once a change has held still for one listing: once after the
// tick (a lease may have moved since the tick read it), once per settled
// change, never while the leases are still moving, never on a listing it could
// not take, and never past the tick it is waiting for.
func TestTheEstateMapConvergesBetweenTicksOnASettledLeaseChange(t *testing.T) {
	t.Parallel()
	a, b, c, d, e := leasesAt(1), leasesAt(2), leasesAt(3), leasesAt(4), leasesAt(5)
	script := &leaseScript{
		//          1s 2s 3s 4s 5s 6s 7s 8s 9s  10s 11s
		listings: [][]partmap.Presence{a, a, b, b, c, d, e, e, e, e, e},
		errs:     []error{nil, nil, nil, nil, nil, nil, nil, errors.New("unreachable")},
	}
	l, converged := aLoopOn(script)
	l.mapped = true
	l.wait(t.Context(), 10*time.Second)

	want := []time.Duration{2 * time.Second, 4 * time.Second, 9 * time.Second}
	if len(*converged) != len(want) {
		t.Fatalf("the loop converged at %v, want %v: after the tick, on each change "+
			"once it held still, and not while the leases kept moving", *converged, want)
	}
	for i := range want {
		if (*converged)[i] != want[i] {
			t.Errorf("convergence %d ran at %v, want %v", i, (*converged)[i], want[i])
		}
	}
	if script.calls != 10 {
		t.Errorf("the loop listed the leases %d times in a ten-second wait, want one a second", script.calls)
	}
}

// A TURN THAT DID NOT HOLD THE DUTY, OR FOUND NO MAP, WAITS AND NOTHING ELSE.
//
// Only the duty's holder converges, and only a map that exists: under the
// single-file layout there is none, and the loop must not list the leases every
// second for ever on every node that runs workers. The claim and the tick are
// what say which turn it was.
func TestOnlyAHeldDutyWithAMapConvergesBetweenTicks(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		claim  func(context.Context) (bool, error)
		mapped bool
	}{
		"a peer holds the duty": {
			claim: func(context.Context) (bool, error) { return false, nil }, mapped: true},
		"the claim could not be asked": {
			claim: func(context.Context) (bool, error) { return false, errors.New("store down") }, mapped: true},
		"no map": {
			claim: func(context.Context) (bool, error) { return true, nil }, mapped: false},
	} {
		script := &leaseScript{listings: [][]partmap.Presence{leasesAt(1)}}
		l, converged := aLoopOn(script)
		// THE TURN BEFORE held the duty and found a map: what this turn
		// found is what must end the convergence.
		l.mapped = true
		duty := mapDuty{event: "estate_map", claim: l.claimed(c.claim),
			tick: l.ticked(func(context.Context) (mapTick, error) {
				return mapTick{mapped: c.mapped}, nil
			})}
		l.wait(t.Context(), duty.turn(t.Context()))
		if script.calls != 0 || len(*converged) != 0 {
			t.Errorf("%s: the wait listed the leases %d time(s) and converged %d time(s), "+
				"want neither", name, script.calls, len(*converged))
		}
	}

	// THE CONTROL: a held duty whose tick read a map does converge — and a
	// node with no fleet ticks alone, holding the duty by definition.
	for name, claim := range map[string]func(context.Context) (bool, error){
		"held":     func(context.Context) (bool, error) { return true, nil },
		"no fleet": nil,
	} {
		script := &leaseScript{listings: [][]partmap.Presence{leasesAt(1)}}
		l, converged := aLoopOn(script)
		duty := mapDuty{event: "estate_map", claim: l.claimed(claim),
			tick: l.ticked(func(context.Context) (mapTick, error) {
				return mapTick{mapped: true}, nil
			})}
		l.wait(t.Context(), duty.turn(t.Context()))
		if len(*converged) != 1 {
			t.Errorf("%s: a held duty with a map converged %d time(s) between ticks, want once",
				name, len(*converged))
		}
	}
}
