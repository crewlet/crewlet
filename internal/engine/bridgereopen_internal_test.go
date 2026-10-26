package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
)

// restartBridge stands in for this node's process starting again under a run
// that is still out: a fresh bridge on the same keyring holding no session,
// with the engine's own reopener, as [New] installs it — served on a listener
// of its own. It answers the URL the run's box dials once it is pointed at the
// new listener: the token, the box's half of the URL, is unchanged.
func restartBridge(t *testing.T, e *Engine, endpoint string) string {
	t.Helper()
	fresh := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "http://unused.example",
	})
	fresh.SetReopener(e.reopenBridgeSession)
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

// A NODE THAT LOST A RUNNING JOB'S SESSION SERVES ITS BOX AGAIN. Every bridge
// session lives in the process that opened it, so a restart drops them all —
// while the run goes on, and its box keeps dialing this node with the token it
// was handed. The node rebuilds the session from the run's park, under that
// token, and the box's next call — its submit_work, here — lands on the run's
// row as any call does: refused, the run that did its work was rescued as
// incomplete for want of a submission it had made.
//
// Mutation: install no reopener and the call is refused; rebuild under a fresh
// nonce, or record none on the launch, and the token the box holds names
// nothing.
func TestANodeThatLostARunsSessionServesItsBoxAgain(t *testing.T) {
	t.Parallel()
	e, _ := indicating(t, notify.StatusAlways)
	e.Company().Config.TurnEngine.DelegationDepthLimit = 3
	rig := rigRuns(t, e, nil)
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-restart", WorkSince: time.Now().UTC(), RunID: "run-restart",
		Events: []*events.Event{chatTrigger("D0ANA")},
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; the turn did not park, so this asserts nothing", res, err)
	}
	started := rig.started()
	if len(started) != 1 {
		t.Fatalf("%d runs started, want the turn's one", len(started))
	}
	endpoint := started[0].MCPServers[config.BridgeServerName].URL
	if row := rig.row(t, "run-restart"); row.BridgeNonce == "" {
		t.Fatal("the launch recorded no bridge session on its row, so no restart can rebuild it")
	}

	dialed := restartBridge(t, e, endpoint)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: dialed, HTTPClient: httpxtest.Pool(t)}, nil)
	if err != nil {
		t.Fatalf("the box could not reach its tools after the restart: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	called, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: runner.SubmitWorkTool, Arguments: map[string]any{
			"outcome": "blocked", "summary": "nothing to do here",
			"evidence": "this seat holds no write tool",
		},
	})
	if err != nil || called.IsError {
		t.Fatalf("submit_work after the restart = %+v, %v", called, err)
	}
	row := rig.row(t, "run-restart")
	if n := len(row.BridgeCalls); n != 1 || row.BridgeCalls[0].Name != runner.SubmitWorkTool {
		t.Fatalf("the row holds %d bridged calls (%+v), want the submission made after the restart",
			n, row.BridgeCalls)
	}
}

// ONLY THE JOB THE TOKEN NAMES IS REBUILT. A token naming a session a relaunch
// replaced, a run that has ended, and a node that did not launch the run are
// all refused: the first is an earlier box holding a key to the later run's
// surface, and the last is a misrouted URL whose calls would count their
// ordinals apart from the launching node's.
func TestOnlyTheRunningJobATokenNamesIsRebuilt(t *testing.T) {
	t.Parallel()
	e, _ := indicating(t, notify.StatusAlways)
	e.Company().Config.TurnEngine.DelegationDepthLimit = 3
	rig := rigRuns(t, e, nil)
	if res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-only", WorkSince: time.Now().UTC(), RunID: "run-only",
		Events: []*events.Event{chatTrigger("D0ANA")},
	}); err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; the turn did not park", res, err)
	}
	nonce := rig.row(t, "run-only").BridgeNonce
	if _, err := e.reopenBridgeSession(t.Context(), "run-only", nonce); err != nil {
		t.Fatalf("the control: the running job's own session was refused: %v", err)
	}
	for name, tc := range map[string]struct{ runID, nonce, says string }{
		"a superseded session": {"run-only", "another-nonce", "replaced"},
		"a run that has ended": {"run-gone", nonce, "ended"},
	} {
		if _, err := e.reopenBridgeSession(t.Context(), tc.runID, tc.nonce); err == nil ||
			!strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: rebuilt (%v), want a refusal saying %q", name, err, tc.says)
		}
	}
}
