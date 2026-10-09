package engine

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A STOP'S LAST EVENTS SPEND NONE OF ITS ALLOWANCE, AND EACH HAS A BOUND OF
// ITS OWN.
//
// The node's announcement and each released seat's last lifecycle event are
// publishes on the event stream, which can lose its quorum while the
// coordination store answers, and a publish it never acknowledges waits out
// whatever deadline it is handed. As steps of the stop's allowance they spent
// the time every lease behind them needed. Here a stream that holds each one
// past the whole allowance: each is published on the client's own request
// timeout, free of its caller's cancellation, and the allowance is whole when
// both are over.
func TestAStopsLastEventsSpendNoneOfItsAllowance(t *testing.T) {
	t.Parallel()
	const allowance = 200 * time.Millisecond
	stream := &heldPublishes{hold: 2 * allowance}
	e := &Engine{backends: &Backends{Queue: stream}}
	e.epoch.current.Store(companyFor(t, "name: Acme\nroles:\n  - name: Lead\n    handle: lead\n"))

	ctx := seat.WithStopBudget(context.Background(), seat.NewStopBudget(allowance))
	// A CALLER THAT HAS ALREADY GIVEN UP does not take back a stop that
	// happened: a release's context is routinely one a shutdown ended.
	ended, cancel := context.WithCancel(ctx)
	cancel()
	e.publishLifecycle(ctx, events.New(types.OrgStopped{OrgName: "Acme"}, events.NewTrace()))
	e.publishSeatLifecycle(ended, "lead", types.AgentTerminated{})

	asked := stream.asked()
	if len(asked) != 2 {
		t.Fatalf("%d publish(es) reached the stream, want the announcement and "+
			"the seat's last event", len(asked))
	}
	for _, p := range asked {
		if p.err != nil {
			t.Errorf("%s was published on a context already ended (%v): the "+
				"caller's cancellation took back an event that had happened", p.event, p.err)
		}
		if !p.bounded {
			t.Errorf("%s was published with no deadline of its own", p.event)
			continue
		}
		// THE CLIENT'S OWN TIMEOUT, and not what was left of the
		// allowance: a quarter of a second either side tells the two apart
		// with room.
		if bound := p.deadline.Sub(p.at); bound < lifecyclePublishBudget-time.Second/4 ||
			bound > lifecyclePublishBudget+time.Second/4 {
			t.Errorf("%s was published on a bound of %v, want its own %v",
				p.event, bound, lifecyclePublishBudget)
		}
	}
	// AND THE ALLOWANCE IS WHOLE: both publishes held past it, and a give-back
	// begun after them still has all of it.
	step, done := seat.StopStep(ctx)
	defer done()
	deadline, ok := step.Deadline()
	if !ok {
		t.Fatal("a step of the stop has no deadline")
	}
	if left := time.Until(deadline); left < allowance/2 {
		t.Fatalf("after the stop's two events a give-back has %v of a %v "+
			"allowance: the events spent what the leases are owed", left, allowance)
	}
}

// heldPublishes is a stream that acknowledges a publish only after hold, or
// when the request's context ends first, recording each one it was asked for.
type heldPublishes struct {
	queue.EventQueue
	hold time.Duration

	mu   sync.Mutex
	seen []heldPublish
}

// heldPublish is one publish as it reached the stream.
type heldPublish struct {
	event    string
	at       time.Time
	deadline time.Time
	bounded  bool
	err      error
}

func (s *heldPublishes) Publish(ctx context.Context, _ string, ev *events.Event) error {
	p := heldPublish{event: ev.Type, at: time.Now(), err: ctx.Err()}
	p.deadline, p.bounded = ctx.Deadline()
	s.mu.Lock()
	s.seen = append(s.seen, p)
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.hold):
		return nil
	}
}

func (s *heldPublishes) asked() []heldPublish {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
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

// AN ADMISSION IS WITHDRAWN ON A SHARE NO GIVE-BACK BEFORE IT CAN SPEND.
//
// Every lease a stop gives back falls back to lapsing on its TTL, which is
// what lets them share one allowance; an admission has no TTL, so one a stop
// misses stays until an operator excludes the node. Here a store that stopped
// answering while the leases were given back — spending every moment of their
// allowance — and answers again by the time the admission is withdrawn: the
// withdrawal lands. And the share is CARVED OUT of the stop allowance rather
// than added beside it, so a stop against a store that never answers still
// spends one allowance in total.
func TestAnAdmissionIsWithdrawnOnAShareNoGiveBackCanSpend(t *testing.T) {
	t.Parallel()
	const ttl = 3 * time.Second
	fleet := expiringFleet{memFleet: coordmemory.NewFleet()}
	e := &Engine{
		backends: &Backends{Fleet: fleet},
		mode:     statelog.ModeNormal,
		id:       "node-0", incarnation: "node-0:first",
		leaseTTL: ttl,
	}
	if err := fleet.PutAdmission(t.Context(), coord.Admission{
		NodeID: e.id, Incarnation: e.incarnation, At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutAdmission: %v", err)
	}

	ctx := seat.WithStopBudget(context.Background(), e.stopping())
	// THE GIVE-BACKS' WHOLE ALLOWANCE, spent by a step that waited out a
	// store answering nothing.
	spent, done := seat.StopStep(ctx)
	<-spent.Done()
	done()
	if err := e.withdraw(ctx); err != nil {
		t.Fatalf("withdraw after the give-backs spent their allowance: %v — the "+
			"admission has no TTL, and an operator now has to exclude this node", err)
	}
	admissions, err := fleet.Admissions(t.Context())
	if err != nil {
		t.Fatalf("Admissions: %v", err)
	}
	if len(admissions) != 0 {
		t.Fatalf("admissions = %+v after the withdrawal, want none", admissions)
	}

	// ONE ALLOWANCE IN TOTAL: what a fresh stop's give-backs get, and the
	// admission's share beside it. Never more — a share added beside the
	// allowance is a third over it — and less only by the moment between
	// beginning the step and reading the clock, which a quarter of the
	// allowance covers on any machine.
	fresh := &Engine{leaseTTL: ttl}
	step, end := seat.StopStep(seat.WithStopBudget(context.Background(), fresh.stopping()))
	defer end()
	deadline, _ := step.Deadline()
	total := time.Until(deadline) + fresh.admissionShare()
	if want := seat.StopAllowance(ttl); total > want || total < want-want/4 {
		t.Fatalf("the give-backs' budget and the admission's share come to %v, "+
			"want the one stop allowance of %v", total, want)
	}
}

// expiringFleet is the in-memory fleet with the one property every real store
// has and the twin does not need: a request on a context that has ended fails.
// Embedded through [memFleet], as [failingFleet] is.
type expiringFleet struct{ *memFleet }

func (f expiringFleet) ForgetAdmission(ctx context.Context, nodeID, incarnation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.memFleet.ForgetAdmission(ctx, nodeID, incarnation)
}
