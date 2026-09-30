package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// THE RUNNING TRACKER IS HANDED THE CHART'S IDENTITIES, on both sides.
//
// The seam is a field, and a nil one is a legitimate build — a tracker with
// no chart records and reads every person as given — so dropping it from the
// engine's wiring would compile, pass every tracker case and put the rename
// bug back on every node. This holds the wiring: the writer the engine runs,
// and the reader every surface asks, both carry it.
//
// Mutation: drop `Identities:` from the engine's WriterDeps, or the reader's
// assignment, or the item searcher's.
func TestTheRunningTrackerKnowsPeopleByTheirSeatsIdentity(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	reader, writer := e.Tracker(), e.TrackerWriter()
	if reader == nil || writer == nil {
		t.Fatal("the default company runs no native tracker")
	}
	if writer.Identities == nil {
		t.Error("the engine's tracker writer records every person as the " +
			"caller spelled them, so a renamed seat's work lands under a handle " +
			"no read asks for")
	}
	if reader.Identities == nil {
		t.Error("the engine's tracker reader asks for a renamed seat's work " +
			"by the handle it answers to now, which no row holds")
	}
	if search := e.WorkSearch(); search == nil || search.Identities == nil {
		t.Error("the engine's item search shows a renamed seat's work as held " +
			"by the handle it was created under")
	}
}
