package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/workkey"
)

// Dispatcher turns one inbox partition into one turn.
//
// It is the frame that sits between the broker and the turn engine, and it
// exists as a type so the ORDER of what it does is one readable sequence
// rather than a method on the engine reaching for nine fields. Everything it
// calls is already tested on its own; what is tested HERE is the sequence.
type Dispatcher struct {
	// Screen answers the ownership and posture questions. The dispatcher
	// takes it as a function rather than reaching for a seat host, so the
	// ordering is exercisable without a lease table.
	Conditions func(handle string) inbox.Conditions

	// Ledgered reports whether the completion ledger records this event
	// type. Only those contribute to the work key.
	Ledgered func(eventType string) bool

	// Completions and Conversations are the two durable ledgers. Either may
	// be nil, which is the embedded single-node case: with no peer to race,
	// the seat lease is the whole mutual exclusion and there is nothing for
	// a completion ledger to add.
	Completions   ledgerstore.Completions
	Conversations ledgerstore.Conversations

	// Turn runs the seat's turn once the guards have passed.
	Turn func(ctx context.Context, req Request) (turn.Result, error)

	// Park requeues a partition and reports whether it landed. The
	// dispatcher acks only on true: acking a park whose requeue failed
	// drops the work entirely.
	Park func(ctx context.Context, handle string, evs []*events.Event) error

	// Pause takes the named hold on a seat's inbox before a park, so the
	// requeued copies buffer on the queue rather than looping straight
	// back. The screening names the hold ([inbox.Screening.Hold]), because
	// two subsystems hold inboxes this way and each lifts only its own.
	Pause func(ctx context.Context, handle string, hold inbox.Hold, reason string) error

	// HoldSandbox asks the sandbox coordinator for the hold it keeps on a
	// seat one of whose runs holds it ([inbox.HoldSandbox]), for a delivery
	// that reached the seat anyway — one that raced the hold, or found it
	// refused — before that delivery is deferred. The hold is the
	// coordinator's to take and lift, at its own transitions, so this asks
	// rather than takes: a hold taken here would be one the coordinator does
	// not know to lift when the run settles. Nil asks nothing, and the
	// deferral alone stands — a node with no coordinator has no run holding
	// any seat.
	HoldSandbox func(ctx context.Context, handle string)

	// Budget parks the seat when one of its capped token windows is
	// refusing, and reports the deferral reason naming the window — see
	// budgetpark.go. An error is a park that could not be taken, which
	// DEFERS the delivery rather than running a turn the counter refuses:
	// the refusal is the node's, and a deferral keeps the message's place.
	//
	// Nil parks nothing, which is a dispatcher with no counters: every
	// turn then runs, and its own meter, if it has one, is the gate.
	Budget func(ctx context.Context, handle string) (reason string, parked bool, err error)

	// Answer offers a delivery to a parked coding run as the reply to the
	// question it asked, and reports what to DO with the delivery — see
	// [sandbox.AnswerDisposition].
	//
	// THE ONE WAY A PERSON'S REPLY REACHES THE RUN THAT ASKED. A run that
	// stops to ask a person something gives its seat back, and the reply
	// arrives on the seat's own inbox looking like any other message.
	// Without this seam it is consumed as an unrelated turn: the run sits in
	// [sandbox.StatusAwaiting] until its box's pause TTL reclaims it, and
	// the person who answered is never told anything happened.
	//
	// IT ANSWERS A DISPOSITION RATHER THAN A BOOL, because the bool it used
	// to answer hid four outcomes behind two values and this frame read
	// every error as "not the answer" — so a claim that was taken and given
	// back, or one the store could not confirm, sent the person's reply on
	// to the ordinary route and it was consumed as an unrelated turn while
	// the run that asked waited out its pause TTL for a further message.
	//
	// Nil is a node with no coordinator, where no run is waiting.
	//
	// It takes the WHOLE delivery as a [sandbox.Reply]: the conversation
	// reference (the identity admits a run and the partition picks between
	// the runs it admits — see [sandbox.ConversationRef.Best]), the text,
	// and the events, whose
	// own instants are what decide whether the delivery was written after
	// the question it would answer ([sandbox.PendingRun.AskedAt]).
	Answer func(ctx context.Context, handle string, reply sandbox.Reply) (sandbox.AnswerDisposition, error)

	// AnswerByTurn hands a person's answer BY TURN — a
	// [types.SandboxAnswerGiven] on the seat's inbox — to the parked coding
	// run it names, and reports what to do with the delivery.
	//
	// ROUTED BEFORE THE SCREENING, and never a turn: see
	// [Dispatcher.routeAnswers]. The engine always sets it, to
	// [Engine.answerRunByTurn], which reads the sandbox runtime current when
	// the answer arrives and answers [sandbox.AnswerDeferred] while there is
	// none. Nil — a dispatcher built without the engine — is handed back
	// exactly as that deferral is: a node with no coordinator cannot resume
	// a run, so the delivery is the seat's next holder's rather than spent.
	AnswerByTurn func(ctx context.Context, given types.SandboxAnswerGiven,
		trigger *events.Event) (sandbox.AnswerDisposition, error)

	// NoteDeferred tells the seat host a consumer stopped, so the next
	// successful renew resumes it.
	NoteDeferred func(handle string)

	// Observe publishes an engine-side observability event.
	//
	// It takes a BUILT envelope rather than a payload, because
	// [events.New] is generic over the concrete payload type — a seam
	// typed on the interface could not construct one.
	//
	// BEST EFFORT and nil-safe: the feed is a feed. A node whose queue
	// refused a coalescing record has still coalesced correctly, and
	// failing the dispatch over it would trade real work for a row.
	Observe func(ctx context.Context, ev *events.Event)

	// Prompts resolves the third-party app registry a coalesced partition is merged
	// with — see [Dispatcher.promptRegistry].
	//
	// nil is an empty registry, which merges with the generic fallback's
	// pass-through supersede rule. That is the honest answer for a node with
	// no integrations, and it is what keeps a bare &Dispatcher{} in a test
	// coalescing rather than degrading.
	Prompts func() notify.Prompts

	// Conversation resolves the conversation-ledger policy for the turn
	// about to run.
	//
	// A FUNCTION, and read per dispatch rather than captured at
	// construction: the dispatcher is built once and the policy lives on
	// the company config, which a live apply replaces. A captured copy
	// would keep serving the revision the process started on.
	//
	// nil means the shipped defaults, which is what a test that does not
	// care about the policy wants.
	Conversation func() config.ConversationSession

	// Identify names the seat a handle is, as the role name and agent id
	// every seat-addressed event carries.
	//
	// FOR ONE RECORD ONLY: the guard breach a panic that escaped a turn's
	// own frames publishes. The live projection keys a seat by ROLE, so a
	// breach addressed by handle alone would reach no seat's failure, and the
	// turn's own telemetry, which does know the role, is exactly what did
	// not run.
	//
	// Nil publishes no breach, which is the honest answer for a dispatcher
	// that cannot say which seat it is: the panic is still logged, the
	// trigger still recorded and the delivery still settled.
	Identify func(handle string) (role, agentID string)

	// Now is injectable so a test can pin the clock.
	Now func() time.Time
}

// conversationPolicy is the resolved policy, defaulted when unset.
func (d *Dispatcher) conversationPolicy() config.ConversationSession {
	if d.Conversation == nil {
		return config.DefaultConversationSession()
	}
	return d.Conversation()
}

// Request is one dispatch's worth of work.
type Request struct {
	Handle string

	// Events is the partition, after dedupe and after the completion
	// ledger has dropped what was already worked.
	Events []*events.Event

	// WorkKey identifies this unit of work for the whole dispatch.
	WorkKey string

	// WorkSince is when that unit of work began — [inbox.WorkSinceFor],
	// over the same constituents as the key — and the instant every
	// operation id the turn derives from the key carries.
	WorkSince time.Time

	// RunID identifies THIS EXECUTION of it — minted per dispatch, so a
	// redelivered trigger (which re-derives the same WorkKey by design)
	// runs under an identity of its own. See ADR-0017.
	//
	// Empty means "assembled outside Dispatch", which is a test or a
	// direct driver; [Engine.runTurn] mints one rather than running a turn
	// with no identity.
	RunID string

	// Coalesce is true when the partition must be merged into one digest
	// trigger, so the seat runs one turn instead of N.
	Coalesce bool

	// Trigger is the ask this turn is GIVEN, as opposed to the bookkeeping
	// the partition is.
	//
	// The same events as [Request.Events] for an ordinary single-event
	// partition, and ONE merged digest event when the partition coalesced
	// (see mergeNotifications). The two are separate fields because they
	// answer to different readers and one value cannot serve both: the
	// completion ledger records the CONSTITUENT ids, so a redelivery of a
	// subset is droppable, while the model is handed one ask — and a digest
	// is minted fresh on every merge, so a ledger keyed on it would match
	// nothing and re-run the turn on every redelivery.
	Trigger []*events.Event

	// History is what this seat already said in this conversation.
	History []ledger.Session

	// ConversationKey is the surface-scoped conversation IDENTITY — the
	// durable thread this turn is part of — empty when the trigger has
	// none.
	//
	// The identity, never the inbox partition key beside it: this field is
	// what reaches the conversation ledger, the turn telemetry that becomes
	// the event store's conversation_key tag and the episodes column, and
	// the row a detached coding run reports back through.
	//
	// THE PARTITION KEY IS NOT A FIELD HERE. It is read straight off the
	// events at each of the four places that want it (see [partitionKeyOf])
	// — the coalescing record, whose whole subject is the batch that
	// merged; the turn telemetry, which carries it onto a detached run's
	// row so two runs parked on one direct message can be told apart; the
	// answer match, which uses it to DISAMBIGUATE between the rows the
	// identity admitted; and the digest's own stamp, which puts both keys
	// back on the merged envelope — plus the line logged when a partition
	// cannot be merged. Nothing above the dispatch has a use for it.
	//
	// That list said three readers that WANT the partition, and what
	// changed is not only the count: the parked run's answer match wanted
	// it ALONE and now takes both. The identity admits, because a person
	// answers on the conversation, and the partition only chooses between
	// what it admitted. The engine's own prompt tells a seat replying to a
	// top-level direct message to reply as a thread, so their answer
	// arrives in a finer partition than the question parked under and the
	// two strings never met.
	ConversationKey string

	// Depth is the delegation depth this turn inherited: zero for a turn a
	// person or a schedule started, higher for one a colleague asked for.
	// Carried so a sub-agent spawn or an A2A ask can refuse past the cap
	// rather than discovering the loop at runtime.
	Depth int

	// TimeoutSeconds is the wall-clock cap this turn's trigger carried, zero
	// when it carried none. Only a scheduled fire sets one — see
	// [types.TaskAssigned.TimeoutSeconds].
	TimeoutSeconds int

	// DelegationChain is who asked whom to get here. Provenance rather
	// than a gate — "alice → bob → alice" is exactly what happened — and
	// it travels so an ask this turn makes names the whole path instead of
	// only its immediate asker.
	DelegationChain []string
}

