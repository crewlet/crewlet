package credential

import (
	"errors"
	"testing"
	"time"
)

// A REHASH NOBODY IS WAITING ON NEVER QUEUES FOR THE CAP.
//
// Raising the cost is done after a sign-in has answered, and the cap is full
// exactly when the endpoint is under attack — so a rewrite takes a slot only if
// one is free at once, and answers [ErrSaturated] having derived nothing when
// none is: queued, it would take a slot ahead of the sign-ins arriving behind
// it. And it never runs outside the cap. Mutation: take the slot with a plain
// send and the rehash blocks while every slot is taken; skip the cap and it
// answers a verifier while every slot is taken.
func TestARehashNeverQueuesForTheCap(t *testing.T) {
	t.Parallel()
	h := NewHasher(Params{Memory: 8 * 1024, Time: 1, Threads: 1, KeyLen: 32}, 1)
	h.admit <- struct{}{} // EVERY SLOT TAKEN
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
	<-h.admit // A SLOT FREES
	verifier, err := h.Rehash("a password somebody chose")
	if err != nil {
		t.Fatalf("with a slot free the rehash failed: %v", err)
	}
	if ok, stale := h.Verify(verifier, "a password somebody chose"); !ok || stale {
		t.Errorf("the rehashed verifier verified %v, stale %v — want a "+
			"verifier at this hasher's own cost", ok, stale)
	}
	if len(h.admit) != 0 {
		t.Errorf("the rehash left %d slots taken", len(h.admit))
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
// Mutation: call argon2 from Verify directly and the verification finishes
// while every slot is taken.
func TestAVerificationWaitsForTheCapAHashDoes(t *testing.T) {
	t.Parallel()
	h := NewHasher(Params{Memory: 8 * 1024, Time: 1, Threads: 1, KeyLen: 32}, 1)
	const password = "a password somebody chose"
	verifier, err := h.Hash(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	h.admit <- struct{}{} // EVERY SLOT TAKEN
	done := make(chan bool, 1)
	go func() {
		ok, _ := h.Verify(verifier, password)
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

	<-h.admit // A SLOT FREES
	select {
	case ok := <-done:
		if !ok {
			t.Error("once admitted, the verification refused the right password")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the verification never ran once a slot freed")
	}
}
