package gitproc_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE TRANSFER LANE'S FAIR SHARE (share.go), driven on a clock of each case's
// own through gitproc.NewShareLane — a lane of the shape the case names, its
// cores and its slots — or, where what New makes of a node is the subject,
// through the pool's own lane (gitproc.TransferLane): arrivals, settles and
// leaves, no git.

// fakeClock is a clock a case moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func machineShare(who string) gitproc.Share {
	return gitproc.Share{Class: gitproc.ShareMachine, Principal: who}
}

func personShare(who string) gitproc.Share {
	return gitproc.Share{Class: gitproc.SharePerson, Principal: who}
}

// quarter is what a held slot is charged a second, in core-seconds, on a lane
// of the default shape, TransfersPerProcess slots a core: a quarter of a core.
const quarter = 1.0 / gitproc.TransfersPerProcess

// occupied is what a slot held for d accrues at rate core-seconds a second,
// decayed to its end: each second's charge decayed over ShareWindow by the
// time d ends.
func occupied(rate float64, d time.Duration) float64 {
	w := gitproc.ShareWindow.Seconds()
	return rate * w * (1 - math.Exp(-d.Seconds()/w))
}

// decayed is a charge after d of decay.
func decayed(charge float64, d time.Duration) float64 {
	return charge * math.Exp(-d.Seconds()/gitproc.ShareWindow.Seconds())
}

// near reports a and b equal to within tol.
func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// A CONTENDED LANE IS SHARED BY CLASS, by weight: with persons, a machine and
// a coding box all always waiting, persons take four sevenths of what the lane
// spends, the machine two and the box one — however many principals each
// class has (eight persons, one machine credential, one run) — and the box's
// cap of half the lane, which its seventh never reaches, takes nothing from
// that.
//
// Mutation: next hands the slot to the lane's oldest waiter (FIFO), and the
// ten principals take turns — persons four fifths — red.
func TestAContendedTransferLaneSharesByClass(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	lane := gitproc.NewShareLane(1, 4, clk.now)
	machine := machineShare("ci")
	principals := []gitproc.Share{machine, runShare("box")}
	for i := range 8 {
		principals = append(principals, personShare(fmt.Sprintf("person%d", i)))
	}
	type held struct {
		turn  *gitproc.Turn
		class gitproc.ShareClass
		count bool
	}
	// The lane full of the machine's runs before anybody waits — not counted,
	// since nobody was choosing — then every principal always waiting.
	var running []held
	for range 4 {
		running = append(running, held{turn: lane.Arrive(machine), class: gitproc.ShareMachine})
	}
	queued := map[gitproc.Share]*gitproc.Turn{}
	for _, s := range principals {
		queued[s] = lane.Arrive(s)
	}
	charged := map[gitproc.ShareClass]float64{}
	for settled := 0; settled < 700; {
		// Each run holds its slot a second (four steps), a quarter of a
		// core-second of occupancy, and spends a whole core-second of CPU:
		// charged one core-second each.
		clk.advance(250 * time.Millisecond)
		h := running[0]
		running = running[1:]
		h.turn.Settle(time.Second)
		if h.count {
			charged[h.class]++
			settled++
		}
		granted := 0
		for s, turn := range queued {
			if turn.Granted() {
				granted++
				running = append(running, held{turn: turn, class: s.Class, count: true})
				queued[s] = lane.Arrive(s)
			}
		}
		if granted != 1 {
			t.Fatalf("a settle with ten principals waiting granted %d of them, want one", granted)
		}
	}
	for _, pair := range [][2]gitproc.ShareClass{
		{gitproc.SharePerson, gitproc.ShareMachine},
		{gitproc.ShareMachine, gitproc.ShareRun},
	} {
		ratio := charged[pair[0]] / charged[pair[1]]
		want := float64(pair[0].Weight()) / float64(pair[1].Weight())
		if math.Abs(ratio-want)/want > 0.05 {
			t.Errorf("%s was charged %v core-seconds and %s %v: %.3f to one, want %v to one within 5%%",
				pair[0], charged[pair[0]], pair[1], charged[pair[1]], ratio, want)
		}
	}
}

