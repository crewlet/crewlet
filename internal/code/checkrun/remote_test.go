package checkrun_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/checkrun"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE COMPANY'S REMOTE CELLS HOLD THE CONCURRENCY CAP: with
// code.checks.remote.max_running unset — sixteen — and seventeen e2b checks
// queued, two git nodes each FIRST in its own order, so both claim every
// attempt at once, run sixteen boxes between them and leave the seventeenth
// queued, claimed by nobody; one box ending frees its slot, and the
// seventeenth runs.
//
// Mutation: grant every slot whatever the live leases (Take ignores
// max_running) — seventeen boxes, red.
func TestRemoteCellsHoldTheConcurrencyCap(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.runner(f.options("git-a", 0, &order{nodes: []string{"git-a", "git-b"}}))
	f.runner(f.options("git-b", 0, &order{nodes: []string{"git-b", "git-a"}}))
	var keys []code.CheckKey
	for i := range config.CodeDefaultRemoteMaxRunning + 1 {
		keys = append(keys, f.queue(oidOf(fmt.Sprintf("%02x", i+1)), remote("lint", time.Minute)))
	}
	var boxes []*sandbox.FakeSandbox
	for range config.CodeDefaultRemoteMaxRunning {
		boxes = append(boxes, f.e2b.box(t))
	}
	time.Sleep(20 * fast.Walk)
	if n := f.e2b.count(); n != config.CodeDefaultRemoteMaxRunning {
		t.Fatalf("two git nodes made %d remote boxes under a cap of %d", n, config.CodeDefaultRemoteMaxRunning)
	}
	var waiting []code.CheckKey
	for _, key := range keys {
		if row, _ := f.row(key); row.State == code.CheckStateQueued {
			waiting = append(waiting, key)
		}
	}
	if len(waiting) != 1 {
		t.Fatalf("%d of %d remote checks wait queued, want exactly the one past the cap", len(waiting), len(keys))
	}
	if live := f.slots(); live != config.CodeDefaultRemoteMaxRunning {
		t.Fatalf("%d remote slots are leased for %d boxes", live, config.CodeDefaultRemoteMaxRunning)
	}

	f.e2b.end(boxes[0], 0, "")
	next := f.e2b.box(t)
	f.await(waiting[0], "ran once a box ended", func(r code.CheckRow) bool { return r.Box == next.ID() })
	for _, b := range append(boxes[1:], next) {
		f.e2b.end(b, 0, "")
	}
	for _, key := range keys {
		f.ended(key)
	}
	eventually(t, "every slot given back", func() bool { return f.slots() == 0 })
}

// THE DAILY BOX-TIME CAP HOLDS NEW CHECKS: under box_hours_per_day 1 — sixty
// box-minutes a UTC day — a remote box held for sixty minutes spends the day,
// each minute charged as it begins; a check queued then waits queued, claimed
// by nobody, while that box runs on to its own end and after it; and once the
// next UTC midnight comes it runs.
//
// Mutation: charge no minute after a box's first — the sixty-first is never
// refused, red; neither remember a refused minute nor heed one at a box's
// first — the second check runs at once, red. (Either alone is held by
// TestANodeThatHasNotSeenTheDaySpentStartsNoRemoteBox: here the node's own
// memory of the refusal holds the second check before its first minute.)
func TestTheDailyBoxTimeCapHoldsNewChecks(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.company.Code.Checks.MaxTimeoutSeconds = int(config.CodeCheckTimeoutCeiling / time.Second)
	f.company.Code.Checks.Remote.BoxHoursPerDay = new(1)
	// THE RUNNER'S CLOCK, an hour into the UTC day after today's, so every
	// instant it reads is past every attempt's real queuing.
	clock := &clock{at: time.Now().UTC().Truncate(24 * time.Hour).Add(25 * time.Hour)}
	minutes := &charges{Fleet: f.counter}
	o := f.options("git-a", 0, &order{nodes: []string{"git-a"}})
	o.Now, o.Minutes = clock.now, minutes
	f.runner(o)

	first := f.queue(oidOf("a"), remote("build", 3*time.Hour))
	box := f.e2b.box(t)
	f.await(first, "named its box", func(r code.CheckRow) bool { return r.Box == box.ID() })
	clock.advance(59 * time.Minute)
	eventually(t, "sixty minutes charged", func() bool { return minutes.allowed.Load() == 60 })
	if minutes.refused.Load() != 0 {
		t.Fatalf("a day of sixty minutes refused one of its first sixty")
	}
	clock.advance(2 * time.Minute)
	eventually(t, "the sixty-first minute refused", func() bool { return minutes.refused.Load() > 0 })
	if row, _ := f.row(first); row.State != code.CheckStateRunning {
		t.Fatalf("the box running when the day filled = %+v, want it running on", row)
	}

	second := f.queue(oidOf("b"), remote("build", 3*time.Hour))
	time.Sleep(20 * fast.Walk)
	f.e2b.end(box, 0, "")
	f.ended(first)
	time.Sleep(20 * fast.Walk)
	if row, _ := f.row(second); row.State != code.CheckStateQueued || f.e2b.count() != 1 {
		t.Fatalf("a check queued after the day's hours were spent = %+v with %d remote boxes, want it "+
			"queued and no box", row, f.e2b.count())
	}

	clock.set(clock.now().Truncate(24 * time.Hour).Add(24 * time.Hour))
	next := f.e2b.box(t)
	f.await(second, "ran the next UTC day", func(r code.CheckRow) bool { return r.Box == next.ID() })
	f.e2b.end(next, 0, "")
	f.ended(second)
	if minutes.bucket() != checkrun.RemoteMinutesBucket || minutes.window() != 24*time.Hour {
		t.Errorf("box-minutes were charged to %q in %s windows, want %q in a day's",
			minutes.bucket(), minutes.window(), checkrun.RemoteMinutesBucket)
	}
}

