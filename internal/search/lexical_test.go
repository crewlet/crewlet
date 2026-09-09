package search_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// page writes an applied page row, which is the indexer's input.
//
// THE REPLICATED ESTATE, because that is where a page lives now: the index is
// this node's own and the sources are the fleet's, and the whole reason the
// indexer walks rather than joins is that no read crosses between them.
func page(t *testing.T, db *store.DB, id, container, title, body string, version int) {
	t.Helper()
	_, err := db.Replicated().SQL().ExecContext(t.Context(), `
		INSERT INTO pages_heads (id, container, parent_id, title, title_norm, body,
		                         status, author, edit_version, created_at,
		                         updated_at, version, scoped_through, document)
		VALUES (?, ?, '', ?, lower(?), ?, 'published', '', 1, 0, 0, ?, 0, '{}')
		ON CONFLICT (id) DO UPDATE SET
			title = excluded.title, title_norm = excluded.title_norm,
			body = excluded.body, version = excluded.version`,
		id, container, title, title, body, version)
	if err != nil {
		t.Fatalf("insert page %s: %v", id, err)
	}
}

// indexAll drives the indexer to a fixed point.
//
// UNTIL IT FINDS NOTHING TWICE, not until [search.Indexer.Ready]. Ready is the
// first-build gate — it counts pages the index has no row for at all — and is
// deliberately blind to a row that is merely stale, so waiting on it would
// return the instant an edit's page was represented by its PREVIOUS text.
//
// Twice, because the reconciliation walk wraps: one empty sweep can be the end
// of a pass rather than the end of the work.
func indexAll(t *testing.T, x *search.Indexer) {
	t.Helper()
	quiet := 0
	for range 100 {
		worked, err := x.Sweep(t.Context())
		if err != nil {
			t.Fatalf("index sweep: %v", err)
		}
		if worked {
			quiet = 0
			continue
		}
		if quiet++; quiet == 2 {
			return
		}
	}
	t.Fatal("the indexer never settled")
}

func titles(hits []search.SearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Title
	}
	return out
}

