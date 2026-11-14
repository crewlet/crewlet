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
