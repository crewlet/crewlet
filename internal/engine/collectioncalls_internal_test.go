package engine

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// filingServer is the MCP server the filing tool is a tool of. NOT A BUILTIN:
// a builtin with no authority-table row of its own is left off a seat's
// surface ([Company.withheldFrom]); an MCP server's tool reaches the executor
// under its server's name (mcpbridge.Servers).
const filingServer = "tracker"

// filingTool is a seat's tool that files a work item once it is released,
// committing a work item to its turn's record of changes as the tracker's own
// tool does, and says so on entered as it starts.
type filingTool struct {
	entered chan struct{}
	release chan struct{}
}

func (f *filingTool) Name() string               { return "file_work_item" }
func (f *filingTool) Description() string        { return "files a work item once released" }
func (f *filingTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (f *filingTool) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return f.CallInTurn(ctx, nil, args)
}

func (f *filingTool) CallInTurn(ctx context.Context, turn *turnctx.Turn, _ map[string]any) (tools.Result, error) {
	select {
	case f.entered <- struct{}{}:
	default:
	}
	select {
	case <-f.release:
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
	if turn != nil {
		turn.Written.Commit(types.CommittedItem)
	}
	return tools.Result{Output: "filed"}, nil
}

// A CALL STILL RUNNING AS ITS HARNESS ENDS ITS RUN COUNTS IN THE TURN'S OUTCOME,
// end to end over the real launch, bridge, run store and coordinator. The run
// calls a tool that files a work item and ends while the call runs, as Claude
// Code does past its tool timeout; the tool is let go only once the collection
// ends the job's session, so its update lands while the collection waits, and
// the outcome counts the item.
//
// Mutation: collect without waiting for the calls in flight
// (collection-reads-without-the-drain), and the work item is in no outcome:
// the turn is booked as one that changed nothing.
func TestACallStillRunningAsItsHarnessEndsItsRunCountsInTheTurnsOutcome(t *testing.T) {
	t.Parallel()
	// A MODEL THAT ANSWERS THE TURN'S OWN PHASES, its reviewer's among them:
	// the run is collected, resumed and reviewed, and the turn ends.
	e, _ := indicatingWith(t, notify.StatusAlways, scripted{})
	e.Company().Config.TurnEngine.DelegationDepthLimit = 3
	tool := &filingTool{entered: make(chan struct{}, 1), release: make(chan struct{})}
	if err := e.Company().Tools.Register(tool, tools.Origin(filingServer)); err != nil {
		t.Fatalf("registering the filing tool: %v", err)
	}
	answered := make(chan struct{})
	client := httpxtest.Pool(t)
	play := func(ctx context.Context, req sandbox.RunRequest) sandbox.Result {
		endpoint := req.MCPServers[filingServer].URL
		if endpoint == "" {
			return sandbox.Result{Error: "the run was handed no endpoint for the filing tool's server"}
		}
		session, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}, nil)
		if err != nil {
			return sandbox.Result{Error: "connecting to the bridge: " + err.Error()}
		}
		// THE CALL ITS HARNESS LEAVES RUNNING, answered only once its job's
		// session ends.
		go func() {
			defer close(answered)
			defer func() { _ = session.Close() }()
			_, _ = session.CallTool(context.WithoutCancel(ctx), &mcp.CallToolParams{Name: tool.Name(),
				Arguments: map[string]any{}})
		}()
		select {
		case <-tool.entered:
		case <-answered:
			return sandbox.Result{Error: "the call was answered without reaching its tool"}
		case <-ctx.Done():
			return sandbox.Result{Error: "the call never reached its tool"}
		}
		return sandbox.Result{Text: "filed the item", Success: true}
	}
	// THE TOOL IS LET GO once the box's call was answered, which the end of
	// the job's session is: after the collection began.
	go func() {
		<-answered
		close(tool.release)
	}()
	rig := rigRuns(t, e, play)

	runToItsEnd(t, e, rig, Request{
		Handle: "swe", WorkKey: "wk-filed", RunID: "run-filed", WorkSince: time.Now().UTC(),
		Events: []*events.Event{chatTrigger("D0ANA")},
	})

	q, ok := e.backends.Queue.(*memory.Queue)
	if !ok {
		t.Fatal("the engine's queue is not the in-memory queue whose history a case reads")
	}
	var completed *types.TurnCompleted
	for _, ev := range q.History() {
		if ev.Type == (types.TurnCompleted{}).EventType() {
			completed, _ = events.DataAs[*types.TurnCompleted](ev)
		}
	}
	if completed == nil || completed.Turn == nil {
		t.Fatalf("no turn_completed with a tally was published: %+v", completed)
	}
	if completed.Turn.Outcome != types.OutcomeMoved {
		t.Errorf("the turn's outcome is %q, want %q: the work item the call still running as its run ended "+
			"filed is in it", completed.Turn.Outcome, types.OutcomeMoved)
	}
}

