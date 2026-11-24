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

// spentAs is session spent by loss, as a run's record keeps it.
func spentAs(session string, loss executorkeep.Loss) []executorkeep.SpentSession {
	return []executorkeep.SpentSession{{Session: session, Loss: loss}}
}

// A session that ran out of context is continued by nothing: the collection records
// it, each way a later launch could take it up refuses it, and the end carries it on.

const ranOutWords = "ran out of its model's context"

// finishedOutOfContext is the result of a job its harness says ran out of context.
func finishedOutOfContext(session *FakeSession) Result {
	return Result{Error: "Prompt is too long", ContextExhausted: true, SessionID: "s-1",
		Captured: &session.Archive}
}

// A job that ran out is recorded and uploads nothing; one that failed otherwise keeps its session.
// Mutation: drop the stamp or at.exhausted, or judge on ContextExhausted alone, and it goes red.
func TestAJobThatRanOutOfContextIsRecordedAndKeepsNoCapture(t *testing.T) {
	t.Parallel()
	t.Run("it ran out", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, keep := keepingRig(t)
			rig.launchKept(t, "t1")
			rig.runner.Finish(finishedOutOfContext(rig.runner.Session))
			collectHeld(t, rig, "t1")

			run := rig.get("t1")
			if want := spentAs(run.SessionRan(), executorkeep.LossContextExhausted); want[0].Session == "" ||
				!slices.Equal(run.SpentSessions, want) {
				t.Fatalf("the row says %+v ran out, want the session the job ran, %+v", run.SpentSessions, want)
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
	})
	t.Run("it failed any other way", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep := keepingRig(t)
			rig.launchKept(t, "t1")
			session := rig.runner.Session
			rig.runner.Finish(Result{Error: "exit 1", SessionID: "s-1", Captured: &session.Archive})
			collectHeld(t, rig, "t1")

			run := rig.get("t1")
			if len(run.SpentSessions) != 0 {
				t.Errorf("a launch_failed run retired %+v", run.SpentSessions)
			}
			if run.Kept == nil || len(keep.uploads()) != 1 {
				t.Errorf("a launch_failed run kept %+v, with %d uploads, want its session kept", run.Kept,
					len(keep.uploads()))
			}
			if recs := rig.collected(); len(recs) != 1 || recs[0].ErrorKind != types.ExecutorErrorLaunchFailed {
				t.Errorf("the launch's record is %+v, want launch_failed", recs)
			}
		})
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

// A run its turn's cap cut off that asked on its way out and ran out of context keeps no capture
// either: its read judges it as its collection records it, its question dropped, and the
// collection retires the session it ran out in.
// Mutation: judge it on the row as claimed, its question still on it, and its session is taken
// and kept by the read that the collection then retires.
func TestACappedRunThatAskedAndRanOutOfContextKeepsNoCapture(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, keep := keepingRig(t)
		rig.launchUntil("t1", rig.now.Add(-time.Minute))
		rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
		rig.coordinator.countRun("swe", StatusRunning)
		rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Decision: choosing("which branch?"),
			Branch: "wip/t1"})
		ranOut := finishedOutOfContext(rig.runner.Session)
		ranOut.JobEnd = JobEndAsked
		rig.runner.EndsAs(ranOut)
		// PAST ITS TURN'S CAP a resume that fails is the turn's last: the run is ended,
		// and its row stays, owing its turn's end, for the case to read.
		rig.resumer.failWith(errors.New("held for the case to read the row"))
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Logf("OnCompleted: %v", err)
		}
		rig.awaitKeeps()

		run := rig.get("t1")
		if !slices.Equal(run.SpentSessions, spentAs("s-1", executorkeep.LossContextExhausted)) {
			t.Fatalf("the row says %+v ran out, want the session the job ran, s-1", run.SpentSessions)
		}
		if takes := rig.runner.Takes(); len(takes) != 0 || run.Kept != nil || len(keep.uploads()) != 0 ||
			len(writes.captured()) != 0 {
			t.Fatalf("took %+v and kept %+v, with %d uploads and %d records: want nothing taken of a session "+
				"no launch continues", takes, run.Kept, len(keep.uploads()), len(writes.captured()))
		}
		if run.Capture == nil || run.Capture.State != CaptureRefused {
			t.Fatalf("the row holds capture %+v, want it refused untaken, so the turn's end names it", run.Capture)
		}
		if recs := rig.collected(); len(recs) != 1 || recs[0].ErrorKind != types.ExecutorErrorContextExhausted {
			t.Errorf("the launch's record is %+v, want it to say the job ran out of context", recs)
		}
	})
}

