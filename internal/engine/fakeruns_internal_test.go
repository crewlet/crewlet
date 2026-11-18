package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE EXECUTOR, AS A TEST RUNS IT: a launchable chain member, a catalogue, a
// bridge and a player, built as `providers.sandbox.fake` builds them (the
// double for every cell and harness) over the REAL launch, bridge and
// coordinator, so a driven turn takes every turn's path: launch, park, bridged
// calls on the run's row, collection, resume. What a run DOES is the case's
// [fakePlayer]; [modelPlayer] plays the case's [phasedHarness] script, so one
// fixture scripts the run and its reviewer alike.

// runnerKey is the providers.llm key a rigged seat's executor launches on.
const runnerKey = "cli"

// runnerEntry is a claude-code entry an executor can launch on, whose binary
// does not exist: the rig's double plays the run, and a phase that falls
// through to it by mistake fails rather than starting a real harness on the
// machine running the suite.
func runnerEntry(t *testing.T) phase.Entry {
	t.Helper()
	provider, err := harness.New(harness.Config{
		Key: runnerKey, Harness: "claude-code", Model: "sonnet",
		StateDir:  t.TempDir(),
		Overrides: map[string]any{"binary": filepath.Join(t.TempDir(), "no-cli-here")},
		Timeout:   time.Minute,
		Auth:      harness.Auth{Mode: harness.AuthAPIKey, APIKey: "sk-ant-not-real"},
	})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	return phase.Entry{Key: runnerKey, Provider: provider}
}

// runnable is a model registry over entries with the runner entry appended
// AFTER them and ending every seat's `llm`: host phases still get the case's
// model at the chain's head, and the launch's pick passes over it to the one
// member with an in-box runner.
func runnable(t *testing.T, entries []phase.Entry, seats ...*org.Role) *phase.Registry {
	t.Helper()
	models, err := phase.NewRegistry(append(entries, runnerEntry(t)))
	if err != nil {
		t.Fatalf("phase.NewRegistry: %v", err)
	}
	for _, seat := range seats {
		seat.LLM = append(seat.LLM, runnerKey)
	}
	return models
}

// runRig is a node's executor runtime under a test: the double's catalogue,
// its runners, and the bridge their runs dial.
type runRig struct {
	engine  *Engine
	bridge  *mcpbridge.Bridge
	runners map[string]*sandbox.FakeRunner
}

// rigRuns gives e an executor runtime whose every run play plays: a bridge on
// its own listener, the double for every cell and harness, the engine's own
// coordinator over a run store on e's fleet, and the fake catalogue on its
// company's epoch.
func rigRuns(t *testing.T, e *Engine, play fakePlayer) *runRig {
	t.Helper()
	if e.backends == nil || e.backends.Fleet == nil {
		t.Fatal("rigRuns needs an engine with a coordination store: a run's row is a fleet record")
	}
	return rigRunsOn(t, e, sandbox.NewCoordStore(e.backends.Fleet), play)
}

// rigRunsOn is [rigRuns] over the given run store, for a case about what the
// engine does with a store's answer.
func rigRunsOn(t *testing.T, e *Engine, pending sandbox.PendingStore, play fakePlayer) *runRig {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: server.URL,
	})
	// Every bridge route, as internal/api's mountBridge mounts them.
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	mux.Handle(mcpbridge.ServerPattern, bridge.Handler())
	mux.Handle(mcpbridge.SteerPattern, bridge.SteerHandler())
	server.Config.Handler = mux
	e.bridge = bridge

	providers := make(map[sandbox.Placement]sandbox.Provider, len(sandbox.Placements))
	for _, p := range sandbox.Placements {
		providers[p] = sandbox.NewFakeProvider()
	}
	runners := fakeRunners(play)
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: providers, DefaultPlacement: sandbox.Direct, Runners: runners,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if e.answers == nil {
		// A hand-built engine lacks the seal `crewlet run` gives it
		// ([Engine.answers]), and the coordinator refuses to build without one.
		e.answers = testAnswers()
	}
	rt, err := e.sandboxRuntimeOn(pending, manager)
	if err != nil {
		t.Fatalf("sandboxRuntimeOn: %v", err)
	}
	t.Cleanup(rt.coordinator.Stop)
	e.useSandbox(rt.pending, rt.coordinator)
	if c := e.Company(); c != nil && c.Config != nil {
		if c.Config.Providers.Sandbox == nil {
			c.Config.Providers.Sandbox = &config.SandboxProvider{Fake: true}
		}
		// The epoch's catalogue, as an apply pins it ([Company.catalogue]).
		c.catalogue = manager
	}
	rig := &runRig{engine: e, bridge: bridge, runners: map[string]*sandbox.FakeRunner{}}
	for name, r := range runners {
		rig.runners[name] = r.(*sandbox.FakeRunner)
	}
	return rig
}

