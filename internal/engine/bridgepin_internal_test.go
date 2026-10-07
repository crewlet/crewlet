package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// heldTool is a tool that answers only once released, so a test can hold a
// bridged call in flight across something else happening to its run.
type heldTool struct {
	name    string
	entered chan struct{}
	release chan struct{}
}

func newHeldTool(name string) *heldTool {
	return &heldTool{name: name, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (h *heldTool) Name() string               { return h.name }
func (h *heldTool) Description() string        { return h.name + " waits to be released" }
func (h *heldTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (h *heldTool) Call(ctx context.Context, _ map[string]any) (tools.Result, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	return tools.Result{Output: h.name + " done"}, nil
}

// bridgedRun is an agent-mode run launched through the real launcher onto a
// bridge a test can dial: the engine, the run's row store, and the client
// session its box would hold.
type bridgedRun struct {
	pending sandbox.PendingStore
	session *mcp.ClientSession
}

// launchBridged launches turnID's executor as an agent-mode run over a surface
// offering the given tools, and dials its bridge the way the box's own coding
// agent would.
func launchBridged(t *testing.T, turnID string, offered ...tools.Callable) bridgedRun {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge := mcpbridge.New(mcpbridge.Options{Key: []byte("test-key"), BaseURL: server.URL})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	server.Config.Handler = mux

	c, seat := splitLoginCompany(t, "api", "codex")
	e, jobs := launchRig(t, c, bridge)
	reg := tools.NewRegistry()
	var names []string
	for _, tool := range offered {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", tool.Name(), err)
		}
		names = append(names, tool.Name())
	}
	launcher := &agentLauncher{
		engine: e, turn: &turnctx.Turn{RunID: turnID, Seat: seat}, seat: seat,
		codingAgent: "claude-code", placement: sandbox.E2B,
	}
	if err := launcher.LaunchExecutor(t.Context(), runner.AgentRunRequest{
		Brief: "fix the failing test", Round: 1,
		Surface: tools.NewSurface("execute", reg.Snapshot(), names),
	}); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	started := jobs.Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want 1", len(started))
	}
	endpoint := started[0].MCPServers[config.BridgeServerName].URL
	if endpoint == "" {
		t.Fatal("the job was handed no bridge to dial")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: httpxtest.Pool(t),
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return bridgedRun{pending: e.sandbox.Load().pending, session: session}
}

// call makes one bridged call and waits for its answer.
func (b bridgedRun) call(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := b.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}}); err != nil {
		t.Errorf("CallTool(%s): %v", name, err)
	}
}

func (b bridgedRun) row(t *testing.T, turnID string) sandbox.PendingRun {
	t.Helper()
	row, found, err := b.pending.Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("the run's row: found %v, %v", found, err)
	}
	return row
}

// A LAUNCH BINDS ITS BRIDGE SESSION TO ITS JOB, so the box's calls are filed
// under it. Driven through the real launcher and the real bridge, because the
// binding is one line of wiring in LaunchExecutor and nothing else can show it
// was made.
//
// Mutation: drop the launch's Opened hook, and the call is filed under nothing.
func TestALaunchedRunsBridgedCallIsFiledUnderItsJob(t *testing.T) {
	t.Parallel()
	read := newHeldTool("read_page")
	close(read.release)
	run := launchBridged(t, "t-bound", read)
	job := run.row(t, "t-bound").LaunchID

	run.call(t, "read_page")
	row := run.row(t, "t-bound")
	if row.LaunchID != job || len(row.BridgeCalls) != 1 || row.BridgeCalls[0].Name != "read_page" {
		t.Fatalf("the job's log after one bridged call: job %q (was %q), calls %+v",
			row.LaunchID, job, row.BridgeCalls)
	}
}

// A SESSION'S FIRST CALL, OUTLIVING ITS JOB, IS NOT THE NEXT JOB'S.
//
// The case a session that learned its job from the row got wrong: the coding
// agent opens with a delegate call, which is still running engine-side when the
// box is collected and the reviewer relaunches the run — the row now holds the
// next job. A session that had not yet landed an append learned that job's
// name from the row and filed the late call, and its spend, on the next job's
// record, whose resume then paid for it as its own. Bound before its box
// existed, the session names its own job, and the store refuses it.
func TestABridgedSessionsFirstCallOutlivingItsJobIsNotTheNextJobs(t *testing.T) {
	t.Parallel()
	delegate := newHeldTool("delegate")
	run := launchBridged(t, "t-late", delegate)
	first := run.row(t, "t-late").LaunchID

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		run.call(t, "delegate")
	}()
	select {
	case <-delegate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the bridged call never reached its tool")
	}
	// THE RELAUNCH, as the row sees it: the next job opened on the turn.
	next, err := run.pending.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "t-late", AgentHandle: "swe", Role: "SWE",
	}, sandbox.Fence{})
	if err != nil || next.ID == first {
		t.Fatalf("relaunch = %q, %v (first job %q)", next.ID, err, first)
	}
	close(delegate.release)
	<-answered

	row := run.row(t, "t-late")
	if row.LaunchID != next.ID {
		t.Fatalf("the row holds job %q, want the relaunch's %q", row.LaunchID, next.ID)
	}
	if len(row.BridgeCalls) != 0 || row.LaunchFacts().Bridged != (sandbox.EngineSpend{}) {
		t.Fatalf("the next job took the last one's late call: calls %+v, spend %+v",
			row.BridgeCalls, row.LaunchFacts().Bridged)
	}
}
