package sandboxtest

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A HELD RELEASE HOLDS THE RESUME UNTIL ITS CONDITION LIFTS: the release that
// hands back a claim its seat's conditions refused writes the hold, a lift for
// another condition leaves it, a lift for its own clears it — and the next
// release clears whatever an earlier one held, so no hold outlasts the attempt
// it held back.
func testAHeldReleaseHoldsTheResumeUntilItsConditionLifts(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	until := time.Date(2026, time.September, 24, 0, 0, 0, 0, time.UTC)
	held := releaseOf(mustClaim(t, s, "t1"))
	held.Hold = &sandbox.Refusal{Condition: "budget", Until: until}
	if released, err := s.ReleaseClaim(t.Context(), "t1", held); err != nil || !released {
		t.Fatalf("held release: released=%v err=%v", released, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusRunning || got.HeldFor != "budget" || !got.HeldUntil.Equal(until) {
		t.Fatalf("row = %s held for %q until %v, want running and held for budget until %v",
			got.Status, got.HeldFor, got.HeldUntil, until)
	}
	if !got.Held(until.Add(-time.Second)) || got.Held(until) {
		t.Error("the row is not held exactly until its refusal lifts")
	}
	if lifted, err := s.LiftHold(t.Context(), "t1", "pause", got.Holds); err != nil || lifted {
		t.Fatalf("a lift for another condition: lifted=%v err=%v", lifted, err)
	}
	if lifted, err := s.LiftHold(t.Context(), "t1", "budget", got.Holds); err != nil || !lifted {
		t.Fatalf("a lift for its own condition: lifted=%v err=%v", lifted, err)
	}
	if got := mustGet(t, s, "t1"); got.HeldFor != "" || !got.HeldUntil.IsZero() {
		t.Fatalf("the lifted row is still held for %q until %v", got.HeldFor, got.HeldUntil)
	}
	// PAST ITS INSTANT, so the next claim takes the run: a hold that still
	// stands refuses every claim (testAHeldRunIsClaimedByNoAnnouncementUntilItsHoldLifts).
	again := releaseOf(mustClaim(t, s, "t1"))
	again.Hold = &sandbox.Refusal{Condition: "budget", Until: time.Now().UTC().Add(-time.Minute)}
	if released, err := s.ReleaseClaim(t.Context(), "t1", again); err != nil || !released {
		t.Fatalf("held release: released=%v err=%v", released, err)
	}
	mustRelease(t, s, mustClaim(t, s, "t1"))
	if got := mustGet(t, s, "t1"); got.HeldFor != "" {
		t.Fatalf("an ordinary release kept the earlier hold for %q", got.HeldFor)
	}
}

// A LIFT CLEARS ONLY THE HOLD IT READ: a lift asked from a read made before a
// newer hold was written (a claim and its resume, held again, between them)
// leaves that hold standing, since nothing has signalled its condition since.
func testALiftClearsOnlyTheHoldItRead(t *testing.T, s sandbox.PendingStore) {
	until := time.Now().UTC().Add(time.Hour)
	mustLaunched(t, s, run("t1"))
	hold := func() sandbox.PendingRun {
		t.Helper()
		held := releaseOf(mustClaim(t, s, "t1"))
		held.Hold = &sandbox.Refusal{Condition: "budget", Until: until}
		if released, err := s.ReleaseClaim(t.Context(), "t1", held); err != nil || !released {
			t.Fatalf("held release: released=%v err=%v", released, err)
		}
		return mustGet(t, s, "t1")
	}
	read := hold()
	if lifted, err := s.LiftHold(t.Context(), "t1", "budget", read.Holds); err != nil || !lifted {
		t.Fatalf("a lift of the hold read: lifted=%v err=%v", lifted, err)
	}
	newer := hold()
	if newer.Holds == read.Holds {
		t.Fatalf("two holds share the number %d", read.Holds)
	}
	if lifted, err := s.LiftHold(t.Context(), "t1", "budget", read.Holds); err != nil || lifted {
		t.Fatalf("a lift of the earlier hold cleared the newer one: lifted=%v err=%v", lifted, err)
	}
	if got := mustGet(t, s, "t1"); got.HeldFor != "budget" || got.Holds != newer.Holds {
		t.Fatalf("the row is held for %q (hold %d); want the newer hold %d standing", got.HeldFor, got.Holds,
			newer.Holds)
	}
	if lifted, err := s.LiftHold(t.Context(), "t1", "budget", newer.Holds); err != nil || !lifted {
		t.Fatalf("a lift of the newer hold: lifted=%v err=%v", lifted, err)
	}
}

// A HELD RUN IS NO CLAIM'S UNTIL ITS HOLD LIFTS. The claim is the resume, and
// the hold says the resume waits. The waiter announces nothing for a held run,
// but an announcement made before the hold was written can still be on its way,
// queued behind the very resume that held the run: claimed, it collected the
// job again, waking the box the hold keeps asleep, and ran its review into the
// same refusal. Refused while the hold stands, the run is claimed once the hold
// is lifted, or once its instant has passed.
func testAHeldRunIsClaimedByNoAnnouncementUntilItsHoldLifts(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	held := releaseOf(mustClaim(t, s, "t1"))
	held.Hold = &sandbox.Refusal{Condition: "budget", Until: time.Now().UTC().Add(time.Hour)}
	if released, err := s.ReleaseClaim(t.Context(), "t1", held); err != nil || !released {
		t.Fatalf("held release: released=%v err=%v", released, err)
	}
	row := mustGet(t, s, "t1")
	fence := sandbox.Fence{Owner: row.Owner, Epoch: row.OwnerEpoch}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1"), fence); err != nil || won {
		t.Fatalf("a claim of a held run: won=%v err=%v, want it refused while the hold stands", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning || got.HeldFor != "budget" {
		t.Fatalf("the refused claim moved the run to %s held for %q, want it running and held",
			got.Status, got.HeldFor)
	}
	if lifted, err := s.LiftHold(t.Context(), "t1", "budget", row.Holds); err != nil || !lifted {
		t.Fatalf("lift: lifted=%v err=%v", lifted, err)
	}
	mustRelease(t, s, mustClaim(t, s, "t1"))

	// AND ONCE ITS INSTANT HAS PASSED, though nothing lifted it.
	expired := releaseOf(mustClaim(t, s, "t1"))
	expired.Hold = &sandbox.Refusal{Condition: "budget", Until: time.Now().UTC().Add(-time.Minute)}
	if released, err := s.ReleaseClaim(t.Context(), "t1", expired); err != nil || !released {
		t.Fatalf("held release: released=%v err=%v", released, err)
	}
	mustClaim(t, s, "t1")
}

// A HELD RELEASE OWES ITS COMPLETION ON THE JOB'S RECORD. The hold spends the
// completion's delivery, and a lift clears the hold, so the release that writes
// it records the completion as owed in the same write: the hold's condition and
// the store's instant, which a lift of the hold leaves, so a restart after the
// lift still finds the job owed. Only where the record owes none yet, so an
// earlier refusal's condition and instant stand; and a hold whose condition is
// past what an owed refusal records is refused, the claim left as it was.
func testAHeldReleaseOwesItsCompletionOnTheJobsRecord(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	until := time.Now().UTC().Add(time.Hour)
	mustLaunched(t, s, run("t1"))
	held := releaseOf(mustClaim(t, s, "t1"))
	held.Hold = &sandbox.Refusal{Condition: "budget", Until: until}
	if released, err := s.ReleaseClaim(ctx, "t1", held); err != nil || !released {
		t.Fatalf("held release: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t1"); got.Launch.RefusalOwed != "budget" || got.Launch.RefusalOwedSince.IsZero() {
		t.Fatalf("the held job's record owes %q since %v; want the hold's condition owed, since the release",
			got.Launch.RefusalOwed, got.Launch.RefusalOwedSince)
	}
	if lifted, err := s.LiftHold(ctx, "t1", "budget", mustGet(t, s, "t1").Holds); err != nil || !lifted {
		t.Fatalf("a lift of the hold: lifted=%v err=%v", lifted, err)
	}
	if lifted := mustGet(t, s, "t1"); lifted.HeldFor != "" || lifted.Launch.RefusalOwed != "budget" {
		t.Fatalf("the lifted row is held for %q and owes %q; want the hold gone and the completion still owed",
			lifted.HeldFor, lifted.Launch.RefusalOwed)
	}

	// AN EARLIER REFUSAL STANDS.
	first := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	launch := mustLaunched(t, s, run("t2"))
	row := mustGet(t, s, "t2")
	if wrote, err := s.OweRefusal(ctx, "t2", launch, sandbox.OwedRefusal{Condition: "provider", Since: first},
		sandbox.Fence{Owner: row.Owner, Epoch: row.OwnerEpoch}); err != nil || !wrote {
		t.Fatalf("an owed refusal on a running job = %v, %v; want it written", wrote, err)
	}
	again := releaseOf(mustClaim(t, s, "t2"))
	again.Hold = &sandbox.Refusal{Condition: "budget", Until: until}
	if released, err := s.ReleaseClaim(ctx, "t2", again); err != nil || !released {
		t.Fatalf("held release: released=%v err=%v", released, err)
	}
	if got := mustGet(t, s, "t2"); got.HeldFor != "budget" || got.Launch.RefusalOwed != "provider" ||
		!got.Launch.RefusalOwedSince.Equal(first) {
		t.Fatalf("the held job's record, owing provider since %v before its hold, is held for %q and owes %q "+
			"since %v; want it held and the first refusal standing", first, got.HeldFor,
			got.Launch.RefusalOwed, got.Launch.RefusalOwedSince)
	}

	// PAST THE BOUND, refused, the claim standing.
	mustLaunched(t, s, run("t3"))
	long := releaseOf(mustClaim(t, s, "t3"))
	long.Hold = &sandbox.Refusal{Condition: sandbox.Condition(strings.Repeat("c", sandbox.MaxOwedConditionBytes+1)),
		Until: until}
	if released, err := s.ReleaseClaim(ctx, "t3", long); err == nil || released {
		t.Fatalf("a release holding the run for a condition past the bound = %v, %v; want it refused",
			released, err)
	}
	if got := mustGet(t, s, "t3"); got.Status != sandbox.StatusResumed || got.HeldFor != "" ||
		got.Launch.RefusalOwed != "" {
		t.Fatalf("the refused release left the run %s, held for %q, owing %q; want the claim as it was",
			got.Status, got.HeldFor, got.Launch.RefusalOwed)
	}
}
