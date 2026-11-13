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

// EVERY HAND-BACK THE STORE DOES NOT ANSWER IS KEPT UNTIL IT LANDS, a park's and
// a resume's as much as a collection's. A park whose question could not be
// announced or recorded, and a resume that failed or that its seat's budget
// held, each hands back the claim a completion took, and the release that does
// it is one more write to a coordination store that may be the very one that
// went silent. Each used to end the run where the store did not answer that
// release, as a claim that cannot be given back, announced claim_unreverted,
// while the run's box held its question or its result and the store answered a
// moment later. Now this node keeps the claim, makes the release again on the
// hand-back's spacing while the store is silent, claims nothing of the job
// meanwhile, and keeps the seat counted held by the claim the run still is on
// the store's row; once the store answers, the run is back in running, counted
// once, and its next collection parks it on its question or resumes it.
//
// Mutation: end a park's or a resume's release the store did not answer, as
// unclaimAt did; give a resume's claim's count back before its release, as
// resumeAndSettle did, and the seat reads free while the release is owed; or
// count the run where its release lands instead of moving the claim's count
// there, and the seat counts the one run twice. Each turns a case red.
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
// THEN RESUMED. A person's answer is recorded on the run and resumed by this
// node's own series, and a resume that fails, or that its seat's budget holds,
// hands the claim back to the answer. A release the store did not answer used
// to end the run, letting the person's answer go back to the seat as an
// ordinary message. Now the series that owes the answer owes the release too:
// it makes the release again before anything else, no later than the
// hand-back's spacing whatever the answer waits for, makes no attempt at the
// answer while the release is out, and once the store answers goes on to the
// answer, resuming the run with it once.
//
// Mutation: settle the series once its release lands, as a collection's release
// settles it, and the answer is never resumed; count a read the store did not
// answer after the release landed as a failed attempt, and the next one comes
// on the answer's backoff; or arm a series that owes a release on the wait its
// answer's budget names, and the release waits for the budget's window. Each
// turns a case red.
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