// A store that does not take the fact is a collection to ask again, never a resumed turn.
// Mutation: log the write's failure and carry on, and the collection answers.
func TestAFactTheStoreDidNotTakeHandsTheCollectionBack(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		rig.launchKept(t, "t1")
		rig.runner.Finish(finishedOutOfContext(rig.runner.Session))
		rig.coordinator.pending = &refusesRetirement{PendingStore: rig.pending}
		if _, _, collected, _ := rig.coordinator.collectClaimed(t.Context(), rig.get("t1")); collected {
			t.Fatal("the collection answered with the session's failure unrecorded")
		}
	})
}

// refusesRetirement is a run store that does not take a job's running out of context.
type refusesRetirement struct{ PendingStore }

func (*refusesRetirement) RetireSessions(context.Context, string, executorkeep.Loss, []string) error {
	return errors.New("the store is having a bad moment")
}

// A relaunch continues no kept capture of a session that ran out, and says why.
// Mutation: drop the check from ContinuityOf and the relaunch continues it.
func TestARelaunchContinuesNoKeptCaptureOfASessionThatRanOut(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, restore, kept := keptTurn(t)
		if continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, "")); why != "" ||
			!continued.Capture.samePlace(kept) {
			t.Fatalf("the premise: the relaunch continues %+v (%q), want the kept capture", continued, why)
		}
		if err := rig.pending.RetireSessions(t.Context(), "t1", executorkeep.LossContextExhausted, []string{"s-1"}); err != nil {
			t.Fatalf("RetireSessions: %v", err)
		}
		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
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
	})
}

// A turn that kept nothing does not go back to the base that ran out.
// Mutation: drop the check from BaseContinuityOf and the relaunch continues the base.
func TestARelaunchContinuesNoBaseThatRanOut(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, logbook, _ := conversingRig(t)
		read := frozenReadOf(logbook, logbook.endedTurn(t, "b1", "s-base", "the base's transcript"))
		ctx := t.Context()
		if _, err := rig.coordinator.Launch(ctx, rig.manager, continuingBase("t2", read)); err != nil {
			t.Fatalf("the first launch: %v", err)
		}
		if continued, why := rig.coordinator.ContinuedCapture(ctx, "t2", RestorerUnder(rig.runner, "")); continued.Base == nil {
			t.Fatalf("the premise: the relaunch continues no base (%q)", why)
		}
		if err := rig.pending.RetireSessions(ctx, "t2", executorkeep.LossContextExhausted, []string{"s-base"}); err != nil {
			t.Fatalf("RetireSessions: %v", err)
		}
		continued, why := rig.coordinator.ContinuedCapture(ctx, "t2", RestorerUnder(rig.runner, ""))
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
	})
}

// A relaunch is still told of a base its turn ran out in, while the turn's row holds it: no
// launch continues that session, but it is kept until the turn's end retires it, so the run
// that starts a session of its own beside it may read what it held (ContinuedSession.KeptTurn).
// Mutation: make running out of context in the base one of baseStanding's reasons, and the
// relaunch names no kept session.
func TestARelaunchIsToldOfABaseItRanOutInWhileItsRowHoldsIt(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, logbook, _ := conversingRig(t)
		read := frozenReadOf(logbook, logbook.endedTurn(t, "b1", "s-base", "the base's transcript"))
		ctx := t.Context()
		if _, err := rig.coordinator.Launch(ctx, rig.manager, continuingBase("t2", read)); err != nil {
			t.Fatalf("the first launch: %v", err)
		}
		if err := rig.pending.RetireSessions(ctx, "t2", executorkeep.LossContextExhausted, []string{"s-base"}); err != nil {
			t.Fatalf("RetireSessions: %v", err)
		}
		continued, why := rig.coordinator.ContinuedCapture(ctx, "t2", RestorerUnder(rig.runner, ""))
		if continued.Base != nil || !strings.Contains(why, ranOutWords) {
			t.Fatalf("the premise: the relaunch continues base %+v (%q), want none, saying it ran out",
				continued.Base, why)
		}
		if continued.KeptTurn != "b1" {
			t.Errorf("the relaunch names kept session %q, want b1: the turn's row still holds the base it ran "+
				"out in, which its run may read", continued.KeptTurn)
		}
	})
}

// continuingBaseRequest is a relaunch of turn asked to continue its frozen base.
func continuingBaseRequest(turn string) LaunchRequest {
	req := launchReq(turn)
	req.Brief = "the fresh brief"
	req.Session = &SessionContinuation{Base: true, Replaced: "a new turn begins here"}
	return req
}

