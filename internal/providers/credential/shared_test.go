package credential

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The fleet's half of the pool: what a bench publishes, and what a pull does
// with what a peer published.
//
// Every case here failed before this wiring existed, and none of them by
// erroring. internal/coord shipped the Cooldowns contract, three backends
// implemented it and the certified suite passed — and nothing in the engine
// ever called it, so four nodes each paid their own 429 to learn what the
// first already knew.

// fakeWall is the wall clock at the fleet boundary, driven by hand.
//
// SEPARATE from fakeClock, and that separation is the point: the pool's own
// arithmetic is monotonic and the ledger's is wall-clock, so a test that drove
// both from one counter could not tell a correct conversion from an
// implementation that simply mixed the two units.
type fakeWall struct {
	mu sync.Mutex
	at time.Time
}

func (w *fakeWall) now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

func (w *fakeWall) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.at = w.at.Add(d)
}

// fakeLedger is an in-memory [Shared], with the two failure modes the contract
// names.
type fakeLedger struct {
	mu       sync.Mutex
	cooled   map[string]time.Time
	coolErr  error
	sinceErr error
	cools    int
	// leaky makes Since hand back records that have already lapsed —
	// which the contract says it must not, and which the certified
	// backends do not. [Shared] is an interface a CALLER supplies, so the
	// pool has to survive one that gets it wrong; nothing else in the
	// suite can put such a record in front of it.
	leaky bool

	// lastDeadline records what the pool bounded its write by.
	lastDeadline time.Time
	hadDeadline  bool
}

func newLedger() *fakeLedger { return &fakeLedger{cooled: map[string]time.Time{}} }

func (f *fakeLedger) Cool(ctx context.Context, key string, until time.Time) error {
	// HONOURED, not ignored. A store that answered a dead context would
	// make the detach of the caller's cancellation untestable — and that
	// detach is the whole reason a bench survives a cancelled turn.
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, hasDeadline := ctx.Deadline()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastDeadline, f.hadDeadline = deadline, hasDeadline
	f.cools++
	if f.coolErr != nil {
		return f.coolErr
	}
	f.cooled[key] = until
	return nil
}

func (f *fakeLedger) Since(ctx context.Context, now time.Time) (map[string]time.Time, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sinceErr != nil {
		return nil, f.sinceErr
	}
	out := map[string]time.Time{}
	for k, v := range f.cooled {
		if !f.leaky && !v.After(now) {
			continue
		}
		out[k] = v
	}
	return out, nil
}

func (f *fakeLedger) snapshot() map[string]time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.cooled)
}

func (f *fakeLedger) put(key string, until time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cooled[key] = until
}

// sharedPool is a pool wired to a ledger, with both clocks under the test's
// control.
func sharedPool(t *testing.T, scope string, keys []string, policy Policy) (
	*Pool, *fakeClock, *fakeWall, *fakeLedger,
) {
	t.Helper()
	clock, wall := &fakeClock{}, &fakeWall{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	p := New(Options{Keys: keys, Policy: policy, Clock: clock.now, Wall: wall.now})
	ledger := newLedger()
	p.Share(scope, ledger)
	return p, clock, wall, ledger
}

// A BENCH IS PUBLISHED, as a wall-clock deadline the peer can read.
//
// The deadline is what crosses, not the duration: a peer receiving "cool for
// an hour" would restart the hour from whenever it happened to read the record,
// so a key benched once would stay benched as long as anyone kept pulling.
func TestABenchIsPublishedAsAnInstantThePeerCanRead(t *testing.T) {
	t.Parallel()
	p, _, wall, ledger := sharedPool(t, "zulu", []string{"k"}, Policy{RateLimit: time.Hour})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)

	got := ledger.snapshot()
	want := wall.now().Add(time.Hour)
	until, ok := got["zulu:"+Hint("k")]
	if !ok {
		t.Fatalf("nothing was published; ledger = %v", got)
	}
	if !until.Equal(want) {
		t.Fatalf("published %v, want the bench's wall-clock deadline %v", until, want)
	}
}