// BOXES HOLD AT MOST HALF THE TRANSFER LANE, the one class the share does not
// let fill it: on a lane of eight, sixty-four coding boxes' arrivals never run
// more than four at once, however many settle and arrive; a person arriving
// while the boxes hold their four takes a free slot at once, though boxes
// wait; and a machine with the boxes at their cap is granted the next slot a
// person releases, ahead of every box that waits — while the release of a box
// hands its slot to the next box. A slot is never free while a waiter that may
// take it waits.
//
// Mutation: Cap answering the whole lane for the run class — eight boxes run
// and the person waits — red.
func TestBoxesHoldAtMostHalfTheTransferLane(t *testing.T) {
	t.Parallel()
	const size = 8
	clk := newFakeClock()
	lane := gitproc.NewShareLane(2, size, clk.now)
	if got := gitproc.ShareRun.Cap(size); got != size/2 {
		t.Fatalf("the run class's cap of a lane of %d is %d, want %d", size, got, size/2)
	}
	var boxes []*gitproc.Turn
	for i := range 64 {
		boxes = append(boxes, lane.Arrive(runShare(fmt.Sprintf("launch-%02d", i))))
	}
	running := func() int {
		n := 0
		for _, b := range boxes {
			if b != nil && b.Granted() {
				n++
			}
		}
		return n
	}
	if n := running(); n != size/2 {
		t.Fatalf("64 boxes arrived at a lane of %d and %d run, want %d", size, n, size/2)
	}
	if held, waiting := lane.Counts(); held != size/2 || waiting != 60 {
		t.Fatalf("the lane holds %d and queues %d, want %d held and 60 boxes waiting", held, waiting, size/2)
	}

	// A PERSON TAKES A FREE SLOT AT ONCE beside the waiting boxes, as many as
	// the lane has free.
	var persons []*gitproc.Turn
	for i := range size / 2 {
		p := lane.Arrive(personShare(fmt.Sprintf("person%d", i)))
		if !p.Granted() {
			t.Fatalf("person %d arrived with %d slots free and the boxes at their cap, and waits", i,
				size/2-i)
		}
		persons = append(persons, p)
	}
	// A MACHINE, with every slot held, waits — and is granted the next
	// release of a person's slot ahead of all sixty boxes.
	machine := lane.Arrive(machineShare("ci"))
	if machine.Granted() {
		t.Fatal("a machine arrived at a full lane and was granted at once")
	}
	clk.advance(time.Second)
	persons[0].Settle(0)
	if !machine.Granted() {
		t.Fatal("a person's release with the boxes at their cap went to no machine waiting")
	}
	if n := running(); n != size/2 {
		t.Fatalf("a person's release left %d boxes running, want the %d at the cap", n, size/2)
	}
	// A PERSON'S RELEASE WITH ONLY BOXES WAITING frees its slot: none of
	// them may take it.
	persons[1].Settle(0)
	if n := running(); n != size/2 {
		t.Fatalf("a person's release with only capped boxes waiting started a box: %d run", n)
	}
	if held, _ := lane.Counts(); held != size-1 {
		t.Fatalf("the lane holds %d after a release no waiter could take, want %d", held, size-1)
	}
	// A BOX'S RELEASE hands its slot to the next box, and the free slot stays
	// free: the class is back at its cap.
	for i, b := range boxes {
		if b.Granted() {
			b.Settle(0)
			boxes[i] = nil
			break
		}
	}
	if n := running(); n != size/2 {
		t.Fatalf("a box's release left %d boxes running, want its slot handed to the next (%d)", n, size/2)
	}
	if held, _ := lane.Counts(); held != size-1 {
		t.Fatalf("the lane holds %d after a box's slot went to a box, want %d", held, size-1)
	}
}

// runShare is a coding box's share, under its launch.
func runShare(launch string) gitproc.Share {
	return gitproc.Share{Class: gitproc.ShareRun, Principal: launch}
}

// A CLASS ALONE TAKES THE WHOLE LANE: the share is work-conserving, so one
// machine principal's sixty-four arrivals run four at once from the first
// instant, nobody waits while a slot is free, and every grant is its.
func TestAClassAloneTakesTheWholeLane(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	lane := gitproc.NewShareLane(1, 4, clk.now)
	ci := machineShare("ci")
	turns := make([]*gitproc.Turn, 64)
	for i := range turns {
		turns[i] = lane.Arrive(ci)
	}
	for i, turn := range turns {
		if got, want := turn.Granted(), i < 4; got != want {
			t.Fatalf("arrival %d of 64 on a lane of four, alone: granted %v, want %v", i, got, want)
		}
	}
	running := slices.Clone(turns[:4])
	next := 4
	for len(running) > 0 {
		clk.advance(time.Second)
		running[0].Settle(100 * time.Millisecond)
		running = running[1:]
		if next < len(turns) {
			if !turns[next].Granted() {
				t.Fatalf("a settle with arrival %d next in line granted it nothing", next)
			}
			running = append(running, turns[next])
			next++
		}
		held, waiting := lane.Counts()
		if waiting > 0 && held != 4 {
			t.Fatalf("%d waiting with %d of 4 slots held: a slot is free while a run waits", waiting, held)
		}
	}
	if held, waiting := lane.Counts(); held != 0 || waiting != 0 {
		t.Fatalf("with every run settled the lane holds %d and queues %d", held, waiting)
	}
}

