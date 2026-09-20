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
	// awaiting the conversation, the lookup failed and this seat has no
	// awaiting run for one to have matched, or the run it matched is
	// terminally gone — settled, deleted, unresumable for good. It is an
	// ordinary message and is handled as one.
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

// MaxAnswerAttempts is how many times ONE PROCESS hands one delivery to ONE
// parked run before it stops offering it and lets it be the ordinary message
// it looks like.
//
// It counts ATTEMPTS, not hand-backs, so the last of them is not handed back:
// one message reaches one run at most this many times IN THIS PROCESS.
//
// WHY A BOUND AT ALL. Nothing else ends the loop within a process: a run
// parked on a question stays matchable for ever — the pause reaper moves it to
// [StatusReseed], which is still [Awaiting] — so a resume that fails the same
// way every time would circle the seat's inbox for the life of the process.
//
// THE OTHER CLAUSE IS THE ONE THE MESSAGE CARRIES, and it is not this one:
// see [AnswerDeliveryReserve]. This count is PER NODE AND PER PROCESS and the
// broker's delivery budget is per MESSAGE, so nothing here can make a claim
// about that budget — and this doc made one anyway. It said ten attempts stay
// "well under the broker's 25-delivery dead-letter budget" fourteen lines
// above the paragraph saying a restart or a seat handoff resets the count.
// Both cannot be true, and it was the headroom claim that was false: the
// count the broker enforces rides on the message and survives every handoff,
// so three nodes of ten attempts each hand ONE reply back twenty-seven times
// (measured) and the twenty-fifth dead-letters it — the one ending this route
// must never take. The headroom is now stated where it can be true, on the
// delivery count itself, and this ceiling bounds what it always bounded: one
// process's thrash.
//
// WHY TEN, AND WHAT EACH ATTEMPT COSTS. A deferred answer is handed back with
// a NAK ([Dispatcher.answered]), which is the same return the completion route
// takes and therefore carries the same spacing: the queue's own backoff —
// seed, doubling, ceiling, in internal/queue/jetstream — so ten attempts span
// about two and a half minutes at the shipped values rather than the
// milliseconds an immediate republish burned them in. That span is what the
// failures reaching this path actually need: a seat lease moving to a node
// that can resume the run takes at most one seat lease TTL (45 s, see
// internal/seat), a config apply that brings a missing runner arrives on the
// reconcile poll, and a store blip heals in seconds.
//
// IT IS NOT THE BROKER'S NUMBER, and an earlier doc here claimed it was: 25
// with no spacing is not "the tolerance a completion already had", because a
// completion's 25 deliveries are spread across minutes by that same backoff
// while an immediate republish spent all of them against a transient that had
// not had a millisecond to clear. A count is not a tolerance; the pair of them
// is.
//
// THE THIRD CLAUSE IS TIME, and it is the run's own: see [answerWindow].
//
// PER NODE AND PER PROCESS, deliberately: every failure that reaches here is a
// statement about THIS node — no resumer, a suspended conversation this build
// cannot decode, a seat that is not in this node's company — so a restart or a
// seat handoff is exactly the event that makes a further attempt worth making,
// and both reset the count. That reset is safe now because it is no longer
// the only thing between a reply and the dead-letter subject. It is also why
// the run is NOT settled when the budget is spent: the turn is still resumable
// somewhere, so this node hands the delivery back to the ordinary route rather
// than destroying work a peer or a later build could still finish.
const MaxAnswerAttempts = 10

// AnswerDeliveryReserve is how many of a message's remaining deliveries are
// left to the ordinary route: once a delivery is within this many of the
// backend's dead-letter budget, it is no longer offered to a parked run.
//
// THE CLAUSE THAT MAKES THE HEADROOM CLAIM TRUE, because it is measured on
// what the MESSAGE carries — queue.DeliveriesLeftFor, stated per message by
// the backend at the handler boundary — rather than on what any one process
// remembers. Every bound in this file resets when a seat moves or a node
// restarts, and the broker's does not; see [MaxAnswerAttempts] for the reply
// that was dead-lettered by the gap between those two facts.
//
// PER MESSAGE, which is the whole of what it measures and was briefly not:
// gated on the PARTITION's number instead — the smallest of its messages' —
// the reserve refused a person's clarification reply that was on its first
// delivery with a full budget in hand, because some other message on the same
// conversation happened to be near its own. That is the original defect from
// the other side: the reply spent on an ordinary turn while a parked run was
// still owed it. See [MayOfferAnswer].
//
// FIVE, AND WHAT CONSUMES THEM. The reserve is exactly what the ordinary
// route is left holding, since the offer stops with this many deliveries
// still on the message, and that route spends them one at a time on this same
// reply:
//
//   - a seat handoff while the delivery is in flight. The dispatcher defers,
//     which on this broker is a Nak and costs a delivery, and placement
//     converges within one seat lease TTL (45 s, internal/seat) — so a
//     company whose seats are moving spends one or two here.
//   - a partition that would not merge, and a park whose requeue failed:
//     one each, and both are the paths that hand a delivery back rather than
//     republish it.
//   - the turn's own retries. A turn that broke before it reached outside the
//     engine is Naked and run again, and it is the whole point of falling
//     through to the ordinary route that those retries exist.
//
// Five of them span about two minutes at the queue's backoff ceiling (30 s,
// internal/queue/jetstream), which covers a seat lease TTL and several config
// reconcile intervals (15 s, internal/configplane) — the same transients the
// ten attempts above are sized for, which is the point: what is left over
// must be worth as much as what was spent.
//
// It is deliberately NOT derived from the broker's budget. That number is a
// backend's, configurable, and counted in two conventions (see
// queue.DeliveriesLeft); a reserve expressed as a fraction of it would move
// when an operator shrank it, which is the one moment the ordinary route can
// least afford to have less.
const AnswerDeliveryReserve = 5

