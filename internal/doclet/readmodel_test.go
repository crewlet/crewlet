package doclet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
)

// A PAGE NAMES THE PERSON WHO WROTE IT UNDER THE ONE NAME THEY WRITE AS, their
// seat, on its author, every revision, every comment and the listing's last
// save, whichever credential carried each write. There is no second column to
// read the person off: a screen draws the name the row holds, and the
// credential is the operator id beside it on the page's activity.
func TestAPageNamesThePersonWhoWroteItAsTheirSeat(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.write(personThroughToken(), doclet.NewPage{Title: "Runbook", Body: "prose"})
	if _, err := r.store.SavePage(t.Context(), personThroughToken(), created.Page.ID,
		doclet.Save{BaseVersion: 1, Body: ptr("revised")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	comment, _, err := r.store.Comment(t.Context(), personThroughToken(), created.Page.ID,
		doclet.NewComment{Body: "Looks right."})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	detail := r.get(created.Page.ID)
	if detail.Page.Author != "jane" {
		t.Errorf("the page names its author %q, want the person's seat", detail.Page.Author)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].ID != comment.ID ||
		detail.Comments[0].Author != "jane" {
		t.Errorf("the thread reads %+v, want one comment by jane", detail.Comments)
	}
	if len(detail.History) != 2 {
		t.Fatalf("the page has %d revisions, want 2", len(detail.History))
	}
	for _, revision := range detail.History {
		if revision.Author != "jane" {
			t.Errorf("revision %d names %q, want the person's seat",
				revision.Version, revision.Author)
		}
	}
	summary := r.list(doclet.Filter{Container: "ENG"}).Pages[0]
	if summary.Author != "jane" || summary.SavedBy != "jane" {
		t.Errorf("the listing names author %q and last saver %q, want jane for both",
			summary.Author, summary.SavedBy)
	}
}

// A FOLDER IS A PAGE WITH NOTHING WRITTEN IN IT, derived from the body and
// never stored, so the mark cannot disagree with the page.
func TestAFolderIsAPageWithNothingWrittenInIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	folder := r.write(author("jane"), doclet.NewPage{Title: "Designs", Body: ""})
	r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})

	marks := map[string]bool{}
	for _, page := range r.list(doclet.Filter{Container: "ENG"}).Pages {
		marks[page.Title] = page.Folder
	}
	if !marks["Designs"] || marks["Runbook"] {
		t.Fatalf("the listing marks folders %v, want Designs alone", marks)
	}
	if !r.get(folder.Page.ID).Folder {
		t.Fatal("the folder's own page does not say it is one")
	}

	// A paragraph written into a folder makes it a page with children, and
	// emptying it makes it a folder again.
	if _, err := r.store.SavePage(t.Context(), author("jane"), folder.Page.ID,
		doclet.Save{BaseVersion: 1, Body: ptr("Technical designs, one per work item.")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	if r.get(folder.Page.ID).Folder {
		t.Fatal("a folder with a paragraph in it is still marked a folder")
	}
}

// THE LAST SAVE IS A CHANGE TO WHAT THE PAGE SAYS, not every change: a label,
// a watch or a move moves the page's update time and leaves who last saved it
// where it was.
func TestTheLastSaveIsAChangeToWhatThePageSays(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	if _, err := r.store.SavePage(t.Context(), author("bob"), created.Page.ID,
		doclet.Save{BaseVersion: 1, Body: ptr("revised")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	saved := r.list(doclet.Filter{Container: "ENG"}).Pages[0]
	if saved.SavedBy != "bob" || saved.SavedAt.IsZero() {
		t.Fatalf("after bob's save the listing says saved by %q at %v", saved.SavedBy, saved.SavedAt)
	}

	if _, err := r.store.SavePage(t.Context(), author("carla"), created.Page.ID,
		doclet.Save{BaseVersion: 2, Labels: ptr([]string{"runbook"})}); err != nil {
		t.Fatalf("label: %v", err)
	}
	r.drain()
	labelled := r.list(doclet.Filter{Container: "ENG"}).Pages[0]
	if labelled.SavedBy != "bob" || !labelled.SavedAt.Equal(saved.SavedAt) {
		t.Fatalf("a label change moved the last save to %q at %v, want bob's at %v",
			labelled.SavedBy, labelled.SavedAt, saved.SavedAt)
	}
}

// A PAGE'S REVISIONS ARE LISTED NEWEST FIRST, by the page's own version
// number: the detail's history is what a reader scrolls back through, so its
// first entry is the body the page has now and each after it is the one
// before.
//
// Mutation: list the revisions oldest first, and the order comes back
// reversed.
func TestAPagesRevisionsAreListedNewestFirst(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "v1"})
	for version, body := range []string{"v2", "v3"} {
		if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
			doclet.Save{BaseVersion: version + 1, Body: ptr(body)}); err != nil {
			t.Fatalf("save %s: %v", body, err)
		}
	}
	r.drain()
	var got []int
	for _, revision := range r.get(page.Page.ID).History {
		got = append(got, revision.Version)
	}
	if want := []int{3, 2, 1}; !slices.Equal(got, want) {
		t.Errorf("the page's revisions are listed %v, want %v — newest first",
			got, want)
	}
}
