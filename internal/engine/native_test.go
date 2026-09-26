package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A POSITION IS WAITED FOR ON THE DOMAIN ITS OWN STREAM NAMES, and one this
// node applies no domain for is an error rather than nil.
//
// Nil is the answer that says "applied": a page tool or a work tool told it
// reads its rows back as though its write were in them. The control is a real
// write's position, which settles.
//
// Mutation: answer nil for a stream no domain here runs, and the second half
// fails.
func TestWaitCommittedRefusesAPositionNoDomainHereApplies(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	waitFor(t, "the native backends to hydrate", e.NativeHydrated)

	written, err := e.PagesStore().Create(t.Context(),
		pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ops-1"},
		pages.NewPage{Container: "ENG", Title: "Runbook", Body: "step one"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := e.WaitCommitted(t.Context(), written.Outcome.Position); err != nil {
		t.Fatalf("a page write's own position did not settle: %v", err)
	}

	elsewhere := written.Outcome.Position
	elsewhere.Stream = "CREWLET_NO_SUCH_LOG"
	if err := e.WaitCommitted(t.Context(), elsewhere); err == nil {
		t.Fatal("a position on a stream this node applies no domain for " +
			"answered nil, which a caller reads as applied")
	}

	// AND A ZERO SEQUENCE IS NOTHING TO WAIT FOR, which is what a write
	// that appended no record answers with.
	if err := e.WaitCommitted(t.Context(), statelog.Position{}); err != nil {
		t.Fatalf("a position with no sequence was refused: %v", err)
	}
}
