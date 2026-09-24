package credential

import (
	"testing"
	"time"
)

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