// WITHIN A CLASS THE PRINCIPAL THAT SPENT LEAST GOES FIRST: a machine credential
// that has spent thirty core-seconds waits behind one that has spent none,
// though it arrived first; with equal charges, arrival order.
//
// Mutation: next takes the class's oldest waiter, whatever its principal
// spent — red.
func TestWithinAClassThePrincipalThatSpentLeastGoesFirst(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	lane := gitproc.NewShareLane(1, 1, clk.now)
	spent, fresh := machineShare("spent"), machineShare("fresh")
	lane.Arrive(spent).Settle(30 * time.Second)
	if got, ok := lane.Charge(spent); !ok || got != 30 {
		t.Fatalf("a run that spent 30 s of CPU and held its slot no time charged %v (%v), want 30", got, ok)
	}
	holder := lane.Arrive(personShare("holder"))
	first, second := lane.Arrive(spent), lane.Arrive(fresh)
	holder.Settle(0)
	if !second.Granted() || first.Granted() {
		t.Fatalf("the principal that spent 30 core-seconds was granted %v and the one that spent none %v: "+
			"want the one that spent none, though it arrived second", first.Granted(), second.Granted())
	}

	tied := gitproc.NewShareLane(1, 1, clk.now)
	holder = tied.Arrive(personShare("holder"))
	first, second = tied.Arrive(machineShare("one")), tied.Arrive(machineShare("two"))
	holder.Settle(0)
	if !first.Granted() || second.Granted() {
		t.Fatalf("of two principals that spent nothing, the first to arrive was granted %v and the second %v: "+
			"want arrival order", first.Granted(), second.Granted())
	}
}

// A RUN GRANTED FROM THE QUEUE IS CHARGED TO ITS PRINCIPAL, a fresh one
// included: the account it was granted on is the one the book holds, so what
// it spends is what the principal's next turn is judged by and what its
// class's standing is swept to.
//
// Mutation: forget the account as its waiter is taken out of the queue,
// before its run starts — its charges land on an account the book no longer
// holds, and the principal reads as having spent nothing — red.
func TestARunGrantedFromTheQueueIsChargedToItsPrincipal(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	lane := gitproc.NewShareLane(1, 1, clk.now)
	holder := lane.Arrive(personShare("holder"))
	fresh := personShare("fresh")
	queued := lane.Arrive(fresh)
	holder.Settle(0)
	if !queued.Granted() {
		t.Fatal("a settle with one waiter granted it nothing")
	}
	// A lane of one slot over one core: a held slot is a whole core a second.
	clk.advance(4 * time.Second)
	queued.Settle(10 * time.Second)
	if got, want := chargeOf(t, lane, fresh), occupied(1, 4*time.Second)+(10-4); !near(got, want, 1e-9) {
		t.Fatalf("a fresh principal granted from the queue, run 4 s on 10 s of CPU, stands at %v, want %v", got, want)
	}
	// A WINDOW ON — the sweep's turn — the class reads what its accounts do:
	// the fresh principal's, the holder having spent nothing and been
	// forgotten.
	clk.advance(gitproc.ShareWindow)
	lane.Arrive(machineShare("tick")).Settle(0)
	if got, want := lane.Usage(gitproc.SharePerson), chargeOf(t, lane, fresh); !near(got, want, 1e-9) || got < 1 {
		t.Fatalf("a window on, the person class stands at %v, want its one account's %v", got, want)
	}
}

