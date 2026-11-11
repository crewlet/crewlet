// Package queue defines the EventQueue contract — the transport every
// inter-component message in Crewlet travels through.
//
// One interface, two backends (an in-memory twin, and NATS JetStream —
// embedded by default, external when configured), both certified by ONE
// conformance suite. A
// backend that the suite has not certified does not exist as far as the
// engine is concerned, and nothing above this package may branch on which
// backend is running. One broker carries both this stream and the fleet's
// coordination store, and a node runs on it only while every connection it
// cannot run without is open: the queue's own, and on an embedded broker the
// coordination store's second one, both watched so that either closing for
// good stops the node (a snapshot donor's connection is not one of them) —
// ADR-0001, and the reason a broker without compare-and-set cannot be a
// backend here however good its messaging is.
//
// The rationale a reader should not have to re-derive:
//
//   - A durable subscription IS a seat's mailbox. It exists without a
//     consumer, retains what is published while nothing is attached, and
//     replays in order when someone attaches. Publishing to a topic with no
//     subscription drops the event silently — hence EnsureSubscription.
//   - Handlers have four outcomes, not two. Ack, Nak (a failure), Defer
//     (hand it back and stop consuming: a condition of the node) and Pass
//     (hand it back for a peer and go on consuming: this node will not
//     handle this message, another may) — see Result. The returns keep
//     different places, on every backend: a Nak and a Pass come back BEHIND
//     the mail that was waiting, a Defer at the HEAD. A handler whose
//     message must keep its place in its conversation does not Nak.
//   - A handler is told how many deliveries are LEFT before the backend
//     dead-letters a message, because every outcome that puts one back spends
//     one and the count rides on the message rather than on the process
//     handling it. TWO numbers, because one cannot answer both questions: the
//     PARTITION's (DeliveriesLeft, the smallest of its messages') answers
//     "will handing this batch back dead-letter something", and each
//     MESSAGE's (DeliveriesLeftFor) answers "how much is left of this one".
//     A caller that reads the first where it means the second is bounded by
//     the worst message it happens to be batched with.
//   - Attachment has four verbs with different destructiveness: Quiesce,
//     Unquiesce, Detach, DeleteSubscription.
//   - The durable subscriptions a broker holds can be LISTED
//     (ListSubscriptions). A mailbox outlives everything that knew its name,
//     a removed seat's handle included, so the broker has to be able to say
//     which mailboxes exist or a leaked one can never be found again.
//   - DEAD LETTERS ARE ONE PLACE with one retention, whoever gives up on a
//     message: a subscription's backend when its budget runs out, or a
//     reader that settles its own deliveries and keeps what it gives up on
//     (KeepDeadLetter — a wake feed, which reads a state log's consumer
//     rather than a subscription). An entry is a BODY and a one-line
//     HEADER kept apart, so a reader that only needs to know what is kept
//     never moves what it keeps: a subject's entries are listed by their
//     headers alone (DeadLettersOn), one entry's body is read by its
//     sequence (DeadLetterFrame), what a whole subtree of subjects holds is
//     counted without listing it (DeadLetterMarks), and an entry is removed
//     one at a time (DropDeadLetter). A subscription's entry is the
//     message's copy, kept with no header — except one a PASS retired,
//     whose header names the pass and its reason (PassedLetter), so a
//     reader that can handle it now finds it again (PassedReason).
//   - A failed verb is MARKED with one of three sentinels when the failure is
//     one a caller acts on, and nothing else about it may be branched on:
//     ErrTooLarge (the event can never fit — permanent), ErrNotLive (this
//     queue consumes and answers nothing until it is started again) and
//     ErrUnavailable (the broker did not answer — a CONDITION, which the
//     same call made again in a moment may clear). An unmarked failure is
//     none of those: the caller's own mistake, or a broker answer that
//     waiting does not change.
package queue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/panics"
)

var log = logging.Get("queue.contract")

// ErrTooLarge reports that an event does not fit on the wire.
//
// ONE OF THE THREE SENTINELS THE LAYERS ABOVE MAY BRANCH ON — ErrNotLive and
// ErrUnavailable are the others — and for the same reason as both: they must
// not branch on which backend is running, so each one translates its own
// refusal into this and callers ask one question everywhere.
//
// It exists because "too large" is the one publish failure that is PERMANENT.
// The reasons a publish fails that trying again does answer — a broker
// restarting, a connection dropping, a quorum briefly unavailable — are
// ErrUnavailable, and a producer that could not tell this from them had to
// treat every failure as transient.
// The webhook edge did exactly that and asked the provider to retry a delivery
// whose size guaranteed it would fail identically forever, releasing and
// re-taking its claim on every attempt.
//
// Sizing a producer's own input against MaxPayloadBytes cannot replace this.
// What a body costs on the wire is a property of its BYTES, not its length:
// encoding/json escapes '<', '>' and '&' to six bytes each, so one JSON
// document re-encodes at 1x and another of the same length at 6x. Any
// up-front ratio is therefore either wrong for the second or wasteful for the
// first — measured, and it is why this is an error from the transport rather
// than a division at the edge.
var ErrTooLarge = errors.New("queue: event too large for the transport")

// MaxPayloadBytes is the largest single event any backend must carry.
//
// It is a CONTRACT number rather than a backend's own, because every backend
// has to agree on it: the embedded broker is configured to accept exactly this
// (see internal/queue/jetstream), an external NATS server announcing less is
// refused when the backend opens rather than at the first message past it,
// and the in-memory twin enforces the same ceiling, so a payload that a test
// accepts is one production accepts.
//
// Written down because there was no number at all, and the failure had no
// floor: the webhook edge accepted a 25 MiB delivery, verified its signature,
// claimed it, and then could not publish it through a broker left at its own
// 1 MiB default. The delivery was refused as unavailable, so the provider
// retried, and every retry failed the same way forever. Nothing in that loop
// is a transient, and nothing logged a size. Raising the ceiling is half the
// answer; ErrTooLarge, which lets a producer tell that refusal apart from a
// broker that is merely down, is the other half.
//
// 8 MiB is nats-server's own MAX_PAYLOAD_MAX_SIZE — the threshold above which
// it warns that a payload is too large to be a good idea (server/const.go).
// Sitting AT that boundary takes eight times the default without arguing with
// the broker about what it was built for: a message is buffered whole, in the
// server and again in every client that receives it, so this is memory per
// in-flight event and not a disk number.
const MaxPayloadBytes = 8 << 20

// ConcurrentAnswers is how many requests an [EventQueue.Serve] registration
// answers at once when what it answers is work it does NOW.
//
// SIXTY-FOUR, which is twice the default `node.max_concurrent`: every turn a
// node runs can be inside one tool call that asks a peer, so a registration
// answering one node's full complement of turns and a second node's beside it
// never queues a request behind another. The cap bounds goroutines and
// buffered payloads rather than throughput — a request past it waits for a
// slot rather than failing, as far as its backend buffers it
// ([EventQueue.Serve] says how far). What it costs in memory is sixty-four
// of the largest answer THAT registration serves, and most answer in a few
// kilobytes (a steer, a sandbox tail, the fleet broker's member exchange).
// Four can fill an answer up to [MaxPayloadBytes]: the fleet turn-detail
// scatter (internal/eventfan), the memory read (internal/learning/memread),
// a box's read back by its host (internal/engine's sandbox host, whose result
// is cut to the first of the carries the broker takes) and the estate
// router's answers (internal/estate) — whose paged usage reads are cut at a
// quarter of the message, so one reaches the cap only by one oversized
// record. One of those at the cap, every answer at the ceiling, is
// 512 MiB held at once — a bound nothing here reaches in practice, because an
// answer that large is a dashboard or a seat reading a whole window and
// sixty-four of them in flight on one registration is sixty-four such readers
// at the same instant. Past the cap a request waits for a slot like any
// other, which is back-pressure on the asker rather than a failure: work done
// now frees its slots in the time its answers take, long before a backend's
// buffer fills.
//
// A REGISTRATION WHOSE ANSWERS WAIT — in a queue of their own, for as long as
// their askers' attempts, as a push proposal waits behind its repository's
// earlier pushes — passes the bound of that queue instead, and is a
// registration of its own: a slot held through such a wait is one every other
// request of its registration waits behind, so sixty-four of them on a
// registration shared with work done now would hold every slot it has.
const ConcurrentAnswers = 64

// MaxLingerSeconds is the hard ceiling on a batch linger window.
//
// The linger counts against the broker ack-timeout budget that bounds a
// drained message's unacked lifetime: collection plus ONE handler run must
// fit inside it. Config validation mirrors this cap, but programmatic
// construction bypasses that, so the invariant is enforced here regardless
// of who set the field.
const MaxLingerSeconds = 60.0

// Outcome is what a handler decided about its delivery.
type Outcome int

