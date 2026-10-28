package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// THE NODE'S ADMISSION, ON EVERY RESUME.
//
// A resume starts a turn, and a draining node went on starting them: a
// completion from the seat's control topic and a recorded answer's retry from
// this coordinator's own timer both re-entered their turns without asking the
// node, which admits every mailbox turn. These cases hold both routes to the
// gate ([TurnGate]), BEFORE the claim, and to what a refusal leaves behind: a
// run exactly as the resume found it, for the seat's next holder.

// errLeaving is the gate's cause in these cases, standing in for the node's
// own drain sentinel.
var errLeaving = errors.New("the node is draining")

// conditionDrain is what a refusal for errLeaving waits on.
const conditionDrain Condition = "drain"

// turnGate is a [TurnGate] a case moves by hand: it refuses while it holds a
// refusal, and records every admission it grants and every release of one.
type turnGate struct {
	mu       sync.Mutex
	refusing bool
	asked    int
	granted  int
	released int
}

type gateKey struct{}

func (g *turnGate) admit(ctx context.Context, _ string) (context.Context, func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked++
	if g.refusing {
		return nil, nil, &TurnRefused{Cause: errLeaving,
			Refusal: Refusal{Condition: conditionDrain, Reason: errLeaving.Error()}}
	}
	g.granted++
	var once sync.Once
	return context.WithValue(ctx, gateKey{}, g.granted), func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.released++
		})
	}, nil
}

func (g *turnGate) refuse(refusing bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refusing = refusing
}

// counts is how often the gate was asked, how many admissions it granted and
// how many of those were released.
func (g *turnGate) counts() (asked, granted, released int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked, g.granted, g.released
}

// gated gives the rig's coordinator a gate the case moves by hand.
func gated(rig *coordRig) *turnGate {
	g := &turnGate{}
	rig.coordinator.gate = g.admit
	return g
}

// A COMPLETION THE NODE WILL NOT ADMIT LEAVES ITS RUN UNCLAIMED, and the next
// holder resumes it.
//
// Refused before the claim, nothing about the run moves: it is still running,
// its box uncollected, and the completion goes back carrying the gate's cause,
// so the control topic can tell a draining node from a lost seat. The seat's
// next holder then claims the same completion and resumes the turn, once.
//
// Mutation: drop the admission from OnCompleted and the draining coordinator
// resumes the turn itself; ask it after the claim and the run is left claimed
// (or collected and given back) by a node that was never going to run it.
func TestACompletionTheNodeWillNotAdmitIsLeftForTheNextHolder(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	gate := gated(rig)
	gate.refuse(true)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})

	payload, ev := rig.completion("t1")
	err := rig.coordinator.OnCompleted(t.Context(), payload, ev)
	if !errors.Is(err, ErrResumeUnavailable) || !errors.Is(err, errLeaving) {
		t.Fatalf("OnCompleted on a node that will not admit the turn = %v, want "+
			"ErrResumeUnavailable carrying the gate's cause", err)
	}
	if n := rig.resumer.turnsBegun(); n != 0 {
		t.Fatalf("%d turns began on a node that refused them", n)
	}
	if got := rig.get("t1"); got.Status != StatusRunning || got.Collected {
		t.Fatalf("run = %q (collected %v), want it still running and uncollected: the "+
			"refusal must come before the claim", got.Status, got.Collected)
	}
	if asked, granted, _ := gate.counts(); asked != 1 || granted != 0 {
		t.Fatalf("the gate was asked %d times and granted %d, want one refusal", asked, granted)
	}

	// THE NEXT HOLDER, whose node admits it, takes the completion that came
	// back, and collects the box this node never touched.
	next := successorOf(t, rig, rig.pending)
	next.coordinator.manager = rig.manager
	nextGate := gated(next)
	if err := next.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the next holder's OnCompleted: %v", err)
	}
	if calls := next.resumer.calls(); len(calls) != 1 {
		t.Fatalf("the next holder resumed %d times, want once", len(calls))
	}
	if _, granted, released := nextGate.counts(); granted != 1 || released != 1 {
		t.Fatalf("the next holder's gate granted %d and released %d, want one of each",
			granted, released)
	}
	rig.finished("t1")
}

// AN ADMITTED RESUME RUNS UNDER ITS ADMISSION AND HOLDS IT UNTIL IT SETTLES.
//
// The slot the admission takes is the turn's, so the resumed turn runs on the
// context the gate handed back, and the release comes once the resume has
// returned: released earlier, a second turn could take the slot beside it.
//
// Mutation: resume under the delivery's own context, or release before the
// resume, and the case reads it.
func TestAnAdmittedResumeHoldsItsAdmissionUntilItSettles(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	gate := gated(rig)
	var (
		heldDuring  bool
		underItsCtx bool
	)
	rig.resumer.during = func(ctx context.Context, _ PendingRun) {
		_, granted, released := gate.counts()
		heldDuring = granted == 1 && released == 0
		underItsCtx = ctx.Value(gateKey{}) == 1
	}
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if !heldDuring {
		t.Error("the resumed turn ran without its admission held")
	}
	if !underItsCtx {
		t.Error("the resumed turn ran under a context the gate did not hand back")
	}
	if _, granted, released := gate.counts(); granted != 1 || released != 1 {
		t.Fatalf("granted %d and released %d, want the one admission released once", granted, released)
	}
	rig.finished("t1")
}

