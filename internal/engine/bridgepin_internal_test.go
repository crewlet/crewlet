package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/runtoken"
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

// bridgedRun is an executor run launched through the real launcher onto a
// bridge a test can dial: the engine, the run's row store, and the client
// session its box would hold — and the launcher, the surface and the jobs a
// relaunch of the same turn goes through again ([bridgedRun.launch]).
type bridgedRun struct {
	engine  *Engine
	handle  string
	pending sandbox.PendingStore
	session *mcp.ClientSession

	launcher *agentLauncher
	surface  *tools.Surface
	jobs     *sandbox.FakeRunner
}

// launchBridged launches turnID's executor over a surface offering the given
// tools, and dials its bridge the way the box's own coding agent would.
func launchBridged(t *testing.T, turnID string, offered ...tools.Callable) bridgedRun {
	t.Helper()
	c, seat := executorCompany(t)
	return launchBridgedOn(t, c, launcherOn(nil, c, seat, turnID).turn, offered...)
}

// launchBridgedOn is [launchBridged] for turn, a turn of c's seat, as the
// engine builds the turn it launches — its holds included, where a case gives
// it some.
func launchBridgedOn(t *testing.T, c *Company, turn *turnctx.Turn, offered ...tools.Callable) bridgedRun {
	t.Helper()
	seat := turn.Seat
	if turn.Org == nil {
		turn.Org = c.Org
	}
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: server.URL,
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	server.Config.Handler = mux

	e, jobs := launchRig(t, c, bridge)
	reg := tools.NewRegistry()
	var names []string
	for _, tool := range offered {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", tool.Name(), err)
		}
		names = append(names, tool.Name())
	}
	launcher := &agentLauncher{engine: e, company: c, seat: seat, turn: turn}
	run := bridgedRun{engine: e, handle: seat.Handle(), pending: e.sandbox.Load().pending,
		launcher: launcher, jobs: jobs,
		// BOUND TO THE LAUNCHER'S OWN TURN, as the runner binds every
		// phase surface: the bridge acts as that turn's seat on every call
		// and refuses a surface that names none.
		surface: tools.NewSurface("execute", reg.Snapshot(), names).ForTurn(launcher.turn)}
	return run.launch(t, nil)
}

