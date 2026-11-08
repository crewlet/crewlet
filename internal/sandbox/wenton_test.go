package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain/executordomaintest"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// keptSave stamps on turnID's row, landed, a capture of its job taken while the
// job still worked — packed as it stood, as a capture a box would not end is:
// kept, and not final — failing the case unless the row keeps it.
func keptSave(t *testing.T, rig *coordRig, turnID string) SessionCapture {
	t.Helper()
	run := rig.get(turnID)
	saved := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
		State: CaptureLanded, Interrupted: true, At: rig.now, Harness: "claude-code", SessionID: "s-1",
		ArchiveFormat: 1, Object: objstore.KeyAt(rig.now).String(), ObjectHash: strings.Repeat("a", 64),
		ObjectSize: 1024, ContentKey: "k1"}
	if took, err := rig.pending.StampCapture(t.Context(), turnID, saved); err != nil || !took {
		t.Fatalf("stamping the capture taken as the job worked = %v, %v", took, err)
	}
	if got := rig.get(turnID); got.Kept == nil || !got.Kept.samePlace(saved) || got.Kept.Final {
		t.Fatalf("the premise: the row keeps %+v, want the capture taken as the job worked", got.Kept)
	}
	return saved
}

// lostWithItsBox marks run's job lost with its box, as the waiter records a
// job whose box went with its host ([LossBoxGone]), and makes that box gone
// from its provider: a loss no read of the box can take anything from.
func lostWithItsBox(t *testing.T, rig *coordRig, run PendingRun) {
	t.Helper()
	rig.provider.mu.Lock()
	rig.provider.Vanished[run.SandboxID] = true
	rig.provider.mu.Unlock()
	if marked, err := rig.pending.MarkLost(t.Context(), run.TurnID, run.LaunchID,
		JobLoss{Cause: LossBoxGone, At: rig.now}); err != nil || !marked {
		t.Fatalf("marking the job lost with its box = %v, %v", marked, err)
	}
}

