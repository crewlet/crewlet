package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/period"
)

// EVERY FLEET COMPARE-AND-SWAP LOOP OUTNUMBERS ITS HEAD COUNT.
//
// The arithmetic the bounds' docs state, held to the constants: the attempts
// are twice the head count, the backoff's top window spreads that head count
// over twice as many round trips, and the head count covers the largest fleet
// the engine is measured at.
//
// What it does NOT hold is that coalescing is needed to pass the contract —
// measured, it is not: with every write a contender of its own, the contract's
// contended cases pass on these attempts and this backoff alone. Coalescing is
// held where it can be counted, by the writes a node's charges reach the
// broker as ([TestANodesChargesOnOneScopeAreOneContender]).
func TestEveryFleetCASLoopOutnumbersItsHeadCount(t *testing.T) {
	t.Parallel()
	const measuredFleet = 100 // the hundred nodes kv.go and gate.go were measured with
	if fleetWriters < measuredFleet {
		t.Errorf("fleetWriters = %d, below the %d nodes this engine is measured at", fleetWriters, measuredFleet)
	}
	if fleetCASAttempts < 2*fleetWriters {
		t.Errorf("fleetCASAttempts = %d, under twice the %d writers a key is bounded for: "+
			"a healthy contended write fails closed before its turn comes", fleetCASAttempts, fleetWriters)
	}
	if 1<<casBackoffLevels < 2*fleetWriters {
		t.Errorf("the top backoff window is %d round trips, under twice the %d writers it spreads",
			1<<casBackoffLevels, fleetWriters)
	}
}

// inWindow holds every wait at level to a jittered window of 2^level of rtt.
func inWindow(t *testing.T, what string, rtt time.Duration, level int) {
	t.Helper()
	window := rtt << level
	lo, hi := window/10, window*19/10
	for range 50 {
		if got := casWindow(level, rtt); got < lo || got > hi {
			t.Fatalf("%s: a wait was %v, want within [%v, %v] — 2^%d round trips "+
				"of %v, jittered", what, got, lo, hi, level, rtt)
		}
	}
}

// A WINDOW IS 2^LEVEL ROUND TRIPS, jittered: nothing at level zero or before a
// round trip is known, doubling with each level and stopping at the top.
func TestAWindowIsCountedInRoundTrips(t *testing.T) {
	t.Parallel()
	const rtt = 3 * time.Millisecond
	if got := casWindow(0, rtt); got != 0 {
		t.Fatalf("level zero waits %v, want nothing: a key nobody has lost on", got)
	}
	if got := casWindow(3, 0); got != 0 {
		t.Fatalf("no round trip yet waits %v, want nothing", got)
	}
	for level := 1; level <= casBackoffLevels; level++ {
		inWindow(t, fmt.Sprintf("level %d", level), rtt, level)
	}
	for range 50 {
		if got := casWindow(casBackoffLevels+3, rtt); got > (rtt<<casBackoffLevels)*19/10 {
			t.Fatalf("a level past the top waits %v, over the top window", got)
		}
	}
}

// A ROUND TRIP IS NOT ONE ATTEMPT'S.
//
// Every window multiplies the round trip by up to 256, so no one attempt may
// set it. The first sample alone counts for nothing — a process's first
// attempts on a bucket are its slowest, and a first loss a hundred times slower
// than the rest used to set its line's every window — the estimate is seeded by
// the smaller of the first two, and after that one outlying attempt moves it by
// an eighth of itself however slow it was, while a latency that really rose is
// followed within a few dozen attempts.
func TestARoundTripIsNotOneAttempts(t *testing.T) {
	t.Parallel()
	const rtt = 3 * time.Millisecond
	var trips roundTrip
	trips.observe(100 * rtt)
	if got := trips.estimate(); got != 0 {
		t.Fatalf("after one attempt of %v the estimate is %v, want none yet: one sample "+
			"is the slow first attempt as often as not", 100*rtt, got)
	}
	trips.observe(rtt)
	if got := trips.estimate(); got != rtt {
		t.Fatalf("after a first attempt of %v and a second of %v the estimate is %v, want "+
			"the smaller: the outlying first sets nothing", 100*rtt, rtt, got)
	}
	for level := 1; level <= casBackoffLevels; level++ {
		inWindow(t, "after the outlying first attempt", trips.estimate(), level)
	}
	trips.observe(100 * rtt)
	if got, want := trips.estimate(), rtt+rtt/casRTTGain; got != want {
		t.Fatalf("one later attempt of %v moved the estimate from %v to %v, want %v: at most "+
			"an eighth of itself", 100*rtt, rtt, got, want)
	}
	for range 26 {
		trips.observe(10 * rtt)
	}
	if got := trips.estimate(); got < 9*rtt {
		t.Fatalf("twenty-six attempts at ten times the round trip brought the estimate to %v, "+
			"want it nine-tenths of the way: a latency that rose is followed", got)
	}
}

