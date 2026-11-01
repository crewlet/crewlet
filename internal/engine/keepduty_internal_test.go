package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/schedule"
)

// fakeDutyClock is a [dutyClock] a test drives: Now is what the test set, each
// tick is one the test sends, having moved Now on by the interval, and a
// renewal's call ends once the test moves Now past its deadline.
type fakeDutyClock struct {
	// ticks is unbuffered, so a tick sent is one the renewal loop took —
	// and so one sent after another means the renewal the first started
	// has run to its end.
	ticks chan time.Time

	mu  sync.Mutex
	now time.Time
	// waits is what After was asked for, and bounds what WithTimeout was:
	// when each renewal is due, and how long each may take.
	waits, bounds []time.Duration
	// deadlines is every call bounded on this clock that has not ended.
	deadlines []fakeDeadline
}

// fakeDeadline is one call bounded on a [fakeDutyClock], ended once the clock
// reaches at.
type fakeDeadline struct {
	at  time.Time
	end context.CancelCauseFunc
}

func newFakeDutyClock() *fakeDutyClock {
	return &fakeDutyClock{ticks: make(chan time.Time), now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeDutyClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeDutyClock) After(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	return c.ticks, func() {}
}

func (c *fakeDutyClock) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	call, end := context.WithCancelCause(ctx)
	c.mu.Lock()
	c.bounds = append(c.bounds, d)
	if d > 0 {
		c.deadlines = append(c.deadlines, fakeDeadline{at: c.now.Add(d), end: end})
	}
	c.mu.Unlock()
	if d <= 0 {
		end(context.DeadlineExceeded)
	}
	return call, func() { end(context.Canceled) }
}

// advance moves the clock on by d, ending every call whose deadline it
// reaches.
func (c *fakeDutyClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []fakeDeadline
	c.deadlines = slices.DeleteFunc(c.deadlines, func(dl fakeDeadline) bool {
		if c.now.Before(dl.at) {
			return false
		}
		due = append(due, dl)
		return true
	})
	c.mu.Unlock()
	for _, dl := range due {
		dl.end(context.DeadlineExceeded)
	}
}

// asked is what the clock was asked for so far: the waits for each renewal,
// and the bounds on each renewal's call.
func (c *fakeDutyClock) asked() (waits, bounds []time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.waits), slices.Clone(c.bounds)
}

// awaitWaits waits until the renewal loop has asked for n waits.
func (c *fakeDutyClock) awaitWaits(t *testing.T, n int) {
	t.Helper()
	for give := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if waits, _ := c.asked(); len(waits) >= n {
			return
		}
		if time.Now().After(give) {
			t.Fatalf("the renewal loop never asked for wait %d", n)
		}
	}
}

// tickWithin moves the clock on by d and offers the renewal loop a tick,
// reporting whether it took it within wait.
func (c *fakeDutyClock) tickWithin(d, wait time.Duration) bool {
	c.advance(d)
	select {
	case c.ticks <- c.Now():
		return true
	case <-time.After(wait):
		return false
	}
}

// tick is tickWithin, failing the test when the loop does not take the tick.
func (c *fakeDutyClock) tick(t *testing.T, d time.Duration) {
	t.Helper()
	if !c.tickWithin(d, 10*time.Second) {
		t.Fatal("the renewal loop took no tick: it had stopped renewing")
	}
}

// tickToTheEnd moves the clock on by d to where the window ends, and offers
// the loop a tick until the work ends — ended is closed then. EITHER renewal
// may notice the end: the one whose tick this is, or the renewal before it,
// which looks at the clock again once its unanswered call returns — and the
// test moves the clock as soon as that call has answered, while it is still
// returning. Both end the work with the same cause and neither asks the
// store, so only the end is waited for.
func (c *fakeDutyClock) tickToTheEnd(t *testing.T, d time.Duration, ended <-chan struct{}) {
	t.Helper()
	c.advance(d)
	select {
	case c.ticks <- c.Now():
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the renewal loop neither took a tick nor ended the work")
	}
}

// answer is one claim's answer.
type answer struct {
	held bool
	err  error
}

// scriptedClaim is a claim the test watches: each call answers the next of
// answers — or what next does, once they run out — and is reported on
// answered, so a test that ticked can wait for that renewal's answer.
type scriptedClaim struct {
	answers  []answer
	next     schedule.DutyFunc
	answered chan answer

	mu    sync.Mutex
	calls int
}

