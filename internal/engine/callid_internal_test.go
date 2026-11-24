package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/notify"
)

// A BRIDGED CALL CARRIES ITS HARNESS'S ID FROM ITS ENTRY TO ITS RECORD. A box's
// harness gives each call an id in the request's `_meta` under the key its
// runner declares: the launch names the job with that key, the bridge's
// session reads each call's id under it, and the run's log carries the id on
// the in-flight entry and on the finished call, through the real launcher,
// bridge, ledger and store. Under a key no profile names, beside Claude
// Code's under its own, so the id logged is the runner's key's and no other.
//
// Mutation: name the job without its runner's key (launch-names-no-key), and
// the call is on its run's log with no id.
func TestABridgedCallCarriesItsHarnessIDFromItsEntryToItsRecord(t *testing.T) {
	t.Parallel()
	const (
		turnID = "t-call-id"
		key    = "box/callId"
		id     = "call_0a1b2c"
	)
	read := newHeldTool("read_page")
	rig := bridgedRig(t, turnID, read)
	rig.jobs.CallIDKey = key
	run := rig.launch(t, nil)
	// FREED FIRST of everything the case built, so no close waits on it.
	var freed sync.Once
	free := func() { freed.Do(func() { close(read.release) }) }
	t.Cleanup(free)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		_, err := run.session.CallTool(ctx, &mcp.CallToolParams{Name: "read_page", Arguments: map[string]any{},
			Meta: mcp.Meta{key: id, "claudecode/toolUseId": "toolu_01notthisrunnerskey"}})
		answered <- err
	}()
	select {
	case <-read.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached its tool")
	}
	if inFlight := run.calls(t, turnID).InFlight; len(inFlight) != 1 || inFlight[0].Name != "read_page" ||
		inFlight[0].CallID != id {
		t.Fatalf("as its tool runs, the run's log holds %+v in flight, want the call's entry under the id its "+
			"harness gave it, %q", inFlight, id)
	}
	free()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the call: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the call was never answered")
	}
	if logged := run.calls(t, turnID); len(logged.InFlight) != 0 || len(logged.Calls) != 1 ||
		logged.Calls[0].CallID != id {
		t.Errorf("once the call returned, the run's log holds %+v in flight and %+v, want the finished call "+
			"alone, under the id its harness gave it, %q", logged.InFlight, logged.Calls, id)
	}
}

// A RECOVERED RUN'S CALLS CARRY THE IDS THEIR HARNESS GAVE THEM. A node taking
// over a running job's seat reopens the job's session and reads each later
// call under the key THIS NODE'S runner of the harness declares: the launch's
// session was another node's (or this one's before a restart), and the run's
// row records no key. Launched under a runner that declared none and
// recovered once it declares one, so the id logged is the recovery's reading.
//
// Mutation: open the recovered job's session naming no key
// (reopen-names-no-key), and the call the box makes after the recovery is on
// the run's log with no id.
func TestARecoveredRunsCallsCarryTheIDsTheirHarnessGaveThem(t *testing.T) {
	t.Parallel()
	const id = "toolu_01madeafterrecovery"
	e, _ := indicating(t, notify.StatusAlways)
	e.Company().Config.TurnEngine.DelegationDepthLimit = 3
	rig := rigRuns(t, e, nil)
	endpoint := parkOnARun(t, e, rig, "run-call-id")
	row := rig.row(t, "run-call-id")
	boxRunner, ok := rig.runners[row.Harness]
	if !ok {
		t.Fatalf("the run's harness %q has no runner on this node", row.Harness)
	}
	boxRunner.CallIDKey = "claudecode/toolUseId"

	dialed := restartBridge(t, e, endpoint)
	e.reopenAgentRun(t.Context(), row)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: dialed, HTTPClient: httpxtest.Pool(t)}, nil)
	if err != nil {
		t.Fatalf("the box could not reach its tools after the recovery: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	called, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: runner.SubmitWorkTool, Arguments: map[string]any{
			"outcome": "blocked", "summary": "nothing to do here",
			"evidence": "this seat holds no write tool",
		},
		Meta: mcp.Meta{"claudecode/toolUseId": id},
	})
	if err != nil || called.IsError {
		t.Fatalf("submit_work after the recovery = %+v, %v", called, err)
	}
	if logged := rig.calls(t, "run-call-id"); len(logged.Calls) != 1 || logged.Calls[0].CallID != id {
		t.Errorf("the run's log holds %+v, want the submission made after the recovery under the id its "+
			"harness gave it, %q", logged.Calls, id)
	}
}

// A RECOVERED RUN'S CALLS ARE PARKED BEFORE ITS HARNESS'S CEILING, the one
// THIS NODE'S runner of the harness states ([sandbox.OpenedJob.CallCeiling]):
// the row records none, and a call the reopened session never parks is cut by
// the harness's own timeout and sent again, the double run the ceiling stops.
// Launched under a runner that stated none and recovered once it states one,
// so the park is the recovery's: the call is answered to end the turn while
// its tool runs, and the job is parked on it.
//
// Mutation: open the recovered job's session with no ceiling
// (reopen-names-no-ceiling), and the call is not answered until its tool
// returns.
func TestARecoveredRunsCallsAreParkedBeforeItsHarnesssCeiling(t *testing.T) {
	t.Parallel()
	const ceiling = 6 * time.Second
	read := newHeldTool("read_page")
	run := launchRecoverable(t, read)
	row := run.row(t)
	if row.Harness != run.jobs.Name() {
		t.Fatalf("the run's harness is %q, want the rig's runner's %q", row.Harness, run.jobs.Name())
	}
	run.jobs.Ceiling = ceiling

	run.restart()
	run.e.reopenAgentRun(t.Context(), row)
	session, err := connect(t, run.server)
	if err != nil {
		t.Fatalf("the box's endpoint for the probe's server does not reach the recovered run: %v", err)
	}
	// FREED FIRST of everything the case built, so no close waits on it.
	var freed sync.Once
	free := func() { freed.Do(func() { close(read.release) }) }
	t.Cleanup(free)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	answered := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "read_page", Arguments: map[string]any{}})
		if err != nil {
			res = &mcp.CallToolResult{IsError: true}
		}
		answered <- res
	}()
	select {
	case <-read.entered:
	case <-ctx.Done():
		t.Fatal("the call after the recovery never reached its tool")
	}
	select {
	case res := <-answered:
		if res.IsError || !strings.Contains(contentText(res), "END YOUR TURN NOW") {
			t.Fatalf("the call after the recovery was answered %+v while its tool ran, want it still "+
				"running and the turn to end", res)
		}
	case <-ctx.Done():
		t.Fatalf("the call after the recovery was not answered while its tool ran, %v before its "+
			"harness's ceiling of %v", ceiling-toolloop.ParkBefore(ceiling), ceiling)
	}
	if parked := run.row(t); !parked.WaitsOnOwnCalls() || len(parked.ParkedCalls) != 1 ||
		parked.ParkedCalls[0].Tool != "read_page" {
		t.Errorf("the recovered run's row parks on %+v, want its job parked on its call to read_page",
			parked.ParkedCalls)
	}
}
