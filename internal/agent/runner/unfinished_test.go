package runner_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// A CALL STILL IN FLIGHT IS COUNTED BY ITS IDENTITY, ON EVERY PATH A RUN'S CALLS
// ARE READ BACK ON. The bridge writes a call's entry on its run's log before the
// call runs, so a call whose update has not landed — still running on the node
// its seat was taken from, its update refused by the store, or its process gone
// — is a call the run made, on its record by that entry's identity alone
// (runner.Resume.Unfinished). A segment that reads the run's calls back counts
// it as the run that never parked counts it: its collection, its continuation
// after a person's answer, its reopening on a node that took it over — so a
// later call that asks for something else never derives the operation id the
// call in flight already used. And a closed round's record keeps it, so a later
// round counts it too: one whose entry carries no digest — a tool that may ask a
// person first — as a call of its tool whose arguments are not known, never as
// the tool called with none.
//
// Mutation: leave the calls in flight out of the seed, and a segment counts no
// call before its next one to the tool — the call in flight's operation id,
// derived again; seed a closed round's unfinished call with no digest by its
// arguments, and it counts as the tool called with none.
func TestACallStillInFlightIsCountedByItsIdentityOnEveryPath(t *testing.T) {
	t.Parallel()
	register := func(t *testing.T, reg *tools.Registry) {
		if err := reg.RegisterWith(&statusWriter{}, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
			t.Fatal(err)
		}
	}
	inFlight := []runner.BridgedCall{{Call: ledger.Call{Name: "set_status", Result: runner.UnfinishedNote,
		Failed: true, Digest: turnctx.ArgsDigest(published), Unfinished: true}, Seq: 1}}
	state := execstate.State{Version: execstate.Version, Round: 1, ActiveTools: []string{"set_status"}}
	// THE RUN THAT NEVER PARKED made `published`, still in flight, and then
	// makes `noted`: one different call before it.
	check := func(t *testing.T, calls *turnctx.CallLog) {
		t.Helper()
		if got := calls.Ordinal("set_status", noted); got != 1 {
			t.Errorf("the segment counts %d different calls before its next one, want the call in flight, 1", got)
		}
	}

	t.Run("collected", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
			calls: calls, register: register,
			resume: &runner.Resume{State: state, Answer: "done", Unfinished: inFlight},
		})
		if _, _, err := r.Resume(context.Background(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		check(t, calls)
	})
	t.Run("continued", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
			calls: calls, register: register, launcher: &playLauncher{},
			resume: &runner.Resume{State: state, Unfinished: inFlight, Asked: &runner.Asked{
				Question: "which item?", Answer: "GP-12", SessionID: "s-1", BoxKept: true}},
		})
		if _, _, err := r.Resume(context.Background(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		check(t, calls)
	})
	t.Run("reopened", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{calls: calls, register: register})
		if _, err := r.ReopenAgentRun(context.Background(), state, nil, inFlight, nil, nil, runner.Bridged{},
			false); err != nil {
			t.Fatalf("ReopenAgentRun: %v", err)
		}
		check(t, calls)
	})
	t.Run("a closed round, its entry with no digest", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		closed := state
		closed.Round = 2
		closed.Iterations = []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{{Name: "set_status",
			Result: runner.UnfinishedNote, Failed: true, Unfinished: true}}}}
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{calls: calls, register: register})
		if _, err := r.ReopenAgentRun(context.Background(), closed, nil, nil, nil, nil, runner.Bridged{},
			false); err != nil {
			t.Fatalf("ReopenAgentRun: %v", err)
		}
		if got := calls.Ordinal("set_status", map[string]any{}); got != 1 {
			t.Errorf("the segment counts %d different calls before one with no arguments, want the call whose "+
				"arguments are not known, 1", got)
		}
	})
}

// A COLLECTED ROUND'S RECORD LISTS A CALL WHOSE OUTCOME IS UNKNOWN AS ONE. A
// call its run's log still held in flight when the collection read it is listed
// in the round's record after the round's calls — what the reviewer reads and
// the turn's tool log keeps — FAILED, so no delivery check, citation or
// submission takes it for something done, and answered in the words that say
// its outcome is unknown and its effects may stand: never as a plain failure,
// which a run makes again.
//
// Mutation: leave the unfinished calls out of the round's record, and the
// reviewer and the turn's tool log never see the call.
func TestACollectedRoundListsACallWhoseOutcomeIsUnknown(t *testing.T) {
	t.Parallel()
	register := func(t *testing.T, reg *tools.Registry) {
		if err := reg.RegisterWith(&statusWriter{}, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
		calls: turnctx.NewCallLog(), register: register,
		resume: &runner.Resume{
			State:  execstate.State{Version: execstate.Version, Round: 1, ActiveTools: []string{"set_status"}},
			Answer: "done",
			Unfinished: []runner.BridgedCall{{Call: ledger.Call{Name: "set_status", Result: runner.UnfinishedNote,
				Failed: true, Digest: turnctx.ArgsDigest(published), Unfinished: true}, Seq: 1}},
		},
	})
	work, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(work.Calls) != 1 {
		t.Fatalf("the round's record lists %+v, want the call whose outcome is unknown", work.Calls)
	}
	if c := work.Calls[0]; c.Name != "set_status" || !c.Failed || !c.Unfinished || c.Result != runner.UnfinishedNote {
		t.Errorf("the round's record lists %+v, want the call failed and marked unfinished, in the words "+
			"that say its outcome is unknown", c)
	}
}
