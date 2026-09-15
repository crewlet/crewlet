package pages_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PAGE WINDOW IS APPLIED, AND IT IS APPLIED AFTER THE FILTER.
//
// A listing's SQL takes positional placeholders, so the filter's values and
// the window's share one slice and the only thing keeping them apart is the
// order they were appended in. Nothing about a mis-ordered pair is visible
// from the outside: `LIMIT ?` bound to a container name is a page of nothing,
// and `LIKE ?` bound to a limit is a filter that matched nobody — both of
// which read exactly like a company that has written little down. The window
// is therefore appended by [pages.Reader] itself, at the one place that knows
// it goes last, and this is the guard over that.
func TestAPageListingWindowsAfterItFilters(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, title := range []string{"Alpha", "Beta", "Gamma"} {
		r.write(author("jane"), pages.NewPage{Title: title, Body: "prose"})
	}
	// A FOURTH PAGE IN ANOTHER CONTAINER, so "the window works" cannot be
	// satisfied by a listing that ignores the filter entirely.
	r.write(author("jane"), pages.NewPage{
		Container: "PROD", Title: "Alpha", Body: "prod's own",
	})

	first := r.list(pages.Filter{Container: "ENG", Limit: 2})
	if got := titles(first); len(got) != 2 || got[0] != "Alpha" || got[1] != "Beta" {
		t.Fatalf("the first page of two is %v, want Alpha and Beta", got)
	}
	second := r.list(pages.Filter{Container: "ENG", Limit: 2, Offset: 2})
	if got := titles(second); len(got) != 1 || got[0] != "Gamma" {
		t.Fatalf("the second page of two is %v, want Gamma alone — an offset "+
			"that did not reach the query pages the same rows for ever", got)
	}

	// AND A FILTER VALUE BESIDE THE WINDOW. This is the case a mis-ordered
	// argument slice actually breaks: the title predicate and the bound are
	// two placeholders in one statement.
	narrowed := r.list(pages.Filter{Container: "ENG", Title: "Alpha", Limit: 2})
	if got := titles(narrowed); len(got) != 1 || got[0] != "Alpha" {
		t.Fatalf("filtering ENG by title within a window of two gave %v", got)
	}
}

// A PAGE'S CHILDREN COME BACK WITH IT, off the same listing and the same
// snapshot as the page itself — which is the other caller of that query, and
// the one whose window is the engine's own rather than a caller's.
func TestAPageReportsItsChildren(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	parent := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	r.write(author("jane"), pages.NewPage{
		Title: "Rollback", Body: "prose", ParentID: parent.Page.ID,
	})
	// A SIBLING AT THE TOP LEVEL, so a children list that answered with
	// every page in the container would be caught.
	r.write(author("jane"), pages.NewPage{Title: "Oncall", Body: "prose"})

	detail := r.get(parent.Page.ID)
	if len(detail.Children) != 1 || detail.Children[0].Title != "Rollback" {
		t.Fatalf("the page reports children %v, want Rollback alone",
			titles(pages.Listing{Pages: detail.Children}))
	}
}

// list reads a filtered listing at session level — this node's own writes,
// which is what the harness has just applied.
func (r *roundTrip) list(f pages.Filter) pages.Listing {
	r.t.Helper()
	got, err := r.reader.List(r.t.Context(), f, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		r.t.Fatalf("list %+v: %v", f, err)
	}
	return got
}

func titles(l pages.Listing) []string {
	out := make([]string, 0, len(l.Pages))
	for _, p := range l.Pages {
		out = append(out, p.Title)
	}
	return out
}

func (r *roundTrip) containers() []pages.ContainerListing {
	r.t.Helper()
	got, err := r.reader.Containers(r.t.Context(),
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		r.t.Fatalf("Containers: %v", err)
	}
	return got
}

