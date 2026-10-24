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
// a case decides from outside when each one ends and sees which started.
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

// verifyIn starts one verification and answers when it ends.
func (g *gated) verifyIn(ctx context.Context, password string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, _, err := g.Verify(ctx, verifierFor(cheapParams), password)
		done <- err
	}()
	return done
}

// verifierFor is a well-formed verifier at params nothing will ever match.
func verifierFor(p Params) string {
	h := &Hasher{params: p}
	return h.encode(make([]byte, SaltLen), make([]byte, p.KeyLen))
}

// held is how many slots of h's cap are taken, for the cases that assert
// nothing is left behind.
func (h *Hasher) held() int { return len(h.slots) }

// A REHASH NOBODY IS WAITING ON NEVER QUEUES FOR THE CAP.
//
// Raising the cost is done after a sign-in has answered, and the cap is full
// exactly when the endpoint is under attack — so a rewrite takes a slot only if
// one is free at once, and answers [ErrSaturated] having derived nothing when
// none is: queued, it would take a slot ahead of the sign-ins arriving behind
// it. And it never runs outside the cap. Mutation: take the slot through
// [Hasher.take] and the rehash blocks while every slot is taken; skip the cap
// and it answers a verifier while every slot is taken.
func TestARehashNeverQueuesForTheCap(t *testing.T) {
	t.Parallel()
	h := NewHasher(cheapParams, 1)
	taken, err := h.take(t.Context()) // EVERY SLOT TAKEN
	if err != nil {
		t.Fatalf("an empty cap had no slot: %v", err)
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
	if ok, stale, err := h.Verify(t.Context(), verifier, "a password somebody chose"); !ok ||
		stale || err != nil {
		t.Errorf("the rehashed verifier verified %v, stale %v (%v) — want a "+
			"verifier at this hasher's own cost", ok, stale, err)
	}
	if held := h.held(); held != 0 {
		t.Errorf("the rehash left %d slots held", held)
	}
}

// A REHASH NEVER TAKES A FREED SLOT FROM A SIGN-IN WAITING FOR ONE.
//
// A slot a finishing derivation frees goes to a request already waiting for
// it; a rewrite nobody waits on that took it instead would push the sign-in
// behind it back by a derivation, under exactly the load the cap exists for.
// The property is about the INSTANT of release, so the case asks for the slot
// at that instant: the slot is given back and the rehash asks for it in the
// same goroutine with nothing between them, before the waiter has been
// scheduled at all. That it is refused is Go's own hand-off — a receive from a
// full buffered channel moves a blocked sender's value into the buffer under
// the channel's lock — so the correct cap refuses it every time.
//
// Mutation: wait for a slot by polling it rather than by blocking on the send
// — a free pool the rehash can see before the waiter does — and the rehash
// derives in the slot the waiter was owed.
func TestARehashNeverTakesAFreedSlotFromAWaiter(t *testing.T) {
	t.Parallel()
	g := newGated(t, 1)
	const rewrite = "a rewrite nobody waits on"
	gate := g.work
	g.work = func(password string, salt []byte, params Params) []byte {
		if password == rewrite { // RECORDED, AND NEVER HELD
			g.started <- password
			return make([]byte, params.KeyLen)
		}
		return gate(password, salt, params)
	}

	release, err := g.take(t.Context()) // THE ONLY SLOT, HELD BY HAND
	if err != nil {
		t.Fatalf("an empty cap had no slot: %v", err)
	}
	waiting := g.verifyIn(t.Context(), "waiting")
	g.idle(t, "with the cap held, the waiter")

	release() // THE SLOT FREES — AND AT THAT INSTANT A REHASH ASKS FOR IT
	if _, err := g.Rehash(rewrite); !errors.Is(err, ErrSaturated) {
		t.Fatalf("a rehash at the instant a slot freed answered %v, want %v — "+
			"the slot was the waiting sign-in's", err, ErrSaturated)
	}
	if got := g.begun(t); got != "waiting" {
		t.Fatalf("the freed slot went to %q, want the waiter", got)
	}
	g.finish <- struct{}{}
	if err := <-waiting; err != nil {
		t.Fatal(err)
	}
	if held := g.held(); held != 0 {
		t.Errorf("after the waiter finished, %d slots are held", held)
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
// Mutation: derive in Verify without taking a slot and the verification
// finishes while every slot is taken.
func TestAVerificationWaitsForTheCapAHashDoes(t *testing.T) {
	t.Parallel()
	h := NewHasher(cheapParams, 1)
	const password = "a password somebody chose"
	verifier, err := h.Hash(t.Context(), password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	taken, _ := h.take(t.Context()) // EVERY SLOT TAKEN
	done := make(chan bool, 1)
	go func() {
		ok, _, _ := h.Verify(t.Context(), verifier, password)
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

// AN ABANDONED ATTEMPT GIVES UP ITS PLACE.
//
// A derivation used to wait for the cap with no bound and no way to leave, so
// a flood whose clients had all hung up still ran every one of its
// derivations, one slot each, ahead of whoever came next. A waiting attempt
// leaves the moment its context ends, answers the context's error and costs
// nothing; and one whose request is already gone takes nothing even from a cap
// with a slot free.
//
// Mutations: wait for the slot without the context and the abandoned attempt
// neither answers nor leaves, and its derivation runs; drop the up-front check
// of the context and a request already gone derives in the free slot.
func TestAnAbandonedAttemptGivesUpItsPlace(t *testing.T) {
	t.Parallel()
	g := newGated(t, 1)
	running := g.verifyIn(t.Context(), "running")
	g.begun(t)

	gone, cancel := context.WithCancel(t.Context())
	abandoned := g.verifyIn(gone, "abandoned")
	g.idle(t, "with the cap held, nothing else may start")
	cancel()
	select {
	case err := <-abandoned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("an abandoned attempt answered %v, want the context's error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("an abandoned attempt went on waiting for a slot")
	}

	g.finish <- struct{}{}
	if err := <-running; err != nil {
		t.Fatal(err)
	}
	g.idle(t, "an abandoned attempt's derivation")

	// A SLOT IS FREE NOW, and a request already gone still takes nothing —
	// asked many times, because a select over a free slot and a closed
	// context picks between them at random.
	for range 32 {
		done := g.verifyIn(gone, "already gone")
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("a request already gone answered %v, want the context's "+
					"error", err)
			}
		case <-g.started:
			g.finish <- struct{}{}
			<-done
			t.Fatal("a request already gone took the free slot and derived")
		}
	}
	g.idle(t, "a request already gone")
	if held := g.held(); held != 0 {
		t.Errorf("after every attempt ended or left, %d slots are held", held)
	}
}

// A MISS IS ONE DERIVATION AT THIS BUILD'S COST, UNDER THE CAP, EXACTLY AS A
// HIT IS.
//
// A sign-in that names nobody has no verifier to check, and refused without a
// derivation it answers in microseconds where a real name pays an argon2id
// verification — the roster, read off a stopwatch. So a decoy verifies what was
// presented against the hasher's one dummy verifier: the same derivation, at
// the same cost, waiting for the same cap — and an unreadable stored verifier
// is answered the same way.
//
// Mutations: derive a decoy at any cost but this hasher's, or derive nothing,
// and the recorded derivations differ; let it skip the cap and it answers
// while every slot is taken; draw a new dummy per decoy and two decoys verify
// against two salts.
func TestAMissIsOneDerivationAtThisCostUnderTheCap(t *testing.T) {
	t.Parallel()
	h := NewHasher(cheapParams, 1)
	type derivation struct {
		password string
		salt     string
		params   Params
	}
	var mu sync.Mutex
	var derived []derivation
	h.work = func(password string, salt []byte, params Params) []byte {
		mu.Lock()
		defer mu.Unlock()
		derived = append(derived, derivation{password, string(salt), params})
		return make([]byte, params.KeyLen)
	}
	take := func() []derivation {
		mu.Lock()
		defer mu.Unlock()
		out := derived
		derived = nil
		return out
	}

	if err := h.Decoy(t.Context(), "whatever-was-typed"); err != nil {
		t.Fatal(err)
	}
	first := take()
	if len(first) != 1 || first[0].params != cheapParams ||
		first[0].password != "whatever-was-typed" {
		t.Fatalf("a decoy derived %+v, want one derivation of what was "+
			"presented at this hasher's own cost", first)
	}
	if err := h.Decoy(t.Context(), "something-else"); err != nil {
		t.Fatal(err)
	}
	if again := take(); len(again) != 1 || again[0].salt != first[0].salt {
		t.Errorf("a second decoy derived %+v, want one derivation against the "+
			"same dummy verifier", again)
	}

	// AN UNREADABLE VERIFIER is a decoy, and refuses.
	if ok, _, err := h.Verify(t.Context(), "not-a-verifier", "anything"); ok || err != nil {
		t.Errorf("an unreadable verifier answered ok=%v (%v), want a refusal", ok, err)
	}
	if got := take(); len(got) != 1 || got[0].params != cheapParams {
		t.Errorf("an unreadable verifier derived %+v, want what a decoy does", got)
	}

	// UNDER THE CAP: with every slot taken, a decoy waits.
	taken, _ := h.take(t.Context())
	decoyed := make(chan error, 1)
	go func() { decoyed <- h.Decoy(t.Context(), "a-fake-name") }()
	select {
	case err := <-decoyed:
		t.Fatalf("a decoy answered (%v) while every slot of the cap was taken", err)
	case <-time.After(200 * time.Millisecond):
	}
	taken()
	if err := <-decoyed; err != nil {
		t.Fatal(err)
	}
}
