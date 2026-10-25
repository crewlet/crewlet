package host

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
)

// THE SPOOL ALLOWANCE'S TURNS, over the allowance alone: who starts and is
// granted room, in what order, and what a wait that ends unserved gives back.
// Every case waits at most a second for what it asserts and bounds its waits
// by a minute, so a request that is never granted is a failure here rather
// than a wait.

// turn is one request's start or room, run off the case's goroutine.
type turn struct {
	t   spoolTurn
	w   spoolWait
	err error
}

// queued runs ask off the case's goroutine and waits until it is answered or
// queued — so the order requests are made in is the order they arrive in.
func queued(t *testing.T, a *spools, what string, ask func() turn) <-chan turn {
	t.Helper()
	before := a.waiters()
	out := make(chan turn, 1)
	go func() { out <- ask() }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case r := <-out:
			// Answered at once: hand the answer back as it came.
			done := make(chan turn, 1)
			done <- r
			return done
		default:
		}
		if a.waiters() > before {
			return out
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s was neither answered nor queued in a second", what)
	return nil
}

// begin starts a body for credential on a that claims claim bytes, asking room
// for need of them.
func begin(ctx context.Context, t *testing.T, a *spools, credential string, claim, need int64,
	wait time.Duration) <-chan turn {

	t.Helper()
	return queued(t, a, credential+"'s start", func() turn {
		st, w, err := a.start(ctx, credential, claim, need, need, wait, nil)
		return turn{st, w, err}
	})
}

// grow asks room for need more bytes of body b, arriving.
func grow(ctx context.Context, t *testing.T, a *spools, b *spoolBody, need int64, wait time.Duration) <-chan turn {
	t.Helper()
	return queued(t, a, b.credential+"'s room", func() turn {
		got, w, err := a.more(ctx, b, need, need, wait)
		return turn{spoolTurn{body: b, got: got}, w, err}
	})
}

// granted fails t unless r is answered granted within a second.
func granted(t *testing.T, what string, r <-chan turn) turn {
	t.Helper()
	select {
	case got := <-r:
		if got.err != nil {
			t.Fatalf("%s: %v", what, got.err)
		}
		return got
	case <-time.After(time.Second):
		t.Fatalf("%s was not granted in a second", what)
	}
	return turn{}
}

// waiting fails t unless r is still unanswered.
func waiting(t *testing.T, what string, r <-chan turn) {
	t.Helper()
	select {
	case got := <-r:
		t.Fatalf("%s was answered (%+v) while it should wait", what, got)
	case <-time.After(20 * time.Millisecond):
	}
}

// spooled is a body of n bytes of credential's that has arrived whole and is
// held — as one is through its sync — until released.
func spooled(ctx context.Context, t *testing.T, a *spools, credential string, n int64) *spoolBody {
	t.Helper()
	b := granted(t, credential+"'s spool", begin(ctx, t, a, credential, n, n, time.Minute)).t.body
	a.ended(b, n)
	return b
}

// holdsCondition reports a body arriving whose claim the bodies younger than
// it leave short — of the node's limit, or its credential's share — which no
// grant may ever make. Worked out from the youngest back, a way of its own
// rather than boundsLocked's, so the two cannot share a mistake.
func (a *spools) holdsCondition() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	younger, ofCredential := int64(0), map[string]int64{}
	for i := len(a.bodies) - 1; i >= 0; i-- {
		b := a.bodies[i]
		switch {
		case b.held > b.claim:
			return fmt.Errorf("body %d holds %d, past its claim of %d", b.seq, b.held, b.claim)
		case b.claim+younger > a.limit:
			return fmt.Errorf("body %d claims %d beside %d the bodies younger than it hold, past the node's %d",
				b.seq, b.claim, younger, a.limit)
		case b.claim+ofCredential[b.credential] > a.share:
			return fmt.Errorf("body %d claims %d beside %d its credential's younger bodies hold, past the "+
				"share's %d", b.seq, b.claim, ofCredential[b.credential], a.share)
		}
		younger += b.held
		ofCredential[b.credential] += b.held
	}
	return nil
}