// A CONTAINER SAYS HOW MUCH IS IN IT, which the rail beside it could not.
//
// The count is DERIVED rather than a field on the container document: the
// document is what a writer wrote and this is a fact about other rows, so
// putting it there would have every page create rewrite its container and two
// writers adding a page to one space contend on a counter neither touched.
//
// TRASHED PAGES DO NOT COUNT. A trashed page is invisible to every default
// listing this reader serves, so counting it would put a number on the rail
// the list beside it cannot account for — a reader clicks "3" and finds two.
func TestAContainerSaysHowManyPagesItHolds(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// A CONTAINER IS A DOCUMENT SOMEBODY WROTE, and a page merely NAMES one
	// — so a space with pages and no document is not in this listing at all.
	// Declaring three, one of which nobody writes to, is what makes the zero
	// below a real case rather than an absent key.
	for _, key := range []string{"ENG", "PROD", "EMPTY"} {
		if _, _, err := r.store.EnsureContainer(t.Context(), key, key, ""); err != nil {
			t.Fatalf("EnsureContainer %s: %v", key, err)
		}
	}
	r.drain()
	for _, title := range []string{"Alpha", "Beta", "Gamma"} {
		r.write(author("jane"), pages.NewPage{Title: title, Body: "prose"})
	}
	r.write(author("jane"), pages.NewPage{
		Container: "PROD", Title: "Only one", Body: "prod's own",
	})
	// A FOURTH PAGE IN ENG, TRASHED. Without it "trashed pages do not
	// count" is a claim rather than a case: every count would be identical
	// with or without the predicate.
	binned := r.write(author("jane"), pages.NewPage{Title: "Delta", Body: "prose"})
	if _, err := r.store.Trash(t.Context(), author("jane"), binned.Page.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	r.drain()

	counts := map[string]int{}
	for _, c := range r.containers() {
		counts[c.Key] = c.Pages
	}
	if counts["ENG"] != 3 {
		t.Errorf("ENG holds %d pages, want 3 — the fourth is trashed, and a "+
			"rail that counted it sends a reader to a list of three",
			counts["ENG"])
	}
	if counts["PROD"] != 1 {
		t.Errorf("PROD holds %d pages, want 1", counts["PROD"])
	}

	// AND A CONTAINER NOBODY HAS WRITTEN TO IS ZERO rather than absent,
	// because the count comes from a GROUP BY that has no row for it — the
	// listing has to supply the zero, or an empty space renders blank where
	// every other one renders a number.
	if n, listed := counts["EMPTY"]; !listed || n != 0 {
		t.Errorf("EMPTY is listed=%v with %d pages, want it listed at zero",
			listed, n)
	}
}

func (r *roundTrip) activity(q pages.PageActivityQuery) pages.PageActivity {
	r.t.Helper()
	if q.Freshness.Level == "" {
		q.Freshness = statelog.Freshness{Level: statelog.ReadSession}
	}
	got, err := r.reader.Activity(r.t.Context(), q)
	if err != nil {
		r.t.Fatalf("Activity(%+v): %v", q, err)
	}
	return got
}

// EVERY CHANGE, not only the saves.
//
// `pages_history` has had one row per change since the domain landed — ten
// kinds, who made it, whether it announced anything, and the turn that made it
// — and the schema ships an index literally named "one page's activity". Until
// now nothing read a single row of it: a comment, a rename, a move, a label
// edit and a status change all happened and left no trace any screen could
// show, because the only visible history was the revision list, which is
// SAVES.
func TestEveryChangeToAPageIsReadable(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	if _, _, err := r.store.Comment(t.Context(), author("bob"), page.Page.ID,
		pages.NewComment{Body: "does this still hold?"}); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	r.drain()

	got := r.activity(pages.PageActivityQuery{Page: page.Page.ID})
	kinds := map[pages.ChangeKind]int{}
	for _, change := range got.Changes {
		kinds[change.Kind]++
	}
	if kinds[pages.ChangeCreated] == 0 {
		t.Errorf("the create left no entry: %+v", got.Changes)
	}
	if kinds[pages.ChangeComment] == 0 {
		t.Errorf("the comment left no entry, and a revision list cannot "+
			"show one: %+v", got.Changes)
	}
	// THE TITLE IS RESOLVED, because a feed of uuids is a feed nobody
	// reads — and it is the page's CURRENT one, which is the honest
	// answer for a renamed page's old entries.
	for _, change := range got.Changes {
		if change.Title != "Runbook" {
			t.Errorf("a change names %q, want the page's title", change.Title)
		}
	}
	// NEWEST FIRST, which is what makes a cursor a keyset rather than an
	// offset.
	for i := 1; i < len(got.Changes); i++ {
		if got.Changes[i].LogSeq > got.Changes[i-1].LogSeq {
			t.Fatalf("entry %d sorts above %d: %+v", i, i-1, got.Changes)
		}
	}
}

// ONE PAGE, ONE CONTAINER, OR ONE KIND — the three narrowings, and each is a
// different question a reader actually asks.
func TestThePageFeedNarrowsByPageContainerAndKind(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, _, err := r.store.EnsureContainer(t.Context(), "PROD", "Prod", ""); err != nil {
		t.Fatalf("EnsureContainer: %v", err)
	}
	r.drain()
	eng := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	r.write(author("jane"), pages.NewPage{
		Container: "PROD", Title: "Deploys", Body: "prose",
	})
	if _, _, err := r.store.Comment(t.Context(), author("bob"), eng.Page.ID,
		pages.NewComment{Body: "a remark"}); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	r.drain()

	all := r.activity(pages.PageActivityQuery{})
	if len(all.Changes) < 3 {
		t.Fatalf("the company-wide feed has %d changes, want at least three",
			len(all.Changes))
	}

	onePage := r.activity(pages.PageActivityQuery{Page: eng.Page.ID})
	for _, change := range onePage.Changes {
		if change.PageID != eng.Page.ID {
			t.Errorf("one page's feed carries %s", change.PageID)
		}
	}

	oneContainer := r.activity(pages.PageActivityQuery{Container: "PROD"})
	if len(oneContainer.Changes) == 0 {
		t.Fatal("PROD's feed is empty and a page was created in it")
	}
	for _, change := range oneContainer.Changes {
		if change.Container != "PROD" {
			t.Errorf("PROD's feed carries a change in %q", change.Container)
		}
	}

	comments := r.activity(pages.PageActivityQuery{Kinds: []pages.ChangeKind{pages.ChangeComment}})
	if len(comments.Changes) != 1 {
		t.Fatalf("the comment filter gave %d changes, want the one comment",
			len(comments.Changes))
	}

	// AND AN UNKNOWN KIND IS REFUSED rather than read as a filter that
	// matches nothing, which reads to a person as a quiet company.
	if _, err := r.reader.Activity(t.Context(), pages.PageActivityQuery{
		Kinds:     []pages.ChangeKind{"vandalised"},
		Freshness: statelog.Freshness{Level: statelog.ReadSession},
	}); err == nil {
		t.Error("an unknown change kind was accepted")
	}
}

// A REVISION IS A BODY, and the detail's summaries could only ever say that
// one existed.
//
// The panel that rendered them told the reader why they could not click one:
// "past versions are kept as metadata here; reading one back is a coordination
// read the engine does on demand." There was no such read.
func TestAPastRevisionAnswersItsOwnBody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "the first draft"})
	second := "the second draft"
	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID, pages.Save{
		BaseVersion: page.Page.Version, Body: &second, Message: "rewrote it",
	}); err != nil {
		t.Fatalf("SavePage: %v", err)
	}
	r.drain()

	fresh := statelog.Freshness{Level: statelog.ReadSession}
	// REVISION N IS THE BODY AT VERSION N, including the newest — the
	// other reading collides with itself on the first save.
	first, held, err := r.reader.Revision(t.Context(), page.Page.ID, 1, fresh)
	if err != nil {
		t.Fatalf("Revision 1: %v", err)
	}
	if !held || first.Body != "the first draft" {
		t.Fatalf("version 1 is held=%v body=%q, want the first draft", held, first.Body)
	}
	latest, held, err := r.reader.Revision(t.Context(), page.Page.ID, 2, fresh)
	if err != nil {
		t.Fatalf("Revision 2: %v", err)
	}
	if !held || latest.Body != "the second draft" || latest.Message != "rewrote it" {
		t.Fatalf("version 2 is %+v", latest)
	}

	// A VERSION THIS NODE DOES NOT HOLD IS NOT FOUND, never an error and
	// never an empty body: a page keeps a bounded number of revisions, so
	// an old one is an ordinary absence and a reader has to tell it from a
	// page that never existed.
	if _, held, err := r.reader.Revision(t.Context(), page.Page.ID, 99, fresh); err != nil {
		t.Errorf("an absent version answered an error: %v", err)
	} else if held {
		t.Error("a version nobody saved reports itself held")
	}

	// AND A VERSION THAT IS NOT A VERSION is refused rather than read as
	// the newest: zero is what an unset parameter arrives as.
	if _, _, err := r.reader.Revision(t.Context(), page.Page.ID, 0, fresh); err == nil {
		t.Error("version 0 was accepted")
	}
}