// A REMOTE BOX IS CHARGED EVERY MINUTE IT WAS HELD, the last ones included:
// a box that closes before a renewal reaches the minutes it began since the
// last one is charged them as its slot is given back, so a day's hours count
// a box's time from its slot's taking to its close.
//
// Mutation: charge nothing as the slot is released — two minutes of the
// box's three go uncharged, red.
func TestARemoteBoxIsChargedEveryMinuteItWasHeld(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	clock := &clock{at: time.Now().UTC().Truncate(24 * time.Hour).Add(25 * time.Hour)}
	minutes := &charges{Fleet: f.counter}
	o := f.options("git-a", 0, &order{nodes: []string{"git-a"}})
	// NO RENEWAL WITHIN THE CASE, so every minute after the first is one
	// only the release can charge.
	o.Timing.Renew = time.Hour
	o.Now, o.Minutes = clock.now, minutes
	f.runner(o)

	key := f.queue(oidOf("d"), remote("build", 3*time.Hour))
	box := f.e2b.box(t)
	f.await(key, "named its box", func(r code.CheckRow) bool { return r.Box == box.ID() })
	if n := minutes.allowed.Load(); n != 1 {
		t.Fatalf("a box just made was charged %d minutes, want its first", n)
	}
	clock.advance(2*time.Minute + 30*time.Second)
	f.e2b.end(box, 0, "")
	f.ended(key)
	eventually(t, "the slot given back", func() bool { return f.slots() == 0 })
	if n := minutes.allowed.Load(); n != 3 {
		t.Fatalf("a box held into its third minute was charged %d minutes, want 3", n)
	}
}

// AN UNREACHABLE STORE STARTS NO REMOTE BOX: while the coordination store
// cannot list or lease the slots, or cannot count a minute, a queued e2b check
// waits queued, claimed by nobody — a cap nobody can ask about cannot hold —
// and no slot is left leased behind the minute it could not count; once the
// store answers, the check runs.
//
// Mutation: read a store's error as room (Take answers a slot on one) — a
// box made while the store is away, red.
func TestAnUnreachableStoreStartsNoRemoteBox(t *testing.T) {
	t.Parallel()
	for name, down := range map[string]func(f *fleet, o *checkrun.Options, away *atomic.Bool){
		"the slots": func(f *fleet, o *checkrun.Options, away *atomic.Bool) {
			o.Leases = awayLeases{Backend: f.leases, away: away}
		},
		"the counter": func(f *fleet, o *checkrun.Options, away *atomic.Bool) {
			o.Minutes = awayCounter{Fleet: f.counter, away: away}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t)
			var away atomic.Bool
			away.Store(true)
			o := f.options("git-a", 0, &order{nodes: []string{"git-a"}})
			down(f, &o, &away)
			f.runner(o)
			key := f.queue(oidOf("c"), remote("lint", time.Minute))
			time.Sleep(4 * fast.Backstop)
			if row, _ := f.row(key); row.State != code.CheckStateQueued || f.e2b.count() != 0 {
				t.Fatalf("with %s away the check = %+v and %d remote boxes, want it queued and none", name, row,
					f.e2b.count())
			}
			if live := f.slots(); live != 0 {
				t.Fatalf("with %s away %d remote slots are left leased", name, live)
			}
			away.Store(false)
			box := f.e2b.box(t)
			f.await(key, "ran once the store answered", func(r code.CheckRow) bool { return r.Box == box.ID() })
			f.e2b.end(box, 0, "")
			f.ended(key)
		})
	}
}