// A relaunch continues no copy in its box of a session that ran out; a later session carries on.
// Mutation: drop the check from continueSession and the first case continues it.
func TestARelaunchContinuesNoCopyInItsBoxOfASessionThatRanOut(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		exhausted []executorkeep.SpentSession
		want      BriefKind
		session   string
	}{
		"the session it ran out in": {exhausted: spentAs("sess-own", executorkeep.LossContextExhausted),
			want: BriefFresh},
		"a session started after it": {exhausted: spentAs("sess-older", executorkeep.LossContextExhausted),
			want: BriefContinued, session: "sess-own"},
		"no session ran out": {want: BriefContinued, session: "sess-own"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			req := launchReq("t1")
			req.Brief = "the fresh brief"
			req.Continue = &Continuation{SessionID: "sess-own", Brief: "carry on in the box you left"}
			opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2, SpentSessions: tc.exhausted}
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

// A session started after running out is continued as any other.
// Mutation: refuse on any exhausted session rather than the row's and the job that worked starts over.
func TestAFreshSessionAfterRunningOutIsContinued(t *testing.T) {
	t.Parallel()
	kept := SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: CaptureLanded, SessionID: "s-2", Through: new(0)}
	row := executorkeep.TurnRow{SessionID: "s-2", LaunchSeq: 2, CaptureSeq: 1, Object: keptRow(t,
		landedAt(t)).Object}
	spent := spentAs("s-1", executorkeep.LossContextExhausted)
	run := PendingRun{TurnID: "t1", Capture: &kept, Kept: &kept, SpentSessions: spent}
	if got := ContinuityOf(row, &run, 2, allRestores{}); !got.Continues {
		t.Fatalf("the relaunch does not continue the session that worked: %s", got.Why)
	}
	base := BaseSession{Turn: "b1", Object: "o", SessionID: "s-other", Harness: "claude-code"}
	if got := BaseContinuityOf(base, nil, &PendingRun{TurnID: "t1", SpentSessions: spent},
		allRestores{}); !got.Continues {
		t.Fatalf("a base no job ran out in is refused: %s", got.Why)
	}
}

// A LAUNCH THAT PASSES A SPENT SESSION OVER SAYS HOW IT WAS SPENT, as the run's
// record took it, whatever the turn's newest capture says: one its provider
// refused beside a newer capture naming no refusal, or none since, is
// request_refused, and one that ran out beside a newer capture its provider
// refused is context_exhausted.
//
// Mutation: read the loss off the newest capture, or say every spent session ran
// out, and a case goes red.
func TestASpentSessionIsSaidAsItWasSpent(t *testing.T) {
	t.Parallel()
	landed := SessionCapture{LaunchSeq: 3, CaptureSeq: 1, State: CaptureLanded, SessionID: "s-2", Through: new(0)}
	refused := SessionCapture{LaunchSeq: 3, CaptureSeq: 1, State: CaptureRefused, SessionID: "s-2",
		Loss: executorkeep.LossRequestRefused}
	base := BaseSession{Turn: "b1", Object: "o", SessionID: "s-1", Harness: "claude-code"}
	for name, tc := range map[string]struct {
		spent  executorkeep.Loss
		newest *SessionCapture
	}{
		"refused, a newer capture naming no refusal": {executorkeep.LossRequestRefused, &landed},
		"refused, no capture since":                  {executorkeep.LossRequestRefused, nil},
		"out of context, a newer capture refused":    {executorkeep.LossContextExhausted, &refused},
	} {
		run := &PendingRun{TurnID: "t1", Capture: tc.newest, SpentSessions: spentAs("s-1", tc.spent)}
		if got := BaseContinuityOf(base, nil, run, allRestores{}); got.Continues || got.Loss != tc.spent {
			t.Errorf("%s: the base is answered %+v, want it not continued for %s", name, got, tc.spent)
		}
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

func (allRestores) Restores(SessionArchive) SessionContinuity { return continues }

// A reader is told what the launch would do, in the box as well.
// Mutation: drop the check from inParkedBox and the parked case says it continues.
func TestAReaderIsToldASessionThatRanOutIsNotContinued(t *testing.T) {
	t.Parallel()
	now := time.Now()
	kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, SessionID: "s-1", Through: new(0)}
	row := executorkeep.TurnRow{Harness: "claude-code", SessionID: "s-1", LaunchSeq: 1, CaptureSeq: 1,
		Object: keptRow(t, landedAt(t)).Object}
	target := RelaunchTarget{Harness: "claude-code", LaunchRouted: true, Placement: "local", Node: "node-a", Restorer: allRestores{}}
	run := PendingRun{TurnID: "t1", Status: StatusAwaiting, LaunchSeq: 1, SessionID: "s-1", SandboxID: "box-1",
		Placement: "local", Harness: "claude-code", LaunchNode: "node-a", BoxSince: now,
		Capture: &kept, Kept: &kept, SpentSessions: spentAs("s-1", executorkeep.LossContextExhausted)}

	got := ReadContinuityOf(row, "", &run, target, ContinuedByNextTurn, now)
	if got.Continues || !got.Decided || !strings.Contains(got.Why, ranOutWords) {
		t.Fatalf("a reader is told %+v, want it decided, not continued, saying the session ran out", got)
	}
	if box, why := inParkedBox(run, row, target, now); box != boxNot || !strings.Contains(why, ranOutWords) {
		t.Fatalf("the parked box is answered (%v, %q), want it not continued, saying why", box, why)
	}
}

// The turn's end carries the fact to the log, since the run's record goes with the turn,
// and how each session was spent, which the next turn says.
// Mutation: drop the assignment in publishEnd, or carry the ids without their losses, and
// no later turn hears of it, or hears a guess.
func TestATurnsEndNamesTheSessionsItSpentAndHow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		finish func(*FakeSession) Result
		loss   executorkeep.Loss
		why    string
	}{
		"out of context": {finishedOutOfContext, executorkeep.LossContextExhausted, "ran out"},
		"refused by its provider": {func(s *FakeSession) Result {
			return Result{Error: "API Error: 400", RequestRefused: true, SessionID: "s-1", Captured: &s.Archive}
		}, executorkeep.LossRequestRefused, "refused"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, _ := keepingRig(t)
				rig.launchKept(t, "t1")
				rig.runner.Finish(tc.finish(rig.runner.Session))
				collectHeld(t, rig, "t1")
				ran := rig.get("t1").SessionRan()

				ctx := t.Context()
				if _, err := rig.coordinator.OweEnd(ctx, "t1",
					executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}); err != nil {
					t.Fatalf("OweEnd: %v", err)
				}
				if _, err := rig.coordinator.publishEnd(ctx, rig.get("t1")); err != nil {
					t.Fatalf("publishEnd: %v", err)
				}
				ends := writes.endedRecords()
				if len(ends) != 1 || !slices.Equal(ends[0].Ending.SpentSessions, spentAs(ran, tc.loss)) {
					t.Fatalf("the end recorded %+v, want it naming session %q spent by %s", ends, ran, tc.loss)
				}
				if u := ends[0].Ending.Unkept; u == nil || !strings.Contains(u.Reason, tc.why) {
					t.Fatalf("the end names %+v unkept, want the attempt spent, so the row names nothing older", u)
				}
			})
		})
	}
}

