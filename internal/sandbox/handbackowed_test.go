package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// MarkAwaiting is the store's record of a park's question, not answered while
// "MarkAwaiting" is named silent: marked [coord.ErrUnavailable], as a write the
// coordination store did not answer is.
func (s *unansweredRelease) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	if s.isSilent("MarkAwaiting") {
		return errStoreUnavailable("write the sandbox run", turnID)
	}
	return s.silentStore.MarkAwaiting(ctx, turnID, q)
}

// questionRefused is the rig's queue refusing the announcement of a job's
// question, as a broker that did not take it refuses it, until it is healed,
// and passing every other publish on.
type questionRefused struct {
	Publisher

	mu      sync.Mutex
	refused bool
}

func (q *questionRefused) Publish(ctx context.Context, topic string, ev *events.Event) error {
	q.mu.Lock()
	refused := q.refused
	q.mu.Unlock()
	if _, asked := ev.Data.(*types.ExecutorAsked); asked && refused {
		return errors.New("the broker did not take the question's announcement")
	}
	return q.Publisher.Publish(ctx, topic, ev)
}

// heal has the queue take the question's announcement again.
func (q *questionRefused) heal() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.refused = false
}

// EVERY HAND-BACK THE STORE DOES NOT ANSWER IS KEPT UNTIL IT LANDS, a park's
// and a resume's as much as a collection's: this node keeps the claim, makes
// the release again on the hand-back's spacing, claims nothing of the job
// meanwhile and keeps the seat counted held by the claim; once the store
// answers, the run is back in running, counted once, and its next collection
// parks or resumes it.
//
// Mutation: end a park's or a resume's release the store did not answer; give a
// resume's claim's count back before its release, and the seat reads free while
// the release is owed; or count the run where its release lands instead of
// moving the claim's count there, and it is counted twice.
func TestEveryHandBackTheStoreDoesNotAnswerIsKeptUntilItLands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// arrange has t1's first collection hand its claim back, and returns
		// what lets its next collection go through once the store answers.
		arrange func(rig *coordRig, store *unansweredRelease) func()
		landed  bool
		// asks is whether the job asked a question, which its next collection
		// parks it on; held is the hold its hand-back writes onto the row, ""
		// for none, which its resume waits for.
		asks bool
		held Condition
	}{
		{name: "a park whose question could not be announced", arrange: unannouncedPark, asks: true},
		{name: "a park whose question could not be recorded", arrange: unrecordedPark, asks: true},
		{name: "a resume that failed", arrange: failedResume},
		{name: "a resume that failed, its release landed with its answer lost", arrange: failedResume,
			landed: true},
		{name: "a resume its seat's budget held", arrange: heldResume, held: "budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			holds := rig.withHold()
			store := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.pending}, landed: tc.landed}
			rig.coordinator.pending = store
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			heal := tc.arrange(rig, store)
			store.silence("ReleaseClaim")

			_ = collectAgain(t, rig) // each hand-back's own answer to its delivery
			if tc.held != "" {
				// THE COMPLETION IS KEPT FOR THE HOLD'S INSTANT besides, its
				// re-fire an hour on, which is another case's subject
				// (TestACompletionTheBudgetHeldOnItsRowIsKeptForTheHold): set
				// aside here, where the release is.
				if delays := rig.retries.delays(); !slices.Contains(delays, time.Hour) {
					t.Fatalf("after the held hand-back the retries waiting are %v; want the completion's "+
						"re-fire at the hold's instant among them", delays)
				}
				rig.coordinator.forgetRefused("t1")
			}
			seatHeldByItsClaim(t, rig, holds, tc.landed, "the hand-back")

			// STILL SILENT AT THE RETRY: owed again, never ended.
			if fired := rig.fireRetries(); fired != 1 {
				t.Fatalf("%d retries fired; want the release made again once", fired)
			}
			seatHeldByItsClaim(t, rig, holds, tc.landed, "the release's retry while the store is still silent")
			store.answer()
			if fired := rig.fireRetries(); fired != 1 {
				t.Fatalf("%d retries fired once the store answered; want the owed release made", fired)
			}
			got := rig.get("t1")
			if got.Status != StatusRunning || got.HeldFor != tc.held {
				t.Fatalf("once the store answered the run is %s held for %q; want it back in running held "+
					"for %q", got.Status, got.HeldFor, tc.held)
			}
			if failed := rig.failures(); len(failed) != 0 {
				t.Fatalf("announced %+v for a run whose hand-back the store only did not answer", failed)
			}
			// COUNTED ONCE: the claim's count moved to running with the release.
			rig.coordinator.mu.Lock()
			counted := rig.coordinator.runs["swe"]
			rig.coordinator.mu.Unlock()
			if counted.holding != 1 || !holds.holding("swe") {
				t.Fatalf("once the release landed the seat counts %d holding (inbox held %v); want the run "+
					"counted once, back in running", counted.holding, holds.holding("swe"))
			}
			if tc.held != "" {
				// ITS RESUME WAITS FOR THE HOLD the release carried onto the
				// row, which the poll announces again once it lifts.
				return
			}

			heal()
			if err := collectAgain(t, rig); err != nil {
				t.Fatalf("the collection once the release landed: %v", err)
			}
			if failed := rig.failures(); len(failed) != 0 {
				t.Fatalf("announced %+v", failed)
			}
			if tc.asks {
				if got := rig.get("t1"); got.Status != StatusAwaiting || len(rig.questions()) == 0 {
					t.Fatalf("the job's next collection left it %s with %d questions announced; want it "+
						"parked on its question", got.Status, len(rig.questions()))
				}
				return
			}
			if resumed := rig.resumer.calls(); len(resumed) != 1 {
				t.Fatalf("the turn resumed %d times; want once, on the collection after the release landed",
					len(resumed))
			}
			rig.finished("t1")
			if holds.holding("swe") {
				t.Fatalf("the seat's inbox is held after its run ended: %v", holds.log)
			}
		})
	}
}