// Ask is the events a turn's task text is rendered from.
//
// [Request.Trigger] when the dispatcher set one, and the partition otherwise
// — a Request assembled anywhere but Dispatch (a resumed detached run, a
// test) carries no separate trigger and its partition IS its ask.
func (r Request) Ask() []*events.Event {
	if len(r.Trigger) > 0 {
		return r.Trigger
	}
	return r.Events
}

// Dispatch runs one partition.
//
// THE ORDER IS THE PRODUCT, and it is stated once here because every stage's
// position was earned by a specific failure — see internal/agent/inbox, which
// owns the guard sequence and the reasons.
//
// What this frame adds around it is the two ledger reads and the one ledger
// write, and their placement is equally deliberate:
//
//   - the completion read comes AFTER every parking branch, so a parked
//     partition is never marked done, and BEFORE coalescing, so recorded
//     constituents drop out and only the remainder merges;
//   - the sandbox ANSWER OFFER comes after that read and before the merge, on
//     the ordinary path as well as the park — because a run parked on a
//     question leaves its seat free, so the reply that resumes it arrives
//     here as an ordinary message and must be claimed before a turn eats it.
//     What the offer answers is what happens to the delivery: spent on the
//     run it resumed, HANDED BACK while a run is still owed it, or passed on
//     to the ordinary route. It is the disposition that decides and never the
//     error beside it — see [Dispatcher.answered];
//   - the conversation read comes after that, because it is keyed on a
//     conversation the surviving events name;
//   - the completion WRITE comes after the turn, and a turn that failed is
//     still recorded: the ledger answers "has this trigger been worked", not
//     "did the work succeed", and re-running a failing turn on every
//     redelivery is how one bad trigger becomes an infinite loop. A turn whose
//     PHASE broke is recorded too, but only when its own record proves it had
//     already reached outside the engine — see [Dispatcher.abandon] for why
//     that is a different question from "did it fail", and why answering it
//     with `err != nil` alone replayed a turn's external writes up to
//     twenty-five times.
//
// NO PANIC LEAVES THIS FRAME. A phase that panics is recovered inside the turn
// loop and comes back as an ordinary broken turn; anything else that panics
// between the broker and the loop, in the stages below or in the turn's own
// set-up and tear-down, is recovered here. Letting it through handed it to the
// queue backend's handler guard, which NAKs a panic, so a defect was run again
// up to the whole delivery budget. See [Dispatcher.recoverPanic].
func (d *Dispatcher) Dispatch(ctx context.Context, handle string, evs []*events.Event) (result queue.Result) {
	held := holding{events: evs}
	defer func() {
		if panicked := turn.Recovered(recover()); panicked != nil {
			result = d.recoverPanic(ctx, handle, held, panicked)
		}
	}()
	return d.dispatch(ctx, handle, evs, &held)
}

// holding is what one delivery is still answerable for, narrowed as the stages
// of [Dispatcher.dispatch] settle events or hand them back to the queue.
//
// A RECOVERED PANIC SETTLES THIS, NOT THE DELIVERY. Recording a constituent
// as worked is what stops its copies ever running, so recording the whole
// delivery would be wrong in three directions. The tail of a partition that
// would not merge is requeued before its head runs, and a panic in the head's
// turn would mark the tail worked while its copies sat on the queue waiting to
// be: the exact loss [inbox.Degraded] keys the head apart to prevent. A park
// hands the WHOLE delivery back the same way. And a constituent the completion
// ledger had already dropped would be put on the record a second time, as
// panicked, beside the record saying it had already been worked.
//
// EVERY NARROWING HAPPENS BEFORE THE REQUEUE IT DESCRIBES, never after. A
// requeue publishes one event at a time, so a panic inside one leaves some
// copies on the queue and the rest unpublished; narrowed afterwards, this
// would still be claiming the published ones at the moment they stopped being
// its to claim.
//
// AND THE RUN, once one exists. A panic after the run id is minted is a panic
// in a turn that has already announced itself — its start is on the record
// under that id — so the breach that says why it stopped has to name the same
// run, or the one row saying the turn began and the one saying it died are
// joined by nothing. Empty before the mint, which is the honest answer for a
// panic in a screening stage: no run existed to name.
type holding struct {
	events []*events.Event
	runID  string
}

