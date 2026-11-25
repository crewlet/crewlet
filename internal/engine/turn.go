package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessreview"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	llmchain "github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
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

	// Completions is the durable completion ledger. It may be nil, which is
	// the embedded single-node case: with no peer to race, the seat lease is
	// the whole mutual exclusion and there is nothing for a completion
	// ledger to add.
	Completions ledgerstore.Completions

	// SeenThrough is how far each seat's completed turns read each
	// conversation (ADR-0120); nil absorbs nothing.
	SeenThrough *ledgerstore.SeenThrough

	// Turn runs the seat's turn once the guards have passed.
	Turn func(ctx context.Context, req Request) (turn.Result, error)

	// Settled is told the wakes a delivery has finished with: every
	// constituent of a partition the dispatch acknowledged, whether a turn
	// ran on it or it was settled without one (worked already, answered a
	// parked run, parked). It is what takes a person's queued line down
	// once their message no longer waits ([Engine.settleWaiting]). nil
	// settles nothing.
	Settled func(ctx context.Context, handle string, wakes []string)

	// Idle is told a seat whose last dispatch running on this node has
	// returned, its delivery settled: the router of its held inbox reads
	// nothing while one runs (wakerouter.go). Nil tells nobody.
	Idle func(handle string)

	// DeadLettered is told of a delivery whose hand-back retires a message:
	// the one place that holds both the message's headroom and why it failed.
	DeadLettered func(ctx context.Context, handle string, result queue.Result)

	// Gone is told the events of a delivery that will not come back to the
	// seat: every one [Dispatcher.Settled] is told of, less those the dispatch
	// handed back to the queue as copies of their own (a park's whole
	// delivery, an unmergeable partition's tail), each copy being the same
	// event, by its id, still waiting on the inbox ([goneBy]). It drops what
	// the seat's park recorded for a delivery whose launch was refused below
	// its floor ([Engine.budgetDeliveryGone]), which that delivery's every try
	// reads until it is gone and nothing reads after. nil drops nothing.
	Gone func(ctx context.Context, handle string, gone []*events.Event)

	// running counts the dispatches in progress per seat, which is how
	// "this seat is busy right now" is answered for a person's message
	// that reaches it meanwhile ([Dispatcher.Running]).
	runningMu sync.Mutex
	running   map[string]int

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

	// Beside decides, below the seat's cap, whether a delivery waits on one of
	// the seat's running executor runs (ADR-0169): the key it waits aside
	// under, empty where it starts a turn of its own beside them; an error is a
	// store that could not say, which defers it rather than run beside a run
	// that may be doing its work. SetAside is told one set aside, and
	// HoldAside asks the coordinator to hold the inbox for one that would pass
	// what the inbox keeps aside ([queue.AsideRoom]). Nil starts every one.
	Beside    func(ctx context.Context, handle string, evs []*events.Event) (besideAnswer, error)
	SetAside  func(ctx context.Context, handle string, at besideAnswer, evs []*events.Event)
	HoldAside func(ctx context.Context, handle string)

	// Budget parks the seat when one of its capped token windows is
	// refusing, or leaves less room than the delivery's launch needs, and
	// reports the deferral reason naming the window — see budgetpark.go.
	// refused is the refusal the delivery's turn met, nil before any turn
	// ran; delivery is the events that turn was for, nil with it. A launch refused below its own floor carries that floor ([floorOf]);
	// the park waits for room for it, not the static floor a window may have
	// without room for a larger one, and records the window it waits on for
	// that delivery alone ([floorWaits]). An error is a park that could not
	// be taken: it DEFERS the delivery rather than running a turn the counter
	// refuses, keeping the message's place.
	//
	// Nil parks nothing: every turn runs, and its own meter is the gate.
	Budget func(ctx context.Context, handle string, delivery []*events.Event,
		refused error) (reason string, parked bool, err error)

	// Provider parks the seat when every member of the chain a turn ran on
	// refused for a rate limit, until the first is tried again, and reports
	// the deferral reason naming the limit; see providerpark.go. An error is
	// a park that could not be taken, which DEFERS the delivery for the
	// reason [Dispatcher.Budget]'s does.
	//
	// Nil parks nothing: a limited turn is then NAKed like any other
	// failure, and its redelivery meets the backend's own sit-out.
	Provider func(ctx context.Context, handle string, limit llmchain.Limit) (reason string, parked bool, err error)

	// FleetEmpty holds the seat whose turn met an LLM fleet with no member,
	// until one joins, and reports the deferral reason; an error defers as
	// Provider's does. Nil holds nothing.
	FleetEmpty func(ctx context.Context, handle, fleet string) (reason string, parked bool, err error)

	// Failure pauses the seat on the second failure in a row a retry cannot
	// cure (failurehold.go). An error is a pause that could not be taken,
	// deferred as Budget's is.
	Failure func(ctx context.Context, handle string, failed error) (reason string, parked bool, err error)
	// FailureCleared ends the seat's run of failures: its turn worked.
	FailureCleared func(handle string)

	// Held reports whether the delivery's conversation is held because its
	// context did not fit (conversationhold.go): its wakes are settled unrun.
	Held func(ctx context.Context, handle, conversation string, evs []*events.Event) bool

	// Answer offers a delivery to a parked executor as the reply to the
	// question it asked and reports what to DO with it ([sandbox.AnswerDisposition]).
	//
	// THE ONE WAY A PERSON'S REPLY REACHES THE RUN THAT ASKED. Without it
	// the reply is consumed as an unrelated turn, the run sits in
	// [sandbox.StatusAwaiting] until its pause TTL reclaims it, and the
	// person is never told. A disposition, not a bool: a bool hid four
	// outcomes, so a claim given back, or one the store could not confirm,
	// sent the reply on to the ordinary route. Nil is a node with no coordinator. It takes the WHOLE [sandbox.Reply]
	// (see [sandbox.ConversationRef.Best]): its events' instants decide
	// whether it followed the question ([sandbox.PendingRun.AskedAt]).
	Answer func(ctx context.Context, handle string, reply sandbox.Reply) (sandbox.AnswerDisposition, error)

	// NoteDeferred tells the seat host a consumer stopped, so the next
	// successful renew resumes it.
	NoteDeferred func(handle string)

	// ReviewSettled reports an access review with nothing left to tell
	// ([Engine.AccessReviewSettled]), acked without a turn. Nil settles nothing.
	ReviewSettled func(ctx context.Context, handle string) bool

	// DelegatedRequests reads each delegated review among a delivery where its
	// request waits, and drops one whose request no longer waits on the seat
	// ([Engine.readDelegatedRequests]). Nil reads nothing.
	DelegatedRequests func(ctx context.Context, handle string, evs []*events.Event) []*events.Event

	// StaleWakes drops each wake about work the seat holds whose item was
	// finished, removed or handed on since ([Engine.dropStaleWakes]). Nil
	// drops none.
	StaleWakes func(ctx context.Context, handle string, evs []*events.Event) []*events.Event

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

	// Parties resolves who the company's parties are, which a reply offered
	// to a parked run is labelled by as its trigger was ([replyOf]): the
	// writer's seat, at the trust it earns from the seat.
	//
	// nil is no registry, which names no writer, so a reply is read as words
	// nothing vouches for.
	Parties func() notify.Parties

	// Identify names the seat a handle is, as the role name and agent id
	// every seat-addressed event carries.
	//
	// FOR ONE RECORD ONLY: the guard breach a panic that escaped a turn's
	// own frames publishes. The live projection keys a seat by its AGENT ID,
	// so a breach addressed by handle alone would reach no seat's failure;
	// the turn's own telemetry, which knows both, is what did not run.
	//
	// Nil publishes no breach: the panic is still logged, the trigger still
	// recorded and the delivery still settled.
	Identify func(handle string) (role, agentID string)

	// Now is injectable so a test can pin the clock.
	Now func() time.Time
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

	// ConversationKey is the surface-scoped conversation IDENTITY (the
	// durable thread this turn is part of), empty when the trigger has none.
	// A schedule's fire has one ([notify.ScheduleConversation]).
	//
	// The identity, never the inbox partition key: this is what reaches the
	// turn telemetry (the conversation_key tag, the episodes column), the
	// executor log rows and a detached executor's report-back row. THE
	// PARTITION KEY IS NOT A FIELD HERE: read it off the events
	// ([partitionKeyOf]). The answer match takes both: the identity admits,
	// because a person answers on the conversation, and the partition only
	// chooses between what it admitted (a thread reply is a finer partition
	// than the question parked under).
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
// THE ORDER IS THE PRODUCT: internal/agent/inbox owns the guard sequence and
// the reasons, and this frame adds the two ledger reads and the one ledger
// write, placed deliberately (the package doc's "Dispatch's order"). Every NAK
// here asks [nakFailure], since a failure of this node's is never the mail's
// own (the package doc's section of that name). NO PANIC LEAVES THIS FRAME: a
// phase's panic is recovered inside the turn loop as a broken turn, and any
// other between the broker and the loop, in the stages or the turn's set-up
// and tear-down, here, since the queue's handler guard NAKs a panic, running a
// defect again up to the whole delivery budget ([Dispatcher.recoverPanic]).
func (d *Dispatcher) Dispatch(ctx context.Context, handle string, evs []*events.Event) (result queue.Result) {
	d.enter(handle)
	defer d.leave(handle)
	// SETTLED ONCE THE OUTCOME IS FINAL, which is why this is deferred
	// FIRST: deferred calls run last to first, so it runs after the panic
	// recovery below has decided what a recovered dispatch answers. See
	// [settledBy] for which wakes a dispatch has finished with.
	defer func() {
		if d.Settled == nil {
			return
		}
		if wakes := settledBy(ctx, result, evs); len(wakes) > 0 {
			d.Settled(ctx, handle, wakes)
		}
	}()
	defer func() {
		if d.DeadLettered != nil && deadLetters(ctx, result, evs) {
			d.DeadLettered(ctx, handle, result)
		}
	}()
	held := holding{events: evs}
	// GONE ONCE THE OUTCOME IS FINAL, after the panic recovery below as
	// Settled is, and less what the dispatch requeued, which it reads off
	// held: see [goneBy].
	defer func() {
		if d.Gone == nil {
			return
		}
		if gone := goneBy(ctx, result, evs, held.requeued); len(gone) > 0 {
			d.Gone(ctx, handle, gone)
		}
	}()
	defer func() {
		if panicked := turn.Recovered(panics.ScopeTurn, recover()); panicked != nil {
			result = d.recoverPanic(ctx, handle, held, panicked)
		}
	}()
	return d.dispatch(ctx, handle, evs, &held)
}