// seatHeldByItsClaim holds t1 after step to a claim this node keeps owing its
// release, as [heldOwed] does, and its seat to the claim the run still is:
// counted held, and its inbox held.
func seatHeldByItsClaim(t *testing.T, rig *coordRig, holds *holdSpy, landed bool, step string) {
	t.Helper()
	heldOwed(t, rig, landed, step)
	if held, _ := rig.coordinator.SeatRuns("swe"); !held || !holds.holding("swe") {
		t.Fatalf("after %s the seat counts held %v (inbox held %v); want it held by the claim whose "+
			"release is owed", step, held, holds.holding("swe"))
	}
}

// unannouncedPark has t1's job ask a question whose announcement the broker
// does not take until it is healed.
func unannouncedPark(rig *coordRig, _ *unansweredRelease) func() {
	rig.ask("t1", whichBranch())
	rig.runner.Finish(Result{})
	queue := &questionRefused{Publisher: rig.coordinator.queue, refused: true}
	rig.coordinator.queue = queue
	return queue.heal
}

// unrecordedPark has t1's job ask a question the store does not record while
// it is silent.
func unrecordedPark(rig *coordRig, store *unansweredRelease) func() {
	rig.ask("t1", whichBranch())
	rig.runner.Finish(Result{})
	store.silence("MarkAwaiting")
	return func() {}
}

// failedResume has t1's job finish and its turn's resume fail until it is
// healed.
func failedResume(rig *coordRig, _ *unansweredRelease) func() {
	rig.runner.Finish(Result{Success: true, Text: "the box's account"})
	rig.resumer.failWith(errors.New("the model provider did not answer"))
	return func() { rig.resumer.failWith(nil) }
}

// heldResume has t1's job finish and its turn's resume held by its seat's
// budget for an hour.
func heldResume(rig *coordRig, _ *unansweredRelease) func() {
	rig.runner.Finish(Result{Success: true, Text: "the box's account"})
	rig.resumer.failWith(budgetHeld(rig.now.Add(time.Hour)))
	return func() {}
}

