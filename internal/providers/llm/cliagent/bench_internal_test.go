package cliagent

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

// spentProvider is a fake CLI that reports a spent plan — with a reset
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
// TO LEARN IT AGAIN. Once the CLI has said the plan is spent, every call in
// the window fails at once as a rate limit naming the instant — the chain
// moves to the next model — and the first call after it reaches the CLI again.
//
// Proved by taking the CLI AWAY after the first call: a call that launched
// one would fail as a missing binary, not as the spent plan.
//
// Mutation: make SittingOut always answer nil, and the second call reaches the
// missing binary.
func TestASpentPlanSitsTheEntryOutUntilItsReset(t *testing.T) {
	t.Parallel()
	p, clock := spentProvider(t, 37*time.Minute)
	if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindRateLimit {
		t.Fatalf("the spent plan answered %v, want a rate limit", err)
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
	if left := p.sitOut.until.Sub(clock.read()); left < 36*time.Minute || left > 38*time.Minute {
		t.Errorf("sits out %v, want the 37 minutes the CLI named", left)
	}

	clock.advance(38 * time.Minute)
	_, err = ask(t, p, llm.Request{})
	if err == nil || llm.KindOf(err) == llm.KindRateLimit || strings.Contains(err.Error(), "sitting out") {
		t.Fatalf("the first call after the window answered %v, want it to reach the CLI again", err)
	}
}

// A CALL QUEUED FOR A SLOT WHEN THE ENTRY IS SAT OUT LAUNCHES NOTHING. With
// every slot busy, a call passes the sit-out check before the spent plan is
// reported and waits for a slot; the call ahead of it then reports the plan
// spent. Taking the slot and starting a whole CLI to hear the same sentence is
// what the sit-out exists to prevent, so the call is refused once it holds the
// slot — and gives the slot back for the next one in the queue.
//
// Proved with the CLI taken AWAY: a call that launched one would fail as a
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
	p.bench(time.Minute, "Claude AI usage limit reached")
	<-p.slots

	err := <-done
	if llm.KindOf(err) != llm.KindRateLimit || !strings.Contains(err.Error(), "sitting out until") {
		t.Fatalf("the queued call answered %v, want the sit-out — it took its slot and "+
			"launched a CLI to hear the spent plan again", err)
	}
	if held := len(p.slots); held != 0 {
		t.Errorf("%d slot(s) still held after the refusal, want the slot handed back", held)
	}
}

// A SPENT PLAN THAT NAMED NO RESET SITS OUT A MINUTE — every shipped profile's
// case — and a reset beyond an hour is held to the hour, so an operator who
// signs in again or raises the plan has the entry back within it.
func TestASitOutIsAMinuteWithoutAResetAndNeverPastAnHour(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reset time.Duration
		want  time.Duration
	}{
		"no reset named":        {0, limitBench},
		"a reset days away":     {72 * time.Hour, maxLimitBench},
		"a reset inside a hour": {20 * time.Minute, 20 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, clock := spentProvider(t, tc.reset)
			if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindRateLimit {
				t.Fatalf("the spent plan answered %v, want a rate limit", err)
			}
			left := p.sitOut.until.Sub(clock.read())
			if left < tc.want-time.Minute || left > tc.want+time.Second {
				t.Errorf("sits out %v, want %v", left, tc.want)
			}
		})
	}
}

// A LATER REPORT NEVER SHORTENS THE WINDOW. Two seats' calls that both met the
// spent plan report it, and the one that lands last must not cut the longer
// window the first set.
func TestALaterBenchNeverShortensAnEarlierOne(t *testing.T) {
	t.Parallel()
	clock := &testClock{now: time.Now()}
	p := &Provider{now: clock.read}
	p.bench(30*time.Minute, "first")
	p.bench(time.Minute, "second")
	if left := p.sitOut.until.Sub(clock.read()); left != 30*time.Minute {
		t.Errorf("sits out %v after a shorter report, want the 30 minutes the first set", left)
	}
	if p.sitOut.said != "first" {
		t.Errorf("says %q, want the report that set the window", p.sitOut.said)
	}
}

// A LOGIN THE CLI CANNOT USE IS NOT BENCHED: `crewlet llm login` mends it from
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
