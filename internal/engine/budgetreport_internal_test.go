package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
)

// meteringReporter is a meter loop over a company capped at 1 000 tokens a
// day that has spent 250 today, with the given lease store — everything a
// frame reads, and nothing a publish adds.
func meteringReporter(t *testing.T, leases coord.Backend, now time.Time) *budgetReporter {
	t.Helper()
	fleet := coordmem.NewFleet()
	windows := coord.WindowsAt(now, time.UTC)
	if _, err := fleet.PostCharge(t.Context(), coord.AgentScope("x"), 250, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	e := &Engine{backends: &Backends{Coord: leases, Fleet: fleet}}
	e.epoch.current.Store(meteredCompany(config.TokenBudget{Day: ceiling(1000)}))
	return &budgetReporter{engine: e}
}

// THE METER WAITS FOR THE LAST NODE THAT CHARGES THE LIFETIME COUNTER.
//
// During the rolling upgrade that windowed the counters, the older nodes run
// every seat and charge the lifetime counter, and a newer node claims no seat
// beside them — so the windowed counters it reads stay empty. Its frames would
// read the company as having spent nothing, and every dashboard folds each
// node's frame over the last, so the header would flicker between the two
// readings for the whole rollout. It publishes nothing until the fleet's floor
// reaches the windowed protocol, and once it has, it stops asking.
//
// Asked of [budgetReporter.frame], the decision [budgetReporter.publish]
// sends or does not send, rather than of the gate alone: a gate nothing
// consults passes every test of the gate.
func TestTheMeterWaitsForTheLastLifetimeCounterNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	leases := coordmem.New()
	r := meteringReporter(t, leases, now)

	older, err := leases.TryAcquire(ctx, coord.NodeResource("older"), coord.AcquireOptions{
		Owner: "older:1", TTL: time.Hour, Ungated: true,
		Protocol: coord.WindowedCountersProtocol - 1,
	})
	if err != nil || older == nil {
		t.Fatalf("claim the older node's presence = (%v, %v)", older, err)
	}
	if _, err := leases.TryAcquire(ctx, coord.NodeResource("newer"), coord.AcquireOptions{
		Owner: "newer:1", TTL: time.Hour, Ungated: true,
	}); err != nil {
		t.Fatalf("claim this node's presence: %v", err)
	}
	if frame, sent := r.frame(ctx, now); sent {
		t.Fatalf("the meter would publish %+v from the windowed counters while a node "+
			"of the lifetime counters' build is live and charging the other one", frame)
	}

	if released, err := leases.Release(ctx, coord.NodeResource("older"), "older:1", older.Epoch); err != nil || !released {
		t.Fatalf("release the older node = (%v, %v)", released, err)
	}
	frame, sent := r.frame(ctx, now)
	if !sent {
		t.Fatal("the meter still publishes nothing after the last older node has gone")
	}
	if frame.OrgUsedTokens != 250 || frame.OrgMaxTokens != 1000 {
		t.Fatalf("frame = %d of %d, want the company's 250 of its 1000 a day",
			frame.OrgUsedTokens, frame.OrgMaxTokens)
	}
	// LATCHED: a floor that has reached the windowed protocol falls again
	// only by a downgrade, which needs the whole fleet stopped, so the
	// meter stops paying a lease listing per frame for it.
	r.engine.backends.Coord = unreadableFloor{leases}
	if _, sent := r.frame(ctx, now); !sent {
		t.Fatal("the meter asked the lease store again after it had seen the fleet current")
	}
}

// A FLOOR THAT CANNOT BE READ IS NOT YET: a frame skipped costs one interval of
// a meter that keeps its last reading, while a frame published from the wrong
// counter is a reading that is wrong.
func TestAnUnreadableFloorPublishesNoFrame(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	r := meteringReporter(t, unreadableFloor{coordmem.New()}, now)
	if frame, sent := r.frame(t.Context(), now); sent {
		t.Fatalf("the meter published %+v on a protocol floor nobody could read", frame)
	}
}

// unreadableFloor is a lease store whose protocol floor cannot be read.
type unreadableFloor struct{ coord.Backend }

func (unreadableFloor) FleetProtocolFloor(context.Context) (int, bool, error) {
	return 0, false, errors.New("the lease store is unreachable")
}
