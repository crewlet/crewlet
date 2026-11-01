package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// a job lost with its host: found by the waiter that drives its box
// ---------------------------------------------------------------------

// A JOB ITS ENGINE'S RESTART KILLED IS RECORDED LOST AND ANNOUNCED, never
// collected as a job that ended: the waiter that drives its box finds it lost
// ([JobLost]) and writes the verdict on the row before the completion goes out
// — the seat's holder acts on the row, not on the announcement — and announces
// it again on the next pass, as a finished job's is, until a claim takes it.
//
// Mutation: read a lost job as an ended one in the poll, and the row carries no
// verdict, so its partial output is collected as the round's result.
func TestAJobItsEngineRestartKilledIsRecordedLostAndAnnounced(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.runner.Lose(Result{Text: "half way through"})

	if fired := rig.tick(); fired != 1 {
		t.Fatalf("announced %d completions for a lost job, want 1", fired)
	}
	got := rig.get("t1")
	if got.Lost == nil || got.Lost.Cause != LossEngineRestarted || !got.Lost.At.Equal(rig.now) ||
		got.LaunchID != run.LaunchID {
		t.Fatalf("the row records %+v on job %s; want the engine's restart at %v on %s",
			got.Lost, got.LaunchID, rig.now, run.LaunchID)
	}
	if fired := rig.tick(); fired != 1 {
		t.Errorf("the next pass announced %d; want the unclaimed loss announced again", fired)
	}
}

// A LOSS THE STORE DID NOT RECORD IS NOT ANNOUNCED: the claim acts on the row,
// so a completion announced for a loss the row does not carry would be
// collected as a job that ended — the very thing the verdict is for. The next
// pass asks again.
//
// Mutation: announce whatever the record answered, and a lost job's partial
// output is read as its result when the store blinked.
func TestALossTheStoreDidNotRecordIsNotAnnounced(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	rig.runner.Lose(Result{})
	refusing := &refusingStore{inner: rig.pending, refuse: refusals("MarkLost")}
	w, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: refusing, Manager: rig.managers, Drives: DrivesLocal,
		Now: func() time.Time { return rig.now },
	})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	if fired, err := w.Tick(t.Context()); err != nil || fired != 0 {
		t.Fatalf("Tick = %d, %v; want nothing announced for a loss the row does not carry", fired, err)
	}
	if got := rig.get("t1"); got.Lost != nil {
		t.Fatalf("the refused verdict landed: %+v", *got.Lost)
	}
}

// A BOX GONE FROM WHERE IT RAN IS A LOSS, once the poll's give-up window says
// it is not merely unreachable: its job went with it, and what it did is not
// there to read — so the row says the box is gone, and its round is carried on
// rather than settled as a box that could not be read back.
//
// Mutation: give a vanished box up as unreachable, and the row carries no
// verdict, so its turn ends as collect_unreachable.
func TestABoxGoneFromWhereItRanIsALoss(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.provider.Vanished[run.SandboxID] = true

	for range MinConnectFailures {
		rig.tick()
	}
	if got := rig.get("t1"); got.Lost != nil {
		t.Fatalf("a box gone for less than the give-up window was judged lost: %+v", *got.Lost)
	}
	rig.now = rig.now.Add(ConnectGiveUp)
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("announced %d once the window passed, want 1", fired)
	}
	if got := rig.get("t1"); got.Lost == nil || got.Lost.Cause != LossBoxGone {
		t.Fatalf("the row records %+v; want the box's loss", got.Lost)
	}
}

// A BOX THAT IS ONLY UNREACHABLE IS NO LOSS: a box that does not answer may
// still hold a working job, and only a box its backend says is gone is.
//
// Mutation: read every reach that gave up as a loss, and a box behind a broken
// network has its round started again beside it.
func TestABoxThatIsOnlyUnreachableIsNoLoss(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.provider.AttachFunc = func(context.Context, string) error {
		return errors.New("connection refused")
	}

	for range MinConnectFailures {
		rig.tick()
	}
	rig.now = rig.now.Add(ConnectGiveUp)
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("announced %d once the window passed, want 1", fired)
	}
	if got := rig.get("t1"); got.Lost != nil || got.LaunchID != run.LaunchID {
		t.Fatalf("an unreachable box was judged lost: %+v", got.Lost)
	}
}

// ---------------------------------------------------------------------
// the round carried on: by the seat's holder, from the row
// ---------------------------------------------------------------------

// loseAndClaim launches t1, loses its job with partial as what it had said,
// has the waiter record and announce the loss, and hands the completion to the
// coordinator.
func loseAndClaim(t *testing.T, rig *coordRig, partial Result) error {
	t.Helper()
	return askLoseAndClaim(t, rig, nil, partial)
}

// askLoseAndClaim is [loseAndClaim] for a job that asked a person something
// before it was lost: the ask is recorded on its row while it runs, as the
// run's ask tool records it, and nil asks nothing.
func askLoseAndClaim(t *testing.T, rig *coordRig, asked *Ask, partial Result) error {
	t.Helper()
	rig.launch("t1")
	if asked != nil {
		rig.ask("t1", *asked)
	}
	rig.runner.Lose(partial)
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("announced %d completions for a lost job, want 1", fired)
	}
	payload, ev := rig.completion("t1")
	return rig.coordinator.OnCompleted(t.Context(), payload, ev)
}

