package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
)

// The access sections a dispatch renders, as a seat reads them.
const (
	toldAccess  = "## Your access\nWhat you can reach with your own credentials now.\n- Cloudflare (example.com): Secure."
	toldChanges = "## What changed since your last turn\nYour access changed.\n- Cloudflare: your level is now Secure (was Operate)."
)

// A RELAUNCH CARRIES THE SAME BLOCK. What the dispatch told the seat is frozen
// on the turn's park, so a relaunch of the turn, built from the parked state
// alone, briefs its run with the same "What changed" bytes and the same "Your
// access" section the first launch did, whatever the access is by then.
//
// Mutation: drop Access or CapabilityChanges from frozen or thawed and the
// relaunch's prompts lose them.
func TestARelaunchCarriesTheSameBlock(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, first := launch(t, entries, buildOpts{access: toldAccess, changes: toldChanges}, 1, "", nil)
	if !strings.Contains(first.Brief, toldChanges) || !strings.Contains(first.System, toldAccess) {
		t.Fatalf("the first launch is not told its access and its changes:\nsystem %s\nbrief %s",
			first.System, first.Brief)
	}
	parked, ok := launched.Suspended()
	if !ok {
		t.Fatal("the first launch did not park")
	}
	if parked.State.Context.CapabilityChanges != toldChanges || parked.State.Context.Access != toldAccess {
		t.Fatalf("the park froze %+v, want the dispatch's sections", parked.State.Context)
	}
	parked.State.ActiveTools = first.Surface.Active()
	relauncher := &probingLauncher{basis: keptBasis}
	resumed, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		resume: &runner.Resume{State: parked.State, Lost: &runner.Lost{Why: "its host was drained",
			Losses: 1, Limit: 2}}})
	if _, _, err := resumed.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if !strings.Contains(again.Brief, toldChanges) || !strings.Contains(again.System, toldAccess) {
		t.Errorf("the relaunch is not told what the first launch was:\nsystem %s\nbrief %s",
			again.System, again.Brief)
	}
}

// A CONTINUED CONVERSATION IS TOLD THE CHANGE. A conversation's next turn
// continues the session its last turn left, which said whatever it said about
// the seat's access before it changed; its run is handed the lead and then the
// turn's whole fresh brief, so the change block reaches the continued session
// as the newest message in it.
//
// Mutation: leave CapabilityChanges out of the fresh brief and the continued
// session is never told.
func TestAContinuedConversationIsToldTheChange(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launcher := &probingLauncher{basis: baseBasis}
	next, _ := buildWith(t, entries, buildOpts{launcher: launcher, access: toldAccess,
		changes: toldChanges, calls: turnctx.NewCallLog()})
	if _, _, err := next.Execute(t.Context(), 1, "", nil); err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	req := launcher.last(t)
	s := req.Session
	if s == nil || !s.Basis.ContinuesBase() {
		t.Fatalf("the launch continues %+v, want the conversation's session", s)
	}
	if !strings.Contains(s.Replaced, toldChanges) {
		t.Errorf("the continued session is not told what changed:\n%s", s.Replaced)
	}
	if !strings.Contains(req.System, toldAccess) {
		t.Errorf("the continued run's system prompt does not state its access:\n%s", req.System)
	}
}
