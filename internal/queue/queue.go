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
//   - Handlers have five outcomes, not two. Ack, Nak (a failure), Defer
//     (hand it back and stop consuming: a condition of the node), Pass
//     (hand it back for a peer and go on consuming: this node will not
//     handle this message, another may) and Aside (keep it unacknowledged
//     under a key until Readmit, and go on consuming) — see Result. The
//     returns keep different places, on every backend: a Nak and a Pass come
//     back BEHIND the mail that was waiting, a Defer and a readmitted Aside at
//     the HEAD. A handler whose message must keep its place does not Nak.
//   - AN ASIDE IS SET ASIDE IN FLIGHT, never copied or republished: the
//     delivery stays the backend's, kept from timing out, its count honest,
//     until Readmit hands every delivery of its key back at the head of the
//     next drain (a busy seat's mail waiting on one of its runs, ADR-0169). A
//     Readmit landing while the handler runs readmits what it then sets aside,
//     so none is lost to that race; a release or a stop hands aside mail back
//     as fetched-and-undispatched mail is, one delivery spent; a crash
//     redelivers it once its ack window lapses. At most [MaxAside] at once.
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
//   - A subscription's mail can be READ WITHOUT CONSUMING IT (Backlog): lane
//     first, at most a limit, no delivery made or spent and no hold touched,
//     possibly with mail already acked. A busy seat's router reads its held
//     inbox so (internal/engine, ADR-0167), which is why it is contract and
//     not a test's inspection.
//   - DEAD LETTERS ARE ONE PLACE with one retention, whoever gives up on a
//     message: a subscription's backend when its budget runs out, or a
//     reader that settles its own deliveries and keeps what it gives up on
//     (KeepDeadLetter — a wake feed, which reads a state log's consumer
//     rather than a subscription), under an IDENTITY its keeper derives
//     from what it keeps, so a second keep of one thing within
//     DeadLetterWindow is answered "already kept" rather than kept twice. An
//     entry is a BODY and a one-line HEADER kept apart, so a reader that only needs to know what is kept
//     never moves what it keeps: a subject's entries are listed by their
//     headers alone (DeadLettersOn), one entry's body is read by its
//     sequence (DeadLetterFrame), what a whole subtree of subjects holds is
//     counted without listing it (DeadLetterMarks), and an entry is removed
//     one at a time (DropDeadLetter). A subscription's entry is the
//     message's copy with no header, kept by whether the message failed as
//     itself at any of its deliveries (RetiredTo): one that did on the
//     pair's dead-letter subject, poison, and every other — passed, handed
//     back, failed only for a reason of the node's own — on its UNHANDLED
//     subject, a healthy delivery no member handled, where a reader that
//     can handle it now finds it again.
//   - A delivery that failed for a reason of THIS NODE's is answered NakNode
//     (a broker it could not reach, a queue that is not live, a stop that
//     ended its handler, a coordination store that did not answer a step the
//     handler cannot go on without) and is never counted as the message's
//     own failure. NodeFault is the one test of such a failure the queue
//     itself can tell (a broker that did not answer, a queue no longer live,
//     a handler whose context ended), and every handler that asks it
//     (internal/notify's wakes, internal/engine's seat control topic and
//     seat mailbox, internal/observe's custody keeper) asks the same one, so
//     a failure one of them keeps out of its group's poison the others keep
//     out too.
//   - A PAUSE HOLD IS TAKEN FOR ONE ATTACHMENT, which its caller names
//     (Attachment): the pair's attachment from one Detach to the next, live
//     or about to be made, minted per client from one counter, the zero one
//     naming none. A hold taken before anything attaches gates the attach it
//     was taken for; a hold asked for an attachment a Detach has ended is
//     refused (ErrAttachmentGone), and a ResumeTopic under one lifts nothing,
//     so it never gates the attachment after it. Keyed by the pair alone, a
//     hold whose PauseTopic landed after its seat's release was carried onto
//     the next attachment of that inbox on the node, which then consumed
//     nothing, and nothing that knew of the hold was left to lift it.
//     queuetest's a_hold_taken_for_a_detached_attachment_does_not_pause_the_next
//     holds both backends to it.
//   - A failed verb is MARKED with one of four sentinels when the failure is
//     one a caller acts on, and nothing else about it may be branched on:
//     ErrTooLarge (the event can never fit — permanent), ErrNotLive (this
//     queue consumes and answers nothing until it is started again),
//     ErrUnavailable (the broker did not answer — a CONDITION, which the
//     same call made again in a moment may clear) and ErrAttachmentGone (a
//     hold named an attachment that has ended). An unmarked failure is
//     none of those: the caller's own mistake, or a broker answer that
//     waiting does not change.
//
// # Scatter and fan-out verbs
//
// Four verbs are the opposite of every other. Ask, AskEach and Serve are an
// EPHEMERAL scatter (core request/reply on the real backend: no stream, no
// consumer, no ack, no retention) because a query is not an event, and a
// search fan-out on the durable verbs would write two records and an audit
// row per keystroke. Broadcast and Listen are an ephemeral fan-out (at most
// once, every listener hears every message, one at a time per listener and in
// one sender's order, refused with ErrUnavailable rather than buffered into a
// reconnect); presence travels on them because a heartbeat on a durable verb
// would be a replicated write per cursor move.
//
// An answerer answers concurrently, up to the bound its registration states
// through ONE AnswerGate both backends share, one gate per registration so a
// request waits only behind its own registration's answers. A REQUEST'S
// ARRIVAL IS STAMPED BEFORE ANY WAIT FOR A SLOT (Arrived), and the request
// waits in the gate's queue, never in the transport's buffer: a delivery
// parked for a slot left every later request there, where nothing counted its
// wait, and an answerer bounds its work by its asker's wait from arrival. The
// queue is bounded as the NATS client's buffer is (500 000 requests and 64 MiB
// a subscription, its defaults), past which a request is dropped, logged
// scatter_request_dropped, and its asker hears nothing, so a registration
// whose requests are large and whose answers wait must bound its answers below
// what that holds at once. A withdrawal answers nothing it held waiting and
// waits for what is in flight.
//
// # Passes and the ack-pending budget
//
// A Pass is neither a failure nor a deferral: it is logged as delivery_passed,
// never quiesces the attachment, and returns behind the waiting mail on the
// failure path's backoff counted by the MESSAGE's deliveries (about ten
// minutes over its 25), then is dead-lettered with its group's unhandled
// deliveries. A passed message holds one of its group's ack-pending slots from
// its first delivery until a node acks it or its last delivery is
// dead-lettered; with the server's 1000 slots all held, the group stops
// delivering new mail until slots free and loses nothing. Leaving that in
// place is a stated decision (OutcomePass). A copy marks itself unhandled by
// its dead-letter SUBJECT, never a header: a copy at the transport's ceiling
// has no room for one. The mark is fleet-wide (JetStream keeps one per failed
// message in CREWLET_FAILED, written before the message goes back; a mark it
// could not write or read errs towards unhandled).
//
// # Writes and the connection's pace (JetStream)
//
// A publish, a keep and a state log's append with no caller deadline wait the
// request term plus, across a link (a leaf's, an external cluster's), the time
// their bytes take at the object store's floor, for each of the first
// jsapi.WriteSends sends; the client's own re-sends are off and made by the
// queue, each with its own crossing. Held to the five seconds alone, a keep at
// the ceiling through a slow leaf link failed as ErrUnavailable while its bytes
// crossed, and its keeper retried it for ever (crewlet/crewlet#1005). An
// external dial gives every socket write the largest write's crossing
// (externalWriteDeadline, the client's FlusherTimeout), because the client
// writes a message whole under one deadline and its default minute cut a write
// over about 2 MiB at the floor, and the connection with it. A caller's own
// deadline is never extended, but on an external broker it is not checked
// while its request waits for the connection's lock, so it can be held behind
// every large write ahead of it; a node's holds ride a connection of their own
// (ADR-0174). Not covered: a link slower than the floor or busy with other
// traffic (ADR-0161).
//
// # Broker connections and authentication
//
// Every connection the engine opens to its own embedded broker is in process,
// a clustered member's too: its client listener binds 127.0.0.1 and advertises
// nothing, since dialled over TCP it bound every interface and gossiped every
// member's client URL. Every connection presents a key (ADR-0094): one
// authenticator on every embedded broker admits routes, leaf links and
// operators by keys of secrets.keys, every key of the ring accepted and the
// active one presented, so rotation needs no broker step. An EXTERNAL server
// is admitted only where its INFO says it authenticates clients, read off the
// plaintext handshake by the recording dialer (nats.go parses no tls_verify)
// after the max_payload contract; an INFO not read is a refusal. An embedded
// member locks its store directory before the server exists (nats-server
// accepts a second broker on one directory and both write it; the node
// database's lock cannot stand in, since the broker starts first). A leaf
// dials its members through an in-process HTTP CONNECT tunnel, the one
// supported way onto a socket the server dials.
//
// What a node's data link carries is read at its socket (LinkMeter, nil on a
// member), moving at 70% of the floor one way, because every counter
// nats-server and nats.go keep moves a whole message at a time and a message
// at the ceiling stands them all still for minutes. OpenCoordination reaches
// the broker for the coordination store alone and builds no queue, because a
// queue provisions the engine's streams.
//
// # State log watch
//
// DomainLog.Watch is for a reader that keeps its work in rows and only wants
// to hear sooner which object moved: one ephemeral consumer takes each new
// record's subject and sequence, never its body, because carrying a domain's
// records (up to 8 MiB) to every watcher would cost each the log's whole
// volume again. What it may have missed it says (Gap) and never repairs; the
// caller waits for its copy to reach the position, then reads its rows, so a
// nudge only ever shortens a wait.
//
// # Dead letters
//
// A keep is one entry for one identity its keeper derives from what it keeps
// (LetterIdentity), so a second keep within DeadLetterWindow is answered
// "already kept". The window is fifteen minutes, the longest a keep at the
// ceiling across a link at the floor waits; on JetStream it is CREWLET_DLQ's
// duplicate window, which stream.event_retention_hours must cover. DropDeadLetter
// leaves the identity in the window on a running broker, so a keep inside it
// keeps nothing, which is what a discard means. A listing reads headers only
// from a replica; marks and bodies come from the leader. Across a leaf's link
// an entry may be too large for a read's JSON reply to cross (a leaf's server
// closes its whole link on one past the transport, crewlet/crewlet#1031), so
// it is found from its headers and read raw.
//
// # The node's answer budget
//
// ConcurrentAnswers bounds how many answers a registration builds at once, not
// their bytes, and a payload answer holds a multiple of its reply while it is
// built: 64 box reads at once held 495 MiB of replies and raised the node's RSS
// by up to 1.25 GiB, 64 history answers at the transport's limit 510 MiB and
// 1.18 GiB (crewlet/crewlet#1782). So every payload answer on a node also
// acquires from ONE AnswerBudget of AnswerBudgetBytes, at its weight: its byte
// bound times its measured encode multiple, stated where each answerer reads.
// The hold is taken before the read and given back once the reply is written
// (Replying, which both backends wrap every answer in), FIFO and within the
// answer's context, a wait that ends naming the budget (ErrAnswerBudget). An
// answer acquires once (a second hold in one answer is refused): FIFO, it could
// otherwise wait behind itself. One that holds and then asks a peer is only an
// assistant replay on a node without the estate, whose snapshot crosses the
// estate router; the data nodes answering it read their own stores and ask
// nobody, so no cycle of waits forms. The internal/nodebudget Answers line keeps
// the budget in a node's memory. Every Serve registration in the tree is
// declared payload (the kind it acquires, and where) or small (the bound that
// keeps it so), and a test sweeps the source for one that is neither.
//
// # Testing the contract
//
// The suite reads "nothing more was delivered" behind each backend's own
// report that it has settled (Settle), never after a quiet window, which a
// late leak passed. A handler never runs beside itself, and the suite decides
// only what every backend can show. A linger window ends on a clock its
// backend is handed (LingerClock), so cases move it rather than race the wall
// clock. Test support is four packages, deliberately: jetstreamtest stands up
// a cluster (importing it declares a solo suite), externaltest the single
// external server, brokertest the one store size every embedded fixture
// declares, and linktest a slow link between a node and its broker.
//
// A test's broker is never sized from the disk: an unset limit is three
// quarters of the free space on a volume every package shares, so a boot
// passed or failed on how full the runner was. A test binary refuses to start
// an embedded broker with a store directory and no limit
// (jetstream.ErrUndeclaredSize) or whose store lies outside a brokertest.TempDir
// (jetstream.ErrStoreOutsideTempDir): a stopped nats-server can land a last
// flush after Shutdown returns, which fails a t.TempDir removal
// (crewlet/crewlet#1079). Brokertest's source gates read every Go file of the
// module for a server built from nats-server's options that states no limit.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

