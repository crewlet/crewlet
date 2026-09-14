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