func newScriptedClaim(answers ...answer) *scriptedClaim {
	return &scriptedClaim{answers: answers, answered: make(chan answer, 64)}
}

func (s *scriptedClaim) claim(ctx context.Context) (bool, error) {
	s.mu.Lock()
	n := s.calls
	s.calls++
	s.mu.Unlock()
	a := answer{held: true}
	switch {
	case n < len(s.answers):
		a = s.answers[n]
	case s.next != nil:
		a.held, a.err = s.next(ctx)
	}
	s.answered <- a
	return a.held, a.err
}

func (s *scriptedClaim) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// await is the next answer the claim gave.
func (s *scriptedClaim) await(t *testing.T) answer {
	t.Helper()
	select {
	case a := <-s.answered:
		return a
	case <-time.After(10 * time.Second):
		t.Fatal("no renewal was asked")
		return answer{}
	}
}

// ended waits for work to end, failing the test if it does not.
func ended(work context.Context, t *testing.T) {
	t.Helper()
	select {
	case <-work.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the work was never ended")
	}
}

// alive fails the test if work has ended.
func alive(work context.Context, t *testing.T, when string) {
	t.Helper()
	if err := work.Err(); err != nil {
		t.Fatalf("the work was ended %s: %v", when, context.Cause(work))
	}
}

var unanswered = answer{err: coord.ErrUnavailable}

// A KEPT DUTY IS NEVER TAKEN WHILE ITS WORK RUNS, for any number of leases:
// every renewal extends the lease a peer would need to have lapsed — and the
// renewals are all that keeps it, since a peer takes the duty one lease after
// they stop.
func TestAKeptDutyIsNeverTakenWhileItsWorkRuns(t *testing.T) {
	t.Parallel()
	clk := newFakeDutyClock()
	store := &coordmemory.Backend{Clock: clk.Now}
	const ttl = 3 * time.Minute
	mine := schedule.ClaimNamedDuty(store, "x", "a:1", "a", ttl)
	peer := schedule.ClaimNamedDuty(store, "x", "b:1", "b", ttl)
	renewal := dutyRenewal{duty: "x", ttl: ttl, clock: clk}

	asked := clk.Now()
	if held, err := mine(t.Context()); !held || err != nil {
		t.Fatalf("the premise: the first claim answered %v, %v", held, err)
	}
	watched := newScriptedClaim()
	watched.next = mine
	work, kept := renewal.keep(t.Context(), watched.claim, asked)
	for i := range 20 { // fifteen minutes: five leases
		clk.tick(t, renewal.every())
		if a := watched.await(t); !a.held || a.err != nil {
			t.Fatalf("renewal %d answered %v, %v", i+1, a.held, a.err)
		}
		if held, err := peer(t.Context()); held || err != nil {
			t.Fatalf("a peer claimed the duty after renewal %d (%v, %v)", i+1, held, err)
		}
	}
	alive(work, t, "while every renewal was confirmed")
	if held, err := kept.Stop(); !held || err != nil {
		t.Fatalf("Stop = %v, %v; want held", held, err)
	}
	clk.advance(ttl + time.Second)
	if held, err := peer(t.Context()); !held || err != nil {
		t.Fatalf("a peer could not claim the duty a lease after the renewals stopped (%v, %v)", held, err)
	}
}

// A RENEWAL ANSWERING THAT ANOTHER NODE HOLDS THE DUTY ENDS THE WORK AT ONCE,
// and the renewals with it: that node runs its own.
func TestARefusedRenewalStopsTheWorkAtOnce(t *testing.T) {
	t.Parallel()
	clk := newFakeDutyClock()
	renewal := dutyRenewal{duty: "x", ttl: 3 * time.Minute, clock: clk}
	claim := newScriptedClaim(answer{held: false})
	work, kept := renewal.keep(t.Context(), claim.claim, clk.Now())
	clk.tick(t, renewal.every())
	claim.await(t)
	ended(work, t)
	if cause := context.Cause(work); !errors.Is(cause, errDutyLost) {
		t.Fatalf("the work ended with %v, want %v", cause, errDutyLost)
	}
	if held, err := kept.Stop(); held || err != nil {
		t.Fatalf("Stop = %v, %v; want definitively not held", held, err)
	}
	if clk.tickWithin(renewal.every(), 100*time.Millisecond) {
		t.Fatal("the renewals went on after the duty was lost")
	}
}

