package pages_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

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
// TRASHED PAGES DO NOT COUNT: a trashed page is deleted as far as any reader
// is concerned, so it is not one of the pages a container holds.
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
		t.Errorf("ENG holds %d pages, want 3 — the fourth is trashed, which "+
			"is deleted as far as any reader is concerned", counts["ENG"])
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

// AND BY WHO WAS WRITING, which is the fourth narrowing and the one an audit
// is made of.
//
// "Every page change a token or a person made" is not expressible as a set of
// handles: an `operator` change carries the token's own label and an `agent`
// one carries a seat handle, so the two name spaces are disjoint and the set
// of people is the roster, which changes. The tracker's feed carries the same
// filter for the same reason, and one screen reads both.
func TestThePageFeedNarrowsByWhoWasWriting(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	r.write(pages.Actor{Handle: "ops-1", Kind: pages.AuthorOperator},
		pages.NewPage{Title: "Rotation", Body: "prose"})
	r.drain()

	all := r.activity(pages.PageActivityQuery{})
	if len(all.Changes) < 2 {
		t.Fatalf("the company-wide feed has %d changes, want a human's and a "+
			"token's — this case tests nothing without both", len(all.Changes))
	}

	byOperator := r.activity(pages.PageActivityQuery{
		ActorKinds: []pages.AuthorKind{pages.AuthorOperator},
	})
	if len(byOperator.Changes) != 1 {
		t.Fatalf("actor_kinds=operator gave %d changes, want the one a token "+
			"made", len(byOperator.Changes))
	}
	if got := byOperator.Changes[0].ActorKind; got != string(pages.AuthorOperator) {
		t.Errorf("it answered a change written by a %q", got)
	}

	// A SET IS A UNION, and every change here was written by one or the
	// other — so a filter naming both narrows nothing, which is what tells
	// a working filter from one that is ANDing its values together.
	both := r.activity(pages.PageActivityQuery{
		ActorKinds: []pages.AuthorKind{pages.AuthorOperator, pages.AuthorHuman},
	})
	if len(both.Changes) != len(all.Changes) {
		t.Errorf("actor_kinds=operator,human gave %d of %d changes",
			len(both.Changes), len(all.Changes))
	}

	// AND A KIND OFF THE WIRE THAT THIS BUILD HAS NEVER HEARD OF IS
	// REFUSED, for the reason the change kind above is: read as a filter
	// matching nothing it answers empty, and an empty audit reads as a
	// company nobody has touched.
	if _, err := r.reader.Activity(t.Context(), pages.PageActivityQuery{
		ActorKinds: []pages.AuthorKind{"root"},
		Freshness:  statelog.Freshness{Level: statelog.ReadSession},
	}); err == nil {
		t.Error("an unknown author kind was accepted")
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

// A FULL PAGE OF PAGES SAYS IT IS A PAGE.
//
// `Listing.Complete` covers ONE kind of incompleteness — a deferred record's
// scope meeting the read — and nothing covered the other: the listing filled
// its limit and the container holds more. The tool built on this goes out of
// its way to surface Complete, on the reasoning that "a model that reads a
// short list as the whole truth writes the duplicate"; a full page it cannot
// tell is full is that same mistake with nothing to check.
func TestAFullPageOfPagesSaysSo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, title := range []string{"Alpha", "Beta", "Gamma"} {
		r.write(author("jane"), pages.NewPage{Title: title, Body: "prose"})
	}

	full := r.list(pages.Filter{Container: "ENG", Limit: 2})
	if len(full.Pages) != 2 {
		t.Fatalf("the page holds %d, want the limit of 2", len(full.Pages))
	}
	if !full.Truncated {
		t.Error("a full page does not say so, so three pages and two read alike")
	}

	// THE LAST PAGE CLAIMS NOTHING. The probe row is evidence, so a page
	// that exactly empties the container must not report more behind it.
	last := r.list(pages.Filter{Container: "ENG", Limit: 2, Offset: 2})
	if len(last.Pages) != 1 || last.Truncated {
		t.Errorf("the final page holds %d rows and reports truncated=%v",
			len(last.Pages), last.Truncated)
	}
	// AND A LIMIT THAT EXACTLY FITS is not a cut either, which is the
	// off-by-one that would make every complete listing claim more.
	if exact := r.list(pages.Filter{Container: "ENG", Limit: 3}); exact.Truncated ||
		len(exact.Pages) != 3 {
		t.Errorf("a limit equal to the container reports %d rows, truncated=%v",
			len(exact.Pages), exact.Truncated)
	}
}

// A PAGE WITH MORE CHILDREN THAN THE READ CARRIES SAYS SO, AND SAYS WHERE THE
// REST START.
//
// A detail read has NO paging parameter, so the flag and the cursor are the
// whole of what a caller gets — without them the child past the engine's own
// window was unreachable through the read that claims to answer a page in
// full, and invisible to whoever asked.
//
// ONLY PUBLISHED CHILDREN, because the read is served to seats: a draft and a
// trashed child that sort FIRST would otherwise take two of the window's
// places, and put in front of an agent a page nobody considers current.
func TestAPageWithMoreChildrenThanItCarriesSaysSo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	parent := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	r.write(author("jane"), pages.NewPage{
		Title: "A draft", Body: "prose", ParentID: parent.Page.ID,
		Status: pages.StatusDraft,
	})
	binned := r.write(author("jane"), pages.NewPage{
		Title: "A trashed page", Body: "prose", ParentID: parent.Page.ID,
	})
	if _, err := r.store.Trash(t.Context(), author("jane"), binned.Page.ID); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	r.drain()
	for i := range pages.DefaultLimit + 1 {
		r.write(author("jane"), pages.NewPage{
			Title: fmt.Sprintf("Step %03d", i), Body: "prose",
			ParentID: parent.Page.ID,
		})
	}

	detail := r.get(parent.Page.ID)
	if len(detail.Children) != pages.DefaultLimit {
		t.Fatalf("the read carries %d children, want the window of %d",
			len(detail.Children), pages.DefaultLimit)
	}
	for _, child := range detail.Children {
		if child.Status != pages.StatusPublished {
			t.Errorf("the page's children include %q, which is %s", child.Title,
				child.Status)
		}
	}
	if !detail.ChildrenTruncated || detail.ChildrenCursor == "" {
		t.Fatalf("the page has more children than it carries and says "+
			"truncated=%v with cursor %q", detail.ChildrenTruncated,
			detail.ChildrenCursor)
	}

	// THE CURSOR RESUMES EXACTLY THERE: the listing it names is the same
	// filter and the same order, so the rest is the one child left.
	rest := r.list(pages.Filter{
		ParentID: parent.Page.ID, Status: []pages.Status{pages.StatusPublished},
		After: detail.ChildrenCursor,
	})
	want := fmt.Sprintf("Step %03d", pages.DefaultLimit)
	if got := titles(rest); len(got) != 1 || got[0] != want || rest.Truncated {
		t.Errorf("the children after the detail's cursor are %v (truncated=%v), "+
			"want %s alone", got, rest.Truncated, want)
	}

	// AND A PAGE INSIDE THE WINDOW CLAIMS NOTHING.
	few := r.write(author("jane"), pages.NewPage{Title: "Small", Body: "prose"})
	r.write(author("jane"), pages.NewPage{
		Title: "One", Body: "prose", ParentID: few.Page.ID,
	})
	if got := r.get(few.Page.ID); got.ChildrenTruncated || got.ChildrenCursor != "" {
		t.Error("a page with one child reports its children cut")
	}
}

