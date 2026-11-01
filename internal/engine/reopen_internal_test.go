package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
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

// recoverableRun is an executor run launched over a bridge served at a test
// server's address, its turn parked on it as the engine parks one, and the
// box's own endpoint — the run a node that restarts has to serve again.
type recoverableRun struct {
	e        *Engine
	endpoint string
	tool     *probeTool
	// restart puts a fresh process's bridge at the address the box dials,
	// over the fleet's keyring, holding no session yet.
	restart func()
}

// launchRecoverable launches swe's executor with the probe on its surface and
// parks its turn on the run, as the engine persists a suspension.
func launchRecoverable(t *testing.T) recoverableRun {
	t.Helper()
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
	// AN MCP SERVER'S TOOL, never a builtin: every builtin is decided by the
	// authority table (builtin.TestEveryRegisteredToolHasAnAuthorityRule),
	// and a seat's surface leaves off one the table refuses before any
	// object is named ([Company.withheldFrom]) — which a builtin with no row,
	// as the probe would be, always is. The recovery rebuilds the surface
	// the seat is offered, so the probe has to be a tool it is offered.
	if err := c.Tools.Register(tool, tools.Origin("probe")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	e, jobs := launchRig(t, c, launched)
	e = withSessions(t, e)
	turn := &turnctx.Turn{RunID: "t1", WorkKey: "wk-1", Reply: "tool",
		WorkSince: time.Now().UTC().Add(-time.Minute),
		Seat:      seat, Org: c.Org, Calls: turnctx.NewCallLog()}
	launcher := &agentLauncher{engine: e, turn: turn, seat: seat, company: c}
	if _, err := launcher.LaunchExecutor(t.Context(), runner.AgentRunRequest{
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
	if ok, err := e.sandbox.Load().pending.MarkSuspended(t.Context(), "t1",
		sandbox.Suspension{State: blob, Iteration: 1}); err != nil || !ok {
		t.Fatalf("MarkSuspended: %v %v", ok, err)
	}
	started := jobs.Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want 1", len(started))
	}
	return recoverableRun{
		e: e, endpoint: started[0].MCPServers[config.ToolServerName].URL, tool: tool,
		restart: func() {
			e.bridge = bridgeFor(t, mcpbridge.Options{Material: material, BaseURL: server.URL})
			serve(e.bridge)
		},
	}
}

// row is the run's row as the seat's recovery reads it.
func (r recoverableRun) row(t *testing.T) sandbox.PendingRun {
	t.Helper()
	row, found, err := r.e.sandbox.Load().pending.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("the run's row: found %v, %v", found, err)
	}
	return row
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
	run := launchRecoverable(t)
	before, err := connect(t, run.endpoint)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	callProbe(t, before, "before")

	run.restart()
	if _, err := connect(t, run.endpoint); err == nil {
		t.Fatal("a restarted node served the run before it took the run's seat back")
	}

	// THE RECOVERY, with the run's row as the seat's recovery reads it.
	run.e.reopenAgentRun(t.Context(), run.row(t))

	after, err := connect(t, run.endpoint)
	if err != nil {
		t.Fatalf("the box's own endpoint does not reach the recovered run: %v", err)
	}
	callProbe(t, after, "after")
	if got := run.tool.values(); len(got) != 2 || got[1] != "after" {
		t.Errorf("the seat's tool saw %v, want the call before the restart and the one after", got)
	}
	row := run.row(t)
	if len(row.BridgeCalls) != 2 {
		t.Fatalf("the run's row holds %d calls, want both", len(row.BridgeCalls))
	}
	last := row.BridgeCalls[1]
	if last.Name != "probe" || last.Seq != 2 {
		t.Errorf("the call after the recovery is recorded as %+v, want probe as the run's second", last)
	}
}

// A RUN ON A SEAT A PERSON STOPPED IS NOT SERVED ITS TOOLS AGAIN by the node that
// recovers it. The seat's acquisition ends the run right after its recovery
// (prepareSeatSandbox), so a session opened in between would answer the box's
// handshake and rebuild the seat's surface for a run the person asked to end;
// none is opened, and the box's endpoint reaches nothing here.
//
// Mutation: drop the stop check from reopenAgentRun, and the box's endpoint
// reaches a session again.
func TestARunOnAStoppedSeatIsNotServedAgainByTheNodeThatRecoversIt(t *testing.T) {
	t.Parallel()
	run := launchRecoverable(t)
	run.restart()
	row := run.row(t)
	seat, err := run.e.seatID(row.AgentHandle)
	if err != nil {
		t.Fatalf("seatID: %v", err)
	}
	run.e.pauses.mu.Lock()
	run.e.pauses.pauses = map[uuid.UUID]coord.SeatPause{seat: {
		Seat: seat, By: "jane", ByKind: iam.ActorHuman, StopRunning: true, At: time.Now().UTC(),
	}}
	run.e.pauses.known = true
	run.e.pauses.mu.Unlock()

	run.e.reopenAgentRun(t.Context(), row)

	if live := run.e.bridge.Live(); live != 0 {
		t.Fatalf("the recovery opened %d sessions for a run on a seat a person stopped", live)
	}
	if _, err := connect(t, run.endpoint); err == nil {
		t.Fatal("the box of a run on a stopped seat reached the seat's tools again")
	}
}

// A RUN WHOSE JOB WAS LOST IS NOT SERVED ITS TOOLS AGAIN by the node that
// recovers it: its job went with its host — killed by the restart this node
// is recovering from, its box gone, or its node gone from the fleet
// ([sandbox.PendingRun.Lost]) — so nothing in a box is left to call anything,
// and the completion announced for it carries its round on in a job of its
// own the moment the seat's mailbox opens.
//
// Mutation: drop the verdict check from reopenAgentRun, and the recovery opens
// a session for a job that is not there.
func TestARunWhoseJobWasLostIsNotServedAgain(t *testing.T) {
	t.Parallel()
	for _, cause := range sandbox.LossCauses {
		t.Run(string(cause), func(t *testing.T) {
			t.Parallel()
			run := launchRecoverable(t)
			run.restart()
			row := run.row(t)
			row.Lost = &sandbox.JobLoss{Cause: cause, At: time.Now().UTC()}

			run.e.reopenAgentRun(t.Context(), row)

			if live := run.e.bridge.Live(); live != 0 {
				t.Fatalf("the recovery opened %d sessions for a run whose job was lost", live)
			}
		})
	}
}
