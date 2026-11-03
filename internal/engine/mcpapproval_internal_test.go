package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// recordingHold is a [tools.Holder] that records the calls it is handed and
// answers each held.
type recordingHold struct{ held []tools.HeldCall }

func (h *recordingHold) Hold(_ context.Context, _ *turnctx.Turn, call tools.HeldCall) (tools.Result, bool) {
	h.held = append(h.held, call)
	return tools.Result{Output: "held for a person"}, true
}

// A TOOL AT NEEDS APPROVAL IS OFFERED, AND NO PHASE MAKES THE CALL (ADR-0077).
// A server whose entry sets no level files its unannotated tool at approve, so
// the tool is in the seat's registry and on every surface, marked. Proved end
// to end against a real server: a phase on the engine host, through its real
// tool session (the relay), is refused the call as forbidden, told the seat's
// own run may ask, and the server never answers it; the executor's surface,
// through a real bridge, hands it to its holder, the harness's client is told
// it was held, and the server never answers it either.
//
// Mutation: drop the approval mark from Registry.RegisterMCP, and the server
// answers both calls.
func TestAToolAtNeedsApprovalIsRefusedOnTheHostAndHeldForTheExecutor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:`+
		levelledStandIn("tracker", true, []string{"close_items"}, "{}")+`
roles:
  - {name: CEO, handle: ceo, llm: zulu}
`)
	e := &Engine{}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { _, _ = e.mcp.StopAll(context.Background()) })
	e.epoch.current.Store(company)
	e.startSharedServers(ctx, company)

	registry := e.ToolsFor("ceo")
	entry, ok := registry.Lookup("close_items")
	if !ok || !entry.NeedsApproval {
		t.Fatalf("the seat's registry holds close_items %v, marked %v: want it filed at approve", ok, entry.NeedsApproval)
	}
	seat := company.Org.AgentSeatByHandle("ceo")
	turn := &turnctx.Turn{RunID: "run-1", Seat: seat, Org: company.Org}

	t.Run("a phase on the engine host", func(t *testing.T) {
		host, err := toolsession.NewHost()
		if err != nil {
			t.Fatalf("NewHost: %v", err)
		}
		t.Cleanup(func() { _ = host.Close() })
		script := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
			{Calls: []llmtest.Call{{Name: "close_items"}}}, {Text: "done"},
		}}}}
		surface := tools.NewSurface("review", registry.Snapshot(), []string{"close_items"}).ForTurn(turn)
		_, _ = toolloop.Run(ctx, toolloop.Config{Provider: script, Surface: surface, Host: host, MaxRounds: 2,
			Messages: []llm.Message{{Role: llm.RoleSystem, Content: "you are a seat"},
				{Role: llm.RoleUser, Content: "tidy up"}}})
		results := script.Results()
		if len(results) != 1 || !results[0].Failed || results[0].Output == "ok" ||
			!strings.Contains(results[0].Output, "ceo's own run") {
			t.Fatalf("the call through the relay answered %+v, want it refused, naming the seat's own run", results)
		}
	})

	t.Run("the executor's bridge", func(t *testing.T) {
		mux := http.NewServeMux()
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		bridge, err := mcpbridge.New(mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"),
			BaseURL: server.URL})
		if err != nil {
			t.Fatalf("mcpbridge.New: %v", err)
		}
		mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
		mux.Handle(mcpbridge.ServerPattern, bridge.Handler())
		hold := &recordingHold{}
		surface := tools.NewSurface("execute", registry.Snapshot(), []string{"close_items"}).ForTurn(turn).
			WithHolder(hold)
		url := bridge.Open(ctx, &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "ceo",
			Role: "CEO", Surface: surface})
		dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "harness", Version: "1"}, nil).
			Connect(dialCtx, &sdkmcp.StreamableClientTransport{Endpoint: mcpbridge.ServerURL(url, "tracker"),
				HTTPClient: httpxtest.Pool(t)}, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = session.Close() })
		res, err := session.CallTool(dialCtx, &sdkmcp.CallToolParams{Name: "close_items",
			Arguments: map[string]any{"id": "7"}})
		if err != nil || !res.IsError || len(hold.held) != 1 || hold.held[0].Args["id"] != "7" {
			t.Fatalf("the call over the bridge answered %+v, %v, held %+v: want it held with its arguments",
				res, err, hold.held)
		}
	})
}

// A CALL WAITING FOR APPROVAL STANDS ON ITS SERVER'S CONDITION: counted while
// its run is parked on it, dated by the call waiting longest, ties to the lower
// turn, and named by the seat whose run made it; an answered call, one whose
// run moved on, and one on a server no condition names count for nothing.
func TestACallWaitingForApprovalStandsOnItsServersCondition(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	held := func(turn, seat, server, launch string, heldAt time.Time) sandbox.PendingRun {
		return sandbox.PendingRun{TurnID: turn, AgentHandle: seat, LaunchID: "job-1",
			Status: sandbox.StatusAwaiting, HeldCall: &sandbox.HeldCall{ID: turn, LaunchID: launch,
				Server: server, Tool: "close_items", HeldAt: heldAt}}
	}
	answered := held("t-answered", "ops", "tracker", "job-1", at.Add(-time.Hour))
	answered.Answer = &sandbox.RecordedAnswer{}
	runs := []sandbox.PendingRun{
		held("t-b", "dev", "tracker", "job-1", at),
		held("t-a", "ceo", "tracker", "job-1", at),
		held("t-later", "ceo", "tracker", "job-1", at.Add(time.Minute)),
		answered,
		held("t-moved", "ops", "tracker", "job-0", at.Add(-time.Hour)),
		held("t-other", "ops", "elsewhere", "job-1", at.Add(-time.Hour)),
	}
	conds := []statelog.MCPServerCondition{{Server: "tracker"}}
	heldConditions(conds, runs)
	got := conds[0]
	if got.Approvals != 3 || !got.ApprovalSince.Equal(at) ||
		got.Approval != (statelog.MCPApprovalNote{Seat: "ceo", Tool: "close_items"}) {
		t.Fatalf("the condition is %+v, want three waiting since %s, named by ceo's (the lower turn)", got, at)
	}
}

// THE APPROVAL GRACE IS A THIRD OF THE DEFAULT PAUSE TTL, which a parked run's
// box is kept for: an administrator told then still has the rest of it in
// which an answer carries the run on in the box it left.
func TestTheApprovalGraceIsAThirdOfTheDefaultPauseTTL(t *testing.T) {
	t.Parallel()
	if statelog.MCPApprovalGrace*3 != sandbox.DefaultPauseTTL {
		t.Fatalf("the approval grace is %s and the default pause TTL %s: retune them together",
			statelog.MCPApprovalGrace, sandbox.DefaultPauseTTL)
	}
}
