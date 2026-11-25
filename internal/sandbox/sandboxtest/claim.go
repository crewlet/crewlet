package sandboxtest

import (
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

func testTheTailIsClaimedExactlyOnce(t *testing.T, s sandbox.PendingStore) {
	// THE AT-MOST-ONCE GATE. Two nodes both resuming one parked turn from
	// one job's result produce two turns from one job — which the seat sees
	// as its own work arriving twice.
	mustLaunched(t, s, run("t1"))
	tail := completionOf(t, s, "t1")
	if _, won, err := s.ClaimForResume(t.Context(), "t1", tail, sandbox.Fence{}); err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", tail, sandbox.Fence{}); err != nil || won {
		t.Errorf("a second claim won: won=%v err=%v", won, err)
	}
}

func testAClaimIsExclusiveUnderContention(t *testing.T, s sandbox.PendingStore) {
	// The property a fake cannot have. Ten goroutines racing one tail:
	// exactly one may win.
	mustLaunched(t, s, run("t1"))
	tail := completionOf(t, s, "t1")
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range 10 {
		wg.Go(func() {
			if _, won, err := s.ClaimForResume(t.Context(), "t1", tail, sandbox.Fence{}); err == nil && won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d claims won; exactly one may", wins)
	}
}

func testAClaimReportsWhereItCameFrom(t *testing.T, s sandbox.PendingStore) {
	// A failed resume dispatch reverts to EXACTLY the prior status so the
	// NAK'd trigger can re-claim on redelivery. Inferring it afterwards is
	// unsound: a reused run keeps its old question, so "has a question"
	// does not mean "was parked".
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(t.Context(), "t1",
		sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"), sandbox.Fence{})
	if err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if got.ClaimedFrom != sandbox.StatusAwaiting {
		t.Errorf("claimed_from = %q, want the parked status", got.ClaimedFrom)
	}
	if got.Status != sandbox.StatusResumed {
		t.Errorf("the returned row carries %q, want the POST-flip status", got.Status)
	}
}

func testAReseedIsStillClaimable(t *testing.T, s sandbox.PendingStore) {
	// Reaping the box does NOT end the run — the answer can still arrive,
	// and the work re-seeds from the pushed branch. That is the whole
	// reason reseed is a state rather than a deletion.
	mustLaunched(t, s, run("t1"))
	if err := s.SetStatus(t.Context(), "t1", sandbox.StatusReseed, sandbox.Fence{}); err != nil {
		t.Fatalf("set reseed: %v", err)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"), sandbox.Fence{}); err != nil || !won {
		t.Errorf("a reseeded run could not be claimed: won=%v err=%v", won, err)
	}
}

func testAClaimForAnotherLaunchIsRefused(t *testing.T, s sandbox.PendingStore) {
	// A completion that outlived its job arrives at a row holding the next
	// one. The status alone cannot tell them apart, since both are running;
	// only the name can.
	mustLaunched(t, s, run("t1"))
	stale := completionOf(t, s, "t1")
	mustBeginLaunch(t, s, run("t1"))
	if suspended, err := s.MarkSuspended(t.Context(), "t1", suspension()); err != nil || !suspended {
		t.Fatalf("mark suspended: suspended=%v err=%v", suspended, err)
	}

	if _, won, err := s.ClaimForResume(t.Context(), "t1", stale, sandbox.Fence{}); err != nil || won {
		t.Fatalf("the previous job's completion claimed the next job: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Fatalf("a refused claim moved the row to %q", got.Status)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil || !won {
		t.Errorf("the next job's own completion could not claim it: won=%v err=%v", won, err)
	}
}

func testACompletionDoesNotClaimAParkedRun(t *testing.T, s sandbox.PendingStore) {
	// A parked run is waiting on a person, and only their answer may take
	// it. A completion that finds it there is a duplicate of the one that
	// parked it.
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil || won {
		t.Fatalf("a completion claimed a parked run: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAwaiting {
		t.Errorf("status = %q, want the run still waiting on its answer", got.Status)
	}
}

func testAnAnswerDoesNotClaimARunningRun(t *testing.T, s sandbox.PendingStore) {
	// And the other way round: a running job asked nothing, so an answer
	// that finds one has nothing to answer.
	mustLaunched(t, s, run("t1"))
	if _, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"), sandbox.Fence{}); err != nil || won {
		t.Fatalf("an answer claimed a running job: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Errorf("status = %q, want the job still running", got.Status)
	}
}