// A CHARGE IS THE GREATER OF CPU AND OCCUPANCY: a run holding its slot 40 s on
// 0.1 s of CPU is charged its occupancy, ten core-seconds; one holding it 2 s
// on 30 s of CPU, thirty. Occupancy accrues while the run holds its slot and
// what CPU spent past it is charged at the end, so the two sum to the greater
// exactly — read through the decay, which the occupancy accrues under.
//
// Mutations: the charge counting CPU only, or occupancy only, or the accrual
// skipped — each red.
func TestAChargeIsTheGreaterOfCPUAndOccupancy(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	lane := gitproc.NewShareLane(1, 4, clk.now)

	slow := machineShare("slow")
	turn := lane.Arrive(slow)
	clk.advance(40 * time.Second)
	turn.Settle(100 * time.Millisecond)
	if got, want := chargeOf(t, lane, slow), occupied(quarter, 40*time.Second); !near(got, want, 1e-9) {
		t.Errorf("a run holding its slot 40 s on 0.1 s of CPU stands at %v, want its ten core-seconds of "+
			"occupancy as they accrued: %v", got, want)
	}

	busy := machineShare("busy")
	turn = lane.Arrive(busy)
	clk.advance(2 * time.Second)
	turn.Settle(30 * time.Second)
	if got, want := chargeOf(t, lane, busy), occupied(quarter, 2*time.Second)+(30-0.5); !near(got, want, 1e-9) {
		t.Errorf("a run holding its slot 2 s on 30 s of CPU stands at %v, want its half core-second of "+
			"occupancy as it accrued and the CPU past it: %v", got, want)
	}

	// TWO RUNS OF ONE PRINCIPAL, OVERLAPPING, with the account read and
	// written in between: a at 0–25 s on 1 s of CPU (occupancy 6.25, the
	// greater), b at 10–40 s on 20 s (occupancy 7.5, CPU the greater).
	both := machineShare("both")
	a := lane.Arrive(both)
	clk.advance(10 * time.Second)
	b := lane.Arrive(both)
	clk.advance(15 * time.Second)
	a.Settle(time.Second)
	clk.advance(15 * time.Second)
	b.Settle(20 * time.Second)
	want := decayed(occupied(quarter, 25*time.Second), 15*time.Second) + occupied(quarter, 30*time.Second) + (20 - 7.5)
	if got := chargeOf(t, lane, both); !near(got, want, 1e-9) {
		t.Errorf("two overlapping runs stand at %v, want %v: the greater of CPU and occupancy each", got, want)
	}
}

// A HELD SLOT IS CHARGED THE SHARE OF A CORE IT STANDS FOR, as New sized the
// lane: the node's cores over its transfer slots, and never more than one,
// since a transfer's git runs on one thread. A quarter of a core at the
// default four a core; a whole core where the lane is narrower than the
// cores — 32 cores whose 8 GiB back 16 slots, or an operator's two slots on
// four cores; a half at two a core; and a sixteenth on four cores an
// operator widened to 64.
//
// Mutations: the rate a constant quarter of a core — every row but the first
// red; the cap at one core dropped — the two narrowed rows red.
func TestAHeldSlotIsChargedTheShareOfACoreItStandsFor(t *testing.T) {
	t.Parallel()
	const plenty = 1 << 40
	for _, tc := range []struct {
		name  string
		o     gitproc.Options
		slots int
		rate  float64
	}{
		{"four a core", gitproc.Options{MaxProcesses: 4, Memory: plenty}, 16, quarter},
		{"narrowed by its memory", gitproc.Options{MaxProcesses: 32, Memory: 8 << 30}, 16, 1},
		{"narrowed by its operator", gitproc.Options{MaxProcesses: 4, MaxTransfers: 2, Memory: plenty}, 2, 1},
		{"two a core", gitproc.Options{MaxProcesses: 8, MaxTransfers: 16, Memory: plenty}, 16, 0.5},
		{"widened by its operator", gitproc.Options{MaxProcesses: 4, MaxTransfers: 64, Memory: plenty}, 64, 1.0 / 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := fakePool(t, "exit 0", tc.o)
			if got := p.Slots(gitproc.ClassTransfer); got != tc.slots {
				t.Fatalf("%+v sized the transfer lane at %d slots, want %d", tc.o, got, tc.slots)
			}
			clk := newFakeClock()
			gitproc.SetShareClock(p, clk.now)
			lane := gitproc.TransferLane(p)
			holder := machineShare("token:holder")
			turn := lane.Arrive(holder)
			clk.advance(40 * time.Second)
			if got, want := chargeOf(t, lane, holder), occupied(tc.rate, 40*time.Second); !near(got, want, 1e-9) {
				t.Errorf("a slot of a lane of %d over %d cores held 40 s stands at %v, want %v: %v core-seconds a second",
					tc.slots, tc.o.MaxProcesses, got, want, tc.rate)
			}
			turn.Settle(0)
		})
	}
}

