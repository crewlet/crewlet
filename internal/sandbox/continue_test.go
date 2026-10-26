package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// askAndAnswer launches t1, has its job park on a question in the box it ran
// in — its CLI's session named — with one bridged call made before it asked,
// and answers it, running relaunch inside the resume the answer drives, as a
// turn that carries the run on does.
func askAndAnswer(t *testing.T, rig *coordRig, reap bool,
	relaunch func(ctx context.Context, r PendingRun, asked *AskedRun),
) PendingRun {
	t.Helper()
	run := rig.launch("t1")
	if _, err := rig.pending.AppendBridgeCall(t.Context(), "t1", BridgeAppend{
		Launch: run.LaunchID, Call: BridgeCall{Name: "post_comment", Args: `{"text":"on it"}`},
	}); err != nil {
		t.Fatalf("AppendBridgeCall: %v", err)
	}
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
		WIPBranch: "wip/t1", SessionID: "sess-ask",
	})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if reap {
		rig.now = rig.now.Add(DefaultPauseTTL + time.Second)
		rig.tick()
	}
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		calls := rig.resumer.calls()
		relaunch(ctx, r, calls[len(calls)-1].Asked)
	}
	if _, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", chatReply(answerOnTheDM, "use main", nil)); err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	return run
}

// continuing is the launch a turn carrying an asking run on makes: the round's
// own brief, and the asking run's session with the answer as its next prompt.
func continuing(asked *AskedRun) LaunchRequest {
	req := launchReq("t1")
	req.Brief = "the round's brief, with the question and the answer"
	req.Continue = &Continuation{SessionID: asked.SessionID, Brief: "the answer: " + asked.Answer}
	return req
}

// AN ANSWERED RUN CARRIES ON IN ITS OWN CONVERSATION, IN ITS OWN BOX. The run
// that asked knew what it had read, tried and decided, and all of that is in
// its CLI's session in the box it asked from — so the relaunch continues that
// session there, with the answer as its next prompt, rather than starting a
// conversation told only the question and the answer. And the round goes on:
// the call the run made before it asked is still the round's, on the row.
//
// Mutation: start the continued job on the round's brief with no session, or
// empty the bridged log on the relaunch's reset, and this goes red.
func TestAnAnsweredRunCarriesOnInItsOwnSessionInItsOwnBox(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	var relaunched LaunchResult
	run := askAndAnswer(t, rig, false, func(ctx context.Context, r PendingRun, asked *AskedRun) {
		if asked == nil || asked.SessionID != "sess-ask" || !asked.BoxKept {
			t.Errorf("the resume carries %+v, want the asking run's session and its kept box", asked)
			return
		}
		res, err := rig.launchVia(ctx, rig.manager, continuing(asked))
		if err != nil {
			t.Errorf("relaunch: %v", err)
		}
		relaunched = res
		if row := rig.getIn(ctx, "t1"); len(row.BridgeCalls) != 1 || row.BridgeCalls[0].Name != "post_comment" {
			t.Errorf("the carried-on round's log holds %+v, want the call made before the question",
				row.BridgeCalls)
		}
	})
	if !relaunched.Reused || relaunched.SandboxID != run.SandboxID {
		t.Fatalf("relaunch = %+v, want it in the asking run's box %q", relaunched, run.SandboxID)
	}
	started := rig.runner.Started()
	last := started[len(started)-1]
	if last.ResumeSession != "sess-ask" || last.Brief != "the answer: use main" {
		t.Fatalf("the carried-on job started with session %q and brief %q, want the asking "+
			"run's session and the answer", last.ResumeSession, last.Brief)
	}
}

// A RUN WHOSE BOX WAS RECLAIMED WHILE ITS QUESTION WAITED CARRIES ON FRESH.
// Its session lived in that box, so there is nothing to continue: the job
// starts a conversation of its own on the round's brief — which tells it the
// question, the answer and the branch — in a fresh box.
func TestARunWhoseBoxWasReclaimedCarriesOnFresh(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	var relaunched LaunchResult
	run := askAndAnswer(t, rig, true, func(ctx context.Context, r PendingRun, asked *AskedRun) {
		if asked == nil || asked.BoxKept {
			t.Errorf("the resume carries %+v, want a box that is gone", asked)
			return
		}
		res, err := rig.launchVia(ctx, rig.manager, continuing(asked))
		if err != nil {
			t.Errorf("relaunch: %v", err)
		}
		relaunched = res
	})
	if relaunched.Reused || relaunched.SandboxID == run.SandboxID {
		t.Fatalf("relaunch = %+v, want a fresh box", relaunched)
	}
	started := rig.runner.Started()
	last := started[len(started)-1]
	if last.ResumeSession != "" || !strings.Contains(last.Brief, "the question and the answer") {
		t.Fatalf("the fresh job started with session %q and brief %q, want no session and "+
			"the round's brief", last.ResumeSession, last.Brief)
	}
}

// WHAT A PERSON'S ANSWER CARRIES THE RUN ON WITH IS REDACTED, like everything a
// resume hands the turn: the turn publishes it and hands it to the run.
func TestAnAnsweredQuestionIsRedacted(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	secret := "ghp_" + strings.Repeat("c", 36)
	rig.launch("t1")
	rig.runner.Finish(Result{NeedsInput: true, Question: "may I use " + secret + "?", AskTo: "requester"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if _, err := rig.coordinator.TryResumeFromAnswer(
		t.Context(), "swe", chatReply(answerOnTheDM, "yes, "+secret, nil)); err != nil {
		t.Fatalf("TryResumeFromAnswer: %v", err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want 1", len(calls))
	}
	if asked := askedOf(calls[0]); asked.Question == "" ||
		strings.Contains(asked.Question+asked.Answer, secret) {
		t.Fatalf("a credential reached the carried-on run: %+v", asked)
	}
}