const (
	// OutcomeAck acknowledges the delivery. It is the zero value, so a
	// handler returning Result{} acknowledges: the quiet path is the safe
	// one.
	OutcomeAck Outcome = iota

	// OutcomeNak negatively acknowledges: the handler failed and the
	// message should be redelivered, spending one unit of its
	// dead-letter budget.
	//
	// IT RETURNS BEHIND THE MAIL THAT WAS WAITING, on every backend. The
	// shipped broker SPACES a failure — a second, doubling to thirty — and
	// serves never-delivered messages meanwhile, so the retry comes after
	// every message already queued when the handler failed, newer mail on
	// the same partition key included, each in a call of its own. The
	// in-memory twin models that backoff virtually (behind what was
	// waiting, no timer), and queuetest's
	// a_failure_returns_behind_waiting_mail_and_a_deferral_at_the_head
	// holds both to it. A consumer that needs its message to keep its place
	// does not want a Nak: it wants [OutcomeDefer], for a condition of the
	// node, or to settle the message durably and retry the work itself. See
	// OrderForDispatch for why the contract does not hold newer mail back.
	OutcomeNak

	// OutcomeDefer leaves the delivery unacked AND quiesces the
	// attachment. It is the outcome for work this process has lost the
	// right to do — a seat whose lease moved, a node serving a stale
	// config. Acking would claim work it will not perform.
	//
	// IT COSTS ONE DELIVERY, exactly as a Nak does, on every backend. No
	// broker here has a "give this back without counting it": returning a
	// message promptly means a Nak, and a Nak spends one of its deliveries
	// — so the delivery budget is sized to cover handoffs as well as
	// failures, and the backend dead-letters at the boundary rather than
	// letting the broker's own MaxDeliver backstop discard a healthy event
	// with nothing recorded. The alternative, letting the ack timer expire
	// instead, parks a seat's whole mailbox for the ack window on every
	// lease movement.
	//
	// THIS USED TO DIFFER BY BACKEND, and the contract said so: the
	// in-memory twin returned the events without touching their counters,
	// gated behind a conformance capability. That made one rule two —
	// DeliveriesLeft's doc asserted every return spends one while the
	// suite's own flag asserted a deferral spends nothing — and it made the
	// twin the only backend its own handoff case was ever run against. The
	// twin spends one now, so the sentence above is true of everything that
	// implements this contract.
	//
	// WHAT EVERY BACKEND OWES IS UNCHANGED, and it is the weaker claim the
	// capability used to protect: a deferral must not kill a HEALTHY event.
	// That is answered by sizing the budget so handoffs cannot exhaust it,
	// not by making the handoff free.
	//
	// IT RETURNS AT THE HEAD, on every backend: a hand-back is a plain,
	// unspaced Nak, and the broker serves a redelivery before new mail —
	// so a deferred message keeps its place ahead of its conversation's
	// newer mail, which a failure ([OutcomeNak]) does not. That is the
	// outcome for a delivery that failed on a condition of the NODE or its
	// store rather than of the message: the attachment stops, the message
	// waits at the head, and whatever resumes the attachment (the seat
	// host's next renew, for a seat inbox) brings it back first.
	//
	// When the consumer closes, the broker returns the message to
	// whoever attaches next, in order. Never substitute a republish: that
	// sends the event to the topic tail while its prefetched siblings
	// replay from the head, reordering the conversation.
	OutcomeDefer

	// OutcomePass hands the delivery back to its group for a PEER and goes
	// on consuming: this node will not handle this message and another
	// may, and nothing is wrong with this node — only with this message
	// here. It is the outcome for a delivery one build cannot read and
	// another can: a record a newer build wrote, met by an older one during
	// a roll (internal/notify's Deferral, crewlet#993). Acked, a delivery
	// of a group every node takes from is gone for all of them.
	//
	// NEITHER OF THE OTHER RETURNS FITS IT. A Nak is a FAILURE, logged as
	// one at Warn, for a delivery that failed nothing. A Defer is a
	// condition of the NODE: it quiesces the attachment, so one message
	// this build cannot read would stop the node taking every other; and
	// it returns the message at the head, unspaced, so a fleet whose every
	// node passes it would hand it round as fast as the broker delivers,
	// spending its whole budget before the peer that reads it has started.
	//
	// SO IT IS SPACED AS A FAILURE IS, BY ITS DELIVERY COUNT. The shipped
	// broker withholds the n-th delivery's pass for the failure path's n-th
	// spacing — a second, doubling to thirty — counted off the MESSAGE
	// rather than off the node that passes it, so a message handed from
	// node to node ages at every hand. Like a failure it comes back BEHIND
	// the mail that was waiting, and like every return it spends one
	// delivery: a subscription's budget is 25 deliveries
	// (internal/queue/jetstream's maxDeliver), so a passed message is
	// offered to its group for about ten minutes — 1, 2, 4, 8 and 16
	// seconds, then thirty for each of the nineteen returns after those —
	// and is then dead-lettered with its copy, under a header naming the
	// pass and its reason ([PassedLetter]), never circling for ever. The
	// ack window (internal/queue/jetstream's DefaultAckWait, thirty
	// minutes) does not delay it: a pass is a delayed Nak, not a timeout.
	//
	// NOTHING STEERS A PASS TO A NODE THAT CAN HANDLE IT. The broker hands
	// each return to whichever member of the group asks next, so every one
	// of a message's deliveries can go to nodes that pass it — early in a
	// roll, while one node of the newer build sits among many older ones.
	// What delivers it is the DEAD LETTER: its header tells a reader that
	// it was passed and why ([PassedReason]), and a reader that can handle
	// it now lists its group's dead letters and handles each passed copy
	// as a delivery. internal/notify's Service does that at every start,
	// which is when what makes a copy readable — a newer build — arrives.
	//
	// SO THE GROUP'S OWN BUDGET AND BACKOFF, AND NOT A LONGER WINDOW OF ITS
	// OWN. A dead letter follows, not a drop, so the budget bounds only how
	// long a passed message circles its group before it waits, kept, for
	// the next start of a node that reads it. Ten minutes covers the
	// ordinary case, a newer node winning one of a message's early
	// deliveries, many times over; a longer window would hold the slot it
	// occupies (below) longer and deliver nothing the replay does not.
	//
	// WHILE IT WAITS IT HOLDS ONE OF THE GROUP'S ACK-PENDING SLOTS. A pass
	// waits out its spacing as a delayed Nak, which the broker counts as
	// pending acknowledgement until it redelivers the message, and a
	// consumer at its limit of pending messages delivers no new one: the
	// shipped broker's consumers set none (internal/queue/jetstream's
	// subscriptions leave MaxAckPending unset), so the server's default of
	// 1000 applies. Each pass holds its slot only until it is redelivered,
	// which is a second or two once any real share of the group runs the
	// newer build. The worst case is early in a roll, one newer node among
	// many: a passed message can hold its slot for its whole ten minutes,
	// so about 1.7 newer-record wakes a second, sustained, fill 1000 slots
	// (1000 over the budget's 601 seconds). That delays the group's other
	// mail and loses none of it, and the stall heals as the passes are
	// redelivered or dead-lettered.
	//
	// The in-memory twin redelivers it as it redelivers a Nak, behind the
	// mail that was waiting with no timer, and spends and dead-letters it
	// alike, under the same header.
	OutcomePass
)

// String renders an outcome for logs.
func (o Outcome) String() string {
	switch o {
	case OutcomeNak:
		return "nak"
	case OutcomeDefer:
		return "defer"
	case OutcomePass:
		return "pass"
	default:
		return "ack"
	}
}

// Result is a handler's decision about its delivery.
type Result struct {
	Outcome Outcome
	// Reason is logged. For a deferral or a pass it is why the message
	// went back to the broker, which is the only record of it.
	Reason string
	// Cause is a deferral's class, from a closed set: what a report about a
	// message its hand-backs retired may say, where Reason is free text.
	Cause DeferCause
	// Err is the failure that caused a Nak, or what a pass met, logged
	// beside its reason.
	Err error
}

// DeferCause is why a delivery was handed back ([Defer]).
type DeferCause string

// The causes of a hand-back.
const (
	// DeferSeatMoved is a seat held by another node now, or lost mid-turn.
	DeferSeatMoved DeferCause = "seat_moved"
	// DeferDraining is a node draining or stopping, or a member leaving its group.
	DeferDraining DeferCause = "draining"
	// DeferShedding is the control plane's posture refusing new work.
	DeferShedding DeferCause = "shedding"
	// DeferPauseUnread is a node that has not yet read whether the seat is paused.
	DeferPauseUnread DeferCause = "pause_unread"
	// DeferExecutorHolds is a seat its executor's run holds from its box.
	DeferExecutorHolds DeferCause = "executor_holds"
	// DeferHoldRefused is a hold or a park the queue would not take.
	DeferHoldRefused DeferCause = "hold_refused"
	// DeferParked is a seat parked on its token budget or its models' limit.
	DeferParked DeferCause = "parked"
	// DeferAnswerOwed is a delivery whose answer a parked run is still owed,
	// or a reply too large to be one, kept while the refusal recorded for it
	// is still its run's.
	DeferAnswerOwed DeferCause = "answer_owed"
)

// DeferCauses is every cause, in a stable order.
var DeferCauses = []DeferCause{DeferSeatMoved, DeferDraining, DeferShedding,
	DeferPauseUnread, DeferExecutorHolds, DeferHoldRefused, DeferParked, DeferAnswerOwed}

// Valid reports whether c is one of [DeferCauses].
func (c DeferCause) Valid() bool { return slices.Contains(DeferCauses, c) }

// Ack acknowledges the delivery.
func Ack() Result { return Result{} }

// Nak negatively acknowledges, requesting redelivery.
func Nak(err error) Result { return Result{Outcome: OutcomeNak, Err: err} }

// Defer leaves the delivery unacked and quiesces the attachment, for cause.
func Defer(cause DeferCause, reason string) Result {
	return Result{Outcome: OutcomeDefer, Cause: cause, Reason: reason}
}

