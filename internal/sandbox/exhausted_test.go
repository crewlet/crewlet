package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A SESSION THAT RAN OUT OF CONTEXT IS CONTINUED BY NOTHING: its job's collection
// records it on the run's record and takes no capture of it, every way a later
// launch of the turn could take it up refuses it, and the turn's end carries it to
// the executor log, which retires it for every later turn.

const ranOutWords = "ran out of its model's context"

// finishedOutOfContext is the result of a job its harness says ran out of context.
func finishedOutOfContext(session *FakeSession) Result {
	return Result{Error: "Prompt is too long", ContextExhausted: true, SessionID: "s-1",
		Captured: &session.Archive}
}

// A JOB THAT RAN OUT OF CONTEXT IS RECORDED, AND LEAVES NO CAPTURE TO CONTINUE: the
// row names the session it ran, nothing is taken or uploaded, and the launch's
// record says why. A job that failed any other way keeps its session as ever, since
// another launch of it may well work.
//
// Mutation: drop the stamp in collectClaimed and the row names nothing; drop
// at.exhausted and the doomed session is uploaded; judge on result.ContextExhausted
// alone and a stopped or lost job is taken for one that ran out.
func TestAJobThatRanOutOfContextIsRecordedAndKeepsNoCapture(t *testing.T) {
	t.Parallel()
	t.Run("it ran out", func(t *testing.T) {
		t.Parallel()
		rig, writes, keep := keepingRig(t)
		rig.launchKept(t, "t1")
		rig.runner.Finish(finishedOutOfContext(rig.runner.Session))
		collectHeld(t, rig, "t1")

		run := rig.get("t1")
		if want := []string{run.SessionRan()}; want[0] == "" || !slices.Equal(run.ExhaustedSessions, want) {
			t.Fatalf("the row says %q ran out, want the session the job ran, %q", run.ExhaustedSessions, want)
		}
		if run.Capture == nil || run.Capture.State != CaptureRefused || !run.Capture.Final || run.Kept != nil ||
			len(keep.uploads()) != 0 || len(writes.captured()) != 0 {
			t.Fatalf("the row holds capture %+v, kept %+v, with %d uploads and %d records: want the capture "+
				"refused and nothing taken of a session no launch continues", run.Capture, run.Kept,
				len(keep.uploads()), len(writes.captured()))
		}
		recs := rig.collected()
		if len(recs) != 1 || recs[0].ErrorKind != types.ExecutorErrorContextExhausted ||
			!strings.Contains(recs[0].SessionNotKept, "ran out") {
			t.Fatalf("the launch's record is %+v, want its kind and why its session was not kept", recs)
		}
	})
	t.Run("it failed any other way", func(t *testing.T) {
		t.Parallel()
		rig, _, keep := keepingRig(t)
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		rig.runner.Finish(Result{Error: "exit 1", SessionID: "s-1", Captured: &session.Archive})
		collectHeld(t, rig, "t1")

		run := rig.get("t1")
		if len(run.ExhaustedSessions) != 0 {
			t.Errorf("a launch_failed run retired %q", run.ExhaustedSessions)
		}
		if run.Kept == nil || len(keep.uploads()) != 1 {
			t.Errorf("a launch_failed run kept %+v, with %d uploads, want its session kept", run.Kept,
				len(keep.uploads()))
		}
		if recs := rig.collected(); len(recs) != 1 || recs[0].ErrorKind != types.ExecutorErrorLaunchFailed {
			t.Errorf("the launch's record is %+v, want launch_failed", recs)
		}
	})
	for name, spoil := range map[string]func(*PendingRun){
		"a job a person stopped":     func(r *PendingRun) { r.Stop = &RunStop{By: "ana"} },
		"a job lost with its host":   func(r *PendingRun) { r.Lost = &JobLoss{} },
		"a job parked on a question": func(r *PendingRun) { r.Question = "which branch?" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := PendingRun{}
			spoil(&run)
			if ranOutOfContext(run, Result{ContextExhausted: true}) {
				t.Errorf("%s is judged to have run out of context, and it did not fail", name)
			}
		})
	}
	if ranOutOfContext(PendingRun{}, Result{Success: true, ContextExhausted: true}) {
		t.Error("a job that succeeded is judged to have run out of context")
	}
}