// AN ANSWER'S HAND-BACK THE STORE DOES NOT ANSWER IS MADE AGAIN, AND THE ANSWER
// THEN RESUMED: the series that owes the answer owes the release too, makes it
// first within the hand-back's spacing whatever the answer waits for, attempts
// nothing at the answer while it is out, then resumes the run with it once.
//
// Mutation: settle the series once its release lands, and the answer is never
// resumed; count a read the store did not answer after the release landed as a
// failed attempt, and the next comes on the answer's backoff; or arm a series
// owing a release on its answer's budget wait, and the release waits for the
// window. Each turns a case red.
func TestAnAnswersHandBackTheStoreDoesNotAnswerIsMadeAgainThenResumed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  func(now time.Time) error
		// wait is the next attempt's delay once the hand-back is owed: the
		// answer's backoff after its failure, or the hand-back's spacing where
		// the answer would wait longer for its budget's window.
		wait time.Duration
	}{
		{name: "a resume that failed", wait: time.Second,
			err: func(time.Time) error { return errors.New("the model provider did not answer") }},
		{name: "a resume its seat's budget held for an hour", wait: answerRetryCeiling,
			err: func(now time.Time) error { return budgetHeld(now.Add(time.Hour)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			holds := rig.withHold()
			parkOnAQuestion(t, rig)
			store := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.pending}}
			rig.coordinator.pending = store
			var (
				mu        sync.Mutex
				reentries int
			)
			rig.resumer.during = func(context.Context, PendingRun) {
				mu.Lock()
				defer mu.Unlock()
				reentries++
			}
			reentered := func() int {
				mu.Lock()
				defer mu.Unlock()
				return reentries
			}
			rig.resumer.failWith(tc.err(rig.now))
			store.silence("ReleaseClaim")

			asked := rig.get("t1").AskedAt
			reply := chatReply(answerOnTheDM, "use main", replyAt("use main", asked.Add(time.Minute)))
			if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil ||
				d != AnswerConsumed {
				t.Fatalf("the reply = %q, %v; want it recorded as the answer", d, err)
			}
			kept := func(step string) {
				t.Helper()
				if failed := rig.failures(); len(failed) != 0 {
					t.Fatalf("after %s the run was ended: %+v", step, failed)
				}
				if got := rig.get("t1"); got.Status != StatusResumed || got.Answer == nil {
					t.Fatalf("after %s the run is %s with answer %v; want the claim kept, with the answer "+
						"on it", step, got.Status, got.Answer)
				}
				if !holds.holding("swe") {
					t.Fatalf("after %s the seat's inbox is not held behind the owed answer", step)
				}
			}
			kept("the answer's hand-back")
			if delays := rig.retries.delays(); !slices.Equal(delays, []time.Duration{tc.wait}) {
				t.Fatalf("after the answer's hand-back the retries waiting are %v; want %v", delays, tc.wait)
			}

			// STILL SILENT: the release is made again, and nothing else.
			if fired := rig.fireRetries(); fired != 1 {
				t.Fatalf("%d retries fired; want the owed release made again", fired)
			}
			kept("the release's retry while the store is still silent")
			if n, again := store.released(), reentered(); n != 2 || again != 1 {
				t.Fatalf("the store was asked for %d releases and the turn re-entered %d times; want the "+
					"release made again and no attempt at the answer while it is owed", n, again)
			}
			if delays := rig.retries.delays(); !slices.Equal(delays, []time.Duration{answerRetryCeiling}) {
				t.Fatalf("the retries waiting are %v; want the release made again on the hand-back's "+
					"spacing", delays)
			}

			// THE RELEASE LANDS, AND THE ROW IS NOT READ AFTER IT: read again on
			// the hand-back's spacing, with none of the answer's attempts spent.
			store.answer()
			store.silence("Get")
			rig.resumer.failWith(nil)
			if fired := rig.fireRetries(); fired != 1 {
				t.Fatalf("%d retries fired once the store answered; want the owed release made", fired)
			}
			if got := rig.get("t1"); got.Status != StatusAnswered {
				t.Fatalf("once the release landed the run is %s; want it answered again", got.Status)
			}
			if delays := rig.retries.delays(); !slices.Equal(delays, []time.Duration{answerRetryCeiling}) {
				t.Fatalf("a read after the landed release that the store did not answer left the retries "+
					"%v; want the row read again on the hand-back's spacing, no attempt counted", delays)
			}
			store.answer()
			if fired := rig.fireRetries(); fired != 1 {
				t.Fatalf("%d retries fired once the row could be read; want the answer's attempt", fired)
			}
			calls := rig.resumer.calls()
			if len(calls) != 1 || !strings.Contains(askedOf(calls[0]).Answer, "use main") {
				t.Fatalf("the run was resumed %d times (%+v); want once, with its answer, once the release "+
					"landed", len(calls), calls)
			}
			if failed := rig.failures(); len(failed) != 0 {
				t.Fatalf("announced %+v", failed)
			}
			if holds.holding("swe") {
				t.Fatalf("the seat's inbox is still held after the answer's resume returned: %v", holds.log)
			}
		})
	}
}

