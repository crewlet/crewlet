package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// laterSkill is the required skill that covers the recoverable run's later
// tool.
const laterSkill = "later-howto"

// probeTool answers every call and remembers the value each one carried, and
// the model it was made on ([toolloop.CallModel]). Named "probe" unless name
// says otherwise.
type probeTool struct {
	name   string
	mu     sync.Mutex
	seen   []string
	models []string
}

func (p *probeTool) Name() string {
	if p.name != "" {
		return p.name
	}
	return "probe"
}
func (p *probeTool) Description() string { return "probes" }
func (p *probeTool) Parameters() map[string]any {
	return map[string]any{"type": "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}}}
}

func (p *probeTool) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	value, _ := args["value"].(string)
	p.mu.Lock()
	p.seen = append(p.seen, value)
	p.models = append(p.models, toolloop.CallModel(ctx))
	p.mu.Unlock()
	return tools.Result{Output: "probed " + value}, nil
}

func (p *probeTool) values() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func (p *probeTool) callModels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.models...)
}

// connect dials an executor run's bridge the way its box's harness does,
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

// skillLoader stands for the seat's load_tool_skill builtin, which has a row
// in the authority table (so the seat's surface offers it), and answers every
// load: what a load unlocks is read off its arguments by the run's guard
// ([skills.Guard.Observe]), never off the tool's answer.
type skillLoader struct{}

func (skillLoader) Name() string        { return toolname.LoadToolSkill }
func (skillLoader) Description() string { return "loads a skill" }
func (skillLoader) Parameters() map[string]any {
	return map[string]any{"type": "object",
		"properties": map[string]any{"key": map[string]any{"type": "string"}}}
}

func (skillLoader) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	key, _ := args["key"].(string)
	return tools.Result{Output: "loaded " + key}, nil
}

func callProbe(t *testing.T, session *mcp.ClientSession, value string) {
	t.Helper()
	callTool(t, session, "probe", value)
}

// callTool calls the tool named over a run's bridge, failing on a refusal.
func callTool(t *testing.T, session *mcp.ClientSession, name, value string) {
	t.Helper()
	res := tryTool(t, session, name, value)
	if res.IsError {
		t.Fatalf("%s(%s) was refused: %s", name, value, contentText(res))
	}
}

// contentText is the text of a tool's answer, as a box's harness reads it.
func contentText(res *mcp.CallToolResult) string {
	var out []string
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			out = append(out, text.Text)
		}
	}
	return strings.Join(out, "\n")
}

// tryTool calls the tool named over a run's bridge and answers what the bridge
// did with it, failing only where the call reached nothing.
func tryTool(t *testing.T, session *mcp.ClientSession, name, value string) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: name, Arguments: map[string]any{"value": value}})
	if err != nil {
		t.Fatalf("%s(%s): %v", name, value, err)
	}
	return res
}

