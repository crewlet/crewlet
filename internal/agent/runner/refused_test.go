package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
)

const providerRefusal = "API Error: 400 tools.88.custom.name: String should match pattern '^[a-zA-Z0-9_-]{1,128}$'"

// refusedResume is a resume of a launched round whose job's provider refused
// its request, set as with says, and the launcher its relaunch would reach.
func refusedResume(t *testing.T, with func(*runner.Resume)) (*runner.Runner, *playLauncher) {
	t.Helper()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	resume := &runner.Resume{State: parked.State}
	with(resume)
	relauncher := &playLauncher{}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		resume: resume})
	return collector, relauncher
}

// A ROUND WHOSE CONTINUED SESSION ITS PROVIDER REFUSED IS CARRIED ON IN A FRESH
// ONE: the same round relaunched, continuing no session in its box and none
// the turn kept (the session is what was refused), recorded as the one
// relaunch a refusal gets, and told why it starts over.
//
// Mutation: collect the round instead ([Resume.carriedRound]), let the carry
// take up the kept session, or leave the relaunch unmarked, and it goes red.
func TestARoundWhoseSessionItsProviderRefusedIsCarriedOnFresh(t *testing.T) {
	t.Parallel()
	refused := func(r *runner.Resume) { r.Refused = &runner.Refused{Said: providerRefusal, BoxKept: true} }
	again := guarded{basis: keptBasis, carry: refused}.relaunch(t)
	if again.Session != nil || again.Continue == nil || again.Continue.SessionID != "" {
		t.Fatalf("the relaunch continues kept %+v and in place %+v, want no session at all",
			again.Session, again.Continue)
	}
	if !again.Continue.RequestRefused || again.Continue.SignInReplaced {
		t.Errorf("the relaunch is marked %+v, want the one relaunch a refused request gets", again.Continue)
	}
	if !strings.Contains(again.Brief, "could not start its work") {
		t.Errorf("the fresh run is not told why it starts over:\n%s", again.Brief)
	}
	// THE PREMISE: the same kept session is one a carry that keeps takes up.
	rotated := func(r *runner.Resume) { r.Rotated = &runner.Rotated{SessionID: "sess-1", BoxKept: true} }
	if kept := (guarded{basis: keptBasis, carry: rotated}).relaunch(t); kept.Session == nil {
		t.Fatal("the premise: a carry that keeps takes up the turn's kept session")
	}
}

// A REFUSAL THE RESUME COLLECTS IS THE ROUND'S ENDING: the work carries the
// provider's message for the loop to end the turn on, and nothing relaunches.
//
// Mutation: drop the message from the collected work, and the loop reviews a
// round no model worked on.
func TestACollectedRefusalIsCarriedOnTheRoundsWork(t *testing.T) {
	t.Parallel()
	collector, relauncher := refusedResume(t, func(r *runner.Resume) {
		r.Answer, r.RequestRefused = "The executor's run did NOT fully succeed.", providerRefusal
	})
	work, _, err := collector.Resume(t.Context(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if work.Suspended || work.RequestRefused != providerRefusal {
		t.Fatalf("the collected round is %+v, want it to carry the provider's message", work)
	}
	if relauncher.launched() != 0 {
		t.Errorf("a collected refusal launched %d more runs", relauncher.launched())
	}
}