// started is every run the rig's runners were handed, in launch order per harness.
func (r *runRig) started() []sandbox.RunRequest {
	var out []sandbox.RunRequest
	for _, runner := range r.runners {
		out = append(out, runner.Started()...)
	}
	return out
}

// row is turnID's run row, failing the case where there is none.
func (r *runRig) row(t *testing.T, turnID string) sandbox.PendingRun {
	t.Helper()
	row, found, err := r.engine.sandbox.Load().pending.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("the run row of %s: found %v, %v", turnID, found, err)
	}
	return row
}

// calls is the bridged tool log of turnID's run, as the row's job reads it.
func (r *runRig) calls(t *testing.T, turnID string) sandbox.BridgeLog {
	t.Helper()
	pending := r.engine.sandbox.Load().pending
	log, err := pending.BridgeLog(t.Context(), r.row(t, turnID))
	if err != nil {
		t.Fatalf("the run log of %s: %v", turnID, err)
	}
	return log
}

// collect delivers turnID's run completion as the waiter's poll announces one:
// the run is played if not yet, collected, charged, and its turn resumed to
// its end or next launch. The error is the coordinator's, which a
// control-topic delivery would be NAKed on.
func (r *runRig) collect(t *testing.T, turnID string) error {
	t.Helper()
	row := r.row(t, turnID)
	if row.Status != sandbox.StatusRunning {
		t.Fatalf("run %s is %s, want it running: the turn did not park on it", turnID, row.Status)
	}
	// POLLED FIRST, as the waiter's tick does, so the run's bridged calls land
	// on its row before the completion's claim reads it.
	runner, ok := r.runners[row.Harness]
	if !ok {
		t.Fatalf("run %s is on %q, which the rig has no runner for", turnID, row.Harness)
	}
	if state, err := runner.Poll(t.Context(), nil, sandbox.RunHandle{CommandID: row.CommandID}); err != nil ||
		state != sandbox.JobEnded {
		t.Fatalf("polling run %s: %v, %v; want it ended", turnID, state, err)
	}
	row = r.row(t, turnID)
	completion := types.ExecutorFinished{
		Agent: row.AgentID, AgentHandle: row.AgentHandle, RoleName: row.Role,
		TurnID: row.TurnID, WorkKey: row.WorkKey, LaunchID: row.LaunchID,
		SandboxID: row.SandboxID, Harness: row.Harness,
	}
	ev := events.New(completion, events.TraceContext{TraceID: row.TraceID, ParentSpanID: row.SpanID})
	ev.Source = row.Role
	return r.engine.sandbox.Load().coordinator.OnCompleted(t.Context(), completion, ev)
}