// A RETRIED ANSWER THE NODE WILL NOT ADMIT WAITS, AND SPENDS NOTHING.
//
// A draining node is not a resume that failed, so none of the answer's attempts
// is charged while it refuses, however long that lasts, and the run keeps the
// answer. It waits on what the gate named; nothing signals a drain, so it is
// the backstop that re-checks.
//
// Mutation: drop the admission from attemptOwed and the retry resumes on the
// draining node; count a refusal as a failure and the answer is declined.
func TestARetriedAnswerTheNodeWillNotAdmitWaitsAndSpendsNothing(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	answeredAndOwed(t, rig)
	gate := gated(rig)
	gate.refuse(true)

	for range 3 * MaxAnswerAttempts {
		if rig.fireRetries() != 1 {
			t.Fatal("a refused retry stopped waiting")
		}
		if got := rig.retries.delays(); !slices.Equal(got, []time.Duration{answerWaitBackstop}) {
			t.Fatalf("a refused retry waits %v, want the %v backstop", got, answerWaitBackstop)
		}
	}
	if n := rig.resumer.turnsBegun(); n != 0 {
		t.Fatalf("%d turns began on a node that refused them", n)
	}
	if got := rig.get("t1"); got.Status != StatusAnswered || got.Answer == nil {
		t.Fatalf("run = %q (answer %v), want the answer still recorded on it", got.Status, got.Answer)
	}

	// ADMITTED AGAIN, the answer resumes on its signal, and the admission is
	// held for the whole of the resumed turn: a drain that begins while it
	// runs waits for it.
	var heldDuring bool
	rig.resumer.during = func(context.Context, PendingRun) {
		_, granted, released := gate.counts()
		heldDuring = granted == 1 && released == 0
	}
	gate.refuse(false)
	rig.coordinator.Readmit(conditionDrain, "swe")
	rig.fireRetries()
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("%d resumes once admitted, want 1", len(calls))
	}
	if !heldDuring {
		t.Error("the retried answer's turn ran without its admission held")
	}
	if _, granted, released := gate.counts(); granted != 1 || released != 1 {
		t.Fatalf("granted %d and released %d, want the one admission released once", granted, released)
	}
}

// AN ANSWER WHOSE INLINE RESUME THE NODE REFUSES IS STILL OWED.
//
// The inline attempt runs on the delivery that recorded the answer, which has
// already been spent: a refusal there must open the retry series itself, or the
// answer waits on nothing until the seat moves. And the seat's next holder
// resumes it once this node lets the seat go.
//
// Mutation: return without opening the series and no retry is armed.
func TestAnInlineAnswerTheNodeRefusesIsStillOwed(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	gate := gated(rig)
	gate.refuse(true)
	asked := rig.get("t1").AskedAt

	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", replyAt("use main", asked.Add(time.Minute)))); d != AnswerConsumed {
		t.Fatalf("disposition = %q, want the answer recorded", d)
	}
	if n := rig.resumer.turnsBegun(); n != 0 {
		t.Fatalf("%d turns began on a node that refused them", n)
	}
	if got := rig.retries.delays(); !slices.Equal(got, []time.Duration{answerWaitBackstop}) {
		t.Fatalf("the refused inline resume armed %v, want a wait on the %v backstop",
			got, answerWaitBackstop)
	}

	// THE DRAIN HANDS THE SEAT BACK, and its next holder resumes the answer
	// the run still carries.
	rig.coordinator.ReleaseSeat("swe")
	next := successorOf(t, rig, rig.pending)
	if calls := next.resumer.calls(); len(calls) != 1 {
		t.Fatalf("the next holder resumed %d times, want once", len(calls))
	}
}

// A COMPLETION REFUSED ON A CONDITION IS RE-FIRED THE MOMENT THE CONDITION
// CLEARS, for the seat it was refused on, and not after the seat is let go.
//
// The waiter would announce it again on its next poll; the signal that the
// condition cleared ([Coordinator.Readmit]) does not wait for that. A signal for
// another condition or another seat leaves it kept, and a seat this node lets
// go of takes its kept completions with it, for the successor's waiter to
// announce there.
//
// Mutation: keep nothing on a refusal, or re-fire on any signal, and the case
// reads it; leave the kept completion behind on ReleaseSeat and the released
// seat's run is resumed by a node that no longer holds it.
func TestARefusedCompletionIsReFiredWhenItsConditionClears(t *testing.T) {
	t.Parallel()
	for name, released := range map[string]bool{"held": false, "released": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			gate := gated(rig)
			gate.refuse(true)
			rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); !errors.Is(err, errLeaving) {
				t.Fatalf("OnCompleted = %v, want the gate's refusal", err)
			}

			gate.refuse(false)
			rig.coordinator.Readmit("another_condition", "swe")
			rig.coordinator.Readmit(conditionDrain, "another_seat")
			if released {
				rig.coordinator.ReleaseSeat("swe")
			}
			rig.coordinator.Readmit(conditionDrain, "swe")
			// THE COORDINATOR'S OWN GOROUTINES, which a re-fire runs on, are
			// counted in its in-flight group: waited for here, not stopped,
			// since stopping would cancel a re-fire before it ran.
			rig.coordinator.inflight.Wait()

			want := 1
			if released {
				want = 0
			}
			if calls := rig.resumer.calls(); len(calls) != want {
				t.Fatalf("resumed %d times after the condition cleared, want %d", len(calls), want)
			}
			if asked, _, _ := gate.counts(); asked != 1+want {
				t.Fatalf("the gate was asked %d times, want %d: a signal for another "+
					"condition or seat re-fired the completion", asked, 1+want)
			}
		})
	}
}