// A STORE THAT DOES NOT TAKE THE FACT IS A COLLECTION TO ASK AGAIN, never a turn
// resumed with its relaunches free to continue the session.
//
// Mutation: log the write's failure and carry on, and the collection answers.
func TestAFactTheStoreDidNotTakeHandsTheCollectionBack(t *testing.T) {
	t.Parallel()
	rig, _, _ := keepingRig(t)
	rig.launchKept(t, "t1")
	rig.runner.Finish(finishedOutOfContext(rig.runner.Session))
	rig.coordinator.pending = &refusesRetirement{PendingStore: rig.pending}
	if _, _, collected, _ := rig.coordinator.collectClaimed(t.Context(), rig.get("t1")); collected {
		t.Fatal("the collection answered with the session's failure unrecorded")
	}
}

// refusesRetirement is a run store that does not take a job's running out of context.
type refusesRetirement struct{ PendingStore }

func (*refusesRetirement) RetireSessions(context.Context, string, []string) error {
	return errors.New("the store is having a bad moment")
}

// A RELAUNCH CONTINUES NO CAPTURE OF A SESSION ITS TURN RAN OUT OF CONTEXT IN: the
// question its caller asks says so, and a launch asked to continue it all the same
// starts a session of its own, on the brief written for one.
//
// Mutation: drop the check from ContinuityOf and the relaunch continues it.
func TestARelaunchContinuesNoKeptCaptureOfASessionThatRanOut(t *testing.T) {
	t.Parallel()
	rig, restore, kept := keptTurn(t)
	if continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner); why != "" ||
		!continued.Capture.samePlace(kept) {
		t.Fatalf("the premise: the relaunch continues %+v (%q), want the kept capture", continued, why)
	}
	if err := rig.pending.RetireSessions(t.Context(), "t1", []string{"s-1"}); err != nil {
		t.Fatalf("RetireSessions: %v", err)
	}
	continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", rig.runner)
	if continued.Capture.CaptureSeq != 0 || !strings.Contains(why, ranOutWords) || !strings.Contains(why, "s-1") {
		t.Fatalf("the relaunch continues %+v (%q), want none, saying the session ran out", continued, why)
	}
	if _, err := launchRestoring(t, rig, restore, continuingKept(kept)); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	started := rig.runner.Started()
	if last := started[len(started)-1]; last.Resume != "" || last.Brief != "the fresh brief" {
		t.Fatalf("the relaunch started session %q with brief %q, want a session of its own", last.Resume, last.Brief)
	}
}

// A RELAUNCH CONTINUES NO BASE ITS TURN RAN OUT OF CONTEXT IN: a turn that kept
// nothing goes back to the base its first launch froze, the very session that ran
// out, unless the decision every launch of a base asks refuses it.
//
// Mutation: drop the check from BaseContinuityOf and the relaunch continues the base.
func TestARelaunchContinuesNoBaseThatRanOut(t *testing.T) {
	t.Parallel()
	rig, logbook, _ := conversingRig(t)
	read := frozenReadOf(logbook, logbook.endedTurn(t, "b1", "s-base", "the base's transcript"))
	ctx := t.Context()
	if _, err := rig.coordinator.Launch(ctx, rig.manager, continuingBase("t2", read)); err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	if continued, why := rig.coordinator.ContinuedCapture(ctx, "t2", rig.runner); continued.Base == nil {
		t.Fatalf("the premise: the relaunch continues no base (%q)", why)
	}
	if err := rig.pending.RetireSessions(ctx, "t2", []string{"s-base"}); err != nil {
		t.Fatalf("RetireSessions: %v", err)
	}
	continued, why := rig.coordinator.ContinuedCapture(ctx, "t2", rig.runner)
	if continued.Base != nil || !strings.Contains(why, ranOutWords) {
		t.Fatalf("the relaunch continues base %+v (%q), want none, saying it ran out", continued.Base, why)
	}
	opened := rig.get("t2")
	got := rig.coordinator.continueSession(ctx, continuingBaseRequest("t2"), opened, NewFakeSandbox("box-1"),
		rig.runner, false, false)
	t.Cleanup(got.release)
	if got.kind != BriefFresh || got.session != "" || got.carried != nil {
		t.Fatalf("the launch hands %s on session %q, want a session of its own", got.kind, got.session)
	}
}

// continuingBaseRequest is a relaunch of turn asked to continue its frozen base.
func continuingBaseRequest(turn string) LaunchRequest {
	req := launchReq(turn)
	req.Brief = "the fresh brief"
	req.Session = &SessionContinuation{Base: true, Replaced: "a new turn begins here"}
	return req
}

