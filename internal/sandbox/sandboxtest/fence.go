package sandboxtest

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

func testOwnershipIsNotStolenByAnOlderLease(t *testing.T, s sandbox.PendingStore) {
	// A run whose epoch is HIGHER belongs to a newer lease. Taking it would
	// put two engines on one box, both collecting, both resuming.
	mustLaunched(t, s, run("t1"))
	if won, err := s.ClaimOwnership(t.Context(), "t1", "node-b:2", 5); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if won, err := s.ClaimOwnership(t.Context(), "t1", "node-a:1", 3); err != nil || won {
		t.Errorf("an older lease stole the run: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Owner != "node-b:2" || got.OwnerEpoch != 5 {
		t.Errorf("owner = %+v", got)
	}
	// Equal epochs pass: a node re-claiming its OWN run after a restart
	// within one lease must not be locked out of it.
	if won, err := s.ClaimOwnership(t.Context(), "t1", "node-b:3", 5); err != nil || !won {
		t.Errorf("a node could not re-claim its own run: won=%v err=%v", won, err)
	}
}

// A RUN IS OWNED BY THE LEASE THAT LAUNCHED IT: a fenced launch stamps its
// fence on the row, so the launching node's own writes — which carry that fence
// back off the row — are refused once the seat's next holder fences the row to
// a newer lease. Unstamped, the row sat at the zero epoch, and the zero fence
// constrains nothing: the node that lost the seat could release a claim the
// next holder had reaped. A relaunch restamps; an unfenced launch stamps
// nothing.
func testALaunchStampsTheLeaseThatLaunchedIt(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	lease := sandbox.Fence{Owner: "node-a:1", Epoch: 3}
	if _, err := s.BeginLaunch(ctx, run("t1"), lease); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Owner != lease.Owner || got.OwnerEpoch != lease.Epoch {
		t.Fatalf("owner %q epoch %d, want the launching lease %+v", got.Owner, got.OwnerEpoch, lease)
	}
	if ok, err := s.MarkSuspended(ctx, "t1", suspension()); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	launch := mustGet(t, s, "t1").LaunchID
	claimed, ok, err := s.ClaimForResume(ctx, "t1", sandbox.CompletionTail(launch), lease)
	if err != nil || !ok || claimed.OwnerEpoch != lease.Epoch {
		t.Fatalf("claim = %v (epoch %d), %v, want it carrying the launching lease", ok,
			claimed.OwnerEpoch, err)
	}
	// THE NEXT HOLDER FENCES IT, and the launching node's release is refused.
	if ok, err := s.ClaimOwnership(ctx, "t1", "node-b:1", 4); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	if released, err := s.ReleaseClaim(ctx, "t1", sandbox.Release{
		Launch: launch, To: sandbox.StatusRunning,
		Fence: sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch},
	}); err != nil || released {
		t.Fatalf("the launching node's release after the next holder fenced the row = %v, %v, "+
			"want refused", released, err)
	}
	// A RELAUNCH RESTAMPS, under the lease that relaunched.
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{Owner: "node-c:1", Epoch: 6}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Owner != "node-c:1" || got.OwnerEpoch != 6 {
		t.Fatalf("owner %q epoch %d after the relaunch, want node-c:1 at 6", got.Owner, got.OwnerEpoch)
	}
	// AN UNFENCED LAUNCH STAMPS NOTHING.
	if _, err := s.BeginLaunch(ctx, run("t2"), sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch t2: %v", err)
	}
	if got := mustGet(t, s, "t2"); got.Owner != "" || got.OwnerEpoch != 0 {
		t.Fatalf("an unfenced launch stamped %q at %d", got.Owner, got.OwnerEpoch)
	}
}

// A RELAUNCH THE ROW REFUSES IS AN ERROR, never a launch that went ahead: a run a
// newer lease owns, or one whose ending is decided, is not reset — and a caller
// told nil would go on to start a job in a box the row does not record, killed
// under it by the ending or held and named by nothing.
func testARefusedRelaunchIsAnError(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	if _, err := s.ClaimOwnership(ctx, "t1", "node-b:1", 5); err != nil {
		t.Fatalf("ClaimOwnership: %v", err)
	}
	before := mustGet(t, s, "t1")
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{Owner: "node-a:1", Epoch: 3}); !errors.Is(err, sandbox.ErrSeatNotHeld) {
		t.Fatalf("a relaunch under a lease the run's outranks = %v, want ErrSeatNotHeld", err)
	}
	if got := mustGet(t, s, "t1"); got.LaunchID != before.LaunchID || got.Status != sandbox.StatusRunning {
		t.Fatalf("the refused relaunch moved the run: %q under %q", got.Status, got.LaunchID)
	}
	decide(t, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active))
	if _, err := s.BeginLaunch(ctx, run("t1"), sandbox.Fence{Owner: "node-b:1", Epoch: 5}); !errors.Is(err, sandbox.ErrRunEnding) {
		t.Fatalf("a relaunch of a run whose ending is decided = %v, want ErrRunEnding", err)
	}
}

