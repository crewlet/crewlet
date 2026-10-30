package sandbox

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// beforePart is a run store that calls before once, ahead of the first write of
// a part whose name begins with prefix — the instant between a writer's read of
// the row and the write it decided on, where another node's relaunch can land.
type beforePart struct {
	partRuns
	prefix string
	before func()
	once   sync.Once
}

// partRuns names the embedded store, whose own name is one of its methods.
type partRuns = coord.SandboxRuns

func (b *beforePart) PutSandboxRunPart(ctx context.Context, turnID, part string, value []byte,
	version uint64) (bool, error) {

	if strings.HasPrefix(part, b.prefix) {
		b.once.Do(b.before)
	}
	return b.partRuns.PutSandboxRunPart(ctx, turnID, part, value, version)
}

// relaunchedUnder is a store over runs, with the turn's job launched, whose
// first write of a part named with prefix finds the turn RELAUNCHED by another
// node between the writer's read of the row and that write: the relaunch's own
// removal of the job it replaced has already run, so only the writer can tell
// that what it is about to write names nobody. It answers the store and the
// replaced job's launch.
func relaunchedUnder(t *testing.T, prefix string) (*CoordStore, string) {
	t.Helper()
	runs := memory.NewFleet()
	peer := NewCoordStore(runs)
	opened, err := peer.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentHandle: "swe"}, Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	store := NewCoordStore(&beforePart{partRuns: runs, prefix: prefix, before: func() {
		if _, err := peer.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentHandle: "swe"},
			Fence{}); err != nil {
			t.Errorf("the relaunch: %v", err)
		}
	}})
	return store, opened.LaunchID
}

// A SUSPENSION WHOSE RUN MOVED ON LEAVES NO CONVERSATION BESIDE IT.
//
// The conversation is written beside the row before the flip that makes it
// resumable, so a run relaunched between the turn's read of its row and that
// write leaves the write naming a job no row runs — after the relaunch removed
// what the job it replaced had kept. The flip is refused, and the writer removes
// what it wrote: nothing else would until the run ends, and a conversation is a
// turn's every message.
//
// Mutation: keep the conversation when the flip is refused, and it outlives the
// job that wrote it.
func TestASuspensionWhoseRunMovedOnLeavesNoConversation(t *testing.T) {
	t.Parallel()
	store, replaced := relaunchedUnder(t, conversationKind)
	suspended, err := store.MarkSuspended(t.Context(), "t1", Suspension{State: []byte(`{"messages":[]}`)})
	if err != nil || suspended {
		t.Fatalf("MarkSuspended = (%v, %v), want the flip refused: the run was relaunched under it",
			suspended, err)
	}
	if state, kept, err := store.Conversation(t.Context(), "t1", replaced); err != nil || kept {
		t.Fatalf("the replaced job's conversation = (%s, %v, %v), want none kept", state, kept, err)
	}
	row, _, err := store.Get(t.Context(), "t1")
	if err != nil || row.Status != StatusLaunching || row.LaunchID == replaced {
		t.Fatalf("the row = %+v (%v), want the relaunch's job, still launching", row, err)
	}
}

// A BRIDGED CALL WHOSE JOB WAS REPLACED UNDER IT LEAVES NO LOG BESIDE THE ROW.
//
// The call reaches the job's log before its count reaches the row, so a
// relaunch landing between the append's read of the row and its log write
// leaves a log of a job no row holds — written after the relaunch removed that
// job's. The count is refused, and the appender removes the log: the call is
// not recorded, as a call of a job that is over never is.
//
// Mutation: keep the log when the count is refused, and it outlives its job.
func TestABridgedCallWhoseJobWasReplacedUnderItLeavesNoLog(t *testing.T) {
	t.Parallel()
	store, replaced := relaunchedUnder(t, bridgeLogKind)
	recorded, err := store.AppendBridgeCall(t.Context(), "t1", BridgeAppend{
		Launch: replaced, Call: BridgeCall{Name: "delegate"},
	})
	if err != nil || recorded {
		t.Fatalf("AppendBridgeCall = (%v, %v), want the call not recorded: its job was replaced",
			recorded, err)
	}
	if calls, err := store.BridgeLog(t.Context(), "t1", replaced); err != nil || len(calls) != 0 {
		t.Fatalf("the replaced job's log = (%+v, %v), want none kept", calls, err)
	}
	if row, _, err := store.Get(t.Context(), "t1"); err != nil || row.Launch.BridgedCalls != 0 {
		t.Fatalf("the relaunch's job counts %d bridged calls (%v), want none", row.Launch.BridgedCalls, err)
	}
}
