package engine_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RUNNING KNOWLEDGE BASE KNOWS A PERSON BY THEIR SEAT'S IDENTITY, on both
// sides, through a real rename.
//
// The seam is an option, and a nil one is a legitimate build — a knowledge base
// with no chart records and reads every person as given — so dropping it from
// the engine's wiring would compile, pass every case in internal/pages and put
// the rename bug back on every node: a seat renamed through the org chart would
// be shown as the handle it was created under, a page it wrote after the rename
// would be stored under an address no read asks for, and a listing of the
// pages it watches would come back short.
//
// Mutation: drop `Identities:` from the engine's pages.Options (the page chief
// writes is not listed), or from its pages.ReaderOptions (the page is shown as
// cto's and neither is listed).
func TestTheRunningKnowledgeBaseKnowsPeopleByTheirSeatsIdentity(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	kb, reader, charts := e.PagesStore(), e.Pages(), e.ChartWriter()
	if kb == nil || reader == nil || charts == nil {
		t.Fatal("the default company runs no native knowledge base or chart")
	}
	ctx := t.Context()
	before, err := kb.Create(ctx, pages.Actor{Handle: "cto", Kind: pages.AuthorAgent,
		TurnID: "turn-cto"}, pages.NewPage{Container: "ENG", Title: "Runbook", Body: "prose"})
	if err != nil {
		t.Fatalf("cto writes a page: %v", err)
	}
	if _, err := charts.WriteBatch(ctx, "test:rename-cto", chart.Batch{
		Operations: []chart.Operation{{Kind: chart.OpRename,
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "cto"}, To: "chief"}},
	}); err != nil {
		t.Fatalf("rename cto: %v", err)
	}
	// THE RENAME REACHES THE PUBLISHED COMPANY ON ITS OWN SCHEDULE.
	if !holdsWithin(func() bool {
		company := e.Company()
		return company != nil && company.Org != nil && company.Org.Role("chief") != nil
	}) {
		t.Fatal("the rename never reached the running company")
	}
	after, err := kb.Create(ctx, pages.Actor{Handle: "chief", Kind: pages.AuthorAgent,
		TurnID: "turn-chief"}, pages.NewPage{Container: "ENG", Title: "Design", Body: "prose"})
	if err != nil {
		t.Fatalf("chief writes a page: %v", err)
	}

	stale := statelog.Freshness{Level: statelog.ReadStale}
	want := []string{before.Page.ID, after.Page.ID}
	slices.Sort(want)
	var (
		detail          pages.Detail
		listing         pages.Listing
		listed          []string
		getErr, listErr error
	)
	if !holdsWithin(func() bool {
		detail, getErr = reader.Get(ctx, before.Page.ID, stale)
		listing, listErr = reader.List(ctx, pages.Filter{Watcher: "chief"}, stale)
		listed = listed[:0]
		for _, item := range listing.Pages {
			listed = append(listed, item.ID)
		}
		slices.Sort(listed)
		return getErr == nil && listErr == nil && detail.Page.Author == "chief" &&
			slices.Equal(detail.Page.Watchers, []string{"chief"}) &&
			slices.Equal(listed, want)
	}) {
		t.Fatalf("after the rename cto's page reads author %q and watchers %v "+
			"(%v), and chief is listed as watching %v (%v) — want the page "+
			"shown as chief's, and both it and the page chief wrote after the "+
			"rename, %v, listed", detail.Page.Author, detail.Page.Watchers, getErr,
			listed, listErr, want)
	}
	for _, item := range listing.Pages {
		if item.Author != "chief" {
			t.Errorf("%s is listed as %q's, want chief", item.Title, item.Author)
		}
	}
}

// holdsWithin asks until the condition holds, and reports whether it did
// within twenty seconds — the caller says what it was waiting for, with what
// it last saw.
func holdsWithin(done func() bool) bool {
	deadline := time.Now().Add(20 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}
