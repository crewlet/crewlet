package pages_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
)

// swept builds a store, its sweeper and the clock both run on.
func swept(t *testing.T) (*pages.Store, *pages.Sweeper, coord.Documents, *clock) {
	t.Helper()
	s, docs, c := newStore(t)
	return s, pages.NewSweeper(docs, c.now), docs, c
}

// keysOfClass is how many keys of one class the family holds.
func keysOfClass(t *testing.T, docs coord.Documents, class string) int {
	t.Helper()
	records, err := docs.Documents(t.Context(), coord.FamilyPages, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var n int
	for _, rec := range records {
		if got, ok := pages.ClassOf(rec.Key); ok && got == class {
			n++
		}
	}
	return n
}

// A PAGE'S HISTORY IS CAPPED PER PAGE, NEWEST KEPT.
//
// The cap is a storage decision as much as a product one: a page an
// auto-refiner rewrites after every turn would otherwise grow one full body
// copy per turn, for ever. What must not go with the old bodies is the
// current one — so this asserts both halves, and that the page still reads.
func TestAPagesHistoryIsTrimmedNewestFirst(t *testing.T) {
	s, sweeper, docs, _ := swept(t)

	page := write(t, s, agent("eng"), pages.NewPage{Title: "runbook", Body: "v1"})
	// One more than the cap, so exactly one revision must go.
	for i := range pages.RevisionsKept {
		saved, err := s.SavePage(t.Context(), agent("eng"), page.Page.ID, pages.Save{
			BaseVersion: page.Page.Version, Body: ptr(fmt.Sprintf("body %d", i+2)),
		})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		page = saved
	}
	before := keysOfClass(t, docs, pages.ClassRevision)
	if before != pages.RevisionsKept+1 {
		t.Fatalf("%d revisions before the sweep, want %d", before, pages.RevisionsKept+1)
	}

	n, err := sweeper.SweepRevisions(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("trimmed %d revisions, want the one over the cap", n)
	}
	if got := keysOfClass(t, docs, pages.ClassRevision); got != pages.RevisionsKept {
		t.Errorf("%d revisions survive, want the cap exactly", got)
	}

	// THE OLDEST WENT, NOT THE NEWEST. Trimming the wrong end would leave
	// a history whose most recent entry is a year old and whose current
	// body is unreachable.
	if _, err := s.Revision(t.Context(), page.Page.ID, 1); err == nil {
		t.Error("revision 1 survived the trim; the sweep kept the wrong end")
	}
	if _, err := s.Revision(t.Context(), page.Page.ID, pages.RevisionsKept+1); err != nil {
		t.Errorf("the newest revision was trimmed: %v", err)
	}
	if _, _, err := s.Page(t.Context(), page.Page.ID); err != nil {
		t.Errorf("the page itself did not survive its history being trimmed: %v", err)
	}
}

// A PAGE UNDER THE CAP IS NEVER TOUCHED, and the cap is PER PAGE: a hundred
// revisions of the quiet page must not be spent making room for the noisy
// one.
func TestAPageUnderTheCapKeepsEveryRevision(t *testing.T) {
	s, sweeper, docs, _ := swept(t)

	quiet := write(t, s, agent("eng"), pages.NewPage{Title: "quiet", Body: "v1"})
	if _, err := s.SavePage(t.Context(), agent("eng"), quiet.Page.ID, pages.Save{
		BaseVersion: quiet.Page.Version, Body: ptr("v2"),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	noisy := write(t, s, agent("eng"), pages.NewPage{Title: "noisy", Body: "v1"})
	for i := range pages.RevisionsKept + 5 {
		saved, err := s.SavePage(t.Context(), agent("eng"), noisy.Page.ID, pages.Save{
			BaseVersion: noisy.Page.Version, Body: ptr(fmt.Sprintf("body %d", i+2)),
		})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		noisy = saved
	}

	if _, err := sweeper.SweepRevisions(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Two for the quiet page, the cap for the noisy one.
	want := 2 + pages.RevisionsKept
	if got := keysOfClass(t, docs, pages.ClassRevision); got != want {
		t.Errorf("%d revisions survive, want %d — the cap is per page", got, want)
	}
	if _, err := s.Revision(t.Context(), quiet.Page.ID, 1); err != nil {
		t.Errorf("the quiet page lost a revision to the noisy one's trim: %v", err)
	}
}

// AN ORPHANED TITLE CLAIM MAKES A TITLE UNUSABLE, and on this backend a
// title is how a person addresses a page. Left behind by a node that died
// between its two keys, it is the one orphan that costs more than a key.
func TestAnOrphanTitleClaimIsSweptAndTheTitleFreed(t *testing.T) {
	s, sweeper, docs, c := swept(t)

	// A claim with no page behind it: exactly what a crash between
	// claimTitle and the page create leaves.
	claim := pages.TitleClaim{
		V: 1, Container: "ENG", Title: pages.NormalizeTitle("orphaned"),
		PageID: "a-page-that-never-landed", CreatedAt: c.at,
	}
	data, err := pages.EncodeClaim(claim)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := docs.CreateDocument(t.Context(), coord.FamilyPages,
		pages.TitleKey("ENG", "orphaned"), data); err != nil {
		t.Fatalf("plant the claim: %v", err)
	}

	// INSIDE THE GRACE nothing goes: a create in flight and a crashed one
	// look identical, and deleting the first destroys a live write.
	c.at = c.at.Add(pages.ClaimGrace / 2)
	n, err := sweeper.SweepOrphans(t.Context(), c.at)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("swept %d claims inside the grace, want none", n)
	}

	c.at = c.at.Add(pages.ClaimGrace)
	if n, err = sweeper.SweepOrphans(t.Context(), c.at); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d claims past the grace, want the orphan", n)
	}

	// AND THE TITLE IS USABLE AGAIN — which is the point. A sweep that
	// deleted the key and left the name unusable would have done nothing
	// anybody can observe.
	if _, err := s.Create(t.Context(), agent("eng"), pages.NewPage{
		Container: "ENG", Title: "orphaned", Body: "the real page",
	}); err != nil {
		t.Errorf("the title is still held after its orphan claim was swept: %v", err)
	}
}

// A LIVE PAGE'S CLAIM IS NEVER AN ORPHAN, however old. The sweep keys on
// whether the page exists, not on the claim's age — and every page's claim is
// as old as the page.
func TestALivePagesTitleClaimSurvivesForEver(t *testing.T) {
	s, sweeper, docs, c := swept(t)

	write(t, s, agent("eng"), pages.NewPage{Title: "kept", Body: "v1"})
	before := keysOfClass(t, docs, pages.ClassTitle)
	if before != 1 {
		t.Fatalf("%d title claims after one create, want 1", before)
	}

	c.at = c.at.Add(5 * 365 * 24 * time.Hour)
	n, err := sweeper.SweepOrphans(t.Context(), c.at)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("swept %d claims belonging to live pages, want none", n)
	}
	if got := keysOfClass(t, docs, pages.ClassTitle); got != 1 {
		t.Errorf("%d title claims survive, want the live page's", got)
	}
}

// CHANGES AGE OUT; PAGES, CONTAINERS AND COMMENTS DO NOT. A wiki that forgot
// would be answering "what do we already know about this" from a window.
func TestOnlyPageChangesAgeOut(t *testing.T) {
	s, sweeper, docs, c := swept(t)

	page := write(t, s, agent("eng"), pages.NewPage{Title: "kept", Body: "v1"})
	if _, _, err := s.Comment(t.Context(), agent("eng"), page.Page.ID,
		pages.NewComment{Body: "a remark"}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	c.at = c.at.Add(pages.ChangeRetention + 24*time.Hour)
	n, err := sweeper.SweepChanges(t.Context(), c.at.Add(-pages.ChangeRetention))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n == 0 {
		t.Error("swept no changes past a horizon every one of them is past")
	}
	if got := keysOfClass(t, docs, pages.ClassChange); got != 0 {
		t.Errorf("%d changes survive the horizon, want none", got)
	}
	if got := keysOfClass(t, docs, pages.ClassPage); got != 1 {
		t.Errorf("%d pages survive the change sweep, want the one written", got)
	}
	if got := keysOfClass(t, docs, pages.ClassComment); got != 1 {
		t.Errorf("%d comments survive the change sweep, want the one written", got)
	}
	if _, _, err := s.Page(t.Context(), page.Page.ID); err != nil {
		t.Errorf("the page whose whole history was swept is unreadable: %v", err)
	}
}
