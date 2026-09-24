package builtin_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tools"
)

// AN ASK CARRIES THE ITEM THE ASKING TURN IS ON, off the turn and never off an
// argument: the answering turn is charged to it, and a model that could name
// the item could charge a colleague's work to anything.
func TestAnAskCarriesTheTurnsItem(t *testing.T) {
	t.Parallel()
	svc := &asker{}
	tool := registered(t, builtin.Deps{A2A: svc}, builtin.A2AAskTool)
	turn := turnFor(t, "agent-ceo")
	turn.WorkItem = &types.WorkItem{Backend: types.WorkNative, ID: "task-1", Key: "ENG-1", Project: "ENG"}
	turn.WorkItemBasis = types.BasisTrigger
	res := callFor(t, tool, turn, map[string]any{
		"target": "agent-cto", "brief": "Is the build green?",
		"work_item": map[string]any{"backend": "native", "id": "task-9"}, // ignored
	})
	if res.Failed || len(svc.asks) != 1 {
		t.Fatalf("the ask did not go out: %s", res.Output)
	}
	got := svc.asks[0]
	if got.WorkItem == nil || *got.WorkItem != *turn.WorkItem ||
		got.WorkItemBasis != types.BasisTrigger {
		t.Errorf("the ask carries %+v (%q), want the turn's own item", got.WorkItem,
			got.WorkItemBasis)
	}
}

// A SEAT'S WRITES REPORT INTO ITS TURN'S SET, through every writer the surface
// derives — which is what lets the completion charge a turn by the one item it
// wrote. And a writer built outside a turn reports into nothing: a nil set in
// the interface would be a log that swallowed every report.
func TestASeatsWritesReportIntoItsTurn(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	turn := workTurn(t)
	turn.Written = &turnctx.Written{}
	entry, ok := reg.Lookup(builtin.CreateWorkItemTool)
	if !ok {
		t.Fatal("create_work_item is not registered")
	}
	if _, err := entry.Tool.(tools.SeatCallable).CallForTurn(t.Context(), turn,
		map[string]any{"title": "fix the flaky test", "project": "ENG"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(trk.actors) == 0 {
		t.Fatal("no writer was derived, so this case asserts nothing")
	}
	provenance := trk.actors[0].Provenance()
	if provenance.Written != turn.Written {
		t.Fatalf("the writer reports into %v, want the turn's own set", provenance.Written)
	}
	if provenance.TurnID != "run-1" {
		t.Errorf("the writer's provenance names turn %q, want the calling run", provenance.TurnID)
	}

	if (builtin.Actor{Handle: "ops"}).Provenance().Written != nil {
		t.Error("an actor with no set produced a non-nil log")
	}
}