// THE RANKING PROPERTY A PERSON NOTICES: the short page that is ABOUT the
// subject comes above the long runbook that mentions it in passing. Without
// length normalisation the runbook wins every broad query, which is the
// failure that makes a knowledge search useless rather than merely imperfect.
func TestTheShortAnswerBeatsTheLongRunbook(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	page(t, db, "p.short", "ENG", "Rollback",
		"To roll back a deploy, run the rollback command against the release.", 1)
	page(t, db, "p.long", "ENG", "Platform Runbook",
		strings.Repeat("The platform has many procedures for many situations. ", 200)+
			"Rollback is mentioned once here.", 1)
	indexAll(t, x)

	hits, err := x.Search(t.Context(), search.SearchQuery{Text: "rollback"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %v, want both pages", titles(hits))
	}
	if hits[0].Title != "Rollback" {
		t.Errorf("ranked %v: the long runbook outranked the page about the subject",
			titles(hits))
	}
	if !strings.Contains(strings.ToLower(hits[0].Snippet), "rollback") {
		t.Errorf("the snippet does not show why it matched: %q", hits[0].Snippet)
	}
}

// A TITLE MATCH IS WORTH MORE THAN A BODY MENTION, which is how a person
// searching for a page by its name finds it rather than finding every page
// that happens to reference it.
func TestATitleMatchOutranksABodyMention(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	page(t, db, "p.named", "ENG", "Incident Response",
		"This describes what the team does when something breaks.", 1)
	page(t, db, "p.mentions", "ENG", "Weekly Notes",
		"We talked about incident response and then about incident response again.", 1)
	indexAll(t, x)

	hits, err := x.Search(t.Context(), search.SearchQuery{Text: "incident response"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Title != "Incident Response" {
		t.Errorf("ranked %v, want the page named for the query first", titles(hits))
	}
}

// AN EDIT THAT REMOVES A WORD REMOVES ITS TERM. A merge instead of a replace
// would leave the document matching a word it no longer contains, which reads
// to a person as the search inventing a result.
func TestAnEditRemovesTheTermsItRemoved(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	page(t, db, "p.edited", "ENG", "Deploy Notes", "we use kubernetes for this", 1)
	indexAll(t, x)
	if hits, _ := x.Search(t.Context(), search.SearchQuery{Text: "kubernetes"}); len(hits) != 1 {
		t.Fatalf("the term never indexed: %v", hits)
	}

	page(t, db, "p.edited", "ENG", "Deploy Notes", "we use nomad for this", 2)
	indexAll(t, x)
	if hits, _ := x.Search(t.Context(), search.SearchQuery{Text: "kubernetes"}); len(hits) != 0 {
		t.Errorf("the removed word still matches: %v", titles(hits))
	}
	if hits, _ := x.Search(t.Context(), search.SearchQuery{Text: "nomad"}); len(hits) != 1 {
		t.Errorf("the new word does not match: %v", titles(hits))
	}
}

// A DRAFT AND A TRASHED PAGE ARE NOT SEARCHABLE. A draft is somebody's
// unfinished thought and a trashed page is deleted as far as any reader is
// concerned; surfacing either puts content in front of an agent that no
// person considers current.
func TestOnlyPublishedPagesAreIndexed(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	page(t, db, "p.live", "ENG", "Live", "the migration plan is here", 1)
	for id, status := range map[string]string{"p.draft": "draft", "p.gone": "trashed"} {
		if _, err := db.Replicated().SQL().ExecContext(t.Context(), `
			INSERT INTO pages_heads (id, container, parent_id, title, title_norm,
			                         body, status, author, edit_version,
			                         created_at, updated_at, version,
			                         scoped_through, document)
			VALUES (?, 'ENG', '', 'Hidden', 'hidden',
			        'the migration plan is here', ?, '', 1, 0, 0, 1, 0, '{}')`,
			id, status); err != nil {
			t.Fatal(err)
		}
	}
	indexAll(t, x)

	hits, err := x.Search(t.Context(), search.SearchQuery{Text: "migration plan"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "p.live" {
		t.Errorf("search returned %v, want only the published page", titles(hits))
	}

	// AND UNPUBLISHING REMOVES IT. A page a lead moved to draft must stop
	// being findable, or the retraction did nothing.
	if _, err := db.Replicated().SQL().ExecContext(t.Context(),
		`UPDATE pages_heads SET status = 'draft' WHERE id = 'p.live'`); err != nil {
		t.Fatal(err)
	}
	// THROUGH THE SWEEP, not by calling Remove directly: what has to hold
	// is that the indexer NOTICES an unpublished page on its own, and a
	// test that removed the row itself would pass with no orphan pass at
	// all.
	indexAll(t, x)
	if hits, _ := x.Search(t.Context(), search.SearchQuery{Text: "migration plan"}); len(hits) != 0 {
		t.Errorf("an unpublished page is still findable: %v", titles(hits))
	}
}

// "NOT INDEXED YET" AND "NOTHING MATCHED" ARE DIFFERENT ANSWERS. A seat on a
// freshly joined node would otherwise be told the company has written nothing
// down for the whole first index build.
func TestReadyDistinguishesABuildingIndexFromAnEmptyCompany(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	// An empty company is READY: there is nothing to index, so a search
	// answering empty is the truth.
	ready, err := x.Ready(t.Context())
	if err != nil || !ready {
		t.Fatalf("an empty company reported ready=%v err=%v", ready, err)
	}

	page(t, db, "p.new", "ENG", "New", "something to index", 1)
	if ready, _ := x.Ready(t.Context()); ready {
		t.Error("a page waiting to be indexed reported ready")
	}
	if n, _ := x.Pending(t.Context()); n != 1 {
		t.Errorf("pending = %d, want 1", n)
	}
	indexAll(t, x)
	if ready, _ := x.Ready(t.Context()); !ready {
		t.Error("the index never reported ready")
	}
}

// THE INDEXER RUNS ITSELF, and reaching ready through Run is the path a node
// actually takes.
func TestTheIndexerCatchesUpOnItsOwn(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)
	for i := range 45 { // more than one batch
		page(t, db, fmt.Sprintf("p.%d", i), "ENG", fmt.Sprintf("Page %d", i),
			"shared vocabulary across the whole company", i+1)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go x.Run(ctx)

	waitFor(t, func() bool { ready, _ := x.Ready(t.Context()); return ready },
		"the indexer never caught up on its own")
	hits, err := x.Search(t.Context(), search.SearchQuery{Text: "vocabulary", Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 5 {
		t.Errorf("hits = %d, want the limit", len(hits))
	}
}

// THE SCOPE FILTER NARROWS RESULTS, NEVER THE WEIGHTS. A term's rarity is a
// fact about the whole corpus; counting it per scope would make the same word
// rare in a small container and common in a large one, so a hit's rank would
// depend on which container it was in rather than on how well it matched.
func TestAScopeNarrowsResultsWithoutChangingTheRanking(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	page(t, db, "p.eng", "ENG", "Deploy", "the deploy pipeline runs here", 1)
	page(t, db, "p.prod", "PROD", "Launch", "the deploy pipeline is announced here", 1)
	for i := range 20 {
		page(t, db, fmt.Sprintf("p.filler%d", i), "PROD", "Filler",
			"unrelated words about other things entirely", 1)
	}
	indexAll(t, x)

	all, err := x.Search(t.Context(), search.SearchQuery{Text: "deploy pipeline"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unscoped search returned %v", titles(all))
	}
	scoped, err := x.Search(t.Context(), search.SearchQuery{
		Text: "deploy pipeline", Containers: []string{"ENG"}})
	if err != nil {
		t.Fatalf("scoped search: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != "p.eng" {
		t.Fatalf("scoped search returned %v", titles(scoped))
	}
	// The SAME document scores the same either way, which is the property.
	var unscopedScore float64
	for _, h := range all {
		if h.ID == "p.eng" {
			unscopedScore = h.Score
		}
	}
	if scoped[0].Score != unscopedScore {
		t.Errorf("p.eng scored %v scoped and %v unscoped — the weights moved with the filter",
			scoped[0].Score, unscopedScore)
	}
}

// A SEARCH IS STABLE. Two runs of one query over unchanged data must return
// the same order, or it looks broken long before anyone suspects the ranking.
func TestTheSameQueryRanksTheSameWay(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)
	for i := range 12 {
		page(t, db, fmt.Sprintf("p.%d", i), "ENG", "Tied",
			"identical body for every one of these pages", 1)
	}
	indexAll(t, x)

	first, err := x.Search(t.Context(), search.SearchQuery{Text: "identical", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := x.Search(t.Context(), search.SearchQuery{Text: "identical", Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		for i := range first {
			if again[i].ID != first[i].ID {
				t.Fatalf("run 2 ranked %v, run 1 ranked %v — ties are not broken stably",
					ids(again), ids(first))
			}
		}
	}
}

// A query with nothing searchable in it returns nothing, and never an error
// or the whole corpus.
func TestAnEmptyQueryMatchesNothing(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)
	page(t, db, "p.any", "ENG", "Anything", "some words", 1)
	indexAll(t, x)

	for _, text := range []string{"", "   ", "- , !", "a"} {
		hits, err := x.Search(t.Context(), search.SearchQuery{Text: text})
		if err != nil {
			t.Errorf("query %q errored: %v", text, err)
		}
		if len(hits) != 0 {
			t.Errorf("query %q returned %v", text, titles(hits))
		}
	}
}

func ids(hits []search.SearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

// openStore brings up one node's estates for the index tests.
//
// BOTH OF THEM, which is the whole shape of the indexer now: the sources it
// reads are replicated and the index it writes is this node's own, and a test
// over one estate would not exercise the boundary at all.
func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return db
}

// waitFor polls until want holds, or fails saying what it was waiting for.
func waitFor(t *testing.T, want func() bool, why string) {
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
