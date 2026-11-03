package harness

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clocktest"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// testClock is a clock a case moves by hand, so a sit-out can be watched to
// its end without waiting it out.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// spentProvider is a fake harness that reports a spent plan — with a reset
// instant `reset` from now when reset is non-zero, and none otherwise — on a
// clock the case moves.
func spentProvider(t *testing.T, reset time.Duration) (*Provider, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Now()}
	stdout := "Claude AI usage limit reached"
	marker := map[string]any{"sentinel": "Claude AI usage limit reached"}
	if reset > 0 {
		stdout += fmt.Sprintf("|%d", clock.read().Add(reset).Unix())
		marker["reset_separator"], marker["reset_unit"] = "|", "epoch"
	}
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": stdout},
		map[string]any{"limit_markers": []any{marker}})
	p.now = clock.read
	return p, clock
}

// A SPENT PLAN SITS THE ENTRY OUT UNTIL ITS RESET, AND NO PROCESS IS LAUNCHED
// TO LEARN IT AGAIN. Once the harness has said the plan is spent, every call in
// the window fails at once as a rate limit naming the instant — the chain
// moves to the next model — and the first call after it reaches the harness again.
//
// Proved by taking the harness AWAY after the first call: a call that launched
// one would fail as a missing binary, not as the spent plan.
//
// Mutation: make SittingOut always answer nil, and the second call reaches the
// missing binary.
func TestASpentPlanSitsTheEntryOutUntilItsReset(t *testing.T) {
	t.Parallel()
	p, clock := spentProvider(t, 37*time.Minute)
	_, first := ask(t, p, llm.Request{})
	if llm.KindOf(first) != llm.KindRateLimit {
		t.Fatalf("the spent plan answered %v, want a rate limit", first)
	}
	// WHAT THE LIMIT IS OF, for whoever tells a person: the entry's harness and
	// that it signs in on a plan.
	var failure *llm.Error
	if !errors.As(first, &failure) || failure.Harness != "custom" || !failure.Subscription {
		t.Errorf("the limit names %+v, want the entry's harness on a subscription", failure)
	}
	p.profile.Binary = "/nonexistent/crewlet-no-cli"

	_, err := ask(t, p, llm.Request{})
	if llm.KindOf(err) != llm.KindRateLimit {
		t.Fatalf("a call in the window answered %v, want the rate limit it sits out", err)
	}
	for _, want := range []string{"usage limit reached", "sitting out until"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the sit-out says %q, want it to name %q", err, want)
		}
	}
	if left := sitOutUntil(p).Sub(clock.read()); left < 36*time.Minute || left > 38*time.Minute {
		t.Errorf("sits out %v, want the 37 minutes the harness named", left)
	}

	clock.advance(38 * time.Minute)
	_, err = ask(t, p, llm.Request{})
	if err == nil || llm.KindOf(err) == llm.KindRateLimit || strings.Contains(err.Error(), "sitting out") {
		t.Fatalf("the first call after the window answered %v, want it to reach the harness again", err)
	}
}

// A CALL QUEUED FOR A SLOT WHEN THE ENTRY IS SAT OUT LAUNCHES NOTHING. With
// every slot busy, a call passes the sit-out check before the spent plan is
// reported and waits for a slot; the call ahead of it then reports the plan
// spent. Taking the slot and starting a whole harness to hear the same sentence is
// what the sit-out exists to prevent, so the call is refused once it holds the
// slot — and gives the slot back for the next one in the queue.
//
// Proved with the harness taken AWAY: a call that launched one would fail as a
// missing binary, not as the sit-out.
//
// Mutation: drop the check made once the slot is held, and the queued call
// reaches the missing binary.
func TestACallQueuedWhenTheEntryIsSatOutLaunchesNothing(t *testing.T) {
	t.Parallel()
	p, clock := spentProvider(t, 0)
	p.profile.Binary = "/nonexistent/crewlet-no-cli"
	// ONE slot, and it is taken: the call below has to queue for it.
	p.slots = make(chan struct{}, 1)
	p.slots <- struct{}{}

	// The clock's first read is the queued call's check on arrival, which it
	// passes, since nothing has sat the entry out yet.
	arrived := make(chan struct{})
	var once sync.Once
	p.now = func() time.Time {
		once.Do(func() { close(arrived) })
		return clock.read()
	}

	done := make(chan error, 1)
	go func() {
		_, err := ask(t, p, llm.Request{})
		done <- err
	}()
	<-arrived

	// The call holding the slot reports the spent plan, and lets it go.
	p.bench(limitReport{reset: clock.read().Add(time.Minute), said: "Claude AI usage limit reached", at: clock.read()})
	<-p.slots

	err := <-done
	if llm.KindOf(err) != llm.KindRateLimit || !strings.Contains(err.Error(), "sitting out until") {
		t.Fatalf("the queued call answered %v, want the sit-out — it took its slot and "+
			"launched a harness to hear the spent plan again", err)
	}
	if held := len(p.slots); held != 0 {
		t.Errorf("%d slot(s) still held after the refusal, want the slot handed back", held)
	}
}