// launch launches the run's executor job — the turn's next job when one has
// run, carrying on the one that asked a person where cont is set — and dials
// the bridge of the job it started, the way that job's coding agent would.
func (b bridgedRun) launch(t *testing.T, cont *runner.Continuation) bridgedRun {
	t.Helper()
	before := len(b.jobs.Started())
	if _, err := b.launcher.LaunchExecutor(t.Context(), runner.AgentRunRequest{
		Brief: "fix the failing test", Round: 1,
		Surface:    b.surface,
		OnFallback: func(chain.Fallback) {},
		Fence:      b.engine.seatFence(b.handle),
		Continue:   cont,
	}); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	started := b.jobs.Started()
	if len(started) != before+1 {
		t.Fatalf("%d jobs started, want %d", len(started), before+1)
	}
	endpoint := started[before].MCPServers[config.ToolServerName].URL
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
	b.session = session
	return b
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

// A LAUNCHED RUN'S CALLS STOP WHEN A PERSON STOPS ITS SEAT. The run outlives
// the turn that launched it, so the seat's fence is handed to its session and
// asked before every call the box makes: once a person's pause asks the seat's
// running work to stop, a call is refused rather than made — through the real
// launcher and the real bridge, because the fence is one field of wiring in
// LaunchExecutor and nothing else can show it was set. The refused call is on
// the run's record in its place, failed and never started, so a reader of the
// run sees the box asked after the stop and was turned away.
//
// Mutation: drop the Fence from the session LaunchExecutor opens, and the
// stopped seat's call runs.
func TestALaunchedRunsCallsStopWhenAPersonStopsItsSeat(t *testing.T) {
	t.Parallel()
	read := newHeldTool("read_page")
	close(read.release)
	run := launchBridged(t, "t-fenced", read)
	run.call(t, "read_page")

	seat, err := run.engine.seatID(run.handle)
	if err != nil {
		t.Fatalf("seatID: %v", err)
	}
	run.engine.pauses.mu.Lock()
	if run.engine.pauses.pauses == nil {
		run.engine.pauses.pauses = map[uuid.UUID]coord.SeatPause{}
	}
	run.engine.pauses.pauses[seat] = coord.SeatPause{Seat: seat, By: "ana", StopRunning: true}
	run.engine.pauses.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := run.session.CallTool(ctx, &mcp.CallToolParams{Name: "read_page",
		Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("a call ran on a seat a person had asked to stop")
	}
	calls := run.row(t, "t-fenced").BridgeCalls
	if len(calls) != 2 {
		t.Fatalf("the run recorded %d calls, want the one made before the stop and the one refused after it", len(calls))
	}
	if made := calls[0]; made.Failed || made.StartedAt.IsZero() {
		t.Errorf("the call before the stop is recorded %+v, want it run", made)
	}
	if refused := calls[1]; refused.Name != "read_page" || !refused.Failed || !refused.StartedAt.IsZero() {
		t.Errorf("the call after the stop is recorded %+v, want it refused and never started", refused)
	}
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

// A RUN CARRIED ON AFTER A PERSON'S ANSWER NUMBERS ITS CALLS AFTER THE ASKING
// RUN'S. The row keeps the asking job's calls as the round's, and the
// collection that closes the round reads them by their place: a continuation
// whose bridge session counted from one again put its first call beside the
// asking run's first, and every reader of the round — its record, the log its
// reviewer reads, its submissions, where the last one made wins — had the two
// runs' calls interleaved. Driven through the real launcher, store and bridge,
// because the number reaches the session through the launch's own wiring.
//
// Mutation: open the job's session without the LastSeq the launch was told,
// and the carried-on call is numbered 1.
func TestACarriedOnRunsCallsAreNumberedAfterTheAskingRuns(t *testing.T) {
	t.Parallel()
	read := newHeldTool("read_page")
	close(read.release)
	asking := launchBridged(t, "t-carried", read)
	asking.call(t, "read_page")
	asking.call(t, "read_page")

	carried := asking.launch(t, &runner.Continuation{SessionID: "sess-ask", Brief: "use main"})
	carried.call(t, "read_page")

	row := carried.row(t, "t-carried")
	var seqs []int
	for _, call := range row.BridgeCalls {
		seqs = append(seqs, call.Seq)
	}
	if !slices.Equal(seqs, []int{1, 2, 3}) {
		t.Fatalf("the round's calls are numbered %v, want the carried-on run's after the "+
			"asking run's two: [1 2 3]", seqs)
	}

	// A NEW ROUND'S RUN COUNTS FROM ONE: its row starts the log afresh.
	next := carried.launch(t, nil)
	next.call(t, "read_page")
	if calls := next.row(t, "t-carried").BridgeCalls; len(calls) != 1 || calls[0].Seq != 1 {
		t.Fatalf("the next round's log is %+v, want its one call numbered 1", calls)
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
	if err != nil || next.LaunchID == first {
		t.Fatalf("relaunch = %q, %v (first job %q)", next.LaunchID, err, first)
	}
	close(delegate.release)
	<-answered

	row := run.row(t, "t-late")
	if row.LaunchID != next.LaunchID {
		t.Fatalf("the row holds job %q, want the relaunch's %q", row.LaunchID, next.LaunchID)
	}
	if len(row.BridgeCalls) != 0 || row.Launch.Bridged != (sandbox.EngineSpend{}) {
		t.Fatalf("the next job took the last one's late call: calls %+v, spend %+v",
			row.BridgeCalls, row.Launch.Bridged)
	}
}
