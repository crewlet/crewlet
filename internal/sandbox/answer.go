package sandbox

import (
	"context"
	"time"

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
//   - [AnswerDeferred] — hand the delivery back so this node or the seat's
//     next owner is offered it again. Wrong here, the message circles the
//     seat's own inbox instead of being worked — which is why it is both
//     bounded and SPACED; see [MaxAnswerAttempts].
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
// run before this node stops offering it and lets it be the ordinary message
// it looks like.
//
// It counts ATTEMPTS, not hand-backs, so the last of them is not handed back:
// one message reaches one run at most this many times.
//
// WHY A BOUND AT ALL. Nothing else ends the loop: a run parked on a question
// stays matchable for ever — the pause reaper moves it to [StatusReseed],
// which is still [Awaiting] — so a resume that fails the same way every time
// would circle the seat's inbox for the life of the process.
//
// WHY TEN, AND WHAT EACH ATTEMPT NOW COSTS. A deferred answer is handed back
// with a NAK ([Dispatcher.answered]), which is the same return the completion
// route takes and therefore carries the same spacing: the queue's own backoff
// — seed, doubling, ceiling, in internal/queue/jetstream — so ten attempts
// span about two and a half minutes at the shipped values rather than the
// milliseconds an immediate republish burned them in. That span is what the
// failures reaching this path actually need: a seat lease moving to a node
// that can resume the run takes at most one seat lease TTL (45 s, see
// internal/seat), a config apply that brings a missing runner arrives on the
// reconcile poll, and a store blip heals in seconds. And it stays well under
// the broker's 25-delivery dead-letter budget, which this same message also
// spends on ordinary handoffs — reaching THAT budget dead-letters the
// person's reply, which is the one ending this route must never take.
//
// IT IS NOT THE BROKER'S NUMBER, and an earlier doc here claimed it was: 25
// with no spacing is not "the tolerance a completion already had", because a
// completion's 25 deliveries are spread across minutes by that same backoff
// while an immediate republish spent all of them against a transient that had
// not had a millisecond to clear. A count is not a tolerance; the pair of them
// is.
//
// THE SECOND CLAUSE IS TIME, and it is the run's own: see [answerWindow].
//
// PER NODE AND PER PROCESS, deliberately: every failure that reaches here is a
// statement about THIS node — no resumer, a suspended conversation this build
// cannot decode, a seat that is not in this node's company — so a restart or a
// seat handoff is exactly the event that makes a further attempt worth making,
// and both reset the count. It is also why the run is NOT settled when the
// budget is spent: the turn is still resumable somewhere, so this node hands
// the delivery back to the ordinary route rather than destroying work a peer
// or a later build could still finish.
const MaxAnswerAttempts = 10

// maxAnswerDeliveries is how many of one run's deliveries this node keeps a
// budget for at once.
//
// THE BOUND ON THE TABLE ITSELF. A budget is per (run, delivery), so without
// this a long-lived parked run on a busy conversation accumulates one entry
// per message it was ever offered, for as long as the run lives. A parked run
// has ONE open question, and what can be in hand-back for it at any moment is
// the replies a person sent while this node was failing to hand the first one
// over — within one requeue window (minutes) that is a handful. Four covers
// that and caps one run's whole exposure at four budgets of
// [MaxAnswerAttempts] attempts.
//
// A FULL TABLE DOES NOT EVICT A LIVE BUDGET. Spent entries go first, and when
// every slot holds a live one the new delivery is refused a budget and handled
// as the ordinary message it looks like: N live budgets evicting each other is
// precisely the loop the per-delivery budget replaced, at N messages instead
// of two. Dropping a SPENT entry is safe by comparison — its delivery has
// already been let go to the ordinary route, so a copy of it arriving later
// costs at most one further series.
const maxAnswerDeliveries = 4

// answerKey names what a delivery is owed to.
//
// The handle rides along with the turn id so [Coordinator.releaseAnswerAttempts]
// can drop what a seat handed on leaves behind with a scan over keys rather
// than a second index from seat to run.
type answerKey struct {
	handle string
	turnID string
}

// answerBudget is one delivery's series of failed handoffs to one run.
//
// ONE PER (RUN, DELIVERY), which is what a slot per run could not deliver: a
// single slot holding the delivery it was counting RESET whenever a different
// message arrived, so two replies circling one parked run reset each other on
// every pass and neither budget ever ended. Two messages were enough to make
// the bound unreachable and the loop infinite.
type answerBudget struct {
	// failures is how many handoffs of this delivery have failed.
	failures int

	// first is when the first of them failed, which is what the window in
	// [answerWindow] is measured from.
	first time.Time
}

// live reports whether this delivery is still worth handing back: within the
// attempt ceiling, and within the run's own awaiting window.
func (b answerBudget) live(now time.Time, window time.Duration) bool {
	if b.failures >= MaxAnswerAttempts {
		return false
	}
	// A window of zero is NOT "give up now". pause_ttl_seconds: 0 means
	// "never hold a paused box", so such a run was torn down and re-seeds
	// from git when its answer lands — it waits for the person with no
	// deadline at all, and the attempt ceiling above is the whole bound.
	return window <= 0 || now.Sub(b.first) < window
}

// answerWindow is how long this node keeps handing one message back to a run,
// beside the attempt ceiling.
//
// THE RUN'S OWN AWAITING WINDOW, because that is the tolerance the run itself
// declares: pause_ttl_seconds is how long the engine holds a box paused for
// the person to reply, the same number [Waiter.reapExpiredPauses] enforces on
// it. A seat whose role sets a short one has said a reply is worth little
// after it, and bouncing that reply around the inbox for longer than the
// engine was willing to wait for it costs the person an answer on a failure
// that is plainly not transient.
//
// Measured from the FIRST FAILED HANDOFF rather than from the park, so a run
// whose window has already lapsed — reaped, re-seeded, still awaiting — gets
// the same attempts as any other for the reply that finally arrives.
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
	window := answerWindow(run)
	if c.spendAnswerAttempt(answerKeyFor(run), delivery, window) {
		return AnswerDeferred, cause
	}
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	log.ErrorContext(ctx, "sandbox_answer_requeue_exhausted",
		"turn_id", run.TurnID, "agent", run.AgentHandle, "delivery", delivery,
		"attempts", MaxAnswerAttempts, "window_s", window.Seconds(), "error", detail,
		"detail", "this node could not hand this message to the coding run that asked, "+
			"in every one of its spaced attempts, so the message is run as the ordinary "+
			"message it looks like; the run stays parked on its question and its box "+
			"is bounded by pause_ttl_seconds")
	// THE BUDGET STAYS SPENT, so this delivery does not start a second one
	// if another copy of it reaches this node — the park a held seat makes
	// republishes, so same-id copies do exist. A different message gets its
	// own budget, and the whole table goes when the run ends or the seat
	// does.
	return AnswerNotMine, cause
}