// A CREDENTIAL'S REQUESTS START IN THE ORDER THEY ARRIVED, and another
// credential's request is never held back behind them: x's first body arriving
// claims its credential's whole share, x's next two wait in order, y's starts
// at once, and as each of x's bodies ENDS — its spool still held, as through
// its sync — the earlier of its waiters starts. And within a credential a
// smaller request that would fit never passes a larger one before it.
//
// Mutation: grant a credential's later waiter past an earlier one its own
// credential holds back (drop block in grantLocked) and the last half fails;
// keep a body among those arriving until its spool is given back (leaveLocked
// in release only) and x's second never starts.
func TestACredentialsRequestsStartInTheOrderTheyArrived(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	a := newSpools(spoolSizes{limit: 1 << 20, share: 1 << 10, request: 1 << 10, grain: 1, places: 16,
		placesPerCredential: 8})
	first := granted(t, "x's first", begin(ctx, t, a, "x", 1<<10, 10, time.Minute)).t.body
	second := begin(ctx, t, a, "x", 1<<10, 10, time.Minute)
	third := begin(ctx, t, a, "x", 1<<10, 10, time.Minute)
	granted(t, "y's request beside x's waiters", begin(ctx, t, a, "y", 1<<10, 10, time.Minute))
	waiting(t, "x's second", second)
	a.ended(first, 10)
	got := granted(t, "x's second, its first body ended though its spool is held", second)
	if got.w.outcome != spoolGranted || !got.t.placed {
		t.Errorf("x's second waited and is counted %q, placed %v; want %q, placed", got.w.outcome, got.t.placed,
			spoolGranted)
	}
	waiting(t, "x's third", third)
	a.ended(got.t.body, 10)
	granted(t, "x's third", third)

	// BY BYTES: z holds 50 and 40 of a 100-byte share in spools that ended;
	// its next asks 30 and waits, and the one after it asks 10, which would
	// fit, and waits behind it until the 40 go.
	b := newSpools(spoolSizes{limit: 1 << 20, share: 100, request: 50, grain: 1, places: 8, placesPerCredential: 8})
	spooled(ctx, t, b, "z", 50)
	forty := spooled(ctx, t, b, "z", 40)
	large := begin(ctx, t, b, "z", 30, 30, time.Minute)
	small := begin(ctx, t, b, "z", 10, 10, time.Minute)
	waiting(t, "z's 30", large)
	waiting(t, "z's 10, behind it", small)
	b.release(forty)
	granted(t, "z's 30", large)
	granted(t, "z's 10", small)
}

// THE NODE'S TURNS GO IN ARRIVAL ORDER ACROSS CREDENTIALS: once a request waits
// for the node — for the room a body arriving claims, or for room the spools
// that ended hold — a later request of another credential that would fit waits
// behind it, so a request is never starved by a stream of others; once the
// node's are given back both are granted.
//
// Mutation: let grantLocked go on past a request the node holds back and the
// later request is granted first.
func TestTheNodesTurnsGoInArrivalOrder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// A CLAIM: each body arriving claims the node's whole allowance.
	a := newSpools(spoolSizes{limit: 100, share: 100, request: 100, grain: 1, places: 8, placesPerCredential: 8})
	x := granted(t, "x's body", begin(ctx, t, a, "x", 100, 10, time.Minute)).t.body
	y := begin(ctx, t, a, "y", 100, 10, time.Minute)
	z := begin(ctx, t, a, "z", 100, 10, time.Minute)
	waiting(t, "y's start", y)
	waiting(t, "z's start, behind y's", z)
	a.ended(x, 10)
	yb := granted(t, "y's start", y).t.body
	waiting(t, "z's start", z)
	a.ended(yb, 10)
	granted(t, "z's start", z)

	// ITS ROOM: x's spools hold 90 of 100; y's 30 waits, and z's 10, which
	// would fit, waits behind it.
	b := newSpools(spoolSizes{limit: 100, share: 100, request: 50, grain: 1, places: 8, placesPerCredential: 8})
	spooled(ctx, t, b, "x", 50)
	forty := spooled(ctx, t, b, "x", 40)
	large := begin(ctx, t, b, "y", 30, 30, time.Minute)
	small := begin(ctx, t, b, "z", 10, 10, time.Minute)
	waiting(t, "y's 30", large)
	waiting(t, "z's 10, behind y's", small)
	b.release(forty)
	granted(t, "y's 30", large)
	granted(t, "z's 10", small)
}

