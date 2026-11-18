package sandbox

import (
	"slices"
	"testing"
)

// The briefs for a box whose release the resume was not read in, told apart
// from [savePointSameBox] and [savePointNewBox], and what they told of the
// round, told apart from [toldBySavePoint].
const (
	unheldSameBox = "carry on in the box you left, told again the notes your hooks were served"
	unheldNewBox  = "carry on in a box that replaced yours, told again the notes your hooks were served"
)

var toldUnheld = BriefTold{Omitted: []int{4}, Own: []Lead{answerLead("told again for an unread release")}}

// A HOOK NOTE IS TOLD AGAIN IN A BOX WHOSE RELEASE THE RESUME WAS NOT READ IN.
// A read at a hook's step is held as the job's session showed it, which only a
// release whose resume this build read rebuilds (CarryCheck.ResumeRead). So a
// box answering such a release is handed the save point's brief, and any other,
// the job's own whole copy among them, the one that tells those notes again
// ([UnheldTelling]) for the box it is, its told set recorded and the decision
// saying so; or, where the floor dropped that telling, no session at all.
//
// Mutation: hand the unheld brief whatever the box answers, never hand it, ask
// the job's own copy nothing, record the save point's told set beside it, leave
// it out of the decision, or continue a box the dropped telling was for, and a
// case goes red.
func TestAHookNoteIsToldAgainInABoxWhoseReleaseTheResumeWasNotReadIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		reused, interrupted bool
		unread, dropped     bool
		brief               string
	}{
		{name: "its own box, a release read", reused: true, interrupted: true, brief: savePointSameBox},
		{name: "its own box, a release not read", reused: true, interrupted: true, unread: true,
			brief: unheldSameBox},
		{name: "a new box, a release read", interrupted: true, brief: savePointNewBox},
		{name: "a new box, a release not read", interrupted: true, unread: true, brief: unheldNewBox},
		{name: "a new box of a clean exit, a release not read", unread: true, brief: unheldNewBox},
		{name: "its own whole copy, a release read", reused: true, brief: savePointSameBox},
		{name: "its own whole copy, a release not read", reused: true, unread: true, brief: unheldSameBox},
		{name: "dropped, a new box of a release read", interrupted: true, dropped: true, brief: savePointNewBox},
		{name: "dropped, a new box of a release not read", interrupted: true, unread: true, dropped: true},
		{name: "dropped, its own whole copy of a release not read", reused: true, unread: true, dropped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, opened, req := exactRelaunch(t, tc.interrupted, "2.1.999")
			rig.runner.CarryResumeUnread = tc.unread
			req.Session.Exact = nil
			req.Session.Unheld = &UnheldTelling{Brief: unheldSameBox, Replaced: unheldNewBox, Told: toldUnheld}
			if tc.dropped {
				req.Session.Unheld, req.Session.UnheldDropped = nil, true
			}
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused, false)
			t.Cleanup(got.release)
			if tc.brief == "" {
				if got.session != "" || got.kind != BriefFresh {
					t.Fatalf("the launch continues session %q (%s), want a session of the run's own", got.session,
						got.kind)
				}
				return
			}
			if got.brief != tc.brief || got.session != "s-1" {
				t.Fatalf("the launch hands brief %q continuing session %q, want %q continuing the kept session",
					got.brief, got.session, tc.brief)
			}
			unheld := tc.brief == unheldSameBox || tc.brief == unheldNewBox
			told := toldBySavePoint
			if unheld {
				told = toldUnheld
			}
			if !slices.Equal(got.told.Omitted, told.Omitted) || got.exact {
				t.Errorf("the launch records %+v told (exact %v), want %+v", got.told, got.exact, told)
			}
			if d := got.decision(); d.Unheld != unheld || d.Exact {
				t.Errorf("the launch's decision is %+v, want it to say it handed the unheld telling: %v", d, unheld)
			}
		})
	}
}