// Running reports whether a delivery of the seat is being dispatched on this
// node right now: a turn of it is running, or about to.
func (d *Dispatcher) Running(handle string) bool {
	d.runningMu.Lock()
	defer d.runningMu.Unlock()
	return d.running[handle] > 0
}

func (d *Dispatcher) enter(handle string) {
	d.runningMu.Lock()
	defer d.runningMu.Unlock()
	if d.running == nil {
		d.running = map[string]int{}
	}
	d.running[handle]++
}

func (d *Dispatcher) leave(handle string) {
	d.runningMu.Lock()
	d.running[handle]--
	idle := d.running[handle] <= 0
	if idle {
		delete(d.running, handle)
	}
	d.runningMu.Unlock()
	if idle && d.Idle != nil {
		d.Idle(handle)
	}
}

// settledBy is the wakes a dispatch has finished with, whose queued lines come
// down ([Dispatcher.Settled]).
//
// EVERY WAKE OF AN ACKNOWLEDGED DELIVERY: a turn ran on it, or it was settled
// without one. A delivery handed back (a Nak or a Defer) comes back, and its
// person is still waiting, EXCEPT a wake the hand-back dead-letters: the
// backend retires a message whose deliveries are spent rather than return it,
// so nothing would ever pick it up and end its line. Which ones those are is
// each message's own headroom as the queue states it
// ([queue.DeliveriesLeftFor]); an unstated one is not taken as spent. A wake
// set aside spends nothing and waits on: none.
func settledBy(ctx context.Context, result queue.Result, evs []*events.Event) []string {
	switch result.Outcome {
	case queue.OutcomeAck:
		return wakeIDsOf(evs)
	case queue.OutcomeAside:
		return nil
	}
	var out []string
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if left, known := queue.DeliveriesLeftFor(ctx, ev.ID); known && left <= 0 {
			out = append(out, ev.ID.String())
		}
	}
	return out
}

