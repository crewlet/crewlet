package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A note's read by a hook is confirmed by what the job's session showed of it,
// and a session holds it only where its resume rebuilds the conversation that
// read it. What each case holds is which notes a relaunch tells a continued
// session again, as notes it may already show.

// noteRead is note id read by round 1's job at boundary, at place order among
// the record's reads, restarted where the job's session showed its hook's serve
// taken before its last restart.
func noteRead(id string, order int, boundary types.SteerBoundary, restarted bool) steer.Read {
	return steer.Read{Note: steer.Note{ID: id, Text: "the note " + id, By: "founder"},
		Reading: steer.Reading{Phase: types.PhaseExecute, Iteration: 1, Round: 1, Boundary: boundary,
			Launch: "job-1", Order: order, Restarted: restarted}}
}

// noteMsg is how a brief tells r.
func noteMsg(r steer.Read) string { return prompts.SteerMessage(r.By, r.Text) }

// answeredRelaunch is the relaunch a person's answer carries round 1 on with, the turn
// having read steered: continuing the kept session basis names, or, where it
// names none, the copy in the asking job's box, set applied to the resume.
func answeredRelaunch(t *testing.T, basis runner.SessionBasis, steered []steer.Read,
	set func(*runner.Resume)) runner.AgentRunRequest {

	t.Helper()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	relauncher := &probingLauncher{basis: basis}
	resume := &runner.Resume{State: parked.State, Steered: steered,
		Asked: &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder", SessionID: "s-1",
			BoxKept: true},
		Bridged: []runner.BridgedCall{numbered("jira_create", map[string]any{"summary": "the job's issue"}, 1, 1)}}
	if set != nil {
		set(resume)
	}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		steer: steer.New(), resume: resume})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	return relauncher.last(t)
}

// continuedBrief is the brief again continues its session with: the kept
// session's where it continues one, else the copy in its job's box.
func continuedBrief(t *testing.T, again runner.AgentRunRequest, kept bool) string {
	t.Helper()
	if kept {
		if again.Session == nil {
			t.Fatalf("the relaunch continues no kept session, in place %+v", again.Continue)
		}
		return again.Session.Brief
	}
	if again.Session != nil || again.Continue == nil {
		t.Fatalf("the relaunch continues kept %+v and in place %+v, want the copy in the job's box",
			again.Session, again.Continue)
	}
	return again.Continue.Brief
}

// toldAgainOnly fails t unless brief tells every note of told once each, in
// order, under the header of notes it may already show, and none of not.
func toldAgainOnly(t *testing.T, brief string, told, not []steer.Read) {
	t.Helper()
	at := strings.Index(brief, notesAgainHeader)
	if at < 0 {
		t.Fatalf("the session is told no note as one it may already show:\n%s", brief)
	}
	for _, r := range told {
		i := strings.Index(brief, noteMsg(r))
		if i < at || strings.Count(brief, noteMsg(r)) != 1 {
			t.Errorf("the session is not told %q once, under the header, in the order read:\n%s", r.ID, brief)
		}
		at = i
	}
	for _, r := range not {
		if strings.Contains(brief, noteMsg(r)) {
			t.Errorf("the session is told %q, which it holds:\n%s", r.ID, brief)
		}
	}
}

