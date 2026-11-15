package doclet_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
)

// A DRAFT IS PUBLISHED BY A SAVE THAT SAYS SO, and by nothing else. A page
// written as a draft is left out of what every reader lists; a save naming
// `published` moves it, with no new version since its body did not change;
// and a save that names no status leaves a draft a draft, so an edit to
// unfinished prose does not publish it. The same save takes a page back.
//
// Mutation: drop the status from the save's patch (Store.patchOf) and the
// draft is still a draft after the save that published it.
func TestADraftIsPublishedByASaveThatSaysSo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	draft := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "half written",
		Status: doclet.StatusDraft})
	r.drain()
	published := func() []string {
		return titles(r.list(doclet.Filter{Container: "ENG", Status: []doclet.Status{doclet.StatusPublished}}))
	}
	if got := published(); len(got) != 0 {
		t.Fatalf("a draft is listed among the published pages: %v", got)
	}

	// AN EDIT KEEPS IT A DRAFT.
	edited, err := r.store.SavePage(t.Context(), author("jane"), draft.Page.ID,
		doclet.Save{BaseVersion: 1, Body: ptr("written out")})
	if err != nil {
		t.Fatalf("edit the draft: %v", err)
	}
	r.drain()
	if edited.Page.Status != doclet.StatusDraft || len(published()) != 0 {
		t.Fatalf("an edit that named no status left the page %s, listed published as %v",
			edited.Page.Status, published())
	}

	status := doclet.StatusPublished
	got, err := r.store.SavePage(t.Context(), author("jane"), draft.Page.ID,
		doclet.Save{BaseVersion: edited.Page.Version, Status: &status})
	if err != nil {
		t.Fatalf("publish the draft: %v", err)
	}
	r.drain()
	if got.Page.Status != doclet.StatusPublished || got.Page.Version != edited.Page.Version {
		t.Errorf("the publish answered status %s at version %d, want published at the version it had (%d)",
			got.Page.Status, got.Page.Version, edited.Page.Version)
	}
	if listed := published(); !slices.Equal(listed, []string{"Runbook"}) {
		t.Errorf("after the publish the published pages are %v, want Runbook", listed)
	}
	if head := r.get(draft.Page.ID).Page; head.Status != doclet.StatusPublished {
		t.Errorf("the page reads back as %s", head.Status)
	}

	// AND BACK: the same save takes a published page out of every listing of
	// published ones.
	status = doclet.StatusDraft
	if _, err := r.store.SavePage(t.Context(), author("jane"), draft.Page.ID,
		doclet.Save{BaseVersion: got.Page.Version, Status: &status}); err != nil {
		t.Fatalf("take the page back to a draft: %v", err)
	}
	r.drain()
	if listed := published(); len(listed) != 0 {
		t.Errorf("a page taken back to a draft is still listed published: %v", listed)
	}
}

// THE TRASH IS NOT A STATUS A SAVE SETS, in either direction. A save naming
// `trashed` is refused: the trash stamps when a page went in, which the
// reaper's term counts from. And a save naming a status on a trashed page is
// refused: only a restore clears that instant, and a page published over it
// would be found by every reader and purged on schedule all the same.
//
// Mutation: admit any valid status in Store.SavePage, or drop the trashed
// head's refusal from its decision, and a page is trashed or brought back
// with no trash instant moved.
func TestASaveNeverMovesAPageIntoOrOutOfTheTrash(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	r.drain()

	trashed := doclet.StatusTrashed
	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		doclet.Save{BaseVersion: 1, Status: &trashed}); !errors.Is(err, doclet.ErrInvalid) {
		t.Fatalf("a save naming the trash = %v, want it refused as invalid", err)
	}
	r.drain()
	if head := r.get(page.Page.ID).Page; head.Status != doclet.StatusPublished || head.TrashedAt != nil {
		t.Fatalf("the refused save left the page %s, trashed at %v", head.Status, head.TrashedAt)
	}

	if _, err := r.store.Trash(t.Context(), author("jane"), page.Page.ID); err != nil {
		t.Fatalf("trash: %v", err)
	}
	r.drain()
	published := doclet.StatusPublished
	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		doclet.Save{BaseVersion: 1, Status: &published}); !errors.Is(err, doclet.ErrInvalid) {
		t.Fatalf("a save publishing a trashed page = %v, want it refused as invalid", err)
	}
	r.drain()
	if head := r.get(page.Page.ID).Page; head.Status != doclet.StatusTrashed || head.TrashedAt == nil {
		t.Errorf("the refused save left the trashed page %s, trashed at %v", head.Status, head.TrashedAt)
	}
}