// goneBy is the events of a delivery that will not come back to its seat
// ([Dispatcher.Gone]): every one the dispatch has finished with, by
// [settledBy]'s rule, an acknowledged delivery's every event and a hand-back's
// dead-lettered ones, less the ones it requeued as copies of their own. A
// requeue publishes each event again as the event it is, so the copy carries
// its id and waits on the inbox for the seat, its delivery's next try still to
// come, though the original is acknowledged.
func goneBy(ctx context.Context, result queue.Result, evs, requeued []*events.Event) []*events.Event {
	finished := settledBy(ctx, result, evs)
	var out []*events.Event
	for _, ev := range evs {
		if ev == nil || !slices.Contains(finished, ev.ID.String()) ||
			slices.ContainsFunc(requeued, func(copied *events.Event) bool {
				return copied != nil && copied.ID == ev.ID
			}) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// deadLetters reports whether handing this delivery back retires one of its
// messages: the backend dead-letters a message whose deliveries are spent. An
// unstated headroom is not taken as spent, as [settledBy]'s is not.
func deadLetters(ctx context.Context, result queue.Result, evs []*events.Event) bool {
	if result.Outcome == queue.OutcomeAck || result.Outcome == queue.OutcomeAside {
		return false
	}
	return slices.ContainsFunc(evs, func(ev *events.Event) bool {
		if ev == nil {
			return false
		}
		left, known := queue.DeliveriesLeftFor(ctx, ev.ID)
		return known && left <= 0
	})
}

// wakeIDsOf is the ids of a delivery's wakes, the names a queued line is
// held under ([notify.WaitingNotice.Wake]).
func wakeIDsOf(evs []*events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		if ev != nil {
			out = append(out, ev.ID.String())
		}
	}
	return out
}

// holding is what one delivery is still answerable for, narrowed as the stages
// of [Dispatcher.dispatch] settle events or hand them back to the queue.
//
// A RECOVERED PANIC SETTLES THIS, NOT THE DELIVERY: recording a constituent
// as worked stops its copies ever running. The whole delivery would be wrong
// three ways: an unmergeable partition's tail is requeued before its head
// runs (a head panic would mark it worked, the loss [inbox.Degraded] prevents);
// a park hands the WHOLE delivery back; and a constituent the completion
// ledger dropped would be recorded again, as panicked. EVERY NARROWING HAPPENS BEFORE THE REQUEUE IT DESCRIBES: a requeue publishes
// one event at a time, so a panic inside one leaves some copies published,
// which a later narrowing would still claim. AND THE RUN, once minted, so a
// breach names the run whose start is on the record (empty before the mint).
type holding struct {
	events []*events.Event
	runID  string

	// requeued is what the dispatch handed back to the queue as copies of
	// their own, a park's whole delivery or an unmergeable partition's tail,
	// set before the requeue as every narrowing above is: each copy is the
	// event it was, by its id, waiting on the inbox, so the delivery is not
	// gone though its original is acknowledged ([goneBy]). A copy a requeue
	// that failed part-way never published leaves its record to the seat's
	// release, which drops every one.
	requeued []*events.Event
}

// dispatch is [Dispatcher.Dispatch]'s body, separated so the recovery around
// it is one deferred call rather than a frame every return has to pass. It
// narrows held at each point where the delivery stops being answerable for an
// event.
func (d *Dispatcher) dispatch(ctx context.Context, handle string, evs []*events.Event, held *holding) queue.Result {
	if len(evs) == 0 {
		return queue.Ack()
	}
	conditions := d.conditions(handle)
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
				// The pause stops the requeued copies looping back at the
				// broker's rate; without it a park is worse than nothing,
				// so the delivery is handed back rather than parked.
				//
				// DEFERRED, NOT NAKED: a hold the queue would not take is
				// this NODE's condition, not the message's, and a Nak
				// returns a message behind its conversation's newer mail
				// (queue.OutcomeNak), into the very park this one could
				// not set up. A deferral keeps its place at the head and
				// stops the attachment until the seat host's next renew.
				if d.NoteDeferred != nil {
					d.NoteDeferred(handle)
				}
				return queue.Defer(queue.DeferHoldRefused, fmt.Sprintf("engine: pause %s: %s", handle, err))
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
	// ledger reads it, before a parked executor is offered it as an
	// answer — resuming one charges tokens exactly as a turn does — and
	// before any model is asked anything. A seat whose capped window is
	// refusing cannot run a round, and a turn started anyway is refused on
	// its first charge and NAKed, again and again, until the broker
	// dead-letters a healthy message. Parked instead, it waits on its inbox
	// for the window to turn over. See budgetpark.go.
	//
	// AFTER THE SCREENING, whose every non-proceeding outcome already hands
	// the delivery on without running anything, so it costs those no read.
	if result, parked := d.parkOnBudget(ctx, handle, nil, nil); parked {
		return result
	}

	surviving, skipped := d.dropWorked(ctx, handle, screening.Events)
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
	d.noteSkipped(ctx, handle, screening.Events, surviving, skipped)
	if len(surviving) == 0 {
		return queue.Ack()
	}
	// AN ACCESS REVIEW WITH NOTHING LEFT TO TELL, acked unrun: the turn that
	// told the change already asked for the review (internal/accessreview).
	if d.ReviewSettled != nil && accessReviewsOnly(surviving) && d.ReviewSettled(ctx, handle) {
		log.InfoContext(ctx, "access_review_settled", "seat", handle, "events", len(surviving),
			"detail", "the seat was already told its access as it stands; no review turn runs")
		return queue.Ack()
	}
	// A DELEGATED REQUEST IS READ WHERE IT WAITS, sealed on its run's row
	// and never on the stream, and one no longer waiting on this seat
	// starts no turn (ADR-0125).
	if d.DelegatedRequests != nil {
		if surviving = d.DelegatedRequests(ctx, handle, surviving); len(surviving) == 0 {
			return queue.Ack()
		}
		held.events = surviving
	}
	if d.StaleWakes != nil {
		if surviving = d.StaleWakes(ctx, handle, surviving); len(surviving) == 0 {
			return queue.Ack()
		}
		held.events = surviving
	}

	// THE ANSWER IS CLAIMED BEFORE ANYTHING ELSE EATS IT.
	//
	// A run parked on a question does not hold its seat, so the reply
	// reaches here on the ordinary path looking like any message. This is
	// the ONLY offer: a seat a run holds has its inbox held, and an answer
	// to another of its runs waits there to be offered here when it lifts.
	// (Offered only while the seat was HELD, the one state a parked run is
	// never in, every answer was consumed as an unrelated turn.)
	//
	// AFTER THE LEDGER: it has already said which events were worked, and a
	// trigger that produced a turn must not also be spliced into somebody's
	// executor.
	if screening.OfferAsSandboxAnswer {
		disposition, cause := d.answered(ctx, handle, surviving)
		switch disposition {
		case sandbox.AnswerConsumed:
			// Spent on the run it answered: the answer is recorded on
			// the run, the coordinator owns its resume from here, and no
			// turn runs on it here.
			d.SpendAnswer(ctx, handle, surviving)
			return queue.Ack()
		case sandbox.AnswerDeferred:
			// STILL OWED TO A RUN and not recorded against it (its runs
			// could not be read, or the record written): it comes back.
			// A DEFERRAL, NEVER A NAK: both failures are the NODE's or its
			// STORE's, and a Nak puts a message BEHIND its conversation's
			// newer mail (queue.OutcomeNak), so the person's next message
			// reached the waiting run first and was taken as its answer. A
			// deferral returns it at the HEAD (queue.OutcomeDefer) and stops
			// the seat's inbox until the next renew. It spends one of the
			// delivery's deliveries, which is why [Dispatcher.mayOfferAnswer]
			// stops offering while [sandbox.AnswerDeliveryReserve] are left
			// and [sandbox.MaxAnswerAttempts] bounds the series on this node.
			if d.NoteDeferred != nil {
				d.NoteDeferred(handle)
			}
			return queue.Defer(queue.DeferAnswerOwed, answerOwedReason(handle, cause))
		case sandbox.AnswerRefused:
			// A REPLY TOO LARGE TO BE ITS RUN'S ANSWER: its REFUSAL is
			// recorded on the run in the answer's place (ADR-0114). No turn
			// runs on it; the seat answering it as a new message is the
			// defect the refusal ends.
			//
			// KEPT, NOT SPENT: deferred at the head of the inbox, behind the
			// hold the owed refusal keeps, and never recorded as worked
			// here. The run's row carries none of the reply, so this
			// delivery is its one copy: a turn that takes the refusal
			// records it worked; a refusal let go of leaves it to come
			// round as the ordinary message it is. Acked, it would be lost.
			if d.NoteDeferred != nil {
				d.NoteDeferred(handle)
			}
			return queue.Defer(queue.DeferAnswerOwed, refusedReplyReason(handle))
		}
	}

	// BESIDE THE SEAT'S RUNNING RUNS, below its cap (ADR-0169): a delivery
	// of a conversation one of them works, or related to one, is routed to it
	// and waits aside until it leaves; any other starts a turn beside them.
	// After the answer offer, which a parked run is owed first.
	if result, waits := d.waitBeside(ctx, handle, surviving, held); waits {
		return result
	}

	// A HELD CONVERSATION starts no turn: its wakes are settled unrun and
	// counted, and a person writing on it is what lifts the hold.
	if d.Held != nil && d.Held(ctx, handle, conversationIdentityOf(surviving), surviving) {
		return d.settleHeld(ctx, handle, surviving)
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
		merged, ok := mergePartition(d.promptRegistry(), routing.Events)
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
			// is ever this delivery's to settle, and the tail, its copies
			// waiting for the seat, is never gone with it ([goneBy]).
			held.events, held.requeued = head, tail
			// A requeue that failed for a reason of this node's is NAK'd
			// as the node's own, never the mail's ([nakFailure]).
			if err := d.Park(ctx, handle, tail); err != nil {
				return nakFailure(ctx, fmt.Errorf("engine: requeue %s: %w", handle, err))
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
	// so this turn's spans hang under whatever caused it (a webhook, a
	// schedule, another agent) instead of each seat rooting its own trace.
	// Bound FIRST of the three context values: the coalescing record and the
	// history warning are emitted before the others and must name the trace.
	//
	// A COALESCED TURN HAS ONE TRACE AND SEVERAL CAUSES: the one the merged
	// trigger carries (a digest's is the partition's first event; a schedule's
	// waiting fires, the LATEST). A span cannot have two parents, so the
	// others are recorded as interactions or superseded under their own
	// trace ([Dispatcher.noteSuperseded]).
	ctx = tracing.WithRemote(ctx, triggerTrace(trigger))
	depth, chain := delegationOf(routing.Events)
	// MINTED HERE, at the one frame that knows a partition is about to
	// RUN, because what the turn records and what the dispatch records of
	// it both need it: the turn's rows on the executor log name the run a
	// reader can open, and the abandon record names the run that stopped.
	// See ADR-0017.
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
	d.noteSuperseded(ctx, handle, routing.Events, trigger)

	// The acting seat travels the same way, and for the same reason: the
	// only consumer is a leaf. A provider gives every seat its own harness
	// home, because seven seats on one subscription sharing one
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
			// THE SCREENING'S CONDITION, detected a phase later: the seat's
			// grant moved mid-turn. A healthy delivery a successor is
			// entitled to, so deferred, not naked. It buys speed and
			// silence, not a free delivery: it spends one of the trigger's
			// twenty-five like a failure (why the budget is 25, not ~10),
			// but returns in about a millisecond and quiesces the
			// attachment. BELOW [turn.Abandon]: a turn that panicked or
			// wrote outward must not run again where the seat now lives.
			if errors.Is(err, seat.ErrSeatMoved) {
				if d.NoteDeferred != nil {
					d.NoteDeferred(handle)
				}
				log.InfoContext(ctx, "turn_seat_moved", "seat", handle,
					"run_id", req.RunID, "work_key", req.WorkKey, "error", err.Error())
				return queue.Defer(queue.DeferSeatMoved, "the seat moved to another node mid-turn")
			}
			// AND THE BUDGET STAGE'S CONDITION, found a phase later: parked
			// until the window turns over, not NAKed into a redelivery
			// refused the same way. Below [turn.Abandon] as above. Parked on
			// the floor the launch was refused below ([launchFloor]), or a
			// larger floor would find the window not starved and NAK into
			// the same refusal; and for the events dispatched, so the park
			// holds their next launch, not whatever the release hands the
			// seat first ([floorWaits]).
			if errors.Is(err, toolloop.ErrBudgetExhausted) {
				if result, parked := d.parkOnBudget(ctx, handle, req.Events, err); parked {
					log.InfoContext(ctx, "turn_budget_parked", "seat", handle,
						"run_id", req.RunID, "work_key", req.WorkKey, "error", err.Error())
					return result
				}
			}
			// AND A CHAIN EVERY MEMBER OF WHICH IS LIMITED: the seat is
			// parked until the first is tried again, rather than NAKed
			// into the same refusal a second later, which, with every
			// partition of every seat doing it, was a turn started and
			// failed per seat per second for as long as the limit lasted.
			// See providerpark.go. Below [turn.Abandon] for the reason the
			// budget's branch is.
			if limit, limited := llmchain.Limited(err); limited && d.Provider != nil {
				result, parked := d.parkOn(ctx, handle, "provider park", func(ctx context.Context) (string, bool, error) {
					return d.Provider(ctx, handle, limit)
				})
				if parked {
					log.InfoContext(ctx, "turn_provider_parked", "seat", handle,
						"run_id", req.RunID, "work_key", req.WorkKey, "error", err.Error())
					return result
				}
			}
			// AND A FLEET WITH NO MEMBER: held until one joins, which no
			// redelivery can bring about.
			var empty *phase.EmptyFleetError
			if errors.As(err, &empty) && d.FleetEmpty != nil {
				result, held := d.parkOn(ctx, handle, "empty fleet hold", func(ctx context.Context) (string, bool, error) {
					return d.FleetEmpty(ctx, handle, empty.Fleet)
				})
				if held {
					log.InfoContext(ctx, "turn_fleet_empty_held", "seat", handle,
						"run_id", req.RunID, "work_key", req.WorkKey, "fleet", empty.Fleet)
					return result
				}
			}
			// AND A FAILURE A RETRY CANNOT CURE, the second time in a row:
			// its seat is paused until its settings change or a person
			// resumes it, rather than spending its deliveries on the refusal.
			if d.Failure != nil {
				result, held := d.parkOn(ctx, handle, "failure pause", func(ctx context.Context) (string, bool, error) {
					return d.Failure(ctx, handle, err)
				})
				if held {
					log.InfoContext(ctx, "turn_failure_paused", "seat", handle,
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
			// failing turn needs, which a deferral is not; the order's
			// cost is closed in workedthrough.go. THE NODE'S OWN where
			// [nakFailure] says so: a turn a drain ended, a publish the
			// broker did not answer or a coordination store that did not
			// answer failed for nothing the mail did.
			return nakFailure(ctx, fmt.Errorf("engine: turn for %s: %w", handle, err))
		}
		return d.abandon(ctx, handle, req, err, reason)
	}
	d.recordWorked(ctx, handle, req, result)
	if d.FailureCleared != nil {
		d.FailureCleared(handle)
	}
	return queue.Ack()
}

// parkOnBudget runs the budget stage, reporting the disposition when it parked
// the seat or could not take the park it decided on. refused is the budget
// refusal the delivery's turn met, nil before any ran, and delivery the events
// that turn was dispatched for, nil with it ([Dispatcher.Budget]).
//
// A PARK IS A DEFERRAL, and noted as one, so the seat host resumes the
// attachment the deferral quiesces on its next renew — the park's own hold is
// what keeps the resumed attachment from being handed anything until the
// window turns over. See budgetpark.go for why the two stops have two owners.
func (d *Dispatcher) parkOnBudget(ctx context.Context, handle string, delivery []*events.Event,
	refused error) (queue.Result, bool) {

	if d.Budget == nil {
		return queue.Result{}, false
	}
	return d.parkOn(ctx, handle, "budget park", func(ctx context.Context) (string, bool, error) {
		return d.Budget(ctx, handle, delivery, refused)
	})
}

// parkOn runs one park stage, reporting the disposition when it parked the
// seat or could not take the park it decided on. what names the park in a
// deferral that could not take it.
func (d *Dispatcher) parkOn(ctx context.Context, handle, what string,
	stage func(context.Context) (string, bool, error)) (queue.Result, bool) {

	reason, parked, err := stage(ctx)
	if err != nil {
		// THE PARK COULD NOT BE TAKEN (the hold the stage decided on
		// was refused by the queue), which is this NODE's condition
		// rather than the message's. Deferred, so the delivery keeps its
		// place at the head of the seat's inbox and comes back first
		// once the seat host's next renew resumes the attachment; a Nak
		// would return it behind the conversation's newer mail
		// (queue.OutcomeNak), and the turn that next ran on this seat
		// would be answering a later message first.
		if d.NoteDeferred != nil {
			d.NoteDeferred(handle)
		}
		return queue.Defer(queue.DeferHoldRefused, what+" could not be taken: "+err.Error()), true
	}
	if !parked {
		return queue.Result{}, false
	}
	if d.NoteDeferred != nil {
		d.NoteDeferred(handle)
	}
	return queue.Defer(queue.DeferParked, reason), true
}

// abandon stops redelivering a trigger whose turn must not run again.
//
// THE REDELIVERY IS THE HARM, not the failure: only the two writes [workkey]
// guards are safe to replay; every MCP write, chat post, `a2a_ask` and
// executor launch is unkeyed, so a deterministic failure replayed them for the
// broker's whole budget (twenty-five attempts, about ten minutes of backoff).
// So the trigger is recorded and acked, which LOSES the rest of the turn and
// needs a reason: [turn.Abandon] gives one only for a record showing it reached
// outside, a panic, or a refusal every redelivery would meet
// ([turn.ErrPermanent]). All else keeps its retry: a moved seat by DEFERRAL
// ([seat.Host.Fence]), a recurring failure parked or paused. Not silent: the
// failed completion ([Engine.publishTurnCompleted]) lacks that it won't return.
func (d *Dispatcher) abandon(ctx context.Context, handle string, req Request, cause error, reason string) queue.Result {
	// BOTH IDENTITIES. This record is paired with the turn's own completion
	// event (see [Engine.publishTurnCompleted]) and the two are joined by a
	// reader: the completion names the RUN, so a line carrying only the work
	// key stopped joining it the moment the two stopped being one value.
	log.ErrorContext(ctx, "turn_abandoned", "seat", handle,
		"run_id", req.RunID, "work_key", req.WorkKey, "reason", reason,
		"error", cause.Error(),
		"detail", "the trigger is recorded rather than redelivered")

	// The completion rows ONLY. A broken turn has no reply to account for:
	// its artifact is whichever round closed last, and nothing here files it
	// as this turn's answer. What its conversation's next turn is told of it
	// is whatever end the turn left on the executor log.
	d.recordUnrun(ctx, handle, req.Events)
	d.noteAbandoned(ctx, handle, req.Events, cause, reason, "")
	return queue.Ack()
}

// recordUnrun records evs worked without a turn, so a redelivery is dropped.
func (d *Dispatcher) recordUnrun(ctx context.Context, handle string, evs []*events.Event) {
	if d.Completions == nil {
		return
	}
	for _, ev := range evs {
		if ev == nil || !d.ledgered(ev.Type) {
			continue
		}
		key := workkey.Derive([]string{ev.ID.String()})
		if err := d.Completions.Record(ctx, handle, key, "", d.now()); err != nil {
			log.WarnContext(ctx, "unrun_trigger_not_recorded", "seat", handle,
				"error", err, "detail", "the trigger may be redelivered and run")
		}
	}
}

// errConversationHeld is why a held conversation's wake was settled unrun.
var errConversationHeld = errors.New("its context did not fit its models twice in a row")

// settleHeld settles a held conversation's delivery without a turn.
func (d *Dispatcher) settleHeld(ctx context.Context, handle string, evs []*events.Event) queue.Result {
	log.InfoContext(ctx, "turn_conversation_held", "seat", handle, "events", len(evs),
		"detail", "the conversation is held until a person writes on it or the seat's models change")
	d.recordUnrun(ctx, handle, evs)
	d.noteAbandoned(ctx, handle, evs, errConversationHeld, "its conversation is held",
		types.DispositionBreakerHeld)
	return queue.Ack()
}

// stopped settles a delivery whose turn a person stopped.
//
// SPENT, NOT RETRIED. The trigger is recorded in the completion ledger and the
// delivery acked, as a finished turn's is: a NAK would run the turn again, on
// this node when the pause lifts or on a peer sooner, and the person stopped
// it so it would not go on. The seat's other mail waits on its held inbox.
//
// ON THE RECORD three ways: the turn's completion reads `stopped`, not
// `failed` ([Engine.publishTurnCompleted]); this frame publishes
// [types.AgentTurnStopped] naming who stopped it; the log line joins the run
// to the work key. No [types.TurnTriggerSkipped]: the trigger WAS worked, as
// far as anybody asked.
func (d *Dispatcher) stopped(ctx context.Context, handle string, req Request, cause error) queue.Result {
	pause, _ := stopOf(cause)
	log.InfoContext(ctx, "turn_stopped", "seat", handle, "run_id", req.RunID,
		"work_key", req.WorkKey, "by", pause.By, "by_kind", string(pause.ByKind),
		"operator_id", pause.OperatorID,
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
		StoppedBy: pause.By, StoppedByKind: string(pause.ByKind),
		OperatorID: pause.OperatorID, Reason: pause.Reason,
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
// ACKED AND RECORDED, never NAKed or deferred ([turn.Abandon]): a redelivery,
// or a defer to a peer on the same build, runs the same defect. The cost goes
// on the record instead: the stack in the log, the unhandled-exception guard
// on the seat, and a skipped-trigger record per event.
//
// held is what the delivery still held (see [holding]). The work key is
// recomputed from its events, since the panic may precede the frame deriving
// it; the RUN is a mint, so it is whatever had been minted, or nothing.
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
		Detail:   events.Diagnostic(panicked.Error()),
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
	cause error, reason string, disposition types.WakeDisposition,
) {
	if d.Observe == nil {
		return
	}
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		// THE CAUSE WHOLE, held only to the event's delivery bound:
		// it was cut at 200 bytes, which a wrapped error spends on
		// its outer operations before it reaches what failed.
		rec := d.skippedRecord(handle, ev, disposition,
			events.Diagnostic(reason+", so it was not redelivered: "+cause.Error()))
		rec.Source = "engine.dispatch"
		d.Observe(ctx, rec)
	}
}

// mayOfferAnswer reports whether this delivery still has the deliveries to
// spare for the answer route, on what the MESSAGES carry.
//
// THE OFFER IS GATED, not the hand-back: every deferral spends a delivery, so
// offering to the last would let the broker dead-letter a person's reply (the
// coordinator's ceiling resets on a handoff; the broker's count does not). See
// [sandbox.AnswerDeliveryReserve] and [sandbox.MaxAnswerAttempts]. EACH
// EVENT'S OWN COUNT ([queue.DeliveriesLeftFor], folded by
// [sandbox.MayOfferAnswer]), never the partition's smallest, which refused a
// first delivery for an older spent sibling. The two log lines are different
// facts: refusing makes an ordinary turn while an executor may be parked;
// offering inside the partition's reserve may spend a sibling.
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
				"than offered to a parked executor again; handing it back once "+
				"more risks the broker dead-lettering the reply instead")
		return false
	}
	if leastKnown && least <= sandbox.AnswerDeliveryReserve {
		log.WarnContext(ctx, "sandbox_answer_offered_over_a_spent_sibling",
			"seat", handle, "partition_deliveries_left", least,
			"reserve", sandbox.AnswerDeliveryReserve,
			"detail", "a message of this delivery still has the deliveries to spare, "+
				"so a parked executor is being offered it; another message in the "+
				"same partition is inside the reserve, and a hand-back returns the "+
				"whole partition, so that one may dead-letter")
	}
	return true
}

// answered offers a delivery to one of this seat's executors that is waiting
// for somebody to answer its question, and reports what to do with it.
//
// FAIL-OPEN WHERE NOTHING WAS MATCHED: a missing seam, an unreplyable
// conversation, no deliveries to spare ([Dispatcher.mayOfferAnswer]) and a
// failed lookup report [sandbox.AnswerNotMine], so the screening's disposition
// stands: recoverable, where acking an unhandled message is not.
//
// HANDED BACK WHERE A RUN IS STILL OWED IT: [sandbox.AnswerDeferred] defers
// keeping the delivery's place (a NAK lands BEHIND newer mail,
// queue.OutcomeNak), [sandbox.AnswerRefused] (ADR-0114) likewise, unspent: the
// delivery is the reply's only copy. Offered are its own words ([replyWords]).
func (d *Dispatcher) answered(ctx context.Context, handle string, evs []*events.Event) (sandbox.AnswerDisposition, error) {
	if d.Answer == nil {
		return sandbox.AnswerNotMine, nil
	}
	// The identity admits the runs a delivery may answer and the partition
	// picks among them ([sandbox.ConversationRef.Best]): a run from a
	// top-level DM parks under the bare channel while [notify.ChatPrompt] has
	// the seat reply as a thread, a partition the row never named.
	conv := sandbox.ConversationRef{
		Identity:  conversationIdentityOf(evs),
		Partition: partitionKeyOf(evs),
	}
	// NEVER OFFERED: a conversation that is no key, or a SCHEDULE'S
	// ([notify.Derived]): every fire carries the conversation its last run
	// parked under, so the next fire's cron text would be taken as that run's
	// answer. Refused before the coordinator and the headroom are asked.
	if !notify.Derived(conv.Identity) || !d.mayOfferAnswer(ctx, handle, evs) {
		return sandbox.AnswerNotMine, nil
	}
	disposition, err := d.Answer(ctx, handle,
		replyOf(d.promptRegistry(), d.parties(), handle, conv, evs))
	if err != nil {
		// An error explains the disposition and never overrides it: reading
		// it instead spent an answer on an unrelated turn where the run was
		// still owed it. Both travel out, as a NAK needs the cause.
		log.WarnContext(ctx, "sandbox_answer_dispatch_failed",
			"agent_handle", handle, "conversation", conv.Identity,
			"partition", conv.Partition, "disposition", disposition.String(),
			"error", err)
	}
	if !disposition.Valid() {
		// Not a requeue: a defer has nothing to bound it, the bound lives with
		// the matched RUN, which an unreadable disposition does not name.
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

// answerOwedReason is the deferral reason a chat reply is handed back with
// while a parked run is still owed it.
func answerOwedReason(handle string, cause error) string {
	if cause == nil {
		return fmt.Sprintf("%s is still owed this answer", handle)
	}
	return fmt.Sprintf("%s is still owed this answer: %s", handle, cause)
}

// refusedReplyReason is the deferral reason a reply too large for its run is
// kept with while the refusal recorded for it is the run's
// ([sandbox.AnswerRefused]).
func refusedReplyReason(handle string) string {
	return fmt.Sprintf("a run of %s holds this reply's refusal, which a resumed turn has not taken yet; "+
		"the reply is kept until it does, or comes back as an ordinary message if the refusal is let go of",
		handle)
}

// AnswerWorked reports whether an answer by turn's delivery is recorded as
// worked in the completion ledger ([Dispatcher.SpendAnswer]), for the
// coordinator that settles it off the seat's control topic
// ([sandbox.CoordinatorOptions.WasSpent]). FAILS OPEN, as every ledger read
// does: unreadable, the delivery reaches the coordinator, which finds what the
// run says about it.
func (d *Dispatcher) AnswerWorked(ctx context.Context, handle string, ev *events.Event) bool {
	if d.Completions == nil || ev == nil {
		return false
	}
	key := workkey.Derive([]string{ev.ID.String()})
	return d.Completions.Worked(ctx, handle, []string{key})[key]
}

// SpendAnswer records the deliveries a parked run's answer arrived in as
// worked: a chat reply once recorded on its run, an answer by turn once the
// coordinator has settled it ([sandbox.Coordinator.OnAnswerGiven]), and
// either once its run spends it ([sandbox.CoordinatorOptions.Spent]).
//
// A copy of a spent answer must become nothing: a redelivery or a park's
// republished copy would run as an ordinary message, or be announced twice.
// The ledger is the fleet's, so the copy is dropped on whichever node it
// reaches. EVERY DELIVERY, WHATEVER ITS TYPE: [inbox.Ledgered] is the types
// that run a turn, which an answer by turn never does. Both routes read this
// ([Dispatcher.AnswerWorked]). FAILS OPEN, like every completion write.
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
// requeued, as the node's own where the requeue failed for a reason of this
// node's ([nakFailure]). Only behind a pause hold ([inbox.ActionPauseAndPark]):
// the copies land on the inbox the delivery came from, and with nothing
// stopping the consumer they would be fetched straight back.
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
	// AND REQUEUED, before the requeue as well: the copies wait for the
	// seat as the events they are, so the delivery is not gone with the ack
	// that settles its original ([goneBy]).
	held.requeued = evs
	if err := d.Park(ctx, handle, evs); err != nil {
		return nakFailure(ctx, fmt.Errorf("engine: park %s: %w", handle, err))
	}
	return queue.Ack()
}

// nakFailure NAKs a seat's mail that failed, for a retry: as THIS NODE'S OWN
// failure ([queue.NakNode]) where [nodesFailure] says it is one, and as the
// mail's ([queue.Nak]) otherwise. Both return the delivery on the same
// backoff and spend the same budget; what differs is where a delivery whose
// budget is spent is kept ([queue.RetiredTo]): as the mail's, with its group's
// poison, which nothing replays; as the node's, with its unhandled deliveries.
// The requeue of a partition's tail, a park's requeue and a turn's failure all
// answer through it, so a failure one keeps out of the poison the others keep
// out too.
func nakFailure(ctx context.Context, err error) queue.Result {
	if nodesFailure(ctx, err) {
		return queue.NakNode(err)
	}
	return queue.Nak(err)
}

// nodesFailure reports whether err, the failure of a delivery a seat was
// handed, is THIS NODE'S OWN or its infrastructure's rather than the work's:
// the one answer the seat's mailbox ([nakFailure]) and its control topic
// ([controlOutcome]) NAK by, and the sandbox coordinator counts a failed resume
// toward its give-up by ([sandbox.CoordinatorOptions.NodesFailure]), since the
// same failure reaches all three. The node's: what the queue tells as the
// node's ([queue.NodeFault]), a condition of the infrastructure by the one
// classification ([builtin.InfrastructureKind], read off the failure), and the
// sandbox coordinator's marks ([sandbox.ErrStoreSilent],
// [sandbox.ErrUnannounced]); never [sandbox.ErrResumeElsewhere], which the
// give-up counts once no node can make it. The package doc's "A failure of
// this node's is never the mail's own" says why.
func nodesFailure(ctx context.Context, err error) bool {
	return storeUnheard(err) || queue.NodeFault(ctx, err) || infrastructureFailure(err)
}

// storeUnheard reports whether err carries one of the sandbox coordinator's
// marks for a store that did not answer it: a step's silence
// ([sandbox.ErrStoreSilent]) or a hand-back no waiter can announce again
// ([sandbox.ErrUnannounced]).
func storeUnheard(err error) bool {
	return errors.Is(err, sandbox.ErrStoreSilent) || errors.Is(err, sandbox.ErrUnannounced)
}

// operatorsCondition is [sandbox.CoordinatorOptions.OperatorsCondition]: the
// kind of err where it is a condition only an operator clears
// ([builtin.OperatorKind]), by the one classification of conditions
// ([builtin.ConditionKind]) every failure is told by.
func operatorsCondition(err error) (string, bool) {
	kind, ok := builtin.ConditionKind(err)
	return kind, ok && builtin.OperatorKind(kind)
}

// infrastructureFailure reports whether err is a condition of the
// infrastructure the engine runs on, by the one classification of conditions
// ([builtin.ConditionKind], [builtin.InfrastructureKind]).
func infrastructureFailure(err error) bool {
	kind, ok := builtin.ConditionKind(err)
	return ok && builtin.InfrastructureKind(kind)
}

// dropWorked removes the events the completion ledger has already recorded.
//
// A partial overlap matters: a redelivery of (A, B) after (A, B, C) was worked
// drops A and B and runs C.
//
// TWO KEYS PER CHAT MESSAGE, either of which drops it: the delivery's own, and
// the message's identity on its chat backend, which a LATER turn records when
// its thread block showed it waiting (workedthrough.go): a failed message
// coming round behind newer mail is not answered again, out of order. It
// reports the events covered ONLY by the second key, so the skip record says a
// later turn answered them in their thread.
func (d *Dispatcher) dropWorked(ctx context.Context, handle string, evs []*events.Event) ([]*events.Event, map[uuid.UUID]skipCause) {
	var worked map[string]bool
	if d.Completions != nil {
		keys := make([]string, 0, len(evs))
		for _, ev := range evs {
			if d.ledgered(ev.Type) {
				keys = append(keys, deliveryKeys(ev)...)
			}
		}
		if len(keys) > 0 {
			worked = d.Completions.Worked(ctx, handle, keys)
		}
	}
	if len(worked) == 0 && d.SeenThrough == nil {
		return evs, nil
	}
	marked := d.markedOf(ctx, handle, evs, worked)
	out := make([]*events.Event, 0, len(evs))
	var skipped map[uuid.UUID]skipCause
	skip := func(id uuid.UUID, cause skipCause) {
		if skipped == nil {
			skipped = map[uuid.UUID]skipCause{}
		}
		skipped[id] = cause
	}
	marks := map[string]statelog.Position{}
	for _, ev := range evs {
		if !d.ledgered(ev.Type) {
			out = append(out, ev)
			continue
		}
		chat, edit, isChat := chatKeyOf(ev)
		switch {
		case marked[joinedKey(ev.ID.String())]:
			skip(ev.ID, skipJoined)
		case marked[steeredKey(ev.ID.String())]:
			skip(ev.ID, skipSteered)
		case marked[foldedKey(ev.ID.String())]:
			skip(ev.ID, skipFolded)
		case marked[absorbedKey(ev.ID.String())]:
			skip(ev.ID, skipTriaged)
		case worked[workkey.Derive([]string{ev.ID.String()})]:
			skip(ev.ID, skipWorked)
		case isChat && edit && worked[chat]:
			skip(ev.ID, skipEditInThread)
		case isChat && worked[chat]:
			skip(ev.ID, skipInThread)
		case d.absorbed(ctx, handle, ev, marks):
			skip(ev.ID, skipAbsorbed)
		default:
			out = append(out, ev)
		}
	}
	return out, skipped
}

// deliveryKeys is every completion-ledger key that drops ev's delivery
// ([Dispatcher.dropWorked]): its own, and a chat message's identity on its
// backend. ONE FUNCTION, so a wake the router hands a running run is claimed by
// exactly the keys its delivery is dropped by (wakerouter.go).
func deliveryKeys(ev *events.Event) []string {
	keys := []string{workkey.Derive([]string{ev.ID.String()})}
	if chat, _, ok := chatKeyOf(ev); ok {
		keys = append(keys, chat)
	}
	return keys
}

// markedOf is which of evs a running turn took while its run held the seat, by
// the mark it recorded: read as a wake ([joinedKey]), filed as its lead's steer
// note ([steeredKey]) or as an approved fold-in's ([foldedKey]), or absorbed by
// its triage ([absorbedKey]). Asked only of
// those whose own key is worked, so a dispatch no turn worked any trigger of
// reads the ledger no more than it did.
func (d *Dispatcher) markedOf(ctx context.Context, handle string, evs []*events.Event,
	worked map[string]bool) map[string]bool {
	var keys []string
	for _, ev := range evs {
		if id := ev.ID.String(); d.ledgered(ev.Type) && worked[workkey.Derive([]string{id})] {
			keys = append(keys, joinedKey(id), steeredKey(id), foldedKey(id), absorbedKey(id))
		}
	}
	if len(keys) == 0 || d.Completions == nil {
		return nil
	}
	return d.Completions.Worked(ctx, handle, keys)
}

// recordWorked writes the completion ledger after a turn.
//
// Per CONSTITUENT event, not per partition: a redelivery of a SUBSET keys
// differently from the partition's key and would run again; each constituent
// under its own key makes a partial overlap droppable.
//
// The write fails open: a turn that ran and could not be recorded may run
// again; one refused because its bookkeeping failed never runs at all.
//
// How the turn ended is not written here: the next turn is told from the end
// on the executor log ([Engine.runTurn], or [Engine.recordResume] for a
// detached turn).
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
	d.advanceSeen(ctx, handle, req.Events)
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
// Nil-tolerant on the receiver: a caller reaching a dispatcher that was never
// built — a partially wired engine, a test driving one method — asks the wall
// clock rather than panicking.
func (d *Dispatcher) now() time.Time {
	if d == nil || d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now()
}

// conversationIdentityOf takes the durable conversation identity from the
// events, and partitionKeyOf takes the inbox partition key.
//
// TWO FUNCTIONS BECAUSE THERE ARE TWO QUESTIONS: the executor log, telemetry,
// episodes and a parked run's answer match want the identity; the coalescing
// record wants the partition, which decides that N events MERGED. The answer
// match takes both. One value for both filed a seat's prior turn under a key
// its next turn never looked up and lost thread clarifications. The FIRST
// event naming one wins: a source's key refines its identity
// ([notify.Prompt.ConversationIdentity]), so a later event naming another is a
// routing bug. Delegates to [notify.ConversationIdentityOfAll], [notify.KeyOfAll].
func conversationIdentityOf(evs []*events.Event) string {
	return notify.ConversationIdentityOfAll(evs)
}

func partitionKeyOf(evs []*events.Event) string { return notify.KeyOfAll(evs) }

// DescribeTrigger renders a partition as the ask a turn is given.
//
// The FIRST event leads: its opening message is what the rest reply to. THE
// BRIEF, THEN THE SUMMARY, THEN THE TYPE NAME: [events.Briefer] is the ask;
// [events.Summarizer] is a dashboard line that as the ask handed every turn a
// stub, serving only a wake with no Briefer. A NOTIFICATION'S FENCES MOVE ONTO fence, the turn's own token, and what
// surrounds them (a display name) is neutralised ([provenance.Rekey]); no
// token keeps the wake's. A parked run's answer is no reader of the trigger
// ([replyWords]). EVERY OTHER WAKE is rendered here under the turn's fence: a
// colleague's words inside one ([fencedBriefer]), every other brief (a
// schedule's task) neutralised whole, as the company's own instruction.
func DescribeTrigger(evs []*events.Event, fence provenance.Boundary) string {
	var parts []string
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if n, ok := events.DataAs[*types.ExternalNotification](ev); ok && n != nil {
			if b := strings.TrimSpace(provenance.Rekey(n.Body,
				provenance.Boundary(n.FenceBoundary), fence)); b != "" {
				parts = append(parts, b)
				continue
			}
		}
		if brief, ok := ev.Data.(fencedBriefer); ok {
			if b := strings.TrimSpace(brief.FencedBrief(fence)); b != "" {
				parts = append(parts, b)
				continue
			}
		}
		if brief, ok := ev.Data.(events.Briefer); ok {
			if b := strings.TrimSpace(provenance.Neutralise(fence, brief.Brief())); b != "" {
				parts = append(parts, b)
				continue
			}
		}
		if summarised(ev) {
			if s := provenance.Neutralise(fence, ev.Summary()); s != "" {
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

// summarised reports a payload that writes its own line, as a summary or as a
// line that names what it is about ([events.Phraser]).
func summarised(ev *events.Event) bool {
	_, summary := ev.Data.(events.Summarizer)
	_, phrase := ev.Data.(events.Phraser)
	return summary || phrase
}

// fencedBriefer is a wake whose ask carries somebody else's words: its brief,
// with those words inside a fence of the token it is handed (ADR-0040).
type fencedBriefer interface {
	FencedBrief(provenance.Boundary) string
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
// Written here because this is the one frame still holding the constituent
// list: a coalesced digest is minted fresh on every merge and remembers
// nothing of what it absorbed. Nothing else emits this event; without it a
// seat draining a backlog as one turn looked, from the feed, like a seat that
// ignored twelve messages.
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

// noteSuperseded records every scheduled fire the turn's trigger stands for
// and does not run ([mergeScheduledFires]), each under that fire's OWN trace.
//
// UNDER ITS OWN TRACE because a fire's trace is how its schedule history finds
// what became of it: the run is found by the turn under the kept fire's trace,
// and a fire that was not run is found by this record under its own. Recorded
// at the merge, like the coalescing record beside it, because this is the one
// frame that holds both the constituents and the run they became. A turn that
// fails and comes back merges again and records again, which a reader asking
// "was this fire run" answers the same way.
//
// Best effort and nil-safe, like the records beside it.
func (d *Dispatcher) noteSuperseded(ctx context.Context, handle string, evs, trigger []*events.Event) {
	if d.Observe == nil || len(trigger) != 1 {
		return
	}
	kept, ok := events.DataAs[*types.TaskAssigned](trigger[0])
	if !ok || kept == nil || len(kept.Missed) == 0 {
		return
	}
	missed := make(map[string]bool, len(kept.Missed))
	for _, m := range kept.Missed {
		missed[m.TaskID] = true
	}
	for _, ev := range evs {
		fire, ok := events.DataAs[*types.TaskAssigned](ev)
		if !ok || fire == nil || !missed[fire.TaskID] {
			continue
		}
		rec := events.New(types.ScheduledFireSuperseded{
			ScopeType:    fire.ScopeType,
			ScopeID:      fire.ScopeID,
			ScheduleName: fire.Schedule,
			TargetHandle: handle,
			TaskID:       fire.TaskID,
			ScheduledAt:  dueAt(ev, fire).Format(time.RFC3339),
			SupersededBy: kept.TaskID,
		}, events.TraceContext{TraceID: ev.TraceID, ParentSpanID: ev.SpanID})
		rec.Source = "engine." + ev.Source
		d.Observe(ctx, rec)
	}
}

// noteSkipped records every constituent the completion ledger already worked.
//
// Best effort and nil-safe, like the coalescing record beside it: a node whose
// queue refused the row has still skipped correctly, and failing the dispatch
// over an observability event would trade real work for a feed entry.
func (d *Dispatcher) noteSkipped(ctx context.Context, handle string, all, surviving []*events.Event,
	skipped map[uuid.UUID]skipCause,
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
	markedBy := d.markedBy(ctx, handle, all, skipped)
	for _, ev := range all {
		if ev == nil || kept[ev.ID] {
			continue
		}
		// WHICH TURN WORKED IT is the half of the reason a reader needs:
		// a turn woken for this very trigger, or a later turn that was
		// shown it waiting in its thread and answered the thread as it
		// stood (see workedthrough.go).
		reason, disposition := "a previous turn already worked this trigger", types.WakeDisposition("")
		switch skipped[ev.ID] {
		case skipInThread:
			reason = "a later turn was shown this message waiting in its thread and answered it there"
		case skipEditInThread:
			reason = "a later turn was shown this edit waiting in its thread and answered it there"
		case skipAbsorbed:
			reason, disposition = "a completed turn already read its conversation past it, and it asks nothing", types.DispositionAbsorbed
		case skipJoined:
			reason, disposition = joinedReason(markedBy[joinedKey(ev.ID.String())]), types.DispositionJoined
		case skipSteered:
			reason, disposition = steeredReason(markedBy[steeredKey(ev.ID.String())]), types.DispositionJoined
		case skipFolded:
			reason, disposition = foldedReason(markedBy[foldedKey(ev.ID.String())]), types.DispositionJoined
		case skipTriaged:
			reason, disposition = triagedReason(markedBy[absorbedKey(ev.ID.String())]), types.DispositionAbsorbed
		}
		// THE SKIPPED TRIGGER'S OWN TRACE, not the dispatch's: the
		// question this record answers is "what happened to my webhook",
		// and the answer belongs under the webhook rather than under a
		// dispatch that went on to do something else.
		rec := d.skippedRecord(handle, ev, disposition, reason)
		rec.Source = "engine.dispatch"
		d.Observe(ctx, rec)
	}
}

// markedBy is the running turn each of all's skips by a mark went to, by that
// mark ([Dispatcher.markedOf]): one ledger read for them all, none where no
// running turn took any.
func (d *Dispatcher) markedBy(ctx context.Context, handle string, all []*events.Event,
	skipped map[uuid.UUID]skipCause) map[string]string {

	var keys []string
	for _, ev := range all {
		if ev == nil {
			continue
		}
		switch id := ev.ID.String(); skipped[ev.ID] {
		case skipJoined:
			keys = append(keys, joinedKey(id))
		case skipSteered:
			keys = append(keys, steeredKey(id))
		case skipFolded:
			keys = append(keys, foldedKey(id))
		case skipTriaged:
			keys = append(keys, absorbedKey(id))
		}
	}
	if len(keys) == 0 || d.Completions == nil {
		return nil
	}
	return d.Completions.WorkedBy(ctx, handle, keys)
}

// joinedReason is a joined skip's reason, naming the turn it joined where the
// ledger's record does.
func joinedReason(turnID string) string {
	turn := "a running turn"
	if turnID != "" {
		turn = "running turn " + turnID
	}
	return "it joined " + turn + " as a message about its work, which that turn's run read: the turn's " +
		"agent_turn_joined record names it"
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
// DERIVED FROM THE TRIGGER, not from the model's say: a declared `skip` on a
// direct @mention read like the message never arriving. STRONGEST WINS across a coalesced partition, in any arrival order, so a
// merge cannot launder an obligation: [turn.ReplyTool] beats [turn.ReplyEngine]
// beats [turn.ReplyNone]. A tool obligation is the one the engine ENFORCES
// ([turn.Check], [turn.OverrideDone]); an A2A ask is answered from the
// artifact either way, and engine would let the turn end in text a tool-side
// requester never sees. The inbox builds no such partition today
// ([inbox.Route]); the ranking is a contract. The default [turn.ReplyNone] is
// the safe half: a seat wrongly told somebody waits must post on every broadcast.
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

		case types.DelegatedReviewType:
			// A REQUEST WAITS ON THE DELEGATE'S VERDICT, which only its tool
			// gives (ADR-0125): a turn that ends without one leaves the
			// request to its deadline, so the turn is told it owes one.
			owed = turn.ToolReply(toolname.ReviewDelegated)

		case types.WorkPickupType:
			// A PICKUP IS ANSWERED ON ITS ITEM, TYPED (ADR-0122): the turn
			// is delivered only by a change on the item it was handed or
			// an ask, never by a comment saying it still waits.
			owed = turn.ToolReply("")
			if p, ok := events.DataAs[*types.WorkPickup](ev); ok && p != nil {
				owed.Item = p.Item
			}

		case types.ExternalNotification{}.EventType():
			// The app's own reading of its routing ([notify.Prompt.Addressed]);
			// absent decodes false, a freedom to stay silent. OFF THE TYPED
			// PAYLOAD, never the free-form bag: nothing writes Addressed
			// there, so a bag read answered false for EVERY notification.
			//
			// AND WHERE THEY ARE WAITING: the source is the vendor's name
			// (`mattermost`, `slack`, `jira`), the vocabulary a delivering
			// tool reports; without it any tool satisfies the obligation, and
			// a Mattermost DM was closed out by a tracker row. UNLESS THE WAKE
			// OWES ANOTHER SURFACE: [types.ExternalNotification.Owes] names
			// the chat a tracker-arriving decision promised its outcome to,
			// and an owed wake is awaited though unaddressed.
			if n, ok := events.DataAs[*types.ExternalNotification](ev); ok &&
				(n.Addressed || n.Owes != "") {
				owed = turn.ToolReply(cmp.Or(n.Owes, n.NotificationSource))
			}

			// types.A2AMessageType is deliberately absent: that hop wakes
			// the REQUESTER with an answer it asked for, on a channel that
			// is already closed. Nobody is waiting on what this turn does
			// with it.
		}
		out = strongerReply(out, owed)
	}
	return out
}

// strongerReply is out with owed merged in as [ReplyFor] merges a partition's:
// a stronger kind wins, and A PICKUP'S ITEM SURVIVES A MERGE with another tool
// obligation, the typed answer it owes being the stricter; otherwise out
// stands. Also how a wake a run read joins its turn's (ADR-0167).
func strongerReply(out, owed turn.Reply) turn.Reply {
	if replyRank[owed.Kind] > replyRank[out.Kind] ||
		owed.Kind == out.Kind && out.Item == "" && owed.Item != "" {
		return owed
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

// accessReviewsOnly reports a delivery made only of access reviews.
func accessReviewsOnly(evs []*events.Event) bool {
	for _, ev := range evs {
		n, ok := events.DataAs[*types.ExternalNotification](ev)
		if !ok || n == nil || n.NotificationSource != accessreview.Source {
			return false
		}
	}
	return len(evs) > 0
}