// A RELAUNCH TELLS AGAIN A HOOK NOTE ITS SESSION HELD ONLY BEFORE A RESTART. The
// job's session showed the hook's serve taken, then restarted: a resume
// rebuilds the conversation from the restart on, so a session continued from it
// is not known to hold that note, and the record holds it read, restarted. So a
// relaunch tells it again, and every note read after it, as notes the session
// may already show, and none before it, which the session holds: in a kept
// capture whose cursor counts the read, and in the copy in the job's box whose
// harness ended its own run.
//
// Mutation: ignore Restarted in firstUnheld, and both cases go red.
func TestARelaunchTellsAgainAHookNoteItsSessionHeldOnlyBeforeARestart(t *testing.T) {
	t.Parallel()
	brief := noteRead("brief", 1, types.SteerBoundaryBrief, false)
	restarted := noteRead("restarted", 2, types.SteerBoundaryToolBatch, true)
	after := noteRead("after", 3, types.SteerBoundaryToolBatch, false)
	steered := []steer.Read{brief, restarted, after}
	kept := keptBasis
	kept.NoteCursor = 3
	for name, tc := range map[string]struct {
		basis runner.SessionBasis
		set   func(*runner.Resume)
	}{
		"a kept capture": {basis: kept},
		"the copy in its box": {set: func(r *runner.Resume) {
			r.HarnessEnd = sandbox.HarnessEndOwnRun
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			again := answeredRelaunch(t, tc.basis, steered, tc.set)
			toldAgainOnly(t, continuedBrief(t, again, tc.basis.Continues()), []steer.Read{restarted, after},
				[]steer.Read{brief})
		})
	}
}

// A BRIEF'S NOTE BEFORE A COMPACTION IN ITS JOB IS TOLD AGAIN. The job's thread
// restarted after its brief, so the session may lack that brief and every note
// it told: the path lacks its brief, and is told every note the turn has read,
// as notes it may already show, whatever the capture's cursor counts — in a
// kept capture and in the copy in the job's box alike.
//
// Mutation: read firstUnheld where the path lacks its brief, and both cases go
// red.
func TestABriefsNoteBeforeACompactionInItsJobIsToldAgain(t *testing.T) {
	t.Parallel()
	brief := noteRead("brief", 1, types.SteerBoundaryBrief, false)
	after := noteRead("after", 2, types.SteerBoundaryToolBatch, false)
	steered := []steer.Read{brief, after}
	compacted := &sandbox.ResumedCount{Tokens: 10_000, Restart: new("r-1")}
	kept := keptClaude(compacted, false)
	kept.NoteCursor = 2
	for name, tc := range map[string]struct {
		basis runner.SessionBasis
		set   func(*runner.Resume)
	}{
		"a kept capture": {basis: kept, set: func(r *runner.Resume) {
			r.BasisRestart = new("")
		}},
		"the copy in its box": {set: func(r *runner.Resume) {
			copyOf(sandbox.HarnessEndOwnRun, compacted)(r)
			r.BasisRestart = new("")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			again := answeredRelaunch(t, tc.basis, steered, tc.set)
			toldAgainOnly(t, continuedBrief(t, again, tc.basis.Continues()), steered, nil)
		})
	}
}

// A HOOK NOTE IS TOLD AGAIN IN A BOX WHOSE RELEASE THE RESUME WAS NOT READ IN.
// A read at a hook's step is held as the job's session showed it, which only a
// release whose resume this build read rebuilds, and the launch learns the
// box's release only once its briefs are written. So where the capture's cursor
// counts such a read, the continuation carries beside its briefs the ones for
// such a box (runner.SessionContinuation.Unheld): every note from the first
// read at a hook's step on, as notes the session may already show, the ones
// before it not, and the box's own words for a box that replaced the job's.
// Where the cursor counts none, it carries none.
//
// Mutation: drop the hook-step stop from firstUnheld's strict reading, or
// build no unheld telling, and a case goes red.
func TestAHookNoteIsToldAgainInABoxWhoseReleaseTheResumeWasNotReadIn(t *testing.T) {
	t.Parallel()
	brief := noteRead("brief", 1, types.SteerBoundaryBrief, false)
	hook := noteRead("hook", 2, types.SteerBoundaryToolBatch, false)
	review := steer.Read{Note: steer.Note{ID: "review", Text: "the note review", By: "founder"},
		Reading: steer.Reading{Phase: types.PhaseReview, Iteration: 1, Round: 1,
			Boundary: types.SteerBoundaryRound, Order: 3}}
	kept := keptBasis
	kept.NoteCursor = 3
	t.Run("a cursor counting a hook's read", func(t *testing.T) {
		t.Parallel()
		again := answeredRelaunch(t, kept, []steer.Read{brief, hook, review}, nil)
		s := continuedBrief(t, again, true)
		if strings.Contains(s, notesAgainHeader) || strings.Contains(s, noteMsg(hook)) {
			t.Errorf("the brief for a box whose release was read tells a note its session holds:\n%s", s)
		}
		if again.Session.Unheld == nil {
			t.Fatal("the continuation carries no brief for a box whose release the resume was not read in")
		}
		toldAgainOnly(t, again.Session.Unheld.Brief, []steer.Read{hook, review}, []steer.Read{brief})
		toldAgainOnly(t, again.Session.Unheld.Replaced, []steer.Read{hook, review}, []steer.Read{brief})
		if newBox := strings.TrimPrefix(again.Session.Replaced, again.Session.Brief); newBox == "" ||
			!strings.HasSuffix(again.Session.Unheld.Replaced, newBox) {
			t.Errorf("the unheld brief for a box that replaced the job's does not say what that box changes:\n%s",
				again.Session.Unheld.Replaced)
		}
	})
	t.Run("a cursor counting no hook's read", func(t *testing.T) {
		t.Parallel()
		again := answeredRelaunch(t, kept, []steer.Read{brief, review}, nil)
		if s := continuedBrief(t, again, true); again.Session.Unheld != nil {
			t.Errorf("a continuation whose cursor counts no hook's read carries an unheld brief beside %q", s)
		}
	})
}