var log = logging.Get("queue.contract")

// ErrTooLarge reports that an event does not fit on the wire.
//
// ONE OF THE FOUR SENTINELS THE LAYERS ABOVE MAY BRANCH ON (ErrNotLive,
// ErrUnavailable and ErrAttachmentGone are the others): each backend
// translates its own refusal into this.
//
// "Too large" is the one PERMANENT publish failure; a producer unable to tell
// it from ErrUnavailable must retry everything forever. Sizing input against
// MaxPayloadBytes cannot replace it: wire cost depends on the BYTES, not the
// length (encoding/json escapes '<', '>' and '&' to six bytes each), so any
// up-front ratio is wrong for one document or wasteful for another.
var ErrTooLarge = errors.New("queue: event too large for the transport")

// MaxPayloadBytes is the largest single event any backend must carry.
//
// A CONTRACT number every backend agrees on: the embedded broker accepts
// exactly this (internal/queue/jetstream), an external NATS server announcing
// less is refused when the backend opens, and the in-memory twin enforces it,
// so a payload a test accepts is one production accepts.
//
// 8 MiB is nats-server's MAX_PAYLOAD_MAX_SIZE, above which it warns a payload
// is too large to be a good idea (server/const.go). A message is buffered
// whole in the server and every receiving client: memory per in-flight event.
const MaxPayloadBytes = 8 << 20