// A LEVEL DECAYS WHILE ITS LINE IS IDLE, one level for every window at that
// level the key was left quiet, with what is left of the window it is in.
func TestALevelDecaysByTheWindowsItSatIdle(t *testing.T) {
	t.Parallel()
	const rtt = time.Millisecond
	for _, c := range []struct {
		name      string
		level     int
		idle      time.Duration
		wantLevel int
		wantInto  time.Duration
	}{
		{"not idle at all", 4, 0, 4, 0},
		{"inside its first window", 4, 15 * rtt, 4, 15 * rtt},
		{"one window", 4, 16 * rtt, 3, 0},
		{"one window and part of the next", 4, 20 * rtt, 3, 4 * rtt},
		{"every window to nothing", 4, (16 + 8 + 4 + 2) * rtt, 0, 0},
		{"long past it", casBackoffLevels, time.Hour, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			level, into := decayLevel(c.level, c.idle, rtt)
			if level != c.wantLevel || (level > 0 && into != c.wantInto) {
				t.Fatalf("level %d idle %v = (%d, %v), want (%d, %v)",
					c.level, c.idle, level, into, c.wantLevel, c.wantInto)
			}
		})
	}
	if level, _ := decayLevel(casBackoffLevels, 0, 0); level != 0 {
		t.Fatalf("a level with no round trip to count windows in decayed to %d, want nothing", level)
	}
}

// holdWriting is the budgets bucket with the first write of one key HELD:
// reached closes when that write arrives, and it is made once release closes.
// What queues behind a held write is the next batch, whole.
type holdWriting struct {
	jetstream.KeyValue
	key     string
	once    *sync.Once
	reached chan struct{}
	release chan struct{}
}

func holdFirstWrite(kv jetstream.KeyValue, key string) holdWriting {
	return holdWriting{KeyValue: kv, key: key, once: new(sync.Once),
		reached: make(chan struct{}), release: make(chan struct{})}
}

func (k holdWriting) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key == k.key {
		k.once.Do(func() {
			close(k.reached)
			<-k.release
		})
	}
	return k.KeyValue.Update(ctx, key, value, revision)
}

// queuedOn is how many writes wait on key's line behind the batch in flight.
func (l *writeLines[T]) queuedOn(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ln := l.lines[key]; ln != nil {
		return len(ln.queue)
	}
	return 0
}

// waitQueued waits until n writes are queued on key's line.
func waitQueued[T any](t *testing.T, l *writeLines[T], key string, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for l.queuedOn(key) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d writes queued on %s, waited for %d", l.queuedOn(key), key, n)
		}
		time.Sleep(time.Millisecond)
	}
}

type chargeAnswer struct {
	spend coord.Spend
	err   error
}

// A BATCH DECIDES EACH CHARGE WHERE A SERIAL ORDER WOULD.
//
// Four charges queue behind a held one and so form ONE batch, decided by ONE
// compare-and-swap. Each is judged in arrival order against what the charges
// before it left — the first two fit the company's ceiling of 100, the third
// crosses it and is refused, and the fourth is refused against what the third
// counted — and each is answered with its own figures. A batch judged against
// the record as it was read would admit all four; one judged as a whole would
// refuse the two that fit.
func TestABatchDecidesEachChargeWhereASerialOrderWould(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	org := encodeKey(coord.OrgScope)
	held := holdFirstWrite(store.budgets, org)
	store.budgets = held
	var (
		mu      sync.Mutex
		batches [][2]int
	)
	store.tallies.observe = func(key string, batch, attempts int) {
		if key != org {
			return // the seats' writes, one each
		}
		mu.Lock()
		defer mu.Unlock()
		batches = append(batches, [2]int{batch, attempts})
	}
	w := testWindows()
	caps := coord.Caps{period.Day: 100}
	charge := func(seat string, tokens int) chan chargeAnswer {
		out := make(chan chargeAnswer, 1)
		go func() {
			spend, err := store.Charge(t.Context(), coord.ChargeRequest{
				Seat: coord.AgentScope(seat), Tokens: tokens, Windows: w, OrgCaps: caps,
			})
			out <- chargeAnswer{spend, err}
		}()
		return out
	}

	first := charge("held", 10)
	<-held.reached
	sizes := []int{30, 50, 20, 5}
	answers := make([]chan chargeAnswer, len(sizes))
	for i, tokens := range sizes {
		answers[i] = charge(fmt.Sprintf("queued-%d", i), tokens)
		waitQueued(t, &store.tallies, org, i+1)
	}
	close(held.release)

	if got := <-first; got.err != nil || !got.spend.OK {
		t.Fatalf("the held charge = (%+v, %v), want it admitted", got.spend, got.err)
	}
	want := []struct {
		ok   bool
		used int
	}{{true, 40}, {true, 90}, {false, 110}, {false, 115}}
	for i, a := range answers {
		got := <-a
		if got.err != nil {
			t.Fatalf("charge %d of %d: %v", i, sizes[i], got.err)
		}
		used := got.spend.Org.In(period.Day).Used
		if got.spend.OK != want[i].ok || used != want[i].used {
			t.Errorf("charge %d of %d = admitted %v at %d, want admitted %v at %d — "+
				"its place in a serial order", i, sizes[i], got.spend.OK, used, want[i].ok, want[i].used)
		}
		if !want[i].ok && (got.spend.RefusedScope != "org" || got.spend.RefusedUsed != want[i].used ||
			got.spend.RefusedLimit != 100) {
			t.Errorf("charge %d's refusal = %+v, want the org refusing at %d of 100",
				i, got.spend, want[i].used)
		}
	}
	if used := orgSpent(t, store); used != [3]int{115, 115, 115} {
		t.Errorf("the org's windows hold %v, want every round counted: 115", used)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) < 2 || batches[0] != [2]int{1, 1} || batches[1] != [2]int{len(sizes), 1} {
		t.Errorf("the org's batches were %v, want the held charge alone and then all %d "+
			"queued behind it in one compare-and-swap", batches, len(sizes))
	}
}

