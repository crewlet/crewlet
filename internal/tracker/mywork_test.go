package tracker_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) myWork(handle string) tracker.MyWork {
	r.t.Helper()
	out, err := r.reader.MyWork(r.t.Context(), tracker.MyWorkQuery{
		Handle: handle, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		r.t.Fatalf("MyWork(%s): %v", handle, err)
	}
	return out
}

// askOn writes one comment onto a task, which is how a comment is written at
// all: a comment is a MUTATION of its task, arbitrated by the task's own
// version, so it rides the task's write rather than having a verb of its own.
func askOn(t *testing.T, r *roundTrip, opID, task string, comment tracker.Comment) {
	t.Helper()
	if _, err := r.writer.UpdateTask(t.Context(), opID, task, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &comment}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("comment on %s: %v", task, err)
	}
	r.drain()
}

// assign files a task held by one handle.
func assign(t *testing.T, r *roundTrip, id, handle string) {
	t.Helper()
	task := newTask(id)
	task.Assignee = handle
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// PRIORITIES COME BACK IN THEIR STORED ORDER, because the order IS the
// content: it is what somebody decided, and re-sorting it by anything at all
// discards the decision.
func TestMyWorkKeepsThePriorityOrder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"a", "b", "c"} {
		assign(t, r, id, "ana")
	}
	// DELIBERATELY NOT ALPHABETICAL and not creation order, so an answer
	// sorted by either is visibly wrong.
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		[]string{"c", "a", "b"}, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()

	got := r.myWork("ana")
	var order []string
	for _, row := range got.Priorities {
		order = append(order, row.ID)
	}
	if len(order) != 3 || order[0] != "c" || order[1] != "a" || order[2] != "b" {
		t.Fatalf("priorities came back as %v, want [c a b] — the stored order "+
			"is what somebody decided, and sorting it discards the decision",
			order)
	}

	// A FINISHED TASK DROPS OUT OF THE LIST WITHOUT THE LIST BEING
	// REWRITTEN. A read must not write to somebody's own object, so it
	// filters — and the next write to the list drops it for good.
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-done", "a", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()
	after := r.myWork("ana")
	if len(after.Priorities) != 2 || after.Priorities[0].ID != "c" ||
		after.Priorities[1].ID != "b" {
		t.Fatalf("after finishing one, priorities are %+v, want c then b",
			after.Priorities)
	}
	if held := r.person("ana"); len(held.Priorities) != 3 {
		t.Errorf("the stored list is now %v — a READ rewrote somebody's own "+
			"object", held.Priorities)
	}
}

// THE SEVEN BLOCKS ARE SEVEN DIFFERENT CLAIMS, and a seat that saw only its
// assignments would miss six of them.
func TestMyWorkSeparatesTheSevenClaims(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "mine", "ana")
	assign(t, r, "theirs", "bob")

	// COLLABORATING: brought on without owning. It is a separate block
	// precisely because it is not an assignment.
	if _, err := r.writer.UpdateTask(t.Context(), "op-collab", "theirs", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Collaborators: &[]string{"ana"},
		}, tracker.ChangeCollaborators, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	got := r.myWork("ana")
	if len(got.Assigned) != 1 || got.Assigned[0].ID != "mine" {
		t.Fatalf("assigned is %+v, want the one task ana holds", got.Assigned)
	}
	if len(got.Collaborating) != 1 || got.Collaborating[0].ID != "theirs" {
		t.Fatalf("collaborating is %+v, want the task she was brought onto",
			got.Collaborating)
	}
	// AND THE TWO NEVER OVERLAP: a task already in `assigned` listed again
	// under `collaborating` is one row spending two of the seven blocks.
	for _, row := range got.Collaborating {
		if row.Assignee == "ana" {
			t.Errorf("%s is in collaborating and ana holds it — the block is "+
				"for work she was brought onto WITHOUT owning", row.ID)
		}
	}
	if got.Handle != "ana" {
		t.Errorf("the answer names %q", got.Handle)
	}
}

// AN ASK CARRIES THE CALL THAT ANSWERS IT.
//
// A model handed a comment id still has to compose the answer, and every one
// it composes differently is a round spent being refused.
func TestMyWorkCarriesTheAnswerForm(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "asked", "ana")
	askOn(t, r, "op-ask", "asked", tracker.Comment{
		ID: "c-1", Task: "asked", Author: "ana", AuthorKind: tracker.AuthorHuman,
		Body: "which region?", Ask: "ana", CreatedAt: wednesday,
	})

	got := r.myWork("ana")
	if len(got.AskedOfMe) != 1 {
		t.Fatalf("asked_of_me is %+v, want the one open ask", got.AskedOfMe)
	}
	ask := got.AskedOfMe[0]
	if ask.Comment == "" {
		t.Error("the ask names no comment, so nothing can answer it")
	}
	if !strings.Contains(ask.Answer, tracker.CommentOnWorkTool) ||
		!strings.Contains(ask.Answer, ask.Comment) {
		t.Errorf("the answer form is %q and does not carry the tool and the "+
			"comment — a model composing it itself spends a round being "+
			"refused", ask.Answer)
	}
	if ask.Body == "" || ask.AskedBy != "ana" {
		t.Errorf("the ask is %+v, want the question and who asked it", ask)
	}
	if ask.Key == "" {
		t.Error("the ask carries no task key, so it cannot be opened")
	}

	// AND AN ANSWERED ASK LEAVES THE BLOCK, because the block is what is
	// still waiting.
	answers := ask.Comment
	askOn(t, r, "op-answer", "asked", tracker.Comment{
		ID: "c-2", Task: "asked", Author: "ana", AuthorKind: tracker.AuthorHuman,
		Body: "eu-west-1", Answers: &answers, CreatedAt: wednesday,
	})
	if after := r.myWork("ana"); len(after.AskedOfMe) != 0 {
		t.Errorf("asked_of_me still holds %d after the ask was answered",
			len(after.AskedOfMe))
	}
}

// MY_WORK NAMES SOMEBODY, always. A day with nobody's name on it is
// everybody's, and a reader that defaulted the viewer would answer about
// whoever it happened to pick.
func TestMyWorkNamesSomebody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.reader.MyWork(t.Context(), tracker.MyWorkQuery{
		Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("my_work with no handle answered")
	}
	if _, err := r.reader.MyWork(t.Context(), tracker.MyWorkQuery{
		Handle: "ana",
	}, wednesday); err == nil {
		t.Error("my_work with no read level answered — a level a surface did " +
			"not resolve is a label rather than a guarantee")
	}
}
