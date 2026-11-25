package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
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

// THE UNHELD BRIEFS REACH THE SANDBOX BESIDE THE OTHER TWO. A kept capture's
// continuation carries, beside its briefs told by the save point, the two for a
// box whose release the resume was not read in and what they told, as the
// runner wrote them; one without them carries none; one whose floor dropped
// them carries the mark instead; and a base never carries them.
//
// Mutation: drop the unheld briefs, swap their two, lose their told set or the
// mark, or carry them beside a base, and a case goes red.
func TestTheUnheldBriefsReachTheSandboxBesideTheOtherTwo(t *testing.T) {
	t.Parallel()
	unheld := &runner.UnheldTelling{Brief: "told again in the box you left",
		Replaced: "told again in a box that replaced yours", Told: sandbox.BriefTold{Omitted: []int{4}}}
	kept := runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 2, SessionID: "s-1", Harness: "claude-code"}
	told := sandbox.BriefTold{Omitted: []int{1, 4}}

	got := sandboxSession(&runner.SessionContinuation{Basis: kept, Brief: "carry on in the box you left",
		Replaced: "carry on in a box that replaced yours", Told: told, Unheld: unheld})
	want := &sandbox.SessionContinuation{LaunchSeq: 1, CaptureSeq: 2, Brief: "carry on in the box you left",
		Replaced: "carry on in a box that replaced yours", Told: told,
		Unheld: &sandbox.UnheldTelling{Brief: "told again in the box you left",
			Replaced: "told again in a box that replaced yours", Told: sandbox.BriefTold{Omitted: []int{4}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a kept capture with unheld briefs reaches the sandbox as %+v (unheld %+v), want %+v (unheld %+v)",
			got, got.Unheld, want, want.Unheld)
	}

	if got := sandboxSession(&runner.SessionContinuation{Basis: kept, Brief: "b", Replaced: "r"}); got == nil ||
		got.Unheld != nil || got.UnheldDropped {
		t.Errorf("a kept capture with no unheld briefs reaches the sandbox as %+v, want none", got)
	}
	if got := sandboxSession(&runner.SessionContinuation{Basis: kept, Brief: "b", Replaced: "r",
		UnheldDropped: true}); got == nil || got.Unheld != nil || !got.UnheldDropped {
		t.Errorf("a kept capture whose floor dropped its unheld briefs reaches the sandbox as %+v, want the mark",
			got)
	}

	base := runner.SessionBasis{SessionID: "s-0", Harness: "claude-code", Base: &runner.BaseBasis{Turn: "t0"}}
	if got := sandboxSession(&runner.SessionContinuation{Basis: base, Replaced: "carry on", Unheld: unheld}); got == nil ||
		!got.Base || got.Unheld != nil {
		t.Errorf("a base reaches the sandbox as %+v, want it with no unheld briefs", got)
	}
}

// THE UNHELD BRIEFS THE FLOOR CANNOT CARRY ARE DROPPED ALONE. A kept capture's
// floor counts the briefs for a box whose release the resume was not read in,
// since its box may be handed them: where theirs is the floor no window leaves
// room for and the session's own briefs' is not, the launch still offers the
// session without them, on a copy, held to its own briefs' floor and marked so
// that only a box whose release was read continues it; where both fail, the
// session is dropped whole; and where nothing caps the seat, the launch is held
// to their floor, the longest.
//
// Mutation: leave the unheld briefs out of the floor, drop the whole session
// where only their floor fails, drop them unmarked or on the caller's session,
// and a case goes red.
func TestTheUnheldBriefsTheFloorCannotCarryAreDroppedAlone(t *testing.T) {
	// ABOUT 70,000 TOKENS OF BRIEF, beside a 70,000-token session: 140,000,
	// past a 100,000-token day, where the session's own briefs are not.
	long := strings.Repeat("x", 4*70000)
	member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: "claude-opus-5-5"})
	for name, tc := range map[string]struct {
		ceiling       int
		context       int64
		offered, kept bool
		floor         int
	}{
		"the unheld briefs past the room, the session's own within it": {ceiling: 100000, context: 70000,
			offered: true, floor: 70000 + 6},
		"both past the room": {ceiling: 100000, context: 150000},
		"no window capped":   {context: 70000, offered: true, kept: true, floor: 70000 + 70000},
	} {
		t.Run(name, func(t *testing.T) {
			ceilings := org.TokenCeilings(nil)
			if tc.ceiling > 0 {
				ceilings = org.TokenCeilings{period.Day: tc.ceiling}
			}
			_, l, _ := cappedFloorRig(t, ceilings, nil, 0, nil)
			l.turn.RunID = "unheld-floor " + name
			session := &runner.SessionContinuation{Brief: "carry on", Replaced: "carry on in a new box",
				Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
					Harness: executor.ClaudeCodeName, ContextTokens: tc.context},
				Unheld: &runner.UnheldTelling{Brief: "carry on", Replaced: long}}
			got, floor := l.continuedFloor(t.Context(), runner.AgentRunRequest{Brief: "the fresh brief",
				Session: session}, member, true)
			if (got.Session != nil) != tc.offered {
				t.Fatalf("the launch offers the session: %v, want %v", got.Session != nil, tc.offered)
			}
			if session.Unheld == nil || session.UnheldDropped {
				t.Errorf("the caller's continuation lost its unheld briefs")
			}
			if !tc.offered {
				return
			}
			if kept, dropped := got.Session.Unheld != nil, got.Session.UnheldDropped; kept != tc.kept ||
				dropped == tc.kept {
				t.Errorf("the launch offers the unheld briefs: %v, marked dropped: %v, want %v", kept, dropped,
					tc.kept)
			}
			if got.Session.Brief != "carry on" || got.Session.Replaced != "carry on in a new box" {
				t.Errorf("the offered session's briefs are %q and %q, want its own", got.Session.Brief,
					got.Session.Replaced)
			}
			if floor.tokens() != tc.floor {
				t.Errorf("the launch is held to %d, want %d", floor.tokens(), tc.floor)
			}
		})
	}
}
