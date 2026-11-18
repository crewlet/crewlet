package runner_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RELAUNCH HANDS ITS LAUNCH THE READ ITS BRIEFS WERE BUILT FROM. Every brief
// of a relaunch is built from one read of the round's log, the collection's,
// and the launch is readied (probe, long-call rewrite, approved call) before
// its opening moves the log on, so a call of the job before landing in between
// is told by no brief. The launch is handed that read
// (runner.AgentRunRequest.BriefRead): a relaunch carrying the round on, whose
// opening marks the round's calls past it late, and a send-back, whose opening
// carries them late into the new round. A turn's first launch follows no read
// and hands none.
//
// Mutation: hand no read on the send-back's launch, or on the one carrying the
// round on, and this goes red.
func TestARelaunchHandsItsLaunchTheReadItsBriefsWereBuiltFrom(t *testing.T) {
	t.Parallel()
	read := sandbox.LogRead{Launch: "job-1", Took: 7,
		Absorbed: []sandbox.ElidedCall{{Name: "read_file", Digest: "d1", Calls: 2}}}
	same := func(got *sandbox.LogRead) bool {
		return got != nil && got.Launch == read.Launch && got.Took == read.Took && len(got.Absorbed) == 1 &&
			got.Absorbed[0].Digest == "d1" && got.Absorbed[0].Calls == 2
	}
	t.Run("carrying the round on", func(t *testing.T) {
		t.Parallel()
		bridged := []runner.BridgedCall{numbered("slack_post", map[string]any{"text": "posted"}, 1, 1)}
		req := lostRelaunch(t, keptBasis, runner.Resume{Bridged: bridged, BridgedRead: read})
		if !same(req.BriefRead) {
			t.Fatalf("the relaunch carrying the round on hands its launch the read %+v, want %+v, the one its "+
				"briefs were built from", req.BriefRead, read)
		}
	})
	t.Run("a send-back, and a turn's first launch", func(t *testing.T) {
		t.Parallel()
		entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
		launched, first := launch(t, entries, buildOpts{}, 1, "", nil)
		if first.BriefRead != nil {
			t.Errorf("a turn's first launch hands the read %+v, want none: no collection precedes it",
				first.BriefRead)
		}
		parked, _ := launched.Suspended()
		parked.State.ActiveTools = first.Surface.Active()
		bridged := []runner.BridgedCall{post("hi", 1)}
		relauncher := &probingLauncher{basis: keptBasis}
		collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
			resume: &runner.Resume{State: parked.State, Bridged: bridged, BridgedRead: read, Answer: "posted"}})
		if _, _, err := collector.Resume(t.Context(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		history := []ledger.Iteration{{Iteration: 1, Calls: ledgerOf(bridged), ReviewNotes: "post it to C2"}}
		if _, _, err := collector.Execute(t.Context(), 2, "post it to C2", history); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if got := relauncher.last(t).BriefRead; !same(got) {
			t.Errorf("the send-back hands its launch the read %+v, want the collection's %+v", got, read)
		}
	})
}
