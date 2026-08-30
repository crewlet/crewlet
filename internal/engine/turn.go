package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
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

	// Pause stops delivery on a seat's inbox before a park, so the requeued
	// copies buffer on the queue rather than looping straight back.
	Pause func(ctx context.Context, handle, reason string) error

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

	// Coalesce is true when the partition must be merged into one digest
	// trigger, so the seat runs one turn instead of N.
	Coalesce bool

	// History is what this seat already said in this conversation.
	History []ledger.Session

	// ConversationKey is the surface-scoped conversation identity, empty
	// when the trigger has none.
	ConversationKey string

	// Depth is the delegation depth this turn inherited: zero for a turn a
	// person or a schedule started, higher for one a colleague asked for.
	// Carried so a sub-agent spawn or an A2A ask can refuse past the cap
	// rather than discovering the loop at runtime.
	Depth int
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
//   - the conversation read comes after that, because it is keyed on a
//     conversation the surviving events name;
//   - the completion WRITE comes after the turn, and a turn that failed is
//     still recorded: the ledger answers "has this trigger been worked", not
//     "did the work succeed", and re-running a failing turn on every
//     redelivery is how one bad trigger becomes an infinite loop.
func (d *Dispatcher) Dispatch(ctx context.Context, handle string, evs []*events.Event) queue.Result {
	screening := inbox.Screen(d.conditions(handle), evs)
	if screening.NoteDeferred && d.NoteDeferred != nil {
		d.NoteDeferred(handle)
	}
	switch screening.Action {
	case inbox.ActionDrop, inbox.ActionDefer:
		return screening.Result()
	case inbox.ActionPauseAndPark:
		if d.Pause != nil {
			if err := d.Pause(ctx, handle, screening.Reason); err != nil {
				// The pause is what stops the requeued copies looping
				// back at whatever rate the broker will serve. Without it
				// the park is worse than doing nothing, so NAK and let
				// the delivery come back rather than spin.
				return queue.Nak(fmt.Errorf("engine: pause %s: %w", handle, err))
			}
		}
		return d.park(ctx, handle, screening)
	case inbox.ActionPark:
		return d.park(ctx, handle, screening)
	}

	surviving := d.dropWorked(ctx, handle, screening.Events)
	if len(surviving) == 0 {
		return queue.Ack()
	}

	routing := inbox.Route(surviving, d.ledgered)
	req := Request{
		Handle: handle, Events: routing.Events,
		WorkKey: routing.WorkKey, Coalesce: routing.Coalesce,
		ConversationKey: conversationKeyOf(routing.Events),
	}
	d.noteCoalesced(ctx, handle, req.ConversationKey, routing)
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if history, err := d.history(ctx, handle, req.ConversationKey); err == nil {
		req.History = history
	} else {
		// A conversation ledger that cannot be read is a seat with less
		// context, not a seat that cannot work. The read RAISES rather
		// than returning empty precisely so this decision is made here,
		// visibly, instead of a database outage looking like a first turn.
		log.Warn("conversation_history_unreadable",
			"seat", handle, "conversation", req.ConversationKey, "error", err)
	}

	// The work key travels on the context, not as an argument, because the
	// writers that must not duplicate under it sit frames below the
	// dispatch behind functions with no other reason to carry it.
	ctx = workkey.With(ctx, req.WorkKey)

	// The acting seat travels the same way, and for the same reason: the
	// only consumer is a leaf. A cli-agent provider gives every seat its
	// own CLI home, because seven seats on one subscription sharing one
	// home would read each other's transcripts — and an unbound call would
	// silently put them all back in one. Bound HERE, at the single place a
	// turn's context is built, rather than at each phase, so a new phase
	// cannot forget it.
	ctx = llm.WithSeat(ctx, handle)

	// And the trigger's trace, so this turn's spans hang under whatever
	// caused it — a webhook, a schedule, another agent — instead of each
	// seat rooting a trace of its own and the join a reader follows from
	// "a message arrived" to "here is what it did" never existing.
	//
	// Bound HERE, with the other two, because the consumer is a leaf and
	// because this is the single place a turn's context is built: a phase
	// added later cannot forget it.
	//
	// A COALESCED TURN HAS ONE TRACE AND SEVERAL CAUSES. The trace is taken
	// from the first event of the partition, which is the same event
	// describeTurn takes the trigger from — ten Slack comments become one
	// turn under one trace. The others are already recorded as the turn's
	// interactions; a span cannot have two parents, and inventing a root to
	// hold them would put a node above the webhook that actually happened.
	ctx = tracing.WithRemote(ctx, triggerTrace(routing.Events))

	result, err := d.Turn(ctx, req)
	if err != nil {
		// A broken phase, not a failed turn. NAK so the delivery comes
		// back: nothing was recorded, so a redelivery runs it cleanly.
		return queue.Nak(fmt.Errorf("engine: turn for %s: %w", handle, err))
	}
	d.recordWorked(ctx, handle, req, result)
	return queue.Ack()
}