// A NEWER LEASE'S ENDING IS STILL A SEAT NOT HELD to the launch it outranks.
//
// The seat's next holder recovers a run it finds claimed by fencing it to its
// own lease before it ends it, so the old holder's relaunch of it, its resumed
// turn's, almost always meets an ending as well as the newer lease. Answered
// [sandbox.ErrRunEnding], the launch was told its own turn was closing the
// run, when what happened is that the seat moved. A launch under the lease
// that decided the ending is still told the ending.
func testANewerLeasesEndingIsStillASeatNotHeld(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	old := sandbox.Fence{Owner: "node-a:1", Epoch: 3}
	if _, err := s.BeginLaunch(ctx, run("t1"), old); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if ok, err := s.ClaimOwnership(ctx, "t1", "node-b:1", 7); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	newer := sandbox.Fence{Owner: "node-b:1", Epoch: 7}
	decide(t, s, "t1", everyJob(newer, sandbox.Active))
	if _, err := s.BeginLaunch(ctx, run("t1"), old); !errors.Is(err, sandbox.ErrSeatNotHeld) {
		t.Errorf("a relaunch under the outgrown lease = %v, want ErrSeatNotHeld", err)
	}
	if err := s.AttachSandbox(ctx, "t1", sandbox.BoxRef{SandboxID: "box-1"}, old); !errors.Is(err, sandbox.ErrSeatNotHeld) {
		t.Errorf("an attach under the outgrown lease = %v, want ErrSeatNotHeld", err)
	}
	if err := s.AttachSandbox(ctx, "t1", sandbox.BoxRef{SandboxID: "box-1"}, newer); !errors.Is(err, sandbox.ErrRunEnding) {
		t.Errorf("an attach under the lease that decided the ending = %v, want ErrRunEnding", err)
	}
	if got := mustGet(t, s, "t1"); got.SandboxID != "" {
		t.Errorf("a refused attach recorded box %q", got.SandboxID)
	}
}

func testAStaleFenceCannotWrite(t *testing.T, s sandbox.PendingStore) {
	// THE FENCE IS THE GUARANTEE, the ownership check only an optimisation:
	// a node whose lease moved cannot write even if it has not noticed yet.
	mustLaunched(t, s, run("t1"))
	if _, err := s.ClaimOwnership(t.Context(), "t1", "node-b:2", 7); err != nil {
		t.Fatalf("claim: %v", err)
	}
	stale := sandbox.Fence{Owner: "node-a:1", Epoch: 3}
	if err := s.SetStatus(t.Context(), "t1", sandbox.StatusReseed, stale); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Errorf("a stale fence wrote %q", got.Status)
	}
	// A STALE ATTACH IS REFUSED ALOUD: its caller is a launch about to start
	// a job in that box, which must abandon the box rather than run a job
	// the row does not name.
	if err := s.AttachSandbox(t.Context(), "t1",
		sandbox.BoxRef{SandboxID: "ghost"}, stale); !errors.Is(err, sandbox.ErrSeatNotHeld) {
		t.Fatalf("a stale fence's attach = %v, want ErrSeatNotHeld", err)
	}
	if got := mustGet(t, s, "t1"); got.SandboxID != "" {
		t.Errorf("a stale fence attached a box: %q", got.SandboxID)
	}
	// Nor may it END the run: a node whose lease moved deleting the record
	// its successor recovered strands the successor's box.
	if _, finished, err := end(t.Context(), s, "t1", everyJob(stale, sandbox.Active)); err != nil || finished {
		t.Errorf("a stale fence finished the run: %v, %v", finished, err)
	}
	if _, found, err := s.Get(t.Context(), "t1"); err != nil || !found {
		t.Fatalf("the record is gone after a stale Finish (found %v, %v)", found, err)
	}
	if _, finished, err := end(t.Context(), s, "t1",
		everyJob(sandbox.Fence{Owner: "node-b:2", Epoch: 7}, sandbox.Active)); err != nil || !finished {
		t.Errorf("the owning lease could not finish its own run: %v, %v", finished, err)
	}
}