// THE KEY IS NEVER IN THE LEDGER. It is a store peers read, so a credential
// legible in it is a credential leaked to everything that can read the fleet's
// coordination state.
func TestThePublishedRecordCarriesTheHintNotTheKey(t *testing.T) {
	t.Parallel()
	secret := "sk-ant-not-a-real-key-0123456789"
	p, _, _, ledger := sharedPool(t, "zulu", []string{secret}, Policy{RateLimit: time.Hour})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)

	for key := range ledger.snapshot() {
		if key == "zulu:"+secret || key == secret {
			t.Fatalf("the ledger holds the credential itself: %q", key)
		}
	}
	if _, ok := ledger.snapshot()["zulu:"+Hint(secret)]; !ok {
		t.Fatalf("the hinted record is missing; ledger = %v", ledger.snapshot())
	}
}

// A TRANSPORT FAILURE PUBLISHES NOTHING, because it benches nothing. Telling
// the fleet a healthy key is cooling on a network blip is how four nodes talk
// each other out of every credential they have.
func TestAFailureThatBenchesNothingPublishesNothing(t *testing.T) {
	t.Parallel()
	p, _, _, ledger := sharedPool(t, "zulu", []string{"k"}, Policy{RateLimit: time.Hour})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindTimeout, 0)

	if got := ledger.snapshot(); len(got) != 0 {
		t.Fatalf("a transport failure published %v", got)
	}
}

// A CANCELLED REQUEST STILL PUBLISHES. The most common way for the caller's
// context to be dead by the time a bench is recorded is the turn being
// cancelled — which is exactly when losing the record makes every peer
// rediscover the same refusal.
func TestABenchOnACancelledContextIsStillPublished(t *testing.T) {
	t.Parallel()
	p, _, _, ledger := sharedPool(t, "zulu", []string{"k"}, Policy{RateLimit: time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	lease, _ := p.Acquire()
	lease.Fail(ctx, llm.KindRateLimit, 0)

	if _, ok := ledger.snapshot()["zulu:"+Hint("k")]; !ok {
		t.Fatal("a cancelled context swallowed the cooldown the fleet needed")
	}
}

// A LEDGER THAT REFUSES THE WRITE COSTS THE SHARING AND NOTHING ELSE. The
// local bench is the one that stops this node calling a refused key, and it
// must not depend on a coordination store being up.
func TestAnUnwritableLedgerStillBenchesLocally(t *testing.T) {
	t.Parallel()
	p, _, _, ledger := sharedPool(t, "zulu", []string{"k"}, Policy{RateLimit: time.Hour})
	ledger.mu.Lock()
	ledger.coolErr = errors.New("store unreachable")
	ledger.mu.Unlock()

	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)

	if _, ok := p.Acquire(); ok {
		t.Fatal("the key stayed live because sharing failed")
	}
}

// A POOL WITH NO KEYS PUBLISHES NOTHING. Its placeholder entry has an empty
// hint, and "the empty credential is cooling" is a record every keyless pool
// in the fleet would collide on while telling nobody anything.
func TestAKeylessPoolPublishesNothing(t *testing.T) {
	t.Parallel()
	clock, wall := &fakeClock{}, &fakeWall{at: time.Now()}
	p := New(Options{Clock: clock.now, Wall: wall.now})
	ledger := newLedger()
	p.Share("zulu", ledger)

	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindAuth, 0)

	ledger.mu.Lock()
	calls := ledger.cools
	ledger.mu.Unlock()
	if calls != 0 {
		t.Fatalf("a keyless pool made %d ledger writes", calls)
	}
}

