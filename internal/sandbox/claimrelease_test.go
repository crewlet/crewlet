package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// A COLLECTION'S HAND-BACK THE STORE DOES NOT ANSWER NEVER ENDS THE RUN: this
// node keeps the claim and makes the release again on the hand-back's spacing,
// and once the store answers the run is back in running, collected and resumed
// once. Every kind of collection's hand-back keeps it, a counted one counting
// its failure once when its release lands, a release that landed with its
// answer lost included. See the package doc, "Owed claim releases".
//
// Mutation: end a release the store did not answer (unclaimOwing), at the first
// attempt or at a retry; end any one branch of collectFailed's hand-back; or
// leave the series unsettled once the release lands, holding the seat's mail
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
// (claimCompletion), and the release asked again gives back the claim the
// resumed turn is working under.
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

// claimOnItsWay is [unansweredRelease] whose next claim of a job's completion
// stops before it reaches the store, as a claim already sent stops in a store
// slow to answer it, until the case lets it go on; and which runs a step of the
// case's around the next release it is asked for.
type claimOnItsWay struct {
	*unansweredRelease

	mu      sync.Mutex
	holding bool
	// held is closed once the held claim has stopped before the store, goOn
	// lets it go on, and claimed is closed once the store has answered it.
	// read is closed by the case once that claim, having won, has read the
	// job's box, which its collection does only after this node has taken the
	// claim's answer ([Coordinator.claimCompletion]); once claimed is closed,
	// the node may not have taken it yet.
	held, goOn, claimed, read chan struct{}
	// before and after, set, run once each around the next release: before it
	// reaches the store, and once it has landed or failed, before its caller
	// hears.
	before, after func()
}

func newClaimOnItsWay(rig *coordRig) *claimOnItsWay {
	return &claimOnItsWay{
		unansweredRelease: &unansweredRelease{silentStore: &silentStore{PendingStore: rig.pending}, landed: true},
		held:              make(chan struct{}), goOn: make(chan struct{}), claimed: make(chan struct{}),
		read: make(chan struct{}),
	}
}

// hold has the next claim stop before the store.
func (s *claimOnItsWay) hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holding = true
}

// around sets the steps run around the next release.
func (s *claimOnItsWay) around(before, after func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before, s.after = before, after
}

func (s *claimOnItsWay) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	s.mu.Lock()
	held := s.holding
	s.holding = false
	s.mu.Unlock()
	if !held {
		return s.unansweredRelease.ClaimForResume(ctx, turnID, tail, fence)
	}
	close(s.held)
	<-s.goOn
	defer close(s.claimed)
	return s.unansweredRelease.ClaimForResume(ctx, turnID, tail, fence)
}

func (s *claimOnItsWay) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	s.mu.Lock()
	before, after := s.before, s.after
	s.before, s.after = nil, nil
	s.mu.Unlock()
	if before != nil {
		before()
	}
	released, err := s.unansweredRelease.ReleaseClaim(ctx, turnID, release)
	if after != nil {
		after()
	}
	return released, err
}