// THE OLDEST BODY'S CLAIM IS KEPT FROM THE REST, so it can always finish: x
// claims 60 and has sent 10, y claims 40 and takes all of it, and z's start
// then WAITS though half the allowance is free — granted, it would leave x
// room for less than its claim — while x's rest is granted at once. Bytes taken
// as they arrive and nothing more, z's start takes room x needs; z's body and
// x's then each hold part of what is left and wait for the other's.
//
// Mutation: grant without the room boundsLocked leaves (drop nodeRoom from
// grantLocked's checks) and z starts at once, and x's rest waits for z; drop
// credentialRoom and w's third starts at once.
func TestTheOldestBodysClaimIsKeptFromTheRest(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	a := newSpools(spoolSizes{limit: 100, share: 100, request: 100, grain: 1, places: 8, placesPerCredential: 8})
	x := granted(t, "x's start", begin(ctx, t, a, "x", 60, 10, time.Minute)).t.body
	y := granted(t, "y's start", begin(ctx, t, a, "y", 40, 10, time.Minute)).t.body
	granted(t, "y's rest, which leaves x its claim", grow(ctx, t, a, y, 30, time.Minute))
	z := begin(ctx, t, a, "z", 20, 1, time.Minute)
	waiting(t, "z's start, with half the allowance free", z)
	if used, _ := a.inUse(); used != 50 {
		t.Fatalf("the allowance holds %d bytes, want x's 10 and y's 40: the case proves nothing", used)
	}
	granted(t, "x's rest", grow(ctx, t, a, x, 50, time.Minute))
	waiting(t, "z's start, the room now x's and y's", z)
	a.ended(x, 60)
	a.ended(y, 40)
	waiting(t, "z's start, the spools that ended holding the room", z)
	a.release(x)
	granted(t, "z's start", z)
	if err := a.holdsCondition(); err != nil {
		t.Error(err)
	}

	// AND A CREDENTIAL'S: the same of one credential's bodies against its
	// share, while another credential's request starts beside them.
	b := newSpools(spoolSizes{limit: 1000, share: 100, request: 100, grain: 1, places: 8, placesPerCredential: 8})
	bx := granted(t, "w's first", begin(ctx, t, b, "w", 60, 10, time.Minute)).t.body
	by := granted(t, "w's second", begin(ctx, t, b, "w", 40, 10, time.Minute)).t.body
	granted(t, "w's second's rest", grow(ctx, t, b, by, 30, time.Minute))
	third := begin(ctx, t, b, "w", 20, 1, time.Minute)
	waiting(t, "w's third, with half its share free", third)
	granted(t, "v's start beside them", begin(ctx, t, b, "v", 20, 1, time.Minute))
	granted(t, "w's first's rest", grow(ctx, t, b, bx, 50, time.Minute))
	b.ended(bx, 60)
	b.ended(by, 40)
	b.release(bx)
	granted(t, "w's third", third)
}

