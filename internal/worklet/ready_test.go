package worklet_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
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

	// THE LAST CHANGE THAT WOULD HAVE WOKEN THE SEAT: its own comment does
	// not move it, and a person's does.
	before := got.Items[3].Moved
	ana := r.writer.As("ana", worklet.AuthorAgent, worklet.Provenance{})
	comment := func(w *worklet.Writer, op, author string, kind worklet.AuthorKind) {
		t.Helper()
		c := worklet.Comment{ID: op, Task: "low", Author: author, AuthorKind: kind,
			Body: "noted", CreatedAt: wednesday}
		detail, err := r.reader.Task(t.Context(), "low", worklet.DetailWants{},
			statelog.Freshness{Level: statelog.ReadSession})
		if err != nil {
			t.Fatalf("read low: %v", err)
		}
		wake := worklet.Wake{Kind: worklet.ChangeComment, Before: detail.Task, After: detail.Task,
			Comment: &c}.Notify(nil)
		if _, err := w.UpdateTask(t.Context(), op, "low", "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Comment: &c}, worklet.ChangeComment, wake); err != nil {
			t.Fatalf("comment %s: %v", op, err)
		}
		r.drain()
	}
	comment(ana, "c-ana", "ana", worklet.AuthorAgent)
	if after := r.readyWork("ana").Items[3].Moved; after != before {
		t.Errorf("the seat's own comment moved the item's key "+
			"from %d to %d, so a seat that declines with a comment is handed it again", before, after)
	}
	comment(r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{}), "c-pm", "pm",
		worklet.AuthorHuman)
	if after := r.readyWork("ana").Items[3].Moved; after <= before {
		t.Errorf("a person's comment left the item's key at %d", after)
	}
}

// READY WORK LEAVES OUT WHAT CANNOT START NOW: work waiting on an open
// blocker, work whose start date is ahead, work held on a question the seat
// asked and nobody answered, work in review the seat submitted itself, and work
// in the trash or done.
func TestReadyWorkLeavesOutWhatCannotStartNow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileFor(t, r, "ready", "ana", worklet.StatusTodo, worklet.PriorityNormal)
	fileFor(t, r, "blocker", "bob", worklet.StatusTodo, worklet.PriorityNormal)
	fileFor(t, r, "blocked", "ana", worklet.StatusTodo, worklet.PriorityNormal)
	submitted := newTask("review")
	submitted.Assignee, submitted.Status = "ana", worklet.StatusInReview
	submitted.StatusGroup = worklet.StatusInReview.Group()
	if _, err := r.writer.As("ana", worklet.AuthorAgent, worklet.Provenance{}).CreateTask(t.Context(),
		"op-review", submitted, nil); err != nil {
		t.Fatalf("CreateTask review: %v", err)
	}
	r.drain()
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

// READY WORK SAYS WHOM EACH ITEM'S WORK IS FOR (ADR-0109): the writer of its
// last assignment by somebody else, else the writer that filed it, a
// credential with the apps it wrote under; nobody for the seat's own work.
//
// Mutation: read the reporter column, and the item a token filed already
// assigned is for a seat named `token:ci` that no directory holds.
func TestReadyWorkSaysWhomTheWorkIsFor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	pm := r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{})
	token := r.writer.As("token:ci", worklet.AuthorOperator,
		worklet.Provenance{Apps: []iam.Grant{iam.GrantWorklet}})
	ana := r.writer.As("ana", worklet.AuthorAgent, worklet.Provenance{})
	file := func(w *worklet.Writer, id, assignee string) {
		t.Helper()
		task := newTask(id)
		task.Assignee = assignee
		var n *worklet.Notify
		if assignee != "" {
			n = &worklet.Notify{Kind: worklet.ChangeCreated, Snapshot: worklet.Snapshot{Assignee: assignee}}
		}
		if _, err := w.CreateTask(t.Context(), "op-"+id, task, n); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	assign := func(w *worklet.Writer, id string) {
		t.Helper()
		to := "ana"
		if _, err := w.UpdateTask(t.Context(), "op-assign-"+id, id, "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Assignee: &to}, worklet.ChangeAssignee,
			&worklet.Notify{Kind: worklet.ChangeAssignee,
				Snapshot: worklet.Snapshot{Assignee: "ana"}}); err != nil {
			t.Fatalf("assign %s: %v", id, err)
		}
		r.drain()
	}
	file(pm, "by-token", "")
	assign(token, "by-token")
	file(token, "filed-by-token", "ana")
	file(pm, "filed-by-pm", "")
	assign(ana, "filed-by-pm")
	file(ana, "own", "ana")

	got := map[string]*types.WorkingFor{}
	for _, item := range r.readyWork("ana").Items {
		got[item.ID] = item.For
	}
	credential := &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
		Apps: []iam.Grant{iam.GrantWorklet}}
	want := map[string]*types.WorkingFor{
		"by-token":       credential,
		"filed-by-token": credential,
		"filed-by-pm":    {Kind: types.WorkingForSeat, Seat: "pm"},
		"own":            nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the work is for %+v, want %+v", got, want)
	}
}