// dispatch is [Dispatcher.Dispatch]'s body, separated so the recovery around
// it is one deferred call rather than a frame every return has to pass. It
// narrows held at each point where the delivery stops being answerable for an
// event.
func (d *Dispatcher) dispatch(ctx context.Context, handle string, evs []*events.Event, held *holding) queue.Result {
	// AN ANSWER BY TURN FIRST, before the screening can offer it to a parked
	// question or the ledger can read it: it is addressed to a run, never to
	// the seat, and whatever it becomes it is not a turn. What the screening
	// says about the seat and the node still stops it — a paused seat, or a
	// seat busy coding — see [Dispatcher.routeAnswers].
	//
	// ONE READING OF THE CONDITIONS for both, taken once at the top as the
	// screening's own contract asks: read twice, the answer stage could see
	// a node that may not run anything and hand the delivery on, and the
	// screening a moment later a node that may — which would PROCEED with
	// an answer in the partition and run it as a turn.
	conditions := d.conditions(handle)
	evs, answered, settled := d.routeAnswers(ctx, handle, conditions, evs)
	if settled {
		return answered
	}
	held.events = evs
	if len(evs) == 0 {
		return queue.Ack()
	}
	screening := inbox.Screen(conditions, evs)
	if screening.NoteDeferred && d.NoteDeferred != nil {
		d.NoteDeferred(handle)
	}
	switch screening.Action {
	case inbox.ActionDrop, inbox.ActionDefer:
		return screening.Result()
	case inbox.ActionPauseAndPark:
		if d.Pause != nil {
			if err := d.Pause(ctx, handle, screening.Hold, screening.Reason); err != nil {
				// The pause is what stops the requeued copies looping
				// back at whatever rate the broker will serve. Without it
				// the park is worse than doing nothing, so the delivery
				// is handed back rather than parked.
				//
				// DEFERRED, NOT NAKED: a hold the queue would not take is
				// this NODE's condition, not the message's, and a Nak
				// returns a message behind its conversation's newer mail
				// (queue.OutcomeNak) — which the next delivery of this
				// seat would then be screened ahead of, into the very
				// park this one could not set up. A deferral keeps its
				// place at the head and stops the attachment until the
				// seat host's next renew, which is the spacing a retry of
				// a refused queue call wants.
				if d.NoteDeferred != nil {
					d.NoteDeferred(handle)
				}
				return queue.Defer(fmt.Sprintf("engine: pause %s: %s", handle, err))
			}
		}
		return d.park(ctx, handle, screening.Events, held)
	case inbox.ActionHoldAndDefer:
		// A SEAT A DETACHED RUN HOLDS, reached by a delivery that raced the
		// coordinator's hold on it or found that hold refused. The hold is
		// asked for again — the retry a refused one gets — and the delivery
		// is DEFERRED: it keeps its place at the head of the inbox, and the
		// hold keeps it there until the run stops holding the seat.
		//
		// NEVER A PARK, which is what this was: a park republishes onto
		// the inbox the delivery was just fetched from, and with nothing
		// stopping the consumer the copy came straight back, for the
		// length of the run. See sandbox.SeatHold.
		if d.HoldSandbox != nil {
			d.HoldSandbox(ctx, handle)
		}
		return screening.Result()
	}

	// THE BUDGET, BEFORE THE DELIVERY IS CLAIMED: before the completion
	// ledger reads it, before a parked coding run is offered it as an
	// answer — resuming one charges tokens exactly as a turn does — and
	// before any model is asked anything. A seat whose capped window is
	// refusing cannot run a round, and a turn started anyway is refused on
	// its first charge and NAKed, again and again, until the broker
	// dead-letters a healthy message. Parked instead, it waits on its inbox
	// for the window to turn over. See budgetpark.go.
	//
	// AFTER THE SCREENING, whose every non-proceeding outcome already hands
	// the delivery on without running anything, so it costs those no read.
	if result, parked := d.parkOnBudget(ctx, handle); parked {
		return result
	}

	surviving, answeredInThread := d.dropWorked(ctx, handle, screening.Events)
	// WHAT THE LEDGER DROPPED, on the record.
	//
	// [types.TurnTriggerSkipped] was registered, categorised, documented as
	// shipped and produced by nothing, so the one case it exists for was
	// exactly as invisible as it was before the type was written: a turn
	// that finished, shipped its outbound effects, and whose delivery came
	// back — from a node that died before acking, or from a drain whose
	// partitions together outlasted the ack window. The feed then shows the
	// arrivals and one turn, and nothing at all distinguishes "the agent
	// never answered" from "the agent already answered". Emitted here
	// because here is the only frame that holds both lists.
	// The dropped constituents were settled by the turn that worked them.
	held.events = surviving
	d.noteSkipped(ctx, handle, screening.Events, surviving, answeredInThread)
	if len(surviving) == 0 {
		return queue.Ack()
	}

	// THE ANSWER IS CLAIMED BEFORE ANYTHING ELSE EATS IT.
	//
	// A run parked on a question does not hold its seat — that is the whole
	// design, the answer arrives on the seat's own inbox — so the reply
	// reaches here, on the ordinary path, looking like any other message.
	// Offered only while the seat was HELD, which is the one state a parked
	// run is never in, every clarification answer was consumed as an
	// unrelated turn while the box waited out its pause TTL. This is the
	// ONLY offer now: a seat a run holds has its inbox held, and an answer
	// to another of its runs waits there to be offered here when it lifts.
	//
	// AFTER THE LEDGER, because the completion ledger has already said
	// which of these events were worked, and a trigger that produced a turn
	// must not also be spliced into somebody's coding run.
	if screening.OfferAsSandboxAnswer && d.mayOfferAnswer(ctx, handle, surviving) {
		disposition, cause := d.answered(ctx, handle, surviving)
		switch disposition {
		case sandbox.AnswerConsumed:
			// Spent on the run it answered: the answer is recorded on
			// the run, the coordinator owns its resume from here, and no
			// turn runs on it here.
			d.SpendAnswer(ctx, handle, surviving)
			return queue.Ack()
		case sandbox.AnswerDeferred:
			// STILL OWED TO A RUN, and not recorded against it — the
			// seat's runs could not be read, or the record could not be
			// written — so it comes back rather than being worked.
			//
			// A DEFERRAL, AND NEVER A NAK, which is what this used to
			// be. Both failures are the NODE's or its STORE's, not the
			// message's, and a Nak is the one return that puts a message
			// BEHIND its conversation's newer mail: on the only broker
			// this engine ships a failure waits out its backoff while
			// never-delivered messages are served (queue.OutcomeNak). The
			// person's next message then reached the still-waiting run
			// first and was taken as its answer, and this one came round
			// afterwards to answer whatever the run asked next. A
			// deferral returns the delivery at the HEAD
			// (queue.OutcomeDefer), so it is still the first reply the
			// question is offered.
			//
			// AND IT STOPS THE SEAT'S INBOX until the seat host's next
			// renew resumes it, which is right for the condition it
			// reports: a store that cannot record an answer cannot run a
			// turn either, and the renew interval is the spacing the
			// retry gets. The delivery spends one of its deliveries, as
			// every hand-back does, which is why
			// [Dispatcher.mayOfferAnswer] stops offering while
			// [sandbox.AnswerDeliveryReserve] of them are left, and
			// [sandbox.MaxAnswerAttempts] bounds the series on this node.
			if d.NoteDeferred != nil {
				d.NoteDeferred(handle)
			}
			return queue.Defer(answerOwedReason(handle, cause))
		}
	}

	routing := inbox.Route(surviving, d.ledgered)

	// THE MERGE, here and only here, because this is the last frame that
	// holds the partition: below it a turn is one ask.
	//
	// The result is a SECOND list rather than a replacement — see
	// [Request.Trigger]. Everything the dispatcher derives from a partition
	// (the work key, the trace, the deepest delegation, the smallest wall
	// clock, the reply obligation, the senders and the interactions, and the
	// completion ledger's per-constituent record) keeps reading the
	// constituents, and only the ask a model is handed is merged.
	trigger := routing.Events
	if routing.Coalesce {
		merged, ok := mergeNotifications(d.promptRegistry(), routing.Events)
		if !ok {
			// A PARTITION THAT CANNOT BE MERGED degrades to per-event
			// dispatch: requeue the tail FIRST, then run the head in the
			// ack scope already open. The order is the point — a requeue
			// failure has to abort before any work has run, or a completed
			// turn is replayed by a later event's failure. Partially
			// requeued copies collapse on the next drain through the
			// same-id dedupe in [inbox.Screen].
			log.WarnContext(ctx, "partition_not_mergeable", "seat", handle,
				"partition", partitionKeyOf(routing.Events),
				"events", len(routing.Events),
				"detail", "a partition whose constituents are not all decodable "+
					"external notifications; dispatching per event")
			head, tail, headKey := inbox.Degraded(routing.Events, d.ledgered)
			if d.Park == nil {
				return queue.Nak(fmt.Errorf(
					"engine: %s: no requeue path for a partition that would not merge", handle))
			}
			// The tail's copies are the queue's the moment they are
			// published, and they are published one at a time: narrowed
			// AFTER the call, a panic part-way through it would record a
			// tail whose copies are already waiting to run. Only the head
			// is ever this delivery's to settle.
			held.events = head
			if err := d.Park(ctx, handle, tail); err != nil {
				return queue.Nak(fmt.Errorf("engine: requeue %s: %w", handle, err))
			}
			routing = inbox.Routing{
				WorkKey: headKey, Events: head,
				// THE HEAD'S OWN, for the reason its key is the head's
				// alone: only the head runs.
				WorkSince: inbox.WorkSinceFor(head, d.ledgered),
			}
			trigger = head
		} else {
			trigger = []*events.Event{merged}
		}
	}

	// THE TRIGGER'S TRACE, restored before anything below publishes or logs,
	// so this turn's spans hang under whatever caused it — a webhook, a
	// schedule, another agent — instead of each seat rooting a trace of its
	// own and the join a reader follows from "a message arrived" to "here is
	// what it did" never existing.
	//
	// It is bound FIRST of the three context values rather than beside them,
	// because the coalescing record and the conversation-history warning are
	// both emitted between here and there: bound after them, those would be
	// the two lines about a turn that do not name its trace, which is
	// precisely when an operator is looking for them.
	//
	// A COALESCED TURN HAS ONE TRACE AND SEVERAL CAUSES. The trace comes from
	// the first event of the partition, the same event describeTurn takes the
	// trigger from — ten Slack comments become one turn under one trace. The
	// others are already recorded as the turn's interactions; a span cannot
	// have two parents, and inventing a root to hold them would put a node
	// above the webhook that actually happened.
	ctx = tracing.WithRemote(ctx, triggerTrace(routing.Events))
	depth, chain := delegationOf(routing.Events)
	// MINTED HERE, at the one frame that knows a partition is about to
	// RUN, because both halves of the dispatch's own bookkeeping need it:
	// the conversation entry names the run a reader can open, and the
	// abandon record names the run that stopped. See ADR-0017.
	req := Request{
		RunID:  newRunID(),
		Handle: handle, Events: routing.Events, Trigger: trigger,
		WorkKey: routing.WorkKey, WorkSince: routing.WorkSince,
		Coalesce:        routing.Coalesce,
		TimeoutSeconds:  wallClockOf(routing.Events),
		ConversationKey: conversationIdentityOf(routing.Events),
		// READ OFF THE TRIGGER, and it was read off nothing: this field
		// was set at no site on the inbox path, so every turn ran at
		// depth 0, turn.CheckDepth could never fire, and
		// turn_engine.delegation_depth_limit bounded nothing at all. The
		// one guard against two agents asking each other the same
		// question until a budget runs out was inert.
		Depth:           depth,
		DelegationChain: chain,
	}
	// Held from the mint, before anything below can publish under it — the
	// turn's own start is the first thing that does. See [holding].
	held.runID = req.RunID
	// THE PARTITION, not the identity: this records that N events were
	// MERGED, and merging is what the partition key decides. The two differ
	// for a direct message's thread reply, where the record would otherwise
	// name the whole DM channel and say nothing about which batch collapsed.
	d.noteCoalesced(ctx, handle, partitionKeyOf(routing.Events), routing)
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if history, err := d.history(ctx, handle, req.ConversationKey); err == nil {
		req.History = history
	} else {
		// A conversation ledger that cannot be read is a seat with less
		// context, not a seat that cannot work. The read RAISES rather
		// than returning empty precisely so this decision is made here,
		// visibly, instead of a database outage looking like a first turn.
		log.WarnContext(ctx, "conversation_history_unreadable",
			"seat", handle, "conversation", req.ConversationKey, "error", err)
	}

	// The acting seat travels the same way, and for the same reason: the
	// only consumer is a leaf. A cli-agent provider gives every seat its
	// own CLI home, because seven seats on one subscription sharing one
	// home would read each other's transcripts — and an unbound call would
	// silently put them all back in one. Bound HERE, at the single place a
	// turn's context is built, rather than at each phase, so a new phase
	// cannot forget it.
	ctx = llm.WithSeat(ctx, handle)

	result, err := d.Turn(ctx, req)
	if err != nil {
		// A PERSON ENDED IT, which is neither a broken phase nor a retry:
		// see [Dispatcher.stopped]. First, because the stop is the account
		// of the turn's end that is TRUE — a turn stopped after it wrote
		// outside the engine was ended by somebody, not broken, and one
		// stopped on a moved seat would be handed to a successor that would
		// run exactly what the person stopped.
		if turn.Stopped(err) {
			return d.stopped(ctx, handle, req, err)
		}
		// A broken phase, not a failed turn. WHICH broken phase decides
		// what to do with the delivery, and `err != nil` does not say:
		// [turn.Abandon] is the one rule, shared with the sandbox resume.
		reason, abandon := turn.Abandon(result, err)
		if !abandon {
			// THE SAME CONDITION THE SCREENING ABOVE REFUSES, detected a
			// phase later: the seat's grant moved while the turn was
			// running and its fence closed. That is a healthy delivery a
			// successor is already entitled to, not a broken turn, so it
			// gets the disposition the screening gives it, deferred
			// rather than naked. Detected in two places because the
			// window is open the whole length of a turn; answered in one
			// way, because it is one condition.
			//
			// WHAT THE DEFERRAL BUYS IS SPEED AND SILENCE, NOT A FREE
			// DELIVERY. It spends one of the trigger's twenty-five
			// exactly as a failure does, on every backend — which is why
			// that budget is 25 rather than the ~10 a broker with a free
			// handoff would need. What it does buy is a return in about a
			// millisecond instead of the failure path's backoff, so the
			// seat's new owner sees the delivery now rather than waiting
			// out a delay this node earned, and a quiesced attachment, so
			// this process stops fetching further work it has equally
			// lost the right to do.
			//
			// BELOW [turn.Abandon] rather than above it: a turn that
			// panicked or that proved an outward write must not be run
			// again wherever the seat now lives, and handing it on would
			// do exactly that.
			if errors.Is(err, seat.ErrSeatMoved) {
				if d.NoteDeferred != nil {
					d.NoteDeferred(handle)
				}
				log.InfoContext(ctx, "turn_seat_moved", "seat", handle,
					"run_id", req.RunID, "work_key", req.WorkKey, "error", err.Error())
				return queue.Defer("the seat moved to another node mid-turn")
			}
			// AND THE BUDGET STAGE'S CONDITION, found a phase later: a
			// window that had room when the delivery was claimed and
			// none by this turn's round. Answered the way that stage
			// answers it — the seat is parked until the window turns
			// over — rather than by the NAK below, whose redelivery is
			// refused the same way until the budget of deliveries runs
			// out. Below [turn.Abandon] for the reason the seat-moved
			// branch is: a turn that proved an outward write is
			// recorded, not run again when the window resets.
			if errors.Is(err, toolloop.ErrBudgetExhausted) {
				if result, parked := d.parkOnBudget(ctx, handle); parked {
					log.InfoContext(ctx, "turn_budget_parked", "seat", handle,
						"run_id", req.RunID, "work_key", req.WorkKey, "error", err.Error())
					return result
				}
			}
			// Nothing this turn did can be proven to have left the
			// engine, and nothing about the failure says it will recur,
			// so a redelivery really does run it cleanly.
			//
			// A NAK, and so it comes back BEHIND the conversation's
			// newer mail (queue.OutcomeNak): the spaced, budgeted retry a
			// failing turn needs, which a deferral is not. What that
			// order costs is closed where it would bite — a newer
			// message's turn that was shown this one waiting in its
			// thread answers the thread as it stands and records it as
			// worked through, so this delivery is dropped when it comes
			// round rather than answered a second time, out of order.
			// See workedthrough.go.
			return queue.Nak(fmt.Errorf("engine: turn for %s: %w", handle, err))
		}
		return d.abandon(ctx, handle, req, err, reason)
	}
	d.recordWorked(ctx, handle, req, result)
	return queue.Ack()
}

// parkOnBudget runs the budget stage, reporting the disposition when it parked
// the seat or could not take the park it decided on.
//
// A PARK IS A DEFERRAL, and noted as one, so the seat host resumes the
// attachment the deferral quiesces on its next renew — the park's own hold is
// what keeps the resumed attachment from being handed anything until the
// window turns over. See budgetpark.go for why the two stops have two owners.
func (d *Dispatcher) parkOnBudget(ctx context.Context, handle string) (queue.Result, bool) {
	if d.Budget == nil {
		return queue.Result{}, false
	}
	reason, parked, err := d.Budget(ctx, handle)
	if err != nil {
		// THE PARK COULD NOT BE TAKEN — the hold the budget stage
		// decided on was refused by the queue — which is this NODE's
		// condition rather than the message's. Deferred, so the delivery
		// keeps its place at the head of the seat's inbox and comes back
		// first once the seat host's next renew resumes the attachment; a
		// Nak would return it behind the conversation's newer mail
		// (queue.OutcomeNak), and the turn that next ran on this seat
		// would be answering a later message first.
		if d.NoteDeferred != nil {
			d.NoteDeferred(handle)
		}
		return queue.Defer("budget park could not be taken: " + err.Error()), true
	}
	if !parked {
		return queue.Result{}, false
	}
	if d.NoteDeferred != nil {
		d.NoteDeferred(handle)
	}
	return queue.Defer(reason), true
}

