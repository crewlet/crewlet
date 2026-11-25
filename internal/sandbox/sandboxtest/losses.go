package sandboxtest

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A LOSS IS RECORDED ONLY ON THE JOB IT WAS JUDGED ON: running, the same
// launch, and the node whose host its box is on, named. A verdict that landed
// on a relaunch, or on a run whose completion is already being collected, would
// have a round started again that nothing found lost. Recorded once, it stands
// at its first instant and cause, and a relaunch clears it.
func testALossIsRecordedOnlyOnTheJobItWasJudgedOn(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	first := run("t-lost")
	first.LaunchNode = "node-a"
	launch := mustLaunched(t, s, first)
	at := base.Add(time.Hour)
	lost := sandbox.JobLoss{Cause: sandbox.LossEngineRestarted, Node: "node-a", At: at}

	for _, refused := range []struct {
		name, launch, node string
	}{
		{"another job", "not-" + launch, "node-a"},
		{"another node", launch, "node-b"},
		{"no node", launch, ""},
		{"no job", "", "node-a"},
	} {
		loss := lost
		loss.Node = refused.node
		marked, err := s.MarkLost(ctx, "t-lost", refused.launch, loss)
		if err != nil || marked {
			t.Errorf("a verdict on %s: marked=%v err=%v; want refused", refused.name, marked, err)
		}
	}
	if got := mustGet(t, s, "t-lost"); got.Lost != nil {
		t.Fatalf("a refused verdict landed: %+v", *got.Lost)
	}

	marked, err := s.MarkLost(ctx, "t-lost", launch, lost)
	if err != nil || !marked {
		t.Fatalf("the verdict on the job it was judged on: marked=%v err=%v", marked, err)
	}
	if got := mustGet(t, s, "t-lost"); got.Lost == nil || !got.Lost.At.Equal(at) ||
		got.Lost.Cause != sandbox.LossEngineRestarted || got.Lost.Node != "node-a" {
		t.Fatalf("the row records the verdict %+v, want %+v", got.Lost, lost)
	}
	// It STANDS: asked again, the row still carries the first, at its first
	// instant and with its own cause.
	again := sandbox.JobLoss{Cause: sandbox.LossBoxGone, Node: "node-a", At: at.Add(time.Minute)}
	if marked, err := s.MarkLost(ctx, "t-lost", launch, again); err != nil || !marked {
		t.Errorf("a second verdict on the same job: marked=%v err=%v; want the verdict standing", marked, err)
	}
	if got := mustGet(t, s, "t-lost"); got.Lost == nil || !got.Lost.At.Equal(at) ||
		got.Lost.Cause != sandbox.LossEngineRestarted {
		t.Errorf("a second verdict moved the first to %+v", got.Lost)
	}

	// A RELAUNCH is a job nothing has judged.
	next := run("t-lost")
	next.LaunchNode = "node-b"
	mustBeginLaunch(t, s, next)
	if got := mustGet(t, s, "t-lost"); got.Lost != nil {
		t.Errorf("the relaunch kept the previous job's verdict: %+v", *got.Lost)
	}

	// A JOB NO LONGER RUNNING — its completion claimed — takes none.
	claimedRun := run("t-claimed")
	claimedRun.LaunchNode = "node-a"
	claimedLaunch := mustLaunched(t, s, claimedRun)
	mustClaim(t, s, "t-claimed")
	if marked, err := s.MarkLost(ctx, "t-claimed", claimedLaunch, lost); err != nil || marked {
		t.Errorf("a verdict on a claimed job: marked=%v err=%v; want refused", marked, err)
	}
	// Nor does a run with no record.
	if marked, err := s.MarkLost(ctx, "t-none", "l", lost); err != nil || marked {
		t.Errorf("a verdict on no run: marked=%v err=%v; want refused", marked, err)
	}
}