// AN END OF A TURN THAT ENDED ON A SAVE SAYS IT WENT ON. The turn's row names a
// capture its job took while it still worked, and the turn went on past it: a
// stop whose read found the box gone, a third loss of the round whose box went
// with its host — which its turn's frame ends, since the round is not carried
// on again — and a loss whose box went with its host that the engine ended,
// its carry's resume broken before its turn began, take nothing more, so
// nothing newer was attempted and the end names no capture unkept. Said
// nowhere, the conversation's next turn would be told the session runs through
// the turn's end; the end says the turn went on past it, which the log keeps
// the session for and says.
//
// Mutation: decide went_on by whether an unkept capture rides the end, or by
// the kept capture's launch alone, and the end of a turn that kept its last
// launch's capture taken as it worked says nothing of the turn going on; have
// a loss's ending read a box its loss took with its host (capturesLoss blind
// to the loss's cause), and the premise of the loss the engine ended goes red.
func TestAnEndOfATurnThatEndedOnASaveSaysItWentOn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		outcome executorkeep.Outcome
		end     func(t *testing.T, rig *coordRig, run PendingRun)
	}{
		{name: "a stop whose read found its box gone", outcome: executorkeep.OutcomeStopped,
			end: func(t *testing.T, rig *coordRig, run PendingRun) {
				rig.provider.mu.Lock()
				rig.provider.Vanished[run.SandboxID] = true
				rig.provider.mu.Unlock()
				if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
					t.Fatalf("StopSeat: %v", err)
				}
			}},
		{name: "a third loss of its round whose box went with its host", outcome: executorkeep.OutcomeFailed,
			end: func(t *testing.T, rig *coordRig, run PendingRun) {
				// TWO OF THE ROUND'S JOBS WERE LOST BEFORE THIS ONE.
				if _, _, err := rig.pending.mutateAny(t.Context(), run.TurnID, func(r *PendingRun) bool {
					r.RoundLosses = MaxRoundLosses
					return true
				}); err != nil {
					t.Fatalf("counting the round's earlier losses: %v", err)
				}
				lostWithItsBox(t, rig, run)
				// THE TURN'S FRAME, told the round may not be carried on
				// again, closes its executor phase failed and owes the
				// turn's end as it returns.
				rig.resumer.during = func(ctx context.Context, r PendingRun) {
					if _, err := rig.coordinator.OweEnd(ctx, r.TurnID, executorkeep.Ending{
						Outcome: executorkeep.OutcomeFailed,
						Failure: "the executor's round was lost with its host too often to start again"}); err != nil {
						t.Errorf("OweEnd: %v", err)
					}
				}
				payload, ev := rig.completion(run.TurnID)
				if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
					t.Fatalf("OnCompleted: %v", err)
				}
				resumes := rig.resumer.calls()
				if len(resumes) != 1 || resumes[0].Lost == nil || resumes[0].Lost.BoxKept ||
					resumes[0].Lost.Losses <= resumes[0].Lost.Limit {
					t.Fatalf("the premise: the turn resumed %+v, want it told its round's third loss, "+
						"past the limit, with no box kept", resumes)
				}
			}},
		{name: "a loss the engine ended whose box went with its host", outcome: executorkeep.OutcomeLost,
			end: func(t *testing.T, rig *coordRig, run PendingRun) {
				lostWithItsBox(t, rig, run)
				rig.coordinator.finish(t.Context(), rig.get(run.TurnID), ending{fence: fenceOf(run),
					note: &failureNote{reason: types.ExecutorFailureResumeBroken, detail: resumeBrokenDetail}})
				if decided := rig.get(run.TurnID); !decided.lost() || rig.coordinator.capturesLoss(decided) {
					t.Fatalf("the premise: the ending is %+v, its loss %+v, want a loss whose ending reads "+
						"no box", decided.Ending, decided.Lost)
				}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rig, writes, _ := keepingRig(t)
			run := rig.launchKept(t, "t1")
			keptSave(t, rig, "t1")
			c.end(t, rig, run)
			// THE ENDING'S OWN RETRY, off the frame that decided it, which
			// waits for the turn's captures and records the end.
			for range 3 {
				if rig.fireRetries() == 0 {
					break
				}
			}

			ends := writes.endedRecords()
			if len(ends) != 1 || ends[0].Ending.Outcome != c.outcome {
				t.Fatalf("recorded %+v, want the turn's one end, %s", ends, c.outcome)
			}
			if end := ends[0].Ending; !end.WentOn || end.Unkept != nil {
				t.Errorf("the end says went on %v and names %+v unkept, want it saying the turn went on past "+
					"the capture its job took as it worked, and naming nothing unkept", end.WentOn, end.Unkept)
			}
			if takes := rig.runner.Takes(); len(takes) != 0 {
				t.Errorf("took %d sessions out of a box that was gone", len(takes))
			}
		})
	}
}