// A PAGE IN THE TRASH TAKES NO CHANGE BUT A RESTORE. It is deleted as far as
// any reader is concerned and still recoverable, so it is kept as it was
// trashed: a save of its body, its labels or its watchers, a rename and every
// comment gesture are refused, and once it is restored each lands again. Only
// a status was refused once, so a deleted page could be rewritten,
// relabelled, renamed and discussed while nobody could find it.
//
// Mutation: drop refuseTrashed from any one write's decision (SavePage,
// either path of Rename, Comment, EditComment, RemoveComment) and that write
// lands on the trashed page.
func TestATrashedPageTakesNoChangeButARestore(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{Title: "Vendor notes", Body: "prose"})
	r.drain()
	comment, _, err := r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		doclet.NewComment{Body: "Is this still true?"})
	if err != nil {
		t.Fatalf("comment before the trash: %v", err)
	}
	r.drain()
	if _, err = r.store.Trash(t.Context(), author("jane"), page.Page.ID); err != nil {
		t.Fatalf("trash: %v", err)
	}
	r.drain()
	head := r.get(page.Page.ID).Page
	if head.Status != doclet.StatusTrashed {
		t.Fatalf("the page is %s after a trash", head.Status)
	}

	body, labels, watch := "rewritten", []string{"old"}, true
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, doclet.ErrInvalid) {
			t.Errorf("%s on a trashed page = %v, want it refused", what, err)
		}
	}
	_, err = r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		doclet.Save{BaseVersion: head.Version, Body: &body})
	refused("a body save", err)
	_, err = r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		doclet.Save{BaseVersion: head.Version, Labels: &labels})
	refused("a label save", err)
	_, err = r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		doclet.Save{BaseVersion: head.Version, Watch: &watch})
	refused("a watch", err)
	_, err = r.store.Rename(t.Context(), author("jane"), page.Page.ID, "Supplier notes", false)
	refused("a rename to a new address", err)
	_, err = r.store.Rename(t.Context(), author("jane"), page.Page.ID, "VENDOR NOTES", false)
	refused("a retitle at the same address", err)
	_, _, err = r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		doclet.NewComment{Body: "Still here?"})
	refused("a comment", err)
	_, _, err = r.store.EditComment(t.Context(), author("jane"), page.Page.ID,
		comment.ID, "Is this still right?")
	refused("a comment edit", err)
	_, err = r.store.RemoveComment(t.Context(), author("jane"), page.Page.ID,
		comment.ID, doclet.CommentAuthority{})
	refused("a comment removal", err)
	r.drain()
	after := r.get(page.Page.ID)
	if after.Page.Body != "prose" || after.Page.Title != "Vendor notes" ||
		len(after.Page.Labels) != 0 || len(after.Comments) != 1 {
		t.Errorf("a refused write moved the trashed page: %q %q %v, %d comments",
			after.Page.Body, after.Page.Title, after.Page.Labels, len(after.Comments))
	}

	// RESTORED, IT TAKES A CHANGE AGAIN.
	if _, err = r.store.Restore(t.Context(), author("jane"), page.Page.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	r.drain()
	head = r.get(page.Page.ID).Page
	if _, err = r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		doclet.Save{BaseVersion: head.Version, Body: &body}); err != nil {
		t.Errorf("a save after the restore: %v", err)
	}
	if _, _, err = r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		doclet.NewComment{Body: "Back again."}); err != nil {
		t.Errorf("a comment after the restore: %v", err)
	}
}
