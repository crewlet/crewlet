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

	// ActionPark — requeue the events, then ack. For a wait that outlasts
	// any broker ack window.
	ActionPark

	// ActionPauseAndPark — pause the topic FIRST so the requeued copies
	// buffer on the queue rather than looping straight back, then park.
	ActionPauseAndPark
)

func (a Action) String() string {
	switch a {
	case ActionProceed:
		return "proceed"
	case ActionDrop:
		return "drop"
	case ActionDefer:
		return "defer"
	case ActionPark:
		return "park"
	case ActionPauseAndPark:
		return "pause_and_park"
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

	// SeatHeldBySandbox is whether a detached coding run HOLDS the seat, so
	// it starts no new turn. Such a job can run for hours, far past any ack
	// window.
	SeatHeldBySandbox bool

	// SandboxAwaitsAnswer is whether one of the seat's detached runs stopped
	// to ask a person something and is waiting for the reply.
	//
	// THE OPPOSITE OF THE FIELD ABOVE, not a synonym for it: a run parked on
	// a question gives the seat back precisely so the answer can arrive on
	// its inbox, so this is true exactly when that one is not — and the
	// reply, when it comes, is an ordinary message on an ordinary seat with
	// nothing about it that says what it answers.
	//
	// Both were one field called AwaitingSandbox, which named this question
	// and held that one. The match guarded by it therefore ran only while
	// the seat was HELD, which is the one state a parked run is never in, so
	// no answer to a clarification ever reached the run that asked.
	SandboxAwaitsAnswer bool

	// AdmitsTriggers is the config posture: false when this node cannot
	// apply an epoch its peers have, so it must not start NEW work under a
	// stale company.
	AdmitsTriggers bool
}

// Screening is the outcome of the pre-ledger stages.
type Screening struct {
	Action Action
	Reason string

	// Events are what survived. Meaningful for ActionProceed (the list to
	// read the ledger about) and for the park actions (the list to
	// requeue).
	Events []*events.Event

	// OfferAsSandboxAnswer says this delivery must be offered to the
	// sandbox's answer match BEFORE the action above is carried out: one of
	// the seat's detached runs may be waiting on a person's reply that this
	// very delivery carries.
	//
	// ORTHOGONAL TO THE ACTION, which is why it is a flag beside it rather
	// than an action of its own. Nothing here can tell whether the delivery
	// IS the answer — that is a store read, and this package reaches no
	// store — so what a screening states is the pair: offer it, and if the
	// offer does not claim it, do this. Both actions that CONSUME a delivery
	// carry it: the park a held seat makes, and the ordinary proceed a free
	// seat makes while one of its runs waits for a reply. A defer does not,
	// and does not need to: it consumes nothing and stops the consumer, so
	// the delivery is still there to be offered when the condition clears.
	//
	// Named rather than inferred from Reason, which is prose for a log: a
	// caller matching on the sentence would break silently the first time
	// it was reworded, and the failure mode is a clarification answer
	// requeued for ever behind the question it answers.
	//
	// IT SAYS OFFER IT, NEVER THAT THE OFFER IS FREE. On the proceed path
	// an offer that is not claimed hands the delivery back, which spends
	// one of the message's deliveries — so the caller stops offering once
	// what is left has to be kept for the ordinary route, and it is the
	// caller that knows the count because it holds the queue's own
	// delivery headroom. See sandbox.AnswerDeliveryReserve. Nothing about
	// that decision belongs here: this package reaches no store and no
	// transport, and a flag stating one screening's opinion of a broker's
	// budget would be the same false claim it corrects.
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
// that map onto one directly. The park actions do not: they ack only after
// their requeue succeeds, which is the caller's to sequence.
func (s Screening) Result() queue.Result {
	if s.Action == ActionDefer {
		return queue.Defer(s.Reason)
	}
	return queue.Ack()
}

// Screen runs every guard that must precede the completion-ledger read.
//
// The order, and why each stage is where it is:
//
//  1. SAME-ID DEDUPE, before any parking branch. At-least-once delivery — and
//     the requeue machinery's own republish edges, a publish that timed out
//     client-side but landed, a partial requeue followed by a partition NAK —
//     can put two copies of one event in a single drain. Identical ids mean
//     identical payloads by construction, so dropping the extras is the one
//     always-safe dedupe. It must run FIRST because the parking branches
//     REPUBLISH: deduping after them meant every park pushed the duplicates
//     back onto the topic, so copies multiplied across shed and sandbox cycles
//     instead of holding steady.
//
//  2. OWNERSHIP. This node consumes the seat only while it holds the lease.
//     Defers rather than requeues, for the reason on ActionDefer.
//
//  3. NO TURN ENGINE. Pause the topic first so the requeued copies buffer,
//     then park. Consuming and dropping them would lose the work outright;
//     requeuing without the pause loops them at whatever rate the broker will
//     serve. The pause is the caller's to release, when a model arrives.
//
//  4. SEAT HELD BY A SANDBOX RUN. Park. The job outlasts any ack window.
//
//  5. CONFIG POSTURE. Defer. This sits AFTER the sandbox branch deliberately:
//     a seat mid-sandbox is already parked there, so a clarification answer
//     reaching a shedding node behaves exactly as it does on a healthy one.
//     Requeue would be wrong twice over — a shed releases this node's seats and
//     a release is fenced, so republishing reorders the conversation for the
//     successor; and the copy lands back on a topic this node is still attached
//     to, so a failed release means it comes straight back and is shed again,
//     forever. Deferring cannot spin: the consumer stops after the first one.
//
// There is NO re-entrancy stage, and its absence is a decision rather than an
// omission. The hazard is real where a publish to a seat's own inbox from
// inside its running turn dispatches inline on the same task, so handling it
// waits on the turn from within the turn. Every queue backend here forecloses
// that structurally — the pull loops fetch again only
// after a handler returns, and the in-process twin defers a nested drain to the
// loop already running rather than starting a second one — so the condition
// cannot arise and a guard for it would be a branch no delivery can reach.
// queuetest's Reentrancy group pins that property on every backend, which is
// where a change that brought the hazard back would fail.
//
// The completion-ledger read comes after ALL of these — so a parked partition
// is never marked done — and before coalescing, so recorded constituents drop
// out and only the remainder merges. See [Route].
//
// The sandbox ANSWER OFFER is not a stage, and deliberately: it decides
// nothing about the delivery's disposition, it only says the disposition is
// conditional on a store read the caller makes. So it rides as a flag on the
// two outcomes that consume a delivery — see [Screening.OfferAsSandboxAnswer]
// — and leaves the order above exactly as it was.
func Screen(c Conditions, evs []*events.Event) Screening {
	evs = dedupe(evs)
	if len(evs) == 0 {
		return Screening{Action: ActionDrop, Reason: "empty"}
	}
	switch {
	case !c.Owned:
		return Screening{Action: ActionDefer, Reason: "seat is not owned here", NoteDeferred: true}
	case !c.TurnEngineReady:
		return Screening{
			Action: ActionPauseAndPark, Events: evs,
			Reason: "no turn engine: the company configures no providers.llm",
		}
	case c.SeatHeldBySandbox:
		// UNCONDITIONALLY OFFERED, without consulting the second
		// condition: a seat can hold one run while another of its runs
		// waits for a person, and this is the park that would otherwise
		// requeue the reply behind the question it answers.
		return Screening{
			Action: ActionPark, Reason: "a detached sandbox run holds the seat",
			OfferAsSandboxAnswer: true, Events: evs,
		}
	case !c.AdmitsTriggers:
		return Screening{Action: ActionDefer, Reason: "config posture refuses new work", NoteDeferred: true}
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
// A multi-event partition is always external notifications for one
// conversation — every other inbox event type keys uniquely and so arrives
// alone — which is why merging is safe and why a heterogeneous partition
// reaching here is a key-scheme bug rather than an ordinary case.
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
// instant any event the key is derived from was created, and the zero instant
// exactly when the key is empty.
//
// THE SAME CONSTITUENTS AS THE KEY, filtered by the same predicate, because the
// two are one identity: every operation id a turn derives from the key carries
// this instant as its mint time, and the state log will not decide again an
// operation minted before its node adopted a donated snapshot. A redelivery
// hands over the very same events, so it reproduces the instant exactly as it
// reproduces the key — which no instant of the dispatch itself could do.
//
// The EARLIEST rather than any other, because the instant is read as a lower
// bound on when the work could first have written anything: an operation that
// looks older than it is is answered `unknown` on a node that adopted since,
// while one that looks younger is decided again. An event carrying no
// timestamp is read as the zero instant for the same reason.
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
