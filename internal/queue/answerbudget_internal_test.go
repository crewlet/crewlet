package queue

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// heldMeter records what an answer budget measures: the bytes it holds, and
// each wait's outcome.
type heldMeter struct {
	mu       sync.Mutex
	held     int64
	outcomes []AnswerOutcome
}

func (m *heldMeter) AnswerWaited(_ AnswerKind, outcome AnswerOutcome, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes = append(m.outcomes, outcome)
}

func (m *heldMeter) AnswerHeld(bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.held = bytes
}

func (m *heldMeter) bytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held
}

// A LARGE WAITER IS NEVER STARVED: a box read at its cap (160 MiB) queued
// behind small holders is admitted before small answers that arrived after
// it, even while those would fit beside the holders.
//
// Mutations, each red: admit by whatever fits now (TryAcquire in a loop);
// count a hold's bytes before its wait ends.
func TestALargeWaiterIsAdmittedBeforeSmallerAnswersThatArrivedAfterIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const small, large = 16 << 20, 160 << 20
		meter := &heldMeter{}
		b := NewAnswerBudget(meter)
		var releases []func()
		for range AnswerBudgetBytes / small {
			release, err := b.Acquire(t.Context(), AnswerHistory, small)
			if err != nil {
				t.Fatalf("a small holder on an empty budget: %v", err)
			}
			releases = append(releases, release)
		}
		type admission struct {
			name    string
			release func()
		}
		admitted := make(chan admission, 8)
		wait := func(name string, weight int64) {
			go func() {
				release, err := b.Acquire(t.Context(), AnswerBoxRead, weight)
				if err != nil {
					t.Errorf("%s: %v", name, err)
					return
				}
				admitted <- admission{name, release}
			}()
		}
		wait("large", large)
		synctest.Wait()
		for _, name := range []string{"small-1", "small-2", "small-3"} {
			wait(name, small)
		}
		synctest.Wait()
		for i, release := range releases[:large/small] {
			release()
			synctest.Wait()
			if i < large/small-1 && len(admitted) > 0 {
				t.Fatalf("with %d MiB free, %s was admitted ahead of the 160 MiB waiter that arrived first",
					(i+1)*small>>20, (<-admitted).name)
			}
		}
		first := <-admitted
		if first.name != "large" {
			t.Fatalf("with 160 MiB free, %s was admitted first, want the 160 MiB waiter", first.name)
		}
		synctest.Wait()
		if len(admitted) > 0 {
			t.Fatalf("%s was admitted past a full budget", (<-admitted).name)
		}
		if got := meter.bytes(); got != AnswerBudgetBytes {
			t.Errorf("the budget holds %d bytes, want all %d", got, AnswerBudgetBytes)
		}
		first.release()
		for _, release := range releases[large/small:] {
			release()
		}
		for range 3 {
			(<-admitted).release()
		}
	})
}

// A WAIT THAT ENDS NAMES THE BUDGET: an answer whose context ends while the
// budget is held is refused with ErrAnswerBudget, its context's own error
// beside it and the constant to read in its words, holds nothing, and is
// measured as ended.
//
// Mutations, each red: wait past the context; return the context's error
// alone; count the refused hold's bytes.
func TestAWaitThatEndsNamesTheNodesAnswerBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		meter := &heldMeter{}
		b := NewAnswerBudget(meter)
		release, err := b.Acquire(t.Context(), AnswerReplay, AnswerBudgetBytes)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err = b.Acquire(ctx, AnswerMemory, 1<<20)
		switch {
		case !errors.Is(err, ErrAnswerBudget) || !errors.Is(err, context.DeadlineExceeded):
			t.Fatalf("a wait past its deadline: %v, want ErrAnswerBudget beside the deadline", err)
		case !strings.Contains(err.Error(), "queue.AnswerBudgetBytes") ||
			!strings.Contains(err.Error(), "memory answer"):
			t.Errorf("the refusal %q names neither the budget nor the answer", err)
		}
		if got := meter.bytes(); got != AnswerBudgetBytes {
			t.Errorf("the budget holds %d bytes after a refusal, want the first hold's %d", got,
				AnswerBudgetBytes)
		}
		if got := meter.outcomes; len(got) != 2 || got[1] != AnswerEnded {
			t.Errorf("the waits ended %v, want admitted then ended", got)
		}
	})
}

