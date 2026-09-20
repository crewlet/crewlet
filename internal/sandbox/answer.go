package sandbox

import (
	"context"

	"github.com/crewlet/crewlet/internal/events"
)

// AnswerDisposition is what the engine must DO with a delivery it offered to a
// parked coding run — never what happened inside the coordinator.
//
// The offer's caller is holding somebody's chat message with one question:
// this arrived, what do I do with it. It was a bool beside an error, and those
// two values carried FOUR outcomes — nothing matched, the answer resumed the
// run, the claim was taken and given back, the store could not say whether the
// claim landed — which the dispatcher collapsed into two by reading any error
// as "not handled". So the delivery fell through to the ordinary route and was
// consumed as an unrelated turn ON THE VERY MESSAGE THAT CARRIED THE ANSWER,
// while the run that asked the question waited out its pause TTL for a further
// message that may never come. It was worst on [ErrResumeUnavailable], whose
// whole purpose is "send this to a node that can read it": the message was
// spent here instead.
//
// THREE ANSWERS, each mapped onto an action the inbox already has, and what
// each costs when it is chosen wrongly:
//
//   - [AnswerConsumed] — ack, run no turn. Wrong here, the person's message is
//     dropped outright: no turn runs, no run was resumed, and nothing ever
//     tells them so.
//   - [AnswerDeferred] — requeue the delivery ([inbox.ActionPark]: republish,
//     then ack), so this node or the seat's next owner offers it again. Wrong
//     here, the message circles the seat's own inbox instead of being worked —
//     which is why it is bounded; see [MaxAnswerAttempts].
//   - [AnswerNotMine] — fall through to the ordinary route and let it be the
//     turn it looks like. Wrong here, the answer is SPENT on an unrelated turn
//     while a coding run is still owed it, which is the defect this type
//     exists to end.
//
// The asymmetry decides every ambiguous case: an answer that arrives twice is
// recoverable, and an answer that is spent is not — so where the coordinator
// cannot establish that nothing is owed the delivery, it defers.
//
// NOT [inbox.Action] itself. This package must not depend on the inbox's
// guard vocabulary to say what it knows, and the two answer different
// questions: an Action is the screening's decision about a SEAT, this is the
// coordinator's decision about one delivery's relationship to one parked run.
// The mapping between them belongs to the dispatcher, which is the frame that
// holds both.
type AnswerDisposition string

const (
	// AnswerConsumed — the delivery WAS the answer and is spent: the
	// suspended turn was resumed with it, or another inbound had already
	// claimed the run it answers. Either way no turn runs on it.
	AnswerConsumed AnswerDisposition = "consumed"

	// AnswerDeferred — a run was matched and the answer is STILL OWED to
	// it: the claim was taken and given back, or the store could not say
	// whether it was taken at all. The delivery has to come back.
	AnswerDeferred AnswerDisposition = "deferred"

	// AnswerNotMine — nothing here is owed this delivery: no run was
	// awaiting the conversation, the lookup failed open, or the run it
	// matched is terminally gone — settled, deleted, unresumable for good.
	// It is an ordinary message and is handled as one.
	AnswerNotMine AnswerDisposition = "not_mine"
)

func (d AnswerDisposition) String() string { return string(d) }

// Valid reports whether d is one of the three this package defines.
//
// EMPTY IS NOT VALID: the zero value is what a seam that answered nothing
// returns, and a delivery's disposition is exactly the thing no caller may
// guess at. What a caller does with an invalid one is the caller's own rule —
// see [Dispatcher.answered], which falls back to the behaviour of a node with
// no coordinator at all rather than to a requeue nothing would bound.
func (d AnswerDisposition) Valid() bool {
	switch d {
	case AnswerConsumed, AnswerDeferred, AnswerNotMine:
		return true
	default:
		return false
	}
}

// MaxAnswerAttempts is how many times ONE delivery is handed to ONE parked
// run before this node stops requeueing it.
//
// It counts ATTEMPTS, not requeues, so the last of them is not requeued: one
// message reaches a run at most this many times, which is exactly what the
// broker’s own budget means by 25 deliveries of one message.
//
// A REQUEUE IS NOT A REDELIVERY, which is what makes this constant necessary.
// The park path republishes the event onto the seat's own inbox and acks the
// original ([Engine.park]), so the copy is a NEW message: the broker's
// delivery budget — 25, in internal/queue/jetstream — counts deliveries of ONE
// message and starts again on the republish. Nothing else bounds the loop — a run
// parked on a question stays matchable for ever, since the pause reaper moves
// it to [StatusReseed], which is still [Awaiting] — so a resume that fails the
// same way every time would circle the inbox at whatever rate the broker will
// serve, for the life of the process.
//
// 25 IS THAT SAME BUDGET, chosen so the two routes into one resume tolerate an
// identical failure identically: a completion that cannot be resumed NAKs and
// is redelivered until the broker's budget is spent, and an answer that cannot
// be resumed is requeued until this one is. A number of its own would have
// been a second opinion about how many times a failing resume is worth
// retrying, and the two would drift.
//
// PER NODE AND PER PROCESS, deliberately: every failure that reaches here is a
// statement about THIS node — no resumer, a suspended conversation this build
// cannot decode, a seat that is not in this node's company — so a restart or a
// seat handoff is exactly the event that makes a further attempt worth making,
// and both reset the count. It is also why the run is NOT settled when the
// budget is spent: the turn is still resumable somewhere, so this node hands
// the delivery back to the ordinary route rather than destroying work a peer
// or a later build could still finish.
const MaxAnswerAttempts = 25

