package tracker_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// segment is one completed segment of a run, as the engine charges it.
func segment(run string, turns int, outcome string, phases ...string) tracker.TurnRecord {
	return tracker.TurnRecord{
		Task: "t-1", Seat: "dev", TurnID: run, Trigger: "work_item",
		Outcome: outcome, Phases: phases,
		Spend: tracker.TurnSpend{Turns: turns, Rounds: 2, Input: 100, Output: 20, WallMs: 1000},
	}
}

// A RUN IS PLACED ON THE TASK IT WAS CHARGED TO, numbered by the task's own
// count of turns — so "turn 2 on ENG-1" beside a page's reader, "session 2" on
// a story and the task's `spend.turns` are one count. A turn that parked and
// resumed is ONE turn: only its counted segment numbers it. A run whose only
// row here carries no count is placed with ordinal zero rather than an
// invented one, and a run the tracker never saw is absent rather than guessed.
func TestTurnPlacesNamesTheChargedTaskAndItsOrdinal(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	for i, rec := range []struct {
		op   string
		turn tracker.TurnRecord
	}{
		{"turn/run-a/dispatch", segment("run-a", 1, "suspended", "execute")},
		{"turn/run-a/resume/l-1", segment("run-a", 0, "done", "execute", "review")},
		{"turn/run-b/dispatch", segment("run-b", 1, "done", "execute")},
		{"turn/run-c/resume/l-9", segment("run-c", 0, "done", "execute")},
	} {
		if _, err := r.writer.RecordTurn(t.Context(), rec.op, rec.turn); err != nil {
			t.Fatalf("RecordTurn %d: %v", i, err)
		}
		r.drain()
	}

	got, err := r.reader.TurnPlaces(t.Context(), []string{"run-a", "run-b", "run-c", "run-z", "run-b", ""},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("TurnPlaces: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("placed %d runs, want run-a, run-b and run-c only: %+v", len(got), got)
	}
	if a := got["run-a"]; a.Key != "ENG-1" || a.Ordinal != 1 || a.TaskID != "t-1" {
		t.Errorf("run-a = %+v, want turn 1 on ENG-1 — its two segments are one turn", a)
	}
	if b := got["run-b"]; b.Key != "ENG-1" || b.Ordinal != 2 || b.TaskID != "t-1" {
		t.Errorf("run-b = %+v, want turn 2 on ENG-1", b)
	}
	if c := got["run-c"]; c.Key != "ENG-1" || c.Ordinal != 0 {
		t.Errorf("run-c = %+v, want ENG-1 with no ordinal — its row counts no turn", c)
	}
	if spend := r.taskSpend("t-1")["spend_turns"]; spend != 2 {
		t.Errorf("spend_turns = %d, want 2 — the newest ordinal and the count agree", spend)
	}

	if _, err := r.reader.TurnPlaces(t.Context(), []string{"run-a"}, statelog.Freshness{}); err == nil {
		t.Error("a read naming no level was served")
	}

	// A REMOVED TASK PLACES NOTHING: a reader line linking to it would be a
	// link to a task that is gone.
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-1", "ENG", false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()
	got, err = r.reader.TurnPlaces(t.Context(), []string{"run-b"}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("TurnPlaces after the removal: %v", err)
	}
	if place, ok := got["run-b"]; ok {
		t.Errorf("run-b is still placed on %+v, a removed task", place)
	}
}

// WHAT A RUN CHANGED IS READ BY THE RUN: every change a turn's writer committed
// carries its run, in the order it landed, with the status a move went to —
// and nothing anybody else did is attributed to it.
//
// Mutation: drop the `turn_id IN (…)` term, the `subject_kind` term, or the
// removed-task join's condition from readTurnWrites, and a case below fails.
func TestTurnWritesAreWhatEachRunChanged(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	first := r.createTask("the first one")
	second := r.createTask("the second one")

	agent := r.writer.As("eng", tracker.AuthorAgent, tracker.Provenance{TurnID: "run-1"})
	moving := tracker.StatusInProgress
	if _, err := agent.UpdateTask(t.Context(), "op-move", first.ID, "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Status: &moving}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := agent.UpdateTask(t.Context(), "op-retitle", second.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: ptr("the second one, renamed")},
		tracker.ChangeFields, nil); err != nil {
		t.Fatalf("retitle: %v", err)
	}
	r.drain()
	// ANOTHER RUN'S CHANGE, and a person's with no run at all.
	other := r.writer.As("ops", tracker.AuthorAgent, tracker.Provenance{TurnID: "run-2"})
	done := tracker.StatusDone
	if _, err := other.UpdateTask(t.Context(), "op-other", second.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("the other run: %v", err)
	}
	r.drain()
	if _, err := r.writer.UpdateTask(t.Context(), "op-person", first.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: ptr("a person looked")},
		tracker.ChangeFields, nil); err != nil {
		t.Fatalf("the person: %v", err)
	}
	r.drain()

	fresh := statelog.Freshness{Level: statelog.ReadStale}
	got, err := r.reader.TurnWrites(t.Context(), []string{"run-1", "run-2", "run-none", "run-1", ""}, fresh)
	if err != nil {
		t.Fatalf("TurnWrites: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("answered %d runs, want run-1 and run-2 — a run that changed "+
			"nothing is absent: %+v", len(got), got)
	}
	one := got["run-1"]
	if len(one) != 2 {
		t.Fatalf("run-1 changed %d things, want its two: %+v", len(one), one)
	}
	if one[0].TaskID != first.ID || one[0].Kind != tracker.ChangeStatus ||
		one[0].Status != string(tracker.StatusInProgress) {
		t.Errorf("run-1's first change = %+v, want the move of %s to in progress", one[0], first.Key)
	}
	if one[1].TaskID != second.ID || one[1].Kind != tracker.ChangeFields || one[1].Status != "" ||
		one[1].Title != "the second one, renamed" {
		t.Errorf("run-1's second change = %+v, want the retitle of %s, named as it "+
			"reads now and moving no status", one[1], second.Key)
	}
	if two := got["run-2"]; len(two) != 1 || two[0].Status != string(tracker.StatusDone) {
		t.Errorf("run-2 = %+v, want its one move to done", two)
	}
	if slices.ContainsFunc(one, func(w tracker.TurnWrite) bool { return w.Kind == tracker.ChangeCreated }) {
		t.Error("run-1 is credited with a create a person made")
	}

	if _, err := r.reader.TurnWrites(t.Context(), []string{"run-1"}, statelog.Freshness{}); err == nil {
		t.Error("a read naming no level was served")
	}

	// A REMOVED TASK IS NOT LEFT BEHIND: a story must not link to it.
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", first.ID, "ENG", false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()
	got, err = r.reader.TurnWrites(t.Context(), []string{"run-1"}, fresh)
	if err != nil {
		t.Fatalf("TurnWrites after the removal: %v", err)
	}
	if left := got["run-1"]; len(left) != 1 || left[0].TaskID != second.ID {
		t.Errorf("after %s's removal run-1 left %+v, want the second task alone", first.Key, left)
	}
}

