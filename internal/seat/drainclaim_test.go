package seat

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// A SWEEP BEGUN BEFORE THE DRAIN CLAIMS NOTHING AFTER IT.
//
// A pass reads whether its host is draining once, near its start, and decides
// to claim from that. A drain begun after the read hands every seat back —
// each under that seat's own lock — and a pass that went on to claim waited on
// the lock and took straight back the seat the drain had just let go, onto a
// node that is leaving: attached after the drain had quiesced every seat, held
// through the teardown, and left to lapse on its TTL when the stop cancelled
// the pass mid-acquire. Here the pass is held at its readiness gate, after its
// read, while the drain begins and gives both seats back: it asks the store
// for neither seat again.
func TestASweepBegunBeforeTheDrainClaimsNothingAfterIt(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	hooks := &hookLog{}
	store := &countedAcquires{Backend: f.store}
	var hold atomic.Bool
	atGate, proceed := make(chan struct{}), make(chan struct{})
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo", "eng"), Hooks: hooks, Backend: store,
		Ready: func(context.Context) bool {
			if hold.CompareAndSwap(true, false) {
				close(atGate)
				<-proceed
			}
			return true
		},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(f.ctx)
	wantHeld(t, h, "ceo", "eng")

	hold.Store(true)
	swept := make(chan SweepResult, 1)
	go func() { swept <- h.Sweep(f.ctx) }()
	<-atGate
	h.BeginDrain(f.ctx)
	h.ReleaseAll(f.ctx, ReasonDrain)
	store.counting.Store(true)
	close(proceed)
	result := <-swept

	wantHeld(t, h)
	if len(result.Claimed) != 0 {
		t.Errorf("a pass begun before the drain claimed %v after it", result.Claimed)
	}
	if n := store.seats.Load(); n != 0 {
		t.Errorf("a pass begun before the drain asked the store for %d seat(s) "+
			"after the drain had given them back", n)
	}
	for _, handle := range []string{"ceo", "eng"} {
		if lease := f.leaseOf(coord.SeatResource(handle)); lease != nil {
			t.Errorf("seat %s is leased to %s after the drain gave it back",
				handle, lease.Owner)
		}
	}
}

// A DRAIN BEGUN WHILE A SEAT IS BEING TAKEN GETS THE SEAT BACK.
//
// The pass's own check comes before the acquire, which is a round trip; a
// drain that begins during it reads a held set the seat is not in yet, so the
// drain's release cannot give it back. Whether the seat is entered is decided
// under the lock the drain sets its flag under: here the drain begins while
// the acquire is in flight, and the lease the acquire returns goes straight
// back, with no hook run on it.
func TestADrainBegunWhileASeatIsBeingTakenGetsTheSeatBack(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	hooks := &hookLog{}
	store := &heldAcquires{Backend: f.store, at: make(chan struct{}), proceed: make(chan struct{})}
	h := f.newHost("node-a", Config{Seats: seatsNamed("ceo"), Hooks: hooks, Backend: store})
	h.renewNodePresence(f.ctx)

	store.armed.Store(true)
	swept := make(chan SweepResult, 1)
	go func() { swept <- h.Sweep(f.ctx) }()
	<-store.at
	h.BeginDrain(f.ctx)
	h.ReleaseAll(f.ctx, ReasonDrain)
	close(store.proceed)
	<-swept

	wantHeld(t, h)
	wantStrings(t, hooks.acquired(), nil, "acquire hooks run")
	if lease := f.leaseOf(coord.SeatResource("ceo")); lease != nil {
		t.Errorf("the seat taken as the drain began is leased to %s: it was "+
			"never given back", lease.Owner)
	}
}

// A SEAT WHOSE ACQUIRE FAILED AS ITS HOST STOPPED IS STILL GIVEN BACK.
//
// A stopping host cancels its pass, which is the commonest way an acquire hook
// fails mid-pass. The give-back that follows ran on that same cancelled
// context and released nothing, and the seat — already out of the held set
// the stop gives back — lapsed on its TTL instead.
func TestASeatWhoseAcquireFailedAsItsHostStoppedIsGivenBack(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	pass, stop := context.WithCancel(f.ctx)
	defer stop()
	hooks := &hookLog{
		block: func(string) { stop() },
		acquireErr: func(string, int) error {
			return errors.New("the host stopped while the seat was being prepared")
		},
	}
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), Hooks: hooks,
		Backend: refusesEndedContexts{Backend: f.store},
	})
	h.renewNodePresence(f.ctx)
	h.Sweep(pass)

	wantHeld(t, h)
	if lease := f.leaseOf(coord.SeatResource("ceo")); lease != nil {
		t.Errorf("the seat whose acquire failed is leased to %s: its give-back "+
			"ran on the cancelled pass and released nothing", lease.Owner)
	}
}

// countedAcquires is the lease store counting the seat acquires asked of it
// once counting is set.
type countedAcquires struct {
	coord.Backend
	counting atomic.Bool
	seats    atomic.Int64
}

func (s *countedAcquires) TryAcquire(ctx context.Context, resource string,
	opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {

	if coord.IsSeatResource(resource) && s.counting.Load() {
		s.seats.Add(1)
	}
	return s.Backend.TryAcquire(ctx, resource, opts)
}

// heldAcquires is the lease store with a seat acquire held, once armed, until
// proceed: at is closed when the held acquire has begun.
type heldAcquires struct {
	coord.Backend
	armed   atomic.Bool
	at      chan struct{}
	proceed chan struct{}
}

func (s *heldAcquires) TryAcquire(ctx context.Context, resource string,
	opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {

	if coord.IsSeatResource(resource) && s.armed.CompareAndSwap(true, false) {
		close(s.at)
		<-s.proceed
	}
	return s.Backend.TryAcquire(ctx, resource, opts)
}

// refusesEndedContexts is the in-memory lease store with the one property
// every real store has and the twin does not need: a release asked on a
// context that has ended fails.
type refusesEndedContexts struct{ coord.Backend }

func (s refusesEndedContexts) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.Backend.Release(ctx, resource, owner, epoch)
}