// recordedPark is [unansweredRelease] whose coordination store records a park's
// question and then loses its answer while "MarkAwaiting" is named silent, as a
// write sent and unanswered may: the run waits on its question, though the park
// was told the store did not answer. With a gate, a claim's release waits there
// until the gate is opened, reached saying it has arrived: a release still on
// its way to the store.
type recordedPark struct {
	*unansweredRelease
	gate, reached chan struct{}
}

func (s *recordedPark) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	if !s.isSilent("MarkAwaiting") {
		return s.unansweredRelease.MarkAwaiting(ctx, turnID, q)
	}
	if err := s.silentStore.PendingStore.MarkAwaiting(ctx, turnID, q); err != nil {
		return err
	}
	return errStoreUnavailable("write the sandbox run", turnID)
}

func (s *recordedPark) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	if s.gate != nil {
		select {
		case s.reached <- struct{}{}:
		default:
		}
		<-s.gate
	}
	return s.unansweredRelease.ReleaseClaim(ctx, turnID, release)
}

// AN ANSWER IS NEVER CLAIMED OVER A RELEASE OF ITS JOB STILL OUT: a park's
// release names the same job, status and lease as the answer's claim, so it
// would give that claim back to running, the answer with it. The reply's
// attempt leaves the answer to the series owing the release, which makes it
// first: owed, the series is armed already; on its way, the refused attempt
// arms it to read the row once the release lands.
//
// Mutation: let an answer's attempt claim while a release of its job is out,
// and the release gives the answer's claim back to running under its turn; or
// arm nothing where the release was still on its way, and the answer is never
// resumed.
func TestAnAnswerIsNeverClaimedOverAReleaseOfItsJobStillOut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// onItsWay holds the park's release at the store's gate, out and not
		// yet owed, rather than leaving it unanswered there.
		onItsWay bool
	}{
		{name: "a release the store did not answer, owed"},
		{name: "a release still on its way to the store", onItsWay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			holds := rig.withHold()
			store := &recordedPark{unansweredRelease: &unansweredRelease{
				silentStore: &silentStore{PendingStore: rig.pending}}}
			rig.coordinator.pending = store
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.ask("t1", whichBranch())
			rig.runner.Finish(Result{})
			store.silence("MarkAwaiting")

			// THE RELEASE IS MADE as its owner makes it while the answer's turn
			// runs: the owed one again by the series' own clock, the one on its
			// way reaching the store.
			release := func() { rig.fireRetries() }
			var during []string
			rig.resumer.during = func(ctx context.Context, _ PendingRun) {
				release()
				during = append(during, rig.getIn(ctx, "t1").Status)
			}
			if tc.onItsWay {
				store.gate, store.reached = make(chan struct{}), make(chan struct{}, 1)
				payload, ev := rig.completion("t1")
				parked := make(chan struct{})
				go func() {
					defer close(parked)
					_ = rig.coordinator.OnCompleted(t.Context(), payload, ev) // the park's own answer
				}()
				<-store.reached
				release = sync.OnceFunc(func() {
					close(store.gate)
					<-parked
				})
			} else {
				store.silence("ReleaseClaim")
				_ = collectAgain(t, rig) // the park's own answer to its delivery
			}
			run := rig.get("t1")
			if out := rig.coordinator.releasing("t1", run.LaunchID); run.Status != StatusAwaiting || !out {
				t.Fatalf("the premise: the park left the run %s, its release out %v; want its question "+
					"recorded and its release out", run.Status, out)
			}

			store.answer()
			reply := chatReply(answerOnTheDM, "use main", replyAt("use main", run.AskedAt.Add(time.Minute)))
			if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil ||
				d != AnswerConsumed {
				t.Fatalf("the reply = %q, %v; want it recorded as the answer", d, err)
			}
			if len(during) != 0 {
				t.Fatalf("the reply's own attempt claimed the answer while the park's release was out, and "+
					"the release left the run %v under the answer's turn", during)
			}
			if got := rig.get("t1"); got.Status != StatusAnswered {
				t.Fatalf("the reply left the run %s; want it answered, its resume left to the series that "+
					"owes the release", got.Status)
			}

			if tc.onItsWay {
				// THE RELEASE REACHES THE STORE, which finds the run answered
				// and gives nothing back.
				release()
			}
			if fired := rig.fireRetries(); fired != 1 {
				t.Fatalf("%d retries fired; want the series' one attempt, the release made first", fired)
			}
			if !slices.Equal(during, []string{StatusResumed}) {
				t.Fatalf("while the answer's turn ran the run was %v; want it the answer's claim", during)
			}
			calls := rig.resumer.calls()
			if len(calls) != 1 || !strings.Contains(askedOf(calls[0]).Answer, "use main") {
				t.Fatalf("the run was resumed %d times (%+v); want once, with its answer", len(calls), calls)
			}
			if failed, asked := rig.failures(), rig.questions(); len(failed) != 0 || len(asked) != 1 {
				t.Fatalf("announced %+v and asked %d times; want nothing ended and the question asked once",
					failed, len(asked))
			}
			if holds.holding("swe") {
				t.Fatalf("the seat's inbox is held after the answer's turn ended: %v", holds.log)
			}
		})
	}
}

