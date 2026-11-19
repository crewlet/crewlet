// Package inbox holds the ordering rule for a seat's inbox partition.
//
// THE ORDER IS THE PRODUCT. Every stage below sits where it does because a
// different position produced a specific, observed failure, so the sequence is
// written out once, in one place, with each stage's reason at the stage rather
// than in a comment on the caller.
//
// The decision is separated from the EFFECTS — no broker, no database, no
// event queue is reachable from here. A handler that mixes the two can only be
// exercised against a live broker — which means its ordering can only be
// pinned by integration tests that take a container to run.
//
// Two stages, and the split is structural rather than stylistic: [Screen] runs
// every guard that must precede the completion-ledger read, and returns the
// events that survive. The caller cannot perform the ledger read before
// Screen, because Screen is what hands it the list to read about.
package inbox

import (
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/workkey"
)

// Action is what to do with a partition.
type Action int

const (
	// ActionProceed — no guard fired; continue to the ledger read.
	ActionProceed Action = iota

	// ActionDrop — ack and do nothing. Nothing is left to work.
	ActionDrop

	// ActionDefer — leave the delivery unacked and STOP CONSUMING.
	//
	// Not a requeue: a requeue sends these to the topic tail while the
	// successor replays its prefetched siblings from the head, which
	// reorders the conversation. Not a NAK either: the delivery budget is
	// finite (25), it is shared with real failures, and a condition that
	// holds for minutes would spend it on a perfectly healthy event.
	ActionDefer

	// ActionPauseAndPark — pause the topic FIRST so the requeued copies
	// buffer on the queue rather than looping straight back, then requeue
	// the events and ack. For a wait that outlasts any broker ack window.
	// The hold it takes is named by [Screening.Hold].
	ActionPauseAndPark

	// ActionHoldAndDefer — make sure the hold named by [Screening.Hold] is
	// on the topic, then defer. For a delivery that reached a seat whose
	// inbox is SUPPOSED to be held already, by the subsystem that owns the
	// hold: it raced the hold, or the hold was refused, and asking for it
	// again is the retry. The deferral keeps the delivery at the head, and
	// the hold — not the deferral's quiesce, which the seat host lifts on its
	// next renew — is what keeps it there until the condition clears.
	//
	// NOT A PARK, which is what this used to be for a seat an executor
	// holds: a park REPUBLISHES onto the inbox the delivery came from, and
	// with nothing stopping the consumer the copy was the next thing it
	// fetched — a loop at broker speed for the length of the run.
	ActionHoldAndDefer
)

// Hold names a pause hold on a seat's inbox — the reason
// queue.EventQueue.PauseTopic is given, and the one ResumeTopic must be given
// to lift it.
//
// STABLE KEYS rather than the screening's prose, because holds are keyed by
// reason so that two subsystems gating one inbox cannot release each other's:
// the pause and the release must spell a hold identically, and a key derived
// from a log sentence would strand every seat held under the old wording.
// Named HERE, where the screening decides which one a pause-and-park takes,
// so the stage that asks for a hold and the subsystem that lifts it cannot
// disagree about its name.
type Hold string

const (
	// HoldNoTurnEngine is taken while the company configures no model, and
	// lifted by the apply that brings one.
	HoldNoTurnEngine Hold = "no_turn_engine"

	// HoldSeatPaused is taken while a person has the seat paused, and lifted
	// when they resume it.
	HoldSeatPaused Hold = "seat_paused"

	// HoldSandbox is taken while one of the seat's detached executors
	// holds it, or a reply recorded as a parked run's answer is still owed
	// the resume it drives — and lifted when neither is true. The sandbox
	// coordinator takes and lifts it, at its own transitions (see
	// sandbox.SeatHold); the screening asks for it only for a delivery that
	// raced it ([ActionHoldAndDefer]).
	HoldSandbox Hold = "sandbox"
)

func (a Action) String() string {
	switch a {
	case ActionProceed:
		return "proceed"
	case ActionDrop:
		return "drop"
	case ActionDefer:
		return "defer"
	case ActionPauseAndPark:
		return "pause_and_park"
	case ActionHoldAndDefer:
		return "hold_and_defer"
	default:
		return "unknown"
	}
}