// Which sessions a job ran out in is the row's word, and both where the box never said which.
// Mutation: prefer result.SessionID over SessionRan, or drop ValidSessionID, and a case goes red.
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

// The run's record is bounded, with no id twice, and keeps the first and the newest.
// Mutation: drop the oldest at the bound, or trim nothing, and a check goes red.
func TestTheSessionsARunRanOutInAreBounded(t *testing.T) {
	t.Parallel()
	var held []executorkeep.SpentSession
	for _, session := range []string{"base", "a", "b", "c", "d", "e", "f", "g", "h", "i", "base", "i"} {
		held = withSpent(held, executorkeep.LossContextExhausted, []string{session})
	}
	if len(held) != executorkeep.SpentMaxSessions {
		t.Fatalf("the record holds %d sessions, want the bound, %d: %+v", len(held),
			executorkeep.SpentMaxSessions, held)
	}
	if held[0].Session != "base" || held[len(held)-1].Session != "i" {
		t.Fatalf("the record holds %+v, want the first and the newest kept", held)
	}
	if again := withSpent(held, executorkeep.LossRequestRefused, []string{"i"}); !slices.Equal(again, held) {
		t.Errorf("a second ask changed the record to %+v, want it as first spent", again)
	}
}

// A LAUNCH ON A HARNESS THAT CONTINUES NO SESSION STARTS ONE OF ITS OWN, handed
// the fresh brief, whatever session its box or its turn keeps: handed one to
// continue, its runner would refuse the run. The fake stands in for such a
// harness ([freshStarter]).
//
// Mutations, each red: drop the check from continueSession (the box's copy is
// continued); read StartsFresh as false for every runner.
func TestALaunchOnAHarnessThatContinuesNoSessionStartsItsOwn(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	runner := NewFakeRunner("fresh-harness")
	runner.Fresh = true
	req := launchReq("t1")
	req.Brief = "the fresh brief"
	req.Continue = &Continuation{SessionID: "sess-own", Brief: "carry on in the box you left"}
	opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2}
	got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"), runner, true, false)
	t.Cleanup(got.release)
	if got.kind != BriefFresh || got.session != "" || got.brief != "the fresh brief" {
		t.Fatalf("the launch hands %s on session %q with %q, want the fresh brief on a session of its own",
			got.kind, got.session, got.brief)
	}
}