// A NODE THAT HAS NOT SEEN THE DAY SPENT STARTS NO REMOTE BOX: a git node
// that never had a charge refused — a second git node, or a runner restarted
// mid-day — finds the UTC day's box-hours spent by another at its first box's
// first minute: the slot it took for that box is given back, no box is made
// and the check waits queued; and the refusal is remembered, so the node's
// later walks take no slot and ask the counter nothing more.
//
// Mutation: start a box whatever the first minute's charge answers — a box
// made on a spent day, red; forget a refused day between walks (daySpent
// answers false) — a slot taken and a minute refused every walk, red.
func TestANodeThatHasNotSeenTheDaySpentStartsNoRemoteBox(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.company.Code.Checks.Remote.BoxHoursPerDay = new(1)
	clock := &clock{at: time.Now().UTC().Truncate(24 * time.Hour).Add(25 * time.Hour)}
	// THE DAY SPENT BY ANOTHER NODE before this one's first walk: its sixty
	// minutes charged to the fleet's counter, not through this node.
	for range 60 {
		if ok, err := f.counter.Allow(t.Context(), checkrun.RemoteMinutesBucket, 60, checkrun.RemoteMinutesWindow,
			clock.now()); err != nil || !ok {
			t.Fatalf("spend the day's minutes: allowed %t, %v", ok, err)
		}
	}
	minutes := &charges{Fleet: f.counter}
	leases := &countedLeases{Leases: f.leases}
	o := f.options("git-b", 0, &order{nodes: []string{"git-b"}})
	o.Now, o.Minutes, o.Leases = clock.now, minutes, leases
	f.runner(o)

	key := f.queue(oidOf("9"), remote("build", time.Hour))
	time.Sleep(4 * fast.Backstop)
	if row, _ := f.row(key); row.State != code.CheckStateQueued || f.e2b.count() != 0 {
		t.Fatalf("a check on a day another node spent = %+v with %d remote boxes, want it queued and no box", row,
			f.e2b.count())
	}
	if live := f.slots(); live != 0 {
		t.Fatalf("%d remote slots are left leased on a spent day, want the one taken given back", live)
	}
	took, refused, allowed := leases.acquires.Load(), minutes.refused.Load(), minutes.allowed.Load()
	if took != 1 || refused != 1 || allowed != 0 {
		t.Fatalf("over its walks the node took %d slots and was refused %d minutes (allowed %d), want one slot "+
			"taken and one minute refused, then nothing asked: the spent day it found is remembered", took,
			refused, allowed)
	}
}

// A REMOTE BOX WHOSE SLOT LAPSED IS COUNTED AGAIN: a git node whose renewals
// did not reach the store for longer than a slot's TTL — a host asleep, a
// store away — has its running box's slot lapse, and another node fills the
// cap meanwhile. Once its node reaches the store again the box takes a slot
// again, past the cap, so the company counts it: no further box starts while
// it runs, even after the box that filled the cap ends, and the queued check
// runs only once the box over the cap has ended too.
//
// Mutation: leave a lost renewal as it is (no slot taken again) — the box is
// never counted again, red; take a slot again only under the cap — the cap
// is full, so it is never counted, red.
func TestARemoteBoxWhoseSlotLapsedIsCountedAgain(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.company.Code.Checks.Remote.MaxRunning = new(1)
	var away atomic.Bool
	a := f.options("git-a", 0, &order{nodes: []string{"git-a"}})
	a.Leases = awayLeases{Backend: f.leases, away: &away}
	f.runner(a)
	first := f.queue(oidOf("e"), remote("build", 3*time.Hour))
	boxA := f.e2b.box(t)
	f.await(first, "named its box", func(r code.CheckRow) bool { return r.Box == boxA.ID() })

	// GIT-A'S SLOT LAPSES, and its node can neither list nor claim: its
	// renewals still reach the store, which answers that the tenure is gone.
	away.Store(true)
	f.leases.Advance(checkrun.RemoteSlotTTL + time.Second)
	if live := f.slots(); live != 0 {
		t.Fatalf("%d remote slots live once git-a's lapsed, want none", live)
	}
	// GIT-B FILLS THE CAP meanwhile.
	f.runner(f.options("git-b", 0, &order{nodes: []string{"git-b"}}))
	second := f.queue(oidOf("f"), remote("build", 3*time.Hour))
	boxB := f.e2b.box(t)
	f.await(second, "named its box", func(r code.CheckRow) bool { return r.Box == boxB.ID() })

	away.Store(false)
	eventually(t, "git-a's box counted again", func() bool { return f.slots() == 2 })
	third := f.queue(oidOf("8"), remote("build", time.Hour))
	time.Sleep(4 * fast.Backstop)
	if row, _ := f.row(third); row.State != code.CheckStateQueued || f.e2b.count() != 2 {
		t.Fatalf("with two boxes running under a cap of one the third check = %+v with %d remote boxes, want it "+
			"queued", row, f.e2b.count())
	}
	f.e2b.end(boxB, 0, "")
	f.ended(second)
	time.Sleep(4 * fast.Backstop)
	if row, _ := f.row(third); row.State != code.CheckStateQueued || f.e2b.count() != 2 {
		t.Fatalf("with git-a's box still running under a cap of one the third check = %+v with %d remote boxes, "+
			"want it queued", row, f.e2b.count())
	}
	f.e2b.end(boxA, 0, "")
	f.ended(first)
	next := f.e2b.box(t)
	f.await(third, "ran once the box over the cap ended", func(r code.CheckRow) bool { return r.Box == next.ID() })
	f.e2b.end(next, 0, "")
	f.ended(third)
	eventually(t, "every slot given back", func() bool { return f.slots() == 0 })
}