// Conditions is what the node knows about itself and the seat right now.
//
// Read once, at the top of a delivery, rather than re-read per stage: two
// stages disagreeing about whether this node still owns the seat is worse than
// either answer.
type Conditions struct {
	// Owned is whether this node currently holds the seat's lease with
	// enough freshness to start work. False means a peer may already be
	// doing it.
	Owned bool

	// TurnEngineReady is false when the company this node serves configures
	// no model (an empty providers.llm, which is a valid company), so no
	// turn can run until a revision adds one.
	TurnEngineReady bool

	// SeatHeldBySandbox is whether a detached executor HOLDS the seat, so
	// it starts no new turn. An executor can work for hours, far past any
	// ack window.
	SeatHeldBySandbox bool

	// SandboxAwaitsAnswer is whether one of the seat's detached runs stopped
	// to ask a person something and is waiting for the reply.
	//
	// The opposite of the field above, not a synonym: a run parked on a
	// question gives the seat back so the answer can arrive on its inbox, so
	// this is true exactly when that one is not. The reply is an ordinary
	// message on an ordinary seat, with nothing that says what it answers. A
	// match guarded by the held field would never run, since a parked run
	// never holds the seat.
	SandboxAwaitsAnswer bool

	// AdmitsTriggers is the config posture: false when this node cannot
	// apply an epoch its peers have, so it must not start NEW work under a
	// stale company.
	AdmitsTriggers bool

	// Paused is whether a person has paused the seat. Its mail waits on the
	// inbox until they resume it.
	Paused bool

	// PauseUnknown is whether this node has not yet read the fleet's
	// pauses at all — the first answer of its watch has not arrived — so
	// it cannot say whether the seat is paused.
	//
	// NOT FOLDED INTO Paused, in either direction. Read as "not paused" it
	// would run a turn on a seat somebody stopped, in the seconds after a
	// node boots, which is exactly when a seat that was doing damage is
	// placed somewhere new. Read as "paused" it would pause-and-park every
	// seat's mail on a boot, taking a hold nothing would lift.
	PauseUnknown bool
}

// Screening is the outcome of the pre-ledger stages.
type Screening struct {
	Action Action
	Reason string

	// Cause is a deferral's class ([queue.DeferCause]), set where Action
	// defers.
	Cause queue.DeferCause

	// Hold is the pause hold an [ActionPauseAndPark] takes or an
	// [ActionHoldAndDefer] makes sure of, and empty for every other action.
	Hold Hold

	// Events are what survived. Meaningful for ActionProceed (the list to
	// read the ledger about) and for the park (the list to requeue).
	Events []*events.Event

	// OfferAsSandboxAnswer says this delivery must be offered to the sandbox's
	// answer match BEFORE the action above is carried out: a detached run may
	// be waiting on a reply it carries. A flag beside the action because
	// whether it IS the answer is a store read this package cannot make. Only
	// a free seat's proceed carries it: a defer consumes nothing, so the
	// delivery is still there to offer (see sandbox.SeatHold); named, not
	// inferred from Reason, which is log prose.
	//
	// It says offer it, never that the offer is free: an unclaimed offer spends
	// one of the message's deliveries, so the caller, which holds the queue's
	// headroom, stops offering once the rest must be kept for the ordinary
	// route (sandbox.AnswerDeliveryReserve).
	OfferAsSandboxAnswer bool

	// NoteDeferred asks the seat host to record that this consumer stopped,
	// so the next successful renew resumes it.
	//
	// Load-bearing on every defer path. Ownership freshness refuses inside
	// an ordinary heartbeat window on a perfectly healthy node — nothing
	// detaches, nothing changes hands, so nothing else would ever
	// un-quiesce the consumer. Without this the seat goes deaf for the life
	// of the process the first time a batch lands in that window.
	NoteDeferred bool
}

// Result converts a screening to the queue's own disposition, for the actions
// that map onto one directly. The park does not: it acks only after its
// requeue succeeds, which is the caller's to sequence.
func (s Screening) Result() queue.Result {
	if s.Action == ActionDefer || s.Action == ActionHoldAndDefer {
		return queue.Defer(s.Cause, s.Reason)
	}
	return queue.Ack()
}