// TAKEN OR WITHDRAWN, NEVER BOTH.
//
// A charge whose caller hangs up while it waits behind a batch is WITHDRAWN at
// once — answered as unreachable, its round on no counter — and one whose
// caller hangs up after its batch formed is TAKEN: it lands, and its caller is
// told what it did, never that nothing happened, because the company's half
// of a charge that landed with the seat's never written would be a partial
// nobody was told about.
func TestAChargeIsTakenOrWithdrawnNeverBoth(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	org := encodeKey(coord.OrgScope)
	held := holdFirstWrite(store.budgets, org)
	store.budgets = held
	w := testWindows()
	charge := func(ctx context.Context, seat string, tokens int) chan chargeAnswer {
		out := make(chan chargeAnswer, 1)
		go func() {
			spend, err := store.Charge(ctx, coord.ChargeRequest{
				Seat: coord.AgentScope(seat), Tokens: tokens, Windows: w,
			})
			out <- chargeAnswer{spend, err}
		}()
		return out
	}

	takenCtx, hangUpTaken := context.WithCancel(t.Context())
	defer hangUpTaken()
	taken := charge(takenCtx, "taken", 10)
	<-held.reached
	// Its batch has formed and its write is in flight: the hang-up comes
	// too late to withdraw it.
	hangUpTaken()

	waitingCtx, hangUpWaiting := context.WithCancel(t.Context())
	defer hangUpWaiting()
	withdrawn := charge(waitingCtx, "withdrawn", 1000)
	waitQueued(t, &store.tallies, org, 1)
	hangUpWaiting()
	select {
	case got := <-withdrawn:
		if !errors.Is(got.err, coord.ErrUnavailable) || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("the withdrawn charge = (%+v, %v), want its hang-up as an unreachable "+
				"counter", got.spend, got.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a charge whose caller hung up while it waited was not answered until " +
			"the batch ahead of it ended: it was taken, not withdrawn")
	}

	live := charge(t.Context(), "live", 5)
	waitQueued(t, &store.tallies, org, 1)
	close(held.release)

	if got := <-taken; got.err != nil || !got.spend.OK || got.spend.Org.In(period.Day).Used != 10 {
		t.Fatalf("the taken charge = (%+v, %v), want it answered with the write that landed",
			got.spend, got.err)
	}
	if got := <-live; got.err != nil || !got.spend.OK {
		t.Fatalf("the charge queued after the withdrawal = (%+v, %v), want it admitted", got.spend, got.err)
	}
	if used := orgSpent(t, store); used != [3]int{15, 15, 15} {
		t.Errorf("the org's windows hold %v, want the taken and the live charge only: 15", used)
	}
	for seat, want := range map[string][3]int{
		"taken": {10, 10, 10}, "withdrawn": {0, 0, 0}, "live": {5, 5, 5},
	} {
		if got := spentBy(t, store, coord.AgentScope(seat)); got != want {
			t.Errorf("seat %s holds %v, want %v", seat, got, want)
		}
	}
}

// alwaysLosing is the budgets bucket where another writer always wins one
// key: every write of it is refused as a revision somebody else moved.
type alwaysLosing struct {
	jetstream.KeyValue
	key string
}

func (k alwaysLosing) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key == k.key {
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	return k.KeyValue.Update(ctx, key, value, revision)
}