// AnswerHeadroom is what the transport said about ONE message of a delivery:
// how many further deliveries it has, and whether the transport said at all.
//
// The pair queue.DeliveriesLeftFor returns, carried as a value so the rule
// below is exercisable without a queue, a broker or a partition — the same
// separation internal/agent/extension's Policy is built on and for the same
// reason: a policy that can only be reached through live machinery is a
// policy nobody re-measures.
type AnswerHeadroom struct {
	// Left is how many further deliveries this message has before the
	// transport dead-letters it.
	Left int

	// Known is whether the transport stated Left at all. FALSE IS NOT A
	// ZERO: see [MayOfferAnswer].
	Known bool
}

// MayOfferAnswer reports whether a delivery may still be offered to a parked
// coding run, given what each of its messages has left before the transport
// dead-letters it.
//
// PER MESSAGE, AND ANY OF THEM IS ENOUGH. A delivery is a partition, and its
// messages sit at different counts as a matter of course: a conversation
// whose earlier message has been handed back a dozen times keeps collecting
// fresh replies, and each of those arrives with a whole budget. The question
// this answers is whether the answer route can still afford an attempt at the
// reply it may be owed — so it is enough that ONE message here still has the
// deliveries, because that is the one an answer would come on.
//
// THE SMALLEST IS THE WRONG FOLD HERE, and it was what this read: the
// partition's number, [queue.DeliveriesLeft], which is the right answer to a
// different question ("will handing this batch back dead-letter something").
// Gated on it, a fresh reply was refused the answer route outright whenever
// any co-partitioned message was near its own budget — and once one message
// on a conversation is spent, EVERY later reply on it is refused, so the
// route is poisoned for that conversation for good. The cost of that refusal
// is the defect this whole route exists to end: the reply is consumed by an
// ordinary turn while a run is still parked on the question it answers.
//
// WHAT THE OTHER FOLD WOULD HAVE BOUGHT, stated plainly because it is a real
// trade and not an oversight: a hand-back returns the whole partition, so
// offering on one message's headroom can spend the last delivery of another
// and dead-letter it. That message is not saved by refusing — the ordinary
// route hands the same partition back on its own failures at exactly the same
// cost — and the two endings are not equal even when it is. A dead-letter is
// LOUD: a copy on the dead-letter subject and a `dead_lettered` line naming
// the deliveries it took. A reply spent on an unrelated turn is SILENT, and
// what it leaves behind is a paused box waiting out its pause TTL for an
// answer that already arrived. Where the quiet failure is the worse one, the
// route offers; the dispatcher says so on the log when the partition's own
// number is inside the reserve, so the loud one is never a surprise either.
//
// AN ABSENT COUNT IS NOT A SPENT ONE: a transport that did not say leaves the
// route exactly as it was before this clause existed, bounded by
// [MaxAnswerAttempts] and [answerWindow] alone. The alternative — reading
// silence as "no headroom" — would turn every delivery on a node whose
// metadata is unreadable into an ordinary turn, which spends the reply on the
// one failure this whole type exists to prevent. An EMPTY list is the same
// silence: nothing was stated about anything.
//
// Here rather than in the dispatcher because the rule belongs with the number
// it reads, and because a rule stated in a frame that also holds a queue, a
// screening and a turn is one nobody can exercise on its own.
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

// answerLookupFailed decides a delivery whose match could not be READ.
//
// A failed lookup is exactly the state this type's own asymmetry is about —
// the coordinator could not establish that nothing is owed the delivery — and
// it was classified [AnswerNotMine] anyway, so on the one failure where a
// retry seconds later would have matched the run, the person's reply was spent
// on an ordinary turn instead. The original fail-open reasoning is right too:
// an unreadable store must not swallow a message that has nothing to do with
// any coding run, which on a company with no parked runs at all is every
// message the seat receives. Both hold, and they resolve on a question this
// node can answer WITHOUT the store that just failed.
//
// THE SEAT'S OWN AWAITING COUNT is that question — [Coordinator.SeatRuns]'s
// second value. It is in memory: seeded from the store when this node claimed
// the seat, and moved since by the transitions this process itself made. That
// is precisely why it is the right source here — it needs no read from the
// store the read just failed against, so it still answers when the alternative
// is a guess.
//
//   - NO AWAITING RUN ON THIS SEAT: [AnswerNotMine], as before. Nothing is
//     owed the delivery, so there would be nothing for a hand-back to come
//     back to, and the message is the ordinary one it looks like. The error is
//     resolved here rather than reported, because the caller has nothing left
//     to decide about it.
//   - AN AWAITING RUN: [AnswerDeferred]. Something on this seat IS waiting for
//     somebody's reply and only WHICH row could not be read — an answer that
//     arrives twice is recoverable and an answer that is spent is not.
//
// The bound is the ordinary one, under a key naming the SEAT rather than a
// run, because the run is the thing this path could not read. Its window is
// the run's, and no run was read, so the attempt ceiling is the whole of it —
// the same ceiling, reached at the same spacing.
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
