package worklet_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// handOffsOnHistory is each assignee-moving row's hand-off count, oldest
// first, and every other row's alongside so a reset is visible.
func handOffsOnHistory(t *testing.T, r *roundTrip, id string) (assignee []int, all []*int) {
	t.Helper()
	detail, err := r.reader.Task(t.Context(), id, worklet.DetailWants{History: true},
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

	agent := r.writer.As("eng", worklet.AuthorAgent, worklet.Provenance{TurnID: "turn-1"})
	hand := func(opID, to string) {
		t.Helper()
		if _, err := agent.UpdateTask(t.Context(), opID, task.ID, "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Assignee: &to}, worklet.ChangeAssignee, nil); err != nil {
			t.Fatalf("hand-off to %s: %v", to, err)
		}
		r.drain()
	}
	hand("op-1", "ops")
	hand("op-2", "qa")
	// A PERSON TOUCHES SOMETHING ELSE, which returns the budget.
	if _, err := r.writer.UpdateTask(t.Context(), "op-human", task.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Title: ptr("a person looked")},
		worklet.ChangeFields, nil); err != nil {
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

	// THE BACKFILL EQUALS THE REPLAY: a node whose checkpoint's rule set
	// differs (a reanchored cursor, or a later rule set) re-derives exactly
	// what the apply maintained.
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
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
		t.Errorf("the re-derivation gives %v and the apply maintained %v — a "+
			"re-derived node would disagree with one that applied the records",
			rederived, assignee)
	}
}

// THE RE-DERIVATION WALKS EACH TASK WHOLE, whatever order the log interleaved
// them in.
//
// A row whose record states no count CARRIES the one the row before it left,
// so the walk is right only while each task's rows arrive together and in log
// order: a run of one task's rows read on its own starts from zero, and a
// carrying row in it takes the zero. One task cannot tell, and neither can
// rows that each state their own count. Two tasks whose hand-offs and quiet
// edits alternate in the log can.
func TestTheHandOffRederivationWalksEachTaskWhole(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	first := r.createTask("the first")
	second := r.createTask("the second")

	agent := r.writer.As("eng", worklet.AuthorAgent, worklet.Provenance{TurnID: "turn-1"})
	write := func(opID, id string, patch worklet.TaskPatch, kind worklet.ChangeKind) {
		t.Helper()
		if _, err := agent.UpdateTask(t.Context(), opID, id, "ENG", worklet.NoIfMatch,
			patch, kind, nil); err != nil {
			t.Fatalf("%s on %s: %v", opID, id, err)
		}
		r.drain()
	}
	hand := func(opID, id, to string) {
		t.Helper()
		write(opID, id, worklet.TaskPatch{Assignee: &to}, worklet.ChangeAssignee)
	}
	// AN AGENT'S EDIT OF ANYTHING ELSE states no count — only a person's
	// touch resets one — so its row carries the hand-off before it.
	edit := func(opID, id string) {
		t.Helper()
		write(opID, id, worklet.TaskPatch{Title: ptr(opID)}, worklet.ChangeFields)
	}
	hand("op-1", first.ID, "ops")
	hand("op-2", second.ID, "ops")
	edit("op-3", first.ID)
	edit("op-4", second.ID)
	hand("op-5", first.ID, "qa")
	edit("op-6", second.ID)
	edit("op-7", first.ID)

	maintained := map[string]string{}
	for _, id := range []string{first.ID, second.ID} {
		_, all := handOffsOnHistory(t, r, id)
		maintained[id] = counts(all)
	}
	if maintained[first.ID] != "[0 1 1 2 2]" || maintained[second.ID] != "[0 1 1 1]" {
		t.Fatalf("the apply maintained %s and %s, want [0 1 1 2 2] and [0 1 1 1]",
			maintained[first.ID], maintained[second.ID])
	}
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`UPDATE tracker_history SET reassignments = NULL`); err != nil {
			return err
		}
		_, err := r.applier.Rederive(t.Context(), tx, statelog.ApplyOptions{})
		return err
	}); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if _, all := handOffsOnHistory(t, r, id); counts(all) != maintained[id] {
			t.Errorf("%s re-derives to %s and the apply maintained %s — the walk "+
				"did not read the task's rows together and in log order",
				id, counts(all), maintained[id])
		}
	}
}

// counts spells each row's hand-off count, an absent one as nil.
func counts(rows []*int) string {
	out := make([]any, len(rows))
	for i, n := range rows {
		if n != nil {
			out[i] = *n
		}
	}
	return fmt.Sprint(out)
}

// AN ASSIGNMENT'S REASON IS THE HAND-OFF ROW'S EXCERPT — the line the history
// row stores and the wake renders — so the item's history shows "assigned it
// to ops — <why>" beside the count, with no field of its own on the record.
func TestAnAssignmentReasonIsTheHandOffRowsExcerpt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the explained one")
	agent := r.writer.As("eng", worklet.AuthorAgent, worklet.Provenance{TurnID: "turn-1"})

	const why = "ops owns the rollout from here"
	to := "ops"
	after := task
	after.Assignee = to
	notify := worklet.Wake{Kind: worklet.ChangeAssignee, Before: task, After: after,
		Excerpt: why}.Notify(nil)
	if _, err := agent.UpdateTask(t.Context(), "op-hand", task.ID, "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Assignee: &to}, worklet.ChangeAssignee, notify); err != nil {
		t.Fatalf("hand-off: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), task.ID, worklet.DetailWants{History: true},
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
