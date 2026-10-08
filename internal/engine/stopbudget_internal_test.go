package engine

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/seat"
)

// A STOP'S LAST EVENTS ARE STEPS OF ITS ALLOWANCE. The node's own
// announcement and each released seat's last lifecycle event are publishes to
// a broker that, on a member without quorum, answers only when the request's
// context ends — and the drain's context has no deadline, since it waits for
// running turns. Each publish is therefore bounded by what is left of the
// stop's one allowance, and a broker that never answers costs the stop that
// allowance once rather than an unbounded wait per event.
func TestAStopsLastEventsAreStepsOfItsAllowance(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{Queue: unansweredPublishes{}}}
	e.epoch.current.Store(companyFor(t, "name: Acme\nroles:\n  - name: Lead\n    handle: lead\n"))

	const allowance = 200 * time.Millisecond
	ctx := seat.WithStopBudget(context.Background(), seat.NewStopBudget(allowance))
	finished := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(finished)
		e.publishLifecycle(ctx, events.New(types.OrgStopped{OrgName: "Acme"}, events.NewTrace()))
		e.publishSeatLifecycle(ctx, "lead", types.AgentTerminated{})
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("a stop's publish to a broker that never answers was never " +
			"ended: it is not a step of the stop's allowance")
	}
	if took := time.Since(started); took > allowance+2*time.Second {
		t.Fatalf("the stop's two publishes took %v against an allowance of %v: "+
			"each waited out a bound of its own", took, allowance)
	}
}

// unansweredPublishes is a broker that answers a publish only when the
// request's context ends.
type unansweredPublishes struct{ queue.EventQueue }

func (unansweredPublishes) Publish(ctx context.Context, _ string, _ *events.Event) error {
	<-ctx.Done()
	return ctx.Err()
}

// A HOLD GIVEN BACK ONCE THE STOP HAS BEGUN IS A STEP OF ITS ALLOWANCE, AND
// ONE GIVEN BACK BEFORE IT IS NOT.
//
// The integration loop's pass holds its surface's lease and gives it back as
// it ends, on the context the hold was TAKEN on with its cancellation removed
// — a context made long before any stop — and the teardown waits for that
// pass. Against a store that had gone away the give-back sat out a client's
// whole request timeout beside the stop's allowance, five seconds past it
// ([holdLeases]). Here a store that answers a release only after a stall of
// four allowances: before the stop the give-back waits it out, because a pass
// ending on its own is no stop's step and charging it would spend what the
// stop is owed; once the stop has begun, it ends with the allowance.
func TestAHoldGivenBackDuringTheStopIsAStepOfItsAllowance(t *testing.T) {
	t.Parallel()
	const ttl = 1500 * time.Millisecond
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	b.Coordination.LeaseTTLSeconds = ttl.Seconds()
	SeedStore(t, &b)
	company, err := config.ParseCompany([]byte(watchdogCompanyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, company)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	slow := &slowReleases{Backend: back.Coord}
	back.Coord = slow
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: company, Backends: back})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	allowance := seat.StopAllowance(ttl)
	stall := 4 * allowance
	hold := e.workerHold("hold-probe", time.Minute)
	giveBack := func() time.Duration {
		t.Helper()
		release, held, err := hold(t.Context())
		if err != nil || !held {
			t.Fatalf("hold = (%v, %v), want it held", held, err)
		}
		slow.stall.Store(int64(stall))
		defer slow.stall.Store(0)
		started := time.Now()
		release()
		return time.Since(started)
	}

	if took := giveBack(); took < stall {
		t.Fatalf("before any stop, a hold's give-back ended after %v, inside the "+
			"%v its store took: a pass ending on its own was charged to a stop", took, stall)
	}
	e.stopping()
	// HALF THE STALL separates the outcomes with room on both sides: the
	// allowance is a quarter of it, and a give-back left to its own bound
	// takes all of it.
	if took := giveBack(); took >= stall/2 {
		t.Fatalf("once the stop had begun, a hold's give-back took %v against "+
			"an allowance of %v: it waited out its store rather than the stop's "+
			"allowance", took, allowance)
	}
}

// slowReleases is a lease store that answers a release only after stall, or
// when the request's context ends first.
type slowReleases struct {
	coord.Backend
	stall atomic.Int64
}

func (s *slowReleases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	if d := time.Duration(s.stall.Load()); d > 0 {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(d):
		}
	}
	return s.Backend.Release(ctx, resource, owner, epoch)
}