// A KEPT DUTY THIS NODE DECLINES ENDS ITS WORK UNRENEWED (ADR-0072): once a
// copy the work decides from has gone out of service, the renewal due next
// asks the store nothing, ends the work at once, and none is asked after it —
// renewed, the work would go on deciding from the copy that went wrong. Stop
// says DECLINED rather than lost or unconfirmed, because the lease is still
// this node's and its holder has to give it back.
func TestADeclinedKeptDutyEndsItsWorkUnrenewed(t *testing.T) {
	t.Parallel()
	clk := newFakeDutyClock()
	var declined atomic.Bool
	renewal := dutyRenewal{duty: "x", ttl: 4 * time.Minute, clock: clk}.declining(
		func(context.Context) bool { return declined.Load() })
	claim := newScriptedClaim()
	work, kept := renewal.keep(t.Context(), claim.claim, clk.Now())

	clk.tick(t, time.Minute)
	claim.await(t)
	alive(work, t, "while every copy it decides from served")
	declined.Store(true)
	clk.tick(t, time.Minute)
	ended(work, t)
	if cause := context.Cause(work); !errors.Is(cause, errDutyDeclined) {
		t.Fatalf("the work ended with %v, want %v", cause, errDutyDeclined)
	}
	if n := claim.count(); n != 1 {
		t.Fatalf("the lease was asked for %d times; want once, and never once the duty was declined", n)
	}
	if held, err := kept.Stop(); held || !errors.Is(err, errDutyDeclined) {
		t.Fatalf("Stop = %v, %v; want declined, for the holder to give the lease back", held, err)
	}
	if clk.tickWithin(time.Minute, 100*time.Millisecond) {
		t.Fatal("the renewals went on after the duty was declined")
	}
}

// A RENEWAL THE STORE DOES NOT ANSWER IS NEITHER: the work goes on while the
// last confirmation keeps every peer out, the next renewal is asked inside
// that window, and the work ends — without asking again — only once the
// window has run out with nothing confirming the duty, when a peer may hold
// it. Stop says which: not held, with the store's own error.
func TestAnUnansweredRenewalStopsTheWorkOnlyOnceItsLeaseMayHaveLapsed(t *testing.T) {
	t.Parallel()
	clk := newFakeDutyClock()
	renewal := dutyRenewal{duty: "x", ttl: 4 * time.Minute, clock: clk} // a renewal a minute
	claim := newScriptedClaim(unanswered, unanswered)
	work, kept := renewal.keep(t.Context(), claim.claim, clk.Now())

	clk.tick(t, time.Minute)
	claim.await(t)
	clk.tick(t, time.Minute) // taken: the first renewal has run to its end
	alive(work, t, "by one renewal the store did not answer")
	claim.await(t) // the second renewal, asked inside the window
	// Three minutes: the window a confirmation buys. Whichever renewal
	// notices it ends the work, so nothing here may assert the work alive.
	clk.tickToTheEnd(t, time.Minute, work.Done())
	ended(work, t)
	if n := claim.count(); n != 2 {
		t.Fatalf("the lease was asked for %d times; want two, and none once the window had run out", n)
	}
	if cause := context.Cause(work); !errors.Is(cause, errDutyUnconfirmed) {
		t.Fatalf("the work ended with %v, want %v", cause, errDutyUnconfirmed)
	}
	held, err := kept.Stop()
	if held || !errors.Is(err, errDutyUnconfirmed) || !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("Stop = %v, %v; want unknown, carrying the store's own error", held, err)
	}
}

// A RENEWAL THAT ANSWERS RESTORES THE WINDOW, measured from when IT was asked:
// one confirmation between failures is a full window again.
func TestAnAnsweredRenewalRestoresTheWindow(t *testing.T) {
	t.Parallel()
	clk := newFakeDutyClock()
	renewal := dutyRenewal{duty: "x", ttl: 4 * time.Minute, clock: clk}
	claim := newScriptedClaim(unanswered, answer{held: true}, unanswered, unanswered)
	work, kept := renewal.keep(t.Context(), claim.claim, clk.Now())

	for minute := 1; minute <= 4; minute++ {
		clk.tick(t, time.Minute)
		alive(work, t, "inside the window")
		claim.await(t)
	}
	// Confirmed at the second minute, so through the fifth.
	clk.tickToTheEnd(t, time.Minute, work.Done())
	ended(work, t)
	if n := claim.count(); n != 4 {
		t.Fatalf("the lease was asked for %d times, want 4", n)
	}
	if held, err := kept.Stop(); held || !errors.Is(err, errDutyUnconfirmed) {
		t.Fatalf("Stop = %v, %v; want unknown", held, err)
	}
}

