package tracker_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A REMARK IS REWRITTEN BY WHOEVER MADE IT AND BY NOBODY ELSE.
//
// The authority table admits the deployment grant beside the author on a
// comment, because it answers removal and edit alike; the WRITE is narrower,
// for internal/pages' reason — a remark somebody else can rewrite is a remark
// attributed to a person who did not make it, quoted in the wake it sends. So
// the refusal has to be the writer's own, decided against the row it read,
// and a caller holding every grant there is still refused here.
func TestOnlyACommentsAuthorMayEditIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("who owns the rollback")
	asked := "bob"
	if _, err := r.writer.UpdateTask(t.Context(), "op-ask", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-1", Task: created.ID, Author: "ana",
			AuthorKind: tracker.AuthorHuman, Body: "who owns the rollback?",
			Ask: asked, Mentions: []string{"bob"}, CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	// SOMEBODY ELSE, however they are authorised.
	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
	if _, err := bob.EditComment(t.Context(), "op-bob", created.ID, "ENG",
		"cm-1", "platform owns it", nil); !errors.Is(err, tracker.ErrNotAuthor) {
		t.Fatalf("bob rewrote ana's remark: %v", err)
	}

	// THE AUTHOR, and the edit keeps everything the remark was besides its
	// words: the question it asked and who it woke. The apply is an upsert
	// of the whole row, so an edit carrying only the body would have
	// closed a question nobody answered.
	if _, err := r.writer.EditComment(t.Context(), "op-ana", created.ID, "ENG",
		"cm-1", "who owns the rollback now?", nil); err != nil {
		t.Fatalf("ana's own edit: %v", err)
	}
	r.drain()
	detail, err := r.reader.Task(t.Context(), created.ID,
		tracker.DetailWants{Comment: "cm-1"},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || len(detail.Comments) != 1 {
		t.Fatalf("read the comment: %v (%d)", err, len(detail.Comments))
	}
	got := detail.Comments[0]
	if got.Body != "who owns the rollback now?" {
		t.Errorf("the remark reads %q after its author's edit", got.Body)
	}
	if got.Ask != asked || len(got.Mentions) != 1 || got.Author != "ana" {
		t.Errorf("the edit rewrote more than the words: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Errorf("an edited remark carries no edit instant: %+v", got)
	}

	// AND A REMARK THAT IS NOT THERE is not one anybody wrote.
	if _, err := r.writer.EditComment(t.Context(), "op-ghost", created.ID,
		"ENG", "cm-9", "anything", nil); !errors.Is(err, tracker.ErrNoComment) {
		t.Errorf("an edit to a comment that does not exist answered %v", err)
	}
}
