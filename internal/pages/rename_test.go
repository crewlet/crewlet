package pages_test

import (
	"slices"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/seatnames"
	"github.com/crewlet/crewlet/internal/statelog"
)

// renamed is a chart in which each seat answers to one handle and was created
// under another — [pages.Identities] over a rename, with nothing else of a
// chart in it.
type renamed map[string]string // current handle → the handle it was created under

func (r renamed) Identity(handle string) string {
	if origin, ok := r[handle]; ok {
		return origin
	}
	return handle
}

func (r renamed) Current(identity string) string {
	for current, origin := range r {
		if origin == identity {
			return current
		}
	}
	return identity
}

// Pin implements [pages.Identities]: the map IS the chart, so a test that
// renames a seat by writing to it is read by every call after.
func (r renamed) Pin() seatnames.Chart { return r }

// A RENAMED SEAT KEEPS ITS PAGES, ITS WATCHES AND ITS REMARKS.
//
// Every value the knowledge base keeps about a person — a page's author, its
// watchers and mutes, a remark's author, the actor on a change — held the
// handle they answered to at the write. So once `cto` was renamed `chief`, the
// seat could not edit a remark it had written ("only its author may"), its
// unwatch muted `chief` on a page `cto` still watched, a watch it made after
// the rename added it a second time, and the pages it watched were nowhere in
// a listing asked for `chief`. Each is now written and asked for by the seat's
// identity — the handle it was created under — and shown by the handle it
// answers to now.
//
// Mutation: drop the actor's rewrite from any store wrapper and the edit is
// refused or the unwatch mutes the wrong name; drop the filter's from List and
// the listing is empty; drop the answer's from Get and the page is shown as
// `cto`'s.
func TestARenamedSeatKeepsItsPagesWatchesAndRemarks(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	chart := renamed{}
	r.knowing(chart)
	ctx := t.Context()

	// BEFORE: cto writes a page — and so watches it — remarks on it, and is
	// named a watcher of a page jane writes.
	own := r.write(agent("cto"), pages.NewPage{Title: "Runbook", Body: "prose"})
	remark, _, err := r.store.Comment(ctx, agent("cto"), own.Page.ID,
		pages.NewComment{Body: "taking this"})
	if err != nil {
		t.Fatalf("cto's remark: %v", err)
	}
	r.drain()
	janes := r.write(author("jane"), pages.NewPage{
		Title: "Design", Body: "prose", Watchers: []string{"cto"},
	})

	// THE RENAME. Nothing is rewritten: the rows still say `cto`.
	chart["chief"] = "cto"

	watching := r.list(pages.Filter{Watcher: "chief"})
	if got := summaryIDs(watching.Pages); !sameSet(got, []string{own.Page.ID, janes.Page.ID}) {
		t.Fatalf("chief watches %v, want the two pages it watched as cto", got)
	}
	for _, item := range watching.Pages {
		if item.ID == own.Page.ID && item.Author != "chief" {
			t.Errorf("chief's page is listed as %q's", item.Author)
		}
	}

	// ITS OWN REMARK IS ITS OWN, and edited by the name it answers to now.
	if _, _, err := r.store.EditComment(ctx, agent("chief"), own.Page.ID, remark.ID,
		"handing over"); err != nil {
		t.Fatalf("chief may not edit the remark it wrote as cto: %v", err)
	}
	r.drain()
	// A WATCH IT MAKES AGAIN is the watch it already holds.
	if _, err := r.store.SavePage(ctx, agent("chief"), own.Page.ID,
		pages.Save{BaseVersion: own.Page.Version, Watch: ptr(true)}); err != nil {
		t.Fatalf("chief watches its own page again: %v", err)
	}
	// AND AN UNWATCH MUTES THE SEAT, not a name nobody watches under.
	if _, err := r.store.SavePage(ctx, agent("chief"), janes.Page.ID,
		pages.Save{BaseVersion: janes.Page.Version, Watch: ptr(false)}); err != nil {
		t.Fatalf("chief unwatches jane's page: %v", err)
	}
	r.drain()

	detail := r.get(own.Page.ID)
	if detail.Page.Author != "chief" {
		t.Errorf("the page is shown as %q's, want chief", detail.Page.Author)
	}
	if !slices.Equal(detail.Page.Watchers, []string{"chief"}) {
		t.Errorf("the page's watchers are %v, want chief once", detail.Page.Watchers)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].Author != "chief" ||
		detail.Comments[0].Body != "handing over" {
		t.Errorf("the remark is %+v, want chief's, edited", detail.Comments)
	}
	if got := summaryIDs(r.list(pages.Filter{Watcher: "chief"}).Pages); !slices.Equal(got,
		[]string{own.Page.ID}) {
		t.Errorf("after the unwatch chief watches %v, want only its own page", got)
	}
	muted := r.get(janes.Page.ID).Page
	if !slices.Equal(muted.Muted, []string{"chief"}) {
		t.Errorf("jane's page mutes %v, want chief", muted.Muted)
	}

	// THE ROWS HOLD THE IDENTITY, which a reader with no chart shows as it
	// is stored — and which is why nothing already written needs migrating.
	bare, err := pages.NewReader(r.readerOptions)
	if err != nil {
		t.Fatalf("a reader with no chart: %v", err)
	}
	stored, err := bare.Get(ctx, own.Page.ID, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read the rows: %v", err)
	}
	if stored.Page.Author != "cto" || !slices.Equal(stored.Page.Watchers, []string{"cto"}) ||
		stored.Comments[0].Author != "cto" {
		t.Errorf("the rows hold author %q, watchers %v and remark author %q, "+
			"want the identity cto throughout", stored.Page.Author,
			stored.Page.Watchers, stored.Comments[0].Author)
	}
}

