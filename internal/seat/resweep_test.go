package seat

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/seat/placement"
)

// changingSeats is a seat set a case changes under a running host, counting
// the sweeps that read it: a pass reads the set once, and nothing else on the
// host's loops does.
type changingSeats struct {
	mu      sync.Mutex
	handles []string
	reads   atomic.Int64
}

func (c *changingSeats) seats() []placement.Seat {
	c.reads.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]placement.Seat, 0, len(c.handles))
	for _, h := range c.handles {
		out = append(out, testSeat(h))
	}
	return out
}

func (c *changingSeats) add(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handles = append(c.handles, handle)
}

// waitHolds polls the host until it holds handle, on the wall clock: the pass
// it waits for runs on the host's own loop.
func waitHolds(t *testing.T, h *Host, handle string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(h.Held(), handle) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited 10s for the host to claim %s; it holds %v", handle, h.Held())
}

// A SEAT THE COMPANY GAINS IS CLAIMED AT ONCE WHEN THE HOST IS ASKED.
//
// An apply is the only thing that changes the seat set, and it knows when it
// has: a role a revision added, or a node's whole first company, sat unclaimed
// until the sweep's next tick noticed — up to [SweepInterval] of seats nobody
// ran, on every apply. The interval here is an hour, so only the ask can be
// what claims the seat.
func TestResweepClaimsANewSeatWithoutWaitingForTheTick(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	set := &changingSeats{}
	h := f.newHost("node-a", Config{
		Seats: set.seats, SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	h.Start(f.ctx)
	t.Cleanup(func() { h.Stop(f.ctx) })
	wantHeld(t, h)

	set.add("ceo")
	h.Resweep()
	waitHolds(t, h, "ceo")
}

// AN ASK MADE BEFORE THE HOST RUNS IS ANSWERED BY ITS FIRST PASS, NOT BY ONE
// OF ITS OWN.
//
// An apply can reach the host while the engine around it is still being built,
// before [Host.Start], whose first pass reads the seats as they are by then.
// Kept for the loop, that ask was a second pass straight after the first with
// nothing new to see — and it spent the one ask an interval honours, so the
// first apply after the boot, the one that did change the seats, waited for
// the tick.
func TestAResweepBeforeTheHostRunsIsDropped(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	set := &changingSeats{}
	h := f.newHost("node-a", Config{
		Seats: set.seats, SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	set.add("ceo")
	h.Resweep()
	h.Start(f.ctx)
	t.Cleanup(func() { h.Stop(f.ctx) })
	wantHeld(t, h, "ceo")
	// The loop is given time to misbehave, as above.
	before := set.reads.Load()
	time.Sleep(300 * time.Millisecond)
	if got := set.reads.Load(); got != before {
		t.Fatalf("an ask made before the host ran swept %d more time(s) once "+
			"it started, after the first pass had answered it", got-before)
	}
	set.add("eng")
	h.Resweep()
	waitHolds(t, h, "eng")
}

// AN ASK INSIDE THE INTERVAL OF THE LAST ONE HONOURED WAITS FOR THE TICK.
//
// The per-sweep claim limit bounds the rate a node spawns seats' MCP children
// at only while passes stay an interval apart, and applies can arrive faster
// than that. So one ask per interval brings a pass forward and the rest wait
// for the tick, which bounds a node to two passes' claims in any interval.
func TestAResweepInsideTheIntervalWaitsForTheTick(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	set := &changingSeats{}
	h := f.newHost("node-a", Config{
		Seats: set.seats, SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	h.Start(f.ctx)
	t.Cleanup(func() { h.Stop(f.ctx) })

	set.add("ceo")
	h.Resweep()
	waitHolds(t, h, "ceo")
	set.add("eng")
	h.Resweep()
	// The loop is given time to misbehave: a pass reads the seat set, and
	// none may run for an ask inside the interval of the one before.
	before := set.reads.Load()
	time.Sleep(300 * time.Millisecond)
	if got := set.reads.Load(); got != before {
		t.Fatalf("a second ask inside the interval ran %d pass(es) at once; "+
			"it holds %v", got-before, h.Held())
	}
	wantHeld(t, h, "ceo")
}

// AN ASKED-FOR PASS STANDS IN FOR THE TICK RATHER THAN ADDING ONE.
//
// The interval starts again from the pass an ask brought forward, so the tick
// that was due inside it does not come as well: two seconds here, asked at one
// second, and nothing may sweep again before the third.
func TestAnAskedForPassRestartsTheInterval(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	set := &changingSeats{}
	const every = 2 * time.Second
	h := f.newHost("node-a", Config{
		Seats: set.seats, SweepInterval: every, HeartbeatInterval: time.Hour,
	})
	started := time.Now()
	h.Start(f.ctx)
	t.Cleanup(func() { h.Stop(f.ctx) })

	time.Sleep(every / 2)
	set.add("ceo")
	h.Resweep()
	waitHolds(t, h, "ceo")
	asked := set.reads.Load()
	// Until well past where the boot's tick was due and well short of a full
	// interval after the ask, with half a second of slack on each side.
	time.Sleep(time.Until(started.Add(every + every/4)))
	if got := set.reads.Load(); got != asked {
		t.Fatalf("%d pass(es) ran between the asked-for pass and a full "+
			"interval after it: the tick it stood in for came as well",
			got-asked)
	}
}

// AN ASK WHOSE PASS THE CLAIM GATE WITHHELD SPENDS NOTHING OF THE INTERVAL.
//
// The gate's owner asks for a pass the moment the gate opens ([Config.Ready]),
// and an ask honoured a moment before — an apply's, made while the node was
// still catching up with its logs — had claimed nothing: counted against the
// interval, it left the ask that would have claimed to wait for the tick, an
// hour away here. The second ask comes well inside the interval of the first,
// so only the rule can be what honours it.
func TestAnAskTheGateWithheldDoesNotSpendTheInterval(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	set := &changingSeats{}
	gate := &countedGate{}
	h := f.newHost("node-a", Config{
		Seats: set.seats, SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
		Ready: gate.ready,
	})
	h.Start(f.ctx)
	t.Cleanup(func() { h.Stop(f.ctx) })

	set.add("ceo")
	h.Resweep()
	// THE ASKED PASS HAS DECIDED once it has asked the gate, after the
	// start's own pass did: a pass that asked it and was refused claims
	// nothing, whenever it finishes.
	gate.waitAsked(t, 2)
	gate.open.Store(true)
	h.Resweep()
	waitHolds(t, h, "ceo")
}

// AN ASK WHOSE WITHHELD PASS GAVE A SEAT BACK SPENDS THE INTERVAL ALL THE SAME.
//
// The gate withholds claims and never give-backs — a node behind its logs
// still sheds what it holds past its share — and give-backs are rate-limited
// per pass like claims. So only a pass that changed NOTHING is free: one that
// shed a seat counts, and the next ask inside its interval waits for the tick.
func TestAWithheldAskThatGaveASeatBackSpendsTheInterval(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	setA := &changingSeats{handles: []string{"ceo", "eng"}}
	gate := &countedGate{}
	gate.open.Store(true)
	a := f.newHost("node-a", Config{
		Seats: setA.seats, SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
		Ready: gate.ready,
	})
	a.Start(f.ctx)
	t.Cleanup(func() { a.Stop(f.ctx) })
	wantHeld(t, a, "ceo", "eng")

	// A PEER JOINS, so node-a's share is one and its next pass sheds one —
	// with the gate closed, which withholds the claims and not the shed.
	gate.open.Store(false)
	setB := &changingSeats{handles: []string{"ceo", "eng"}}
	b := f.newHost("node-b", Config{
		Seats: setB.seats, SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	b.Start(f.ctx)
	t.Cleanup(func() { b.Stop(f.ctx) })

	a.Resweep()
	last := waitWithheld(t, a, "the pass the first ask brought forward")
	if len(last.Lost) != 1 {
		t.Fatalf("the premise: the first asked pass gave one seat back, got %+v", last)
	}

	shed := setA.reads.Load()
	a.Resweep()
	// The loop is given time to misbehave, as the cases above give it.
	time.Sleep(300 * time.Millisecond)
	if got := setA.reads.Load(); got != shed {
		t.Fatalf("an ask inside the interval of a pass that gave a seat back "+
			"ran %d pass(es) at once", got-shed)
	}
}

// countedGate is a claim gate a case opens, counting how often it was asked.
type countedGate struct {
	open  atomic.Bool
	asked atomic.Int64
}

// ready is the gate as [Config.Ready]: the answer is read before the ask is
// counted, so a count a case has seen is an answer already given.
func (g *countedGate) ready(context.Context) bool {
	admits := g.open.Load()
	g.asked.Add(1)
	return admits
}

// waitAsked waits until the gate has been asked n times.
func (g *countedGate) waitAsked(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if g.asked.Load() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited 10s for the gate to be asked %d times; asked %d", n, g.asked.Load())
}

// waitWithheld waits until the host's last pass is one the gate withheld, and
// returns it.
func waitWithheld(t *testing.T, h *Host, what string) SweepResult {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if last, ok := h.LastSweep(); ok && last.Withheld {
			return last
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited 10s for %s to be withheld", what)
	return SweepResult{}
}