// ConcurrentAnswers is how many requests an [EventQueue.Serve] registration
// answers at once when what it answers is work it does NOW. SIXTY-FOUR, twice
// the default `node.max_concurrent`: two nodes' full turns, each asking a peer,
// never queue behind each other, and a request past it waits for a slot
// ([EventQueue.Serve] says how far it is buffered). THE COUNT BOUNDS SMALL
// ANSWERS, of a few KiB. A PAYLOAD answer is bounded by bytes where it reads,
// and holds its weight of the node's [AnswerBudget] until its reply is written:
// the count times those bounds fits no node (package doc; answersweep_test.go).
// A REGISTRATION WHOSE ANSWERS WAIT (a push proposal behind its repository's
// earlier pushes) passes its own queue's bound and is a registration of its
// own: a slot held through the wait blocks every other request there.
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
	// shipped broker SPACES a failure (a second, doubling to thirty) and serves
	// never-delivered messages meanwhile, so the retry follows every message
	// already queued, newer mail on the same partition key included. The
	// in-memory twin models that backoff virtually; queuetest's
	// a_failure_returns_behind_waiting_mail_and_a_deferral_at_the_head holds
	// both. A consumer that must keep its place wants [OutcomeDefer], or settles
	// the message durably and retries itself. See OrderForDispatch.
	OutcomeNak

	// OutcomeDefer leaves the delivery unacked AND quiesces the attachment: for
	// work this process lost the right to do (a seat whose lease moved, a stale
	// config). Acking would claim work it will not perform.
	// IT COSTS ONE DELIVERY, as a Nak does, on every backend; the budget is
	// sized to cover handoffs, and the backend dead-letters at the boundary
	// instead of the broker's MaxDeliver discarding a healthy event unrecorded.
	// Such an event (or one passed until a hand-back spent its last delivery) is
	// kept on its group's UNHANDLED subject ([RetiredTo]), never with the poison,
	// which is any message one of whose deliveries FAILED as itself.
	// IT RETURNS AT THE HEAD: an unspaced Nak, ahead of the conversation's newer
	// mail. Never substitute a republish: the event would go to the topic tail
	// while prefetched siblings replay from the head, reordering it.
	OutcomeDefer

	// OutcomePass hands the delivery back to its group for a PEER and goes on
	// consuming: nothing is wrong with this node, only with this message here
	// (a record a newer build wrote, met by an older one during a roll). A Nak
	// would log a failure; a Defer would quiesce the attachment and, unspaced
	// at the head, spend the budget before the reading peer started.
	// SPACED AS A FAILURE IS, BY THE MESSAGE'S DELIVERY COUNT, behind waiting
	// mail, one of 25 deliveries (jetstream's maxDeliver): about ten minutes,
	// then a dead letter on the group's UNHANDLED subject ([RetiredTo]). Nothing
	// steers a pass to a reader; that dead letter does, replayed by
	// internal/notify's Service. A passed message holds a group ack-pending
	// slot throughout; the server's default 1000 stays on purpose (the group
	// then waits on new mail, losing none).
	OutcomePass

	// OutcomeAside keeps the delivery unacknowledged under a key ([Result.Key],
	// [Aside]) and goes on consuming past it: for mail that must wait on work
	// already running (a busy seat's conversation, ADR-0169) without holding
	// the mail behind it. IT SPENDS NOTHING: the backend keeps it from timing
	// out while it waits. [EventQueue.Readmit] of its key hands it back at the
	// head of the next drain, partitioned there as any drain's mail; a release
	// or a stop hands it back at one delivery, a crash after the ack window.
	// One past [MaxAside], or naming no key, is handed back at the head as a
	// deferral is, one delivery spent, the attachment not quiesced.
	OutcomeAside
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
	case OutcomeAside:
		return "aside"
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
	// NodeFault marks a Nak as a failure of THIS NODE's rather than of the
	// message ([NakNode]): returned as any failure is, and never counted
	// as the message's own ([Result.FailsMessage]).
	NodeFault bool
	// Key is what an aside waits under ([OutcomeAside]), the name
	// [EventQueue.Readmit] hands it back by.
	Key string
}

// FailsMessage reports whether r is a failure of the MESSAGE: a Nak that is
// not this node's own ([NakNode]). A message one of whose deliveries was so
// answered is kept with its group's poison once its budget is spent, whatever
// return spent it ([RetiredTo]).
func (r Result) FailsMessage() bool { return r.Outcome == OutcomeNak && !r.NodeFault }

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

// Nak negatively acknowledges, requesting redelivery: a failure of the
// message, which keeps it with its group's poison should its budget be spent
// ([RetiredTo]).
func Nak(err error) Result { return Result{Outcome: OutcomeNak, Err: err} }

// NakNode negatively acknowledges a delivery that failed for a reason of THIS
// NODE's, never of the message: a broker it could not reach ([ErrUnavailable]),
// a queue that is not live ([ErrNotLive]), a stop that ended its handler, a
// coordination store that did not answer. Returned as a [Nak] is (spaced,
// behind waiting mail, the attachment taking mail on) because the condition
// may clear, here or on a peer; but never counted as the message's own failure
// ([Result.FailsMessage]), so a message whose budget it spends is kept with its
// group's unhandled deliveries, not its poison ([RetiredTo]).
//
// NOT A DEFER, for a node that lost the right to the work: that quiesces the
// attachment, and a node whose broker blipped would stop taking every delivery
// of its group for the sake of one.
func NakNode(err error) Result { return Result{Outcome: OutcomeNak, Err: err, NodeFault: true} }