// Screen runs every guard that must precede the completion-ledger read, in the
// order of its cases (which say why each sits where it does). The ledger read
// comes after ALL of them, so a parked partition is never marked done, and
// before coalescing ([Route]).
//
// There is no re-entrancy stage, by decision: every queue backend fetches again
// only after a handler returns and the in-process twin defers a nested drain,
// so a publish to a seat's own inbox from inside its turn cannot dispatch
// inline, and queuetest's Reentrancy group pins that. The sandbox answer offer
// is no stage either: it is a flag ([Screening.OfferAsSandboxAnswer]).
func Screen(c Conditions, evs []*events.Event) Screening {
	// Same-id dedupe runs FIRST: at-least-once delivery and the requeue
	// machinery's republish edges can put two copies of one event in a drain,
	// identical by construction. The parking cases REPUBLISH, so deduping after
	// them would multiply the copies across shed and sandbox cycles.
	evs = dedupe(evs)
	if len(evs) == 0 {
		return Screening{Action: ActionDrop, Reason: "empty"}
	}
	switch {
	case !c.Owned:
		// Defers rather than requeues, for the reason on ActionDefer.
		return Screening{Action: ActionDefer, Cause: queue.DeferSeatMoved,
			Reason: "seat is not owned here", NoteDeferred: true}
	case c.PauseUnknown:
		// A node that has not read the fleet's pauses cannot say whether
		// somebody stopped this seat, and a turn it runs cannot be taken back.
		return Screening{
			Action: ActionDefer, Cause: queue.DeferPauseUnread, NoteDeferred: true,
			Reason: "this node has not yet read whether the seat is paused",
		}
	case c.Paused:
		// Before the turn engine: a person's decision is the one a later apply
		// cannot undo, so a seat paused and held for want of a model must still
		// be held when the model arrives. The node's pause watch normally holds
		// the inbox first; this closes the race with a delivery in flight.
		return Screening{
			Action: ActionPauseAndPark, Hold: HoldSeatPaused, Events: evs,
			Reason: "a person paused this seat",
		}
	case !c.TurnEngineReady:
		// Pause the topic first so requeued copies buffer, then park: dropping
		// them loses the work, requeuing without the pause loops them at the
		// broker's rate. The caller releases the pause when a model arrives.
		return Screening{
			Action: ActionPauseAndPark, Hold: HoldNoTurnEngine, Events: evs,
			Reason: "no turn engine: the company configures no providers.llm",
		}
	case c.SeatHeldBySandbox:
		// Hold and defer: what reaches here raced the coordinator's hold or found
		// it refused, so the hold is asked for again and the delivery waits at
		// the head of the inbox. A park would republish it onto the very inbox
		// it came from, fetched again at once.
		//
		// NOT OFFERED as an answer, even when the seat's executor on another
		// turn waits for a person: the held seat receives nothing, and that
		// answer waits on the inbox with the rest of its mail — offered
		// first when the executor stops holding the seat. See sandbox.SeatHold.
		return Screening{
			Action: ActionHoldAndDefer, Hold: HoldSandbox, NoteDeferred: true,
			Cause: queue.DeferExecutorHolds, Reason: "the seat's executor holds it from its box",
		}
	case !c.AdmitsTriggers:
		// After the sandbox case, deliberately: a seat mid-sandbox is already
		// held there. Defer, never requeue: a shed's release is fenced, so
		// republishing reorders the conversation for the successor, and the copy
		// lands on a topic this node still attaches to and is shed again for
		// ever. Deferring cannot spin: the consumer stops after the first one.
		return Screening{Action: ActionDefer, Cause: queue.DeferShedding,
			Reason: "config posture refuses new work", NoteDeferred: true}
	}
	return Screening{
		Action: ActionProceed, Events: evs,
		OfferAsSandboxAnswer: c.SandboxAwaitsAnswer,
	}
}