// AN ANSWER'S HOLD IS GIVEN BACK ONCE ITS REPLY IS WRITTEN, not when its
// handler lets go: under Replying's context the release the handler calls
// gives nothing back, and the backend's done gives back everything. Outside an
// answer the release gives it back.
//
// Mutations, each red: give back on the handler's release inside an answer;
// leave a hold to done outside one; done giving back nothing.
func TestAnAnswersHoldIsGivenBackOnceItsReplyIsWritten(t *testing.T) {
	t.Parallel()
	const weight = 20 << 20
	meter := &heldMeter{}
	b := NewAnswerBudget(meter)
	ctx, replied := Replying(t.Context())
	release, err := b.Acquire(ctx, AnswerHistory, weight)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if got := meter.bytes(); got != weight {
		t.Fatalf("the handler let go and the budget holds %d bytes, want %d until the reply is written", got,
			weight)
	}
	replied()
	replied()
	if got := meter.bytes(); got != 0 {
		t.Fatalf("the reply is written and the budget still holds %d bytes", got)
	}
	release, err = b.Acquire(t.Context(), AnswerBoxRead, weight)
	if err != nil {
		t.Fatal(err)
	}
	if got := meter.bytes(); got != weight {
		t.Fatalf("a collection outside an answer holds %d bytes, want %d", got, weight)
	}
	release()
	if got := meter.bytes(); got != 0 {
		t.Errorf("a collection outside an answer let go and the budget holds %d bytes", got)
	}
}

// AN ANSWER ACQUIRES ONCE: a second hold in the same answer is refused at
// once, never queued behind the first (FIFO, it could wait on itself), and
// leaves the first held; a hold whose wait ended leaves the answer free to
// take one.
//
// Mutations, each red: let a second hold wait; drop the first on the refusal;
// leave the answer marked holding after its wait ended.
func TestAnAnswerAcquiresTheBudgetOnce(t *testing.T) {
	t.Parallel()
	const weight = 8 << 20
	meter := &heldMeter{}
	b := NewAnswerBudget(meter)
	ctx, replied := Replying(t.Context())
	defer replied()
	if _, err := b.Acquire(ctx, AnswerTail, weight); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire(ctx, AnswerTail, weight); !errors.Is(err, errAnswerHeldTwice) {
		t.Fatalf("a second hold in one answer: %v, want it refused", err)
	}
	if got := meter.bytes(); got != weight {
		t.Errorf("after the refusal the budget holds %d bytes, want the first hold's %d", got, weight)
	}
	other, done := Replying(t.Context())
	defer done()
	ended, cancel := context.WithCancel(other)
	cancel()
	if _, err := b.Acquire(ended, AnswerTail, weight); !errors.Is(err, ErrAnswerBudget) {
		t.Fatalf("a hold on an ended context: %v, want ErrAnswerBudget", err)
	}
	if _, err := b.Acquire(other, AnswerTail, weight); err != nil {
		t.Errorf("a hold after a wait that ended: %v, want it taken", err)
	}
}

// A WEIGHT THE BUDGET CANNOT ADMIT IS REFUSED: none, or more than the whole
// budget, which would wait for ever.
//
// Mutations, each red: admit a weight of nothing; let one past the budget wait.
func TestAWeightOutsideTheBudgetIsRefused(t *testing.T) {
	t.Parallel()
	b := NewAnswerBudget(nil)
	for _, weight := range []int64{0, -1, AnswerBudgetBytes + 1} {
		if _, err := b.Acquire(t.Context(), AnswerBridge, weight); !errors.Is(err, errAnswerWeight) {
			t.Errorf("a weight of %d: %v, want it refused", weight, err)
		}
	}
}
