package queue

import "context"

// deliveriesLeftKey is the private context key the value below travels under.
type deliveriesLeftKey struct{}

// WithDeliveriesLeft states, for one handler invocation, how many FURTHER
// deliveries the message being handled still has before the backend
// dead-letters it.
//
// ONLY A BACKEND CALLS THIS, on the delivery it is about to hand over. It is
// part of the contract rather than a backend's private courtesy because both
// backends have to answer it identically — see [DeliveriesLeft] for what the
// number means and what it is for.
func WithDeliveriesLeft(ctx context.Context, left int) context.Context {
	if left < 0 {
		left = 0
	}
	return context.WithValue(ctx, deliveriesLeftKey{}, left)
}

// DeliveriesLeft reports how many further times this delivery can be handed
// back before the backend dead-letters it, and whether the transport said.
//
// # What the number counts
//
// It is the headroom AFTER the delivery in hand: 0 means the next hand-back
// dead-letters the message, 1 means it comes back exactly once more. A Nak, a
// Defer and any other return that puts a message back all spend one of them,
// because on the broker this engine ships there is no free handoff — see
// internal/queue/jetstream, whose delivery budget is sized for handoffs for
// exactly that reason.
//
// # Why the count and not the budget
//
// A caller must not do the subtraction itself. The budget is a BACKEND's
// number and a configurable one at that (jetstream Config.MaxDeliver, the
// twin's WithMaxRedeliveries), and the two backends count it in different
// conventions — deliveries including the first on one, redeliveries after it
// on the other. A layer above that reasoned about "the broker's 25" would be
// reasoning about a number it cannot see and a convention nothing states to
// it, which is precisely the claim this value exists to replace: the sandbox
// answer route's own attempt budget was documented as staying "well under the
// broker's 25-delivery dead-letter budget" while being counted PER PROCESS,
// so two handoffs of it walked straight through a budget the message carries
// and dead-lettered a person's reply. See internal/sandbox.MaxAnswerAttempts.
//
// # What a partition reports
//
// The SMALLEST of its messages' — see [LeastDeliveriesLeft]. A batch handler
// returns one outcome for the whole partition, so a hand-back spends a
// delivery of every message in it, and the one nearest its budget is the one
// that would dead-letter first.
//
// # An absent value
//
// false means the transport did not state it: a message whose metadata could
// not be read, or a caller that is not running under a queue handler at all
// (a test, a direct call). A BOOL rather than an error because there is no
// failure to report and nothing for a caller to act on beyond the absence
// itself — the value was either carried or it was not. What to do about it is
// the caller's own rule: a caller bounding work on the headroom should treat
// an absent value as the state it was in before this existed, because a
// missing number is not evidence of a spent budget.
func DeliveriesLeft(ctx context.Context) (int, bool) {
	left, ok := ctx.Value(deliveriesLeftKey{}).(int)
	return left, ok
}

// LeastDeliveriesLeft folds a partition's per-message headroom into the one
// number its handler is told.
//
// The SMALLEST, because a batch handler's outcome covers the whole partition:
// a hand-back returns every message in it, so the message nearest its budget
// is the one that dead-letters first and therefore the one the partition's
// headroom is.
//
// Written here rather than in each backend because both must fold it the same
// way — a twin that reported the largest would certify a bound production
// does not have. Callers pass only the counts they could READ: a message whose
// metadata would not parse contributes nothing, so a partition of nothing
// readable reports false and one with a readable message reports the smallest
// of those. That is the best available answer rather than a complete one, and
// it is the safe direction to be wrong in — an unreadable message might be
// nearer its budget than any of these, and a caller told nothing at all would
// instead read the absence as "no bound to respect".
func LeastDeliveriesLeft(perMessage []int) (int, bool) {
	least, known := 0, false
	for _, left := range perMessage {
		if !known || left < least {
			least, known = left, true
		}
	}
	if least < 0 {
		least = 0
	}
	return least, known
}
