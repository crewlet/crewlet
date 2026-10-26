package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// restartBridge stands in for this node's process starting again under a run
// that is still out: a fresh bridge on the same keyring holding no session,
// served on a listener of its own. It answers the URL the run's box dials once
// it is pointed at the new listener: the token, the box's half of the URL, is
// unchanged.
func restartBridge(t *testing.T, e *Engine, endpoint string) string {
	t.Helper()
	fresh := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "http://unused.example",
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", fresh.Handler())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	e.bridge = fresh
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("the run's bridge endpoint %q: %v", endpoint, err)
	}
	return server.URL + parsed.Path
}

// parkOnARun runs a chat turn on the seat to its park on its executor's run,
// and answers the endpoint the run's box was launched with.
func parkOnARun(t *testing.T, e *Engine, rig *runRig, runID string) string {
	t.Helper()
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-" + runID, WorkSince: time.Now().UTC(), RunID: runID,
		Events: []*events.Event{chatTrigger("D0ANA")},
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; the turn did not park, so this asserts nothing", res, err)
	}
	started := rig.started()
	if len(started) != 1 {
		t.Fatalf("%d runs started, want the turn's one", len(started))
	}
	return started[0].MCPServers[config.ToolServerName].URL
}

// A NODE THAT RECOVERS A RUNNING JOB SERVES ITS BOX AGAIN, FROM THE TURN IT
// PARKED. Every bridge session lives in the process that opened it, so a
// restart drops them all — while the run goes on, and its box keeps dialing
// with the token it was handed. As the node takes the seat back it rebuilds
// the run's surface from the park a real turn wrote ([Engine.reopenAgentRun],
// the coordinator's Recovered hook), opens the job again under that token, and
// the box's next call — its submit_work, here — lands on the run's row as any
// call does: refused, the run that did its work was rescued as incomplete for
// want of a submission it had made.
//
// Mutation: rebuild the parked turn with another construction than the
// resume's, so the surface loses submit_work, or open the session under
// another job, and the call is refused.
func TestARecoveredRunsBoxIsServedFromItsParkedTurn(t *testing.T) {
	t.Parallel()
	e, _ := indicating(t, notify.StatusAlways)
	e.Company().Config.TurnEngine.DelegationDepthLimit = 3
	rig := rigRuns(t, e, nil)
	endpoint := parkOnARun(t, e, rig, "run-restart")

	dialed := restartBridge(t, e, endpoint)
	e.reopenAgentRun(t.Context(), rig.row(t, "run-restart"))

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
	})
	if err != nil || called.IsError {
		t.Fatalf("submit_work after the recovery = %+v, %v", called, err)
	}
	row := rig.row(t, "run-restart")
	if n := len(row.BridgeCalls); n != 1 || row.BridgeCalls[0].Name != runner.SubmitWorkTool {
		t.Fatalf("the row holds %d bridged calls (%+v), want the submission made after the recovery",
			n, row.BridgeCalls)
	}
}

// A RUN WHOSE ENDING IS DECIDED IS NOT SERVED AGAIN. A person's stop, or a
// failure, records the run's ending before its steps close the session and
// reclaim the box; a node recovering the seat in between would hand the box
// its tools back for the moments before its reclaim, as a seat the stop said
// was done.
//
// Mutation: drop the ending check from reopenAgentRun, and the ended run's
// session is open.
func TestARunWhoseEndingIsDecidedIsNotServedAgain(t *testing.T) {
	t.Parallel()
	e, _ := indicating(t, notify.StatusAlways)
	e.Company().Config.TurnEngine.DelegationDepthLimit = 3
	rig := rigRuns(t, e, nil)
	endpoint := parkOnARun(t, e, rig, "run-ended")

	restartBridge(t, e, endpoint)
	row := rig.row(t, "run-ended")
	// THE CONTROL: the same row with no ending is served, so the refusal
	// below is the ending's and nothing else's.
	e.reopenAgentRun(t.Context(), row)
	if n := e.bridge.Live(); n != 1 {
		t.Fatalf("the control: %d sessions open for the running job, want its one", n)
	}

	restartBridge(t, e, endpoint)
	row.Ending = &sandbox.RecordedEnding{ID: "ending-1", At: time.Now().UTC(),
		Reason: "stopped", Reclaim: true}
	e.reopenAgentRun(t.Context(), row)
	if n := e.bridge.Live(); n != 0 {
		t.Errorf("%d sessions opened for a run whose ending is decided", n)
	}
}