// NodeFault reports whether err, what a handler met with ctx, its delivery's
// context, is a failure of THIS NODE's that the queue itself can tell: a verb
// the broker did not answer ([ErrUnavailable]), one refused by a queue that is
// no longer live ([ErrNotLive]), or any failure under a context that has
// ended, a stop, a drain or a seat moving on cancelling the handler. A handler
// answers such a failure with [NakNode]. ONE TEST FOR EVERY HANDLER THAT ASKS
// IT (internal/notify's wakes, internal/engine's seat control topic and seat
// mailbox, internal/observe's custody keeper), so a failure one of them keeps
// out of its group's poison is one the others keep out too; a handler adds the
// faults of its own node that only it can tell (a rate valve's refusal, a
// coordination store's silence) beside it.
func NodeFault(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotLive))
}

// Defer leaves the delivery unacked and quiesces the attachment, for cause.
func Defer(cause DeferCause, reason string) Result {
	return Result{Outcome: OutcomeDefer, Cause: cause, Reason: reason}
}

// Pass hands the delivery back to its group for a peer and goes on consuming
// ([OutcomePass]): reason is why, and err what this node met, both logged —
// reason on the dead letter's line too, should this pass spend the delivery's
// budget, whose copy is kept on its group's unhandled subject unless one of its
// deliveries failed as itself ([RetiredTo]).
//
// The group's own budget, about ten minutes, is all a pass gets, and enough:
// a dead letter follows, not a drop, and the replay of a node that reads the
// copy delivers it (internal/notify). From its first delivery
// until a node that reads it acks it, or its budget is spent, it holds one of
// the group's ack-pending slots, which [OutcomePass] does the arithmetic of:
// it delays the group's mail and loses none.
func Pass(reason string, err error) Result {
	return Result{Outcome: OutcomePass, Reason: reason, Err: err}
}

// Handler processes one event.
type Handler func(ctx context.Context, ev *events.Event) Result

// BatchHandler processes one PARTITION of a drained batch.
type BatchHandler func(ctx context.Context, evs []*events.Event) Result

// ErrNotLive reports that a verb reached a queue that is not live: never
// started, stopped, or holding a connection its client closed for good (which
// stops the node). ONE OF THE FOUR SENTINELS THE LAYERS MAY BRANCH ON (see
// ErrTooLarge): each backend wraps its own error (jetstream.ErrClosed,
// memory.ErrNotStarted) in this.
//
// "The queue is down" is not a failure for every caller. A seat release KEEPS
// THE LEASE if its detach cannot be proven, since this process may still be
// consuming the seat. A queue that is not live consumes nothing, so a refusal
// there is proof of teardown; reading it as failure would strand the seat for
// a full TTL on the path where the node is handing it back.
var ErrNotLive = errors.New("queue: not live")

// ErrUnavailable reports that a verb did not reach the broker or was not
// answered in the client's own time. A CONDITION: a retry may succeed, so it is
// never a fault of this node. Backends wrap the transport's error.
//
// NOT ErrNotLive: a silent broker proves no teardown (consumers resume), so
// reading it so would hand a seat to a peer while this node may still consume
// it. Quiesce, Unquiesce, Detach, Attachment, PauseTopic, ResumeTopic and
// Readmit NEVER ANSWER IT: they touch no broker, and a node that lost a seat's proof quiesces
// on the very outage that cost it. NOT "NOTHING HAPPENED": an unacknowledged
// publish may be stored, so retries tolerate a second copy. A caller's own
// deadline on a connected broker stays the context's error; one ending while
// disconnected is marked.
var ErrUnavailable = errors.New("queue: broker unavailable")

// ErrAttachmentGone reports a PauseTopic for an attachment that is not the
// pair's current one: a Detach has ended it, or it is the zero Attachment,
// which names none. Nothing was held.
//
// ONE OF THE FOUR SENTINELS THE LAYERS ABOVE MAY BRANCH ON, and what it tells
// a caller is that the seat it decided to hold is no longer attached under
// the attachment it decided for: the release that ended it dropped every hold
// it carried, and the holder that attaches next takes its own. So it is an
// answer, never a condition to retry: asking again under the same attachment
// is refused again, and asking under the current one would hold an attachment
// the caller's decision was never about.
var ErrAttachmentGone = errors.New("queue: the attachment the hold was taken for has ended")

// CheckBacklogLimit refuses an [EventQueue.Backlog] limit below one: a read of
// nothing is a caller's bug, never an empty mailbox.
func CheckBacklogLimit(limit int) error {
	if limit < 1 {
		return fmt.Errorf("queue: a backlog read needs a limit of at least one, got %d", limit)
	}
	return nil
}

// Attachment names ONE ATTACHMENT of a (topic, group) pair on one client: the
// span from one Detach of the pair to the next, whether a consumer is attached
// in it yet or not. A client mints them from one counter, so no two
// attachments of any pair on it share a value, and the zero Attachment names
// none: every PauseTopic refuses it, and a ResumeTopic under it lifts nothing.
//
// It is what a pause hold is taken for. A hold has to be taken BEFORE its
// consumer starts (an engine that knows a seat is busy holds first and
// attaches after), so the queue cannot tell a hold meant for the next
// attachment from one meant for the attachment a release has just ended by
// looking at whether anything is attached: only the caller knows which it
// decided for, and it says so with this.
type Attachment uint64

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
// NOT the conversation the event belongs to: a direct message is one
// conversation however threaded, but a reply in a thread on it still
// partitions on the thread, since merging it with unrelated top-level pings
// would hand one digest a reply target naming only one. internal/queue cannot
// import internal/notify, so the producer stamps both values and the caller
// hands the partition one down (node.partitionKey, over notify.KeyOf).
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
	// none — every subscription's copy, its poison and its unhandled
	// deliveries alike ([RetiredTo]), which its backend keeps as the
	// message's own bytes and nothing beside them.
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
// its header's and body's bytes: [EventQueue.KeepDeadLetter] refuses, with
// [ErrTooLarge], an entry within LetterFraming of [MaxPayloadBytes].
//
// A CONTRACT NUMBER, so the twin never accepts what the broker refuses. The
// JetStream header block (`NATS/1.0`, the expected stream, the header's name,
// the blank line) costs 70 bytes around the header's own; its test holds that
// under this. A QUARTER KIBIBYTE leaves room for a renamed header or stream,
// and a wake feed's largest entry leaves over 3 KiB (internal/changefeed's
// deadLetterDetailBytes); the keep's identity rides in it too (jetstream's
// letterID).
const LetterFraming = 256