// A SPENT PLAN THAT NAMED NO RESET SITS OUT THE FIRST STEP OF THE BACKOFF,
// and a reset beyond an hour is held to the hour, so an operator who signs in
// again or raises the plan has the entry back within it.
func TestASitOutIsTheFirstBackoffStepWithoutAResetAndNeverPastAnHour(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reset time.Duration
		want  time.Duration
	}{
		"no reset named":        {0, limitBackoff[0]},
		"a reset days away":     {72 * time.Hour, maxLimitBench},
		"a reset inside a hour": {20 * time.Minute, 20 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, clock := spentProvider(t, tc.reset)
			if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindRateLimit {
				t.Fatalf("the spent plan answered %v, want a rate limit", err)
			}
			left := sitOutUntil(p).Sub(clock.read())
			if left < tc.want-time.Minute || left > tc.want+time.Second {
				t.Errorf("sits out %v, want %v", left, tc.want)
			}
		})
	}
}

// A LIMIT THAT NAMES NO RESET IS PROBED LESS OFTEN EACH TIME IT REFUSES AGAIN,
// and from the first step once the entry answers. Every probe that finds the
// window over and the limit still there takes the next step of [limitBackoff],
// up to the last; reports that land while a window is in force (two seats'
// calls refused together) take none, or one afternoon's concurrent refusals
// would jump straight to the longest wait.
//
// Mutation: step the backoff on every report, and the concurrent report moves
// the second window to two minutes.
func TestAnUnnamedResetBacksOffAndStartsAgainOnceAnswered(t *testing.T) {
	t.Parallel()
	clock := &testClock{now: time.Date(2026, time.October, 27, 23, 16, 0, 0, time.UTC)}
	p := &Provider{now: clock.read}
	window := func() time.Duration {
		at := clock.read()
		return p.bench(limitReport{said: "limited", at: at}).Sub(at)
	}
	if got := window(); got != time.Minute {
		t.Fatalf("first window %v, want a minute", got)
	}
	if got := window(); got != time.Minute {
		t.Errorf("a report inside the window moved it to %v, want the minute it set", got)
	}
	want := []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, step := range want {
		clock.advance(sitOutUntil(p).Sub(clock.read()))
		if got := window(); got != step {
			t.Errorf("probe %d refused again sits out %v, want %v", i+1, got, step)
		}
	}
	clock.advance(sitOutUntil(p).Sub(clock.read()))
	p.answered(clock.read())
	if got := window(); got != time.Minute {
		t.Errorf("a limit after the entry answered sits out %v, want the first step again", got)
	}
}

// A LATER REPORT NEVER SHORTENS THE WINDOW. Two seats' calls that both met the
// spent plan report it, and the one that lands last must not cut the longer
// window the first set.
func TestALaterBenchNeverShortensAnEarlierOne(t *testing.T) {
	t.Parallel()
	clock := &testClock{now: time.Now()}
	p := &Provider{now: clock.read}
	p.bench(limitReport{reset: clock.read().Add(30 * time.Minute), said: "first", at: clock.read()})
	p.bench(limitReport{reset: clock.read().Add(time.Minute), said: "second", at: clock.read()})
	if left := sitOutUntil(p).Sub(clock.read()); left != 30*time.Minute {
		t.Errorf("sits out %v after a shorter report, want the 30 minutes the first set", left)
	}
	if sitOutSaid(p) != "first" {
		t.Errorf("says %q, want the report that set the window", sitOutSaid(p))
	}
}

// A LOGIN THE HARNESS CANNOT USE IS NOT BENCHED: `crewlet llm login` mends it from
// another process, and the very next call must find it rather than sit out a
// window on the fallback.
//
// Mutation: bench every classified marker, and the entry sits out.
func TestARejectedLoginDoesNotSitTheEntryOut(t *testing.T) {
	t.Parallel()
	// A refused login is a FAILED run, which is what lets the sentinel stand
	// mid-line: on a clean exit it would have to open the answer.
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "Not logged in · Please run /login", "FAKE_EXIT": "1"},
		map[string]any{"auth_markers": []any{map[string]any{"sentinel": "Please run /login"}}})
	if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindAuth {
		t.Fatalf("the rejected login answered %v, want auth", err)
	}
	if err := p.SittingOut(); err != nil {
		t.Errorf("a rejected login sat the entry out: %v", err)
	}
}