// abandon stops redelivering a trigger whose turn must not run again.
//
// THE REDELIVERY IS THE HARM HERE, not the failure. A broken phase used to NAK
// unconditionally, on the premise that "nothing was recorded, so a redelivery
// runs it cleanly". That premise holds for the two writes [workkey] guards and
// for nothing else: every MCP write, every chat post, every `a2a_ask` (a fresh
// channel per call) and every `run_sandbox` is keyed on nothing at all. A
// deterministic mid-turn failure therefore replayed round one's external
// effects up to the broker's whole delivery budget: twenty-five attempts spaced
// by a backoff that doubles from a second to thirty, so about ten minutes of
// them, and the seat's own colleagues, issue trackers and billed boxes wore
// every one.
//
// So the trigger is recorded and acked. That LOSES the rest of the turn, which
// is the honest price and the reason it needs a reason rather than suspicion:
// [turn.Abandon] gives one only for a turn whose record proves it reached
// outside, or for a panic, which runs the same defect again however many times
// it is delivered. Everything else (a provider that never answered, a runner
// that could not be built, a refused budget) proves nothing and keeps its
// retry. A seat handed to another node mid-turn keeps its retry too, but as a
// DEFERRAL rather than a NAK: see the branch above, and [seat.Host.Fence] for
// what detects it. So does a refused budget, whose seat is parked until the
// refusing window turns over rather than NAKed into the same refusal: see
// [Dispatcher.parkOnBudget].
//
// It is not silent. A turn that ran has already published its own completion
// marked failed (see [Engine.publishTurnCompleted], which fires on the error
// path), and this adds the half that record cannot carry: that the trigger
// behind it will not come back, and why. Both halves name the same seat and the
// same work key.
func (d *Dispatcher) abandon(ctx context.Context, handle string, req Request, cause error, reason string) queue.Result {
	// BOTH IDENTITIES. This record is paired with the turn's own completion
	// event (see [Engine.publishTurnCompleted]) and the two are joined by a
	// reader: the completion names the RUN, so a line carrying only the work
	// key stopped joining it the moment the two stopped being one value.
	log.ErrorContext(ctx, "turn_abandoned", "seat", handle,
		"run_id", req.RunID, "work_key", req.WorkKey, "reason", reason,
		"error", cause.Error(),
		"detail", "the trigger is recorded rather than redelivered")

	// The completion rows ONLY, never RecordSession. A broken turn has no
	// reply to file: its artifact is whichever round closed last, and
	// writing that to the thread as this turn's answer is the bug
	// [Dispatcher.RecordSession]'s own doc commemorates for the suspended
	// case, because the next turn reads it back as what this one did.
	if d.Completions != nil {
		for _, ev := range req.Events {
			if ev == nil || !d.ledgered(ev.Type) {
				continue
			}
			key := workkey.Derive([]string{ev.ID.String()})
			if err := d.Completions.Record(ctx, handle, key, "", d.now()); err != nil {
				log.WarnContext(ctx, "abandoned_trigger_not_recorded", "seat", handle,
					"error", err, "detail", "the trigger may be redelivered and "+
						"repeat this turn's work")
			}
		}
	}
	d.noteAbandoned(ctx, handle, req.Events, cause, reason)
	return queue.Ack()
}

// stopped settles a delivery whose turn a person stopped.
//
// SPENT, NOT RETRIED. The trigger is recorded in the completion ledger and the
// delivery acked, exactly as a finished turn's is, because a NAK would run the
// turn again — on this node the moment the pause is lifted, or on a peer
// sooner — and the person stopped it precisely so that it would not go on.
// What they asked for next is a new ask, and the seat's other mail waits on
// its held inbox for the resume.
//
// ON THE RECORD, three ways, each saying what only it can: the turn's own
// completion reads `stopped` rather than `failed` (see
// [Engine.publishTurnCompleted]), this frame publishes
// [types.AgentTurnStopped] naming who stopped it, and the log line joins the
// run to the work key. No [types.TurnTriggerSkipped]: the trigger WAS worked,
// as far as anybody asked it to be.
func (d *Dispatcher) stopped(ctx context.Context, handle string, req Request, cause error) queue.Result {
	pause, _ := stopOf(cause)
	log.InfoContext(ctx, "turn_stopped", "seat", handle, "run_id", req.RunID,
		"work_key", req.WorkKey, "by", pause.By, "person", pause.Seat,
		"detail", "a person paused this seat and asked for its running turn to stop; "+
			"the trigger is recorded as worked rather than redelivered")
	if d.Completions != nil {
		now := d.now()
		for _, ev := range req.Events {
			if ev == nil || !d.ledgered(ev.Type) {
				continue
			}
			key := workkey.Derive([]string{ev.ID.String()})
			if err := d.Completions.Record(ctx, handle, key, "", now); err != nil {
				log.WarnContext(ctx, "stopped_trigger_not_recorded", "seat", handle,
					"error", err, "detail", "the trigger may be redelivered and run the "+
						"turn a person stopped once the seat is resumed")
			}
		}
	}
	if d.Observe != nil {
		var role, agentID string
		if d.Identify != nil {
			role, agentID = d.Identify(handle)
		}
		d.Observe(ctx, turnStoppedEvent(handle, role, agentID, req.RunID, req.WorkKey,
			pause, triggerTrace(req.Events)))
	}
	return queue.Ack()
}

// turnStoppedEvent is the record of a turn a person stopped, built once for
// both paths a turn runs on — a dispatched turn and a resumed one.
func turnStoppedEvent(handle, role, agentID, turnID, workKey string,
	pause coord.SeatPause, trace events.TraceContext,
) *events.Event {
	rec := events.New(types.AgentTurnStopped{
		Agent: agentID, AgentHandle: handle, RoleName: role,
		TurnID: turnID, WorkKey: workKey,
		StoppedBy: pause.By, StoppedBySeat: pause.Seat, Reason: pause.Reason,
	}, trace)
	// The seat's role, as every turn-scoped event's source is, so the feed
	// attributes the row to the seat rather than to "system".
	rec.Source = role
	if rec.Source == "" {
		rec.Source = "engine.dispatch"
	}
	return rec
}

// recoverPanic settles a delivery whose handling panicked outside the turn
// loop.
//
// ACKED AND RECORDED, never NAKed and never deferred, for the reason
// [turn.Abandon] gives for every panic: a redelivery runs the same defect on
// the same input, and a defer would only hand that defect to a peer running the
// same build. What the panic cost is put on the record instead: the stack in
// the log, the unhandled-exception guard on the seat, so it renders its failure
// with a cause rather than as whatever it was last doing, and a skipped-trigger record
// per event saying it will not come back.
//
// held is what the delivery still held when it panicked (see [holding]), not
// everything it arrived with. The work key is recomputed from its events,
// because the panic may have happened before the frame that derives it ran; it
// is the same derivation, so the breach and the skipped records name the key a
// completed turn would have carried. The RUN is not recomputable — it is a
// mint, not a derivation — so it is whatever the delivery had minted, and
// nothing when it had not.
func (d *Dispatcher) recoverPanic(ctx context.Context, handle string, held holding,
	panicked *turn.PanicError,
) queue.Result {
	log.ErrorContext(ctx, "dispatch_panicked", "seat", handle, "events", len(held.events),
		"run_id", held.runID, "panic", panicked.Value, "stack", panicked.Stack)
	req := Request{
		RunID: held.runID, Handle: handle, Events: held.events,
		WorkKey: inbox.WorkKeyFor(held.events, d.ledgered),
	}
	d.noteBreach(ctx, handle, req, panicked)
	return d.abandon(ctx, handle, req, panicked, turn.AbandonedPanicked)
}

// noteBreach publishes the unhandled-exception guard for a panic that no
// turn's own telemetry reported.
func (d *Dispatcher) noteBreach(ctx context.Context, handle string, req Request, panicked *turn.PanicError) {
	if d.Observe == nil || d.Identify == nil {
		return
	}
	role, agentID := d.Identify(handle)
	if rec := panicBreach(role, agentID, req.RunID, req.WorkKey, triggerTrace(req.Events), panicked); rec != nil {
		d.Observe(ctx, rec)
	}
}

// panicBreach is the unhandled-exception guard for a panic no turn's own
// telemetry reported, or nil when there is no seat to address it to.
//
// One builder for the two frames that need it, the dispatcher and the sandbox
// resume, so a breach reads the same whichever path the panic took.
//
// BOTH IDENTITIES, as every other breach carries them ([Engine.publishFailure]):
// the RUN in `turn_id`, because that is the key every turn event is joined on,
// and the unit of work beside it. The dispatcher's used to put the work key in
// the turn id's slot — the shape from before ADR-0017 split the two — so the
// breach of a turn that died joined no turn at all, least of all the one whose
// start said it had begun.
func panicBreach(role, agentID, runID, workKey string, trace events.TraceContext,
	panicked *turn.PanicError,
) *events.Event {
	if role == "" {
		// A handle this company does not name has no seat to fail, and
		// a breach addressed to nobody moves nothing.
		return nil
	}
	rec := events.New(types.TurnGuardBreach{
		Agent:    agentID,
		RoleName: role,
		Kind:     types.GuardUnhandledException,
		Detail:   events.ClipDiagnostic(panicked.Error()),
		TurnID:   runID,
		WorkKey:  workKey,
	}, trace)
	// SOURCED AS THE SEAT, as every turn-level event is (see
	// [Engine.publishEvent]): a consumer with no other attribution renders
	// an unsourced event as "system", and this one is about the seat.
	rec.Source = role
	return rec
}

// noteAbandoned puts each abandoned constituent on the record.
//
// [types.TurnTriggerSkipped] rather than a type of its own: it already means
// "this trigger will not be worked, and here is why", and its Reason is the
// field that says which why. The FAILURE is already red in the feed, carried by
// the turn's own completion or by the guard breach, and a second failure event
// for one turn would double-count it in every projection that reads them.
func (d *Dispatcher) noteAbandoned(ctx context.Context, handle string, evs []*events.Event,
	cause error, reason string,
) {
	if d.Observe == nil {
		return
	}
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		rec := events.New(types.TurnTriggerSkipped{
			AgentHandle: handle,
			TriggerID:   ev.ID.String(),
			TriggerType: ev.Type,
			Reason: reason + ", so it was not redelivered: " +
				textcut.Ellipsis(cause.Error(), 200),
		}, triggerTrace([]*events.Event{ev}))
		rec.Source = "engine.dispatch"
		d.Observe(ctx, rec)
	}
}