// Pass hands the delivery back to its group for a peer and goes on consuming
// ([OutcomePass]): reason is why, and err what this node met, both logged.
// reason is also the one a pass that spends the delivery's budget keeps its
// dead letter under ([PassedLetter]), which is how a reader that can handle
// the copy finds it again, so it is a word that reader matches, whole.
//
// The group's own budget, about ten minutes, is all a pass gets, and enough:
// a dead letter follows, not a drop, and the replay at the next start of a node
// that reads the copy delivers it (internal/notify). While it waits out its
// spacing it holds one of the group's ack-pending slots, which [OutcomePass]
// does the arithmetic of: it delays the group's mail and loses none.
func Pass(reason string, err error) Result {
	return Result{Outcome: OutcomePass, Reason: reason, Err: err}
}

// Handler processes one event.
type Handler func(ctx context.Context, ev *events.Event) Result

// BatchHandler processes one PARTITION of a drained batch.
type BatchHandler func(ctx context.Context, evs []*events.Event) Result

// ErrNotLive reports that a verb reached a queue that is not live — never
// started, stopped, or holding a connection its broker client has closed for
// good, which no later call reopens and which stops the node.
//
// ONE OF THE THREE SENTINELS THE LAYERS ABOVE MAY BRANCH ON — see ErrTooLarge
// and ErrUnavailable for the others — because they must not branch on which
// backend is running: each backend keeps its own error (jetstream.ErrClosed,
// jetstream.ErrConnectionLost, memory.ErrNotStarted) and wraps this, so
// errors.Is(err, queue.ErrNotLive) is the same question everywhere.
//
// It exists because "the queue is down" is not a failure for every caller. A
// seat release detaches the mailbox and the seat host KEEPS THE LEASE if that
// detach cannot be proven — a seat this process may still be consuming must
// not go to a peer. But a queue that is not live consumes nothing, so a
// refusal there is proof of teardown rather than a failure to prove it, and
// reading it as a failure would strand the seat for a full TTL on the one
// path where the node is trying to hand it back.
var ErrNotLive = errors.New("queue: not live")

// ErrUnavailable reports that a verb did not reach the broker, or reached it
// and was not answered: this node's connection to it is down and its client is
// reconnecting, or the broker — or the stream or the consumer the verb
// addressed, a leader mid-election among them — did not reply within the
// client's own time.
//
// A CONDITION, and the only one of the three sentinels that is: the same call
// made again once the connection is back may succeed, so a caller answers it
// as it answers any wait that clears — later, or with "try again shortly" —
// and never as a fault of this node, which it is not. Each backend wraps its
// transport's own words in it rather than replacing them (on JetStream a
// timeout, no responders, a reconnecting or disconnected connection, a
// reconnect buffer that filled), so a log still reads the client's message
// and errors.Is on the client's own sentinel still answers.
//
// IT IS NOT ErrNotLive, and the difference is what a caller may conclude. A
// queue that is not live consumes nothing, which a seat release reads as
// proof of teardown. A broker that did not answer proves nothing of the kind:
// this queue's consumers stay attached and resume when the connection does,
// so reading this as teardown would hand a seat to a peer while this node may
// still be consuming it.
//
// THE VERBS ABOUT THIS NODE'S OWN ATTACHMENTS NEVER ANSWER IT — Quiesce,
// Unquiesce, Detach, PauseTopic and ResumeTopic touch no broker, and must not
// need one: a node that can no longer prove it owns a seat quiesces that
// seat's mailbox on exactly the outage that cost it the proof, when the
// coordination store rides the same broker. Every other verb has to reach the
// broker and may.
//
// IT IS NOT A STATEMENT THAT NOTHING HAPPENED, exactly as no failed publish
// ever was: a publish whose acknowledgement did not arrive may still have
// been stored, so a producer that retries must tolerate a second copy.
// "Published means durable" is a claim a success makes and a failure does not
// unmake. Ask is the verb that refuses before anything is sent — see its
// contract.
//
// A CALLER'S OWN DEADLINE OR CANCELLATION, ending a wait on a broker this
// node is connected to, stays the context's error alone and is not marked:
// the caller decided it, and the context says whose decision it was. One that
// ends while the connection is down is marked, because the connection is why
// nothing answered.
var ErrUnavailable = errors.New("queue: broker unavailable")

// StreamHandler receives broadcast events. Stream delivery is best-effort:
// there is no ack, and a handler's failure is logged, never redelivered.
type StreamHandler func(ctx context.Context, topic string, ev *events.Event)

// AnswerFunc answers one scattered request.
//
// AN ERROR ANSWERS NOTHING. The asker learns that this server did not answer
// and nothing else — the same fact as a server that was down, slow or absent,
// and deliberately so: an asker able to tell a server's failure from its
// absence would have two cases to handle where the honest answer is one. What
// a failure is worth saying is said in the server's own log, where the server
// is.
//
// The context is the ANSWERER's, cancelled when its registration ends. The
// asker's deadline is enforced by the asker, because only the asker knows it.
type AnswerFunc func(ctx context.Context, request []byte) ([]byte, error)

// ListenFunc handles one broadcast message: the literal subject it was
// published on and its bytes, exactly as sent.
//
// NO CONTEXT AND NO RESULT, because there is nobody to answer: the publisher
// has moved on, and a broadcast is never redelivered. A handler that cannot use
// a message drops it, which is what at most once already means.
type ListenFunc func(subject string, payload []byte)

// LiteralSubject reports why subject cannot be broadcast on, or nil when it can:
// a non-empty dotted name whose every token is non-empty and none is a pattern
// token (`*` or `>`).
//
// THE CONTRACT'S RULE, SHARED BY BOTH BACKENDS: the broker refuses a publish to
// a pattern with a protocol error the client reports on its own goroutine, long
// after the call returned success, so a backend that passed one through would
// answer "sent" for a message nobody could ever hear, and the twin would match
// it against every listener's pattern instead. Both refuse it here, before
// anything is sent.
func LiteralSubject(subject string) error {
	if subject == "" {
		return errors.New("queue: an empty subject names nobody to broadcast to")
	}
	for token := range strings.SplitSeq(subject, ".") {
		switch token {
		case "":
			return fmt.Errorf("queue: subject %q has an empty token", subject)
		case "*", ">":
			return fmt.Errorf("queue: subject %q is a pattern; a broadcast names one "+
				"literal subject and a listener names the pattern", subject)
		}
		if strings.ContainsAny(token, " \t\r\n") {
			return fmt.Errorf("queue: subject %q holds whitespace, which ends a "+
				"subject on the wire", subject)
		}
	}
	return nil
}

// Unsubscribe withdraws a registration, a stream subscription, an answerer
// ([EventQueue.Serve]) or a listener ([EventQueue.Listen]), and releases its
// backend resources.
//
// ONE A QUEUE RETURNS MAY BE CALLED AGAIN. Only the first call withdraws; a
// later one finds the registration withdrawn, which is all it asks, and
// answers nil once nothing the verb's withdrawal waits for is still running.
// A broker that reports the registration already gone is reporting what the
// first call made true, not a failure. So a deferred withdrawal beside an
// explicit one, or a teardown that runs twice, hears the same from every
// backend, and the conformance suite holds each to it (#777).
type Unsubscribe func(ctx context.Context) error

// BatchKeyFunc derives the PARTITION KEY an event is handled under: which
// other events this one is drained, merged and dispatched WITH.
//
// NOT the conversation the event belongs to, which it was and which is a
// different question with a different answer on at least one source. A direct
// message is one conversation however it is threaded, so its identity is the
// bare channel — while a reply in a thread on that line still partitions on
// the thread, because merging it with unrelated top-level pings would hand
// one digest a reply target that names only one of them. This layer must not
// know that: internal/queue cannot import internal/notify, so the producer
// stamps both values and the caller hands the partition one down (see
// node.partitionKey, over notify.KeyOf). Prose is the only thing tying the
// two ends together, which is exactly why it has to name the right key.
type BatchKeyFunc func(ev *events.Event) string

// PublishListener is invoked inline on every publish. Listeners run in the
// publishing goroutine and must not block for long; their failures are
// logged and never prevent the publish.
type PublishListener func(ctx context.Context, topic string, ev *events.Event)

// Subscription names one durable subscription by the (topic, group) pair it
// was created with, exactly as the caller spelled both.
//
// The PAIR, because every backend keys a subscription on the pair and two
// groups on one topic are two mailboxes; see topics.AgentControlGroupSuffix for
// a pair of seats whose groups alone would collide.
type Subscription struct {
	Topic string
	Group string

	// Partial reports a subscription on a laned topic ([HasLane]) whose
	// priority lane the broker does not hold: its topic's mail is retained
	// and its lane's is not, so a priority event published to it reaches
	// nobody. EnsureSubscription completes it. A reader that converges
	// mailboxes treats a partial one as missing; a reader that retires them
	// treats it as present, because its retained mail is still somebody's.
	Partial bool
}

// Kept is one entry a dead-letter subject holds, as [EventQueue.DeadLettersOn]
// lists it: everything the backend and the keeper say of the entry, and never
// the body it was kept with ([EventQueue.DeadLetterFrame] reads that).
type Kept struct {
	// Seq is the entry's identity: what [EventQueue.DropDeadLetter] and
	// [EventQueue.DeadLetterFrame] name. It is the backend's own sequence,
	// unique across every dead-letter subject of one broker and never
	// reused, so a seq read off one subject can name nothing on another.
	Seq uint64

	// StoredAt is when the backend kept the entry, on the backend's own
	// clock, which every node reads alike.
	StoredAt time.Time

	// Expires is when the backend lets the entry go: StoredAt plus the
	// backend's dead-letter retention. AN INSTANT RATHER THAN THE RETENTION,
	// because the retention is a backend's setting the caller has no other
	// way to read — the JetStream backend's is the event stream's own age
	// bound (`stream.event_retention_hours`), a safety field every node of a
	// fleet holds equal — and what a caller acts on is how long this entry
	// has left.
	Expires time.Time

	// Header is the line the keeper kept the entry under, exactly as it was
	// handed to [EventQueue.KeepDeadLetter]; empty for an entry kept with
	// none — a subscription's poison, which its backend keeps as the
	// message's own bytes and nothing beside them. A subscription's copy a
	// pass retired is the one it marks, under [PassedLetter].
	Header []byte
}