// A WINDOW IS KEPT ON THE WALL CLOCK, the clock its end is named on.
//
// An instant read from time.Now carries a monotonic reading as well, and two
// instants that both carry one compare on it alone. The monotonic clock stops
// while the host sleeps, so a window kept with one outlasted a sleep by the
// sleep's length: the entry went on refusing while the end it named, which
// every reader compares on the wall clock, was already past, and the engine's
// probe, armed for that end, asked again at once, thousands of times a second.
// So a window holds no monotonic reading, whether its own report opened it or
// another provider's ([Plan.Hold]), and every comparison against it is on the
// wall clock.
//
// Mutation: store the report's own instant in record, and the window keeps
// its monotonic reading.
func TestAWindowIsKeptOnTheWallClock(t *testing.T) {
	t.Parallel()
	monotonic := func(t time.Time) bool { return strings.Contains(t.String(), " m=") }
	at := time.Now()
	if !monotonic(at) {
		t.Fatal("control: time.Now carries no monotonic reading here, so nothing below can fail")
	}
	for name, report := range map[string]limitReport{
		"a named reset": {reset: at.Add(30 * time.Minute), said: "usage limit reached",
			at: at, window: llm.LimitSession},
		"no reset named": {said: "usage limit reached", at: at},
	} {
		var b bench
		until := b.record(report)
		for field, instant := range map[string]time.Time{
			"end": b.until, "start": b.since, "reset": b.resetsAt, "answer": until,
		} {
			if monotonic(instant) {
				t.Errorf("%s: the window's %s %s carries a monotonic reading", name, field, instant)
			}
		}
		if out, held := b.sitOut(until.Add(-time.Second), ""); !held || !out.Until.Equal(until) {
			t.Errorf("%s: the window is not in force a second before its end", name)
		}
		if _, held := b.sitOut(until, ""); held {
			t.Errorf("%s: the window is still in force at its end on the wall clock", name)
		}
	}

	plan := newPlan()
	plan.Hold(SitOut{Until: at.Add(time.Hour), ResetsAt: at.Add(time.Hour), Since: at,
		Window: llm.LimitSession})
	if monotonic(plan.wide.until) || monotonic(plan.wide.since) || monotonic(plan.wide.resetsAt) {
		t.Errorf("a window held for another provider carries a monotonic reading: %+v", plan.wide)
	}
}

// A HOST THAT SLEPT THROUGH A WINDOW'S END WAKES WITH THE ENTRY TRIED AGAIN,
// and no refusal names a next try the wall clock has already passed.
//
// After a host slept inside a window, an entry whose window was judged on the
// monotonic clock went on refusing for the sleep's length while naming as its
// next try an end the wall clock had passed. The engine's probe, armed for that
// end, was due at once and asked again as fast as the refusal returned: on
// 2026-11-01 one node renewed its parks 20 834 times in a minute, every flood
// beginning just after the host woke. The case reads the entry through a clock
// that slept (clocktest.AfterSleep), inside the window and past its end.
//
// Mutation: store the report's own instant in record, and after two hours'
// sleep the entry still refuses, naming a next try two hours past.
func TestAHostThatSleptThroughAWindowWakesWithTheEntryTriedAgain(t *testing.T) {
	t.Parallel()
	p, clock := spentProvider(t, 0)
	if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindRateLimit {
		t.Fatalf("the spent plan answered %v, want a rate limit", err)
	}
	opened := clock.read()
	if !strings.Contains(opened.String(), " m=") {
		t.Fatal("control: the case's clock carries no monotonic reading, so nothing here can fail")
	}

	for name, tc := range map[string]struct {
		slept   time.Duration
		sitsOut bool
	}{
		"a sleep inside the window":  {30 * time.Second, true},
		"a sleep past the window":    {2 * time.Hour, false},
		"a sleep exactly to its end": {limitBackoff[0], false},
	} {
		woke := clocktest.AfterSleep(t, opened, tc.slept)
		clock.mu.Lock()
		clock.now = woke
		clock.mu.Unlock()
		err := p.SittingOut()
		if (err != nil) != tc.sitsOut {
			t.Errorf("%s: the entry sits out = %v (%v), want %v", name, err != nil, err, tc.sitsOut)
		}
		if retry, _, limited := llm.LimitedUntil(err); limited && !retry.After(woke) {
			t.Errorf("%s: the refusal names %s as its next try, already past at %s",
				name, retry, woke.Round(0))
		}
		if out, held := p.SitOut(); held && !out.Until.After(woke) {
			t.Errorf("%s: the window ends at %s and is still in force at %s",
				name, out.Until, woke.Round(0))
		}
	}
}
