package sandbox

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// AnswerDisposition is what the engine must DO with a delivery it offered to a
// parked executor, never what happened inside the coordinator: a bool beside an
// error carried four outcomes, and a caller reading any error as "not handled"
// spent the answer on an unrelated turn. Each constant names its action and what
// choosing it wrongly costs. The asymmetry decides every ambiguous case: an
// answer that arrives twice is recoverable and one that is spent is not, so
// where the coordinator cannot establish that nothing is owed the delivery, it
// defers.
//
// Not [inbox.Action]: that is the screening's decision about a SEAT, this the
// coordinator's about one delivery and one parked run, and this package must not
// depend on the inbox's vocabulary. The dispatcher maps one onto the other.
type AnswerDisposition string

const (
	// AnswerConsumed — ack and run no turn: the delivery WAS the answer and is
	// spent. It is RECORDED on the run it answers, which owns its resume from
	// here ([RecordedAnswer]); or another inbound had already claimed that run;
	// or it is a too-large reply whose refusal a turn kept ([AnswerRefused]).
	// Chosen wrongly, the person's message is dropped and nobody is told, so
	// nothing answers it before the record has landed.
	AnswerConsumed AnswerDisposition = "consumed"

	// AnswerDeferred — hand the delivery back by DEFERRAL: a run was matched and
	// the answer is STILL OWED (the claim was taken and given back, or the store
	// could not say whether it was taken). Deferral keeps its place at the
	// inbox's head, where a Nak would put it behind the person's next message.
	// Chosen wrongly, it circles the seat's inbox instead of being worked:
	// [MaxAnswerAttempts] bounds it.
	AnswerDeferred AnswerDisposition = "deferred"

	// AnswerNotMine — fall through to the ordinary route: nothing here is owed
	// this delivery. No run awaited the conversation; the lookup failed and this
	// seat has no awaiting run; or, for an answer BY TURN, its run is gone, no
	// longer waiting on the question, or let this answer go (a chat reply is
	// then an ordinary message, an answer by turn is spent). Chosen wrongly, the
	// answer is SPENT on an unrelated turn while an executor is still owed it.
	// An answer RECORDED on a run is never this: an ending that reaches it first
	// hands it back through the run's row ([PendingStore.OweHandBack]), and the
	// delivery is spent.
	AnswerNotMine AnswerDisposition = "not_mine"

	// AnswerRefused — run no turn, and hand the delivery back by DEFERRAL,
	// UNSPENT: it is a chat reply too large to be the answer a run is parked on
	// ([ErrAnswerTooLarge]), so the question's REFUSAL is recorded on the run
	// ([PendingStore.RecordRefusal]), owing it a resume saying the reply did not
	// reach it; while owed, the question takes nothing written after it
	// (ADR-0114). The row holds none of the reply, so the delivery, its only
	// copy, waits held at the inbox's head until the refusal leaves the run. A
	// turn that keeps the refusal records it worked ([Coordinator.spendRefused])
	// and it is dropped when it comes round; a refusal let go of returns it to
	// the seat as an ordinary message. Acked, the reply is lost; run as a turn,
	// the seat answers a reply its run was owed. An answer by turn past the
	// bound is [AnswerNotMine], its author told by its tool.
	AnswerRefused AnswerDisposition = "refused"
)

func (d AnswerDisposition) String() string { return string(d) }

// Valid reports whether d is one of the four this package defines. Empty is not:
// it is what a seam that answered nothing returns, and no caller may guess a
// delivery's disposition. What a caller does with an invalid one is its own rule
// ([Dispatcher.answered] acts as a node with no coordinator rather than requeue
// without bound).
func (d AnswerDisposition) Valid() bool {
	switch d {
	case AnswerConsumed, AnswerDeferred, AnswerNotMine, AnswerRefused:
		return true
	default:
		return false
	}
}