// SLOTS HELD ALIKE ARE CHARGED ALIKE WHERE SLOTS ARE WHAT RUNS OUT: on 32
// cores whose 8 GiB back 16 slots, two cores' worth each, one credential's
// four clones paced by slow links (no CPU) and another's four reading as fast
// as one thread serves them (a core each) hold four slots each for a minute,
// and stand equal — so when both wait, arrival order decides between them, as
// it does between any two who spent the same. Where the cores run out first —
// the default four a core — the fast one is charged its CPU, above the slow
// one's slot-time.
//
// Mutation: the rate a constant quarter of a core — on 32 cores the slow
// credential stands at 38 core-seconds to the fast one's 218, and would be
// granted ahead of it though it asked second — red.
func TestSlotsHeldAlikeAreChargedAlikeWhereSlotsRunOut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		o     gitproc.Options
		alike bool
	}{
		{"narrowed by its memory", gitproc.Options{MaxProcesses: 32, Memory: 8 << 30}, true},
		{"four a core", gitproc.Options{MaxProcesses: 4, Memory: 1 << 40}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := fakePool(t, "exit 0", tc.o)
			clk := newFakeClock()
			gitproc.SetShareClock(p, clk.now)
			lane := gitproc.TransferLane(p)
			slots := p.Slots(gitproc.ClassTransfer)
			slow, fast := machineShare("token:slow"), machineShare("token:fast")
			var held []*gitproc.Turn
			for range slots - 8 {
				held = append(held, lane.Arrive(machineShare("token:holder")))
			}
			var slowTurns, fastTurns []*gitproc.Turn
			for range 4 {
				slowTurns, fastTurns = append(slowTurns, lane.Arrive(slow)), append(fastTurns, lane.Arrive(fast))
			}
			clk.advance(time.Minute)
			for i := range 4 {
				slowTurns[i].Settle(0)
				fastTurns[i].Settle(time.Minute)
			}
			slowAt, fastAt := chargeOf(t, lane, slow), chargeOf(t, lane, fast)
			if tc.alike {
				if !near(slowAt, fastAt, 1e-9) {
					t.Fatalf("four slots held a minute stand at %v by slow clients and %v by fast ones, want equal",
						slowAt, fastAt)
				}
			} else {
				want := 4*occupied(quarter, time.Minute) + 4*(60-60*quarter)
				if !near(fastAt, want, 1e-9) || fastAt <= slowAt {
					t.Fatalf("on a lane of four a core the fast credential stands at %v and the slow one at %v, "+
						"want the fast one charged its CPU: %v", fastAt, slowAt, want)
				}
				return
			}
			// THE LANE FULL AGAIN, the fast credential asking first: a tie,
			// and the older waiter goes first.
			for range 8 {
				held = append(held, lane.Arrive(machineShare("token:filler")))
			}
			first, second := lane.Arrive(fast), lane.Arrive(slow)
			held[0].Settle(0)
			if !first.Granted() || second.Granted() {
				t.Fatalf("with the two standing equal, the first to ask was granted %v and the second %v: want "+
					"arrival order", first.Granted(), second.Granted())
			}
		})
	}
}

// chargeOf is s's standing on lane, failing the case when the book forgot it.
func chargeOf(t *testing.T, lane *gitproc.ShareLane, s gitproc.Share) float64 {
	t.Helper()
	v, ok := lane.Charge(s)
	if !ok {
		t.Fatalf("the book holds no account for %v", s)
	}
	return v
}

// A RUNNING TRANSFER ACCRUES WHILE IT RUNS: a principal holding all four
// slots for nine and a half minutes reads four slots' occupancy — a core-second
// a second, decayed — at a grant, not zero, so a fresh principal of its class
// is granted ahead of its own next arrival though that arrived first.
//
// Mutation: a standing that leaves its running runs out — red.
func TestARunningTransferAccruesWhileItRuns(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	lane := gitproc.NewShareLane(1, 4, clk.now)
	hog := machineShare("hog")
	var turns []*gitproc.Turn
	for range 4 {
		turns = append(turns, lane.Arrive(hog))
	}
	later, fresh := lane.Arrive(hog), lane.Arrive(machineShare("fresh"))
	clk.advance(9*time.Minute + 30*time.Second)
	want := 4 * occupied(quarter, 9*time.Minute+30*time.Second)
	if got := chargeOf(t, lane, hog); !near(got, want, 1e-9) || got < 59 {
		t.Fatalf("a principal holding every slot for 570 s stands at %v, want %v", got, want)
	}
	turns[0].Settle(0)
	if !fresh.Granted() || later.Granted() {
		t.Fatalf("the slot the hog let go went to its own next arrival (%v) rather than to the fresh "+
			"principal (%v)", later.Granted(), fresh.Granted())
	}
}