// AN END OF A TURN WHOSE LATER LAUNCH KEPT NOTHING SAYS IT WENT ON. The turn's
// first launch's final capture landed, and its row names it; a relaunch then
// ran a job that kept nothing newer — its box gone when the turn ended, or its
// own final capture refused — so the session the row names is an earlier
// launch's, and the turn went on past it: the end says so, naming the refused
// capture where there was one. A relaunch whose job never started worked no
// session past the first launch's, which is the turn's last word still, and
// its end says nothing of the kind.
//
// Mutation: decide the turn's last word on the kept capture's finality alone,
// and both relaunches that ran say nothing of the turn going on; decide it on
// the row's launch alone, and the relaunch that never started says the turn
// went on past the session it ended on.
func TestAnEndOfATurnWhoseLaterLaunchKeptNothingSaysItWentOn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		started bool
		refused bool
		wentOn  bool
	}{
		{name: "a relaunch whose box was gone when the turn ended", started: true, wentOn: true},
		{name: "a relaunch whose own final capture was refused", started: true, refused: true, wentOn: true},
		{name: "a relaunch whose job never started"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			rig, writes, _ := keepingRig(t)
			rig.launchKept(t, "t1")
			session := rig.runner.Session
			rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
			collectHeld(t, rig, "t1")
			first := rig.get("t1")
			if k := first.Kept; k == nil || !k.Final || k.Launch != first.LaunchID {
				t.Fatalf("the premise: the first launch keeps %+v, want its final capture", k)
			}

			// THE RELAUNCH, in the box the first launch left.
			relaunched, err := rig.pending.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe",
				AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease)
			if err != nil || relaunched.LaunchSeq != first.LaunchSeq+1 {
				t.Fatalf("relaunching = launch %d, %v; want launch %d", relaunched.LaunchSeq, err,
					first.LaunchSeq+1)
			}
			if c.started {
				if err := rig.pending.AttachSandbox(ctx, "t1", BoxRef{SandboxID: first.SandboxID,
					CommandID: "cmd-2", SessionID: "sess-t1-2", Harness: "claude-code",
					PauseTTLSec: DefaultPauseTTL.Seconds()}, rigLease); err != nil {
					t.Fatalf("starting the relaunch's job: %v", err)
				}
			}
			if c.refused {
				refused := SessionCapture{LaunchSeq: relaunched.LaunchSeq, CaptureSeq: 1,
					Launch: relaunched.LaunchID, State: CaptureRefused, Final: true,
					Reason: "the seat's budget is spent", At: rig.now}
				if took, err := rig.pending.StampCapture(ctx, "t1", refused); err != nil || !took {
					t.Fatalf("stamping the relaunch's refused capture = %v, %v", took, err)
				}
			}
			if started := rig.get("t1").Launch.Started; started != c.started {
				t.Fatalf("the premise: the relaunch's job started %v, want %v", started, c.started)
			}
			if _, err := rig.coordinator.OweEnd(ctx, "t1", executorkeep.Ending{
				Outcome: executorkeep.OutcomeFailed}); err != nil {
				t.Fatalf("OweEnd: %v", err)
			}
			if _, err := rig.coordinator.publishEnd(ctx, rig.get("t1")); err != nil {
				t.Fatalf("publishEnd: %v", err)
			}

			ends := writes.endedRecords()
			if len(ends) != 1 {
				t.Fatalf("recorded %d ends, want the turn's one", len(ends))
			}
			end := ends[0].Ending
			if end.WentOn != c.wentOn {
				t.Errorf("the end says went on %v, want %v", end.WentOn, c.wentOn)
			}
			switch u := end.Unkept; {
			case c.refused && (u == nil || u.LaunchSeq != relaunched.LaunchSeq || u.CaptureSeq != 1 ||
				!strings.Contains(u.Reason, "budget is spent")):
				t.Errorf("the end names %+v unkept, want the relaunch's refused capture and why", u)
			case !c.refused && u != nil:
				t.Errorf("the end names %+v unkept, want nothing: no capture was attempted after the kept one", *u)
			}
		})
	}
}

// AN END WITHOUT WENT_ON NEVER CARRIES AN UNKEPT CAPTURE NEWER THAN ITS ROW —
// IT CARRIES NONE AT ALL. A turn whose last launch's final capture was kept is
// a turn whose session was taken whole, and nothing of that job is taken
// again — its collection made again, after a resume that failed, finds it taken
// once it had ended — so its end says nothing of the turn going on and names
// nothing unkept. A turn whose final capture was refused names it unkept, and
// says the turn went on past whatever its row names: none, on its run's record,
// though the log may have taken a capture the record holds refused. So no end
// this build writes asks the log to take a session off for an unkept capture,
// which only an end stamped below version 4 does.
//
// Mutation: say the turn went on wherever it kept a capture, and the turn whose
// final capture landed says so; name the newest capture unkept where it
// landed, and the end names it beside the capture it kept; decide went_on on
// what the record kept alone, and the refused capture rides an end that says
// nothing of the turn going on.
func TestAnEndWithoutWentOnNeverCarriesAnUnkeptCaptureNewerThanItsRow(t *testing.T) {
	t.Parallel()
	for name, putErr := range map[string]error{"its final capture landed": nil,
		"its final capture refused": objstore.ErrFull} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig, writes, keep := keepingRig(t)
			keep.putErr = putErr
			rig.launchKept(t, "t1")
			session := rig.runner.Session
			rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
			collectHeld(t, rig, "t1")
			// THE SAME JOB READ AGAIN, as a collection retried after a resume
			// that failed reads it.
			collectHeld(t, rig, "t1")
			ctx := t.Context()
			if _, err := rig.coordinator.OweEnd(ctx, "t1", executorkeep.Ending{
				Outcome: executorkeep.OutcomeCompleted}); err != nil {
				t.Fatalf("OweEnd: %v", err)
			}
			run := rig.get("t1")
			if _, err := rig.coordinator.publishEnd(ctx, run); err != nil {
				t.Fatalf("publishEnd: %v", err)
			}

			ends := writes.endedRecords()
			if len(ends) != 1 {
				t.Fatalf("recorded %d ends, want the turn's one", len(ends))
			}
			end := ends[0].Ending
			if !end.WentOn && end.Unkept != nil {
				t.Errorf("the end names %+v unkept and says nothing of the turn going on: the rule of an end "+
					"stamped below version 4 would take an older session off the row", *end.Unkept)
			}
			switch {
			case putErr == nil && (run.Kept == nil || !run.Kept.Final || end.Unkept != nil || end.WentOn):
				t.Errorf("the row keeps %+v and the end names %+v unkept, went on %v; want the final capture "+
					"kept, and the end saying nothing of the turn going on", run.Kept, end.Unkept, end.WentOn)
			case putErr != nil && (run.Kept != nil || end.Unkept == nil || !end.WentOn):
				t.Errorf("the row keeps %+v and the end names %+v unkept, went on %v; want nothing kept, the "+
					"refused capture named, and the end saying the turn went on", run.Kept, end.Unkept,
					end.WentOn)
			}
			if takes := rig.runner.Takes(); len(takes) != 1 {
				t.Errorf("took the job's session %d times, want once", len(takes))
			}
		})
	}
}

