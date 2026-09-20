package queue_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue"
)

// TWO NUMBERS, TWO QUESTIONS, and a handler must be able to ask either.
//
// The partition's is its NEAREST message's, because one outcome covers all of
// them: a hand-back spends a delivery of every message in the partition, so
// the one closest to its budget is the one that dead-letters first. A fold
// that took the largest — or the first — would report headroom a caller does
// not have, on exactly the delivery that has least.
//
// Each message's own is what a caller deciding about ONE event has to read,
// and the two differ the moment a partition's messages sit at different
// counts. Reading the partition's where this one is meant is a defect with a
// name: the sandbox answer route refused a reply with a whole budget in hand
// because a co-partitioned message was near its own — see
// internal/sandbox.MayOfferAnswer.
//
// Folded in the CONTRACT rather than in each backend, and asserted here for
// the same reason: two backends reading their own counters must not become
// two rules.
func TestAPartitionReportsItsNearestMessagesHeadroom(t *testing.T) {
	t.Parallel()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	for name, tc := range map[string]struct {
		perMessage []queue.Headroom
		want       int
		wantKnown  bool
		wantEach   map[uuid.UUID]int
	}{
		"one message": {
			perMessage: []queue.Headroom{{ID: a, Left: 7}},
			want:       7, wantKnown: true,
			wantEach: map[uuid.UUID]int{a: 7},
		},
		"the smallest of many": {
			perMessage: []queue.Headroom{{ID: a, Left: 9}, {ID: b, Left: 2}, {ID: c, Left: 24}},
			want:       2, wantKnown: true,
			// AND EACH KEEPS ITS OWN. The whole point of stating both:
			// the fresh message in this partition has 24 deliveries in
			// hand and must not be read as having 2.
			wantEach: map[uuid.UUID]int{a: 9, b: 2, c: 24},
		},
		"smallest arriving last": {
			perMessage: []queue.Headroom{{ID: a, Left: 24}, {ID: b, Left: 9}, {ID: c, Left: 0}},
			want:       0, wantKnown: true,
			wantEach: map[uuid.UUID]int{a: 24, b: 9, c: 0},
		},
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
		"past the budget": {
			perMessage: []queue.Headroom{{ID: a, Left: -3}},
			want:       0, wantKnown: true,
			wantEach: map[uuid.UUID]int{a: 0},
		},
		// A REPEATED ID KEEPS ITS SMALLEST, so the answer cannot depend
		// on the order a backend happened to walk its messages in.
		"one id stated twice": {
			perMessage: []queue.Headroom{{ID: a, Left: 11}, {ID: a, Left: 4}, {ID: b, Left: 20}},
			want:       4, wantKnown: true,
			wantEach: map[uuid.UUID]int{a: 4, b: 20},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := queue.WithHeadroom(context.Background(), tc.perMessage)
			got, known := queue.DeliveriesLeft(ctx)
			if got != tc.want || known != tc.wantKnown {
				t.Errorf("the partition reported (%d, %v), want (%d, %v)",
					got, known, tc.want, tc.wantKnown)
			}
			for id, want := range tc.wantEach {
				left, stated := queue.DeliveriesLeftFor(ctx, id)
				if !stated || left != want {
					t.Errorf("message %s reported (%d, %v), want (%d, true)",
						id, left, stated, want)
				}
			}
		})
	}
}

// AND AN UNSTATED COUNT IS UNSTATED, never a zero — on both readers.
//
// A handler reached outside a queue — a test, a direct call, a backend whose
// metadata would not parse — must be able to tell "this message has no
// deliveries left" from "nobody said". Collapsing the two is what a plain int
// with a zero value would do, and the caller this exists for stops offering
// work at a low count.
func TestAnUnstatedDeliveryCountIsNotAZero(t *testing.T) {
	t.Parallel()
	id := uuid.New()

	if left, known := queue.DeliveriesLeft(context.Background()); known {
		t.Errorf("a bare context reported %d deliveries left", left)
	}
	if left, known := queue.DeliveriesLeftFor(context.Background(), id); known {
		t.Errorf("a bare context reported %d deliveries left for a message", left)
	}

	ctx := queue.WithHeadroom(context.Background(), []queue.Headroom{{ID: id, Left: 0}})
	if left, known := queue.DeliveriesLeft(ctx); !known || left != 0 {
		t.Errorf("a stated zero read back as (%d, %v), want (0, true)", left, known)
	}
	if left, known := queue.DeliveriesLeftFor(ctx, id); !known || left != 0 {
		t.Errorf("a stated zero read back as (%d, %v) for its own message, "+
			"want (0, true)", left, known)
	}

	// AN ID THAT WAS NOT PART OF THIS DELIVERY is unstated too, and must
	// not inherit the partition's number: a caller asking about a message
	// this handler was never handed is asking about nothing.
	if left, known := queue.DeliveriesLeftFor(ctx, uuid.New()); known {
		t.Errorf("an id outside the delivery reported %d deliveries left", left)
	}

	// A message a backend could NOT read is left out of the list, so it has
	// no answer of its own while its siblings keep theirs.
	unreadable := uuid.New()
	ctx = queue.WithHeadroom(context.Background(), []queue.Headroom{{ID: id, Left: 6}})
	if left, known := queue.DeliveriesLeftFor(ctx, unreadable); known {
		t.Errorf("a message nothing was stated for reported %d deliveries left", left)
	}
	if left, known := queue.DeliveriesLeft(ctx); !known || left != 6 {
		t.Errorf("the partition reported (%d, %v) beside an unreadable message, "+
			"want (6, true): an unreadable one contributes nothing rather than a zero",
			left, known)
	}
}
