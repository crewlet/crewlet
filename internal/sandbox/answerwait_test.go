package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// seatConditions is an [Admission] a case moves by hand: it refuses with the
// refusal it holds until a case clears it, and counts how often it was asked.
type seatConditions struct {
	mu      sync.Mutex
	refusal *Refusal
	asked   int
}

func (s *seatConditions) admit(context.Context, string) (Refusal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked++
	if s.refusal == nil {
		return Refusal{}, false
	}
	return *s.refusal, true
}

func (s *seatConditions) refuse(r Refusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusal = &r
}

func (s *seatConditions) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusal = nil
}

func (s *seatConditions) checks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

// answeredAndOwed parks t1 on a question, records an answer whose inline
// resume fails, and leaves the run owed a retry with the resumer working
// again and the seat's conditions in the case's hands.
func answeredAndOwed(t *testing.T, rig *coordRig) *seatConditions {
	t.Helper()
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").AskedAt
	rig.resumer.failWith(errors.New("transient"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", replyAt("use main", asked.Add(time.Minute)))); d != AnswerConsumed {
		t.Fatalf("disposition = %q, want the answer recorded", d)
	}
	rig.resumer.failWith(nil)
	conditions := &seatConditions{}
	rig.coordinator.admit = conditions.admit
	return conditions
}

const conditionPaused Condition = "paused"

// A REFUSED RETRY WAITS FOR ITS SIGNAL, AND NOTHING RUNS WHILE IT WAITS.
//
// A person paused the seat. The retry is refused once and then WAITS — on the
// backstop, not on the 30-second ceiling it used to re-check at, so a pause
// that lasts the afternoon costs one admission check every five minutes
// rather than every thirty seconds — and a signal for any OTHER condition, or
// for another seat, wakes nothing. The resume runs only when the pause's own
// signal says it may have cleared, at once, and charges nothing for the wait.
func TestARefusedRetryWaitsForItsSignalAndNothingRunsMeanwhile(t *testing.T) {
	rig := newCoordRig(t)
	conditions := answeredAndOwed(t, rig)
	conditions.refuse(Refusal{Condition: conditionPaused, Reason: "a person paused this seat"})

	if rig.fireRetries() != 1 {
		t.Fatal("the failed resume scheduled no retry")
	}
	if got := rig.retries.delays(); !slices.Equal(got, []time.Duration{answerWaitBackstop}) {
		t.Fatalf("a refused retry waits %v, want only the %v backstop: a pause changes on an "+
			"event, and re-checking it on a timer is a poll", got, answerWaitBackstop)
	}
	if conditions.checks() != 1 || len(rig.resumer.calls()) != 0 {
		t.Fatalf("%d checks and %d resumes while refused, want one check and no resume",
			conditions.checks(), len(rig.resumer.calls()))
	}

	// SIGNALS FOR SOMETHING ELSE wake nothing.
	rig.coordinator.Readmit("budget")
	rig.coordinator.Readmit(conditionPaused, "somebody-else")
	if got := rig.retries.delays(); !slices.Equal(got, []time.Duration{answerWaitBackstop}) {
		t.Fatalf("a signal for another condition or seat re-armed the wait: %v", got)
	}
	if conditions.checks() != 1 {
		t.Fatalf("the seat's conditions were asked %d times, want once", conditions.checks())
	}

	// THE PAUSE'S OWN SIGNAL, once the person resumes the seat.
	conditions.clear()
	rig.coordinator.Readmit(conditionPaused, "swe")
	if got := rig.retries.delays(); !slices.Equal(got, []time.Duration{0}) {
		t.Fatalf("after the signal the next attempt waits %v, want it at once", got)
	}
	rig.fireRetries()
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("%d resumes after the signal, want 1", len(calls))
	}
	if left := rig.retries.delays(); len(left) != 0 {
		t.Fatalf("something is still scheduled after the resume: %v", left)
	}
}

// A REFUSAL THE CLOCK LIFTS IS RE-CHECKED THEN. A spent budget window turns
// over at an instant the refusal names, so the retry waits until exactly then
// — and never past the backstop.
func TestARefusalTheClockLiftsIsRecheckedWhenItLifts(t *testing.T) {
	rig := newCoordRig(t)
	conditions := answeredAndOwed(t, rig)
	conditions.refuse(Refusal{Condition: "budget", Reason: "budget: day window resets soon",
		Until: rig.now.Add(42 * time.Second)})
	rig.fireRetries()
	if got := rig.retries.delays(); !slices.Equal(got, []time.Duration{42 * time.Second}) {
		t.Fatalf("waits %v, want the 42s to the window's end", got)
	}

	conditions.refuse(Refusal{Condition: "budget", Until: rig.now.Add(9 * time.Hour)})
	rig.fireRetries()
	if got := rig.retries.delays(); !slices.Equal(got, []time.Duration{answerWaitBackstop}) {
		t.Fatalf("waits %v, want no longer than the backstop", got)
	}
}

// A MISSED SIGNAL IS STILL RECOVERED. The condition clears and nothing says
// so; the backstop re-checks, finds it clear, and the answer is resumed rather
// than stranded on a seat that could take it.
func TestAMissedSignalIsRecoveredByTheBackstop(t *testing.T) {
	rig := newCoordRig(t)
	conditions := answeredAndOwed(t, rig)
	conditions.refuse(Refusal{Condition: conditionPaused})
	rig.fireRetries()

	conditions.clear() // and no Readmit
	if rig.fireRetries() != 1 {
		t.Fatal("nothing was waiting to re-check a refusal whose signal never came")
	}
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("%d resumes after the backstop, want 1", len(calls))
	}
}

// A WAIT CHARGES NOTHING. However long the seat's conditions refuse, the
// attempts that exist for a resume that FAILS are all still there when they
// clear: a resume failing afterwards is retried, not let go.
func TestAWaitChargesNoAttempts(t *testing.T) {
	rig := newCoordRig(t)
	conditions := answeredAndOwed(t, rig)
	conditions.refuse(Refusal{Condition: conditionPaused})
	for range 3 * MaxAnswerAttempts {
		if rig.fireRetries() != 1 {
			t.Fatal("a refused retry stopped waiting")
		}
	}
	conditions.clear()
	rig.resumer.failWith(errors.New("transient"))
	rig.fireRetries()
	if got := rig.get("t1"); got.Status != StatusAnswered {
		t.Fatalf("status = %q after one failure following a long wait, want the answer still "+
			"owed: the wait spent the attempts", got.Status)
	}
	if got := rig.retries.delays(); len(got) != 1 || got[0] != answerRetryDelay(2) {
		t.Fatalf("next attempt in %v, want the second failure's %v", got, answerRetryDelay(2))
	}
}
