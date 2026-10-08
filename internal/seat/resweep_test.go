package seat

import (
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
		out = append(out, placement.Seat{Handle: h})
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
