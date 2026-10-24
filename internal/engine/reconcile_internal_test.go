package engine

import (
	"errors"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
)

// A RECONCILER HOLDS THE ENGINE'S KEYRING, OR IS NOT BUILT.
//
// It used to take the keyring as an option, and a nil one read every payload
// as plaintext — exactly the forged document the seal exists to refuse — with
// nothing to say it was missing: the e2e harness was wired that way until it
// was noticed by hand. [New] refuses to start without a keyring, so only an
// Engine built by hand can lack one, and a reconciler over it is refused by
// name rather than built to trust whatever the coordination store holds.
//
// The control is the same options on an engine that holds one: built, and
// opening with that very keyring.
//
// Mutation: drop the refusal and the first half builds a reconciler with no
// keyring; take the cipher from anywhere but the engine and the second half
// holds another.
func TestAReconcilerIsRefusedAnEngineWithNoKeyring(t *testing.T) {
	t.Parallel()
	opts := ReconcilerOptions{
		Store: refinementStore(t), Fleet: coordmem.NewFleet(), Queue: qmem.New(),
		NodeID: "node-a",
	}
	if r, err := (&Engine{}).NewReconciler(opts); !errors.Is(err, ErrNoKeyring) {
		t.Fatalf("a reconciler over an engine with no keyring = (%v, %v), want ErrNoKeyring", r, err)
	}

	_, cipher := testKeyring(t)
	r, err := (&Engine{cipher: cipher}).NewReconciler(opts)
	if err != nil {
		t.Fatalf("NewReconciler over an engine holding a keyring: %v", err)
	}
	if r.cipher != cipher {
		t.Error("the reconciler opens revisions under a keyring other than the engine's")
	}
}
