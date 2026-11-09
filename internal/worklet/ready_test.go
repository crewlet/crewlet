package worklet_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

func (r *roundTrip) readyWork(handle string) worklet.ReadyWork {
	r.t.Helper()
	out, err := r.reader.ReadyWork(r.t.Context(), worklet.ReadyWorkQuery{
		Handle: handle, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		r.t.Fatalf("ReadyWork(%s): %v", handle, err)
	}
	return out
}

// fileFor files a task held by handle with a status and a priority.
func fileFor(t *testing.T, r *roundTrip, id, handle string, status worklet.Status,
	priority worklet.Priority) {
	t.Helper()
	task := newTask(id)
	task.Assignee, task.Status, task.StatusGroup = handle, status, status.Group()
	task.Priority = priority
	// FILED BY A COLLEAGUE: the harness's own writer acts as ana.
	pm := r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{})
	if _, err := pm.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

func readyIDs(w worklet.ReadyWork) []string {
	out := make([]string, 0, len(w.Items))
	for _, item := range w.Items {
		out = append(out, item.ID)
	}
	return out
}

// READY WORK COMES STARTED FIRST, THEN THE PRIORITY LIST, THEN THE QUEUE: an
// item the seat started and nothing is working on is work somebody believes
// is moving, the list is the order its lead set, and the rest go by priority.
func TestReadyWorkComesStartedFirstThenTheListThenTheQueue(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileFor(t, r, "low", "ana", worklet.StatusTodo, worklet.PriorityLow)
	fileFor(t, r, "urgent", "ana", worklet.StatusTodo, worklet.PriorityUrgent)
	fileFor(t, r, "listed", "ana", worklet.StatusTodo, worklet.PriorityLow)
	fileFor(t, r, "started", "ana", worklet.StatusInProgress, worklet.PriorityLow)
	fileFor(t, r, "theirs", "bob", worklet.StatusTodo, worklet.PriorityUrgent)
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		[]string{"listed"}, nil, worklet.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()

	got := r.readyWork("ana")
	if ids := readyIDs(got); !slices.Equal(ids, []string{"started", "listed", "urgent", "low"}) {
		t.Fatalf("ready work = %v, want started, then the list, then the queue", ids)
	}
	for i, want := range []types.PickupWhy{types.PickupInProgress, types.PickupPriorityList,
		types.PickupQueue, types.PickupQueue} {
		if got.Items[i].Why != want {
			t.Errorf("%s sits there because %q, want %q", got.Items[i].ID, got.Items[i].Why, want)
		}
	}

	// THE LAST CHANGE SOMEBODY ELSE MADE: the seat's own comment does not
	// move it, and a colleague's does.
	before := got.Items[3].Moved
	if before == 0 {
		t.Fatal("an item somebody else filed reads as never changed by anybody else")
	}
	ana := r.writer.As("ana", worklet.AuthorAgent, worklet.Provenance{})
	comment := func(w *worklet.Writer, op, author string, kind worklet.AuthorKind) {
		t.Helper()
		c := worklet.Comment{ID: op, Task: "low", Author: author, AuthorKind: kind,
			Body: "noted", CreatedAt: wednesday}
		if _, err := w.UpdateTask(t.Context(), op, "low", "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Comment: &c}, worklet.ChangeComment, nil); err != nil {
			t.Fatalf("comment %s: %v", op, err)
		}
		r.drain()
	}
	comment(ana, "c-ana", "ana", worklet.AuthorAgent)
	if after := r.readyWork("ana").Items[3].Moved; after != before {
		t.Errorf("the seat's own comment moved the item's version by somebody else "+
			"from %d to %d, so a seat that declines with a comment is handed it again", before, after)
	}
	comment(r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{}), "c-pm", "pm",
		worklet.AuthorHuman)
	if after := r.readyWork("ana").Items[3].Moved; after <= before {
		t.Errorf("a colleague's comment left the version by somebody else at %d", after)
	}
}

// READY WORK LEAVES OUT WHAT CANNOT START NOW: work waiting on an open
// blocker, work whose start date is ahead, work held on a question the seat
// asked and nobody answered, work in review, and work in the trash or done.
func TestReadyWorkLeavesOutWhatCannotStartNow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileFor(t, r, "ready", "ana", worklet.StatusTodo, worklet.PriorityNormal)
	fileFor(t, r, "blocker", "bob", worklet.StatusTodo, worklet.PriorityNormal)
	fileFor(t, r, "blocked", "ana", worklet.StatusTodo, worklet.PriorityNormal)
	fileFor(t, r, "review", "ana", worklet.StatusInReview, worklet.PriorityNormal)
	fileFor(t, r, "done", "ana", worklet.StatusDone, worklet.PriorityNormal)
	fileFor(t, r, "asked", "ana", worklet.StatusTodo, worklet.PriorityNormal)

	later := newTask("later")
	later.Assignee = "ana"
	start := wednesday.Add(48 * time.Hour)
	later.StartAt = &start
	if _, err := r.writer.CreateTask(t.Context(), "op-later", later, nil); err != nil {
		t.Fatalf("CreateTask later: %v", err)
	}
	if _, err := r.writer.Depend(t.Context(), "op-dep", worklet.DependencyChange{
		Task: "blocked", Project: "ENG", WaitingOnAdd: []string{"blocker"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()
	ana := r.writer.As("ana", worklet.AuthorAgent, worklet.Provenance{})
	ask := worklet.Comment{ID: "q-1", Task: "asked", Author: "ana",
		AuthorKind: worklet.AuthorAgent, Body: "which environment?", Ask: "pm",
		CreatedAt: wednesday}
	if _, err := ana.UpdateTask(t.Context(), "op-ask", "asked", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Comment: &ask}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()

	if ids := readyIDs(r.readyWork("ana")); !slices.Equal(ids, []string{"ready"}) {
		t.Fatalf("ready work = %v, want only the item that can start now", ids)
	}

	// THE BLOCKER CLOSING MAKES ITS DEPENDENT READY.
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "blocker", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()
	if ids := readyIDs(r.readyWork("ana")); !slices.Contains(ids, "blocked") {
		t.Errorf("ready work = %v after its blocker closed, want the dependent in it", ids)
	}
}
