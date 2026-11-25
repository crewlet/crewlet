package sandboxtest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// releaseOf is the release that hands a claimed run back where it was taken
// from, under the lease it was taken under.
func releaseOf(claimed sandbox.PendingRun) sandbox.Release {
	return sandbox.Release{
		Launch: claimed.LaunchID, To: claimed.ClaimedFrom,
		Fence: sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch},
	}
}

// mustRelease hands a claimed run back.
func mustRelease(t *testing.T, s sandbox.PendingStore, claimed sandbox.PendingRun) {
	t.Helper()
	released, err := s.ReleaseClaim(t.Context(), claimed.TurnID, releaseOf(claimed))
	if err != nil || !released {
		t.Fatalf("release %s: released=%v err=%v", claimed.TurnID, released, err)
	}
}

func testAReleaseHandsTheClaimBackWhereItFoundIt(t *testing.T, s sandbox.PendingStore) {
	// The retry a failed resume opens has to find the run exactly where the
	// signal it is retrying can take it: a completion's job running, an
	// answer's question waiting.
	mustLaunched(t, s, run("t1"))
	mustRelease(t, s, mustClaim(t, s, "t1"))
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Fatalf("status = %q, want the completion's job running again", got.Status)
	}
	mustClaim(t, s, "t1")

	mustLaunched(t, s, run("t2"))
	Ask(t, s, "t2", Question("which branch?", sandbox.AskToRequester))
	if err := s.MarkAwaiting(t.Context(), "t2", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	answered, won, err := s.ClaimForResume(t.Context(), "t2", answerTo(t, s, "t2"), sandbox.Fence{})
	if err != nil || !won {
		t.Fatalf("claim the answer: won=%v err=%v", won, err)
	}
	mustRelease(t, s, answered)
	if got := mustGet(t, s, "t2"); got.Status != sandbox.StatusAwaiting || got.Question != "which branch?" {
		t.Fatalf("row = %s %q, want the question waiting on its answer again", got.Status, got.Question)
	}
}