// Marks is what a subtree of the dead-letter namespace holds, as
// [EventQueue.DeadLetterMarks] counts it: enough for a reader that keeps a
// listing to tell whether it is still what the backend holds, without
// listing anything.
//
// TWO FACTS, because a count alone cannot tell one entry from another: an
// entry kept on a subject and another let go from it in the same moment leave
// the subject's count where it was. Last moves with every keep and with
// nothing else, so a reader holding a listing taken at Last knows it can
// only have LOST entries since — each of which lowers its subject's count —
// while Last stands still.
type Marks struct {
	// Counts is each subject under the subtree that holds an entry, with
	// how many it holds. A subject holding none is absent.
	Counts map[string]uint64

	// Last is the last sequence the backend gave any dead letter, under the
	// subtree or anywhere else in the dead-letter namespace: the identity
	// the next keep exceeds, and zero for a backend that has kept none.
	Last uint64
}

// LetterFraming is the most a backend spends carrying one dead letter beside
// its header's and its body's own bytes, and so what the transport's ceiling
// is held to for a keep: [EventQueue.KeepDeadLetter] refuses an entry whose
// header and body come within LetterFraming of [MaxPayloadBytes], with
// [ErrTooLarge], on every backend alike.
//
// A CONTRACT NUMBER, so the twin refuses exactly what the broker does. The
// JetStream backend carries the header as a header of the message, so the
// message on the wire is the body and a header block — `NATS/1.0`, the
// expected stream the keep is checked against, the dead-letter header's own
// name and the blank line that ends them, 70 bytes around the header's own
// (internal/queue/jetstream holds that under this with its own test). Before
// this bound a twin that counted the body alone kept an entry within those
// bytes of the ceiling that the broker refused — the one direction a twin must
// never be wrong in.
//
// A QUARTER KIBIBYTE, which a header name or a stream renamed by a later build
// still fits in, and which costs a keeper nothing it needs: a wake feed's
// largest entry leaves over three kibibytes of the transport between its
// frame and the ceiling (internal/changefeed's deadLetterDetailBytes).
const LetterFraming = 256

// LetterFits reports whether a dead letter of header and body can be kept: the
// two within [LetterFraming] of [MaxPayloadBytes]. It is the one ceiling every
// keep is held to — [EventQueue.KeepDeadLetter] refuses an entry past it with
// [ErrTooLarge] on every backend — and the one a backend holds the header of a
// passed delivery's dead letter to ([PassedLetter]).
func LetterFits(header, body []byte) bool {
	return len(header)+len(body)+LetterFraming <= MaxPayloadBytes
}

// PassedLetter is the header a backend keeps the dead letter of a PASSED
// delivery under — one whose budget a pass ([OutcomePass]) spent — beside its
// copy: `pass ` and reason. Every other dead letter of a subscription, a
// failure's or a hand-back's, is its copy with no header at all.
//
// WHY A PASSED COPY IS MARKED. A pass is a delivery this node would not
// handle and a peer may, and one that spent its budget is one no member of
// its group handled in the ten minutes the budget spans: during a roll, a
// delivery the older nodes kept passing while no node of the newer build won
// it. Its copy is a delivery a node of that build CAN handle, so a reader of
// the group lists its dead letters, finds the passed ones by this header
// ([PassedReason]) and handles each copy as a delivery, dropping the entry
// once it has (internal/notify, at every start). A failure's dead letter is
// poison, which nothing replays, and an unmarked entry is one.
//
// THE REASON IS QUOTED, as an ASCII literal (strconv.QuoteToASCII), so the
// header is one line of printable ASCII ([LetterHeader]) whatever the reason
// holds — a quote, a line ending, a rune past the ASCII — and reads back
// whole.
//
// AND IT HAS TO FIT BESIDE THE COPY ([LetterFits]). A delivery is as large as
// the transport carries ([MaxPayloadBytes]), so a copy within a few hundred
// bytes of that ceiling leaves its header no room: a backend keeps that copy
// with no header — every byte of it still kept for whoever investigates — and
// logs that it did, since no reader of the group will find it as a pass.
func PassedLetter(reason string) []byte {
	return []byte(passedPrefix + strconv.QuoteToASCII(reason))
}

// PassedReason is the reason a dead letter kept under header was passed for
// ([PassedLetter]), and whether header is a passed delivery's at all: false
// for an entry kept with no header — a failure's, a hand-back's, or a passed
// copy that left its header no room — and for every other keeper's.
func PassedReason(header []byte) (string, bool) {
	quoted, ok := bytes.CutPrefix(header, []byte(passedPrefix))
	// A DOUBLE-QUOTED literal and nothing else: strconv.Unquote also reads a
	// raw string and a rune, which PassedLetter never writes.
	if !ok || len(quoted) == 0 || quoted[0] != '"' {
		return "", false
	}
	reason, err := strconv.Unquote(string(quoted))
	if err != nil {
		return "", false
	}
	return reason, true
}

// passedPrefix begins every passed delivery's dead-letter header
// ([PassedLetter]): a word no other keeper's header begins with — a wake
// feed's is a JSON object (internal/changefeed's EncodeDeadLetter).
const passedPrefix = "pass "

// LetterHeader reports why header cannot be kept beside a dead letter, or nil
// when it can: one line of printable ASCII — every byte from the space to the
// tilde — that neither begins nor ends with a space. An empty header is no
// header at all, and is kept as one.
//
// THE CONTRACT'S RULE, SHARED BY BOTH BACKENDS, and the transport's: NATS
// carries a header's value as one line of ASCII (its ADR-4), and the pinned
// client writes what is not into something else — a carriage return or a line
// feed as a space, a space at either end trimmed off (nats.go v1.55.0,
// nats.go:1025-1037, `writeHeaderValue`). A header kept that way would be
// listed as bytes other than the ones handed over, so a backend refuses it
// before anything is kept, as the caller's mistake, and the twin refuses
// exactly the same headers.
func LetterHeader(header []byte) error {
	if len(header) == 0 {
		return nil
	}
	for i, b := range header {
		if b < ' ' || b > '~' {
			return fmt.Errorf("queue: a dead letter's header holds the byte %#02x at %d, "+
				"and a header is one line of printable ASCII", b, i)
		}
	}
	if header[0] == ' ' || header[len(header)-1] == ' ' {
		return errors.New("queue: a dead letter's header begins or ends with a space, " +
			"which the transport trims")
	}
	return nil
}