// A WRITE THAT NEVER SETTLES FAILS CLOSED AS AN OUTAGE, after exactly the
// shared bound and with a backoff between every two attempts — never as a
// refusal, which would tell an agent its company is out of budget because the
// counter is busy.
func TestAWriteThatNeverSettlesFailsClosedAsAnOutage(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	w := testWindows()
	if _, err := store.Charge(t.Context(), coord.ChargeRequest{
		Seat: coord.AgentScope("x"), Tokens: 1, Windows: w,
	}); err != nil {
		t.Fatalf("setup Charge: %v", err)
	}
	store.budgets = alwaysLosing{KeyValue: store.budgets, key: encodeKey(coord.OrgScope)}
	var (
		mu       sync.Mutex
		waits    int
		attempts int
	)
	// The clock stands still, so every window the line schedules is still
	// ahead of it when the line waits, however long an attempt took; and a
	// wait is over the moment it is asked for.
	frozen := time.Now()
	store.tallies.now = func() time.Time { return frozen }
	store.tallies.after = func(time.Duration) <-chan time.Time {
		mu.Lock()
		defer mu.Unlock()
		waits++
		over := make(chan time.Time, 1)
		over <- frozen
		return over
	}
	store.tallies.observe = func(_ string, _, n int) {
		mu.Lock()
		defer mu.Unlock()
		attempts = n
	}

	got, err := store.Charge(t.Context(), coord.ChargeRequest{
		Seat: coord.AgentScope("x"), Tokens: 1, Windows: w, OrgCaps: coord.Caps{period.Day: 100},
	})
	if !errors.Is(err, coord.ErrUnavailable) || !strings.Contains(err.Error(), "lost every compare-and-swap") {
		t.Fatalf("Charge = (%+v, %v), want an outage naming the lost compare-and-swaps", got, err)
	}
	if got.OK || got.RefusedScope != "" {
		t.Fatalf("Charge = %+v: a busy counter reported as a decision", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != fleetCASAttempts || waits != fleetCASAttempts-1 {
		t.Fatalf("the batch made %d attempts with %d waits, want %d attempts and a wait "+
			"before every one after the first", attempts, waits, fleetCASAttempts)
	}
}

// countWrites is the budgets bucket counting the writes of one key — every
// compare-and-swap that reaches the broker, landed or lost.
type countWrites struct {
	jetstream.KeyValue
	key    string
	writes *atomic.Int64
}

func (k countWrites) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key == k.key {
		k.writes.Add(1)
	}
	return k.KeyValue.Update(ctx, key, value, revision)
}

// A NODE'S CHARGES ON ONE SCOPE ARE ONE CONTENDER, counted where they reach
// the broker.
//
// [coordtest.ContendedCharges] charges released together through one store
// reach the broker as fewer than half as many compare-and-swaps — each batch
// writes once, and the next is everything that queued behind it — where a
// store that let every charge write for itself writes at least once a charge.
//
// THIS IS THE TEST THAT TELLS THE TWO APART, and the contract's contended
// cases are not: with every write made a contender of its own, those cases
// still pass on [fleetCASAttempts] and the round-trip window, because 320
// contenders spread over up to 256 round trips mostly find their turn. What
// coalescing buys shows only in what a bound can be sized for — a counter's
// writers are every round in flight on every node, which no attempt count
// covers, or the nodes, which [fleetWriters] does.
func TestANodesChargesOnOneScopeAreOneContender(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	var writes atomic.Int64
	store.budgets = countWrites{KeyValue: store.budgets, key: encodeKey(coord.OrgScope), writes: &writes}
	w := testWindows()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, coordtest.ContendedCharges)
	for i := range coordtest.ContendedCharges {
		wg.Go(func() {
			<-start
			if _, err := store.Charge(t.Context(), coord.ChargeRequest{
				Seat: coord.AgentScope(fmt.Sprintf("one-contender-%d", i)), Tokens: 1, Windows: w,
			}); err != nil {
				errs <- err
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a charge failed: %v", err)
	}
	if used := orgSpent(t, store); used[0] != coordtest.ContendedCharges {
		t.Fatalf("the org's day holds %d after %d charges of 1", used[0], coordtest.ContendedCharges)
	}
	if got := writes.Load(); got >= coordtest.ContendedCharges/2 {
		t.Fatalf("%d charges reached the broker as %d writes of the org's counter, want fewer "+
			"than half as many: a node's charges on one scope are batched into one write",
			coordtest.ContendedCharges, got)
	}
}

// AND JUDGED AGAIN AS ITS BATCH FORMS: a write whose caller hung up before its
// goroutine could withdraw it — the hang-up and the batch forming in the same
// instant — is withdrawn by the batch rather than taken, its change never
// asked, while the write that leads the batch lands.
func TestAWriteWhoseCallerHungUpIsWithdrawnAsItsBatchForms(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	lines := writeLines[int]{codec: codec[int]{
		decode: func(raw []byte) (int, error) {
			var n int
			err := json.Unmarshal(raw, &n)
			return n, err
		},
		encode:  func(n int) ([]byte, error) { return json.Marshal(n) },
		reading: "read the probe", decoding: "decode the probe",
	}}
	gone, hangUp := context.WithCancel(t.Context())
	hangUp()
	asked := false
	write := func(ctx context.Context, change func(int, bool) (int, bool)) *lineWrite[int] {
		return &lineWrite[int]{ctx: ctx, verb: "write the probe", resource: "probe", change: change,
			done: make(chan struct{}), lead: make(chan struct{})}
	}
	leader := write(t.Context(), func(n int, _ bool) (int, bool) { return n + 1, true })
	hungUp := write(gone, func(n int, _ bool) (int, bool) { asked = true; return n + 10, true })
	key := encodeKey("probe")
	ln := &writeLine[int]{queue: []*lineWrite[int]{leader, hungUp}, leader: leader}
	lines.lines = map[string]*writeLine[int]{key: ln}

	lines.lead(t.Context(), store.budgets, store.js, key, ln, leader)

	<-leader.done
	<-hungUp.done
	if leader.err != nil || leader.result != 1 {
		t.Fatalf("the leading write = (%d, %v), want it landed at 1", leader.result, leader.err)
	}
	if !errors.Is(hungUp.err, coord.ErrUnavailable) || !errors.Is(hungUp.err, context.Canceled) || asked {
		t.Fatalf("the hung-up write = (%d, %v), asked %v: want it withdrawn with its "+
			"hang-up, never decided", hungUp.result, hungUp.err, asked)
	}
	if _, idle := lines.lines[key]; idle {
		t.Fatal("the line outlived its last write: an idle line keeps a node's memory of a key for ever")
	}
}

// probeLines is a line over plain integers, for the cases that are about the
// line rather than about any one record.
func probeLines() *writeLines[int] {
	return &writeLines[int]{codec: codec[int]{
		decode: func(raw []byte) (int, error) {
			var n int
			err := json.Unmarshal(raw, &n)
			return n, err
		},
		encode:  func(n int) ([]byte, error) { return json.Marshal(n) },
		reading: "read the probe", decoding: "decode the probe",
	}}
}

// probeAnswer is what one probe write's caller got: its value, its error, or
// the panic its own goroutine was handed.
type probeAnswer struct {
	n        int
	err      error
	panicked any
}

// probeWrite runs one write of key on its own goroutine — a caller of its own
// — and answers what that caller got, a panic included.
func probeWrite(ctx context.Context, lines *writeLines[int], kv jetstream.KeyValue, js jetstream.JetStream,
	key string, change func(int, bool) (int, bool)) chan probeAnswer {

	out := make(chan probeAnswer, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				out <- probeAnswer{panicked: r}
			}
		}()
		n, err := lines.write(ctx, kv, js, key, key, "write the probe", change)
		out <- probeAnswer{n: n, err: err}
	}()
	return out
}

// add is a probe change that adds n to whatever the key holds.
func add(n int) func(int, bool) (int, bool) {
	return func(stored int, _ bool) (int, bool) { return stored + n, true }
}

func awaitProbe(t *testing.T, what string, ch chan probeAnswer) probeAnswer {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(30 * time.Second):
		t.Fatalf("%s was never answered: a panic in its batch left the line with nobody to run it", what)
		return probeAnswer{}
	}
}