// A CLAIM'S COLLECTION RECORDS ITS JOB LOST ON ITS CLAIM ALONE. The collection
// of a job whose box only its node's host can read, from a host that has not
// been able to read the run's row for the host grace, records the job lost
// with that host on the claim it holds, and that claim carries the job's round
// on: the relaunch that does counts the loss. A job still running is the
// waiter's to judge, and a verdict on another job, another node's box or none
// named, or a claim under a lease since outranked is one about a claim no collection
// holds, which would have a round carried on that nothing found lost. Recorded
// once, it stands at its first instant and cause.
func testAClaimsCollectionRecordsItsJobLostOnItsClaimAlone(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	first := run("t-claim-lost")
	first.LaunchNode = "node-a"
	launch := mustLaunched(t, s, first)
	at := base.Add(time.Hour)
	lost := sandbox.JobLoss{Cause: sandbox.LossLaunchNodeStoreSilent, Node: "node-a", At: at}
	holder := sandbox.Fence{Owner: "node-b:1", Epoch: 3}
	if _, err := s.ClaimOwnership(ctx, "t-claim-lost", holder.Owner, holder.Epoch); err != nil {
		t.Fatalf("own: %v", err)
	}

	// RUNNING, the job is the waiter's to judge.
	if marked, err := s.MarkClaimLost(ctx, "t-claim-lost", launch, lost, holder); err != nil || marked {
		t.Fatalf("a verdict on a running job: marked=%v err=%v; want refused", marked, err)
	}
	mustClaim(t, s, "t-claim-lost")
	for _, refused := range []struct {
		name, launch, node string
		fence              sandbox.Fence
	}{
		{"another job", "not-" + launch, "node-a", holder},
		{"another node", launch, "node-b", holder},
		{"no node", launch, "", holder},
		{"no job", "", "node-a", holder},
		{"a lease the claim's outranks", launch, "node-a", sandbox.Fence{Owner: "node-c:1", Epoch: 2}},
	} {
		loss := lost
		loss.Node = refused.node
		if marked, err := s.MarkClaimLost(ctx, "t-claim-lost", refused.launch, loss, refused.fence); err != nil ||
			marked {
			t.Errorf("a verdict on %s: marked=%v err=%v; want refused", refused.name, marked, err)
		}
	}
	if got := mustGet(t, s, "t-claim-lost"); got.Lost != nil {
		t.Fatalf("a refused verdict landed: %+v", *got.Lost)
	}

	if marked, err := s.MarkClaimLost(ctx, "t-claim-lost", launch, lost, holder); err != nil || !marked {
		t.Fatalf("the verdict on the claim's own job: marked=%v err=%v", marked, err)
	}
	got := mustGet(t, s, "t-claim-lost")
	if got.Status != sandbox.StatusResumed || got.Lost == nil || !got.Lost.At.Equal(at) ||
		got.Lost.Cause != sandbox.LossLaunchNodeStoreSilent || got.Lost.Node != "node-a" {
		t.Fatalf("the row is %s and records %+v; want the claim kept with the verdict %+v on it",
			got.Status, got.Lost, lost)
	}
	// It STANDS: asked again, the row still carries the first.
	again := sandbox.JobLoss{Cause: sandbox.LossBoxGone, Node: "node-a", At: at.Add(time.Minute)}
	if marked, err := s.MarkClaimLost(ctx, "t-claim-lost", launch, again, holder); err != nil || !marked {
		t.Errorf("a second verdict on the same claim: marked=%v err=%v; want the verdict standing", marked, err)
	}
	if got := mustGet(t, s, "t-claim-lost"); got.Lost == nil || !got.Lost.At.Equal(at) ||
		got.Lost.Cause != sandbox.LossLaunchNodeStoreSilent {
		t.Errorf("a second verdict moved the first to %+v", got.Lost)
	}

	// THE RELAUNCH THAT CARRIES THE ROUND ON counts the loss, and its job is
	// one nothing has judged.
	next := run("t-claim-lost")
	next.LaunchNode = "node-b"
	next.Continues = true
	mustBeginLaunch(t, s, next)
	if got := mustGet(t, s, "t-claim-lost"); got.Lost != nil || got.RoundLosses != 1 {
		t.Errorf("the relaunch left the verdict %+v and counts %d losses; want it cleared and counted once",
			got.Lost, got.RoundLosses)
	}

	// Nor does a run with no record.
	if marked, err := s.MarkClaimLost(ctx, "t-none", "l", lost, holder); err != nil || marked {
		t.Errorf("a verdict on no run: marked=%v err=%v; want refused", marked, err)
	}
}

// A ROUND COUNTS ITS LOST JOBS ACROSS THE RELAUNCHES THAT CARRY IT ON — each
// one counted by the relaunch that follows it — and a new round starts from
// none. The count is what stops a run that keeps taking its host down from
// being started again for ever; one that a relaunch forgot would never stop it,
// and one that outlived its round would end the next round early.
func testARoundCountsItsLostJobsAcrossTheRelaunchesThatCarryItOn(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	lose := func(launch string) {
		t.Helper()
		loss := sandbox.JobLoss{Cause: sandbox.LossEngineRestarted, Node: "node-a", At: base}
		if marked, err := s.MarkLost(ctx, "t-round", launch, loss); err != nil || !marked {
			t.Fatalf("mark lost: marked=%v err=%v", marked, err)
		}
	}
	carryOn := func() string {
		t.Helper()
		next := run("t-round")
		next.LaunchNode = "node-a"
		next.Continues = true
		return mustLaunched(t, s, next)
	}

	first := run("t-round")
	first.LaunchNode = "node-a"
	lose(mustLaunched(t, s, first))
	second := carryOn()
	if got := mustGet(t, s, "t-round"); got.RoundLosses != 1 {
		t.Fatalf("after one lost job carried on, the round counts %d losses, want 1", got.RoundLosses)
	}
	lose(second)
	carryOn()
	if got := mustGet(t, s, "t-round"); got.RoundLosses != 2 {
		t.Fatalf("after two lost jobs carried on, the round counts %d losses, want 2", got.RoundLosses)
	}
	// A JOB OF THE ROUND THAT WAS NOT LOST — one that asked and is carried
	// on with its answer — adds nothing.
	carryOn()
	if got := mustGet(t, s, "t-round"); got.RoundLosses != 2 {
		t.Errorf("a carried-on job that was not lost moved the count to %d, want 2", got.RoundLosses)
	}
	// A NEW ROUND has lost nothing.
	fresh := run("t-round")
	fresh.LaunchNode = "node-a"
	mustLaunched(t, s, fresh)
	if got := mustGet(t, s, "t-round"); got.RoundLosses != 0 {
		t.Errorf("a new round starts at %d losses, want 0", got.RoundLosses)
	}
}
