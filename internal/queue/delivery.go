package queue

import (
	"context"

	"github.com/google/uuid"
)

type headroomKey struct{}

// Headroom is ONE message's remaining deliveries, as the backend handing it
// over states them. Keyed on the event's id: a handler holds decoded events,
// never transport messages, so the id is all both sides can name.
type Headroom struct {
	// ID is the event this count belongs to.
	ID uuid.UUID

	// Left is how many FURTHER deliveries remain before the backend
	// dead-letters it: 0 means the next hand-back dead-letters it.
	Left int
}

// headroom is one handler invocation's per-message and partition counts, both
// derived from the one list [WithHeadroom] is given so they cannot disagree.
type headroom struct {
	least      int
	leastKnown bool
	perMessage map[uuid.UUID]int
}

// WithHeadroom states, for one handler invocation, how many FURTHER
// deliveries each message being handled still has before the backend
// dead-letters it. Only a backend calls it, on the delivery it hands over.
//
// A backend states only what it reads off each message; the partition's
// number ([DeliveriesLeft]) is folded HERE, so no backend folds it wrongly or
// inconsistently. A message whose metadata would not parse is absent: no
// answer of its own, nothing in the partition's. A repeated id keeps its
// SMALLEST count, so walk order cannot matter. A count past the budget (an unkept dead letter re-offered, see jetstream's
// keepRetries, or a shrunk MaxDeliver) is CLAMPED at zero: a negative would
// read as plenty to a caller comparing against a reserve.
func WithHeadroom(ctx context.Context, perMessage []Headroom) context.Context {
	if len(perMessage) == 0 {
		return ctx
	}
	h := headroom{perMessage: make(map[uuid.UUID]int, len(perMessage))}
	for _, m := range perMessage {
		left := max(m.Left, 0)
		if prior, seen := h.perMessage[m.ID]; seen && prior < left {
			left = prior
		}
		h.perMessage[m.ID] = left
		if !h.leastKnown || left < h.least {
			h.least, h.leastKnown = left, true
		}
	}
	return context.WithValue(ctx, headroomKey{}, h)
}

// DeliveriesLeft reports how many further times THE PARTITION in hand can be
// handed back before the backend dead-letters one of its messages (0: the next
// hand-back does), and whether the transport said. It is the SMALLEST count,
// since a hand-back spends every message's delivery: it answers "will this
// dead-letter something", never "can I retry THIS message"
// ([DeliveriesLeftFor]). Every putting-back return spends one, including an
// undispatched partition a blocked batch loop returns (certified by
// Batch/an_undispatched_partition_pays_for_its_hand_back). Use this count,
// never a subtraction from a backend's budget, whose convention differs per
// backend (see internal/sandbox.MaxAnswerAttempts). false means not stated
// (unreadable metadata, or no queue handler): a missing number is not evidence
// of a spent budget.
func DeliveriesLeft(ctx context.Context) (int, bool) {
	h, ok := ctx.Value(headroomKey{}).(headroom)
	if !ok || !h.leastKnown {
		return 0, false
	}
	return h.least, true
}

// DeliveriesLeftFor reports how many further times ONE MESSAGE of the
// delivery in hand can be handed back before the backend dead-letters IT, and
// whether the transport said: the question a caller deciding about one event
// asks. It differs from [DeliveriesLeft] whenever batch-mates sit at different
// counts (a fresh reply beside a much-retried message), and reading it where
// the partition's is meant hands a batch back that dead-letters its nearest
// message; no fold answers both.
//
// false as for [DeliveriesLeft], or for an id not in this delivery; the rule
// is the same: a missing number is not evidence of a spent budget.
func DeliveriesLeftFor(ctx context.Context, id uuid.UUID) (int, bool) {
	h, ok := ctx.Value(headroomKey{}).(headroom)
	if !ok {
		return 0, false
	}
	left, stated := h.perMessage[id]
	return left, stated
}