// Publisher is the write half of the queue.
//
// Declared here rather than in each consumer because several subsystems
// publish without ever subscribing, and a per-package copy of a one-method
// interface is one more place for the signature to drift from the thing it
// describes. EventQueue satisfies it, asserted below.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// EventQueue is the transport contract.
type EventQueue interface {
	// Publish sends an event to a topic. It must not return until the
	// event is persisted: callers rely on "published means durable".
	//
	// An event marked [Prioritize] and published to a laned topic is
	// written to the topic's priority lane ([SubjectFor]), where every
	// subscription on the topic takes it ahead of the topic's other mail.
	// The mark on an event published to any other topic changes nothing.
	//
	// It names the event's ORIGIN. An event whose Node is empty leaves
	// carrying the node this client was built for ([WithNode]), stamped
	// before any publish listener or consumer can see it; one that already
	// names a node — an event this node received and is handing on — keeps
	// it. The caller's own event is never written to: see [Options.Stamp].
	//
	// A nil event is REFUSED, and reaches no listener and no consumer.
	// There is nothing to send, and the encoding of nothing is not
	// nothing: a backend that published it delivered an event with no id
	// and no type to every consumer on the topic, where the memory twin
	// refused the same call — the one divergence a caller can never see
	// from its own side of the contract.
	Publish(ctx context.Context, topic string, ev *events.Event) error

	// Subscribe attaches a competing-consumer handler to topic/group.
	// Exactly one member of a group receives each message. On a laned topic
	// the handler is handed the lane's mail before the topic's.
	Subscribe(ctx context.Context, topic, group string, h Handler) error

	// SubscribeBatch attaches with batched, key-partitioned delivery:
	// drain what is locally available (plus a linger window), partition
	// by the key [BatchKeyFunc] derives preserving arrival order,
	// dispatch one handler call per partition oldest-partition-first,
	// and ack per partition. A failing partition never blocks or replays
	// a different one.
	//
	// On a laned topic a drain takes the priority lane's mail first, and a
	// partition holding a priority event is dispatched ahead of every other
	// ([OrderForDispatch]). The partition holds what the drain took: an event
	// of it deeper on the topic than the drain's cap is delivered in its own
	// turn once the topic reaches it, exactly once (the drain window is the
	// bound of what one turn is handed; see lane.go).
	//
	// opts is read live on every cycle, so a config reload changes
	// linger and batch size with no re-subscription.
	SubscribeBatch(ctx context.Context, topic, group string, h BatchHandler, key BatchKeyFunc, opts *BatchOptions) error

	// Quiesce stops taking NEW work on topic/group while staying
	// attached; a running handler finishes. Reports whether an
	// attachment existed. Idempotent.
	Quiesce(ctx context.Context, topic, group string) (bool, error)

	// Unquiesce resumes a quiesced attachment, reporting whether it was
	// quiesced. Required, not optional: a node whose lease store blipped
	// quiesces and must be able to come back, or it holds the seat,
	// stays attached, and consumes nothing for the rest of its life.
	//
	// Does NOT touch pause holds: a seat resuming from a stale-renew
	// window may still be legitimately held by another subsystem (the
	// engine's own is the park of a node with no turn engine).
	Unquiesce(ctx context.Context, topic, group string) (bool, error)

	// Detach closes this process's consumers, leaving the subscription.
	//
	// NON-DESTRUCTIVE, WHICH IS A CLAIM ABOUT THE MAIL AND NOT ABOUT THE
	// COST. The subscription, its cursor and everything it retains all
	// survive, so nothing is lost and whoever attaches next gets the lot,
	// including what was published while nothing was attached. That is
	// what makes a seat handoff cheap and an unowned seat safe.
	//
	// WHAT THIS DOES COST is one delivery of each message the consumer
	// had taken and not settled. Those go back the way every hand-back
	// goes back, because no backend here has a "return this without
	// counting it" — see DeliveriesLeft, which is the one place that rule
	// and the blockers that trigger it are written down, a detach among
	// them. The partitions a stopped drain never dispatched are charged
	// on the same terms as the one that stopped it. A message under a
	// RUNNING handler is the exception, and not because it is cheaper:
	// the handler runs to completion and its own outcome settles it.
	//
	// A message a detach returns is a HAND-BACK, so it returns at the head
	// as a deferral's does (see OutcomeDefer) — but a re-attaching
	// consumer races whatever is published while it starts, so nothing
	// above this package may depend on it being handled before a message
	// published after the detach. Event timestamps restore the order of
	// what ONE handler call carries, which is what OrderForDispatch is for
	// — and only that: a message returned behind its conversation's newer
	// mail is handled after it whenever the two are not drained together,
	// and no backend owes that they are.
	//
	// Releases this attachment's pause holds — a hold that outlived a
	// detach would leave a re-attaching node silently deaf.
	//
	// Detach does NOT wait for a running handler. It stops the
	// subscription taking new work and returns; a handler already
	// in-flight runs to completion and its outcome still applies.
	//
	// This is not a performance choice, it is the fenced-release path.
	// A node that has LOST a seat's lease must detach FIRST and abandon
	// whatever is in flight — the successor already owns the seat, and
	// blocking the release until a multi-minute turn finishes would keep
	// the seat unclaimable for exactly as long as the wrong node keeps
	// working on it. The VOLUNTARY path gets the other behaviour by
	// composing verbs it already has: quiesce, wait for in-flight, then
	// detach.
	Detach(ctx context.Context, topic, group string) (bool, error)

	// EnsureSubscription creates the durable subscription if absent,
	// with NO consumer attached, positioned at the earliest message.
	// Creating an existing subscription is success. On a laned topic it
	// creates the priority lane with it, and completes a [Subscription]
	// listed as Partial.
	//
	// The bool reports whether this call found it absent AND then
	// provisioned it. That is exact on a backend with one writer, and on
	// a fleet it is the closest thing to an answer there is: two nodes
	// that create the same subscription in the same instant both receive
	// it, because a broker that returns the object either way gives a
	// client no way to tell "I made this" from "this was already here".
	// So on a simultaneous boot more than one node can report true.
	//
	// Deliberately NOT a three-valued answer, and nothing in the engine
	// branches on it: it is a count in a log line. A tri-state nobody
	// reads would be machinery invented to describe a race the broker
	// does not expose, rather than an answer anyone could act on.
	EnsureSubscription(ctx context.Context, topic, group string) (bool, error)

	// DeleteSubscription destroys the subscription and its retained
	// mail, its priority lane's included. Must not require a local
	// consumer: decommissioning a role cannot depend on which node
	// happened to run the seat.
	//
	// It DETACHES THIS CLIENT FIRST, and a delete the broker does not
	// hear — ErrUnavailable — has still done that: the detach is this
	// node's own state and reaches no broker. What it leaves is the
	// mailbox standing with nothing of this node's attached, the state
	// every mailbox is in while no node holds its seat, which the retry
	// finishes; never this node still consuming a mailbox its caller
	// asked to destroy.
	DeleteSubscription(ctx context.Context, topic, group string) (bool, error)

	// ListSubscriptions reports every durable subscription whose topic
	// matches topicPattern (`*` one segment, `>` trailing segments, the
	// grammar SubscribeStream takes), as the exact pair it was created
	// with, in no promised order.
	//
	// EVERY one the broker holds, not this client's: one made through
	// EnsureSubscription, Subscribe or SubscribeBatch, by any client of
	// the broker, attached or not. A deleted subscription is not listed,
	// and neither is an ephemeral SubscribeStream subscription, which is
	// not a mailbox. A priority lane is part of its topic's subscription
	// and never listed as one of its own; a laned subscription whose lane
	// is missing is listed with Partial set. An empty pattern is refused rather than read as
	// "everything"; ">" asks for everything.
	//
	// It exists because a durable subscription outlives every record of
	// its name. A seat's mailbox is named after its handle, a removed
	// seat's handle is gone from the org every node derives names from,
	// and a registry written beside the broker can miss one (a write that
	// failed, a mailbox older than the registry). Without a listing, a
	// mailbox that escaped the registry retains its mail for the life of
	// the deployment and nothing can ever find it.
	ListSubscriptions(ctx context.Context, topicPattern string) ([]Subscription, error)

	// Waiting reports the mail a subscription holds that no handler of THIS
	// client is running on, per mailbox of a laned topic ([Waiting]): what
	// is waiting for the subscription's consumer, whether it was never
	// delivered, was fetched in a drain and not yet dispatched, or was
	// handed back. The mail a handler of this client is running on is not
	// waiting and is not counted, which is why the count is asked of the
	// client that holds the subscription's attachment: any other client
	// counts that mail too.
	//
	// A DISPLAY, NOT A LEDGER. A backend may answer from consumer state
	// that trails a publish by a moment; nothing may decide delivery on it.
	// A subscription that does not exist waits for nothing.
	Waiting(ctx context.Context, topic, group string) (Waiting, error)

	// SubscribeStream creates an ephemeral per-caller broadcast
	// subscription — every subscriber receives every matching event.
	// topicPattern supports `*` (one segment) and `>` (trailing
	// segments).
	SubscribeStream(ctx context.Context, topicPattern string, h StreamHandler) (Unsubscribe, error)

	// AddPublishListener registers a listener called inline on every
	// publish.
	AddPublishListener(l PublishListener)

	// Ask scatters one request to every process serving subject and
	// collects the replies, returning when want of them have arrived or
	// when ctx is done, whichever comes first. want of 0 waits out the
	// deadline, which is what a caller that does not know how many
	// answerers exist has to do.
	//
	// EPHEMERAL, and that is why it is a verb of its own rather than a
	// Publish and a Subscribe. Nothing here is retained, redelivered or
	// recorded: no stream, no consumer group, no ack, no publish
	// listener, no event-store row. A request nobody serves is a request
	// that never existed, and a reply that arrives after its asker left
	// is dropped rather than held for the next one. The durable verbs are
	// the wrong shape for a query: a search fan-out that wrote two
	// records and an audit row per keystroke would make the audit log a
	// function of how often somebody typed.
	//
	// FEWER REPLIES THAN want IS NOT AN ERROR. A peer that is slow, gone,
	// or simply not serving this subject is the ordinary case, and only
	// the caller knows what a missing answer costs it — an error would
	// force every caller to unwrap one to find out how many it got. An
	// error means the ask could not be made at all — never a request
	// handed to the broker whose delivery could not be confirmed, which
	// may have reached a server: a caller reads an error as "nobody
	// received it", and acts on that by asking again.
	//
	// A BROKER THIS NODE CANNOT REACH IS REFUSED BEFORE ANYTHING IS SENT,
	// with ErrUnavailable — never written into a client's reconnect buffer
	// to go out once the connection is back. A buffered request is a
	// RETAINED one, which is the one thing this verb promises never to
	// be, and it would reach its servers after its asker had been told
	// the ask failed: a steer note delivered to a turn its sender was
	// told nothing reached. What stays possible is the instant between
	// that check and the send, in which a connection that drops can still
	// carry the request out later — so a request must be safe to receive
	// once its asker has gone, as every reply to a vanished mailbox
	// already is.
	//
	// THE REPLIES ARE UNORDERED and carry no sender. What a reply means
	// is inside its own bytes, because the transport cannot say: a
	// scatter has no roster, so "who did not answer" is a question only
	// the payload can answer.
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)

	// AskEach is [EventQueue.Ask] with each reply handed to each AS IT
	// ARRIVES rather than collected for the end, until ctx is done or
	// each answers false. Everything Ask promises holds: nothing is
	// retained, fewer replies than hoped for is not an error, a broker
	// this node cannot reach is refused before anything is sent, and the
	// replies are unordered and carry no sender.
	//
	// IT EXISTS FOR A CALLER THAT ACTS ON EACH ANSWER: one that waits for
	// none in particular (a scatter with no roster) would otherwise wait
	// out its whole budget before it could act on the fastest reply. each
	// runs on the asker's goroutine, one reply at a time, and an ask lasts
	// no longer than each lets it.
	AskEach(ctx context.Context, subject string, request []byte, each func(reply []byte) bool) error

	// Serve makes this process one of subject's answerers.
	//
	// EVERY SERVER OF A SUBJECT SEES EVERY REQUEST — the opposite of
	// Subscribe's competing consumers, and the reason this is not a
	// consumer group: a scatter divides work by what the request NAMES,
	// so a broker that handed each request to one member would divide it
	// twice and cover a fraction.
	//
	// An answerer that returns an error answers NOTHING. Its asker sees a
	// server that did not answer, which is the same fact as a server that
	// was not running — see AnswerFunc.
	//
	// ANSWERS RUN CONCURRENTLY, up to answers of them at once, and a
	// request beyond that waits for a slot rather than failing, as far as
	// its backend buffers it (below). One slow answer must never hold the
	// next request behind it: an answerer serves every asker in the fleet,
	// and a registration that answered one at a time made each of them wait
	// for the sum of everybody else's latency.
	//
	// A WAITING REQUEST IS HELD IN A BUFFER ITS BACKEND BOUNDS, and dropped
	// past it. The real backend's NATS client holds a subscription's
	// pending requests to 64 MiB and 500 000 messages (nats.go's defaults,
	// which this package leaves as they are) and drops a request past them
	// as a slow consumer: logged on the serving node, naming no subject,
	// and heard by its asker as no answer at all, at its deadline. The
	// in-memory twin holds every one. Work done now drains its slots long
	// before that bound; a registration whose requests are LARGE and whose
	// answers hold their slots through long waits does not, so it bounds
	// its own backlog — it answers a request past what it holds AT ONCE,
	// in its own words for "not now", and never leaves it to wait where it
	// can be dropped unheard (internal/estate's engine writes' lane).
	//
	// THE BOUND IS THE REGISTRATION'S OWN, and so are its slots: a
	// request waiting for one waits only behind its own registration's
	// answers, never another's. Only the answerer knows what an answer
	// waits on — [ConcurrentAnswers] for one that does its work now, the
	// bound of its own queue for one that waits in it — and a slot held
	// through a wait is one every other request of the registration
	// waits behind, so work that waits and work that does not are served
	// on registrations of their own. A bound below one is refused
	// ([ErrAnswerBound]): a registration that may answer nothing would
	// hold every request it is sent until its askers gave up.
	//
	// The returned Unsubscribe cancels the answerer's context and then
	// WAITS for the answers in flight, bounded by the context it is given
	// — so a caller that withdraws and then closes what the answerer
	// reads never closes it under an answer still reading. A bound that
	// ends first is answered with an error wrapping that context's own,
	// never with success: the answers are still running.
	//
	// RETURNING MEANS THIS PROCESS'S OWN SERVER HOLDS THE REGISTRATION,
	// and on a clustered broker no more than that: the other servers learn
	// of it over their routes a moment later, so an asker on another server
	// that scatters in that moment does not reach this answerer and counts
	// it as one that did not answer — the same honest fact a scatter reports
	// about a node that has not started. Nothing that needs every answerer
	// may assume it from Serve's return; it asks, and reads who answered.
	Serve(ctx context.Context, subject string, answers int, h AnswerFunc) (Unsubscribe, error)

	// Broadcast hands payload to every process listening on a pattern
	// that matches subject, and returns.
	//
	// EPHEMERAL, the fan-out counterpart of Ask: no stream, no consumer,
	// no ack, no retention, no publish listener, no event-store row. A
	// message nobody is listening for never existed, and a listener that
	// starts a moment later never hears it. It exists for what is worth
	// nothing once superseded, presence above all: a cursor or a
	// heartbeat on a durable verb would be a replicated write per
	// movement, kept for a retention window nobody reads back.
	//
	// AT MOST ONCE AND UNORDERED ACROSS PUBLISHERS. A listener that falls
	// behind loses messages rather than holding the publisher, and two
	// processes broadcasting on one subject are heard in whatever order
	// the broker delivered them. What a message means, and whether a later
	// one supersedes it, is inside its own bytes.
	//
	// ONE PUBLISHER'S ORDER IS KEPT: what one process broadcasts, each call
	// returning before the next is made, every listener hears in the order
	// it was sent, of what it hears at all — the source order a broker
	// keeps per publisher, and the twin's per listener. So a later
	// broadcast of the same sender is a FENCE: a listener that has heard
	// it has heard everything of that sender's it ever will.
	//
	// A BROKER THIS NODE CANNOT REACH IS REFUSED BEFORE ANYTHING IS SENT,
	// with ErrUnavailable, for Ask's reason: a message written into a
	// reconnect buffer is a retained one, delivered after its moment. The
	// subject is a LITERAL (see [LiteralSubject]): a pattern is what a
	// listener names, and a broker refuses a publish to one.
	Broadcast(ctx context.Context, subject string, payload []byte) error

	// Listen makes this process a listener on pattern (`*` one token,
	// `>` the trailing tokens) until the returned Unsubscribe runs.
	//
	// EVERY LISTENER HEARS EVERY MATCHING MESSAGE, Serve's rule rather
	// than Subscribe's: a broadcast is addressed to whoever is present, so
	// a group that handed each message to one member would show each room
	// to a fraction of the fleet.
	//
	// ONE MESSAGE AT A TIME PER LISTENER, in the order this process
	// received them, on a goroutine of the backend's own: a handler never
	// races itself, and never runs on the publisher's goroutine. A handler
	// that panics loses that message and keeps its registration.
	//
	// The returned Unsubscribe stops delivery and then WAITS for a handler
	// already running, bounded by the context it is given, so a caller
	// that withdraws and then tears down what the handler reads never
	// tears it down under a message still being handled. A bound that ends
	// first is answered with an error wrapping that context's own, never
	// with success: the handler is still running. A handler must
	// therefore never withdraw its own registration.
	//
	// RETURNING MEANS THIS PROCESS'S OWN SERVER HOLDS THE REGISTRATION,
	// and no more, exactly as for Serve.
	Listen(ctx context.Context, pattern string, h ListenFunc) (Unsubscribe, error)

	// KeepDeadLetter keeps body on subject under header, which must be a
	// literal subject in the dead-letter namespace (topics.DeadLetter names
	// one; any other is refused, unmarked, as the caller's mistake), until
	// the backend's dead-letter retention lets it go ([Kept.Expires]) or
	// DropDeadLetter removes it.
	//
	// FOR A READER THAT SETTLES ITS OWN DELIVERIES, never for a subscription
	// of this contract: a subscription is dead-lettered by its backend, whose
	// delivery budget decides it. A wake feed reads a state log's own durable
	// consumer and decides for itself when a record has circled long enough
	// (internal/changefeed), so what it gives up on is kept here — in the
	// same place, under the same retention, as every other dead letter, and
	// listed by the same verb.
	//
	// THE HEADER AND THE BODY ARE KEPT APART: the header is what every
	// listing answers ([Kept.Header]) and the body what only
	// DeadLetterFrame reads, so a keeper puts in the header what a reader
	// decides by — a line of printable ASCII ([LetterHeader]; any other is
	// refused, unmarked) — and in the body the bytes only a reader acting
	// on one entry needs. Both are the caller's bytes, verbatim, never an
	// event: no publish listener hears them and no event-store row is
	// written for them. RETURNING MEANS IT IS DURABLE, as for Publish, and
	// a header and body the transport cannot carry together
	// ([LetterFraming]) are refused with ErrTooLarge, which no retry
	// changes.
	KeepDeadLetter(ctx context.Context, subject string, header, body []byte) error

	// DeadLettersOn lists every entry subject holds, oldest first —
	// whoever kept it, a subscription's backend or a KeepDeadLetter — each
	// with the sequence DropDeadLetter names it by, when it was kept, when
	// the backend lets it go and the header it was kept under: NEVER ITS
	// BODY, which moves for no listing, however large. An empty answer
	// with no error is a subject that holds none.
	//
	// A COPY'S ANSWER, which may be BEHIND the broker's own: a broker that
	// keeps several copies of the dead letters may list them from any one,
	// so a listing can miss an entry kept a moment ago or still show one
	// dropped a moment ago — though it never hides an entry the copy it
	// read holds. A reader that has to know its listing is current holds it
	// to DeadLetterMarks, which is the broker's own: a listing whose count
	// of a subject is the marks' count is the subject as the broker holds it.
	DeadLettersOn(ctx context.Context, subject string) ([]Kept, error)

	// DeadLetterFrame is the body the entry seq of subject was kept with,
	// exactly as it was handed to KeepDeadLetter — for a wake feed's entry,
	// the record's sealed frame — and whether subject holds the entry at
	// all.
	//
	// THE ONE VERB THAT MOVES A BODY, for the one reader that needs it: the
	// reader acting on that entry, as a feed's replay reopens one record.
	// It reads what the broker holds now, never a copy behind it.
	//
	// AN ENTRY ALREADY GONE is answered as not held, with no error —
	// dropped by a peer, or let go by the retention since it was listed.
	// A seq holding an entry on ANOTHER subject is refused, unmarked, and
	// answers nothing of it, as DropDeadLetter refuses it; so is a seq of
	// zero.
	DeadLetterFrame(ctx context.Context, subject string, seq uint64) ([]byte, bool, error)

	// DeadLetterMarks counts what every dead-letter subject under under
	// holds — under is a literal subject in the dead-letter namespace, and
	// the subtree every subject `under.…` — and answers beside the counts
	// the last sequence the backend gave any dead letter ([Marks]).
	//
	// THE BROKER'S OWN ANSWER, READ-YOUR-OWN-WRITE on every client: an entry
	// whose KeepDeadLetter returned is counted and has moved Last, and one
	// whose DropDeadLetter returned is not counted. It costs one request
	// however many entries the subtree holds, so a reader that keeps a
	// listing asks it on every beat and lists again only when it moved.
	DeadLetterMarks(ctx context.Context, under string) (Marks, error)

	// DropDeadLetter removes the entry seq from subject.
	//
	// AN ENTRY ALREADY GONE IS NO ERROR — dropped by a peer, or let go by
	// the retention — because what the caller asks is that it be gone: two
	// nodes that both finished with one entry both succeed. A seq holding an
	// entry on ANOTHER subject is refused, unmarked, and removes nothing, so
	// a caller that names the wrong subject cannot drop somebody else's; so
	// is a seq of zero, which names no entry on any backend.
	DropDeadLetter(ctx context.Context, subject string, seq uint64) error

	// Start connects the backend and begins consuming.
	//
	// WHETHER A BACKEND REQUIRES IT IS ITS OWN ANSWER, and both shipped
	// answers are legitimate: on JetStream, Open establishes the
	// connection and the streams, so Start is a no-op and a publish
	// before it works; on the in-memory twin, Start is what makes the
	// client live.
	//
	// What a backend may NOT do is answer DIFFERENTLY PER VERB. Before
	// Start, all twenty-one of the publish, subscription, attachment,
	// ephemeral and dead-letter verbs must give the same answer: Publish,
	// Subscribe, SubscribeBatch, Quiesce, Unquiesce, Detach,
	// EnsureSubscription, DeleteSubscription, ListSubscriptions,
	// SubscribeStream, PauseTopic, ResumeTopic, Serve, Ask, Broadcast,
	// Listen, KeepDeadLetter, DeadLettersOn, DeadLetterFrame,
	// DeadLetterMarks and DropDeadLetter. Either they all refuse, or none
	// of them require it. Capabilities.RequiresStart says which, and
	// queuetest sends all twenty-one to hold a backend to one answer.
	//
	// A caller cannot reason about a lifecycle whose rules change per
	// method, and this is not hypothetical: a twin that refused Publish
	// and Subscribe while PauseTopic still took a hold left that hold to
	// survive into the next life, and the restarted queue reported itself
	// running and was silently deaf.
	//
	// AFTER Stop the answer is not a capability at all — see Stop, which
	// is where the asymmetry is written up.
	//
	// The DRAIN protocol is exempt along with Start and Stop, because it
	// exists to run around a stop rather than in spite of one:
	// PauseDelivery, WaitForHandlers and InFlightCount answer on a
	// stopped queue, and must — a drain that could not report its own
	// in-flight count once the queue was down would have no way to say
	// whether it finished. Backend and AddPublishListener return no error
	// and so have nothing to refuse with.
	Start(ctx context.Context) error

	// InFlightCount reports handler invocations currently mid-flight —
	// the number an operator watches converge to zero during a drain.
	InFlightCount() int

	// Backend is a stable lowercase backend name for operator display.
	// Nothing may branch behaviour on it.
	Backend() string

	// PauseDelivery stops dispatching new events to handlers while
	// leaving Publish working, so in-flight handlers can emit terminal
	// events. One-way: once paused, the engine is shutting down.
	PauseDelivery(ctx context.Context) error

	// PauseTopic pauses ONE subscription's delivery under a named
	// reason. Holds are reason-scoped and keyed by the (topic, group)
	// PAIR, so a second subsystem gating the same inbox cannot release
	// the first one's hold by lifting its own, and a hold on one group
	// does not gate every other group on a shared subject. The engine
	// holds a seat's inbox under four, each lifted only by the subsystem
	// that took it: a company with no turn engine, a person's pause, a
	// spent budget window, and a detached executor holding the seat
	// (or an answer's resume it owes) — see internal/agent/inbox's Hold
	// and the engine's budget park.
	PauseTopic(ctx context.Context, topic, group, reason string) error

	// ResumeTopic releases one reason's hold, flushing when none remain.
	ResumeTopic(ctx context.Context, topic, group, reason string) error

	// WaitForHandlers waits for in-flight handlers, returning how many
	// were still running when the wait ended. Zero means a clean drain;
	// non-zero means the timeout expired, which is not an error — the
	// caller owns any "too long" policy. Always PauseDelivery first.
	WaitForHandlers(ctx context.Context, timeout time.Duration) (int, error)

	// Stop closes the connection.
	//
	// AFTERWARDS EVERY ONE OF THE TWENTY-ONE VERBS REFUSES. A requirement
	// rather than a capability, and the asymmetry with the pre-Start rule
	// is the point: "not connected yet" is a state a backend may
	// legitimately not have — JetStream's Open connects, so there is
	// nothing before Start — while "closed" is a state every backend
	// genuinely reaches, and a verb that mutates through a closed client
	// is wrong on all of them.
	//
	// It is wrong in two different ways, and both were measured. On the
	// twin, PauseTopic after Stop took a hold that outlived the client and
	// left the next life silently deaf. On JetStream, Quiesce, Unquiesce,
	// Detach, PauseTopic and ResumeTopic all returned success on a closed
	// connection while Publish and Subscribe returned "nats: connection
	// closed" — so a shutdown path could believe it had gated a
	// subscription that no longer existed.
	//
	// Whether Start can revive a stopped queue is a separate question the
	// contract does not answer; see Capabilities.Restartable.
	Stop(ctx context.Context) error
}