// A RELAUNCH CONTINUES NO COPY ITS BOX HOLDS OF A SESSION THAT RAN OUT: the job
// before's own session is continued in place only where that job did not run out
// in it. A later session of the turn is another matter, and carries on in that box.
//
// Mutation: drop the check from continueSession and the first case continues it.
func TestARelaunchContinuesNoCopyInItsBoxOfASessionThatRanOut(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		exhausted []string
		want      BriefKind
		session   string
	}{
		"the session it ran out in":  {exhausted: []string{"sess-own"}, want: BriefFresh},
		"a session started after it": {exhausted: []string{"sess-older"}, want: BriefContinued, session: "sess-own"},
		"no session ran out":         {want: BriefContinued, session: "sess-own"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			req := launchReq("t1")
			req.Brief = "the fresh brief"
			req.Continue = &Continuation{SessionID: "sess-own", Brief: "carry on in the box you left"}
			opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2, ExhaustedSessions: tc.exhausted}
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, true, false)
			t.Cleanup(got.release)
			if got.kind != tc.want || got.session != tc.session {
				t.Fatalf("the launch hands %s on session %q, want %s on %q", got.kind, got.session, tc.want,
					tc.session)
			}
		})
	}
}

// A SESSION STARTED AFTER RUNNING OUT IS CONTINUED AS ANY OTHER: what a job ran out
// in names that session and no other, so a turn is not left starting fresh for good.
//
// Mutation: refuse on any exhausted session rather than on the row's and the
// relaunch of the job that worked starts over.
func TestAFreshSessionAfterRunningOutIsContinued(t *testing.T) {
	t.Parallel()
	kept := SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: CaptureLanded, SessionID: "s-2"}
	row := executorkeep.TurnRow{SessionID: "s-2", LaunchSeq: 2, CaptureSeq: 1, Object: keptRow(t,
		landedAt(t)).Object}
	run := PendingRun{TurnID: "t1", Capture: &kept, Kept: &kept, ExhaustedSessions: []string{"s-1"}}
	if got := ContinuityOf(row, &run, 2, allRestores{}); !got.Continues {
		t.Fatalf("the relaunch does not continue the session that worked: %s", got.Why)
	}
	base := BaseSession{Turn: "b1", Object: "o", SessionID: "s-other", Harness: "claude-code"}
	if got := BaseContinuityOf(base, nil, &PendingRun{TurnID: "t1", ExhaustedSessions: []string{"s-1"}},
		allRestores{}); !got.Continues {
		t.Fatalf("a base no job ran out in is refused: %s", got.Why)
	}
}

// landedAt is a landed capture naming a real object key, for a row built by hand.
func landedAt(t *testing.T) SessionCapture {
	t.Helper()
	return SessionCapture{Object: objstore.KeyAt(time.Now()).String(), ObjectHash: "h", ObjectSize: 1,
		LaunchSeq: 2, CaptureSeq: 1}
}

// allRestores is a runner that restores every archive it is asked about.
type allRestores struct{}

func (allRestores) Restores(SessionArchive) (bool, string) { return true, "" }

// A READER IS TOLD WHAT THE LAUNCH WOULD DO: a live turn's session that ran out is
// decided, not continued, says why, and is not offered in the box either, where a
// person's answer would otherwise carry it on.
//
// Mutation: drop the check from inParkedBox and the parked case says it continues.
func TestAReaderIsToldASessionThatRanOutIsNotContinued(t *testing.T) {
	t.Parallel()
	now := time.Now()
	kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, SessionID: "s-1"}
	row := executorkeep.TurnRow{Harness: "claude-code", SessionID: "s-1", LaunchSeq: 1, CaptureSeq: 1,
		Object: keptRow(t, landedAt(t)).Object}
	target := RelaunchTarget{Harness: "claude-code", Placement: "local", Node: "node-a", Restorer: allRestores{}}
	run := PendingRun{TurnID: "t1", Status: StatusAwaiting, LaunchSeq: 1, SessionID: "s-1", SandboxID: "box-1",
		Placement: "local", Harness: "claude-code", LaunchNode: "node-a", BoxSince: now,
		Capture: &kept, Kept: &kept, ExhaustedSessions: []string{"s-1"}}

	got := ReadContinuityOf(row, "", &run, target, ContinuedByNextTurn, now)
	if got.Continues || !got.Decided || !strings.Contains(got.Why, ranOutWords) {
		t.Fatalf("a reader is told %+v, want it decided, not continued, saying the session ran out", got)
	}
	if box, why := inParkedBox(run, row, target, now); box != boxNot || !strings.Contains(why, ranOutWords) {
		t.Fatalf("the parked box is answered (%v, %q), want it not continued, saying why", box, why)
	}
}

