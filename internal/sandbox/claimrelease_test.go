package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// errStoreUnavailable is what a write the fleet's coordination store did not
// answer returns, as the store and CoordStore above it wrap it: marked
// [coord.ErrUnavailable], since it may have landed.
func errStoreUnavailable(what, turnID string) error {
	return fmt.Errorf("sandbox: update run %s: %w: %s: %w", turnID, coord.ErrUnavailable, what, errStoreUnanswered)
}

// unansweredRelease is [silentStore] whose coordination store does not answer
// a claim's release either while "ReleaseClaim" is named silent, nor an
// ending's decision while "DecideEnding" is. landed has the release land before
// its answer is lost, as a write sent and unanswered may; answerAfter has the
// store answer every call again the moment the release has failed; refused
// has the store REFUSE the release instead, a definite answer no retry
// changes.
type unansweredRelease struct {
	*silentStore
	landed, answerAfter, refused bool

	mu       sync.Mutex
	releases int
}

func (s *unansweredRelease) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	s.mu.Lock()
	s.releases++
	s.mu.Unlock()
	if !s.isSilent("ReleaseClaim") {
		return s.silentStore.ReleaseClaim(ctx, turnID, release)
	}
	err := errStoreUnavailable("write the sandbox run", turnID)
	switch {
	case s.refused:
		err = fmt.Errorf("sandbox: release run %s: %w", turnID, errRefusedCall)
	case s.landed:
		if _, landErr := s.silentStore.ReleaseClaim(ctx, turnID, release); landErr != nil {
			return false, landErr
		}
	}
	if s.answerAfter {
		s.answer()
	}
	return false, err
}

func (s *unansweredRelease) DecideEnding(ctx context.Context, turnID string, d Decision) (PendingRun, bool, error) {
	if s.isSilent("DecideEnding") {
		return PendingRun{}, false, errStoreUnavailable("decide the end of the sandbox run", turnID)
	}
	return s.silentStore.DecideEnding(ctx, turnID, d)
}

// released is how many releases the store was asked for.
func (s *unansweredRelease) released() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releases
}

// A COLLECTION'S HAND-BACK THE STORE DOES NOT ANSWER NEVER ENDS THE RUN. A
// collection that could not finish hands its claim back, and the release that
// does it is one more write to a coordination store that may be the very one
// that went silent. It used to be decided as a hand-back that cannot be
// written: the claim's ending, decided at once or kept and decided the moment
// the store answered, and the run announced claim_unreverted, "its result is
// lost", while its box held the result. Now this node keeps the claim, makes
// the release again on the hand-back's spacing while the store is silent, and
// the run is back in running for its completion's retry once the store answers:
// collected then and resumed once, on what its box held. Every kind of
// collection's hand-back keeps it, the counted one counting its failure once
// when its release lands, a release that landed with its answer lost included.
//
// Mutation: end a release the store did not answer (unclaimOwing), at the first
// attempt or at a retry; hand back any one branch of collectFailed the old way;
// or leave the series unsettled once the release lands, holding the seat's mail
// after its run ended: each turns a case red.
func TestAHandBackTheStoreDoesNotAnswerIsKeptUntilItLands(t *testing.T) {
	t.Parallel()
	account := Result{Success: true, Text: "the box's account", InputTokens: 900, OutputTokens: 100}
	for _, tc := range []struct {
		name string
		// arrange launches t1 and makes its collection fail with the store
		// silent for the claim's release, and returns the context the
		// collection runs under and what heals the box for the collection
		// made once the store answers.
		arrange             func(t *testing.T, rig *coordRig, store *unansweredRelease) (context.Context, func())
		landed, answerAfter bool
		// failures is the job's failed collections once the release lands.
		failures int
	}{
		{name: "a store silent from the box's read on, its release and the ending's decision with it",
			arrange: silentFromTheRead("WatchLog", "ReleaseClaim", "DecideEnding", "RetireSessions")},
		{name: "a store silent for the drain and the release, answering the moment the release failed",
			arrange: silentFromTheRead("WatchLog", "ReleaseClaim"), answerAfter: true},
		{name: "a release that landed with its answer lost",
			arrange: silentFromTheRead("WatchLog", "ReleaseClaim"), landed: true},
		{name: "a box that could not be read, counted", arrange: unreadBox, failures: 1},
		{name: "a box that could not be read, counted, its release landed with its answer lost",
			arrange: unreadBox, landed: true, failures: 1},
		{name: "a collection this node stopped", arrange: stoppedHere},
		{name: "a host that did not answer", arrange: silentHost(account)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			holds := rig.withHold()
			store := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.pending},
				landed: tc.landed, answerAfter: tc.answerAfter}
			rig.coordinator.pending = store
			ctx, heal := tc.arrange(t, rig, store)
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(account)

			payload, ev := rig.completion("t1")
			_ = rig.coordinator.OnCompleted(ctx, payload, ev) // each branch's own answer to its delivery
			heldOwed(t, rig, tc.landed, "the collection's hand-back")

			if !tc.answerAfter {
				// STILL SILENT AT THE RETRY: owed again, never ended.
				if fired := rig.fireRetries(); fired != 1 {
					t.Fatalf("%d retries fired; want the release made again once", fired)
				}
				heldOwed(t, rig, tc.landed, "the release's retry while the store is still silent")
				store.answer()
			}
			if fired := rig.fireRetries(); fired != 1 {
				t.Fatalf("%d retries fired once the store answered; want the owed release made", fired)
			}
			got := rig.get("t1")
			if got.Status != StatusRunning || got.Launch.CollectFailures != tc.failures {
				t.Fatalf("once the store answered the run is %s with %d failed collections; want it back in "+
					"running with %d", got.Status, got.Launch.CollectFailures, tc.failures)
			}
			if failed := rig.failures(); len(failed) != 0 {
				t.Fatalf("announced %+v for a run whose hand-back the store only did not answer", failed)
			}

			heal()
			if err := collectAgain(t, rig); err != nil {
				t.Fatalf("the collection once the release landed: %v", err)
			}
			resumed := rig.resumer.calls()
			if len(resumed) != 1 || resumed[0].Ended.Text != account.Text {
				t.Fatalf("the turn resumed on %+v; want it resumed once on its box's account", resumed)
			}
			if failed := rig.failures(); len(failed) != 0 {
				t.Fatalf("announced %+v", failed)
			}
			// AND NOTHING IS OWED ONCE THE RUN IS OVER: the seat's mail goes.
			rig.finished("t1")
			if holds.holding("swe") {
				t.Fatalf("the seat's inbox is held after its run ended: %v", holds.log)
			}
		})
	}
}

