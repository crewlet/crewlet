// Package pickup decides the WORK PICKUP: the one wake the engine derives for
// an agent seat that is idle while it holds assigned work that is ready
// (ADR-0112).
//
// # Why a wake of its own
//
// Every other wake is derived from a change (ADR-0012): an assignment wakes
// its assignee once, and a turn that does not start the work (the seat was
// busy, it answered a chat message instead, it said it would come back to the
// item) is the last word anybody says about that item. Nothing returns an idle
// seat to work it already holds, so ready items sat for days with seats that
// were free. The pickup is derived from STATE instead: the seat is idle, and
// the tracker says it holds ready work.
//
// # Once per item version
//
// A pickup is identified by (seat, item, the last change somebody else made
// to the item, the item's status) — [Key]. Its event id is derived from that
// key ([Key.WakeID]), so the fleet's completion ledger, which every node
// shares, records a pickup the seat had its turn on and refuses it again for
// [coord.LedgerRetention], whichever node holds the seat and across restarts;
// and a fleet claim on the same key ([Key.ClaimKey]) keeps two nodes that both
// believe they hold the seat during a move from publishing it twice. A seat
// that declines an item, or comments on it, is not handed the same item again
// until somebody else changes it or its status moves. A seat that started an
// item is handed it once more, as work in progress, when it is idle again.
//
// # When a seat is idle
//
// Decided by the node that holds the seat, from what only that node knows
// ([Conditions]): no turn of the seat dispatching, no executor run holding it
// or waiting on a person's answer, nothing waiting in its inbox, not paused,
// not parked on a budget or a provider limit, its provider not limited, the
// node neither shedding nor draining, and all of that true on two passes in a
// row ([Conditions.IdleBefore]), so the writes of the turn that just ended are
// on the node's copy before the pickup reads the tracker. [Idle] is that
// decision, pure over the conditions, and [Skip] is what each refusal is
// called in the log line and the counter an operator reads.
//
// # One per seat per cooldown
//
// A fleet claim on the seat ([CooldownKey]) taken for the configured
// cooldown, so a seat that declines one ready item after another is offered
// the next at most once per cooldown, wherever it is placed.
package pickup

import (
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/workkey"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Source is the event source a pickup is published under.
const Source = "pickup"

// Tick is how often the holder of a seat decides its pickup. A minute: the
// idle settle is one tick, so a seat whose turn ended is handed its next item
// within two, and a company of fifty idle seats costs fifty indexed reads a
// minute.
const Tick = time.Minute

// Key identifies one pickup: the seat, the item, the last change somebody
// other than the seat made to it, and its status.
type Key struct {
	Seat   uuid.UUID
	Item   string
	Moved  uint64
	Status string
}

// KeyOf is the pickup an item would be for seat.
func KeyOf(seat uuid.UUID, item worklet.ReadyItem) Key {
	return Key{Seat: seat, Item: item.ID, Moved: item.Moved, Status: string(item.Status)}
}

// pickupNamespace scopes derived pickup ids. Fixed for the life of the
// deployment: a new one would hand every seat every item again.
var pickupNamespace = uuid.MustParse("7d0c6f2e-4b1a-5e39-8c27-91f3a6b2d4e8")

func (k Key) text() string {
	return k.Seat.String() + "\x00" + k.Item + "\x00" +
		strconv.FormatUint(k.Moved, 10) + "\x00" + k.Status
}

// WakeID is the pickup's event id: the same on every node and every attempt.
func (k Key) WakeID() uuid.UUID { return uuid.NewSHA1(pickupNamespace, []byte(k.text())) }

// WorkKey is the completion ledger's key for the pickup: the one a turn on
// its event alone records ([workkey.Derive]).
func (k Key) WorkKey() string { return workkey.Derive([]string{k.WakeID().String()}) }

// ClaimKey is the fleet claim that keeps a pickup from being published twice
// while its first copy waits for its turn.
func (k Key) ClaimKey() string { return "pickup|item|" + k.WakeID().String() }

// CooldownKey is the fleet claim that spaces one seat's pickups.
func CooldownKey(seat uuid.UUID) string { return "pickup|seat|" + seat.String() }

// Choose is the first ready item whose pickup the seat has not had, and its
// key; false when every ready item's pickup is already worked.
func Choose(seat uuid.UUID, items []worklet.ReadyItem, worked map[string]bool) (
	worklet.ReadyItem, Key, bool) {

	for _, item := range items {
		key := KeyOf(seat, item)
		if !worked[key.WorkKey()] {
			return item, key, true
		}
	}
	return worklet.ReadyItem{}, Key{}, false
}

// WorkKeys is every ready item's ledger key, in order: what [Choose] asks the
// completion ledger about, in one read.
func WorkKeys(seat uuid.UUID, items []worklet.ReadyItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, KeyOf(seat, item).WorkKey())
	}
	return out
}

// Event is the pickup's wake: a [types.TaskAssigned] naming the item, with
// the derived id, the item's own last-updated instant as its timestamp (the
// same on every attempt, so a re-run turn derives the same operation ids),
// and the item's conversation as both inbox keys, so the turn continues the
// item's own session.
func Event(seat uuid.UUID, role string, item worklet.ReadyItem, key Key, ready int,
	trace events.TraceContext) *events.Event {

	ev := events.New(types.TaskAssigned{
		TaskID:   "pickup:" + key.WakeID().String(),
		Agent:    seat.String(),
		RoleName: role,
		Pickup: &types.WorkPickup{
			// THE ADDRESS, the id where another task claimed the key
			// first, so the brief never sends the seat to the claimant.
			Item: item.ID, Key: worklet.ItemAddress(item.ID, item.Key, item.KeyCollision),
			Status:  string(item.Status),
			Version: item.Moved, Why: item.Why, Ready: ready,
		},
	}, trace)
	ev.ID = key.WakeID()
	if !item.Updated.IsZero() {
		ev.Timestamp = item.Updated.UTC()
	}
	ev.Source = Source
	conversation := worklet.ItemConversation(item.ID)
	notify.Stamp(ev, conversation, conversation)
	return ev
}