// mayOfferAnswer reports whether this delivery still has the deliveries to
// spare for the answer route, on what the MESSAGES carry.
//
// THE OFFER IS WHAT IS GATED, not the hand-back. A reply the coordinator could
// not record is handed back with a deferral, and on the broker this engine
// ships every return spends one of the message's deliveries — so a route that
// kept offering until the last one would hand the twenty-fifth back and the
// broker would dead-letter a person's reply, which is the one ending this
// route must never take. The
// coordinator's own ceiling cannot prevent that: it is per node and per
// process, and it resets on exactly the event (a seat handoff, a restart)
// that does NOT reset the count the broker enforces. See
// [sandbox.AnswerDeliveryReserve], which owns the rule and the number, and
// [sandbox.MaxAnswerAttempts], which is the second clause and bounds one
// process's thrash.
//
// EACH EVENT'S OWN COUNT, never the partition's, and the distinction is the
// whole of this frame. [queue.DeliveriesLeft] is the SMALLEST of the
// partition's — the right answer to "will handing this batch back dead-letter
// something" and the wrong one to the question asked here, which is whether
// the reply this route may be owed can still afford an attempt. Read as the
// latter it refused a clarification reply on its FIRST delivery, with a whole
// budget in hand, because some older message on the same conversation was
// near its own — and it kept refusing every later reply on that conversation,
// since a spent message stays in the partition. So the gate asks
// [queue.DeliveriesLeftFor] per event and [sandbox.MayOfferAnswer] folds
// them, which is where the reasoning for that fold lives.
//
// BOTH OFFERS ARE GATED, the held seat's as well as the free seat's. The held
// seat's used to be exempt, because a delivery it did not consume was
// REPUBLISHED by the park — a new message with a budget of its own. An answer
// still owed is no longer parked: a republish lands at the TAIL of the inbox,
// behind the person's next message, which would then be offered to the
// question first. So both hand it back by deferral, both spend the count this
// guard protects, and both stop offering at the same reserve.
//
// ON THIS NODE'S LOG EITHER WAY, and the two lines are different facts.
// Refusing means the delivery becomes an ordinary turn while a coding run may
// still be parked on its question — the answer route giving up its claim on a
// message, invisible anywhere else. Offering while the PARTITION's own number
// is inside the reserve means the hand-back this may cause will also spend a
// co-partitioned message that has nearly nothing left, which is the cost this
// gate deliberately accepts and therefore the one an operator reading a
// `dead_lettered` line needs told in advance.
func (d *Dispatcher) mayOfferAnswer(ctx context.Context, handle string, evs []*events.Event) bool {
	perMessage := make([]sandbox.AnswerHeadroom, 0, len(evs))
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		left, known := queue.DeliveriesLeftFor(ctx, ev.ID)
		perMessage = append(perMessage, sandbox.AnswerHeadroom{Left: left, Known: known})
	}
	least, leastKnown := queue.DeliveriesLeft(ctx)
	if !sandbox.MayOfferAnswer(perMessage) {
		log.WarnContext(ctx, "sandbox_answer_headroom_reserved",
			"seat", handle, "deliveries_left", least,
			"reserve", sandbox.AnswerDeliveryReserve,
			"detail", "every message of this delivery has spent nearly all of its "+
				"deliveries, so what is left is kept for the ordinary route rather "+
				"than offered to a parked coding run again; handing it back once "+
				"more risks the broker dead-lettering the reply instead")
		return false
	}
	if leastKnown && least <= sandbox.AnswerDeliveryReserve {
		log.WarnContext(ctx, "sandbox_answer_offered_over_a_spent_sibling",
			"seat", handle, "partition_deliveries_left", least,
			"reserve", sandbox.AnswerDeliveryReserve,
			"detail", "a message of this delivery still has the deliveries to spare, "+
				"so a parked coding run is being offered it; another message in the "+
				"same partition is inside the reserve, and a hand-back returns the "+
				"whole partition, so that one may dead-letter")
	}
	return true
}

// answered offers a delivery to a coding run of this seat that is waiting for
// somebody to answer its question, and reports what to do with it.
//
// FAIL-OPEN WHERE NOTHING WAS MATCHED. A missing seam, a partition with no key
// at all and a lookup the coordinator could not make all report
// [sandbox.AnswerNotMine], and the delivery goes on to whatever the screening
// said to do with it — parked behind a held seat, or run as the ordinary turn
// it looks like — which is recoverable, where acking a message nothing handled
// is not.
//
// AND HANDED BACK WHERE A RUN IS STILL OWED IT. That is the other half, and
// the one this frame used to get wrong: a run the coordinator matched and
// could not resume is still waiting for this exact message, so falling through
// would spend it on an unrelated turn. [sandbox.AnswerDeferred] is the
// coordinator saying so, and the caller returns the delivery rather than
// working it — see [Dispatcher.handBackAnswer] for why that return is a NAK.
//
// THE CONVERSATION IDENTITY is the disambiguation, and the partition travels
// beside it to tell apart runs one conversation admits. The rule — "the
// next inbound on the question's conversation IS the answer" — is positional
// within a CONVERSATION rather than within a batch, and the engine's own chat
// prompt is what forces the distinction: a run launched from a top-level
// direct message parks under the bare DM channel, while [notify.ChatPrompt]
// tells the seat to reply as a thread, so the person's answer arrives in a
// partition the row never named. Offered the partition, the coordinator
// compared two strings that could not meet and the box waited out its pause
// TTL with the answer sitting in this very inbox.
//
// BOTH VALUES GO, because the identity decides which runs a delivery may
// answer and the partition which of them it does. See
// [sandbox.ConversationRef.Best].
//
// AN ERROR EXPLAINS THE DISPOSITION AND NEVER OVERRIDES IT, which is the exact
// inversion of what this frame used to do. The coordinator classifies its own
// failures — it is the only frame that can, since it holds the run — and hands
// back both: the disposition to act on and the error to log. Reading the error
// instead is what spent a person's answer on an unrelated turn on the one
// route where the run was still owed it. BOTH TRAVEL OUT of this function for
// the same reason they travel in: a NAK carries a cause, and the cause a
// caller would otherwise invent is the one thing it does not know.
//
// AN UNUSABLE DISPOSITION FALLS BACK TO [sandbox.AnswerNotMine], not to a
// requeue. Nothing in this build produces one — the seam has a single
// implementation — so it means a wiring that answered nothing, and the honest
// reading of that is a node with no coordinator, which is a state the engine
// supports and which cannot loop. A defer would requeue with nothing to bound
// it: the bound lives with the RUN the coordinator matched, and a disposition
// this frame cannot read names no run.
func (d *Dispatcher) answered(ctx context.Context, handle string, evs []*events.Event) (sandbox.AnswerDisposition, error) {
	if d.Answer == nil {
		return sandbox.AnswerNotMine, nil
	}
	conv := sandbox.ConversationRef{
		Identity:  conversationIdentityOf(evs),
		Partition: partitionKeyOf(evs),
	}
	if conv.Identity == "" {
		return sandbox.AnswerNotMine, nil
	}
	disposition, err := d.Answer(ctx, handle, sandbox.Reply{
		Conv: conv, Text: DescribeTrigger(evs), Events: evs,
	})
	if err != nil {
		// BOTH KEYS, for the reason the offer carries both: a line naming
		// the identity alone cannot say which of its runs was picked.
		// AND THE DISPOSITION, because the failure alone no longer says
		// what became of the delivery.
		log.WarnContext(ctx, "sandbox_answer_dispatch_failed",
			"agent_handle", handle, "conversation", conv.Identity,
			"partition", conv.Partition, "disposition", disposition.String(),
			"error", err)
	}
	if !disposition.Valid() {
		log.ErrorContext(ctx, "sandbox_answer_disposition_unknown",
			"agent_handle", handle, "conversation", conv.Identity,
			"disposition", disposition.String(),
			"detail", "the sandbox coordinator answered with no disposition this build "+
				"knows, so the delivery is handled as it would be on a node with no "+
				"coordinator at all")
		return sandbox.AnswerNotMine, err
	}
	return disposition, err
}

