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
		if _, err := r.store.EnsureContainer(t.Context(), key, key, ""); err != nil {
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
