// Package inbox holds the ordering rule for a seat's inbox partition.
//
// THE ORDER IS THE PRODUCT. Every stage below sits where it does because a
// different position produced a specific, observed failure, so the sequence is
// written out once, in one place, with each stage's reason at the stage rather
// than in a comment on the caller.
//
// The decision is separated from the EFFECTS — no broker, no database, no
// event queue is reachable from here. A handler that mixes the two can only be
// exercised against a live broker, which is why the Python this replaces had
// its ordering pinned by integration tests that took a container to run.
//
// Two stages, and the split is structural rather than stylistic: [Screen] runs
// every guard that must precede the completion-ledger read, and returns the
// events that survive. The caller cannot perform the ledger read before
// Screen, because Screen is what hands it the list to read about.
package inbox

import (
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
	// reorders the conversation. Not a NAK either: three redeliveries
	// dead-letter a perfectly healthy event, and the condition can hold
	// for minutes.
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

	// TurnEngineReady is false when the node booted with no LLM providers
	// and cannot run a turn at all.
	TurnEngineReady bool

	// AwaitingSandbox is whether the seat is parked on a detached coding
	// run. Such a job can run for hours, far past any ack window.
	AwaitingSandbox bool

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
//     serve.
//
//  4. AWAITING SANDBOX. Park. The job outlasts any ack window.
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
// omission. The Python this replaces carried one: a publish to a seat's own
// inbox from inside its running turn dispatched inline within the same asyncio
// task, so handling it awaited the turn from within the turn. Every queue
// backend here forecloses that structurally — the pull loops fetch again only
// after a handler returns, and the in-process twin defers a nested drain to the
// loop already running rather than starting a second one — so the condition
// cannot arise and a guard for it would be a branch no delivery can reach.
// queuetest's Reentrancy group pins that property on every backend, which is
// where a change that brought the hazard back would fail.
//
// The completion-ledger read comes after ALL of these — so a parked partition
// is never marked done — and before coalescing, so recorded constituents drop
// out and only the remainder merges. See [Route].
func Screen(c Conditions, evs []*events.Event) Screening {
	evs = dedupe(evs)
	if len(evs) == 0 {
		return Screening{Action: ActionDrop, Reason: "empty"}
	}
	switch {
	case !c.Owned:
		return Screening{Action: ActionDefer, Reason: "seat is not owned here", NoteDeferred: true}
	case !c.TurnEngineReady:
		return Screening{Action: ActionPauseAndPark, Reason: "no turn engine", Events: evs}
	case c.AwaitingSandbox:
		return Screening{Action: ActionPark, Reason: "awaiting a detached sandbox run", Events: evs}
	case !c.AdmitsTriggers:
		return Screening{Action: ActionDefer, Reason: "config posture refuses new work", NoteDeferred: true}
	}
	return Screening{Action: ActionProceed, Events: evs}
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
		WorkKey:  WorkKeyFor(evs, ledgered),
		Coalesce: len(evs) > 1,
		Events:   evs,
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
