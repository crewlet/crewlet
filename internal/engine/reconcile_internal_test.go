package engine

import (
	"errors"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
)

// A RECONCILER HOLDS THE ENGINE'S KEYRING, OR IS NOT BUILT: without one every
// payload reads as plaintext, the forged document the seal exists to refuse.
// [New] refuses to start without a keyring, so only a hand-built Engine can
// lack one, and its reconciler is refused by name. The control is the same
// options on an engine holding one: built, opening with that very keyring.
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
	if r, err := (&Engine{}).NewReconciler(t.Context(), opts); !errors.Is(err, ErrNoKeyring) {
		t.Fatalf("a reconciler over an engine with no keyring = (%v, %v), want ErrNoKeyring", r, err)
	}

	_, cipher := testKeyring(t)
	r, err := (&Engine{cipher: cipher}).NewReconciler(t.Context(), opts)
	if err != nil {
		t.Fatalf("NewReconciler over an engine holding a keyring: %v", err)
	}
	if r.cipher != cipher {
		t.Error("the reconciler opens revisions under a keyring other than the engine's")
	}
}