// A CONTAINER THAT ALREADY SAYS WHAT THE CHART SAYS IS NOT WRITTEN AGAIN.
//
// The reconcile runs on every boot and every config apply, so the ordinary
// outcome is no change at all — and a caller that could not tell that from a
// create would log "applied" on every restart for a company nobody had edited,
// while the log itself grew with restarts rather than with edits.
func TestEnsuringAnUnchangedContainerWritesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// EACH WRITE IS APPLIED BEFORE THE NEXT ONE DECIDES: a decide runs
	// against this node's own applied rows, so a second ensure that has not
	// seen the first would be refused as behind rather than answering.
	ensure := func(key, name, purpose string) bool {
		t.Helper()
		_, changed, err := r.store.EnsureContainer(t.Context(), key, name, purpose)
		if err != nil {
			t.Fatalf("EnsureContainer %s: %v", key, err)
		}
		r.drain()
		return changed
	}

	if !ensure("ENG", "Engineering", "Build it") {
		t.Fatal("the first ensure wrote nothing — want a create")
	}
	if ensure("ENG", "Engineering", "Build it") {
		t.Error("the second ensure wrote a record — want a no-op")
	}
	// A REAL EDIT IS STILL A WRITE, which is what makes the no-op above a
	// saving rather than a container that can never be renamed.
	if !ensure("ENG", "Platform", "Build it") {
		t.Error("a rename wrote nothing")
	}
	// AND THE KEY IS CASE-INSENSITIVE, so the lower-case spelling is the
	// same container rather than a second one.
	if ensure("eng", "Platform", "Build it") {
		t.Error("the lower-cased key wrote a record — want the same container")
	}
}