// THE RENEWALS END WITH THE WORK: none is asked after Stop, a renewal in flight
// when the work ends is abandoned rather than waited out — and a failure it
// meets for that reason says nothing about the duty — and a node with no
// fleet runs no renewals at all.
func TestAKeptDutysRenewalsEndWithItsWork(t *testing.T) {
	t.Parallel()
	t.Run("stopped", func(t *testing.T) {
		t.Parallel()
		clk := newFakeDutyClock()
		renewal := dutyRenewal{duty: "x", ttl: 3 * time.Minute, clock: clk}
		claim := newScriptedClaim()
		_, kept := renewal.keep(t.Context(), claim.claim, clk.Now())
		clk.tick(t, renewal.every())
		claim.await(t)
		if held, err := kept.Stop(); !held || err != nil {
			t.Fatalf("Stop = %v, %v; want held", held, err)
		}
		if clk.tickWithin(renewal.every(), 100*time.Millisecond) || claim.count() != 1 {
			t.Fatalf("a renewal was asked after Stop (%d in all)", claim.count())
		}
		if held, err := kept.Stop(); !held || err != nil {
			t.Fatalf("Stop again = %v, %v; want the same answer", held, err)
		}
	})
	t.Run("a renewal in flight", func(t *testing.T) {
		t.Parallel()
		clk := newFakeDutyClock()
		renewal := dutyRenewal{duty: "x", ttl: 4 * time.Minute, clock: clk}
		asked := make(chan struct{})
		claim := func(ctx context.Context) (bool, error) {
			close(asked)
			<-ctx.Done()
			return false, ctx.Err()
		}
		parent, cancel := context.WithCancel(t.Context())
		work, kept := renewal.keep(parent, claim, clk.Now())
		clk.tick(t, time.Minute)
		<-asked
		cancel()
		ended(work, t)
		stopped := make(chan struct{})
		var (
			held bool
			err  error
		)
		go func() {
			defer close(stopped)
			held, err = kept.Stop()
		}()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Fatal("Stop waited on a renewal the work's end should have abandoned")
		}
		if !held || err != nil {
			t.Fatalf("Stop = %v, %v; a renewal abandoned because the work ended is no answer "+
				"about the duty, inside a window that had not run out", held, err)
		}
	})
	t.Run("no fleet", func(t *testing.T) {
		t.Parallel()
		clk := newFakeDutyClock()
		renewal := dutyRenewal{duty: "x", ttl: 3 * time.Minute, clock: clk}
		work, kept := renewal.keep(t.Context(), nil, clk.Now())
		alive(work, t, "on a node with no fleet")
		if held, err := kept.Stop(); !held || err != nil {
			t.Fatalf("Stop = %v, %v; a node with no fleet always holds its duties", held, err)
		}
		if waits, _ := clk.asked(); len(waits) != 0 {
			t.Fatal("a node with no fleet started renewing a lease nobody holds")
		}
	})
}