// recoverableRun is an executor run launched over a bridge served at a test
// server's address, its turn parked on it as the engine parks one, and the
// box's own endpoints — the run a node that restarts has to serve again.
type recoverableRun struct {
	e *Engine
	// endpoint is the job's own endpoint, where the bridge serves the
	// engine's tools; server is the probe's MCP server's beneath it, where
	// the bridge serves that server's tools under the server's own name
	// (mcpbridge.Servers): the two URLs the launch handed the box.
	endpoint string
	server   string
	tool     *probeTool
	// later is a tool of the probe's server that a required skill covers
	// (laterSkill): the guard refuses it until the run loads that skill.
	later *probeTool
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
	// THE ROUTES A NODE MOUNTS for the bridge (internal/api, mcp.go): the
	// job's endpoint and each of its MCP servers beneath it.
	serve := func(b *mcpbridge.Bridge) {
		mux := http.NewServeMux()
		mux.Handle(mcpbridge.PathPrefix+"{token}", b.Handler())
		mux.Handle(mcpbridge.ServerPattern, b.Handler())
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
	later := &probeTool{name: "later"}
	for _, each := range []*probeTool{tool, later} {
		if err := c.Tools.Register(each, tools.Origin("probe")); err != nil {
			t.Fatalf("Register(%s): %v", each.Name(), err)
		}
	}
	if err := c.Tools.Register(skillLoader{}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(%s): %v", toolname.LoadToolSkill, err)
	}
	e, jobs := launchRig(t, c, launched)
	e = withSessions(t, e)
	// THE SKILL THE SEAT MUST LOAD BEFORE IT CALLS later, as the company's
	// knowledge base would put it on the engine's registry: every surface
	// the seat's turns rebuild arms a guard over it.
	e.skills = skills.NewRegistry()
	if _, err := e.skills.PutPage(skills.Skill{Key: laterSkill, Title: "Later", Summary: "how to call later",
		Trigger: skills.Trigger{Tool: "later"}, Required: true, SourcePageID: "later-page"}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	// EVERY TOOL THE SEAT HOLDS, as the runner offers an executor its
	// seat's whole surface from its launch.
	active := []string{"probe", "later", toolname.LoadToolSkill}
	turn := &turnctx.Turn{RunID: "t1", WorkKey: "wk-1", Reply: "tool",
		WorkSince: time.Now().UTC().Add(-time.Minute),
		Seat:      seat, Org: c.Org, Calls: turnctx.NewCallLog()}
	launcher := &agentLauncher{engine: e, turn: turn, seat: seat, company: c}
	if _, err := launcher.LaunchExecutor(t.Context(), runner.AgentRunRequest{
		Brief: "fix the failing test", Round: 1, Fence: e.seatFence(seat.Handle()),
		Surface: tools.NewSurface("execute", c.Tools.Snapshot(), active).ForTurn(turn),
	}); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	// THE SUSPENSION THE LAUNCHING TURN WRITES, as the engine persists it.
	blob, err := execstate.Encode(execstate.State{
		Version: execstate.Version, Round: 1,
		ActiveTools: active, Task: "fix the failing test",
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
	box := started[0].MCPServers
	if box["probe"].URL == "" {
		t.Fatalf("the launch handed the box no endpoint for the probe's server: %+v", box)
	}
	return recoverableRun{
		e: e, endpoint: box[config.ToolServerName].URL, server: box["probe"].URL,
		tool: tool, later: later,
		restart: func() {
			e.bridge = bridgeFor(t, mcpbridge.Options{Material: material, BaseURL: server.URL})
			serve(e.bridge)
		},
	}
}

// calls is the run's bridged tool log as the seat's recovery reads it.
func (r recoverableRun) calls(t *testing.T) sandbox.BridgeLog {
	t.Helper()
	log, err := r.e.sandbox.Load().pending.BridgeLog(t.Context(), r.row(t))
	if err != nil {
		t.Fatalf("the run's log: %v", err)
	}
	return log
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
// run's own row and opens the run's JOB again, so the endpoints the box was
// launched with reach it: the same URLs, the same token — the job's own and
// each of the seat's MCP servers beneath it — the seat's tools, and the calls
// recorded on the run's row under its own job, numbered after the ones before
// the restart.
//
// Mutation: open the recovered session under anything but the job the row
// names, and the box's endpoint is refused; leave the hook out of the sandbox
// runtime's wiring, and nothing is reopened when the seat is recovered (the
// wiring is held by the coordinator test, this one by calling the hook).
func TestARunIsServedAgainByTheNodeThatRecoversIt(t *testing.T) {
	t.Parallel()
	run := launchRecoverable(t)
	before, err := connect(t, run.server)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	callProbe(t, before, "before")

	run.restart()
	for _, endpoint := range []string{run.endpoint, run.server} {
		if _, err := connect(t, endpoint); err == nil {
			t.Fatalf("a restarted node served the run at %s before it took the run's seat back", endpoint)
		}
	}

	// THE RECOVERY, with the run's row as the seat's recovery reads it.
	run.e.reopenAgentRun(t.Context(), run.row(t))

	if _, err := connect(t, run.endpoint); err != nil {
		t.Fatalf("the box's own endpoint does not reach the recovered run: %v", err)
	}
	after, err := connect(t, run.server)
	if err != nil {
		t.Fatalf("the box's endpoint for the probe's server does not reach the recovered run: %v", err)
	}
	callProbe(t, after, "after")
	if got := run.tool.values(); len(got) != 2 || got[1] != "after" {
		t.Errorf("the seat's tool saw %v, want the call before the restart and the one after", got)
	}
	logged := run.calls(t)
	if len(logged.Calls) != 2 {
		t.Fatalf("the run's log holds %d calls, want both", len(logged.Calls))
	}
	last := logged.Calls[1]
	if last.Name != "probe" || last.Seq != 2 {
		t.Errorf("the call after the recovery is recorded as %+v, want probe as the run's second", last)
	}
}

// A RECOVERED RUN'S SURFACE IS THE ONE ITS CALLS LEFT IT. The run holds every
// tool its seat holds from its launch, and later is one a required skill
// covers, so the run's guard refuses it until the run loads that skill; the run
// loaded it over the bridge before its node restarted. The node that recovers
// the run rebuilds the surface from the run's row AND the calls in the log
// beside it ([runner.Runner.ReopenAgentRun]), so the guard it arms holds the
// skill the run loaded and later is still the run's to call.
//
// Mutation: reopen the run without its log's calls, and later is refused after
// the recovery for a skill the run already loaded.
func TestARecoveredRunsSurfaceKeepsWhatItsCallsLoaded(t *testing.T) {
	t.Parallel()
	run := launchRecoverable(t)
	row := run.row(t)
	// THE LOAD THE RUN MADE before its node restarted, as the bridge logs a
	// call under the run's job.
	if ok, err := run.e.sandbox.Load().pending.AppendBridgeCall(t.Context(), row.TurnID,
		sandbox.BridgeAppend{Launch: row.LaunchID, Call: sandbox.BridgeCall{
			Name: toolname.LoadToolSkill, Args: `{"key":"` + laterSkill + `"}`,
			Output: "loaded " + laterSkill, Seq: 1, At: time.Now().UTC(),
		}}); err != nil || !ok {
		t.Fatalf("AppendBridgeCall = %v, %v", ok, err)
	}

	run.restart()
	run.e.reopenAgentRun(t.Context(), run.row(t))
	after, err := connect(t, run.server)
	if err != nil {
		t.Fatalf("the box's endpoint for the probe's server does not reach the recovered run: %v", err)
	}
	callTool(t, after, "later", "after")
	if got := run.later.values(); len(got) != 1 || got[0] != "after" {
		t.Errorf("the skill-covered tool saw %v, want the call after the recovery", got)
	}
}

// A RECOVERED RUN'S GUARD STILL REFUSES WHAT ITS RUN NEVER UNLOCKED: the case
// above passes only because the log's load unlocked later, never because the
// recovery armed no guard at all.
//
// Mutation: rebuild the recovered surface with no skill registry, and later is
// answered for a run that never loaded its skill.
func TestARecoveredRunsGuardRefusesWhatItsRunNeverLoaded(t *testing.T) {
	t.Parallel()
	run := launchRecoverable(t)
	run.restart()
	run.e.reopenAgentRun(t.Context(), run.row(t))
	after, err := connect(t, run.server)
	if err != nil {
		t.Fatalf("the box's endpoint for the probe's server does not reach the recovered run: %v", err)
	}
	if res := tryTool(t, after, "later", "unloaded"); !res.IsError {
		t.Errorf("later was answered for a run that never loaded %s: %s", laterSkill, contentText(res))
	}
	if got := run.later.values(); len(got) != 0 {
		t.Errorf("the skill-covered tool ran %v for a run that never loaded its skill", got)
	}
}

// A RECOVERED RUN'S CALLS ARE MADE ON THE MODEL ITS LAUNCH NAMED. Every call a
// box makes over the bridge carries the model the run's harness was pointed at
// ([mcpbridge.Session.Model]), which a seat's code write names on its push. The
// launch's session is given it from the launch; the session a recovering node
// opens has only the run's row to give it from, and the row records it
// ([sandbox.LaunchRecord.Model]) — so a call after the seat moved, or its node
// restarted, is made on the same model as the call before.
//
// Mutation: drop Model from the session reopenAgentRun builds, and the call
// after the recovery carries no model.
func TestARecoveredRunsCallsCarryTheModelItsLaunchNamed(t *testing.T) {
	t.Parallel()
	run := launchRecoverable(t)
	launched := run.row(t).Launch.Model
	if launched == "" {
		t.Fatal("the launch recorded no model on the run's row, so this asserts nothing")
	}
	before, err := connect(t, run.server)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	callProbe(t, before, "before")

	run.restart()
	run.e.reopenAgentRun(t.Context(), run.row(t))
	after, err := connect(t, run.server)
	if err != nil {
		t.Fatalf("the box's endpoint for the probe's server does not reach the recovered run: %v", err)
	}
	callProbe(t, after, "after")

	got := run.tool.callModels()
	if len(got) != 2 {
		t.Fatalf("the seat's tool saw %d calls (%v), want the one before the restart and the one after",
			len(got), got)
	}
	if got[0] != launched {
		t.Fatalf("the call before the restart was made on %q, want the launch's %q", got[0], launched)
	}
	if got[1] != launched {
		t.Errorf("the call after the recovery was made on %q, want the launch's %q, as the call "+
			"before it was", got[1], launched)
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
	for _, endpoint := range []string{run.endpoint, run.server} {
		if _, err := connect(t, endpoint); err == nil {
			t.Fatalf("the box of a run on a stopped seat reached the seat's tools again at %s", endpoint)
		}
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
