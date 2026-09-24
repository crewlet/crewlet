package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// THE COMPANY'S COALESCING KNOBS REACH THE VALUE EVERY SEAT READS.
//
// notification_coalesce_window_seconds and notification_coalesce_max_batch are
// validated, documented settings; a seat attachment that took
// queue.DefaultBatchOptions instead would make setting either a revision that
// changes nothing an operator can observe.
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

// THE GATE READS THE EPOCH'S BACKEND.
//
// A company with no knowledge base gets no search tool at all, and every other
// company gets one — including the one that declares nothing, which runs the
// native knowledge base.
func TestSearchKnowledgeIsGatedOnTheBackend(t *testing.T) {
	t.Parallel()
	// A NIL INTERFACE when the company runs NO knowledge base, not a live
	// adapter over a nil searcher: the tool is omitted rather than
	// registered-and-empty, so a seat is never offered a search its
	// company cannot serve.
	off := &Company{Config: &config.Company{
		Knowledge: config.Knowledge{Backend: config.KnowledgeNone},
	}}
	if got := KnowledgeSearch(&Engine{}, off); got != nil {
		t.Errorf("a company that turned its knowledge base off got %v", got)
	}

	// THE GATE IS THE BACKEND, not the presence of a vendor block. A
	// company that declares nothing runs the NATIVE knowledge base, and
	// gating on `integrations.confluence` left every one of them without
	// search_knowledge while the pages it was meant to find sat in the
	// index.
	bare := &Company{Config: &config.Company{}}
	if got := KnowledgeSearch(&Engine{}, bare); got == nil {
		t.Error("a company on the default backend got no search tool")
	}

	wired := &Company{Config: &config.Company{
		Integrations: config.Integrations{Confluence: &config.Confluence{}},
	}}
	e := &Engine{}
	e.epoch.current.Store(wired)
	got := KnowledgeSearch(e, wired)
	if got == nil {
		t.Fatal("a company with a knowledge block got no search tool")
	}
	// And with nothing started yet it answers CLOSED rather than
	// panicking: a configured backend that failed to start is a real
	// state, and one the seat can act on.
	if refused := got.CanSearch(nil, nil); refused.State != knowledge.NotServed {
		t.Errorf("an unstarted knowledge base answered %+v, want it named as "+
			"not served", refused)
	}
	// AND A SEARCH THAT REACHES NO SEARCHER DID NOT RUN, so it says it
	// failed: "nothing matched" would send a seat to try other words, or
	// to write the page it could not find.
	if answer := got.Search(t.Context(), knowledge.Query{Text: "x"}); len(answer.Hits) != 0 ||
		answer.Partial != nil || !answer.Failed {
		t.Errorf("an unstarted knowledge base answered %+v, want a search "+
			"marked failed", answer)
	}
}

// A KNOWLEDGE BASE THIS NODE IS NOT SERVING IS NAMED AS ONE, apart from the
// two other reasons a search cannot run.
//
// The company runs a knowledge base, so "no knowledge backend is configured"
// is false, and so is anything about a read scope: nothing on this node would
// read the scope. Each of the ways into the state says what to change — the
// org credential Confluence searches on, or the restart that brings up the
// engine's own knowledge base on a node that did not start with it.
func TestAKnowledgeBaseThisNodeIsNotServingIsNamedAsOne(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cfg   *config.Company
		names string
	}{
		"confluence, with an org token that does not resolve": {
			cfg: &config.Company{
				Knowledge: config.Knowledge{Backend: config.KnowledgeConfluence},
				Integrations: config.Integrations{Confluence: &config.Confluence{
					Token: "${CREWLET_TEST_UNSET_CONFLUENCE_TOKEN}",
				}},
			},
			names: "`integrations.confluence.token` resolves to no credential",
		},
		"confluence, whose connection did not build": {
			cfg: &config.Company{
				Knowledge: config.Knowledge{Backend: config.KnowledgeConfluence},
				Integrations: config.Integrations{Confluence: &config.Confluence{
					Token: "a-token",
				}},
			},
			names: "`confluence_unavailable`",
		},
		"the engine's own, on a node that did not start with it": {
			cfg:   &config.Company{},
			names: "this node started without it",
		},
	} {
		e := &Engine{}
		e.epoch.current.Store(&Company{Config: tc.cfg})
		refused := LiveKnowledge(e).CanSearch(nil, &org.Organization{Name: "Acme"})
		if refused.State != knowledge.NotServed || !strings.Contains(refused.Reason(), tc.names) {
			t.Errorf("%s: the adapter answered %+v — want it named not served, "+
				"saying %q", name, refused, tc.names)
		}
		for _, wrong := range []string{"no knowledge base", "`knowledge.scope`"} {
			if strings.Contains(refused.Reason(), wrong) {
				t.Errorf("%s: the refusal names a cause that is not the one: %q",
					name, refused.Reason())
			}
		}
	}

	// THE OTHER TWO STATES, through the same adapter, each named as itself.
	none := &Engine{}
	none.epoch.current.Store(&Company{Config: &config.Company{
		Knowledge: config.Knowledge{Backend: config.KnowledgeNone},
	}})
	if refused := LiveKnowledge(none).CanSearch(nil, nil); refused.State != knowledge.NoBackend {
		t.Errorf("a company with no knowledge base answered %+v", refused)
	}
	scoped := &Engine{}
	scoped.notify.confluence = confluenceParts{
		searcher: confluence.NewSearcher(confluence.SearcherOptions{}),
	}
	scoped.epoch.current.Store(&Company{Config: &config.Company{
		Knowledge:    config.Knowledge{Backend: config.KnowledgeConfluence},
		Integrations: config.Integrations{Confluence: &config.Confluence{}},
	}})
	if refused := LiveKnowledge(scoped).CanSearch(nil, &org.Organization{}); refused.State != knowledge.NoScope ||
		!strings.Contains(refused.Reason(), "`knowledge.scope`") {
		t.Errorf("a served backend with nothing to read answered %+v", refused)
	}
}