// AN ANSWERED PARK CONTINUES ON A FRESH JOB AND A FRESH SESSION, AND NO CALL OF
// THE PARKED SESSION LANDS ON IT. The park's collection ends the asking job's
// session (the coordinator's Ended, wired to the bridge's close in
// [Engine.coordinatorOptions]), so its next call is refused; the answer's
// relaunch is a job of its own in the same round, its calls numbered after
// the asking job's, and the parked session's token, dialled again, reaches
// nothing.
//
// Mutation: resolve a job's token by its run alone, and the parked session's
// call reaches the new job's opening.
func TestAnAnsweredParkContinuesOnAFreshJobAndSession(t *testing.T) {
	t.Parallel()
	read := newHeldTool("read_page")
	close(read.release)
	asking := launchBridged(t, "t-answered", read)
	asking.call(t, "read_page")
	<-read.entered
	parked := asking.row(t, "t-answered")
	parkedAt := asking.jobs.Started()[0].MCPServers[config.ToolServerName].URL

	// THE PARK'S COLLECTION ENDS THE ASKING JOB'S SESSION, as the engine
	// wires the coordinator's Ended.
	asking.engine.bridge.Close("t-answered")
	refused := func(when string, session *mcp.ClientSession) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "read_page", Arguments: map[string]any{}})
		if err == nil && !res.IsError {
			t.Errorf("%s, the parked session's call was answered %+v, want it refused", when, res.Content)
		}
		select {
		case <-read.entered:
			t.Errorf("%s, the parked session's call ran its tool", when)
		default:
		}
	}
	refused("once the park ended its session", asking.session)

	carried := asking.launch(t, &runner.Continuation{SessionID: "sess-ask", Brief: "use main"})
	next := carried.row(t, "t-answered")
	started := carried.jobs.Started()
	if next.LaunchID == parked.LaunchID || next.Round != parked.Round || len(started) != 2 ||
		started[1].MCPServers[config.ToolServerName].URL == parkedAt {
		t.Fatalf("the answer's relaunch is job %s of round %q after %d launches, want a job of its own in "+
			"round %q, dialled at an endpoint of its own", next.LaunchID, next.Round, len(started), parked.Round)
	}
	// THE PARKED SESSION'S TOKEN, dialled afresh once the new job's opening
	// is served: it resolves to nothing.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	redialled, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: parkedAt, HTTPClient: httpxtest.Pool(t)}, nil)
	if err == nil {
		t.Cleanup(func() { _ = redialled.Close() })
		refused("once the answer's relaunch opened its own", redialled)
	}

	carried.call(t, "read_page")
	<-read.entered
	logged := carried.calls(t, "t-answered")
	if len(logged.Calls) != 2 || logged.Calls[0].Seq != 1 || logged.Calls[1].Seq != 2 ||
		logged.Calls[0].Late || logged.Calls[1].Late || len(logged.InFlight) != 0 {
		t.Errorf("the round's log lists %+v (in flight %+v), want the asking job's call and the new job's "+
			"after it, neither late, and nothing of the parked session's", logged.Calls, logged.InFlight)
	}
}