func testAReleaseOfAnotherLaunchIsRefused(t *testing.T, s sandbox.PendingStore) {
	// The resumed turn relaunched its executor, so the row holds a new
	// launch with the claimed job's conversation cleared. Handed back, that
	// launch would read as a running job with nothing to resume into, and
	// its completion would collect whatever the box still held.
	mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	mustBeginLaunch(t, s, run("t1"))
	relaunched := mustGet(t, s, "t1")

	if released, err := s.ReleaseClaim(t.Context(), "t1", releaseOf(claimed)); err != nil || released {
		t.Fatalf("the claim handed back a launch it never took: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusLaunching || got.LaunchID != relaunched.LaunchID {
		t.Fatalf("row = %s under %q, want the relaunch left as it was", got.Status, got.LaunchID)
	}
	// Nor once the new job is claimed in its turn: the status is the same,
	// and only the launch tells the two claims apart.
	if suspended, err := s.MarkSuspended(t.Context(), "t1", suspension()); err != nil || !suspended {
		t.Fatalf("mark suspended: suspended=%v err=%v", suspended, err)
	}
	next := mustClaim(t, s, "t1")
	if released, err := s.ReleaseClaim(t.Context(), "t1", releaseOf(claimed)); err != nil || released {
		t.Fatalf("the claim handed back the next job's claim: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed {
		t.Fatalf("status = %q, want the next job still claimed", got.Status)
	}
	mustRelease(t, s, next)
}

func testAReleaseOfARunNoLongerClaimedIsRefused(t *testing.T, s sandbox.PendingStore) {
	// The seat's next owner reaps a claim its previous owner abandoned, and
	// announces the run lost. A release landing after that would revive it
	// with its box torn down. And a run nobody claimed has no claim to hand
	// back at all.
	mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	if _, finished, err := end(t.Context(), s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil || !finished {
		t.Fatalf("reap: finished=%v err=%v", finished, err)
	}
	if released, err := s.ReleaseClaim(t.Context(), "t1", releaseOf(claimed)); err != nil || released {
		t.Fatalf("a reaped run was handed back: released=%v err=%v", released, err)
	}
	if _, found, err := s.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("a release recreated the record of a run already ended: found=%v err=%v", found, err)
	}

	mustLaunched(t, s, run("t2"))
	unclaimed := sandbox.Release{Launch: mustGet(t, s, "t2").LaunchID, To: sandbox.StatusRunning}
	if released, err := s.ReleaseClaim(t.Context(), "t2", unclaimed); err != nil || released {
		t.Errorf("a run nobody claimed reported a release: released=%v err=%v", released, err)
	}
}

func testAStaleFenceCannotRelease(t *testing.T, s sandbox.PendingStore) {
	// A claim taken under a lease that has since moved hands nothing back:
	// the seat's new owner decides what becomes of the run.
	mustLaunched(t, s, run("t1"))
	if _, err := s.ClaimOwnership(t.Context(), "t1", "node-a:1", 3); err != nil {
		t.Fatalf("own: %v", err)
	}
	claimed := mustClaim(t, s, "t1")
	if _, err := s.ClaimOwnership(t.Context(), "t1", "node-b:2", 5); err != nil {
		t.Fatalf("take over: %v", err)
	}
	if released, err := s.ReleaseClaim(t.Context(), "t1", releaseOf(claimed)); err != nil || released {
		t.Fatalf("a stale fence handed the claim back: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed {
		t.Fatalf("status = %q, want the claim left where the new owner finds it", got.Status)
	}
	current := releaseOf(claimed)
	current.Fence = sandbox.Fence{Owner: "node-b:2", Epoch: 5}
	if released, err := s.ReleaseClaim(t.Context(), "t1", current); err != nil || !released {
		t.Errorf("the current lease could not hand the claim back: released=%v err=%v", released, err)
	}
}

func testAReleaseGoesBackOnlyToAClaimableStatus(t *testing.T, s sandbox.PendingStore) {
	// No claim takes a run out of any other status, so a release naming one
	// is a caller's mistake, and writing it would strand the run where no
	// signal can take it.
	mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	for _, to := range []string{sandbox.StatusLaunching, sandbox.StatusResumed, "done", ""} {
		release := releaseOf(claimed)
		release.To = to
		if released, err := s.ReleaseClaim(t.Context(), "t1", release); err == nil || released {
			t.Errorf("a release to %q was accepted: released=%v err=%v", to, released, err)
		}
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed {
		t.Errorf("a refused release moved the run to %q", got.Status)
	}
}

func testAReleaseOfAMissingRunIsNotAnError(t *testing.T, s sandbox.PendingStore) {
	released, err := s.ReleaseClaim(t.Context(), "never-launched",
		sandbox.Release{To: sandbox.StatusRunning})
	if err != nil || released {
		t.Errorf("a missing run reported a release: released=%v err=%v", released, err)
	}
}

// completionOf is the tail a completion of the run's current job claims.
func completionOf(t *testing.T, s sandbox.PendingStore, turnID string) sandbox.Tail {
	t.Helper()
	return sandbox.CompletionTail(mustGet(t, s, turnID).LaunchID)
}

// answerTo is the tail an answer to the run's current question claims.
func answerTo(t *testing.T, s sandbox.PendingStore, turnID string) sandbox.Tail {
	t.Helper()
	return sandbox.Tail{Launch: mustGet(t, s, turnID).LaunchID, From: sandbox.Awaiting}
}

// mustClaim takes a running run's tail, as its job's completion would on the
// node holding the seat: under the lease the row carries.
func mustClaim(t *testing.T, s sandbox.PendingStore, turnID string) sandbox.PendingRun {
	t.Helper()
	row := mustGet(t, s, turnID)
	got, won, err := s.ClaimForResume(t.Context(), turnID, completionOf(t, s, turnID),
		sandbox.Fence{Owner: row.Owner, Epoch: row.OwnerEpoch})
	if err != nil || !won {
		t.Fatalf("claim %s: won=%v err=%v", turnID, won, err)
	}
	return got
}

// mustReleaseCharged hands a claimed run back with its charge recorded.
func mustReleaseCharged(t *testing.T, s sandbox.PendingStore, claimed sandbox.PendingRun) {
	t.Helper()
	release := releaseOf(claimed)
	release.Charged = true
	released, err := s.ReleaseClaim(t.Context(), claimed.TurnID, release)
	if err != nil || !released {
		t.Fatalf("release %s charged: released=%v err=%v", claimed.TurnID, released, err)
	}
}

func testAReleaseRecordsTheClaimsCharge(t *testing.T, s sandbox.PendingStore) {
	// THE DURABLE HALF OF CHARGING A RUN ONCE, in the one write that reopens
	// the run to a retry. The retry reads nothing but the row its own claim
	// returns, on this node or the seat's next owner, so the record has to
	// come back on it.
	mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	if claimed.Charged {
		t.Fatal("a run nothing has charged reads as charged")
	}
	mustReleaseCharged(t, s, claimed)
	if got := mustClaim(t, s, "t1"); !got.Charged {
		t.Error("the retry's claim came back without the charge the first attempt made")
	}
}

func testAReleaseCountsAFailedCollectionOntoTheJob(t *testing.T, s sandbox.PendingStore) {
	// THE BOUND ON A COLLECTION'S RETRIES LIVES ON THE JOB'S RECORD, in the
	// write that reopens the run to the retry: a count kept in memory would
	// grant every node, and every restart, a fresh allowance.
	mustLaunched(t, s, run("t1"))
	first := base.Add(3 * time.Second)
	for i, at := range []time.Time{first, first.Add(40 * time.Second)} {
		release := releaseOf(mustClaim(t, s, "t1"))
		release.CollectFailedAt = at
		if released, err := s.ReleaseClaim(t.Context(), "t1", release); err != nil || !released {
			t.Fatalf("release %d: released=%v err=%v", i, released, err)
		}
	}
	facts := mustGet(t, s, "t1").Launch
	if facts.CollectFailures != 2 || !facts.CollectFailingSince.Equal(first) {
		t.Errorf("the job's record = %d failures since %v; want 2 since the first, %v",
			facts.CollectFailures, facts.CollectFailingSince, first)
	}

	// A release that is not a failed collection counts nothing.
	mustRelease(t, s, mustClaim(t, s, "t1"))
	if got := mustGet(t, s, "t1").Launch.CollectFailures; got != 2 {
		t.Errorf("an ordinary hand-back moved the count to %d", got)
	}

	// A COLLECTION THAT READ THE BOX ENDS THE RUN: the bound is on
	// consecutive failures, and a later failure starts a run of its own.
	collected := releaseOf(mustClaim(t, s, "t1"))
	collected.Collected = true
	if released, err := s.ReleaseClaim(t.Context(), "t1", collected); err != nil || !released {
		t.Fatalf("release after a collection: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1").Launch; got.CollectFailures != 0 || !got.CollectFailingSince.IsZero() {
		t.Errorf("a release after a collection that read the box left %+v", got)
	}
	later := first.Add(2 * time.Minute)
	failed := releaseOf(mustClaim(t, s, "t1"))
	failed.CollectFailedAt = later
	if released, err := s.ReleaseClaim(t.Context(), "t1", failed); err != nil || !released {
		t.Fatalf("release after the run ended: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1").Launch; got.CollectFailures != 1 || !got.CollectFailingSince.Equal(later) {
		t.Errorf("the job's record = %d failures since %v; want 1 since %v",
			got.CollectFailures, got.CollectFailingSince, later)
	}

	// A release cannot say a collection both read the box and failed to.
	claimed := mustClaim(t, s, "t1")
	both := releaseOf(claimed)
	both.Collected, both.CollectFailedAt = true, later
	if _, err := s.ReleaseClaim(t.Context(), "t1", both); err == nil {
		t.Error("a release claiming a collection both read and failed was accepted")
	}
	mustRelease(t, s, claimed)

	// And the next launch is a new job, with an allowance of its own.
	mustBeginLaunch(t, s, run("t1"))
	if got := mustGet(t, s, "t1").Launch; got.CollectFailures != 0 || !got.CollectFailingSince.IsZero() {
		t.Errorf("a new launch inherited the last job's failed collections: %+v", got)
	}
}

// A RELEASE WHOSE COLLECTION READ THE BOX ENDS THE RUN OF FAILURES WITHOUT
// COLLECTING THE JOB. A collection whose read answered, and that a later step
// handed back uncounted (a coordination store that did not answer the step, or
// a node that stopped), ends the job's run of failed collections as a
// collected one does, so the next failure starts a run of its own. But the job
// is not collected: it stays stoppable, and the collection made again charges
// and records it. And a release cannot both count a failed collection and end
// the run of failures it counts onto.
func testAReleaseWhoseReadAnsweredEndsTheRunOfFailures(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t1"))
	first := base.Add(3 * time.Second)
	failed := releaseOf(mustClaim(t, s, "t1"))
	failed.CollectFailedAt = first
	if released, err := s.ReleaseClaim(ctx, "t1", failed); err != nil || !released {
		t.Fatalf("release of a failed collection = %v, %v", released, err)
	}

	answered := releaseOf(mustClaim(t, s, "t1"))
	answered.BoxAnswered = true
	if released, err := s.ReleaseClaim(ctx, "t1", answered); err != nil || !released {
		t.Fatalf("release of a collection whose read answered = %v, %v", released, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusRunning || got.Launch.CollectFailures != 0 ||
		!got.Launch.CollectFailingSince.IsZero() {
		t.Fatalf("the job's row reads %s with %d failed collections since %v; want it running with its "+
			"run of failures ended", got.Status, got.Launch.CollectFailures, got.Launch.CollectFailingSince)
	}
	if got.Launch.Collected || !got.Launch.CollectedAt.IsZero() {
		t.Fatalf("a release whose read answered recorded the job collected (%v at %v)", got.Launch.Collected,
			got.Launch.CollectedAt)
	}
	stop := sandbox.RunStop{By: "founder"}
	if decided, ok, err := s.DecideEnding(ctx, "t1", sandbox.Decision{
		License: sandbox.License{WhileIn: sandbox.Stoppable, Launch: launch}, Reclaim: true, Stop: &stop,
	}); err != nil || !ok || decided.Stop == nil {
		t.Fatalf("a stop of a job whose read answered = %+v, %v, %v; want it decided, the job uncollected",
			decided.Stop, ok, err)
	}

	mustLaunched(t, s, run("t2"))
	claimed := mustClaim(t, s, "t2")
	both := releaseOf(claimed)
	both.BoxAnswered, both.CollectFailedAt = true, first
	if _, err := s.ReleaseClaim(ctx, "t2", both); err == nil {
		t.Error("a release that both counts a failed collection and ends the run it counts onto was accepted")
	}
	if got := mustGet(t, s, "t2"); got.Status != sandbox.StatusResumed || got.Launch.CollectFailures != 0 {
		t.Errorf("the refused release wrote the row: %s with %d failed collections", got.Status,
			got.Launch.CollectFailures)
	}
	mustRelease(t, s, claimed)
}

// A RELEASE RECORDS WHAT THE STORE OF THE HOST OF THE JOB'S BOX LAST SAID, on
// the job's record, where its claim learned anything of it: a release whose
// collection found that store silent marks the job with when its clock
// started, a later one with the later start, one that learned that store
// answers clears the mark, and one that learned nothing leaves it as it
// stands. A release cannot say both, and one that does is refused and writes
// nothing. The job's next launch starts without it: the host of its box, and
// what that host's store says, are that job's.
func testAReleaseRecordsWhatTheHostsStoreLastSaid(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	first := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	later := first.Add(time.Minute)
	mustLaunched(t, s, run("t1"))
	silent := releaseOf(mustClaim(t, s, "t1"))
	silent.HostStoreSilentSince = first
	if released, err := s.ReleaseClaim(ctx, "t1", silent); err != nil || !released {
		t.Fatalf("release of a collection whose host found its store silent = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning ||
		!got.Launch.HostStoreSilentSince.Equal(first) {
		t.Fatalf("the row reads %s, its host's store silent since %v; want it running and marked since %v",
			got.Status, got.Launch.HostStoreSilentSince, first)
	}

	mustRelease(t, s, mustClaim(t, s, "t1"))
	if got := mustGet(t, s, "t1"); !got.Launch.HostStoreSilentSince.Equal(first) {
		t.Fatalf("a release that learned nothing of the host's store left it silent since %v; want %v",
			got.Launch.HostStoreSilentSince, first)
	}

	again := releaseOf(mustClaim(t, s, "t1"))
	again.HostStoreSilentSince = later
	if released, err := s.ReleaseClaim(ctx, "t1", again); err != nil || !released {
		t.Fatalf("release of a later collection whose host found its store silent = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); !got.Launch.HostStoreSilentSince.Equal(later) {
		t.Fatalf("a later clock's release left the job silent since %v; want the later start, %v",
			got.Launch.HostStoreSilentSince, later)
	}

	claimed := mustClaim(t, s, "t1")
	both := releaseOf(claimed)
	both.HostStoreSilentSince, both.HostStoreAnswered = first, true
	if _, err := s.ReleaseClaim(ctx, "t1", both); err == nil {
		t.Error("a release that says the host's store was both silent and answering was accepted")
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed ||
		!got.Launch.HostStoreSilentSince.Equal(later) {
		t.Errorf("the refused release wrote the row: %s, its host's store silent since %v", got.Status,
			got.Launch.HostStoreSilentSince)
	}
	answered := releaseOf(claimed)
	answered.HostStoreAnswered = true
	if released, err := s.ReleaseClaim(ctx, "t1", answered); err != nil || !released {
		t.Fatalf("release of a claim that learned the host's store answers = %v, %v", released, err)
	}
	if got := mustGet(t, s, "t1"); !got.Launch.HostStoreSilentSince.IsZero() {
		t.Fatalf("a release that learned the host's store answers left the job silent since %v",
			got.Launch.HostStoreSilentSince)
	}

	marked := releaseOf(mustClaim(t, s, "t1"))
	marked.HostStoreSilentSince = first
	if released, err := s.ReleaseClaim(ctx, "t1", marked); err != nil || !released {
		t.Fatalf("release marking the job again = %v, %v", released, err)
	}
	mustBeginLaunch(t, s, run("t1"))
	if got := mustGet(t, s, "t1"); !got.Launch.HostStoreSilentSince.IsZero() {
		t.Error("the job's next launch inherited what the last job's host's store said")
	}
}

// AN OWED REFUSAL IS RECORDED ONCE, ON A RUNNING JOB, AND SETTLED BY THE PARK.
// The seat's holder records a refused delivery of a job's completion it
// acknowledges: the job's record takes the condition and the instant, a second
// refusal writes nothing and leaves the first standing, and one that names no
// condition, one past the bound, or no instant is refused. A claim leaves it,
// a claim handed back puts the completion back still owed, the park on the
// job's question clears it in its own write, and the job's next launch starts
// without it. Only a running job of the launch named takes it, and a row a
// newer lease owns refuses it.
func testAnOwedRefusalIsRecordedOnceAndSettledByThePark(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	first := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	launch := mustLaunched(t, s, run("t1"))
	row := mustGet(t, s, "t1")
	fence := sandbox.Fence{Owner: row.Owner, Epoch: row.OwnerEpoch}
	owe := func(owed sandbox.OwedRefusal) (bool, error) {
		return s.OweRefusal(ctx, "t1", launch, owed, fence)
	}
	for _, bad := range []sandbox.OwedRefusal{
		{Since: first},
		{Condition: sandbox.Condition(strings.Repeat("c", sandbox.MaxOwedConditionBytes+1)), Since: first},
		{Condition: "provider"},
	} {
		if wrote, err := owe(bad); err == nil || wrote {
			t.Errorf("an owed refusal %+v was taken (%v, %v); want it refused", bad, wrote, err)
		}
	}
	if got := mustGet(t, s, "t1"); got.Launch.RefusalOwed != "" {
		t.Fatalf("a refused record wrote the row: it owes %q", got.Launch.RefusalOwed)
	}
	if wrote, err := owe(sandbox.OwedRefusal{Condition: "provider", Since: first}); err != nil || !wrote {
		t.Fatalf("an owed refusal on a running job = %v, %v; want it written", wrote, err)
	}
	if wrote, err := owe(sandbox.OwedRefusal{Condition: "pause", Since: first.Add(time.Minute)}); err != nil || wrote {
		t.Fatalf("a second owed refusal = %v, %v; want nothing written", wrote, err)
	}
	if got := mustGet(t, s, "t1"); got.Launch.RefusalOwed != "provider" ||
		!got.Launch.RefusalOwedSince.Equal(first) {
		t.Fatalf("the job's record owes %q since %v; want the first refusal's, provider since %v",
			got.Launch.RefusalOwed, got.Launch.RefusalOwedSince, first)
	}
	if wrote, err := s.OweRefusal(ctx, "t1", "another-launch", sandbox.OwedRefusal{Condition: "pause",
		Since: first}, fence); err != nil || wrote {
		t.Errorf("an owed refusal of another launch = %v, %v; want nothing written", wrote, err)
	}

	claimed := mustClaim(t, s, "t1")
	if wrote, err := owe(sandbox.OwedRefusal{Condition: "pause", Since: first}); err != nil || wrote {
		t.Errorf("a second owed refusal on a claimed job = %v, %v; want nothing written", wrote, err)
	}
	mustRelease(t, s, claimed)
	if got := mustGet(t, s, "t1"); got.Launch.RefusalOwed != "provider" {
		t.Fatalf("a claim handed back left the job's record owing %q; want it still owed", got.Launch.RefusalOwed)
	}
	newer := sandbox.Fence{Owner: "node-c:1", Epoch: fence.Epoch + 5}
	if err := s.MarkAwaiting(ctx, "t1", sandbox.Clarification{AskedAt: first}); err != nil {
		t.Fatalf("parking the run: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Launch.RefusalOwed != "" || !got.Launch.RefusalOwedSince.IsZero() {
		t.Fatalf("the park left the job's record owing %q since %v; want it settled", got.Launch.RefusalOwed,
			got.Launch.RefusalOwedSince)
	}

	launch = mustLaunched(t, s, run("t2"))
	if won, err := s.ClaimOwnership(ctx, "t2", newer.Owner, newer.Epoch); err != nil || !won {
		t.Fatalf("fencing t2 to a newer lease = %v, %v", won, err)
	}
	if wrote, err := s.OweRefusal(ctx, "t2", launch, sandbox.OwedRefusal{Condition: "provider", Since: first},
		sandbox.Fence{Owner: "node-a:1", Epoch: newer.Epoch - 1}); !errors.Is(err, sandbox.ErrSeatNotHeld) || wrote {
		t.Errorf("an owed refusal under a lease a newer one outranks = %v, %v; want it refused, the seat not "+
			"held", wrote, err)
	}
	if wrote, err := s.OweRefusal(ctx, "t2", launch, sandbox.OwedRefusal{Condition: "provider", Since: first},
		newer); err != nil || !wrote {
		t.Fatalf("an owed refusal under the row's lease = %v, %v; want it written", wrote, err)
	}
	mustBeginLaunch(t, s, run("t2"))
	if got := mustGet(t, s, "t2"); got.Launch.RefusalOwed != "" {
		t.Error("the job's next launch inherited the refusal the last job owed")
	}
}

// AN OWED REFUSAL IS RECORDED ON A JOB CLAIMED FOR ITS COMPLETION, NEVER ON A
// RECORDED ANSWER'S CLAIM. A claim settles nothing by itself: one handed back
// puts the completion back as it was, so a refusal acknowledged while another
// attempt holds the job's claim is recorded on the claimed row, and is still
// owed once that claim is handed back. A recorded answer's claim holds a job
// that parked on its question, which settled its completion, and takes none.
func testAnOwedRefusalIsRecordedOnAClaimOfItsCompletion(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	owed := sandbox.OwedRefusal{Condition: "provider", Since: at}
	launch := mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	fence := sandbox.Fence{Owner: claimed.Owner, Epoch: claimed.OwnerEpoch}
	if wrote, err := s.OweRefusal(ctx, "t1", launch, owed, fence); err != nil || !wrote {
		t.Fatalf("an owed refusal on a job claimed for its completion = %v, %v; want it written", wrote, err)
	}
	mustRelease(t, s, claimed)
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning || got.Launch.RefusalOwed != "provider" ||
		!got.Launch.RefusalOwedSince.Equal(at) {
		t.Fatalf("the claim handed back left the run %s owing %q since %v; want it running and still owing "+
			"provider since %v", got.Status, got.Launch.RefusalOwed, got.Launch.RefusalOwedSince, at)
	}

	asking := claimedAnswerOn(t, s, "t2", answerOf("r1", "use main"), false)
	if wrote, err := s.OweRefusal(ctx, "t2", asking, owed, sandbox.Fence{}); err != nil || wrote {
		t.Errorf("an owed refusal on a recorded answer's claim = %v, %v; want nothing written", wrote, err)
	}
	if got := mustGet(t, s, "t2"); got.Launch.RefusalOwed != "" {
		t.Errorf("a recorded answer's claim owes %q; want nothing owed of a job its question settled",
			got.Launch.RefusalOwed)
	}
}

func testAReleaseNeverClearsAChargeRecord(t *testing.T, s sandbox.PendingStore) {
	// A charge that landed stays landed. A later release that says nothing
	// about it (a retry whose resume failed too, having charged nothing
	// because the record said not to) must not erase what the first
	// attempt recorded, or the retry after it charges the run again.
	mustLaunched(t, s, run("t1"))
	mustReleaseCharged(t, s, mustClaim(t, s, "t1"))
	retry := mustClaim(t, s, "t1")
	retry.Charged = false
	mustRelease(t, s, retry)
	if got := mustGet(t, s, "t1"); !got.Charged {
		t.Error("a release that carried no charge erased the record of one")
	}
}

func testARefusedReleaseRecordsNoCharge(t *testing.T, s sandbox.PendingStore) {
	// A release that hands nothing back writes nothing, the record included:
	// on a row a second launch has opened it would let that job's own spend
	// go uncounted.
	mustLaunched(t, s, run("t1"))
	claimed := mustClaim(t, s, "t1")
	mustBeginLaunch(t, s, run("t1"))
	release := releaseOf(claimed)
	release.Charged = true
	if released, err := s.ReleaseClaim(t.Context(), "t1", release); err != nil || released {
		t.Fatalf("the claim handed back a launch it never took: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Charged {
		t.Error("a refused release recorded its charge on the next launch")
	}
}

func testOnlyALaunchClearsAChargeRecord(t *testing.T, s sandbox.PendingStore) {
	// The record is launch-scoped, and every write to the row other than a
	// launch is about the same launch. One of them dropping it would let the
	// retry that write opens charge the run again, so each is walked here;
	// the launch that follows is a new job, and must not inherit it.
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t1"))
	mustReleaseCharged(t, s, mustClaim(t, s, "t1"))
	tail := completionOf(t, s, "t1")
	var claimed sandbox.PendingRun
	for _, step := range []struct {
		name  string
		write func() error
	}{
		{"pause the box", func() error { return s.MarkBoxPaused(ctx, "t1", base) }},
		{"take ownership", func() error {
			_, err := s.ClaimOwnership(ctx, "t1", "node-b:2", 3)
			return err
		}},
		{"claim the tail", func() error {
			var err error
			claimed, _, err = s.ClaimForResume(ctx, "t1", tail, sandbox.Fence{Owner: "node-b:2", Epoch: 3})
			return err
		}},
		{"hand the claim back", func() error {
			_, err := s.ReleaseClaim(ctx, "t1", releaseOf(claimed))
			return err
		}},
		{"change status", func() error {
			return s.SetStatus(ctx, "t1", sandbox.StatusRunning, sandbox.Fence{})
		}},
		{"park on a question", func() error {
			return s.MarkAwaiting(ctx, "t1", sandbox.Clarification{AskedAt: time.Now().UTC()})
		}},
		{"expire the pause", func() error {
			_, err := s.ExpirePause(ctx, "t1")
			return err
		}},
		{"attach a box", func() error {
			return s.AttachSandbox(ctx, "t1", sandbox.BoxRef{SandboxID: "box-2"}, sandbox.Fence{})
		}},
		{"release the box", func() error { return s.ReleaseBox(ctx, "t1") }},
		{"append a bridged call", func() error {
			recorded, err := s.AppendBridgeCall(ctx, "t1", sandbox.BridgeAppend{
				Launch: launch, Call: sandbox.BridgeCall{Name: "read_page", At: base}})
			if err == nil && !recorded {
				return errors.New("the append was not recorded, so it exercised nothing")
			}
			return err
		}},
	} {
		if err := step.write(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := mustGet(t, s, "t1"); !got.Charged {
			t.Fatalf("%s dropped the charge record", step.name)
		}
	}

	mustBeginLaunch(t, s, run("t1"))
	if got := mustGet(t, s, "t1"); got.Charged {
		t.Error("a second launch inherited the first job's charge, so its own spend would go uncounted")
	}
}

func testReleasingABoxClearsBothHalves(t *testing.T, s sandbox.PendingStore) {
	// A paused_at pointing at no box is a paused box the reaper looks for
	// every tick and never finds — a warning per tick, for ever.
	mustLaunched(t, s, run("t1"))
	if err := s.AttachSandbox(t.Context(), "t1",
		sandbox.BoxRef{SandboxID: "box-1", CommandID: "c-1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.MarkBoxPaused(t.Context(), "t1", base); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := mustGet(t, s, "t1"); !got.Paused() || !got.HasBox() {
		t.Fatalf("run = %+v", got)
	}
	if err := s.ReleaseBox(t.Context(), "t1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// And the box's date goes with the box: a row naming no box has no age
	// for a relaunch to read ([sandbox.PendingRun.BoxSince]).
	got := mustGet(t, s, "t1")
	if got.Paused() || got.HasBox() || got.CommandID != "" || !got.BoxSince.IsZero() {
		t.Errorf("release left %+v", got)
	}
}
