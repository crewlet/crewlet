package sandbox

import (
	"strings"
	"testing"

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

// AN END OF A TURN THAT ENDED ON A SAVE SAYS IT WENT ON. The turn's row names a
// capture its job took while it still worked, and the turn went on past it: a
// stop whose read found the box gone, and a loss whose box went with its host,
// take nothing more, so nothing newer was attempted and the end names no
// capture unkept. Said nowhere, the conversation's next turn would be told the
// session runs through the turn's end; the end says the turn went on past it,
// which the log keeps the session for and every reader is told.
//
// Mutation: decide went_on by whether an unkept capture rides the end, or by
// the kept capture's launch alone, and the end of a turn that kept its last
// launch's capture taken as it worked says nothing of the turn going on.
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
		{name: "a loss whose box went with its host", outcome: executorkeep.OutcomeLost,
			end: func(t *testing.T, rig *coordRig, run PendingRun) {
				rig.coordinator.finish(t.Context(), rig.get(run.TurnID), ending{fence: fenceOf(run),
					note: &failureNote{reason: string(LossBoxGone), detail: "the box is gone from its host"}})
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

// AN END WITHOUT WENT_ON NEVER CARRIES AN UNKEPT CAPTURE NEWER THAN ITS ROW. A
// turn whose last launch's final capture was taken is a turn whose session was
// taken whole, and nothing of that job is taken again — its collection made
// again, after a resume that failed, finds it taken once it had ended. So its
// end says nothing of the turn going on past its session, and names an unkept
// capture only where none was kept — the final capture refused, the row naming
// none to release. An end stamped below version 4 takes the row's session off
// for an unkept capture newer than it: this build's ends never ask it to.
//
// Mutation: say the turn went on wherever it kept a capture, and the turn whose
// final capture landed says so; name the newest capture unkept where it
// landed, and the end names it beside the capture it kept.
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
			if end.WentOn {
				t.Errorf("the end of a turn whose last launch's final capture was taken says the turn went on "+
					"past %+v", run.Kept)
			}
			if run.Kept != nil && end.Unkept != nil {
				t.Errorf("the end names %+v unkept beside %+v kept and says nothing of the turn going on: the "+
					"rule of an end stamped below version 4 would take that session off the row", *end.Unkept,
					*run.Kept)
			}
			switch {
			case putErr == nil && (run.Kept == nil || !run.Kept.Final || end.Unkept != nil):
				t.Errorf("the premise: the row keeps %+v and the end names %+v unkept, want the final capture "+
					"kept and nothing unkept", run.Kept, end.Unkept)
			case putErr != nil && (run.Kept != nil || end.Unkept == nil):
				t.Errorf("the premise: the row keeps %+v and the end names %+v unkept, want nothing kept and "+
					"the refused capture named", run.Kept, end.Unkept)
			}
			if takes := rig.runner.Takes(); len(takes) != 1 {
				t.Errorf("took the job's session %d times, want once", len(takes))
			}
		})
	}
}