// answerAttempt is this node's count of failed handoffs of one delivery to one
// parked run.
//
// ONE SLOT PER RUN, keyed by turn id, holding the delivery it is counting: a
// different message is a new attempt at the same question and gets its own
// budget, so the count resets rather than accumulating across the several
// replies a person may send. That also bounds the map by the number of parked
// runs this node has, rather than by every message their conversations ever
// carried.
type answerAttempt struct {
	// handle is the seat the run belongs to, so [Coordinator.ReleaseSeat]
	// can drop what a seat handed on leaves behind.
	handle string

	// delivery is the event id the failures are counted against. Empty is a
	// legitimate value — a caller with no trigger to name — and it simply
	// shares one slot, which bounds more tightly rather than less.
	delivery string

	failures int
}

// deliveryOf names the message an offer is carrying, for the attempt count.
func deliveryOf(trigger *events.Event) string {
	if trigger == nil {
		return ""
	}
	return trigger.ID.String()
}

// deferAnswer records one failed handoff and reports what the caller must do
// with the delivery now.
//
// [AnswerDeferred] while the budget holds, and [AnswerNotMine] once it is
// spent — the point at which this node stops offering a message to a run it
// cannot resume and lets it be the ordinary message it looks like. The run is
// left exactly where the revert put it: see [MaxAnswerAttempts] for why a
// spent budget does not end it.
//
// The error travels either way, because it is the explanation and never the
// decision: the caller logs it and acts on the disposition.
func (c *Coordinator) deferAnswer(ctx context.Context, run PendingRun, trigger *events.Event, cause error) (AnswerDisposition, error) {
	delivery := deliveryOf(trigger)
	left := c.spendAnswerAttempt(run.AgentHandle, run.TurnID, delivery)
	if left > 0 {
		return AnswerDeferred, cause
	}
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	log.ErrorContext(ctx, "sandbox_answer_requeue_exhausted",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "delivery", delivery,
		"attempts", MaxAnswerAttempts, "error", detail,
		"detail", "this node could not hand this message to the coding run that asked, "+
			"in every one of its attempts, so the message is run as the ordinary "+
			"message it looks like; the run stays parked on its question and its box "+
			"is bounded by pause_ttl_seconds")
	// THE COUNT STAYS, so this delivery is spent for good rather than
	// starting a second budget if another copy of it reaches this node — a
	// partial requeue leaves same-id copies behind, and the whole point of
	// the bound is that ONE message gets one budget. A different message
	// resets it, and the entry goes when the run ends or the seat does.
	return AnswerNotMine, cause
}

// spendAnswerAttempt charges one failed handoff and reports how many are left.
//
// Under the same lock as the seat counts, because both are read on the hot
// path of a delivery and a second mutex would be a second thing to order.
func (c *Coordinator) spendAnswerAttempt(handle, turnID, delivery string) (left int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.attempts[turnID]
	if at.delivery != delivery {
		// A DIFFERENT MESSAGE, so the budget starts again: this is a fresh
		// attempt at the same question rather than another go at the same
		// answer, and carrying the old count over would spend a new
		// reply's chances on the last one's failures.
		at = answerAttempt{handle: handle, delivery: delivery}
	}
	at.handle = handle
	at.failures++
	c.attempts[turnID] = at
	return max(MaxAnswerAttempts-at.failures, 0)
}

// clearAnswerAttempts forgets a run's count, which every outcome but a defer
// does: the delivery is either spent or handed on, and the next failure is the
// first of its own series.
func (c *Coordinator) clearAnswerAttempts(turnID string) {
	if turnID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.attempts, turnID)
}

// releaseAnswerAttempts drops what a seat's runs were counting, for a seat
// this node no longer holds.
//
// A LINEAR SWEEP over a map holding one entry per parked run this node failed
// to resume, which is a handful at the very most; an index from seat to run
// would be a second structure to keep true for a scan that costs nothing.
func (c *Coordinator) releaseAnswerAttempts(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for turnID, at := range c.attempts {
		if at.handle == handle {
			delete(c.attempts, turnID)
		}
	}
}
