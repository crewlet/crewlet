package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/seat"
)

// A STOP AGAINST A COORDINATION STORE THAT HAS GONE AWAY SPENDS ONE ALLOWANCE.
//
// A member that has lost quorum still stops in order: it gives its presence
// and its seats back, withdraws its admission and releases its duties — and
// every one of those fails, each falling back to a lease lapsing on its TTL.
// Each used to wait out a bound of its own, so the stop spent their sum (the
// e2e fleet measured 25 s for one member), close to an orchestrator's kill
// grace and past it once the flushes behind them ran. Here every round trip to
// the lease and fleet stores stalls for five seconds once the stop begins,
// against a lease TTL of three — an allowance of one second — so the old sum
// is past half a minute and one allowance is a second. (The stream stays up:
// the native backends this company runs are reached through the broker's own
// client, which a stand-in cannot be.)
func TestAStopAgainstAnUnreachableStoreSpendsOneAllowance(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
		b.Coordination.LeaseTTLSeconds = 3
	})
	back, err := engine.OpenBackends(t.Context(), b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	var gone atomic.Bool
	back.Coord = stallingLeases{coordBackend: back.Coord, gone: &gone}
	back.Fleet = stallingFleet{fleet: back.Fleet, gone: &gone}

	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: b, Company: parsedCompany(t, companyDoc), Backends: back,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(e.Node().Host().Held()) == 0 {
		t.Fatal("the premise: the node holds seats for its stop to give back")
	}

	gone.Store(true)
	started := time.Now()
	e.Stop(context.Background())
	took := time.Since(started)
	allowance := seat.StopAllowance(3 * time.Second)
	// THE ALLOWANCE, and room for the local teardown around it: one stalled
	// round trip alone is five seconds, so a stop that left even one of its
	// steps to a bound of its own would be past this.
	if limit := allowance + 3*time.Second; took > limit {
		t.Fatalf("the stop took %v against a store that answers nothing, past "+
			"its %v allowance and %v of local teardown: its round trips each "+
			"waited out a bound of their own", took, allowance, limit-allowance)
	}
}

// errGone is what the stalled store answers once its stall is over.
var errGone = errors.New("the coordination store has lost quorum")

// stall waits out a round trip to a store that has gone away: until the
// request's context ends, or five seconds — a client's own timeout.
func stall(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errGone
	}
}

type coordBackend = coord.Backend

// stallingLeases is a lease store whose reads and releases stall once gone is
// set.
type stallingLeases struct {
	coordBackend
	gone *atomic.Bool
}

func (l stallingLeases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	if l.gone.Load() {
		return false, stall(ctx)
	}
	return l.coordBackend.Release(ctx, resource, owner, epoch)
}

func (l stallingLeases) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	if l.gone.Load() {
		return nil, stall(ctx)
	}
	return l.coordBackend.Get(ctx, resource)
}

// stallingFleet is a fleet store whose admission withdrawal stalls once gone
// is set.
type stallingFleet struct {
	fleet
	gone *atomic.Bool
}

func (f stallingFleet) ForgetAdmission(ctx context.Context, nodeID, incarnation string) error {
	if f.gone.Load() {
		return stall(ctx)
	}
	return f.fleet.ForgetAdmission(ctx, nodeID, incarnation)
}
