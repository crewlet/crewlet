package worklet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/worklet"
)

// A DATE COMES OFF BY ITS FLAG. A patch takes a start or a due date off with
// clear_start or clear_due — a date has no empty value to point at — and the
// applier takes it off; a patch that names neither the date nor its flag leaves
// it as it is.
//
// Mutation: read a patch's dates by StartAt and DueAt alone, as the applier did
// before the flags, and the dates the clear named stay set.
func TestADateComesOffByItsFlag(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	created := r.createTask("ship the rollback")
	due := time.Date(2031, 4, 16, 0, 0, 0, 0, time.UTC)
	start := due.AddDate(0, 0, -7)
	patch := func(op string, p worklet.TaskPatch) {
		t.Helper()
		if _, err := r.writer.UpdateTask(t.Context(), op, created.ID, "ENG", worklet.NoIfMatch, p,
			worklet.ChangeFields, nil); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		r.drain()
	}
	patch("op-dates", worklet.TaskPatch{StartAt: &start, DueAt: &due})
	title := "ship the rollback, then the fix"
	patch("op-title", worklet.TaskPatch{Title: &title})
	if got := r.task(t, created.ID).Task; got.StartAt == nil || !got.StartAt.Equal(start) ||
		got.DueAt == nil || !got.DueAt.Equal(due) {
		t.Fatalf("after a patch naming no date the task's dates are (%v, %v), want (%s, %s)",
			got.StartAt, got.DueAt, start, due)
	}
	patch("op-clear", worklet.TaskPatch{ClearStart: true, ClearDue: true})
	if got := r.task(t, created.ID).Task; got.StartAt != nil || got.DueAt != nil {
		t.Errorf("after the clears the task's dates are (%v, %v), want none", got.StartAt, got.DueAt)
	}
}

// THE ZERO INSTANT IS NO DATE A WRITE CARRIES. Stored, it is January of year
// one — a due date every overdue predicate reads as the most overdue task the
// company ever had — and it is no value the estate's wire carries as itself: a
// pointer at it crosses as no date at all. So a patch setting a date to it, a
// patch both setting and clearing one date, a create whose task holds it as any
// of its instants and a comment resolved at it are each refused as the content
// they are, naming the field to send instead.
//
// Mutation: drop CheckDates from Writer.UpdateTask, or the CheckInstants calls
// from createTask, and the zero instant is accepted.
func TestTheZeroInstantIsNoDateAWriteCarries(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	created := r.createTask("ship the rollback")
	zero, due := time.Time{}, time.Date(2031, 4, 16, 0, 0, 0, 0, time.UTC)
	for name, p := range map[string]worklet.TaskPatch{
		"the start set to the zero instant":    {StartAt: &zero},
		"the due date set to the zero instant": {DueAt: &zero},
		"the start both set and cleared":       {StartAt: &due, ClearStart: true},
		"the due date both set and cleared":    {DueAt: &due, ClearDue: true},
		"a comment resolved at the zero instant": {Comment: &worklet.Comment{ID: "cm-1",
			Task: created.ID, Author: "bo", AuthorKind: worklet.AuthorHuman, Body: "done",
			ResolvedAt: &zero, CreatedAt: wednesday}},
	} {
		_, err := r.writer.UpdateTask(t.Context(), "op-refused", created.ID, "ENG", worklet.NoIfMatch, p,
			worklet.ChangeFields, nil)
		if !errors.Is(err, worklet.ErrInvalid) {
			t.Errorf("a patch with %s was answered %v, want a refusal of its content", name, err)
		}
	}
	for name, mutate := range map[string]func(*worklet.Task){
		"a start at the zero instant":          func(task *worklet.Task) { task.StartAt = &zero },
		"a due date at the zero instant":       func(task *worklet.Task) { task.DueAt = &zero },
		"a done stamp at the zero instant":     func(task *worklet.Task) { task.DoneAt = &zero },
		"a closed stamp at the zero instant":   func(task *worklet.Task) { task.ClosedAt = &zero },
		"an archive stamp at the zero instant": func(task *worklet.Task) { task.ArchivedAt = &zero },
	} {
		task := newTask("t-zero")
		task.Title = "zero"
		mutate(&task)
		if _, err := r.writer.CreateTask(t.Context(), "op-zero", task, nil); !errors.Is(err,
			worklet.ErrInvalid) {
			t.Errorf("a create holding %s was answered %v, want a refusal of its content", name, err)
		}
	}
	task := newTask("t-ask")
	task.Title = "asked"
	if _, err := r.writer.CreateTaskAsking(t.Context(), "op-ask", task, worklet.Comment{ID: "cm-ask",
		Task: task.ID, Author: "bo", AuthorKind: worklet.AuthorHuman, Body: "which?", Ask: "ana",
		ResolvedAt: &zero, CreatedAt: wednesday}, nil); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("a create asking a question resolved at the zero instant was answered %v, want a "+
			"refusal of its content", err)
	}
}
