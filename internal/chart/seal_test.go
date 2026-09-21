package chart_test

import (
	"context"
	"crypto/rand"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
)

// SEALING, THE MASK, AND THE BLIND INDEX.
//
// Three rules that all turn on one question — is this value a credential, a
// pointer at one, or the marker that stands in for one on a read — and the
// whole of what makes them safe is that the question is asked in ONE place.
// A second spelling of it is either a credential written to a log every node
// applies, or a working credential silently replaced by the eight characters
// `__redacted__`.

// fakeSealer is a secret store that records what it was asked to seal.
type fakeSealer struct {
	mu     sync.Mutex
	sealed map[string]string
	key    []byte

	// failBlind makes the key unreadable, which is a real state: a node
	// that cannot reach the secret store must not compute an index under a
	// key it invented, because it would match nothing anywhere else.
	failBlind error
}

func newSealer(t *testing.T) *fakeSealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("mint a blind-index key: %v", err)
	}
	return &fakeSealer{sealed: map[string]string{}, key: key}
}

func (f *fakeSealer) Seal(_ context.Context, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sealed[name] = value
	return nil
}

func (f *fakeSealer) BlindKey(context.Context) ([]byte, error) {
	if f.failBlind != nil {
		return nil, f.failBlind
	}
	return f.key, nil
}

func (f *fakeSealer) get(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.sealed[name]
	return v, ok
}

// A WHOLE ${VAR} IS STORED VERBATIM AND A LITERAL IS SEALED.
//
// The distinction is the entire point: a reference NAMES a credential rather
// than being one, it is what an operator edits, and sealing it would put a
// pointer inside the store and a pointer to that pointer on the record. A
// literal is the credential itself, and a record carrying one is a credential
// in a log every node applies, in every snapshot and in every backup of both.
func TestAWholeReferenceIsStoredVerbatimAndALiteralIsSealed(t *testing.T) {
	t.Parallel()

	object := chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"}
	name := chart.SecretName(object, "email")
	if !strings.HasPrefix(name, "CHART_SEAT_SARAH_CHEN_") {
		t.Errorf("the derived name is %q — it is what a re-seal of one field "+
			"overwrites, so it has to be a function of the object and the "+
			"field and nothing else", name)
	}
	if ref := chart.SecretRef(object, "email"); ref != "${"+name+"}" {
		t.Errorf("the reference is %q, want ${%s}", ref, name)
	}
}

// THE BLIND INDEX IS DETERMINISTIC UNDER ONE KEY AND DIFFERS UNDER ANOTHER.
//
// Every node computes it, so two nodes with the fleet's key must produce the
// same bytes for one address or the lookup matches nothing — and anybody
// WITHOUT the key must not be able to produce it at all, or the column is the
// plaintext it replaced.
func TestTheBlindIndexIsStableUnderAKeyAndUselessWithoutIt(t *testing.T) {
	t.Parallel()

	key := []byte("a fleet's own blind-index key, 32b")
	other := []byte("some other key entirely, also 32b!")

	got := chart.BlindIndex(key, "Sarah.Chen@example.com")
	if got == "" {
		t.Fatal("an address produced no index at all")
	}
	// NORMALISED FIRST, so the address a vendor sends and the address a
	// founder typed reach one value. Without it the lookup misses on
	// exactly the case it exists for: a payload whose capitalisation
	// differs from the config's.
	if same := chart.BlindIndex(key, "  sarah.chen@EXAMPLE.com  "); same != got {
		t.Errorf("two spellings of one address produced %q and %q — a vendor "+
			"payload and a founder's config rarely agree about case, and a "+
			"lookup that missed there would miss every time it mattered",
			got, same)
	}
	if chart.BlindIndex(other, "Sarah.Chen@example.com") == got {
		t.Error("two different keys produced one index, so the column is a " +
			"plain digest — and the whole corpus of plausible addresses at " +
			"one company is enumerable in seconds")
	}
	if chart.BlindIndex(key, "someone.else@example.com") == got {
		t.Error("two different addresses produced one index")
	}
	// AN EMPTY ADDRESS IS EMPTY, not a hash of nothing: a seat with no
	// address must not be findable by looking up the empty string, which
	// would resolve every vendor payload with a missing sender to whichever
	// seat happened to have no email.
	if chart.BlindIndex(key, "") != "" {
		t.Error("an absent address produced an index, so every seat without " +
			"one shares it")
	}
	if chart.BlindIndex(key, "   ") != "" {
		t.Error("whitespace produced an index")
	}
}

// THE INDEX GOES IN A TEXT COLUMN AND IS COMPARED FOR EQUALITY, so it must
// carry nothing a comparison could normalise differently on two nodes.
func TestTheBlindIndexIsSafeInAColumnAndInASubject(t *testing.T) {
	t.Parallel()

	got := chart.BlindIndex([]byte("k"), "sarah.chen@example.com")
	if strings.ContainsAny(got, "=+/ \t\n") {
		t.Errorf("the index is %q — padding, a slash or whitespace makes two "+
			"nodes' values depend on how each one wrote them out", got)
	}
	if strings.ToUpper(got) != got {
		t.Errorf("the index is %q and is not case-stable, so an equality "+
			"comparison depends on a collation nobody declared", got)
	}
}