// A CLAIM OF THE JOB ALREADY ON ITS WAY WHEN ITS RELEASE WENT OUT IS NEVER
// GIVEN BACK BY THAT RELEASE: its win proves the release landed, so the release
// is never made again, owed or out, nor while such a claim is on its way, and
// its move in the run's entry is made there. The cases make such a claim with a
// coordinator that has no gate, as a deadline's claim asks for none.
//
// Mutation: forget the win (keep the release recorded when a claim of its job
// wins on the run it put back), owe a release such a claim took over while it
// was out, make it again while a claim of the job is on its way, or leave its
// move in its run's entry out when a claim takes it over: each turns a case
// red.
func TestAClaimOnItsWayWhenTheReleaseWentOutIsNeverGivenBackByIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// before is run before the first collection, and after once it has
		// handed its claim back and the store answers again: between them they
		// let the claim on its way go on, at the point the case names. The
		// claim is let go at the end in any case.
		before func(s *claimOnItsWay, letGo func())
		after  func(rig *coordRig, s *claimOnItsWay, letGo func())
		// owed is whether the first collection's hand-back leaves its release
		// owed: not where the claim on its way won while it was out, since
		// that win proved it landed.
		owed bool
	}{
		{name: "it lands once the release is owed", owed: true,
			after: func(_ *coordRig, _ *claimOnItsWay, letGo func()) { letGo() }},
		{name: "it lands while the release is out",
			before: func(s *claimOnItsWay, letGo func()) {
				// TAKEN BEFORE THE RELEASE'S ANSWER IS: the claim's read of the
				// box, not the store's answer to it, is what says this node
				// has taken the claim's answer, so the release's lost answer
				// finds it taken over whichever goroutine runs first.
				s.around(nil, func() { s.answer(); letGo(); <-s.read })
			}},
		{name: "it lands while the owed release is made again", owed: true,
			after: func(rig *coordRig, s *claimOnItsWay, letGo func()) {
				s.around(func() { letGo(); <-s.claimed }, nil)
				rig.fireRetries()
				letGo()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			store := newClaimOnItsWay(rig)
			rig.coordinator.pending = store
			run := rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(Result{Success: true, Text: "the box's account"})
			var once sync.Once
			letGo := func() { once.Do(func() { close(store.goOn) }) }

			// THE CLAIM ON ITS WAY sets off during the first collection's read
			// and stops before the store; the store then goes silent for that
			// collection's drain and its release, which lands with its answer
			// lost.
			var (
				reads    atomic.Int32
				theirs   = make(chan error, 1)
				handed   = make(chan struct{})
				mu       sync.Mutex
				statuses []string
				readOnce sync.Once
			)
			rig.runner.CollectFunc = func(ctx context.Context) error {
				if reads.Add(1) != 1 {
					// THE CLAIM ON ITS WAY, having won, reads the box.
					readOnce.Do(func() { close(store.read) })
					return nil
				}
				store.hold()
				go func() { theirs <- collectAgainIn(context.WithoutCancel(ctx), t, rig) }()
				<-store.held
				store.silence("WatchLog", "ReleaseClaim")
				return nil
			}
			// ANY RETRY OWED fires while the claim's resumed turn works, which
			// is when a release made again would give that claim back.
			rig.resumer.during = func(ctx context.Context, _ PendingRun) {
				<-handed
				rig.fireRetries()
				mu.Lock()
				defer mu.Unlock()
				statuses = append(statuses, rig.getIn(ctx, "t1").Status)
			}
			if tc.before != nil {
				tc.before(store, letGo)
			}

			if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
				t.Fatalf("the collection the store went silent in = %v", err)
			}
			if owed := len(rig.retries.delays()) > 0; owed != tc.owed {
				t.Fatalf("the first collection's hand-back left its release owed: %v, want %v (retries "+
					"waiting %v)", owed, tc.owed, rig.retries.delays())
			}
			store.answer()
			if tc.after != nil {
				tc.after(rig, store, letGo)
			}
			letGo()
			close(handed)
			if err := <-theirs; err != nil {
				t.Fatalf("the claim on its way = %v; want it collected and the turn resumed", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(statuses, []string{StatusResumed}) {
				t.Fatalf("the resumed turn's run read %v while it worked; want its own claim throughout: the "+
					"release of the claim before it gave this one back", statuses)
			}
			if resumed := rig.resumer.calls(); len(resumed) != 1 {
				t.Fatalf("resumed %d times; want once", len(resumed))
			}
			if failed := rig.failures(); len(failed) != 0 {
				t.Fatalf("announced %+v", failed)
			}
			if rig.coordinator.releasing("t1", run.LaunchID) {
				t.Error("the release is still owed after a claim of its job won on the run it put back")
			}
			if held, waits := rig.coordinator.SeatRuns("swe"); held || waits {
				t.Errorf("once the run is over the seat is counted held %v, a question open %v; want it free",
					held, waits)
			}
		})
	}
}

// A RELEASE A CLAIM TOOK OVER BEFORE IT WAS OWED IS NOT OWED: found out with
// its answer lost and owed in one step, so a claim on its way cannot win
// between, which would leave the seat's mail held behind a release no longer
// out. The claim wins here where the node logs sandbox_claim_revert_owed.
//
// Mutation: record the owed release in a step of its own after the one that
// found it out; the series stays, and the seat's mail with it, after the run is
// over. ([TestOnlyAReleaseStillOutIsOwed] holds the step's own question.)
//
// Not parallel: it hooks the process's logger ([onLogLine]).
func TestAReleaseAClaimTookOverBeforeItWasOwedIsNotOwed(t *testing.T) {
	rig := newCoordRig(t)
	holds := rig.withHold()
	store := newClaimOnItsWay(rig)
	rig.coordinator.pending = store
	run := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "the box's account"})
	var once sync.Once
	letGo := func() { once.Do(func() { close(store.goOn) }) }
	defer letGo()

	// THE CLAIM ON ITS WAY sets off during the first collection's read and
	// stops before the store; the store then goes silent for that
	// collection's drain and its release, which lands with its answer lost.
	var reads atomic.Int32
	theirs := make(chan error, 1)
	rig.runner.CollectFunc = func(ctx context.Context) error {
		if reads.Add(1) != 1 {
			return nil
		}
		store.hold()
		go func() { theirs <- collectAgainIn(context.WithoutCancel(ctx), t, rig) }()
		<-store.held
		store.silence("WatchLog", "ReleaseClaim")
		return nil
	}
	// IT WINS ON THE RUN THE RELEASE PUT BACK once the node has found the
	// release out and before it is recorded as owed, and the node has taken
	// its answer, the release no longer out, before the line is written.
	onLogLine(t, slog.LevelWarn, "sandbox_claim_revert_owed", func() {
		store.answer()
		letGo()
		eventually(t, func() bool { return !rig.coordinator.releasing("t1", run.LaunchID) },
			"the claim on its way never took the release over")
	})

	if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
		t.Fatalf("the collection the store went silent in = %v", err)
	}
	if err := <-theirs; err != nil {
		t.Fatalf("the claim on its way = %v; want it collected and the turn resumed", err)
	}
	if resumed := rig.resumer.calls(); len(resumed) != 1 {
		t.Fatalf("the premise: resumed %d times; want the claim on its way resumed once", len(resumed))
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
		t.Fatalf("the premise: t1 found %v (%v); want its run over", found, err)
	}
	if waiting := rig.retries.delays(); len(waiting) != 0 {
		t.Errorf("retries waiting %v once the run is over; want none: the release a claim took over is owed",
			waiting)
	}
	rig.coordinator.mu.Lock()
	series, owed := rig.coordinator.retries["t1"], len(rig.coordinator.owed["swe"])
	rig.coordinator.mu.Unlock()
	if series != nil || owed != 0 {
		t.Errorf("t1's series %v and %d owed on the seat once the run is over; want neither", series != nil, owed)
	}
	if holds.holding("swe") {
		t.Error("the seat's mail is held once the run is over, behind a release no longer out")
	}
}

