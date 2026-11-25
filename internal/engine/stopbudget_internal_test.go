package engine

import (
	"context"
	"errors"
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
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A STOP'S LAST EVENTS SPEND NONE OF ITS ALLOWANCE, AND EACH HAS A BOUND OF
// ITS OWN.
//
// The node's announcement and each released seat's last lifecycle event are
// publishes on the event stream, which can lose quorum while the coordination
// store answers; an unacknowledged publish waits out whatever deadline it is
// handed, so charged to the allowance it spends what the leases need. Here the
// stream holds each past the whole allowance: each must be published on the
// client's own request timeout, free of its caller's cancellation, and the
// allowance is whole afterwards.
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

// unarmedGiveBacks is how many give-backs e holds in flight that are not yet a
// step of the stop.
func unarmedGiveBacks(e *Engine) int {
	e.giveBacks.mu.Lock()
	defer e.giveBacks.mu.Unlock()
	n := 0
	for g := range e.giveBacks.inFlight {
		if g.end == nil {
			n++
		}
	}
	return n
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

// A HOLD GIVEN BACK ONCE THE TEARDOWN HAS BEGUN IS A STEP OF THE STOP'S
// ALLOWANCE; ONE GIVEN BACK BEFORE IT, THE DRAIN'S WAIT INCLUDED, IS NOT.
//
// The integration loop's pass gives its hold back on the context it was taken
// on ([holdLeases]). Against a store stalling each release four allowances, it
// waits the stall out before any stop and during the drain; in flight as the
// TEARDOWN begins, or begun after it, it is cut by the allowance counted from
// the teardown's start. Each claim is what ended the release and the deadline
// its cause names, never a duration. An Engine literal, its TTL below
// [config.MinLeaseTTL], which New refuses: at the floor it would wait 40 s.
//
// Mutation: begin the in-flight step when a watcher of the signal wakes.
func TestAHoldGivenBackDuringTheTeardownIsAStepOfItsAllowance(t *testing.T) {
	t.Parallel()
	const ttl = 1500 * time.Millisecond
	if ttl >= config.MinLeaseTTL {
		t.Fatalf("the premise: %v is no shorter than the %v floor a booted engine "+
			"is held to, so this case could build its engine through New", ttl, config.MinLeaseTTL)
	}
	slow := &slowReleases{Backend: coordmemory.New(), entered: make(chan struct{}, 1)}
	e := &Engine{
		backends: &Backends{Coord: slow},
		id:       "node-0", incarnation: "node-0:first",
		leaseTTL: ttl,
	}
	allowance := seat.StopAllowance(ttl)
	budget := allowance - e.admissionShare()
	stall := 4 * allowance
	hold := e.workerHold("hold-probe", time.Minute)
	held := func() func() {
		t.Helper()
		release, ok, err := hold(t.Context())
		if err != nil || !ok {
			t.Fatalf("hold = (%v, %v), want it held", ok, err)
		}
		return release
	}
	giveBack := func() stalledRelease {
		t.Helper()
		release := held()
		slow.stall.Store(int64(stall))
		defer slow.stall.Store(0)
		release()
		return slow.lastStalled()
	}

	if r := giveBack(); r.cut {
		t.Fatalf("before any stop, a hold's give-back was cut (%v) inside the %v its store "+
			"took: a pass ending on its own was charged to a stop", r.cause, stall)
	}

	// THE DRAIN'S ALLOWANCE, as [Engine.Drain] builds it at its first line.
	drain := e.stopping()
	if r := giveBack(); r.cut {
		t.Fatalf("while the drain waited, a hold's give-back was cut (%v) inside the %v its "+
			"store took: a pass ending on its own during the drain was charged to the stop",
			r.cause, stall)
	}
	asked := time.Now()
	step, done := seat.StopStep(seat.WithStopBudget(context.Background(), drain))
	deadline, _ := step.Deadline()
	done()
	if left := deadline.Sub(asked); left < budget {
		t.Fatalf("after a pass gave its hold back during the drain, a seat's give-back "+
			"has %v of the stop's %v: the pass spent what the seats are owed", left, budget)
	}

	// IN FLIGHT ACROSS THE TEARDOWN'S START: begun during the drain, against
	// the stalled store, and the teardown begins while it waits. Its step is
	// begun by the teardown's call, so its deadline is inside the allowance
	// from the call's return.
	select {
	case <-slow.entered:
	default:
	}
	release := held()
	slow.stall.Store(int64(stall))
	returned := make(chan stalledRelease, 1)
	go func() { release(); returned <- slow.lastStalled() }()
	<-slow.entered
	e.tearingDown(t.Context())
	called := time.Now()
	if n := unarmedGiveBacks(e); n > 0 {
		t.Fatalf("%d give-back(s) in flight were no step of the stop when the teardown's call "+
			"returned: a step begun later is cut that much past the allowance", n)
	}
	r := <-returned
	slow.stall.Store(0)
	var spent *stopAllowanceSpent
	if !r.cut || !errors.As(r.cause, &spent) {
		t.Fatalf("a hold's give-back in flight when the teardown began ended %+v, want it cut "+
			"by the stop's allowance: the teardown waited out its store", r)
	}
	if late := spent.deadline.Sub(called); late > budget {
		t.Fatalf("a hold's give-back in flight when the teardown began was given until %v "+
			"past the teardown's call, against an allowance of %v: its step began after the "+
			"teardown did", late, budget)
	}

	// BEGUN AFTER IT, the give-back is a step from its own start, and the
	// in-flight one spent the allowance.
	began := time.Now()
	if r := giveBack(); !r.cut || !errors.As(r.cause, &spent) {
		t.Fatalf("once the teardown had begun, a hold's give-back ended %+v, want it cut by "+
			"the stop's allowance", r)
	} else if late := spent.deadline.Sub(began); late > budget {
		t.Fatalf("once the teardown had begun, a hold's give-back was given %v, against an "+
			"allowance of %v", late, budget)
	}
}

// A SANDBOX'S TAKER LEASE IS GIVEN BACK ON THE STOP'S ONE ALLOWANCE.
//
// The sandbox coordinator gives its taker lease back as its stop ends, on the
// context it claimed it on, which no stop reaches; through the lease store the
// engine hands it ([holdLeases]) the give-back is a step of the stop. Here the
// allowance is already spent, so the release against a silent store must be
// cancelled by the stop at once, not cut by its own renewal interval's
// deadline. Told by what cut the call, as the store saw it, never by a clock.
// The TTL is below [config.MinLeaseTTL] for the hold case's reason above.
//
// Mutation: hand the coordinator the node's lease store as it is, and the
// release is cut by its own interval's deadline rather than by the stop.
func TestASandboxsTakerLeaseIsGivenBackOnTheStopsAllowance(t *testing.T) {
	t.Parallel()
	const ttl = 6 * time.Second
	if ttl >= config.MinLeaseTTL {
		t.Fatalf("the premise: %v is no shorter than the %v floor a booted engine is held to", ttl,
			config.MinLeaseTTL)
	}
	slow := &slowReleases{Backend: coordmemory.New(), entered: make(chan struct{}, 1)}
	e := &Engine{backends: &Backends{Queue: queuememory.New(), Coord: slow}, id: "node-a",
		incarnation: "node-a:1", leaseTTL: ttl, answers: testAnswers()}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		DefaultPlacement: sandbox.Direct,
		Runners:          map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rt, err := e.sandboxRuntimeOn(sandbox.NewCoordStore(coordmemory.NewFleet()), manager)
	if err != nil {
		t.Fatalf("sandboxRuntimeOn: %v", err)
	}
	e.useSandbox(rt.pending, rt.coordinator)
	if lease, err := slow.Get(t.Context(), coord.TakerResource("node-a:1")); err != nil || lease == nil {
		t.Fatalf("the premise: this process's taker lease is %+v (%v), want it held", lease, err)
	}

	// THE STOP'S ALLOWANCE, spent by the give-backs before the sandbox's.
	e.stopBudgetOnce.Do(func() { e.stopBudget = seat.NewStopBudget(0) })
	e.tearingDown(t.Context())
	every := ttl / coord.KeptRenewals
	slow.stall.Store(int64(4 * every))
	e.stopSandbox()
	// HOW THE STALLED RELEASE WAS CUT, as the store saw it, never a clock read:
	// a step of a spent allowance is cancelled by the stop at once, and a
	// release left to its own bound runs out its interval's deadline first —
	// the stall outlasts both.
	switch cut := slow.cutBy(); {
	case cut == nil:
		t.Fatal("the premise: the sandbox's stop gave its taker lease back on no call the stalled store cut")
	case !errors.Is(cut, context.Canceled):
		t.Fatalf("the sandbox's stop gave its taker lease back on a call cut by %v once the stop's allowance "+
			"was spent, want it cancelled by the stop: the release waited out its own %v bound beside the "+
			"allowance", cut, every)
	}
}

// slowReleases is a lease store that answers a release only after stall, or
// when the request's context ends first, signalling entered (when it has room)
// as a release begins stalling and keeping what cut the last one
// ([slowReleases.cutBy]).
type slowReleases struct {
	coord.Backend
	stall   atomic.Int64
	entered chan struct{}

	mu   sync.Mutex
	cut  error
	last stalledRelease
}

// stalledRelease is how a release the stall held ended: cut is whether its
// context ended first, and cause that context's cause where it did.
type stalledRelease struct {
	cut   bool
	cause error
}

func (s *slowReleases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	if d := time.Duration(s.stall.Load()); d > 0 {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cut = ctx.Err()
			s.last = stalledRelease{cut: true, cause: context.Cause(ctx)}
			s.mu.Unlock()
			return false, ctx.Err()
		case <-time.After(d):
			s.mu.Lock()
			s.last = stalledRelease{}
			s.mu.Unlock()
		}
	}
	return s.Backend.Release(ctx, resource, owner, epoch)
}

// cutBy is the error the context of the last release the stall held ended
// with — the stop's cancellation, or the release's own deadline — and nil where
// no stalled release was cut.
func (s *slowReleases) cutBy() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cut
}

// lastStalled is how the last release the stall held ended.
func (s *slowReleases) lastStalled() stalledRelease {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// AN ADMISSION IS WITHDRAWN ON A SHARE NO GIVE-BACK BEFORE IT CAN SPEND.
//
// A lease a stop misses lapses on its TTL; an admission has none, so one missed
// stays until an operator excludes the node. Here the store is silent while the
// leases are given back, spending their whole allowance, and answers again for
// the withdrawal, which lands. The share is CARVED OUT of the allowance, not
// added beside it, so a stop against a silent store still spends one allowance.
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

	// ONE ALLOWANCE IN TOTAL: a fresh stop's give-back budget plus the
	// admission's share, never more, and less only by the moment before the
	// clock is read, which a quarter of the allowance covers.
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
