package sandbox

import (
	"slices"
	"testing"
)

// The briefs for a box whose release the resume was not read in, told apart
// from [savePointSameBox] and [savePointNewBox].
const (
	unheldSameBox = "carry on in the box you left, told again the notes your hooks were served"
	unheldNewBox  = "carry on in a box that replaced yours, told again the notes your hooks were served"
)

// A HOOK NOTE IS TOLD AGAIN IN A BOX WHOSE RELEASE THE RESUME WAS NOT READ IN.
// A read at a hook's step is held as the job's session showed it, which only a
// release whose resume this build read rebuilds (CarryCheck.ResumeRead). So a
// box whose harness answers such a release is handed the brief told by the
// save point, and any other the one that tells those notes again
// ([UnheldTelling]), for the box it is, its told set the save point's: the
// calls they tell are the same. The job's own box, its copy taken whole there,
// is asked nothing and holds what its own release wrote, so it is handed the
// save point's.
//
// Mutation: hand the unheld brief whatever the box answers, never hand it, or
// hand it to the job's own whole copy, and a case goes red.
func TestAHookNoteIsToldAgainInABoxWhoseReleaseTheResumeWasNotReadIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		reused, interrupted bool
		unread              bool
		brief               string
	}{
		{name: "its own box, a release read", reused: true, interrupted: true, brief: savePointSameBox},
		{name: "its own box, a release not read", reused: true, interrupted: true, unread: true,
			brief: unheldSameBox},
		{name: "a new box, a release read", interrupted: true, brief: savePointNewBox},
		{name: "a new box, a release not read", interrupted: true, unread: true, brief: unheldNewBox},
		{name: "a new box of a clean exit, a release not read", unread: true, brief: unheldNewBox},
		{name: "its own whole copy, asked nothing", reused: true, unread: true, brief: savePointSameBox},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, opened, req := exactRelaunch(t, tc.interrupted, "2.1.999")
			rig.runner.CarryResumeUnread = tc.unread
			req.Session.Exact = nil
			req.Session.Unheld = &UnheldTelling{Brief: unheldSameBox, Replaced: unheldNewBox}
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused, false)
			t.Cleanup(got.release)
			if got.brief != tc.brief || got.session != "s-1" {
				t.Fatalf("the launch hands brief %q continuing session %q, want %q continuing the kept session",
					got.brief, got.session, tc.brief)
			}
			if !slices.Equal(got.told.Omitted, toldBySavePoint.Omitted) || got.exact {
				t.Errorf("the launch records %+v told (exact %v), want the save point's", got.told, got.exact)
			}
		})
	}
}