// ONLY A RELEASE STILL OUT IS OWED, asked in the step that keeps it
// ([Coordinator.oweRelease]): one a claim took over, or the seat's release
// dropped, is not kept. And only one it keeps is said kept:
// sandbox_claim_revert_owed is written after that step, never before.
//
// Mutation: keep the release without asking whether it is still out, or write
// the line before the step that asks.
//
// Not parallel: it reads the process's logger ([captureLogs]).
func TestOnlyAReleaseStillOutIsOwed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gone takes the recorded release out of the releases out, as the
		// case names.
		gone func(rig *coordRig, cr *claimRelease)
		out  bool
	}{
		{name: "still out", gone: func(*coordRig, *claimRelease) {}, out: true},
		{name: "taken over by a claim that won",
			gone: func(rig *coordRig, cr *claimRelease) {
				rig.coordinator.mu.Lock()
				defer rig.coordinator.mu.Unlock()
				delete(rig.coordinator.releases, cr.run.TurnID)
				_ = rig.coordinator.wroteCountLocked(cr.write, true)
			}},
		{name: "dropped with the seat",
			gone: func(rig *coordRig, _ *claimRelease) { rig.coordinator.releaseOwed("swe") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			rig := newCoordRig(t)
			holds := rig.withHold()
			run := rig.launch("t1")
			run.Status = StatusResumed
			rig.coordinator.mu.Lock()
			cr, _ := rig.coordinator.handingBackLocked(run, "", failure{}, nil)
			rig.coordinator.mu.Unlock()
			tc.gone(rig, cr)

			rig.coordinator.oweRelease(t.Context(), cr, errStoreUnavailable("write the sandbox run", "t1"))
			rig.coordinator.mu.Lock()
			series := rig.coordinator.retries["t1"]
			kept := series != nil && series.release == cr
			_, owed := rig.coordinator.owed["swe"]["t1"]
			rig.coordinator.mu.Unlock()
			waiting := len(rig.retries.delays()) > 0
			if kept != tc.out || owed != tc.out || waiting != tc.out || holds.holding("swe") != tc.out {
				t.Fatalf("kept %v, owed %v, a retry waiting %v, the seat's mail held %v; want each %v",
					kept, owed, waiting, holds.holding("swe"), tc.out)
			}
			said, want := 0, 0
			if tc.out {
				want = 1
			}
			for line := range strings.SplitSeq(logs.String(), "\n") {
				if strings.Contains(line, " msg=sandbox_claim_revert_owed ") &&
					strings.Contains(line, " launch_id="+run.LaunchID+" ") {
					said++
				}
			}
			if said != want {
				t.Fatalf("the node said %d times that it keeps the claim's release; want %d", said, want)
			}
		})
	}
}

