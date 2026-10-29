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
	return askAndAnswerAfter(t, rig, reap,
		[]BridgeCall{{Name: "post_comment", Args: `{"text":"on it"}`}}, relaunch)
}

// askAndAnswerAfter is [askAndAnswer] with the bridged calls the asking job
// made before it asked stated by the case, in the order they finished.
func askAndAnswerAfter(t *testing.T, rig *coordRig, reap bool, made []BridgeCall,
	relaunch func(ctx context.Context, r PendingRun, asked *AskedRun),
) PendingRun {
	t.Helper()
	run := rig.launch("t1")
	for _, call := range made {
		if _, err := rig.pending.AppendBridgeCall(t.Context(), "t1", BridgeAppend{
			Launch: run.LaunchID, Call: call,
		}); err != nil {
			t.Fatalf("AppendBridgeCall: %v", err)
		}
	}
	rig.ask("t1", Ask{Question: "which branch?", To: AskToRequester, Branch: "wip/t1"})
	rig.runner.Finish(Result{SessionID: "sess-ask"})
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
	if last.Resume != "sess-ask" || last.Brief != "the answer: use main" {
		t.Fatalf("the carried-on job started with session %q and brief %q, want the asking "+
			"run's session and the answer", last.Resume, last.Brief)
	}
}

// A CARRIED-ON JOB NUMBERS ITS CALLS AFTER THE ASKING JOB'S. The row keeps the
// asking job's calls as the round's, and the collection that closes the round
// reads the round's calls in the order the run made them, by their place
// ([BridgeCall.Seq]): numbered from one again, the carried-on job's calls were
// sorted in among the asking job's, its record, its reviewer's log and its
// last submission with them. So the launch tells what serves the new job
// where the row's calls end — the highest place, not the last call's, since a
// call made first can finish last — and a job that starts a new round, whose
// log the row starts afresh, is told zero.
//
// Mutation: tell Opened zero, or the place of the last call in the row's
// list, and this goes red.
func TestACarriedOnJobIsNumberedAfterTheAskingJobsCalls(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	var told []OpenedJob
	opened := func(job OpenedJob) (Opening, error) {
		told = append(told, job)
		return Opening{}, nil
	}
	askAndAnswerAfter(t, rig, false, []BridgeCall{
		// FINISHED OUT OF ORDER: the call made last returned first.
		{Name: "post_comment", Args: `{"text":"on it"}`, Seq: 3},
		{Name: "read_file", Args: `{"path":"go.mod"}`, Seq: 1},
		{Name: "run_tests", Seq: 2},
	}, func(ctx context.Context, _ PendingRun, asked *AskedRun) {
		req := continuing(asked)
		req.Opened = opened
		if _, err := rig.launchVia(ctx, rig.manager, req); err != nil {
			t.Errorf("relaunch: %v", err)
		}
	})
	row := rig.get("t1")
	if len(told) != 1 || told[0].LaunchID != row.LaunchID || told[0].LastSeq != 3 {
		t.Fatalf("the carried-on job %q was told %+v, want it numbered after the "+
			"asking job's three calls", row.LaunchID, told)
	}

	// A NEW ROUND STARTS ITS LOG AFRESH, and its numbering with it.
	req := launchReq("t1")
	req.Opened = opened
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("the next round's launch: %v", err)
	}
	if len(told) != 2 || told[1].LastSeq != 0 {
		t.Fatalf("the next round's job was told %+v, want it numbered from the start", told[1:])
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
	if last.Resume != "" || !strings.Contains(last.Brief, "the question and the answer") {
		t.Fatalf("the fresh job started with session %q and brief %q, want no session and "+
			"the round's brief", last.Resume, last.Brief)
	}
}

// WHAT A PERSON'S ANSWER CARRIES THE RUN ON WITH IS REDACTED, like everything a
// resume hands the turn: the turn publishes it and hands it to the run.
func TestAnAnsweredQuestionIsRedacted(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	secret := "ghp_" + strings.Repeat("c", 36)
	rig.launch("t1")
	rig.ask("t1", Ask{Question: "may I use " + secret + "?", To: AskToRequester})
	rig.runner.Finish(Result{})
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

// A JOB CONTINUED IN ITS KEPT BOX STARTS ON FILES BROUGHT UP TO DATE. A kept
// box runs no setup, and no poll reached it while it was paused, so a
// credential file in it holds what the last poll of the asking job wrote —
// expired, after a long enough wait for the answer. The job carried on acts on
// the answer at once ("push it"), so the launch hands the box to the refresh
// hook the waiters poll with, live again and named by the row, BEFORE the job
// starts. A fresh box is not handed over: its setup has just written every
// file from the launch's own values.
//
// Mutation: drop the launch's call to the refresh, or move it after the job's
// start, and this goes red.
func TestAJobContinuedInItsKeptBoxStartsOnRefreshedFiles(t *testing.T) {
	t.Parallel()
	type handed struct {
		turn, launch, box, named string
		paused                   bool
		startedBefore            int
	}
	launcherFor := func(rig *coordRig, got *[]handed) *Coordinator {
		c, err := NewCoordinator(CoordinatorOptions{
			Answers: rigAnswers(), Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
			Resume: &resumeSpy{}, Audience: &audienceSpy{}, Lease: leased(rigLease),
			Refresh: func(_ context.Context, m *Manager, run PendingRun, box Sandbox) {
				if m != rig.manager {
					t.Error("the refresh was handed a manager other than the launch's")
				}
				*got = append(*got, handed{
					turn: run.TurnID, launch: run.LaunchID, box: box.ID(), named: run.SandboxID,
					paused:        rig.provider.Box(box.ID()).Paused(),
					startedBefore: len(rig.runner.Started()),
				})
			},
		})
		if err != nil {
			t.Fatalf("NewCoordinator: %v", err)
		}
		t.Cleanup(c.Stop)
		return c
	}

	t.Run("kept", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		var got []handed
		launcher := launcherFor(rig, &got)
		var relaunched LaunchResult
		run := askAndAnswer(t, rig, false, func(ctx context.Context, _ PendingRun, asked *AskedRun) {
			res, err := launcher.Launch(ctx, rig.manager, continuing(asked))
			if err != nil {
				t.Errorf("relaunch: %v", err)
			}
			relaunched = res
		})
		if !relaunched.Reused || relaunched.SandboxID != run.SandboxID {
			t.Fatalf("relaunch = %+v, want it continued in the asking run's box %q", relaunched, run.SandboxID)
		}
		job := rig.get("t1").LaunchID
		want := handed{turn: "t1", launch: job, box: run.SandboxID, named: run.SandboxID, startedBefore: 0}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("the refresh was handed %+v, want once %+v: the kept box, live, named by the "+
				"continuing job's row, before that job started", got, want)
		}
		if started := rig.runner.Started(); len(started) != 1 {
			t.Fatalf("%d jobs started, want the continuing one", len(started))
		}
	})

	t.Run("fresh", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		var got []handed
		launcher := launcherFor(rig, &got)
		var relaunched LaunchResult
		run := askAndAnswer(t, rig, true, func(ctx context.Context, _ PendingRun, asked *AskedRun) {
			res, err := launcher.Launch(ctx, rig.manager, continuing(asked))
			if err != nil {
				t.Errorf("relaunch: %v", err)
			}
			relaunched = res
		})
		if relaunched.Reused || relaunched.SandboxID == run.SandboxID {
			t.Fatalf("relaunch = %+v, want a fresh box", relaunched)
		}
		if len(got) != 0 {
			t.Fatalf("a fresh box, whose setup has just written its files, was handed to the "+
				"refresh: %+v", got)
		}
	})
}