// A CALL NAMES EVERY SEAT FROM ONE READING OF THE CHART.
//
// A page's detail names the same seat as its author, among its watchers and
// on every remark, each in its own pass over the answer — and asked afresh per
// name, a rename landing between two passes showed one seat under two names in
// one answer. A call takes one reading, and a write the same.
//
// Mutation: take a reading per helper, or take a fresh one in a wrapper that
// already holds one.
func TestAPageCallNamesEverySeatFromOneReadingOfTheChart(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(agent("cto"), pages.NewPage{Title: "Runbook", Body: "prose"})
	if _, _, err := r.store.Comment(t.Context(), agent("cto"), page.Page.ID,
		pages.NewComment{Body: "taking this"}); err != nil {
		t.Fatalf("remark: %v", err)
	}
	r.drain()

	chart := &renaming{}
	r.knowing(chart)
	detail := r.get(page.Page.ID)
	if got := chart.readings.Load(); got != 1 {
		t.Errorf("one read took %d readings of the chart, want one", got)
	}
	names := map[string]bool{detail.Page.Author: true}
	for _, watcher := range detail.Page.Watchers {
		names[watcher] = true
	}
	for _, comment := range detail.Comments {
		names[comment.Author] = true
	}
	if len(names) != 1 {
		t.Errorf("one answer named one seat %d ways: %v", len(names), names)
	}

	before := chart.readings.Load()
	if _, _, err := r.store.Comment(t.Context(), agent("chief"), page.Page.ID,
		pages.NewComment{Body: "and another", Mentions: []string{"chief"}}); err != nil {
		t.Fatalf("a second remark: %v", err)
	}
	if got := chart.readings.Load() - before; got != 1 {
		t.Errorf("one write took %d readings of the chart, want one", got)
	}
}

// renaming is a chart a rename lands on every time somebody reads it: the
// seat created as `cto` answers to `chief` on the first reading, `boss` on the
// second, and so on — so two readings inside one call name it two ways.
type renaming struct{ readings atomic.Int32 }

func (r *renaming) Pin() seatnames.Chart {
	names := []string{"chief", "boss", "head", "lead"}
	n := int(r.readings.Add(1)) - 1
	return renamed{names[n%len(names)]: "cto"}
}

// A RENAMED LEAD'S OWN NEW PAGE DOES NOT WAKE THEM.
//
// A page created in a team's container nobody else watches reaches that
// team's lead — unless the lead wrote it. The lead map names a seat as the
// chart spells it now and the record names its actor by identity, so compared
// as strings a renamed lead's own page woke them as a stranger's. They are
// compared as seats, through the party registry, which answers every handle a
// seat answers to.
//
// Mutation: compare the lead with the actor by string again.
func TestARenamedLeadIsNotWokenByTheirOwnPage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.knowing(renamed{"chief": "cto"})
	r.write(agent("chief"), pages.NewPage{Container: "ENG", Title: "Nobody watches this"})
	created := r.lastRecord()

	o := &org.Organization{Name: "nimbus", Roles: []*org.Role{
		{Name: "Chief", DeclaredHandle: "chief", OriginHandle: "cto",
			FormerHandles: []string{"cto"}},
		{Name: "Jane", DeclaredHandle: "jane"},
	}}
	o.Normalize()
	reg := notify.NewRegistry(o, nil)

	if routed := r.routeVia(created, pages.Leads{"ENG": "chief"}, reg); len(routed) != 0 {
		t.Errorf("the lead's own page woke %v", recipients(routed))
	}
	// AND ANOTHER SEAT'S PAGE STILL REACHES THEM, as the name they answer to.
	r.write(agent("jane"), pages.NewPage{Container: "ENG", Title: "Jane's"})
	routed := r.routeVia(r.lastRecord(), pages.Leads{"ENG": "chief"}, reg)
	if got := recipients(routed); !slices.Equal(got, []string{"chief"}) {
		t.Errorf("jane's page reached %v, want the lead", got)
	}
}

func summaryIDs(items []pages.Summary) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func recipients(routed []notify.Routed) []string {
	out := make([]string, 0, len(routed))
	for _, one := range routed {
		out = append(out, one.To.Handle)
	}
	return out
}
