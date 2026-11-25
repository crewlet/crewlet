package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
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

// minimalSeatCompany is a company of one agent seat with nothing of its own:
// no code host, no tool server, every setting at its default. Its chain's head
// is swapped for a model that answers every host phase ([swapModel]), and its
// run launches on a Claude Code entry whose binary does not exist.
const minimalSeatCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    only:
      harness: claude-code
      model: sonnet
      auth: {mode: api-key, api_key: "${MINIMAL_SEAT_KEY}"}
      overrides: {binary: /nonexistent/claude}
    cli:
      harness: claude-code
      model: sonnet
      run_in: container
      auth: {mode: api-key, api_key: "${MINIMAL_SEAT_KEY}"}
      overrides: {binary: /nonexistent/claude}
roles:
  - name: SWE
    handle: swe
    llm: [only, cli]
`

// minimalSeatToolsBudget is the most bytes the tools a minimal agent seat's
// executor is bridged may weigh ([bridgedBytes]): the least every agent seat's
// first request carries where its harness sends them up front.
//
// 138 500 is the 134 962 (76 tools) the launch floor was measured over and
// most of the slack the floor leaves: its default, 65000, is 858 tokens over
// the largest first request a minimal seat was measured sending (Muse Code's,
// its bridged tools at 4.3 bytes a token), about 3 700 bytes of this list.
// Past it, that request may pass the floor: measure the floor again
// (ADR-0066) and raise both together.
const minimalSeatToolsBudget = 138_500

// A MINIMAL SEAT'S BRIDGED TOOLS STAY UNDER THEIR BUDGET: each listed tool's
// name, description, schema and hints is in the first request of every run
// whose harness loads them up front, which the launch floor's fixed part was
// measured holding (ADR-0066). Measured as a harness receives them, over an
// MCP session to the bridge of a run launched on a running node, so a tool or
// a sentence added to every seat is a decision about the floor, not drift.
//
// Mutation: grow every seat's list past the headroom (a tool, a longer
// description), or lower the budget, and this goes red.
func TestAMinimalSeatsBridgedToolsStayUnderBudget(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNodeWith(t, minimalSeatCompany, Options{
		Environment: config.MapSource{"MINIMAL_SEAT_KEY": "sk-ant-not-real"},
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHeld(t, e, "swe")
	swapModel(t, e, billingModel())
	var (
		mu     sync.Mutex
		listed []*mcp.Tool
		failed error
	)
	client := httpxtest.Pool(t)
	inner := modelPlayer(t, billingRun)
	rig := rigRuns(t, e, func(ctx context.Context, req sandbox.RunRequest) sandbox.Result {
		got, err := bridgedTools(ctx, client, req.MCPServers[config.ToolServerName].URL)
		mu.Lock()
		listed, failed = got, err
		mu.Unlock()
		return inner(ctx, req)
	})
	runToItsEnd(t, e, rig, Request{Handle: "swe", WorkKey: "wk-1", RunID: "run-1",
		WorkSince: time.Now().UTC(), Events: []*events.Event{taskWake("task-9", "ENG-9")}})
	mu.Lock()
	defer mu.Unlock()
	if failed != nil || len(listed) == 0 {
		t.Fatalf("the run's bridge listed %d tools (%v), so this measured nothing", len(listed), failed)
	}
	got := bridgedBytes(t, listed)
	if got > minimalSeatToolsBudget {
		t.Fatalf("a minimal seat's %d bridged tools weigh %d bytes, past their budget of %d: re-measure "+
			"turn_engine.executor_min_budget_tokens (ADR-0066) and raise both together", len(listed), got,
			minimalSeatToolsBudget)
	}
	t.Logf("a minimal seat's %d bridged tools weigh %d bytes (budget %d, headroom %d)", len(listed), got,
		minimalSeatToolsBudget, minimalSeatToolsBudget-got)
}

// bridgedTools is every tool the bridge at endpoint lists, page by page, as a
// harness's MCP client receives them.
func bridgedTools(ctx context.Context, client *http.Client, endpoint string) ([]*mcp.Tool, error) {
	if endpoint == "" {
		return nil, errors.New("the run was handed no bridge to dial")
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	var out []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		out = append(out, tool)
	}
	return out, nil
}

// bridgedBytes is what tools cost in a tools/list answer: each tool's JSON as
// its client received it, summed, as internal/agent/builtin measures the code
// tools' catalogue against theirs.
func bridgedBytes(t *testing.T, tools []*mcp.Tool) int {
	t.Helper()
	total := 0
	for _, tool := range tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("encode %s as it was listed: %v", tool.Name, err)
		}
		total += len(raw)
	}
	return total
}
