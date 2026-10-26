package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// probeTool answers every call and remembers the value each one carried.
type probeTool struct {
	mu   sync.Mutex
	seen []string
}

func (p *probeTool) Name() string        { return "probe" }
func (p *probeTool) Description() string { return "probes" }
func (p *probeTool) Parameters() map[string]any {
	return map[string]any{"type": "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}}}
}

func (p *probeTool) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	value, _ := args["value"].(string)
	p.mu.Lock()
	p.seen = append(p.seen, value)
	p.mu.Unlock()
	return tools.Result{Output: "probed " + value}, nil
}

func (p *probeTool) values() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// connect dials an executor run's bridge the way its box's coding agent does,
// reporting the error rather than failing on it.
func connect(t *testing.T, endpoint string) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: httpxtest.Pool(t),
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = session.Close() })
	}
	return session, err
}

func callProbe(t *testing.T, session *mcp.ClientSession, value string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "probe", Arguments: map[string]any{"value": value}})
	if err != nil {
		t.Fatalf("probe(%s): %v", value, err)
	}
	if res.IsError {
		t.Fatalf("probe(%s) was refused: %+v", value, res.Content)
	}
}

// AN EXECUTOR'S RUN OUTLIVES THE PROCESS THAT LAUNCHED IT, AND SO DOES ITS BRIDGE.
// The box goes on calling the seat's tools after its node restarts; the
// restarted node holds no session until it takes the run's seat back, and
// then — as it recovers the seat — it rebuilds the run's surface from the
// run's own row and opens the run's JOB again, so the endpoint the box was
// launched with reaches it: the same URL, the same token, the seat's tools,
// and the calls recorded on the run's row under its own job, numbered after
// the ones before the restart.
//
// Mutation: open the recovered session under anything but the job the row
// names, and the box's endpoint is refused; leave the hook out of the sandbox
// runtime's wiring, and nothing is reopened when the seat is recovered (the
// wiring is held by the coordinator test, this one by calling the hook).
func TestARunIsServedAgainByTheNodeThatRecoversIt(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	material := runtoken.OneKey("k", "test-key")
	serve := func(b *mcpbridge.Bridge) {
		mux := http.NewServeMux()
		mux.Handle(mcpbridge.PathPrefix+"{token}", b.Handler())
		server.Config.Handler = mux
	}
	launched := bridgeFor(t, mcpbridge.Options{Material: material, BaseURL: server.URL})
	serve(launched)

	c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code",
		model: "claude-golden", runIn: config.PlacementE2B})
	tool := &probeTool{}
	c.Tools = tools.NewRegistry()
	if err := c.Tools.Register(tool, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	e, jobs := launchRig(t, c, launched)
	e = withSessions(t, e)
	turn := &turnctx.Turn{RunID: "t1", WorkKey: "wk-1", Reply: "tool",
		WorkSince: time.Now().UTC().Add(-time.Minute),
		Seat:      seat, Org: c.Org, Calls: turnctx.NewCallLog()}
	launcher := &agentLauncher{engine: e, turn: turn, seat: seat, company: c}
	if err := launcher.LaunchExecutor(t.Context(), runner.AgentRunRequest{
		Brief: "fix the failing test", Round: 1, Fence: e.seatFence(seat.Handle()),
		Surface: tools.NewSurface("execute", c.Tools.Snapshot(), []string{"probe"}).ForTurn(turn),
	}); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	// THE SUSPENSION THE LAUNCHING TURN WRITES, as the engine persists it.
	blob, err := execstate.Encode(execstate.State{
		Version: execstate.Version, Round: 1,
		ActiveTools: []string{"probe"}, Task: "fix the failing test",
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	pending := e.sandbox.Load().pending
	if ok, err := pending.MarkSuspended(t.Context(), "t1",
		sandbox.Suspension{State: blob, Iteration: 1}); err != nil || !ok {
		t.Fatalf("MarkSuspended: %v %v", ok, err)
	}
	started := jobs.Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want 1", len(started))
	}
	endpoint := started[0].MCPServers[config.ToolServerName].URL
	before, err := connect(t, endpoint)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	callProbe(t, before, "before")

	// THE RESTART: a fresh process at the address the box dials, over the
	// fleet's keyring, holding no session yet.
	e.bridge = bridgeFor(t, mcpbridge.Options{Material: material, BaseURL: server.URL})
	serve(e.bridge)
	if _, err := connect(t, endpoint); err == nil {
		t.Fatal("a restarted node served the run before it took the run's seat back")
	}

	// THE RECOVERY, with the run's row as the seat's recovery reads it.
	row, found, err := pending.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("the run's row: found %v, %v", found, err)
	}
	e.reopenAgentRun(t.Context(), row)

	after, err := connect(t, endpoint)
	if err != nil {
		t.Fatalf("the box's own endpoint does not reach the recovered run: %v", err)
	}
	callProbe(t, after, "after")
	if got := tool.values(); len(got) != 2 || got[1] != "after" {
		t.Errorf("the seat's tool saw %v, want the call before the restart and the one after", got)
	}
	row, _, err = pending.Get(t.Context(), "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(row.BridgeCalls) != 2 {
		t.Fatalf("the run's row holds %d calls, want both", len(row.BridgeCalls))
	}
	last := row.BridgeCalls[1]
	if last.Name != "probe" || last.Seq != 2 {
		t.Errorf("the call after the recovery is recorded as %+v, want probe as the run's second", last)
	}
}
