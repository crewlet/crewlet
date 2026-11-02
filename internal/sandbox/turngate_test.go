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
// refusal, and records the route of every resume it is asked about, every
// admission it grants and every release of one.
type turnGate struct {
	mu       sync.Mutex
	refusing bool
	asked    int
	granted  int
	released int
	routes   []ResumeRoute
}

type gateKey struct{}

func (g *turnGate) admit(ctx context.Context, _ string, route ResumeRoute) (context.Context, func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked++
	g.routes = append(g.routes, route)
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

// told is the route of every resume the gate was asked about, in order.
func (g *turnGate) told() []ResumeRoute {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.routes)
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

// THE GATE IS TOLD WHICH ROUTE A RESUME TAKES. The turn's first model call is
// on a different chain on each — a completion's is its reviewer's, an answer's
// the relaunch that carries the run on — and the engine's gate refuses a resume
// whose models are all at their limit by asking the chain that call is made
// on: told one route for both, it held a finished run's review for its
// executor's limit, and admitted an answer into it.
//
// Mutation: tell the gate the completion route from attemptOwed, and the
// answer's resume is asked about as a completion.
func TestTheGateIsToldEachResumesRoute(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	gate := gated(rig)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := gate.told(); !slices.Equal(got, []ResumeRoute{ResumeCompletion}) {
		t.Errorf("a completion told the gate %v, want %v", got, []ResumeRoute{ResumeCompletion})
	}
	rig.finished("t1")

	// The run's own completion parks it on its question; the person's answer
	// is then tried inline, under the delivery that carried it, and again
	// by its retry.
	answering := newCoordRig(t)
	answered := gated(answering)
	answeredAndOwed(t, answering)
	answering.fireRetries()
	want := []ResumeRoute{ResumeCompletion, ResumeAnswer, ResumeAnswer}
	if got := answered.told(); !slices.Equal(got, want) {
		t.Errorf("a run parked on its question and then answered told the gate %v, want %v", got, want)
	}
	if !ResumeCompletion.Valid() || !ResumeAnswer.Valid() || ResumeRoute("retry").Valid() {
		t.Error("Valid does not hold the routes to the two")
	}
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
// go of takes its kept completions with it, for the waiter that drives each
// run's box to announce to the seat's next holder.
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

// conditionLimit is what a resume refused for its models' limit waits on, in
// these cases.
const conditionLimit Condition = "provider"

// errAtTheLimit is a resumed turn's failure before it wrote anything: every
// model of its chain at a usage limit, as the engine's resumer reports it.
var errAtTheLimit = &TurnRefused{Cause: errors.New("llm chain exhausted: usage limit"),
	Refusal: Refusal{Condition: conditionLimit, Reason: "provider limit: default"}}

// A COMPLETION WHOSE RESUME MEETS A LIMIT BEFORE IT WRITES IS A WAIT, NOT A
// FAILURE: the claim is given back, the refusal is what the caller reads (the
// control topic acknowledges it, so it spends none of the completion's
// deliveries), and the completion is kept for the signal that ends the wait,
// which resumes it once.
//
// Mutation: keep only the gate's refusals in OnCompleted and the signal resumes
// nothing; hand the refusal back as a plain error and the control topic NAKs
// it into the same limit until its deliveries run out.
func TestACompletionWhoseResumeMeetsALimitIsKeptAndResumedOnce(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	gated(rig)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
	rig.resumer.failWith(errAtTheLimit)
	rig.resumer.failsInTurn = true

	payload, ev := rig.completion("t1")
	err := rig.coordinator.OnCompleted(t.Context(), payload, ev)
	var refused *TurnRefused
	if !errors.As(err, &refused) || refused.Condition != conditionLimit {
		t.Fatalf("OnCompleted = %v, want the resume's refusal naming %q", err, conditionLimit)
	}
	if errors.Is(err, ErrResumeAbandoned) {
		t.Fatalf("OnCompleted = %v, a resume that wrote nothing abandoned", err)
	}
	if got := rig.get("t1"); got.Status != StatusRunning {
		t.Fatalf("run = %q, want the claim given back to running", got.Status)
	}

	// THE LIMIT LIFTS: its signal re-fires the kept completion, once.
	rig.resumer.failWith(nil)
	rig.coordinator.Readmit(conditionLimit, "swe")
	eventually(t, func() bool { return len(rig.resumer.calls()) == 1 },
		"the kept completion never resumed on its condition's signal")
	rig.coordinator.Readmit(conditionLimit, "swe")
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("the completion resumed %d times, want once", len(calls))
	}
	eventually(t, func() bool {
		_, ok, err := rig.pending.Get(t.Context(), "t1")
		return err == nil && !ok
	}, "the resumed run was never settled")
}

// AN ANSWER WHOSE RESUME MEETS A LIMIT BEFORE IT WRITES WAITS, UNCHARGED.
//
// A limit lasts hours; the answer's attempts are ten and its window the run's
// pause_ttl_seconds. Charged as failures, the attempts are spent on the wait
// and the answer is let go of while nothing was wrong with it.
//
// Mutation: charge a refused resume as a failure (owedFailed) and the answer is
// declined before the limit lifts.
func TestAnAnswerWhoseResumeMeetsALimitWaitsUncharged(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	answeredAndOwed(t, rig)
	rig.resumer.failWith(errAtTheLimit)
	rig.resumer.failsInTurn = true

	for range 3 * MaxAnswerAttempts {
		if rig.fireRetries() != 1 {
			t.Fatal("a refused resume stopped waiting")
		}
	}
	if got := rig.get("t1"); got.Status != StatusAnswered || got.Answer == nil {
		t.Fatalf("run = %q (answer %v), want the answer still recorded on it", got.Status, got.Answer)
	}

	rig.resumer.failWith(nil)
	rig.coordinator.Readmit(conditionLimit, "swe")
	rig.fireRetries()
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("%d resumes once the limit lifted, want 1", len(calls))
	}
}

// A CONDITION THAT CLEARS WHILE THE GATE IS REFUSING STILL RE-FIRES THE
// COMPLETION, at once.
//
// The gate here signals the clearing itself, after it has decided to refuse and
// before it returns: the one interleaving in which Readmit runs while nothing
// is kept yet. Nothing else signals, and no poll runs, so only the keep seeing
// that signal can bring the completion back.
//
// Mutation: read what was signalled after the gate rather than before it, or
// keep the completion without asking whether its condition was signalled
// since, and it is never resumed.
func TestASignalWhileTheGateRefusesIsNotLost(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	var mu sync.Mutex
	refused := false
	rig.coordinator.gate = func(ctx context.Context, handle string, _ ResumeRoute) (context.Context, func(), error) {
		mu.Lock()
		first := !refused
		refused = true
		mu.Unlock()
		if first {
			// The condition clears, and is signalled, between the
			// refusal and the keep.
			rig.coordinator.Readmit(conditionDrain, handle)
			return nil, nil, &TurnRefused{Cause: errLeaving,
				Refusal: Refusal{Condition: conditionDrain, Reason: errLeaving.Error()}}
		}
		return ctx, func() {}, nil
	}
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "fixed the flake"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); !errors.Is(err, errLeaving) {
		t.Fatalf("OnCompleted = %v, want the gate's refusal", err)
	}
	rig.coordinator.inflight.Wait()
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("resumed %d times after a condition cleared while the gate refused, want once",
			len(calls))
	}
}