// answerKeyFor is the table key for a matched run.
func answerKeyFor(run PendingRun) answerKey {
	return answerKey{handle: run.AgentHandle, turnID: run.TurnID}
}

// spendAnswerAttempt charges one failed handoff of one delivery to one run and
// reports whether the delivery should come back.
//
// Under the same lock as the seat counts, because both are read on the hot
// path of a delivery and a second mutex would be a second thing to order —
// and because the seat counts are what the first branch reads.
func (c *Coordinator) spendAnswerAttempt(key answerKey, delivery string, window time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// NOTHING IS CHARGED WHILE ANOTHER RUN HOLDS THE SEAT. There the
	// delivery is parked for the SEAT's sake whatever this offer says — an
	// immediate republish, at a rate nothing here controls, for as long as
	// that job runs — so charging would spend a message's whole budget
	// inside one held run's park loop and leave nothing for the attempts
	// that are actually spaced: the ones made once the seat is free, which
	// is the state a parked run leaves it in.
	if c.runs[key.handle].holding > 0 {
		return true
	}

	now := c.now()
	budgets := c.attempts[key]
	if budgets == nil {
		budgets = map[string]answerBudget{}
		c.attempts[key] = budgets
	}
	at, known := budgets[delivery]
	if !known && !roomForAnswerBudget(budgets, now, window) {
		// Every slot holds a live budget, so this run already has as many
		// messages in hand-back as it is allowed. See [maxAnswerDeliveries]
		// for why a live one is never evicted to make room.
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

// releaseAnswerAttempts drops what a seat's runs were counting, for a seat
// this node no longer holds.
//
// A LINEAR SWEEP over a table holding at most [maxAnswerDeliveries] budgets
// per parked run this node failed to resume, which is a handful at the very
// most; an index from seat to run would be a second structure to keep true for
// a scan that costs nothing.
func (c *Coordinator) releaseAnswerAttempts(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.attempts {
		if key.handle == handle {
			delete(c.attempts, key)
		}
	}
}
