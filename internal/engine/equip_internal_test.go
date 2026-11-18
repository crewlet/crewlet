package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/queue"
)

// THE COMPANY'S COALESCING KNOBS REACH THE VALUE EVERY SEAT READS:
// notification_coalesce_window_seconds and notification_coalesce_max_batch,
// rather than queue.DefaultBatchOptions.
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
// Every attachment on this node shares one *queue.BatchOptions, so a hot
// reload lands on the next batch, not only on seats that later move node.
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

// THE GATE READS CONFIG; THE SEARCHER IS RESOLVED PER CALL, for the reason
// [knowledgeSearch] gives: one captured at equip is the previous epoch's.
func TestSearchKnowledgeIsGatedOnConfigAndResolvedPerCall(t *testing.T) {
	t.Parallel()
	// A NIL INTERFACE when the company runs NO knowledge base: the tool is
	// omitted, never registered-and-empty.
	off := &Company{Config: &config.Company{
		Knowledge: config.Knowledge{Backend: config.KnowledgeNone},
	}}
	if got := knowledgeSearch(&Engine{}, off); got != nil {
		t.Errorf("a company that turned its knowledge base off got %v", got)
	}

	// THE GATE IS THE BACKEND, not a vendor block: a company that declares
	// nothing runs DOCLET and still gets search_knowledge.
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
	// With nothing started it answers CLOSED rather than panicking.
	if got.CanSearch(nil, nil) {
		t.Error("an unstarted knowledge base reported itself searchable")
	}
	if hits := got.Search(t.Context(), knowledge.Query{Text: "x"}).Hits; hits != nil {
		t.Errorf("an unstarted knowledge base returned %v", hits)
	}
}