// A WRITE WHOSE CHANGE PANICS FAILS ALONE, ON ITS OWN CALLER'S GOROUTINE.
//
// Other callers' changes run on whichever goroutine leads their batch, so a
// panic in one of them used to unwind that leader — another caller — and leave
// the line led by nobody: every write in the batch, and every later write on
// the key on this node, waiting for ever, the engine's meter among them, whose
// charges run on a context nothing cancels. Recovered, the panicking write is
// answered with its panic, which its own caller's goroutine raises again; the
// batch is decided again without it; and the line runs on.
func TestAWriteWhoseChangePanicsFailsAlone(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	lines := probeLines()
	key := encodeKey("probe-panic")
	held := holdFirstWrite(store.budgets, key)

	first := probeWrite(t.Context(), lines, held, store.js, key, add(1))
	<-held.reached
	before := probeWrite(t.Context(), lines, held, store.js, key, add(10))
	waitQueued(t, lines, key, 1)
	panicking := probeWrite(t.Context(), lines, held, store.js, key, func(int, bool) (int, bool) {
		panic("the change's own panic")
	})
	waitQueued(t, lines, key, 2)
	after := probeWrite(t.Context(), lines, held, store.js, key, add(100))
	waitQueued(t, lines, key, 3)
	close(held.release)

	if got := awaitProbe(t, "the held write", first); got.err != nil || got.panicked != nil || got.n != 1 {
		t.Fatalf("the held write = %+v, want it landed at 1", got)
	}
	if got := awaitProbe(t, "the write queued before the panic", before); got.err != nil ||
		got.panicked != nil || got.n != 11 {
		t.Fatalf("the write queued before the panicking one = %+v, want it landed at 11: "+
			"another caller's panic is not its own", got)
	}
	got := awaitProbe(t, "the panicking write", panicking)
	if got.panicked == nil || !strings.Contains(fmt.Sprint(got.panicked), "the change's own panic") {
		t.Fatalf("the panicking write = %+v, want its own panic raised on its own caller's goroutine", got)
	}
	if got := awaitProbe(t, "the write queued after the panic", after); got.err != nil ||
		got.panicked != nil || got.n != 111 {
		t.Fatalf("the write queued after the panicking one = %+v, want it landed at 111: "+
			"decided as though the panicking write had never been", got)
	}
	later := probeWrite(t.Context(), lines, store.budgets, store.js, key, add(1000))
	if got := awaitProbe(t, "a later write on the key", later); got.err != nil || got.n != 1111 {
		t.Fatalf("a later write on the key = %+v, want it landed at 1111: the line runs on", got)
	}
}

// encodeTrap is the probe's codec with an encode that panics on one value: a
// panic in the line's own work rather than in any caller's change.
func encodeTrap(lines *writeLines[int], trap int) {
	encode := lines.codec.encode
	lines.codec.encode = func(n int) ([]byte, error) {
		if n == trap {
			panic(fmt.Sprintf("the encoder's own panic at %d", n))
		}
		return encode(n)
	}
}

// AND A PANIC IN THE LINE'S OWN WORK FAILS ITS BATCH, NEVER THE LINE: every
// write the batch carried is answered with an error naming the panic — nothing
// it asked for landed — the goroutine that led it raises the panic again so the
// fault surfaces, and the next write on the key leads a line that runs.
//
// THE ERROR NAMES WHAT PANICKED, the line's own work, with the stack it
// panicked on. A write's change that panics is recovered before the line's
// recover can see it, so the line's recover only ever catches the line's own
// code — and an error blaming another write would send whoever read it
// looking for a change closure that never panicked.
func TestAPanicInABatchFailsTheBatchNotTheLine(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	lines := probeLines()
	encodeTrap(lines, 11)
	key := encodeKey("probe-encode")
	held := holdFirstWrite(store.budgets, key)

	first := probeWrite(t.Context(), lines, held, store.js, key, add(1))
	<-held.reached
	leading := probeWrite(t.Context(), lines, held, store.js, key, add(4))
	waitQueued(t, lines, key, 1)
	carried := probeWrite(t.Context(), lines, held, store.js, key, add(6))
	waitQueued(t, lines, key, 2)
	close(held.release)

	if got := awaitProbe(t, "the held write", first); got.err != nil || got.n != 1 {
		t.Fatalf("the held write = %+v, want it landed at 1", got)
	}
	if got := awaitProbe(t, "the batch's leader", leading); got.panicked == nil ||
		!strings.Contains(fmt.Sprint(got.panicked), "the encoder's own panic") {
		t.Fatalf("the goroutine leading the batch = %+v, want the encoder's panic raised there", got)
	}
	got := awaitProbe(t, "the write the batch carried", carried)
	if got.panicked != nil || !errors.Is(got.err, coord.ErrUnavailable) {
		t.Fatalf("the write the batch carried = %+v, want an unknown outcome (coord.ErrUnavailable), "+
			"never the panic itself on a caller whose change did not raise it", got)
	}
	var own *linePanic
	if !errors.As(got.err, &own) {
		t.Fatalf("the write the batch carried failed with %q, want the line's own panic (*linePanic): "+
			"no write's change panicked", got.err)
	}
	if !strings.Contains(fmt.Sprint(own.value), "the encoder's own panic at 11") {
		t.Fatalf("the line's panic carries %v, want the encoder's own panic value", own.value)
	}
	if text := got.err.Error(); !strings.Contains(text, "panicked in its own work") ||
		!strings.Contains(text, "never in a write's change") {
		t.Fatalf("the write the batch carried failed with %q, want it to name the line's own work "+
			"as what panicked, and no write's change", text)
	}
	// The stack it panicked on, never the recover's: the encoder's frame is in
	// it, as it is in the panic the leader raises.
	if text := got.err.Error(); !strings.Contains(text, "encodeTrap") {
		t.Fatalf("the write the batch carried failed with %q, want the stack the line panicked on, "+
			"which names the encoder's frame", text)
	}
	later := probeWrite(t.Context(), lines, store.budgets, store.js, key, add(100))
	if got := awaitProbe(t, "a later write on the key", later); got.err != nil || got.n != 101 {
		t.Fatalf("a later write on the key = %+v, want it landed at 101 over the one write "+
			"that landed: the line runs on and the failed batch wrote nothing", got)
	}
}