// --- batch options --------------------------------------------------------

// BatchOptions carries the live-mutable knobs for batched delivery.
//
// The consume loop re-reads these for every batch, at the moment the batch's
// FIRST event is in hand — which is also the moment its linger window opens —
// so a hot config reload takes effect on the next batch with no
// re-subscription. Not when a cycle starts: a cycle on an idle subscription
// starts by waiting for that first event, and options read before the wait
// applied a reload that landed during it to the batch after the next one.
//
// Mutable and read concurrently is a data race unless it is guarded, so it
// is: this is safe to write from another goroutine while a loop is reading it.
type BatchOptions struct {
	mu            sync.RWMutex
	lingerSeconds float64
	maxBatch      int
}

// NewBatchOptions builds options with the given window and cap, clamped on
// read through the Effective accessors.
func NewBatchOptions(lingerSeconds float64, maxBatch int) *BatchOptions {
	return &BatchOptions{lingerSeconds: lingerSeconds, maxBatch: maxBatch}
}

// DefaultBatchOptions is the engine's default: no linger (still drains
// everything already locally available, so backlog accumulated while a
// previous handler ran coalesces at zero added latency) and 20 events.
func DefaultBatchOptions() *BatchOptions { return NewBatchOptions(0, 20) }

// Set replaces both knobs atomically.
func (o *BatchOptions) Set(lingerSeconds float64, maxBatch int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lingerSeconds, o.maxBatch = lingerSeconds, maxBatch
}

