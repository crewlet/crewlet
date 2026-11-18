package queue_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue"
)

// TWO NUMBERS, TWO QUESTIONS, and a handler must be able to ask either. The
// partition's is its NEAREST message's, since a hand-back spends a delivery of
// every message: a fold taking the largest or the first reports headroom the
// caller lacks. Each message's own is what a caller deciding about ONE event
// reads (internal/sandbox.MayOfferAnswer), and differs whenever counts do.
// Folded in the contract, so two backends cannot become two rules.
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
			// AND EACH KEEPS ITS OWN: the fresh message has 24 in hand,
			// not the partition's 2.
			wantEach: map[uuid.UUID]int{a: 9, b: 2, c: 24},
		},
		"smallest arriving last": {
			perMessage: []queue.Headroom{{ID: a, Left: 24}, {ID: b, Left: 9}, {ID: c, Left: 0}},
			want:       0, wantKnown: true,
			wantEach: map[uuid.UUID]int{a: 24, b: 9, c: 0},
		},
		// NOTHING READABLE reports the absence, not a zero: a caller told
		// "no deliveries left" stops offering work it could still do.
		"nothing readable": {perMessage: nil, want: 0, wantKnown: false},
		// A COUNT PAST THE BUDGET IS SPENT, NOT NEGATIVE: a negative
		// would compare below every reserve and read as "plenty".
		"past the budget": {
			perMessage: []queue.Headroom{{ID: a, Left: -3}},
			want:       0, wantKnown: true,
			wantEach: map[uuid.UUID]int{a: 0},
		},
		// A REPEATED ID KEEPS ITS SMALLEST, so walk order cannot matter.
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

// AND AN UNSTATED COUNT IS UNSTATED, never a zero, on both readers: a handler
// outside a queue, or one whose metadata would not parse, must tell "no
// deliveries left" from "nobody said", since its caller stops offering work at
// a low count.
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

	// AN ID THAT WAS NOT PART OF THIS DELIVERY is unstated too, never the
	// partition's number.
	if left, known := queue.DeliveriesLeftFor(ctx, uuid.New()); known {
		t.Errorf("an id outside the delivery reported %d deliveries left", left)
	}

	// A message a backend could NOT read is absent from the list: no answer
	// of its own, while its siblings keep theirs.
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