// THE BODIES WAITING FOR ROOM GO OLDEST FIRST, whatever order they asked in:
// y, the younger, asks for more than x's claim leaves it and waits for x to
// end; x then asks for its own rest and is granted at once, ahead of y. Taken
// in the order they asked, x would wait behind y, which waits for x.
//
// Mutation: queue a body's room where it asks (more inserting at a.rooms)
// and x's rest waits behind y until both expire.
func TestTheBodiesWaitingForRoomGoOldestFirst(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	a := newSpools(spoolSizes{limit: 100, share: 100, request: 100, grain: 1, places: 8, placesPerCredential: 8})
	x := granted(t, "x's start", begin(ctx, t, a, "x", 60, 10, time.Minute)).t.body
	y := granted(t, "y's start", begin(ctx, t, a, "y", 50, 10, time.Minute)).t.body
	more := grow(ctx, t, a, y, 35, time.Minute)
	waiting(t, "y's rest, past what x's claim leaves it", more)
	granted(t, "x's rest, asked after y's", grow(ctx, t, a, x, 50, time.Minute))
	waiting(t, "y's rest, x still arriving", more)
	a.ended(x, 60)
	waiting(t, "y's rest, x's spool holding the room", more)
	a.release(x)
	granted(t, "y's rest", more)
}

// A REQUEST THAT CAN START NOW STARTS NOW, whatever waits ahead of it held back
// by its own credential, and takes no place to wait in — so the waiting room
// full of other credentials' waiters refuses only a request that must wait.
//
// Mutation: ask for a place before the queue (place, then grant, in start) and
// z's request, which fits, is refused.
func TestARequestThatCanStartNowStartsNow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	a := newSpools(spoolSizes{limit: 1000, share: 10, request: 10, grain: 1, places: 4, placesPerCredential: 3})
	granted(t, "x's body", begin(ctx, t, a, "x", 10, 10, time.Minute))
	for range 3 {
		begin(ctx, t, a, "x", 10, 10, time.Minute)
	}
	granted(t, "y's body", begin(ctx, t, a, "y", 10, 10, time.Minute))
	begin(ctx, t, a, "y", 10, 10, time.Minute)
	if n := a.placesHeld(); n != 4 {
		t.Fatalf("%d places held, want the room's four: the case proves nothing", n)
	}
	got := granted(t, "z's request, which fits, beside a full waiting room",
		begin(ctx, t, a, "z", 10, 10, time.Minute))
	if got.t.placed || got.w.outcome != "" {
		t.Errorf("z's request started placed %v, counted %q: it waited for nothing", got.t.placed, got.w.outcome)
	}
	if n := a.placesHeld(); n != 4 {
		t.Errorf("%d places held after z's start, want the four it found", n)
	}
	// One that must wait finds no place: y's second, its credential's body
	// arriving, and the room full.
	r := <-begin(ctx, t, a, "y", 10, 10, time.Minute)
	var unserved *spoolUnserved
	if !errors.As(r.err, &unserved) || unserved.reason != spoolRefused || r.w.outcome != spoolRefused {
		t.Errorf("a request that must wait beside a full room answered %v (%q), want refused a place", r.err,
			r.w.outcome)
	}
}

