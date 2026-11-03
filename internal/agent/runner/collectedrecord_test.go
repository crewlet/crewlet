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
// DID NOT KEEP. A long run's row keeps its calls' head and tail and tallies the
// middle it dropped, and the calls its calls' own work made; its collection is
// handed both (runner.Resume), and the round it collects is the whole of them
// in two places that outlive the row:
//
//   - the round's Work, which the loop closes the round with — the next
//     round's call log is seeded from its tallies, and a delivery check reads
//     the dropped calls as calls the round made;
//   - the execute phase's published record, the turn's account of what its
//     executor did once the row is gone — the dropped calls counted in its
//     round's calls and said by tool.
//
// Mutation: hand the round's Work back without the tallies, or publish the
// record without the dropped calls, from the collection's own path — the
// result it builds or the record it files — and the round reads as the calls
// its row kept: a reply dropped from the middle is refuted, the next round's
// seed under-counts, and the turn's record says two hundred calls of a run
// that made more.
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
