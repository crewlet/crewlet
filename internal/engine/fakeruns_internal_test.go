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
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE EXECUTOR, AS A TEST RUNS IT.
//
// Every seat's executor is its coding CLI's own run in a box, so a case that
// drives a turn has to give it one: a chain member that can take a launch, a
// catalogue to place it in, a bridge its box can dial, and something that plays
// the run. These are those, built the way `providers.sandbox.fake` builds them
// in a deployment — the in-process double for every cell, the double's runner
// for every CLI — over the REAL launch, the real bridge and the real
// coordinator, so a case that drives a turn exercises the path every turn
// takes: the launch, the park, the bridged calls landing on the run's row, the
// collection and the resume.
//
// What a run DOES is the case's: a [fakePlayer]. [modelPlayer] is the usual one
// — the pick of the case's scripted CLI ([phasedCLI]), played as the run,
// calling the seat's tools over the bridge — so one fixture scripts the run
// and the reviewer that judges it alike.

// runnerKey is the providers.llm key a rigged seat's executor launches on.
const runnerKey = "cli"

// runnerEntry is a claude-code entry an executor can launch on and that no
// phase on this host can ever reach: its binary does not exist. The run itself is played by
// the rig's double, so the CLI is never started — and a phase that fell
// through to this entry by mistake fails on the missing binary rather than
// starting a real CLI on the machine running the suite.
func runnerEntry(t *testing.T) phase.Entry {
	t.Helper()
	provider, err := cliagent.New(cliagent.Config{
		Key: runnerKey, Agent: "claude-code", Model: "sonnet",
		StateDir:  t.TempDir(),
		Overrides: map[string]any{"binary": filepath.Join(t.TempDir(), "no-cli-here")},
		Timeout:   time.Minute,
		Auth:      cliagent.Auth{Mode: cliagent.AuthAPIKey, APIKey: "sk-ant-not-real"},
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	return phase.Entry{Key: runnerKey, Provider: provider}
}

// runnable is a model registry over entries with the runner entry appended
// AFTER them, and every seat's `llm` ending on it: each seat's phases on this
// host are still answered by the case's own model — the head of every chain — and its
// executor launches on the one member that has an in-box runner, which the
// launch's pick reaches by passing over the head.
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

// rigRuns gives e an executor runtime whose every run play plays: a bridge
// mounted on a listener of its own, the double for every cell and every CLI,
// and the engine's own coordinator over a run store on e's fleet — and its
// company the fake catalogue the launch checks the epoch for.
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
	// BOTH OF THE BRIDGE'S ROUTES, as the engine's own listener mounts them
	// (internal/api, mountBridge): a run's tools, and its notes route.
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
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
		// THE SEAL EVERY ENGINE `crewlet run` builds holds, over its
		// keyring ([Engine.answers]): an engine a case builds by hand has
		// none, and its coordinator would refuse to be built without one.
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
		// THE EPOCH'S OWN CATALOGUE, as an apply pins it
		// ([Company.catalogue]): the launches of its turns place their
		// boxes by this manager.
		c.catalogue = manager
	}
	rig := &runRig{engine: e, bridge: bridge, runners: map[string]*sandbox.FakeRunner{}}
	for name, r := range runners {
		rig.runners[name] = r.(*sandbox.FakeRunner)
	}
	return rig
}

// started is every run the rig's runners were handed, in launch order per CLI.
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

// collect delivers the completion of turnID's run the way the waiter's poll
// announces one, to the coordinator that claims it: the run is played if it
// has not been, collected, charged, and its turn resumed — the reviewer and
// everything after the run, to the turn's end or its next launch. The error
// is the coordinator's, which a control-topic delivery would be NAKed on.
func (r *runRig) collect(t *testing.T, turnID string) error {
	t.Helper()
	row := r.row(t, turnID)
	if row.Status != sandbox.StatusRunning {
		t.Fatalf("run %s is %s, want it running: the turn did not park on it", turnID, row.Status)
	}
	// POLLED FIRST, as the waiter's tick polls a running job before it
	// announces the completion: the run does its work — its bridged calls
	// landing on its row — before the completion's claim reads the row.
	runner, ok := r.runners[row.CodingAgent]
	if !ok {
		t.Fatalf("run %s is on %q, which the rig has no runner for", turnID, row.CodingAgent)
	}
	if state, err := runner.Poll(t.Context(), nil, sandbox.RunHandle{CommandID: row.CommandID}); err != nil ||
		state != sandbox.JobEnded {
		t.Fatalf("polling run %s: %v, %v; want it ended", turnID, state, err)
	}
	row = r.row(t, turnID)
	completion := types.SandboxRunCompleted{
		Agent: row.AgentID, AgentHandle: row.AgentHandle, RoleName: row.Role,
		TurnID: row.TurnID, WorkKey: row.WorkKey, LaunchID: row.LaunchID,
		SandboxID: row.SandboxID, CodingAgent: row.CodingAgent,
	}
	ev := events.New(completion, events.TraceContext{TraceID: row.TraceID, ParentSpanID: row.SpanID})
	ev.Source = row.Role
	return r.engine.sandbox.Load().coordinator.OnCompleted(t.Context(), completion, ev)
}

// runToItsEnd runs req's turn to its end through its executor's run: the
// dispatch parks on the launch, the run is played and collected, and the
// resume reviews it — failing the case wherever the turn did not take that
// path.
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
	if row, found, err := e.sandbox.Load().pending.Get(t.Context(), req.RunID); err != nil || found {
		t.Fatalf("the run of %s is still on the record after its resume (%+v, %v): the "+
			"turn did not end", req.RunID, row.Status, err)
	}
}

// modelPlayer plays a run as its coding CLI would, with pick as the CLI's
// model: the seat's tools are the bridge's, listed over a real MCP session to
// the run's own endpoint, and the run is the one pick chooses for the names
// they offer — the pick a [phasedCLI] makes for a phase of the engine's own,
// so a fixture scripts the run and the phases around it the same way. Each response's calls are made over that
// session in order, so every call lands on the run's row exactly as a box's
// would, and the run ends after its last response, or once it has submitted
// its work. Its closing text is the run's report, and what its responses cost
// — the cached share included — is the run's spend, as a CLI reports its own.
//
// A failure to reach the bridge, or a run the script fails, is the run's
// failure, with the reason, as a CLI that could not start or whose vendor
// refused it.
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

// callBridged makes one of a script's calls over the run's bridge session and
// answers what the model reads back: the tool's text, and whether it failed.
// Arguments that do not parse fail the call as the CLI's own client would,
// before anything is sent.
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