// A LOST JOB'S ROUND IS CARRIED ON, NOT COLLECTED: the turn resumes told what
// was lost, that its box was kept — a restart kills the job and leaves the box
// — and how many of the round's jobs have been, against the most a round is
// carried on after. What the job did up to then is read off its box, charged
// and published as its own record, once, marked cut off rather than failed:
// its host cut it off, which says nothing about the run.
//
// Mutation: collect the lost job as an ended one, and the resume carries no
// loss — the turn judges a truncated run as the round's result; skip the read,
// and its spend is charged nowhere.
func TestALostJobsRoundIsCarriedOnNotCollected(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	if err := loseAndClaim(t, rig, Result{Text: "half way through", InputTokens: 900,
		OutputTokens: 100}); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}

	requests := rig.resumer.calls()
	if len(requests) != 1 {
		t.Fatalf("the turn was resumed %d times, want once", len(requests))
	}
	req := requests[0]
	if lost := req.Lost; lost == nil || lost.Cause != LossEngineRestarted || !lost.BoxKept ||
		lost.Losses != 1 || lost.Limit != MaxRoundLosses {
		t.Fatalf("resumed with %+v; want the engine's restart, the box kept, the round's first loss", lost)
	}
	if req.Success || req.InputTokens != 900 || req.OutputTokens != 100 {
		t.Errorf("resumed as success=%v with %d/%d tokens; want the cut-off job's spend, never a success",
			req.Success, req.InputTokens, req.OutputTokens)
	}
	if !strings.Contains(req.Answer, "cut off") || !strings.Contains(req.Answer, "half way through") {
		t.Errorf("the job's account %q does not say it was cut off beside what it said", req.Answer)
	}
	if charged := rig.accountant.total(); charged != 1000 {
		t.Errorf("charged %d tokens for the lost job, want its 1000", charged)
	}
	launches := rig.collected()
	if len(launches) != 1 {
		t.Fatalf("published %d records for the lost job, want its own one", len(launches))
	}
	if rec := launches[0]; rec.Failed || !strings.Contains(rec.Notes, "cut off") {
		t.Errorf("the lost job's record is failed=%v with notes %q; want it noted cut off, not failed",
			rec.Failed, rec.Notes)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Errorf("announced %+v; a lost job is carried on, not failed", failed)
	}
}

// A ROUND ALREADY LOST COUNTS IT: the resume says how many of the round's jobs
// have been lost, this one included, so the turn stops starting a round that
// keeps taking its host down.
//
// Mutation: hand the turn the row's count without this job, and a round is
// carried on once more than [MaxRoundLosses] allows.
func TestALostJobCountsTheRoundsLossesBeforeIt(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	ctx := t.Context()
	first := rig.launch("t1")
	// THE ROUND WAS CARRIED ON ONCE ALREADY: its first job lost, and a
	// relaunch continuing it, in the same box.
	if marked, err := rig.pending.MarkLost(ctx, "t1", first.LaunchID,
		JobLoss{Cause: LossEngineRestarted, At: rig.now}); err != nil || !marked {
		t.Fatalf("MarkLost = %v, %v", marked, err)
	}
	relaunch := first
	relaunch.Continues = true
	if _, err := rig.pending.BeginLaunch(ctx, relaunch, rigLease); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := rig.pending.AttachSandbox(ctx, "t1", BoxRef{
		SandboxID: first.SandboxID, CommandID: "cmd-2", CodingAgent: "claude-code",
		PauseTTLSec: DefaultPauseTTL.Seconds(),
	}, rigLease); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	rig.suspend("t1")
	rig.runner.Lose(Result{})
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("announced %d completions for the second lost job, want 1", fired)
	}
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	requests := rig.resumer.calls()
	if len(requests) != 1 || requests[0].Lost == nil || requests[0].Lost.Losses != 2 {
		t.Fatalf("resumed with %+v; want the round's second loss", requests)
	}
}

// A LOST JOB THAT HAD ASKED A PERSON IS PARKED ON ITS QUESTION, as a job that
// finished asking is: its question was written and its harness on its way out, and
// the answer is what carries it on — a fresh run would ask again, or guess.
//
// Mutation: carry every lost job's round on, and a question already put to a
// person is dropped.
func TestALostJobThatHadAskedIsParkedOnItsQuestion(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	asked := whichBranch()
	if err := askLoseAndClaim(t, rig, &asked, Result{}); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Question != "which branch?" {
		t.Fatalf("the run is %s asking %q; want it parked on its question", got.Status, got.Question)
	}
	if requests := rig.resumer.calls(); len(requests) != 0 {
		t.Errorf("the turn was resumed %d times for a job parked on its question", len(requests))
	}
}

// A LOST JOB'S BOX THAT DOES NOT ANSWER IS ASKED AGAIN, as any collection is,
// so its spend is not lost to one blip — and once it has not answered for the
// give-up window, the round is carried on without it.
//
// Mutation: carry the round on at the first failed read, and the lost job's
// spend is never charged; hand it back for ever, and the round never goes on.
func TestALostJobsUnreadableBoxIsAskedAgainThenLeftBehind(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.runner.CollectErr = errors.New("connection reset")
	err := loseAndClaim(t, rig, Result{InputTokens: 500})
	if err == nil {
		t.Fatal("a failed read of the lost job's box was not handed back for a retry")
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.Lost == nil {
		t.Fatalf("the run is %s with loss %+v; want it handed back running, its verdict kept",
			got.Status, got.Lost)
	}
	if requests := rig.resumer.calls(); len(requests) != 0 {
		t.Fatalf("the turn was resumed before the read was given up: %+v", requests)
	}

	for range MinConnectFailures {
		if len(rig.resumer.calls()) > 0 {
			break
		}
		rig.now = rig.now.Add(ConnectGiveUp)
		payload, ev := rig.completion("t1")
		_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
	}
	requests := rig.resumer.calls()
	if len(requests) != 1 || requests[0].Lost == nil {
		t.Fatalf("resumed %+v; want the round carried on once the read was given up", requests)
	}
	if charged := rig.accountant.total(); charged != 0 {
		t.Errorf("charged %d tokens nobody read", charged)
	}
}