// runToItsEnd runs req's turn through its executor's run (park on launch,
// play, collect, resume and review), failing the case off that path.
//
// THE RESUME DECIDES THE ENDING, AND THE ENDING DELETES THE RECORD, possibly
// just after the resume: a turn's end owed on the run's record
// ([sandbox.Coordinator.OweEnd]) is written off the deciding frame, before the
// delete. So the record must be gone or its ending decided right after the
// resume (never relaunched or claimed), and is then waited on until it goes.
func runToItsEnd(t *testing.T, e *Engine, rig *runRig, req Request) {
	t.Helper()
	res, err := e.runTurn(t.Context(), req)
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if !res.Suspended {
		t.Fatalf("the turn ended without parking on its executor's run: %+v", res)
	}
	if err := rig.collect(t, req.RunID); err != nil {
		t.Fatalf("collecting the run of %s: %v", req.RunID, err)
	}
	pending := e.sandbox.Load().pending
	if row, found, err := pending.Get(t.Context(), req.RunID); err != nil || (found && row.Ending == nil) {
		t.Fatalf("the run of %s is still on the record after its resume, its ending undecided (%+v, %v): "+
			"the turn did not end", req.RunID, row.Status, err)
	}
	eventually(t, "the ending of the run of "+req.RunID+" to record its turn's end and delete its record",
		func() bool {
			_, found, err := pending.Get(t.Context(), req.RunID)
			return err == nil && !found
		})
}

// modelPlayer plays a run as its harness would, with pick as its model: the
// bridge's tools are listed over a real MCP session to the run's endpoint, and
// pick chooses the run for them as a [phasedHarness] does for the engine's
// phases. Each response's calls go over that session in order, landing on the
// run's row as a box's would; the run ends after its last response or a
// successful submit. Its closing text is the report and its responses' cost,
// cache included, the spend. An unreachable bridge or a failed script is the
// run's failure, as a harness that could not start or was refused.
func modelPlayer(t *testing.T, pick func(offered []string) llmtest.Run) fakePlayer {
	t.Helper()
	client := httpxtest.Pool(t)
	return func(ctx context.Context, req sandbox.RunRequest) sandbox.Result {
		endpoint := req.MCPServers[config.ToolServerName].URL
		if endpoint == "" {
			return sandbox.Result{Error: "the run was handed no bridge to dial"}
		}
		session, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}, nil)
		if err != nil {
			return sandbox.Result{Error: "connecting to the bridge: " + err.Error()}
		}
		defer func() { _ = session.Close() }()
		listed, err := session.ListTools(ctx, nil)
		if err != nil {
			return sandbox.Result{Error: "listing the bridge's tools: " + err.Error()}
		}
		offered := make([]string, 0, len(listed.Tools))
		for _, tool := range listed.Tools {
			offered = append(offered, tool.Name)
		}
		run := pick(offered)
		var spent llm.Usage
		billed := func(r sandbox.Result) sandbox.Result {
			r.InputTokens, r.OutputTokens = spent.Input, spent.Output
			r.CacheReadTokens, r.CacheWriteTokens = spent.CacheRead, spent.CacheWrite
			return r
		}
		var text string
		for _, response := range run.Responses {
			usage := response.Usage
			if usage == (llm.Usage{}) {
				usage = llmtest.DefaultUsage
			}
			spent.Input += usage.Input
			spent.Output += usage.Output
			spent.CacheRead += usage.CacheRead
			spent.CacheWrite += usage.CacheWrite
			text = cmp.Or(response.Text, text)
			submitted := false
			for _, call := range response.Calls {
				if _, failed := callBridged(ctx, session, call); call.Name == runner.SubmitWorkTool && !failed {
					submitted = true
				}
			}
			if submitted {
				return billed(sandbox.Result{Success: true, Text: cmp.Or(text, "submitted")})
			}
		}
		if run.Err != nil {
			return billed(sandbox.Result{Error: run.Err.Error()})
		}
		return billed(sandbox.Result{Success: true, Text: cmp.Or(text, "done")})
	}
}

// callBridged makes one scripted call over the run's bridge session and
// returns the tool's text and whether it failed. Unparseable arguments fail
// before sending, as the harness's own client would.
func callBridged(ctx context.Context, session *mcp.ClientSession, call llmtest.Call) (string, bool) {
	var args map[string]any
	if err := json.Unmarshal([]byte(cmp.Or(call.Args, "{}")), &args); err != nil {
		return "the call's arguments do not parse: " + err.Error(), true
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.Name, Arguments: args})
	if err != nil {
		return err.Error(), true
	}
	var text []string
	for _, content := range res.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			text = append(text, tc.Text)
		}
	}
	return strings.Join(text, "\n"), res.IsError
}
