package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
)

// THE LAUNCH'S DECISION SAYS WHETHER ITS BRIEF WAS THE UNHELD ONE. A relaunch
// handed, beside the briefs told by its save point, the ones for a box whose
// release the resume was not read in starts its run on one of them only where
// its box's harness answers such a release, and tells the runner so before the
// run starts, so the run's guard and its published prompt follow the brief it
// read; a box whose release was read starts on the brief told by the save
// point, and the runner is told that.
//
// Mutation: carry the unheld briefs nowhere, hand them whatever the box
// answers, or leave the launch's word on them out of what the runner is told,
// and a case goes red.
func TestTheLaunchsDecisionSaysWhetherItsBriefWasTheUnheldOne(t *testing.T) {
	t.Parallel()
	for name, unread := range map[string]bool{"a box of a release read": false, "a box of a release not read": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := newKeptLaunch(t, 40000, 0, 0)
			k.jobs.CarryResumeUnread = unread
			req := launchRequest(k.l, nil)
			req.Brief = "the fresh brief"
			req.Session = &runner.SessionContinuation{Basis: k.l.ProbeSession(k.ctx),
				Brief: "carry on in the box you left", Replaced: "carry on in a box that replaced yours",
				Unheld: &runner.UnheldTelling{Brief: "told again in the box you left",
					Replaced: "told again in a box that replaced yours"}}
			var decided []runner.SessionDecision
			req.Decided = func(d runner.SessionDecision) { decided = append(decided, d) }
			if _, err := k.l.LaunchExecutor(k.ctx, req); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			if len(decided) != 1 || !decided[0].Continues || !decided[0].Kept || decided[0].Unheld != unread ||
				decided[0].Exact {
				t.Fatalf("the runner was told %+v, want once: the turn's kept session, the unheld brief: %v",
					decided, unread)
			}
			want := "carry on in the box you left"
			if decided[0].NewBox {
				want = "carry on in a box that replaced yours"
			}
			if unread {
				want = strings.Replace(want, "carry on", "told again", 1)
			}
			started := k.jobs.Started()
			if last := started[len(started)-1]; last.Resume != "s-1" || last.Brief != want {
				t.Fatalf("the relaunched job started with session %q and brief %q, want the kept session and %q",
					last.Resume, last.Brief, want)
			}
		})
	}
}
