package engine

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// bridgeRunOn opens one agent-mode run on a fresh store and returns both.
func bridgeRunOn(t *testing.T, turnID string) (*sandbox.CoordStore, *Engine) {
	t.Helper()
	store := sandbox.NewCoordStore(coordmemory.NewFleet())
	if err := store.BeginLaunch(t.Context(), sandbox.PendingRun{TurnID: turnID, AgentHandle: "swe"},
		sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	return store, &Engine{sandboxPending: store}
}

// resumedCalls is what a resume of the run hands its rebuilt phase.
func resumedCalls(t *testing.T, e *Engine, store *sandbox.CoordStore, turnID string) []bridgedCall {
	t.Helper()
	run, _, err := store.Get(t.Context(), turnID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	calls, _, err := e.resumeBridged(t.Context(), resumeInput{
		Run: run, State: execstate.State{Version: execstate.Version, AgentRun: true, Round: 1},
	})
	if err != nil {
		t.Fatalf("resumeBridged: %v", err)
	}
	out := make([]bridgedCall, 0, len(calls))
	for _, call := range calls {
		out = append(out, bridgedCall{name: call.Name, args: call.Args, result: call.Result})
	}
	return out
}

type bridgedCall struct {
	name   string
	args   map[string]any
	result string
}

// A CALL TOO LARGE FOR ITS RECORD REACHES THE RESUMED PHASE WHOLE.
//
// The resumed phase's record is the engine's durable account of what the run
// did and was told, and the run's end purges the log it was read from. A call
// handed over in its record's cut form would be cut for good; handed over
// whole, from the parts filed under its record, it is the tool's whole output
// and the whole arguments it was called with.
func TestTheResumeReadsACallTooLargeForItsRecordWhole(t *testing.T) {
	t.Parallel()
	store, e := bridgeRunOn(t, "t-whole")
	body := strings.Repeat("b", sandbox.MaxBridgeCallBytes)
	output := strings.Repeat("o", sandbox.MaxBridgeCallBytes*2)
	if ok, err := store.AppendBridgeCall(t.Context(), "t-whole", sandbox.BridgeCall{
		Name: "create_page", Args: `{"body":"` + body + `"}`, Output: output,
	}); err != nil || !ok {
		t.Fatalf("AppendBridgeCall = %v, %v", ok, err)
	}
	calls := resumedCalls(t, e, store, "t-whole")
	if len(calls) != 1 {
		t.Fatalf("the resume read %d calls, want the one", len(calls))
	}
	if calls[0].result != output {
		t.Errorf("the resumed call's result is %d bytes, want the %d the tool returned",
			len(calls[0].result), len(output))
	}
	if got, _ := calls[0].args["body"].(string); got != body {
		t.Errorf("the resumed call's body argument is %d bytes, want the %d it was called with",
			len(got), len(body))
	}
}

// A NINETEEN-DIGIT ID IN A BRIDGED CALL'S ARGUMENTS COMES BACK EXACT.
//
// The bridge decodes a box's arguments with json.Number and records them as
// text, and the resume decodes that text for the phase's record, its review
// and a replayed submission. Decoded as float64, 1234567890123456789 comes
// back as 1234567890123456800: a record naming a different entity from the
// one the tool was called on, with nothing anywhere reporting an error.
func TestAResumedCallsLargeIDIsExact(t *testing.T) {
	t.Parallel()
	store, e := bridgeRunOn(t, "t-id")
	if ok, err := store.AppendBridgeCall(t.Context(), "t-id", sandbox.BridgeCall{
		Name: "get_issue", Args: `{"issue_id":1234567890123456789}`,
	}); err != nil || !ok {
		t.Fatalf("AppendBridgeCall = %v, %v", ok, err)
	}
	id := resumedCalls(t, e, store, "t-id")[0].args["issue_id"]
	if n, ok := id.(json.Number); !ok || n.String() != "1234567890123456789" {
		t.Errorf("issue_id = %v (%T), want the exact 1234567890123456789", id, id)
	}
}

// AN ARGUMENT THAT CANNOT BE WRITTEN AS JSON IS AN ERROR, NOT "NONE".
//
// Recorded as empty text, the call would read as one made with no arguments —
// a statement about the call rather than a note that something went wrong. The
// append fails instead, and the bridge's own log line for a failed append says
// so; nothing is recorded that claims otherwise.
func TestAnUnencodableArgumentIsRefusedRatherThanRecordedAsNone(t *testing.T) {
	t.Parallel()
	store, _ := bridgeRunOn(t, "t-nan")
	err := bridgeLedger{store: store}.Append(t.Context(), "t-nan", tools.Call{
		Name: "set_score", Args: map[string]any{"score": math.NaN()},
	})
	if err == nil {
		t.Fatal("a call whose arguments are not JSON was recorded")
	}
	run, _, getErr := store.Get(t.Context(), "t-nan")
	if getErr != nil {
		t.Fatal(getErr)
	}
	logged, logErr := store.BridgeCalls(t.Context(), run)
	if logErr != nil || len(logged.Calls) != 0 {
		t.Errorf("the log holds %d calls, %v; want none recorded", len(logged.Calls), logErr)
	}
}