// MaxAnswerTextBytes bounds an answer BY TURN, measured trimmed and BEFORE
// redaction, the one measure both ends take: `answer_executor`
// (internal/agent/builtin) refuses past it before delivery, and
// [Coordinator.AnswerByTurn] holds any other answer to it where it records, so an
// answer the source accepted is never refused after ([redact.Secrets]'s marker
// outgrows many a secret, so measuring after redaction would). It is the page
// [MaxAskContextBytes] holds the question to: the answer is sealed onto the run's
// row, which every status flip and listing reads whole, then becomes the run's
// prompt, re-sent and charged at every later step. Past a page is a document, for
// a page or the work item. Not a chat reply's bound ([MaxAnswerBytes],
// [AnswerRefused]), nor internal/agent/builtin's ToolAnswerBytes.
const MaxAnswerTextBytes = MaxAskContextBytes

// MaxAnswerAttempts is how many times ONE PROCESS tries to hand one answer to ONE
// parked run before letting it go as the ordinary message it looks like: a
// RECORDED answer's resume, retried ([Coordinator.owedFailed]) on
// [answerRetryDelay] then declined ([Coordinator.declineAnswer]), and a delivery
// the store could not record, deferred then run ([Coordinator.deferAnswer]). It
// counts attempts, so the last is not handed back. Unbounded, a parked run stays
// matchable for ever ([StatusReseed] is still [Awaiting]) and a failing resume
// holds the seat's inbox. Ten span about 2.5 min at the queue's backoff
// ([answerRetrySeed]): a seat lease TTL (45 s), a config reconcile, a store
// blip. PER NODE AND PROCESS, since each such failure is this node's: a restart
// or handoff resets it, and the run stays parked for a peer. The broker's
// budget is [AnswerDeliveryReserve]'s, and time is [answerWindow]'s.
const MaxAnswerAttempts = 10

// AnswerDeliveryReserve is how many of a message's remaining deliveries are left
// to the ordinary route: a delivery within this many of the backend's dead-letter
// budget is no longer offered to a parked run. It is measured per MESSAGE
// (queue.DeliveriesLeftFor), which survives a seat move or restart as no
// per-process bound does, never per partition ([MayOfferAnswer]). Five is what
// the ordinary route then spends on the same reply: a seat handoff's deferral (a
// Nak on this broker; one or two while seats move), a partition that would not
// merge or a failed park requeue (one each), and the turn's own retries. At the
// 30 s backoff ceiling they span about two minutes, the transients
// [MaxAnswerAttempts] is sized for. Not a fraction of the broker's configurable
// budget: an operator shrinking it must not shrink the reserve.
const AnswerDeliveryReserve = 5

// AnswerHeadroom is what the transport said about ONE message of a delivery: the
// pair queue.DeliveriesLeftFor returns, as a value so [MayOfferAnswer] is
// exercisable without a queue, a broker or a partition.
type AnswerHeadroom struct {
	// Left is how many further deliveries this message has before the
	// transport dead-letters it.
	Left int

	// Known is whether the transport stated Left at all. FALSE IS NOT A ZERO:
	// see [MayOfferAnswer].
	Known bool
}

// MayOfferAnswer reports whether a delivery may still be offered to a parked
// executor: whether ANY of its messages has more than [AnswerDeliveryReserve]
// deliveries left, since a fresh reply arrives with a whole budget beside a
// sibling handed back a dozen times. The smallest ([queue.DeliveriesLeft])
// would refuse every later reply on that conversation for good, each spent on an
// ordinary turn. The trade: a hand-back returns the whole partition and can
// dead-letter a sibling, but the ordinary route risks the same, and a dead-letter
// is LOUD where a spent reply is SILENT; the dispatcher logs when the partition's
// own number is inside the reserve. An unstated count or an empty list is not a
// spent one (read so, a node with unreadable metadata spends every reply on a
// turn): [MaxAnswerAttempts] and [answerWindow] alone bound the route. Here, not
// in the dispatcher, so the rule sits with its number and is testable alone.
func MayOfferAnswer(perMessage []AnswerHeadroom) bool {
	if len(perMessage) == 0 {
		return true
	}
	for _, h := range perMessage {
		if !h.Known || h.Left > AnswerDeliveryReserve {
			return true
		}
	}
	return false
}

