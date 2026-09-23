package chart_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
)

// SEALING AND THE MASK.
//
// Two rules that both turn on one question — is this value a credential, a
// pointer at one, or the marker that stands in for one on a read — and the
// whole of what makes them safe is that the question is asked in ONE place.
// A second spelling of it is either a credential written to a log every node
// applies, or a working credential silently replaced by the eight characters
// `__redacted__`.

// fakeSealer is a secret store that records what it was asked to seal.
type fakeSealer struct {
	mu     sync.Mutex
	sealed map[string]string
}

func newSealer(t *testing.T) *fakeSealer {
	t.Helper()
	return &fakeSealer{sealed: map[string]string{}}
}

func (f *fakeSealer) Seal(_ context.Context, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sealed[name] = value
	return nil
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