// THE RUN'S OPENING FRAME IS THE UNHELD BRIEF WHERE IT WAS HANDED: the phase's
// record states the prompt the run read, the brief for a box whose release the
// resume was not read in where the launch decided it handed that one, for a
// new box or for the box the session was last worked in, and the one told by
// the save point where it did not.
//
// Mutation: publish the default brief whatever the launch handed, or the
// unheld brief for the wrong box, and a case goes red.
func TestTheRunsOpeningFrameIsTheUnheldBriefWhereItWasHanded(t *testing.T) {
	t.Parallel()
	steered := []steer.Read{noteRead("brief", 1, types.SteerBoundaryBrief, false),
		noteRead("hook", 2, types.SteerBoundaryToolBatch, false)}
	kept := keptBasis
	kept.NoteCursor = 2
	unheld := func(newBox bool) func(runner.AgentRunRequest) runner.SessionDecision {
		return func(runner.AgentRunRequest) runner.SessionDecision {
			return runner.SessionDecision{Continues: true, Kept: true, NewBox: newBox, Unheld: true}
		}
	}
	for name, tc := range map[string]struct {
		decides func(runner.AgentRunRequest) runner.SessionDecision
		want    func(*runner.SessionContinuation) string
	}{
		"handed, a new box": {decides: unheld(true),
			want: func(s *runner.SessionContinuation) string { return s.Unheld.Replaced }},
		"handed, its own box": {decides: unheld(false),
			want: func(s *runner.SessionContinuation) string { return s.Unheld.Brief }},
		"not handed": {want: func(s *runner.SessionContinuation) string { return s.Replaced }},
	} {
		pub := newCapture()
		req := guarded{basis: kept, pub: pub, decides: tc.decides, carry: func(r *runner.Resume) {
			answered("use main")(r)
			r.Steered = steered
		}}.relaunch(t)
		if req.Session == nil || req.Session.Unheld == nil {
			t.Fatalf("%s: the relaunch carries no brief for a box whose release the resume was not read in", name)
		}
		s, published := req.Session, pub.promptsFor("execute")
		want := tc.want(s)
		if len(published) == 0 || !strings.Contains(published[len(published)-1], want) {
			t.Fatalf("%s: the opening frame does not state the brief the run was handed", name)
		}
		for _, other := range []string{s.Brief, s.Replaced, s.Unheld.Brief, s.Unheld.Replaced} {
			if !strings.Contains(want, other) && strings.Contains(published[len(published)-1], other) {
				t.Errorf("%s: the opening frame states a brief the run was not handed:\n%s", name, other)
			}
		}
	}
}