// countedLeases is the company's leases, counting the listings and the
// claims a runner made of them.
type countedLeases struct {
	checkrun.Leases
	lists, acquires atomic.Int64
}

func (c *countedLeases) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	c.lists.Add(1)
	return c.Leases.ListLive(ctx, class)
}

func (c *countedLeases) TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease,
	coord.Refusal, error) {

	c.acquires.Add(1)
	return c.Leases.TryAcquire(ctx, resource, opts)
}

// remote is a check that runs in an e2b cell.
func remote(name string, timeout time.Duration) code.CheckDef {
	d := def(name, timeout)
	d.RunIn = code.CellE2B
	return d
}

// slots is how many remote slots the company's coordination store holds live.
func (f *fleet) slots() int {
	f.t.Helper()
	live, err := f.leases.ListLive(context.Background(), checkrun.RemoteSlotClass)
	if err != nil {
		f.t.Fatalf("list the remote slots: %v", err)
	}
	return len(live)
}

// eventually waits, a few seconds at most, for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// clock is a runner's clock a case moves.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// charges is the fleet's counter, counting the charges it allowed and
// refused, and naming the bucket and window it was last asked about.
type charges struct {
	*memory.Fleet
	allowed, refused atomic.Int64
	mu               sync.Mutex
	lastBucket       string
	lastWindow       time.Duration
}

func (c *charges) Allow(ctx context.Context, bucket string, limit int, window time.Duration,
	now time.Time) (bool, error) {

	ok, err := c.Fleet.Allow(ctx, bucket, limit, window, now)
	switch {
	case err != nil:
	case ok:
		c.allowed.Add(1)
	default:
		c.refused.Add(1)
	}
	c.mu.Lock()
	c.lastBucket, c.lastWindow = bucket, window
	c.mu.Unlock()
	return ok, err
}

func (c *charges) bucket() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastBucket
}

func (c *charges) window() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastWindow
}

// errAway is a coordination store that cannot be reached.
var errAway = errors.New("the coordination store is unreachable")

// awayLeases is the company's leases, unreachable while away holds.
type awayLeases struct {
	*memory.Backend
	away *atomic.Bool
}

func (a awayLeases) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	if a.away.Load() {
		return nil, errAway
	}
	return a.Backend.ListLive(ctx, class)
}

func (a awayLeases) TryAcquire(ctx context.Context, resource string, opts coord.AcquireOptions) (*coord.Lease,
	coord.Refusal, error) {

	if a.away.Load() {
		return nil, "", errAway
	}
	return a.Backend.TryAcquire(ctx, resource, opts)
}

// awayCounter is the fleet's counter, unreachable while away holds.
type awayCounter struct {
	*memory.Fleet
	away *atomic.Bool
}

func (a awayCounter) Allow(ctx context.Context, bucket string, limit int, window time.Duration,
	now time.Time) (bool, error) {

	if a.away.Load() {
		return false, errAway
	}
	return a.Fleet.Allow(ctx, bucket, limit, window, now)
}