// routeAnswers settles every answer BY TURN in a delivery, and returns what is
// left for the ordinary route.
//
// # Before the screening, and what still stops it
//
// First because the answer is addressed to a run, never to the seat: the
// screening's decisions about what a seat does with its mail — offer it to a
// parked question, run it as a turn — are not this delivery's.
//
// What the screening decides about the SEAT AND THE NODE still applies, and is
// left to it: a node that does not hold the seat, has no turn engine, or is
// refusing new work under a stale company runs no resume either, so on any of
// those the whole delivery goes to the screening untouched, which defers or
// parks it — and an answer that comes back is routed here again. A seat parked
// on its budget waits the same way: a resume charges tokens exactly as a turn
// does. A PAUSED seat takes nothing off its inbox at all, this included, which
// is what "an answer waits behind a pause" means. And A SEAT BUSY CODING takes
// no other work until that run settles or parks, a person's answer to another
// of its runs included: the agent is in the middle of one job, and resuming a
// second beside it would put two of its turns in flight at once. Its inbox is
// held for exactly that long (sandbox.SeatHold), so an answer reaches here on
// a held seat only by racing the hold; it goes to the screening, which defers
// it under the hold, and it is offered first when the run stops holding the
// seat — as the chat route's reply is.
//
// # A wait never pairs it with a later question
//
// Every one of those waits — a pause, a busy seat, a NAK's backoff, a retried
// copy — can outlast the question the answer was given against: the run is
// answered another way meanwhile, resumes, calls run_sandbox again and parks
// on a new question, and the held answer is the first thing offered when the
// seat frees. The answer names the question it answers
// ([types.SandboxAnswerGiven.LaunchID]), so the coordinator resumes the run
// with it only while the run still waits on that one, and spends it as
// `not_awaiting` otherwise — rather than resuming the run a second time with
// the first question's answer presented as the second's.
//
// # Never a turn
//
// Every disposition but one spends the delivery: the answer was recorded on
// its run — whose resume is the coordinator's from there, as a chat reply's
// is — or the run was not waiting, let this answer go, or is gone, each
// announced by the coordinator; and there is nothing else an answer addressed
// to a run can become. [sandbox.AnswerDeferred] — the answer could not be
// recorded — hands the delivery back with a NAK, the spaced return, bounded by
// the broker's own budget; see [sandbox.Coordinator.AnswerByTurn] for why
// nothing shorter bounds it.
//
// # Spent once, and said once
//
// A delivery spent here is recorded as worked in the completion ledger
// ([Dispatcher.SpendAnswer]), and so is one its run spent — taken by a resumed
// turn, let go of, reaped ([sandbox.CoordinatorOptions.Spent]); and the ledger
// is read BEFORE the coordinator is asked. The original comes round when the
// node that took it stopped before acknowledging it — mid-turn, with the turn's
// reply perhaps already sent — and by then its run has been resumed, relaunched,
// revived and resumed by the next holder, or reaped. Asked again, the
// coordinator found nothing on the run that recognised it, and announced the
// answer `gone` or `not_awaiting` after it had resumed the run — or as the only
// word on it, where the node stopped before announcing `resumed`. The ledger
// does not depend on the run still holding the answer. A copy a let-go hands
// back goes under an id of its own, so it is announced once, as `declined` or
// `gone`, before it is recorded in turn.
//
// ONE ANSWER PER DELIVERY IN PRACTICE — the event names no conversation, so it
// partitions on its own id — but the rule holds for any mix: the answers are
// settled first, a hand-back returns the delivery before anything else in it
// has run, and what is left goes on as the ordinary delivery it is.
func (d *Dispatcher) routeAnswers(ctx context.Context, handle string, c inbox.Conditions,
	evs []*events.Event,
) ([]*events.Event, queue.Result, bool) {
	var answers, rest []*events.Event
	for _, ev := range evs {
		if _, ok := events.DataAs[*types.SandboxAnswerGiven](ev); ok {
			answers = append(answers, ev)
			continue
		}
		rest = append(rest, ev)
	}
	if len(answers) == 0 {
		return evs, queue.Result{}, false
	}
	if !c.Owned || !c.TurnEngineReady || !c.AdmitsTriggers || c.Paused || c.PauseUnknown ||
		c.SeatHeldBySandbox {
		return evs, queue.Result{}, false
	}
	if result, parked := d.parkOnBudget(ctx, handle); parked {
		return nil, result, true
	}
	for _, ev := range answers {
		given, _ := events.DataAs[*types.SandboxAnswerGiven](ev)
		if d.answerSpent(ctx, handle, ev) {
			log.InfoContext(ctx, "sandbox_answer_by_turn_already_spent",
				"agent_handle", handle, "turn_id", given.TurnID, "event_id", ev.ID.String(),
				"detail", "this answer by turn was spent before — taken by a resumed turn, let go "+
					"of, or settled — so its redelivery is acknowledged without reaching its run "+
					"or being announced again")
			continue
		}
		if d.AnswerByTurn == nil {
			// THE SAME HAND-BACK a node with no runtime gives: nil is a
			// dispatcher assembled without the engine (a test's), since
			// the engine always wires [Engine.answerRunByTurn], which
			// answers deferred when no coordinator is running. One
			// return, so the two cannot drift apart.
			return nil, d.handBackAnswer(handle, fmt.Errorf("this node holds no "+
				"sandbox coordinator to resume run %s with the answer it was given",
				given.TurnID)), true
		}
		disposition, err := d.AnswerByTurn(ctx, *given, ev)
		switch {
		case disposition == sandbox.AnswerDeferred:
			return nil, d.handBackAnswer(handle, err), true
		case !disposition.Valid():
			log.ErrorContext(ctx, "sandbox_answer_disposition_unknown",
				"agent_handle", handle, "turn_id", given.TurnID,
				"disposition", disposition.String(),
				"detail", "the sandbox coordinator answered an answer by turn with no "+
					"disposition this build knows; it is spent, because an answer "+
					"addressed to a run is never a turn")
		case err != nil:
			log.WarnContext(ctx, "sandbox_answer_by_turn_failed",
				"agent_handle", handle, "turn_id", given.TurnID,
				"disposition", disposition.String(), "error", err)
		}
		// SPENT, whatever it became: a redelivery whose acknowledgement was
		// lost has already been announced, and is acknowledged above.
		d.SpendAnswer(ctx, handle, []*events.Event{ev})
	}
	return rest, queue.Result{}, false
}

// answerSpent reports whether an answer by turn's delivery is recorded as
// worked in the completion ledger — spent by this route, or by the run it
// answered ([Dispatcher.SpendAnswer]). FAILS OPEN, as every ledger read does:
// unreadable, the delivery reaches the coordinator, which finds what the run
// says about it.
func (d *Dispatcher) answerSpent(ctx context.Context, handle string, ev *events.Event) bool {
	if d.Completions == nil {
		return false
	}
	key := workkey.Derive([]string{ev.ID.String()})
	return d.Completions.Worked(ctx, handle, []string{key})[key]
}

// handBackAnswer returns an answer BY TURN that the parked coding run it names
// is still owed and could not be recorded against, so the broker offers it
// again.
//
// A NAK, and on this route — and only this one — that is right. The delivery
// names its run, so where it comes back relative to the seat's other mail
// decides nothing: no later message can be taken as its answer, and it cannot
// be taken as the answer to anything else. What the Nak buys is SPACING — the
// queue backs a failed delivery off (seed, doubling, ceiling), so the attempts
// are spread across the minutes a seat handoff, a config apply or a store blip
// actually take — and the message's IDENTITY, which a republish would replace
// with a new message whose delivery budget starts over on every copy. Its
// bound is that budget: an answer by turn has nowhere else to go, so it is
// dead-lettered loudly rather than dropped (see
// [sandbox.Coordinator.AnswerByTurn]).
//
// A CHAT REPLY IS NEVER HANDED BACK THIS WAY. It names no run, so a Nak —
// which returns a message BEHIND its conversation's newer mail
// (queue.OutcomeNak) — let the person's next message reach the question first
// and be taken as its answer. A chat reply is recorded on the run instead and
// its resume retried by the coordinator, and the one hand-back it still has,
// when the record itself could not be made, is a deferral that keeps its
// place (see [Dispatcher.dispatch]).
//
// The cause travels into the NAK because the queue logs it and the
// dead-letter boundary reads it: "this seat is still owed this answer" with
// the coordinator's own failure under it is the whole explanation, and it is
// the one a reader of a dead-lettered inbox event needs.
func (d *Dispatcher) handBackAnswer(handle string, cause error) queue.Result {
	if cause == nil {
		// A deferral always carries one today, and a NAK with no error
		// would be the one line in the log that says nothing at all.
		cause = errors.New("the coordinator could not hand it to the run that asked")
	}
	return queue.Nak(fmt.Errorf("engine: %s is still owed this answer: %w", handle, cause))
}

// answerOwedReason is the deferral reason a chat reply is handed back with
// while a parked run is still owed it.
func answerOwedReason(handle string, cause error) string {
	if cause == nil {
		return fmt.Sprintf("%s is still owed this answer", handle)
	}
	return fmt.Sprintf("%s is still owed this answer: %s", handle, cause)
}

// SpendAnswer records the deliveries a parked run's answer arrived in as
// worked, in the completion ledger: a chat reply once it is recorded on its
// run, an answer by turn once its route has settled it, and either once its run
// spends it — taken by a resumed turn, let go of, reaped
// ([sandbox.CoordinatorOptions.Spent], which the engine wires here).
//
// THE ANSWER IS SPENT, and a copy of it must become nothing: a redelivery
// whose acknowledgement was lost, or a park's republished copy, would
// otherwise reach a seat whose question is no longer waiting — the recorded
// answer is matched only while its run still holds it — and be run as an
// ordinary message the run has already been resumed with, or, for an answer
// by turn, reach a run that has moved on and be announced a second time. The
// ledger is the fleet's, so the copy is dropped on whichever node it reaches.
//
// EVERY DELIVERY, WHATEVER ITS TYPE. The ledgered set ([inbox.Ledgered]) is
// the types that run a turn, and an answer by turn never runs one, so a filter
// on that set recorded nothing for it at all — which every document claiming
// its delivery is spent at the take was wrong about. What this records is that
// the delivery was spent as an answer, and both routes read it: the ordinary
// route through its ledger check, the answer-by-turn route before it asks the
// coordinator ([Dispatcher.routeAnswers]).
//
// FAILS OPEN, like every completion write: the answer is recorded on the run
// either way, and while the run holds it a copy is recognised there.
func (d *Dispatcher) SpendAnswer(ctx context.Context, handle string, evs []*events.Event) {
	if d.Completions == nil {
		return
	}
	now := d.now()
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		key := workkey.Derive([]string{ev.ID.String()})
		if err := d.Completions.Record(ctx, handle, key, "", now); err != nil {
			log.WarnContext(ctx, "answer_not_recorded_worked", "seat", handle, "error", err,
				"detail", "a copy of this answer reaching the seat after its run has "+
					"settled may be run as an ordinary message")
			return
		}
	}
}

// park requeues a delivery and acks it, or NAKs where it could not be
// requeued. Only behind a pause hold ([inbox.ActionPauseAndPark]): the copies
// land on the inbox the delivery came from, and with nothing stopping the
// consumer they would be fetched straight back.
//
// It takes the EVENTS rather than the screening that asked for them, so the
// list it requeues is stated where it is decided.
func (d *Dispatcher) park(ctx context.Context, handle string, evs []*events.Event, held *holding) queue.Result {
	// NARROWED BEFORE THE REQUEUE, not after it. [Engine.park] publishes one
	// event at a time, so a panic inside it leaves some copies on the queue
	// and the rest unpublished — and recording the delivery would then mark
	// the published ones worked, which is the one thing that stops them ever
	// running. The unpublished ones are lost to the ack either way; the
	// published ones are only lost if this frame claims them.
	held.events = nil
	if d.Park == nil {
		// No park path wired. Acking would drop the work; NAK returns it
		// to the broker, which is the only honest answer.
		return queue.Nak(fmt.Errorf("engine: %s: no requeue path for a park", handle))
	}
	if err := d.Park(ctx, handle, evs); err != nil {
		return queue.Nak(fmt.Errorf("engine: park %s: %w", handle, err))
	}
	return queue.Ack()
}

// dropWorked removes the events the completion ledger has already recorded.
//
// A partial overlap is the case that matters: a redelivery of (A, B) after
// (A, B, C) was worked drops A and B and runs C, rather than re-running all
// three or skipping all three.
//
// TWO KEYS PER CHAT MESSAGE, either of which drops it: the delivery's own —
// a turn that was woken for it — and the message's identity on its chat
// backend, which a LATER turn records when its thread block showed it the
// message still waiting (see workedthrough.go). The second is what stops a
// failed message that comes round behind its conversation's newer mail being
// answered a second time, out of order, after the newer one's turn answered
// the thread it was in.
//
// It reports the dropped events that were covered ONLY by the second key, so
// the record of the skip can say a later turn answered them in their thread
// rather than that a turn was woken for them.
func (d *Dispatcher) dropWorked(ctx context.Context, handle string, evs []*events.Event) ([]*events.Event, map[uuid.UUID]bool) {
	if d.Completions == nil {
		return evs, nil
	}
	keys := make([]string, 0, len(evs))
	for _, ev := range evs {
		if !d.ledgered(ev.Type) {
			continue
		}
		keys = append(keys, workkey.Derive([]string{ev.ID.String()}))
		if chat, ok := chatKeyOf(ev); ok {
			keys = append(keys, chat)
		}
	}
	if len(keys) == 0 {
		return evs, nil
	}
	worked := d.Completions.Worked(ctx, handle, keys)
	if len(worked) == 0 {
		return evs, nil
	}
	out := make([]*events.Event, 0, len(evs))
	var inThread map[uuid.UUID]bool
	for _, ev := range evs {
		if !d.ledgered(ev.Type) {
			out = append(out, ev)
			continue
		}
		if worked[workkey.Derive([]string{ev.ID.String()})] {
			continue
		}
		if chat, ok := chatKeyOf(ev); ok && worked[chat] {
			if inThread == nil {
				inThread = map[uuid.UUID]bool{}
			}
			inThread[ev.ID] = true
			continue
		}
		out = append(out, ev)
	}
	return out, inThread
}

