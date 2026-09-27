package credential

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// cheapParams is a cost a test can afford; the shipped one is asserted in
// password_test.go.
var cheapParams = Params{Memory: 8 * 1024, Time: 1, Threads: 1, KeyLen: 32}

// gated is a hasher whose derivations wait to be let through one at a time, so
// a case decides from outside when each one ends and sees who got a slot.
type gated struct {
	*Hasher
	started chan string   // the password of each derivation, as it starts
	finish  chan struct{} // one send ends one running derivation
}

func newGated(t *testing.T, slots int) *gated {
	t.Helper()
	g := &gated{Hasher: NewHasher(cheapParams, slots),
		started: make(chan string, 64), finish: make(chan struct{})}
	g.work = func(password string, _ []byte, _ Params) []byte {
		g.started <- password
		<-g.finish
		return make([]byte, cheapParams.KeyLen)
	}
	return g
}

// begun is the next derivation to start, or a failure if none does in time.
func (g *gated) begun(t *testing.T) string {
	t.Helper()
	select {
	case p := <-g.started:
		return p
	case <-time.After(30 * time.Second):
		t.Fatal("no derivation started")
		return ""
	}
}

// idle fails if a derivation starts within a moment — long enough, race
// detector included, for one that had a slot to have reached the work.
func (g *gated) idle(t *testing.T, why string) {
	t.Helper()
	select {
	case p := <-g.started:
		t.Fatalf("%s: the derivation for %q started", why, p)
	case <-time.After(200 * time.Millisecond):
	}
}

// verifyIn starts one verification in source's turn and answers when it ends.
func (g *gated) verifyIn(ctx context.Context, source, password string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, _, err := g.Verify(ctx, source, verifierFor(cheapParams), password)
		done <- err
	}()
	return done
}

// verifierFor is a well-formed verifier at params nothing will ever match.
func verifierFor(p Params) string {
	h := &Hasher{params: p}
	return h.encode(make([]byte, SaltLen), make([]byte, p.KeyLen))
}

