package pages_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/store"
)

// skillWhen reports a body as a skill when it contains a marker, so the
// derived flag can be exercised without the real parser.
type skillWhen string

func (s skillWhen) IsSkill(body string) bool { return strings.Contains(body, string(s)) }

func projected(t *testing.T) (*pages.Store, *store.DB, *projection.Projector) {
	t.Helper()
	docs := memory.NewFleet()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "p.db"), store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s, err := pages.NewStore(pages.Options{Documents: docs})
	if err != nil {
		t.Fatal(err)
	}
	p, err := projection.New(projection.Options{
		Documents: docs, DB: db, Applier: pages.NewApplier(skillWhen("<!--skill-->")),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the projector did not stop")
		}
	})
	settle(t, p.Hydrated, "the projector never hydrated")
	return s, db, p
}

func settle(t *testing.T, want func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(why)
}

func rowCount(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.SQL().QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func reader(t *testing.T, db *store.DB) *pages.Reader {
	t.Helper()
	r, err := pages.NewReader(pages.ReaderOptions{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A PAGE REACHES EVERY NODE'S OWN TABLES, which is the whole reason the
// projection exists.
func TestAWrittenPageBecomesAQueryableRow(t *testing.T) {
	t.Parallel()
	s, db, _ := projected(t)
	got := write(t, s, author("jane"), pages.NewPage{
		Title: "Deploy Runbook", Body: "how we ship", Labels: []string{"ops"},
	})
	settle(t, func() bool { return rowCount(t, db, `SELECT COUNT(*) FROM pages`) == 1 },
		"the page never projected")

	detail, err := reader(t, db).Get(t.Context(), got.Page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Page.Title != "Deploy Runbook" || detail.Page.Body != "how we ship" {
		t.Errorf("page = %+v", detail.Page)
	}
	if detail.Revision != got.Revision {
		t.Errorf("revision = %d, the write reported %d", detail.Revision, got.Revision)
	}
	if len(detail.History) != 1 || detail.History[0].Version != 1 {
		t.Errorf("history = %+v, want the first revision", detail.History)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM page_labels`); n != 1 {
		t.Errorf("labels projected = %d", n)
	}

	// AND BY "CONTAINER/Title", which is how a person and a model name a
	// page — the title is its address.
	byTitle, err := reader(t, db).Get(t.Context(), "ENG/deploy runbook")
	if err != nil {
		t.Fatalf("Get by title: %v", err)
	}
	if byTitle.Page.ID != got.Page.ID {
		t.Errorf("the title lookup found %q", byTitle.Page.ID)
	}
}

// THE SKILL AND ONBOARDING FLAGS ARE DERIVED AT APPLY TIME, so a parser fix
// or a renamed convention reaches every existing page on the next rebuild
// rather than only the ones edited since. A page written by a newer node must
// not carry a claim an older node's parser disagrees with.
func TestTheDerivedFlagsAreRecomputedOnEveryApply(t *testing.T) {
	t.Parallel()
	s, db, _ := projected(t)
	ordinary := write(t, s, author("jane"), pages.NewPage{Title: "Notes", Body: "prose"})
	skill := write(t, s, author("jane"), pages.NewPage{
		Title: "Using the tracker", Body: "<!--skill--> how to use it",
	})
	onboarding := write(t, s, author("jane"), pages.NewPage{
		Title: pages.OnboardingTitle, Body: "start here",
	})
	settle(t, func() bool { return rowCount(t, db, `SELECT COUNT(*) FROM pages`) == 3 },
		"the pages never projected")

	for _, tc := range []struct {
		id                string
		skill, onboarding int
	}{
		{ordinary.Page.ID, 0, 0},
		{skill.Page.ID, 1, 0},
		{onboarding.Page.ID, 0, 1},
	} {
		got := rowCount(t, db,
			`SELECT skill FROM pages WHERE id = ?`, tc.id)
		if got != tc.skill {
			t.Errorf("%s skill = %d, want %d", tc.id, got, tc.skill)
		}
		got = rowCount(t, db, `SELECT onboarding FROM pages WHERE id = ?`, tc.id)
		if got != tc.onboarding {
			t.Errorf("%s onboarding = %d, want %d", tc.id, got, tc.onboarding)
		}
	}

	// AN EDIT THAT REMOVES THE MARKER CLEARS THE FLAG, which is what makes
	// the derivation meaningful rather than a one-time stamp.
	if _, err := s.SavePage(t.Context(), author("jane"), skill.Page.ID, pages.Save{
		BaseVersion: 1, Body: ptr("just prose now"),
	}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		return rowCount(t, db, `SELECT skill FROM pages WHERE id = ?`, skill.Page.ID) == 0
	}, "the skill flag survived an edit that removed the marker")
}

// A REVISION'S BODY STAYS IN THE BUCKET. A 512 KiB body times a hundred
// revisions times every page would be a local copy an order of magnitude
// larger than the record it copies, on every node.
func TestTheProjectionKeepsRevisionMetadataOnly(t *testing.T) {
	t.Parallel()
	s, db, _ := projected(t)
	got := write(t, s, author("jane"), pages.NewPage{Body: "the original"})
	if _, err := s.SavePage(t.Context(), author("eng"), got.Page.ID, pages.Save{
		BaseVersion: 1, Body: ptr("the rewrite"), Message: "tightened it",
	}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM page_revisions`) == 2
	}, "the revisions never projected")

	var columns int
	rows, err := db.SQL().QueryContext(t.Context(), `SELECT * FROM page_revisions LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := rows.Columns()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	columns = len(names)
	for _, name := range names {
		if name == "body" {
			t.Errorf("page_revisions carries a body column: %v", names)
		}
	}
	if columns == 0 {
		t.Error("page_revisions has no columns")
	}

	// The BODY is still readable, from coordination, on demand.
	rev, err := s.Revision(t.Context(), got.Page.ID, 1)
	if err != nil || rev.Body != "the original" {
		t.Errorf("revision 1 = %+v, %v", rev, err)
	}
}

// A BOOT RECONCILE ENUMERATES KEYS IN MAP ORDER, so a revision or a comment
// can be reached before its page. Skipping it there is permanent — the key
// set records the child as applied and nothing re-fetches it.
func TestABootReconcileNeverDropsAPagesHistory(t *testing.T) {
	t.Parallel()
	docs := memory.NewFleet()
	s, err := pages.NewStore(pages.Options{Documents: docs})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Create(t.Context(), author("jane"), pages.NewPage{
		Container: "ENG", Title: "Much edited", Body: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	const saves = 15
	for i := range saves {
		if _, err := s.SavePage(t.Context(), author("jane"), got.Page.ID, pages.Save{
			BaseVersion: i + 1, Body: ptr("v" + itoa(i+2)),
		}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		if _, _, err := s.Comment(t.Context(), author("jane"), got.Page.ID,
			pages.NewComment{Body: "remark " + itoa(i)}); err != nil {
			t.Fatalf("comment %d: %v", i, err)
		}
	}

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "fresh.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := projection.New(projection.Options{
		Documents: docs, DB: db, Applier: pages.NewApplier(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	settle(t, p.Hydrated, "the fresh node never hydrated")

	if n := rowCount(t, db, `SELECT COUNT(*) FROM page_revisions`); n != saves+1 {
		t.Errorf("the reconcile projected %d of %d revisions", n, saves+1)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM page_comments`); n != saves {
		t.Errorf("the reconcile projected %d of %d comments", n, saves)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

// THE SEARCH ANSWERS THROUGH THE KNOWLEDGE SEAM, which is what makes the
// native backend a drop-in for Confluence: the turn-start prefetch and the
// search_knowledge builtin both read through it and neither knows which
// answered.
func TestTheNativeSearcherFulfilsTheKnowledgeSeam(t *testing.T) {
	t.Parallel()
	s, db, _ := projected(t)
	write(t, s, author("jane"), pages.NewPage{
		Title: "Rollback", Body: "to roll back a deploy, run the rollback command",
	})
	write(t, s, author("jane"), pages.NewPage{
		Container: "TS", Title: "Using the tracker",
		Body: "how to use the rollback tool, step by step",
	})
	settle(t, func() bool { return rowCount(t, db, `SELECT COUNT(*) FROM pages`) == 2 },
		"the pages never projected")

	indexer := projection.NewIndexer(db)
	searcher := pages.NewSearcher(pages.SearcherOptions{
		Index: indexer, SkillsContainer: func() string { return "TS" },
	})

	// BUILDING AND EMPTY ARE DIFFERENT ANSWERS. A seat on a freshly joined
	// node must not be told the company has written nothing down for the
	// whole first index build.
	if !searcher.Building(t.Context()) {
		t.Error("a projection with an un-built index does not report as building")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go indexer.Run(ctx)
	settle(t, func() bool { return !searcher.Building(t.Context()) },
		"the index never caught up")

	if !searcher.CanSearch(nil, nil) {
		t.Error("a seat with no credential cannot search natively — but there " +
			"is no second account here, so every seat can read every page")
	}
	hits := searcher.Search(t.Context(), knowledge.Query{Text: "rollback"})
	if len(hits) != 1 {
		t.Fatalf("search returned %d hits, want only the knowledge page: %+v", len(hits), hits)
	}
	if hits[0].Title != "Rollback" {
		t.Errorf("hit = %+v", hits[0])
	}
	// THE SKILLS CONTAINER IS EXCLUDED: those pages are machinery, and a
	// seat told to read one would follow an instruction written for a
	// different phase of a different turn.
	for _, hit := range hits {
		if hit.Container == "TS" {
			t.Errorf("a tool-skill page was returned as knowledge: %+v", hit)
		}
	}

	// BEST EFFORT: a query with nothing searchable answers empty rather
	// than failing a turn.
	if got := searcher.Search(t.Context(), knowledge.Query{Text: "   "}); got != nil {
		t.Errorf("an empty query returned %+v", got)
	}
}

// A CYCLE IN THE PARENT CHAIN TRUNCATES THE BREADCRUMB rather than hanging
// the read that found it.
func TestAParentCycleDoesNotHangARead(t *testing.T) {
	t.Parallel()
	s, db, _ := projected(t)
	a := write(t, s, author("jane"), pages.NewPage{Title: "A"})
	b := write(t, s, author("jane"), pages.NewPage{Title: "B", ParentID: a.Page.ID})
	if _, err := s.SavePage(t.Context(), author("jane"), a.Page.ID, pages.Save{
		BaseVersion: 1, ParentID: ptr(b.Page.ID),
	}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM pages WHERE parent_id <> ''`) == 2
	}, "the cycle never projected")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := reader(t, db).Get(t.Context(), b.Page.ID); err != nil {
			t.Errorf("Get: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reading a page in a parent cycle hung")
	}
}