// AN UNSHARED POOL BEHAVES EXACTLY AS IT DID BEFORE ANY OF THIS. That is the
// single-node deployment, and it must not need a coordination store.
func TestAnUnsharedPoolNeitherPublishesNorPulls(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k"}, Policy{RateLimit: time.Hour})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)

	applied, err := p.Refresh(t.Context())
	if err != nil || applied != 0 {
		t.Fatalf("Refresh() on an unshared pool = %d, %v; want 0, nil", applied, err)
	}
	if got := coolingOf(t, p, Hint("k")); got != time.Hour {
		t.Fatalf("Cooling = %v, want the local bench untouched", got)
	}
}

// THE PULL IS THE WHOLE POINT: a key a PEER benched must not be leased here.
//
// This is the case the subsystem exists for, and the one nothing exercised
// before — with the pull missing, both nodes call the vendor and both get a
// 429, which is the cost this pays a coordination read to avoid.
func TestAPeersBenchStopsThisNodeLeasingTheKey(t *testing.T) {
	t.Parallel()
	p, _, wall, ledger := sharedPool(t, "zulu", []string{"k0", "k1"}, Policy{RateLimit: time.Hour})
	ledger.put("zulu:"+Hint("k0"), wall.now().Add(30*time.Minute))

	applied, err := p.Refresh(t.Context())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if applied != 1 {
		t.Fatalf("Refresh() applied %d, want 1", applied)
	}
	lease, ok := p.Acquire()
	if !ok {
		t.Fatal("Acquire() found nothing after one of two keys was benched")
	}
	if lease.Key() != "k1" {
		t.Fatalf("leased %q, want the key the fleet has NOT benched", lease.Key())
	}
	if got := coolingOf(t, p, Hint("k0")); got != 30*time.Minute {
		t.Fatalf("Cooling = %v, want the peer's remaining 30m", got)
	}
}

// THE DEADLINE IS CONVERTED, not copied. The ledger speaks wall-clock instants
// and the pool speaks elapsed monotonic time; a peer's record read half way
// through its life must bench for what is LEFT of it.
func TestAPeersDeadlineIsConvertedToWhatIsLeftOfIt(t *testing.T) {
	t.Parallel()
	p, clock, wall, ledger := sharedPool(t, "zulu", []string{"k"}, Policy{})
	ledger.put("zulu:"+Hint("k"), wall.now().Add(time.Hour))
	// Both clocks move together, as two clocks on one machine do.
	wall.advance(45 * time.Minute)
	clock.advance(45 * time.Minute)

	if _, err := p.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := coolingOf(t, p, Hint("k")); got != 15*time.Minute {
		t.Fatalf("Cooling = %v, want the 15m remaining of the peer's hour", got)
	}
}

// A LAPSED RECORD IS NOT A BENCH. The bucket's own retention is coarse (a day,
// sized to the longest cooldown anything sets), so a record outlives the
// cooldown it describes by design — and a ledger that handed one back would
// otherwise be counted as having benched a key while leaving it leasable,
// which reads as sharing working when it is not.
func TestALapsedPeerRecordBenchesNothing(t *testing.T) {
	t.Parallel()
	p, clock, wall, ledger := sharedPool(t, "zulu", []string{"k"}, Policy{})
	ledger.mu.Lock()
	ledger.leaky = true
	ledger.mu.Unlock()
	ledger.put("zulu:"+Hint("k"), wall.now().Add(time.Minute))
	wall.advance(2 * time.Minute)
	clock.advance(2 * time.Minute)

	applied, err := p.Refresh(t.Context())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if applied != 0 {
		t.Fatalf("Refresh() applied %d lapsed records, want 0", applied)
	}
	if _, ok := p.Acquire(); !ok {
		t.Fatal("a lapsed record benched the key")
	}
}

