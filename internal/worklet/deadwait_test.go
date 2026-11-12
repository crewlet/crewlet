package worklet_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The pull request every dead-wait case waits on, at two heads.
const (
	deadPull  = "https://github.com/acme/web/pull/12"
	deadHeadA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	deadHeadB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// waitOn makes task id wait on each of others.
func waitOn(t *testing.T, r *roundTrip, id string, others ...string) {
	t.Helper()
	if _, err := r.writer.Depend(t.Context(), "op-wait-"+id, worklet.DependencyChange{
		Task: id, Project: "ENG", WaitingOnAdd: others}, nil); err != nil {
		t.Fatalf("wait %s on %v: %v", id, others, err)
	}
	r.drain()
}

// markDead records that nothing moves deadPull at head on task id.
func markDead(t *testing.T, r *roundTrip, id, head string) {
	t.Helper()
	if _, err := r.writer.RecordDeadWait(t.Context(), "op-dead-"+id+head, r.task(t, id).Task,
		worklet.DeadWait{Ref: deadPull, Head: head, Author: "ana", At: wednesday}); err != nil {
		t.Fatalf("record %s dead at %s: %v", id, head, err)
	}
	r.drain()
}

// A DEAD WAIT IS THE ENGINE'S RECORD ON THE TASK, AND IT MAKES THE ITEM READY:
// an item whose one open wait nothing moves is ready for its holder, named as a
// dead wait at the head recorded, never rechecked, and nobody is woken by the
// record itself; the record ends when something moves the change again, when
// the change settles, and when the item stops waiting on it.
func TestADeadWaitIsRecordedOnItsTaskAndMakesItReady(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileFor(t, r, "w", "ana", worklet.StatusInProgress, worklet.PriorityLow)
	waitOn(t, r, "w", deadPull)
	if ids := readyIDs(r.readyWork("ana")); slices.Contains(ids, "w") {
		t.Fatalf("an item waiting on an open pull request is ready before anything is recorded: %v", ids)
	}

	markDead(t, r, "w", deadHeadA)
	if d := r.task(t, "w").Task.DeadWaits[deadPull]; d.Head != deadHeadA || d.Author != "ana" {
		t.Fatalf("the task records %+v, want the dead wait at %s", d, deadHeadA)
	}
	got := r.readyWork("ana")
	if len(got.Items) != 1 || got.Items[0].ID != "w" || got.Items[0].Why != types.PickupDeadWait {
		t.Fatalf("ready work = %+v, want w as a dead wait", got.Items)
	}
	if it := got.Items[0]; len(it.DeadWaits) != 1 || it.DeadWaits[0].Head != deadHeadA || !it.Quiet.IsZero() {
		t.Errorf("the dead wait reads %+v quiet since %v, want %s at %s and no recheck", it.DeadWaits,
			it.Quiet, deadPull, deadHeadA)
	}
	for _, n := range r.wakes(t) {
		if n.Kind == worklet.ChangeDeadWait || n.Kind == worklet.ChangeDeadWaitEnded {
			t.Errorf("the dead wait's record notified %+v; the pickup alone hands it back", n)
		}
	}

	if _, err := r.writer.EndDeadWait(t.Context(), "op-moving", r.task(t, "w").Task, deadPull); err != nil {
		t.Fatalf("end the dead wait: %v", err)
	}
	r.drain()
	if _, dead := r.task(t, "w").Task.DeadWaits[deadPull]; dead || slices.Contains(readyIDs(r.readyWork("ana")), "w") {
		t.Errorf("a wait something moves again still reads dead (%v) or ready", dead)
	}

	// SETTLED: the record goes, and the item is ready as started work.
	markDead(t, r, "w", deadHeadB)
	if _, err := r.writer.SettleChange(t.Context(), "op-settle", r.task(t, "w").Task, worklet.ChangeSettlement{
		Ref: deadPull, State: worklet.ChangeMerged, At: wednesday, Source: worklet.SettledByPoll}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	r.drain()
	if _, dead := r.task(t, "w").Task.DeadWaits[deadPull]; dead {
		t.Error("a settled pull request still reads as a dead wait")
	}
	if got := r.readyWork("ana"); len(got.Items) != 1 || got.Items[0].Why != types.PickupInProgress {
		t.Errorf("after the settle ready work = %+v, want w as started work", got.Items)
	}

	// NO LONGER WAITED ON: the record goes with the edge.
	fileFor(t, r, "v", "ana", worklet.StatusTodo, worklet.PriorityLow)
	waitOn(t, r, "v", deadPull)
	markDead(t, r, "v", deadHeadA)
	if _, err := r.writer.Depend(t.Context(), "op-unwait", worklet.DependencyChange{
		Task: "v", Project: "ENG", WaitingOnRemove: []string{deadPull}}, nil); err != nil {
		t.Fatalf("stop waiting: %v", err)
	}
	r.drain()
	if d := r.task(t, "v").Task.DeadWaits; len(d) != 0 {
		t.Errorf("an item no longer waiting on the pull request still records %+v", d)
	}
}

// A DEAD WAIT IS READY ONLY WHERE NOTHING ELSE MOVES THE ITEM: not beside a
// live blocker, not in review that somebody else asked for, and not while its
// holder's question on it is open; its holder's own review nobody holds is.
func TestADeadWaitIsReadyOnlyWhereNothingElseMovesTheItem(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileFor(t, r, "blocker", "bob", worklet.StatusTodo, worklet.PriorityLow)
	fileFor(t, r, "beside", "ana", worklet.StatusTodo, worklet.PriorityLow)
	waitOn(t, r, "beside", deadPull, "blocker")
	markDead(t, r, "beside", deadHeadA)

	fileFor(t, r, "theirs", "ana", worklet.StatusInReview, worklet.PriorityLow)
	waitOn(t, r, "theirs", deadPull)
	markDead(t, r, "theirs", deadHeadA)

	fileFor(t, r, "asked", "ana", worklet.StatusInProgress, worklet.PriorityLow)
	waitOn(t, r, "asked", deadPull)
	markDead(t, r, "asked", deadHeadA)
	askOn(t, r, "op-ask", "asked", worklet.Comment{ID: "c-asked", Task: "asked", Author: "ana",
		AuthorKind: worklet.AuthorAgent, Body: "Who reviews this?", Ask: "jo", CreatedAt: wednesday})
	r.drain()

	ana := r.writer.As("ana", worklet.AuthorAgent, aTurnNobodyWoke())
	mine := newTask("mine")
	mine.Assignee, mine.Status, mine.StatusGroup = "ana", worklet.StatusTodo, worklet.StatusTodo.Group()
	if _, err := ana.CreateTask(t.Context(), "op-mine", mine, nil); err != nil {
		t.Fatalf("CreateTask mine: %v", err)
	}
	r.drain()
	review := worklet.StatusInReview
	if _, err := ana.UpdateTask(t.Context(), "op-submit", "mine", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &review}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("submit: %v", err)
	}
	r.drain()
	waitOn(t, r, "mine", deadPull)
	markDead(t, r, "mine", deadHeadA)

	if ids := readyIDs(r.readyWork("ana")); !slices.Equal(ids, []string{"mine"}) {
		t.Errorf("ana's ready work = %v, want only her own review nobody holds", ids)
	}
}

// A DEAD WAIT IS RECORDED ONLY OF A WAIT THE TASK HAS, AT A HEAD, AND ENDED
// ONLY WHERE ONE IS RECORDED.
func TestADeadWaitIsRefusedWhereTheTaskDoesNotWait(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileFor(t, r, "w", "ana", worklet.StatusTodo, worklet.PriorityLow)
	task := r.task(t, "w").Task
	if _, err := r.writer.RecordDeadWait(t.Context(), "op-stray", task, worklet.DeadWait{Ref: deadPull,
		Head: deadHeadA, At: wednesday}); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("a dead wait on a change the task does not wait on: %v, want it refused", err)
	}
	waitOn(t, r, "w", deadPull)
	task = r.task(t, "w").Task
	if _, err := r.writer.RecordDeadWait(t.Context(), "op-headless", task, worklet.DeadWait{Ref: deadPull,
		At: wednesday}); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("a dead wait with no head: %v, want it refused", err)
	}
	if _, err := r.writer.EndDeadWait(t.Context(), "op-end", task, deadPull); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("ending a dead wait nobody recorded: %v, want it refused", err)
	}
}