// THE SEARCHER IS RESOLVED AT THE CALL, never captured when the tool is built.
//
// equip runs BEFORE an apply reconciles the knowledge base and before the new
// epoch is published, so a searcher captured there is the PREVIOUS epoch's —
// its backend may be one the company has left, its lead map the old org chart
// and its credential the pre-rotation one. Captured, a seat's tool would read
// the company it used to be, silently, since a stale search returns an empty
// result exactly like a real one. So one adapter, built while the node answers
// from its native pages, must answer the very next call from Confluence once
// the epoch moves there.
func TestSearchKnowledgeIsResolvedAtTheCall(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	index := search.NewIndexerOver(db, []search.LexicalSource{search.PageSource{}})
	pagesSearcher, err := pages.NewSearcher(pages.SearcherOptions{Index: index, DB: db})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	e := &Engine{native: &native{searcher: pagesSearcher}}
	onNative := &Company{Config: &config.Company{}}
	e.epoch.current.Store(onNative)
	adapter := KnowledgeSearch(e, onNative)
	chart := &org.Organization{Name: "Acme"}

	// THE PREMISE: the native searcher can always search, and this node's
	// index has built nothing yet.
	if adapter.CanSearch(nil, chart).Refused() || !adapter.Building(t.Context()) {
		t.Fatal("the adapter does not answer from the native searcher to begin with")
	}

	// THE EPOCH MOVES TO CONFLUENCE, whose searcher keeps no index and, with
	// no read scope and no seat credential, can search nothing — so both
	// answers flip only if the same adapter asks the searcher the node holds
	// now.
	e.notify.confluence = confluenceParts{
		searcher: confluence.NewSearcher(confluence.SearcherOptions{}),
	}
	e.epoch.current.Store(&Company{Config: &config.Company{
		Knowledge:    config.Knowledge{Backend: config.KnowledgeConfluence},
		Integrations: config.Integrations{Confluence: &config.Confluence{}},
	}})
	if !adapter.CanSearch(nil, chart).Refused() {
		t.Error("after the move the adapter still answers CanSearch from the native searcher")
	}
	if adapter.Building(t.Context()) {
		t.Error("after the move the adapter still reports the native index building")
	}
}

// THE TOOL HEARS THAT THIS NODE'S INDEX IS STILL BUILDING.
//
// search_knowledge tells "not indexed yet" from "nothing matched" by asking its
// searcher whether it is building — and what it is handed is this adapter, not
// the native searcher. An adapter that forwarded only the rest would hide the
// question, so a seat's own search on a node on its first build would answer
// "no team documents match … not everything is written down": the answer the
// gate exists to prevent, and one a seat acts on by writing a page that
// already exists.
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
	// THE DEFAULT COMPANY, which runs the native knowledge base — the epoch
	// [Engine.Knowledge] answers by.
	native := &Company{Config: &config.Company{}}
	e.epoch.current.Store(native)

	adapter := KnowledgeSearch(e, native)
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
}

// A NODE WITH NOTHING TO WAIT FOR IS NEVER BUILDING: one with no searcher
// wired, and one whose searcher keeps no index of its own — the live
// Confluence search, which is the case a company on Confluence is in for its
// whole life.
func TestANodeWithNoIndexIsNeverBuilding(t *testing.T) {
	t.Parallel()
	if (liveKnowledge{engine: &Engine{}}).Building(t.Context()) {
		t.Error("a node with no knowledge searcher reported itself building")
	}

	onConfluence := &Company{Config: &config.Company{
		Knowledge:    config.Knowledge{Backend: config.KnowledgeConfluence},
		Integrations: config.Integrations{Confluence: &config.Confluence{}},
	}}
	e := &Engine{}
	e.notify.confluence = confluenceParts{
		searcher: confluence.NewSearcher(confluence.SearcherOptions{}),
	}
	e.epoch.current.Store(onConfluence)
	adapter := KnowledgeSearch(e, onConfluence)
	// THE CONTROL: this adapter is answering from the Confluence searcher,
	// so the false below is that searcher's and not an absent one's.
	if got := e.Knowledge(); got == nil || got.Backend() != confluence.Backend {
		t.Fatalf("the engine answers with %v, want the Confluence searcher", got)
	}
	if adapter.Building(t.Context()) {
		t.Error("a live search that keeps no index reported itself building")
	}
}
