package maintenance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/maintenance"
)

// lifetime is a coordination store holding the lifetime token counters until
// they are retired.
type lifetime struct {
	present bool
	retires int
}

func (l *lifetime) RetireLifetimeCounters(context.Context) (bool, error) {
	l.retires++
	was := l.present
	l.present = false
	return was, nil
}

// floorAt is a lease store whose live leases bottom out at one protocol.
type floorAt struct {
	floor int
	live  bool
	err   error
}

func (f floorAt) FleetProtocolFloor(context.Context) (int, bool, error) {
	return f.floor, f.live, f.err
}

// THE LIFETIME COUNTERS OUTLIVE EVERY NODE THAT CHARGES THEM.
//
// A build before the windowed counters charges the lifetime bucket on every
// round it runs, and deleting it under such a node fails that node's every
// charge closed. So while any live lease sits below the protocol that
// windowed the counters, the job has no work; at that protocol, the first tick
// deletes the bucket and every later tick finds nothing.
func TestTheLifetimeCountersAreRetiredOnlyOnceNoOlderNodeIsLive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		floor   floorAt
		retired bool
	}{
		{"an older node is live", floorAt{floor: coord.WindowedCountersProtocol - 1, live: true}, false},
		{"nothing is live at all", floorAt{}, false},
		{"every live lease is windowed", floorAt{floor: coord.WindowedCountersProtocol, live: true}, true},
		{"a later protocol still counts", floorAt{floor: coord.WindowedCountersProtocol + 1, live: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bucket := &lifetime{present: true}
			w := newWorker(t, maintenance.Options{
				Now: fixed(base), Jobs: maintenance.RetiredBudgetJobs(bucket, tc.floor),
			})
			swept, err := w.Tick(t.Context())
			if err != nil {
				t.Fatalf("tick: %v", err)
			}
			if got := !bucket.present; got != tc.retired {
				t.Fatalf("retired = %v, want %v (floor %d, live %v)",
					got, tc.retired, tc.floor.floor, tc.floor.live)
			}
			if tc.retired && swept["retired_budget_bucket"] != 1 {
				t.Fatalf("swept = %v, want the retirement reported once", swept)
			}
			if !tc.retired && bucket.retires != 0 {
				t.Fatalf("the retirement was attempted %d time(s) behind a gate that "+
					"said an older node still charges the bucket", bucket.retires)
			}
			// And the next tick is quiet: nothing is left to retire.
			swept, err = w.Tick(t.Context())
			if err != nil || swept["retired_budget_bucket"] != 0 {
				t.Fatalf("the second tick = (%v, %v), want nothing retired", swept, err)
			}
		})
	}
}

// A FLOOR THAT CANNOT BE READ IS NOT "NOT YET". The job does nothing — a
// deletion cannot be taken back — and the tick says it could not tell.
func TestAnUnreadableFloorRetiresNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	bucket := &lifetime{present: true}
	w := newWorker(t, maintenance.Options{
		Now: fixed(base),
		Jobs: maintenance.RetiredBudgetJobs(bucket,
			floorAt{err: errors.New("the lease store is unreachable")}),
	})
	if _, err := w.Tick(t.Context()); err == nil {
		t.Fatal("a floor that could not be read was reported as no work")
	}
	if !bucket.present || bucket.retires != 0 {
		t.Fatal("the lifetime counters were retired on a floor nobody could read")
	}
}

// NO COORDINATION STORE, NO JOB: a node without one has no bucket to retire.
func TestNoCoordinationStoreContributesNoRetirement(t *testing.T) {
	t.Parallel()
	if jobs := maintenance.RetiredBudgetJobs(nil, floorAt{}); jobs != nil {
		t.Fatalf("jobs = %v with no store", jobs)
	}
	if jobs := maintenance.RetiredBudgetJobs(&lifetime{}, nil); jobs != nil {
		t.Fatalf("jobs = %v with no lease store", jobs)
	}
}
