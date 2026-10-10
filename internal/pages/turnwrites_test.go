package pages_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHAT A RUN CHANGED IN THE KNOWLEDGE BASE IS READ BY THE RUN: every change an
// agent's turn committed carries its run, in the order it landed, and nothing a
// person or another run did is attributed to it. A page in the trash is not
// left behind — a story must not link to a page a reader cannot open.
//
// Mutation: drop the `turn_id IN (…)` term or the trashed-page condition from
// readTurnWrites, and a case below fails.
func TestTurnWritesAreWhatEachRunChangedOnThePages(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// agent("swe") writes as run "turn-swe"; agent("ops") as "turn-ops".
	runbook := r.write(agent("swe"), pages.NewPage{Title: "Runbook", Body: "the steps"})
	if _, err := r.store.SavePage(t.Context(), agent("swe"), runbook.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("the steps, corrected")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	notes := r.write(agent("ops"), pages.NewPage{Title: "Notes", Body: "jotted"})
	// A PERSON'S CHANGE, which no run made.
	if _, err := r.store.SavePage(t.Context(), author("jane"), runbook.Page.ID,
		pages.Save{BaseVersion: 2, Body: ptr("a person tidied it")}); err != nil {
		t.Fatalf("the person's save: %v", err)
	}
	r.drain()

	fresh := statelog.Freshness{Level: statelog.ReadStale}
	got, err := r.reader.TurnWrites(t.Context(),
		[]string{"turn-swe", "turn-ops", "turn-nobody", "turn-swe", ""}, fresh)
	if err != nil {
		t.Fatalf("TurnWrites: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("answered %d runs, want turn-swe and turn-ops — a run that "+
			"changed nothing is absent: %+v", len(got), got)
	}
	swe := got["turn-swe"]
	if len(swe) != 2 || swe[0].Kind != pages.ChangeCreated || swe[1].Kind != pages.ChangeSaved ||
		swe[0].PageID != runbook.Page.ID || swe[0].Title != "Runbook" {
		t.Errorf("turn-swe = %+v, want the runbook created and then saved, in that order", swe)
	}
	if ops := got["turn-ops"]; len(ops) != 1 || ops[0].PageID != notes.Page.ID {
		t.Errorf("turn-ops = %+v, want its one page", ops)
	}

	if _, err := r.reader.TurnWrites(t.Context(), []string{"turn-swe"}, statelog.Freshness{}); err == nil {
		t.Error("a read naming no level was served")
	}

	if _, err := r.store.Trash(t.Context(), author("jane"), notes.Page.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	r.drain()
	got, err = r.reader.TurnWrites(t.Context(), []string{"turn-ops"}, fresh)
	if err != nil {
		t.Fatalf("TurnWrites after the trash: %v", err)
	}
	if left, ok := got["turn-ops"]; ok {
		t.Errorf("turn-ops still left %+v behind, a page in the trash", left)
	}
}
