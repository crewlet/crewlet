package worklet_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A REMARK IS TAKEN DOWN BY WHOEVER MADE IT, OR BY A MODERATOR, AND THEN NOTHING
// QUOTES IT.
//
// The authority table admits the author and an admin; the writer takes the
// removal from the author and, where the caller says it moderates, from
// anybody else, deciding both against the row it read. Once taken down the
// remark keeps its row (a reply under it still resolves) with no words, and
// neither its post's history row nor the inbox notice it left quotes it.
//
// Mutation: drop the author check and bob's removal lands; drop the unquoting
// statements and bo's notice still quotes the words; drop the applier's arm and
// the remark reads as posted after its author took it down.
func TestARemarkIsTakenDownByItsAuthorOrAModerator(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-1")
	task.Assignee = "bo"
	if _, err := r.writer.CreateTask(t.Context(), "op-create", task, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()
	post := func(id, author, body string) {
		t.Helper()
		comment := worklet.Comment{ID: id, Task: "t-1", Author: author,
			AuthorKind: worklet.AuthorHuman, Body: body, CreatedAt: wednesday}
		w := r.writer.As(author, worklet.AuthorHuman, worklet.Provenance{})
		if _, err := w.UpdateTask(t.Context(), "op-"+id, "t-1", "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Comment: &comment}, worklet.ChangeComment,
			worklet.Wake{Kind: worklet.ChangeComment, Before: task, After: task,
				Comment: &comment}.Notify(nil)); err != nil {
			t.Fatalf("post %s: %v", id, err)
		}
		r.drain()
	}
	post("cm-1", "ana", "the staging password is hunter2")
	post("cm-2", "cy", "ship it on Friday")

	ana := r.writer.As("ana", worklet.AuthorHuman, worklet.Provenance{})
	bob := r.writer.As("bob", worklet.AuthorHuman, worklet.Provenance{})

	// SOMEBODY ELSE, unless they moderate.
	if _, err := bob.RemoveComment(t.Context(), "op-bob", "t-1", "ENG", "cm-1",
		worklet.CommentAuthority{}); !errors.Is(err, worklet.ErrNotAuthor) {
		t.Fatalf("bob took ana's remark down: %v", err)
	}

	// THE AUTHOR.
	if _, err := ana.RemoveComment(t.Context(), "op-ana", "t-1", "ENG", "cm-1",
		worklet.CommentAuthority{}); err != nil {
		t.Fatalf("ana's own removal: %v", err)
	}
	r.drain()

	// A MODERATOR, of a remark they did not write.
	if _, err := bob.RemoveComment(t.Context(), "op-moderate", "t-1", "ENG", "cm-2",
		worklet.CommentAuthority{Moderate: true}); err != nil {
		t.Fatalf("a moderator's removal: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), "t-1",
		worklet.DetailWants{Comments: true, History: true},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if len(detail.Comments) != 2 {
		t.Fatalf("the thread holds %d remarks after two removals, want both rows kept",
			len(detail.Comments))
	}
	for _, c := range detail.Comments {
		if !c.Removed || c.Body != "" {
			t.Errorf("%s reads removed=%v %q after it was taken down", c.ID, c.Removed, c.Body)
		}
	}
	removals := 0
	for _, h := range detail.History {
		if h.Kind == string(worklet.ChangeCommentRemoved) {
			removals++
		}
		if h.Excerpt != "" && (h.CommentID == "cm-1" || h.CommentID == "cm-2") {
			t.Errorf("the history row %s (%s) still quotes a removed remark: %q",
				h.ID, h.Kind, h.Excerpt)
		}
	}
	if removals != 2 {
		t.Errorf("the history holds %d removal rows, want one per removal", removals)
	}
	notices := r.inbox(worklet.InboxQuery{Handle: "bo"}).Notices
	if len(notices) == 0 {
		t.Fatal("bo was told of neither remark, so there is no quote to check")
	}
	for _, n := range notices {
		if n.Excerpt != "" {
			t.Errorf("bo's notice %s still quotes a removed remark: %q", n.RecordID, n.Excerpt)
		}
	}

	// AND A REMARK ALREADY TAKEN DOWN, or never posted, is not one to take down.
	for _, id := range []string{"cm-1", "cm-9"} {
		if _, err := ana.RemoveComment(t.Context(), "op-again-"+id, "t-1", "ENG", id,
			worklet.CommentAuthority{Moderate: true}); !errors.Is(err, worklet.ErrNoComment) {
			t.Errorf("removing %s answered %v, want no such comment", id, err)
		}
	}
}

// A QUESTION OR AN ANSWER IS NEVER TAKEN DOWN, by its author or a moderator.
//
// A question closes by its asker withdrawing it or by an answer, each saying
// why; an answer closed a question and said what was decided. Taking either
// down would leave an ask with no question behind it, or a question closed with
// no answer, so the writer refuses both and names what to do instead, and
// publishes nothing.
//
// Mutation: drop either refusal and that remark is taken down.
func TestAQuestionOrAnAnswerIsNotTakenDown(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("who owns the rollback")
	answers := "cm-ask"
	for _, c := range []worklet.Comment{
		{ID: "cm-ask", Task: created.ID, Author: "ana", AuthorKind: worklet.AuthorHuman,
			Body: "who owns the rollback?", Ask: "bob", CreatedAt: wednesday},
		{ID: "cm-answer", Task: created.ID, Author: "bob", AuthorKind: worklet.AuthorHuman,
			Body: "platform does", Answers: &answers, CreatedAt: wednesday},
	} {
		w := r.writer.As(c.Author, worklet.AuthorHuman, worklet.Provenance{})
		if _, err := w.UpdateTask(t.Context(), "op-"+c.ID, created.ID, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &c}, worklet.ChangeComment,
			nil); err != nil {
			t.Fatalf("post %s: %v", c.ID, err)
		}
		r.drain()
	}
	end := r.logEnd(t)
	for _, c := range []struct{ id, author, says string }{
		{"cm-ask", "ana", "withdraws"},
		{"cm-answer", "bob", "Edit its words"},
	} {
		w := r.writer.As(c.author, worklet.AuthorHuman, worklet.Provenance{})
		for _, authority := range []worklet.CommentAuthority{{}, {Moderate: true}} {
			_, err := w.RemoveComment(t.Context(), "op-remove-"+c.id, created.ID, "ENG",
				c.id, authority)
			if !errors.Is(err, worklet.ErrInvalid) || !strings.Contains(err.Error(), c.says) {
				t.Errorf("removing %s (moderate=%v) answered %v, want invalid naming %q",
					c.id, authority.Moderate, err, c.says)
			}
		}
	}
	r.drain()
	if now := r.logEnd(t); now != end {
		t.Errorf("a refused removal published: the log moved from %d to %d", end, now)
	}
}
