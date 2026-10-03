package pages_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
)

// AN EDITED COMMENT IS STILL THE COMMENT IT WAS: it keeps the instant it was
// written and carries the instant it was edited beside it — not a remark
// somebody made just now, with nothing to say it was ever changed.
func TestAnEditedCommentKeepsWhenItWasWrittenAndIsRecordedAsAnEdit(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	comment, _, err := r.store.Comment(t.Context(), author("maya"), page.Page.ID,
		pages.NewComment{Body: "is this still true?"})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	written := r.get(page.Page.ID).Comments[0].CreatedAt

	time.Sleep(20 * time.Millisecond)
	if _, _, err := r.store.EditComment(t.Context(), author("maya"), page.Page.ID, comment.ID,
		"is this still true after the migration?"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	r.drain()

	// A SECOND EDIT, whose own answer is built from the document the FIRST
	// edit's apply wrote — the reader that reported the previous edit's
	// instant as when the comment was written.
	time.Sleep(20 * time.Millisecond)
	answered, _, err := r.store.EditComment(t.Context(), author("maya"), page.Page.ID,
		comment.ID, "is this still true after the second migration?")
	if err != nil {
		t.Fatalf("second edit: %v", err)
	}
	r.drain()
	if !answered.CreatedAt.Equal(written) {
		t.Errorf("the edit answered created_at %s, want %s — the document "+
			"recorded the previous edit as when the comment was written",
			answered.CreatedAt, written)
	}

	// EVERY READER OF THE DOCUMENT AGREES: the page detail and the store's
	// own thread read the same row.
	thread, err := r.store.Thread(t.Context(), page.Page.ID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	for name, got := range map[string][]pages.Comment{
		"the page detail": r.get(page.Page.ID).Comments,
		"Store.Thread":    thread,
	} {
		if len(got) != 1 {
			t.Fatalf("%s: comments = %d, want the one, edited", name, len(got))
		}
		if !got[0].CreatedAt.Equal(written) {
			t.Errorf("%s: created_at moved from %s to %s on an edit",
				name, written, got[0].CreatedAt)
		}
		if !got[0].UpdatedAt.After(got[0].CreatedAt) {
			t.Errorf("%s: updated_at %s is not after created_at %s — the edit is invisible",
				name, got[0].UpdatedAt, got[0].CreatedAt)
		}
	}

}