// A CHARGE DECAYS OVER THE WINDOW: after one ShareWindow it is 1/e of what it
// was, and once it has decayed under ShareForget an account that neither runs
// nor waits is gone from the book — while one above it stays.
//
// Mutations: the decay's time constant changed, or the sweep skipped — red.
func TestAChargeDecaysOverTheWindow(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	lane := gitproc.NewShareLane(1, 1, clk.now)
	burst := machineShare("burst")
	lane.Arrive(burst).Settle(time.Minute)
	if got := chargeOf(t, lane, burst); got != 60 {
		t.Fatalf("a run of 60 s of CPU charged %v", got)
	}
	clk.advance(gitproc.ShareWindow)
	if got := chargeOf(t, lane, burst); !near(got, 60/math.E, 1e-9) {
		t.Fatalf("a charge of 60 reads %v a window later, want 60/e = %v", got, 60/math.E)
	}

	// Ten windows on — e^-11 of 60 is 0.00100, just over the line — the
	// account is still held after the lane's events; two more and a lane
	// event forgets it.
	other := personShare("other")
	clk.advance(10 * gitproc.ShareWindow)
	lane.Arrive(other).Settle(0)
	if got, ok := lane.Charge(burst); !ok || got < gitproc.ShareForget {
		t.Fatalf("eleven windows on, a charge of 60 stands at %v (held %v), want it held above %v",
			got, ok, gitproc.ShareForget)
	}
	clk.advance(2 * gitproc.ShareWindow)
	lane.Arrive(other).Settle(0)
	if got, ok := lane.Charge(burst); ok {
		t.Fatalf("thirteen windows on, an idle account standing at %v is still in the book", got)
	}
	if n := lane.Principals(); n != 0 {
		t.Fatalf("the book holds %d accounts with nobody running, waiting or standing above %v", n,
			gitproc.ShareForget)
	}
}

