package sandboxtest

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

func testParkingCarriesTheBranch(t *testing.T, s sandbox.PendingStore) {
	// The WIP is pushed BEFORE the question is asked, so a paused box reaped
	// days later loses nothing a re-seed cannot recover. A question parked
	// over unpushed work is a question whose answer arrives to an empty box.
	mustLaunched(t, s, run("t1"))
	asked := Question("which base branch?", sandbox.AskToRequester)
	asked.Branch = "wip/swe/t1"
	Ask(t, s, "t1", asked)
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{
		AskedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAwaiting || got.Branch != "wip/swe/t1" ||
		got.Question != "which base branch?" || got.Audience != "requester" {
		t.Errorf("parked run = %+v", got)
	}
}

// A QUESTION IS PARKED WITH ITS ANCHOR, or not at all: the instant it was asked
// is what every answer is measured against, and a question parked without one
// is one no reply could be shown to answer. The refused park leaves the run as
// it was.
func testAQuestionIsParkedWithItsAnchor(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	if err := s.MarkAwaiting(ctx, "t1", sandbox.Clarification{}); err == nil {
		t.Fatal("a park with no instant the question was asked at was accepted")
	}
	if got := mustGet(t, s, "t1"); got.Status == sandbox.StatusAwaiting || got.Question != "" {
		t.Fatalf("run %q asking %q, want the refused park to have written nothing", got.Status, got.Question)
	}
	// THE ANCHOR STATED, years from any clock and to the nanosecond: a store
	// that parked the question at its own clock's instant, not the one it was
	// handed, is told apart from one that kept it, which an anchor read from the
	// clock just before the park equals inside one tick.
	asked := time.Date(2003, time.April, 5, 6, 7, 8, 9, time.UTC)
	if err := s.MarkAwaiting(ctx, "t1", sandbox.Clarification{AskedAt: asked}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusAwaiting || !got.AskedAt.Equal(asked) {
		t.Fatalf("run %q asked at %v, want it parked at %v", got.Status, got.AskedAt, asked)
	}
}

// The reaper's flip is the AUTHORITY for the whole reap — it decides whether
// the box gets destroyed — so two reapers racing must produce exactly one
// destruction.
func testAPauseExpiresExactlyOnce(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")

	const racers = 10
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range racers {
		wg.Go(func() {
			<-start
			won, err := s.ExpirePause(t.Context(), "t1")
			if err != nil {
				t.Errorf("ExpirePause: %v", err)
				return
			}
			if won {
				wins.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("%d of %d reapers won the flip, want exactly 1 — each winner kills a box", got, racers)
	}
	got, _, err := s.Get(t.Context(), "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != sandbox.StatusReseed {
		t.Fatalf("status = %q, want %q", got.Status, sandbox.StatusReseed)
	}
}

// Every other paused box belongs to a tail that is actively being driven, and
// expiring one from the reaper would kill it out from under live work.
func testOnlyAParkedRunCanExpire(t *testing.T, s sandbox.PendingStore) {
	for _, status := range []string{
		sandbox.StatusRunning, sandbox.StatusResumed, sandbox.StatusReseed,
	} {
		mustLaunched(t, s, run(status))
		if err := s.SetStatus(t.Context(), status, status, sandbox.Fence{}); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		won, err := s.ExpirePause(t.Context(), status)
		if err != nil {
			t.Fatalf("ExpirePause(%s): %v", status, err)
		}
		if won {
			t.Fatalf("a run in %q was expired by the pause reaper", status)
		}
	}
}

// The answer that un-parks a run and the reaper that expires it are the same
// race, from the two sides: whichever lands first, the other must lose.
func testAnAnsweredRunCannotBeExpiredUnderTheResume(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")

	if _, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"), sandbox.Fence{}); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	won, err := s.ExpirePause(t.Context(), "t1")
	if err != nil {
		t.Fatalf("ExpirePause: %v", err)
	}
	if won {
		t.Fatal("the reaper expired a run whose answer had already claimed it — it would destroy the box the resume is reconnecting to")
	}
}

// A recorded answer waits on its resume, which waits on the seat's holder and
// its conditions — on a seat no node holds, for as long as nobody takes it.
// Its box is held for that whole wait, so the reaper expires it exactly as it
// would a parked run's: the answer and its debt survive, the box does not, and
// letting the answer go reopens the question on a run with nothing to
// reconnect to.
func testAnAnswerWaitingOnItsResumeStillExpiresTheBox(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	if err := s.MarkBoxPaused(ctx, "t1", base); err != nil {
		t.Fatalf("MarkBoxPaused: %v", err)
	}
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if held, ok := mustGet(t, s, "t1").HeldSince(); !ok || !held.Equal(base) {
		t.Fatalf("HeldSince = %v, %v: an answered run's box is still being held", held, ok)
	}

	won, err := s.ExpirePause(ctx, "t1")
	if err != nil || !won {
		t.Fatalf("ExpirePause = %v, %v: the box of an answer waiting on its resume was never reclaimed", won, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAnswered || got.Answer == nil || !bytes.Equal(got.Answer.Sealed, sealOf("use main")) {
		t.Fatalf("run = %q answer %+v, want the answer still recorded and owed", got.Status, got.Answer)
	}
	if got.SandboxID != "" || got.CommandID != "" || !got.PausedAt.IsZero() {
		t.Fatalf("the row still names the box being destroyed: %+v", got)
	}
	if _, held := got.HeldSince(); held {
		t.Fatal("the row still reads as holding a box")
	}

	if ok, err := decline(t, s, launch, "r1"); err != nil || !ok {
		t.Fatalf("DeclineAnswer = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusReseed {
		t.Fatalf("a let-go answer reopened the question as %q, want %q — the box it was parked in is gone",
			got.Status, sandbox.StatusReseed)
	}
}

// An answered run whose resume already claimed it is not the reaper's.
func testAClaimedAnswerIsNotExpired(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	launch := mustGet(t, s, "t1").LaunchID
	if _, ok, err := s.RecordAnswer(ctx, "t1", launch, answerOf("r1", "use main"), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	if _, won, err := s.ClaimForResume(ctx, "t1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{}); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	if won, err := s.ExpirePause(ctx, "t1"); err != nil || won {
		t.Fatalf("ExpirePause = %v, %v: the reaper took a box the answer's resume is reconnecting to", won, err)
	}
}

// park has a seeded run ask a question and moves it to awaiting_clarification
// with a box attached.
func park(t *testing.T, s sandbox.PendingStore, turnID string) {
	t.Helper()
	ctx := t.Context()
	if err := s.AttachSandbox(ctx, turnID, sandbox.BoxRef{
		SandboxID: "box-" + turnID, Harness: "claude-code", PauseTTLSec: 1800,
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	// The branch is what the answer re-seeds from once the box is
	// reclaimed, so a parked run without one is not a realistic starting
	// point for anything the reaper does.
	asked := Question("which branch?", sandbox.AskToRequester)
	asked.Branch = "wip/" + turnID
	Ask(t, s, turnID, asked)
	if err := s.MarkAwaiting(ctx, turnID, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
}

// The gap between two writes is a state a reader can SEE: a run reading as
// `reseed` while it still names its box tells an arriving answer that the
// checkout is live, moments before the reaper destroys it. One statement, no
// window.
func testExpiringAPauseClearsTheBoxInTheSameWrite(t *testing.T, s sandbox.PendingStore) {
	mustLaunched(t, s, run("t1"))
	park(t, s, "t1")
	if err := s.MarkBoxPaused(t.Context(), "t1", base); err != nil {
		t.Fatalf("MarkBoxPaused: %v", err)
	}

	won, err := s.ExpirePause(t.Context(), "t1")
	if err != nil || !won {
		t.Fatalf("ExpirePause = %v, %v", won, err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusReseed {
		t.Fatalf("status = %q, want %q", got.Status, sandbox.StatusReseed)
	}
	if got.SandboxID != "" || got.CommandID != "" {
		t.Fatalf("the row still names a box that is about to be destroyed: %+v", got)
	}
	if !got.PausedAt.IsZero() {
		t.Fatal("the row still claims a paused box is held")
	}
	// The QUESTION survives: the run is not over, and the answer still has
	// to find it.
	if got.Question == "" || got.Branch == "" {
		t.Fatalf("the reseed lost what the answer needs: question=%q branch=%q",
			got.Question, got.Branch)
	}
}