// heldOwed holds t1 to a claim this node keeps owing its release, after step:
// nothing ended or announced, the claim still the row's (or the run back in
// running, where the release landed though its answer was lost), the release
// asked again on the hand-back's spacing, and a completion of the job refused
// rather than claimed.
func heldOwed(t *testing.T, rig *coordRig, landed bool, step string) {
	t.Helper()
	if failed := rig.failures(); len(failed) != 0 {
		t.Fatalf("after %s the run was ended: %+v", step, failed)
	}
	want := StatusResumed
	if landed {
		want = StatusRunning
	}
	if got := rig.get("t1"); got.Status != want {
		t.Fatalf("after %s the run is %s; want %s", step, got.Status, want)
	}
	if delays := rig.retries.delays(); !slices.Equal(delays, []time.Duration{answerRetryCeiling}) {
		t.Fatalf("after %s the retries waiting are %v; want the release made again on the hand-back's "+
			"spacing, %v", step, delays, answerRetryCeiling)
	}
	before := rig.get("t1")
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); !errors.Is(err, errReleaseOwed) {
		t.Fatalf("a completion of the job after %s = %v; want it handed back while the release is owed", step, err)
	}
	if got := rig.get("t1"); got.Status != before.Status || len(rig.resumer.calls()) != 0 {
		t.Fatalf("a completion of the job after %s claimed it (%s) or resumed it (%d)", step, got.Status,
			len(rig.resumer.calls()))
	}
}

// silentFromTheRead launches t1 and silences calls the moment its box is read.
func silentFromTheRead(calls ...string) func(*testing.T, *coordRig, *unansweredRelease) (context.Context, func()) {
	return func(t *testing.T, rig *coordRig, store *unansweredRelease) (context.Context, func()) {
		rig.launch("t1")
		rig.runner.CollectFunc = func(context.Context) error {
			store.silence(calls...)
			return nil
		}
		return t.Context(), func() { rig.runner.CollectFunc = nil }
	}
}

// unreadBox launches t1 whose box cannot be read once, with the store silent
// for the release that counts it.
func unreadBox(t *testing.T, rig *coordRig, store *unansweredRelease) (context.Context, func()) {
	rig.launch("t1")
	rig.runner.CollectErr = errBoxBlip
	store.silence("ReleaseClaim")
	return t.Context(), func() { rig.runner.CollectErr = nil }
}

// stoppedHere launches t1 whose collection this node stops mid-read, with the
// store silent for the release.
func stoppedHere(t *testing.T, rig *coordRig, store *unansweredRelease) (context.Context, func()) {
	rig.launch("t1")
	ctx, cancel := context.WithCancel(t.Context())
	rig.runner.CollectFunc = func(ctx context.Context) error {
		store.silence("ReleaseClaim")
		cancel()
		return ctx.Err()
	}
	return ctx, func() { rig.runner.CollectFunc = nil }
}

