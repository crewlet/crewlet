package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// A CALL LANDING BEFORE A SEND-BACK'S OPENING REACHES THE SENT-BACK JOB. A
// call of the collected job still in flight as its collection read the log
// lands through its session's ledger before the send-back's opening: as that
// job's own, in its own round, after the read. The send-back's launch hands
// the store the read its briefs were built from, and the opening carries the
// call into the new round late, for the new round's first job, whose next tool
// batch is answered it under the engine's heading for a late call.
//
// Mutation: carry nothing into a new round, and the sent-back job is told
// nothing.
func TestACallLandingBeforeASendBacksOpeningReachesTheSentBackJob(t *testing.T) {
	t.Parallel()
	const turn = "run-carried"
	fleet := coordmem.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	runs, _ := steersOver(t, steerRing, store)
	launch := sandbox.PendingRun{TurnID: turn, AgentID: sweAgent, AgentHandle: "swe", Role: "SWE",
		Harness: "claude-code", TraceID: "tr-1", CreatedAt: time.Now().UTC(),
		Steer: sandbox.RunSteer{Seat: runs.anchor(turn, sweAgent, "swe")}}
	first, err := store.BeginLaunch(t.Context(), launch, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	ledger := &bridgeLedger{store: store, spend: &steppingMeter{}}
	ledger.bind(first.NamedJob(first.OpenedAfter, ""))
	record := func(seq int, call tools.Call) {
		t.Helper()
		if err := ledger.Record(t.Context(), turn, mcpbridge.Call{Call: call, Seq: seq,
			StartedAt: time.Date(2026, 11, 9, 9, 0, seq, 0, time.UTC)}); err != nil {
			t.Fatalf("Record %s: %v", call.Name, err)
		}
	}
	record(1, tools.Call{Name: "read_page"})
	logged, err := store.BridgeLog(t.Context(), first)
	if err != nil {
		t.Fatalf("the collection's read: %v", err)
	}
	read := logged.Read()
	record(2, tools.Call{Name: "post_message", Args: map[string]any{"channel": "#eng"}})

	launch.BriefRead = &read
	next, err := store.BeginLaunch(t.Context(), launch, sandbox.Fence{})
	if err != nil {
		t.Fatalf("the send-back's launch: %v", err)
	}
	if next.Round == first.Round {
		t.Fatalf("the send-back works in round %q, want a new one", next.Round)
	}
	var items []sandbox.RunNote
	for _, n := range mustRow(t, store, turn).Steer.Notes {
		if n.LateCall != nil {
			items = append(items, n)
		}
	}
	if len(items) != 1 || items[0].For != next.LaunchID || items[0].LateCall.Call.Name != "post_message" {
		t.Fatalf("the record holds items %+v, want the call landed past the read, for the sent-back job", items)
	}
	inbox := boundInbox(t, runs, store, turn, next.LaunchID)
	viewHolds(t, inbox, 1)
	hook := readHook(t, inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch, hookCall()))
	if !strings.HasPrefix(hook.Output.Context, prompts.LateCallHead+"\n") ||
		!strings.Contains(hook.Output.Context, `Call: post_message({"channel":"#eng"})`) {
		t.Errorf("the sent-back job's next batch was handed %q, want the call under the late call's heading",
			hook.Output.Context)
	}
}