// REFRESH EXTENDS, NEVER SHORTENS. A peer's record is evidence a key is
// refused; the absence of a longer one is not evidence it works. A pool whose
// own 429 no peer heard about must not be talked out of it.
func TestRefreshNeverShortensThisNodesOwnBench(t *testing.T) {
	t.Parallel()
	p, _, wall, ledger := sharedPool(t, "zulu", []string{"k"}, Policy{RateLimit: time.Hour})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)
	// A peer says one minute — a Retry-After hint on its own call.
	ledger.put("zulu:"+Hint("k"), wall.now().Add(time.Minute))

	if _, err := p.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := coolingOf(t, p, Hint("k")); got != time.Hour {
		t.Fatalf("Cooling = %v, want this node's own hour kept", got)
	}
}

// AND RE-READING ONE RECORD IS NOT NEW INFORMATION. Both clocks advance
// together, so the same record yields the same deadline every tick; without a
// floor the pool would report a peer-imposed bench on every refresh for as
// long as the record lives.
func TestRefreshIsIdempotentAcrossTicks(t *testing.T) {
	t.Parallel()
	p, clock, wall, ledger := sharedPool(t, "zulu", []string{"k"}, Policy{})
	ledger.put("zulu:"+Hint("k"), wall.now().Add(time.Hour))

	for tick := range 4 {
		applied, err := p.Refresh(t.Context())
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		want := 0
		if tick == 0 {
			want = 1
		}
		if applied != want {
			t.Fatalf("tick %d applied %d, want %d", tick, applied, want)
		}
		// NOT IN LOCKSTEP, and jittering in BOTH directions. Two clocks
		// on one machine disagree by a little and by a different little
		// each tick — NTP slews the wall clock while the monotonic one
		// runs free — so re-reading ONE record yields a target a hair
		// either side of last tick's. Advancing both by the same amount
		// would make this case pass against an implementation with no
		// tolerance at all; drifting one way only would make it pass
		// against one whose tolerance merely absorbed a trend.
		slew := clockSlew
		if tick%2 == 0 {
			slew = -clockSlew
		}
		wall.advance(cooldownTick + slew)
		clock.advance(cooldownTick)
	}
}

// cooldownTick is the cadence the engine pulls on, restated here so the
// idempotence case runs at the interval production actually uses.
const cooldownTick = 15 * time.Second

// clockSlew is how far the two clocks drift apart per tick. Generous — a real
// slew is parts per million — because the property is that ANY drift below
// the tolerance is absorbed, and a value at the edge of realism would pass
// for the wrong reason.
const clockSlew = 300 * time.Millisecond

// THE PUBLISH CARRIES ITS OWN DEADLINE. It runs inside a call that has already
// failed and is about to be retried on another key; a coordination store that
// has stopped answering must not add its own hang to the latency of working
// around a 429.
func TestThePublishIsBounded(t *testing.T) {
	t.Parallel()
	p, _, _, ledger := sharedPool(t, "zulu", []string{"k"}, Policy{RateLimit: time.Hour})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)

	ledger.mu.Lock()
	deadline, ok := ledger.lastDeadline, ledger.hadDeadline
	ledger.mu.Unlock()
	if !ok {
		t.Fatal("the publish ran on a context with no deadline: an unreachable " +
			"store would hang the rotation it is meant to be invisible to")
	}
	if left := time.Until(deadline); left <= 0 || left > publishTimeout {
		t.Fatalf("publish deadline is %v out, want at most %v", left, publishTimeout)
	}
}