// A CHANGE'S EXCERPT IS MARKED, FITS ITS CAP, AND THE WHOLE TEXT IS WHERE THE
// CAP SAYS IT IS.
//
// The excerpt is a card's text and a woken seat's notification body, and the
// field is documented as at most [pages.MaxExcerpt] bytes. A cut that left the
// marker outside that budget broke the field's own contract; a cut with no
// marker read as a comment that ENDED at the cap; and a cut through a
// two-byte character put a replacement character in front of every reader.
// Two-byte runes throughout, so the cap always falls inside one.
func TestAChangesExcerptIsMarkedInsideItsCapAndTheWholeIsReachable(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	opening := strings.Repeat("é", pages.MaxExcerpt)
	page := r.write(author("jane"), pages.NewPage{
		Title: "Runbook", Body: opening + "\n\nthe rest of it",
	})
	remark := strings.Repeat("ü", pages.MaxExcerpt)
	if _, _, err := r.store.Comment(t.Context(), author("bob"), page.Page.ID,
		pages.NewComment{Body: remark}); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	r.drain()

	cut := map[pages.ChangeKind]string{}
	for _, change := range r.activity(pages.PageActivityQuery{Page: page.Page.ID}).Changes {
		cut[change.Kind] = change.Excerpt
	}
	for kind, whole := range map[pages.ChangeKind]string{
		pages.ChangeCreated: opening, pages.ChangeComment: remark,
	} {
		got, ok := cut[kind]
		switch {
		case !ok:
			t.Fatalf("the %s change left no entry", kind)
		case len(got) > pages.MaxExcerpt:
			t.Errorf("the %s excerpt is %d bytes, past its %d-byte cap",
				kind, len(got), pages.MaxExcerpt)
		case !strings.HasSuffix(got, "…"):
			t.Errorf("the %s excerpt was cut and not marked: it reads as "+
				"text that ended there", kind)
		case !utf8.ValidString(got):
			t.Errorf("the %s excerpt was cut through a character", kind)
		case !strings.HasPrefix(whole, strings.TrimSuffix(got, "…")):
			t.Errorf("the %s excerpt is not the opening of what was written", kind)
		}
	}

	// THE WHOLE TEXT IS WHERE MaxExcerpt SAYS: a create's first line in the
	// body of version 1, a comment on its own row.
	first, held, err := r.reader.Revision(t.Context(), page.Page.ID, 1,
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil || !held {
		t.Fatalf("Revision 1: held=%v err=%v", held, err)
	}
	if !strings.HasPrefix(first.Body, opening) {
		t.Error("version 1 does not hold the first line the create's card cut")
	}
	comments := r.get(page.Page.ID).Comments
	if len(comments) != 1 || comments[0].Body != remark {
		t.Error("the comment's row does not hold the text its card cut")
	}
}

// A TEXT THAT FITS IS NOT MARKED. The marker says something was cut, so a
// comment of exactly the cap carrying one would claim a cut that never
// happened.
func TestAnExcerptThatFitsIsNotMarked(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	remark := strings.Repeat("x", pages.MaxExcerpt)
	if _, _, err := r.store.Comment(t.Context(), author("bob"), page.Page.ID,
		pages.NewComment{Body: remark}); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	r.drain()
	for _, change := range r.activity(pages.PageActivityQuery{
		Page: page.Page.ID, Kinds: []pages.ChangeKind{pages.ChangeComment},
	}).Changes {
		if change.Excerpt != remark {
			t.Errorf("a comment of exactly the cap came back as %d bytes "+
				"ending %q", len(change.Excerpt),
				change.Excerpt[max(0, len(change.Excerpt)-8):])
		}
	}
}

// ONLY A BODY'S FIRST LINE AND A COMMENT CAN REACH THE CUT.
//
// [pages.MaxExcerpt] says where the whole text of a cut excerpt is, and it
// names two places because only two kinds of text can be longer than the cap:
// a save's message and a title are the other excerpts a change carries, and
// they are refused at write above their own caps. Raise either cap past
// MaxExcerpt and a message or a title becomes a third thing that is cut, with
// no statement of where the rest of it is.
func TestOnlyABodyAndACommentCanReachTheExcerptCut(t *testing.T) {
	t.Parallel()
	for name, limit := range map[string]int{
		"MaxMessage": pages.MaxMessage, "MaxTitle": pages.MaxTitle,
	} {
		if limit > pages.MaxExcerpt {
			t.Errorf("%s is %d and MaxExcerpt %d: an excerpt of one would be "+
				"cut, and MaxExcerpt does not say where the rest is",
				name, limit, pages.MaxExcerpt)
		}
	}
}

// A LISTING'S LARGEST PAGE FITS THE ONE STATEMENT ITS LABELS ARE READ IN.
//
// The labels of every page on a listing are read with ONE `IN` list of their
// ids, so [pages.MaxLimit] is also a bound parameter count. internal/store
// falls back to 999 parameters when its probe of the engine cannot tell, and a
// full page past that is a refused statement at exactly the moment somebody
// asked for everything.
func TestTheLargestListingFitsTheStatementItsLabelsAreReadIn(t *testing.T) {
	t.Parallel()
	const conservativeParameters = 999 // internal/store's fallback
	if pages.MaxLimit > conservativeParameters {
		t.Errorf("MaxLimit is %d and a statement may carry %d parameters: a "+
			"full page's label read would be refused", pages.MaxLimit,
			conservativeParameters)
	}
}

// A PARENT CHAIN IS READ WHOLE, HOWEVER DEEP IT IS.
//
// Nothing at write bounds a tree's depth, so a cap on the walk would drop the
// outermost pages of a deep chain — the ones a breadcrumb exists to show —
// with nothing on the answer saying so. Eighteen levels, so the chain above
// the deepest page is past the sixteen the walk was once capped at.
func TestAParentChainIsReadWholeHoweverDeepItIs(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const depth = 18
	var chain []string
	parent := ""
	for i := range depth {
		page := r.write(author("jane"), pages.NewPage{
			Title: fmt.Sprintf("Level %02d", i), Body: "prose", ParentID: parent,
		})
		parent = page.Page.ID
		chain = append(chain, parent)
	}

	got := r.get(parent).Ancestors
	if len(got) != depth-1 {
		t.Fatalf("the deepest page reports %d ancestors, want all %d above it",
			len(got), depth-1)
	}
	for i, ancestor := range got {
		if ancestor.ID != chain[i] {
			t.Fatalf("ancestor %d is %s, want %s: the chain is outermost "+
				"first", i, ancestor.ID, chain[i])
		}
	}
}