// A REQUEST THAT MUST WAIT IS REFUSED ONLY WHEN THERE IS NO PLACE: its
// credential's quarter of the room taken, or the room full — and a place is
// given back when its request starts and its head is in its spool, or leaves.
func TestARequestThatMustWaitIsRefusedOnlyWithoutAPlace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	refused := func(t *testing.T, what string, r turn) {
		t.Helper()
		var unserved *spoolUnserved
		if !errors.As(r.err, &unserved) || unserved.reason != spoolRefused || r.w.outcome != spoolRefused {
			t.Errorf("%s answered %v (%q), want refused a place", what, r.err, r.w.outcome)
		}
	}
	a := newSpools(spoolSizes{limit: 100, share: 10, request: 10, grain: 1, places: 3, placesPerCredential: 1})
	first := granted(t, "x's body", begin(ctx, t, a, "x", 10, 10, time.Minute)).t.body
	second := begin(ctx, t, a, "x", 10, 10, time.Minute)
	refused(t, "x's third with its one place taken", <-begin(ctx, t, a, "x", 10, 10, time.Minute))
	a.ended(first, 10)
	a.release(first)
	got := granted(t, "x's second", second)
	if n := a.placesHeld(); n != 1 || !got.t.placed {
		t.Fatalf("x's second started placed %v with %d places held: its place is the caller's to give back",
			got.t.placed, n)
	}
	a.unplace("x")
	if n := a.placesHeld(); n != 0 {
		t.Errorf("%d places held once x's second gave its back", n)
	}

	b := newSpools(spoolSizes{limit: 10, share: 10, request: 10, grain: 1, places: 2, placesPerCredential: 2})
	granted(t, "x's body", begin(ctx, t, b, "x", 10, 10, time.Minute))
	begin(ctx, t, b, "y", 5, 5, time.Minute)
	begin(ctx, t, b, "z", 5, 5, time.Minute)
	refused(t, "w's request with the room full", <-begin(ctx, t, b, "w", 5, 5, time.Minute))
}

// A WAIT ENDS AT ITS BOUND, unserved: errSpoolFull saying what held it back,
// counted expired, and its place given back — and a request it held back from
// the node is granted as it leaves.
func TestAWaitEndsAtItsBound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	a := newSpools(spoolSizes{limit: 100, share: 10, request: 10, grain: 1, places: 8, placesPerCredential: 8})
	granted(t, "x's first", begin(ctx, t, a, "x", 10, 1, time.Minute))
	start := time.Now()
	r := <-begin(ctx, t, a, "x", 10, 1, 50*time.Millisecond)
	var unserved *spoolUnserved
	switch {
	case !errors.Is(r.err, errSpoolFull) || !errors.As(r.err, &unserved) || unserved.reason != spoolExpired:
		t.Fatalf("a wait past its bound answered %v, want an expired errSpoolFull", r.err)
	case r.w.outcome != spoolExpired:
		t.Errorf("an expired wait is counted %q", r.w.outcome)
	case time.Since(start) < 50*time.Millisecond:
		t.Errorf("the wait ended after %v, before its bound", time.Since(start))
	}
	const says = "behind this credential's own requests still arriving (1 of its fetches, which may yet need 9 " +
		"of the 10 bytes one credential's may)"
	if unserved == nil || !strings.Contains(unserved.detail, says) {
		t.Errorf("an expired wait says %q, want it to say %q", unserved.detail, says)
	}
	if n, p := a.waiters(), a.placesHeld(); n != 0 || p != 0 {
		t.Errorf("%d requests still wait and %d places are held after the only waiter expired", n, p)
	}

	// A HEAD THAT EXPIRES lets the node's room go to what waited behind it.
	b := newSpools(spoolSizes{limit: 100, share: 100, request: 60, grain: 1, places: 8, placesPerCredential: 8})
	spooled(ctx, t, b, "x", 60)
	expiring := begin(ctx, t, b, "y", 60, 60, 50*time.Millisecond)
	behind := begin(ctx, t, b, "z", 30, 30, time.Minute)
	if r := <-expiring; !errors.Is(r.err, errSpoolFull) {
		t.Fatalf("y's wait answered %v", r.err)
	}
	granted(t, "z's 30, once y's 60 left the queue", behind)
}