// A REHASH NOBODY IS WAITING ON NEVER QUEUES FOR THE CAP.
//
// Raising the cost is done after a sign-in has answered, and the cap is full
// exactly when the endpoint is under attack — so a rewrite takes a slot only if
// one is free at once, and answers [ErrSaturated] having derived nothing when
// none is: queued, it would take a slot ahead of the sign-ins arriving behind
// it. And it never runs outside the cap. Mutation: take the slot through a
// source's turn and the rehash blocks while every slot is taken; skip the cap
// and it answers a verifier while every slot is taken.
func TestARehashNeverQueuesForTheCap(t *testing.T) {
	t.Parallel()
	h := NewHasher(cheapParams, 1)
	taken, ok := h.turns.tryTake() // EVERY SLOT TAKEN
	if !ok {
		t.Fatal("an empty cap had no slot")
	}
	answered := make(chan error, 1)
	go func() {
		verifier, err := h.Rehash("a password somebody chose")
		if err == nil && verifier != "" {
			err = errors.New("a verifier was derived with every slot taken")
		}
		answered <- err
	}()
	select {
	case err := <-answered:
		if !errors.Is(err, ErrSaturated) {
			t.Fatalf("with every slot taken the rehash answered %v, want %v",
				err, ErrSaturated)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("with every slot taken the rehash waited for one")
	}
	taken() // A SLOT FREES
	verifier, err := h.Rehash("a password somebody chose")
	if err != nil {
		t.Fatalf("with a slot free the rehash failed: %v", err)
	}
	if ok, stale, err := h.Verify(t.Context(), "", verifier, "a password somebody chose"); !ok ||
		stale || err != nil {
		t.Errorf("the rehashed verifier verified %v, stale %v (%v) — want a "+
			"verifier at this hasher's own cost", ok, stale, err)
	}
	if slots, lanes := h.turns.held(); slots != 0 || lanes != 0 {
		t.Errorf("the rehash left %d slots and %d lanes held", slots, lanes)
	}
}

// A VERIFICATION WAITS FOR THE SAME CAP A HASH DOES.
//
// Each derivation holds the memory cost for as long as it runs, and
// verification is the half an UNAUTHENTICATED caller reaches — every sign-in.
// It used to call argon2 directly, so [VerifyCap] bounded setting a password
// and nothing a stranger could send. With every slot of the cap taken, a
// verification must not run at all until one frees.
//
// Mutation: derive in Verify without taking a turn and the verification
// finishes while every slot is taken.
func TestAVerificationWaitsForTheCapAHashDoes(t *testing.T) {
	t.Parallel()
	h := NewHasher(cheapParams, 1)
	const password = "a password somebody chose"
	verifier, err := h.Hash(t.Context(), "", password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	taken, _ := h.turns.tryTake() // EVERY SLOT TAKEN
	done := make(chan bool, 1)
	go func() {
		ok, _, _ := h.Verify(t.Context(), "203.0.113.7", verifier, password)
		done <- ok
	}()
	// TWO HUNDRED MILLISECONDS is roughly a thousand times what an
	// ungated derivation at this cost takes, race detector included — so a
	// verification that skipped the cap has finished long before it, and
	// one that waits has not by construction.
	select {
	case <-done:
		t.Fatal("a verification ran while every slot of the cap was taken")
	case <-time.After(200 * time.Millisecond):
	}

	taken() // A SLOT FREES
	select {
	case ok := <-done:
		if !ok {
			t.Error("once admitted, the verification refused the right password")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the verification never ran once a slot freed")
	}
}

// ONE SOURCE HOLDS ONE SLOT, HOWEVER MUCH IT SENDS.
//
// The cap was one queue, first come first served, so one address that opened
// a dozen sign-ins at once took every slot and every honest sign-in on the
// node waited behind its whole queue. A source holds at most one turn now: its
// second verification waits for its first to end although slots are free, and
// another source is served from a free slot at once.
//
// Mutations: hand a free slot to a source already holding one and its second
// verification starts beside its first; serve one queue in arrival order and
// the second source waits behind the first's.
func TestOneSourceHoldsOneSlotHoweverMuchItSends(t *testing.T) {
	t.Parallel()
	g := newGated(t, 4)
	ctx := t.Context()
	flood := make([]<-chan error, 0, 6)
	for range 6 {
		flood = append(flood, g.verifyIn(ctx, "198.51.100.23", "flood"))
	}
	if got := g.begun(t); got != "flood" {
		t.Fatalf("the first derivation was %q", got)
	}
	g.idle(t, "a source already holding a slot was given a second")

	other := g.verifyIn(ctx, "203.0.113.9", "colleague")
	if got := g.begun(t); got != "colleague" {
		t.Fatalf("another source's verification waited behind the flood: %q "+
			"started first", got)
	}
	g.finish <- struct{}{} // whichever of the two ends first
	g.finish <- struct{}{}
	if err := <-other; err != nil {
		t.Fatalf("the other source's verification: %v", err)
	}
	// The flood drains one at a time.
	for i := 1; i < len(flood); i++ {
		g.begun(t)
		g.finish <- struct{}{}
	}
	for _, done := range flood {
		if err := <-done; err != nil {
			t.Fatalf("a flood verification: %v", err)
		}
	}
	if slots, lanes := g.turns.held(); slots != 0 || lanes != 0 {
		t.Errorf("a drained cap holds %d slots and %d lanes", slots, lanes)
	}
}

// A FLOOD CANNOT STARVE ANOTHER SOURCE, EVEN WHEN THE CAP IS ONE.
//
// A source whose lane still has something waiting rejoins the BACK of the line
// when its turn ends, so a source that arrives while a flood is queued is
// served at the next free slot — after at most the one derivation the flood
// is running, never after the flood's whole queue.
//
// Mutation: put a source back at the front of the line when its turn ends and
// the other source waits for the whole flood.
func TestAFloodCannotStarveAnotherSource(t *testing.T) {
	t.Parallel()
	g := newGated(t, 1)
	ctx := t.Context()
	var flood []<-chan error
	for range 20 {
		flood = append(flood, g.verifyIn(ctx, "198.51.100.23", "flood"))
	}
	g.begun(t)
	other := g.verifyIn(ctx, "2001:db8:7::1", "colleague")
	g.idle(t, "with the cap held, nothing else may start")
	g.finish <- struct{}{}
	if got := g.begun(t); got != "colleague" {
		t.Fatalf("with a flood of 20 queued at one source, the next slot went "+
			"to %q — the other source waited behind the flood", got)
	}
	g.finish <- struct{}{}
	if err := <-other; err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(flood); i++ {
		g.begun(t)
		g.finish <- struct{}{}
	}
	for _, done := range flood {
		<-done
	}
}

// AN ATTEMPT WHOSE REQUEST WENT AWAY GIVES UP ITS PLACE, AND NOTHING IS
// DERIVED FOR IT.
//
// A derivation waited for a slot with no context, so a flood whose clients had
// all hung up still ran every one of its derivations, one slot each, ahead of
// whoever came next. A waiting attempt leaves its lane the moment its context
// ends, answers the context's error, and costs nothing — and a lane nobody is
// left in is forgotten.
//
// Mutation: wait for the turn without the context and the abandoned attempt
// neither answers nor leaves, and its derivation runs.
func TestAnAbandonedAttemptGivesUpItsPlace(t *testing.T) {
	t.Parallel()
	g := newGated(t, 1)
	running := g.verifyIn(t.Context(), "198.51.100.23", "running")
	g.begun(t)

	gone, cancel := context.WithCancel(t.Context())
	abandoned := g.verifyIn(gone, "198.51.100.23", "abandoned")
	elsewhere, cancelElsewhere := context.WithCancel(t.Context())
	abandonedElsewhere := g.verifyIn(elsewhere, "203.0.113.9", "abandoned-elsewhere")
	g.idle(t, "with the cap held, nothing else may start")
	cancel()
	cancelElsewhere()
	for _, done := range []<-chan error{abandoned, abandonedElsewhere} {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("an abandoned attempt answered %v, want the context's error", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("an abandoned attempt went on waiting for its turn")
		}
	}

	g.finish <- struct{}{}
	if err := <-running; err != nil {
		t.Fatal(err)
	}
	g.idle(t, "an abandoned attempt's derivation")
	if slots, lanes := g.turns.held(); slots != 0 || lanes != 0 {
		t.Errorf("after every attempt ended or left, %d slots and %d lanes are held",
			slots, lanes)
	}
}

// A DECOY TAKES THE SAME TURN AS A VERIFICATION, AND HOLDS IT AS LONG.
//
// The pad equalises the two arms only while a verification finishes inside it,
// and a decoy that queued for nothing let one address separate them by
// itself: its real names queued behind each other in its lane and answered
// late, its decoys answered on time. So a decoy waits for its source's turn —
// behind that source's verification here — and holds its slot for the
// measured length of a derivation, which it takes itself, once, when nothing
// has been measured yet.
//
// Mutations: let a decoy skip the turn and it answers while its source's
// verification is still running; hold for anything but the measure and the
// hold is not the derivation's length; skip the first decoy's derivation and
// nothing is ever measured.
func TestADecoyTakesTheSameTurnAndHoldsItAsLong(t *testing.T) {
	t.Parallel()
	h := NewHasher(cheapParams, 4)
	var mu sync.Mutex
	var derived int
	h.work = func(string, []byte, Params) []byte {
		mu.Lock()
		derived++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return make([]byte, cheapParams.KeyLen)
	}
	var holds []time.Duration
	h.hold = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		holds = append(holds, d)
	}

	// NOTHING MEASURED YET: the first decoy derives, and that is the measure.
	if err := h.Decoy(t.Context(), "198.51.100.23", "whatever-was-typed"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	first, measured := derived, time.Duration(h.took.Load())
	mu.Unlock()
	if first != 1 || measured < 20*time.Millisecond {
		t.Fatalf("the first decoy derived %d times and measured %s, want one "+
			"derivation and its length", first, measured)
	}
	// MEASURED: the next decoy holds for the measure and derives nothing.
	if err := h.Decoy(t.Context(), "198.51.100.23", "whatever-was-typed"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if derived != 1 || len(holds) != 1 || holds[0] != measured {
		t.Errorf("a decoy after the measure derived %d times and held %v, want "+
			"no derivation and one hold of %s", derived-1, holds, measured)
	}
	mu.Unlock()

	// IN THE SAME LANE: behind its source's verification, not beside it.
	g := newGated(t, 4)
	g.took.Store(int64(time.Millisecond))
	running := g.verifyIn(t.Context(), "198.51.100.23", "a-real-name")
	g.begun(t)
	decoyed := make(chan error, 1)
	go func() { decoyed <- g.Decoy(t.Context(), "198.51.100.23", "a-fake-name") }()
	select {
	case err := <-decoyed:
		t.Fatalf("a decoy answered (%v) while its source's verification still "+
			"held the turn — it queued for nothing", err)
	case <-time.After(200 * time.Millisecond):
	}
	g.finish <- struct{}{}
	if err := <-running; err != nil {
		t.Fatal(err)
	}
	if err := <-decoyed; err != nil {
		t.Fatal(err)
	}
}

// A GRANTED TURN RUNS TO ITS END WHATEVER ITS REQUEST DOES — A DECOY'S AS WELL
// AS A DERIVATION'S.
//
// What the next attempt in a lane sees is when the turn ahead of it ended.
// argon2 cannot be interrupted, so a verification abandoned mid-derivation
// holds its slot to the end; a decoy's hold ended with its request, so an
// address that queued attempts behind a candidate and then hung up on it saw
// them start at once behind a name nobody holds and a derivation later behind
// a real one — the roster, read off its own lane. Both arms are abandoned
// mid-turn here, and in both the next waiter in the lane starts only once the
// turn has run its length.
//
// Mutation: hold a decoy's slot on its request's context and the waiter behind
// the abandoned decoy starts the moment it is abandoned.
func TestAGrantedTurnRunsToItsEndWhateverItsRequestDoes(t *testing.T) {
	t.Parallel()
	const source = "198.51.100.23"

	t.Run("a verification", func(t *testing.T) {
		t.Parallel()
		g := newGated(t, 4)
		gone, abandon := context.WithCancel(t.Context())
		abandoned := g.verifyIn(gone, source, "a-real-name")
		g.begun(t)
		next := g.verifyIn(t.Context(), source, "next")
		abandon()
		g.idle(t, "the attempt queued behind an abandoned verification")
		g.finish <- struct{}{} // THE DERIVATION ENDS, and only then the turn
		if got := g.begun(t); got != "next" {
			t.Fatalf("the derivation that started was %q", got)
		}
		g.finish <- struct{}{}
		for _, done := range []<-chan error{abandoned, next} {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("a decoy", func(t *testing.T) {
		t.Parallel()
		// A SECOND, so the window [gated.idle] watches — a fifth of it
		// — ends long before the hold does however slowly this runner
		// gets from the grant to the abandonment.
		const measure = time.Second
		g := newGated(t, 4)
		g.took.Store(int64(measure))
		gone, abandon := context.WithCancel(t.Context())
		decoyed := make(chan error, 1)
		go func() { decoyed <- g.Decoy(gone, source, "a-fake-name") }()
		heldBy(t, g.turns, 1)
		next := g.verifyIn(t.Context(), source, "next")
		abandon()
		g.idle(t, "the attempt queued behind an abandoned decoy")
		if got := g.begun(t); got != "next" {
			t.Fatalf("the derivation that started was %q", got)
		}
		g.finish <- struct{}{}
		if err := <-next; err != nil {
			t.Fatal(err)
		}
		if err := <-decoyed; err != nil {
			t.Fatalf("a decoy abandoned after its turn was granted answered %v, "+
				"want nil: it held the turn it was given", err)
		}
	})
}

// heldBy waits until slots of t's cap are held, or fails.
func heldBy(tb testing.TB, t *turns, slots int) {
	tb.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if held, _ := t.held(); held == slots {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("the cap never came to hold %d slots", slots)
		}
		time.Sleep(time.Millisecond)
	}
}