// THE WAKE INSTANT IS THE CHANGE THE KEY NAMES, and the seat's progress on
// started work is its own change other than a comment: a comment moves
// neither, a field it sets moves the progress alone.
func TestReadyWorkTimesAndProgressFollowTheKey(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileFor(t, r, "started", "ana", worklet.StatusInProgress, worklet.PriorityLow)
	first := r.readyWork("ana").Items[0]
	if first.MovedAt.IsZero() {
		t.Fatal("an item somebody else filed has no instant for the change its key names")
	}
	ana := r.writer.As("ana", worklet.AuthorAgent, worklet.Provenance{})
	c := worklet.Comment{ID: "c-1", Task: "started", Author: "ana", AuthorKind: worklet.AuthorAgent,
		Body: "on it", CreatedAt: wednesday}
	if _, err := ana.UpdateTask(t.Context(), "op-c", "started", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Comment: &c}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	commented := r.readyWork("ana").Items[0]
	if !commented.MovedAt.Equal(first.MovedAt) || commented.Progressed != first.Progressed {
		t.Errorf("the seat's comment moved the instant %s -> %s or the progress %d -> %d",
			first.MovedAt, commented.MovedAt, first.Progressed, commented.Progressed)
	}
	// A WRITE THAT CHANGES NOTHING IS NO PROGRESS.
	low := worklet.PriorityLow
	if _, err := ana.UpdateTask(t.Context(), "op-same", "started", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Priority: &low}, worklet.ChangeFields, nil); err != nil {
		t.Fatalf("set the same priority: %v", err)
	}
	r.drain()
	if same := r.readyWork("ana").Items[0]; same.Progressed != commented.Progressed {
		t.Errorf("a write that changed nothing moved the progress %d -> %d",
			commented.Progressed, same.Progressed)
	}
	high := worklet.PriorityHigh
	if _, err := ana.UpdateTask(t.Context(), "op-p", "started", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Priority: &high}, worklet.ChangeFields, nil); err != nil {
		t.Fatalf("set the priority: %v", err)
	}
	r.drain()
	if moved := r.readyWork("ana").Items[0]; moved.Progressed <= commented.Progressed ||
		!moved.MovedAt.Equal(first.MovedAt) {
		t.Errorf("the seat's field change left the progress at %d (was %d) or moved the instant",
			moved.Progressed, commented.Progressed)
	}
}

// READY WORK IS PAGED PAST THE FIRST PAGE, so item 21 onwards can be offered.
func TestReadyWorkIsPaged(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for i := range worklet.ReadyRows + 3 {
		fileFor(t, r, fmt.Sprintf("t%02d", i), "ana", worklet.StatusTodo, worklet.PriorityNormal)
	}
	first := r.readyWork("ana")
	if len(first.Items) != worklet.ReadyRows || !first.More || first.Total != worklet.ReadyRows+3 {
		t.Fatalf("page 0 = %d items, more %v, total %d", len(first.Items), first.More, first.Total)
	}
	second, err := r.reader.ReadyWork(t.Context(), worklet.ReadyWorkQuery{
		Handle: "ana", Level: statelog.ReadStale, Page: 1}, wednesday)
	if err != nil || len(second.Items) != 3 || second.More || second.Fingerprint != first.Fingerprint {
		t.Fatalf("page 1 = %d items, more %v (%v), want the last 3 under one fingerprint",
			len(second.Items), second.More, err)
	}
}

// A REVIEW IS READY FOR THE SEAT THAT HOLDS IT WHEN SOMEBODY ELSE ASKED FOR IT:
// it comes after started work and before work not started, and a seat never
// reviews what it moved into review itself, whoever holds it after.
func TestAReviewIsReadyForTheSeatThatHoldsIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fileFor(t, r, "queued", "ana", worklet.StatusTodo, worklet.PriorityUrgent)
	fileFor(t, r, "started", "ana", worklet.StatusInProgress, worklet.PriorityLow)
	fileFor(t, r, "asked", "ana", worklet.StatusInReview, worklet.PriorityLow)
	ana := r.writer.As("ana", worklet.AuthorAgent, worklet.Provenance{})
	mine := newTask("mine")
	mine.Assignee, mine.Status, mine.StatusGroup = "ana", worklet.StatusTodo, worklet.StatusTodo.Group()
	if _, err := ana.CreateTask(t.Context(), "op-mine", mine, nil); err != nil {
		t.Fatalf("CreateTask mine: %v", err)
	}
	r.drain()
	review, bob := worklet.StatusInReview, "bob"
	if _, err := ana.UpdateTask(t.Context(), "op-submit", "mine", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &review}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("submit: %v", err)
	}
	r.drain()

	got := r.readyWork("ana")
	if ids := readyIDs(got); !slices.Equal(ids, []string{"started", "asked", "queued"}) {
		t.Fatalf("ana's ready work = %v, want started, then the review she was asked for, then the queue", ids)
	}
	if got.Items[1].Why != types.PickupReview {
		t.Errorf("the review sits there because %q, want %q", got.Items[1].Why, types.PickupReview)
	}
	if _, err := ana.UpdateTask(t.Context(), "op-hand", "mine", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Assignee: &bob}, worklet.ChangeAssignee, nil); err != nil {
		t.Fatalf("hand to bob: %v", err)
	}
	r.drain()
	if ids := readyIDs(r.readyWork("bob")); !slices.Equal(ids, []string{"mine"}) {
		t.Errorf("bob's ready work = %v, want the review ana handed him", ids)
	}
}
