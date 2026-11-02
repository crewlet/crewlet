package configapi_test

import (
	"context"
	"slices"
	"testing"
)

// WHAT A SECRET WRITE ASKS IS THE REFERENCE INDEX, BY NAME.
//
// A write re-activates the revision only when it reads the name written
// (ADR-0070), so Reads has to answer from the same index GET
// /config/references does (the fixture's `${ROTATED}` behind a credential
// bundle) and leave out a name the company does not read, or every write of
// anything would move the fleet onto a new epoch.
func TestReadsAnswersThePathsReadingEachName(t *testing.T) {
	t.Parallel()
	s := newSurface(t)

	// NOTHING ACTIVE READS NOTHING, which is an answer and not a failure: a
	// value sealed before the first import is resolved by that import.
	got, err := s.service().Reads(context.Background(), []string{"ROTATED"})
	if err != nil || len(got) != 0 {
		t.Fatalf("before the first import: (%v, %v), want nothing read", got, err)
	}

	s.seed(t, companyDoc)
	got, err = s.service().Reads(context.Background(), []string{"ROTATED", "UNREAD"})
	if err != nil {
		t.Fatal(err)
	}
	if paths := got["ROTATED"]; !slices.Equal(paths,
		[]string{"providers.llm.zulu.auth.credential_bundle"}) {
		t.Errorf("ROTATED is read at %v", paths)
	}
	if _, ok := got["UNREAD"]; ok {
		t.Errorf("a name the company does not read was answered read: %v", got)
	}
}