// LetterFits reports whether a dead letter of header and body can be kept: the
// two within [LetterFraming] of [MaxPayloadBytes]. It is the one ceiling every
// keep is held to — [EventQueue.KeepDeadLetter] refuses an entry past it with
// [ErrTooLarge] on every backend.
func LetterFits(header, body []byte) bool {
	return len(header)+len(body)+LetterFraming <= MaxPayloadBytes
}

// RetiredTo is the dead-letter subject a subscription keeps its message on
// once a return spent its delivery budget. A message that FAILED AS ITSELF at
// any delivery ([Result.FailsMessage]) is poison on the dead-letter subject
// ([topics.DeadLetter]); nothing replays it. Every other goes to the UNHANDLED
// subject ([topics.Unhandled]): a HEALTHY delivery no member handled, which a
// reader that can handle it replays (internal/notify). JUDGED BY ALL THE MESSAGE'S DELIVERIES, NOT THE LAST RETURN: a hand-back or
// node fault judges nothing, and the last return alone replayed failing
// messages forever or kept wakes with the poison. So each backend remembers
// FLEET-WIDE which messages failed as themselves (jetstream's failure marks).
// THE SUBJECT SAYS WHICH, NEVER A HEADER: a copy at the transport ceiling has
// no room for one.
func RetiredTo(topic, group string, failed bool) string {
	if failed {
		return topics.DeadLetter(topic, group)
	}
	return topics.Unhandled(topic, group)
}

// DeadLetterWindow is how long a backend remembers the identity a dead letter
// was kept under ([EventQueue.KeepDeadLetter]): a repeat keep within it keeps
// nothing and is answered already kept. A CONTRACT NUMBER: the JetStream
// backend makes it `CREWLET_DLQ`'s duplicate window.
//
// FIFTEEN MINUTES: THE LONGEST A KEEP MAY TAKE, AND ROOM, so it outlasts a copy
// still on its way. A keep with no deadline waits the request term plus its
// bytes at the slowest link pace (internal/objstore's MiBPace, 30 s/MiB) for
// each of three sends (internal/jsapi's WriteSends): 5 s + 3 x 240 s at 8 MiB,
// about twelve minutes. The retention must cover it (a broker refuses a longer
// duplicate window), so Tier A refuses `stream.event_retention_hours` below it.
const DeadLetterWindow = 15 * time.Minute

// LetterIdentity reports why id cannot name a dead letter's keep, or nil when
// it can: any identity but the empty one.
//
// THE CONTRACT'S RULE, SHARED BY BOTH BACKENDS: an identity is what makes a
// second keep of one thing harmless ([EventQueue.KeepDeadLetter]), and an
// empty one names nothing — every keeper that forgot to derive one would share
// it, and their entries would collapse into whichever was kept first. So it is
// refused, unmarked, as the caller's mistake, and the twin refuses exactly the
// same.
func LetterIdentity(id string) error {
	if id == "" {
		return errors.New("queue: a dead letter's keep names no identity; its keeper " +
			"derives one from what it keeps, so a second keep of the same thing is " +
			"answered already kept")
	}
	return nil
}