// A WAITER WHOSE REQUEST ENDS LEAVES AT ONCE, answered its context's error and
// counted abandoned, and the request behind it is granted as it leaves.
func TestAWaiterWhoseRequestEndsLeavesAtOnce(t *testing.T) {
	t.Parallel()
	a := newSpools(spoolSizes{limit: 100, share: 100, request: 60, grain: 1, places: 8, placesPerCredential: 8})
	spooled(t.Context(), t, a, "x", 60)
	ctx, cancel := context.WithCancel(t.Context())
	leaving := begin(ctx, t, a, "y", 60, 60, time.Minute)
	behind := begin(t.Context(), t, a, "z", 30, 30, time.Minute)
	cancel()
	select {
	case r := <-leaving:
		if !errors.Is(r.err, context.Canceled) || r.w.outcome != spoolAbandoned {
			t.Errorf("a waiter whose request ended answered %v, counted %q", r.err, r.w.outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("a waiter whose request ended was still waiting a second later")
	}
	granted(t, "z's 30, once y's 60 left", behind)
	if p := a.placesHeld(); p != 1 {
		t.Errorf("%d places held, want z's alone", p)
	}
}

// A BODY ARRIVING IS SERVED BEFORE ANY REQUEST YET TO START: x's two bodies
// fill the node beside y's ended spools, z's start waits for room, and x's
// first body then asks for as much and waits AHEAD of it — so the five bytes
// y's smallest spool gives back go to the body already arriving, though z
// asked first.
//
// Mutation: queue a body's room behind the starts (more appending) and z's
// start takes the five bytes, leaving x's body waiting.
func TestABodyArrivingIsServedBeforeARequestYetToStart(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	a := newSpools(spoolSizes{limit: 100, share: 100, request: 30, grain: 1, places: 8, placesPerCredential: 8})
	spooled(ctx, t, a, "y", 25)
	twenty := spooled(ctx, t, a, "y", 20)
	five := spooled(ctx, t, a, "y", 5)
	x := granted(t, "x's first body", begin(ctx, t, a, "x", 30, 25, time.Minute)).t.body
	granted(t, "x's second body", begin(ctx, t, a, "x", 30, 25, time.Minute))
	z := begin(ctx, t, a, "z", 5, 5, time.Minute)
	more := grow(ctx, t, a, x, 5, time.Minute)
	waiting(t, "z's start", z)
	waiting(t, "x's body's room, asked after z's start", more)
	a.release(five)
	granted(t, "x's body's room", more)
	waiting(t, "z's start, behind it", z)
	a.release(twenty)
	granted(t, "z's start", z)
}

// A BODY NEVER TAKES ROOM PAST ITS CLAIM: its claim is room the bodies behind
// it were told was theirs, so asking for more — a body longer than its client
// declared — is refused rather than granted, and a claim past the most one
// request spools is refused at its start.
func TestABodyNeverTakesRoomPastItsClaim(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	a := newSpools(spoolSizes{limit: 1000, share: 1000, request: 100, grain: 1, places: 8, placesPerCredential: 8})
	if _, _, err := a.start(ctx, "x", 10, 11, 11, time.Minute, nil); !errors.Is(err, errPastClaim) {
		t.Errorf("a start asking past its claim answered %v, want errPastClaim", err)
	}
	if _, _, err := a.start(ctx, "x", 101, 1, 1, time.Minute, nil); !errors.Is(err, errSpoolPastCeiling) {
		t.Errorf("a claim past what one request spools answered %v, want errSpoolPastCeiling", err)
	}
	b := granted(t, "x's start", begin(ctx, t, a, "x", 10, 4, time.Minute)).t.body
	if got, _, err := a.more(ctx, b, 4, 100, time.Minute); err != nil || got != 6 {
		t.Errorf("room asked up to 100 past a claim of 10, 4 held, granted %d (%v), want the 6 it leaves", got, err)
	}
	if _, _, err := a.more(ctx, b, 1, 1, time.Minute); !errors.Is(err, errPastClaim) {
		t.Errorf("room past a claim all taken answered %v, want errPastClaim", err)
	}
	if used, _ := a.inUse(); used != 10 {
		t.Errorf("the allowance holds %d bytes for a body that claims 10", used)
	}
}

// EVERY BODY ARRIVES WHOLE, however the requests interleave and however their
// claims run — each claiming what one request spools, as git's undeclared
// bodies do, or exactly its size, as a declared one does: a hundred and twenty
// requests of five credentials, each arriving in pieces with pauses between
// them, on an allowance six of the largest wide — less than the five shares
// together, so the node's room binds as well as each credential's — all start
// and all arrive, none of them expiring, and no grant ever leaves a body's
// claim short. And the allowance is empty when they have gone.
//
// Mutation: grant without the room boundsLocked leaves — drop nodeRoom, or
// credentialRoom, from grantLocked's checks — and a grant leaves a body's
// claim short, and bodies that filled the room between them wait on each
// other until they expire.
func TestEveryBodyArrivesWhole(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const request = 1 << 10
	a := newSpools(spoolSizes{limit: 6 * request, share: 2 * request, request: request, grain: 64, places: 120,
		placesPerCredential: 120})
	rng := rand.New(rand.NewPCG(1, 2))
	type plan struct {
		credential  string
		size, claim int64
		pause, hold time.Duration
	}
	plans := make([]plan, 120)
	for i := range plans {
		p := plan{credential: []string{"x", "y", "w", "v", "u"}[rng.IntN(5)], size: 1 + rng.Int64N(request),
			pause: time.Duration(rng.IntN(200)) * time.Microsecond,
			hold:  time.Duration(rng.IntN(2000)) * time.Microsecond}
		p.claim = request
		if rng.IntN(2) == 0 {
			p.claim = p.size
		}
		plans[i] = p
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2*len(plans))
	for _, p := range plans {
		wg.Go(func() {
			first := min(p.size, 64)
			st, _, err := a.start(ctx, p.credential, p.claim, first, a.ask(0, first, p.claim), 10*time.Second, nil)
			if err != nil {
				errs <- err
				return
			}
			if st.placed {
				a.unplace(p.credential)
			}
			room, size := st.got, first
			for size < p.size {
				time.Sleep(p.pause)
				k := min(64, p.size-size)
				if short := size + k - room; short > 0 {
					got, _, err := a.more(ctx, st.body, short, a.ask(room, short, p.claim), 10*time.Second)
					if err != nil {
						a.release(st.body)
						errs <- err
						return
					}
					room += got
				}
				size += k
				if err := a.holdsCondition(); err != nil {
					errs <- err
				}
			}
			a.ended(st.body, size)
			time.Sleep(p.hold)
			a.release(st.body)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a request did not arrive whole: %v", err)
	}
	if used, open := a.inUse(); used != 0 || open != 0 || a.waiters() != 0 || a.placesHeld() != 0 {
		t.Errorf("the allowance holds %d bytes in %d spools, %d waiters and %d places after every request went",
			used, open, a.waiters(), a.placesHeld())
	}
}

// AN ALLOWANCE A BODY COULD NOT FINISH IN IS REFUSED: sizes under which the
// largest claim is past the node's limit or a credential's share — where even
// the oldest body arriving could wait on nothing but itself — or under which
// nothing could ever arrive, never become an allowance.
func TestAnAllowanceABodyCouldNotFinishInIsRefused(t *testing.T) {
	t.Parallel()
	for _, z := range []spoolSizes{
		{limit: 100, share: 100, request: 101, grain: 1, places: 1, placesPerCredential: 1},
		{limit: 1000, share: 100, request: 101, grain: 1, places: 1, placesPerCredential: 1},
		{limit: 100, share: 100, request: 0, grain: 1, places: 1, placesPerCredential: 1},
		{limit: 100, share: 100, request: 10, grain: 0, places: 1, placesPerCredential: 1},
		{limit: 100, share: -1, request: 10, grain: 1, places: 1, placesPerCredential: 1},
	} {
		if z.check() == nil {
			t.Errorf("sizes %+v pass the check", z)
		}
	}
	if err := (spoolSizes{limit: 60, share: 60, request: 60, grain: 1}).check(); err != nil {
		t.Errorf("sizes whose largest claim is the whole allowance are refused: %v", err)
	}
}