// EffectiveLinger is the linger window clamped to [0, MaxLingerSeconds].
//
// The window is fixed — measured from the first message of a drain, not
// sliding — so a steady trickle cannot delay dispatch unboundedly.
func (o *BatchOptions) EffectiveLinger() time.Duration {
	o.mu.RLock()
	defer o.mu.RUnlock()
	v := o.lingerSeconds
	if v < 0 {
		v = 0
	}
	if v > MaxLingerSeconds {
		v = MaxLingerSeconds
	}
	return time.Duration(v * float64(time.Second))
}

// EffectiveMaxBatch is the per-cycle cap, at least 1. A pathological backlog
// is delivered as successive capped batches rather than one unbounded one.
func (o *BatchOptions) EffectiveMaxBatch() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.maxBatch < 1 {
		return 1
	}
	return o.maxBatch
}

// --- partitioning and dispatch ordering -----------------------------------

// Partition is one partition key's slice of a drained batch.
type Partition[T any] struct {
	Key   string
	Items []T
}

// PartitionByKey groups items by key, preserving arrival order both between
// partitions (first-arrival order) and within each.
//
// DISPATCH order is a separate policy — see OrderPartitionsOldestFirst. A
// key function that panics falls back to a unique per-event key, because key
// derivation must never block delivery.
func PartitionByKey[T any](items []T, key BatchKeyFunc, eventOf func(T) *events.Event) []Partition[T] {
	index := map[string]int{}
	var out []Partition[T]
	for _, item := range items {
		ev := eventOf(item)
		k := safeKey(ev, key)
		if i, ok := index[k]; ok {
			out[i].Items = append(out[i].Items, item)
			continue
		}
		index[k] = len(out)
		out = append(out, Partition[T]{Key: k, Items: []T{item}})
	}
	return out
}

func safeKey(ev *events.Event, key BatchKeyFunc) (k string) {
	defer func() {
		if r := recover(); r != nil {
			contained := panics.Recovered(panics.ScopeQueue, r)
			log.Error("batch_key_failed", "event_type", eventType(ev), "panic", r,
				"stack", contained.Stack)
			k = fallbackKey(ev)
		}
	}()
	if key == nil {
		return fallbackKey(ev)
	}
	if k = key(ev); k == "" {
		return fallbackKey(ev)
	}
	return k
}

func fallbackKey(ev *events.Event) string {
	if ev == nil {
		return "event:"
	}
	return "event:" + ev.ID.String()
}

func eventType(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	return ev.Type
}