// silentHost launches t1 on node-a's host, which does not answer this node's
// read, with the store silent for the release; healed, it reads account.
func silentHost(account Result) func(*testing.T, *coordRig, *unansweredRelease) (context.Context, func()) {
	return func(t *testing.T, rig *coordRig, store *unansweredRelease) (context.Context, func()) {
		asks := &boxAsks{err: fmt.Errorf("%w: node-a, within a minute", ErrBoxHostSilent)}
		rig.onNode(t, "node-b", asks, nil)
		rig.launchedBy("t1", "node-a")
		store.silence("ReleaseClaim")
		return t.Context(), func() {
			asks.mu.Lock()
			defer asks.mu.Unlock()
			asks.err, asks.result = nil, account
		}
	}
}

// A RELEASE ASKED AGAIN NEVER GIVES BACK A CLAIM IT DID NOT TAKE. A release
// the store did not answer may have landed: the run is back in running, and
// the claim the release named (its job, its status, its lease) is the very
// claim this node's next collection of the job takes. So nothing of the job is
// claimed here while the release is owed: a completion is handed back for a
// retry, the release made again finds the run moved on, and only then is the
// job claimed, collected and resumed, its claim its own through the resume.
//
// Mutation: drop the refusal of a completion whose release is owed
// (OnCompleted), and the release asked again gives back the claim the resumed
// turn is working under.
func TestAReleaseAskedAgainNeverGivesBackAClaimItDidNotTake(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	store := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.pending}, landed: true}
	rig.coordinator.pending = store
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "the box's account"})
	rig.runner.CollectFunc = func(context.Context) error {
		store.silence("WatchLog", "ReleaseClaim")
		return nil
	}
	if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
		t.Fatalf("the collection the store went silent in = %v", err)
	}
	rig.runner.CollectFunc = nil
	store.answer()
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("the premise: the release that lost its answer left the run %s, want it back in running",
			got.Status)
	}

	// THE OWED RELEASE COMES DUE WHILE A RESUMED TURN WORKS, which is when a
	// claim taken since would be given back under it.
	var (
		mu       sync.Mutex
		statuses []string
	)
	rig.resumer.during = func(ctx context.Context, _ PendingRun) {
		rig.fireRetries()
		mu.Lock()
		defer mu.Unlock()
		statuses = append(statuses, rig.getIn(ctx, "t1").Status)
	}
	for attempt := 1; ; attempt++ {
		err := collectAgain(t, rig)
		if err == nil {
			break
		}
		if !errors.Is(err, errReleaseOwed) || attempt > 1 {
			t.Fatalf("collection %d = %v; want it handed back once while the release is owed", attempt, err)
		}
		if fired := rig.fireRetries(); fired != 1 {
			t.Fatalf("%d retries fired; want the owed release made", fired)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(statuses, []string{StatusResumed}) {
		t.Fatalf("the resumed turn's run read %v while it worked; want its own claim throughout: a release "+
			"owed by an earlier claim gave back the claim this turn holds", statuses)
	}
	if resumed := rig.resumer.calls(); len(resumed) != 1 {
		t.Fatalf("resumed %d times; want once", len(resumed))
	}
	if n := store.released(); n != 2 {
		t.Fatalf("the store was asked for %d releases; want the collection's and its one retry", n)
	}
}

// A RELEASE THE STORE REFUSED STILL ENDS THE RUN. A refusal is a definite
// answer no retry changes, a row this build cannot decode or encode, a value
// past its key's bound, and a claim it leaves is picked up by nothing: the run
// is ended while it is still the claim, and announced claim_unreverted, as it
// always was. Only a release the store did not answer is kept.
//
// Mutation: keep every release that failed owed, and the refused one is
// retried for ever with the seat's mail held behind it.
func TestAReleaseTheStoreRefusedStillEndsTheRun(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	store := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.pending}, refused: true,
		answerAfter: true}
	rig.coordinator.pending = store
	run := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "the box's account"})
	rig.runner.CollectFunc = func(context.Context) error {
		store.silence("WatchLog", "ReleaseClaim")
		return nil
	}

	_ = collectAgain(t, rig)
	rig.finished("t1")
	failed := rig.failures()
	if len(failed) != 1 || failed[0].Reason != types.ExecutorFailureClaimStranded {
		t.Fatalf("announced %+v; want one %q", failed, types.ExecutorFailureClaimStranded)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != run.SandboxID {
		t.Errorf("killed %v; want the box of the run nothing can pick up reclaimed", killed)
	}
	if rig.coordinator.releasing("t1", run.LaunchID) {
		t.Error("a release the store refused is still owed")
	}
}