// A SEAT RELEASED AS AN ANSWER IS LEFT TO ITS JOB'S RELEASE OWES THE ANSWER
// NOTHING ON THIS NODE ([Coordinator.leaveToRelease]): the series is recorded
// and the run owed in one step, so a seat's release cannot leave the run owed
// with nothing to drive it. The seat is released where the node logs that the
// answer is left.
//
// Mutation: owe the seat's run in a step after the one that records the series;
// t1 stays owed on the released seat with no series, and the seat's hold is
// taken again after its release.
// ([TestAnOwedRunIsHeldBehindOnlyASeriesRecordedAndArmed] holds the rest.)
//
// Not parallel: it hooks the process's logger ([onLogLine]).
func TestASeatReleasedAsAnAnswerIsLeftToItsReleaseOwesItNothing(t *testing.T) {
	rig := newCoordRig(t)
	holds := rig.withHold()
	run := rig.launch("t1")
	run.Status = StatusResumed
	rig.coordinator.mu.Lock()
	_, _ = rig.coordinator.handingBackLocked(run, "", failure{}, nil)
	rig.coordinator.mu.Unlock()
	if !rig.coordinator.releasing("t1", run.LaunchID) {
		t.Fatal("the premise: the release of t1's claim is not out")
	}

	released, holdsBefore := false, 0
	onLogLine(t, slog.LevelInfo, "sandbox_answer_release_owed", func() {
		rig.coordinator.ReleaseSeat("swe")
		released = true
		holds.mu.Lock()
		holdsBefore = len(holds.log)
		holds.mu.Unlock()
	})
	run.Status = StatusAnswered
	if !rig.coordinator.leaveToRelease(t.Context(), run) {
		t.Fatal("the premise: the answer was not left to the release its job owes")
	}
	if !released {
		t.Fatal("the premise: the node never logged that it left the answer to the release")
	}

	rig.coordinator.mu.Lock()
	series, owed := rig.coordinator.retries["t1"], len(rig.coordinator.owed["swe"])
	rig.coordinator.mu.Unlock()
	if series != nil || owed != 0 {
		t.Errorf("t1's series %v and %d owed on the seat once the seat is released; want neither",
			series != nil, owed)
	}
	if waiting := rig.retries.delays(); len(waiting) != 0 {
		t.Errorf("retries waiting %v once the seat is released; want none", waiting)
	}
	holds.mu.Lock()
	after := slices.Clone(holds.log[holdsBefore:])
	holds.mu.Unlock()
	if len(after) != 0 {
		t.Errorf("the seat's inbox was %v after the seat's release; want nothing done to it here", after)
	}
}

// A CLAIM AT THE TURN'S DEADLINE WAITS FOR AN OWED RELEASE TOO. A completion
// the node will not resume once its turn's deadline has passed is claimed with
// no admission asked ([Coordinator.completeAtDeadline]), and is a claim of the
// job like any other: while the release of the job's last claim is owed it is
// refused, or the release made again would give back the claim its ending is
// collecting under.
//
// Mutation: claim at the deadline without asking what this node owes, and the
// run the release put back is claimed while the release is owed.
func TestAClaimAtTheTurnsDeadlineWaitsForAnOwedRelease(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	store := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.pending}, landed: true}
	rig.coordinator.pending = store
	run := rig.launchUntil("t1", rig.now.Add(time.Hour))
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "the box's account"})
	var reads atomic.Int32
	rig.runner.CollectFunc = func(context.Context) error {
		if reads.Add(1) == 1 {
			store.silence("WatchLog", "ReleaseClaim")
		}
		return nil
	}
	if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
		t.Fatalf("the collection the store went silent in = %v", err)
	}
	store.answer()
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("the premise: the release that lost its answer left the run %s, want it back in running",
			got.Status)
	}

	gated(rig).refuse(true)
	rig.now = run.Deadline.Add(time.Minute)
	if err := collectAgain(t, rig); !errors.Is(err, errReleaseOwed) {
		t.Fatalf("a completion past its turn's deadline while the release is owed = %v; want it handed back", err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("the run is %s; want it left in running while the release is owed", got.Status)
	}
	if failed, resumed := rig.failures(), rig.resumer.calls(); len(failed) != 0 || len(resumed) != 0 {
		t.Fatalf("the run was ended (%+v) or resumed (%d) while its release was owed", failed, len(resumed))
	}
}

// AN OWED RELEASE OUTLIVES ITS SERIES' SETTLING. The series that owes a
// collection's release is the turn's one series, and another of its duties (a
// hand-back an earlier answer's decline still owes the seat, which settles the
// series once the run is no longer answered) can settle it while the release
// is owed. Settled then, the release went with it and was never made again: a
// claim left where nothing picks it up, with nothing driving it, as an ending
// or a suspected claim would be.
//
// Mutation: settle a series whose release is owed, and the release is never
// made again.
func TestAnOwedReleaseOutlivesItsSeriesSettling(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	store := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.pending}}
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

	rig.coordinator.settleOwed(t.Context(), "swe", "t1")
	if fired := rig.fireRetries(); fired != 1 {
		t.Fatalf("%d retries fired after the series was settled; want the owed release made", fired)
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("the run is %s; want the owed release made once the store answered", got.Status)
	}
}