// LetterHeader reports why header cannot be kept beside a dead letter, or nil
// when it can: one line of printable ASCII — every byte from the space to the
// tilde — that neither begins nor ends with a space. An empty header is no
// header at all, and is kept as one.
//
// THE CONTRACT'S RULE, SHARED BY BOTH BACKENDS: NATS carries a header value as
// one line of ASCII (its ADR-4), and the pinned client rewrites anything else
// (CR or LF to a space, edge spaces trimmed; nats.go v1.55.0 `writeHeaderValue`),
// so the listing would differ from what was handed over. Both refuse it before
// anything is kept, as the caller's mistake.
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
	// An event marked [Prioritize] on a laned topic goes to its priority lane
	// ([SubjectFor]), taken ahead of the topic's other mail; elsewhere the mark
	// changes nothing. An event whose Node is empty is stamped with this client's
	// ([WithNode]) before any listener or consumer sees it; one naming a node
	// keeps it. The caller's event is never written to ([Options.Stamp]). A nil
	// event is REFUSED, reaching no listener or consumer: its encoding is an
	// event with no id or type, which a backend once delivered where the twin
	// refused.
	Publish(ctx context.Context, topic string, ev *events.Event) error

	// Subscribe attaches a competing-consumer handler to topic/group.
	// Exactly one member of a group receives each message. On a laned topic
	// the handler is handed the lane's mail before the topic's.
	Subscribe(ctx context.Context, topic, group string, h Handler) error

	// SubscribeBatch attaches with batched, key-partitioned delivery: drain what
	// is locally available (plus a linger window), partition by [BatchKeyFunc]
	// preserving arrival order, dispatch one call per partition oldest first,
	// ack per partition. A failing partition never blocks or replays another.
	// On a laned topic a drain takes the lane's mail first and again before each
	// partition ([JoinLane]), and a partition holding a priority event goes
	// first ([OrderForDispatch]). An event deeper than the drain's cap is
	// delivered in its own turn, exactly once (see lane.go). opts is read live
	// each cycle, so a reload changes linger and batch size without resubscribing.
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
	// NON-DESTRUCTIVE FOR THE MAIL, NOT FREE: the subscription and its mail
	// survive for whoever attaches next, but each message taken and not settled
	// COSTS one delivery, as every hand-back does (see DeliveriesLeft); one under
	// a RUNNING handler is settled by its outcome. Returned mail goes back at the
	// head (OutcomeDefer), yet a re-attaching consumer races new mail, so only
	// what ONE handler call carries is ordered (OrderForDispatch). Releases this
	// attachment's pause holds, or a re-attaching node would be silently deaf.
	// It does NOT wait for a running handler (the fenced-release path): a node
	// that LOST the lease detaches FIRST and abandons in-flight work. The
	// VOLUNTARY path composes quiesce, wait for in-flight, then detach.
	Detach(ctx context.Context, topic, group string) (bool, error)

	// EnsureSubscription creates the durable subscription if absent, with NO
	// consumer attached, positioned at the earliest message. An existing one is
	// success. On a laned topic it creates the priority lane and completes a
	// [Subscription] listed as Partial.
	//
	// The bool reports that this call found it absent AND provisioned it. On a
	// fleet two nodes creating at once can both report true, as a broker
	// returning the object either way cannot tell "I made this" from "already
	// here". Deliberately NOT three-valued: nothing branches on it (it is a log
	// count), and a tri-state would describe a race the broker does not expose.
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

	// ListSubscriptions reports every durable subscription whose topic matches
	// topicPattern (SubscribeStream's grammar), as the pair it was created with,
	// in no promised order. EVERY one the broker holds, not this client's,
	// attached or not; never an ephemeral SubscribeStream one. A priority lane
	// is not listed on its own; a laned subscription missing its lane is listed
	// Partial. An empty pattern is refused; ">" asks for everything.
	//
	// It exists because a durable subscription outlives every record of its
	// name: a registry beside the broker can miss a removed seat's mailbox,
	// which would retain its mail for the life of the deployment unfound.
	ListSubscriptions(ctx context.Context, topicPattern string) ([]Subscription, error)

	// Waiting reports the mail a subscription holds that no handler of THIS
	// client is running on, per mailbox of a laned topic ([Waiting]): what
	// is waiting for the subscription's consumer, whether it was never
	// delivered, fetched and not yet dispatched, handed back or set aside
	// ([OutcomeAside]). The mail a handler of this client is running on is not
	// waiting and is not counted, which is why the count is asked of the
	// client that holds the subscription's attachment: any other client
	// counts that mail too.
	//
	// A DISPLAY, NOT A LEDGER. A backend may answer from consumer state
	// that trails a publish by a moment; nothing may decide delivery on it.
	// A subscription that does not exist waits for nothing.
	Waiting(ctx context.Context, topic, group string) (Waiting, error)

	// Backlog reads the mail a subscription holds WITHOUT CONSUMING IT: no
	// delivery is made or spent, no hold, quiesce or attachment is touched,
	// and a held subscription stays held. A laned topic's lane first, then
	// the topic's own, each in the order its subscription holds it, at most
	// limit events (below one is refused). It MAY INCLUDE MAIL ALREADY ACKED
	// or under a running handler (a backend reads from its group's ack
	// floor), so a caller acting on it checks what it acts on was not worked;
	// mail THIS client holds aside ([OutcomeAside]) is never in it, decided
	// already. READ-YOUR-OWN-WRITE: a publish that has returned is in it.
	// What a seat's router reads a held inbox by (internal/engine); a
	// subscription that does not exist holds nothing.
	Backlog(ctx context.Context, topic, group string, limit int) ([]*events.Event, error)

	// SubscribeStream creates an ephemeral per-caller broadcast
	// subscription — every subscriber receives every matching event.
	// topicPattern supports `*` (one segment) and `>` (trailing
	// segments).
	SubscribeStream(ctx context.Context, topicPattern string, h StreamHandler) (Unsubscribe, error)

	// AddPublishListener registers a listener called inline on every
	// publish.
	AddPublishListener(l PublishListener)

	// Ask scatters one request to every process serving subject and collects
	// replies until want arrive or ctx is done; want of 0 waits out the deadline.
	// EPHEMERAL, hence a verb of its own: nothing is retained, redelivered or
	// recorded (durable verbs would make the audit log a function of typing).
	// FEWER REPLIES THAN want IS NOT AN ERROR: a slow or absent peer is
	// ordinary. An error means the ask could not be made, never an unconfirmed
	// delivery: a caller reads an error as "nobody received it". A BROKER THIS NODE CANNOT REACH IS REFUSED BEFORE ANYTHING IS SENT
	// (ErrUnavailable), never buffered for reconnect: a retained request would
	// reach servers after its asker was told it failed, so a request must still
	// be safe to receive after its asker left. REPLIES ARE UNORDERED and carry
	// no sender: only the payload can say who did not answer.
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

	// Serve makes this process one of subject's answerers. EVERY SERVER SEES
	// EVERY REQUEST, unlike Subscribe: a scatter divides work by what the
	// request NAMES. An answerer's error answers NOTHING (AnswerFunc). ANSWERS
	// RUN CONCURRENTLY, up to answers at once; one beyond waits for a slot, its
	// arrival stamped first (Arrived), up to 500 000 requests and 64 MiB, past
	// which it is dropped, heard as no answer (package doc), so LARGE, slow-held
	// requests bound their own backlog and answer past it AT ONCE with "not
	// now" (internal/estate). The
	// bound and slots are the registration's own ([ConcurrentAnswers] for work
	// done now; below one is [ErrAnswerBound]). Unsubscribe WAITS for in-flight
	// answers, bounded by its context. Returning means only THIS server holds
	// the registration: ask and read who answered.
	Serve(ctx context.Context, subject string, answers int, h AnswerFunc) (Unsubscribe, error)

	// Broadcast hands payload to every process listening on a matching pattern.
	// EPHEMERAL, Ask's fan-out counterpart: nothing is retained, acked or
	// recorded; a later listener never hears it. For what is worth nothing once
	// superseded (presence): a durable verb would be a replicated write per
	// movement. AT MOST ONCE AND UNORDERED ACROSS PUBLISHERS: a lagging listener
	// loses messages rather than holding the publisher; what a message means,
	// and whether a later one supersedes it, is in its bytes.
	// ONE PUBLISHER'S ORDER IS KEPT, so a later broadcast of a sender is a
	// FENCE: a listener that heard it has heard all it will ever hear of that
	// sender's. An unreachable broker is REFUSED BEFORE ANYTHING IS SENT
	// (ErrUnavailable), for Ask's reason. The subject is a LITERAL
	// ([LiteralSubject]); brokers refuse a publish to a pattern.
	Broadcast(ctx context.Context, subject string, payload []byte) error

	// Listen makes this process a listener on pattern (`*` one token, `>` the
	// trailing tokens) until the returned Unsubscribe runs. EVERY LISTENER HEARS
	// EVERY MATCHING MESSAGE (Serve's rule): a group would show each room to a
	// fraction of the fleet. ONE MESSAGE AT A TIME PER LISTENER, in arrival
	// order, on the backend's own goroutine: a handler never races itself or
	// runs on the publisher's; one that panics loses that message and keeps its
	// registration. Unsubscribe stops delivery then WAITS for a running handler,
	// bounded by its context (else an error wrapping it, never success), so a
	// handler must never withdraw its own registration. RETURNING MEANS ONLY
	// THIS PROCESS'S SERVER HOLDS THE REGISTRATION, as for Serve.
	Listen(ctx context.Context, pattern string, h ListenFunc) (Unsubscribe, error)

	// KeepDeadLetter keeps body on subject (a literal in the dead-letter
	// namespace, else refused) under header until the retention lets it go
	// ([Kept.Expires]) or DropDeadLetter removes it, and answers whether id was
	// ALREADY KEPT there (then it keeps nothing). FOR A READER THAT SETTLES ITS
	// OWN DELIVERIES (internal/changefeed), never a subscription. The header
	// ([Kept.Header]; [LetterHeader]) is what listings answer; the body only
	// DeadLetterFrame reads; neither is an event; past [LetterFraming] is
	// ErrTooLarge. A storing keep returns once durable. id derives from WHAT is
	// kept, never the delivery, so a second keep within [DeadLetterWindow] comes
	// to nothing; per subject; empty is refused ([LetterIdentity]). A dropped
	// entry's id holds for the window on a running broker only. ALREADY KEPT IS A
	// BOOL, NOT AN ERROR (which would loop the keep), and says not which keep stored.
	KeepDeadLetter(ctx context.Context, subject, id string, header, body []byte) (bool, error)

	// DeadLettersOn lists every entry subject holds, oldest first, whoever kept
	// it, each with its DropDeadLetter sequence, keep time, expiry and header:
	// NEVER ITS BODY, which moves for no listing. Empty is a subject holding none.
	//
	// A COPY'S ANSWER, which may be BEHIND the broker's: it can miss an entry
	// kept a moment ago or show one just dropped, never hide one the copy holds.
	// A reader that must know its listing is current holds it to
	// DeadLetterMarks (the broker's own): equal counts mean the subject as the
	// broker holds it.
	DeadLettersOn(ctx context.Context, subject string) ([]Kept, error)

	// DeadLetterFrame is the body the entry seq of subject was kept with,
	// exactly as handed to KeepDeadLetter, and whether subject holds the entry.
	// THE ONE VERB THAT MOVES A BODY, for the reader acting on that entry (a
	// feed's replay); it reads what the broker holds now, never a copy behind.
	// AN ENTRY ALREADY GONE (dropped, or retention let it go) is not held, no
	// error. A seq holding an entry on ANOTHER subject is refused, unmarked,
	// answering nothing of it, as DropDeadLetter refuses; so is zero.
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

	// DropDeadLetter removes the entry seq from subject. AN ENTRY ALREADY GONE
	// IS NO ERROR, as the caller asks only that it be gone: two nodes finishing
	// with one entry both succeed. A seq holding an entry on ANOTHER subject is
	// refused, unmarked, removing nothing; so is zero.
	//
	// THE ENTRY GOES AND ITS ID STAYS, on a running broker: a keep of that id
	// inside [DeadLetterWindow] answers already kept ([EventQueue.KeepDeadLetter]),
	// as a drop means no node is to read it. A broker that restarts or rebuilds
	// its window from held entries forgets the id, and a keep then stores a new
	// entry the listing shows again.
	DropDeadLetter(ctx context.Context, subject string, seq uint64) error

	// Start connects the backend and begins consuming. WHETHER A BACKEND
	// REQUIRES IT IS ITS OWN ANSWER (JetStream's Open connects, so Start is a
	// no-op; the twin's Start makes it live), but it may NOT answer DIFFERENTLY
	// PER VERB: before Start, all twenty-three publish, subscription, attachment,
	// ephemeral and dead-letter verbs either all refuse or none require it
	// (Capabilities.RequiresStart; queuetest sends all twenty-three). A twin that
	// refused Publish but let PauseTopic take a hold left it to survive into
	// the next life, silently deaf. AFTER Stop it is no capability (see Stop).
	// The DRAIN protocol (PauseDelivery, WaitForHandlers, InFlightCount) is
	// exempt and answers on a stopped queue, or a drain could not say whether it
	// finished; Backend and AddPublishListener have no error to refuse with.
	Start(ctx context.Context) error

	// InFlightCount reports handler invocations currently mid-flight —
	// the number an operator watches converge to zero during a drain.
	// A delivery is in flight until what its handler returned is settled,
	// and so is one the backend hands back itself, until that hand-back is
	// (WaitForHandlers says why).
	InFlightCount() int

	// Backend is a stable lowercase backend name for operator display.
	// Nothing may branch behaviour on it.
	Backend() string

	// PauseDelivery stops dispatching new events to handlers while
	// leaving Publish working, so in-flight handlers can emit terminal
	// events. One-way: once paused, the engine is shutting down.
	PauseDelivery(ctx context.Context) error

	// Attachment is the (topic, group) pair's CURRENT attachment on this
	// client: the one attached now or, with none attached, the one the
	// next attach makes. It changes only when a Detach (or Stop) ends it,
	// so a caller reads it where it decides to hold a seat and names it
	// in every PauseTopic and ResumeTopic that decision makes. It touches
	// no broker.
	Attachment(ctx context.Context, topic, group string) (Attachment, error)

	// PauseTopic pauses ONE subscription's delivery under a named reason, for
	// the attachment at names, and refuses with ErrAttachmentGone, holding
	// nothing, when at is not the pair's current attachment (the package doc
	// says why). Holds are reason-scoped and keyed by the (topic, group) PAIR,
	// so a second subsystem gating the same inbox cannot release the first one's
	// hold by lifting its own, and a hold on one group does not gate every other
	// group on a shared subject. The engine holds a seat's inbox under four,
	// each lifted only by the subsystem that took it: a company with no turn
	// engine, a person's pause, a spent budget window, and a detached executor
	// holding the seat (or an answer's resume it owes) — see
	// internal/agent/inbox's Hold and the engine's budget park.
	PauseTopic(ctx context.Context, topic, group string, at Attachment, reason string) error

	// ResumeTopic releases one reason's hold on the attachment at names,
	// flushing when none remain. Under an attachment that has ended it
	// lifts nothing and answers nil: the Detach that ended it dropped every
	// hold it carried, so the hold named is not in force, and a hold the
	// next attachment took under the same reason is not this caller's.
	ResumeTopic(ctx context.Context, topic, group string, at Attachment, reason string) error

	// Readmit hands back every delivery this client's attachments of the pair
	// hold aside under key ([OutcomeAside]), at the head of each one's next
	// drain, in the order the subscription holds them, and answers how many.
	// None aside is no error: a caller readmits whatever its transition freed.
	// A Readmit landing while a handler runs readmits the delivery that handler
	// then sets aside under key. It touches no broker, so it never answers
	// ErrUnavailable; the delivery is still unacknowledged and its count
	// unspent, so what it hands back is the delivery the backend made.
	Readmit(ctx context.Context, topic, group, key string) (int, error)

	// WaitForHandlers waits for in-flight handlers, returning how many
	// were still running when the wait ended. Zero means a clean drain;
	// non-zero means the timeout expired, which is not an error — the
	// caller owns any "too long" policy. Always PauseDelivery first.
	//
	// A HANDLER IS IN FLIGHT UNTIL WHAT IT RETURNED IS SETTLED: a return that
	// spends the delivery budget keeps a dead letter, a write as slow as its
	// bytes over a link, and a drain stopping the queue under it loses the copy.
	// SO IS A DELIVERY THE BACKEND HANDS BACK ITSELF (a hold, pause, quiesce,
	// detach or stop met between fetch and handler), though no handler runs.
	WaitForHandlers(ctx context.Context, timeout time.Duration) (int, error)

	// Stop closes the connection. AFTERWARDS EVERY ONE OF THE TWENTY-THREE VERBS
	// REFUSES: a requirement, not a capability, unlike the pre-Start rule
	// ("not connected yet" is a state JetStream's Open never has; "closed" is
	// one every backend reaches). A verb mutating through a closed client is
	// wrong on all of them: on the twin a PauseTopic hold outlived the client
	// and left the next life deaf; on JetStream the attachment verbs succeeded
	// while Publish failed, so shutdown could believe it had gated a
	// subscription that no longer existed. Whether Start can revive a stopped
	// queue is Capabilities.Restartable's question.
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