// maxAnswerDeliveries is how many of one run's deliveries this node keeps a
// budget for at once, bounding a table kept per (run, delivery). A parked run has
// one open question, and what is in hand-back for it is the replies sent during
// one store outage, a handful; four caps a run at four budgets of
// [MaxAnswerAttempts]. A full table drops SPENT entries (their delivery already
// went to the ordinary route, so a late copy costs one series) but never evicts
// a LIVE one: the new delivery is refused a budget and handled as an ordinary
// message, since live budgets evicting each other would loop for ever.
const maxAnswerDeliveries = 4

// answerKey names what a delivery is owed to. The handle rides beside the turn id
// so [Coordinator.releaseAnswerAttempts] can drop a seat's entries by scanning
// keys, with no index from seat to run.
type answerKey struct {
	handle string
	turnID string
}

// answerBudget is one delivery's series of failed handoffs to one run. One per
// (run, delivery): a single slot per run resets on each different message, so
// two replies circling one run would reset each other and never reach the bound.
type answerBudget struct {
	// failures is how many handoffs of this delivery have failed.
	failures int

	// first is when the first of them failed: [answerWindow] runs from it.
	first time.Time
}

// live reports whether this delivery is still worth handing back: within the
// attempt ceiling, and within the run's own awaiting window.
func (b answerBudget) live(now time.Time, window time.Duration) bool {
	if b.failures >= MaxAnswerAttempts {
		return false
	}
	// A window of zero is NOT "give up now": pause_ttl_seconds: 0 tears the box
	// down and re-seeds from git when the answer lands, with no deadline, so the
	// attempt ceiling is the whole bound.
	return window <= 0 || now.Sub(b.first) < window
}

// answerWindow is how long this node keeps handing one message back to a run,
// beside the attempt ceiling: the run's own pause_ttl_seconds, the wait
// [Waiter.reapOne] enforces, since a role that sets it short has said a reply is
// worth little after it. Measured from the FIRST FAILED HANDOFF, not the park, so
// a run whose window lapsed (reaped, re-seeded, still awaiting) gets the same
// attempts for the reply that finally arrives.
func answerWindow(run PendingRun) time.Duration {
	if run.PauseTTLSeconds <= 0 {
		return 0
	}
	return time.Duration(run.PauseTTLSeconds * float64(time.Second))
}

// deliveryOf names the message an offer is carrying, for the attempt count.
func deliveryOf(trigger *events.Event) string {
	if trigger == nil {
		return ""
	}
	return trigger.ID.String()
}

// deferAnswer counts one delivery the coordinator could not RECORD as a run's
// answer: [AnswerDeferred] while the budget holds, [AnswerNotMine] once it is
// spent, the run left waiting ([MaxAnswerAttempts] says why). The error travels
// either way as the explanation, never the decision: the caller logs it and acts
// on the disposition.
func (c *Coordinator) deferAnswer(ctx context.Context, run PendingRun, trigger *events.Event, cause error) (AnswerDisposition, error) {
	delivery := deliveryOf(trigger)
	window := answerWindow(run)
	if c.spendAnswerAttempt(answerKeyFor(run), delivery, window) {
		return AnswerDeferred, cause
	}
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	log.ErrorContext(ctx, "sandbox_answer_record_exhausted",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "delivery", delivery,
		"attempts", MaxAnswerAttempts, "window_s", window.Seconds(), "error", detail,
		"detail", "this node could not record this message as the answer the executor "+
			"that asked is waiting on, in every one of its attempts, so the message is run "+
			"as the ordinary message it looks like; the executor stays parked on its question "+
			"and its box is bounded by pause_ttl_seconds")
	// THE BUDGET STAYS SPENT, so another copy of this delivery (a held seat's park
	// republishes it under the same id) does not start a second one. A different
	// message gets its own; the table goes when the run or the seat does.
	return AnswerNotMine, cause
}

