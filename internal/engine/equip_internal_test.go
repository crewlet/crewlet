package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// THE COMPANY'S COALESCING KNOBS REACH THE VALUE EVERY SEAT READS.
//
// The regression this exists for: node.Config.BatchOptions was never set, so
// every seat attachment took queue.DefaultBatchOptions and both knobs —
// notification_coalesce_window_seconds and notification_coalesce_max_batch —
// were declared, defaulted, schema'd, validated and documented while being
// read by nothing. Setting either produced a revision that changed nothing an
// operator could observe.
func TestTheCompanysCoalescingKnobsReachTheInbox(t *testing.T) {
	t.Parallel()
	e := &Engine{batch: queue.DefaultBatchOptions()}
	if got := e.batch.EffectiveLinger(); got != 0 {
		t.Fatalf("default linger = %v, want none — the premise", got)
	}

	e.tuneBatching(&Company{Config: &config.Company{
		NotificationCoalesceWindowSeconds: 2.5,
		NotificationCoalesceMaxBatch:      7,
	}})

	if got := e.batch.EffectiveLinger(); got != 2500*time.Millisecond {
		t.Errorf("linger = %v, want the company's 2.5s", got)
	}
	if got := e.batch.EffectiveMaxBatch(); got != 7 {
		t.Errorf("max batch = %d, want the company's 7", got)
	}
}

// AN APPLY MOVES THE VALUE SEATS ARE ALREADY HOLDING.
//
// Every attachment on this node shares one *queue.BatchOptions, which is what
// makes a hot reload land on the next batch instead of only on seats that
// happen to move node afterwards.
func TestAnApplyRetunesTheSeatsAlreadyAttached(t *testing.T) {
	t.Parallel()
	e := &Engine{batch: queue.DefaultBatchOptions()}
	held := e.batch // what a seat attached before the apply is reading

	e.tuneBatching(&Company{Config: &config.Company{
		NotificationCoalesceWindowSeconds: 1,
		NotificationCoalesceMaxBatch:      3,
	}})

	if got := held.EffectiveMaxBatch(); got != 3 {
		t.Errorf("a seat attached before the apply still reads max batch %d", got)
	}
	if got := held.EffectiveLinger(); got != time.Second {
		t.Errorf("a seat attached before the apply still reads linger %v", got)
	}
}

// A NODE WITH NOTHING TO TUNE DOES NOT PANIC. equip runs on every apply,
// including on an engine a test built without a broker.
func TestTuningBatchingWithoutAnythingToTuneIsHarmless(t *testing.T) {
	t.Parallel()
	(&Engine{}).tuneBatching(&Company{Config: &config.Company{}})
	(&Engine{batch: queue.DefaultBatchOptions()}).tuneBatching(nil)
	(&Engine{batch: queue.DefaultBatchOptions()}).tuneBatching(&Company{})
}

// THE GATE READS CONFIG; THE SEARCHER IS RESOLVED PER CALL.
//
// Those cannot be the same read. equip runs BEFORE an apply reconciles the
// knowledge base, so a searcher captured here is the PREVIOUS epoch's — its
// lead map is the old org chart and its credential is the pre-rotation one.
// Capturing it would give a seat a tool that reads the company it used to be,
// silently, since a stale-credential search returns an empty result exactly
// like a real one.
func TestSearchKnowledgeIsGatedOnConfigAndResolvedPerCall(t *testing.T) {
	t.Parallel()
	// A NIL INTERFACE when the company runs NO knowledge base, not a live
	// adapter over a nil searcher: the tool is omitted rather than
	// registered-and-empty, so a seat is never offered a search its
	// company cannot serve.
	off := &Company{Config: &config.Company{
		Knowledge: config.Knowledge{Backend: config.KnowledgeNone},
	}}
	if got := knowledgeSearch(&Engine{}, off); got != nil {
		t.Errorf("a company that turned its knowledge base off got %v", got)
	}

	// THE GATE IS THE BACKEND, not the presence of a vendor block. A
	// company that declares nothing runs the NATIVE knowledge base, and
	// gating on `integrations.confluence` left every one of them without
	// search_knowledge while the pages it was meant to find sat in the
	// index.
	bare := &Company{Config: &config.Company{}}
	if got := knowledgeSearch(&Engine{}, bare); got == nil {
		t.Error("a company on the default backend got no search tool")
	}

	wired := &Company{Config: &config.Company{
		Integrations: config.Integrations{Confluence: &config.Confluence{}},
	}}
	got := knowledgeSearch(&Engine{}, wired)
	if got == nil {
		t.Fatal("a company with a knowledge block got no search tool")
	}
	// And with nothing started yet it answers CLOSED rather than
	// panicking: a configured backend that failed to start is a real
	// state, and one the seat can act on.
	if got.CanSearch(nil, nil) {
		t.Error("an unstarted knowledge base reported itself searchable")
	}
	if hits := got.Search(t.Context(), knowledge.Query{Text: "x"}); hits != nil {
		t.Errorf("an unstarted knowledge base returned %v", hits)
	}
}

// THE TOOL HEARS THAT THIS NODE'S INDEX IS STILL BUILDING.
//
// search_knowledge tells "not indexed yet" from "nothing matched" by asking its
// searcher whether it is building — and what it is handed is this adapter, not
// the native searcher. An adapter that forwarded only the seam's two methods
// would hide the question, so a seat's own search on a node on its first build
// would answer "no team documents match … not everything is written down": the
// answer the gate exists to prevent, and one a seat acts on by writing a page
// that already exists.
func TestSearchKnowledgeHearsTheIndexIsStillBuilding(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	index := search.NewIndexerOver(db, []search.LexicalSource{search.PageSource{}})
	searcher, err := pages.NewSearcher(pages.SearcherOptions{Index: index, DB: db})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	e := &Engine{native: &native{searcher: searcher}}

	// THE SHAPE THE TOOL ASKS: an optional method on what it was handed.
	adapter, ok := knowledgeSearch(e, &Company{Config: &config.Company{}}).(interface {
		Building(ctx context.Context) bool
	})
	if !ok {
		t.Fatal("the adapter a seat's search_knowledge holds cannot say whether " +
			"the index is building")
	}
	if !adapter.Building(t.Context()) {
		t.Error("a node whose index has built nothing is not reported as building")
	}
	for sweeps := 0; !index.ReadyFor(string(search.SourcePage)); sweeps++ {
		if sweeps == 100 {
			t.Fatal("the page corpus never finished its first lap")
		}
		if _, err := index.Sweep(t.Context()); err != nil {
			t.Fatalf("index the pages: %v", err)
		}
	}
	if adapter.Building(t.Context()) {
		t.Error("a node whose pages are built is still reported as building")
	}

	// AND A NODE WITH NO SEARCHER, or one that keeps no index, is never
	// building: there is nothing to wait for.
	if (liveKnowledge{engine: &Engine{}}).Building(t.Context()) {
		t.Error("a node with no knowledge searcher reported itself building")
	}
}