// dedupe drops repeat ids, preserving first-seen order.
//
// Order matters because the surviving list is a CONVERSATION: the events are
// one key-partition and the turn reads them in sequence. Sorting or set-ifying
// here would reorder a thread.
func dedupe(evs []*events.Event) []*events.Event {
	out := make([]*events.Event, 0, len(evs))
	seen := make(map[string]bool, len(evs))
	for _, e := range evs {
		if e == nil {
			continue
		}
		id := e.ID.String()
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, e)
	}
	return out
}

// Routing is what to do with the events that survived the ledger.
type Routing struct {
	// WorkKey identifies the unit of work this dispatch does, bound for its
	// whole duration.
	//
	// Derived from the CONSTITUENT event ids, here, because this is where
	// the constituent list exists. A coalesced digest is minted fresh on
	// every merge, so a key taken from it would differ on every redelivery
	// and match nothing — which is the same trap the completion ledger
	// itself is keyed to avoid.
	WorkKey string

	// WorkSince is when that unit of work began — see [WorkSinceFor] —
	// and it is derived from the same constituents as the key, for the
	// reason the key is: a redelivery must reproduce it.
	WorkSince time.Time

	// Coalesce is true when the partition must be merged into one digest
	// trigger, so the seat runs one turn instead of N.
	Coalesce bool

	// Events are what to dispatch.
	Events []*events.Event
}

// Route decides how the surviving events reach a turn.
//
// A multi-event partition is always one of two things: external notifications
// for one conversation, or fires of one schedule that waited in the inbox
// together (schedule.FirePartition). Every other inbox event type keys
// uniquely and so arrives alone, which is why merging is safe and why a
// heterogeneous partition reaching here is a key-scheme bug rather than an
// ordinary case.
//
// ledgered names the event types the completion ledger records. Only those
// contribute to the work key: a type the ledger never writes cannot be looked
// up by it, so including it would produce a key that matches nothing.
func Route(evs []*events.Event, ledgered func(eventType string) bool) Routing {
	return Routing{
		WorkKey:   WorkKeyFor(evs, ledgered),
		WorkSince: WorkSinceFor(evs, ledgered),
		Coalesce:  len(evs) > 1,
		Events:    evs,
	}
}

// WorkKeyFor derives the key for a set of events.
func WorkKeyFor(evs []*events.Event, ledgered func(string) bool) string {
	ids := make([]string, 0, len(evs))
	for _, e := range evs {
		if e != nil && ledgered(e.Type) {
			ids = append(ids, e.ID.String())
		}
	}
	return workkey.Derive(ids)
}

// WorkSinceFor is when the unit of work [WorkKeyFor] names BEGAN: the earliest
// creation of any event the key derives from, zero exactly when the key is empty.
//
// Same constituents and predicate as the key, being one identity: every
// operation id derived from the key carries this instant as its mint time, and
// the state log will not decide again an operation minted before its node
// adopted a donated snapshot. A redelivery reproduces it, as the key.
//
// The earliest, read as a lower bound on when the work could first have
// written anything: one that looks older than it is is answered `unknown` on a
// node that adopted since, one that looks younger is decided again. No
// timestamp reads as the zero instant likewise.
func WorkSinceFor(evs []*events.Event, ledgered func(string) bool) time.Time {
	var since time.Time
	first := true
	for _, e := range evs {
		if e == nil || !ledgered(e.Type) {
			continue
		}
		if first || e.Timestamp.Before(since) {
			since, first = e.Timestamp.UTC(), false
		}
	}
	return since
}

// Degraded is the fallback when coalescing declines or fails.
//
// Per-event semantics: REQUEUE THE TAIL FIRST, then dispatch the head. The
// order is the point — a requeue failure must NAK the partition before any
// work has run, so a completed turn is never replayed by a later event's
// failure. A partially-requeued tail can leave same-id copies behind after the
// NAK; the dedupe in [Screen] collapses them on the next drain.
//
// The head's work key is the HEAD'S ALONE, not the partition's: only the head
// ran, and recording the partition's key would mark the tail worked while its
// copies are still on the queue waiting to be.
func Degraded(evs []*events.Event, ledgered func(string) bool) (head []*events.Event, tail []*events.Event, headKey string) {
	if len(evs) == 0 {
		return nil, nil, ""
	}
	head = evs[:1]
	tail = evs[1:]
	return head, tail, WorkKeyFor(head, ledgered)
}