// A CAPTURE THE LOG TOOK AFTER A RELAUNCH SETTLED IT STAYS THE TURN'S SESSION
// THROUGH ITS END. The first launch's final capture was taken by node-a's
// process, which handed its record on and stopped before the log took it; the
// relaunch's wait for the job it follows settled it refused, its taker gone,
// and only then did the log take the record — so the turn's row names it,
// though the run's record kept nothing. The relaunch started without it, and
// its own final capture was refused. The end names that refused capture
// unkept, newer than the row's place, and says the turn went on past whatever
// its row names: the shipped applier, applying the end by the rule of the
// version its went_on is stamped at, keeps the late capture on the row; the
// row reads as one its turn went on past, beside the newer capture that was
// not kept; and the conversation's next turn is handed it. DRIVEN THROUGH THE
// SHIPPED APPLIER AND READER (executordomaintest).
//
// Mutation: decide went_on on what the run's record kept alone, and the end is
// stamped below version 4 and takes the late capture off the row, the
// conversation's next turn handed none; answer Session.WentOn off the
// lost outcome alone, and the row reads as the turn's whole session.
func TestACaptureTheLogTookAfterARelaunchSettledItStaysTheTurnsSession(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTakerStore()
	rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
	applied := &appliedLog{log: executordomaintest.Open(t)}
	rig.coordinator.sessions, rig.coordinator.restore = applied, applied
	// THE SHIPPED RECORD BUDGET: every record here is applied to a real store,
	// which a loaded runner can hold past the rig's fifth of a second.
	rig.coordinator.recordBudget = sessionRecordBudget
	turn := uuid.NewString()
	run := rig.launchKept(t, turn)

	// node-a's PROCESS TOOK THE JOB'S FINAL CAPTURE under its taker lease,
	// uploaded it and sent its record, and stopped before the log took it.
	lease := claimTaker(t, store, takingProcess)
	begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
		State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
		TakerEpoch: lease.Epoch, Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1",
		ArchiveFormat: FakeArchiveFormat, Through: new(0), SeatBudget: 64 << 20}
	if took, err := rig.pending.StampCapture(ctx, turn, begun); err != nil || !took {
		t.Fatalf("stamping node-a's begun capture = %v, %v", took, err)
	}
	body := []byte("node-a's session of the job")
	object := objstore.Object{Key: objstore.KeyAt(begun.At), Hash: objstore.HashOf(body), Size: int64(len(body))}
	handedOn := begun
	handedOn.Object, handedOn.ObjectHash, handedOn.ObjectSize = object.Key.String(), string(object.Hash), object.Size
	handedOn.ContentKey = "k1"
	late := &recordedLate{PendingStore: rig.coordinator.pending, land: func(ctx context.Context) (LogAnswer, error) {
		return applied.Capture(ctx, CaptureRecord{Handle: run.AgentHandle, Turn: turn,
			ConversationKey: run.ConversationKey, WorkKey: run.WorkKey, Object: object, Stamp: handedOn})
	}}
	rig.coordinator.pending = late
	lapse(store)

	// THE RELAUNCH'S WAIT FOR THE JOB IT FOLLOWS settles node-a's capture, and
	// the late record lands after it.
	if _, found, err := rig.coordinator.awaitCapture(ctx, rig.get(turn), rig.coordinator.relaunchWait); err != nil ||
		!found {
		t.Fatalf("the relaunch's wait = found %v, %v", found, err)
	}
	switch settled, landed, landErr := late.late(); {
	case settled == nil || settled.State != CaptureRefused || !settled.sameTake(begun):
		t.Fatalf("the premise: the settle stamped %+v, want node-a's begun take refused", settled)
	case landErr != nil || landed != LogLanded:
		t.Fatalf("the premise: the late record = %s, %v; want the log to take it after the settle", landed,
			landErr)
	}
	if kept := rig.get(turn).Kept; kept != nil {
		t.Fatalf("the premise: the run's record keeps %+v, want nothing: it holds the capture refused", kept)
	}

	// THE RELAUNCH, which started without it: its job ran, and its own final
	// capture was refused.
	relaunched, err := rig.pending.BeginLaunch(ctx, PendingRun{TurnID: turn, AgentHandle: "swe",
		AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease)
	if err != nil {
		t.Fatalf("relaunching: %v", err)
	}
	if err := rig.pending.AttachSandbox(ctx, turn, BoxRef{SandboxID: run.SandboxID, CommandID: "cmd-2",
		SessionID: "sess-2", Harness: "claude-code", PauseTTLSec: DefaultPauseTTL.Seconds()},
		rigLease); err != nil {
		t.Fatalf("starting the relaunch's job: %v", err)
	}
	refused := SessionCapture{LaunchSeq: relaunched.LaunchSeq, CaptureSeq: 1, Launch: relaunched.LaunchID,
		State: CaptureRefused, Final: true, Reason: "the object store's reservation is spent",
		At: rig.coordinator.now()}
	if took, err := rig.pending.StampCapture(ctx, turn, refused); err != nil || !took {
		t.Fatalf("stamping the relaunch's refused capture = %v, %v", took, err)
	}
	if _, err := rig.coordinator.OweEnd(ctx, turn, executorkeep.Ending{
		Outcome: executorkeep.OutcomeCompleted}); err != nil {
		t.Fatalf("OweEnd: %v", err)
	}
	if _, err := rig.coordinator.publishEnd(ctx, rig.get(turn)); err != nil {
		t.Fatalf("publishEnd: %v", err)
	}

	ends := applied.ended()
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends, want the turn's one", len(ends))
	}
	if end := ends[0].Ending; !end.WentOn || end.Unkept == nil ||
		end.Unkept.LaunchSeq != relaunched.LaunchSeq || end.Unkept.CaptureSeq != 1 {
		t.Fatalf("the end says went on %v and names %+v unkept, want it saying the turn went on, beside the "+
			"relaunch's refused capture", end.WentOn, end.Unkept)
	}
	read, err := applied.log.Session(ctx, run.AgentHandle, turn)
	if err != nil {
		t.Fatalf("the next read of the turn's row: %v", err)
	}
	row := read.Session
	switch u := row.Unkept(); {
	case !row.Ended() || !row.HasObject() || row.Object.Key.String() != object.Key.String() ||
		row.LaunchSeq != begun.LaunchSeq || row.CaptureSeq != begun.CaptureSeq:
		t.Fatalf("the turn's row is %+v, want it ended and still naming the capture the log took late", row)
	case !row.WentOn():
		t.Errorf("the row reads as its turn's whole session: %+v", row.Ending)
	case u == nil || u.LaunchSeq != relaunched.LaunchSeq:
		t.Errorf("the row stands %+v unkept, want the relaunch's refused capture beside the session it keeps", u)
	case row.NotKept() != "":
		t.Errorf("the row that keeps its session says %q", row.NotKept())
	}
	head, err := applied.log.Head(ctx, run.AgentHandle, run.ConversationKey)
	if err != nil || head.Session == nil || head.Session.Turn != turn ||
		head.Session.Object.Key.String() != object.Key.String() {
		t.Errorf("the conversation's next turn is handed %+v (%v), want the capture the log took late",
			head.Session, err)
	}
}