// answerLookupFailed decides a delivery whose match could not be READ. Not
// [AnswerNotMine] outright (a retry seconds later could match the run, and the
// reply would be spent on an ordinary turn), nor deferred outright (an unreadable
// store must not swallow messages on a seat with no parked runs). It resolves on
// the seat's in-memory awaiting count ([Coordinator.SeatRuns]), which needs no
// read from the store that just failed: none awaiting is [AnswerNotMine], the
// error resolved here; one awaiting is [AnswerDeferred], since only WHICH row is
// unknown. The budget is keyed by the SEAT ([lookupAnswerKey]), as no run was
// read, so no window applies and the attempt ceiling is the whole bound.
func (c *Coordinator) answerLookupFailed(ctx context.Context, handle string,
	trigger *events.Event, cause error,
) (AnswerDisposition, error) {
	if _, awaits := c.SeatRuns(handle); !awaits {
		return AnswerNotMine, nil
	}
	delivery := deliveryOf(trigger)
	if c.spendAnswerAttempt(lookupAnswerKey(handle), delivery, 0) {
		return AnswerDeferred, cause
	}
	log.ErrorContext(ctx, "sandbox_answer_lookup_exhausted",
		"agent", handle, "delivery", delivery, "attempts", MaxAnswerAttempts,
		"error", cause.Error(),
		"detail", "this node could not read which of this seat's runs is awaiting an "+
			"answer, in every one of its spaced attempts, so the message is run as "+
			"the ordinary message it looks like; a run still parked on a question "+
			"keeps waiting, bounded by pause_ttl_seconds")
	return AnswerNotMine, cause
}

// lookupAnswerKey is the budget key for a delivery whose run could not be
// read: the seat is all this node knows about what is owed it.
func lookupAnswerKey(handle string) answerKey {
	return answerKey{handle: handle}
}

// clearLookupAttempts forgets a seat's unreadable-lookup budgets, which a
// lookup that ANSWERED does: the store is readable again, so the next failure
// is the first of its own series.
func (c *Coordinator) clearLookupAttempts(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.attempts, lookupAnswerKey(handle))
}

// answerKeyFor is the table key for a matched run.
func answerKeyFor(run PendingRun) answerKey {
	return answerKey{handle: run.AgentHandle, turnID: run.TurnID}
}

// spendAnswerAttempt charges one failed handoff of one delivery to one run and
// reports whether the delivery should come back. Under the seat counts' lock:
// both are read on a delivery's hot path, and a second mutex would need an order.
// Charged on a held seat too, where an owed answer is deferred, never parked (a
// republish lands behind the person's next message), spaced by the host's renew.
func (c *Coordinator) spendAnswerAttempt(key answerKey, delivery string, window time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	budgets := c.attempts[key]
	if budgets == nil {
		budgets = map[string]answerBudget{}
		c.attempts[key] = budgets
	}
	at, known := budgets[delivery]
	if !known && !roomForAnswerBudget(budgets, now, window) {
		// Every slot holds a live budget: [maxAnswerDeliveries] says why none
		// is evicted to make room.
		return false
	}
	if !known {
		at = answerBudget{first: now}
	}
	at.failures++
	budgets[delivery] = at
	return at.live(now, window)
}

// roomForAnswerBudget drops a run's spent budgets and reports whether a new
// delivery can have one.
func roomForAnswerBudget(budgets map[string]answerBudget, now time.Time, window time.Duration) bool {
	if len(budgets) < maxAnswerDeliveries {
		return true
	}
	for delivery, at := range budgets {
		if !at.live(now, window) {
			delete(budgets, delivery)
		}
	}
	return len(budgets) < maxAnswerDeliveries
}

// clearAnswerAttempts forgets a run's budgets, which every outcome but a defer
// does: the delivery is either spent or handed on, and the next failure is the
// first of its own series.
func (c *Coordinator) clearAnswerAttempts(handle, turnID string) {
	if turnID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.attempts, answerKey{handle: handle, turnID: turnID})
}

// releaseAnswerAttempts drops a seat's budgets once this node no longer holds it:
// a linear sweep over at most [maxAnswerDeliveries] per failing parked run,
// cheaper than keeping an index from seat to run true.
func (c *Coordinator) releaseAnswerAttempts(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.attempts {
		if key.handle == handle {
			delete(c.attempts, key)
		}
	}
}