// JoinLane is what a drain has left to dispatch once lane mail that landed
// mid-drain joins it: partitioned and ordered again, so the lane's partitions
// run next and an event the drain held under the same key joins their turn.
func JoinLane[T any](rest []Partition[T], lane []T, key BatchKeyFunc, eventOf func(T) *events.Event) []Partition[T] {
	if len(lane) == 0 {
		return rest
	}
	var items []T
	for _, p := range rest {
		items = append(items, p.Items...)
	}
	return OrderForDispatch(PartitionByKey(append(items, lane...), key, eventOf), eventOf)
}

// OrderForDispatch is the one place BOTH levels of a drained batch's ordering
// are decided. Between partitions: one holding a priority event
// ([Prioritized]) first, so a person waiting on a seat is answered before
// background work, then oldest event first (receive order alone starves a quiet
// partition, whose requeued copies re-enter AFTER the hot one's). Within one:
// event timestamp, not delivery order, so returned messages read correctly
// beside newer ones. IT ORDERS ONE HANDLER CALL, NOTHING ACROSS TWO: a failed
// delivery in its backoff misses the drain carrying newer events, and the
// contract deliberately does not hold them back (a stall, a partition's one
// outcome charging poison to healthy mail, no guarantee). Stable sorts that
// fall back to arrival order: ordering must never block delivery.
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
// outcome. An Ack logs nothing: a line per success buries the ones that matter.
//
// A PASS IS NO FAILURE: a `handler_failed` at Warn for each would read as a
// fleet failing its mail during a roll, so it logs at Info under its own name.
// A FAILURE NAMES WHOSE IT IS (`fault`): the message's, or the node's own
// ([NakNode]), as the backend does when deciding where the message is kept.
// UNDER ctx, because the trace a context carries (internal/logging) correlates
// the line; a backend logging under its handler's context must not respell it.
func LogResult(ctx context.Context, l *slog.Logger, topic, group string, ev *events.Event, r Result) {
	switch r.Outcome {
	case OutcomeNak:
		l.WarnContext(ctx, "handler_failed", "topic", topic, "group", group,
			"event_type", eventType(ev), "error", errText(r.Err), "fault", faultOf(r))
	case OutcomeDefer:
		l.InfoContext(ctx, "delivery_deferred", "topic", topic, "group", group,
			"event_type", eventType(ev), "reason", r.Reason)
	case OutcomePass:
		l.InfoContext(ctx, "delivery_passed", "topic", topic, "group", group,
			"event_type", eventType(ev), "reason", r.Reason, "error", errText(r.Err))
	case OutcomeAside:
		l.InfoContext(ctx, "delivery_set_aside", "topic", topic, "group", group,
			"event_type", eventType(ev), "key", r.Key, "reason", r.Reason)
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
			"event_type", eventType(head), "error", errText(r.Err), "fault", faultOf(r))
	case OutcomeDefer:
		l.InfoContext(ctx, "batch_delivery_deferred", "topic", topic, "group", group,
			"batch_key", batchKey, "event_count", len(evs),
			"event_type", eventType(head), "reason", r.Reason)
	case OutcomePass:
		l.InfoContext(ctx, "batch_delivery_passed", "topic", topic, "group", group,
			"batch_key", batchKey, "event_count", len(evs),
			"event_type", eventType(head), "reason", r.Reason, "error", errText(r.Err))
	case OutcomeAside:
		l.InfoContext(ctx, "batch_delivery_set_aside", "topic", topic, "group", group,
			"batch_key", batchKey, "event_count", len(evs),
			"event_type", eventType(head), "key", r.Key, "reason", r.Reason)
	}
}