// AN UNREADABLE LEDGER CHANGES NOTHING. It is the pre-sharing behaviour, and
// the one that cannot make a healthy fleet refuse to use any of its
// credentials — the direction internal/coord's Cooldowns doc names.
func TestAnUnreadableLedgerLeavesEveryKeyAsItWas(t *testing.T) {
	t.Parallel()
	p, _, wall, ledger := sharedPool(t, "zulu", []string{"k0", "k1"}, Policy{RateLimit: time.Hour})
	ledger.put("zulu:"+Hint("k0"), wall.now().Add(time.Hour))
	if _, err := p.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	ledger.mu.Lock()
	ledger.sinceErr = errors.New("store unreachable")
	ledger.mu.Unlock()

	applied, err := p.Refresh(t.Context())
	if err == nil {
		t.Fatal("Refresh() hid an unreadable ledger behind a nil error")
	}
	if applied != 0 {
		t.Fatalf("Refresh() applied %d from a failed read", applied)
	}
	if got := coolingOf(t, p, Hint("k0")); got != time.Hour {
		t.Fatalf("Cooling = %v, want the benched key still benched", got)
	}
	if _, ok := p.Acquire(); !ok {
		t.Fatal("a failed read un-benched nothing but cost the live key too")
	}
}

// THE SCOPE IS LOAD-BEARING. One key listed under two config entries is two
// rate-limit buckets at the vendor — different models, possibly different
// endpoints — so a bare hint would turn one model's burst into a company-wide
// outage.
func TestOnePoolsBenchDoesNotReachAnotherEntryOnTheSameKey(t *testing.T) {
	t.Parallel()
	ledger := newLedger()
	clock, wall := &fakeClock{}, &fakeWall{at: time.Now()}
	opts := Options{Keys: []string{"k"}, Policy: Policy{RateLimit: time.Hour},
		Clock: clock.now, Wall: wall.now}
	fast, smart := New(opts), New(opts)
	fast.Share("fast", ledger)
	smart.Share("smart", ledger)

	lease, _ := fast.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)

	applied, err := smart.Refresh(t.Context())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if applied != 0 {
		t.Fatal("one entry's rate limit benched the same key under another entry")
	}
	if _, ok := smart.Acquire(); !ok {
		t.Fatal("the second entry lost its key to the first entry's quota")
	}
}

// SHARE(nil) DETACHES WITHOUT FORGETTING. An epoch rebuilt on a node that has
// lost its coordination store must stop publishing through a stale handle —
// and must not hand out a key the vendor is still refusing.
func TestDetachingKeepsTheBenchAndStopsPublishing(t *testing.T) {
	t.Parallel()
	p, _, _, ledger := sharedPool(t, "zulu", []string{"k0", "k1"}, Policy{RateLimit: time.Hour})
	first, _ := p.Acquire()
	first.Fail(t.Context(), llm.KindRateLimit, 0)
	ledger.mu.Lock()
	before := ledger.cools
	ledger.mu.Unlock()

	p.Share("zulu", nil)
	second, _ := p.Acquire()
	second.Fail(t.Context(), llm.KindRateLimit, 0)

	ledger.mu.Lock()
	after := ledger.cools
	ledger.mu.Unlock()
	if after != before {
		t.Fatalf("a detached pool wrote to the ledger (%d then %d)", before, after)
	}
	if got := coolingOf(t, p, Hint("k0")); got != time.Hour {
		t.Fatalf("Cooling = %v, want the bench from before the detach", got)
	}
}

// ROTATE PUBLISHES EVERY KEY IT BURNS THROUGH, not just the last. A request
// that exhausts a two-key pool has told the fleet two things, and reporting
// one leaves a peer to rediscover the other.
func TestRotatePublishesEveryKeyItBenches(t *testing.T) {
	t.Parallel()
	p, _, _, ledger := sharedPool(t, "zulu", []string{"k0", "k1"}, Policy{RateLimit: time.Hour})
	_, err := Rotate(t.Context(), p, Identity{Provider: "anthropic", Model: "m"},
		classifier(llm.KindRateLimit, 0),
		func(string) (string, error) { return "", errors.New("429") })
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("Rotate() = %v, want the pool exhausted", err)
	}
	got := ledger.snapshot()
	for _, key := range []string{"k0", "k1"} {
		if _, ok := got["zulu:"+Hint(key)]; !ok {
			t.Errorf("%q was benched locally but never published; ledger = %v", key, got)
		}
	}
}