// OrderForDispatch establishes BOTH levels of ordering a drained batch
// needs, and is the one place either is decided.
//
// Between partitions: a partition holding a priority event ([Prioritized])
// first, then oldest constituent event first within each of the two classes.
// The priority class is what a laned topic's drain exists for: the lane's
// mail is taken first so that a person waiting on a seat is answered before
// the seat's background work, and a drain that took both and dispatched the
// older background partition first would hand that back. Oldest first within
// the class is what it always was. Receive order alone
// starves a quiet partition behind a hot one under deferral — the quiet
// partition's requeued copies re-enter the topic AFTER whatever arrived
// during the hot one's turn, so receive-ordered dispatch picks the hot one
// on every drain. Timestamps carry the aging signal, so the partition that
// has waited longest dispatches first.
//
// Within a partition: event timestamp, not delivery order. This is what
// makes a partition read correctly however redeliveries interleave with fresh
// arrivals — a hand-back (a deferral, a hold, a stop) returns at the head and
// a FAILED or PASSED delivery is withheld for its redelivery backoff behind
// never-delivered mail (see OutcomeNak, OutcomePass and OutcomeDefer), and a
// drain that carries both a returned message and newer ones has them in that
// order, not in the order they were written. Relying on the timestamps the
// engine already trusts removes a correctness dependency on how one drain
// happened to be assembled.
//
// IT ORDERS ONE HANDLER CALL, AND NOTHING ACROSS TWO. A drain is what was
// available when it opened plus what arrived inside its linger window, so a
// failed delivery still waiting out its backoff is not in the drain that
// carries its conversation's newer events: on the shipped broker the backoff
// is a second, doubling to thirty, against a default window of zero, so a
// newer event of the same conversation that arrives meanwhile is dispatched
// FIRST, in a call of its own, and the failed one follows in a later one.
// No sort reaches a call that has already returned, and the contract
// deliberately does not hold a conversation's newer mail back behind a
// failure instead. That would stall the conversation for the whole backoff
// schedule, hand a poison message's failures to the healthy ones held back
// with it (a partition takes one outcome, so every failure is charged to every
// message in it), and still not be a guarantee: the hold would be one
// attachment's memory, gone at a seat handoff, and a group with two members
// redelivers to either. A consumer that cares whether an event is older than
// one it already handled has the timestamp to ask.
//
// Both levels are stable sorts, so ties keep arrival order, and both fall
// back to arrival order rather than failing: ordering is a fairness and
// readability policy, and must never block delivery.
func OrderForDispatch[T any](parts []Partition[T], eventOf func(T) *events.Event) []Partition[T] {
	out := slices.Clone(parts)
	oldest := make(map[string]time.Time, len(out))
	urgent := make(map[string]bool, len(out))
	for i, p := range out {
		items := slices.Clone(p.Items)
		slices.SortStableFunc(items, func(a, b T) int {
			ea, eb := eventOf(a), eventOf(b)
			if ea == nil || eb == nil {
				return 0
			}
			return ea.Timestamp.Compare(eb.Timestamp)
		})
		out[i].Items = items

		var min time.Time
		for _, item := range items {
			ev := eventOf(item)
			if ev == nil {
				continue
			}
			if Prioritized(ev) {
				urgent[p.Key] = true
			}
			if min.IsZero() || ev.Timestamp.Before(min) {
				min = ev.Timestamp
			}
		}
		oldest[p.Key] = min
	}
	slices.SortStableFunc(out, func(a, b Partition[T]) int {
		if ua, ub := urgent[a.Key], urgent[b.Key]; ua != ub {
			if ua {
				return -1
			}
			return 1
		}
		ta, tb := oldest[a.Key], oldest[b.Key]
		switch {
		// A partition with no comparable timestamp sorts LAST, and
		// stably among its own kind — so it keeps arrival order relative
		// to the others like it, and the stamped partitions still age
		// against each other.
		//
		// Answering 0 whenever EITHER side was unstamped read as "these
		// two are equal" and was not an ordering at all: it is not
		// transitive, so one unstamped partition anywhere in a drain made
		// the sort leave unrelated stamped partitions in arrival order,
		// silently switching aging off for the whole drain. An event with
		// no timestamp is a bug upstream; the fairness policy has to keep
		// working for everything around it either way.
		case ta.IsZero() && tb.IsZero():
			return 0
		case ta.IsZero():
			return 1
		case tb.IsZero():
			return -1
		case ta.Before(tb):
			return -1
		case tb.Before(ta):
			return 1
		default:
			return 0
		}
	})
	return out
}

// LogResult emits the standard structured line for a single delivery's
// outcome. An Ack logs nothing: the normal case is the overwhelming majority
// of deliveries, and a line per success buries the ones that matter.
//
// A PASS IS NO FAILURE, and is never logged as one: it is a delivery this node
// hands to a peer that can read it, a roll's ordinary minutes, and a
// `handler_failed` at Warn for each would read as a fleet failing its mail. It
// is logged at Info under its own name, with its reason and what the node met.
//
// UNDER ctx, because the line is correlated by the trace a context carries
// (internal/logging), and a backend that logs under its handler's context
// would otherwise spell the line itself to keep that: the in-memory twin did,
// and logged a partition's deferral under a single delivery's name.
func LogResult(ctx context.Context, l *slog.Logger, topic, group string, ev *events.Event, r Result) {
	switch r.Outcome {
	case OutcomeNak:
		l.WarnContext(ctx, "handler_failed", "topic", topic, "group", group,
			"event_type", eventType(ev), "error", errText(r.Err))
	case OutcomeDefer:
		l.InfoContext(ctx, "delivery_deferred", "topic", topic, "group", group,
			"event_type", eventType(ev), "reason", r.Reason)
	case OutcomePass:
		l.InfoContext(ctx, "delivery_passed", "topic", topic, "group", group,
			"event_type", eventType(ev), "reason", r.Reason, "error", errText(r.Err))
	}
}

// LogBatchResult emits the standard line for one partition's outcome, under
// ctx for [LogResult]'s reason.
//
// A distinct event name from LogResult, carrying the partition key and the
// partition size, because the two failures are operationally different
// things: one delivery failing is a bad event, a whole partition failing is
// a bad turn. A log consumer must be able to tell them apart without
// parsing, and every backend must emit the same name for the same situation
// — which is why this lives in the contract rather than in each backend.
func LogBatchResult(ctx context.Context, l *slog.Logger, topic, group, batchKey string, evs []*events.Event, r Result) {
	var head *events.Event
	if len(evs) > 0 {
		head = evs[0]
	}
	switch r.Outcome {
	case OutcomeNak:
		l.WarnContext(ctx, "batch_handler_failed", "topic", topic, "group", group,
			"batch_key", batchKey, "event_count", len(evs),
			"event_type", eventType(head), "error", errText(r.Err))
	case OutcomeDefer:
		l.InfoContext(ctx, "batch_delivery_deferred", "topic", topic, "group", group,
			"batch_key", batchKey, "event_count", len(evs),
			"event_type", eventType(head), "reason", r.Reason)
	case OutcomePass:
		l.InfoContext(ctx, "batch_delivery_passed", "topic", topic, "group", group,
			"batch_key", batchKey, "event_count", len(evs),
			"event_type", eventType(head), "reason", r.Reason, "error", errText(r.Err))
	}
}

// LogListenerPanic emits the standard line for a publish listener that
// panicked and was recovered, and [LogStreamHandlerPanic] the one for a
// stream handler.
//
// THE PANIC IS READ HERE, not passed in, and that works because these are
// called from inside the deferred function that recovered: a deferred call
// runs before its frame is popped, so [panics.Recovered] still walks down
// through runtime.gopanic into the function that actually panicked. Reading it
// at the recover site and threading it through would be identical minus one
// frame, and minus the chance a caller forgets.
//
// # Why these live in the contract
//
// The same reason [LogBatchResult] does, and here it is not hypothetical:
// the backends had drifted into different spellings of one situation.
// `memory` logged `publish_listener_failed` with the recovered value under
// `error`; the broker-backed side logged `publish_listener_panicked` with
// it under `panic`. The stream side was worse — it keyed the topic as
// `subject` while `memory` keyed it as `topic` and added a `topic_pattern`
// nobody else emitted. An operator grepping `publish_listener_panicked` saw
// every backend but the in-memory twin, and the twin is what the tests run
// on, so nothing caught it.
//
// # `panic` rather than `error`
//
// A recovered value is not an error. It is whatever was passed to panic(),
// carries no Error() method in general, and the distinction is exactly what
// tells an operator the callback CRASHED rather than returned badly — two
// different bugs in two different places.
//
// Error level, not Warn: unlike a Nak there is no redelivery behind this.
// The listener's work for this event is simply gone.
func LogListenerPanic(l *slog.Logger, topic string, ev *events.Event, r any) {
	contained := panics.Recovered(panics.ScopeQueue, r)
	l.Error("publish_listener_panicked", "topic", topic,
		"event_type", eventType(ev), "panic", r, "stack", contained.Stack)
}

// LogStreamHandlerPanic emits the standard line for a stream handler that
// panicked and was recovered. See [LogListenerPanic] for why it is here.
//
// The topic key is `topic`, matching [StreamHandler]'s own parameter name
// and every other line in this file. Two backends called it `subject`,
// which is NATS's word for the same thing and a word this contract does not
// use.
func LogStreamHandlerPanic(l *slog.Logger, topic string, ev *events.Event, r any) {
	contained := panics.Recovered(panics.ScopeQueue, r)
	l.Error("stream_handler_panicked", "topic", topic,
		"event_type", eventType(ev), "panic", r, "stack", contained.Stack)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// EventQueue is a Publisher. A compile-time assertion rather than a
// convention, because the narrow interface exists only to be satisfied by
// this one.
var _ Publisher = (EventQueue)(nil)
