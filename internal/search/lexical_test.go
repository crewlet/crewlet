package search_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// page writes an applied page row, which is the indexer's input.
//
// THE REPLICATED ESTATE, because that is where a page lives now: the index is
// this node's own and the sources are the fleet's, and the whole reason the
// indexer walks rather than joins is that no read crosses between them.
func page(t testing.TB, db *store.DB, id, container, title, body string, version int) {
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
func indexAll(t testing.TB, x *search.Indexer) {
	t.Helper()
	quiet := 0
	// Bounded only so a broken sweep fails rather than hangs. One sweep
	// carries [search.IndexBatch] documents, so the ceiling has to clear
	// the largest fixture in this package with room for the orphan and
	// stale walks that share the tick.
	for range 500 {
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

func titles(hits []search.LexicalHit) []string {
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

	hits, err := x.Search(t.Context(), search.LexicalQuery{Text: "rollback"})
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

	hits, err := x.Search(t.Context(), search.LexicalQuery{Text: "incident response"})
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
	if hits, _ := x.Search(t.Context(), search.LexicalQuery{Text: "kubernetes"}); len(hits) != 1 {
		t.Fatalf("the term never indexed: %v", hits)
	}

	page(t, db, "p.edited", "ENG", "Deploy Notes", "we use nomad for this", 2)
	indexAll(t, x)
	if hits, _ := x.Search(t.Context(), search.LexicalQuery{Text: "kubernetes"}); len(hits) != 0 {
		t.Errorf("the removed word still matches: %v", titles(hits))
	}
	if hits, _ := x.Search(t.Context(), search.LexicalQuery{Text: "nomad"}); len(hits) != 1 {
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

	hits, err := x.Search(t.Context(), search.LexicalQuery{Text: "migration plan"})
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
	if hits, _ := x.Search(t.Context(), search.LexicalQuery{Text: "migration plan"}); len(hits) != 0 {
		t.Errorf("an unpublished page is still findable: %v", titles(hits))
	}
}

// "NOT INDEXED YET" AND "NOTHING MATCHED" ARE DIFFERENT ANSWERS. A seat on a
// freshly joined node would otherwise be told the company has written nothing
// down for the whole first index build.
//
// THE GATE IS THE FIRST LAP, not "nothing is pending", and the difference is
// the whole of this case. Read from the pending count, a single page saved a
// moment ago made it false — so every empty search on the node answered "the
// index is still building, try again" rather than "nothing matched", and on a
// company writing continuously it never recovered.
func TestReadyDistinguishesABuildingIndexFromAnEmptyCompany(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	// A BUILD THAT HAS NOT RUN IS NOT READY, even over an empty company:
	// this node has established nothing, and answering "nothing matched"
	// from a walk that never ran is the claim the gate exists to prevent.
	if x.Ready() {
		t.Fatal("an indexer whose walk has never run reported ready")
	}
	page(t, db, "p.new", "ENG", "New", "something to index", 1)
	if n, _ := x.Pending(t.Context()); n != 1 {
		t.Errorf("pending = %d, want 1", n)
	}
	indexAll(t, x)
	if !x.Ready() {
		t.Error("the index never reported ready")
	}

	// AND A PAGE SAVED AFTER THE BUILD DOES NOT UN-READY IT. It is
	// ordinary staleness: a search over the index is a true answer about
	// slightly older rows, and the document that has not been indexed yet
	// is exactly the one the caller would not have found anyway.
	page(t, db, "p.later", "ENG", "Later", "written after the build", 1)
	if n, _ := x.Pending(t.Context()); n != 1 {
		t.Fatalf("pending = %d after a later save, want 1", n)
	}
	if !x.Ready() {
		t.Error("one page saved after the first build turned every empty " +
			"search on this node into \"still building, try again\" — which on " +
			"a company with people in it never clears")
	}
}

// A LAP DOES NOT READ BODIES TO ESTABLISH THAT NOTHING MOVED.
//
// This is what made freshness a function of corpus size: the walk's only
// query selected the BODY, so it was paced at the tokeniser's speed — twenty
// documents per idle tick — and a page saved just behind the cursor waited a
// full lap, which at ten thousand documents is about seventeen minutes rather
// than the "idle tick plus one batch" the pacing constant claims.
//
// Asserted on the SCAN COUNT rather than on a clock: what has to hold is that
// a quiet corpus costs one cheap scan per lap and no fetch at all, and a
// duration would measure this machine.
func TestALapOverAQuietCorpusReadsNoBodies(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	counted := &countingSource{LexicalSource: search.PageSource{}}
	x := search.NewIndexerOver(db, []search.LexicalSource{counted})
	for i := range 45 { // more than one index batch
		page(t, db, fmt.Sprintf("p.%d", i), "ENG", fmt.Sprintf("Page %d", i),
			"shared vocabulary across the whole company", 1)
	}
	indexAll(t, x)

	counted.scans, counted.fetches = 0, 0
	worked, err := x.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if worked {
		t.Fatal("a sweep over an already-indexed corpus found work")
	}
	if counted.fetches != 0 {
		t.Errorf("a lap over an unchanged corpus read bodies %d time(s) — "+
			"that is what paced the walk at the tokeniser's speed and made a "+
			"saved page wait a full lap to become findable", counted.fetches)
	}
	// One scan reaching the whole corpus, and one more that finds the end.
	if counted.scans > 2 {
		t.Errorf("a lap over 45 documents took %d scans, so the walk is still "+
			"paced per index batch rather than per scan batch", counted.scans)
	}

	// AND THE NEXT SAVE IS PICKED UP BY THE LAP AFTER IT, wherever it
	// sorts: the cursor wrapped, so there is no id it is already past.
	page(t, db, "p.zzz", "ENG", "Late", "a distinctive marmalade phrase", 1)
	indexAll(t, x)
	hits, err := x.Search(t.Context(), search.LexicalQuery{Text: "marmalade"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "p.zzz" {
		t.Errorf("the page saved after the build is not findable: %v", titles(hits))
	}
}

// countingSource counts what a lap actually asked the source for.
type countingSource struct {
	search.LexicalSource
	scans, fetches int
}

func (c *countingSource) Versions(ctx context.Context, tx *sql.Tx, after string,
	limit int) ([]search.DocVersion, error) {

	c.scans++
	return c.LexicalSource.Versions(ctx, tx, after, limit)
}

func (c *countingSource) Fetch(ctx context.Context, tx *sql.Tx,
	ids []string) ([]search.Doc, error) {

	c.fetches++
	return c.LexicalSource.Fetch(ctx, tx, ids)
}

// A LAP THAT WAS STOPPED IS NOT A LAP THAT FOUND NOTHING.
//
// [search.Indexer.Sweep]'s `false` is the caller's licence to stop asking — it
// says a whole lap over every source came back with no work — and the two
// honest ways out of the walk are "it wrapped" and "it found documents". A
// cancelled context is neither, so answering it as an empty lap tells a driver
// like [indexAll] that the index has caught up with a corpus it never finished
// reading, and tells [search.Indexer.Run] to go to sleep.
func TestACancelledLapIsNotAnEmptyLap(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	page(t, db, "p.one", "ENG", "Rollback", "roll back a deploy by release", 1)

	ctx, cancel := context.WithCancel(t.Context())
	// Cancelled from INSIDE the walk, after the scan has committed this
	// node to a batch: an already-dead context would be refused by the
	// first statement and would never reach the walk's own check.
	x := search.NewIndexerOver(db, []search.LexicalSource{
		&stoppingSource{LexicalSource: search.PageSource{}, stop: cancel},
	})
	worked, err := x.Sweep(ctx)
	if err == nil {
		t.Fatalf("a lap stopped part way answered worked=%v and no error, which "+
			"is exactly what a source with nothing stale in it answers", worked)
	}
}

// stoppingSource cancels the walk's context while the walk is inside it, and
// answers no documents — the shape that reaches the check at the bottom of the
// lap rather than failing a statement on the way there.
type stoppingSource struct {
	search.LexicalSource
	stop context.CancelFunc
}

func (s *stoppingSource) Fetch(context.Context, *sql.Tx, []string) ([]search.Doc, error) {
	s.stop()
	return nil, nil
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

	waitFor(t, x.Ready, "the indexer never caught up on its own")
	hits, err := x.Search(t.Context(), search.LexicalQuery{Text: "vocabulary", Limit: 5})
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

	all, err := x.Search(t.Context(), search.LexicalQuery{Text: "deploy pipeline"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unscoped search returned %v", titles(all))
	}
	scoped, err := x.Search(t.Context(), search.LexicalQuery{
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

	first, err := x.Search(t.Context(), search.LexicalQuery{Text: "identical", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := x.Search(t.Context(), search.LexicalQuery{Text: "identical", Limit: 5})
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
		hits, err := x.Search(t.Context(), search.LexicalQuery{Text: text})
		if err != nil {
			t.Errorf("query %q errored: %v", text, err)
		}
		if len(hits) != 0 {
			t.Errorf("query %q returned %v", text, titles(hits))
		}
	}
}

func ids(hits []search.LexicalHit) []string {
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
func openStore(t testing.TB) *store.DB {
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

// A RESULT SET THAT FAILS PART WAY THROUGH IS AN ERROR, NEVER A SHORT ANSWER.
//
// # The branch, and why nothing else reaches it
//
// A read has two failure paths and they are not the same. The query failing
// outright is the one every technique reaches — close the database and it
// happens. The other is the result set failing DURING ITERATION, after the
// query succeeded and some rows have already been scanned. That path decides
// whether a caller gets "nothing is known" or a silent PARTIAL answer, and
// only [storetest.FailReadsAfter] arms it.
//
// It matters most here. [Indexer.Search] RAISES rather than answering empty —
// the adapter above it is what turns a failure into the empty block a turn
// tolerates — so a missing rows.Err() check does not surface as an error at
// all. It surfaces as a shorter list of hits, which is indistinguishable from
// a company that has written less down, and a seat acts on it by writing a
// page that already exists.
func TestAReadThatFailsPartWayThroughIsNotAShortAnswer(t *testing.T) {
	t.Parallel()
	fault := storetest.FailReadsAfter(2, errors.New("the result set gave up"))
	db, err := store.Open(t.Context(),
		filepath.Join(t.TempDir(), "node.db"),
		store.Options{WrapDriver: fault.Wrap})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	x := search.NewIndexer(db)
	for i := range 12 {
		page(t, db, fmt.Sprintf("p.%02d", i), "ENG", fmt.Sprintf("Doc %02d", i),
			"the migration plan is here", 1)
	}
	indexAll(t, x)

	// THE CONTROL, on a healthy store and the same handle. Without it, an
	// assertion that a failed read answers nothing also passes for a store
	// that never found anything.
	healthy, err := x.Search(t.Context(), search.LexicalQuery{
		Text: "migration plan", Limit: 50,
	})
	if err != nil {
		t.Fatalf("the control search failed: %v", err)
	}
	if len(healthy) != 12 {
		t.Fatalf("the control found %d of 12 documents, so the assertion below "+
			"would hold for a corpus this test never wrote", len(healthy))
	}

	fault.Arm()
	broken, err := x.Search(t.Context(), search.LexicalQuery{
		Text: "migration plan", Limit: 50,
	})
	if err == nil {
		t.Fatalf("a result set that failed after 2 rows returned %d hits and no "+
			"error — a partial answer here is indistinguishable from a company "+
			"that has written less down, and a seat acts on it by writing a "+
			"page that already exists", len(broken))
	}
	if len(broken) != 0 {
		t.Errorf("a failed read returned %d hits beside its error", len(broken))
	}
	// AND IT NAMES THE READ THAT FAILED — the posting scan, which is the
	// first result set of this query wide enough to fail during
	// iteration. Without this the case would hold while any ONE of the
	// checks downstream survived, and would only go red when the last of
	// them went: measured, dropping both this scan's rows.Err() and the
	// hydration's returns 2 hits and no error at all.
	if !strings.Contains(err.Error(), "read postings") {
		t.Errorf("the failure is reported as %q — the posting scan is the read "+
			"that failed, and an error naming a later one means this scan "+
			"returned a short list somebody downstream tripped over", err)
	}

	// AND THE HYDRATION IS ITS OWN READ, reached directly because it
	// cannot be reached through Search: the posting scan is always the
	// wider result set, so any fault that fails the hydration has already
	// failed the scan above it. The fan-out calls this one on its own —
	// it fuses keys and reads the rows here — so a dropped rows.Err()
	// would turn a partial answer into a short list of hits with no error
	// anywhere.
	if _, err := x.Hydrate(t.Context(), []string{
		"page:p.00", "page:p.01", "page:p.02", "page:p.03", "page:p.04",
	}, "migration plan"); err == nil {
		t.Error("hydrating five keys through a result set that fails after 2 " +
			"rows returned no error — the fused answer would be silently short")
	}

	// AND THE SAME HANDLE RECOVERS, so the failure was the fault rather
	// than a database this test broke.
	fault.Disarm()
	again, err := x.Search(t.Context(), search.LexicalQuery{
		Text: "migration plan", Limit: 50,
	})
	if err != nil {
		t.Fatalf("the store did not recover once the fault was disarmed: %v", err)
	}
	if len(again) != 12 {
		t.Errorf("after recovery the search found %d of 12", len(again))
	}
}

// item writes an applied work-item row, which is the tracker half of the
// indexer's input.
func item(t testing.TB, db *store.DB, id, project, title, body string, version int) {
	t.Helper()
	_, err := db.Replicated().SQL().ExecContext(t.Context(), `
		INSERT INTO tracker_tasks (id, key, project_key, root_id, type, title,
		                           status, status_group, rank, document,
		                           version, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'task', ?, 'todo', 'not_started', 'a0',
		        json_object('body', ?), ?, 0, 0)
		ON CONFLICT (id) DO UPDATE SET
			title = excluded.title, document = excluded.document,
			version = excluded.version`,
		id, id, project, id, title, body, version)
	if err != nil {
		t.Fatalf("insert item %s: %v", id, err)
	}
}

// A WORK ITEM IS FINDABLE BY ITS OWN WORDS, and until this it was findable by
// none of them.
//
// The engine already EMBEDS every task — [TaskCorpus] is registered on the
// fleet-singleton duty, so a company pays a provider bill per item and stores
// a replicated, snapshotted, backed-up vector for each — while the lexical
// half had never heard of the tracker: the walk selected from `pages_heads`,
// the orphan sweep and the readiness gate spelled `source = 'page'` into their
// own predicates, and the one ranked reader asked for pages only. A seat's
// whole vocabulary for finding an item it half remembered was a substring of
// the key or the title.
func TestAWorkItemIsFoundByItsOwnWords(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	item(t, db, "i.retry", "ENG", "Flaky checkout",
		"the payment client retries with no backoff and hammers the gateway", 1)
	page(t, db, "p.runbook", "ENG", "On-call runbook",
		"pager rotation, escalation, and who to call at night", 1)
	indexAll(t, x)

	hits, err := x.Search(t.Context(), search.LexicalQuery{Text: "backoff gateway"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "i.retry" {
		t.Fatalf("search returned %v, and the words are in a work item's "+
			"DESCRIPTION — which no filter this engine offers can reach",
			titles(hits))
	}
	if hits[0].Source != string(search.SourceTask) {
		t.Errorf("the hit came back as source %q", hits[0].Source)
	}

	// AND A REMOVED ITEM LEAVES, through the sweep rather than by hand —
	// what has to hold is that the indexer NOTICES it, and the trash is
	// reachable by asking for it rather than by ranking above live work.
	if _, err := db.Replicated().SQL().ExecContext(t.Context(),
		`UPDATE tracker_tasks SET removed_at = 1 WHERE id = 'i.retry'`); err != nil {
		t.Fatal(err)
	}
	indexAll(t, x)
	if hits, _ := x.Search(t.Context(), search.LexicalQuery{Text: "backoff gateway"}); len(hits) != 0 {
		t.Errorf("a removed item is still findable: %v", titles(hits))
	}
}

// THE GATE WAITS FOR EVERY CORPUS. A node whose pages have been walked and
// whose items have not is one that would report itself ready and answer an
// item search empty — which is the "nothing written down" lie the gate exists
// to prevent, moved one corpus over.
func TestTheReadinessGateCountsEveryCorpus(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	pages := &countingSource{LexicalSource: search.PageSource{}}
	// A SECOND CORPUS THIS NODE CANNOT WALK, which is what a node still
	// building one while the other is done looks like from the gate.
	x := search.NewIndexerOver(db, []search.LexicalSource{pages, unreachableSource{}})

	page(t, db, "p.1", "ENG", "Something", "a body worth indexing", 1)
	if x.Ready() {
		t.Fatal("the index reports itself ready with a corpus it has never " +
			"walked, so a seat is told that corpus holds nothing")
	}
	// The page corpus indexes what it has and then laps; the second one
	// refuses every time it is reached.
	var refused bool
	for range 10 {
		if _, err := x.Sweep(t.Context()); err != nil {
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("the unreachable corpus was walked")
	}
	if pages.scans == 0 {
		t.Fatal("the page corpus was never scanned")
	}
	if x.Ready() {
		t.Error("one corpus finishing its lap made the whole index ready, so " +
			"a seat searching the other is told it holds nothing")
	}
}

// unreachableSource is a corpus this node cannot read — the estate is not
// open, the table is not there yet — so its walk never laps.
type unreachableSource struct{}

func (unreachableSource) Source() string { return "unreachable" }

func (unreachableSource) Versions(context.Context, *sql.Tx, string,
	int) ([]search.DocVersion, error) {

	return nil, errors.New("this corpus cannot be read on this node")
}

func (unreachableSource) Fetch(context.Context, *sql.Tx, []string) ([]search.Doc, error) {
	return nil, nil
}

func (unreachableSource) Live(context.Context, *sql.Tx, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (unreachableSource) Count(context.Context, *sql.Tx) (int, error) { return 0, nil }

// TestAFusedHitCarriesNoScore is a type-level claim, and it is the point of
// [search.FusedHit] existing at all. Hydrate returned [search.LexicalHit] and
// never set its Score, so every hit the two ranked readers in this tree render
// carried a confident zero — and a zero that is not a value is exactly what a
// type must refuse rather than document.
//
// It is asserted by REFLECTION rather than by reading a field, because the
// claim is that the field is ABSENT: a test that read `hit.Score` and expected
// zero would pass on the very shape this change removed.
func TestAFusedHitCarriesNoScore(t *testing.T) {
	t.Parallel()
	if _, held := reflect.TypeFor[search.FusedHit]().FieldByName("Score"); held {
		t.Fatal("a fused hit carries a Score — the fused number is a sum of " +
			"reciprocal placements across two rankers and disjoint slices, " +
			"so it is not comparable with a BM25 score and a caller reading " +
			"it is reading a number that means nothing")
	}
	// AND THE RANKED ONE STILL DOES, or the split would have moved the
	// problem rather than fixed it: a per-ranker BM25 score is exactly
	// what the fan-out merges its own slices on.
	if _, held := reflect.TypeFor[search.LexicalHit]().FieldByName("Score"); !held {
		t.Fatal("a ranked hit lost its Score, which is what a slice merge " +
			"orders on")
	}
}

// EVERY POSTING SURVIVES THE CHUNK BOUNDARY.
//
// A document's inverted list goes out as multi-row INSERTs — one statement per
// chunk rather than one per unique term, which is what the hottest per-row loop
// in the tree became. That is precisely the shape that can bind one row's term
// against another row's frequency and still look correct on a fixture small
// enough to fit in a single statement, so this one is deliberately WIDER than a
// chunk and checks every row rather than counting them.
//
// The frequencies VARY BY POSITION for the same reason: a conversion that
// shifted the rows against each other but kept the set of terms would pass a
// membership check, and fail this one.
func TestEveryPostingSurvivesTheChunkBoundary(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	// THE SAME ARITHMETIC THE WRITER USES, read from the live estate rather
	// than assumed: the limit is probed at open and differs per engine, so a
	// hardcoded fixture size would stop crossing a boundary the day it moved.
	perStatement := store.RowsPerInsert(db.Caps().MaxVariables, 3)
	want := map[string]int{}
	var body strings.Builder
	for i := range perStatement + 7 {
		term := fmt.Sprintf("term%05d", i)
		freq := i%9 + 1
		want[term] = freq
		for range freq {
			body.WriteString(term)
			body.WriteByte(' ')
		}
	}
	if len(want) <= perStatement {
		t.Fatalf("the fixture holds %d unique terms and one statement carries "+
			"%d, so this test would never cross a chunk boundary",
			len(want), perStatement)
	}

	// NO TITLE, so the indexer's triple-title repetition contributes no terms
	// of its own and the expectation above is the whole of what it should
	// write. Straight through Upsert rather than through a page fixture and a
	// sweep: this is about what one document's postings ARE, not about how the
	// walk finds it.
	if err := x.Upsert(t.Context(), []search.Doc{{
		Source: "page", ID: "chunky", Container: "handbook",
		Body: body.String(), Version: 1,
	}}); err != nil {
		t.Fatalf("index the document: %v", err)
	}

	// "page:chunky" is the index's own source-qualified key for that document.
	rows, err := db.SQL().QueryContext(t.Context(),
		`SELECT term, freq FROM kb_postings WHERE doc_id = ?`, "page:chunky")
	if err != nil {
		t.Fatalf("read the postings back: %v", err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var term string
		var freq int
		if err := rows.Scan(&term, &freq); err != nil {
			t.Fatalf("scan a posting: %v", err)
		}
		got[term] = freq
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the postings back: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the index holds %d postings, want %d; first disagreement: %s",
			len(got), len(want), firstDisagreement(got, want))
	}
}

// firstDisagreement names one term the two maps differ on, in term order, so a
// failure over several hundred postings reads as one fact rather than as two
// dumps.
func firstDisagreement(got, want map[string]int) string {
	terms := make([]string, 0, len(want))
	for term := range want {
		terms = append(terms, term)
	}
	for term := range got {
		if _, ok := want[term]; !ok {
			terms = append(terms, term)
		}
	}
	sort.Strings(terms)
	for _, term := range terms {
		g, held := got[term]
		w, wanted := want[term]
		switch {
		case held && wanted && g != w:
			return fmt.Sprintf("%q has freq %d, want %d", term, g, w)
		case !held:
			return fmt.Sprintf("%q is missing, want freq %d", term, w)
		case !wanted:
			return fmt.Sprintf("%q was written with freq %d and should not "+
				"be there at all", term, g)
		}
	}
	return "none"
}

// A FAILED STEP IS RETRIED OVER ITS OWN WINDOW, NOT SKIPPED PAST.
//
// The scan cursor is in-memory state and lives outside the transaction that
// produces it, so advancing it inside that transaction advanced it for a step
// that then failed — and every document in that window went unindexed until
// the walk wrapped, which at ten thousand documents was about seventeen
// minutes. It is the exact staleness this walk's scan-then-fetch design exists
// to remove, reintroduced on the error path.
func TestAFailedIndexStepDoesNotSkipItsWindow(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	for i := range 3 {
		page(t, db, fmt.Sprintf("p.%d", i), "ENG", fmt.Sprintf("Rollback %d", i),
			"roll back a deploy by release", 1)
	}
	src := &failingFetchSource{LexicalSource: search.PageSource{}, failuresLeft: 1}
	x := search.NewIndexerOver(db, []search.LexicalSource{src})

	if _, err := x.Sweep(t.Context()); err == nil {
		t.Fatal("the sweep whose fetch failed reported success")
	}
	// The SAME window, which is what says the cursor did not move: a second
	// scan starting after the first one's last id is a scan that has given
	// up on every document in between.
	src.scanned = nil
	if _, err := x.Sweep(t.Context()); err != nil {
		t.Fatalf("the retry after a failed fetch: %v", err)
	}
	if len(src.scanned) == 0 {
		t.Fatal("the retry scanned nothing at all")
	}
	if got := src.scanned[0]; got != "" {
		t.Fatalf("the retry resumed after %q, so the failed step's window was "+
			"skipped and those pages stay unfindable until the walk wraps", got)
	}

	// And the corpus really does become findable, which is the property the
	// cursor exists to serve rather than a restatement of the assertion.
	indexAll(t, x)
	hits, err := x.Search(t.Context(), search.LexicalQuery{Text: "rollback deploy"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 3 {
		t.Errorf("found %d of 3 pages after a failed step: %v", len(hits), titles(hits))
	}
}

// failingFetchSource fails its first N fetches and records where each scan
// resumed from.
type failingFetchSource struct {
	search.LexicalSource
	failuresLeft int
	scanned      []string
}

func (f *failingFetchSource) Versions(ctx context.Context, tx *sql.Tx, after string,
	limit int) ([]search.DocVersion, error) {

	f.scanned = append(f.scanned, after)
	return f.LexicalSource.Versions(ctx, tx, after, limit)
}

func (f *failingFetchSource) Fetch(ctx context.Context, tx *sql.Tx,
	ids []string) ([]search.Doc, error) {

	if f.failuresLeft > 0 {
		f.failuresLeft--
		return nil, errors.New("the replicated estate went away mid-batch")
	}
	return f.LexicalSource.Fetch(ctx, tx, ids)
}

// THE SCANNER IS WHAT KNOWS, and it has to say so before it scans.
//
// A node holds the whole corpus from the moment it opens its replicated file;
// its lexical index over that corpus is its own and is built by its own walk.
// Between those two moments the node can answer a bucket range with almost
// nothing and look exactly like a node whose range is almost empty.
func TestAScanOverAnUnbuiltIndexSaysItCoveredNothing(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	page(t, db, "p.one", "ENG", "Rollback", "roll back a deploy by release", 1)
	x := search.NewIndexerOver(db, []search.LexicalSource{search.PageSource{}})
	scanner := search.NodeScanner{Index: x}

	got, err := scanner.Scan(t.Context(), search.FanQuery{Text: "rollback"},
		search.Everything())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !got.Building {
		t.Fatal("a scan before the index's first lap reported a covered range, " +
			"so its near-empty answer merges under complete coverage")
	}
	if len(got.Lexical) != 0 {
		t.Errorf("a slice that says it covered nothing carried %d hits — a range "+
			"cannot be both scanned and unscanned", len(got.Lexical))
	}

	indexAll(t, x)
	got, err = scanner.Scan(t.Context(), search.FanQuery{Text: "rollback"},
		search.Everything())
	if err != nil {
		t.Fatalf("scan after the build: %v", err)
	}
	if got.Building {
		t.Fatal("a built index still reports itself building, which would make " +
			"every search in the company permanently scoped")
	}
	if len(got.Lexical) != 1 {
		t.Errorf("the built index answered with %d hits", len(got.Lexical))
	}
}

// PER SOURCE, so one corpus's lap does not hold the other's answers.
func TestReadyForNarrowsToTheSourcesAQueryNames(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	page(t, db, "p.one", "ENG", "Rollback", "roll back a deploy by release", 1)
	x := search.NewIndexerOver(db, []search.LexicalSource{search.PageSource{}})

	if x.ReadyFor(string(search.SourcePage)) {
		t.Error("a corpus that has not wrapped reported itself built")
	}
	// A NAME THIS INDEX DOES NOT COVER IS IGNORED, not refused: the source
	// filter crosses the broker, and a peer on a build that knows a corpus
	// this one does not must answer over what it has rather than declare
	// itself permanently unbuilt.
	if !x.ReadyFor("a-corpus-this-build-has-never-heard-of") {
		t.Error("an unknown source name made the index report itself building, " +
			"which would scope every search a newer peer's query reaches")
	}

	indexAll(t, x)
	if !x.ReadyFor(string(search.SourcePage)) {
		t.Error("the page corpus wrapped and still reports itself building")
	}
	if !x.Ready() {
		t.Error("every source wrapped and Ready() disagrees with ReadyFor()")
	}
}

// A SOURCE THAT IGNORES THE CURSOR FAILS THE WALK RATHER THAN SPINNING IN IT.
//
// The lap's three exits are "it wrapped", "it found work" and "the context
// ended". A source answering the same batch for ever reaches none of them, so
// before this guard the indexer span on one batch: it never indexed again,
// never marked the source built, and the only symptom was search permanently
// reporting scoped coverage with nothing in the log to say why. The realistic
// cause is not a hand-written fake but a collation under which the source's own
// ORDER BY disagrees with the comparison the cursor is carried through.
func TestASourceThatNeverAdvancesTheCursorFailsRatherThanHangs(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexerOver(db, []search.LexicalSource{
		stuckSource{LexicalSource: search.PageSource{}},
	})

	done := make(chan error, 1)
	go func() {
		_, err := x.Sweep(t.Context())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a source answering the same batch for ever reported a clean sweep")
		}
		if !strings.Contains(err.Error(), "not after it") {
			t.Errorf("err = %v, want it to name what the source did wrong", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the walk never returned, which is the hang this guard exists for")
	}
}

// stuckSource answers every scan with the same id, whatever cursor it is given.
type stuckSource struct{ search.LexicalSource }

func (stuckSource) Versions(context.Context, *sql.Tx, string, int) ([]search.DocVersion, error) {
	return []search.DocVersion{{ID: "same", Version: 1}}, nil
}

// A SNIPPET THAT DOES NOT CONTAIN THE SEARCH TERM READS AS A WRONG RESULT.
//
// [textindex.Snippet] centres on the match for exactly that reason, and it
// can only do so over text that HOLDS the match. It was handed the stored
// excerpt — the document's first 600 bytes — so every hit that matched deeper
// than that got the page's preamble instead, under an ellipsis that says text
// was cut but not that the match is in the part that was. A planner reading
// those results sees a list of openings and no reason any of them is a hit.
//
// The Confluence backend behind the same knowledge seam snippets from the
// whole body, so which searcher a company ran decided whether its hits showed
// why they were hits.
func TestASnippetShowsTheMatchEvenDeepInALongPage(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	// The term sits well past the stored excerpt, behind enough preamble
	// that no window over the opening could reach it.
	page(t, db, "p.deep", "ENG", "Platform Handbook",
		strings.Repeat("This handbook covers the platform and its many procedures. ", 40)+
			"The quarterly budget is approved by the finance lead.", 1)
	indexAll(t, x)

	hits, err := x.Search(t.Context(), search.LexicalQuery{Text: "budget"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %v, want the handbook", titles(hits))
	}
	if !strings.Contains(strings.ToLower(hits[0].Snippet), "budget") {
		t.Errorf("the snippet does not contain the term that made it a hit: %q\n"+
			"a reader cannot tell this from a ranking mistake", hits[0].Snippet)
	}
}

// A SHORT PAGE IS UNAFFECTED, which is the half a body read could break: the
// excerpt already held the whole document there, so recutting from the source
// must produce the same answer rather than a different one.
func TestASnippetOverAShortPageIsUnchangedByTheBodyRead(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	page(t, db, "p.short", "ENG", "Rollback",
		"To roll back a deploy, run the rollback command against the release.", 1)
	indexAll(t, x)

	hits, err := x.Search(t.Context(), search.LexicalQuery{Text: "rollback"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %v, want the page", titles(hits))
	}
	if got := hits[0].Snippet; !strings.Contains(got, "roll back a deploy") {
		t.Errorf("snippet = %q, want the whole short body", got)
	}
}

// THE FUSED PATH NEEDS THE SAME SNIPPET, and it is a different function.
//
// A fan-out participant answers with keys and scores — a Slice carries no
// text at all — so [search.Indexer.Hydrate] is the only place in that path
// where a snippet exists. It was cutting from the same 600-byte opening, so a
// company big enough to fan out got preamble snippets on every long page
// while a single-node company got the match.
func TestAFusedHitsSnippetAlsoShowsTheMatch(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	page(t, db, "p.deep", "ENG", "Platform Handbook",
		strings.Repeat("This handbook covers the platform and its many procedures. ", 40)+
			"The quarterly budget is approved by the finance lead.", 1)
	indexAll(t, x)

	hits, err := x.Hydrate(t.Context(),
		[]string{search.Key(search.SourcePage, "p.deep")}, "budget")
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hydrated %d hits, want the handbook", len(hits))
	}
	if !strings.Contains(strings.ToLower(hits[0].Snippet), "budget") {
		t.Errorf("the fused hit's snippet does not contain the term that "+
			"made it a hit: %q", hits[0].Snippet)
	}
}

// A MERGE INPUT DOES NOT PAY FOR A SNIPPET NOBODY READS.
//
// A fan-out participant's answer becomes a [search.Slice], which carries keys
// and scores and no text — so a body read to centre its snippets is fifty
// documents fetched and discarded, per query, per node. The flag is what
// keeps the fix above off that path, and the observable consequence is that
// such an answer still carries the excerpt-cut snippet: the preamble, which
// is free because it is a column of a row the hydration already reads.
func TestAMergeInputKeepsTheFreeSnippetAndReadsNoBody(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)

	page(t, db, "p.deep", "ENG", "Platform Handbook",
		strings.Repeat("This handbook covers the platform and its many procedures. ", 40)+
			"The quarterly budget is approved by the finance lead.", 1)
	indexAll(t, x)

	hits, err := x.Search(t.Context(), search.LexicalQuery{
		Text: "budget", MergeInput: true,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %v, want the handbook", titles(hits))
	}
	if strings.Contains(strings.ToLower(hits[0].Snippet), "budget") {
		t.Errorf("a merge input read the body to cut a snippet its own "+
			"transport cannot carry: %q", hits[0].Snippet)
	}
}

// THE FALLBACK SNIPPET SAYS THE PAGE GOES ON.
//
// When a hit's body cannot be read, its snippet is cut from the opening the
// index stored instead. A match near the END of that opening gives a window
// reaching its last byte — an edge the snippet does not mark, because it is
// the end of the text the snippet was handed. The page goes on well past it,
// so the stored opening has to carry the marker itself, or the reader is shown
// a fragment that stops mid-word as though that were where the page ends.
func TestAFallbackSnippetMarksWhereTheStoredOpeningWasCut(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	src := &blindableSource{LexicalSource: search.PageSource{}}
	x := search.NewIndexerOver(db, []search.LexicalSource{src})

	// The match sits about five hundred bytes in, behind filler that shares
	// no word with the query, inside the stored opening but near its end.
	page(t, db, "p.long", "ENG", "Handbook",
		strings.Repeat("alpha beta gamma delta ", 22)+"budget approvals "+
			strings.Repeat("omega ", 200), 1)
	indexAll(t, x)

	// THE BODY READ FAILS from here on, which is the path every fallback
	// takes: the fetch that would have cut the snippet from the real body.
	src.blind.Store(true)
	hits, err := x.Hydrate(t.Context(),
		[]string{search.Key(search.SourcePage, "p.long")}, "budget")
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hydrated %d hits, want the handbook", len(hits))
	}
	snippet := hits[0].Snippet
	if !strings.Contains(snippet, "budget") {
		t.Fatalf("the fallback snippet lost the match, so this case is not "+
			"about the edge it asserts: %q", snippet)
	}
	if !strings.HasSuffix(snippet, "…") {
		t.Errorf("the fallback snippet ends %q with no marker, on a page that "+
			"goes on for another thousand bytes", snippet[max(0, len(snippet)-24):])
	}
}

// blindableSource is a source whose body read can be made to fail, after the
// index has been built through it.
type blindableSource struct {
	search.LexicalSource
	blind atomic.Bool
}

func (b *blindableSource) Fetch(ctx context.Context, tx *sql.Tx,
	ids []string) ([]search.Doc, error) {

	if b.blind.Load() {
		return nil, errors.New("the replicated estate would not answer")
	}
	return b.LexicalSource.Fetch(ctx, tx, ids)
}

// A QUIET SWEEP HAS LOOKED AT EVERY INDEXED ROW, in both directions.
//
// `false` from [search.Indexer.Sweep] is the loop's licence to sleep. The stale
// walk earns it by lapping; the orphan walk has to as well, or a page somebody
// trashed stays findable by keyword while the walk that would drop it is
// paced at one small batch per idle tick. So: trash the page the walk reaches
// LAST, sweep until the first quiet answer, and it must already be gone.
func TestAQuietSweepHasDroppedEveryOrphan(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	x := search.NewIndexer(db)
	const pages = 5 * search.IndexBatch
	for i := range pages {
		page(t, db, fmt.Sprintf("p.%03d", i), "ENG", fmt.Sprintf("Plan %d", i),
			"the migration plan is here", 1)
	}
	indexAll(t, x)

	last := fmt.Sprintf("p.%03d", pages-1)
	if _, err := db.Replicated().SQL().ExecContext(t.Context(),
		`UPDATE pages_heads SET status = 'trashed' WHERE id = ?`, last); err != nil {
		t.Fatal(err)
	}
	quiet := false
	for range pages {
		worked, err := x.Sweep(t.Context())
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if quiet = !worked; quiet {
			break
		}
	}
	if !quiet {
		t.Fatal("the indexer never reported a quiet sweep over a settled corpus")
	}
	hits, err := x.Search(t.Context(), search.LexicalQuery{
		Text: "migration plan", Limit: pages,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, hit := range hits {
		if hit.ID == last {
			t.Fatalf("the sweep reported nothing left to do while the trashed "+
				"page %s was still findable — a quiet answer is a licence to "+
				"sleep, and it was given before the orphan walk reached it", last)
		}
	}
	if len(hits) != pages-1 {
		t.Fatalf("found %d of the %d pages still published", len(hits), pages-1)
	}
}

// A FAILED EXISTENCE CHECK DOES NOT SKIP ITS WINDOW, which is the orphan
// walk's half of [TestAFailedIndexStepDoesNotSkipItsWindow].
//
// The orphan cursor is in-memory state and the check against the source can
// fail after the index rows were read. A cursor moved before the check would
// pass over every row in that window until the walk next wrapped — and a
// trashed page among them would stay findable for that long.
func TestAFailedOrphanCheckDoesNotSkipItsWindow(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	for i := range 3 {
		page(t, db, fmt.Sprintf("p.%d", i), "ENG", fmt.Sprintf("Rollback %d", i),
			"roll back a deploy by release", 1)
	}
	src := &failingLiveSource{LexicalSource: search.PageSource{}}
	x := search.NewIndexerOver(db, []search.LexicalSource{src})
	indexAll(t, x)

	if _, err := db.Replicated().SQL().ExecContext(t.Context(),
		`UPDATE pages_heads SET status = 'trashed' WHERE id = 'p.0'`); err != nil {
		t.Fatal(err)
	}
	src.failuresLeft, src.asked = 1, nil
	if _, err := x.Sweep(t.Context()); err == nil {
		t.Fatal("the sweep whose existence check failed reported success")
	}
	if _, err := x.Sweep(t.Context()); err != nil {
		t.Fatalf("the retry after a failed check: %v", err)
	}
	if len(src.asked) < 2 || !reflect.DeepEqual(src.asked[0], src.asked[1]) {
		t.Fatalf("the failed check asked about %v and the retry about %v — a "+
			"retry that starts anywhere else has given up on the window "+
			"between", src.asked[0], src.asked[1:])
	}
	hits, err := x.Search(t.Context(), search.LexicalQuery{Text: "rollback deploy"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("found %v after the retry, want the two pages still published",
			titles(hits))
	}
}

// failingLiveSource fails its first N existence checks and records what each
// one was asked about.
type failingLiveSource struct {
	search.LexicalSource
	failuresLeft int
	asked        [][]string
}

func (f *failingLiveSource) Live(ctx context.Context, tx *sql.Tx,
	ids []string) (map[string]bool, error) {

	f.asked = append(f.asked, append([]string(nil), ids...))
	if f.failuresLeft > 0 {
		f.failuresLeft--
		return nil, errors.New("the replicated estate went away mid-check")
	}
	return f.LexicalSource.Live(ctx, tx, ids)
}
