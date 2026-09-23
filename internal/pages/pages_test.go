package pages_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
)

// A RESTORE PUBLISHES A PAGE WHATEVER IT WAS BEFORE THE TRASH.
//
// The trash records when a page went in and not what it was, so a draft that
// is trashed and restored comes back published — its first publication.
// [pages.Status] says so; this case is what keeps that sentence true rather
// than remembered, and what a change to the restore has to answer to.
func TestARestoredDraftComesBackPublished(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	draft := r.write(author("jane"), pages.NewPage{
		Title: "Half a thought", Body: "not ready", Status: pages.StatusDraft,
	})
	// THE CONTROL: it starts as a draft, so the status below is the
	// restore's doing rather than the create's.
	if got := r.get(draft.Page.ID).Page.Status; got != pages.StatusDraft {
		t.Fatalf("the draft was created %q", got)
	}

	if _, err := r.store.Trash(t.Context(), author("jane"), draft.Page.ID); err != nil {
		t.Fatalf("trash: %v", err)
	}
	r.drain()
	if _, err := r.store.Restore(t.Context(), author("jane"), draft.Page.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	r.drain()
	if got := r.get(draft.Page.ID).Page.Status; got != pages.StatusPublished {
		t.Errorf("a trashed draft was restored as %q, want %q — the doc on "+
			"pages.Status says a restore publishes it", got, pages.StatusPublished)
	}
}
