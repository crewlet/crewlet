package statelog_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A CALL'S OWN RESOLVE BUDGET BOUNDS ITS WAITS, AND ONLY ITS OWN.
//
// [statelog.WithResolveBudget] is how a case that halted this node's applier
// learns `pending` without spending the publisher's whole budget — while every
// other write it makes, against appliers that are running or resuming, keeps
// the budget their answers depend on. So both halves are pinned: a write on a
// context carrying one waits at most that long — in the resolution, the
// session wait and the wait behind a peer's record alike, the three waits that
// share the write path's budget — and a write beside it on a plain context
// waits the publisher's own (the harness's 250 ms).
func TestACallsResolveBudgetBoundsItsWaitsAndOnlyItsOwn(t *testing.T) {
	t.Parallel()
	const short = 40 * time.Millisecond

	t.Run("resolution", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.applier.mu.Lock()
		h.applier.auto = false // this node never reaches its own record
		h.applier.mu.Unlock()

		for _, write := range []struct {
			ctx    context.Context
			op     string
			within time.Duration
			above  time.Duration
		}{
			{statelog.WithResolveBudget(t.Context(), short), "op-short", short, 0},
			// The plain write must not inherit the short budget.
			{t.Context(), "op-plain", 250 * time.Millisecond, short},
		} {
			before := len(h.applier.waitBudgets())
			res, err := h.pub.Publish(write.ctx, sessionWrite(probeSubject(write.op),
				write.op, statelog.Position{}, func() {}))
			if err != nil || res.Outcome != statelog.OutcomePending {
				t.Fatalf("%s answered %q (%v), want pending", write.op, res.Outcome, err)
			}
			waits := h.applier.waitBudgets()[before:]
			if len(waits) == 0 {
				t.Fatalf("%s resolved without waiting for its applier", write.op)
			}
			for _, w := range waits {
				if w > write.within || w <= write.above {
					t.Fatalf("%s waited on a budget of %v, want %v", write.op, w, write.within)
				}
			}
		}
	})

	t.Run("behind", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil, []byte("peer"))
		if err != nil {
			t.Fatalf("the peer's write: %v", err)
		}
		h.anchorAt(probeSubject("a"), peer-1) // behind a peer's record...
		h.applier.mu.Lock()
		h.applier.stalled = true // ...that this node never reaches
		h.applier.mu.Unlock()

		_, err = h.pub.Publish(statelog.WithResolveBudget(t.Context(), short),
			sessionWrite(probeSubject("a"), "op-1", statelog.Position{}, func() {}))
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonBehind {
			t.Fatalf("the write answered %v, want behind", err)
		}
		if !strings.Contains(refusal.Detail, short.String()) {
			t.Errorf("the refusal reads %q, which does not name the %v the write "+
				"waited", refusal.Detail, short)
		}
		for _, w := range h.applier.waitBudgets() {
			if w > short {
				t.Fatalf("the wait behind the peer ran on a budget of %v, want %v", w, short)
			}
		}
	})

	t.Run("session", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.applier.mu.Lock()
		h.applier.stalled = true // the caller's own write is never reached
		h.applier.mu.Unlock()

		mark := statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}
		_, err := h.pub.Publish(statelog.WithResolveBudget(t.Context(), short),
			sessionWrite(probeSubject("a"), "op-1", mark, func() {}))
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonBehind {
			t.Fatalf("the write answered %v, want behind", err)
		}
		if !strings.Contains(refusal.Detail, short.String()) {
			t.Errorf("the refusal reads %q, which does not name the %v the write "+
				"waited", refusal.Detail, short)
		}
		for _, w := range h.applier.waitBudgets() {
			if w > short {
				t.Fatalf("the session wait ran on a budget of %v, want %v", w, short)
			}
		}
	})
}

// A NON-POSITIVE CALL BUDGET CHANGES NOTHING: zero is the absence of one, as
// every other budget here reads it, never a write that gives up at once.
func TestANonPositiveCallBudgetChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, d := range []time.Duration{0, -time.Second} {
		if got := statelog.WithResolveBudget(ctx, d); got != ctx {
			t.Errorf("a budget of %v produced a context of its own", d)
		}
	}
}

// A PUBLISHER WITH NO BUDGET OF ITS OWN WAITS THE DEFAULT — which is every
// publisher the engine builds, since it sets none: the per-call budget above
// is the only way a write here waits anything shorter. The wait's budget is
// read as it begins and the caller then gives up, so the case costs nothing
// near the five seconds it pins.
func TestAPublisherWithNoBudgetOfItsOwnWaitsTheDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.applier.mu.Lock()
	h.applier.auto = false
	h.applier.mu.Unlock()
	deps := h.deps
	deps.ResolveBudget = 0
	pub, err := statelog.NewPublisher(deps)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := pub.Publish(ctx, sessionWrite(probeSubject("a"), "op-1",
			statelog.Position{}, func() {}))
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(h.applier.waitBudgets()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the write never waited for its applier")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if w := h.applier.waitBudgets()[0]; w > statelog.DefaultResolveBudget ||
		w <= statelog.DefaultResolveBudget-time.Second {
		t.Fatalf("a publisher with no budget of its own waited on %v, want %v",
			w, statelog.DefaultResolveBudget)
	}
}
