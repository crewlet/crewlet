package sandbox_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// vanishing is the fleet's run records with one write that, once armed, finds
// the row removed by another writer between its decision and its commit: the
// decision agrees to write, the commit loses, and the retry finds no row.
type vanishing struct {
	*memory.Fleet
	armed atomic.Bool
}

func (v *vanishing) MutateSandboxRun(ctx context.Context, turnID string,
	decide func(coord.Record) (coord.SandboxRunChange, error)) (coord.Record, bool, error) {
	return v.Fleet.MutateSandboxRun(ctx, turnID, func(read coord.Record) (coord.SandboxRunChange, error) {
		change, err := decide(read)
		if v.armed.CompareAndSwap(true, false) {
			if _, _, gone := v.Fleet.MutateSandboxRun(ctx, turnID,
				func(coord.Record) (coord.SandboxRunChange, error) {
					return coord.SandboxRunChange{Delete: true}, nil
				}); gone != nil {
				return coord.SandboxRunChange{}, gone
			}
		}
		return change, err
	})
}

// AN ASK IS RECORDED ONLY WHERE THE WRITE LANDED. A run's row removed by
// another writer between the decision to record its question and the commit
// (its ending, say) leaves nothing to park on, and the run must be told so,
// never that its question was asked.
//
// Mutation: answer the outcome the last decision chose rather than whether the
// write won, and the run is told its question is recorded on a row that is gone.
func TestAnAskWhoseRowWentMidWriteIsNotRecorded(t *testing.T) {
	t.Parallel()
	fleet := &vanishing{Fleet: memory.NewFleet()}
	store := sandbox.NewCoordStore(fleet)
	run, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe",
		Status: sandbox.StatusLaunching}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	fleet.armed.Store(true)
	got, err := store.RecordAsk(t.Context(), "t1", run.LaunchID,
		sandbox.Ask{Question: "Which branch?", To: sandbox.AskToRequester})
	if err != nil || got != sandbox.AskNotRunning {
		t.Fatalf("RecordAsk on a row removed mid-write = %q, %v; want not running", got, err)
	}
	if _, found, _ := store.Get(t.Context(), "t1"); found {
		t.Fatal("the premise: the row was removed by the other writer")
	}
}
