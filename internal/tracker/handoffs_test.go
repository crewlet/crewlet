package tracker_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// handOffsOnHistory is each assignee-moving row's hand-off count, oldest
// first, and every other row's alongside so a reset is visible.
func handOffsOnHistory(t *testing.T, r *roundTrip, id string) (assignee []int, all []*int) {
	t.Helper()
	detail, err := r.reader.Task(t.Context(), id, tracker.DetailWants{History: true},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	for i := len(detail.History) - 1; i >= 0; i-- {
		row := detail.History[i]
		all = append(all, row.Reassignments)
		if _, moved := row.Fields["assignee"]; moved && row.Reassignments != nil {
			assignee = append(assignee, *row.Reassignments)
		}
	}
	return assignee, all
}

// AN ASSIGNEE HISTORY ROW CARRIES THE HAND-OFF COUNT THE CHANGE LEFT.
//
// The task row holds the counter NOW; the history row is the only place "this
// was hand-off 2 of 8" can live, and it is derived from the records in log
// order — what a record states, or what the row before it left — so a person's
// reset shows on the row that did it and the next agent hand-off starts again
// from one.
func TestAssigneeHistoryCarriesReassignmentsAfter(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the circulating one")

	agent := r.writer.As("eng", tracker.AuthorAgent, tracker.Provenance{TurnID: "turn-1"})
	hand := func(opID, to string) {
		t.Helper()
		if _, err := agent.UpdateTask(t.Context(), opID, task.ID, "ENG", tracker.NoIfMatch,
			tracker.TaskPatch{Assignee: &to}, tracker.ChangeAssignee, nil); err != nil {
			t.Fatalf("hand-off to %s: %v", to, err)
		}
		r.drain()
	}
	hand("op-1", "ops")
	hand("op-2", "qa")
	// A PERSON TOUCHES SOMETHING ELSE, which returns the budget.
	if _, err := r.writer.UpdateTask(t.Context(), "op-human", task.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: ptr("a person looked")},
		tracker.ChangeFields, nil); err != nil {
		t.Fatalf("the human touch: %v", err)
	}
	r.drain()
	hand("op-3", "eng")

	assignee, all := handOffsOnHistory(t, r, task.ID)
	if fmt.Sprint(assignee) != "[1 2 1]" {
		t.Errorf("the hand-off rows carry %v, want [1 2 1] — each is the count "+
			"that change left, and the person's touch in between reset it", assignee)
	}
	// Every task row carries the count, the create's zero and the reset's
	// zero included.
	want := []int{0, 1, 2, 0, 1}
	if len(all) != len(want) {
		t.Fatalf("the task has %d history rows, want %d", len(all), len(want))
	}
	for i, n := range all {
		if n == nil || *n != want[i] {
			t.Errorf("row %d carries %v, want %d", i, n, want[i])
		}
	}

	// THE BACKFILL EQUALS THE REPLAY: a node upgrading onto rows its
	// predecessor wrote without the column re-derives exactly what the apply
	// maintained.
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`UPDATE tracker_history SET reassignments = NULL`); err != nil {
			return err
		}
		_, err := r.applier.Rederive(t.Context(), tx, statelog.ApplyOptions{})
		return err
	}); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	rederived, _ := handOffsOnHistory(t, r, task.ID)
	if fmt.Sprint(rederived) != fmt.Sprint(assignee) {
		t.Errorf("the re-derivation gives %v and the apply maintained %v — an "+
			"upgraded node would disagree with one that applied the records",
			rederived, assignee)
	}
	if tracker.DerivationVersion < 4 {
		t.Error("the applier derives the history hand-off count and its " +
			"derivation version does not say so — an upgraded node would never " +
			"fill the column")
	}
}

// AN ASSIGNMENT'S REASON IS THE HAND-OFF ROW'S EXCERPT — the line the history
// row stores and the wake renders — so the item's history shows "assigned it
// to ops — <why>" beside the count, with no field of its own on the record.
func TestAnAssignmentReasonIsTheHandOffRowsExcerpt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the explained one")
	agent := r.writer.As("eng", tracker.AuthorAgent, tracker.Provenance{TurnID: "turn-1"})

	const why = "ops owns the rollout from here"
	to := "ops"
	after := task
	after.Assignee = to
	notify := tracker.Wake{Kind: tracker.ChangeAssignee, Before: task, After: after,
		Excerpt: why}.Notify(nil)
	if _, err := agent.UpdateTask(t.Context(), "op-hand", task.ID, "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Assignee: &to}, tracker.ChangeAssignee, notify); err != nil {
		t.Fatalf("hand-off: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), task.ID, tracker.DetailWants{History: true},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	row := detail.History[0]
	if row.Excerpt != why || row.Reassignments == nil || *row.Reassignments != 1 {
		t.Errorf("the hand-off row is %+v — want the reason as its excerpt and "+
			"a count of 1", row)
	}
}