// recordWorked writes both ledgers after a turn.
//
// Per CONSTITUENT event, not per partition. The partition's own key covers the
// set that ran together; a later redelivery of a SUBSET keys differently and
// would match nothing, so the subset would run again. Recording each
// constituent under its own key is what makes a partial overlap droppable.
//
// Both writes fail open. A turn that ran and could not be recorded may run
// again; a turn refused because its bookkeeping failed never runs at all.
func (d *Dispatcher) recordWorked(ctx context.Context, handle string, req Request, res turn.Result) {
	now := d.now()
	if d.Completions != nil {
		keys := make([]string, 0, len(req.Events)+len(res.WorkedThrough))
		for _, ev := range req.Events {
			if d.ledgered(ev.Type) {
				keys = append(keys, workkey.Derive([]string{ev.ID.String()}))
			}
		}
		// AND THE MESSAGES IT ANSWERED WITHOUT BEING WOKEN FOR THEM — the
		// waiting messages its thread block showed it. See
		// workedthrough.go.
		keys = append(keys, res.WorkedThrough...)
		for _, key := range keys {
			if err := d.Completions.Record(ctx, handle, key, "", now); err != nil {
				log.WarnContext(ctx, "completion_not_recorded", "seat", handle, "error", err)
				break
			}
		}
	}
	d.RecordSession(ctx, handle, req.ConversationKey, req.RunID, req.WorkKey,
		DescribeTrigger(req.Ask()), res, now)
}

// RecordSession appends what this turn said to the conversation it served.
//
// EXPORTED AND SEPARATE because a turn has two ways of ending and both owe
// the conversation an entry. An ordinary turn ends here, in the dispatcher.
// A turn that suspended on a detached coding run ends somewhere else
// entirely — in another process, on another node, days later — and that path
// recorded nothing at all: the thread's history stopped at the moment the run
// detached, so the seat's next turn on it re-read a conversation in which the
// coding work had never happened and planned it again.
//
// Fails open, like the completion write beside it: a turn whose bookkeeping
// failed has already delivered, and refusing to admit it happened is the
// worse of the two errors.
// TWO IDENTITIES, BECAUSE THE ENTRY AND THE DEDUPE ARE DIFFERENT QUESTIONS.
// runID is what the entry RENDERS — the seat reads "(turn a1b2c3d4)" back on
// its next turn of this thread, and that has to name the execution somebody
// can open. workKey is what the row is DEDUPED on, and it has to survive a
// re-run or the same trigger files two entries into one conversation. One
// value served both until a turn id stopped meaning the unit of work; see
// ADR-0017.
func (d *Dispatcher) RecordSession(ctx context.Context, handle, conversation,
	runID, workKey, trigger string, res turn.Result, now time.Time,
) {
	if d == nil {
		return
	}
	policy := d.conversationPolicy()
	if d.Conversations == nil || conversation == "" || !policy.Records() {
		return
	}
	// A SUSPENDED TURN HAS SAID NOTHING YET. It parked on a detached coding
	// run, and its Result carries the self_iterate the suspend returns and
	// an empty artifact — so filing it wrote the seat's "reply" to the
	// thread as a decision it never made and an answer it never gave, and
	// the next turn on that thread read it back as what this one had done.
	// The completion ledger's write at the same moment IS right: the
	// trigger has been worked, it is simply not finished. The finish comes
	// back through the resume, which records then.
	if res.Suspended {
		return
	}
	// EVERY FIELD THE ENTRY RENDERS, not just the reply. Only Reply and
	// Decision were ever filled, so a seat re-reading its own history on the
	// next turn of a thread saw a list of answers with no account of what
	// produced them — no trigger, no intent, no calls — and the renderer's
	// other three sections never appeared at all.
	in := ledger.SessionInput{
		TurnID:  runID,
		At:      now.Format(time.RFC3339),
		Trigger: trigger,
		Reply:   res.Artifact,
		// WHICH FIELD THAT ARTIFACT LANDS IN. It is the reviewer's
		// `final_artifact`, which is prose about the turn rather than
		// anything a tool sent, and this was filed unconditionally as the
		// seat's own "You replied" — so a turn that did real work and told
		// nobody wrote into its own history that it had. That record is
		// what the next turn on this thread reads, which is what made the
		// failure self-sealing: the founder's follow-up would be answered
		// against a reply that was never sent.
		Delivered: res.Delivered,
		Decision:  res.Decision.String(),
		Skip:      MetaToolNames(),
	}
	if w := res.LastWork; w != nil {
		// The LAST round's, which is the one the reply came out of. The
		// earlier rounds are the turn's own business and end with it.
		in.Intent, in.Calls = w.Summary, w.Calls
		if w.Outcome == turn.OutcomeBlocked {
			// See [ledger.Session.BlockedOn]: the ledger cannot name this
			// outcome itself, because turn imports ledger. The evidence is
			// mandatory on a blocked submission, so this is never the
			// empty string standing in for a real account.
			in.BlockedOn = w.Evidence
		}
	}
	entry := ledger.BuildSession(in)
	if res.LastReview != nil {
		entry.CompletedWork = res.LastReview.CompletedWork
	}
	// TRIMMED TO THE CONFIGURED KEEP. Passing 0 here meant "keep
	// everything", so max_entries — documented as what bounds a DM whose
	// conversation key is the whole channel and therefore never stops
	// receiving entries — bounded nothing, and the table grew for the life
	// of the deployment.
	if err := d.Conversations.Append(ctx, handle, conversation, entry,
		workKey, now, policy.MaxEntries); err != nil {
		log.WarnContext(ctx, "conversation_not_recorded", "seat", handle,
			"conversation", conversation, "error", err)
	}
}

func (d *Dispatcher) history(ctx context.Context, handle, conversation string) ([]ledger.Session, error) {
	policy := d.conversationPolicy()
	if d.Conversations == nil || conversation == "" || !policy.Records() {
		return nil, nil
	}
	// UNLIMITED on the READ. What is recorded is read back whole; the block
	// a turn is given is bounded at render time by dropping whole entries
	// (ledger.InjectedMaxChars), which is where a bound belongs — the two
	// config knobs that used to claim this job were never threaded to any
	// caller and cut nothing.
	return d.Conversations.History(ctx, handle, conversation, 0)
}

func (d *Dispatcher) conditions(handle string) inbox.Conditions {
	if d.Conditions == nil {
		// Nothing wired means nothing to refuse. A dispatcher with no
		// ownership question is the embedded single-node case, where this
		// process is the only one that could own the seat.
		return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
	}
	return d.Conditions(handle)
}

func (d *Dispatcher) ledgered(eventType string) bool {
	if d.Ledgered == nil {
		return false
	}
	return d.Ledgered(eventType)
}

// now is the dispatcher's clock, injectable for tests.
//
// Nil-tolerant on the receiver, like [Dispatcher.RecordSession] beside it: a
// caller reaching a dispatcher that was never built — a partially wired
// engine, a test driving one method — asks the wall clock rather than
// panicking on the way to a write the same nil check is about to decline.
func (d *Dispatcher) now() time.Time {
	if d == nil || d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now()
}

// conversationIdentityOf takes the durable conversation identity from the
// events, and partitionKeyOf takes the inbox partition key.
//
// TWO FUNCTIONS BECAUSE THERE ARE TWO QUESTIONS, and the dispatcher asks both
// on every turn: the ledger, the telemetry, the episodes and a parked run's
// answer match want the identity, while the coalescing record wants the
// partition — it records that N events MERGED, which is what the partition
// decides. The answer match takes both: the identity admits a run and the
// partition picks between runs one direct message admits. One value
// answering every question is what filed a seat's own prior turn on a direct
// message under a key its next turn never looked up, and what lost every
// clarification a seat was told to ask for in a thread.
//
// The FIRST event that names one wins, for both. A partition is one
// conversation by construction — the broker's key function guarantees the
// partition key, and a source's partition key refines its identity (see
// [notify.Prompt.ConversationIdentity]) — so a later event naming a different
// one is a routing bug, and taking the first keeps the answer stable rather
// than depending on which event happened to sort last.
//
// [notify.ConversationIdentityOfAll] and [notify.KeyOfAll], not copies of
// them: the field names lived here as literals as well, so the grammar that
// calls itself the one definition had three.
func conversationIdentityOf(evs []*events.Event) string {
	return notify.ConversationIdentityOfAll(evs)
}

func partitionKeyOf(evs []*events.Event) string { return notify.KeyOfAll(evs) }