func (d *Dispatcher) park(ctx context.Context, handle string, s inbox.Screening) queue.Result {
	if d.Park == nil {
		// No park path wired. Acking would drop the work; NAK returns it
		// to the broker, which is the only honest answer.
		return queue.Nak(fmt.Errorf("engine: %s: no requeue path for a park", handle))
	}
	if err := d.Park(ctx, handle, s.Events); err != nil {
		return queue.Nak(fmt.Errorf("engine: park %s: %w", handle, err))
	}
	return queue.Ack()
}

// dropWorked removes the events the completion ledger has already recorded.
//
// A partial overlap is the case that matters: a redelivery of (A, B) after
// (A, B, C) was worked drops A and B and runs C, rather than re-running all
// three or skipping all three.
func (d *Dispatcher) dropWorked(ctx context.Context, handle string, evs []*events.Event) []*events.Event {
	if d.Completions == nil {
		return evs
	}
	keys := make([]string, 0, len(evs))
	for _, ev := range evs {
		if d.ledgered(ev.Type) {
			keys = append(keys, workkey.Derive([]string{ev.ID.String()}))
		}
	}
	if len(keys) == 0 {
		return evs
	}
	worked := d.Completions.Worked(ctx, handle, keys)
	if len(worked) == 0 {
		return evs
	}
	out := make([]*events.Event, 0, len(evs))
	for _, ev := range evs {
		if d.ledgered(ev.Type) && worked[workkey.Derive([]string{ev.ID.String()})] {
			continue
		}
		out = append(out, ev)
	}
	return out
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
		for _, ev := range req.Events {
			if !d.ledgered(ev.Type) {
				continue
			}
			key := workkey.Derive([]string{ev.ID.String()})
			if err := d.Completions.Record(ctx, handle, key, "", now); err != nil {
				log.Warn("completion_not_recorded", "seat", handle, "error", err)
				break
			}
		}
	}
	if d.Conversations == nil || req.ConversationKey == "" {
		return
	}
	entry := ledger.BuildSession(ledger.SessionInput{
		At:       now.Format(time.RFC3339),
		Reply:    res.Artifact,
		Decision: res.Decision.String(),
	})
	if res.LastReview != nil {
		entry.CompletedWork = res.LastReview.CompletedWork
	}
	if err := d.Conversations.Append(ctx, handle, req.ConversationKey, entry,
		req.WorkKey, now, 0); err != nil {
		log.Warn("conversation_not_recorded", "seat", handle,
			"conversation", req.ConversationKey, "error", err)
	}
}

func (d *Dispatcher) history(ctx context.Context, handle, conversation string) ([]ledger.Session, error) {
	if d.Conversations == nil || conversation == "" {
		return nil, nil
	}
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

func (d *Dispatcher) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now()
}

// conversationKeyOf takes the conversation identity from the events.
//
// The FIRST event that names one wins. A partition is one conversation by
// construction — that is what the broker's key function guarantees — so a
// later event naming a different one is a routing bug, and taking the first
// keeps the answer stable rather than depending on which event happened to
// sort last.
func conversationKeyOf(evs []*events.Event) string {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if key, _ := ev.Payload["conversation_key"].(string); key != "" {
			return key
		}
	}
	return ""
}

// DescribeTrigger renders a partition as the ask a turn is given.
//
// The FIRST event's own description leads, because a coalesced partition is
// one conversation and its opening message is what the rest are replies to. A
// digest that led with the newest would hand the seat a follow-up with no idea
// what it follows.
func DescribeTrigger(evs []*events.Event) string {
	var parts []string
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if text, _ := ev.Payload["text"].(string); text != "" {
			parts = append(parts, text)
			continue
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

// noteCoalesced records a partition merged into one digest trigger.
//
// # Here, because here is where the constituent list exists
//
// A coalesced digest is minted fresh on every merge and carries no memory of
// what it absorbed; by the time a turn is running there is nothing left to
// count. So the record is written at the one frame that still holds the
// events — the same reason the work key is derived here.
//
// Nothing else emits this event, which is why the Integrations room could
// report how many deliveries ARRIVED and not how many turns they became: a
// seat draining a thread's backlog as one turn looked, from the feed, like a
// seat that ignored twelve messages.
func (d *Dispatcher) noteCoalesced(ctx context.Context, handle, conversation string,
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
		AgentHandle: handle, ConversationKey: conversation,
		// THE SOURCE OF THE FIRST EVENT names the integration. A merge is
		// always one conversation's worth of external notifications, and a
		// conversation belongs to one vendor — so the constituents cannot
		// disagree, and taking the first is a lookup rather than a choice.
		NotificationSource: routing.Events[0].Source,
		Count:              len(routing.Events),
		FirstAt:            first.UTC().Format(time.RFC3339),
		LastAt:             last.UTC().Format(time.RFC3339),
	}, events.NewTrace())
	ev.Source = "engine." + routing.Events[0].Source
	d.Observe(ctx, ev)
}

// triggerTrace is the trace a partition of trigger events belongs to.
//
// The FIRST non-nil event, matching describeTurn's choice of trigger, so the
// trace and the trigger a turn reports are always the same event's. Taking
// them from different events is how a turn ends up filed under a trace whose
// root says something it did not react to.
//
// An empty result is ordinary rather than exceptional: an event written by a
// build older than tracing carries no ids, and a rolling upgrade guarantees
// some do. WithRemote turns that into a fresh root.
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