// THE TURN'S END CARRIES THE FACT TO THE LOG, however the turn ended, as it carries
// a capture that was not kept: the run's record that knew it is deleted with the
// turn, and every later turn reads the log.
//
// Mutation: drop the assignment in publishEnd and no later turn hears of it.
func TestATurnsEndNamesTheSessionsItRanOutOfContextIn(t *testing.T) {
	t.Parallel()
	rig, writes, _ := keepingRig(t)
	rig.launchKept(t, "t1")
	rig.runner.Finish(finishedOutOfContext(rig.runner.Session))
	collectHeld(t, rig, "t1")
	ran := rig.get("t1").SessionRan()

	ctx := t.Context()
	if _, err := rig.coordinator.OweEnd(ctx, "t1", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}); err != nil {
		t.Fatalf("OweEnd: %v", err)
	}
	if _, err := rig.coordinator.publishEnd(ctx, rig.get("t1")); err != nil {
		t.Fatalf("publishEnd: %v", err)
	}
	ends := writes.endedRecords()
	if len(ends) != 1 || !slices.Equal(ends[0].Ending.Exhausted, []string{ran}) {
		t.Fatalf("the end recorded %+v, want it naming session %q", ends, ran)
	}
	if u := ends[0].Ending.Unkept; u == nil || !strings.Contains(u.Reason, "ran out") {
		t.Fatalf("the end names %+v unkept, want the attempt that ran out, so the row names nothing older", u)
	}
}

// WHICH SESSIONS A JOB RAN OUT IN, in the row's word. Where the box's word never
// reached the row the job ran one of its launch's two sessions and both are named,
// since naming one could leave the session it really ran to be continued. The box's
// own id is used only for a harness that mints it, where the row names none, and
// never one that is not a session id's shape.
//
// Mutation: prefer result.SessionID over SessionRan, or drop ValidSessionID, and a
// case goes red.
func TestWhichSessionsAJobRanOutInAreTheRowsWord(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		run    PendingRun
		result Result
		want   []string
	}{
		"the session it named": {run: PendingRun{SessionID: "s-1"}, result: Result{SessionID: "s-forged"},
			want: []string{"s-1"}},
		"its box said it continued": {run: PendingRun{SessionID: "s-1",
			Launch: LaunchRecord{FreshSessionID: "s-2", SessionStart: SessionStartContinued}}, want: []string{"s-1"}},
		"its box said it started its own": {run: PendingRun{SessionID: "s-1",
			Launch: LaunchRecord{FreshSessionID: "s-2", SessionStart: SessionStartFresh}}, want: []string{"s-2"}},
		"its box never said which": {run: PendingRun{SessionID: "s-1",
			Launch: LaunchRecord{FreshSessionID: "s-2"}}, want: []string{"s-1", "s-2"}},
		"a harness that mints its own id": {result: Result{SessionID: "ses_abc"}, want: []string{"ses_abc"}},
		"an id of no session's shape":     {result: Result{SessionID: "../../etc"}, want: []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := sessionsRanOutOfContext(tc.run, tc.result); !slices.Equal(got, tc.want) {
				t.Errorf("the job ran out in %q, want %q", got, tc.want)
			}
		})
	}
}

// THE RUN'S RECORD IS BOUNDED, AND KEEPS WHAT A LATER LAUNCH COULD REACH: no id
// twice, and past the bound the first stays, the session the turn took on from its
// conversation, and the newest, the one a relaunch would otherwise continue.
//
// Mutation: drop the oldest at the bound, or trim nothing, and a check goes red.
func TestTheSessionsARunRanOutInAreBounded(t *testing.T) {
	t.Parallel()
	var held []string
	for _, session := range []string{"base", "a", "b", "c", "d", "e", "f", "g", "h", "i", "base", "i"} {
		held = withExhausted(held, []string{session})
	}
	if len(held) != executorkeep.ExhaustedMaxSessions {
		t.Fatalf("the record holds %d sessions, want the bound, %d: %q", len(held),
			executorkeep.ExhaustedMaxSessions, held)
	}
	if held[0] != "base" || held[len(held)-1] != "i" {
		t.Fatalf("the record holds %q, want the first and the newest kept", held)
	}
	if again := withExhausted(held, []string{"i"}); !slices.Equal(again, held) {
		t.Errorf("a second ask changed the record to %q", again)
	}
}