// faultOf names whose a failure is, on its line: the node's ([NakNode]) or the
// message's.
func faultOf(r Result) string {
	if r.NodeFault {
		return "node"
	}
	return "message"
}

// LogDeadLettered emits the standard line for a subscription's message whose
// delivery budget a return spent, once its backend has kept the copy:
// `dead_lettered`, at Error, with the event's type, its deliveries (the last
// included), the return that spent it (`returned`: nak, defer or pass), why
// (`reason`, empty for a failure) and the subject its copy is kept on
// ([RetiredTo]), which says poison or a delivery no member handled.
//
// ONE LINE FOR ONE SITUATION ON EVERY BACKEND, hence in the contract (for
// [LogBatchResult]'s reason): a twin spelling its own with redeliveries counts
// made a log consumer miss one backend's dead letters. UNDER ctx, for
// [LogResult]'s reason.
func LogDeadLettered(ctx context.Context, l *slog.Logger, topic, group string, ev *events.Event,
	deliveries int, spent Outcome, reason string, failed bool) {

	l.ErrorContext(ctx, "dead_lettered", "topic", topic, "group", group,
		"event_type", eventType(ev), "deliveries", deliveries, "returned", spent.String(),
		"reason", reason, "subject", RetiredTo(topic, group, failed))
}

// LogListenerPanic emits the standard line for a publish listener that
// panicked and was recovered, and [LogStreamHandlerPanic] the one for a
// stream handler.
//
// THE PANIC IS READ HERE, not passed in: these are called from inside the
// deferred function that recovered, so [panics.Recovered] still walks down
// through runtime.gopanic into the function that actually panicked. They live in the contract, as [LogBatchResult] does, so every backend spells
// one situation alike (an operator grepping `publish_listener_panicked` must
// see the twin too, which the tests run on). The value is logged under `panic`,
// not `error`: it is whatever panic() was given, and says the callback CRASHED
// rather than returned badly. Error level, not Warn: no redelivery follows.
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
