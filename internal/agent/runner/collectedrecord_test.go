package runner_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A COLLECTED ROUND IS EVERY CALL ITS RUN MADE, ITS RECORD'S AND THE ONES IT
// DID NOT KEEP. A long run's log keeps its calls' head and tail and tallies
// both the dropped middle and the calls its calls' own work made; the
// collection is handed both (runner.Resume) and counts them where the round
// outlives the log: its Work (which seeds the next round's call log, and whose
// dropped calls a delivery check reads as the round's) and the execute phase's
// published record (the dropped calls in its round's count and said by tool).
//
// Mutation: drop the tallies from the Work the collection builds, or the
// dropped calls from the record it files, and a reply dropped from the middle
// is refuted, the next round's seed under-counts, and the record says fewer
// calls than the run made.
func TestACollectedRoundIsEveryCallItsRunMade(t *testing.T) {
	t.Parallel()
	launched := time.Now().Add(-time.Hour).UTC()
	dropped := []ledger.Elided{
		{Name: "slack_post", Digest: "a", Calls: 40},
		{Name: "slack_post", Digest: "b", Calls: 19, Failed: 2},
		{Calls: 3},
	}
	absorbed := []ledger.Elided{{Name: "leave_channel", Digest: "w", Calls: 1}}
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
		pub: pub,
		resume: &runner.Resume{
			State: execstate.State{Version: execstate.Version, Round: 1, LaunchedAt: launched,
				ActiveTools: []string{"slack_post"}},
			Answer: "posted the summary",
			Bridged: []runner.BridgedCall{{Call: ledger.Call{Name: "slack_post", Result: "posted"}, Seq: 1,
				StartedAt: launched.Add(time.Minute)}},
			Elided: dropped, Absorbed: absorbed,
			Run: runner.RunRecord{Harness: "claude-code", SandboxID: "box-1"},
		},
	})
	w, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !slices.Equal(w.Elided, dropped) || !slices.Equal(w.Absorbed, absorbed) {
		t.Errorf("the collected round's work carries %+v dropped and %+v absorbed, want %+v and %+v",
			w.Elided, w.Absorbed, dropped, absorbed)
	}

	var done *types.AgentPhaseCompleted
	for _, ev := range pub.events {
		if rec, ok := ev.Data.(*types.AgentPhaseCompleted); ok && rec.Phase == types.PhaseExecute {
			done = rec
		}
	}
	if done == nil {
		t.Fatal("the collection published no execute record")
	}
	want := []types.ElidedCalls{{Name: "slack_post", Calls: 59, Failed: 2}, {Calls: 3}}
	if done.CallsElided != 62 || !slices.Equal(done.ElidedCalls, want) {
		t.Errorf("the execute record says %d calls were dropped, %+v; want 62, %+v", done.CallsElided,
			done.ElidedCalls, want)
	}
	if len(done.Rounds) != 1 || done.Rounds[0].ToolCalls != 63 {
		t.Errorf("the record's round counts %+v calls, want the one kept and the 62 dropped", done.Rounds)
	}
}