// A RELEASE THAT PUT ITS RUN BACK IN RUNNING COUNTS NO READ AFTER IT: such a
// run owes the series no answer, so each unanswered read is read again on the
// hand-back's spacing, uncounted, or an answer it never had would be given up
// as unreadable. An answer's release keeps its answer's bound: its second
// unanswered read counts as an answer series' does.
//
// Mutation: count the reads after the first wherever the release put the run,
// and the completion's next read comes on the answer's backoff; or count none
// wherever it put it, and the answer's comes on the hand-back's spacing.
func TestAReleaseThatPutItsRunBackInRunningCountsNoReadAfterIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// handBack hands t1's claim back with its release owed.
		handBack func(t *testing.T, rig *coordRig)
		// waits is the retry waiting after each read the store leaves
		// unanswered once the release has landed, one per read.
		waits []time.Duration
	}{
		{name: "a completion's resume that failed", handBack: func(t *testing.T, rig *coordRig) {
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(Result{Success: true, Text: "the box's account"})
			rig.resumer.failWith(errors.New("the model provider did not answer"))
			_ = collectAgain(t, rig) // the resume's own answer to its delivery
		}, waits: slices.Repeat([]time.Duration{answerRetryCeiling}, MaxAnswerAttempts+1)},
		{name: "an answer's resume that failed", handBack: func(t *testing.T, rig *coordRig) {
			parkOnAQuestion(t, rig)
			rig.resumer.failWith(errors.New("the model provider did not answer"))
			reply := chatReply(answerOnTheDM, "use main",
				replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute)))
			if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil ||
				d != AnswerConsumed {
				t.Fatalf("the reply = %q, %v; want it recorded as the answer", d, err)
			}
		}, waits: []time.Duration{answerRetryCeiling, answerRetryDelay(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.withHold()
			store := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.pending,
				silent: []string{"ReleaseClaim"}}}
			rig.coordinator.pending = store
			tc.handBack(t, rig)
			if got := rig.get("t1"); got.Status != StatusResumed {
				t.Fatalf("the premise: the hand-back left the run %s; want the claim kept, its release owed",
					got.Status)
			}

			// THE STORE TAKES THE RELEASE, and answers no read after it.
			store.answer()
			store.silence("Get")
			for read, want := range tc.waits {
				if fired := rig.fireRetries(); fired != 1 {
					t.Fatalf("read %d: %d retries fired; want the series' one", read+1, fired)
				}
				if delays := rig.retries.delays(); !slices.Equal(delays, []time.Duration{want}) {
					t.Fatalf("after %d reads the store did not answer, the retries waiting are %v; want %v",
						read+1, delays, want)
				}
			}
			if failed := rig.failures(); len(failed) != 0 {
				t.Fatalf("announced %+v for a run whose reads the store only did not answer", failed)
			}
		})
	}
}