// gatedWaits makes a line's waits the case's: each wait the line asks for is
// sent on asked, and is over when the case sends on over.
type gatedWaits struct {
	asked chan time.Duration
	over  chan time.Time
}

func gateWaits[T any](lines *writeLines[T]) gatedWaits {
	g := gatedWaits{asked: make(chan time.Duration, 16), over: make(chan time.Time)}
	lines.after = func(d time.Duration) <-chan time.Time {
		g.asked <- d
		return g.over
	}
	return g
}

// awaitWait is the next wait the line asks for.
func (g gatedWaits) awaitWait(t *testing.T, what string) time.Duration {
	t.Helper()
	select {
	case d := <-g.asked:
		return d
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: the line never asked for its wait", what)
		return 0
	}
}

// rememberedLine is key's line idle at level, as a line that lost and landed
// before leaves it, on a bucket whose round trip is an hour — so no window
// passes while a case runs and every wait is one the case ends.
func rememberedLine[T any](lines *writeLines[T], key string, level int, at time.Time) {
	lines.trips.observe(time.Hour)
	lines.trips.observe(time.Hour)
	lines.lines = map[string]*writeLine[T]{key: {level: level, idle: at}}
	lines.now = func() time.Time { return at }
}

// batchesOf records the batches the line settles on key.
func batchesOf[T any](lines *writeLines[T], key string) func() [][2]int {
	var (
		mu  sync.Mutex
		got [][2]int
	)
	lines.observe = func(k string, batch, attempts int) {
		if k != key {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		got = append(got, [2]int{batch, attempts})
	}
	return func() [][2]int {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// A WAIT IS MADE BEFORE A BATCH FORMS, NEVER AFTER.
//
// A line whose level outlived its last batch waits a window before its next
// attempt, and it used to form that batch first: everything that arrived
// during the wait missed it and queued for the next, which waited a window
// again — more compare-and-swaps for smaller batches, and a write taken, and
// so beyond its caller's reach, for the whole of a wait before anything was
// sent. Waited first, every write that arrives meanwhile is in the attempt that
// follows.
func TestWritesQueuedDuringAWaitJoinTheBatchAfterIt(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	lines := probeLines()
	key := encodeKey("probe-wait")
	rememberedLine(lines, key, 3, time.Now())
	waits := gateWaits(lines)
	batches := batchesOf(lines, key)

	first := probeWrite(t.Context(), lines, store.budgets, store.js, key, add(1))
	if d := waits.awaitWait(t, "the first write on a remembered line"); d <= 0 {
		t.Fatalf("a line remembered at level 3 waited %v before its first attempt, want a window", d)
	}
	var rest []chan probeAnswer
	for i, n := range []int{10, 100, 1000} {
		rest = append(rest, probeWrite(t.Context(), lines, store.budgets, store.js, key, add(n)))
		waitQueued(t, lines, key, i+2)
	}
	waits.over <- time.Now()

	for i, ch := range append([]chan probeAnswer{first}, rest...) {
		want := []int{1, 11, 111, 1111}[i]
		if got := awaitProbe(t, fmt.Sprintf("write %d", i), ch); got.err != nil || got.n != want {
			t.Fatalf("write %d = %+v, want it landed at %d", i, got, want)
		}
	}
	if got := batches(); len(got) != 1 || got[0] != [2]int{4, 1} {
		t.Fatalf("the line settled %v, want ONE batch of all four writes in one attempt: "+
			"the writes that arrived during the wait are the batch after it", got)
	}
}

// AND A WAIT HOLDS NOTHING ITS CALLER CANNOT TAKE BACK. A leader whose caller
// hangs up during the wait is withdrawn — counted nowhere — and the line goes
// to the next write, which waits out WHAT IS LEFT of the same window rather
// than a window anew.
func TestALeaderWhoseCallerHangsUpDuringAWaitIsWithdrawn(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	lines := probeLines()
	key := encodeKey("probe-hangup")
	rememberedLine(lines, key, 3, time.Now())
	waits := gateWaits(lines)

	gone, hangUp := context.WithCancel(t.Context())
	defer hangUp()
	leader := probeWrite(gone, lines, store.budgets, store.js, key, add(1))
	first := waits.awaitWait(t, "the leader")
	next := probeWrite(t.Context(), lines, store.budgets, store.js, key, add(10))
	waitQueued(t, lines, key, 2)
	hangUp()
	if got := awaitProbe(t, "the leader whose caller hung up", leader); !errors.Is(got.err, coord.ErrUnavailable) ||
		!errors.Is(got.err, context.Canceled) {
		t.Fatalf("the leader whose caller hung up during its wait = %+v, want it withdrawn", got)
	}
	if second := waits.awaitWait(t, "the write the line passed to"); second != first {
		t.Fatalf("the write the line passed to waited %v, want the %v left of the same window: "+
			"the clock has not moved", second, first)
	}
	waits.over <- time.Now()
	if got := awaitProbe(t, "the next write", next); got.err != nil || got.n != 10 {
		t.Fatalf("the next write = %+v, want it landed at 10: the withdrawn write counted nowhere", got)
	}
}

// scriptedUpdates is the budgets bucket with the Update calls of one key scripted,
// counted from one: a step holds its call until release closes (once reached
// is closed), and then loses it as another writer's revision or makes it.
type scriptedUpdates struct {
	jetstream.KeyValue
	key   string
	calls *atomic.Int64
	steps map[int64]updateStep
}

type updateStep struct {
	reached, release chan struct{}
	lose             bool
}

func (k scriptedUpdates) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key != k.key {
		return k.KeyValue.Update(ctx, key, value, revision)
	}
	step, ok := k.steps[k.calls.Add(1)]
	if ok && step.reached != nil {
		close(step.reached)
		<-step.release
	}
	if ok && step.lose {
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	return k.KeyValue.Update(ctx, key, value, revision)
}

func heldStep(lose bool) updateStep {
	return updateStep{reached: make(chan struct{}), release: make(chan struct{}), lose: lose}
}

// A WRITE A LOST ATTEMPT CARRIED IS WITHDRAWN BEFORE THE NEXT.
//
// A lost attempt wrote nothing, so between it and the next a write is no more
// taken than one that never left the queue: a caller who hung up while it was
// in flight is answered as the loss comes back, and one who hangs up during the
// wait that follows is answered at once — neither held for the line's next
// attempt, neither counted.
func TestAWriteALostAttemptCarriedIsWithdrawnBeforeTheNext(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	lines := probeLines()
	key := encodeKey("probe-lost")
	lines.trips.observe(time.Hour)
	lines.trips.observe(time.Hour)
	waits := gateWaits(lines)
	first, lost := heldStep(false), heldStep(true)
	kv := scriptedUpdates{KeyValue: store.budgets, key: key, calls: new(atomic.Int64),
		steps: map[int64]updateStep{2: first, 3: lost}}
	// The key's first write is its create — an Update at no revision, and
	// the script's first call — and the second the held one the case's batch
	// queues behind.
	if got := awaitProbe(t, "the create", probeWrite(t.Context(), lines, kv, store.js, key, add(1))); got.err != nil {
		t.Fatalf("create the probe: %+v", got)
	}

	holder := probeWrite(t.Context(), lines, kv, store.js, key, add(1))
	<-first.reached
	stays := probeWrite(t.Context(), lines, kv, store.js, key, add(10))
	waitQueued(t, lines, key, 1)
	inFlight, hangUpInFlight := context.WithCancel(t.Context())
	defer hangUpInFlight()
	leavesInFlight := probeWrite(inFlight, lines, kv, store.js, key, add(100))
	waitQueued(t, lines, key, 2)
	duringWait, hangUpDuringWait := context.WithCancel(t.Context())
	defer hangUpDuringWait()
	leavesDuringWait := probeWrite(duringWait, lines, kv, store.js, key, add(1000))
	waitQueued(t, lines, key, 3)
	close(first.release)
	if got := awaitProbe(t, "the holder", holder); got.err != nil || got.n != 2 {
		t.Fatalf("the holder = %+v, want it landed at 2", got)
	}

	<-lost.reached
	hangUpInFlight()
	close(lost.release)
	waits.awaitWait(t, "the lost attempt's line")
	answered := func(what string, ch chan probeAnswer) {
		t.Helper()
		select {
		case got := <-ch:
			if !errors.Is(got.err, coord.ErrUnavailable) || !errors.Is(got.err, context.Canceled) {
				t.Fatalf("%s = %+v, want it withdrawn with its hang-up", what, got)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s was not answered while the line waited: held for the next attempt", what)
		}
	}
	answered("the write whose caller hung up while its attempt was in flight", leavesInFlight)
	hangUpDuringWait()
	answered("the write whose caller hung up during the wait after its lost attempt", leavesDuringWait)

	waits.over <- time.Now()
	if got := awaitProbe(t, "the write that stayed", stays); got.err != nil || got.n != 12 {
		t.Fatalf("the write that stayed = %+v, want it landed at 12: neither withdrawn write "+
			"counted", got)
	}
}

// A ROUND TRIP OUTLIVES ITS LINE. The estimate a window is counted in is the
// bucket's, refined by every attempt this node makes there, so a line that
// went idle and comes back to a first loss an outlier slow does not set its
// window by that one attempt: the loss moves the estimate by an eighth of
// itself, and the window is two of those round trips, never two of the
// outlier's.
func TestARoundTripOutlivesItsLine(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	lines := probeLines()
	key := encodeKey("probe-outlier")
	for range 4 {
		if got := awaitProbe(t, "a write that lands",
			probeWrite(t.Context(), lines, store.budgets, store.js, key, add(1))); got.err != nil {
			t.Fatalf("a write that lands: %+v", got)
		}
	}
	lines.mu.Lock()
	_, kept := lines.lines[key]
	lines.mu.Unlock()
	if kept {
		t.Fatal("a line that never lost outlived its last write")
	}
	before := lines.trips.estimate()
	const stall = 2 * time.Second
	if before == 0 || 5*before > stall/10 {
		t.Fatalf("the bucket's round trip is %v, too near the %v stall for the case to tell "+
			"one from the other", before, stall)
	}
	stalled := heldStep(true)
	kv := scriptedUpdates{KeyValue: store.budgets, key: key, calls: new(atomic.Int64),
		steps: map[int64]updateStep{1: stalled}}
	frozen := time.Now()
	lines.now = func() time.Time { return frozen }
	waits := gateWaits(lines)

	write := probeWrite(t.Context(), lines, kv, store.js, key, add(1))
	<-stalled.reached
	time.Sleep(stall)
	close(stalled.release)
	got := waits.awaitWait(t, "the line whose first attempt was an outlier")
	ceiling := (2 * (before + before/casRTTGain)) * 19 / 10
	if got > ceiling {
		t.Fatalf("after a first loss of %v on a bucket whose round trip is %v the line waits %v, "+
			"over the %v two smoothed round trips allow: the outlier set the window", stall, before,
			got, ceiling)
	}
	waits.over <- frozen
	if answer := awaitProbe(t, "the write", write); answer.err != nil || answer.n != 5 {
		t.Fatalf("the write = %+v, want it landed at 5", answer)
	}
}

// A LEVEL OUTLIVES ITS LINE'S IDLENESS. A line that lost and then landed is
// kept when its queue empties, remembering how contended the key was, so the
// next write after a pause waits a window before its first attempt rather than
// going straight at a key the other nodes are still waiting out — and once it
// has sat idle through every window, it is forgotten and swept.
func TestALevelOutlivesItsLinesIdleness(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	lines := probeLines()
	key := encodeKey("probe-level")
	if got := awaitProbe(t, "the create",
		probeWrite(t.Context(), lines, store.budgets, store.js, key, add(1))); got.err != nil {
		t.Fatalf("create the probe: %+v", got)
	}
	steps := map[int64]updateStep{}
	for call := range int64(3) {
		steps[call+1] = updateStep{lose: true}
	}
	kv := scriptedUpdates{KeyValue: store.budgets, key: key, calls: new(atomic.Int64), steps: steps}
	lines.trips.observe(time.Hour)
	lines.trips.observe(time.Hour)
	now := time.Now()
	lines.now = func() time.Time { return now }
	waits := make(chan time.Duration, fleetCASAttempts)
	lines.after = func(d time.Duration) <-chan time.Time {
		waits <- d
		over := make(chan time.Time, 1)
		over <- now
		return over
	}

	if got := awaitProbe(t, "the contended write",
		probeWrite(t.Context(), lines, kv, store.js, key, add(1))); got.err != nil || got.n != 2 {
		t.Fatalf("the contended write = %+v, want it landed at 2 after three losses", got)
	}
	for range 3 {
		<-waits
	}
	lines.mu.Lock()
	ln, kept := lines.lines[key]
	level := 0
	if kept {
		level = ln.level
	}
	lines.mu.Unlock()
	if !kept || level != 2 {
		t.Fatalf("after three losses and a landing the idle line is kept %v at level %d, "+
			"want it kept at 2", kept, level)
	}

	if got := awaitProbe(t, "the write after a pause",
		probeWrite(t.Context(), lines, store.budgets, store.js, key, add(10))); got.err != nil || got.n != 12 {
		t.Fatalf("the write after a pause = %+v, want it landed at 12", got)
	}
	select {
	case d := <-waits:
		if d <= 0 {
			t.Fatalf("the write after a pause waited %v, want a window", d)
		}
	default:
		t.Fatal("the write after a pause went straight at the key: the line forgot its level " +
			"when its queue emptied")
	}

	// A thousand hours on, every window of its level has passed. The sweep
	// runs once the map has doubled since the last one — here, the remembered
	// line, a line held mid-write and a third written beside them.
	now = now.Add(1000 * time.Hour)
	busy := encodeKey("probe-busy")
	held := holdFirstWrite(store.budgets, busy)
	busyWrite := probeWrite(t.Context(), lines, held, store.js, busy, add(1))
	<-held.reached
	if got := awaitProbe(t, "a write on a third key",
		probeWrite(t.Context(), lines, store.budgets, store.js, encodeKey("probe-third"), add(1))); got.err != nil {
		t.Fatalf("a write on a third key: %+v", got)
	}
	lines.mu.Lock()
	_, kept = lines.lines[key]
	lines.mu.Unlock()
	if kept {
		t.Fatal("a line idle through every window of its level was never swept")
	}
	close(held.release)
	if got := awaitProbe(t, "the held write", busyWrite); got.err != nil {
		t.Fatalf("the held write: %+v", got)
	}
}