// THE INVARIANT HOLDS UNDER THE SHARE, its capped class included: whichever
// waiter a release picks, no slot is free while a waiter that MAY take it
// waits — a coding box at its class's cap of half the lane may not — a
// release hands its slot straight to such a waiter where there is one and to
// the lane where there is none, an arrival never takes a slot ahead of one,
// the boxes never hold more than their cap, and every arrival, settle and
// leave is accounted for — over 10 000 random steps of all three classes and
// eighteen principals, and again with waits whose contexts end, on
// goroutines.
func TestTheInvariantHoldsUnderTheShare(t *testing.T) {
	t.Parallel()
	t.Run("steps", func(t *testing.T) {
		t.Parallel()
		const size = 5
		limit := gitproc.ShareRun.Cap(size)
		clk := newFakeClock()
		lane := gitproc.NewShareLane(1, size, clk.now)
		rng := rand.New(rand.NewPCG(1, 2))
		var principals []gitproc.Share
		for i := range 6 {
			principals = append(principals, machineShare(fmt.Sprintf("m%d", i)), personShare(fmt.Sprintf("p%d", i)),
				runShare(fmt.Sprintf("r%d", i)))
		}
		type turn struct {
			*gitproc.Turn
			class gitproc.ShareClass
		}
		var granted, waiting []turn
		boxes := func() int {
			n := 0
			for _, g := range granted {
				if g.class == gitproc.ShareRun {
					n++
				}
			}
			return n
		}
		// mayTake reports a waiter of class c that may take a free slot now.
		mayTake := func(c gitproc.ShareClass) bool { return c != gitproc.ShareRun || boxes() < limit }
		eligible := func() bool { return slices.ContainsFunc(waiting, func(w turn) bool { return mayTake(w.class) }) }
		for step := range 10_000 {
			clk.advance(time.Duration(rng.IntN(2000)) * time.Millisecond)
			switch op := rng.IntN(10); {
			case op < 5:
				s := principals[rng.IntN(len(principals))]
				before := boxes()
				arrived := turn{lane.Arrive(s), s.Class}
				if arrived.Granted() {
					if eligible() {
						t.Fatalf("step %d: an arrival took a slot ahead of a waiter that may take one", step)
					}
					if s.Class == gitproc.ShareRun && before >= limit {
						t.Fatalf("step %d: a box took a slot with %d boxes running, its class's cap", step, before)
					}
					granted = append(granted, arrived)
				} else {
					waiting = append(waiting, arrived)
				}
			case op < 8 && len(granted) > 0:
				i := rng.IntN(len(granted))
				granted[i].Settle(time.Duration(rng.IntN(3000)) * time.Millisecond)
				granted = slices.Delete(granted, i, i+1)
				want := 0
				if eligible() {
					want = 1
				}
				before := len(waiting)
				waiting = slices.DeleteFunc(waiting, func(w turn) bool {
					if w.Granted() {
						granted = append(granted, w)
						return true
					}
					return false
				})
				if handed := before - len(waiting); handed != want {
					t.Fatalf("step %d: a settle with %d waiting granted %d of them, want %d", step, before, handed,
						want)
				}
			case len(waiting) > 0:
				i := rng.IntN(len(waiting))
				waiting[i].Leave()
				waiting = slices.Delete(waiting, i, i+1)
			}
			held, queued := lane.Counts()
			if held != int64(len(granted)) || queued != int64(len(waiting)) {
				t.Fatalf("step %d: the lane holds %d and queues %d, the case %d and %d", step, held, queued,
					len(granted), len(waiting))
			}
			if held < size && eligible() {
				t.Fatalf("step %d: %d of %d slots held while a waiter that may take one waits", step, held, size)
			}
			if n := boxes(); n > limit {
				t.Fatalf("step %d: %d boxes hold a slot, past their class's cap of %d", step, n, limit)
			}
		}
	})
	t.Run("contexts", func(t *testing.T) {
		t.Parallel()
		lane := gitproc.NewShareLane(1, 2, time.Now)
		var wg sync.WaitGroup
		for i := range 200 {
			wg.Go(func() {
				s := machineShare(fmt.Sprintf("m%d", i%7))
				switch i % 3 {
				case 0:
					s = personShare(fmt.Sprintf("p%d", i%5))
				case 1:
					s = runShare(fmt.Sprintf("r%d", i%4))
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Duration(i%4)*time.Millisecond)
				defer cancel()
				turn, err := lane.Acquire(ctx, s)
				if err != nil {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("an acquire ended %v, want its context's deadline", err)
					}
					return
				}
				held, _ := lane.Counts()
				if held > 2 {
					t.Errorf("%d of 2 slots held", held)
				}
				time.Sleep(time.Duration(i%3) * time.Millisecond)
				turn.Settle(time.Millisecond)
			})
		}
		wg.Wait()
		if held, waiting := lane.Counts(); held != 0 || waiting != 0 {
			t.Fatalf("with every acquire ended the lane holds %d and queues %d", held, waiting)
		}
	})
}

// A TRANSFER WITHOUT A SHARE IS REFUSED, and so is a share on anything else:
// a transfer carrying none would be scheduled as nobody's, a read or a
// maintenance run carrying one claims what no lane reads. Each refusal is the
// call's, before any process starts. A principal is 1..255 bytes with no NUL:
// the longest name an identity is given, a credential's or a seat's, is
// admitted at the bound and one byte past it is not.
//
// Mutations: drop prepare's share checks — red; drop Share.Valid's length
// bound or its NUL check — TestAMalformedCommandIsRefused red; tighten the
// bound by a byte — the 255-byte principal here red.
func TestATransferWithoutAShareIsRefused(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "true", gitproc.Options{})
	for _, tc := range []struct {
		name string
		cmd  gitproc.Cmd
	}{
		{"a transfer with none", gitproc.Cmd{Args: []string{"upload-pack"}, Class: gitproc.ClassTransfer}},
		{"a transfer of no class", gitproc.Cmd{Args: []string{"upload-pack"}, Class: gitproc.ClassTransfer,
			Share: gitproc.Share{Class: "robot", Principal: "x"}}},
		{"a transfer of nobody", gitproc.Cmd{Args: []string{"upload-pack"}, Class: gitproc.ClassTransfer,
			Share: gitproc.Share{Class: gitproc.ShareMachine}}},
		{"a transfer of a principal past 255 bytes", gitproc.Cmd{Args: []string{"upload-pack"}, Class: gitproc.ClassTransfer,
			Share: machineShare(strings.Repeat("m", 256))}},
		{"a transfer of a principal holding a NUL", gitproc.Cmd{Args: []string{"upload-pack"}, Class: gitproc.ClassTransfer,
			Share: personShare("alice\x00")}},
		{"a read with one", gitproc.Cmd{Args: []string{"rev-parse"}, Class: gitproc.ClassRead, Share: machineShare("ci")}},
		{"maintenance with one", gitproc.Cmd{Args: []string{"commit-graph"}, Class: gitproc.ClassMaintenance,
			Share: personShare("ceo")}},
	} {
		tc.cmd.Deadline = time.Minute
		res, err := p.Run(t.Context(), tc.cmd)
		if !errors.Is(err, gitproc.ErrInvalid) || !res.Began.IsZero() {
			t.Errorf("%s: %v (a process began: %v), want ErrInvalid before any process", tc.name, err, !res.Began.IsZero())
		}
	}
	for _, s := range []gitproc.Share{personShare("ceo"), machineShare("token:ci"), machineShare(strings.Repeat("m", 255))} {
		if _, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"upload-pack"}, Class: gitproc.ClassTransfer,
			Share: s, Deadline: time.Minute}); err != nil {
			t.Fatalf("a transfer of %s %d-byte principal: %v", s.Class, len(s.Principal), err)
		}
	}
}