// A RENEWAL NEVER COVERS WORK PAST THE WINDOW IT WAS TRYING TO EXTEND: one due
// at the window's end is not asked — an answer then could not cover the work
// done while asking — and one asked inside it that answers only once the
// window has run out ends the work at once rather than at the next renewal,
// an interval late. Each renewal is run here by hand, on the test's goroutine
// (the loop is parked waiting for a tick nobody sends), so which instant it
// reads is the test's alone.
func TestARenewalNeverCoversWorkPastItsWindow(t *testing.T) {
	t.Parallel()
	const ttl = 4 * time.Minute // a renewal a minute: a window of three
	t.Run("due at its end", func(t *testing.T) {
		t.Parallel()
		clk := newFakeDutyClock()
		renewal := dutyRenewal{duty: "x", ttl: ttl, clock: clk}
		claim := newScriptedClaim()
		work, kept := renewal.keep(t.Context(), claim.claim, clk.Now())
		clk.advance(3 * time.Minute)
		if kept.renew(work, claim.claim) {
			t.Fatal("the renewals go on past the window the last confirmation bought")
		}
		if n := claim.count(); n != 0 {
			t.Fatalf("a renewal due at the window's end was asked (%d times)", n)
		}
		if cause := context.Cause(work); !errors.Is(cause, errDutyUnconfirmed) {
			t.Fatalf("the work ended with %v, want %v", cause, errDutyUnconfirmed)
		}
		if held, err := kept.Stop(); held || !errors.Is(err, errDutyUnconfirmed) {
			t.Fatalf("Stop = %v, %v; want unknown", held, err)
		}
	})
	t.Run("answered past its end", func(t *testing.T) {
		t.Parallel()
		clk := newFakeDutyClock()
		renewal := dutyRenewal{duty: "x", ttl: ttl, clock: clk}
		slow := func(context.Context) (bool, error) {
			clk.advance(2 * time.Minute) // asked inside the window, answered at its end
			return false, coord.ErrUnavailable
		}
		work, kept := renewal.keep(t.Context(), slow, clk.Now())
		clk.advance(time.Minute)
		if kept.renew(work, slow) {
			t.Fatal("a renewal answered past the window left the work running until the next one")
		}
		if cause := context.Cause(work); !errors.Is(cause, errDutyUnconfirmed) {
			t.Fatalf("the work ended with %v, want %v", cause, errDutyUnconfirmed)
		}
		if held, err := kept.Stop(); held || !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("Stop = %v, %v; want unknown, carrying the store's own error", held, err)
		}
	})
}

// holdOn is a claim the store holds on to — a request no member answers while
// the coordination stream's leader moves — until the call's own deadline ends
// it, for the calls numbered in hold (from zero), and held for the rest.
// holding has a value each time one is being held on to.
type holdOn struct {
	hold    map[int]bool
	holding chan struct{}

	mu    sync.Mutex
	calls int
}

func newHoldOn(hold ...int) *holdOn {
	h := &holdOn{hold: map[int]bool{}, holding: make(chan struct{}, 64)}
	for _, n := range hold {
		h.hold[n] = true
	}
	return h
}

func (h *holdOn) claim(ctx context.Context) (bool, error) {
	h.mu.Lock()
	n := h.calls
	h.calls++
	h.mu.Unlock()
	if !h.hold[n] {
		return true, nil
	}
	h.holding <- struct{}{}
	<-ctx.Done()
	return false, ctx.Err()
}

// held waits until a call is being held on to.
func (h *holdOn) held(t *testing.T) {
	t.Helper()
	select {
	case <-h.holding:
	case <-time.After(10 * time.Second):
		t.Fatal("no renewal was asked to be held on to")
	}
}

// A RENEWAL THE STORE HOLDS ON TO COSTS THE WORK NOTHING: its call gives up
// after one renewal interval, so the next is asked inside the window the last
// confirmation bought — straight away, being already due — and its answer
// keeps the work going past that window. Bounded by the window alone, the one
// call held the work until the window had run out, and the renewal after it
// was never asked: a coordination store's leader election ended the pass.
func TestARenewalTheStoreHoldsOnToCostsTheWorkNothing(t *testing.T) {
	t.Parallel()
	clk := newFakeDutyClock()
	renewal := dutyRenewal{duty: "x", ttl: 4 * time.Minute, clock: clk} // a renewal a minute: a window of three
	store := newHoldOn(0)
	claim := newScriptedClaim()
	claim.next = store.claim
	work, kept := renewal.keep(t.Context(), claim.claim, clk.Now())

	clk.tick(t, time.Minute)
	store.held(t)
	clk.advance(time.Minute) // the call's deadline: one interval, not the window's two
	if a := claim.await(t); a.err == nil {
		t.Fatalf("the renewal held on to answered %+v; want it to have given up", a)
	}
	clk.tick(t, 0) // the second renewal, already due
	if a := claim.await(t); !a.held || a.err != nil {
		t.Fatalf("the renewal after the one held on to answered %+v, want held", a)
	}
	clk.tick(t, time.Minute) // three minutes: the end of the first confirmation's window
	claim.await(t)
	alive(work, t, "past the first window, by a renewal confirmed inside it")
	if held, err := kept.Stop(); !held || err != nil {
		t.Fatalf("Stop = %v, %v; want held", held, err)
	}
	if _, bounds := clk.asked(); len(bounds) != 3 || slices.Max(bounds) != renewal.every() {
		t.Fatalf("the renewals' calls were bounded by %v; want three, none past an interval", bounds)
	}
}

