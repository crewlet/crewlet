package queue

import (
	"context"

	"github.com/google/uuid"
)

// headroomKey is the private context key the value below travels under.
type headroomKey struct{}

// Headroom is ONE message's remaining deliveries, as the backend handing it
// over states them.
//
// Keyed on the EVENT's own id because that is what a handler holds: a handler
// is given decoded events, never the transport's messages, so an id is the
// only thing the two sides can both name.
type Headroom struct {
	// ID is the event this count belongs to.
	ID uuid.UUID

	// Left is how many FURTHER deliveries this message has before the
	// backend dead-letters it: 0 means the next hand-back dead-letters it,
	// 1 means it comes back exactly once more.
	Left int
}

// headroom is everything one handler invocation is told about its delivery:
// each message's own count, and the partition's.
//
// Both are derived HERE, from the one list a backend states, so the two can
// never disagree and no backend folds anything of its own — see
// [WithHeadroom].
type headroom struct {
	least      int
	leastKnown bool
	perMessage map[uuid.UUID]int
}

// WithHeadroom states, for one handler invocation, how many FURTHER
// deliveries each message being handled still has before the backend
// dead-letters it.
//
// ONLY A BACKEND CALLS THIS, on the delivery it is about to hand over. It is
// part of the contract rather than a backend's private courtesy because both
// backends have to answer it identically — see [DeliveriesLeft] and
// [DeliveriesLeftFor] for what the two numbers mean and what each is for.
//
// ONE LIST IN, TWO NUMBERS OUT, and that shape is the point: a backend states
// what it can READ off each message and nothing else. The partition's number
// is folded here rather than in each backend, because a twin that folded the
// LARGEST would certify a bound production does not have — and because a
// backend that stated the two separately could state them inconsistently,
// which is a disagreement nothing outside this function could ever catch.
//
// A MESSAGE A BACKEND COULD NOT READ IS SIMPLY ABSENT from the list: its
// metadata would not parse, so there is no count to state. It then has no
// per-message answer, and it contributes nothing to the partition's. That is
// the best available answer rather than a complete one, and it is the safe
// direction to be wrong in on both counts — see the two readers.
//
// A REPEATED ID KEEPS ITS SMALLEST count. Nothing in this engine partitions
// two copies of one event together on purpose, but an at-least-once transport
// can deliver one twice, and the alternative — last-write-wins — would make
// the answer depend on the order a backend happened to walk its messages in.
//
// A count past the budget is CLAMPED at zero. An operator who shrinks
// MaxDeliver under a backlog produces a message delivered more times than its
// budget allows, and a negative number compares as less than every reserve a
// caller holds and therefore reads as plenty.
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
// handed back before the backend dead-letters one of its messages, and
// whether the transport said.
//
// # There are two numbers, and they answer different questions
//
// This one is the SMALLEST of the partition's messages' — the message nearest
// its budget. A batch handler returns one outcome for the whole partition, so
// a hand-back spends a delivery of every message in it and the nearest is the
// one that dead-letters first. So this is the answer to "will handing this
// batch back dead-letter SOMETHING", and it is the only honest answer to it.
//
// It is NOT the answer to "can I afford one more attempt at THIS message",
// and reading it as one is a mistake with a name: the engine's sandbox answer
// route gated its offer on this number, so a person's clarification reply on
// its FIRST delivery — a whole budget in hand — was refused the answer route
// outright whenever any co-partitioned message happened to be near its own
// budget, and was spent on an ordinary turn while a parked coding run was
// still owed it. That question is [DeliveriesLeftFor]'s.
//
// # What the number counts
//
// It is the headroom AFTER the delivery in hand: 0 means the next hand-back
// dead-letters the message, 1 means it comes back exactly once more. A Nak, a
// Defer and any other return that puts a message back all spend one of them,
// because no backend here has a free handoff — see internal/queue/jetstream,
// whose delivery budget is sized for handoffs for exactly that reason, and
// internal/queue/memory, whose twin spends one for the same reason the broker
// does rather than modelling a cheaper broker nobody runs.
//
// "ANY OTHER RETURN" INCLUDES THE ONE NO HANDLER ASKED FOR. A batch loop that
// finds itself blocked between partitions — a deferral it just applied, a
// hold, a pause, a detach — hands the partitions it never dispatched back, and
// those pay a delivery each as well. They were drained, which on a fetching
// broker is already the delivery; a backend that returned them uncounted would
// charge one partition of a drain the broker's price and the rest of it
// nothing. Certified by
// Batch/an_undispatched_partition_pays_for_its_hand_back.
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
// # An absent value
//
// false means the transport did not state it: a partition whose metadata
// could not be read for any of its messages, or a caller that is not running
// under a queue handler at all (a test, a direct call). A BOOL rather than an
// error because there is no failure to report and nothing for a caller to act
// on beyond the absence itself — the value was either carried or it was not.
// What to do about it is the caller's own rule: a caller bounding work on the
// headroom should treat an absent value as the state it was in before this
// existed, because a missing number is not evidence of a spent budget.
func DeliveriesLeft(ctx context.Context) (int, bool) {
	h, ok := ctx.Value(headroomKey{}).(headroom)
	if !ok || !h.leastKnown {
		return 0, false
	}
	return h.least, true
}

// DeliveriesLeftFor reports how many further times ONE MESSAGE of the
// delivery in hand can be handed back before the backend dead-letters IT, and
// whether the transport said.
//
// # The other question
//
// [DeliveriesLeft] answers "will handing this batch back dead-letter
// something". This answers "how much is left of THIS message", which is what
// a caller deciding what to do with one particular event has to ask — and the
// two are different numbers the moment a partition's messages sit at
// different delivery counts, which is ordinary: a conversation whose first
// message has been handed back a dozen times keeps collecting fresh replies,
// and each of those arrives with a whole budget.
//
// A caller that reads the partition's number where it means this one is
// bounded by the WORST message it happens to be batched with, which is how
// the sandbox answer route came to refuse a reply that had a whole budget in
// hand. A caller that reads this one where it means the partition's will hand
// a batch back believing it costs nothing, and dead-letter the message
// nearest its budget. There is no fold that answers both, which is why both
// are stated rather than one being derived by whoever needs the other.
//
// # An id nothing was stated for
//
// false, for the same three reasons the partition's number can be absent —
// nothing was stated at all, this message's metadata would not parse, or the
// caller is not under a queue handler — plus one of its own: an id that was
// not part of this delivery. None of them is distinguishable to a caller and
// none needs to be: the rule is the same either way, that a missing number is
// not evidence of a spent budget.
func DeliveriesLeftFor(ctx context.Context, id uuid.UUID) (int, bool) {
	h, ok := ctx.Value(headroomKey{}).(headroom)
	if !ok {
		return 0, false
	}
	left, stated := h.perMessage[id]
	return left, stated
}