// THE SHARE IS RECORDED BY CLASS: every transfer's wait for its slot is an
// observation of crewlet.code.git.share_wait under its class, and the lane's
// standing of each class is crewlet.code.git.share, the class that ran read
// above the one that did not.
func TestTheShareIsRecordedByClass(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	p := fakePool(t, "sleep 0.2", gitproc.Options{Metrics: rec})
	for range 3 {
		if _, err := p.Run(t.Context(), gitproc.Cmd{Args: []string{"upload-pack"}, Class: gitproc.ClassTransfer,
			Share: machineShare("ci"), Deadline: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	share := func(name string, class gitproc.ShareClass) metrics.Snapshot {
		for _, s := range rec.Read() {
			if s.Name == name && s.Attrs["share"] == string(class) {
				return s
			}
		}
		return metrics.Snapshot{}
	}
	if got := share(metrics.CodeGitShareWait, gitproc.ShareMachine).Count; got != 3 {
		t.Errorf("%d machine share-wait observations for three transfers, want 3", got)
	}
	if got := share(metrics.CodeGitShareWait, gitproc.SharePerson).Count; got != 0 {
		t.Errorf("%d person share-wait observations with no person's transfer", got)
	}
	machine, person := share(metrics.CodeGitShare, gitproc.ShareMachine), share(metrics.CodeGitShare, gitproc.SharePerson)
	if machine.Name == "" || person.Name == "" || machine.Value <= 0 || person.Value != 0 {
		t.Errorf("the lane's standing reads machine %v (%q) and person %v (%q), want the machine's above "+
			"none and the person's at none", machine.Value, machine.Name, person.Value, person.Name)
	}
}

// BenchmarkTransferGrant is ONE GRANT OF A FULL, CONTENDED LANE: 10 000
// waiters over 1 000 principals of both classes behind one slot, each
// iteration a settle that grants the next — the scan over the waiting
// principals that decides it — and the granted principal's arrival at the back
// again, so the queue stays as deep. What it decides: whether next's linear
// scan stays under a grant's budget (100 µs) at that depth, or a heap is owed.
func BenchmarkTransferGrant(b *testing.B) {
	const principals, waiters = 1_000, 10_000
	clk := newFakeClock()
	lane := gitproc.NewShareLane(1, 1, clk.now)
	shares := make([]gitproc.Share, principals)
	for i := range shares {
		shares[i] = machineShare(fmt.Sprintf("m%04d", i))
		if i%2 == 0 {
			shares[i] = personShare(fmt.Sprintf("p%04d", i))
		}
	}
	holder := lane.Arrive(personShare("holder"))
	queued := make([]*gitproc.Turn, 0, waiters)
	byTurn := map[*gitproc.Turn]gitproc.Share{}
	for i := range waiters {
		s := shares[i%principals]
		turn := lane.Arrive(s)
		queued = append(queued, turn)
		byTurn[turn] = s
	}
	b.ResetTimer()
	for b.Loop() {
		clk.advance(10 * time.Millisecond)
		holder.Settle(5 * time.Millisecond)
		b.StopTimer()
		i := slices.IndexFunc(queued, (*gitproc.Turn).Granted)
		if i < 0 {
			b.Fatal("a settle granted nobody")
		}
		holder = queued[i]
		s := byTurn[holder]
		queued = slices.Delete(queued, i, i+1)
		b.StartTimer()
		turn := lane.Arrive(s)
		b.StopTimer()
		queued = append(queued, turn)
		byTurn[turn] = s
		b.StartTimer()
	}
}
