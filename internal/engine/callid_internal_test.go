package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/notify"
)

// A BRIDGED CALL CARRIES ITS HARNESS'S ID FROM ITS ENTRY TO ITS RECORD. A box's
// harness hands each call it makes its own id in the request's `_meta`, under
// the key the runner that starts the box declares: the launch names the job
// with that key, the session the bridge opens for the job reads each call's id
// under it, and the run's log carries the id on the call's entry while its
// tool runs and on the finished call its one update lists — through the real
// launcher, the bridge, the session's ledger and the run's store. Under a key
// no profile names, beside Claude Code's under its own, so the id on the log
// is the one read under the runner's key and no other.
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

// A RECOVERED RUN'S CALLS CARRY THE IDS THEIR HARNESS GAVE THEM. A node that
// takes a running job's seat over opens the job's session again, its box still
// dialing with the token it was handed, and reads each call the box makes from
// then on under the key THIS NODE'S runner of the job's harness declares: the
// bridge reading the box's calls is this node's, the launch's session was
// another node's, or this one's before a restart, and the run's row records no
// key. Launched here under a runner that declared none, and recovered once it
// declares one, so the id on the log is the recovery's own reading.
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
