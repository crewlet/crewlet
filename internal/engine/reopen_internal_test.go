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

// probeTool answers every call, remembering its value and the model it was
// made on ([toolloop.CallModel]). Named "probe" unless name says otherwise.
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

// connect dials a run's bridge as its box's harness does, returning the error.
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

// skillLoader stands for the load_tool_skill builtin (which has an authority
// row, so the surface offers it). The guard reads what a load unlocks off its
// arguments ([skills.Guard.Observe]), never off the answer.
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

// tryTool calls the tool named over a run's bridge, failing only where the
// call reached nothing.
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

// recoverableRun is an executor run launched over a bridge at a test server's
// address, its turn parked on it: the run a restarted node must serve again.
type recoverableRun struct {
	e *Engine
	// endpoint is the job's own (the engine's tools); server is the probe's
	// MCP server's beneath it (mcpbridge.Servers): the URLs the box was handed.
	endpoint string
	server   string
	tool     *probeTool
	// later is refused by the guard until the run loads laterSkill.
	later *probeTool
	// jobs is the runner of the run's harness, on this node before its restart
	// and after it alike.
	jobs *sandbox.FakeRunner
	// restart puts a fresh process's bridge, holding no session, at the
	// address the box dials, over the same keyring.
	restart func()
}

// launchRecoverable launches swe's executor with the probe, and each tool of
// extra, on its surface, all tools of the probe's MCP server, and parks its
// turn on the run, as the engine persists a suspension.
func launchRecoverable(t *testing.T, extra ...tools.Callable) recoverableRun {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	material := runtoken.OneKey("k", "test-key")
	// The routes a node mounts for the bridge (internal/api, mcp.go).
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
	// AN MCP SERVER'S TOOL, never a builtin: the surface the recovery rebuilds
	// leaves off a builtin with no authority row ([Company.withheldFrom],
	// builtin.TestEveryRegisteredToolHasAnAuthorityRule).
	later := &probeTool{name: "later"}
	served := append([]tools.Callable{tool, later}, extra...)
	for _, each := range served {
		if err := c.Tools.Register(each, tools.Origin("probe")); err != nil {
			t.Fatalf("Register(%s): %v", each.Name(), err)
		}
	}
	if err := c.Tools.Register(skillLoader{}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(%s): %v", toolname.LoadToolSkill, err)
	}
	e, jobs := launchRig(t, c, launched)
	e = withSessions(t, e)
	// The skill the seat must load before calling later; every surface the
	// seat's turns rebuild arms a guard over it.
	e.skills = skills.NewRegistry()
	if _, err := e.skills.PutPage(skills.Skill{Key: laterSkill, Title: "Later", Summary: "how to call later",
		Trigger: skills.Trigger{Tool: "later"}, Required: true, SourcePageID: "later-page"}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	// The seat's whole surface, as the runner offers an executor from launch.
	var active []string
	for _, each := range served {
		active = append(active, each.Name())
	}
	active = append(active, toolname.LoadToolSkill)
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
	// The suspension the launching turn writes.
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
		tool: tool, later: later, jobs: jobs,
		restart: func() {
			e.bridge = bridgeFor(t, mcpbridge.Options{Material: material, BaseURL: server.URL})
			serve(e.bridge)
		},
	}
}

// calls is the run's bridged tool log, as the seat's recovery reads it.
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
// A restarted node holds no session until it takes the run's seat back; then it
// rebuilds the run's surface from its row and reopens the run's JOB, so the
// URLs and token the box was launched with (the job's and each MCP server's)
// reach the seat's tools, and calls are logged under the job, numbered after
// the ones before the restart.
//
// Mutation: open the recovered session under any job but the row's, and the
// box's endpoint is refused. (This test calls the hook; its wiring into the
// sandbox runtime is held by the coordinator test.)
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

// A RECOVERED RUN'S SURFACE IS THE ONE ITS CALLS LEFT IT. The run loaded
// later's required skill over the bridge before its node restarted; recovery
// rebuilds the surface from the row AND the log's calls
// ([runner.Runner.ReopenAgentRun]), so the guard still holds that skill.
//
// Mutation: reopen without the log's calls, and later is refused.
func TestARecoveredRunsSurfaceKeepsWhatItsCallsLoaded(t *testing.T) {
	t.Parallel()
	run := launchRecoverable(t)
	row := run.row(t)
	// The load the run made before its node restarted, as the bridge logs it.
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

// A RECOVERED RUN'S GUARD STILL REFUSES WHAT ITS RUN NEVER UNLOCKED, so the
// case above passes because of the log's load, not a missing guard.
//
// Mutation: rebuild the recovered surface with no skill registry, and later is
// answered.
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

// A RECOVERED RUN'S CALLS ARE MADE ON THE MODEL ITS LAUNCH NAMED
// ([mcpbridge.Session.Model], which a seat's code write names on its push). A
// recovering node has only the row to take it from ([sandbox.LaunchRecord.Model]).
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

// A RUN ON A SEAT A PERSON STOPPED IS NOT SERVED ITS TOOLS AGAIN on recovery:
// the seat's acquisition ends it right after (prepareSeatSandbox), and a
// session opened in between would serve a run the person asked to end.
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

// A RUN WHOSE JOB WAS LOST IS NOT SERVED ITS TOOLS AGAIN on recovery
// ([sandbox.PendingRun.Lost]): no box is left to call, and its announced
// completion carries the round on in a new job.
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
