package queue_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
)

// A PARTITION'S HEADROOM IS ITS NEAREST MESSAGE'S, because one outcome covers
// all of them: a hand-back spends a delivery of every message in the
// partition, so the one closest to its budget is the one that dead-letters
// first. A fold that took the largest — or the first — would report headroom
// a caller does not have, on exactly the delivery that has least.
//
// Folded HERE rather than in each backend, and asserted here for the same
// reason: two backends reading their own counters must not become two rules.
func TestAPartitionReportsItsNearestMessagesHeadroom(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		perMessage []int
		want       int
		wantKnown  bool
	}{
		"one message":            {perMessage: []int{7}, want: 7, wantKnown: true},
		"the smallest of many":   {perMessage: []int{9, 2, 24}, want: 2, wantKnown: true},
		"smallest arriving last": {perMessage: []int{24, 9, 0}, want: 0, wantKnown: true},
		// NOT A COUNT ANYONE CAN READ: a partition nothing readable came
		// back for reports the absence rather than a zero, because a
		// caller told "no deliveries left" stops offering work it could
		// still have done, while one told nothing keeps the bounds it had
		// before the value existed.
		"nothing readable": {perMessage: nil, want: 0, wantKnown: false},
		// A COUNT PAST THE BUDGET IS SPENT, NOT NEGATIVE. A message
		// delivered more times than its budget — an operator who shrank
		// MaxDeliver under a backlog — has no headroom, and a negative
		// number would compare as less than every reserve and read as
		// "plenty".
		"past the budget": {perMessage: []int{-3}, want: 0, wantKnown: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, known := queue.LeastDeliveriesLeft(tc.perMessage)
			if got != tc.want || known != tc.wantKnown {
				t.Errorf("LeastDeliveriesLeft(%v) = (%d, %v), want (%d, %v)",
					tc.perMessage, got, known, tc.want, tc.wantKnown)
			}
		})
	}
}

// AND AN UNSTATED COUNT IS UNSTATED, never a zero.
//
// A handler reached outside a queue — a test, a direct call, a backend whose
// metadata would not parse — must be able to tell "this message has no
// deliveries left" from "nobody said". Collapsing the two is what a plain int
// with a zero value would do, and the caller this exists for stops offering
// work at a low count.
func TestAnUnstatedDeliveryCountIsNotAZero(t *testing.T) {
	t.Parallel()
	if left, known := queue.DeliveriesLeft(context.Background()); known {
		t.Errorf("a bare context reported %d deliveries left", left)
	}
	ctx := queue.WithDeliveriesLeft(context.Background(), 0)
	if left, known := queue.DeliveriesLeft(ctx); !known || left != 0 {
		t.Errorf("a stated zero read back as (%d, %v), want (0, true)", left, known)
	}
	// A backend that subtracted past the budget states a spent message,
	// for the reason the fold clamps: negative reads as plenty.
	ctx = queue.WithDeliveriesLeft(context.Background(), -2)
	if left, known := queue.DeliveriesLeft(ctx); !known || left != 0 {
		t.Errorf("a negative count read back as (%d, %v), want (0, true)", left, known)
	}
}