// A RUN IS ANSWERED WITH ITS FIRST CHANGES, bounded: a session that rewrote a
// task forty times is told by what it did first, and one read for a page of
// stories cannot return a run's whole history.
func TestTurnWritesKeepARunsFirstChanges(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("edited over and over")
	agent := r.writer.As("eng", tracker.AuthorAgent, tracker.Provenance{TurnID: "run-busy"})
	moving := tracker.StatusInProgress
	if _, err := agent.UpdateTask(t.Context(), "op-first", task.ID, "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Status: &moving}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	r.drain()
	for i := range tracker.MaxTurnWrites {
		if _, err := agent.UpdateTask(t.Context(), fmt.Sprintf("op-edit-%d", i), task.ID, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Title: ptr(fmt.Sprintf("edit %d", i))},
			tracker.ChangeFields, nil); err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
		r.drain()
	}
	got, err := r.reader.TurnWrites(t.Context(), []string{"run-busy"},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("TurnWrites: %v", err)
	}
	busy := got["run-busy"]
	if len(busy) != tracker.MaxTurnWrites {
		t.Fatalf("a run of %d changes answered %d, want the bound %d",
			tracker.MaxTurnWrites+1, len(busy), tracker.MaxTurnWrites)
	}
	if busy[0].Kind != tracker.ChangeStatus {
		t.Errorf("the run's first answered change is %s, want the move it made first", busy[0].Kind)
	}
}