// RENEWALS ARE PHASED FROM THE CONFIRMATION, never from when the work began:
// a claim that took half an interval to answer is renewed half an interval
// after it returned, and one that took longer than an interval is renewed at
// once — each then an interval after the one before. Phased from the work's
// start, a slow claim pushed every renewal later against a window measured
// from its asking, and the second fell past the window's end.
func TestRenewalsArePhasedFromTheConfirmation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		took   time.Duration
		renews time.Duration // after keep is called
	}{
		{"a claim half an interval long", 30 * time.Second, 30 * time.Second},
		{"a claim longer than an interval", 90 * time.Second, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			clk := newFakeDutyClock()
			renewal := dutyRenewal{duty: "x", ttl: 4 * time.Minute, clock: clk}
			claim := newScriptedClaim()
			confirmed := clk.Now()
			clk.advance(c.took)
			_, kept := renewal.keep(t.Context(), claim.claim, confirmed)
			defer func() { _, _ = kept.Stop() }()
			// EACH WAIT IS READ BEFORE THE CLOCK MOVES, so it is the one
			// the loop chose rather than what was left of it.
			clk.awaitWaits(t, 1)
			clk.tick(t, c.renews)
			claim.await(t)
			clk.awaitWaits(t, 2)
			clk.tick(t, time.Minute)
			claim.await(t)
			waits, _ := clk.asked()
			if want := []time.Duration{c.renews, time.Minute}; !slices.Equal(waits[:2], want) {
				t.Fatalf("the renewals waited %v, want %v", waits, want)
			}
		})
	}
}

// EVERY KEPT DUTY SURVIVES ONE RENEWAL THE STORE DID NOT ANSWER — refused, or
// held on to until its call gave up: at the TTL each is claimed with, no
// renewal's call is given more than an interval, and the renewal after an
// unanswered one is still asked inside the window, so a coordination store's
// leader election costs a half-hour audit nothing.
func TestEveryKeptDutyToleratesOneUnansweredRenewal(t *testing.T) {
	t.Parallel()
	for duty, ttl := range map[string]time.Duration{
		objectCollectorDuty: collectorDutyTTL,
		embedDutyName:       embedDutyTTL,
	} {
		t.Run(duty+", refused", func(t *testing.T) {
			t.Parallel()
			clk := newFakeDutyClock()
			renewal := newDutyRenewal(duty, ttl)
			renewal.clock = clk
			claim := newScriptedClaim(unanswered, answer{held: true})
			work, kept := renewal.keep(t.Context(), claim.claim, clk.Now())
			clk.tick(t, renewal.every())
			claim.await(t)
			clk.tick(t, renewal.every())
			claim.await(t)
			clk.tick(t, renewal.every())
			alive(work, t, "by one renewal the store did not answer")
			if held, err := kept.Stop(); !held || err != nil {
				t.Fatalf("Stop = %v, %v; want held", held, err)
			}
		})
		t.Run(duty+", held on to", func(t *testing.T) {
			t.Parallel()
			clk := newFakeDutyClock()
			renewal := newDutyRenewal(duty, ttl)
			renewal.clock = clk
			store := newHoldOn(0)
			claim := newScriptedClaim()
			claim.next = store.claim
			work, kept := renewal.keep(t.Context(), claim.claim, clk.Now())
			clk.tick(t, renewal.every())
			store.held(t)
			clk.advance(renewal.every())
			claim.await(t)
			clk.tick(t, 0)
			claim.await(t)
			clk.tick(t, renewal.every())
			claim.await(t)
			alive(work, t, "by one renewal the store held on to")
			if held, err := kept.Stop(); !held || err != nil {
				t.Fatalf("Stop = %v, %v; want held", held, err)
			}
			_, bounds := clk.asked()
			for i, b := range bounds {
				if b > renewal.every() {
					t.Errorf("renewal %d's call was given %v, past one renewal interval (%v)",
						i+1, b, renewal.every())
				}
			}
		})
	}
}
