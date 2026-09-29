package chart_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/secrets"
)

// SEALING AND THE MASK.
//
// Two rules that both turn on one question — is this value a credential, a
// pointer at one, or the marker that stands in for one on a read — and the
// whole of what makes them safe is that the question is asked in ONE place.
// A second spelling of it is either a credential written to a log every node
// applies, or a working credential silently replaced by the eight characters
// `__redacted__`.

// fakeSealer is a secret store that records what it was asked to seal, and
// keeps the [chart.Sealer] contract the fleet's store does: a name is created
// once, confirmed when it is sealed again with the value it holds, and never
// written over.
type fakeSealer struct {
	mu     sync.Mutex
	sealed map[string]string
	// by is who each sealed value records as its author.
	by map[string]secrets.Author
	// afterSeal, when set, runs once a value is stored — a case's way of
	// making something happen between a write's seal and its publish.
	afterSeal func()
}

func newSealer(t *testing.T) *fakeSealer {
	t.Helper()
	return &fakeSealer{sealed: map[string]string{}, by: map[string]secrets.Author{}}
}

func (f *fakeSealer) Seal(_ context.Context, name, value string, by secrets.Author) error {
	f.mu.Lock()
	if held, found := f.sealed[name]; found {
		f.mu.Unlock()
		if held != value {
			return chart.ErrSealTaken
		}
		return nil
	}
	f.sealed[name] = value
	f.by[name] = by
	after := f.afterSeal
	f.mu.Unlock()
	if after != nil {
		after()
	}
	return nil
}

// author is who the value under name records.
func (f *fakeSealer) author(name string) secrets.Author {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.by[name]
}

func (f *fakeSealer) get(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.sealed[name]
	return v, ok
}

// A SEALED NAME IS THE OBJECT'S, THE FIELD'S AND THE WRITE'S.
//
// The readable half names the object by its identity and the field, which is
// what an operator listing their secrets reads; the digest is over those and
// the OPERATION, so no two writes of one field derive one name — a seal is
// made before its write is arbitrated, and under the name the live row already
// referenced, a write that never landed replaced the credential every node
// resolves. The same operation derives the same name, which is what lets a
// write decided twice find its own value.
func TestASealedNameIsTheObjectsTheFieldsAndTheWrites(t *testing.T) {
	t.Parallel()

	object := chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"}
	name := chart.SecretName(object, "op-one", "email")
	if !strings.HasPrefix(name, "CHART_SEAT_SARAH_CHEN_EMAIL_") || !chart.OwnsSecret(name) {
		t.Errorf("the derived name is %q — its readable half is the object's "+
			"identity and the field, and it has the shape this domain owns", name)
	}
	if again := chart.SecretName(object, "op-one", "email"); again != name {
		t.Errorf("one operation derived %q and then %q — a write decided again "+
			"must find the value its first decision sealed", name, again)
	}
	if other := chart.SecretName(object, "op-two", "email"); other == name {
		t.Errorf("two operations derived one name, %q — the second write's seal "+
			"is then a write over the value the first one's row names", name)
	}
	if ref := chart.SecretRef(object, "op-one", "email"); ref != "${"+name+"}" {
		t.Errorf("the reference is %q, want ${%s}", ref, name)
	}
}