// DescribeTrigger renders a partition as the ask a turn is given.
//
// The FIRST event's own description leads, because a coalesced partition is
// one conversation and its opening message is what the rest are replies to. A
// digest that led with the newest would hand the seat a follow-up with no idea
// what it follows.
//
// THE BRIEF, NEVER THE SUMMARY. This function is the only thing standing
// between a wake and the string a model is asked to act on, and the two
// interfaces answer different questions: [events.Briefer] is the ask,
// [events.Summarizer] is one line for a dashboard row. Reading the summary
// here handed every turn in the company a stub: "Message from alice: deploy"
// for a notification whose body was the actual request, "(a2a_request)" for a
// colleague's question, "(task_assigned)" for a schedule's task text. The type
// name remains only as the last resort it was always meant to be.
//
// THE BRIEF, THEN THE SUMMARY, THEN THE TYPE NAME. Every wake type that runs a
// turn states its ask through Briefer; the summary and the type name are for a
// wake that does not — a successor's type this build decodes with no typed
// payload still names itself rather than reaching a seat as a blank ask.
func DescribeTrigger(evs []*events.Event) string {
	var parts []string
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if brief, ok := ev.Data.(events.Briefer); ok {
			if b := strings.TrimSpace(brief.Brief()); b != "" {
				parts = append(parts, b)
				continue
			}
		}
		if summary, ok := ev.Data.(events.Summarizer); ok {
			if s := summary.Summary(); s != "" {
				parts = append(parts, s)
				continue
			}
		}
		// A trigger with no readable body is still a trigger: naming its
		// TYPE is what stops the turn being handed a blank ask, which a
		// model answers by inventing one.
		parts = append(parts, "("+ev.Type+")")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// wallClockOf is the cap this partition's triggers carried.
//
// THE SMALLEST non-zero, for the same reason delegationOf takes the deepest: a
// coalesced partition can hold triggers that arrived by different routes, and
// a cap exists to bound the turn — so a batch containing one capped fire is
// capped, and taking the first event's value would let an uncapped trigger
// arriving alongside it remove the bound.
func wallClockOf(evs []*events.Event) int {
	smallest := 0
	for _, ev := range evs {
		fire, ok := events.DataAs[*types.TaskAssigned](ev)
		if !ok || fire.TimeoutSeconds <= 0 {
			continue
		}
		if smallest == 0 || fire.TimeoutSeconds < smallest {
			smallest = fire.TimeoutSeconds
		}
	}
	return smallest
}

// delegationOf is the delegation this partition inherits.
//
// THE DEEPEST of the batch, not the first. A coalesced partition can hold
// triggers that arrived by different routes, and the cap exists to bound a
// chain — so a batch containing one deep ask is as deep as that ask, and
// taking the first event's depth would let a shallow trigger arriving
// alongside it reset the count.
//
// The chain comes from the same event as the depth, so the provenance and the
// number it justifies cannot disagree.
func delegationOf(evs []*events.Event) (int, []string) {
	depth, chain := 0, []string(nil)
	for _, ev := range evs {
		if ev != nil && ev.DelegationDepth > depth {
			depth, chain = ev.DelegationDepth, ev.DelegationChain
		}
	}
	return depth, chain
}

// noteCoalesced records a partition merged into one digest trigger.
//
// # Here, because here is where the constituent list exists
//
// A coalesced digest is minted fresh on every merge and carries no memory of
// what it absorbed; by the time a turn is running there is nothing left to
// count. So the record is written at the one frame that still holds the
// events — the same reason the work key is derived here.
//
// Nothing else emits this event, which is why Settings › Integrations could
// report how many deliveries ARRIVED and not how many turns they became: a
// seat draining a thread's backlog as one turn looked, from the feed, like a
// seat that ignored twelve messages.
func (d *Dispatcher) noteCoalesced(ctx context.Context, handle, partition string,
	routing inbox.Routing,
) {
	if !routing.Coalesce || d.Observe == nil || len(routing.Events) == 0 {
		return
	}
	first, last := routing.Events[0].Timestamp, routing.Events[0].Timestamp
	for _, ev := range routing.Events {
		if ev.Timestamp.Before(first) {
			first = ev.Timestamp
		}
		if ev.Timestamp.After(last) {
			last = ev.Timestamp
		}
	}
	ev := events.New(types.NotificationsCoalesced{
		// THE PARTITION KEY, under a field that says so: what merged is a
		// partition, and this event exists to say "N deliveries became one
		// turn". It rode the `conversation_key` field once, which made the
		// promoted tag of that name mean the identity on every other event
		// and the batch on this one.
		AgentHandle: handle, PartitionKey: partition,
		// THE VENDOR NAMES THE INTEGRATION. A merge is always one
		// conversation's worth of external notifications and a conversation
		// belongs to one third-party app, so the constituents cannot disagree and
		// taking the first is a lookup rather than a choice.
		NotificationSource: notificationSourceOf(routing.Events),
		Count:              len(routing.Events),
		FirstAt:            first.UTC().Format(time.RFC3339),
		LastAt:             last.UTC().Format(time.RFC3339),
	}, tracing.TraceOf(ctx))
	ev.Source = "engine." + routing.Events[0].Source
	d.Observe(ctx, ev)
}

// noteSkipped records every constituent the completion ledger already worked.
//
// Best effort and nil-safe, like the coalescing record beside it: a node whose
// queue refused the row has still skipped correctly, and failing the dispatch
// over an observability event would trade real work for a feed entry.
func (d *Dispatcher) noteSkipped(ctx context.Context, handle string, all, surviving []*events.Event,
	answeredInThread map[uuid.UUID]bool,
) {
	if d.Observe == nil || len(all) == len(surviving) {
		return
	}
	kept := make(map[uuid.UUID]bool, len(surviving))
	for _, ev := range surviving {
		if ev != nil {
			kept[ev.ID] = true
		}
	}
	for _, ev := range all {
		if ev == nil || kept[ev.ID] {
			continue
		}
		// WHICH TURN WORKED IT is the half of the reason a reader needs:
		// a turn woken for this very trigger, or a later turn that was
		// shown it waiting in its thread and answered the thread as it
		// stood (see workedthrough.go).
		reason := "a previous turn already worked this trigger"
		if answeredInThread[ev.ID] {
			reason = "a later turn was shown this message waiting in its thread and answered it there"
		}
		// THE SKIPPED TRIGGER'S OWN TRACE, not the dispatch's: the
		// question this record answers is "what happened to my webhook",
		// and the answer belongs under the webhook rather than under a
		// dispatch that went on to do something else.
		rec := events.New(types.TurnTriggerSkipped{
			AgentHandle: handle,
			TriggerID:   ev.ID.String(),
			TriggerType: ev.Type,
			Reason:      reason,
		}, triggerTrace([]*events.Event{ev}))
		rec.Source = "engine.dispatch"
		d.Observe(ctx, rec)
	}
}

// notificationSourceOf is the third-party app a partition came from.
//
// OFF THE TYPED PAYLOAD, not the envelope's Source. internal/notify stamps the
// envelope "notify.slack" — it names the PRODUCER of the wake, which is the
// notification service — so reading it here filed every coalescing record
// under a source string no other notification event uses and no dashboard
// filter matches, leaving every integration's coalesced count permanently
// zero. The payload's own NotificationSource is the bare third-party app name every
// other consumer reads.
func notificationSourceOf(evs []*events.Event) string {
	for _, ev := range evs {
		if n, ok := events.DataAs[*types.ExternalNotification](ev); ok && n.NotificationSource != "" {
			return n.NotificationSource
		}
	}
	return ""
}

// triggerTrace is the trace a partition of trigger events belongs to.
//
// The FIRST non-nil event, matching describeTurn's choice of trigger, so the
// trace and the trigger a turn reports are always the same event's. Taking
// them from different events is how a turn ends up filed under a trace whose
// root says something it did not react to.
//
// An empty result is ordinary rather than exceptional: an event its publisher
// gave no trace context (an A2A wake, an operator's answer to a parked run)
// carries no ids, and WithRemote turns that into a fresh root.
func triggerTrace(evs []*events.Event) events.TraceContext {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		return events.TraceContext{
			TraceID: ev.TraceID, SpanID: ev.SpanID, ParentSpanID: ev.ParentSpanID,
		}
	}
	return events.TraceContext{}
}

// ReplyFor says who is waiting for the turn a partition wakes, and how they
// get an answer.
//
// DERIVED FROM THE TRIGGER, before the turn starts and from nothing the model
// says. It is the half of the delivery question a model cannot get wrong: the
// old engine asked the model to declare its own intent, and a turn that
// declared `skip` on a direct @mention read to the person who sent it exactly
// like the message never arriving.
//
// STRONGEST WINS across a coalesced partition, whatever order the events
// arrived in. A partition is one conversation, and if any part of it asked
// this seat something, the turn owes an answer — a merge must not be able to
// launder an obligation. The strength order is [turn.ReplyTool] over
// [turn.ReplyEngine] over [turn.ReplyNone]: a tool obligation is the one the
// engine ENFORCES ([turn.Check] and [turn.OverrideDone] send a round back
// until a tool delivered), while an A2A ask is answered by the engine from the
// turn's artifact whichever value this returns. So where both were owed, tool
// loses nothing for the asker and keeps the check; engine would let the turn
// end in text the tool-side requester never sees.
//
// Today the inbox never builds such a partition — every type but a
// notification burst keys uniquely and arrives alone (see [inbox.Route]) — so
// the ranking is a contract held for the key scheme that would, rather than a
// path that runs. It is held anyway, because the alternative is a function
// whose answer depends on which event a broker happened to deliver first.
//
// The default is [turn.ReplyNone], and it is the safe half. A seat wrongly
// told nobody is waiting keeps the freedom to end a turn having done nothing,
// which is what makes triage cheap; a seat wrongly told somebody is must post
// on every broadcast it observes.
func ReplyFor(evs []*events.Event) turn.Reply {
	out := turn.NoReply()
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		var owed turn.Reply
		switch ev.Type {
		case types.A2ARequestType:
			// A colleague asked, and the ENGINE answers: engine/a2a.go
			// returns the turn's artifact on the channel the ask opened.
			// Nothing here calls a tool to deliver, so demanding one
			// for the ask alone would loop every colleague exchange to
			// exhaustion.
			owed = turn.EngineReply()

		case types.TaskAssigned{}.EventType():
			// Work was assigned to this seat. The answer lives wherever
			// the tracker is, and only a tool puts it there.
			//
			// NO SURFACE, deliberately. Unlike a notification, an
			// assignment does not say where its answer belongs: moving
			// the item, commenting on the ticket and replying in the
			// thread the work came from are all honest answers, and the
			// engine has no basis to pick one. Naming a surface here
			// would refuse the other two. See [turn.Reply.Surface].
			owed = turn.ToolReply("")

		case types.ExternalNotification{}.EventType():
			// The third-party app's own reading of its routing. See
			// [notify.Prompt.Addressed]. Absent decodes as false, which is
			// how an unaddressed notification is written, so an omission
			// is a freedom to stay silent rather than an obligation nobody
			// recorded.
			//
			// OFF THE TYPED PAYLOAD, never the envelope's free-form bag.
			// Addressed is a field of [types.ExternalNotification], and
			// nothing has ever written it into Payload — so the bag read
			// this replaces answered false for EVERY notification, in
			// process and across the wire alike. The delivery obligation
			// the reviewer enforces was therefore never raised by an
			// inbound message: a seat could end a turn woken by a direct
			// ask having posted nothing, and the guard that exists to
			// catch exactly that saw ReplyNone.
			//
			// AND WHERE THEY ARE WAITING. The source is the vendor's own
			// name — `mattermost`, `slack`, `jira` — which is the same
			// vocabulary a delivering tool reports, because an MCP tool's
			// surface is its server and a company names that server after
			// the vendor it serves. Carried because the obligation is
			// useless without it: "somebody is waiting on a tool" is
			// satisfied by any tool at all, so a founder's Mattermost DM
			// was closed out by a row in the tracker.
			//
			// UNLESS THE WAKE OWES ANOTHER SURFACE. The answer to a
			// decision whose asker said it would report the outcome in a
			// channel arrives from the TRACKER, and on the source alone a
			// comment on the item would close the turn: the person who
			// answered was told the outcome would be posted, and it never
			// was. [types.ExternalNotification.Owes] names the chat
			// surface instead, and an owed wake is awaited whether or not
			// its source reads it as addressed — the promise is the
			// obligation.
			if n, ok := events.DataAs[*types.ExternalNotification](ev); ok &&
				(n.Addressed || n.Owes != "") {
				owed = turn.ToolReply(cmp.Or(n.Owes, n.NotificationSource))
			}

			// types.A2AMessageType is deliberately absent: that hop wakes
			// the REQUESTER with an answer it asked for, on a channel that
			// is already closed. Nobody is waiting on what this turn does
			// with it.
		}
		if replyRank[owed.Kind] > replyRank[out.Kind] {
			out = owed
		}
	}
	return out
}

// replyRank orders the obligations for [ReplyFor]: the one the engine
// enforces outranks the one it answers itself, and both outrank none. A
// value not in the map ranks zero, below every real obligation.
var replyRank = map[turn.ReplyKind]int{
	turn.ReplyNone:   1,
	turn.ReplyEngine: 2,
	turn.ReplyTool:   3,
}
